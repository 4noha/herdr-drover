//go:build unix

package main

// 被テスト対象 attach.go が //go:build unix（Windows は attach viewer 非対応＝
// platform_windows.go のスタブ）のため、本テストも unix 限定。

import (
	"context"
	"fmt"
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

// TestDialWithTimeoutReturnsOnSlowDial は「dial がネットワーク不通で応答なく
// ブロックし続けても、dialWithTimeout が timeout で確実に戻る」ことを検証する
// （実障害の回帰テスト: Wi-Fi 切替後 relayclient.DialViewerFrom が長時間
// ブロックし得た事象への対処）。dial 関数自体は cancelDialCtx が呼ばれるまで
// 戻らない fake で、無期限ブロックを模す。
func TestDialWithTimeoutReturnsOnSlowDial(t *testing.T) {
	cancelled := make(chan struct{})
	cancelDialCtx := func() { close(cancelled) }
	dial := func() (net.Conn, error) {
		<-cancelled // cancelDialCtx が呼ばれるまで戻らない＝無期限ブロックの模擬
		return nil, fmt.Errorf("dial canceled")
	}

	done := make(chan error, 1)
	go func() {
		_, err := dialWithTimeout(30*time.Millisecond, cancelDialCtx, dial)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("timeout のはずが nil error で戻った")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dialWithTimeout が timeout 後も戻らなかった（無期限ブロックの再発）")
	}
}

// TestDialWithTimeoutReturnsFastSuccess は「dial が timeout 内に成功すれば、
// その conn がそのまま返る（不要に待たされない）」ことを検証する。
func TestDialWithTimeoutReturnsFastSuccess(t *testing.T) {
	client, peer := net.Pipe()
	defer peer.Close()
	dial := func() (net.Conn, error) { return client, nil }

	conn, err := dialWithTimeout(time.Second, func() {}, dial)
	if err != nil {
		t.Fatalf("速い dial が失敗扱いになった: %v", err)
	}
	if conn != client {
		t.Fatal("dial が返した conn がそのまま返っていない")
	}
}

// closableConn は net.Conn の最小サブセットを満たしつつ Close 呼出を観測できる
// fake。connHolder.forceClose のテスト用（実 net.Pipe だと defer close ハンド
// リングと閉じ合わせが煩雑なので shim を用意する）。
type closableConn struct {
	closed atomic.Bool
}

func (c *closableConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *closableConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *closableConn) Close() error                     { c.closed.Store(true); return nil }
func (c *closableConn) LocalAddr() net.Addr              { return nil }
func (c *closableConn) RemoteAddr() net.Addr             { return nil }
func (c *closableConn) SetDeadline(time.Time) error      { return nil }
func (c *closableConn) SetReadDeadline(time.Time) error  { return nil }
func (c *closableConn) SetWriteDeadline(time.Time) error { return nil }

// TestConnHolderForceCloseClosesCurrentConn は forceClose が現接続を Close し、
// 参照を nil に落として以後の write が破棄扱い（nil ガード）になることを検証。
// 旧コード（forceClose 未実装）ではコンパイル自体通らないので、DESIGN 鉄則の
// 「修正前に旧コードでテストが落ちる」は build-time で担保される。
func TestConnHolderForceCloseClosesCurrentConn(t *testing.T) {
	c := &closableConn{}
	h := &connHolder{}
	h.set(c)

	h.forceClose()

	if !c.closed.Load() {
		t.Fatal("forceClose が現 conn を Close していない")
	}
	// 参照 nil 化: 以後の write は nil ガードで no-op を返すべき
	if err := h.write([]byte("dropped")); err != nil {
		t.Fatalf("forceClose 後の write が nil error で返らなかった: %v", err)
	}
}

// TestConnHolderForceCloseNilSafe は c==nil（未接続中＝backoff sleep 中）の
// forceClose 呼出が panic せず no-op なことを確認する。
func TestConnHolderForceCloseNilSafe(t *testing.T) {
	h := &connHolder{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil conn への forceClose で panic: %v", r)
		}
	}()
	h.forceClose()
}

// TestWatchLifecycleFiresOnClockJump は wall clock が >15s ジャンプした（=
// スリープ復帰した）時に forceClose と wakeCh 送信が発火することを検証する。
// fake now/tickCh を注入し実 sleep なしで数十 ms で判定する。baselineReady で
// goroutine start と nowVal.Store の race を排除（敵対的レビュー指摘への対処）。
// 旧コード（watchLifecycle 未実装）ではコンパイルが通らない＝build-time で回帰確認。
func TestWatchLifecycleFiresOnClockJump(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := &closableConn{}
	h := &connHolder{}
	h.set(conn)
	wake := make(chan struct{}, 1)
	tick := make(chan time.Time, 1)
	ready := make(chan struct{})

	base := time.Unix(1_700_000_000, 0)
	var nowVal atomic.Int64
	nowVal.Store(base.UnixNano())
	nowFn := func() time.Time { return time.Unix(0, nowVal.Load()) }
	fpFn := func() string { return "192.168.1.10/24" }

	done := make(chan struct{})
	go func() {
		watchLifecycle(ctx, h, wake, nowFn, fpFn, tick, nil, ready)
		close(done)
	}()

	<-ready // baseline 取得完了を待つ＝この後の nowVal.Store は必ず baseline より後

	// tick 1 (baseline 後の最初): 変化なし → 発火しない
	tick <- base.Add(3 * time.Second)
	if drained := drainOne(wake, 50*time.Millisecond); drained {
		t.Fatal("変化ないのに wakeCh が発火した")
	}
	if conn.closed.Load() {
		t.Fatal("変化ないのに forceClose された")
	}

	// tick 2: 壁時計を 20s 進めた（=スリープ復帰） → 発火するはず
	nowVal.Store(base.Add(23 * time.Second).UnixNano())
	tick <- base.Add(23 * time.Second)

	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("clock jump 検知で wakeCh に送信されなかった")
	}
	if !conn.closed.Load() {
		t.Fatal("clock jump 検知で forceClose されなかった")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchLifecycle が ctx cancel 後も終了しなかった")
	}
}

// TestWatchLifecycleFiresOnNICChange は NIC fingerprint が変わった時に発火する
// ことを検証する。壁時計は動かさない。
func TestWatchLifecycleFiresOnNICChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := &closableConn{}
	h := &connHolder{}
	h.set(conn)
	wake := make(chan struct{}, 1)
	tick := make(chan time.Time, 1)
	ready := make(chan struct{})

	base := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return base }
	var fp atomic.Value
	fp.Store("10.0.0.5/24")
	fpFn := func() string { return fp.Load().(string) }

	done := make(chan struct{})
	go func() {
		watchLifecycle(ctx, h, wake, nowFn, fpFn, tick, nil, ready)
		close(done)
	}()
	<-ready

	// tick 1: 変化なし
	tick <- base
	if drainOne(wake, 50*time.Millisecond) {
		t.Fatal("変化ないのに発火した")
	}

	// NIC 変化を注入 → tick 2 で検知
	fp.Store("10.0.0.99/24,192.168.5.42/24")
	tick <- base
	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("NIC 変化検知で wakeCh に送信されなかった")
	}
	if !conn.closed.Load() {
		t.Fatal("NIC 変化検知で forceClose されなかった")
	}

	cancel()
	<-done
}

// TestWatchLifecycleCooldownSuppressesDoubleFire は cooldown 期間中の追加変化が
// 発火を再誘発しないことを検証する（Wi-Fi 切替の「旧→空→新」2 段遷移、
// wall clock jump 直後の NIC associate 連鎖の抑制）。
func TestWatchLifecycleCooldownSuppressesDoubleFire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := &closableConn{}
	h := &connHolder{}
	h.set(conn)
	wake := make(chan struct{}, 2) // 誤発火を捕らえられるよう capacity 2
	tick := make(chan time.Time, 1)
	ready := make(chan struct{})

	base := time.Unix(1_700_000_000, 0)
	var nowVal atomic.Int64
	nowVal.Store(base.UnixNano())
	nowFn := func() time.Time { return time.Unix(0, nowVal.Load()) }
	var fp atomic.Value
	fp.Store("a")
	fpFn := func() string { return fp.Load().(string) }

	done := make(chan struct{})
	go func() {
		watchLifecycle(ctx, h, wake, nowFn, fpFn, tick, nil, ready)
		close(done)
	}()
	<-ready

	// baseline は既に確立済（ready で同期完了）。tick 1 は変化なし
	tick <- base
	if drainOne(wake, 50*time.Millisecond) {
		t.Fatal("baseline tick で発火してしまった")
	}

	// 1 回目の NIC 変化 → 発火
	fp.Store("b")
	nowVal.Store(base.Add(3 * time.Second).UnixNano())
	tick <- base.Add(3 * time.Second)
	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("1 回目の変化で発火しなかった")
	}

	// cooldown 内（10s 後）の追加変化 → 発火しない
	fp.Store("c")
	nowVal.Store(base.Add(13 * time.Second).UnixNano())
	tick <- base.Add(13 * time.Second)
	if drainOne(wake, 100*time.Millisecond) {
		t.Fatal("cooldown 中の変化で再発火した（2 段遷移抑制の失敗）")
	}

	// cooldown 経過後（40s 後）に更に変化 → 発火
	fp.Store("d")
	nowVal.Store(base.Add(40 * time.Second).UnixNano())
	tick <- base.Add(40 * time.Second)
	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("cooldown 経過後の変化で発火しなかった")
	}

	cancel()
	<-done
}

// TestWatchLifecycleAbsorbsTransientEmptyFingerprint は fp が中間 tick で ""
// を返した後に真の NIC 変化があった場合、それを吸収せず検知することを確認する
// （敵対的レビュー指摘: "a" → "" → "b" が silent に消える回帰への回帰テスト）。
func TestWatchLifecycleAbsorbsTransientEmptyFingerprint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := &closableConn{}
	h := &connHolder{}
	h.set(conn)
	wake := make(chan struct{}, 1)
	tick := make(chan time.Time, 1)
	ready := make(chan struct{})

	base := time.Unix(1_700_000_000, 0)
	nowFn := func() time.Time { return base }
	var fp atomic.Value
	fp.Store("192.168.1.5/24")
	fpFn := func() string { return fp.Load().(string) }

	done := make(chan struct{})
	go func() {
		watchLifecycle(ctx, h, wake, nowFn, fpFn, tick, nil, ready)
		close(done)
	}()
	<-ready

	// tick 1: 中間で fp が空を返す（net.InterfaceAddrs 一時失敗）
	fp.Store("")
	tick <- base
	if drainOne(wake, 50*time.Millisecond) {
		t.Fatal("transient 空で誤発火した")
	}

	// tick 2: 真の NIC 変化（Wi-Fi 切替） → 発火するはず
	fp.Store("192.168.2.10/24")
	tick <- base
	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("transient 空の後の実 NIC 変化が silent に吸収された（regression）")
	}

	cancel()
	<-done
}

// drainOne は wake チャネルから 1 個取り出せたら true。timeout 内に何も来なければ false。
// テスト用のポーリング helper。
func drainOne(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestDialWithTimeoutClosesLateSuccess は「timeout 後に dial が遅れて成功した
// 場合、その conn は呼び手に返らず即 close される」ことを検証する（リーク防止・
// 呼び手がもう待っていない conn を握り続けないことの確認）。
func TestDialWithTimeoutClosesLateSuccess(t *testing.T) {
	client, peer := net.Pipe()
	dialReturn := make(chan struct{})
	dial := func() (net.Conn, error) {
		<-dialReturn // timeout 発火後にこちらを進める
		return client, nil
	}

	_, err := dialWithTimeout(20*time.Millisecond, func() {}, dial)
	if err == nil {
		t.Fatal("timeout のはずが成功扱いになった")
	}
	close(dialReturn) // 遅れて dial を成功させる

	// client 側が close されれば peer 側の Read は io.EOF で返る（close 済みの
	// 証拠）。close されていなければ Read は無期限ブロックするので timeout で判定。
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, rerr := peer.Read(buf)
		readDone <- rerr
	}()
	select {
	case rerr := <-readDone:
		if rerr == nil {
			t.Fatal("peer の Read が io.EOF 等のエラーなしで返った（client が close されていない）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout 後に成功した conn が close されなかった（リーク）")
	}
}

// wall clock が**後退**しても以後の検知が止まらないこと。
//
// cooldownUntil は旧時計基準の絶対時刻なので、NTP の大幅補正や VM スナップショット
// 復元で時計が戻ると未来に取り残され、その差が消えるまで（最悪 cooldown 幅ぶん）
// スリープ復帰も NIC 変化も検知できなくなる。後退を見たら基準を捨てる実装で塞ぐ。
// 後退補正が無い旧コードでは 2 回目の NIC 変化が発火せず FAIL する。
func TestWatchLifecycleRecoversFromBackwardClockJump(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &connHolder{}
	h.set(&closableConn{})
	wake := make(chan struct{}, 2)
	tick := make(chan time.Time, 1)
	ready := make(chan struct{})

	base := time.Unix(1_700_000_000, 0)
	var nowVal atomic.Int64
	nowVal.Store(base.UnixNano())
	nowFn := func() time.Time { return time.Unix(0, nowVal.Load()) }
	var fp atomic.Value
	fp.Store("a")
	fpFn := func() string { return fp.Load().(string) }

	done := make(chan struct{})
	go func() {
		watchLifecycle(ctx, h, wake, nowFn, fpFn, tick, nil, ready)
		close(done)
	}()
	<-ready

	// 1) NIC 変化で発火 → cooldownUntil = base+3s+30s に入る
	fp.Store("b")
	nowVal.Store(base.Add(3 * time.Second).UnixNano())
	tick <- base.Add(3 * time.Second)
	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("1 回目の NIC 変化で発火しなかった")
	}

	// 2) wall clock が 1 時間**後退**（NTP 大幅補正相当）。この時点で
	//    cooldownUntil は新しい now から見て遥か未来に取り残されている。
	back := base.Add(-1 * time.Hour)
	nowVal.Store(back.UnixNano())
	tick <- back
	drainOne(wake, 50*time.Millisecond) // 後退自体では発火しない想定（するなら吸収）

	// 3) 後退後に本物の NIC 変化。cooldown を捨てていれば発火する。
	//    捨てていない旧コードでは now(=back+3s) < cooldownUntil(=base+33s) のため
	//    抑止され、発火しない＝FAIL。
	fp.Store("c")
	nowVal.Store(back.Add(3 * time.Second).UnixNano())
	tick <- back.Add(3 * time.Second)
	if !drainOne(wake, 500*time.Millisecond) {
		t.Fatal("wall clock 後退のあと NIC 変化を検知できていない（cooldown が未来に取り残されている）")
	}

	cancel()
	<-done
}
