//go:build !windows

// bridge_test.go — 実 herdr 隔離サーバでの統合テスト（鉄則: 合成ストリーム
// だけで緑にしない。herdr 不在環境のみ Skip）。
//
// 検証シナリオ（タスク指定の一気通貫）:
//
//	fake conn（net.Pipe）に RESIZE magic を書く → observe が spawn され
//	frame（DECSET 2026 括り ANSI）が conn へ流れる → 入力バイトを書く →
//	pane 到達を pane.read で確認 → RESIZE 再送で observe respawn（新サイズ
//	argv・旧 PID 消滅・新 full frame）→ observe 外部 kill で backoff respawn
//	→ ctx cancel で subprocess 完全掃除（リーク検査）
//
// 隔離レシピは producer_test.go / client_test.go / test/e2e_test.go と同一
// （短い /tmp dir＝sun_path 104B 制約・XDG_CONFIG_HOME 隔離・停止は自
// socket への server stop → 自 spawn PID のみ kill。裸の pkill は恒久禁止）。
package bridge

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// ============ 実 herdr 隔離サーバ（確定レシピのコピー） ============

type testServer struct {
	t    *testing.T
	bin  string
	sock string
	env  []string
	cmd  *exec.Cmd
}

// startHerdr は隔離 herdr サーバを起動する。XDG_CONFIG_HOME/XDG_STATE_HOME
// も隔離（HERDR_SOCKET_PATH だけの隔離ではユーザー共有の session 状態を
// 引き込む＝Phase1 実測の知見）。
func startHerdr(t *testing.T) (*testServer, *herdrapi.Client) {
	t.Helper()
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed; skipping real-server test")
	}
	dir, err := os.MkdirTemp("/tmp", "hd")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	xdg := filepath.Join(dir, "xdg")
	xdgState := filepath.Join(dir, "xdgs")
	for _, d := range []string{xdg, xdgState} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	s := &testServer{
		t:    t,
		bin:  bin,
		sock: filepath.Join(dir, "h.sock"),
		env: append(os.Environ(),
			"HERDR_SOCKET_PATH="+filepath.Join(dir, "h.sock"),
			"XDG_CONFIG_HOME="+xdg,
			"XDG_STATE_HOME="+xdgState),
	}
	t.Cleanup(func() {
		s.stop()
		os.RemoveAll(dir)
	})
	cmd := exec.Command(bin, "server")
	cmd.Env = s.env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start herdr server: %v", err)
	}
	s.cmd = cmd
	c := herdrapi.New(s.sock)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := c.Ping(); err == nil {
			return s, c
		}
		if time.Now().After(deadline) {
			s.stop()
			t.Fatalf("herdr server did not become ready at %s", s.sock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop は自分の spawn したサーバだけを graceful stop → 5s → 自 PID kill。
func (s *testServer) stop() {
	if s.cmd == nil {
		return
	}
	stop := exec.Command(s.bin, "server", "stop")
	stop.Env = s.env
	_ = stop.Run()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := cond()
		if ok {
			return
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s (last err: %v)", what, lastErr)
}

// ============ conn 側ヘルパ ============

// connSink は fake conn（net.Pipe の viewer 端）から bridge 出力を吸い出して
// 蓄積する（net.Pipe は無バッファ＝読み手が居ないと bridge の frame 書込が
// 詰まるため、常時 drain が必須）。
type connSink struct {
	mu  sync.Mutex
	buf []byte
}

func newConnSink(c net.Conn) *connSink {
	s := &connSink{}
	go func() {
		b := make([]byte, 64*1024)
		for {
			n, err := c.Read(b)
			if n > 0 {
				s.mu.Lock()
				s.buf = append(s.buf, b[:n]...)
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *connSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

// containsFrom は offset 以降に needle が現れるか。
func (s *connSink) containsFrom(offset int, needle []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset > len(s.buf) {
		return false
	}
	return bytes.Contains(s.buf[offset:], needle)
}

// textContainsFrom は offset 以降の ANSI エスケープを剥がした可視文字列に
// needle が現れるか。observe frame はセル毎に CUP/SGR を挟んで塗る（実測:
// 約 20B/セル）ため、画面テキストは生バイト列上で連続しない＝エスケープを
// 落としてから照合する（display-oracle の簡易版。完全な VT 照合は Phase 2
// の実ブラウザゲート）。
func (s *connSink) textContainsFrom(offset int, needle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset > len(s.buf) {
		return false
	}
	return strings.Contains(stripANSI(s.buf[offset:]), needle)
}

// stripANSI は CSI（ESC [ … 終端 0x40-0x7e）・OSC（ESC ] … BEL / ESC \）・
// その他 2 バイト ESC シーケンスを除去して可視バイトだけ残す。
func stripANSI(b []byte) string {
	var out []byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != 0x1b {
			if c >= 0x20 || c == '\n' || c == '\t' {
				out = append(out, c)
			}
			continue
		}
		i++
		if i >= len(b) {
			break
		}
		switch b[i] {
		case '[': // CSI: パラメータ/中間バイトを飛ばし終端バイトまで
			for i++; i < len(b); i++ {
				if b[i] >= 0x40 && b[i] <= 0x7e {
					break
				}
			}
		case ']': // OSC: BEL か ST(ESC \) まで
			for i++; i < len(b); i++ {
				if b[i] == 0x07 {
					break
				}
				if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
					i++
					break
				}
			}
		default: // 2 バイト ESC シーケンス（ESC 7 / ESC 8 等）
		}
	}
	return string(out)
}

func resizeMagic(rows, cols int) []byte {
	return []byte{0xff, 0xff, byte(rows >> 8), byte(rows), byte(cols >> 8), byte(cols)}
}

// pidAlive は PID が生きているか（signal 0）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// ============ 統合テスト本体 ============

// TestBridgeRealHerdrEndToEnd は bridge の主要経路を実 herdr で一気通貫。
func TestBridgeRealHerdrEndToEnd(t *testing.T) {
	srv, hc := startHerdr(t)
	_ = srv

	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	viewerConn, bridgeConn := net.Pipe()
	defer viewerConn.Close()
	sink := newConnSink(viewerConn)

	b := New(paneID, bridgeConn, hc)
	b.Idle = -1 // このテストは段階間に静止があり得る＝quiescence は別テストで検証
	b.Logf = t.Logf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	// --- 1) 初回 RESIZE → observe spawn → frame が流れる ---
	// 初回 RESIZE 前は observe が居ないこと（初回 RESIZE 待ち仕様）も確認。
	time.Sleep(300 * time.Millisecond)
	if pid := b.observePID(); pid != 0 {
		t.Fatalf("初回 RESIZE 前に observe が居る: pid=%d", pid)
	}
	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	// 全フレームは DECSET 2026 括り（実測）＝frame 到来の機械判定に使う。
	waitFor(t, 15*time.Second, "初回 frame (DECSET 2026)", func() (bool, error) {
		return sink.containsFrom(0, []byte("\x1b[?2026h")), nil
	})
	pid1 := b.observePID()
	if pid1 == 0 || !pidAlive(pid1) {
		t.Fatalf("observe が居ない/死んでいる: pid=%d", pid1)
	}
	if args := strings.Join(b.observeArgs(), " "); !strings.Contains(args, "--cols 80 --rows 24") {
		t.Fatalf("observe argv が要求サイズでない: %q", args)
	}

	// --- 2) 入力バイト → pane 到達（pane.read で確認） ---
	// クォート越しのマーカー: 画面には typed 行が `echo HD_BRIDGE_'OK'`、
	// 実行出力が `HD_BRIDGE_OK` と出る＝後者の一致は「シェルが実行した」
	// 証拠（send_input の \r 欠落ならエコーだけで出力は出ない）。
	if _, err := viewerConn.Write([]byte("echo HD_BRIDGE_'OK'\r")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	waitFor(t, 15*time.Second, "入力が pane で実行される", func() (bool, error) {
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_BRIDGE_OK"), nil
	})
	// 画面変化が frame として viewer へも流れる（diff frame。セル毎に
	// CUP/SGR が挟まるため ANSI 除去後のテキストで照合）
	waitFor(t, 15*time.Second, "実行結果が frame に載る", func() (bool, error) {
		return sink.textContainsFrom(0, "HD_BRIDGE_OK"), nil
	})

	// --- 3) RESIZE 再受信 → observe respawn（新 argv・旧 PID 消滅・full frame） ---
	off := sink.len()
	if _, err := viewerConn.Write(resizeMagic(33, 100)); err != nil {
		t.Fatalf("write RESIZE2: %v", err)
	}
	waitFor(t, 15*time.Second, "respawn 後の新 observe（--rows 33）", func() (bool, error) {
		args := strings.Join(b.observeArgs(), " ")
		pid := b.observePID()
		return pid != 0 && pid != pid1 && strings.Contains(args, "--cols 100 --rows 33"), fmt.Errorf("pid=%d args=%q", pid, args)
	})
	pid2 := b.observePID()
	waitFor(t, 10*time.Second, "旧 observe の消滅（交錯防止）", func() (bool, error) {
		return !pidAlive(pid1), nil
	})
	// respawn 直後は必ず full frame（\x1b[2J 全再描画・実測）が来る
	waitFor(t, 15*time.Second, "respawn 後の full frame (\\x1b[2J)", func() (bool, error) {
		return sink.containsFrom(off, []byte("\x1b[2J")), nil
	})

	// --- 4) observe 外部 kill → backoff respawn ---
	off = sink.len()
	if err := syscall.Kill(pid2, syscall.SIGKILL); err != nil {
		t.Fatalf("kill observe: %v", err)
	}
	waitFor(t, 15*time.Second, "kill 後の backoff respawn", func() (bool, error) {
		pid := b.observePID()
		return pid != 0 && pid != pid2 && pidAlive(pid), fmt.Errorf("pid=%d", pid)
	})
	waitFor(t, 15*time.Second, "respawn 後に frame が再開する", func() (bool, error) {
		return sink.containsFrom(off, []byte("\x1b[?2026h")), nil
	})
	pid3 := b.observePID()

	// --- 5) ctx cancel → Run が戻り subprocess 完全掃除（リーク禁止） ---
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx cancel 後 10s で Run が戻らない")
	}
	waitFor(t, 5*time.Second, "cancel 後の observe 掃除", func() (bool, error) {
		return !pidAlive(pid3), nil
	})
}

// TestBridgeConnCloseCleansUp は viewer 側 conn close（relay 切断相当）で
// Run が戻り observe が残らないことを検証する（wake→再接続サイクルの
// 基本律: 切断のたびにサブプロセスを畳む）。
func TestBridgeConnCloseCleansUp(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	viewerConn, bridgeConn := net.Pipe()
	sink := newConnSink(viewerConn)

	b := New(ws.RootPane.PaneID, bridgeConn, hc)
	b.Idle = -1
	b.Logf = t.Logf
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(context.Background()) }()

	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	waitFor(t, 15*time.Second, "frame 到来", func() (bool, error) {
		return sink.containsFrom(0, []byte("\x1b[?2026h")), nil
	})
	pid := b.observePID()

	_ = viewerConn.Close()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("conn close 後 10s で Run が戻らない")
	}
	waitFor(t, 5*time.Second, "conn close 後の observe 掃除", func() (bool, error) {
		return !pidAlive(pid), nil
	})
}

// TestBridgeQuiescenceIdleClose は無通信 Idle での自切断（DESIGN: 無通信
// 30s → BridgeSourceIdle 自切断 → M9 push で自動復帰、の bridge 側）を
// 短い Idle で実証する。自切断は Run が nil で戻り、observe も残らない。
func TestBridgeQuiescenceIdleClose(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	viewerConn, bridgeConn := net.Pipe()
	sink := newConnSink(viewerConn)

	b := New(ws.RootPane.PaneID, bridgeConn, hc)
	b.Idle = 2 * time.Second // 初回 frame（起動直後の full+初期描画）が済んだ後に静止させる
	b.Logf = t.Logf
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(context.Background()) }()

	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	waitFor(t, 15*time.Second, "frame 到来", func() (bool, error) {
		return sink.containsFrom(0, []byte("\x1b[?2026h")), nil
	})
	pid := b.observePID()

	// 以後は入力もフレームも流さない（シェルは静止画面＝observe は無変化時
	// フレームを出さない・実測）→ Idle で自切断するはず。
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("quiescence 自切断は nil で戻るべき: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("quiescence 自切断が起きない")
	}
	waitFor(t, 5*time.Second, "自切断後の observe 掃除", func() (bool, error) {
		return !pidAlive(pid), nil
	})
}

// TestSendControlBytesFallback は input.go の fallback 経路（control 一時
// 接続の terminal.input{bytes}）を実 herdr で単体駆動する。primary
// （send_text）は上の e2e が通しているので、ここでは「非 UTF-8 時に使う
// 経路そのもの」が実際に pane へ届き実行されることを確認する。
func TestSendControlBytesFallback(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	b := New(paneID, nil, hc) // conn 不要（入力単体）
	b.Logf = t.Logf

	// シェル起動を待ってから送る（早すぎる入力は捨てられ得る）。
	waitFor(t, 15*time.Second, "pane 準備（send_text echo が通る）", func() (bool, error) {
		if err := hc.PaneSendText(paneID, "echo HD_WARM_'UP'\r"); err != nil {
			return false, err
		}
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_WARM_UP"), nil
	})

	if err := b.sendControlBytes([]byte("echo HD_CTL_'OK'\r")); err != nil {
		t.Fatalf("sendControlBytes: %v", err)
	}
	waitFor(t, 15*time.Second, "control bytes が pane で実行される", func() (bool, error) {
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_CTL_OK"), nil
	})
}

// TestSendInputRouting は sendInput の決定木（UTF-8→send_text／非 UTF-8→
// control bytes）を **raw capture pane** で byte-perfect 検証する。
// 方法はプローブバッテリ（battery.py）と同一: pane で `stty raw -echo;
// cat > file` を回し、届いたバイトをファイルで hexdump 照合する。
//
// ⚠シェル行で検証してはならない（本テスト開発中の実測教訓）: zsh zle は
// 不正 UTF-8 バイト（例 0xC3）の直後のバイトを多バイト文字の継続として
// 消費・置換表示（<ffffffff>）するため、「バイトは届いたのに行が壊れる」
// ＝転送と行編集の失敗を区別できない。raw pane なら zle が居ないので
// 到達バイトそのものを比較できる。
func TestSendInputRouting(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	b := New(paneID, nil, hc)
	b.Logf = t.Logf

	waitFor(t, 15*time.Second, "pane 準備", func() (bool, error) {
		if err := hc.PaneSendText(paneID, "echo HD_RT_'UP'\r"); err != nil {
			return false, err
		}
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_RT_UP"), nil
	})

	// raw capture 開始（pane のシェルと本テストは同一マシン・同一ユーザー
	// ＝ファイルを直接読んで照合できる）。
	capFile := filepath.Join(t.TempDir(), "cap.bin")
	if err := hc.PaneSendText(paneID, "stty raw -echo; cat > "+capFile+"\r"); err != nil {
		t.Fatalf("start capture: %v", err)
	}
	waitFor(t, 10*time.Second, "capture ファイル出現", func() (bool, error) {
		_, err := os.Stat(capFile)
		return err == nil, err
	})

	// 1) 有効 UTF-8 → primary（send_text）で byte-perfect
	utf8Part := []byte("HD_UTF8_あ✓\r")
	if err := b.sendInput(utf8Part); err != nil {
		t.Fatalf("sendInput(utf8): %v", err)
	}
	// 2) 非 UTF-8（0xC3 の後に継続バイト無し・0x80 孤立・0xFE）→ fallback
	//    （control bytes）で byte-perfect
	rawPart := []byte{0xC3, 0x28, 0x80, 0xFE, 'E', 'N', 'D'}
	if err := b.sendInput(rawPart); err != nil {
		t.Fatalf("sendInput(non-UTF8): %v", err)
	}

	want := append(append([]byte(nil), utf8Part...), rawPart...)
	waitFor(t, 15*time.Second, "両経路のバイトが順序どおり byte-perfect 到達", func() (bool, error) {
		got, err := os.ReadFile(capFile)
		if err != nil {
			return false, err
		}
		return bytes.Equal(got, want), fmt.Errorf("got=%x want=%x", got, want)
	})
}
