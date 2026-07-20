package main

// reconcile — リモート pane 注入（Phase 3・↗窓相当）。他 PC の claude セッションを
// ローカル herdr へ **注入 pane** として出現させ、出現/消滅/重複を自己修復する。
// cm internal/cloud/agent/remotesync.go（tmux ↗窓）の herdr pane 版。
//
// 各注入 pane は `herdr-drover attach <pc> <sid>` を実行（attach.go が primary
// クラウドの relay へ viewer 接続しリモート画面を映す）。identity は三重符号化:
//   - metadata token（source=drover-inj・pc/sid）＝reconcile の cur 認識（exact-match）
//   - launch argv（attach <pc> <sid>）＝pane プロセス自身の接続先
//   - injectindex（drover 側の永続化・token race 窓と再起動 token 消失の穴を塞ぐ）
//
// **判定の権威は token+injectindex** (v0.5.x〜)。旧来は workspace label==↗remote
// 所属を第一権威にしていたが、ユーザーが mv-tab で別 WS へ動かしたり workspace を
// rename すると判定が壊れる（cross-PC 増殖・二重注入）。判定を workspace label /
// workspace_id から完全に切り離した。`InjWorkspaceLabel` は「注入 workspace を
// **新規作成する時の初期 label** のデフォルトのみ」に意味変え済み。
//
// fail-safe: pane.list / ListPCs / ListSessions のどれかが失敗した周は **何もせず
// abort**（desired を空とみなして全 kill する破壊を防ぐ。idempotent＝次で再試行）。
// 暴走上限ガード（M8f2 教訓）: cur が desired を著しく超える周は作成停止＝整理のみ。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/injectindex"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

const (
	injSource = "drover-inj"          // pane.report_metadata の source
	injTokPC  = herdrapi.InjTokenPC   // identity token キー（producer と共有＝herdrapi）
	injTokSID = herdrapi.InjTokenSID  // exact-match・非ヒューリスティック
	// injWorkspace は「注入 workspace の**新規作成時の初期 label**」。判定には
	// 一切使わない（v0.5.x で workspace 所属判定は完全廃止・token+index が権威）。
	// ユーザーは herdr UI で自由に rename 可能（drover は workspace_id を持ち回るので追随不要）。
	injWorkspace = herdrapi.InjWorkspaceLabel
)

// injMarkerKey は (pc,sid) の一意結合キー（cur/desired 突合せ用）。
func injMarkerKey(pc, sid string) string { return pc + "\x00" + sid }

// sessSID は session 行（ListSessions の d.Data()）から sid（=pane_id）を取り出す。
// producer が key/session_id 両方に pane_id を書く契約（internal/session）。
func sessSID(s map[string]any) string {
	if v, _ := s["key"].(string); v != "" {
		return v
	}
	if v, _ := s["session_id"].(string); v != "" {
		return v
	}
	return ""
}

// injTabName は注入 pane の tab 表示名（先頭 ↗＝リモート由来を一目で示す）。
func injTabName(dir string) string {
	if dir == "" {
		return "↗remote"
	}
	return "↗" + dir
}

type injMeta struct{ pc, sid, dir string }

// remoteSource は reconcile が他 PC のセッションを読むのに必要な最小 API
// （*state.Client が満たす）。テストは herdr 側を実 herdr で検証しつつ、リモート
// データ（他 PC のセッション行）を fake で注入するために interface 化する
// （herdr の pane 生成/list/close/dedup/cap＝リスクの本体は実 herdr で担保）。
type remoteSource interface {
	// DroverPCs は agent_kind==herdr-drover の PC のみ（#inj に応答できる drover
	// agent）。cm agent の pc は含めない＝blank pane・自己注入を構造的に防ぐ。
	DroverPCs(ctx context.Context) ([]string, error)
	ListSessions(ctx context.Context, pc string) ([]map[string]any, error)
}

// injPaneEnv は注入 pane（attach プロセス）へ渡すクラウド設定 env。attach は
// daemon の launchd env を継承せず config.json も無いことがあるため、reconcile が
// primary クラウドの設定を pane env で明示注入する（これが無いと attach が
// 「クラウド未設定」で失敗し続ける＝実障害で確認）。空値は入れない。
func injPaneEnv(cl Cloud) map[string]string {
	env := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			env[k] = v
		}
	}
	set("GCP_PROJECT", cl.Project)
	set("CLOUD_RELAY_URL", cl.RelayURL)
	set("GOOGLE_APPLICATION_CREDENTIALS", cl.SAKeyPath)
	set("PC_ID", cl.PCName)
	return env
}

// resolveActiveInjectWSID は「アクティブな注入 workspace_id」を返す。決定順:
//  1. injectindex の生存 entry が **pane.list 上のどの workspace に居るか** を集計し、
//     最多の workspace_id を採る（ユーザーが herdr UI で workspace を rename しても、
//     drover は workspace_id で追跡＝rename に耐性）。同数は空返（label 経路にフォールバック）。
//  2. workspace.list を label==injWorkspace で検索（既存ユーザーのデフォルト維持）。
//  3. どちらも無ければ "" を返し、呼び手が wsmap.ResolveWorkspaceID で新規作成する。
func resolveActiveInjectWSID(api *herdrapi.Client, idx *injectindex.Index, panes []herdrapi.PaneInfo) (string, error) {
	// 1: index 集計。pane_id → workspace_id を pane.list から引き、index に載ってる
	//    pane_id の workspace_id を集計する。
	if idx != nil {
		paneWS := make(map[string]string, len(panes))
		for i := range panes {
			paneWS[panes[i].PaneID] = panes[i].WorkspaceID
		}
		count := map[string]int{}
		for _, e := range idx.Snapshot() {
			if ws, ok := paneWS[e.PaneID]; ok && ws != "" {
				count[ws]++
			}
		}
		best, bestN := "", 0
		tie := false
		for ws, n := range count {
			if n > bestN {
				best, bestN, tie = ws, n, false
			} else if n == bestN {
				tie = true
			}
		}
		if best != "" && !tie {
			return best, nil
		}
	}
	// 2: label 検索（従来経路）。error は上位が abort 判断する。
	return findWorkspaceID(api, injWorkspace)
}

// reconcileRemote は他 PC のセッションをローカル herdr の注入 pane へ同期する 1 周。
// cl は primary クラウド（selfPC=cl.PCName で自 PC を除外・pane env に設定を注入）。
// idx は injectindex（判定の権威・race 窓予約・再起動 token 消失復元）。
func reconcileRemote(ctx context.Context, api *herdrapi.Client, st remoteSource,
	cl Cloud, selfExe string, idx *injectindex.Index, lg *log.Logger) {

	selfPC := cl.PCName
	env := injPaneEnv(cl)

	// cur: 既存の注入 pane。list 失敗は abort＝「注入 pane ゼロ」誤認 runaway を防ぐ。
	panes, err := api.PaneList()
	if err != nil {
		lg.Printf("[reconcile] ABORT: pane.list 失敗（作成/削除しない）: %v", err)
		return
	}

	// アクティブ注入 workspace_id を index 集計 → label 検索の順で解決。
	// workspace.list 失敗は abort（"" 退行で新規作成経路に落ち意図しない ws を作りうる）。
	activeWSID, werr := resolveActiveInjectWSID(api, idx, panes)
	if werr != nil {
		lg.Printf("[reconcile] ABORT: workspace.list 失敗（active ws 解決不能）: %v", werr)
		return
	}

	// cur 構築: **判定の権威は token / index**（workspace 所属は見ない）。
	//   (a) pane.list に token あり → cur に載せる
	//   (b) pane.list に token 無し・index に非 Pending entry あり → cur に救済＋token 再表明
	//       （drover 単独再起動 / herdr 単独再起動どちらでも生き延びる）
	// index に居ない token 無し pane は cur に入れない（構造 root pane・ユーザー任意 pane を
	// 誤って掃除しないため）。
	cur := map[string]injMeta{} // pane_id -> (pc,sid)
	for i := range panes {
		p := &panes[i]
		pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]
		if pc != "" && sid != "" {
			cur[p.PaneID] = injMeta{pc: pc, sid: sid}
			// index 側にも取り込む（AdoptToken は既に一致していれば no-op＝persist 節約）。
			if idx != nil {
				_ = idx.AdoptToken(p.PaneID, pc, sid)
			}
			continue
		}
		if idx == nil {
			continue
		}
		if e, ok := idx.Get(p.PaneID); ok && !e.Pending {
			// (b) token 消失の self-heal: cur に載せてから token 再表明。
			cur[p.PaneID] = injMeta{pc: e.PC, sid: e.SID}
			if merr := api.PaneReportMetadata(p.PaneID, injSource, herdrapi.ReportMetadata{
				Tokens: map[string]string{injTokPC: e.PC, injTokSID: e.SID},
			}); merr != nil {
				lg.Printf("[reconcile] token 再表明失敗 %s（cur には載せる・次周に再試行）: %v", p.PaneID, merr)
			}
		}
	}

	// desired: **drover PC のみ**の session（cm agent の pc は #inj に応答できず
	// 永久 blank になるため除外・同居 cm agent の自己注入も防ぐ）。DroverPCs/
	// ListSessions 失敗は abort（desired を空とみなすと全 kill＝破壊的）。
	pcs, perr := st.DroverPCs(ctx)
	if perr != nil {
		lg.Printf("[reconcile] ABORT: DroverPCs 失敗: %v", perr)
		return
	}
	desired := map[string]injMeta{} // markerKey -> (pc,sid,dir)
	for _, pc := range pcs {
		if pc == selfPC || pc == "" {
			continue // 自 PC はローカル producer の担当（自分を注入しない）
		}
		ss, serr := st.ListSessions(ctx, pc)
		if serr != nil {
			lg.Printf("[reconcile] ABORT: ListSessions(%s) 失敗: %v", pc, serr)
			return
		}
		for _, s := range ss {
			sid := sessSID(s)
			if sid == "" {
				continue
			}
			dir, _ := s["short_dir"].(string)
			desired[injMarkerKey(pc, sid)] = injMeta{pc: pc, sid: sid, dir: dir}
		}
	}

	// 暴走上限ガード（検出が部分的に壊れても pane が無限増殖しない最後の砦）。
	// index が権威になった今、guard = len(cur) + index の Pending 数（token 付与前で
	// cur には載らないが producer からは除外済みの pane 数）。旧 label 依存の injWsPanes
	// は廃止（label==↗remote 判定が消えたため）。
	pendingCount := 0
	if idx != nil {
		for _, e := range idx.Snapshot() {
			if e.Pending {
				pendingCount++
			}
		}
	}
	guard := len(cur) + pendingCount
	maxPanes := len(desired)*3 + 8
	noCreate := guard > maxPanes
	lg.Printf("[reconcile] desired=%d cur(injected)=%d pending=%d", len(desired), len(cur), pendingCount)
	if noCreate {
		lg.Printf("[reconcile] CAP: 注入 pane=%d > %d → 作成停止し整理のみ", guard, maxPanes)
	}

	wsID := activeWSID // 既存の注入 workspace（無ければ作成時に遅延解決）
	for mk, d := range desired {
		var ids []string
		for pid, m := range cur {
			if injMarkerKey(m.pc, m.sid) == mk {
				ids = append(ids, pid)
				delete(cur, pid)
			}
		}
		if len(ids) == 0 {
			if noCreate {
				continue
			}
			if wsID == "" {
				id, werr := wsmap.ResolveWorkspaceID(api, injWorkspace)
				if werr != nil {
					lg.Printf("[reconcile] workspace 解決失敗（作成 skip）: %v", werr)
					continue
				}
				wsID = id
			}
			argv := []string{selfExe, "attach", d.pc, d.sid}
			pid, aerr := applyInjectPane(api, wsID, injTabName(d.dir), argv, env)
			if aerr != nil {
				lg.Printf("[reconcile] CREATE 失敗 %s/%s: %v", d.pc, d.sid, aerr)
				continue
			}
			// **race 窓予約**: pane を作った直後・token 付与前に index へ Pending 登録。
			// この瞬間から producer は index Snapshot 経由でこの pane_id を除外する
			// （token 付与前でも cross-PC 増殖しない・token 権威化の穴 (a) を塞ぐ）。
			// Reserve 失敗は pane close して orphan を残さない（既存 metadata 失敗経路と同型）。
			if idx != nil {
				if rerr := idx.Reserve(pid, d.pc, d.sid); rerr != nil {
					lg.Printf("[reconcile] index Reserve 失敗 %s → 即 close: %v", pid, rerr)
					_ = api.PaneClose(pid)
					continue
				}
			}
			if merr := api.PaneReportMetadata(pid, injSource, herdrapi.ReportMetadata{
				Tokens: map[string]string{injTokPC: d.pc, injTokSID: d.sid},
			}); merr != nil {
				// token 付与失敗＝orphan 化するので pane を close＋index からも Abandon。
				lg.Printf("[reconcile] metadata 付与失敗 %s → 即 close（orphan 回避）: %v", pid, merr)
				_ = api.PaneClose(pid)
				if idx != nil {
					_ = idx.Forget(pid)
				}
				continue
			}
			// token 付与成功 → index を Live に昇格。
			if idx != nil {
				if cerr := idx.Commit(pid, d.pc, d.sid); cerr != nil {
					lg.Printf("[reconcile] index Commit 失敗 %s（pane は生存・次周で救済）: %v", pid, cerr)
				}
			}
			lg.Printf("[reconcile] CREATE %s/%s -> %s", d.pc, d.sid, pid)
		} else {
			for _, extra := range ids[1:] {
				_ = api.PaneClose(extra) // 重複（過去 race）を自己修復
				if idx != nil {
					_ = idx.Forget(extra)
				}
			}
		}
	}
	// desired に無い注入 pane = リモートで消えたセッション → close + index Forget。
	for pid := range cur {
		_ = api.PaneClose(pid)
		if idx != nil {
			_ = idx.Forget(pid)
		}
		lg.Printf("[reconcile] CLOSE %s（リモート消滅）", pid)
	}
	// ⚠ token 無し pane の一括掃除はしない: 注入 workspace には WorkspaceCreate 由来の
	// **構造 root pane（token 無し）**が常駐する（実 herdr 0.7.4 で実測）ため、「token 無し
	// ＝孤児」で掃除すると root pane を毎周 kill してしまう。index 権威になった今も、index に
	// 無い token 無し pane（ユーザーが任意に置いた pane・root pane）を触らない規律は維持。
	// 再起動で token を失った pane は上の cur 救済で index から復元して token 再表明する。
}

// runRemoteInject は他 PC のセッションをローカル herdr へ注入し続ける（push 駆動:
// 起動時 1 回＋WatchSessions 変更のたび reconcile。5s ポーリングしない）。ctx 終了で
// 戻る。**primary クラウドのみ**が呼ぶ（複数クラウドが同一 herdr へ同 pane を注入する
// 二重窓・競合を構造的に防ぐ＝runOneCloud の primary 分岐から起動）。
func runRemoteInject(ctx context.Context, api *herdrapi.Client, st *state.Client,
	cl Cloud, idx *injectindex.Index, lg *log.Logger) {

	selfExe, err := os.Executable()
	if err != nil {
		lg.Printf("[reconcile] 自 exe 解決失敗（リモート pane 注入 無効）: %v", err)
		return
	}
	trigger := make(chan struct{}, 1)
	kick := func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	kick() // 起動時 1 回

	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			if werr := st.WatchSessions(ctx, kick); werr != nil && ctx.Err() == nil {
				lg.Printf("[reconcile] WatchSessions 終了（再購読）: %v", werr)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-trigger:
			// バースト変更を最小デバウンスで coalesce（cm と同じ）。
			select {
			case <-time.After(300 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			reconcileRemote(ctx, api, st, cl, selfExe, idx, lg)
		}
	}
}

// findWorkspaceID は label の workspace id を返す（無ければ ""・error は伝播）。
// resolveActiveInjectWSID の fallback（label 検索）と、注入 workspace の初回作成時
// の label 一致検索に使う。重複 label は number 最小を採る（wsmap.ResolveWorkspaceID
// の作成選択と一致＝数える ws と作る ws が同一になる）。
func findWorkspaceID(api *herdrapi.Client, label string) (string, error) {
	wss, err := orgListWorkspaces(api)
	if err != nil {
		return "", err
	}
	best, bestNum := "", 0
	for _, w := range wss {
		if w.Label == label && (best == "" || w.Number < bestNum) {
			best, bestNum = w.WorkspaceID, w.Number
		}
	}
	return best, nil
}

// applyInjectPane は layout.apply で「wsID に新 tab＋argv 直接実行 pane 1 枚」を
// **env 付き**で作る（applyClaudeTab の注入版＝pane に GCP_PROJECT 等を注入して
// attach が primary クラウドを解決できるようにする）。focus は奪わない。
func applyInjectPane(api *herdrapi.Client, wsID, tabLabel string, argv []string, env map[string]string) (string, error) {
	type layoutPane struct {
		Type    string            `json:"type"`
		Command []string          `json:"command"`
		Env     map[string]string `json:"env,omitempty"`
	}
	raw, err := api.Call("layout.apply", struct {
		WorkspaceID string     `json:"workspace_id"`
		TabLabel    string     `json:"tab_label"`
		Focus       bool       `json:"focus"`
		Root        layoutPane `json:"root"`
	}{wsID, tabLabel, false, layoutPane{Type: "pane", Command: argv, Env: env}})
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
		return "", fmt.Errorf("layout.apply 応答に root pane_id が無い")
	}
	return out.Layout.Root.PaneID, nil
}

// selfHealOnStartup は agent 起動時に呼ぶ 3 分岐 self-heal:
//   (a) pane.list に token あり／index に無し → index に取り込む（AdoptToken）
//   (b) pane.list に token 無し／index に非 Pending entry あり → pane に token 再表明
//   (c) index に entry あり／pane.list に無し → index から Forget（stale 掃除）
// Pending entry は「reconcile 中の pending が index 書込後に crash」した名残なので、
// pane.list に該当 pane_id が居れば (b) と同じ扱い、居なければ (c) と同じ扱い。
// 戻り値は healed/adopted/dropped の件数（ログ・テスト検証用）。
func selfHealOnStartup(api *herdrapi.Client, idx *injectindex.Index, panes []herdrapi.PaneInfo, lg *log.Logger) (healed, adopted, dropped int) {
	if idx == nil {
		return 0, 0, 0
	}
	paneByID := make(map[string]*herdrapi.PaneInfo, len(panes))
	for i := range panes {
		paneByID[panes[i].PaneID] = &panes[i]
	}
	// (a) と (c) を index snapshot ベースで判定。
	for _, e := range idx.Snapshot() {
		p, ok := paneByID[e.PaneID]
		if !ok {
			// (c) index には有るが pane.list に無い＝ stale 掃除
			if err := idx.Forget(e.PaneID); err != nil {
				lg.Printf("[self-heal] Forget 失敗 %s: %v", e.PaneID, err)
				continue
			}
			dropped++
			continue
		}
		if pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]; pc == e.PC && sid == e.SID {
			// 完全一致＝何もしない
			continue
		}
		// (b) token 消失 or 不一致 → 再表明。Pending は index の (pc,sid) を権威にする
		// （reconcile 中に crash した名残＝index 情報が最新の意図）。
		if err := api.PaneReportMetadata(e.PaneID, injSource, herdrapi.ReportMetadata{
			Tokens: map[string]string{injTokPC: e.PC, injTokSID: e.SID},
		}); err != nil {
			lg.Printf("[self-heal] token 再表明失敗 %s: %v", e.PaneID, err)
			continue
		}
		// Pending だった場合は Live へ昇格させる（reconcile 中断のクリーンアップ）。
		if e.Pending {
			_ = idx.Commit(e.PaneID, e.PC, e.SID)
		}
		healed++
	}
	// (a) pane.list に token あり／index に無し → index に取り込む
	for i := range panes {
		p := &panes[i]
		pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]
		if pc == "" || sid == "" {
			continue
		}
		if _, ok := idx.Get(p.PaneID); ok {
			continue // 既に index に居る（(a)(b) で処理済 or 完全一致）
		}
		if err := idx.AdoptToken(p.PaneID, pc, sid); err != nil {
			lg.Printf("[self-heal] AdoptToken 失敗 %s: %v", p.PaneID, err)
			continue
		}
		adopted++
	}
	if healed+adopted+dropped > 0 {
		lg.Printf("[self-heal] index restore: healed=%d adopted=%d dropped=%d", healed, adopted, dropped)
	}
	return healed, adopted, dropped
}
