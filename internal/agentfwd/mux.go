// Package agentfwd は、単一のバイト透過ストリーム（drover-cloud relay の
// source⇄viewer パイプ）上で複数の論理接続（チャネル）を多重化する。
// 用途は owner（自機）の SSH エージェント socket を slave（共用 PC）へ
// relay 越しに転送すること＝秘密鍵を slave のディスクに置かずに git/gh の
// SSH 認証を行う（設計は DESIGN_SSH_FORWARD.md）。
//
// relay は 1 対 1 でしかペアリングしない（relay/relay.go）が、SSH agent 転送は
// 「短命な接続を複数（時に並行）」必要とする（ssh/git 起動ごとに $SSH_AUTH_SOCK
// へ 1 接続）。本 mux が単一パイプ上でそれらをチャネルとして束ねる。
//
// ワイヤ（big-endian）:
//
//	[type:1][channel:4][length:4][payload:length]
//	type 1 = DATA, type 2 = CLOSE（length 0）
//
// チャネル ID は LISTENER 端（socket を所有し accept する側＝slave）が単調に
// 割り当てる。DIALER 端（owner）は未知 ID の最初の DATA を見たら agent を dial
// する（OPEN は暗黙）。単調 ID ＋順序保証パイプにより CLOSE 後の同 ID DATA は
// 来ないが、DIALER 側は closed 集合で late-DATA の再 dial を防ぐ（防御的）。
package agentfwd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

const (
	// maxFrame は 1 フレーム payload の上限（壊れ/悪意パイプの OOM 防御）。
	// 実 SSH agent メッセージ（鍵一覧・署名）は数 KB〜数十 KB で十分収まる。
	maxFrame = 512 * 1024
	// readBuf は local conn からの 1 read 上限（＝1 DATA フレームの上限）。
	readBuf = 32 * 1024
	hdrLen  = 9
)

type frameType byte

const (
	ftData  frameType = 1
	ftClose frameType = 2
)

// errFrameTooBig は length ヘッダが maxFrame を超えた時（防御）。
var errFrameTooBig = errors.New("agentfwd: frame too big")

// mux は単一パイプ上のチャネル多重化状態。
type mux struct {
	pipe io.ReadWriteCloser

	wmu sync.Mutex // フレーム書込の直列化（フレーム途中の割込みを防ぐ）

	mu     sync.Mutex
	chans  map[uint32]net.Conn
	closed map[uint32]bool // 一度閉じた ID（late-DATA の再 dial 防止）
	nextID atomic.Uint32   // LISTENER 端のみ使用（単調割当）
}

func newMux(pipe io.ReadWriteCloser) *mux {
	return &mux{
		pipe:   pipe,
		chans:  map[uint32]net.Conn{},
		closed: map[uint32]bool{},
	}
}

// writeFrame は 1 フレームを atomically にパイプへ書く（ヘッダ+payload を
// 単一 Write）。relay はバイト透過なので message 境界は無意味。
func (m *mux) writeFrame(t frameType, id uint32, payload []byte) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	frame := make([]byte, hdrLen+len(payload))
	frame[0] = byte(t)
	binary.BigEndian.PutUint32(frame[1:5], id)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[hdrLen:], payload)
	_, err := m.pipe.Write(frame)
	return err
}

func (m *mux) register(id uint32, c net.Conn) {
	m.mu.Lock()
	m.chans[id] = c
	m.mu.Unlock()
}

func (m *mux) get(id uint32) net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chans[id]
}

func (m *mux) isClosed(id uint32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[id]
}

// closeChan はチャネルを畳む（local conn を close し closed に記録）。
// 二重呼び出しは安全（既に無ければ no-op）。
func (m *mux) closeChan(id uint32) {
	m.mu.Lock()
	c := m.chans[id]
	delete(m.chans, id)
	m.closed[id] = true
	m.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (m *mux) closeAll() {
	m.mu.Lock()
	conns := make([]net.Conn, 0, len(m.chans))
	for id, c := range m.chans {
		conns = append(conns, c)
		delete(m.chans, id)
	}
	m.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// pumpChannel は local conn c → framed DATA をパイプへ流し、EOF/エラーで
// CLOSE を送ってチャネルを畳む（チャネルごとに 1 goroutine）。
func (m *mux) pumpChannel(id uint32, c net.Conn) {
	buf := make([]byte, readBuf)
	for {
		n, rerr := c.Read(buf)
		if n > 0 {
			if werr := m.writeFrame(ftData, id, buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = m.writeFrame(ftClose, id, nil) // 相手へ close 通知（best-effort）
	m.closeChan(id)
}

// readLoop はパイプからフレームを読んで dispatch する。newChan は DIALER 端で
// のみ非 nil＝未知 ID の DATA を見たら agent を dial して conn を返す（nil で
// drop）。LISTENER 端は newChan=nil（新チャネルは accept 側でのみ生まれる）。
// パイプ read エラー（相手切断/ctx cancel での close）で戻る。
func (m *mux) readLoop(newChan func(id uint32) net.Conn) error {
	hdr := make([]byte, hdrLen)
	for {
		if _, err := io.ReadFull(m.pipe, hdr); err != nil {
			return err
		}
		t := frameType(hdr[0])
		id := binary.BigEndian.Uint32(hdr[1:5])
		n := binary.BigEndian.Uint32(hdr[5:9])
		if n > maxFrame {
			return errFrameTooBig
		}
		var payload []byte
		if n > 0 {
			payload = make([]byte, n)
			if _, err := io.ReadFull(m.pipe, payload); err != nil {
				return err
			}
		}
		switch t {
		case ftData:
			c := m.get(id)
			if c == nil {
				if m.isClosed(id) {
					continue // 既に閉じたチャネルへの late-DATA は無視
				}
				if newChan != nil {
					c = newChan(id) // DIALER: agent を dial（register+pump 込み）
				}
			}
			if c != nil {
				if _, err := c.Write(payload); err != nil {
					_ = m.writeFrame(ftClose, id, nil)
					m.closeChan(id)
				}
			}
		case ftClose:
			m.closeChan(id)
		}
	}
}

// ServeListener は LISTENER 端を回す。ln（local unix socket）を accept し、
// 各接続を単調 ID のチャネルとしてパイプ上へ多重化する。パイプ切断／ctx 終了で
// 全チャネルを畳んで戻る。戻り時に ln と pipe は close 済み。
func ServeListener(ctx context.Context, pipe io.ReadWriteCloser, ln net.Listener) error {
	m := newMux(pipe)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// ctx 終了で accept/read をほどく。
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = pipe.Close()
	}()

	// accept ループ: 接続ごとにチャネル割当＋pump。
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			id := m.nextID.Add(1)
			m.register(id, c)
			go m.pumpChannel(id, c)
		}
	}()

	err := m.readLoop(nil)
	m.closeAll()
	_ = ln.Close()
	_ = pipe.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// ServeDialer は DIALER 端を回す。新チャネル（未知 ID の最初の DATA）ごとに
// dial() を呼んで agent 接続を確立し、双方向にポンプする。dial 失敗時はその
// チャネルへ CLOSE を返す。パイプ切断／ctx 終了で全チャネルを畳んで戻る。
func ServeDialer(ctx context.Context, pipe io.ReadWriteCloser, dial func() (net.Conn, error)) error {
	m := newMux(pipe)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = pipe.Close()
	}()

	newChan := func(id uint32) net.Conn {
		c, err := dial()
		if err != nil {
			_ = m.writeFrame(ftClose, id, nil)
			m.closeChan(id) // closed に記録＝同 ID の再 dial を防ぐ
			return nil
		}
		m.register(id, c)
		go m.pumpChannel(id, c)
		return c
	}

	err := m.readLoop(newChan)
	m.closeAll()
	_ = pipe.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
