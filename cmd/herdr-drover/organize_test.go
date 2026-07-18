package main

// organize/capture/live 学習のテスト。
//
// 鉄則②: 実 herdr 隔離サーバ（startHerdrForTest＝status_test.go の harness）で
// 実キーパスを検証する（合成だけで緑にしない）。純関数（計画・分類・capture
// 差分）はテーブルテスト、Tab 移動・dry-run 無変更・capture 往復・learn の
// 実イベント→ルール反映は実 herdr で検証する。
//
// ヘルパは他テストファイル（main_test.go の runCapture / claudeshim_newtab_
// test.go の listWorkspaces 等）との名前衝突を避けるため org プレフィクス。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

// ============ 純関数テスト ============

func TestClassifyClaudePaneTable(t *testing.T) {
	names := map[string]string{
		"w1:p1": "claude",   // シム命名
		"w1:p2": "claude-2", // シム命名（採番）
		"w1:p3": "mybot",    // 非 claude 名
		"w1:p4": "claude",   // 名は claude 形だが検出種別が別物（矛盾）
	}
	cases := []struct {
		pane     orgPane
		want     bool
		conflict bool
	}{
		{orgPane{PaneID: "w1:p1", Agent: ""}, true, false},       // (a) 名のみ
		{orgPane{PaneID: "w1:p2", Agent: "claude"}, true, false}, // (a)+(b)
		{orgPane{PaneID: "w1:p3", Agent: ""}, false, false},      // どちらでもない
		{orgPane{PaneID: "w1:p5", Agent: "claude"}, true, false}, // (b) 検出のみ（name 無し＝herdr 直接起動）
		{orgPane{PaneID: "w1:p6", Agent: "codex"}, false, false}, // 別 agent 検出＝非 claude が機械確定
		{orgPane{PaneID: "w1:p4", Agent: "codex"}, false, true},  // 矛盾＝対象外＋報告
		{orgPane{PaneID: "w1:p3", Agent: "claude"}, true, false}, // 名は別だが検出が claude → 対象
	}
	for i, c := range cases {
		got, conflict := classifyClaudePane(c.pane, names)
		if got != c.want || (conflict != "") != c.conflict {
			t.Errorf("[%d] pane=%+v: got=(%v,%q) want=(%v,conflict=%v)", i, c.pane, got, conflict, c.want, c.conflict)
		}
	}
}

func TestCarryTabLabelTable(t *testing.T) {
	tabs := []herdrapi.TabInfo{
		{TabID: "w1:t1", WorkspaceID: "w1", Label: "1"},    // 位置 1・未命名（位置番号）
		{TabID: "w1:t2", WorkspaceID: "w1", Label: "work"}, // 位置 2・custom
		{TabID: "w1:t3", WorkspaceID: "w1", Label: "2"},    // 位置 3・"2" は位置と不一致＝custom 扱い
		{TabID: "w2:t1", WorkspaceID: "w2", Label: ""},     // label 無し
	}
	cases := []struct {
		tab  herdrapi.TabInfo
		want string
	}{
		{tabs[0], ""},     // 位置番号は引き継がない
		{tabs[1], "work"}, // custom は引き継ぐ
		{tabs[2], "2"},    // 位置(3)と不一致の "2" はユーザー命名として引き継ぐ
		{tabs[3], ""},
	}
	for i, c := range cases {
		if got := carryTabLabel(c.tab, tabs); got != c.want {
			t.Errorf("[%d] tab=%s label=%q: got %q want %q", i, c.tab.TabID, c.tab.Label, got, c.want)
		}
	}
}

func TestComputeOrganizePlanTable(t *testing.T) {
	// w1=src / w2=dst 既存。dst2 は未存在（ToWSID ""＝実行時自動作成）。
	wss := []herdrapi.WorkspaceInfo{
		{WorkspaceID: "w1", Number: 1, Label: "src"},
		{WorkspaceID: "w2", Number: 2, Label: "dst"},
	}
	tabs := []herdrapi.TabInfo{
		{TabID: "w1:t1", WorkspaceID: "w1", Label: "1"},     // 単独 claude（→dst）
		{TabID: "w1:t2", WorkspaceID: "w1", Label: "mixed"}, // 同居（claude+shell）
		{TabID: "w1:t3", WorkspaceID: "w1", Label: "dup"},   // claude 複数＝曖昧
		{TabID: "w1:t4", WorkspaceID: "w1", Label: "solo2"}, // 単独 claude（→dst2 未存在）
		{TabID: "w2:t1", WorkspaceID: "w2", Label: "ok"},    // 既に配置済
		{TabID: "w1:t5", WorkspaceID: "w1", Label: "none"},  // ルールなし
	}
	panes := []orgPane{
		{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/a", Agent: "claude"},
		{PaneID: "w1:p2", TabID: "w1:t2", WorkspaceID: "w1", Cwd: "/b", Agent: "claude"},
		{PaneID: "w1:p3", TabID: "w1:t2", WorkspaceID: "w1", Cwd: "/x", Agent: ""}, // 同居 shell
		{PaneID: "w1:p4", TabID: "w1:t3", WorkspaceID: "w1", Cwd: "/c1", Agent: "claude"},
		{PaneID: "w1:p5", TabID: "w1:t3", WorkspaceID: "w1", Cwd: "/c2", Agent: "claude"},
		{PaneID: "w1:p6", TabID: "w1:t4", WorkspaceID: "w1", Cwd: "/d", Agent: "claude"},
		{PaneID: "w2:p1", TabID: "w2:t1", WorkspaceID: "w2", Cwd: "/e", Agent: "claude"},
		{PaneID: "w1:p7", TabID: "w1:t5", WorkspaceID: "w1", Cwd: "/norule", Agent: "claude"},
		{PaneID: "w1:p8", TabID: "w1:t5", WorkspaceID: "w1", Cwd: "/y", Agent: "conflictbot"}, // 矛盾（名 claude 形×検出別物）
	}
	names := map[string]string{"w1:p8": "claude-3"}
	rules := map[string]string{"/a": "dst", "/b": "dst", "/c1": "dst", "/c2": "dst", "/d": "dst2", "/e": "dst"}
	resolve := func(cwd string) string { return rules[cwd] }

	plan := computeOrganizePlan(panes, tabs, wss, names, resolve)

	byPane := map[string]orgPlanItem{}
	byTab := map[string]orgPlanItem{}
	for _, it := range plan {
		if it.PaneID != "" {
			byPane[it.PaneID] = it
		} else {
			byTab[it.TabID] = it
		}
	}
	// 単独 Tab → MOVE（既存 dst へ・tab label "1" は位置番号＝引継なし）
	if it := byPane["w1:p1"]; it.Action != "MOVE" || it.ToWSID != "w2" || it.CarryLabel != "" {
		t.Errorf("w1:p1: %+v", it)
	}
	// 同居 → CARVE
	if it := byPane["w1:p2"]; it.Action != "CARVE" || it.ToWSID != "w2" {
		t.Errorf("w1:p2: %+v", it)
	}
	// claude 複数 Tab → SKIP（tab 単位）
	if it := byTab["w1:t3"]; it.Action != "SKIP" || !strings.Contains(it.Reason, "曖昧") {
		t.Errorf("w1:t3: %+v", it)
	}
	// 未存在 workspace → MOVE with ToWSID ""（実行時自動作成）・custom label 引継ぎ
	if it := byPane["w1:p6"]; it.Action != "MOVE" || it.ToWSID != "" || it.ToLabel != "dst2" || it.CarryLabel != "solo2" {
		t.Errorf("w1:p6: %+v", it)
	}
	// 配置済 → KEEP
	if it := byPane["w2:p1"]; it.Action != "KEEP" || !strings.Contains(it.Reason, "配置済") {
		t.Errorf("w2:p1: %+v", it)
	}
	// ルールなし → KEEP
	if it := byPane["w1:p7"]; it.Action != "KEEP" || !strings.Contains(it.Reason, "ルールなし") {
		t.Errorf("w1:p7: %+v", it)
	}
	// 矛盾 pane → SKIP（pane 単位・理由に矛盾）
	if it := byPane["w1:p8"]; it.Action != "SKIP" || !strings.Contains(it.Reason, "矛盾") {
		t.Errorf("w1:p8: %+v", it)
	}
	// 同居 shell（非 claude）は計画に現れない
	if _, ok := byPane["w1:p3"]; ok {
		t.Errorf("非 claude 同居 pane が計画に混入: %+v", byPane["w1:p3"])
	}
}

func TestComputeCaptureTable(t *testing.T) {
	wss := []herdrapi.WorkspaceInfo{
		{WorkspaceID: "w1", Number: 1, Label: "src"},
		{WorkspaceID: "w2", Number: 2, Label: "dst"},
		{WorkspaceID: "w3", Number: 3, Label: ""}, // label 無し
	}
	panes := []orgPane{
		{PaneID: "p1", WorkspaceID: "w1", Cwd: "/a", Agent: "claude"},
		{PaneID: "p2", WorkspaceID: "w1", Cwd: "/amb", Agent: "claude"},
		{PaneID: "p3", WorkspaceID: "w2", Cwd: "/amb", Agent: "claude"}, // 同 cwd が複数 ws に散在
		{PaneID: "p4", WorkspaceID: "w3", Cwd: "/nolabel", Agent: "claude"},
		{PaneID: "p5", WorkspaceID: "w2", Cwd: "/b", Agent: ""}, // 非 claude は無視
	}
	claude := map[string]bool{"p1": true, "p2": true, "p3": true, "p4": true}
	items := computeCapture(panes, claude, wss)
	got := map[string]captureItem{}
	for _, it := range items {
		got[it.Cwd] = it
	}
	if it := got["/a"]; it.Skip != "" || it.Label != "src" {
		t.Errorf("/a: %+v", it)
	}
	if it := got["/amb"]; !strings.Contains(it.Skip, "曖昧") {
		t.Errorf("/amb: %+v", it)
	}
	if it := got["/nolabel"]; !strings.Contains(it.Skip, "label が無い") {
		t.Errorf("/nolabel: %+v", it)
	}
	if _, ok := got["/b"]; ok {
		t.Errorf("/b（非 claude）が capture に混入")
	}
}

// label 重複 workspace はルール化不能＝skip（レビュー指摘の検証で確定）。
// herdr は label 重複を許容し（Probe 実測・wsmap.go 明記）、organize /
// ResolveWorkspaceID は number 最小の同名 workspace を採る決定則のため、
// 重複 label をルール化すると capture 直後の organize がユーザー配置を
// **別の**同名 workspace へ移動する（capture「現配置の保存」契約の直後に
// 配置が壊れる＝非冪等）。旧コードは Skip="" で label をそのまま保存＝FAIL
// を確認済み。
func TestComputeCaptureDuplicateLabelSkips(t *testing.T) {
	wss := []herdrapi.WorkspaceInfo{
		{WorkspaceID: "w1", Number: 1, Label: "work"},
		{WorkspaceID: "w2", Number: 2, Label: "work"}, // 重複 label
		{WorkspaceID: "w3", Number: 3, Label: "solo"},
	}
	panes := []orgPane{
		// number 最小でない方に配置＝organize が w1 へ差し戻す危険ケース
		{PaneID: "p1", WorkspaceID: "w2", Cwd: "/dup", Agent: "claude"},
		// number 最小の方でも label は語彙として曖昧＝一貫して skip
		{PaneID: "p2", WorkspaceID: "w1", Cwd: "/dupmin", Agent: "claude"},
		// 重複の無い label は従来どおりルール化
		{PaneID: "p3", WorkspaceID: "w3", Cwd: "/ok", Agent: "claude"},
	}
	claude := map[string]bool{"p1": true, "p2": true, "p3": true}
	items := computeCapture(panes, claude, wss)
	got := map[string]captureItem{}
	for _, it := range items {
		got[it.Cwd] = it
	}
	for _, cwd := range []string{"/dup", "/dupmin"} {
		if it := got[cwd]; it.Skip == "" || !strings.Contains(it.Skip, "重複") {
			t.Errorf("%s: label 重複がルール化された: %+v", cwd, it)
		}
	}
	if it := got["/ok"]; it.Skip != "" || it.Label != "solo" {
		t.Errorf("/ok: 重複の無い label が巻き添え skip された: %+v", it)
	}
}

// capture の保存が「Load 後に別プロセス（learn daemon）が書いたルール」を
// 巻き戻さないこと（レビュー指摘: 旧コードは cmdOrganize が先読みした stale
// な Map を runCaptureMode が全量 Save＝lost update。learn が書いた直後の
// ルールが無警告で古い値へ逆転する）。実際の interleave〔capture の Load →
// learn の Load→Save → capture の Save〕を同プロセスで決定的に再現する
// （learn 側の書込は handleLearnEvent と同じファイル往復＝実書込）。
func TestCaptureSaveKeepsConcurrentWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	orgWriteRules(t, home, `{"exact":{"/proj/a":"src"}}`)
	m, err := wsmap.Load() // cmdOrganize と同じ「先読み」snapshot
	if err != nil {
		t.Fatal(err)
	}
	// capture が snapshot を保持している間に learn（別プロセス相当）が書く。
	m2, err := wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	m2.Exact["/proj/b"] = "learned"
	if err := m2.Save(); err != nil {
		t.Fatal(err)
	}
	// capture 実行: /proj/a のみ newlabel へ upsert（/proj/b は触らない）。
	panes := []orgPane{{PaneID: "p1", WorkspaceID: "w1", Cwd: "/proj/a", Agent: "claude"}}
	wss := []herdrapi.WorkspaceInfo{{WorkspaceID: "w1", Number: 1, Label: "newlabel"}}
	var out bytes.Buffer
	if err := runCaptureMode(m, panes, nil, wss, false, &out); err != nil {
		t.Fatalf("runCaptureMode: %v\n%s", err, out.String())
	}
	got, err := wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Exact["/proj/b"] != "learned" {
		t.Fatalf("learn の書込が capture の保存で失われた（lost update）: %+v\n%s", got.Exact, out.String())
	}
	if got.Exact["/proj/a"] != "newlabel" {
		t.Fatalf("capture 自身の upsert が無い: %+v", got.Exact)
	}
}

func TestFindExactKeyTildePreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := &wsmap.Map{Exact: map[string]string{"~/proj": "alpha", "/other": "beta"}}
	// "~/proj" キーは home 展開で同一パス＝既存キーの書式を保って上書き対象
	old, existed, key := findExactKey(m, filepath.Join(home, "proj"))
	if !existed || old != "alpha" || key != "~/proj" {
		t.Fatalf("got old=%q existed=%v key=%q", old, existed, key)
	}
	// 未登録 cwd はキー=cwd そのもの
	old, existed, key = findExactKey(m, "/newdir")
	if existed || old != "" || key != "/newdir" {
		t.Fatalf("miss: got old=%q existed=%v key=%q", old, existed, key)
	}
}

func TestReadLearnMovesTable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// ファイル不在 → false（挙動完全不変の既定）
	if on, err := readLearnMoves(); err != nil || on {
		t.Fatalf("不在: on=%v err=%v", on, err)
	}
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	// キー無し → false
	if err := os.WriteFile(path, []byte(`{"gcp_project":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, err := readLearnMoves(); err != nil || on {
		t.Fatalf("キー無し: on=%v err=%v", on, err)
	}
	// true → true
	if err := os.WriteFile(path, []byte(`{"learn_moves":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, err := readLearnMoves(); err != nil || !on {
		t.Fatalf("true: on=%v err=%v", on, err)
	}
	// 壊れた JSON → エラー（silent 無効化しない）
	if err := os.WriteFile(path, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLearnMoves(); err == nil {
		t.Fatalf("壊れた JSON がエラーにならない")
	}
}

// dispatch/使い方エラー（herdr 不要で常に走る）。
func TestOrganizeDispatchErrors(t *testing.T) {
	// 余分な位置引数は明示エラー
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(t.TempDir(), "none.sock"))
	code, _, errb := runCapture(t, "organize", "bogus")
	if code != 1 || !strings.Contains(errb, "余分な引数") {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	// herdr 不達は接続エラーを明示
	code, _, errb = runCapture(t, "organize")
	if code != 1 || !strings.Contains(errb, "接続できない") {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
}

// ============ 実 herdr ヘルパ（org プレフィクス＝他ファイルと衝突しない） ============

// orgFakeClaude は「プロセス名 claude」の偽バイナリを作る（herdr の検出種別
// はプロセス名 exact-match＝偽物でも agent:"claude" に検出されることを実測
// 済み。実 claude を長時間走らせずに検出経路そのものを踏むための実物代替）。
// ⚠exec は使わない: `exec sleep` だとシェルが置換されプロセス名が "sleep" に
// なり検出されない（実測で確認済み。sleep を子に持つ sh のままなら検出される）。
func orgFakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatalf("fake claude 作成: %v", err)
	}
	return bin
}

// orgPhysDir は物理パス（symlink 解決済）の一時 dir。herdr は pane cwd を
// 物理パスで返す（実測）ため、ルール・突合せは物理パスで揃える。
func orgPhysDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return p
}

func orgWSCreate(t *testing.T, api *herdrapi.Client, label string) string {
	t.Helper()
	raw, err := api.Call("workspace.create", struct {
		Label string `json:"label"`
		Focus bool   `json:"focus"`
	}{label, false})
	if err != nil {
		t.Fatalf("workspace.create %q: %v", label, err)
	}
	var out herdrapi.WorkspaceCreated
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("workspace_created decode: %v", err)
	}
	return out.Workspace.WorkspaceID
}

func orgPaneNode(cwd string, argv ...string) map[string]any {
	return map[string]any{"type": "pane", "cwd": cwd, "command": argv}
}

// orgLayoutApply は layout.apply で新 Tab を作り tab_id を返す（Probe 確定:
// 指定 workspace に新 tab＋argv 直接実行 pane を一発生成する唯一の API）。
func orgLayoutApply(t *testing.T, api *herdrapi.Client, wsid, tabLabel string, root map[string]any) string {
	t.Helper()
	raw, err := api.Call("layout.apply", map[string]any{
		"workspace_id": wsid, "tab_label": tabLabel, "focus": false, "root": root})
	if err != nil {
		t.Fatalf("layout.apply ws=%s: %v", wsid, err)
	}
	var out struct {
		Layout struct {
			TabID string `json:"tab_id"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("layout_apply decode: %v", err)
	}
	return out.Layout.TabID
}

func orgWaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func orgPanes(t *testing.T, api *herdrapi.Client) []orgPane {
	t.Helper()
	panes, err := listPanesWithAgent(api)
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	return panes
}

// orgFindPaneByCwd は cwd 完全一致の pane を返す（無ければ nil）。
func orgFindPaneByCwd(panes []orgPane, cwd string) *orgPane {
	for i := range panes {
		if panes[i].Cwd == cwd {
			return &panes[i]
		}
	}
	return nil
}

// orgWaitClaudeDetected は指定 cwd 群の pane が全て agent:"claude" に検出される
// まで待つ（検出は herdr の非同期処理＝実測 ~2s）。
func orgWaitClaudeDetected(t *testing.T, api *herdrapi.Client, cwds ...string) {
	t.Helper()
	orgWaitFor(t, 20*time.Second, fmt.Sprintf("claude 検出 %v", cwds), func() bool {
		panes := orgPanes(t, api)
		for _, cwd := range cwds {
			p := orgFindPaneByCwd(panes, cwd)
			if p == nil || p.Agent != "claude" {
				return false
			}
		}
		return true
	})
}

// orgWriteRules は隔離 HOME の workspaces.json を書く。
func orgWriteRules(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspaces.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// orgSyncBuf は learn loop の並行ログ受け（データ競合なしで String できる）。
type orgSyncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *orgSyncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *orgSyncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ============ 実 herdr: organize（MOVE/CARVE/KEEP・dry-run 無変更） ============

func TestOrganizeRealHerdrMoveCarveKeepDryRun(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	api := herdrapi.New(sock)
	claude := orgFakeClaude(t)

	w1 := orgWSCreate(t, api, "src")
	orgWSCreate(t, api, "dst") // 既存の解決先
	dirA := orgPhysDir(t)      // 単独 Tab → dst2（未存在＝自動作成）
	dirB := orgPhysDir(t)      // 同居 Tab → dst（切り出し）
	dirC := orgPhysDir(t)      // ルールなし → KEEP

	orgLayoutApply(t, api, w1, "work", orgPaneNode(dirA, claude))
	mixedTab := orgLayoutApply(t, api, w1, "mixed", map[string]any{
		"type": "split", "direction": "right", "ratio": 0.5,
		"first":  orgPaneNode(dirB, claude),
		"second": orgPaneNode(dirB, "/bin/sleep", "300"),
	})
	orgLayoutApply(t, api, w1, "norule", orgPaneNode(dirC, claude))
	orgWaitClaudeDetected(t, api, dirA, dirB, dirC)

	orgWriteRules(t, home, fmt.Sprintf(`{"exact":{%q:"dst2",%q:"dst"}}`, dirA, dirB))

	// --- dry-run: 計画表示のみ・herdr/wsmap 無変更 ---
	code, out, errb := runCapture(t, "organize", "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit=%d\n%s%s", code, out, errb)
	}
	for _, want := range []string{"MOVE ", "CARVE", "KEEP ", "(新規作成: dst2)", "dry-run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run 出力に %q が無い:\n%s", want, out)
		}
	}
	panes := orgPanes(t, api)
	if p := orgFindPaneByCwd(panes, dirA); p == nil || p.WorkspaceID != w1 {
		t.Fatalf("dry-run で pane が動いた: %+v", p)
	}
	wss, err := orgListWorkspaces(api)
	if err != nil {
		t.Fatal(err)
	}
	if wsidIndex(wss)["dst2"] != "" {
		t.Fatalf("dry-run で workspace dst2 が作られた")
	}

	// --- 実行: MOVE（Tab ごと・label 引継ぎ・自動作成）＋CARVE（同居温存） ---
	code, out, errb = runCapture(t, "organize")
	if code != 0 {
		t.Fatalf("organize exit=%d\n%s%s", code, out, errb)
	}
	if !strings.Contains(out, "移動完了") {
		t.Fatalf("実行結果の報告行が無い:\n%s", out)
	}
	wss, err = orgListWorkspaces(api)
	if err != nil {
		t.Fatal(err)
	}
	dst2 := wsidIndex(wss)["dst2"]
	dst := wsidIndex(wss)["dst"]
	if dst2 == "" {
		t.Fatalf("dst2 が自動作成されていない: %+v", wss)
	}
	panes = orgPanes(t, api)
	pA := orgFindPaneByCwd(panes, dirA)
	if pA == nil || pA.WorkspaceID != dst2 {
		t.Fatalf("単独 Tab が dst2 に移動していない: %+v", pA)
	}
	// Tab label "work"（custom）引継ぎ
	tabs, err := listTabs(api)
	if err != nil {
		t.Fatal(err)
	}
	labelOf := map[string]string{}
	pcOf := map[string]int{}
	for _, tb := range tabs {
		labelOf[tb.TabID] = tb.Label
		pcOf[tb.TabID] = tb.PaneCount
	}
	if labelOf[pA.TabID] != "work" {
		t.Fatalf("単独 Tab の label が引き継がれていない: tab=%s label=%q", pA.TabID, labelOf[pA.TabID])
	}
	// CARVE: claude は dst へ・同居 sleep は元 Tab "mixed" に残る
	var pB *orgPane
	for i := range panes {
		if panes[i].Cwd == dirB && panes[i].Agent == "claude" {
			pB = &panes[i]
		}
	}
	if pB == nil || pB.WorkspaceID != dst {
		t.Fatalf("同居 claude が dst へ切り出されていない: %+v", pB)
	}
	if pcOf[mixedTab] != 1 || labelOf[mixedTab] != "mixed" {
		t.Fatalf("同居 Tab が温存されていない: pane_count=%d label=%q", pcOf[mixedTab], labelOf[mixedTab])
	}
	// KEEP: ルールなしは現状維持
	if p := orgFindPaneByCwd(panes, dirC); p == nil || p.WorkspaceID != w1 {
		t.Fatalf("ルールなし pane が動いた: %+v", p)
	}

	// --- 再実行: 全て KEEP（冪等）＝MOVE/CARVE ゼロ ---
	code, out, errb = runCapture(t, "organize")
	if code != 0 {
		t.Fatalf("再実行 exit=%d\n%s%s", code, out, errb)
	}
	if strings.Contains(out, "MOVE ") || strings.Contains(out, "CARVE") {
		t.Fatalf("再実行で移動が計画された（冪等でない）:\n%s", out)
	}
	if !strings.Contains(out, "配置済") {
		t.Fatalf("配置済 KEEP の報告が無い:\n%s", out)
	}
}

// ============ 実 herdr: capture（ルール往復・dry-run 無変更・曖昧 skip） ============

func TestOrganizeCaptureRealHerdrRoundTrip(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	api := herdrapi.New(sock)
	claude := orgFakeClaude(t)

	w1 := orgWSCreate(t, api, "src")
	w2 := orgWSCreate(t, api, "dst")
	dirA := orgPhysDir(t) // w1: 既存 exact "old" を上書き
	dirB := orgPhysDir(t) // w2: 新規
	dirC := orgPhysDir(t) // w1 と w2 に散在＝曖昧 skip
	dirD := orgPhysDir(t) // w2: シム命名（agent.start name=claude・検出なし）＝(a) 経路

	orgLayoutApply(t, api, w1, "a", orgPaneNode(dirA, claude))
	orgLayoutApply(t, api, w2, "b", orgPaneNode(dirB, claude))
	orgLayoutApply(t, api, w1, "c1", orgPaneNode(dirC, claude))
	orgLayoutApply(t, api, w2, "c2", orgPaneNode(dirC, claude))
	if _, err := api.AgentStart("claude", []string{"/bin/sleep", "300"}, &herdrapi.AgentStartOptions{Cwd: dirD, WorkspaceID: w2}); err != nil {
		t.Fatalf("agent.start named claude: %v", err)
	}
	orgWaitClaudeDetected(t, api, dirA, dirB, dirC)
	orgWaitFor(t, 10*time.Second, "named agent 出現", func() bool {
		agents, err := api.AgentList()
		if err != nil {
			return false
		}
		for _, a := range agents {
			if a.Name == "claude" {
				return true
			}
		}
		return false
	})

	// 既存ルール: exact 1 件＋prefix/default（capture は exact のみ触る契約）
	orgWriteRules(t, home, fmt.Sprintf(`{"exact":{%q:"old"},"rules":[{"prefix":"/zzz-keep","workspace":"keep"}],"default":"dflt"}`, dirA))

	// --- dry-run: 差分表示のみ・保存しない ---
	code, out, errb := runCapture(t, "organize", "--capture", "--dry-run")
	if code != 0 {
		t.Fatalf("capture dry-run exit=%d\n%s%s", code, out, errb)
	}
	for _, want := range []string{
		"~ " + dirA + " → src（旧: old",
		"+ " + dirB + " → dst",
		"+ " + dirD + " → dst",
		"SKIP " + dirC + ": 曖昧",
		"dry-run",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("capture dry-run 出力に %q が無い:\n%s", want, out)
		}
	}
	m, err := wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Exact) != 1 || m.Exact[dirA] != "old" {
		t.Fatalf("dry-run で wsmap が書き換わった: %+v", m.Exact)
	}

	// --- 実行: exact 往復＋prefix/default 不変 ---
	code, out, errb = runCapture(t, "organize", "--capture")
	if code != 0 {
		t.Fatalf("capture exit=%d\n%s%s", code, out, errb)
	}
	if !strings.Contains(out, "保存") {
		t.Fatalf("保存の報告が無い:\n%s", out)
	}
	m, err = wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if m.Exact[dirA] != "src" || m.Exact[dirB] != "dst" || m.Exact[dirD] != "dst" {
		t.Fatalf("exact が期待どおりでない: %+v", m.Exact)
	}
	if _, ok := m.Exact[dirC]; ok {
		t.Fatalf("曖昧 cwd が保存された: %+v", m.Exact)
	}
	if len(m.Rules) != 1 || m.Rules[0].Prefix != "/zzz-keep" || m.Default != "dflt" {
		t.Fatalf("prefix/default が変わった: rules=%+v default=%q", m.Rules, m.Default)
	}

	// --- 再実行: 変更なし（冪等） ---
	code, out, errb = runCapture(t, "organize", "--capture")
	if code != 0 {
		t.Fatalf("capture 再実行 exit=%d\n%s%s", code, out, errb)
	}
	if !strings.Contains(out, "変更なし") {
		t.Fatalf("冪等の報告が無い:\n%s", out)
	}
}

// ============ 実 herdr: live 学習（実イベント→ルール反映・バックログ dedup） ============

func TestLearnMovesRealEventsAndBacklogDedup(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	api := herdrapi.New(sock)
	claude := orgFakeClaude(t)

	w1 := orgWSCreate(t, api, "src")
	w2 := orgWSCreate(t, api, "dst")
	dirA := orgPhysDir(t)
	dirB := orgPhysDir(t)

	// --- Phase A: 購読前の移動＋pane 消滅 → バックログは stale＝学習しない ---
	// herdr は新規購読のたびに過去 event を再送する（実測）。旧実装が event を
	// 鵜呑みにすると dirA→dst を誤学習する。ライブ照合（pane 消滅＝現況取得
	// 不能）で捨てることを実イベントで検証する。
	orgLayoutApply(t, api, w1, "la", orgPaneNode(dirA, claude))
	orgWaitClaudeDetected(t, api, dirA)
	pA := orgFindPaneByCwd(orgPanes(t, api), dirA)
	mv, err := paneMoveNewTab(api, pA.PaneID, w2, "")
	if err != nil {
		t.Fatalf("pane.move: %v", err)
	}
	if _, err := api.Call("pane.close", struct {
		PaneID string `json:"pane_id"`
	}{mv.Pane.PaneID}); err != nil {
		t.Fatalf("pane.close: %v", err)
	}
	orgWaitFor(t, 10*time.Second, "pane 消滅", func() bool {
		return orgFindPaneByCwd(orgPanes(t, api), dirA) == nil
	})

	buf := &orgSyncBuf{}
	lg := log.New(buf, "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runLearnLoop(ctx, api, lg) }()

	// stale バックログが処理された（skip ログ）ことを確認してから無学習を検証
	orgWaitFor(t, 15*time.Second, "stale バックログの skip 処理", func() bool {
		return strings.Contains(buf.String(), "現況取得不能")
	})
	m, err := wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Exact) != 0 {
		t.Fatalf("stale バックログから誤学習した: %+v", m.Exact)
	}

	// --- Phase B: ライブの Tab 移動 → exact ルール自動反映＋1 行ログ ---
	orgLayoutApply(t, api, w1, "lb", orgPaneNode(dirB, claude))
	orgWaitClaudeDetected(t, api, dirB)
	pB := orgFindPaneByCwd(orgPanes(t, api), dirB)
	if _, err := paneMoveNewTab(api, pB.PaneID, w2, ""); err != nil {
		t.Fatalf("pane.move live: %v", err)
	}
	orgWaitFor(t, 15*time.Second, "live 移動の学習", func() bool {
		m, err := wsmap.Load()
		return err == nil && m.Exact[dirB] == "dst"
	})
	if !strings.Contains(buf.String(), "learn: exact ルール") {
		t.Fatalf("ルール書込の 1 行ログが無い:\n%s", buf.String())
	}
	m, err = wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Exact[dirA]; ok {
		t.Fatalf("stale の dirA が学習されている: %+v", m.Exact)
	}
}

// ============ 実 herdr: learn のバックログ再学習防止（daemon 再起動相当） ============

// daemon 再起動（=新規購読）のバックログ再送で、ユーザーが明示削除した
// ルールが復活しないこと（レビュー指摘の実再現ケース: 「pane がまだ移動先に
// 居る」バックログはライブ照合をすり抜ける。旧コードで復活＝FAIL を確認済み）。
func TestLearnRestartDoesNotResurrectDeletedRule(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	api := herdrapi.New(sock)
	claude := orgFakeClaude(t)

	w1 := orgWSCreate(t, api, "src")
	w2 := orgWSCreate(t, api, "dst")
	dirB := orgPhysDir(t)
	dirC := orgPhysDir(t)

	// --- loop#1 稼働中の live 移動 → 学習される（前提の確立） ---
	buf1 := &orgSyncBuf{}
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() { _ = runLearnLoop(ctx1, api, log.New(buf1, "", 0)) }()
	orgLayoutApply(t, api, w1, "lb", orgPaneNode(dirB, claude))
	orgWaitClaudeDetected(t, api, dirB)
	pB := orgFindPaneByCwd(orgPanes(t, api), dirB)
	if _, err := paneMoveNewTab(api, pB.PaneID, w2, ""); err != nil {
		t.Fatalf("pane.move dirB: %v", err)
	}
	orgWaitFor(t, 15*time.Second, "loop#1 の live 学習", func() bool {
		m, err := wsmap.Load()
		return err == nil && m.Exact[dirB] == "dst"
	})
	cancel1()

	// --- ユーザーがルールを明示削除（pane は dst に置いたまま＝普通の使い方） ---
	orgWriteRules(t, home, `{}`)

	// --- loop#2（daemon 再起動相当）: dirB のバックログ再送が来ても再学習しない ---
	buf2 := &orgSyncBuf{}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = runLearnLoop(ctx2, api, log.New(buf2, "", 0)) }()
	// 「バックログを処理し終えた」ことは後続 live イベントの処理完了で機械
	// 判定する（Subscribe は同一接続で順序保存＝dirC の学習が見えた時点で
	// dirB のバックログは処理済み）。
	orgLayoutApply(t, api, w1, "lc", orgPaneNode(dirC, claude))
	orgWaitClaudeDetected(t, api, dirC)
	pC := orgFindPaneByCwd(orgPanes(t, api), dirC)
	if _, err := paneMoveNewTab(api, pC.PaneID, w2, ""); err != nil {
		t.Fatalf("pane.move dirC: %v", err)
	}
	orgWaitFor(t, 15*time.Second, "loop#2 の live 学習（順序マーカー）", func() bool {
		m, err := wsmap.Load()
		return err == nil && m.Exact[dirC] == "dst"
	})
	m, err := wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Exact[dirB]; ok {
		t.Fatalf("削除したルールが daemon 再起動のバックログ再送で復活した: %+v\nlog:\n%s", m.Exact, buf2.String())
	}
}

// learn も label 重複 workspace への移動はルール化しない（capture と同一
// 判定＝レビュー指摘: 重複 label をルール化すると次の organize が手動配置を
// number 最小の同名 workspace へ差し戻す。旧コードはルール化＝FAIL 確認済み）。
func TestLearnSkipsDuplicateLabelWorkspace(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	home := t.TempDir()
	t.Setenv("HOME", home)
	api := herdrapi.New(sock)
	claude := orgFakeClaude(t)

	w1 := orgWSCreate(t, api, "src")
	orgWSCreate(t, api, "work")
	wDup := orgWSCreate(t, api, "work") // 重複 label（herdr は許容＝実測）
	dirA := orgPhysDir(t)

	buf := &orgSyncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runLearnLoop(ctx, api, log.New(buf, "", 0)) }()
	orgLayoutApply(t, api, w1, "ld", orgPaneNode(dirA, claude))
	orgWaitClaudeDetected(t, api, dirA)
	p := orgFindPaneByCwd(orgPanes(t, api), dirA)
	if _, err := paneMoveNewTab(api, p.PaneID, wDup, ""); err != nil {
		t.Fatalf("pane.move: %v", err)
	}
	// skip は理由報告必須（silent 禁止）＝ログで処理完了を機械判定する。
	orgWaitFor(t, 15*time.Second, "重複 label skip の報告", func() bool {
		return strings.Contains(buf.String(), "重複")
	})
	m, err := wsmap.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Exact) != 0 {
		t.Fatalf("重複 label がルール化された: %+v", m.Exact)
	}
}
