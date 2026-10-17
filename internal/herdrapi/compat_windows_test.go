//go:build windows

package herdrapi_test

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/bridge"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// Exercise the shipped Windows binary and ConPTY bundle. Keep all config/state
// private, and stop only the process created by this test.
func TestWindowsHerdrCompatibility(t *testing.T) {
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr is not installed")
	}
	root := t.TempDir()
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("HERDR_SESSION", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("APPDATA", filepath.Join(root, "roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	// Rust distinguishes an absent environment override from an empty one.
	if err := os.Unsetenv("HERDR_SOCKET_PATH"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("HERDR_SESSION"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := exec.CommandContext(ctx, bin, "server")
	var serverLog bytes.Buffer
	server.Stdout, server.Stderr = &serverLog, &serverLog
	if err := server.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// graceful stop で pane 内プロセス（cwd が TempDir）ごと終わらせる。kill だと
		// 子が残り TempDir を消せない。自分の隔離 socket 宛て＝他サーバに触れない。
		_ = exec.Command(bin, "server", "stop").Run()
		done := make(chan struct{})
		go func() { _ = server.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cancel() // 自分が spawn した PID のみ
			<-done
		}
		cancel()
		if t.Failed() {
			t.Logf("server: %s", serverLog.String())
		}
	})
	api := herdrapi.New("")
	api.Timeout = 5 * time.Second
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err = api.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ping %s: %v", api.SocketPath, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("connected to %s", api.SocketPath)
	// cmd.exe を claude.exe という名前で複製して起動する（herdr の agent 検出は
	// 前景プロセス名基準＝claude と認識される。MS 署名のまま＝Smart App Control
	// にも弾かれない。新規ビルドの stub exe は SAC にブロックされる）。
	// 0.9.x の agent.start 非互換（kind 必須）→ layout.apply＋検出待ち rename の
	// fallback を、シム本体と同じ経路で通す。出力マーカーは echo 除外のため分割。
	fakeClaude := filepath.Join(root, "claude.exe")
	cmdExe, err := os.ReadFile(os.Getenv("COMSPEC"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeClaude, cmdExe, 0o755); err != nil {
		t.Fatal(err)
	}
	agent, err := api.AgentStart("claude", []string{fakeClaude, "/Q", "/D"}, &herdrapi.AgentStartOptions{Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "claude" || agent.Agent != "claude" || agent.PaneID == "" {
		t.Fatalf("agent.start: %+v", agent)
	}
	t.Cleanup(func() { _ = api.WorkspaceClose(agent.WorkspaceID) })
	captureRawResponses(t, api, agent.PaneID)
	source, viewer := net.Pipe()
	bridgeCtx, stopBridge := context.WithCancel(ctx)
	b := bridge.New(agent.PaneID, source, api)
	b.HerdrBin = bin
	b.Idle = -1
	done := make(chan error, 1)
	go func() { done <- b.Run(bridgeCtx) }()
	t.Cleanup(func() {
		stopBridge()
		_ = viewer.Close()
		_ = source.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	_ = viewer.SetDeadline(time.Now().Add(25 * time.Second))
	if _, err := viewer.Write([]byte{0xff, 0xff, 0, 24, 0, 80}); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 64*1024)
	if n, err := viewer.Read(frame); err != nil || n == 0 {
		t.Fatalf("observe frame: n=%d err=%v", n, err)
	}
	// Drain subsequent frames so rendering cannot block input verification.
	go func() {
		for {
			if _, err := viewer.Read(frame); err != nil {
				return
			}
		}
	}()
	if _, err := viewer.Write([]byte("echo HD_WINDOWS_^COMPAT\r")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		pane, readErr := api.PaneRead(agent.PaneID, "visible")
		if readErr == nil && strings.Contains(pane.Text, "HD_WINDOWS_COMPAT") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("input did not reach pane: %v", readErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("named pipe ping, agent.start, observe frames and bridge input passed")
}

// captureRawResponses は drover が decode する主要 method の生応答を
// HD_HERDR_RAW_DIR へ書き出す（新しい herdr 版の採取用・未設定なら何もしない）。
// 型コメントの「実採取」はここで取った値から写す（合成値で型を直さない）。
func captureRawResponses(t *testing.T, api *herdrapi.Client, paneID string) {
	dir := os.Getenv("HD_HERDR_RAW_DIR")
	if dir == "" {
		return
	}
	pane := struct {
		PaneID string `json:"pane_id"`
	}{paneID}
	for _, c := range []struct {
		method string
		params any
	}{
		{"ping", nil},
		{"pane.list", nil},
		{"pane.get", pane},
		{"agent.list", nil},
		{"workspace.list", nil},
		{"pane.layout", pane},
		{"pane.process_info", pane},
	} {
		raw, err := api.Call(c.method, c.params)
		if err != nil {
			t.Errorf("raw %s: %v", c.method, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, c.method+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
