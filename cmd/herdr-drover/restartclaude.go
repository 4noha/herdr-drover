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
// 対象 identity は **agentid.Resolve（agentid.go）に一元化**（v0.5.23）。
// shim / restart・update / organize が同じ規則を共有する＝以前あった
// 「organize は拾うのに restart は拾わない」非対称を解消した。
// 注入 pane 除外・矛盾判定もそちらに含まれる。
//
// ⚠identity（何のエージェントか）だけでは**作り直してよい根拠にならない**。
// 検出値まで identity を広げた結果 `zsh -lc '… claude'` のような wrapper 起動も
// kind=claude になるが、その argv に --resume を足しても claude には届かない。
// よって **agentid.IsDirectInvocation で argv[0] が claude 本体か**を必ず確認し、
// そうでなければ loud に skip する（誤って作り直すと会話を失ったまま「done」と
// 報告する＝最悪の失敗）。
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
	"github.com/4noha/herdr-drover/internal/agentid"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// restartTarget は再起動対象 1 件（exact-match で選ばれたローカル claude pane）。
type restartTarget struct {
	PaneID      string
	TabID       string
	WorkspaceID string
	AgentName   string // agent 名（未命名の herdr ネイティブ pane は ""）
	AgentKind   string // canonical label（agentid.Resolve の結果）
	AgentStatus string
	Cwd         string
	// Session は herdr が持つ会話 ref。Kind は "id" / "path"（agent による）。
	// **uuid 固定ではない** — pi / omp は path も取る。Value=="" は未検出。
	Session herdrapi.AgentSession
}

// label は表示用の識別子。未命名 pane（herdr UI 直接起動）は agent 名が空なので
// 種別＋pane_id で特定できるようにする（監査記録が空文字にならないように）。
func (t restartTarget) label() string {
	if t.AgentName != "" {
		return t.AgentName
	}
	if t.AgentKind != "" {
		return t.AgentKind + "@" + t.PaneID
	}
	return t.PaneID
}

// resumeDesc は会話引き継ぎの説明（ログ・Ack detail 用）。
// resume 非対応エージェントは「原理的に不可能」であることを明示する
// （未実装と誤解させない／黙って会話を失ったように見せない）。
func (t restartTarget) resumeDesc() string {
	if t.Session.Value == "" {
		if k := t.AgentKind; k != "" && !agentid.Resume(k).Supported {
			return fmt.Sprintf("なし（%s は herdr が会話 ref を出さない＝resume 原理的に不可）", k)
		}
		return "なし（会話 ref 未検出＝argv そのまま）"
	}
	return fmt.Sprintf("%v", agentid.BuildResume(t.AgentKind, []string{t.AgentKind}, t.Session.Value)[1:])
}

// restartOptions は restart-claude の実行オプション（引数の並びで取り違えないよう
// 構造体で渡す）。
type restartOptions struct {
	SID   string // "" = その PC のローカル agent pane 全部
	Agent string // "" = 全エージェント種別。canonical label で絞る
	// Force は既定の安全網（作業中 pane／会話 ref が取れない pane を触らない）を外す。
	// ⚠遠隔命令は flag を運べないので**常に安全側**で動く。
	Force  bool
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

// selectRestartTargets は再起動対象を exact-match で選ぶ純関数。
// sid=="" は「その PC のローカル claude pane 全部」、sid 指定はその 1 枚だけ。
// 対象外の sid は **loud に error**（黙って 0 件にすると「押したのに何も起きない」
// が成功に見える＝silent 変更禁止の鉄則⑤）。
// **agent.list 単独で判定する**（v0.5.22）。以前は pane.list と join して
// tokens/agent_session/tab_id を得ていたが、herdr の AgentInfo は PaneInfo と
// ほぼ同一スキーマでこれらを全て持つ（実測 2026-07-25: 全 15 pane・全照合
// フィールドで両者の値が完全一致、pane_id 集合も一致）。join は冗長なうえ、
// herdr の ndjson は **1 接続=1 リクエスト**なので 2 回の往復の間に構成が
// 変わりうる＝**join 自体が競合の窓**だった。1 リクエストに閉じて解消する。
func selectRestartTargets(agents []herdrapi.AgentInfo, sid, agentFilter string) ([]restartTarget, []string, error) {
	var conflicts []string
	var out []restartTarget
	for _, a := range agents {
		// identity は agentid.Resolve に一元化（v0.5.23）。注入 pane の除外も
		// 矛盾判定もここに含まれる。**シム命名だけでなく herdr の検出値も見る**
		// ＝organize と規則が揃い、herdr UI から直接起動された claude セッションも
		// 対象になる（従来は restart/update だけが取りこぼしていた）。
		kind, conflict := agentid.Resolve(agentid.OfAgent(a))
		if conflict != "" {
			// 機械確定不能は黙って落とさず報告する（silent skip 禁止）。
			conflicts = append(conflicts, fmt.Sprintf("%s: %s", a.PaneID, conflict))
			continue
		}
		if kind == "" {
			continue
		}
		// --agent 指定があればその種別だけ。空は「全エージェント種別」。
		if sid == "" && agentFilter != "" && kind != agentFilter {
			continue
		}
		t := restartTarget{
			PaneID:      a.PaneID,
			TabID:       a.TabID,
			WorkspaceID: a.WorkspaceID,
			AgentName:   a.Name,
			AgentKind:   kind,
			AgentStatus: a.AgentStatus,
			Cwd:         a.Cwd,
		}
		// 会話 ref は herdr の agent_session をそのまま持つ。**その agent が
		// その kind を扱えるか**は Spec が知っている（claude は id のみ、
		// pi/omp は path も）。扱えない kind は resume に使わない。
		if agentid.SupportsKind(kind, a.AgentSession.Kind) {
			t.Session = a.AgentSession
		}
		out = append(out, t)
	}
	if sid == "" {
		return out, conflicts, nil
	}
	for _, t := range out {
		if t.PaneID == sid {
			return []restartTarget{t}, conflicts, nil
		}
	}
	return nil, conflicts, fmt.Errorf("sid %q は再起動対象のローカル claude pane ではない"+
		"（claude と判定できない・↗窓 注入 pane・既に消滅、のいずれか）", sid)
}

// stripResumeArgv は argv から resume 指定だけを取り除く純関数
// （argv[0] と resume 以外のフラグは順序ごとそのまま）。
// rebuildResumeArgv は argv に会話 resume と --model を張り直す。
// **resume の形はエージェントごとに違う**（--resume / --session / --thread /
// --resume= / codex の位置引数サブコマンド）ので agentid.BuildResume に委ねる。
//
// ⚠旧実装は `--resume` の直後を isUUID で判定して落とすか決めていた。claude の
// 会話 ref はたまたま uuid だが pi / omp は **path** も取るので、書式判定では
// 二重指定や誤削除が起きる。「そのフラグが値を取るか」は Spec が知っている事実。
func rebuildResumeArgv(kind string, argv []string, sess herdrapi.AgentSession, model string) []string {
	if len(argv) == 0 {
		return nil
	}
	// モデル指定も Spec 駆動（フラグ名・短縮形が agent ごとに違う。codex の
	// `-m` を剥がし損ねると二重指定になる）。Spec を持たない agent では
	// **argv を一切いじらない**＝知らないエージェントに勝手なフラグを足さない。
	out := agentid.BuildModel(kind, argv, model)
	if sess.Value != "" {
		out = agentid.BuildResume(kind, out, sess.Value)
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
			fmt.Fprintf(out, "restart-agent-session: warn: tab 位置復元 失敗 tab=%s idx=%d: %v\n",
				newTabID, origIdx, err)
		}
	}
	// 元が未命名（herdr UI から直接起動され drover が命名していない pane）なら
	// **命名しない**。ここで採番すると herdr ネイティブの pane を drover 管理下の
	// 名前に変えてしまう＝ユーザーが頼んでいない identity の変更になる。
	// 命名しなくても次回以降は herdr の検出値で拾える（agentid.Resolve の系統 3）。
	if preferredName == "" {
		fmt.Fprintf(out, "restart-agent-session: note: 元 pane は未命名のため agent 名を付けない"+
			"（herdr の検出値のまま。次回も検出で拾える）\n")
		return ""
	}
	name, err := renameClaudePaneTo(api, newPaneID, preferredName)
	if err != nil {
		fmt.Fprintf(out, "restart-agent-session: warn: agent 名 %q を戻せなかった pane=%s: %v\n",
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
	// **resume できるはずなのに会話 ref が取れない pane は既定で触らない**。
	//
	// 作り直せば新バイナリは掴むが、resume 引数を付けられないので**会話は失われる**。
	// しかも pane は生き残るので status=done で成功に見える＝最悪の失敗（wrapper 起動
	// pane で同じ型を潰したのと同種）。
	//
	// 「ref が無い」は 2 つの状態を区別できない:
	//   (a) まだ発話していない  … 失うものが無い（codex/cursor は初回発話まで ref 無し）
	//   (b) integration 未設置  … 会話はあるのに ref が取れない＝**再起動で失う**
	// 区別できない以上、安全側に倒す。claude は起動時に ref が付くので実質影響なく、
	// codex/cursor は (a) が skip されるだけで失うものが無い。
	//
	// ⚠**resume 非対応の 7 種は対象外**（ref が無いのが恒常状態なので、ここで
	// 弾くと永久に再起動できなくなる）。
	if !opt.Force && t.Session.Value == "" && agentid.Resume(t.AgentKind).Supported {
		return "skip", fmt.Sprintf("会話 ref（agent_session）が取れない"+
			"＝作り直すと %s の会話が失われる（まだ発話していないか、`herdr integration "+
			"install %s` が未設置。承知のうえなら --force）", t.AgentKind, t.AgentKind)
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
	// identity が決まっていても、argv が**そのエージェント本体の直接起動**で
	// なければ触らない。例: `zsh -lc '… claude'` は末尾に --resume を足しても
	// 本体に届かないので、作り直すと会話を失ったまま「done」と報告してしまう。
	//
	// ⚠**判定する種別は t.AgentKind**（固定文字列にしない）。ここを "claude" に
	// 固定していたため、codex/cursor の pane が argv[0] は正しいのに常に skip され、
	// **エラーメッセージだけ t.AgentKind を出す**ので一見正しく見えた（実 codex の
	// e2e で発覚。単体テストも dry-run も claude 経路しか通らず気づけなかった）。
	if !agentid.IsDirectInvocation(t.AgentKind, leaf.Command) {
		return "skip", fmt.Sprintf("launch argv が %s の直接起動でない（argv[0]=%q）"+
			"＝resume 引数を付けても本体に届かないため作り直さない", t.AgentKind, leaf.Command[0])
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
		fmt.Fprintf(out, "restart-agent-session: warn: 旧 tab %s が tab.list に無く位置復元を skip\n", t.TabID)
	}
	tabLabel := ""
	for _, tb := range tabs {
		if tb.TabID == t.TabID {
			tabLabel = tb.Label
			break
		}
	}

	// 第 1 段: 会話を引き継ぐ argv で同 Tab を差し替え。
	newArgv := rebuildResumeArgv(t.AgentKind, leaf.Command, t.Session, opt.Model)
	newPaneID, newTabID, err := replaceTabWithCommand(api, t.TabID, cwd, newArgv)
	if err != nil {
		return "error", err.Error()
	}
	name := settlePlacement(api, newPaneID, newTabID, t.AgentName, origIdx, haveIdx, out)
	if waitPaneSettled(api, newPaneID, restartGraceWindow) {
		resume := t.resumeDesc()
		return "done", fmt.Sprintf("pane %s→%s name=%s %s", t.PaneID, newPaneID, name, resume)
	}

	// 第 2 段: 起動直後に落ちた＝その会話は復元できない（herdr の agent_session が
	// 指す uuid の jsonl が消えている等。実測 2026-07-25）。単独 pane の Tab は
	// プロセス終了で Tab ごと閉じているので、resume 無しの元 argv で**新しい Tab**
	// を作り直して pane を必ず残す。
	fmt.Fprintf(out, "restart-agent-session: warn: %s は %s で即終了（この会話は復元できない）"+
		"＝resume 無しで作り直します\n", t.label(), t.resumeDesc())
	if tabLabel == "" {
		tabLabel = filepath.Base(cwd)
	}
	fbArgv := agentid.StripResume(t.AgentKind, leaf.Command)
	fbArgv = agentid.BuildModel(t.AgentKind, fbArgv, opt.Model)
	fbPaneID, err := applyClaudeTab(api, t.WorkspaceID, tabLabel, fbArgv, cwd)
	if err != nil {
		return "error", fmt.Sprintf("%s で即終了し、resume 無しの作り直しにも失敗"+
			"（pane が失われた）: %v", t.resumeDesc(), err)
	}
	fbTabID := ""
	if p, gerr := api.PaneGet(fbPaneID); gerr == nil {
		fbTabID = p.TabID
	}
	name = settlePlacement(api, fbPaneID, fbTabID, t.AgentName, origIdx, haveIdx, out)
	if !waitPaneSettled(api, fbPaneID, restartGraceWindow) {
		return "error", fmt.Sprintf("resume 無しでも起動直後に終了した pane=%s argv=%v"+
			"（%s 側の問題＝手動確認が要る）", fbPaneID, fbArgv, t.AgentKind)
	}
	return "done", fmt.Sprintf("pane %s→%s name=%s resume 復元不可のため新規会話で起動"+
		"（会話 %q の実体が不在）", t.PaneID, fbPaneID, name, t.Session.Value)
}

// restartClaudePanes は対象を選んで順に差し替える。CLI と遠隔命令の**共通の芯**
// （経路ごとに別ロジックを持たない）。
func restartClaudePanes(api *herdrapi.Client, opt restartOptions, out io.Writer) ([]restartOutcome, error) {
	agents, err := api.AgentList()
	if err != nil {
		return nil, fmt.Errorf("agent.list: %w", err)
	}
	targets, conflicts, err := selectRestartTargets(agents, opt.SID, opt.Agent)
	// 機械確定不能な pane は対象外にしたうえで**必ず報告**する（黙って落とすと
	// 「対象に入らない理由が分からない」になる＝silent skip 禁止の鉄則⑤）。
	for _, c := range conflicts {
		fmt.Fprintf(out, "restart-agent-session: skip  %s\n", c)
	}
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "restart-agent-session: 対象のローカル claude pane がありません\n")
		return nil, nil
	}

	results := make([]restartOutcome, 0, len(targets))
	for _, t := range targets {
		if opt.DryRun {
			root, err := exportTabLayout(api, t.TabID)
			argv := "(layout.export 失敗)"
			if err == nil {
				if ls := root.leaves(); len(ls) == 1 && ls[0].PaneID == t.PaneID {
					argv = fmt.Sprintf("%v", rebuildResumeArgv(t.AgentKind, ls[0].Command, t.Session, opt.Model))
				} else {
					argv = fmt.Sprintf("(tab に pane %d 枚＝skip 予定)", len(ls))
				}
			}
			fmt.Fprintf(out, "restart-agent-session: [dry-run] %s pane=%s tab=%s status=%s argv=%s\n",
				t.AgentName, t.PaneID, t.TabID, t.AgentStatus, argv)
			results = append(results, restartOutcome{PaneID: t.PaneID, Name: t.AgentName, Status: "skip", Detail: "dry-run"})
			continue
		}
		status, detail := restartOneClaudePane(api, t, opt, out)
		fmt.Fprintf(out, "restart-agent-session: %-5s %s pane=%s %s\n", status, t.AgentName, t.PaneID, detail)
		results = append(results, restartOutcome{PaneID: t.PaneID, Name: t.AgentName, Status: status, Detail: detail})
	}
	return results, nil
}

// summarizeRestart は遠隔命令 Ack 用の 1 行要約（純関数）。
func summarizeRestart(results []restartOutcome) string {
	if len(results) == 0 {
		return "対象のローカルセッションなし"
	}
	// 未命名 pane（herdr UI 直接起動）は Name が空になりうる。要約はそのまま
	// 遠隔 Ack に載って Firestore に残る監査記録なので、**必ず pane を特定できる
	// 文字列**にする（空文字を join すると「再起動 2 件: ,」になり復元不能）。
	label := func(r restartOutcome) string {
		if r.Name != "" {
			return r.Name
		}
		return r.PaneID
	}
	var done, skip, fail []string
	for _, r := range results {
		switch r.Status {
		case "done":
			done = append(done, label(r))
		case "skip":
			skip = append(skip, label(r)+"("+r.Detail+")")
		default:
			fail = append(fail, label(r)+"("+r.Detail+")")
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
// cmdRestartClaude は restart-agent-session / 旧名 restart-claude の入口。
// legacyClaudeName=true（旧名で呼ばれた）なら **agent は claude 固定**として扱う
// ＝旧名は claude 専用だったので、既存の手順書（`restart-claude --model opus`）を
// そのまま通す。新名では --model に --agent を要求する（モデル名が種別固有のため）。
func cmdRestartClaudeNamed(args []string, stdout, stderr io.Writer, legacyClaudeName bool) error {
	fs := flag.NewFlagSet("restart-claude", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "既定の安全網を外す（作業中 pane・会話 ref が取れない pane も再起動する＝実行中タスクや会話が失われる）")
	dryRun := fs.Bool("dry-run", false, "対象と再起動後 argv を表示するだけで何もしない")
	model := fs.String("model", "", "再起動時に claude へ渡すモデル（例 opus）。空なら既存指定に触らない")
	agent := fs.String("agent", "", "対象のエージェント種別（claude / codex 等）。空なら全種別")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	// 未知の種別は「一致 0 件」に degrade せず loud に撥ねる（黙って何もしない
	// のが一番わかりにくい失敗）。
	if *agent != "" && !agentid.IsCanonical(*agent) {
		return fmt.Errorf("%w: 未知のエージェント種別 %q（canonical label 21 種のいずれか）",
			errUsage, *agent)
	}
	// ⚠**モデル名は agent 固有**（claude=opus / codex=gpt-5 / cursor=sonnet-4-thinking）。
	// 種別を絞らずに渡すと、値が通らないエージェントの pane を壊す（作り直しに
	// 失敗して会話を失う）。旧名 restart-claude 経由は claude 固定なので許す。
	if legacyClaudeName && *agent == "" {
		*agent = "claude"
	}
	if *model != "" && *agent == "" {
		return fmt.Errorf("%w: --model はモデル名が種別ごとに違うため --agent と併用が必要"+
			"（例: --agent claude --model opus）", errUsage)
	}
	if *model != "" && *agent != "" {
		if _, ok := agentid.Model(*agent); !ok {
			return fmt.Errorf("%w: %s のモデル指定方法が未登録＝推測でフラグを足さない"+
				"（internal/agentid の modelSpecs に追加が要る）", errUsage, *agent)
		}
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
		restartOptions{SID: sid, Agent: *agent, Force: *force, DryRun: *dryRun, Model: *model}, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "restart-agent-session: %s\n", summarizeRestart(results))
	return nil
}

// cmdRestartClaude は後方互換の薄いラッパ（新名の既定入口）。
func cmdRestartClaude(args []string, stdout, stderr io.Writer) error {
	return cmdRestartClaudeNamed(args, stdout, stderr, false)
}
