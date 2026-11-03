//go:build !windows

// agent_clisync_e2e_test.go — daemon 起動時に CLI バイナリ（~/.local/bin/herdr-drover）を
// 稼働バイナリと揃えることの通し試験（実バイナリの agent を実 HOME 配置で起動する）。
// 遠隔 self-update は daemon のバイナリだけを置換するため、再起動後の daemon が CLI 側を
// 揃えないと `claude` シムが旧版のまま残る（2026-11-03 実測）。
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestE2EAgentStartupSyncsCLIBinary(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java 不在のため Firestore emulator 検証不可")
	}
	srv, _ := startHerdr(t)
	bin := buildBinary(t)
	home := t.TempDir()
	// 稼働バイナリ（binDst）＝新版、CLI＝旧版（内容が違う）を実 HOME 配置で用意する。
	binDst := filepath.Join(home, ".herdr-drover", "bin", "herdr-drover")
	cli := filepath.Join(home, ".local", "bin", "herdr-drover")
	newBin, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Dir(binDst), filepath.Dir(cli)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(binDst, newBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("OLD-CLI\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=e2e-clisync-herdr",
		"DROVER_TICK=1s",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
	}
	var out syncBuf
	agent.Stdout, agent.Stderr = &out, &out
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })

	waitFor(t, 20*time.Second, "CLI バイナリが稼働バイナリと同じ内容になる", func() (bool, error) {
		b, err := os.ReadFile(cli)
		if err != nil {
			return false, err
		}
		return bytes.Equal(b, newBin), fmt.Errorf("cli=%dB bin=%dB\nagent log:\n%s", len(b), len(newBin), out.String())
	})
	if !bytes.Contains([]byte(out.String()), []byte("CLI バイナリも同期")) {
		t.Fatalf("同期したことがログに無い:\n%s", out.String())
	}
}
