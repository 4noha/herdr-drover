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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

const (
	injSource    = "drover-inj"                  // pane.report_metadata の source
	injTokPC     = herdrapi.InjTokenPC           // identity token キー（producer と共有＝herdrapi）
	injTokSID    = herdrapi.InjTokenSID          // exact-match・非ヒューリスティック
	injWorkspace = herdrapi.InjWorkspaceLabel    // 注入 pane を集める専用 workspace（＝注入判定の権威）
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

// reconcileRemote は他 PC のセッションをローカル herdr の注入 pane へ同期する 1 周。
// cl は primary クラウド（selfPC=cl.PCName で自 PC を除外・pane env に設定を注入）。
func reconcileRemote(ctx context.Context, api *herdrapi.Client, st remoteSource,
	cl Cloud, selfExe string, lg *log.Logger) {

	selfPC := cl.PCName
	env := injPaneEnv(cl)

	// cur: 既存の注入 pane（metadata token pc/sid 付き）。list 失敗は abort＝
	// 「注入 pane ゼロ」と誤認して毎周全作成する runaway を防ぐ。
	panes, err := api.PaneList()
	if err != nil {
		lg.Printf("[reconcile] ABORT: pane.list 失敗（作成/削除しない）: %v", err)
		return
	}
	// 注入 workspace の id を先に解決（無ければ "" ＝まだ作られていない）。cap は
	// この workspace の**全 pane 数**で判定する（token 無し orphan も数える＝
	// token 付与失敗で cur に映らない pane が cap を擦り抜けて無限増殖するのを防ぐ）。
	// workspace.list の失敗は abort（"" 退行で cap/orphan 掃除が無効化されるため）。
	injWsID, werr := findWorkspaceID(api, injWorkspace)
	if werr != nil {
		lg.Printf("[reconcile] ABORT: workspace.list 失敗（cap/掃除の基準が立たない）: %v", werr)
		return
	}
	cur := map[string]injMeta{} // pane_id -> (pc,sid)
	injWsPanes := 0
	for i := range panes {
		p := &panes[i]
		pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]
		if pc != "" && sid != "" {
			cur[p.PaneID] = injMeta{pc: pc, sid: sid}
		}
		if injWsID != "" && p.WorkspaceID == injWsID {
			injWsPanes++ // cap 用（token 無しの構造 root pane も含む＝安全側の上限）
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
	lg.Printf("[reconcile] desired=%d cur(injected)=%d", len(desired), len(cur))

	// 暴走上限ガード（検出が部分的に壊れても pane が無限増殖しない最後の砦）。
	// **cur ではなく注入 workspace の全 pane 数**で判定＝token 無し orphan も
	// カウントに含める（token 付与失敗の pane は cur に映らないため cur 基準だと
	// cap を擦り抜ける。敵対的レビューで確認）。
	guard := len(cur)
	if injWsPanes > guard {
		guard = injWsPanes
	}
	maxPanes := len(desired)*3 + 8
	noCreate := guard > maxPanes
	if noCreate {
		lg.Printf("[reconcile] CAP: 注入 pane=%d > %d → 作成停止し整理のみ", guard, maxPanes)
	}

	wsID := injWsID // 既存の注入 workspace（無ければ作成時に遅延解決）
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
			if merr := api.PaneReportMetadata(pid, injSource, herdrapi.ReportMetadata{
				Tokens: map[string]string{injTokPC: d.pc, injTokSID: d.sid},
			}); merr != nil {
				// ⚠ identity は layout.apply では原子設定できない（token 非対応）。
				// token 付与に失敗した pane は cur に映らず reconcile が二度と認識
				// できない orphan になり、毎周再作成されて無限増殖する（敵対的
				// レビューで確認）。token を付けられないなら pane を即 close して
				// orphan を残さない（次周に新規作成でやり直す）。
				lg.Printf("[reconcile] metadata 付与失敗 %s → 即 close（orphan 回避）: %v", pid, merr)
				_ = api.PaneClose(pid)
				continue
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
	// ⚠ token 無し pane の一括掃除はしない: 注入 workspace には WorkspaceCreate 由来の
	// **構造 root pane（token 無し）**が常駐する（実 herdr 0.7.4 で実測）ため、「token 無し
	// ＝孤児」で掃除すると root pane を毎周 kill してしまう。代わりに再起動で token を失った
	// 復元 pane は attach プロセスが起動時に自分の HERDR_PANE_ID へ token を **再表明**して
	// 自己治癒し（attach.go）、万一 reconcile が重複を作っても上の dedup（ids[1:] close）が
	// 回収する。producer は注入 workspace 所属で push を止めるので cross-PC 増殖は起きない。
}

// runRemoteInject は他 PC のセッションをローカル herdr へ注入し続ける（push 駆動:
// 起動時 1 回＋WatchSessions 変更のたび reconcile。5s ポーリングしない）。ctx 終了で
// 戻る。**primary クラウドのみ**が呼ぶ（複数クラウドが同一 herdr へ同 pane を注入する
// 二重窓・競合を構造的に防ぐ＝runOneCloud の primary 分岐から起動）。
func runRemoteInject(ctx context.Context, api *herdrapi.Client, st *state.Client,
	cl Cloud, lg *log.Logger) {

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
			reconcileRemote(ctx, api, st, cl, selfExe, lg)
		}
	}
}

// findWorkspaceID は label の workspace id を返す（無ければ ""・error は伝播）。cap の
// 「注入 workspace の全 pane 数」カウント／orphan 掃除の基準に使う。重複 label は number
// 最小を採る（wsmap.ResolveWorkspaceID の作成選択と一致＝数える ws と作る ws が同一に
// なる）。**error を握り潰さない**: workspace.list が落ちた周に "" を返すと injWsPanes=0 に
// 退行して cap が cur-only へ弱まり orphan 掃除も無効化される＝他の scan（PaneList/
// DroverPCs/ListSessions）と同じ abort 規律に揃える（呼び手が abort する）。
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
