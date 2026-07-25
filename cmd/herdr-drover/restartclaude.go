package main

// claude セッションの再起動（ローカル CLI `restart-claude` ／遠隔命令 restart-claude）。
//
// 動機: claude バイナリを入れ替えても **exec 済みプロセスは旧 inode に貼り付いた
// まま**（`~/.local/bin/claude` は `versions/<ver>` への symlink＝再 exec して初めて
// 新版になる。実測 2026-07-25: 7/18 起動の 3 セッションが 2.1.214 のまま／同日
// 09:25 起動が 2.1.219）。本コマンドは claude pane を**会話を引き継いだまま**
// 作り直して新バイナリを掴ませる。
//
// ⚠**claude バイナリを PATH から解決しない**。daemon（launchd）の PATH には
// ~/.local/bin も ~/.herdr-drover/bin も入らない＝遠隔命令経路と CLI 経路で別物を
// 起動する（あるいは解決失敗する）。権威は「その pane が今実際に走らせている
// argv」＝`layout.export` の launch_argv（実測で command として返る）。symlink 先の
// 切替は再 exec だけで効くので、argv をそのまま使い回すのが最も正確かつ安全。
//
// 対象 identity は exact-match の 2 条件（ヒューリスティック分類禁止の鉄則③）:
//
//	(a) agent.list の name がシム encode 形（isClaudeAgentName＝"claude"/"claude-N"）
//	(b) pane.list の tokens に drover_inj_pc / drover_inj_sid が**無い**
//	    ＝↗窓 注入 pane（argv は `herdr-drover attach <pc> <sid>`）を構造的に除外
//
// 会話の引き継ぎは herdr の検出値 agent_session（kind=id）の uuid を `--resume
// <uuid>` として argv へ付け直す（claudeshim の resume backstop と同じ権威値）。
//
// tab 差し替えは layout.apply{tab_id}（herdr 0.7.4 実測: **新 tab を末尾に作って
// から旧 tab を close** する。tab label=custom_name は継承されるが**位置は末尾へ
// 移る**ので tab.move で元 index へ戻す）。tab_id と workspace_id の同時指定は
// herdr が invalid_target で撥ねる＝tab_id のみ送る。
//
// 巻き添え防止: layout.apply{tab_id} は**その tab の全 pane を作り直す**。よって
// 対象 claude pane が単独 pane の tab のときだけ実行し、同居 pane がある tab は
// skip する（silent skip 禁止＝理由を必ず 1 行出す）。

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// injectTokenKeys は ↗窓 注入 pane に reconcile が付ける metadata token。
// 1 つでも付いていれば注入 pane＝再起動対象から外す（実測 2026-07-25: 注入 11 枚は
// 全て両方を持ち、ローカル claude 4 枚は tokens 空）。
var injectTokenKeys = []string{"drover_inj_pc", "drover_inj_sid"}

// restartTarget は再起動対象 1 件（exact-match で選ばれたローカル claude pane）。
type restartTarget struct {
	PaneID      string
	TabID       string
	WorkspaceID string
	AgentName   string // シム encode 形 "claude" / "claude-N"
	AgentStatus string
	Cwd         string
	ResumeUUID  string // agent_session(kind=id) の会話 uuid。"" は未検出
}

// restartOptions は restart-claude の実行オプション（引数の並びで取り違えないよう
// 構造体で渡す）。
type restartOptions struct {
	SID    string // "" = その PC のローカル claude pane 全部
	Force  bool   // agent_status=working の pane も対象にする
	DryRun bool   // 対象と再起動後 argv を表示するだけ
	Model  string // "" = 既存の --model 指定に触らない。指定時は張り替える
}

// restartOutcome は 1 件の結果（CLI 表示・遠隔命令 Ack の監査に使う）。
// Status は "done"（作り直した）/ "skip"（意図的に見送り）/ "error"（失敗）。
type restartOutcome struct {
	PaneID string
	Name   string
	Status string
	Detail string
}

func isInjectedPane(p *herdrapi.PaneInfo) bool {
	for _, k := range injectTokenKeys {
		if _, ok := p.Tokens[k]; ok {
			return true
		}
	}
	return false
}

// selectRestartTargets は再起動対象を exact-match で選ぶ純関数。
// sid=="" は「その PC のローカル claude pane 全部」、sid 指定はその 1 枚だけ。
// 対象外の sid は **loud に error**（黙って 0 件にすると「押したのに何も起きない」
// が成功に見える＝silent 変更禁止の鉄則⑤）。
func selectRestartTargets(agents []herdrapi.AgentInfo, panes []herdrapi.PaneInfo, sid string) ([]restartTarget, error) {
	byPane := make(map[string]*herdrapi.PaneInfo, len(panes))
	for i := range panes {
		byPane[panes[i].PaneID] = &panes[i]
	}
	var out []restartTarget
	for _, a := range agents {
		if !isClaudeAgentName(a.Name) {
			continue
		}
		p := byPane[a.PaneID]
		if p == nil {
			// agent.list と pane.list の間で消えた pane（1 接続=1 リクエストの
			// 非原子性）。次回の呼び出しで拾える＝ここでは静かに落とす。
			continue
		}
		if isInjectedPane(p) {
			continue
		}
		t := restartTarget{
			PaneID:      a.PaneID,
			TabID:       p.TabID,
			WorkspaceID: p.WorkspaceID,
			AgentName:   a.Name,
			AgentStatus: p.AgentStatus,
			Cwd:         p.Cwd,
		}
		if p.AgentSession.Kind == "id" {
			t.ResumeUUID = p.AgentSession.Value
		}
		out = append(out, t)
	}
	if sid == "" {
		return out, nil
	}
	for _, t := range out {
		if t.PaneID == sid {
			return []restartTarget{t}, nil
		}
	}
	return nil, fmt.Errorf("sid %q は再起動対象のローカル claude pane ではない"+
		"（agent 名が claude/claude-N でない・↗窓 注入 pane・既に消滅、のいずれか）", sid)
}

// stripModelArgv は argv から `--model <値>` / `--model=<値>` を取り除く純関数。
// claude の model 指定に短縮形は無い（実測 2.1.220 の --help）＝`-m` は扱わない
// （勝手に別フラグを食べない）。
func stripModelArgv(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := make([]string, 0, len(argv))
	out = append(out, argv[0])
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--model" {
			if i+1 < len(argv) {
				i++ // 値も一緒に落とす
			}
			continue
		}
		if strings.HasPrefix(a, "--model=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// stripResumeArgv は argv から resume 指定だけを取り除く純関数
// （argv[0] と resume 以外のフラグは順序ごとそのまま）。
func stripResumeArgv(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := make([]string, 0, len(argv))
	out = append(out, argv[0])
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--resume" || a == "-r" {
			// 直後が uuid ならその値も一緒に落とす。値を持たない対話 picker 形
			// （`claude --resume` 単独）はフラグだけ落とす。`-r report.md` の
			// ような非 uuid 値は claude 側の別解釈なので値は残す（isUUID の
			// exact 判定＝claudeshim.parseResumeUUID と同じ規律）。
			if i+1 < len(argv) && isUUID(argv[i+1]) {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "--resume=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// rebuildResumeArgv は pane の実 argv から resume/model 指定を張り替えた新 argv を
// 返す純関数。
//
// argv[0]（実バイナリ絶対パス）とそれ以外のフラグはそのまま保つ＝PATH 解決も
// フラグ推測もしない。
//   - uuid=="" は「herdr がまだ会話 uuid を検出していない」＝resume には触らない
//     （既存の --resume を落とすと会話を失う）。
//   - model=="" は既存の --model 指定に**触らない**（会話が固定しているモデルを
//     勝手に変えない）。model 指定時は既存を落として張り替える。
//
// ⚠`--resume` した会話は **settings.json の既定モデルを無視して会話固定のモデルで
// 動く**（実測 2026-07-25: 既定 opus・sonnet で作った会話を resume → sonnet のまま）。
// 既存会話のモデルを変えるには argv に `--model` を渡すしかない＝この引数が要る理由。
func rebuildResumeArgv(argv []string, uuid, model string) []string {
	if len(argv) == 0 {
		return nil
	}
	out := append([]string(nil), argv...)
	if model != "" {
		out = append(stripModelArgv(out), "--model", model)
	}
	if uuid != "" {
		out = append(stripResumeArgv(out), "--resume", uuid)
	}
	return out
}

// layoutNode は layout.export / layout.apply の LayoutNode（実採取の tagged union:
// {"type":"pane",...} ｜ {"type":"split","direction","ratio","first","second"}）。
type layoutNode struct {
	Type    string      `json:"type"`
	PaneID  string      `json:"pane_id"`
	Label   string      `json:"label"`
	Cwd     string      `json:"cwd"`
	Command []string    `json:"command"`
	First   *layoutNode `json:"first"`
	Second  *layoutNode `json:"second"`
}

// leaves は木を左から辿って pane ノードだけを返す。
func (n *layoutNode) leaves() []*layoutNode {
	if n == nil {
		return nil
	}
	if n.Type == "pane" {
		return []*layoutNode{n}
	}
	return append(n.First.leaves(), n.Second.leaves()...)
}

// exportTabLayout は layout.export{tab_id} の root を返す。pane ノードの command
// は herdr が保持する launch_argv（実測）＝再起動に使う権威値。
func exportTabLayout(api *herdrapi.Client, tabID string) (*layoutNode, error) {
	raw, err := api.Call("layout.export", struct {
		TabID string `json:"tab_id"`
	}{tabID})
	if err != nil {
		return nil, fmt.Errorf("layout.export %s: %w", tabID, err)
	}
	var out struct {
		Layout struct {
			Root layoutNode `json:"root"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("layout_export decode: %w", err)
	}
	return &out.Layout.Root, nil
}

// replaceTabWithCommand は layout.apply{tab_id} で当該 tab を「argv 直接実行 pane
// 1 枚」へ差し替え、新 pane_id / 新 tab_id を返す。focus:false＝表示を奪わない
// （ただし差し替え対象が active tab だった場合は herdr が新 tab を active にする＝
// 実測 replace_was_active。ユーザーが見ていた tab がそのまま残る挙動で正しい）。
func replaceTabWithCommand(api *herdrapi.Client, tabID, cwd string, argv []string) (paneID, newTabID string, err error) {
	type layoutPane struct {
		Type    string   `json:"type"`
		Cwd     string   `json:"cwd"`
		Command []string `json:"command"`
	}
	raw, err := api.Call("layout.apply", struct {
		TabID string     `json:"tab_id"`
		Focus bool       `json:"focus"`
		Root  layoutPane `json:"root"`
	}{tabID, false, layoutPane{Type: "pane", Cwd: cwd, Command: argv}})
	if err != nil {
		return "", "", fmt.Errorf("layout.apply tab=%s: %w", tabID, err)
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
		return "", "", fmt.Errorf("layout_apply decode: %w", err)
	}
	if out.Layout.Root.PaneID == "" || out.Layout.TabID == "" {
		return "", "", fmt.Errorf("layout.apply 応答に pane_id/tab_id が無い（wire 変化?）")
	}
	return out.Layout.Root.PaneID, out.Layout.TabID, nil
}

// tabIndexInWorkspace は tab.list の**並び順**から当該 tab の 0 始まり index を
// 返す。⚠TabInfo.number は安定番号であって位置ではない（実測 2026-07-25: w1 は
// tab 3 枚で number=5/21/23）＝number-1 を index に使うと tab.move が壊れる。
func tabIndexInWorkspace(tabs []herdrapi.TabInfo, wsID, tabID string) (int, bool) {
	idx := 0
	for _, t := range tabs {
		if t.WorkspaceID != wsID {
			continue
		}
		if t.TabID == tabID {
			return idx, true
		}
		idx++
	}
	return 0, false
}

// 差し替え後の生存確認パラメータ。**この猶予が本機能の安全弁**:
// herdr の agent_session が指す uuid は「復元可能な会話」を保証しない
// （実測 2026-07-25: 対応する ~/.claude/projects/**/<uuid>.jsonl が存在しない
// pane があった）。`claude --resume <無い uuid>` は即 exit し、単独 pane の Tab は
// プロセス終了で **Tab ごと自動 close** される＝pane を消したまま終わる実害が出た。
// 差し替え直後にここで検知し、resume 無しで作り直して pane を必ず残す。
const (
	restartGraceWindow   = 4 * time.Second
	restartGraceInterval = 250 * time.Millisecond
)

// paneGone は pane が**確実に消えた**ときだけ true（herdr の exact なエラー
// コード pane_not_found のみ）。socket 一時障害など他のエラーは「生存」扱いに
// 倒す＝一過性の失敗で作り直しを誘発して pane を二重に壊さない。
func paneGone(api *herdrapi.Client, paneID string) bool {
	_, err := api.PaneGet(paneID)
	if err == nil {
		return false
	}
	var apiErr *herdrapi.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "pane_not_found"
}

// waitPaneSettled は grace の間 pane が生き続けたら true。落ちた瞬間に false。
func waitPaneSettled(api *herdrapi.Client, paneID string, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if paneGone(api, paneID) {
			return false
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(restartGraceInterval)
	}
}

// moveTabTo は同一 workspace 内で tab を index へ動かす（tab.move は同 WS の
// reorder 専用＝WS 間移動には使えない）。
func moveTabTo(api *herdrapi.Client, tabID string, index int) error {
	_, err := api.Call("tab.move", struct {
		TabID       string `json:"tab_id"`
		InsertIndex int    `json:"insert_index"`
	}{tabID, index})
	if err != nil {
		return fmt.Errorf("tab.move %s→%d: %w", tabID, index, err)
	}
	return nil
}

// renameClaudePaneTo は差し替え後の pane に**元と同じ agent 名**を付け直す
// （claude シムの cwd 一致 attach は agent 名 exact-match が identity＝名前が
// 変わると既存セッションを見失う）。旧 pane は layout.apply 内で既に閉じている
// ので通常は元名が空いている。万一 taken なら nameClaudePane の採番へ落とす
// （silent に無名のままにしない）。
func renameClaudePaneTo(api *herdrapi.Client, paneID, preferred string) (string, error) {
	if preferred != "" {
		raw, err := api.Call("agent.rename", struct {
			Target string `json:"target"`
			Name   string `json:"name"`
		}{paneID, preferred})
		if err == nil {
			var out struct {
				Agent herdrapi.AgentInfo `json:"agent"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return "", fmt.Errorf("agent_info decode: %w", err)
			}
			return out.Agent.Name, nil
		}
		var apiErr *herdrapi.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "agent_name_taken" {
			return "", fmt.Errorf("agent.rename %s→%s: %w", paneID, preferred, err)
		}
	}
	ag, err := nameClaudePane(api, paneID)
	if err != nil {
		return "", err
	}
	return ag.Name, nil
}

// settlePlacement は新 pane を元の並び位置へ戻し agent 名を復元する。どちらの
// 失敗も「pane は生きている」ので再起動自体は台無しにしない＝warn 止まり
// （silent にはしない）。戻り値は最終的な agent 名（付けられなければ ""）。
func settlePlacement(api *herdrapi.Client, newPaneID, newTabID, preferredName string,
	origIdx int, haveIdx bool, out io.Writer) string {
	if haveIdx && newTabID != "" {
		if err := moveTabTo(api, newTabID, origIdx); err != nil {
			fmt.Fprintf(out, "restart-claude: warn: tab 位置復元 失敗 tab=%s idx=%d: %v\n",
				newTabID, origIdx, err)
		}
	}
	name, err := renameClaudePaneTo(api, newPaneID, preferredName)
	if err != nil {
		fmt.Fprintf(out, "restart-claude: warn: agent 名 %q を戻せなかった pane=%s: %v\n",
			preferredName, newPaneID, err)
		return ""
	}
	return name
}

// restartOneClaudePane は 1 枚を差し替える。戻り値は (status, detail)＝呼び側が
// そのまま監査へ流す（error を返さないのは「1 枚の失敗で残りを止めない」ため）。
//
// 二段構え: まず会話を引き継ぐ argv で差し替え、**起動直後に落ちたら**（＝その
// 会話が復元できない）resume 無しで作り直す。pane を消したまま終わらせないことを
// 最優先の不変条件にする。
func restartOneClaudePane(api *herdrapi.Client, t restartTarget, opt restartOptions, out io.Writer) (string, string) {
	// working は既定でスキップ＝実行中タスクを道連れにしない。
	if !opt.Force && t.AgentStatus == "working" {
		return "skip", "agent_status=working（作業中。--force で強制）"
	}
	root, err := exportTabLayout(api, t.TabID)
	if err != nil {
		return "error", err.Error()
	}
	leaves := root.leaves()
	if len(leaves) != 1 {
		return "skip", fmt.Sprintf("tab %s の pane が %d 枚（layout.apply は tab の全 pane を"+
			"作り直す＝同居 pane の巻き添えを避けて見送り）", t.TabID, len(leaves))
	}
	leaf := leaves[0]
	if leaf.PaneID != t.PaneID {
		return "error", fmt.Sprintf("tab %s の唯一の pane が %s（対象 %s と不一致＝直前に配置が変わった）",
			t.TabID, leaf.PaneID, t.PaneID)
	}
	if len(leaf.Command) == 0 {
		return "skip", "pane に launch argv が無い（shell pane＝claude を直接起動していない）"
	}
	cwd := leaf.Cwd
	if cwd == "" {
		cwd = t.Cwd
	}

	// 位置・label は差し替え**前**に採る（layout.apply 後は旧 tab が消える）。
	tabs, err := listTabs(api)
	if err != nil {
		return "error", err.Error()
	}
	origIdx, haveIdx := tabIndexInWorkspace(tabs, t.WorkspaceID, t.TabID)
	if !haveIdx {
		fmt.Fprintf(out, "restart-claude: warn: 旧 tab %s が tab.list に無く位置復元を skip\n", t.TabID)
	}
	tabLabel := ""
	for _, tb := range tabs {
		if tb.TabID == t.TabID {
			tabLabel = tb.Label
			break
		}
	}

	// 第 1 段: 会話を引き継ぐ argv で同 Tab を差し替え。
	newArgv := rebuildResumeArgv(leaf.Command, t.ResumeUUID, opt.Model)
	newPaneID, newTabID, err := replaceTabWithCommand(api, t.TabID, cwd, newArgv)
	if err != nil {
		return "error", err.Error()
	}
	name := settlePlacement(api, newPaneID, newTabID, t.AgentName, origIdx, haveIdx, out)
	if waitPaneSettled(api, newPaneID, restartGraceWindow) {
		resume := "なし（会話 uuid 未検出＝argv そのまま）"
		if t.ResumeUUID != "" {
			resume = "--resume " + t.ResumeUUID
		}
		return "done", fmt.Sprintf("pane %s→%s name=%s %s", t.PaneID, newPaneID, name, resume)
	}

	// 第 2 段: 起動直後に落ちた＝その会話は復元できない（herdr の agent_session が
	// 指す uuid の jsonl が消えている等。実測 2026-07-25）。単独 pane の Tab は
	// プロセス終了で Tab ごと閉じているので、resume 無しの元 argv で**新しい Tab**
	// を作り直して pane を必ず残す。
	fmt.Fprintf(out, "restart-claude: warn: %s は --resume %s で即終了（この会話は復元できない）"+
		"＝resume 無しで作り直します\n", t.AgentName, t.ResumeUUID)
	if tabLabel == "" {
		tabLabel = filepath.Base(cwd)
	}
	fbArgv := stripResumeArgv(leaf.Command)
	if opt.Model != "" {
		fbArgv = append(stripModelArgv(fbArgv), "--model", opt.Model)
	}
	fbPaneID, err := applyClaudeTab(api, t.WorkspaceID, tabLabel, fbArgv, cwd)
	if err != nil {
		return "error", fmt.Sprintf("--resume %s で即終了し、resume 無しの作り直しにも失敗"+
			"（pane が失われた）: %v", t.ResumeUUID, err)
	}
	fbTabID := ""
	if p, gerr := api.PaneGet(fbPaneID); gerr == nil {
		fbTabID = p.TabID
	}
	name = settlePlacement(api, fbPaneID, fbTabID, t.AgentName, origIdx, haveIdx, out)
	if !waitPaneSettled(api, fbPaneID, restartGraceWindow) {
		return "error", fmt.Sprintf("resume 無しでも起動直後に終了した pane=%s argv=%v"+
			"（claude 側の問題＝手動確認が要る）", fbPaneID, fbArgv)
	}
	return "done", fmt.Sprintf("pane %s→%s name=%s resume 復元不可のため新規会話で起動"+
		"（会話 %s は jsonl 不在）", t.PaneID, fbPaneID, name, t.ResumeUUID)
}

// restartClaudePanes は対象を選んで順に差し替える。CLI と遠隔命令の**共通の芯**
// （経路ごとに別ロジックを持たない）。
func restartClaudePanes(api *herdrapi.Client, opt restartOptions, out io.Writer) ([]restartOutcome, error) {
	agents, err := api.AgentList()
	if err != nil {
		return nil, fmt.Errorf("agent.list: %w", err)
	}
	panes, err := api.PaneList()
	if err != nil {
		return nil, fmt.Errorf("pane.list: %w", err)
	}
	targets, err := selectRestartTargets(agents, panes, opt.SID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "restart-claude: 対象のローカル claude pane がありません\n")
		return nil, nil
	}

	results := make([]restartOutcome, 0, len(targets))
	for _, t := range targets {
		if opt.DryRun {
			root, err := exportTabLayout(api, t.TabID)
			argv := "(layout.export 失敗)"
			if err == nil {
				if ls := root.leaves(); len(ls) == 1 && ls[0].PaneID == t.PaneID {
					argv = fmt.Sprintf("%v", rebuildResumeArgv(ls[0].Command, t.ResumeUUID, opt.Model))
				} else {
					argv = fmt.Sprintf("(tab に pane %d 枚＝skip 予定)", len(ls))
				}
			}
			fmt.Fprintf(out, "restart-claude: [dry-run] %s pane=%s tab=%s status=%s argv=%s\n",
				t.AgentName, t.PaneID, t.TabID, t.AgentStatus, argv)
			results = append(results, restartOutcome{PaneID: t.PaneID, Name: t.AgentName, Status: "skip", Detail: "dry-run"})
			continue
		}
		status, detail := restartOneClaudePane(api, t, opt, out)
		fmt.Fprintf(out, "restart-claude: %-5s %s pane=%s %s\n", status, t.AgentName, t.PaneID, detail)
		results = append(results, restartOutcome{PaneID: t.PaneID, Name: t.AgentName, Status: status, Detail: detail})
	}
	return results, nil
}

// summarizeRestart は遠隔命令 Ack 用の 1 行要約（純関数）。
func summarizeRestart(results []restartOutcome) string {
	if len(results) == 0 {
		return "対象のローカル claude pane なし"
	}
	var done, skip, fail []string
	for _, r := range results {
		switch r.Status {
		case "done":
			done = append(done, r.Name)
		case "skip":
			skip = append(skip, r.Name+"("+r.Detail+")")
		default:
			fail = append(fail, r.Name+"("+r.Detail+")")
		}
	}
	parts := []string{fmt.Sprintf("再起動 %d 件", len(done))}
	if len(done) > 0 {
		parts[0] += ": " + strings.Join(done, ",")
	}
	if len(skip) > 0 {
		parts = append(parts, "skip "+strings.Join(skip, ","))
	}
	if len(fail) > 0 {
		parts = append(parts, "失敗 "+strings.Join(fail, ","))
	}
	return strings.Join(parts, " / ")
}

// cmdRestartClaude は CLI 入口。
func cmdRestartClaude(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("restart-claude", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "agent_status=working の pane も再起動する（実行中タスクは失われる）")
	dryRun := fs.Bool("dry-run", false, "対象と再起動後 argv を表示するだけで何もしない")
	model := fs.String("model", "", "再起動時に claude へ渡すモデル（例 opus）。空なら既存指定に触らない")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	rest := fs.Args()
	if len(rest) > 1 {
		return fmt.Errorf("%w: 余分な引数 %v（sid は 1 つまで）", errUsage, rest[1:])
	}
	sid := ""
	if len(rest) == 1 {
		sid = rest[0]
	}

	api := herdrapi.New("")
	if _, err := api.Ping(); err != nil {
		return fmt.Errorf("herdr server に繋がらない（socket=%s）: %w", api.SocketPath, err)
	}
	results, err := restartClaudePanes(api,
		restartOptions{SID: sid, Force: *force, DryRun: *dryRun, Model: *model}, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "restart-claude: %s\n", summarizeRestart(results))
	return nil
}
