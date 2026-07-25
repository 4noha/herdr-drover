package main

// restart-claude の回帰テスト。純関数はテーブル、経路は**実 herdr**（隔離
// HERDR_SOCKET_PATH・短い /tmp パス）で担保する（合成ストリームだけで緑にしない）。
//
// ⚠実バイナリの解決に PATH を使わないことが本機能の肝（daemon の PATH には
// ~/.local/bin が入らない）。よって e2e の stub は **PATH に置かず絶対パスだけ**を
// argv[0] に渡す＝PATH 依存が混入したらこのテストが落ちる。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func TestRebuildResumeArgv(t *testing.T) {
	const uuid = "ebdd35b1-f1b6-4e90-8395-c9ff09e00d32"
	const other = "4a94103f-056e-4578-8578-33ede47e6195"
	for _, tc := range []struct {
		name string
		argv []string
		uuid string
		want []string
	}{
		{
			name: "resume 無し argv へ付与",
			argv: []string{"/p/claude"},
			uuid: uuid,
			want: []string{"/p/claude", "--resume", uuid},
		},
		{
			name: "既存 --resume は現在の会話 uuid へ張替え",
			argv: []string{"/p/claude", "--resume", other},
			uuid: uuid,
			want: []string{"/p/claude", "--resume", uuid},
		},
		{
			name: "--resume=uuid 形も張替え",
			argv: []string{"/p/claude", "--resume=" + other},
			uuid: uuid,
			want: []string{"/p/claude", "--resume", uuid},
		},
		{
			name: "-r uuid 形も張替え",
			argv: []string{"/p/claude", "-r", other},
			uuid: uuid,
			want: []string{"/p/claude", "--resume", uuid},
		},
		{
			name: "値無し --resume（対話 picker 形）はフラグだけ落とす",
			argv: []string{"/p/claude", "--resume"},
			uuid: uuid,
			want: []string{"/p/claude", "--resume", uuid},
		},
		{
			name: "resume 以外のフラグは順序ごと保つ",
			argv: []string{"/p/claude", "--model", "opus", "--resume", other, "--verbose"},
			uuid: uuid,
			want: []string{"/p/claude", "--model", "opus", "--verbose", "--resume", uuid},
		},
		{
			name: "-r の非 uuid 値は claude 側の別解釈＝値を落とさない",
			argv: []string{"/p/claude", "-r", "report.md"},
			uuid: uuid,
			want: []string{"/p/claude", "report.md", "--resume", uuid},
		},
		{
			name: "uuid 未検出なら argv を一切いじらない（既存 resume を失わない）",
			argv: []string{"/p/claude", "--resume", other},
			uuid: "",
			want: []string{"/p/claude", "--resume", other},
		},
		{
			name: "argv 空は nil",
			argv: nil,
			uuid: uuid,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rebuildResumeArgv(tc.argv, tc.uuid, "")
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("rebuildResumeArgv(%v, %q) = %v, want %v", tc.argv, tc.uuid, got, tc.want)
			}
		})
	}
}

// モデル張り替え。**`--resume` した会話は settings.json の既定モデルを無視して
// 会話固定のモデルで動く**（実測 2026-07-25: 既定 opus・sonnet で作った会話を
// resume → sonnet のまま。`--model opus --resume` を渡すと opus-5 に切替わる）＝
// 既存会話のモデルを変える唯一の手段が argv の --model なので、ここが本質。
func TestRebuildResumeArgvModel(t *testing.T) {
	const uuid = "ebdd35b1-f1b6-4e90-8395-c9ff09e00d32"
	for _, tc := range []struct {
		name  string
		argv  []string
		uuid  string
		model string
		want  []string
	}{
		{
			name: "model 指定なしは既存 --model に触らない（会話の固定モデルを勝手に変えない）",
			argv: []string{"/p/claude", "--model", "sonnet"}, uuid: uuid, model: "",
			want: []string{"/p/claude", "--model", "sonnet", "--resume", uuid},
		},
		{
			name: "model 指定で新規付与", argv: []string{"/p/claude"}, uuid: uuid, model: "opus",
			want: []string{"/p/claude", "--resume", uuid, "--model", "opus"},
		},
		{
			name: "既存 --model は張り替える", argv: []string{"/p/claude", "--model", "sonnet"},
			uuid: uuid, model: "opus",
			want: []string{"/p/claude", "--resume", uuid, "--model", "opus"},
		},
		{
			name: "--model=値 形も張り替える", argv: []string{"/p/claude", "--model=fable"},
			uuid: uuid, model: "opus",
			want: []string{"/p/claude", "--resume", uuid, "--model", "opus"},
		},
		{
			name: "model 以外のフラグは保つ",
			argv: []string{"/p/claude", "--verbose", "--model", "sonnet"}, uuid: "", model: "opus",
			want: []string{"/p/claude", "--verbose", "--model", "opus"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rebuildResumeArgv(tc.argv, tc.uuid, tc.model)
			// 順序は実装都合で変わり得るので集合＋隣接ペアで検証する。
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v（要素数違い）", got, tc.want)
			}
			joined := strings.Join(got, " ")
			for i := 0; i+1 < len(tc.want); i++ {
				if tc.want[i] == "--model" || tc.want[i] == "--resume" {
					pair := tc.want[i] + " " + tc.want[i+1]
					if !strings.Contains(joined, pair) {
						t.Fatalf("got %v に %q が無い", got, pair)
					}
				}
			}
			if strings.Contains(joined, "sonnet") && tc.model != "" {
				t.Fatalf("張り替えたのに古い model が残っている: %v", got)
			}
			if got[0] != tc.argv[0] {
				t.Fatalf("argv[0] が変わった: %v", got)
			}
		})
	}
}

func TestSelectRestartTargets(t *testing.T) {
	// agent.list 単独で判定する（v0.5.22）＝tokens/agent_session/tab_id は
	// AgentInfo が持つ（herdr の実採取で PaneInfo と同値であることを確認済み）。
	agents := []herdrapi.AgentInfo{
		{Name: "claude", PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", AgentStatus: "idle",
			AgentSession: herdrapi.AgentSession{Kind: "id", Value: "u-1"}},
		{Name: "claude-2", PaneID: "w1:p2", TabID: "w1:t2", WorkspaceID: "w1", AgentStatus: "working"},
		// シム encode 形でない＝対象外
		{Name: "hdprobe", PaneID: "w1:p3", TabID: "w1:t3", WorkspaceID: "w1"},
		// ↗窓 注入 pane（identity token あり）＝対象外
		{Name: "claude-9", PaneID: "w1:p4", TabID: "w1:t4", WorkspaceID: "w1",
			Tokens: map[string]string{herdrapi.InjTokenPC: "other-herdr", herdrapi.InjTokenSID: "w1:p5"}},
		// token が片方だけでも注入 pane として扱う（部分的にしか読めない場合に
		// 「ローカル claude」へ倒さない＝安全側）
		{Name: "claude-8", PaneID: "w1:p5", TabID: "w1:t5", WorkspaceID: "w1",
			Tokens: map[string]string{herdrapi.InjTokenSID: "w1:p7"}},
	}

	t.Run("sid 空は全ローカル claude pane", func(t *testing.T) {
		got, _, err := selectRestartTargets(agents, "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("対象 %d 件: %+v（claude と claude-2 の 2 件のはず）", len(got), got)
		}
		if got[0].PaneID != "w1:p1" || got[0].ResumeUUID != "u-1" {
			t.Fatalf("1 件目 = %+v（pane w1:p1・uuid u-1 のはず）", got[0])
		}
		if got[1].AgentStatus != "working" {
			t.Fatalf("2 件目の status = %q（working のはず）", got[1].AgentStatus)
		}
	})

	t.Run("注入 pane は token で構造的に除外", func(t *testing.T) {
		got, _, _ := selectRestartTargets(agents, "")
		for _, g := range got {
			if g.PaneID == "w1:p4" || g.PaneID == "w1:p5" {
				t.Fatalf("↗窓 注入 pane %s が対象に入った（token 除外が効いていない）: %+v", g.PaneID, got)
			}
		}
	})

	t.Run("sid 指定はその 1 枚だけ", func(t *testing.T) {
		got, _, err := selectRestartTargets(agents, "w1:p2")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(got) != 1 || got[0].PaneID != "w1:p2" {
			t.Fatalf("got %+v（w1:p2 の 1 件のはず）", got)
		}
	})

	t.Run("対象外 sid は loud に error（黙って 0 件にしない）", func(t *testing.T) {
		for _, sid := range []string{"w1:p3", "w1:p4", "w1:p5", "w1:pZZ"} {
			if _, _, err := selectRestartTargets(agents, sid); err == nil {
				t.Fatalf("sid %q が error にならなかった", sid)
			}
		}
	})
}

func TestTabIndexInWorkspace(t *testing.T) {
	// ⚠number は位置ではない（実測 w1 は 3 tab で number=5/21/23）＝並び順が権威。
	tabs := []herdrapi.TabInfo{
		{TabID: "w1:t5", WorkspaceID: "w1", Number: 5},
		{TabID: "w1:tN", WorkspaceID: "w1", Number: 21},
		{TabID: "w1:tQ", WorkspaceID: "w1", Number: 23},
		{TabID: "w6:t2", WorkspaceID: "w6", Number: 2},
	}
	for _, tc := range []struct {
		ws, tab string
		want    int
		ok      bool
	}{
		{"w1", "w1:t5", 0, true},
		{"w1", "w1:tN", 1, true},
		{"w1", "w1:tQ", 2, true},
		{"w6", "w6:t2", 0, true}, // 別 WS は 0 から数え直す
		{"w1", "w6:t2", 0, false},
	} {
		got, ok := tabIndexInWorkspace(tabs, tc.ws, tc.tab)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("tabIndexInWorkspace(%s,%s) = (%d,%v), want (%d,%v)",
				tc.ws, tc.tab, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSummarizeRestart(t *testing.T) {
	got := summarizeRestart([]restartOutcome{
		{Name: "claude", Status: "done"},
		{Name: "claude-2", Status: "skip", Detail: "working"},
		{Name: "claude-3", Status: "error", Detail: "boom"},
	})
	for _, want := range []string{"再起動 1 件: claude", "skip claude-2(working)", "失敗 claude-3(boom)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summarize = %q（%q を含むはず）", got, want)
		}
	}
	if s := summarizeRestart(nil); !strings.Contains(s, "なし") {
		t.Fatalf("空の要約 = %q", s)
	}
}

// writeStubClaude は生存し続ける stub を作って**絶対パスだけ**返す（PATH には
// 置かない＝PATH 解決に退行したらこの e2e が落ちる）。
// ⚠stub の**ファイル名は "claude" でなければならない**。restart/update は
// isDirectAgentInvocation（argv[0] の basename）で「claude の直接起動か」を
// 確認するため、別名の stub は本番と違う経路（skip）に落ちてテストが本番を
// 担保しなくなる。各呼び出しは t.TempDir() で別ディレクトリになるので、
// 同名でも「別バイナリ」を作れる（曖昧判定のテストが成立する）。
func writeStubClaude(t *testing.T) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatalf("stub 作成: %v", err)
	}
	return stub
}

// writeResumeRejectingStub は「--resume を渡されたら即 exit 1」する stub を作る
// ＝会話 jsonl が消えている claude の実挙動（"No conversation found"）の再現。
func writeResumeRejectingStub(t *testing.T) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$a\" = \"--resume\" ]; then echo 'No conversation found' >&2; exit 1; fi\n" +
		"done\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("stub 作成: %v", err)
	}
	return stub
}

// 復元できない会話 uuid（jsonl 消失）を渡しても **pane を消したまま終わらせない**。
//
// 実インシデント 2026-07-25: herdr の agent_session が指す uuid に対応する
// ~/.claude/projects/**/<uuid>.jsonl が存在せず、`claude --resume <uuid>` が即
// exit → 単独 pane の Tab がプロセス終了で丸ごと自動 close され、セッションが
// 画面から消えた。resume 無しで作り直すフォールバックが無い旧コードで FAIL する。
func TestRestartClaudeFallsBackWhenResumeDies(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeResumeRejectingStub(t)
	dir := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	if _, err := applyClaudeTab(api, wsID, "before", []string{stub}, dir); err != nil {
		t.Fatalf("before tab: %v", err)
	}
	target, err := applyClaudeTab(api, wsID, "doomed", []string{stub}, dir)
	if err != nil {
		t.Fatalf("target tab: %v", err)
	}
	if _, err := applyClaudeTab(api, wsID, "after", []string{stub}, dir); err != nil {
		t.Fatalf("after tab: %v", err)
	}
	if _, err := renameClaudePaneTo(api, target, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	const deadUUID = "48378c2d-4ac5-43f4-b667-08a9bbc3049a"
	if err := api.ReportAgentSession(target, "herdr:claude", "claude", deadUUID); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}

	before, err := api.PaneGet(target)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	tabs, err := listTabs(api)
	if err != nil {
		t.Fatalf("tab.list: %v", err)
	}
	wantIdx, ok := tabIndexInWorkspace(tabs, before.WorkspaceID, before.TabID)
	if !ok {
		t.Fatalf("対象 tab %s が tab.list に無い", before.TabID)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: target, Force: false, DryRun: false}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes: %v（log=%s）", err, log.String())
	}
	if len(results) != 1 || results[0].Status != "done" {
		t.Fatalf("results = %+v（フォールバックで done のはず。log=%s）", results, log.String())
	}

	// **本質**: pane が消えていないこと（旧コードはここで消えた）。
	ag, found := paneOfAgent(t, api, "claude")
	if !found {
		t.Fatalf("pane が失われた（resume 即死時のフォールバックが効いていない）。log=%s", log.String())
	}
	// 実装ヘルパーに依存せず生存を見る（この回帰テストが**挙動**で落ちるように）。
	for i := 0; i < 6; i++ {
		if _, err := api.PaneGet(ag.PaneID); err != nil {
			t.Fatalf("フォールバック後の pane %s も生き残っていない: %v", ag.PaneID, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// resume 無しの元 argv で起動していること。
	root, err := exportTabLayout(api, ag.TabID)
	if err != nil {
		t.Fatalf("layout.export: %v", err)
	}
	leaves := root.leaves()
	if len(leaves) != 1 {
		t.Fatalf("新 tab の pane 数 = %d", len(leaves))
	}
	if got := strings.Join(leaves[0].Command, " "); got != stub {
		t.Fatalf("フォールバック argv = %q, want %q（resume が残っている）", got, stub)
	}

	// 位置と label も復元されていること。
	tabs, err = listTabs(api)
	if err != nil {
		t.Fatalf("tab.list(2): %v", err)
	}
	gotIdx, ok := tabIndexInWorkspace(tabs, ag.WorkspaceID, ag.TabID)
	if !ok {
		t.Fatalf("新 tab %s が tab.list に無い", ag.TabID)
	}
	if gotIdx != wantIdx {
		t.Fatalf("tab 位置 = %d, want %d", gotIdx, wantIdx)
	}
	for _, tb := range tabs {
		if tb.TabID == ag.TabID && tb.Label != "doomed" {
			t.Fatalf("tab label = %q（doomed が引き継がれるはず）", tb.Label)
		}
	}
	if !strings.Contains(results[0].Detail, "復元不可") {
		t.Fatalf("detail が復元不可を報告していない: %q", results[0].Detail)
	}
}

// paneOfAgent は agent 名 exact 一致の pane_id を返す。
func paneOfAgent(t *testing.T, api *herdrapi.Client, name string) (herdrapi.AgentInfo, bool) {
	t.Helper()
	agents, err := api.AgentList()
	if err != nil {
		t.Fatalf("agent.list: %v", err)
	}
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return herdrapi.AgentInfo{}, false
}

// 主経路: 会話 uuid を引き継いで作り直し、agent 名・tab label・tab 位置を保つ。
func TestRestartClaudeAgainstRealHerdr(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	dir := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	// 位置保存を検証するため前後に tab を置く（対象は真ん中）。
	if _, err := applyClaudeTab(api, wsID, "before", []string{stub}, dir); err != nil {
		t.Fatalf("before tab: %v", err)
	}
	target, err := applyClaudeTab(api, wsID, "target", []string{stub}, dir)
	if err != nil {
		t.Fatalf("target tab: %v", err)
	}
	if _, err := applyClaudeTab(api, wsID, "after", []string{stub}, dir); err != nil {
		t.Fatalf("after tab: %v", err)
	}
	if _, err := renameClaudePaneTo(api, target, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	// herdr の claude 検出を模して会話 uuid を載せる（resume 引き継ぎの権威値）。
	// ⚠source は **"herdr:claude"** でなければならない: herdr は agent_session を
	// (source, agent) の exact 許可制で受ける（0.7.4 src/agent_resume.rs
	// is_official_agent_source）。非公式 source は **error にならず黙って捨てられ**
	// agent_session が空のままになる＝この定数を変えるとテストが偽陰性になる。
	const uuid = "ebdd35b1-f1b6-4e90-8395-c9ff09e00d32"
	if err := api.ReportAgentSession(target, "herdr:claude", "claude", uuid); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}
	if p, err := api.PaneGet(target); err != nil || p.AgentSession.Kind != "id" || p.AgentSession.Value != uuid {
		t.Fatalf("会話 uuid が pane に載っていない（前提が壊れている）: %v %+v", err, p.AgentSession)
	}

	before, err := api.PaneGet(target)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	tabs, err := listTabs(api)
	if err != nil {
		t.Fatalf("tab.list: %v", err)
	}
	wantIdx, ok := tabIndexInWorkspace(tabs, before.WorkspaceID, before.TabID)
	if !ok {
		t.Fatalf("対象 tab %s が tab.list に無い", before.TabID)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: target, Force: false, DryRun: false}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes: %v（log=%s）", err, log.String())
	}
	if len(results) != 1 || results[0].Status != "done" {
		t.Fatalf("results = %+v（done 1 件のはず。log=%s）", results, log.String())
	}

	ag, found := paneOfAgent(t, api, "claude")
	if !found {
		t.Fatalf("再起動後に agent 名 claude が消えた（シムの cwd 一致 attach が見失う）")
	}
	if ag.PaneID == target {
		t.Fatalf("pane_id が変わっていない＝作り直されていない: %s", ag.PaneID)
	}

	// 新 pane の argv が「元の絶対パス＋現在の会話 uuid」であること。
	root, err := exportTabLayout(api, ag.TabID)
	if err != nil {
		t.Fatalf("layout.export: %v", err)
	}
	leaves := root.leaves()
	if len(leaves) != 1 {
		t.Fatalf("新 tab の pane 数 = %d（1 のはず）", len(leaves))
	}
	gotArgv := strings.Join(leaves[0].Command, " ")
	wantArgv := stub + " --resume " + uuid
	if gotArgv != wantArgv {
		t.Fatalf("新 argv = %q, want %q", gotArgv, wantArgv)
	}

	// tab label と tab 位置（layout.apply は末尾に作る＝tab.move で戻す）。
	tabs, err = listTabs(api)
	if err != nil {
		t.Fatalf("tab.list(2): %v", err)
	}
	gotIdx, ok := tabIndexInWorkspace(tabs, ag.WorkspaceID, ag.TabID)
	if !ok {
		t.Fatalf("新 tab %s が tab.list に無い", ag.TabID)
	}
	if gotIdx != wantIdx {
		t.Fatalf("tab 位置 = %d, want %d（tab.move による復元が効いていない）", gotIdx, wantIdx)
	}
	for _, tb := range tabs {
		if tb.TabID == ag.TabID && tb.Label != "target" {
			t.Fatalf("tab label = %q（target が継承されるはず）", tb.Label)
		}
	}
}

// working は既定 skip・--force で実行（実行中タスクを黙って殺さない）。
func TestRestartClaudeSkipsWorkingUnlessForced(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	dir := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "busy", []string{stub}, dir)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	if _, err := renameClaudePaneTo(api, pane, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	if err := api.ReportAgent(pane, "test", "claude", "working"); err != nil {
		t.Fatalf("report_agent: %v", err)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: pane, Force: false, DryRun: false}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes: %v", err)
	}
	if len(results) != 1 || results[0].Status != "skip" {
		t.Fatalf("results = %+v（working は skip のはず）", results)
	}
	if !strings.Contains(results[0].Detail, "working") {
		t.Fatalf("skip 理由が working を示していない: %q", results[0].Detail)
	}
	if p, err := api.PaneGet(pane); err != nil || p.PaneID != pane {
		t.Fatalf("skip なのに pane が作り直された（pane.get: %v %+v）", err, p)
	}

	results, err = restartClaudePanes(api, restartOptions{SID: pane, Force: true, DryRun: false}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes --force: %v", err)
	}
	if len(results) != 1 || results[0].Status != "done" {
		t.Fatalf("--force results = %+v（done のはず。log=%s）", results, log.String())
	}
}

// 同居 pane のある Tab は巻き添えを避けて skip する。
func TestRestartClaudeSkipsSharedTab(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	dir := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "shared", []string{stub}, dir)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	if _, err := renameClaudePaneTo(api, pane, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	if err := api.PaneSplit(pane, "right"); err != nil {
		t.Fatalf("pane.split: %v", err)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: pane, Force: false, DryRun: false}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes: %v", err)
	}
	if len(results) != 1 || results[0].Status != "skip" {
		t.Fatalf("results = %+v（同居 pane ありは skip のはず）", results)
	}
	if !strings.Contains(results[0].Detail, "巻き添え") {
		t.Fatalf("skip 理由が巻き添え回避を示していない: %q", results[0].Detail)
	}
	if p, err := api.PaneGet(pane); err != nil || p.PaneID != pane {
		t.Fatalf("skip なのに pane が消えた: %v %+v", err, p)
	}
}

// ↗窓 注入 pane は sid 空の一括再起動でも触らない（実 herdr で token を付けて確認）。
func TestRestartClaudeLeavesInjectedPane(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	dir := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	inj, err := applyClaudeTab(api, wsID, "injected", []string{stub}, dir)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	// 注入 pane は agent 名が付かないのが実運用だが、名前が付いていても token で
	// 外れることを確かめる（防御の層を 1 枚ずつ検証する）。
	if _, err := renameClaudePaneTo(api, inj, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	if err := api.PaneReportMetadata(inj, "drover", herdrapi.ReportMetadata{
		Tokens: map[string]string{"drover_inj_pc": "other-herdr", "drover_inj_sid": "w1:p9"},
	}); err != nil {
		t.Fatalf("report_metadata: %v", err)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: "", Force: false, DryRun: false}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes: %v", err)
	}
	for _, r := range results {
		if r.PaneID == inj {
			t.Fatalf("注入 pane %s が再起動対象に入った: %+v", inj, results)
		}
	}
	if p, err := api.PaneGet(inj); err != nil || p.PaneID != inj {
		t.Fatalf("注入 pane が作り直された: %v %+v", err, p)
	}
	// sid 直指定でも撥ねる（Web から誤って押しても壊れない）。
	if _, err := restartClaudePanes(api, restartOptions{SID: inj, Force: false, DryRun: false}, &log); err == nil {
		t.Fatalf("注入 pane を sid 指定したのに error にならなかった")
	}
}

// **実 herdr で AgentInfo が tokens / agent_session / tab_id を実際に運ぶこと**を
// 担保する。型に足しただけで herdr が返していなければ、注入 pane の除外が
// 常に false になって ↗窓 を再起動してしまう（silent な identity 破壊）。
//
// 旧コメント「agent.list の AgentInfo に tokens は載らない」を信じて pane.list と
// join していたが、herdr 0.7.4 では載る（実採取で確認）。その前提が変わったら
// このテストが落ちる＝join 廃止の安全網。
func TestAgentListCarriesTokensAndSession(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	dir := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "tok", []string{stub}, dir)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	if _, err := renameClaudePaneTo(api, pane, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	const uuid = "ebdd35b1-f1b6-4e90-8395-c9ff09e00d32"
	if err := api.ReportAgentSession(pane, "herdr:claude", "claude", uuid); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}
	if err := api.PaneReportMetadata(pane, "drover", herdrapi.ReportMetadata{
		Tokens: map[string]string{herdrapi.InjTokenPC: "other-herdr"},
	}); err != nil {
		t.Fatalf("report_metadata: %v", err)
	}

	agents, err := api.AgentList()
	if err != nil {
		t.Fatalf("agent.list: %v", err)
	}
	var got *herdrapi.AgentInfo
	for i := range agents {
		if agents[i].PaneID == pane {
			got = &agents[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("agent.list に pane %s が居ない", pane)
	}
	if got.Tokens[herdrapi.InjTokenPC] != "other-herdr" {
		t.Fatalf("AgentInfo.Tokens が空（herdr が agent.list で tokens を返さない）: %+v", got.Tokens)
	}
	if got.AgentSession.Kind != "id" || got.AgentSession.Value != uuid {
		t.Fatalf("AgentInfo.AgentSession が載っていない: %+v", got.AgentSession)
	}
	if got.TabID == "" || got.WorkspaceID == "" || got.Cwd == "" {
		t.Fatalf("AgentInfo に tab_id/workspace_id/cwd が載っていない: %+v", got)
	}
	// pane.list と同値であること（join 廃止の根拠）。
	p, err := api.PaneGet(pane)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	if p.TabID != got.TabID || p.WorkspaceID != got.WorkspaceID ||
		p.AgentStatus != got.AgentStatus || p.AgentSession != got.AgentSession {
		t.Fatalf("agent.list と pane.list が食い違う: agent=%+v pane=%+v", got, p)
	}
	// この状態なら注入 pane として除外されること。
	if targets, _, _ := selectRestartTargets(agents, ""); len(targets) != 0 {
		t.Fatalf("token 付き pane が対象に入った: %+v", targets)
	}
}

// herdr UI から直接起動された（drover が命名していない）claude pane は、
// P1 の identity 一元化で restart 対象に入るようになった。その際に
// **勝手に agent 名を付けない**こと＝herdr ネイティブの pane を drover 管理下の
// 名前に変えてしまわないこと（ユーザーが頼んでいない identity 変更の防止）。
//
// 命名しなくても次回以降は herdr の検出値で拾えるので取りこぼさない。
func TestRestartClaudeKeepsUnnamedPaneUnnamed(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := writeStubClaude(t)
	cwd := t.TempDir()

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "native", []string{stub}, cwd)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	// agent.rename は**しない**（herdr UI 直接起動を模す）。代わりに herdr の
	// 検出値だけを立てる＝identity は系統 3（検出値）で決まる。
	if err := api.ReportAgent(pane, "test-native", "claude", "idle"); err != nil {
		t.Fatalf("report_agent: %v", err)
	}

	agents, err := api.AgentList()
	if err != nil {
		t.Fatalf("agent.list: %v", err)
	}
	targets, conflicts, err := selectRestartTargets(agents, "")
	if err != nil {
		t.Fatalf("selectRestartTargets: %v（conflicts=%v）", err, conflicts)
	}
	if len(targets) != 1 || targets[0].PaneID != pane {
		t.Fatalf("未命名の検出 pane が対象に入っていない: targets=%+v conflicts=%v", targets, conflicts)
	}
	if targets[0].AgentName != "" {
		t.Fatalf("未命名のはずが AgentName=%q", targets[0].AgentName)
	}

	var log bytes.Buffer
	results, err := restartClaudePanes(api, restartOptions{SID: pane}, &log)
	if err != nil {
		t.Fatalf("restartClaudePanes: %v（log=%s）", err, log.String())
	}
	if len(results) != 1 || results[0].Status != "done" {
		t.Fatalf("results = %+v（log=%s）", results, log.String())
	}

	// 作り直した pane に agent 名が付いていないこと。
	agents, err = api.AgentList()
	if err != nil {
		t.Fatalf("agent.list(2): %v", err)
	}
	for _, a := range agents {
		if a.Name != "" {
			t.Fatalf("未命名 pane を再起動したのに agent 名 %q が付いた（herdr ネイティブ pane の identity を変えている）", a.Name)
		}
	}
	if !strings.Contains(log.String(), "未命名のため agent 名を付けない") {
		t.Fatalf("命名しなかったことが報告されていない: %s", log.String())
	}
}
