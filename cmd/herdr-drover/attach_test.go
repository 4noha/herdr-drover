package main

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// timeoutConn は Read が「SetReadDeadline で設定された時刻が来るまで」
// 真にブロックし続ける fake（ネットワーク切断で TCP の Read が応答なく
// ブロックし続ける実障害を模す）。SetReadDeadline が一度も呼ばれなければ
// Read は永久にブロックする＝旧コード（quiescence 監視なし）だとこの conn
// で pumpFrames が never-return することを回帰確認できる。websocket.NetConn
// の「deadline 到達でブロック中の Read も解ける」契約（doc.go 保証）を
// 模した最小実装。
type timeoutConn struct {
	deadlineCh chan time.Time // SetReadDeadline のたび最新値を通知（capacity 1・最新優先）
}

func newTimeoutConn() *timeoutConn {
	return &timeoutConn{deadlineCh: make(chan time.Time, 1)}
}

func (c *timeoutConn) SetReadDeadline(t time.Time) error {
	for {
		select {
		case c.deadlineCh <- t:
			return nil
		default:
			<-c.deadlineCh // 古い値を捨てて最新値で埋め直す
		}
	}
}

func (c *timeoutConn) Read(p []byte) (int, error) {
	deadline := <-c.deadlineCh
	wait := time.Until(deadline)
	if wait < 0 {
		wait = 0
	}
	<-time.After(wait) // 実 conn の「deadline まで真にブロック」を模す
	return 0, errDeadlineExceededFake
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errDeadlineExceededFake = fakeErr("i/o timeout (fake)")

// TestPumpFramesQuiescenceTimeout は「ネットワーク切断で Read がブロックした
// ままでも、idle 超過で pumpFrames が戻る」ことを検証する（実障害の再現：
// attach プロセスへの relay TCP 接続が死んだまま attachOnce が永久に固まり、
// pane close するまで自動復旧しなかった不具合の回帰テスト）。旧コード
// （SetReadDeadline を呼ばない版）はこの conn だと Read が deadline 到達を
// 見ないため never-return し、このテストは timeout する＝旧コードでの
// 落ちを確認済み。
func TestPumpFramesQuiescenceTimeout(t *testing.T) {
	conn := newTimeoutConn()
	var out discardWriter

	done := make(chan struct{})
	go func() {
		pumpFrames(conn, &out, 30*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
		// idle 超過で戻った＝期待どおり。
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames が quiescence idle 超過後も戻らなかった（実障害の再発）")
	}
}

// TestPumpFramesNoTimeoutWhenIdleDisabled は idle<=0 なら SetReadDeadline を
// 一切呼ばない（テスト/無効化経路）ことを確認する。timeoutConn は
// SetReadDeadline が呼ばれないと Read が永久にブロックする（deadlineCh から
// 値が来ない）ため、ここでは代わりに pipeDeadlineConn（close で EOF）を使い、
// 「idle 無効でも Read 自体の終了では正常に戻る」ことを見る。
func TestPumpFramesNoTimeoutWhenIdleDisabled(t *testing.T) {
	pr, pw := io.Pipe()
	conn := &pipeDeadlineConn{r: pr}
	var out discardWriter

	done := make(chan struct{})
	go func() {
		pumpFrames(conn, &out, 0)
		close(done)
	}()

	_ = pw.Close() // Read に即 io.EOF を返させる

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames（idle 無効）が戻らなかった")
	}
	if conn.deadlineCalled.Load() {
		t.Fatal("idle<=0 なのに SetReadDeadline が呼ばれた")
	}
}

// TestPumpFramesForwardsData は受信フレームが out へそのまま転送される
// ことを確認する（quiescence 監視を挟んでも既存の転送動作は不変）。
func TestPumpFramesForwardsData(t *testing.T) {
	pr, pw := io.Pipe()
	conn := &pipeDeadlineConn{r: pr}
	var out captureWriter

	done := make(chan struct{})
	go func() {
		pumpFrames(conn, &out, time.Second)
		close(done)
	}()

	if _, err := pw.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = pw.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames が pipe close 後も戻らなかった")
	}

	if got := out.String(); got != "hello" {
		t.Fatalf("転送データ = %q, want %q", got, "hello")
	}
}

type pipeDeadlineConn struct {
	r              io.Reader
	deadlineCalled atomic.Bool
}

func (c *pipeDeadlineConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *pipeDeadlineConn) SetReadDeadline(time.Time) error {
	c.deadlineCalled.Store(true)
	return nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

type captureWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *captureWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// TestConnHolderWriteTimesOutWhenPeerNotReading は「relay 側（webterm の viewer
// accept）が読まなくなった状態」を net.Pipe（read side を誰も読まない）で模し、
// connHolder.write が無期限ブロックせず inputWriteTimeout で打ち切って戻ることを
// 検証する（実運用フィードバックで繰り返し観測された「TCP は ESTABLISHED のまま
// 何を送っても pane に届かない」症状の回帰テスト。net.Pipe は unbuffered ＝
// 読み手が居ないと Write は即座にブロックするため、relay 側の read 停止を
// 忠実に再現できる）。
func TestConnHolderWriteTimesOutWhenPeerNotReading(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close() // read しない＝write 側を無期限ブロックさせる状況を維持

	h := &connHolder{}
	h.set(client)

	done := make(chan error, 1)
	go func() { done <- h.write([]byte("stuck-input")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("peer が読まないのに write が成功として戻った（timeout が効いていない）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connHolder.write が inputWriteTimeout 後も戻らなかった（無期限ブロックの再発）")
	}
}

// TestConnHolderWriteClosesConnOnTimeout は timeout 後に conn が close され、
// 以後の write 呼出が（再ブロックせず）即座にエラーで返ることを確認する
// （close 済み net.Conn への Write は net.ErrClosed 系で即返るという契約に依存）。
func TestConnHolderWriteClosesConnOnTimeout(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()

	h := &connHolder{}
	h.set(client)

	done := make(chan error, 1)
	go func() { done <- h.write([]byte("first")) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("1 回目の write が戻らなかった")
	}

	// close 済みのはずの conn への 2 回目の write は即座にエラーで返るべき
	// （再ブロックしないことの確認）。
	done2 := make(chan error, 1)
	go func() { done2 <- h.write([]byte("second")) }()
	select {
	case err := <-done2:
		if err == nil {
			t.Fatal("close 済み conn への write が成功として戻った")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2 回目の write が即座に返らなかった（conn が close されていない）")
	}
}
