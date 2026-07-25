package main

// 「InstallSpec で解決したバイナリを起動すると、herdr がそのエージェントとして
// 検出するか」を**実測で**担保する（DESIGN_MULTI_AGENT.md §3.4 の最重要トラップ）。
//
// herdr の検出は前景プロセス名基準で、`lookup_agent` の alias 表に載らない
// basename で起動すると **pane.agent も agent_session も一切付かない**。その場合
// resume backstop も organize の検出系統も **silent に無効化**される（エラーも
// 警告も出ない＝気づけない）。だから机上の突き合わせ（ValidateSpecs）だけでなく
// 実プロセスで確かめる。
//
// 実例: codex を Homebrew Cask で入れると `/opt/homebrew/bin/codex` は
// `codex-aarch64-apple-darwin` への symlink になる。実体名は alias 表に無いので
// 「解決先の実体名で起動していたら検出されない」— drover は argv[0] に
// LookPath の結果（symlink パス）をそのまま渡すので検出される、という関係。
//
// 導入されていないエージェントは skip する（CI/他人の環境でも緑）。

import (
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/agentid"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func TestInstalledAgentsAreDetectedByHerdr(t *testing.T) {
	var installed []struct{ agent, bin string }
	for label := range agentid.CanonicalLabels {
		if _, ok := agentid.Install(label); !ok {
			continue // InstallSpec が無い＝そもそも drover が起動しない
		}
		bin, err := lookupAgentBin(label)
		if err != nil {
			continue // このマシンに未導入
		}
		installed = append(installed, struct{ agent, bin string }{label, bin})
	}
	if len(installed) == 0 {
		t.Skip("InstallSpec を持つエージェントがこのマシンに 1 つも導入されていない")
	}

	for _, tc := range installed {
		t.Run(tc.agent, func(t *testing.T) {
			t.Logf("解決したバイナリ: %s", tc.bin)
			sock := startHerdrForTest(t)
			api := herdrapi.New(sock)
			wsID, err := currentWorkspaceID(api)
			if err != nil {
				t.Fatalf("currentWorkspaceID: %v", err)
			}
			pane, err := applyClaudeTab(api, wsID, tc.agent, []string{tc.bin}, t.TempDir())
			if err != nil {
				t.Fatalf("layout.apply: %v", err)
			}

			deadline := time.Now().Add(25 * time.Second)
			var last herdrapi.PaneInfo
			for time.Now().Before(deadline) {
				panes, err := api.PaneList()
				if err != nil {
					t.Fatalf("pane.list: %v", err)
				}
				found := false
				for _, p := range panes {
					if p.PaneID == pane {
						last, found = p, true
					}
				}
				if !found {
					t.Fatalf("pane %s が消滅（%s が即終了した）", pane, tc.agent)
				}
				if last.Agent != "" {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if last.Agent != tc.agent {
				t.Fatalf("herdr の検出値 = %q, want %q"+
					"（この導入方法では resume も organize の検出系統も silent に無効化される。"+
					"InstallSpec.BinNames が herdr の lookup_agent 表と食い違っていないか確認せよ）",
					last.Agent, tc.agent)
			}
			// 起動しただけでは会話が無いので agent_session は空でよい。
			// resume が効くかは会話開始後の別問題（Spec の Kinds を参照）。
			t.Logf("検出 OK: agent=%q agent_session=%+v", last.Agent, last.AgentSession)
		})
	}
}
