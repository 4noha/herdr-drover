//go:build !windows

package e2e

// wsrules e2e — Tab 単位着地ルールの完全往復ループ（確定 UX 仕様のゲート）。
//
// 実 herdr 隔離サーバ ＋ **実バイナリ**（go build した herdr-drover）＋
// 実 TTY（pty ハーネス）で、ユーザー体験そのものの一気通貫を機械検証する:
//
//	TestE2EWsRulesLoopRoundTrip:
//	  ルール書込 → シム新規が**新 Tab**で指定 workspace に着地（既存 tab の
//	  pane 数不変を機械確認）→ 手動 Tab 移動の模擬 → organize --capture で
//	  ルール化 → 同 cwd の新規 2 本目が学習先 workspace に新 Tab で着地 →
//	  organize --dry-run が「移動不要」（全 KEEP 配置済・MOVE/CARVE ゼロ）
//
//	TestE2EWsRulesDirectStartAndCarveNormalize:
//	  【herdr 直接起動ケース】agent name 無しの claude（stub）を herdr の
//	  プロセス名検出（agent:"claude"）で同定し organize/capture が対象にする
//	  ＋ 旧シム型の「間借り pane」（agent.start が既存 Tab を split）を
//	  organize が新 Tab 切り出し（CARVE）で正規化する
//
//	TestE2ELearnMovesDaemonRealBinary:
//	  learn_moves=true の opt-in で **実バイナリ daemon** 稼働中に手動 Tab
//	  移動 → workspaces.json 自動更新 ＋ 1 行ログ、SIGTERM graceful まで
//
// 鉄則の適用:
//   - 合成ストリームなし（herdr / python3 / エミュレータ不在は Skip）
//   - 同定は exact-match のみ（シム命名 or herdr 検出種別。推測しない）
//   - stub claude は「exec しない sh スクリプト」: `exec sleep` だと
//     プロセス名が sleep になり herdr に検出されない（organize_test の
//     orgFakeClaude で確認済みの実測）。marker echo 後も sh のまま sleep を
//     子に持てば検出は維持される
//
// ⚠「tab.move で手動移動」について（正直な注記・Probe live 実測）:
//   herdr 0.7.4 の tab.move は**同一 workspace 内 reorder 専用**で、別
//   workspace への Tab 移動 API は存在しない。cross-workspace の「Tab 移動」
//   の実体は pane.move {destination:{type:"new_tab", workspace_id}} が唯一の
//   プリミティブ（claude 単独 Tab ならこれが Tab 移動そのもの。terminal_id /
//   agent name 維持・空になったソース Tab の自動 close を実測済み）。
//   本テストの「ユーザー手動移動の模擬」はこの pane.move を使う。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

// ============ wire ヘルパ（wsr プレフィクス＝package 内で一意） ============

// wsrPane は pane.list の pane（herdrapi.PaneInfo に無い検出種別 `agent` を
// 含むローカル decode。cmd 側 orgPane と同じ実採取スキーマ）。
type wsrPane struct {
	PaneID      string `json:"pane_id"`
	TerminalID  string `json:"terminal_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Cwd         string `json:"cwd"`
	Agent       string `json:"agent"` // herdr のプロセス名検出（"claude" 等・null は ""）
}

func wsrPanes(t *testing.T, api *herdrapi.Client) []wsrPane {
	t.Helper()
	raw, err := api.Call("pane.list", nil)
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	var out struct {
		Panes []wsrPane `json:"panes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("pane_list decode: %v", err)
	}
	return out.Panes
}

// wsrTabCounts は tab_id → pane_count（「既存 tab の pane 数不変」の機械確認
// と「新 Tab である」判定の基礎データ）。
func wsrTabCounts(t *testing.T, api *herdrapi.Client) map[string]int {
	t.Helper()
	raw, err := api.Call("tab.list", nil)
	if err != nil {
		t.Fatalf("tab.list: %v", err)
	}
	var out struct {
		Tabs []herdrapi.TabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("tab_list decode: %v", err)
	}
	m := make(map[string]int, len(out.Tabs))
	for _, tb := range out.Tabs {
		m[tb.TabID] = tb.PaneCount
	}
	return m
}

// wsrAssertTabsUnchanged は baseline の全 tab が同じ pane 数のまま在ることを
// 検証する（確定 UX「claude 起動が既存 Tab の表示を邪魔しない」の物証）。
func wsrAssertTabsUnchanged(t *testing.T, phase string, base, cur map[string]int) {
	t.Helper()
	for id, n := range base {
		got, ok := cur[id]
		if !ok {
			t.Fatalf("%s: 既存 tab %s が消えた（base=%v cur=%v）", phase, id, base, cur)
		}
		if got != n {
			t.Fatalf("%s: 既存 tab %s の pane 数が変わった: %d→%d（split された疑い）", phase, id, n, got)
		}
	}
}

func wsrWorkspaceLabels(t *testing.T, api *herdrapi.Client) map[string]string {
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
	m := make(map[string]string, len(out.Workspaces))
	for _, w := range out.Workspaces {
		m[w.WorkspaceID] = w.Label
	}
	return m
}

func wsrWSCreate(t *testing.T, api *herdrapi.Client, label string) string {
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

// wsrLayoutApply は「指定 workspace に新 tab＋argv 直接実行 pane 1 枚」を
// 一発生成する唯一の正規 API layout.apply（Probe 確定）で tab/pane id を返す。
func wsrLayoutApply(t *testing.T, api *herdrapi.Client, wsid, tabLabel, cwd string, argv ...string) (tabID, paneID string) {
	t.Helper()
	raw, err := api.Call("layout.apply", map[string]any{
		"workspace_id": wsid, "tab_label": tabLabel, "focus": false,
		"root": map[string]any{"type": "pane", "cwd": cwd, "command": argv}})
	if err != nil {
		t.Fatalf("layout.apply ws=%s: %v", wsid, err)
	}
	var out struct {
		Layout struct {
			TabID string `json:"tab_id"`
			Root  struct {
				PaneID string `json:"pane_id"`
			} `json:"root"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("layout_apply decode: %v", err)
	}
	return out.Layout.TabID, out.Layout.Root.PaneID
}

// wsrPaneMoveNewTab はユーザーの手動 Tab 移動の模擬（ファイル冒頭の注記の
// とおり cross-workspace Tab 移動の唯一のプリミティブ）。移動後 pane を返す。
func wsrPaneMoveNewTab(t *testing.T, api *herdrapi.Client, paneID, wsid string) wsrPane {
	t.Helper()
	raw, err := api.Call("pane.move", map[string]any{
		"pane_id":     paneID,
		"destination": map[string]any{"type": "new_tab", "workspace_id": wsid},
		"focus":       false})
	if err != nil {
		t.Fatalf("pane.move %s→%s: %v", paneID, wsid, err)
	}
	var out struct {
		MoveResult struct {
			Pane wsrPane `json:"pane"`
		} `json:"move_result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("pane_move decode: %v", err)
	}
	return out.MoveResult.Pane
}

// wsrInstallClaudeStub は herdr に検出される claude stub を作る。
// installStubClaudeAt（claudeshim_e2e）と違い **exec しない**: exec すると
// プロセス名が sleep になり herdr の検出種別（agent:"claude"）に載らない
// 実測があるため（organize_test orgFakeClaude の一次事実）。marker echo は
// pty attach の物証用（layout.apply 直接起動では単に無害）。
func wsrInstallClaudeStub(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho " + marker + "\nsleep 300\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("stub claude 作成: %v", err)
	}
	return stub
}

// wsrWaitDetected は指定 cwd 群の**全** pane が agent:"claude" に検出される
// まで待つ（検出は herdr の非同期処理＝実測 ~2s。同一 cwd に複数 pane が
// あるケース〔ラウンドトリップの 2 本目〕も全数を要求する）。
func wsrWaitDetected(t *testing.T, api *herdrapi.Client, cwds ...string) {
	t.Helper()
	waitFor(t, 20*time.Second, fmt.Sprintf("claude 検出 %v", cwds), func() (bool, error) {
		panes := wsrPanes(t, api)
		for _, cwd := range cwds {
			found := false
			for _, p := range panes {
				if p.Cwd != cwd {
					continue
				}
				if p.Agent != "claude" {
					return false, fmt.Errorf("pane %s (cwd=%s) agent=%q", p.PaneID, cwd, p.Agent)
				}
				found = true
			}
			if !found {
				return false, fmt.Errorf("cwd=%s の pane が未出現", cwd)
			}
		}
		return true, nil
	})
}

// wsrWaitAgent は name＋cwd exact-match の agent を待つ（agent.list の反映は
// 遅延し得る実測＝claudeshim_e2e と同じ猶予設計）。
func wsrWaitAgent(t *testing.T, api *herdrapi.Client, name, cwd string) herdrapi.AgentInfo {
	t.Helper()
	var got herdrapi.AgentInfo
	waitFor(t, 10*time.Second, fmt.Sprintf("agent %s at %s", name, cwd), func() (bool, error) {
		agents, err := api.AgentList()
		if err != nil {
			return false, err
		}
		for _, a := range agents {
			if a.Name == name && a.Cwd == cwd {
				got = a
				return true, nil
			}
		}
		return false, fmt.Errorf("未出現")
	})
	return got
}

func wsrWriteRules(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workspaces.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// wsrTryReadExact はルールファイルの exact map を読む（learn の自動更新待ち
// ループから呼ぶため非 fatal。パースは wsmap.Parse＝実装と同じ意味論で読む）。
func wsrTryReadExact(home string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(home, ".herdr-drover", "workspaces.json"))
	if err != nil {
		return nil, err
	}
	m, err := wsmap.Parse(data)
	if err != nil {
		return nil, err
	}
	return m.Exact, nil
}

func wsrReadExact(t *testing.T, home string) map[string]string {
	t.Helper()
	ex, err := wsrTryReadExact(home)
	if err != nil {
		t.Fatalf("workspaces.json 読取: %v", err)
	}
	return ex
}

// wsrRunBin は実バイナリのサブコマンドを非対話で実行し stdout を返す
// （exit 非 0 は即 FAIL・stderr 込みで報告）。
func wsrRunBin(t *testing.T, bin string, env []string, args ...string) string {
	t.Helper()
	c := exec.Command(bin, args...)
	c.Env = env
	var ob, eb strings.Builder
	c.Stdout, c.Stderr = &ob, &eb
	if err := c.Run(); err != nil {
		t.Fatalf("%s %v 失敗: %v\nstdout:\n%s\nstderr:\n%s", filepath.Base(bin), args, err, ob.String(), eb.String())
	}
	return ob.String()
}

// ============ 1. 完全往復ループ（ルール→着地→手動移動→capture→学習着地→不要） ============

func TestE2EWsRulesLoopRoundTrip(t *testing.T) {
	home := t.TempDir()
	srv, api := startHerdr(t, "HOME="+home, "XDG_STATE_HOME="+t.TempDir())
	bin := buildBinary(t)
	harness := writePtyHarness(t)
	work := physicalDir(t)
	const marker = "HD_E2E_WSRULES_MARK"
	stub := wsrInstallClaudeStub(t, marker)
	env := shimEnv(srv, filepath.Dir(stub), work)

	// 邪魔されない側の既存 Tab: ws "other" の root tab（shell pane 1 枚）
	wsrWSCreate(t, api, "other")

	// --- ① ルール書込: work → proj ---
	wsrWriteRules(t, home, fmt.Sprintf(`{"exact":{%q:"proj"}}`, work))
	base1 := wsrTabCounts(t, api)

	// --- ② シム新規（実 TTY）→ marker（attach 成立）→ Ctrl+B q detach ---
	res := runPtyHarness(t, harness, work, env, [][2]string{{marker, `\x02q`}}, bin, "claude")
	if !strings.Contains(res.stripped, "claude セッションを新規起動しました") {
		t.Fatalf("新規起動経路を通っていない:\n%s", res.stripped)
	}

	// 着地検証: 指定 workspace（proj・ルールから自動作成）に**新 Tab**
	// （pane 1 枚）で生まれ、既存 tab は 1 つも split されていない。
	ag1 := wsrWaitAgent(t, api, "claude", work)
	if got := wsrWorkspaceLabels(t, api)[ag1.WorkspaceID]; got != "proj" {
		t.Fatalf("ルール解決先に着地していない: ws=%s label=%q（want proj）", ag1.WorkspaceID, got)
	}
	cur := wsrTabCounts(t, api)
	if _, existed := base1[ag1.TabID]; existed {
		t.Fatalf("新 Tab でなく既存 Tab %s に着地した", ag1.TabID)
	}
	if cur[ag1.TabID] != 1 {
		t.Fatalf("新 Tab が claude pane 1 枚でない: pane_count=%d", cur[ag1.TabID])
	}
	wsrAssertTabsUnchanged(t, "シム新規 1 本目", base1, cur)
	// capture/organize の検出系照合を決定的にするため検出確立を待つ
	wsrWaitDetected(t, api, work)

	// --- ③ 手動 Tab 移動の模擬: learned へ（ファイル冒頭の注記＝pane.move が
	//     cross-workspace Tab 移動の唯一のプリミティブ） ---
	wsLearned := wsrWSCreate(t, api, "learned")
	moved := wsrPaneMoveNewTab(t, api, ag1.PaneID, wsLearned)
	if moved.WorkspaceID != wsLearned {
		t.Fatalf("手動移動が learned に入っていない: %+v", moved)
	}

	// --- ④ organize --capture（実バイナリ）→ exact ルール上書き学習 ---
	out := wsrRunBin(t, bin, srv.env, "organize", "--capture")
	if !strings.Contains(out, "~ "+work+" → learned") {
		t.Fatalf("capture の上書き差分行が無い:\n%s", out)
	}
	if got := wsrReadExact(t, home)[work]; got != "learned" {
		t.Fatalf("capture 後のルールが learned でない: %q", got)
	}

	// --- ⑤ 同 cwd の新規 2 本目 → 学習先 learned に新 Tab で着地 ---
	// 引数あり×TTY は「常に新規」の設計経路（引数なしだと cwd 一致の既存へ
	// attach する仕様のため、2 本目の新規はこの経路が正）。
	base2 := wsrTabCounts(t, api)
	res2 := runPtyHarness(t, harness, work, env, [][2]string{{marker, `\x02q`}}, bin, "claude", "--wsr-e2e-arg")
	if !strings.Contains(res2.stripped, "claude セッションを新規起動しました") {
		t.Fatalf("2 本目が新規起動経路を通っていない:\n%s", res2.stripped)
	}
	ag2 := wsrWaitAgent(t, api, "claude-2", work)
	if got := wsrWorkspaceLabels(t, api)[ag2.WorkspaceID]; got != "learned" {
		t.Fatalf("2 本目が学習先に着地していない: ws=%s label=%q（want learned）", ag2.WorkspaceID, got)
	}
	cur2 := wsrTabCounts(t, api)
	if _, existed := base2[ag2.TabID]; existed {
		t.Fatalf("2 本目が新 Tab でなく既存 Tab %s に着地した", ag2.TabID)
	}
	if cur2[ag2.TabID] != 1 {
		t.Fatalf("2 本目の新 Tab が claude pane 1 枚でない: pane_count=%d", cur2[ag2.TabID])
	}
	wsrAssertTabsUnchanged(t, "シム新規 2 本目", base2, cur2)
	wsrWaitDetected(t, api, work)

	// --- ⑥ organize --dry-run → 「移動不要」（全 KEEP 配置済・MOVE/CARVE ゼロ） ---
	out = wsrRunBin(t, bin, srv.env, "organize", "--dry-run")
	if strings.Contains(out, "MOVE ") || strings.Contains(out, "CARVE") {
		t.Fatalf("往復完了後に移動が計画された（移動不要のはず）:\n%s", out)
	}
	if !strings.Contains(out, "配置済") {
		t.Fatalf("配置済 KEEP の報告が無い:\n%s", out)
	}
	if !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run 表示が無い:\n%s", out)
	}
}

// ============ 2. herdr 直接起動（name 無し検出）＋間借り pane の正規化 ============

func TestE2EWsRulesDirectStartAndCarveNormalize(t *testing.T) {
	home := t.TempDir()
	srv, api := startHerdr(t, "HOME="+home, "XDG_STATE_HOME="+t.TempDir())
	bin := buildBinary(t)
	stub := wsrInstallClaudeStub(t, "HD_E2E_DIRECT_MARK")

	wsSrc := wsrWSCreate(t, api, "src")
	dirD := physicalDir(t) // herdr 直接起動（name 無し・検出のみ）
	dirM := physicalDir(t) // 旧シム型の間借り（既存 Tab を split）

	// herdr 直接起動相当: layout.apply は agent name を付けない＝pane.list の
	// 検出種別（agent:"claude"）だけが同定手段になる（ユーザーが herdr UI から
	// 手で claude を開いたのと同じ観測面。stub でこの検出を実誘発できることは
	// organize_test の実測どおり＝ここでも wsrWaitDetected が一次確認）。
	tabD, paneD := wsrLayoutApply(t, api, wsSrc, "d", dirD, stub)

	// 旧シム型の間借り: agent.start{workspace_id} は active tab の focused pane
	// を split する（Probe live 実測＝「既存 Tab の表示を邪魔する」機序の再現）。
	agM, err := api.AgentStart("claude", []string{stub}, &herdrapi.AgentStartOptions{Cwd: dirM, WorkspaceID: wsSrc})
	if err != nil {
		t.Fatalf("agent.start（間借り再現）: %v", err)
	}
	wsrWaitDetected(t, api, dirD, dirM)

	// 間借りの物証: agM の Tab は 2 pane（root shell ＋ claude の同居）
	if got := wsrTabCounts(t, api)[agM.TabID]; got != 2 {
		t.Fatalf("間借り状態が再現されていない: tab=%s pane_count=%d（want 2）", agM.TabID, got)
	}

	wsrWriteRules(t, home, fmt.Sprintf(`{"exact":{%q:"projD",%q:"projM"}}`, dirD, dirM))

	// --- dry-run: name 無し検出 pane が MOVE・間借りが CARVE として計画される ---
	out := wsrRunBin(t, bin, srv.env, "organize", "--dry-run")
	var haveMoveD, haveCarveM bool
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "MOVE") && strings.Contains(line, "cwd="+dirD+" ") {
			haveMoveD = true
		}
		if strings.HasPrefix(line, "CARVE") && strings.Contains(line, "cwd="+dirM+" ") {
			haveCarveM = true
		}
	}
	if !haveMoveD {
		t.Fatalf("name 無し検出 pane (cwd=%s) が MOVE 計画に無い:\n%s", dirD, out)
	}
	if !haveCarveM {
		t.Fatalf("間借り pane (cwd=%s) が CARVE 計画に無い:\n%s", dirM, out)
	}
	// dry-run 無変更（pane は動いていない）
	for _, p := range wsrPanes(t, api) {
		if (p.Cwd == dirD || p.Cwd == dirM) && p.WorkspaceID != wsSrc {
			t.Fatalf("dry-run で pane が動いた: %+v", p)
		}
	}

	// --- 実行: MOVE（Tab ごと）＋CARVE（新 Tab へ切り出し・同居温存） ---
	out = wsrRunBin(t, bin, srv.env, "organize")
	if !strings.Contains(out, "移動完了") {
		t.Fatalf("実行結果の報告行が無い:\n%s", out)
	}
	labels := wsrWorkspaceLabels(t, api)
	counts := wsrTabCounts(t, api)
	var pD2, pM2 *wsrPane
	for _, p := range wsrPanes(t, api) {
		p := p
		switch p.Cwd {
		case dirD:
			pD2 = &p
		case dirM:
			pM2 = &p
		}
	}
	if pD2 == nil || labels[pD2.WorkspaceID] != "projD" {
		t.Fatalf("name 無し検出 pane が projD に移動していない: %+v", pD2)
	}
	if counts[pD2.TabID] != 1 {
		t.Fatalf("MOVE 後の Tab が claude 単独でない: pane_count=%d", counts[pD2.TabID])
	}
	if _, still := counts[tabD]; still {
		t.Fatalf("MOVE のソース Tab %s が閉じていない（空 Tab の自動 close が働くはず）", tabD)
	}
	if pM2 == nil || labels[pM2.WorkspaceID] != "projM" {
		t.Fatalf("間借り claude が projM へ切り出されていない: %+v", pM2)
	}
	if counts[pM2.TabID] != 1 {
		t.Fatalf("CARVE 後の新 Tab が claude 単独でない: pane_count=%d", counts[pM2.TabID])
	}
	if got := counts[agM.TabID]; got != 1 {
		t.Fatalf("間借り元 Tab の同居 pane（shell）が温存されていない: pane_count=%d（want 1）", got)
	}
	_ = paneD // 移動で pane_id が変わるため以後は cwd exact-match で追跡（alias 実測に依存しない）

	// --- capture も name 無し検出 pane を対象にする（既存どおり "=" 行の物証） ---
	out = wsrRunBin(t, bin, srv.env, "organize", "--capture", "--dry-run")
	if !strings.Contains(out, "= "+dirD+" → projD") {
		t.Fatalf("capture が name 無し検出 pane を列挙していない:\n%s", out)
	}
	if !strings.Contains(out, "= "+dirM+" → projM") {
		t.Fatalf("capture が正規化済み間借り pane を列挙していない:\n%s", out)
	}
}

// ============ 3. learn_moves=true × 実バイナリ daemon（自動更新＋1 行ログ） ============

func TestE2ELearnMovesDaemonRealBinary(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可（agent は Firestore 必須）")
	}
	home := t.TempDir()
	srv, api := startHerdr(t, "HOME="+home, "XDG_STATE_HOME="+t.TempDir())
	bin := buildBinary(t)
	stub := wsrInstallClaudeStub(t, "HD_E2E_LEARN_MARK")

	// learn_moves=true の opt-in（config.json＝readLearnMoves の実経路。
	// resolveConfig は未知キーを無視するので同居して問題ない）。
	cfgDir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{\"learn_moves\": true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// agent 環境は明示構築で隔離（e2e_test.go TestE2EAgentLifecycle と同じ規律:
	// 外側の資格情報を継承しない・pidfile を隔離 HOME に置く）。
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=demo-hd-learn",
		"PC_ID=e2e-learn-herdr",
		"DROVER_TICK=1s",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
	}
	agent := exec.Command(bin, "agent")
	agent.Env = env
	var stderr, stdout syncBuf
	agent.Stderr = &stderr
	agent.Stdout = &stdout
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- agent.Wait() }()
	agentDead := false
	t.Cleanup(func() {
		if !agentDead {
			_ = agent.Process.Kill()
			<-waitCh
		}
	})
	dead := func() bool {
		select {
		case err := <-waitCh:
			agentDead = true
			t.Fatalf("agent が早期終了: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
			return true
		default:
			return false
		}
	}

	// opt-in が実バイナリで効いている物証（有効化ログ）を先に確認する。
	waitFor(t, 20*time.Second, "learn 有効化ログ", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return strings.Contains(stderr.String(), "learn: live 学習有効"), fmt.Errorf("stderr にまだ無い")
	})

	// claude pane（herdr 検出・name 無し）を作り、検出確立後に手動 Tab 移動。
	wsSrc := wsrWSCreate(t, api, "lsrc")
	wsDst := wsrWSCreate(t, api, "ldst")
	dirB := physicalDir(t)
	_, paneB := wsrLayoutApply(t, api, wsSrc, "lb", dirB, stub)
	wsrWaitDetected(t, api, dirB)
	wsrPaneMoveNewTab(t, api, paneB, wsDst)

	// --- ルールファイル自動更新（daemon の runLearnLoop が書く） ---
	waitFor(t, 20*time.Second, "learn によるルール自動更新", func() (bool, error) {
		if dead() {
			return false, nil
		}
		ex, err := wsrTryReadExact(home)
		if err != nil {
			return false, err
		}
		return ex[dirB] == "ldst", fmt.Errorf("exact=%v", ex)
	})

	// --- 1 行ログ（silent な設定変更の禁止＝書込は必ず・ちょうど 1 行） ---
	logStr := stderr.String()
	if n := strings.Count(logStr, "learn: exact ルール"); n != 1 {
		t.Fatalf("learn のルール書込ログが 1 行でない（%d 行）:\nstderr:\n%s", n, logStr)
	}
	if !strings.Contains(logStr, dirB) || !strings.Contains(logStr, "ldst") {
		t.Fatalf("learn ログに移動内容（%s → ldst）が無い:\nstderr:\n%s", dirB, logStr)
	}

	// --- SIGTERM → graceful 終了（exit 0） ---
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitCh:
		agentDead = true
		if err != nil {
			t.Fatalf("SIGTERM で exit 0 にならない: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 後 15s 経っても agent が終了しない\nstderr:\n%s", stderr.String())
	}
}
