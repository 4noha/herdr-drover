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
	"strings"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/injectindex"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

const (
	injSource = "drover-inj"         // pane.report_metadata の source
	injTokPC  = herdrapi.InjTokenPC  // identity token キー（producer と共有＝herdrapi）
	injTokSID = herdrapi.InjTokenSID // exact-match・非ヒューリスティック
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

// injPaneTitle は注入 pane の terminal_title（herdr の pane.report_metadata
// title）。実装上の理由（layout.apply には注入 pane 用の cwd 指定が無く、
// リモート PC の実パスはこのローカル PC のファイルシステムに存在しない
// ため cwd フィールド自体には渡せない＝渡すと存在しないパスで attach
// プロセスの起動に失敗し得る）で `pane.cwd`/`foreground_cwd` は同一
// workspace 内の既存 pane の値を herdr が便宜的に継承し、全 inject pane が
// 同じ値を示す（実運用フィードバックで報告された誤認要因）。cwd を偽装
// する代わりに、**title へ「↗pc:実パス」を明示**して pane 選定時に画面を
// 見れば出所と実際の作業ディレクトリが一目で分かるようにする（cwd
// フィールドの意味を汚さない・鉄則③のヒューリスティック分類回避と同じ
// 精神＝表示は実データそのまま、推測で埋めない）。cwd が空（旧 session
// データや取得失敗）なら pc のみ表示。
//
// ips はリモート PC の全ローカル IP アドレス（実運用要望「SSH で機密情報を
// 送る時に宛先確認したい」への対処。1 台が複数 IP を持ちうる＝全部並べる）。
// 空なら省略（DROVER_SHARE_LOCAL_IPS opt-out の PC・旧 session データ）。
func injPaneTitle(pc, cwd string, ips []string) string {
	t := "↗ " + pc
	if cwd != "" {
		t += ":" + cwd
	}
	if len(ips) > 0 {
		t += " [" + strings.Join(ips, ", ") + "]"
	}
	return t
}

// title は cur 側のみ使う（pane.list から読んだ現在の terminal_title。desired
// 側は常に空＝比較は cur.title と injPaneTitle(desired.pc, desired.cwd, desired.ips)
// で行う）。ips は desired 側のみ使う（リモート session の local_ips。DROVER_
// SHARE_LOCAL_IPS opt-out の PC からは常に空）。
type injMeta struct {
	pc, sid, dir, cwd, status, name, title string
	ips                                    []string
}

// injAPIState は producer が同期したリモートの agent_status（herdr の**表示値**＝
// done/idle/working/blocked/unknown の 5 値）を、pane.report_agent の --state 語彙
// （idle/working/blocked/unknown の 4 値・"done" は入力に無い）へ写す exact-match。
//
//	working→working, blocked→blocked, idle→idle,
//	done→idle（herdr 内部で done は AgentState::Idle の「未 seen」表示＝報告は idle。
//	         転記先で seen 前なら done、seen 後は idle と表示され、状態としては一致）。
//
// 第 2 戻り値 false は「リモートに生きた agent 無し」（unknown/空/未知値）＝転記せず
// release 対象。ヒューリスティック分類はしない（鉄則③・herdr src の pane_agent_status /
// detect_state_from_api の写像を実コードで確認して決めた）。
func injAPIState(status string) (string, bool) {
	switch status {
	case "working":
		return "working", true
	case "blocked":
		return "blocked", true
	case "idle", "done":
		return "idle", true
	}
	return "", false
}

// mirrorInjectedAgent は present な注入 pane paneID に、リモート session の
// agent_status(status)/window_name(name) を herdr の pane.report_agent で転記し、
// ↗窓 を herdr に「agent」として検出させる（tab/workspace の agent_status・
// agent.list・agent wait が効くようになる）。
//
// reported は paneID→最後に report した agent label で、リモート agent 終了
// （status→unknown）時に正しい label で release_agent し stale 表示を消すための
// state。**reported == nil は「mirror 無効」を意味し即 return する**
// （DROVER_MIRROR_AGENTS 未設定＝runRemoteInject が map を渡さない・既存挙動維持）。
func mirrorInjectedAgent(api *herdrapi.Client, paneID, name, status string, reported map[string]string, lg *log.Logger) {
	if reported == nil {
		return // mirror 無効（opt-in・DROVER_MIRROR_AGENTS 未設定）
	}
	if apiState, ok := injAPIState(status); ok {
		if name == "" {
			name = "agent" // report_agent は agent label 必須（空はデフォルト名）
		}
		if err := api.ReportAgent(paneID, injSource, name, apiState); err != nil {
			lg.Printf("[reconcile] report_agent 失敗 %s (%s=%s→%s): %v", paneID, name, status, apiState, err)
			return
		}
		reported[paneID] = name
		return
	}
	// unknown/空 → リモートに生きた agent 無し。以前 report していれば同じ label で
	// release して stale 表示を消す。
	prev, ok := reported[paneID]
	if !ok {
		return
	}
	if err := api.ReleaseAgent(paneID, injSource, prev); err != nil {
		lg.Printf("[reconcile] release_agent 失敗 %s (%s): %v", paneID, prev, err)
		return
	}
	delete(reported, paneID)
}

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
	cl Cloud, selfExe string, idx *injectindex.Index, lg *log.Logger, reportedOpt ...map[string]string) {

	selfPC := cl.PCName
	env := injPaneEnv(cl)

	// reported は paneID→最後に report した agent label（注入 pane の agent 転記の
	// release 追跡用・runRemoteInject が持ち回る）。可変長で受けるのは既存の全
	// reconcileRemote 呼び出し（テスト含む）と後方互換にするため（未指定＝追跡なし）。
	var reported map[string]string
	if len(reportedOpt) > 0 {
		reported = reportedOpt[0]
	}

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
	cur := map[string]injMeta{} // pane_id -> (pc,sid,title)
	for i := range panes {
		p := &panes[i]
		pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]
		if pc != "" && sid != "" {
			cur[p.PaneID] = injMeta{pc: pc, sid: sid, title: p.Title}
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
			// (b) token 消失の self-heal: cur に載せてから token 再表明。herdr
			// 再起動で report_metadata は title も token 同様に消える（実測）ため
			// 両方を同時に貼り直す（title は desired 側の cwd が無いと復元できない
			// ので、この時点では pc のみで再構築＝下の title 突合せループが desired
			// マッチ後に cwd 込みで最終修復する）。
			cur[p.PaneID] = injMeta{pc: e.PC, sid: e.SID, title: p.Title}
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
			cwd, _ := s["cwd"].(string)
			// agent_status / window_name は producer が既に同期している生値
			// （producer.go BuildSessions）。注入 pane へ exact-match で転記して
			// herdr に agent 検出させるのに使う（reconcile 側で分類はしない）。
			status, _ := s["agent_status"].(string)
			name, _ := s["window_name"].(string)
			// local_ips は Firestore 往復で []any（各要素は string）になる
			// （internal/session.BuildSessions が []any として書く）。型が違う
			// 要素は捨てる（推測しない・壊れた値は無視して落ちない）。
			var ips []string
			if raw, ok := s["local_ips"].([]any); ok {
				for _, v := range raw {
					if ip, ok := v.(string); ok && ip != "" {
						ips = append(ips, ip)
					}
				}
			}
			desired[injMarkerKey(pc, sid)] = injMeta{pc: pc, sid: sid, dir: dir, cwd: cwd, status: status, name: name, ips: ips}
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

	// wsmap の inject_placement を先読み（v0.5.5〜）。CREATE 時に (pc, short_dir)
	// で一致する label があれば、その workspace へ着地させる（ユーザーが手動で
	// 振り分けた配置を wsmap capture で保存 → reconcile が再現する経路）。
	// Load 失敗は skip（inject_placement 無しと同等＝デフォルト着地）で握り潰さない
	// 方針も検討したが、Placement 無し運用ユーザーが多いため warn ログのみで継続。
	placement, plerr := wsmap.Load()
	if plerr != nil {
		lg.Printf("[reconcile] wsmap 読取失敗（inject_placement 無効・デフォルト着地）: %v", plerr)
		placement = &wsmap.Map{}
	}

	defaultWSID := activeWSID // デフォルト着地先（inject_placement にマッチしない sd 用）
	for mk, d := range desired {
		var ids []string
		var ids0Title string // ids[0] の現在 title（cur から delete する前に保存）
		for pid, m := range cur {
			if injMarkerKey(m.pc, m.sid) == mk {
				if len(ids) == 0 {
					ids0Title = m.title
				}
				ids = append(ids, pid)
				delete(cur, pid)
			}
		}
		if len(ids) == 0 {
			if noCreate {
				continue
			}
			// (pc, short_dir) → label 解決。マッチすればその workspace へ着地。
			// マッチしなければデフォルト（activeWSID）で従来挙動。
			wsID := defaultWSID
			// createdRoot: この周で workspace を**新規作成**した場合の空 root pane。
			// inject pane を足した後に close して WorkspaceCreate 由来のゴミ root
			// （label="1"）を残さない。既存 workspace 再利用の周は ""＝掃除不要。
			var createdRoot string
			if label := placement.ResolveInject(d.pc, d.dir); label != "" {
				id, root, werr := wsmap.ResolveWorkspaceIDWithRoot(api, label)
				if werr != nil {
					lg.Printf("[reconcile] inject_placement %s/%s → %q 解決失敗（デフォルト着地に fallback）: %v", d.pc, d.dir, label, werr)
				} else {
					wsID = id
					createdRoot = root
				}
			}
			if wsID == "" {
				id, root, werr := wsmap.ResolveWorkspaceIDWithRoot(api, injWorkspace)
				if werr != nil {
					lg.Printf("[reconcile] workspace 解決失敗（作成 skip）: %v", werr)
					continue
				}
				wsID = id
				createdRoot = root
				defaultWSID = id // 以降のイテレーションで再解決を避ける
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
				Title:  injPaneTitle(d.pc, d.cwd, d.ips),
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
			mirrorInjectedAgent(api, pid, d.name, d.status, reported, lg)
			// workspace を新規作成した周のみ: inject pane が入った後に空 root pane を掃除。
			// WorkspaceCreate 由来の label="1" ゴミ root を残さない（既存 ws 再利用は
			// createdRoot=""＝no-op）。close 失敗はログのみ（致命でない・次周に残るだけ）。
			if createdRoot != "" {
				if cerr := api.PaneClose(createdRoot); cerr != nil {
					lg.Printf("[reconcile] 新規 ws の空 root pane close 失敗 %s（残存）: %v", createdRoot, cerr)
				} else {
					lg.Printf("[reconcile] 新規 ws の空 root pane を掃除: %s", createdRoot)
				}
			}
		} else {
			for _, extra := range ids[1:] {
				_ = api.PaneClose(extra) // 重複（過去 race）を自己修復
				if idx != nil {
					_ = idx.Forget(extra)
				}
			}
			// 既存の注入 pane にもリモートの現 agent_status を転記（idle↔working 等の追随）。
			mirrorInjectedAgent(api, ids[0], d.name, d.status, reported, lg)
			// herdr 再起動で report_metadata の title は token 同様に消える（実測。
			// 上の (b) self-heal コメント参照）。desired 側の cwd がここで手に入るので、
			// 期待 title と現在値が食い違う周だけ再表明する（毎周不要な write を避ける・
			// 実運用フィードバック「インジェクト時に title が落ちる」への対処）。
			if want := injPaneTitle(d.pc, d.cwd, d.ips); ids0Title != want {
				if terr := api.PaneReportMetadata(ids[0], injSource, herdrapi.ReportMetadata{
					Title: want,
				}); terr != nil {
					lg.Printf("[reconcile] title 再表明失敗 %s（次周に再試行）: %v", ids[0], terr)
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
		delete(reported, pid) // agent 転記の release 追跡から除去（pane 消滅で agent も消える。nil map でも安全）
		lg.Printf("[reconcile] CLOSE %s（リモート消滅）", pid)
	}
	// ⚠ token 無し pane の一括掃除はしない: 注入 workspace には WorkspaceCreate 由来の
	// **構造 root pane（token 無し）**が常駐する（実 herdr 0.7.4 で実測）ため、「token 無し
	// ＝孤児」で掃除すると root pane を毎周 kill してしまう。index 権威になった今も、index に
	// 無い token 無し pane（ユーザーが任意に置いた pane・root pane）を触らない規律は維持。
	// 再起動で token を失った pane は上の cur 救済で index から復元して token 再表明する。
}

// remoteInjectBackstopPoll は push（WatchSessions）が死んだままでも reconcile が
// 止まらないための周期 backstop（DESIGN: producer 側 defaultTick の「events
// nudge＋周期 poll backstop」と同じ思想の注入側版）。実障害で確認: Firestore の
// gRPC ストリーム（Snapshots）はネットワーク切断（Wi-Fi 切替・VPN 再接続等）後に
// エラーを返さず無期限ブロックすることがあり、その場合 WatchSessions の
// keepSubscribed 再購読ループそのものが動かなくなる＝kick が一切来ず
// reconcileRemote が完全停止する（手動 kickstart まで自動復旧しなかった）。
// Firestore 読み取りが発生するため producer の 5s より大きく取り、
// near-$0 設計を大きく損なわない値にする。
const remoteInjectBackstopPoll = 2 * time.Minute

// remoteInjectTimeout は reconcileRemote 1 周（PaneList/workspace.list/
// DroverPCs/ListSessions 一式）に許す上限。同じ実障害（Firestore gRPC 呼び出しの
// 無期限ブロック）が reconcileRemote 内で起きても、この 1 周だけ abort して
// メインループ（trigger 待ち）へ確実に戻すための保険（remoteInjectBackstopPoll
// の kick は「trigger に積む」だけで、reconcileRemote 自体が固まっていれば
// 積まれた kick は消費されず無意味＝この timeout が実質の回復手段）。
const remoteInjectTimeout = 20 * time.Second

// runRemoteInject は他 PC のセッションをローカル herdr へ注入し続ける（push 駆動:
// 起動時 1 回＋WatchSessions 変更のたび reconcile＋remoteInjectBackstopPoll 周期の
// backstop）。ctx 終了で戻る。**primary クラウドのみ**が呼ぶ（複数クラウドが同一
// herdr へ同 pane を注入する二重窓・競合を構造的に防ぐ＝runOneCloud の primary
// 分岐から起動）。
func runRemoteInject(ctx context.Context, api *herdrapi.Client, st *state.Client,
	cl Cloud, idx *injectindex.Index, lg *log.Logger, mirrorAgents bool) {

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

	go func() {
		t := time.NewTicker(remoteInjectBackstopPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				kick()
			}
		}
	}()

	// reported は注入 pane の agent 転記（pane.report_agent）の release 追跡用に
	// reconcile 間で持ち回る（paneID→最後に report した agent label）。mirrorAgents が
	// false（既定・opt-in）なら nil のまま渡す＝reconcile は転記を一切しない。
	var reported map[string]string
	if mirrorAgents {
		reported = map[string]string{}
		lg.Printf("[reconcile] agent 転記 有効（DROVER_MIRROR_AGENTS=on・↗窓 を herdr に agent 検出させる）")
	}
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
			// ⚠ reconcileRemote へ渡す ctx は 1 周のタイムアウトを持たせる（実障害で
			// 確認: DroverPCs/ListSessions の Firestore 呼び出しがネットワーク切断で
			// 応答なく無期限ブロックし、このループ自身が停止した＝上の
			// remoteInjectBackstopPoll の kick も trigger に積まれるだけで消費されず
			// 効かない。1 周のタイムアウトで確実にループへ戻す）。ctx（親）が先に
			// Done なら Done を優先。
			rctx, rcancel := context.WithTimeout(ctx, remoteInjectTimeout)
			reconcileRemote(rctx, api, st, cl, selfExe, idx, lg, reported)
			rcancel()
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
//
//	(a) pane.list に token あり／index に無し → index に取り込む（AdoptToken）
//	(b) pane.list に token 無し／index に非 Pending entry あり → pane に token 再表明
//	(c) index に entry あり／pane.list に無し → index から Forget（stale 掃除）
//
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
