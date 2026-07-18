package main

// mv-tab — Tab 単位の対 Workspace 引っ越し（右クリック相当・対話ピッカ）。
//
//	herdr-drover mv-tab                       対話ピッカ（src Tab→dst WS を stdin で選ぶ・TTY 必須）
//	herdr-drover mv-tab --src-tab <id> --dst-ws <id>
//	                                          非対話（テスト／スクリプト向け・TTY 不要）
//	herdr-drover mv-tab-launch                plugin action `mv-tab` の実体。新 Tab に
//	                                          launcher pane を作り、その中で `mv-tab` を
//	                                          対話モードで走らせる（drawer は非 TTY spawn
//	                                          で対話不可＝TTY 内へ迂回する）
//
// 実引越しは organize.go の moveWholeTab（TODO.md 3.5 の合成手順・pane.layout→
// pane.move new_tab→pane.move tab で残り pane 再構築・空 Tab は herdr が自動 close）。
// 成功後は workspace.focus + tab.focus で新 Tab へ自動フォーカス（ユーザー観測用）。
//
// launcher の pane 生成は claudeshim.applyClaudeTab と同じ layout.apply 経路
// （focus:true・split しない）＝実測済みの唯一の「指定 workspace に新 tab＋argv 直接
// 実行 pane 1 枚」正規 API。

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// cmdMvTab は対話 or フラグ指定の 2 モード。
func cmdMvTab(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mv-tab", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var srcTab, dstWS string
	fs.StringVar(&srcTab, "src-tab", "", "移動元 tab_id（非対話。省略時は対話ピッカ）")
	fs.StringVar(&dstWS, "dst-ws", "", "移動先 workspace_id（非対話。省略時は対話ピッカ）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("使い方: herdr-drover mv-tab [--src-tab <id> --dst-ws <id>]")
	}
	interactive := srcTab == "" || dstWS == ""

	api := herdrapi.New("")

	if interactive {
		if !stdinIsTTY() {
			return fmt.Errorf("対話ピッカは TTY が必要です。plugin action `mv-tab` 経由（launcher pane 内）または通常のシェル pane から起動してください。非対話モードは --src-tab <id> --dst-ws <id> を指定してください")
		}
		var err error
		if srcTab == "" {
			srcTab, err = pickSrcTab(api, stdin, stdout)
			if err != nil {
				return err
			}
		}
		if dstWS == "" {
			// src Tab の workspace_id を除外して選ばせる（同 WS 移動は無意味）。
			curWS, err := workspaceOfTab(api, srcTab)
			if err != nil {
				return err
			}
			dstWS, err = pickDstWS(api, curWS, stdin, stdout)
			if err != nil {
				return err
			}
		}
	}

	return runMvTabMove(api, srcTab, dstWS, stdout)
}

// runMvTabMove は src Tab を dst WS へ丸ごと引っ越し、成功後に自動フォーカスする。
// 対話・非対話・launcher いずれからも同一のロジックを通す（silent 分岐禁止）。
func runMvTabMove(api *herdrapi.Client, srcTabID, dstWSID string, stdout io.Writer) error {
	tabs, err := listTabs(api)
	if err != nil {
		return err
	}
	var src *herdrapi.TabInfo
	for i := range tabs {
		if tabs[i].TabID == srcTabID {
			src = &tabs[i]
			break
		}
	}
	if src == nil {
		return fmt.Errorf("src tab %s が見つからない（tab.list に不在）", srcTabID)
	}
	if src.WorkspaceID == dstWSID {
		return fmt.Errorf("src と dst が同一 WS（%s）＝移動不要", dstWSID)
	}

	// 引越し対象の pane を選ぶ（moveWholeTab は「その pane が属する Tab の
	// 全 pane を wsid の新 Tab へ丸ごと」動かす仕様＝tab の任意の pane で OK）。
	panes, err := listPanesWithAgent(api)
	if err != nil {
		return err
	}
	var claudePaneID string
	for _, p := range panes {
		if p.TabID == srcTabID {
			claudePaneID = p.PaneID
			break
		}
	}
	if claudePaneID == "" {
		return fmt.Errorf("tab %s に pane が無い（自動 close 済み？）", srcTabID)
	}

	fmt.Fprintf(stdout, "mv-tab: moving tab=%s label=%q ws=%s → ws=%s\n",
		src.TabID, src.Label, src.WorkspaceID, dstWSID)
	if err := moveWholeTab(api, claudePaneID, dstWSID, src.Label, stdout); err != nil {
		return err
	}
	// 新 Tab の tab_id を実解決してフォーカスを飛ばす（ユーザー観測性）。
	// pane.move の応答から新 tab_id は取れているが、moveWholeTab は
	// stdout ログにしか流していないので pane.get で再解決する（pane.move
	// 応答の新 pane_id は known だが本 wrapper に返っていないため）。
	newPane, err := api.PaneGet(claudePaneID)
	if err != nil {
		// 引越し自体は成功しているので focus 失敗は warn に留める（silent
		// 分岐禁止＝1 行 loud に出す）。
		fmt.Fprintf(stdout, "mv-tab: warn: 移動後の pane.get 失敗＝自動フォーカスを skip: %v\n", err)
		return nil
	}
	if err := focusTab(api, newPane.WorkspaceID, newPane.TabID); err != nil {
		fmt.Fprintf(stdout, "mv-tab: warn: 自動フォーカス失敗（引越し自体は成功）: %v\n", err)
		return nil
	}
	fmt.Fprintf(stdout, "mv-tab: focused ws=%s tab=%s\n", newPane.WorkspaceID, newPane.TabID)
	return nil
}

// focusTab は workspace.focus → tab.focus の 2 段（herdr の focus は workspace と
// tab で独立している＝両方叩かないと WS を切替えても以前 focus した tab に戻らない）。
func focusTab(api *herdrapi.Client, wsID, tabID string) error {
	if _, err := api.Call("workspace.focus", struct {
		WorkspaceID string `json:"workspace_id"`
	}{wsID}); err != nil {
		return fmt.Errorf("workspace.focus: %w", err)
	}
	if _, err := api.Call("tab.focus", struct {
		TabID string `json:"tab_id"`
	}{tabID}); err != nil {
		return fmt.Errorf("tab.focus: %w", err)
	}
	return nil
}

// workspaceOfTab は tab_id から workspace_id を引く（dst 選択時の除外用）。
func workspaceOfTab(api *herdrapi.Client, tabID string) (string, error) {
	tabs, err := listTabs(api)
	if err != nil {
		return "", err
	}
	for _, t := range tabs {
		if t.TabID == tabID {
			return t.WorkspaceID, nil
		}
	}
	return "", fmt.Errorf("tab %s が見つからない", tabID)
}

// pickSrcTab は tab を workspace ごとにグループ表示し、番号で選ばせる。
// exit=0 のキャンセルは受けない（対話ピッカは Ctrl+C で落ちる＝それは異常 exit で明示）。
func pickSrcTab(api *herdrapi.Client, stdin io.Reader, stdout io.Writer) (string, error) {
	tabs, err := listTabs(api)
	if err != nil {
		return "", err
	}
	wss, err := orgListWorkspaces(api)
	if err != nil {
		return "", err
	}
	if len(tabs) == 0 {
		return "", fmt.Errorf("tab が 1 つも無い")
	}
	// WS 順 → tab number 順で決定的に並べる（毎回番号が同じ＝muscle memory 可）。
	wsLabel := make(map[string]string, len(wss))
	wsNumber := make(map[string]int, len(wss))
	for _, w := range wss {
		wsLabel[w.WorkspaceID] = w.Label
		wsNumber[w.WorkspaceID] = w.Number
	}
	sort.SliceStable(tabs, func(i, j int) bool {
		if tabs[i].WorkspaceID != tabs[j].WorkspaceID {
			return wsNumber[tabs[i].WorkspaceID] < wsNumber[tabs[j].WorkspaceID]
		}
		return tabs[i].Number < tabs[j].Number
	})

	fmt.Fprintf(stdout, "\n移動元 Tab を選択:\n")
	lastWS := ""
	for i, t := range tabs {
		if t.WorkspaceID != lastWS {
			fmt.Fprintf(stdout, "  [ws=%s label=%q]\n", t.WorkspaceID, wsLabel[t.WorkspaceID])
			lastWS = t.WorkspaceID
		}
		focus := " "
		if t.Focused {
			focus = "*"
		}
		fmt.Fprintf(stdout, "  %s %2d) %s  #%d  %q  (pane_count=%d)\n",
			focus, i+1, t.TabID, t.Number, t.Label, t.PaneCount)
	}
	n, err := promptChoice(stdin, stdout, "番号を入力 (1-"+strconv.Itoa(len(tabs))+"): ", 1, len(tabs))
	if err != nil {
		return "", err
	}
	return tabs[n-1].TabID, nil
}

// pickDstWS は src の workspace を除外して WS を選ばせる。
func pickDstWS(api *herdrapi.Client, excludeWSID string, stdin io.Reader, stdout io.Writer) (string, error) {
	wss, err := orgListWorkspaces(api)
	if err != nil {
		return "", err
	}
	// 決定的順序（number 昇順）で並べ、excludeWSID を除外。
	sort.SliceStable(wss, func(i, j int) bool { return wss[i].Number < wss[j].Number })
	filtered := make([]herdrapi.WorkspaceInfo, 0, len(wss))
	for _, w := range wss {
		if w.WorkspaceID == excludeWSID {
			continue
		}
		filtered = append(filtered, w)
	}
	if len(filtered) == 0 {
		return "", fmt.Errorf("移動先 WS が無い（src と同じ WS %s しかない）", excludeWSID)
	}
	fmt.Fprintf(stdout, "\n移動先 Workspace を選択:\n")
	for i, w := range filtered {
		focus := " "
		if w.Focused {
			focus = "*"
		}
		fmt.Fprintf(stdout, "  %s %2d) %s  #%d  %q  (tabs=%d panes=%d)\n",
			focus, i+1, w.WorkspaceID, w.Number, w.Label, w.TabCount, w.PaneCount)
	}
	n, err := promptChoice(stdin, stdout, "番号を入力 (1-"+strconv.Itoa(len(filtered))+"): ", 1, len(filtered))
	if err != nil {
		return "", err
	}
	return filtered[n-1].WorkspaceID, nil
}

// promptChoice は 1 行読んで [min,max] の整数として解釈する。EOF/範囲外は明示エラー
// （黙って既定値を選ばない＝claudeshim の picker と同じ規律）。
func promptChoice(stdin io.Reader, stdout io.Writer, prompt string, min, max int) (int, error) {
	fmt.Fprint(stdout, "\n"+prompt)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return 0, fmt.Errorf("入力読取: %w", err)
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return 0, fmt.Errorf("空入力＝キャンセル")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("整数でない入力 %q", s)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("範囲外 %d（%d-%d）", n, min, max)
	}
	return n, nil
}

// ============ launcher（plugin action 経由の入口） ============

// cmdMvTabLaunch は plugin action `mv-tab` の実体。非 TTY spawn（drawer 起動）
// から呼ばれ、layout.apply で新 Tab（focus:true）を作りその中で
// `herdr-drover mv-tab` を対話モードで走らせる。launcher 自体は即 exit＝
// drawer には「Tab 作成完了」だけが plugin log に記録される。
//
// 新 Tab の workspace は「フォーカス中の workspace」＝ HERDR_WORKSPACE_ID または
// `workspace.list` の focused を使う（focused の実測は claudeshim.currentWorkspaceID）。
func cmdMvTabLaunch(stdout, stderr io.Writer) error {
	api := herdrapi.New("")
	wsID := os.Getenv("HERDR_WORKSPACE_ID")
	if wsID == "" {
		var err error
		wsID, err = currentWorkspaceID(api)
		if err != nil {
			return fmt.Errorf("フォーカス WS 解決失敗: %w", err)
		}
	}
	// 自身の絶対パスを新 pane で起動する（PATH に依存しない・claudeshim 同型）。
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	// layout.apply で新 Tab に launcher pane 1 枚（focus:true＝即ユーザー目視）。
	// argv[0] は絶対パス、cwd は herdr の既定に任せる（cwd は指定しない＝現 focused pane の cwd を継承する実測挙動）。
	if _, err := applyLauncherTab(api, wsID, "mv-tab", []string{selfPath, "mv-tab"}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "mv-tab launcher: 新 Tab を ws=%s に開きました（対話ピッカ内で src/dst を選択してください）\n", wsID)
	return nil
}

// applyLauncherTab は claudeshim.applyClaudeTab と同型（focus:true・cwd 省略）。
// 用途が違うので nameClaudePane（agent 名付与）は呼ばない＝launcher は無名 pane
// として作られる（agent.list に載らない・organize 対象外＝副作用ゼロ）。
func applyLauncherTab(api *herdrapi.Client, wsID, tabLabel string, argv []string) (string, error) {
	type layoutPane struct {
		Type    string   `json:"type"`
		Command []string `json:"command"`
	}
	raw, err := api.Call("layout.apply", struct {
		WorkspaceID string     `json:"workspace_id"`
		TabLabel    string     `json:"tab_label"`
		Focus       bool       `json:"focus"`
		Root        layoutPane `json:"root"`
	}{wsID, tabLabel, true, layoutPane{Type: "pane", Command: argv}})
	if err != nil {
		return "", fmt.Errorf("layout.apply: %w", err)
	}
	var out struct {
		Layout struct {
			Root struct {
				PaneID string `json:"pane_id"`
			} `json:"root"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("layout_apply decode: %w", err)
	}
	if out.Layout.Root.PaneID == "" {
		return "", errors.New("layout.apply 応答に root pane_id が無い（wire 変化?）")
	}
	return out.Layout.Root.PaneID, nil
}
