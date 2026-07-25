package main

// resume backstop の検証（鉄則: 純関数＋実 herdr。合成で緑にしない）。
//  1. parseResumeUUID: `--resume <ref>` 系の抽出・欠落時の非発火を純関数で
//  2. findClaudePaneByResumeUUID: 実 herdr で pane に agent_session（uuid）を
//     report_agent_session で設定し、exact-match で当該 pane を見つける／別 uuid は
//     見つけない。herdr の claude 検出は実 claude が要るため report_agent_session で
//     その検出値（agent_session）を模す＝合成でなく実 API の往復。

import (
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func TestParseResumeUUID(t *testing.T) {
	const u = "d135c37f-dd76-4ae5-9cd1-58ec5e1793f1"
	const uUpper = "D135C37F-DD76-4AE5-9CD1-58EC5E1793F1"
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"--resume <uuid>", []string{"--resume", u}, u},
		{"--resume=<uuid>", []string{"--resume=" + u}, u},
		{"-r <uuid>", []string{"-r", u}, u},
		{"大文字 uuid も受ける", []string{"--resume", uUpper}, uUpper},
		{"他フラグに挟まれても拾う", []string{"--verbose", "--resume", u, "--foo"}, u},
		{"--resume 単独（対話 picker）は非発火", []string{"--resume"}, ""},
		{"--resume の次がフラグなら非発火", []string{"--resume", "--verbose"}, ""},
		// ⚠v0.5.23 で**値の書式では判定しない**方式に変えた（Spec 駆動）。
		// claude の会話 ref はたまたま uuid だが pi / omp は path も取るので、
		// uuid 判定だと他エージェントで backstop が永久に発火しない。
		// 安全性は「抽出した ref を **live pane の agent_session と exact-match**
		// で照合する」ことが担保する（下の TestResumeBackstopIgnoresBogusRef）＝
		// 出鱈目な ref はどの pane にも一致せず、素通しで新規起動へ落ちる。
		{"-r の次の値は書式に関わらず抽出する", []string{"-r", "report.md"}, "report.md"},
		{"resume と無関係な args は非発火", []string{"--print", "hello"}, ""},
		{"空 args は非発火", nil, ""},
		{"桁数違いも抽出はする（照合side で弾かれる）", []string{"--resume", "d135c37f-dd76"}, "d135c37f-dd76"},
	}
	for _, c := range cases {
		if got := parseResumeUUID(c.args); got != c.want {
			t.Errorf("%s: parseResumeUUID(%v)=%q want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestResumeBackstopFindsPaneByUUID(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)

	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	const uuid = "d135c37f-dd76-4ae5-9cd1-58ec5e1793f1"
	// herdr の claude 検出が設定する agent_session を模して uuid を報告する。
	if err := api.ReportAgentSession(paneID, "herdr:claude", "claude", uuid); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}

	// exact-match で当該 pane を見つける（非同期反映を待つ）。
	waitCond(t, 15*time.Second, "agent_session.value 一致で pane を発見", func() bool {
		p, e := findClaudePaneByResumeUUID(api, uuid)
		return e == nil && p != nil && p.PaneID == paneID &&
			p.AgentSession.Kind == "id" && p.AgentSession.Value == uuid
	})

	// 別 uuid は見つからない（exact-match＝ヒューリスティックな取り違えをしない）。
	p, e := findClaudePaneByResumeUUID(api, "00000000-0000-0000-0000-000000000000")
	if e != nil {
		t.Fatalf("findClaudePaneByResumeUUID(別 uuid): %v", e)
	}
	if p != nil {
		t.Fatalf("別 uuid で pane を誤検出した: %s", p.PaneID)
	}
}

// backstop の安全性は「抽出した ref を live pane の agent_session と exact-match
// で照合する」ことが担保する（値の書式判定ではない）。出鱈目な ref はどの pane
// にも一致せず nil を返し、呼び手は通常の新規起動へ落ちる。
func TestResumeBackstopIgnoresBogusRef(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	cwd := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "c", []string{stub}, cwd)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	const real = "d135c37f-dd76-4ae5-9cd1-58ec5e1793f1"
	if err := api.ReportAgentSession(pane, "herdr:claude", "claude", real); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}

	for _, bogus := range []string{"report.md", "d135c37f-dd76", "", "/etc/passwd"} {
		p, err := findAgentPaneByResumeRef(api, "claude", bogus)
		if err != nil {
			t.Fatalf("findAgentPaneByResumeRef(%q): %v", bogus, err)
		}
		if p != nil {
			t.Errorf("出鱈目な ref %q が pane %s に一致した", bogus, p.PaneID)
		}
	}
	if p, err := findAgentPaneByResumeRef(api, "claude", real); err != nil || p == nil {
		t.Fatalf("実在 ref が一致しない: p=%v err=%v", p, err)
	}
}
