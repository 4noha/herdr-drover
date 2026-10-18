//go:build windows

package main

// 注入 pane の client 復活（BUG-6）の Windows 実 herdr 検証。
//
// 実障害（2026-10-18・この PC）: 再起動でレイアウトが復元されると、↗窓 56 枚が
// **素の PowerShell** で戻り attach client が居なかった。reconcile は client 不在を
// 正しく検知したが、復活経路（injShellNames＋sh の単一引用）が POSIX シェル専用で
// `前景=[powershell.exe]＝触らない` と見送り続け、↗窓 は全滅したままだった。
// ここでは復元後と同じ「pane は在るが前景が既定シェル」を実 herdr 0.9.5 で作り、
// ensureInjectClient が**同じ pane のまま** client を打ち込んで復活させることを見る。

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// winShellPane は前景が shell の pane を作る。argv 空なら workspace の root pane
// （herdr の既定シェル＝復元後の注入 pane と同じ状態）を返す。
func winShellPane(t *testing.T, api *herdrapi.Client, argv []string) string {
	t.Helper()
	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	if len(argv) == 0 {
		return ws.RootPane.PaneID
	}
	type pane struct {
		Type    string   `json:"type"`
		Command []string `json:"command"`
	}
	var out struct {
		Layout struct {
			Root struct {
				PaneID string `json:"pane_id"`
			} `json:"root"`
		} `json:"layout"`
	}
	raw, err := api.Call("layout.apply", struct {
		TabID string `json:"tab_id"`
		Focus bool   `json:"focus"`
		Root  pane   `json:"root"`
	}{ws.Tab.TabID, false, pane{"pane", argv}})
	if err != nil {
		t.Fatalf("layout.apply %v: %v", argv, err)
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Layout.Root.PaneID == "" {
		t.Fatalf("layout.apply decode: %v %s", err, raw)
	}
	return out.Layout.Root.PaneID
}

// waitInjShellReady は前景が既知シェル（＝復活してよい状態）になり、プロンプトが
// 出るまで待つ（PowerShell は起動直後約 1.5 秒の打鍵を落とす＝実測）。
func waitInjShellReady(t *testing.T, api *herdrapi.Client, paneID string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last []herdrapi.PaneProcess
	for time.Now().Before(deadline) {
		if info, err := api.PaneProcessInfo(paneID); err == nil {
			last = info.ForegroundProcesses
			if sh := injForegroundShell(last); sh != "" {
				if r, err := api.PaneRead(paneID, "visible"); err == nil && strings.Contains(r.Text, ">") {
					return sh
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("前景が復活対象のシェルにならない（最後に見た前景: %+v）", last)
	return ""
}

func testReviveWindowsShell(t *testing.T, shellArgv []string, wantShell string) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)

	prev := injLivenessEvery
	injLivenessEvery = 0
	injLivenessAt = map[string]time.Time{}
	t.Cleanup(func() { injLivenessEvery = prev; injLivenessAt = map[string]time.Time{} })

	marker := filepath.Join(t.TempDir(), "runs")
	// 実 attach の代わり: 起動を記録して生存し続ける。名前も実物と同じ herdr-drover.exe。
	// パスに空白と ' を含めて、打ち込むコマンド行の引用が実シェルで壊れないかを見る
	// （ユーザー名に空白のある実機を想定）。writeAgentStub は stubRoot 直下の固定名に
	// 置くので、同じ手順で引用が要る dir に置き直す。
	base := writeAgentStub(t, filepath.Join(t.TempDir(), "herdr-drover"), "",
		stubSpec{AppendRun: marker, SleepSec: 300})
	quotedDir := filepath.Join(filepath.Dir(base), "my 'drover")
	if err := os.MkdirAll(quotedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(quotedDir, filepath.Base(base))
	if err := copySelfExecutable(stub); err != nil {
		t.Fatal(err)
	}
	spec, err := os.ReadFile(stubSpecPathFor(base))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stubSpecPathFor(stub), spec, 0o644); err != nil {
		t.Fatal(err)
	}

	paneID := winShellPane(t, api, shellArgv)
	if sh := waitInjShellReady(t, api, paneID); sh != wantShell {
		t.Fatalf("前景シェル=%q want %q", sh, wantShell)
	}

	var logBuf strings.Builder
	lg := log.New(&logBuf, "", 0)
	ensureInjectClient(api, paneID, stub, "remoteA", "w9:pA", lg, time.Now())
	t.Logf("reconcile log: %s", logBuf.String())
	if !strings.Contains(logBuf.String(), "REVIVE remoteA/w9:pA pane="+paneID) {
		t.Fatalf("REVIVE しなかった: %s", logBuf.String())
	}
	// stub が起動し（＝打ち込んだコマンド行がシェルで正しく解釈された）、同じ pane の
	// 前景に attach client（argv exact）として居ること。
	deadline := time.Now().Add(20 * time.Second)
	var last []herdrapi.PaneProcess
	for time.Now().Before(deadline) {
		if info, err := api.PaneProcessInfo(paneID); err == nil {
			last = info.ForegroundProcesses
			if injClientRunning(last, stub, "remoteA", "w9:pA") {
				// 前景はシェルのまま（Windows の herdr は子の非 agent を前景に出さない）。
				// 2 回目の生存確認で**打ち込み直さない**こと（実行中の attach の stdin へ
				// コマンド行を流し込む thrash の防止）。
				logBuf.Reset()
				ensureInjectClient(api, paneID, stub, "remoteA", "w9:pA", lg, time.Now())
				if strings.Contains(logBuf.String(), "REVIVE") {
					t.Fatalf("client 稼働中なのに再度 REVIVE した: %s", logBuf.String())
				}
				time.Sleep(time.Second)
				if b, _ := os.ReadFile(marker); strings.Count(string(b), "run") != 1 {
					t.Fatalf("stub 起動回数=%q（1 回のはず）", b)
				}
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	r, _ := api.PaneRead(paneID, "visible")
	t.Fatalf("同じ pane で client が復活しない（前景=%+v）\n--- pane ---\n%s", last, r.Text)
}

// TestReconcileRevivesDeadInjectClientWindowsDefaultShell: 復元後と同じ「既定シェル」
// （この機では Windows PowerShell。pwsh があれば pwsh）の pane で復活する。
func TestReconcileRevivesDeadInjectClientWindowsDefaultShell(t *testing.T) {
	want := "powershell.exe"
	if _, err := exec.LookPath("pwsh"); err == nil {
		want = "pwsh.exe" // herdr 0.9.2+ は pwsh があれば既定にする
	}
	testReviveWindowsShell(t, nil, want)
}

// TestReconcileRevivesDeadInjectClientWindowsCmd: default_shell を cmd にした環境。
func TestReconcileRevivesDeadInjectClientWindowsCmd(t *testing.T) {
	testReviveWindowsShell(t, []string{os.Getenv("COMSPEC")}, "cmd.exe")
}
