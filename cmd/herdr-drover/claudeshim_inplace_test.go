package main

// シムが **herdr の pane の中で走っているとき**は新しい Tab を作らず、その pane で
// 実エージェントへ exec 置換することの検証。
//
// 背景（2026-09-05〜09-06 実観測）: シムは「新規は常に新しい Tab」なので、
// **既に pane がある状態**でシムを起動すると 1 枚余る:
//   - `herdr agent start … -- herdr-drover claude …`（新プロジェクト起こし skill）
//     → agent.start が作った pane でシムが走り、シムがもう 1 枚 Tab を作る。
//       元の pane は observe ビューアに化けて Tab に同居し続ける
//   - マシン再起動のレイアウト復元（復元 pane で `--resume` シムが走る）
//     → 同じ理屈で Tab が倍増する（2026-08-24 に 11 枚→22 枚を実測）
// どちらも「起動元が既に herdr の pane」＝新 Tab は要らない。
//
// ⚠ herdr の**外**（WezTerm 等）から起動したときは従来どおり新 Tab ＋ ローカル
// ビューアのままにする（起動元の端末に pane は無いので、作らないと表示先が無い）。
// 判定は **HERDR_PANE_ID env の有無 exact**（herdr が pane に注入する一次事実）。

import (
	"io"
	"testing"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// shimInPlaceSetup は 2 テスト共通の下ごしらえ（実 herdr・隔離 HOME・stub claude）。
func shimInPlaceSetup(t *testing.T) (api *herdrapi.Client, stub, work, wsID, baseTab, rootPane string) {
	t.Helper()
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	setTestHome(t, t.TempDir())
	stub = installStubClaude(t)
	work = chdirPhysical(t)
	api = herdrapi.New(sock)
	ws := createWorkspaceLabeled(t, api, "hd-base")
	return api, stub, work, ws.Workspace.WorkspaceID, ws.Tab.TabID, ws.RootPane.PaneID
}

// TestShimExecsInPlaceInsideHerdrPane: HERDR_PANE_ID がある＝既に pane の中に
// 居るので、**Tab を増やさず**その場で実バイナリへ exec する。
// 旧コードは startClaudeAgent で新 Tab を作る＝exec seam が呼ばれず FAIL する。
func TestShimExecsInPlaceInsideHerdrPane(t *testing.T) {
	api, stub, work, wsID, _, rootPane := shimInPlaceSetup(t)

	// 復元 pane / agent start が作った pane を模す（**実在する pane** を指す）。
	t.Setenv("HERDR_PANE_ID", rootPane)

	var gotArgv []string
	// TTY=false: 引数なし経路では picker に無関係（既存 newtab テストと同条件）。
	swapSeams(t, false, func(argv0 string, argv []string, _ []string) error {
		gotArgv = append([]string{argv0}, argv...)
		return nil
	})

	before := len(tabsOf(t, api, wsID))
	if code, out, errb := runCapture(t, "claude"); code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}

	// ① 実バイナリへ exec 置換している（argv0 と argv[0] が stub）
	if len(gotArgv) < 2 || gotArgv[0] != stub || gotArgv[1] != stub {
		t.Fatalf("in-place exec が起きていない: gotArgv=%v want [%s %s]", gotArgv, stub, stub)
	}
	// ② Tab を増やしていない（旧挙動はここで +1）
	if after := len(tabsOf(t, api, wsID)); after != before {
		t.Fatalf("Tab が増えた: %d → %d（in-place なら増えないはず）", before, after)
	}
	// ③ 新しい agent pane も作っていない
	if ags := claudeAgents(t, api, work); len(ags) != 0 {
		t.Fatalf("新規 agent pane を作っている: %+v", ags)
	}
}

// TestShimStillCreatesNewTabOutsideHerdr: HERDR_PANE_ID が無い＝herdr の外
// （WezTerm 等）からの起動。従来どおり新 Tab を作る（表示先の pane が無いため）。
func TestShimStillCreatesNewTabOutsideHerdr(t *testing.T) {
	api, _, work, wsID, baseTab, _ := shimInPlaceSetup(t)
	t.Setenv("HERDR_PANE_ID", "") // herdr の外

	var execCalled bool
	swapSeams(t, false, func(string, []string, []string) error {
		execCalled = true
		return nil
	})

	if code, out, errb := runCapture(t, "claude"); code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	ag := waitClaudeAgents(t, api, work, 1)[0]
	if execCalled {
		t.Fatal("herdr の外なのに in-place exec した（新 Tab を作るべき）")
	}
	if ag.TabID == baseTab {
		t.Fatalf("既存 tab %s に同居した（新 Tab で生まれるべき）", baseTab)
	}
	if tabs := tabsOf(t, api, wsID); tabs[baseTab].PaneCount != 1 {
		t.Fatalf("既存 tab を split した: pane_count=%d want 1", tabs[baseTab].PaneCount)
	}
	_ = io.Discard
}

// TestShimIgnoresStaleHerdrPaneID: HERDR_PANE_ID があっても**その pane が
// この herdr に無い**なら pane 内起動と見なさない（別サーバ由来の値が環境に
// 残っているケースが実在する＝2026-09-06 に隔離 e2e へ開発者シェルの値が
// 漏れて pane_not_found になった）。env の存在だけを信じない。
func TestShimIgnoresStaleHerdrPaneID(t *testing.T) {
	api, _, work, wsID, baseTab, _ := shimInPlaceSetup(t)
	t.Setenv("HERDR_PANE_ID", "w9:pZZ") // この herdr に存在しない pane

	var execCalled bool
	swapSeams(t, false, func(string, []string, []string) error {
		execCalled = true
		return nil
	})

	if code, out, errb := runCapture(t, "claude"); code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if execCalled {
		t.Fatal("実在しない HERDR_PANE_ID で in-place exec した")
	}
	ag := waitClaudeAgents(t, api, work, 1)[0]
	if ag.TabID == baseTab {
		t.Fatalf("既存 tab %s に同居した（新 Tab で生まれるべき）", baseTab)
	}
	if tabs := tabsOf(t, api, wsID); tabs[baseTab].PaneCount != 1 {
		t.Fatalf("既存 tab を split した: pane_count=%d want 1", tabs[baseTab].PaneCount)
	}
}
