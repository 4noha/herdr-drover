package herdrapi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============ 実 herdr 隔離サーバ harness（鉄則: 合成で緑にしない） ============
//
// - 短い /tmp dir 必須: sun_path 104B 制約（macOS）＝深い階層だと herdr の
//   socket bind 自体が失敗する（t.TempDir() はテスト名を含み長くなるので不可）。
// - XDG_CONFIG_HOME を隔離 dir に向ける（実測で判明した重要事実）:
//   HERDR_SOCKET_PATH だけの隔離では herdr は **ユーザー共有の
//   ~/.config/herdr/session.json を読み書きする**＝テスト pane が実セッション
//   へ永続汚染される＋前回状態の pane が復元されて pane.list が非決定に
//   なる。XDG 隔離で「起動直後 pane ゼロ」を実測確認済み。
// - 停止は必ず「自分の socket への server stop」→ 自分が spawn した PID の
//   wait/kill のみ。裸の pkill herdr は他エージェント/ユーザーのサーバを
//   殺した実インシデントがあり恒久禁止。

type testServer struct {
	t    *testing.T
	bin  string
	dir  string
	sock string
	env  []string
	cmd  *exec.Cmd
}

// startHerdr は隔離 herdr サーバを起動して Client を返す。herdr が無い
// 環境（CI 等）は Skip。
func startHerdr(t *testing.T) (*testServer, *Client) {
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
	if err := os.MkdirAll(xdg, 0o700); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	s := &testServer{
		t:    t,
		bin:  bin,
		dir:  dir,
		sock: filepath.Join(dir, "h.sock"),
		env: append(os.Environ(),
			"HERDR_SOCKET_PATH="+filepath.Join(dir, "h.sock"),
			"XDG_CONFIG_HOME="+xdg),
	}
	t.Cleanup(func() {
		s.stop()
		os.RemoveAll(dir)
	})
	s.startProcess()
	return s, New(s.sock)
}

// startProcess はサーバプロセスを spawn し ping 応答まで待つ（再起動テスト
// からも使う）。
func (s *testServer) startProcess() {
	s.t.Helper()
	cmd := exec.Command(s.bin, "server")
	cmd.Env = s.env
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start herdr server: %v", err)
	}
	s.cmd = cmd
	// socket 出現＋ping 応答を待つ（実測 ~1s。余裕を見て 15s）。
	c := New(s.sock)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := c.Ping(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			s.stop()
			s.t.Fatalf("herdr server did not become ready at %s", s.sock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop は自分の spawn したサーバだけを止める（graceful stop → 5s 待ち →
// 自 PID kill の backstop。他プロセスには触れない）。
func (s *testServer) stop() {
	if s.cmd == nil {
		return
	}
	stop := exec.Command(s.bin, "server", "stop")
	stop.Env = s.env
	_ = stop.Run() // 実測 exit 0。失敗しても下の kill backstop が拾う

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

// waitFor は poll 汎用（cond が true になるまで interval 刻みで timeout まで）。
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
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s (last err: %v)", what, lastErr)
}

// ============ tests ============

func TestPingRoundTrip(t *testing.T) {
	_, c := startHerdr(t)
	pong, err := c.Ping()
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if pong.Type != "pong" || pong.Version == "" || pong.Protocol <= 0 {
		t.Fatalf("unexpected pong: %+v", pong)
	}
	t.Logf("herdr version=%s protocol=%d capabilities=%v", pong.Version, pong.Protocol, pong.Capabilities)
}

// TestPaneLifecycle は pane 作成 → list → send_text → read 反映 →
// report_metadata 反映を実サーバで往復する（本パッケージの主要経路全部）。
func TestPaneLifecycle(t *testing.T) {
	_, c := startHerdr(t)

	// XDG 隔離済みサーバは起動直後 pane ゼロ（実測）＝決定的な出発点。
	panes, err := c.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	if len(panes) != 0 {
		t.Fatalf("expected isolated server to start with 0 panes, got %d: %+v", len(panes), panes)
	}

	ws, err := c.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	if paneID == "" || ws.Workspace.WorkspaceID == "" {
		t.Fatalf("workspace.create returned empty ids: %+v", ws)
	}

	// list に現れる
	waitFor(t, 5*time.Second, "pane in pane.list", func() (bool, error) {
		panes, err := c.PaneList()
		if err != nil {
			return false, err
		}
		for _, p := range panes {
			if p.PaneID == paneID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pane %s not in list (%d panes)", paneID, len(panes))
	})

	// shell の prompt が描かれるまで待つ（描画前に打つと race するだけなので
	// 先に visible が非空になるのを待つ）。
	waitFor(t, 15*time.Second, "shell prompt", func() (bool, error) {
		r, err := c.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(r.Text) != "", nil
	})

	// send_text → read 反映。マーカーは「打鍵エコー」と「コマンド出力」を
	// 区別できるようクォート分割（HD_M''ARKER_1 とタイプされ、出力は
	// HD_MARKER_1）＝ echo された入力行への誤マッチを構造的に排除。
	if err := c.PaneSendText(paneID, "echo HD_M''ARKER_1\r"); err != nil {
		t.Fatalf("pane.send_text: %v", err)
	}
	waitFor(t, 15*time.Second, "marker output in pane.read", func() (bool, error) {
		r, err := c.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(r.Text, "HD_MARKER_1"), nil
	})

	// report_metadata（drover の identity 符号化経路）→ pane.get 反映
	err = c.PaneReportMetadata(paneID, "drover-test", ReportMetadata{
		Title:  "HD-TITLE",
		Tokens: map[string]string{"pc": "test-pc", "sid": paneID},
	})
	if err != nil {
		t.Fatalf("pane.report_metadata: %v", err)
	}
	waitFor(t, 5*time.Second, "metadata visible in pane.get", func() (bool, error) {
		p, err := c.PaneGet(paneID)
		if err != nil {
			return false, err
		}
		ok := p.Title == "HD-TITLE" && p.Tokens["pc"] == "test-pc" && p.Tokens["sid"] == paneID
		if !ok {
			return false, fmt.Errorf("pane.get: title=%q tokens=%v", p.Title, p.Tokens)
		}
		return true, nil
	})

	// metadata なしはローカルで弾く（サーバは invalid_metadata_request を
	// 返す＝実測。往復せずに済む分岐の確認）。
	if err := c.PaneReportMetadata(paneID, "drover-test", ReportMetadata{}); err == nil {
		t.Fatalf("empty ReportMetadata should error")
	}

	if err := c.WorkspaceClose(ws.Workspace.WorkspaceID); err != nil {
		t.Fatalf("workspace.close: %v", err)
	}
}

// TestAgentStart は agent.start → agent.list 反映（reconcile の注入経路）。
func TestAgentStart(t *testing.T) {
	_, c := startHerdr(t)

	ag, err := c.AgentStart("hdapitest", []string{"/bin/sleep", "30"}, &AgentStartOptions{Cwd: "/private/tmp"})
	if err != nil {
		t.Fatalf("agent.start: %v", err)
	}
	if ag.Name != "hdapitest" || ag.PaneID == "" || ag.TerminalID == "" {
		t.Fatalf("unexpected agent: %+v", ag)
	}
	// cwd は起動 pane に反映される（実測済の受理フィールド）
	if ag.Cwd != "/private/tmp" {
		t.Fatalf("agent cwd not applied: %+v", ag)
	}

	waitFor(t, 5*time.Second, "agent in agent.list", func() (bool, error) {
		agents, err := c.AgentList()
		if err != nil {
			return false, err
		}
		for _, a := range agents {
			if a.Name == "hdapitest" && a.PaneID == ag.PaneID {
				return true, nil
			}
		}
		return false, fmt.Errorf("agent not in list (%d agents)", len(agents))
	})
}

// TestErrorMapping は {"error":{code,message}} が *APIError に exact-match で
// 落ちること（ヒューリスティックでない分岐の土台）。
func TestErrorMapping(t *testing.T) {
	_, c := startHerdr(t)

	_, err := c.PaneGet("w99:p9")
	if err == nil {
		t.Fatalf("expected error for missing pane")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	// 実採取: {"code":"pane_not_found","message":"pane w99:p9 not found"}
	if apiErr.Code != "pane_not_found" {
		t.Fatalf("unexpected code %q (msg=%q)", apiErr.Code, apiErr.Message)
	}
}

// TestSocketPathResolution は解決順（明示 > env > 既定）の機械確認。
func TestSocketPathResolution(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/env.sock")
	if got := ResolveSocketPath("/tmp/explicit.sock"); got != "/tmp/explicit.sock" {
		t.Fatalf("explicit should win: %q", got)
	}
	if got := ResolveSocketPath(""); got != "/tmp/env.sock" {
		t.Fatalf("env should win over default: %q", got)
	}
	t.Setenv("HERDR_SOCKET_PATH", "")
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".config", "herdr", "herdr.sock")
	if got := ResolveSocketPath(""); got != want {
		t.Fatalf("default: got %q want %q", got, want)
	}
}
