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
	// backstop は「会話を実行中の **live** pane」だけを返す（BUG-1 の契約）。
	// live agent（agent_status != "unknown"）を状態→session の順で作る（herdr 0.7.4
	// 実測契約: 状態は非公式 source、逆順だと status が unknown のまま）。
	if err := api.ReportAgent(paneID, "test-native", "claude", "idle"); err != nil {
		t.Fatalf("report_agent: %v", err)
	}
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
	// backstop が返すのは live pane のみ（BUG-1）。状態→session の順で live 化する。
	if err := api.ReportAgent(pane, "test-native", "claude", "idle"); err != nil {
		t.Fatalf("report_agent: %v", err)
	}
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

// TestResumeBackstopSkipsZombieAndSelfPane は BUG-1（shim 再入で pane がゾンビ化）
// の回帰テスト。backstop は契約どおり「会話を**実行中の live pane**」だけを返し、
// 次の 2 種を返してはならない:
//
//	(a) 自 pane（HERDR_PANE_ID が指す pane）— その pane 上でシム自身が走っている。
//	    ここへ attach すると自分自身を observe して自己デッドロック＝空 pane 化。
//	(b) live agent の居ないゾンビ pane（agent_status="unknown"）— herdr 再起動で
//	    復元された pane は古い agent_session.value=uuid を保持したまま live claude が
//	    無い。ここへ attach すると同じくゾンビ化する。
//
// 旧実装は agent_session の exact-match だけで判定し (a)(b) を素通ししていた＝
// 本テストは旧コードで FAIL する（zombie/self を返してしまう）。
func TestResumeBackstopSkipsZombieAndSelfPane(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)

	mkPane := func(label string) string {
		ws, err := api.WorkspaceCreate()
		if err != nil {
			t.Fatalf("workspace.create(%s): %v", label, err)
		}
		return ws.RootPane.PaneID
	}
	reportSession := func(pane, uuid, label string) {
		if err := api.ReportAgentSession(pane, "herdr:claude", "claude", uuid); err != nil {
			t.Fatalf("report_agent_session(%s): %v", label, err)
		}
	}
	// report_agent で live agent（agent_status != "unknown"）に見せる。
	// ⚠herdr 0.7.4 実測契約: 状態は**非公式 source**（"test-native"）で、かつ
	// **状態 → session の順**でしか両立しない（逆順だと status が unknown のまま＝
	// restartclaude_test.go reportTestSession の表）。
	reportLive := func(pane, label string) {
		if err := api.ReportAgent(pane, "test-native", "claude", "idle"); err != nil {
			t.Fatalf("report_agent(%s): %v", label, err)
		}
	}

	const uuidZombie = "aaaaaaaa-1111-4222-8333-444444444444"
	const uuidSelf = "bbbbbbbb-1111-4222-8333-444444444444"
	const uuidLive = "cccccccc-1111-4222-8333-444444444444"

	// (b) ゾンビ pane: agent_session は在るが live agent 無し（status=unknown）。
	zombie := mkPane("zombie")
	reportSession(zombie, uuidZombie, "zombie")

	// (a) 自 pane: live だが HERDR_PANE_ID がこの pane を指す＝自己 attach 禁止。
	self := mkPane("self")
	reportLive(self, "self") // 状態 → session の順
	reportSession(self, uuidSelf, "self")
	t.Setenv("HERDR_PANE_ID", self)

	// 対照: 別の live pane はちゃんと返る（backstop 本来の dup 防止は壊さない）。
	live := mkPane("live")
	reportLive(live, "live") // 状態 → session の順
	reportSession(live, uuidLive, "live")

	// 反映待ち。zombie の agent_session と self の live 化まで**確実に**反映させて
	// から否定アサートする（未反映で nil になる偽 PASS を避ける＝旧コードで
	// zombie/self が確実に「見えている」状態を作ってから返さないことを検査）。
	waitCond(t, 15*time.Second, "zombie の agent_session が反映", func() bool {
		p, e := api.PaneGet(zombie)
		return e == nil && p != nil && p.AgentSession.Value == uuidZombie
	})
	waitCond(t, 15*time.Second, "self が live（agent_status!=unknown）に反映", func() bool {
		p, e := api.PaneGet(self)
		return e == nil && p != nil && p.AgentSession.Value == uuidSelf && p.AgentStatus != "unknown"
	})
	waitCond(t, 15*time.Second, "対照 live pane が backstop で見つかる", func() bool {
		p, e := findAgentPaneByResumeRef(api, "claude", uuidLive)
		return e == nil && p != nil && p.PaneID == live
	})

	// (b) live agent の居ないゾンビ pane は返さない。
	if p, err := findAgentPaneByResumeRef(api, "claude", uuidZombie); err != nil {
		t.Fatalf("findAgentPaneByResumeRef(zombie): %v", err)
	} else if p != nil {
		t.Errorf("live agent の居ないゾンビ pane を返した: %s（自己 observe デッドロック＝空 pane 化の原因）", p.PaneID)
	}

	// (a) 自 pane は live でも返さない（HERDR_PANE_ID 一致）。
	if p, err := findAgentPaneByResumeRef(api, "claude", uuidSelf); err != nil {
		t.Fatalf("findAgentPaneByResumeRef(self): %v", err)
	} else if p != nil {
		t.Errorf("自 pane を返した: %s（自分自身を observe して自己デッドロック＝ゾンビ化）", p.PaneID)
	}
}
