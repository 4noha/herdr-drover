package main

// identity 判定（internal/agentid）を **cmd の各サブシステムから使ったときの**
// 統合テスト。純粋な優先順位・decode の規則そのものは internal/agentid の
// テストが持つ。ここで見るのは「shim / restart・update / organize が同じ
// 規則で判定するか」＝サブシステム間の非対称の再発防止。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func injTok() map[string]string {
	return map[string]string{herdrapi.InjTokenPC: "other-herdr", herdrapi.InjTokenSID: "w1:p9"}
}

// 3 サブシステムが**同じ規則**で判定すること（非対称の再発防止）。
// 従来は organize だけが検出値を見ており、restart/update は取りこぼしていた。
func TestIdentityIsConsistentAcrossSubsystems(t *testing.T) {
	cases := []struct {
		name     string
		agent    herdrapi.AgentInfo
		isClaude bool
	}{
		{"シム命名", herdrapi.AgentInfo{PaneID: "w1:p1", Name: "claude"}, true},
		{"検出のみ", herdrapi.AgentInfo{PaneID: "w1:p2", Agent: "claude"}, true},
		{"注入 pane", herdrapi.AgentInfo{PaneID: "w1:p3", Agent: "claude", Tokens: injTok()}, false},
		{"別 agent", herdrapi.AgentInfo{PaneID: "w1:p4", Agent: "codex"}, false},
		{"無関係", herdrapi.AgentInfo{PaneID: "w1:p5", Name: "hdprobe"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// restart/update 経路。**--agent claude で絞る**（P4 で --agent 空は
			// 「全エージェント種別」の意味になったため、claude 限定の比較には
			// 明示的な絞り込みが要る）。
			targets, _, err := selectRestartTargets([]herdrapi.AgentInfo{c.agent}, "", "claude")
			if err != nil {
				t.Fatalf("selectRestartTargets: %v", err)
			}
			gotRestart := len(targets) == 1

			// organize 経路（同じ pane を PaneInfo として渡す）
			p := herdrapi.PaneInfo{PaneID: c.agent.PaneID, Agent: c.agent.Agent, Tokens: c.agent.Tokens}
			names := map[string]string{c.agent.PaneID: c.agent.Name}
			gotOrganize, conflict := classifyClaudePane(p, names)
			if conflict != "" {
				t.Fatalf("想定外の conflict: %s", conflict)
			}

			if gotRestart != c.isClaude || gotOrganize != c.isClaude {
				t.Fatalf("判定が食い違う: restart=%v organize=%v want=%v",
					gotRestart, gotOrganize, c.isClaude)
			}
		})
	}
}

// --agent 空は「その PC の全エージェント種別」。claude 以外も対象になる
// （P4 の一般化。以前は claude しか拾えなかった）。
func TestSelectRestartTargetsAgentFilter(t *testing.T) {
	agents := []herdrapi.AgentInfo{
		{PaneID: "w1:p1", Name: "claude"},
		{PaneID: "w1:p2", Agent: "codex"},
		{PaneID: "w1:p3", Agent: "gemini"}, // resume 非対応でも対象にはなる
	}
	all, _, err := selectRestartTargets(agents, "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("--agent 空 = 全種別のはずが %d 件: %+v", len(all), all)
	}
	only, _, err := selectRestartTargets(agents, "", "codex")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(only) != 1 || only[0].PaneID != "w1:p2" || only[0].AgentKind != "codex" {
		t.Fatalf("--agent codex = %+v（w1:p2 の 1 件のはず）", only)
	}
	// sid 指定は種別に関わらずその 1 枚（sid が優先。--agent との AND にしない
	// ＝「この pane を再起動」という明示指定を種別で撥ねるのは分かりにくい）。
	one, _, err := selectRestartTargets(agents, "w1:p3", "claude")
	if err != nil {
		t.Fatalf("sid 指定 + 別 --agent: %v", err)
	}
	if len(one) != 1 || one[0].PaneID != "w1:p3" {
		t.Fatalf("sid 指定 = %+v", one)
	}
}

// 矛盾は黙って落とさず報告される（silent skip 禁止）。
func TestSelectRestartTargetsReportsConflict(t *testing.T) {
	agents := []herdrapi.AgentInfo{
		{PaneID: "w1:p1", Name: "claude", Agent: "codex"}, // 命名と検出が矛盾
		{PaneID: "w1:p2", Name: "claude-2"},               // 正常
	}
	targets, conflicts, err := selectRestartTargets(agents, "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(targets) != 1 || targets[0].PaneID != "w1:p2" {
		t.Fatalf("対象 = %+v（w1:p2 の 1 件のはず）", targets)
	}
	if len(conflicts) != 1 || !strings.Contains(conflicts[0], "w1:p1") {
		t.Fatalf("矛盾が報告されていない: %v", conflicts)
	}
}

// 未命名 pane が正規の対象になったので、要約は pane を特定できる文字列にする
// （遠隔 Ack として Firestore に残る監査記録＝空文字だと復元不能）。
func TestSummarizeRestartIdentifiesUnnamedPanes(t *testing.T) {
	got := summarizeRestart([]restartOutcome{
		{PaneID: "w1:p4", Name: "", Status: "done"},
		{PaneID: "w2:p1", Name: "", Status: "done"},
	})
	if strings.Contains(got, ": ,") || !strings.Contains(got, "w1:p4") || !strings.Contains(got, "w2:p1") {
		t.Fatalf("未命名 pane を特定できない要約: %q", got)
	}
}

// argv ゲートは **対象 pane の種別**で判定しなければならない。
//
// 実バグ（実 codex の e2e で発覚・v0.5.24 で修正）: 条件が `"claude"` に
// ハードコードされていたため、codex / cursor の pane は argv[0] が正しくても
// 必ず skip されていた。**エラーメッセージだけ t.AgentKind を出す**ので
// 「codex の直接起動でない（argv[0]="/opt/homebrew/bin/codex"）」という
// 矛盾した文言になり、一見正しく見えた。単体テストも dry-run も claude 経路
// しか通らず気づけなかった＝**種別を変えた経路を必ず 1 本通す**。
func TestArgvGateUsesTargetAgentKind(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	// 実体名が codex の stub（argv ゲートは basename を見る）。
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := applyClaudeTab(api, wsID, "codex", []string{stub}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := api.ReportAgent(pane, "test-native", "codex", "idle"); err != nil {
		t.Fatal(err)
	}
	// 実運用の pane は integration hook が会話 ref を報告している。安全網
	// （ref が取れない pane は既定で触らない）に引っかからないよう実態に合わせる。
	if err := api.ReportAgentSession(pane, "herdr:codex", "codex",
		"019f9964-f4af-7a72-a999-7dc3f64c521e"); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: pane, Agent: "codex"}, &log)
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, log.String())
	}
	if len(results) != 1 {
		t.Fatalf("results=%+v", results)
	}
	if results[0].Status == "skip" && strings.Contains(results[0].Detail, "直接起動でない") {
		t.Fatalf("argv[0] が codex なのに argv ゲートで skip された"+
			"（ゲートが種別を固定していないか確認せよ）: %s", results[0].Detail)
	}
	if results[0].Status != "done" {
		t.Fatalf("再起動できていない: %+v\nlog=%s", results[0], log.String())
	}
}

// **会話 ref が取れない pane は既定で触らない**（安全網）。
//
// 作り直せば新バイナリは掴むが resume 引数を付けられず、**会話が失われたまま
// status=done で成功に見える**。wrapper 起動 pane で潰したのと同じ型の失敗。
// 「ref が無い」は「まだ発話していない」と「integration 未設置」を区別できない
// ので安全側に倒す。--force で明示的に上書きできる。
func TestRestartSkipsPanesWithoutResumeRef(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	dir := t.TempDir()
	stub := filepath.Join(dir, "codex")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := applyClaudeTab(api, wsID, "codex", []string{stub}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 検出はされるが agent_session は無い状態（＝integration 未設置 or 未発話）。
	if err := api.ReportAgent(pane, "test-native", "codex", "idle"); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: pane, Agent: "codex"}, &log)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if len(results) != 1 || results[0].Status != "skip" {
		t.Fatalf("既定で skip されない（会話を失う）: %+v\nlog=%s", results, log.String())
	}
	if !strings.Contains(results[0].Detail, "会話 ref") {
		t.Fatalf("理由が不明瞭: %q", results[0].Detail)
	}
	if paneGone(api, pane) {
		t.Fatal("skip のはずが pane が作り直された")
	}

	// --force なら明示的に上書きできる。
	var log2 bytes.Buffer
	results, err = restartClaudePanes(api, restartOptions{SID: pane, Agent: "codex", Force: true}, &log2)
	if err != nil {
		t.Fatalf("restart --force: %v\n%s", err, log2.String())
	}
	if len(results) != 1 || results[0].Status != "done" {
		t.Fatalf("--force でも再起動されない: %+v\nlog=%s", results, log2.String())
	}
}

// resume 非対応の 7 種は ref が無いのが恒常状態なので、この安全網の対象外。
// 弾くと永久に再起動できなくなる。
func TestRestartAllowsResumeIncapableAgents(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	dir := t.TempDir()
	stub := filepath.Join(dir, "gemini")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := applyClaudeTab(api, wsID, "gemini", []string{stub}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := api.ReportAgent(pane, "test-native", "gemini", "idle"); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: pane, Agent: "gemini"}, &log)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if len(results) != 1 || results[0].Status != "done" {
		t.Fatalf("resume 非対応が再起動できない（恒常的に ref が無いだけ）: %+v\nlog=%s",
			results, log.String())
	}
}
