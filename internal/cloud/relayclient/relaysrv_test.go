// relaysrv_test.go は cm internal/cloud/relay/relay.go の **Server 部**を
// テスト専用にコピーしたもの（同一作者＝コピー自由・cm リポジトリは無改変）。
// relayclient の相手方は本番では既デプロイの Cloud Run relay（無改変共有）
// なので、テストは「実際にデプロイされているものと同じコード」を相手に
// ペアリング/転送/takeover semantics を検証する＝合成モックの相手ではない。
// コメント含め cm からそのまま（コピー元: relay.go:24-198）。
package relayclient

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// relayServer は session id ごとに source 1 + viewer 1 を突合し中継する
// （cm relay.Server のコピー。Grant フック nil＝無認可はローカル relay の
// 実挙動と同じ: 「relay の Grant フックが nil なら公開 /session は無認可で
// 通る」を本機ローカル実起動で実証済みの前提を踏襲）。
type relayServer struct {
	mu       sync.Mutex
	sessions map[string]*sess
	Grant    func(ctx context.Context, sid, role string) bool
}

type sess struct {
	source net.Conn
	viewer net.Conn
	// change は slot 変化（接続/置換/解放）の broadcast。変化のたびに
	// close して張り替える。相手待ちの読み手はこれで起きて現況を再評価。
	change chan struct{}
}

func newRelayServer() *relayServer { return &relayServer{sessions: map[string]*sess{}} }

// ServeHTTP は GET /session?sid=<id>&role=source|viewer を WSS 化。
func (s *relayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	role := r.URL.Query().Get("role")
	if sid == "" || (role != "source" && role != "viewer") {
		http.Error(w, "sid と role(source|viewer) が必要", http.StatusBadRequest)
		return
	}
	if s.Grant != nil && !s.Grant(r.Context(), sid, role) {
		http.Error(w, "未認可（grant 無効）", http.StatusForbidden)
		return
	}
	s.accept(w, r, sid, role)
}

func (s *relayServer) accept(w http.ResponseWriter, r *http.Request, sid, role string) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	s.serve(sid, role, nc)
}

// serve は nc を sid の role slot に登録し、**nc の唯一の読み手**として
// 「その瞬間の現役の相手 slot」へ転送する（cm relay 再接続 takeover 修正
// 2026-06-11 のコードそのまま）。
func (s *relayServer) serve(sid, role string, nc net.Conn) {
	s.mu.Lock()
	se := s.sessions[sid]
	if se == nil {
		se = &sess{change: make(chan struct{})}
		s.sessions[sid] = se
	}
	var old net.Conn
	if role == "source" {
		old, se.source = se.source, nc
	} else {
		old, se.viewer = se.viewer, nc
	}
	close(se.change) // slot 変化を相手待ちへ broadcast
	se.change = make(chan struct{})
	s.mu.Unlock()
	if old != nil {
		old.Close() // 置換: 旧 conn の読み手は read error で退出する
	}

	loneTimer := time.AfterFunc(2*time.Minute, func() {
		s.mu.Lock()
		lone := s.sessions[sid] == se && s.isCurrentLocked(se, role, nc) &&
			(role == "source" && se.viewer == nil ||
				role == "viewer" && se.source == nil)
		s.mu.Unlock()
		if lone {
			nc.Close()
		}
	})
	defer loneTimer.Stop()

	buf := make([]byte, 32*1024)
	for {
		n, rerr := nc.Read(buf)
		if n > 0 {
			if !s.writePeer(sid, se, role, nc, buf[:n]) {
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	s.mu.Lock()
	var peer net.Conn
	if s.sessions[sid] == se && s.isCurrentLocked(se, role, nc) {
		if role == "source" {
			peer = se.viewer
		} else {
			peer = se.source
		}
		delete(s.sessions, sid)
		close(se.change)
		se.change = make(chan struct{})
	}
	s.mu.Unlock()
	nc.Close()
	if peer != nil {
		peer.Close()
	}
}

func (s *relayServer) isCurrentLocked(se *sess, role string, nc net.Conn) bool {
	if role == "source" {
		return se.source == nc
	}
	return se.viewer == nc
}

// writePeer は相手 slot の現役 conn へ p を書く。相手不在なら到着を最大
// 2 分待つ（先着待ち semantics）。
func (s *relayServer) writePeer(sid string, se *sess, role string, nc net.Conn, p []byte) bool {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if s.sessions[sid] != se || !s.isCurrentLocked(se, role, nc) {
			s.mu.Unlock()
			return false
		}
		var peer net.Conn
		if role == "source" {
			peer = se.viewer
		} else {
			peer = se.source
		}
		ch := se.change
		s.mu.Unlock()
		if peer != nil {
			_, err := peer.Write(p)
			return err == nil
		}
		select {
		case <-ch:
		case <-deadline.C:
			return false
		}
	}
}
