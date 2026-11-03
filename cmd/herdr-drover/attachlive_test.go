package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakePingConn struct {
	net.Conn
	n    atomic.Int32
	fail int32 // この回数目の ping で失敗（0=失敗しない）
}

func (f *fakePingConn) Ping(context.Context) error {
	if n := f.n.Add(1); f.fail != 0 && n >= f.fail {
		return errors.New("pong なし")
	}
	return nil
}

// TestWatchLivenessCallsOnFailOnce — ping が失敗したら onFail を 1 回だけ呼んで戻る。
// 成功している間は呼ばない。ping できない conn では何もしない。
func TestWatchLivenessCallsOnFailOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &fakePingConn{fail: 3}
	var fails atomic.Int32
	done := make(chan struct{})
	go func() {
		watchLiveness(ctx, c, 20*time.Millisecond, time.Second, func(error) { fails.Add(1) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ping 失敗後も watchLiveness が戻らない")
	}
	if fails.Load() != 1 || c.n.Load() != 3 {
		t.Fatalf("onFail=%d pings=%d（want 1 / 3）", fails.Load(), c.n.Load())
	}

	// ping できない conn: 即戻る（不明は生きている扱い）。
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done2 := make(chan struct{})
	go func() {
		watchLiveness(ctx, a, time.Millisecond, time.Second, func(error) { t.Error("呼ばれてはいけない") })
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatal("ping できない conn で watchLiveness が戻らない")
	}

	// ctx 終了では onFail を呼ばない。
	c2 := &fakePingConn{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done3 := make(chan struct{})
	go func() {
		watchLiveness(ctx2, c2, 10*time.Millisecond, time.Second, func(error) { t.Error("ctx 終了で onFail") })
		close(done3)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel2()
	<-done3
}

// TestWatchLivenessDetectsSilentPeerOnRealWebsocket — 実 WebSocket（drover-cloud
// relayclient.Dial と同じ経路）で、相手が読まない（pong を返さない）と期限内に
// 失敗として検出する。attach の「黙って死んだ relay 接続」の再現。
func TestWatchLivenessDetectsSilentPeerOnRealWebsocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		<-r.Context().Done() // 読まない＝pong を返さない
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	go func() { // 読み手（本番は pumpFrames）
		buf := make([]byte, 1024)
		for {
			if _, err := nc.Read(buf); err != nil {
				return
			}
		}
	}()
	conn := &pingNetConn{Conn: nc, ws: c}
	failed := make(chan error, 1)
	start := time.Now()
	watchLiveness(ctx, conn, 100*time.Millisecond, 700*time.Millisecond, func(err error) { failed <- err })
	select {
	case err := <-failed:
		if time.Since(start) > 5*time.Second {
			t.Fatalf("検出が遅い: %v", time.Since(start))
		}
		t.Logf("検出: %v（%v）", err, time.Since(start).Round(time.Millisecond))
	default:
		t.Fatal("読まない相手を生存と誤認した")
	}
}

// pingNetConn は relayclient.Conn と同じ形（net.Conn＋WS Ping）のテスト用 conn。
type pingNetConn struct {
	net.Conn
	ws *websocket.Conn
}

func (p *pingNetConn) Ping(ctx context.Context) error { return p.ws.Ping(ctx) }
