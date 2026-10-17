package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// TestWindowsStubAgentIsRealAgent は Windows の偽エージェント機構そのものを実 herdr で
// 検証する（他の実 herdr テストの前提）:
//   - layout.apply で起動でき（sh スクリプトだと os error 193）、
//   - herdr が前景 basename から agent=claude と**検出**し（0.9.x の rename/session
//     報告はこれが無いと黙って効かない）、
//   - 前景プロセスの argv が launch argv のまま（cmd.exe 複製方式だと化ける）、
//   - spec の副作用（起動マーカ追記・stdout）が pane 内で起きる。
func TestWindowsStubAgentIsRealAgent(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	work := t.TempDir()
	marker := filepath.Join(work, "runs")
	stub := writeAgentStub(t, filepath.Join(work, "claude"), "", stubSpec{
		Stdout: "HD_STUB_ALIVE", AppendRun: marker, SleepSec: 300,
	})

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "probe", []string{stub, "--resume", "REF"}, work)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var agent string
	var fps []herdrapi.PaneProcess
	var screen string
	for time.Now().Before(deadline) {
		if p, err := api.PaneGet(pane); err == nil {
			agent = p.Agent
		}
		if pi, err := api.PaneProcessInfo(pane); err == nil && pi != nil {
			fps = pi.ForegroundProcesses
		}
		if r, err := api.PaneRead(pane, "visible"); err == nil && r != nil {
			screen = r.Text
		}
		if agent == "claude" && len(fps) > 0 && strings.Contains(screen, "HD_STUB_ALIVE") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if agent != "claude" {
		t.Fatalf("pane.agent=%q want claude（前景 basename 検出に乗っていない） fps=%+v", agent, fps)
	}
	if !strings.Contains(screen, "HD_STUB_ALIVE") {
		t.Fatalf("stub の stdout が pane に出ない: %q", screen)
	}
	var found bool
	for _, f := range fps {
		if strings.EqualFold(f.Name, "claude.exe") || strings.EqualFold(f.Name, "claude") {
			found = true
			if len(f.Argv) != 3 || f.Argv[1] != "--resume" || f.Argv[2] != "REF" {
				t.Fatalf("前景 argv が launch argv と違う: %q", f.Argv)
			}
		}
	}
	if !found {
		t.Fatalf("前景に claude が居ない: %+v", fps)
	}
	// herdr は pane cwd を前景プロセスの実 cwd から報告する（実測）＝stub は cwd を
	// 動かしてはいけない（cwd 一致で attach するシムのテストが偽陰性になる）。
	if pg, err := api.PaneGet(pane); err != nil {
		t.Fatalf("pane.get: %v", err)
	} else if strings.TrimRight(pg.Cwd, `\`) != strings.TrimRight(work, `\`) {
		t.Fatalf("pane.cwd=%q want %q", pg.Cwd, work)
	}
	b, _ := os.ReadFile(marker)
	if strings.TrimSpace(string(b)) != "run" {
		t.Fatalf("起動マーカ = %q want run", b)
	}
}
