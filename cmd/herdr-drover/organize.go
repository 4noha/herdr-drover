package main

// organize — claude セッションを **Tab 単位**で整理・学習する（確定 UX 仕様）。
//
//	herdr-drover organize [--dry-run]            ルール解決先へ Tab を整理
//	herdr-drover organize --capture [--dry-run]  現配置を exact ルールとして保存
//	（live 学習は agent daemon 内の runLearnLoop。config.json learn_moves=true
//	  のときのみ＝opt-in。既定 false で挙動完全不変）
//
// 設計の根拠（全て実 herdr 0.7.4 の隔離サーバ実測＝Probe 確定事実）:
//   - pane は「1 つの Tab の描画領域の分割」＝organize/capture/learn の単位は
//     Tab（claude pane を含む Tab を丸ごと動かす）。
//   - tab.move は同一 workspace 内 reorder 専用（別 ws への Tab 移動 API は
//     0.7.4 に存在しない）。**pane.move が唯一の移動プリミティブ**で、claude
//     単独 Tab なら pane.move {destination:{type:"new_tab", workspace_id,
//     label}} が実質 Tab 移動（ソース Tab は空になると自動 close・pane の
//     terminal_id/agent name/label は維持を実測）。
//   - 非 claude pane と同居する Tab は **Tab を丸ごと引っ越す**（pane.layout で
//     現トポロジを採取→pane.move で新 Tab へ全 pane を再構築＝連鎖近似・元 Tab は
//     空で自動 close。pane.move は単一 pane しか分割できないため単軸/右偏り連鎖は
//     厳密・複雑な入れ子は連鎖近似。moveWholeTab）。曖昧（1 Tab に別 cwd の claude
//     複数）は skip＋報告。
//   - claude pane の同定は 2 系統 OR・どちらも exact-match（鉄則③）:
//     (a) シム命名: agent 名が claude / claude-N（claudeshim の encode と
//     厳密往復する isClaudeAgentName） (b) herdr 直接起動: pane.list の
//     検出種別 `agent == "claude"`（herdr のプロセス名検出。name 無し＝
//     ユーザーが herdr UI から開いたセッションも取りこぼさない。実測:
//     agent.list は無名の検出 agent も列挙し name キー自体が無い）。
//     両者が矛盾する pane（名は claude 形だが検出種別が別物）は機械確定
//     不能＝対象外＋報告（推測で動かさない）。
//   - herdr の events は新規購読のたびに過去 event のバックログを再送する
//     （実測）＝learn は event を鵜呑みにせず、**購読前 pane 配置 snapshot**
//     （処理済み event で逐次更新）と**ライブ状態**の 2 重 exact 照合で
//     stale を捨てる（誤学習 dedup。ライブ照合だけでは「pane がまだ移動先に
//     居る」再送を新規移動と区別できず、削除済みルールが daemon 再起動で
//     復活する＝レビュー指摘・実再現済）。
//
// wsmap（internal/wsmap）との接続は Load / Resolve(cwd,home) / Save /
// ResolveWorkspaceID の 4 点のみの薄い配線。ルールは exact > 最長 prefix >
// default の決定的解決（wsmap 側の責務）。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

// ============ wire 型（herdrapi 無改変のローカル decode） ============

// orgPane は pane.list / pane_moved event の pane を decode する。
// herdrapi.PaneInfo に無い `agent`（herdr の検出種別・null 可）が要るため
// ローカルに持つ（types.go は本タスクの変更範囲外）。フィールド名は実採取
// JSON と一致（実測: {"pane_id":"w1:p2",...,"agent":"claude",...}）。
type orgPane struct {
	PaneID      string            `json:"pane_id"`
	TerminalID  string            `json:"terminal_id"`
	WorkspaceID string            `json:"workspace_id"`
	TabID       string            `json:"tab_id"`
	Cwd         string            `json:"cwd"`
	Agent       string            `json:"agent"`  // 検出種別（"claude" 等。null は ""）
	Tokens      map[string]string `json:"tokens"` // report_metadata token（inject 判定用・v0.5.5〜）
}

func listPanesWithAgent(api *herdrapi.Client) ([]orgPane, error) {
	raw, err := api.Call("pane.list", nil)
	if err != nil {
		return nil, fmt.Errorf("pane.list: %w", err)
	}
	var out struct {
		Panes []orgPane `json:"panes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("pane_list decode: %w", err)
	}
	return out.Panes, nil
}

// listTabs は tab.list（実採取: {"type":"tab_list","tabs":[TabInfo...]}）。
func listTabs(api *herdrapi.Client) ([]herdrapi.TabInfo, error) {
	raw, err := api.Call("tab.list", nil)
	if err != nil {
		return nil, fmt.Errorf("tab.list: %w", err)
	}
	var out struct {
		Tabs []herdrapi.TabInfo `json:"tabs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tab_list decode: %w", err)
	}
	return out.Tabs, nil
}

func orgListWorkspaces(api *herdrapi.Client) ([]herdrapi.WorkspaceInfo, error) {
	raw, err := api.Call("workspace.list", nil)
	if err != nil {
		return nil, fmt.Errorf("workspace.list: %w", err)
	}
	var out struct {
		Workspaces []herdrapi.WorkspaceInfo `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("workspace_list decode: %w", err)
	}
	return out.Workspaces, nil
}

// paneMoveResult は pane.move 応答の move_result（実採取に一致）。
type paneMoveResult struct {
	Changed        bool              `json:"changed"`
	PreviousPaneID string            `json:"previous_pane_id"`
	PreviousTabID  string            `json:"previous_tab_id"`
	Pane           orgPane           `json:"pane"`
	CreatedTab     *herdrapi.TabInfo `json:"created_tab"`
	ClosedTabID    string            `json:"closed_tab_id"`
}

// paneMoveNewTab は pane.move {destination:{type:"new_tab"}} を発行する。
// label 空は未指定（新 Tab は herdr の未命名＝表示位置番号ラベル）。
// focus:false 固定＝ユーザーの現在フォーカスを奪わない（Probe 実測 params）。
func paneMoveNewTab(api *herdrapi.Client, paneID, workspaceID, label string) (*paneMoveResult, error) {
	dest := struct {
		Type        string `json:"type"`
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label,omitempty"`
	}{"new_tab", workspaceID, label}
	raw, err := api.Call("pane.move", struct {
		PaneID      string `json:"pane_id"`
		Destination any    `json:"destination"`
		Focus       bool   `json:"focus"`
	}{paneID, dest, false})
	if err != nil {
		return nil, err
	}
	var out struct {
		MoveResult paneMoveResult `json:"move_result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("pane_move decode: %w", err)
	}
	return &out.MoveResult, nil
}

// paneMoveIntoTab は pane.move {destination:{type:"tab"}} を発行し、targetPaneID
// を split（"right"|"down"）方向・ratio で分割してその隣へ pane を移す。
func paneMoveIntoTab(api *herdrapi.Client, paneID, tabID, targetPaneID, split string, ratio float64) (*paneMoveResult, error) {
	dest := struct {
		Type         string   `json:"type"`
		TabID        string   `json:"tab_id"`
		TargetPaneID string   `json:"target_pane_id,omitempty"`
		Split        string   `json:"split"`
		Ratio        *float64 `json:"ratio,omitempty"`
	}{"tab", tabID, targetPaneID, split, &ratio}
	raw, err := api.Call("pane.move", struct {
		PaneID      string `json:"pane_id"`
		Destination any    `json:"destination"`
		Focus       bool   `json:"focus"`
	}{paneID, dest, false})
	if err != nil {
		return nil, err
	}
	var out struct {
		MoveResult paneMoveResult `json:"move_result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("pane_move decode: %w", err)
	}
	return &out.MoveResult, nil
}

// chainSplitFrom は読み順で隣り合う 2 pane の rect から、前 pane を分割して後
// pane を付ける向き（right|down）と ratio（前 pane の取り分・clamp）を求める。
// 単軸/右偏り連鎖は厳密・複雑な入れ子は連鎖近似（herdr の pane.move が単一 pane
// しか分割できない構造上、任意の入れ子木は pane.move だけでは厳密再現不能）。
func chainSplitFrom(prev, cur herdrapi.PaneLayoutRect) (string, float64) {
	// 読み順（y,x）ソート済＝cur は prev の右か下。y が重なるなら同行＝right。
	sameRow := cur.Y < prev.Y+prev.Height && cur.Y+cur.Height > prev.Y
	if sameRow && cur.X >= prev.X {
		return "right", clampRatio(float64(prev.Width) / float64(prev.Width+cur.Width))
	}
	return "down", clampRatio(float64(prev.Height) / float64(prev.Height+cur.Height))
}

func clampRatio(r float64) float64 {
	if r < 0.1 {
		return 0.1
	}
	if r > 0.9 {
		return 0.9
	}
	return r
}

// moveWholeTab は claudePaneID が属する Tab の全 pane を wsid の新 Tab へ丸ごと
// 引っ越す（連鎖近似でトポロジ保存）。手順: pane.layout で現トポロジを採取→読み
// 順先頭を new_tab へ→残りを前 pane から split して再構築→元 Tab は空になり自動
// close。**非トランザクション**: 途中失敗は半端状態を残すため loud に報告して即停止。
func moveWholeTab(api *herdrapi.Client, claudePaneID, wsid, carryLabel string, stdout io.Writer) error {
	lay, err := api.PaneLayout(claudePaneID)
	if err != nil {
		return fmt.Errorf("pane.layout: %w", err)
	}
	ps := append([]herdrapi.PaneLayoutPane(nil), lay.Panes...)
	if len(ps) == 0 {
		return fmt.Errorf("pane.layout に pane が無い（tab=%s）", lay.TabID)
	}
	// 読み順（上→下、同行は左→右）に並べる＝再構築の決定的順序。
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].Rect.Y != ps[j].Rect.Y {
			return ps[i].Rect.Y < ps[j].Rect.Y
		}
		return ps[i].Rect.X < ps[j].Rect.X
	})

	// 先頭 pane を新 Tab へ（label 引継ぎ）。
	res, err := paneMoveNewTab(api, ps[0].PaneID, wsid, carryLabel)
	if err != nil {
		return fmt.Errorf("先頭 pane %s の new_tab 移動失敗: %w", ps[0].PaneID, err)
	}
	newTab := ""
	if res.CreatedTab != nil {
		newTab = res.CreatedTab.TabID
	}
	if newTab == "" {
		return fmt.Errorf("new_tab 応答に created_tab が無い（pane=%s）", ps[0].PaneID)
	}
	newID := map[string]string{ps[0].PaneID: res.Pane.PaneID} // 旧→新 pane_id（move で変わる）
	fmt.Fprintf(stdout, "  → 丸ごと引っ越し開始: 先頭 pane %s→%s 新 Tab %s ws=%s\n",
		ps[0].PaneID, res.Pane.PaneID, newTab, wsid)

	prevOld := ps[0].PaneID
	for i := 1; i < len(ps); i++ {
		p := ps[i]
		dir, ratio := chainSplitFrom(ps[i-1].Rect, p.Rect)
		r2, err := paneMoveIntoTab(api, p.PaneID, newTab, newID[prevOld], dir, ratio)
		if err != nil {
			// 半端状態（一部だけ新 Tab へ移動済）を隠さず停止（TODO 3.5 の規律）。
			return fmt.Errorf("pane %s の再構築失敗（半端状態・停止／既に %d/%d 移動済）: %w",
				p.PaneID, i, len(ps), err)
		}
		newID[p.PaneID] = r2.Pane.PaneID
		fmt.Fprintf(stdout, "  → pane %s→%s を %s 分割(ratio=%.2f)で付加\n",
			p.PaneID, r2.Pane.PaneID, dir, ratio)
		prevOld = p.PaneID
	}
	fmt.Fprintf(stdout, "  → 完了: %d pane を新 Tab %s へ引っ越し（元 Tab は空で自動 close）\n", len(ps), newTab)
	return nil
}

// ============ claude pane の同定（organize/capture/learn 共通） ============

// claudeNamesByPane は agent.list から pane_id → agent 名の索引を作る
// （無名の検出 agent は name キー自体が無い実測＝空名は載せない）。
func claudeNamesByPane(agents []herdrapi.AgentInfo) map[string]string {
	m := make(map[string]string, len(agents))
	for _, a := range agents {
		if a.Name != "" {
			m[a.PaneID] = a.Name
		}
	}
	return m
}

// classifyClaudePane は pane が claude セッションかを 2 系統 OR の
// exact-match で判定する（ファイル冒頭コメントの根拠参照）。
// conflict が非空なら「機械確定不能」＝対象外＋報告（推測で動かさない）。
func classifyClaudePane(p orgPane, names map[string]string) (isClaude bool, conflict string) {
	name := names[p.PaneID]
	named := isClaudeAgentName(name)
	detected := p.Agent == "claude"
	if named && p.Agent != "" && !detected {
		return false, fmt.Sprintf("agent 名 %q は claude 形だが herdr 検出種別は %q（矛盾＝機械確定不能）", name, p.Agent)
	}
	return named || detected, ""
}

// ============ organize 計画（純関数＝テーブルテスト対象） ============

type orgPlanItem struct {
	Action     string // "MOVE"（単独 Tab＝Tab ごと）/"MOVE_TAB"（同居 Tab を丸ごと引っ越し）/"KEEP"/"SKIP"
	PaneID     string
	TabID      string
	Cwd        string
	ToLabel    string // 解決先 workspace label（MOVE/MOVE_TAB/KEEP 配置済）
	ToWSID     string // 解決先 workspace_id（""=未存在＝実行時に自動作成）
	CarryLabel string // MOVE で引き継ぐ Tab label（""=引き継がない）
	Reason     string // KEEP/SKIP の理由（報告必須＝silent 禁止）
}

// wsidIndex は label → workspace_id の読み取り専用索引。label 重複は number
// 最小を採る＝wsmap.ResolveWorkspaceID と同じ決定則（dry-run で workspace を
// 作らないための read-only 版。実行時の get-or-create は wsmap 側）。
func wsidIndex(wss []herdrapi.WorkspaceInfo) map[string]string {
	best := map[string]herdrapi.WorkspaceInfo{}
	for _, w := range wss {
		if w.Label == "" {
			continue
		}
		if b, ok := best[w.Label]; !ok || w.Number < b.Number {
			best[w.Label] = w
		}
	}
	out := make(map[string]string, len(best))
	for l, w := range best {
		out[l] = w.WorkspaceID
	}
	return out
}

// carryTabLabel は単独 Tab 移動で引き継ぐ label を決める。herdr の未命名 Tab
// の label は「workspace 内の表示位置番号」で reorder により変わる（Probe
// 実測: "1"→"2"）＝引き継ぐと位置番号が固定 label 化してしまう。判定は
// 「label が自 Tab の表示位置番号（tab.list 順の同 ws 内 1-based index）と
// 一致するか」の機械比較のみ。ユーザーが本当に位置番号と同じ名を付けた
// 稀ケースは引き継がれない（新 Tab でも同表示になるだけ＝実害なし）。
func carryTabLabel(tab herdrapi.TabInfo, tabs []herdrapi.TabInfo) string {
	if tab.Label == "" {
		return ""
	}
	pos := 0
	for _, t := range tabs {
		if t.WorkspaceID != tab.WorkspaceID {
			continue
		}
		pos++
		if t.TabID == tab.TabID {
			break
		}
	}
	if tab.Label == strconv.Itoa(pos) {
		return "" // 未命名（表示位置番号）とみなし引き継がない
	}
	return tab.Label
}

// computeOrganizePlan は organize の計画を決定的に立てる純関数。
// resolve は wsmap の cwd→label 解決（""=ルールなし）。順序は分類矛盾
// （pane.list 順）→ Tab 走査（tab.list 順）＝常に同じ出力。
func computeOrganizePlan(panes []orgPane, tabs []herdrapi.TabInfo, wss []herdrapi.WorkspaceInfo, names map[string]string, resolve func(cwd string) string) []orgPlanItem {
	var plan []orgPlanItem

	claude := map[string]bool{}
	for _, p := range panes {
		ok, conflict := classifyClaudePane(p, names)
		if conflict != "" {
			plan = append(plan, orgPlanItem{Action: "SKIP", PaneID: p.PaneID, TabID: p.TabID, Cwd: p.Cwd, Reason: conflict})
			continue
		}
		if ok {
			claude[p.PaneID] = true
		}
	}

	panesByTab := map[string][]orgPane{}
	claudeByTab := map[string][]orgPane{}
	for _, p := range panes {
		panesByTab[p.TabID] = append(panesByTab[p.TabID], p)
		if claude[p.PaneID] {
			claudeByTab[p.TabID] = append(claudeByTab[p.TabID], p)
		}
	}
	wsids := wsidIndex(wss)

	for _, t := range tabs {
		cl := claudeByTab[t.TabID]
		if len(cl) == 0 {
			continue
		}
		if len(cl) > 1 {
			// 1 Tab に claude 複数は「Tab ごと移動か切り出しか・どの cwd を
			// 基準にするか」が定まらない＝曖昧。cwd が同一でも skip（決定的）。
			descs := make([]string, 0, len(cl))
			for _, p := range cl {
				descs = append(descs, fmt.Sprintf("%s(cwd=%s)", p.PaneID, p.Cwd))
			}
			plan = append(plan, orgPlanItem{Action: "SKIP", TabID: t.TabID,
				Reason: fmt.Sprintf("claude pane が複数（%s）＝Tab 単位の移動先が曖昧", strings.Join(descs, ", "))})
			continue
		}
		p := cl[0]
		label := resolve(p.Cwd)
		if label == "" {
			plan = append(plan, orgPlanItem{Action: "KEEP", PaneID: p.PaneID, TabID: t.TabID, Cwd: p.Cwd,
				Reason: "ルールなし（wsmap 未定義）＝現状維持"})
			continue
		}
		wsid := wsids[label]
		if wsid != "" && wsid == p.WorkspaceID {
			plan = append(plan, orgPlanItem{Action: "KEEP", PaneID: p.PaneID, TabID: t.TabID, Cwd: p.Cwd,
				ToLabel: label, ToWSID: wsid,
				Reason: fmt.Sprintf("既に ws=%s(%s) に配置済", wsid, label)})
			continue
		}
		item := orgPlanItem{PaneID: p.PaneID, TabID: t.TabID, Cwd: p.Cwd, ToLabel: label, ToWSID: wsid}
		item.CarryLabel = carryTabLabel(t, tabs)
		if len(panesByTab[t.TabID]) == 1 {
			item.Action = "MOVE" // claude 単独 Tab ＝ Tab ごと移動（label 引継ぎ）
		} else {
			// 同居 Tab ＝ Tab を丸ごと引っ越す（連鎖近似でトポロジ保存・元 Tab 自動 close）。
			item.Action = "MOVE_TAB"
		}
		plan = append(plan, item)
	}
	return plan
}

// ============ organize 実行 ============

func cmdOrganize(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("organize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	capture := fs.Bool("capture", false, "現配置（claude cwd → workspace label）を exact ルールとして wsmap へ保存")
	dry := fs.Bool("dry-run", false, "計画/差分の表示のみ（herdr・wsmap を一切変更しない）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("余分な引数 %v（organize のフラグは --capture / --dry-run のみ）", fs.Args())
	}

	api := herdrapi.New("")
	if _, err := api.Ping(); err != nil {
		return fmt.Errorf("herdr へ接続できない（socket=%s）: %w", api.SocketPath, err)
	}
	m, err := wsmap.Load()
	if err != nil {
		return err
	}
	panes, err := listPanesWithAgent(api)
	if err != nil {
		return err
	}
	agents, err := api.AgentList()
	if err != nil {
		return fmt.Errorf("agent.list: %w", err)
	}
	wss, err := orgListWorkspaces(api)
	if err != nil {
		return err
	}
	tabs, err := listTabs(api)
	if err != nil {
		return err
	}
	if *capture {
		return runCaptureMode(m, panes, agents, tabs, wss, *dry, stdout)
	}
	return runOrganize(api, m, panes, agents, tabs, wss, *dry, stdout)
}

// wsDesc は解決先 workspace の表示（未存在は「新規作成」を明示）。
func wsDesc(it orgPlanItem) string {
	if it.ToWSID == "" {
		return fmt.Sprintf("(新規作成: %s)", it.ToLabel)
	}
	return fmt.Sprintf("%s(%s)", it.ToWSID, it.ToLabel)
}

func runOrganize(api *herdrapi.Client, m *wsmap.Map, panes []orgPane, agents []herdrapi.AgentInfo, tabs []herdrapi.TabInfo, wss []herdrapi.WorkspaceInfo, dry bool, stdout io.Writer) error {
	home, _ := os.UserHomeDir()
	plan := computeOrganizePlan(panes, tabs, wss, claudeNamesByPane(agents),
		func(cwd string) string { return m.Resolve(cwd, home) })

	if len(plan) == 0 {
		fmt.Fprintf(stdout, "claude セッションが見つからない（整理対象なし）\n")
		return nil
	}
	// 計画を必ず全行報告（silent 禁止）。
	for _, it := range plan {
		switch it.Action {
		case "MOVE":
			carry := ""
			if it.CarryLabel != "" {
				carry = fmt.Sprintf("・label %q 引継ぎ", it.CarryLabel)
			}
			fmt.Fprintf(stdout, "MOVE  pane=%s tab=%s cwd=%s → ws=%s（単独 Tab＝Tab ごと移動%s）\n", it.PaneID, it.TabID, it.Cwd, wsDesc(it), carry)
		case "MOVE_TAB":
			fmt.Fprintf(stdout, "MOVE_TAB pane=%s tab=%s cwd=%s → ws=%s（同居 Tab を丸ごと引っ越し・連鎖近似）\n", it.PaneID, it.TabID, it.Cwd, wsDesc(it))
		case "KEEP":
			fmt.Fprintf(stdout, "KEEP  pane=%s cwd=%s: %s\n", it.PaneID, it.Cwd, it.Reason)
		case "SKIP":
			if it.PaneID != "" {
				fmt.Fprintf(stdout, "SKIP  pane=%s: %s\n", it.PaneID, it.Reason)
			} else {
				fmt.Fprintf(stdout, "SKIP  tab=%s: %s\n", it.TabID, it.Reason)
			}
		}
	}
	if dry {
		fmt.Fprintf(stdout, "（dry-run: 計画表示のみ・herdr/wsmap 無変更）\n")
		return nil
	}

	failures := 0
	created := map[string]string{} // 実行中に作成/解決した label→wsid（同 label 二重作成防止）
	for _, it := range plan {
		if it.Action != "MOVE" && it.Action != "MOVE_TAB" {
			continue
		}
		wsid := it.ToWSID
		if wsid == "" {
			if id, ok := created[it.ToLabel]; ok {
				wsid = id
			} else {
				id, err := wsmap.ResolveWorkspaceID(api, it.ToLabel) // get-or-create（focus 非奪取）
				if err != nil {
					failures++
					fmt.Fprintf(stdout, "  → エラー: pane=%s の解決先 workspace %q: %v\n", it.PaneID, it.ToLabel, err)
					continue
				}
				created[it.ToLabel] = id
				wsid = id
			}
		}
		if it.Action == "MOVE_TAB" {
			// 同居 Tab は丸ごと引っ越し（moveWholeTab が per-pane で報告）。
			if err := moveWholeTab(api, it.PaneID, wsid, it.CarryLabel, stdout); err != nil {
				failures++
				fmt.Fprintf(stdout, "  → エラー: tab=%s の丸ごと引っ越し失敗: %v\n", it.TabID, err)
			}
			continue
		}
		res, err := paneMoveNewTab(api, it.PaneID, wsid, it.CarryLabel)
		if err != nil {
			failures++
			fmt.Fprintf(stdout, "  → エラー: pane=%s の移動失敗: %v\n", it.PaneID, err)
			continue
		}
		// 実行結果は id 変化含め 1 行ずつ報告（silent 禁止）。
		newTab := ""
		if res.CreatedTab != nil {
			newTab = res.CreatedTab.TabID
		}
		closed := ""
		if res.ClosedTabID != "" {
			closed = fmt.Sprintf("（旧 Tab %s は閉鎖）", res.ClosedTabID)
		}
		fmt.Fprintf(stdout, "  → 移動完了: pane %s→%s tab %s→%s ws=%s%s\n",
			it.PaneID, res.Pane.PaneID, it.TabID, newTab, wsid, closed)
	}
	if failures > 0 {
		return fmt.Errorf("%d 件の移動が失敗（上の報告行参照）", failures)
	}
	return nil
}

// ============ capture ============

type captureItem struct {
	Cwd   string
	Label string
	Skip  string // 非空 = skip 理由（曖昧・label 無し）
}

// computeCapture は「claude pane の cwd → その Tab の workspace label」を
// 決定的に列挙する純関数。同一 cwd が複数 workspace に散る場合は曖昧＝skip
// （どちらが意図か推測しない）。label の無い workspace はルール化不能＝skip。
func computeCapture(panes []orgPane, claude map[string]bool, wss []herdrapi.WorkspaceInfo) []captureItem {
	labelByWS := make(map[string]string, len(wss))
	for _, w := range wss {
		labelByWS[w.WorkspaceID] = w.Label
	}
	byCwd := map[string][]orgPane{}
	for _, p := range panes {
		if claude[p.PaneID] && p.Cwd != "" {
			byCwd[p.Cwd] = append(byCwd[p.Cwd], p)
		}
	}
	cwds := make([]string, 0, len(byCwd))
	for c := range byCwd {
		cwds = append(cwds, c)
	}
	sort.Strings(cwds) // 決定的順序

	var out []captureItem
	for _, cwd := range cwds {
		wsSet := map[string]bool{}
		for _, p := range byCwd[cwd] {
			wsSet[p.WorkspaceID] = true
		}
		ids := make([]string, 0, len(wsSet))
		for id := range wsSet {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > 1 {
			descs := make([]string, 0, len(ids))
			for _, id := range ids {
				descs = append(descs, fmt.Sprintf("%s(%s)", id, labelByWS[id]))
			}
			out = append(out, captureItem{Cwd: cwd, Skip: fmt.Sprintf("曖昧（複数 workspace に散在: %s）", strings.Join(descs, ", "))})
			continue
		}
		label, skip := captureLabelFor(wss, ids[0])
		if skip != "" {
			out = append(out, captureItem{Cwd: cwd, Skip: skip})
			continue
		}
		out = append(out, captureItem{Cwd: cwd, Label: label})
	}
	return out
}

// captureLabelFor は workspace wsid の label を「ルールの値」として返す
// （capture / learn 共通の単一判定）。skip 非空はルール化不能の理由:
//   - label 無し: ルールの語彙が存在しない。
//   - label 重複（herdr は重複を許容＝Probe 実測・wsmap.go 明記）: organize /
//     ResolveWorkspaceID は number 最小の同名 workspace を採る決定則のため、
//     重複 label をルール化すると直後の organize がユーザー配置を**別の**
//     同名 workspace へ移動する（capture「現配置の保存」の直後に配置が
//     壊れる＝非冪等。レビュー指摘・旧コードで実再現済）。label はルールの
//     語彙なので重複は原理的に曖昧＝既存の曖昧 skip と同じ流儀で理由報告する。
func captureLabelFor(wss []herdrapi.WorkspaceInfo, wsid string) (label, skip string) {
	for _, w := range wss {
		if w.WorkspaceID == wsid {
			label = w.Label
			break
		}
	}
	if label == "" {
		return "", fmt.Sprintf("workspace %s に label が無い（ルール化不能）", wsid)
	}
	dup := 0
	for _, w := range wss {
		if w.Label == label {
			dup++
		}
	}
	if dup > 1 {
		return "", fmt.Sprintf("workspace label %q が %d 個の workspace で重複（organize は number 最小の同名 workspace へ解決し配置が壊れる＝ルール化不能）", label, dup)
	}
	return label, ""
}

// findExactKey は cwd と同一パスへ展開される既存 exact キーを探す。
// wsmap.Resolve と同じ ~ 展開・Clean 意味論（expandRulePath）で突合せ、
// **既存キーの書式を保持したまま上書き**する（ユーザーの "~/x" 表記を
// silent に abs へ書き換えない＝鉄則④）。無ければキーは cwd そのもの。
// 複数キーが同一パスへ潰れる病的ケースは辞書順先頭を採る（決定的）。
func findExactKey(m *wsmap.Map, cwd string) (old string, existed bool, key string) {
	home, _ := os.UserHomeDir()
	cwd = filepath.Clean(cwd)
	keys := make([]string, 0, len(m.Exact))
	for k := range m.Exact {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if expandRulePath(k, home) == cwd {
			return m.Exact[k], true, k
		}
	}
	return "", false, cwd
}

// expandRulePath は wsmap の展開規則（"~"/"~/x" → home 配下・Clean）を
// 突合せ用途に写したもの（wsmap 側は非公開。規則が変わったら wsmap の
// テーブルテストと本ファイルの capture 実テストの双方が落ちて検知される）。
func expandRulePath(p, home string) string {
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	return filepath.Clean(p)
}

func runCaptureMode(m *wsmap.Map, panes []orgPane, agents []herdrapi.AgentInfo, tabs []herdrapi.TabInfo, wss []herdrapi.WorkspaceInfo, dry bool, stdout io.Writer) error {
	names := claudeNamesByPane(agents)
	claude := map[string]bool{}
	for _, p := range panes {
		ok, conflict := classifyClaudePane(p, names)
		if conflict != "" {
			fmt.Fprintf(stdout, "SKIP pane=%s: %s\n", p.PaneID, conflict)
			continue
		}
		if ok {
			claude[p.PaneID] = true
		}
	}
	items := computeCapture(panes, claude, wss)
	// 注入 pane の (pc, short_dir) → label 配置も同時に capture（v0.5.5〜）。
	// リモート pane 注入は自 PC の claude ではないので上記 items には含まれない。
	// Tab label（"↗<short_dir>"）と token（inj_pc）から (pc, short_dir) → label を導き出す。
	injItems := computeCaptureInject(panes, tabs, wss)
	if len(items) == 0 && len(injItems) == 0 {
		fmt.Fprintf(stdout, "capture 対象なし（claude セッションも注入 pane も見つからない）\n")
		return nil
	}
	if dry {
		// dry-run は先読み済みの m に対する差分表示のみ（無ロック・無変更）。
		applyCaptureItems(m, items, true, stdout)
		applyCaptureInjectItems(m, injItems, true, stdout)
		fmt.Fprintf(stdout, "（dry-run: 差分表示のみ・wsmap 無変更）\n")
		return nil
	}
	// 実書込は wsmap.Update（flock 下で再 Load→自分の items のみ適用→Save）。
	// 先読みの m へ適用して全量 Save すると、Load〜Save の窓（pane/agent/ws
	// snapshot 取得を挟む）に learn daemon が書いたルールを stale 全量で
	// 巻き戻す lost update になる（レビュー指摘・旧コードで実再現済）。
	// 差分表示も flock 下の fresh に対して行う＝保存内容と表示が常に一致。
	changes, injChanges := 0, 0
	if err := wsmap.Update(func(fresh *wsmap.Map) (bool, error) {
		changes = applyCaptureItems(fresh, items, false, stdout)
		injChanges = applyCaptureInjectItems(fresh, injItems, false, stdout)
		return changes+injChanges > 0, nil
	}); err != nil {
		return fmt.Errorf("wsmap 保存: %w", err)
	}
	if changes+injChanges == 0 {
		fmt.Fprintf(stdout, "wsmap 変更なし\n")
		return nil
	}
	path, _ := wsmap.Path()
	if injChanges == 0 {
		fmt.Fprintf(stdout, "wsmap へ exact %d 件保存（%s）\n", changes, path)
	} else if changes == 0 {
		fmt.Fprintf(stdout, "wsmap へ inject_placement %d 件保存（%s）\n", injChanges, path)
	} else {
		fmt.Fprintf(stdout, "wsmap へ exact %d 件・inject_placement %d 件保存（%s）\n", changes, injChanges, path)
	}
	return nil
}

// captureInjectItem は注入 pane 1 個の (pc, short_dir) → label 保存候補。
// Skip 非空はルール化不能の理由（曖昧・label 無し等）。
type captureInjectItem struct {
	PC       string
	ShortDir string
	Label    string
	Skip     string
}

// computeCaptureInject は注入 pane の実配置から (pc, short_dir) → label を導く純関数。
// リモート pane の short_dir は Tab label（reconcile が "↗<short_dir>" で作る）から
// 抜き出す。pane.list に token あり／Tab label が "↗" 始まり／workspace に label あり
// の 3 条件を満たすものだけを候補にする（推測しない＝鉄則③）。
// 同 (pc, short_dir) が複数 workspace に散っていれば skip（曖昧）。
func computeCaptureInject(panes []orgPane, tabs []herdrapi.TabInfo, wss []herdrapi.WorkspaceInfo) []captureInjectItem {
	labelByWS := make(map[string]string, len(wss))
	for _, w := range wss {
		labelByWS[w.WorkspaceID] = w.Label
	}
	labelByTab := make(map[string]string, len(tabs))
	for _, t := range tabs {
		labelByTab[t.TabID] = t.Label
	}
	// (pc, short_dir) -> workspace_id set（曖昧検出用）
	byKey := map[[2]string]map[string]bool{}
	for _, p := range panes {
		pc := p.Tokens[injTokPC]
		if pc == "" {
			continue // 注入 pane でない
		}
		tabLabel := labelByTab[p.TabID]
		if !strings.HasPrefix(tabLabel, "↗") {
			continue // 手動リネームされた等・"↗" prefix を持たない Tab は capture 対象外
		}
		shortDir := strings.TrimPrefix(tabLabel, "↗")
		if shortDir == "" {
			continue
		}
		key := [2]string{pc, shortDir}
		if byKey[key] == nil {
			byKey[key] = map[string]bool{}
		}
		byKey[key][p.WorkspaceID] = true
	}
	// 決定的順序（pc → short_dir 昇順）。
	keys := make([][2]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]captureInjectItem, 0, len(keys))
	for _, k := range keys {
		wsIDs := byKey[k]
		ids := make([]string, 0, len(wsIDs))
		for id := range wsIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > 1 {
			descs := make([]string, 0, len(ids))
			for _, id := range ids {
				descs = append(descs, fmt.Sprintf("%s(%s)", id, labelByWS[id]))
			}
			out = append(out, captureInjectItem{PC: k[0], ShortDir: k[1], Skip: fmt.Sprintf("曖昧（複数 workspace に散在: %s）", strings.Join(descs, ", "))})
			continue
		}
		label, skip := captureLabelFor(wss, ids[0])
		if skip != "" {
			out = append(out, captureInjectItem{PC: k[0], ShortDir: k[1], Skip: skip})
			continue
		}
		out = append(out, captureInjectItem{PC: k[0], ShortDir: k[1], Label: label})
	}
	return out
}

// applyCaptureInjectItems は inject_placement 差分を必ず表示（+ 新規 / ~ 上書き /
// = 既存どおり / SKIP）し、dry でなければ m へ適用して変更数を返す。
// exact / rules / default は不変（本関数は inject_placement のみ触る）。
func applyCaptureInjectItems(m *wsmap.Map, items []captureInjectItem, dry bool, stdout io.Writer) int {
	changes := 0
	for _, it := range items {
		key := fmt.Sprintf("[inj] %s / %s", it.PC, it.ShortDir)
		if it.Skip != "" {
			fmt.Fprintf(stdout, "SKIP %s: %s\n", key, it.Skip)
			continue
		}
		var old string
		var existed bool
		if m.InjectPlacement != nil {
			if byDir, ok := m.InjectPlacement[it.PC]; ok {
				old, existed = byDir[it.ShortDir]
			}
		}
		switch {
		case existed && old == it.Label:
			fmt.Fprintf(stdout, "= %s → %s（既存どおり）\n", key, it.Label)
		case existed:
			fmt.Fprintf(stdout, "~ %s → %s（旧: %s を上書き）\n", key, it.Label, old)
			changes++
			if !dry {
				m.InjectPlacement[it.PC][it.ShortDir] = it.Label
			}
		default:
			fmt.Fprintf(stdout, "+ %s → %s（新規）\n", key, it.Label)
			changes++
			if !dry {
				if m.InjectPlacement == nil {
					m.InjectPlacement = map[string]map[string]string{}
				}
				if m.InjectPlacement[it.PC] == nil {
					m.InjectPlacement[it.PC] = map[string]string{}
				}
				m.InjectPlacement[it.PC][it.ShortDir] = it.Label
			}
		}
	}
	return changes
}

// applyCaptureItems は capture 差分を必ず表示（+ 新規 / ~ 上書き / = 既存
// どおり / SKIP）し、dry でなければ m へ適用して変更数を返す。exact のみ
// 触り prefix ルール・default は不変（capture の契約）。
func applyCaptureItems(m *wsmap.Map, items []captureItem, dry bool, stdout io.Writer) int {
	changes := 0
	for _, it := range items {
		if it.Skip != "" {
			fmt.Fprintf(stdout, "SKIP %s: %s\n", it.Cwd, it.Skip)
			continue
		}
		old, existed, key := findExactKey(m, it.Cwd)
		switch {
		case existed && old == it.Label:
			fmt.Fprintf(stdout, "= %s → %s（既存どおり）\n", it.Cwd, it.Label)
		case existed:
			fmt.Fprintf(stdout, "~ %s → %s（旧: %s・キー %q を上書き）\n", it.Cwd, it.Label, old, key)
			changes++
			if !dry {
				m.Exact[key] = it.Label
			}
		default:
			fmt.Fprintf(stdout, "+ %s → %s（新規）\n", it.Cwd, it.Label)
			changes++
			if !dry {
				if m.Exact == nil {
					m.Exact = map[string]string{}
				}
				m.Exact[key] = it.Label
			}
		}
	}
	return changes
}

// ============ live 学習（agent daemon から起動・opt-in） ============

// readLearnMoves は config.json の learn_moves を読む（env 無し・既定 false
// ＝未設定なら挙動完全不変）。ファイル不在は false・壊れた JSON はエラー
// （silent に無効化しない）。resolveConfig の 4 キーとは独立の opt-in トグル
// なので config.go の fileConfig には足さず本ファイルで読む。
func readLearnMoves() (bool, error) {
	path, err := configFilePath()
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var c struct {
		LearnMoves bool `json:"learn_moves"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return false, fmt.Errorf("設定ファイル %s が壊れている（JSON 解析失敗）: %w", path, err)
	}
	return c.LearnMoves, nil
}

// paneMovedData は pane_moved event の data（Probe で全フィールド実捕捉:
// previous_pane_id/previous_workspace_id/previous_tab_id/pane/created_tab/
// closed_tab_id）。学習に要る分だけ decode する。
type paneMovedData struct {
	PreviousWorkspaceID string  `json:"previous_workspace_id"`
	Pane                orgPane `json:"pane"`
}

// paneLoc はバックログ照合 snapshot の配置（workspace_id と cwd の exact 組）。
type paneLoc struct {
	ws  string
	cwd string
}

// runLearnLoop は pane.moved を購読し、ユーザーの手動 Tab 移動（cross-
// workspace の claude pane 移動）を exact ルールへ自動反映する。ctx 終了まで
// 戻らない。購読の再接続・バックログ再送は herdrapi.Subscribe の実測仕様＝
// handleLearnEvent が snapshot 照合＋ライブ状態照合で stale を捨てる。
func runLearnLoop(ctx context.Context, api *herdrapi.Client, lg *log.Logger) error {
	// バックログ照合 snapshot: 購読前の pane 配置。herdr は新規購読のたびに
	// サーバ稼働中の全 event を再送する（実測）ため、「event の移動後配置が
	// snapshot に既にあった配置と exact 一致」なら過去の移動の再送＝学習
	// しない。ライブ照合（PaneGet）だけでは「pane がまだ移動先に居る」再送を
	// 新規移動と区別できず、ユーザーが削除したルールが daemon 再起動のたびに
	// 復活する実バグがあった（レビュー指摘・旧コードで実再現済）。snapshot は
	// 処理済み event で逐次更新する＝Subscribe の内部再接続の再送も既知配置
	// として落ち、切断中に起きた移動の再送だけが学習される。
	// pane.list 失敗時は照合不能のまま学習を始めない（誤学習より loud な停止）。
	panes, err := listPanesWithAgent(api)
	if err != nil {
		return fmt.Errorf("learn: 起動時 pane.list 失敗（バックログ照合不能のため学習を開始しない）: %w", err)
	}
	snap := make(map[string]paneLoc, len(panes))
	for _, p := range panes {
		snap[p.PaneID] = paneLoc{p.WorkspaceID, p.Cwd}
	}
	ch := make(chan herdrapi.Event, 64)
	done := make(chan error, 1)
	go func() { done <- api.Subscribe(ctx, []string{"pane.moved"}, ch) }()
	for {
		select {
		case err := <-done:
			return err
		case ev := <-ch:
			handleLearnEvent(api, ev, snap, lg)
		}
	}
}

// handleLearnEvent は pane_moved 1 件を検査し、学習できる場合のみ exact
// ルールを書き込む（書込は 1 行ログ必須・同値は書込もログもしない）。
// snap は runLearnLoop 所有のバックログ照合 snapshot（ループ 1 goroutine
// からの逐次呼び出しのみ＝排他不要）で、判定確定した event の移動後配置を
// 本関数が逐次追記する。
func handleLearnEvent(api *herdrapi.Client, ev herdrapi.Event, snap map[string]paneLoc, lg *log.Logger) {
	if ev.Name != "pane_moved" { // 配信名は underscore 形（実測の非対称）
		return
	}
	var d paneMovedData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		lg.Printf("learn: pane_moved decode 失敗: %v", err)
		return
	}
	// 同 workspace 内の reorder / 情報不足は学習対象外。
	if d.PreviousWorkspaceID == "" || d.PreviousWorkspaceID == d.Pane.WorkspaceID || d.Pane.Cwd == "" {
		return
	}
	// バックログ照合（exact-match）: event の移動後配置が snapshot に既に
	// あった配置と一致＝過去の移動の再送＝学習しない（runLearnLoop 参照）。
	// pane_id で引く: cross-workspace の pane.move は新 pane_id を発行する
	// （実測: previous_pane_id が別途付く）ため、購読後の新規移動が古い
	// snapshot エントリに誤一致することはない。
	if loc, ok := snap[d.Pane.PaneID]; ok && loc.ws == d.Pane.WorkspaceID && loc.cwd == d.Pane.Cwd {
		return
	}
	// claude 同定（organize/capture と同一の 2 系統 OR）。
	agents, err := api.AgentList()
	if err != nil {
		lg.Printf("learn: agent.list 失敗（この移動は学習しない）: %v", err)
		return
	}
	ok, conflict := classifyClaudePane(d.Pane, claudeNamesByPane(agents))
	if conflict != "" {
		lg.Printf("learn: skip pane=%s: %s", d.Pane.PaneID, conflict)
		return
	}
	if !ok {
		return // 非 claude pane の移動は対象外（ログも出さない＝ノイズ回避）
	}
	// バックログ再送の誤学習 dedup: ライブ状態と exact 照合。herdr は新規
	// 購読のたびに過去 event を再送する（実測）ため、イベント時点の配置が
	// 現況と一致する場合のみ学習する。旧 pane_id は herdr が alias 解決する
	//（実測）ので移動を重ねた pane でも現況が引ける。
	cur, err := api.PaneGet(d.Pane.PaneID)
	if err != nil {
		lg.Printf("learn: skip pane=%s: 現況取得不能（stale バックログの可能性）: %v", d.Pane.PaneID, err)
		return
	}
	if cur.WorkspaceID != d.Pane.WorkspaceID || cur.Cwd != d.Pane.Cwd {
		return // stale（イベント後にさらに動いた/別 cwd）＝学習しない
	}
	// 移動先 workspace の label（ルールの値）。label 無し・label 重複は
	// ルール化不能（capture と同一の captureLabelFor 判定＝重複 label を
	// ルール化すると次の organize が手動配置を number 最小の同名 workspace
	// へ差し戻す。レビュー指摘・旧コードで実再現済）。
	wss, err := orgListWorkspaces(api)
	if err != nil {
		// 一過性エラー＝snapshot 未更新のまま戻る（再送で再挑戦できる）。
		lg.Printf("learn: workspace.list 失敗（この移動は学習しない）: %v", err)
		return
	}
	// ここまでで判定材料が確定＝以後どの結末でも snapshot へ記録し、同一
	// event の再送（Subscribe の内部再接続）をバックログとして落とす。
	snap[d.Pane.PaneID] = paneLoc{d.Pane.WorkspaceID, d.Pane.Cwd}
	label, skip := captureLabelFor(wss, d.Pane.WorkspaceID)
	if skip != "" {
		lg.Printf("learn: skip pane=%s: 移動先 %s", d.Pane.PaneID, skip)
		return
	}

	// wsmap への書込は Update（flock）経由＝自分のキーだけを mutate する:
	// capture / 他 learn との並行で lost update しない（レビュー指摘）。
	var old, key string
	var existed bool
	if err := wsmap.Update(func(m *wsmap.Map) (bool, error) {
		old, existed, key = findExactKey(m, d.Pane.Cwd)
		if existed && old == label {
			return false, nil // 同値＝書込しない
		}
		if m.Exact == nil {
			m.Exact = map[string]string{}
		}
		m.Exact[key] = label
		return true, nil
	}); err != nil {
		lg.Printf("learn: wsmap 更新失敗: %v", err)
		return
	}
	if existed && old == label {
		return // 同値＝書込もログもしない（再送で毎回同じ event が来ても無害）
	}
	// ルール書込は 1 行ログ必須（silent な設定変更の禁止）。
	if existed {
		lg.Printf("learn: exact ルール %s → %s（旧: %s・pane=%s の Tab 移動から学習）", d.Pane.Cwd, label, old, d.Pane.PaneID)
	} else {
		lg.Printf("learn: exact ルール %s → %s（新規・pane=%s の Tab 移動から学習）", d.Pane.Cwd, label, d.Pane.PaneID)
	}
}
