package main

// レイアウト復元起動の検証（`herdr-drover <agent> --resume <ref>` が **その会話の
// 持ち主 pane 自身**の launch argv として走る経路）。
//
// 実運用の症状: マシン/herdr を再起動するたび **Tab が倍増**し、ユーザーが手で
// 双子（unknown な旧 pane と live な新 pane）を突き合わせて閉じていた
// （skill「再起動後のタブ修復」の手順そのもの）。旧実装は「args 非空＝常に
// 新しい Tab」へ落ちるので本体が別 Tab に生まれ、復元 pane は attachOrReport の
// observe ビューアに化けていた。BUG-1（2026-07-26）はこの同じ復元シナリオの
// 前半（自 pane を observe して空 pane 化）だけを直したので、倍増は残っていた。
//
// harness は status_test.go の startHerdrForTest（隔離 socket・短い /tmp・
// XDG_CONFIG_HOME 隔離）＋ setTestHome（HOME 隔離）。exec はプロセス置換で
// 直接検証できないため execAttach seam に渡る引数列を機械検証する
// （claudeshim_test.go と同じ規律＝合成の別経路を作らない）。

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// totalTabs は全 workspace の tab 数合計（倍増の有無を数で見るための実測値）。
func totalTabs(t *testing.T, api *herdrapi.Client) int {
	t.Helper()
	wss, err := api.WorkspaceList()
	if err != nil {
		t.Fatalf("workspace.list: %v", err)
	}
	n := 0
	for _, ws := range wss {
		n += ws.TabCount
	}
	return n
}

// reportRestoredPane は「レイアウト復元直後の pane」を実 herdr 上に作る:
// 古い会話 ref（agent_session）を保持したまま live agent は居ない
// （report_agent を出さない＝agent_status="unknown"。BUG-1 の実測どおり）。
func reportRestoredPane(t *testing.T, api *herdrapi.Client, ref string) string {
	t.Helper()
	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	pane := ws.RootPane.PaneID
	if err := api.ReportAgentSession(pane, "herdr:claude", "claude", ref); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}
	waitCond(t, 15*time.Second, "復元 pane の agent_session 反映（status は unknown のまま）", func() bool {
		p, e := api.PaneGet(pane)
		return e == nil && p != nil && p.AgentSession.Value == ref && p.AgentStatus == "unknown"
	})
	return pane
}

// TestShimRestoreExecsInPlaceWithoutNewTab は Tab 倍増の回帰テスト。
// 復元起動では **新しい Tab を作らず、その pane で本体へ exec** すること。
//
// 旧コード（この分岐が無い版）では新規経路に落ちて Tab が 1 枚増え、
// execAttach も呼ばれない＝本テストは FAIL する（確認済み）。
func TestShimRestoreExecsInPlaceWithoutNewTab(t *testing.T) {
	const ref = "e1e1e1e1-2222-4333-8444-555555555555"

	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	setTestHome(t, t.TempDir()) // 着地ルール（~/.herdr-drover/workspaces.json）隔離
	stub := installStubClaude(t)
	chdirPhysical(t)
	api := herdrapi.New(sock)

	restored := reportRestoredPane(t, api, ref)
	// herdr が各 pane env に入れる権威値＝「シムはこの pane で走っている」。
	t.Setenv("HERDR_PANE_ID", restored)

	before := totalTabs(t, api)

	var gotArgv0 string
	var gotArgv []string
	calls := 0
	// TTY（復元 pane は実 pty）。exec seam は戻らないのが本番だが、テストでは
	// 「渡った引数列」を捕まえて nil を返す。
	swapSeams(t, true, func(argv0 string, argv []string, _ []string) error {
		calls++
		gotArgv0, gotArgv = argv0, argv
		return nil
	})
	// viewer も seam で潰す。正しい実装ではここは呼ばれない（exec するので
	// ビューアにならない）が、潰さないと**旧挙動が raw mode 失敗の exit=1 で
	// 落ちて**アサートまで届かず、失敗理由が「TTY が無い」に化けてしまう。
	// 呼ばれた回数自体を「ビューアに化けた」証拠として使う。
	viewerCalls := 0
	swapViewer(t, func(_ *herdrapi.Client, paneID string) error {
		viewerCalls++
		return nil
	})

	code, out, errb := runCapture(t, "claude", "--resume", ref)
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if calls != 1 {
		t.Fatalf("この pane での exec（execAttach）が呼ばれていない calls=%d＝新しい Tab 経路に落ちた（viewer 化 %d 回）\nstdout=%s\nstderr=%s", calls, viewerCalls, out, errb)
	}
	if viewerCalls != 0 {
		t.Fatalf("復元 pane が observe ビューアに化けた（%d 回）＝倍増の片割れ\nstdout=%s", viewerCalls, out)
	}
	want := []string{stub, "--resume", ref}
	if gotArgv0 != stub || !reflect.DeepEqual(gotArgv, want) {
		t.Fatalf("exec に渡る引数列が違う: argv0=%q argv=%v（want argv0=%q argv=%v）", gotArgv0, gotArgv, stub, want)
	}
	if !strings.Contains(out, restored) || !strings.Contains(out, "新しい Tab を作らず") {
		t.Errorf("復元起動であることを報告していない（silent な経路変更の禁止）:\n%s", out)
	}

	// **本体のアサート**: Tab が増えていない。作成は非同期反映があり得るので
	// 猶予を置いてから数える（即読みだと増加を見逃す偽陰性になる）。
	time.Sleep(1 * time.Second)
	if after := totalTabs(t, api); after != before {
		t.Fatalf("復元起動で Tab が増えた（= 再起動ごとの倍増）: %d → %d", before, after)
	}
	// 復元 pane がそのまま生きている（exec なので pane_id・Tab・配置が保たれる）。
	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	found := false
	for _, p := range panes {
		if p.PaneID == restored {
			found = true
		}
	}
	if !found {
		t.Fatalf("復元 pane %s が消えた（pane_id は保たれなければならない）", restored)
	}
}

// TestSelfPaneOwningResumeRefIsExact は判定が exact であることを実 herdr で
// 固定する。特に **live な自 pane を対象にしない**こと（対象にすると同じ会話
// jsonl に 2 本目のプロセスを生む＝cm で実害のあった型）。
func TestSelfPaneOwningResumeRefIsExact(t *testing.T) {
	const ref = "f2f2f2f2-2222-4333-8444-555555555555"

	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	api := herdrapi.New(sock)

	restored := reportRestoredPane(t, api, ref)

	// (1) herdr pane 外からの起動（HERDR_PANE_ID 無し）＝復元ではない。
	//     ⚠go test 自身が herdr pane の中で走ると実 env が入っているので
	//     明示的に空へ落とす（隔離 socket に無い pane id で偽陽性/偽陰性にしない）。
	t.Setenv("HERDR_PANE_ID", "")
	if p, err := selfPaneOwningResumeRef(api, "claude", ref); err != nil || p != nil {
		t.Errorf("HERDR_PANE_ID 無しで復元と判定した: p=%v err=%v", p, err)
	}

	t.Setenv("HERDR_PANE_ID", restored)

	// (2) 自 pane × ref 一致 × live でない＝復元（唯一の対象）。
	if p, err := selfPaneOwningResumeRef(api, "claude", ref); err != nil || p == nil || p.PaneID != restored {
		t.Fatalf("復元 pane を対象にしていない: p=%v err=%v", p, err)
	}

	// (3) ref 不一致は対象外（exact-match）。
	for _, bogus := range []string{"", "f2f2f2f2", "00000000-0000-4000-8000-000000000000", "report.md"} {
		if p, err := selfPaneOwningResumeRef(api, "claude", bogus); err != nil || p != nil {
			t.Errorf("ref %q が自 pane に一致した: p=%v err=%v", bogus, p, err)
		}
	}

	// (4) 種別違いは対象外（別エージェントが同じ ref 文字列を持っても横取りしない）。
	if p, err := selfPaneOwningResumeRef(api, "codex", ref); err != nil || p != nil {
		t.Errorf("claude の会話 ref を codex として対象にした: p=%v err=%v", p, err)
	}

	// (5) 自 pane が live なら対象外（復元ではない＝2 本目を作らない）。
	//     ⚠herdr 0.7.4 実測契約: live 化は **状態 → session の順**でしか成立しない。
	live := func() string {
		ws, err := api.WorkspaceCreate()
		if err != nil {
			t.Fatalf("workspace.create(live): %v", err)
		}
		pane := ws.RootPane.PaneID
		if err := api.ReportAgent(pane, "test-native", "claude", "idle"); err != nil {
			t.Fatalf("report_agent(live): %v", err)
		}
		if err := api.ReportAgentSession(pane, "herdr:claude", "claude", ref); err != nil {
			t.Fatalf("report_agent_session(live): %v", err)
		}
		waitCond(t, 15*time.Second, "live 自 pane の反映（status != unknown）", func() bool {
			p, e := api.PaneGet(pane)
			return e == nil && p != nil && p.AgentSession.Value == ref && p.AgentStatus != "unknown" && p.AgentStatus != ""
		})
		return pane
	}()
	t.Setenv("HERDR_PANE_ID", live)
	if p, err := selfPaneOwningResumeRef(api, "claude", ref); err != nil || p != nil {
		t.Errorf("live な自 pane を復元と判定した（同じ会話に 2 本目を作る）: p=%v err=%v", p, err)
	}
}
