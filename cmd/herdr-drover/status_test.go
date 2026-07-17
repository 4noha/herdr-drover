package main

// status の実 herdr 検証（鉄則: 合成で緑にしない。herdr 不在環境は Skip）。
// harness は internal/herdrapi/client_test.go と同じ実測レシピ:
//   - 短い /tmp dir 必須（sun_path 104B 制約＝深い階層は bind 失敗）
//   - XDG_CONFIG_HOME 隔離必須（無いと ~/.config/herdr/session.json を実汚染
//     ＋前回 pane 復元で非決定になる実測事実）
//   - 停止は自分の socket への `herdr server stop` → 自分の spawn した PID の
//     wait/kill のみ。裸の pkill herdr は恒久禁止（他者サーバ殺害の実インシデント）

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// startHerdrForTest は隔離 herdr サーバを起動し socket パスを返す。
// 終了時の graceful stop＋自 PID kill backstop は t.Cleanup で保証。
func startHerdrForTest(t *testing.T) string {
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
	sock := filepath.Join(dir, "h.sock")
	env := append(os.Environ(),
		"HERDR_SOCKET_PATH="+sock,
		"XDG_CONFIG_HOME="+xdg)

	cmd := exec.Command(bin, "server")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start herdr server: %v", err)
	}
	t.Cleanup(func() {
		stop := exec.Command(bin, "server", "stop")
		stop.Env = env
		_ = stop.Run() // 実測 exit 0。失敗しても下の kill backstop が拾う
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill() // 自分が spawn した PID のみ
			<-done
		}
		os.RemoveAll(dir)
	})

	// socket 出現＋ping 応答を待つ（実測 ~1s。余裕 15s）
	c := herdrapi.New(sock)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := c.Ping(); err == nil {
			return sock
		}
		if time.Now().After(deadline) {
			t.Fatalf("herdr server did not become ready at %s", sock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// 実 herdr へ ping する status の主経路（agent 起動前診断のキーパス）。
func TestStatusAgainstRealHerdr(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	t.Setenv("HOME", t.TempDir()) // pidfile 隔離＝実稼働 daemon の状態に依存しない

	var out bytes.Buffer
	code := run([]string{"status"}, &out, &out)
	if code != 0 {
		t.Fatalf("exit=%d\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "herdr  : OK version=") || !strings.Contains(s, "protocol=") {
		t.Fatalf("実 herdr の version/protocol が表示されない:\n%s", s)
	}
	if !strings.Contains(s, "daemon : 停止中") {
		t.Fatalf("隔離 HOME で daemon 停止中にならない:\n%s", s)
	}
	if !strings.Contains(s, "GCP_PROJECT") {
		t.Fatalf("設定充足表示が無い:\n%s", s)
	}
}

// herdr 未接続でも status は exit 0 で全項目を表示し切る（報告であって
// probe ではない設計）。dial 失敗は実 unix socket 不在で起こす。
func TestStatusHerdrUnreachable(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "none.sock"))
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	code := run([]string{"status"}, &out, &out)
	if code != 0 {
		t.Fatalf("exit=%d（NG でも報告は完遂して 0 のはず）\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "herdr  : NG") {
		t.Fatalf("NG 表示が無い:\n%s", s)
	}
	if !strings.Contains(s, "DROVER_TICK") {
		t.Fatalf("NG でも設定表示が続いていない:\n%s", s)
	}
}
