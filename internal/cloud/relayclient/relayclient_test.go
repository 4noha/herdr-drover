// relayclient_test.go — 実 relay（cm relay.Server のバイト同一コピー＝
// 本番 Cloud Run に載っているのと同じコード）を httptest で実起動し、
// 実 WebSocket 越しに Dial/ペアリング/BridgeSourceIdle の quiescence を
// 機械検証する。合成モック相手の緑にしない（鉄則）。
package relayclient

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startRelay は cm コピーの relay server を実 HTTP サーバで起動し
// ws:// ベース URL を返す。
func startRelay(t *testing.T) string {
	t.Helper()
	rs := newRelayServer()
	mux := http.NewServeMux()
	mux.Handle("/session", rs)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func readWithin(t *testing.T, c net.Conn, want []byte, timeout time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	got := make([]byte, 0, len(want))
	buf := make([]byte, 4096)
	for len(got) < len(want) {
		n, err := c.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err != nil {
			t.Fatalf("read: %v (got %q so far)", err, got)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestDialPairsThroughRealRelay は source/viewer 両役を Dial し、実 relay
// 経由で双方向にバイトが透過することを検証する（ワイヤ契約の実証:
// /session?sid=&role= 形式は relay 側 ServeHTTP の検証を通ることそのもの
// が証拠。pane_id 形式の `:` 入り sid も生のまま通す）。
func TestDialPairsThroughRealRelay(t *testing.T) {
	base := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const sid = "w1:p1" // herdr pane_id 形式（`:` を含む）をそのまま sid に使う
	src, err := DialSource(ctx, base, sid)
	if err != nil {
		t.Fatalf("dial source: %v", err)
	}
	defer src.Close()
	vw, err := Dial(ctx, base, sid, "viewer")
	if err != nil {
		t.Fatalf("dial viewer: %v", err)
	}
	defer vw.Close()

	// source→viewer（frame 方向）
	if _, err := src.Write([]byte("FRAME:hello")); err != nil {
		t.Fatalf("src write: %v", err)
	}
	readWithin(t, vw, []byte("FRAME:hello"), 5*time.Second)

	// viewer→source（入力方向・RESIZE magic を含む生バイト透過）
	in := append([]byte{0xff, 0xff, 0x00, 0x18, 0x00, 0x50}, []byte("keys")...)
	if _, err := vw.Write(in); err != nil {
		t.Fatalf("vw write: %v", err)
	}
	readWithin(t, src, in, 5*time.Second)
}

// TestDialRejectsBadRole は relay の 400 検証（role 必須）がハンドシェイク
// エラーとして client に返ることを確認する＝URL 契約から外れると繋がらない
// ことの実証（契約逸脱の早期検知）。
func TestDialRejectsBadRole(t *testing.T) {
	base := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, base, "sid1", "bogus"); err == nil {
		t.Fatal("role=bogus が接続できてしまった（relay 検証が効いていない）")
	}
}

// pipeEnd は net.Pipe の片端に「双方向 idle テスト用の観測」を足すための
// io.ReadWriteCloser（net.Conn がそのまま満たす）。

// TestBridgeSourceIdleQuiescence は BridgeSourceIdle の本体検証:
//  1. viewer→local・local→viewer の双方向透過
//  2. 片方向でも流れていれば idle 切断しない（bump 意味論）
//  3. 両方向 idle で自切断＝関数が戻り、両端が close されている
func TestBridgeSourceIdleQuiescence(t *testing.T) {
	base := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const sid = "w2:p1"
	const idle = 600 * time.Millisecond

	localA, localB := net.Pipe() // localA=BridgeSourceIdle 側 / localB=擬似 bridge 側
	done := make(chan error, 1)
	go func() {
		done <- BridgeSourceIdle(ctx, base, sid, localA, idle)
	}()

	vw, err := Dial(ctx, base, sid, "viewer")
	if err != nil {
		t.Fatalf("dial viewer: %v", err)
	}
	defer vw.Close()

	// localB の読み手（net.Pipe は無バッファ＝読み手がいないと書き手が
	// 詰まる）。受信を蓄積して照合する。
	var mu sync.Mutex
	var fromViewer []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := localB.Read(buf)
			if n > 0 {
				mu.Lock()
				fromViewer = append(fromViewer, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// 1) viewer→local
	if _, err := vw.Write([]byte("input1")); err != nil {
		t.Fatalf("vw write: %v", err)
	}
	waitCond(t, 5*time.Second, "viewer→local 透過", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Contains(fromViewer, []byte("input1"))
	})
	// local→viewer
	if _, err := localB.Write([]byte("frame1")); err != nil {
		t.Fatalf("localB write: %v", err)
	}
	readWithin(t, vw, []byte("frame1"), 5*time.Second)

	// 2) 片方向のみの通信を idle より長く継続 → まだ切れない
	for i := 0; i < 4; i++ {
		time.Sleep(idle / 2)
		if _, err := localB.Write([]byte("tick")); err != nil {
			t.Fatalf("localB keepalive write: %v", err)
		}
		// viewer 側で読み捨て（詰まり防止）
		_ = vw.SetReadDeadline(time.Now().Add(2 * time.Second))
		bufc := make([]byte, 16)
		if _, err := vw.Read(bufc); err != nil {
			t.Fatalf("vw read keepalive: %v", err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("片方向通信中に idle 切断された（bump 意味論違反）: %v", err)
	default:
	}

	// 3) 完全静止 → idle 以内+α で自切断して戻る。両端 close 済み。
	start := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("BridgeSourceIdle error: %v", err)
		}
	case <-time.After(idle*2 + 3*time.Second):
		t.Fatal("quiescence 自切断が起きない")
	}
	if el := time.Since(start); el < idle/2 {
		t.Fatalf("静止から %s で切断＝早すぎる（idle=%s）", el, idle)
	}
	// local 端も close されている（cm: a.Close(); b.Close()）
	if _, err := localB.Write([]byte("x")); err == nil {
		t.Fatal("quiescence 後も local 端へ書けてしまう（close漏れ）")
	}
}

func waitCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestSourceReconnectTakeover は relay の「同 role 再接続は slot 置換・
// 旧 conn close」semantics を client 視点で確認する（agent 再接続の実運用
// パス。cm relay takeover 修正の regression を client 側からも見張る）。
func TestSourceReconnectTakeover(t *testing.T) {
	base := startRelay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const sid = "w3:p9"
	src1, err := DialSource(ctx, base, sid)
	if err != nil {
		t.Fatalf("dial source1: %v", err)
	}
	defer src1.Close()
	vw, err := Dial(ctx, base, sid, "viewer")
	if err != nil {
		t.Fatalf("dial viewer: %v", err)
	}
	defer vw.Close()
	if _, err := src1.Write([]byte("one")); err != nil {
		t.Fatalf("src1 write: %v", err)
	}
	readWithin(t, vw, []byte("one"), 5*time.Second)

	// source 再接続（takeover）→ 新 source からの frame が viewer へ届く
	src2, err := DialSource(ctx, base, sid)
	if err != nil {
		t.Fatalf("dial source2: %v", err)
	}
	defer src2.Close()
	// 置換 broadcast の伝播を待ってから書く（旧 conn close は非同期）
	time.Sleep(200 * time.Millisecond)
	if _, err := src2.Write([]byte("two")); err != nil {
		t.Fatalf("src2 write: %v", err)
	}
	readWithin(t, vw, []byte("two"), 5*time.Second)

	// 旧 source は close されている（read がエラーで返る）
	_ = src1.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 8)
	if _, err := src1.Read(buf); err == nil {
		t.Fatal("旧 source がまだ生きている（takeover close されていない）")
	}
}
