package agentfwd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// --- in-memory listener（filesystem socket 不要＝macOS 104 byte 制限も回避） ---

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial は client 端を返し、server 端を Accept キューへ積む（実 socket の
// 接続と同型）。
func (l *pipeListener) dial() (net.Conn, error) {
	cli, srv := net.Pipe()
	select {
	case l.ch <- srv:
		return cli, nil
	case <-l.closed:
		_ = cli.Close()
		_ = srv.Close()
		return nil, net.ErrClosed
	}
}

// echoAgent は dial() のたびに「読んだバイトをそのまま返す」mock agent を
// 起動し mux 側 conn を返す。onEOF は agent 側 conn が閉じた時に発火。
func echoAgent(onEOF func()) func() (net.Conn, error) {
	return func() (net.Conn, error) {
		agentSide, muxSide := net.Pipe()
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := agentSide.Read(buf)
				if n > 0 {
					if _, werr := agentSide.Write(buf[:n]); werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			_ = agentSide.Close()
			if onEOF != nil {
				onEOF()
			}
		}()
		return muxSide, nil
	}
}

// oneByteConn は Read を 1 byte に絞る（chunked パイプ再組立の検証用）。
type oneByteConn struct{ net.Conn }

func (c oneByteConn) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return c.Conn.Read(p)
}

// startPair は relay パイプ(net.Pipe)で LISTENER/DIALER を起動する。
// wrap!=nil ならパイプ両端を包む（chunked 検証）。
func startPair(t *testing.T, dial func() (net.Conn, error), wrap func(net.Conn) net.Conn) (*pipeListener, context.CancelFunc) {
	t.Helper()
	a, b := net.Pipe()
	var pa, pb net.Conn = a, b
	if wrap != nil {
		pa, pb = wrap(a), wrap(b)
	}
	ln := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = ServeListener(ctx, pa, ln) }()
	go func() { _ = ServeDialer(ctx, pb, dial) }()
	return ln, cancel
}

func readN(t *testing.T, c net.Conn, n int) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("readN(%d): %v", n, err)
	}
	return buf
}

func TestRoundTrip(t *testing.T) {
	ln, cancel := startPair(t, echoAgent(nil), nil)
	defer cancel()

	cli, err := ln.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	msg := []byte("PING-ssh-agent")
	if _, err := cli.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readN(t, cli, len(msg))
	if string(got) != string(msg) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, msg)
	}
}

func TestConcurrentChannels(t *testing.T) {
	ln, cancel := startPair(t, echoAgent(nil), nil)
	defer cancel()

	const N = 6
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cli, err := ln.dial()
			if err != nil {
				errCh <- fmt.Errorf("ch%d dial: %w", i, err)
				return
			}
			defer cli.Close()
			msg := []byte(fmt.Sprintf("REQ-%d-%d-payload", i, i*7+3))
			if _, err := cli.Write(msg); err != nil {
				errCh <- fmt.Errorf("ch%d write: %w", i, err)
				return
			}
			_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, len(msg))
			if _, err := io.ReadFull(cli, buf); err != nil {
				errCh <- fmt.Errorf("ch%d read: %w", i, err)
				return
			}
			if string(buf) != string(msg) {
				errCh <- fmt.Errorf("ch%d crosstalk: got %q want %q", i, buf, msg)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
}

func TestClientCloseClosesAgent(t *testing.T) {
	eof := make(chan struct{}, 1)
	ln, cancel := startPair(t, echoAgent(func() { eof <- struct{}{} }), nil)
	defer cancel()

	cli, err := ln.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// 1 往復してチャネルを確実に確立してから閉じる。
	msg := []byte("hi")
	_, _ = cli.Write(msg)
	_ = readN(t, cli, len(msg))

	_ = cli.Close() // client 切断 → CLOSE 伝播 → agent 側も閉じるはず
	select {
	case <-eof:
	case <-time.After(3 * time.Second):
		t.Fatal("agent 側 conn が client close 後に閉じられなかった（CLOSE 未伝播）")
	}
}

func TestPipeCloseClosesChannels(t *testing.T) {
	a, b := net.Pipe()
	ln := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ServeListener(ctx, a, ln) }()
	go func() { _ = ServeDialer(ctx, b, echoAgent(nil)) }()

	cli, err := ln.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	msg := []byte("x")
	_, _ = cli.Write(msg)
	_ = readN(t, cli, len(msg))

	_ = a.Close() // relay パイプ切断 → 全チャネル撤去のはず

	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := cli.Read(make([]byte, 1)); err == nil {
		t.Fatal("パイプ切断後も client conn が生存（全チャネル撤去が効いていない）")
	}
}

func TestChunkedPipeReassembles(t *testing.T) {
	wrap := func(c net.Conn) net.Conn { return oneByteConn{c} }
	ln, cancel := startPair(t, echoAgent(nil), wrap)
	defer cancel()

	cli, err := ln.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	// ヘッダ(9B)や payload が 1 byte ずつに割れても io.ReadFull で再組立される。
	msg := []byte("chunked-frame-reassembly-check-0123456789")
	if _, err := cli.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readN(t, cli, len(msg))
	if string(got) != string(msg) {
		t.Fatalf("chunked round-trip mismatch: got %q want %q", got, msg)
	}
}

func TestFrameTooBigTearsDown(t *testing.T) {
	a, b := net.Pipe()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- ServeDialer(ctx, b, echoAgent(nil))
	}()

	// maxFrame 超の length ヘッダを注入（payload 読取前に弾かれるべき）。
	hdr := make([]byte, hdrLen)
	hdr[0] = byte(ftData)
	binary.BigEndian.PutUint32(hdr[1:5], 1)
	binary.BigEndian.PutUint32(hdr[5:9], maxFrame+1)
	go func() { _, _ = a.Write(hdr) }()

	select {
	case err := <-done:
		if !errors.Is(err, errFrameTooBig) {
			t.Fatalf("frame-too-big が弾かれていない: err=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frame-too-big で ServeDialer が撤去されなかった")
	}
}
