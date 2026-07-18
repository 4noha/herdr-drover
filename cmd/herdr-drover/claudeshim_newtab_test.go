package main

// claude シム「新 Tab 着地」の実 herdr 検証（鉄則: 合成で緑にしない）。
//
// 背景（ユーザー実観測の不満）: 旧実装は agent.start が **常に既存 tab の
// focused pane を split** する（Probe 実測: 新 tab を作る経路が存在しない）
// ため、claude 新規起動のたびに既存 Tab の表示を邪魔していた。確定 UX 仕様は
// 「claude 新規起動は常に新しい Tab（claude pane 1 枚）」＝本ファイルの各
// テストは旧 pane-split 挙動だと FAIL する形で書いてある（鉄則①: 実装前に
// 旧コードで FAIL を確認済み）。
//
// harness/seam は claudeshim_test.go と同一（startHerdrForTest / installStub
// Claude / chdirPhysical / swapSeams / waitClaudeAgents を共有）。
// 追加の規律: 着地ルール ~/.herdr-drover/workspaces.json を読むようになった
// ため、**全テストで HOME を隔離**する（実環境のルールファイル不可侵）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

// createWorkspaceLabeled は label 付き workspace を API 直で作る（focus:false
// 明示＝Probe 実測 params {cwd?, focus, label?, env}。herdrapi に labeled create
// ラッパは無いので generic Call を使う＝テストも本体と同じ wire を通す）。
func createWorkspaceLabeled(t *testing.T, api *herdrapi.Client, label string) *herdrapi.WorkspaceCreated {
	t.Helper()
	raw, err := api.Call("workspace.create", struct {
		Label string `json:"label"`
		Focus bool   `json:"focus"`
	}{label, false})
	if err != nil {
		t.Fatalf("workspace.create label=%q: %v", label, err)
	}
	var out herdrapi.WorkspaceCreated
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("workspace_created decode: %v", err)
	}
	return &out
}

// listWorkspaces は workspace.list（実採取 wire:
// {"type":"workspace_list","workspaces":[...]}）。
func listWorkspaces(t *testing.T, api *herdrapi.Client) []herdrapi.WorkspaceInfo {
	t.Helper()
	raw, err := api.Call("workspace.list", nil)
	if err != nil {
		t.Fatalf("workspace.list: %v", err)
	}
	var out struct {
		Workspaces []herdrapi.WorkspaceInfo `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("workspace_list decode: %v", err)
	}
	return out.Workspaces
}

// tabsOf は指定 workspace の tab を tab_id で引ける map にする（実採取 wire:
// tab.list params {workspace_id?} → {"type":"tab_list","tabs":[...]}）。
func tabsOf(t *testing.T, api *herdrapi.Client, wsID string) map[string]herdrapi.TabInfo {
	t.Helper()
	raw, err := api.Call("tab.list", struct {
		WorkspaceID string `json:"workspace_id"`
	}{wsID})
	if err != nil {
		t.Fatalf("tab.list %s: %v", wsID, err)
	}
	var out struct {
		Tabs []herdrapi.TabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("tab_list decode: %v", err)
	}
	m := map[string]herdrapi.TabInfo{}
	for _, tb := range out.Tabs {
		m[tb.TabID] = tb
	}
	return m
}

// writeWorkspacesJSON は隔離 HOME に着地ルールファイルを書く。
func writeWorkspacesJSON(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspaces.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("workspaces.json 書込: %v", err)
	}
}

// focusedWorkspaceID は focused な workspace を返す（focus 非奪取の検証用）。
func focusedWorkspaceID(t *testing.T, api *herdrapi.Client) string {
	t.Helper()
	for _, ws := range listWorkspaces(t, api) {
		if ws.Focused {
			return ws.WorkspaceID
		}
	}
	return ""
}

// ============ 核心: 新規は新 Tab で生まれ既存 tab の pane 数不変 ============

// 旧 pane-split 挙動（agent.start が active tab を split）だと
// 「既存 tab pane_count 1→2・新 tab 無し」になり本テストは FAIL する
// （実装前に旧コードで FAIL 確認済み）。
func TestClaudeShimNewSessionCreatesNewTabNotSplit(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	t.Setenv("HOME", t.TempDir()) // ルールファイル無し＝「ルール無し→現 workspace」経路
	installStubClaude(t)
	work := chdirPhysical(t)
	swapSeams(t, false, nil)
	api := herdrapi.New(sock)

	// 既存状態を API 直で用意: workspace 1 つ（最初は必然 focused＝現 workspace）
	// ＋ その root tab に pane 1 枚。
	ws := createWorkspaceLabeled(t, api, "hd-base")
	wsID := ws.Workspace.WorkspaceID
	baseTab := ws.Tab.TabID

	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	ag := waitClaudeAgents(t, api, work, 1)[0]

	// ① 現 workspace（focused）に生まれている
	if ag.WorkspaceID != wsID {
		t.Fatalf("現 workspace %s でなく %s に生まれた", wsID, ag.WorkspaceID)
	}
	// ② 既存 tab を split していない（旧挙動はここで pane_count=2 になり FAIL）
	tabs := tabsOf(t, api, wsID)
	if got := tabs[baseTab].PaneCount; got != 1 {
		t.Fatalf("既存 tab %s が split された: pane_count=%d want 1（旧 pane-split 挙動）", baseTab, got)
	}
	// ③ 新しい Tab として生まれ、claude pane 1 枚である
	if ag.TabID == baseTab {
		t.Fatalf("新規 claude が既存 tab %s に同居している（新 Tab で生まれていない）", baseTab)
	}
	agTab, ok := tabs[ag.TabID]
	if !ok {
		t.Fatalf("claude の tab %s が tab.list に無い: %+v", ag.TabID, tabs)
	}
	if agTab.PaneCount != 1 {
		t.Fatalf("新 Tab の pane 数=%d want 1（claude pane 単独）", agTab.PaneCount)
	}
	// ④ tab label は cwd 末尾（herdr UI での識別性）
	if want := filepath.Base(work); agTab.Label != want {
		t.Fatalf("新 Tab label=%q want %q（cwd 末尾）", agTab.Label, want)
	}
	// ⑤ 既存 tab の focus を奪っていない（表示を邪魔しない）
	if agTab.Focused {
		t.Fatalf("新 Tab が focus を奪っている: %+v", agTab)
	}
}

// ============ ルールあり → 指定 workspace に新 Tab ============

func TestClaudeShimRuleLandsNewTabInLabeledWorkspace(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	installStubClaude(t)
	work := chdirPhysical(t)
	swapSeams(t, false, nil)
	api := herdrapi.New(sock)

	// 最初の workspace が focused（現 workspace）。ルールはそれとは別の
	// workspace を指す＝「ルールが現 workspace を上書きする」ことの検証。
	other := createWorkspaceLabeled(t, api, "hd-other")
	target := createWorkspaceLabeled(t, api, "hd-target")
	targetTab := target.Tab.TabID
	writeWorkspacesJSON(t, home, fmt.Sprintf(`{"exact": {%q: "hd-target"}}`, work))

	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	ag := waitClaudeAgents(t, api, work, 1)[0]

	if ag.WorkspaceID != target.Workspace.WorkspaceID {
		t.Fatalf("ルール指定 workspace %s でなく %s に生まれた", target.Workspace.WorkspaceID, ag.WorkspaceID)
	}
	tabs := tabsOf(t, api, ag.WorkspaceID)
	if got := tabs[targetTab].PaneCount; got != 1 {
		t.Fatalf("指定 workspace の既存 tab が split された: pane_count=%d want 1", got)
	}
	if ag.TabID == targetTab {
		t.Fatalf("新 Tab で生まれていない（既存 tab %s に同居）", targetTab)
	}
	if agTab := tabs[ag.TabID]; agTab.PaneCount != 1 {
		t.Fatalf("新 Tab の pane 数=%d want 1", agTab.PaneCount)
	}
	// focus 非奪取: focused workspace は hd-other のまま
	if got := focusedWorkspaceID(t, api); got != other.Workspace.WorkspaceID {
		t.Fatalf("focused workspace が移動した: %s（want %s のまま）", got, other.Workspace.WorkspaceID)
	}
}

// ============ label 不在 workspace → 自動作成（focus 非奪取） ============

func TestClaudeShimRuleAutoCreatesWorkspace(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	installStubClaude(t)
	work := chdirPhysical(t)
	swapSeams(t, false, nil)
	api := herdrapi.New(sock)

	other := createWorkspaceLabeled(t, api, "hd-other") // 最初＝focused
	// prefix ルール（親 dir）で解決させる＝最長 prefix 経路も実 herdr で通す
	writeWorkspacesJSON(t, home, fmt.Sprintf(
		`{"rules": [{"prefix": %q, "workspace": "hd-auto"}]}`, filepath.Dir(work)))

	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	ag := waitClaudeAgents(t, api, work, 1)[0]

	// 自動作成された workspace の label が hd-auto であること
	var agWs *herdrapi.WorkspaceInfo
	for _, ws := range listWorkspaces(t, api) {
		if ws.WorkspaceID == ag.WorkspaceID {
			w := ws
			agWs = &w
		}
	}
	if agWs == nil {
		t.Fatalf("claude の workspace %s が workspace.list に無い", ag.WorkspaceID)
	}
	if agWs.Label != "hd-auto" {
		t.Fatalf("自動作成 workspace の label=%q want %q", agWs.Label, "hd-auto")
	}
	if tabs := tabsOf(t, api, ag.WorkspaceID); tabs[ag.TabID].PaneCount != 1 {
		t.Fatalf("新 Tab の pane 数=%d want 1", tabs[ag.TabID].PaneCount)
	}
	// focus 非奪取
	if got := focusedWorkspaceID(t, api); got != other.Workspace.WorkspaceID {
		t.Fatalf("focused workspace が移動した: %s（want %s のまま）", got, other.Workspace.WorkspaceID)
	}
}

// ============ 壊れた workspaces.json → loud エラー（silent fallback 禁止） ============

func TestClaudeShimBrokenRulesFileLoudError(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	installStubClaude(t)
	work := chdirPhysical(t)
	swapSeams(t, false, nil)
	api := herdrapi.New(sock)

	writeWorkspacesJSON(t, home, `{"exact": {broken`)

	code, out, errb := runCapture(t, "claude")
	if code == 0 {
		t.Fatalf("壊れたルールファイルで成功している（silent fallback 禁止）:\nstdout=%s", out)
	}
	if !strings.Contains(errb, "workspaces.json") {
		t.Fatalf("エラーにファイル名（workspaces.json）が無い＝ユーザーが直せない:\n%s", errb)
	}
	// ルール読込は tab/pane 生成より前＝副作用ゼロで停止していること
	time.Sleep(1 * time.Second)
	if got := len(claudeAgents(t, api, work)); got != 0 {
		t.Fatalf("loud エラー時に pane が作られた: %d 件", got)
	}
}

// ============ wsmap.ResolveWorkspaceID の実 herdr 検証 ============

func TestResolveWorkspaceIDRealHerdr(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)

	// 重複 label（herdr は label 重複を許容＝Probe 実測）→ number 最小を返す決定性
	first := createWorkspaceLabeled(t, api, "hd-dup")
	createWorkspaceLabeled(t, api, "hd-dup")
	id, err := wsmap.ResolveWorkspaceID(api, "hd-dup")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID(dup): %v", err)
	}
	if id != first.Workspace.WorkspaceID {
		t.Fatalf("重複 label で number 最小を返していない: got %s want %s", id, first.Workspace.WorkspaceID)
	}

	// 不在 label → 自動作成（label 付き・focus 非奪取）
	id2, err := wsmap.ResolveWorkspaceID(api, "hd-fresh")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID(fresh): %v", err)
	}
	var created *herdrapi.WorkspaceInfo
	for _, ws := range listWorkspaces(t, api) {
		if ws.WorkspaceID == id2 {
			w := ws
			created = &w
		}
	}
	if created == nil || created.Label != "hd-fresh" {
		t.Fatalf("自動作成 workspace が list に無いか label 不一致: %+v", created)
	}
	if created.Focused {
		t.Fatalf("自動作成が focus を奪った: %+v", created)
	}
	if got := focusedWorkspaceID(t, api); got != first.Workspace.WorkspaceID {
		t.Fatalf("focused workspace が移動した: %s", got)
	}

	// 再解決は同じ id（冪等＝実行のたび workspace を増やさない）
	id3, err := wsmap.ResolveWorkspaceID(api, "hd-fresh")
	if err != nil {
		t.Fatalf("ResolveWorkspaceID(fresh 再): %v", err)
	}
	if id3 != id2 {
		t.Fatalf("再解決で別 workspace: %s != %s", id3, id2)
	}
}
