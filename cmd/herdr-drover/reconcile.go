package main

// reconcile — リモート pane 注入（Phase 3・↗窓相当）。他 PC の claude セッションを
// ローカル herdr へ **注入 pane** として出現させ、出現/消滅/重複を自己修復する。
// cm internal/cloud/agent/remotesync.go（tmux ↗窓）の herdr pane 版。
//
// 各注入 pane は `herdr-drover attach <pc> <sid>` を実行（attach.go が primary
// クラウドの relay へ viewer 接続しリモート画面を映す）。identity は二重符号化:
//   - metadata token（source=drover-inj・pc/sid）＝reconcile の cur 認識（exact-match）
//   - launch argv（attach <pc> <sid>）＝pane プロセス自身の接続先
// fail-safe: pane.list / ListPCs / ListSessions のどれかが失敗した周は **何もせず
// abort**（desired を空とみなして全 kill する破壊を防ぐ。idempotent＝次で再試行）。
// 暴走上限ガード（M8f2 教訓）: cur が desired を著しく超える周は作成停止＝整理のみ。

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

const (
	injSource    = "drover-inj"    // pane.report_metadata の source
	injTokPC     = "drover_inj_pc" // identity token キー（exact-match・非ヒューリスティック）
	injTokSID    = "drover_inj_sid"
	injWorkspace = "↗remote" // 注入 pane を集める workspace（実作業 ws を汚さない）
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
	ListPCs(ctx context.Context) ([]string, error)
	ListSessions(ctx context.Context, pc string) ([]map[string]any, error)
}

// reconcileRemote は他 PC のセッションをローカル herdr の注入 pane へ同期する 1 周。
func reconcileRemote(ctx context.Context, api *herdrapi.Client, st remoteSource,
	selfPC, selfExe string, lg *log.Logger) {

	// cur: 既存の注入 pane（metadata token pc/sid 付き）。list 失敗は abort＝
	// 「注入 pane ゼロ」と誤認して毎周全作成する runaway を防ぐ。
	panes, err := api.PaneList()
	if err != nil {
		lg.Printf("[reconcile] ABORT: pane.list 失敗（作成/削除しない）: %v", err)
		return
	}
	cur := map[string]injMeta{} // pane_id -> (pc,sid)
	for i := range panes {
		p := &panes[i]
		pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]
		if pc != "" && sid != "" {
			cur[p.PaneID] = injMeta{pc: pc, sid: sid}
		}
	}

	// desired: 他 PC の session。ListPCs/ListSessions 失敗は abort（desired を
	// 空とみなすと全 kill＝破壊的）。
	pcs, perr := st.ListPCs(ctx)
	if perr != nil {
		lg.Printf("[reconcile] ABORT: ListPCs 失敗: %v", perr)
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
	lg.Printf("[reconcile] desired=%d cur(injected)=%d", len(desired), len(cur))

	// 暴走上限ガード（検出が部分的に壊れても pane が無限増殖しない最後の砦）。
	maxPanes := len(desired)*3 + 8
	noCreate := len(cur) > maxPanes
	if noCreate {
		lg.Printf("[reconcile] CAP: cur=%d > %d → 作成停止し整理のみ", len(cur), maxPanes)
	}

	wsID := "" // 注入 workspace（作成が要るときだけ遅延解決＝空 workspace を作らない）
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
			pid, aerr := applyClaudeTab(api, wsID, injTabName(d.dir), argv, "")
			if aerr != nil {
				lg.Printf("[reconcile] CREATE 失敗 %s/%s: %v", d.pc, d.sid, aerr)
				continue
			}
			if merr := api.PaneReportMetadata(pid, injSource, herdrapi.ReportMetadata{
				Tokens: map[string]string{injTokPC: d.pc, injTokSID: d.sid},
			}); merr != nil {
				// token 未付与でも launch argv があり、次周の dup 整理で自己修復。
				lg.Printf("[reconcile] metadata 付与失敗 %s（次周整理で収束）: %v", pid, merr)
			}
			lg.Printf("[reconcile] CREATE %s/%s -> %s", d.pc, d.sid, pid)
		} else {
			for _, extra := range ids[1:] {
				_ = api.PaneClose(extra) // 重複（過去 race）を自己修復
			}
		}
	}
	// desired に無い注入 pane = リモートで消えたセッション → close。
	for pid := range cur {
		_ = api.PaneClose(pid)
		lg.Printf("[reconcile] CLOSE %s（リモート消滅）", pid)
	}
}

// runRemoteInject は他 PC のセッションをローカル herdr へ注入し続ける（push 駆動:
// 起動時 1 回＋WatchSessions 変更のたび reconcile。5s ポーリングしない）。ctx 終了で
// 戻る。**primary クラウドのみ**が呼ぶ（複数クラウドが同一 herdr へ同 pane を注入する
// 二重窓・競合を構造的に防ぐ＝runOneCloud の primary 分岐から起動）。
func runRemoteInject(ctx context.Context, api *herdrapi.Client, st *state.Client,
	selfPC string, lg *log.Logger) {

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
			reconcileRemote(ctx, api, st, selfPC, selfExe, lg)
		}
	}
}
