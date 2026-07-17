package main

// webterm — Phase 2「Web ターミナル」の agent 側配線（制御線→データ線）。
//
// データフロー（DESIGN「Web ターミナル」・cm ワイヤ契約から抽出）:
//
//	ブラウザ /term → relay(viewer) → Firestore wake/{pc}
//	 → WatchWake（本ファイル・常時無料の制御線）
//	 → PutRelayGrant(sid,"source",60s) → relay へ WSS dial（role=source）
//	 → bridge（observe spawn＋cm-wire アダプタ）へ conn を渡す
//
// cm の internal/cloud/agent.Agent.handleWake（agent.go:43-75）と同型の規律:
//   - 失効済（IsSelfRevoked）なら接続しない（relay の grant 検査が権威・
//     agent 側は防御多重）
//   - sid 毎にデータ線は 1 本（active map）。多重 wake は既存 bridge 生存中
//     なら無視する（viewer タブ再読込等で wake は容易に連打される）
//   - grant 書込エラーは無視（best-effort・毎接続書き直し。認可の権威は
//     relay 側 CheckRelayGrant。ローカル relay=Grant フック nil では
//     そもそも不要だが、本番と同順で常に書く＝経路を分岐させない）
//   - SIGTERM（ctx cancel）で WatchWake と全 bridge が停止する。conn は
//     dial 時の ctx に束縛される（websocket.NetConn）ので cancel で必ず死ぬ

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/4noha/herdr-drover/internal/bridge"
	"github.com/4noha/herdr-drover/internal/cloud/relayclient"
	"github.com/4noha/herdr-drover/internal/cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// sourceGrantTTL は source グラントの寿命（cm agent.go:72 と同値。接続
// レイテンシ＋再接続間隔をカバーする短さ＝漏洩 grant の悪用窓を最小化）。
const sourceGrantTTL = 60 * time.Second

// respawnStopWait は respawn（restart-proxy 遠隔命令）が旧 bridge の停止を
// 待つ上限。bridge は ctx cancel で observe kill→conn close まで確実に戻る
// 設計だが、万一戻らない時に命令を pending 滞留させず error Ack へ落とす。
const respawnStopWait = 10 * time.Second

// bridgeRun は稼働中 bridge 1 本のハンドル（respawn 用）。cancel はその
// bridge 専用 ctx を切る（conn/observe が確実に死ぬ）。done は handleWake
// が active map から自分を消した**後**に閉じる＝done 受信後の再 spawn が
// 二重ブリッジ dedup に誤爆しない順序保証。
type bridgeRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// webTerm は Web ターミナル経路の agent 側状態（WatchWake＋sid 毎 bridge）。
type webTerm struct {
	relayURL string
	idle     time.Duration // quiescence 自切断（0=bridge 既定 30s。DROVER_IDLE 由来）
	st       *state.Client
	hcli     *herdrapi.Client
	lg       *log.Logger

	mu     sync.Mutex
	ctx    context.Context       // start() の親 ctx（respawn の再 spawn 用）
	active map[string]*bridgeRun // 同 sid の二重ブリッジ防止＋respawn ハンドル
	wg     sync.WaitGroup        // WatchWake＋全 bridge の停止待ち（drain 用）
}

func newWebTerm(relayURL string, idle time.Duration, st *state.Client, hcli *herdrapi.Client, lg *log.Logger) *webTerm {
	return &webTerm{
		relayURL: relayURL,
		idle:     idle,
		st:       st,
		hcli:     hcli,
		lg:       lg,
		active:   map[string]*bridgeRun{},
	}
}

// start は制御線（WatchWake）を goroutine で起動する。wake ごとに
// handleWake を goroutine 起動（watcher は止めない＝cm Agent.Run と同型）。
// WatchWake は内部で再購読 backoff を持ち ctx 終了まで戻らない（watch.go）。
func (w *webTerm) start(ctx context.Context) {
	// respawn（遠隔命令）が新 bridge を張るときの親 ctx を保存（mu 下＝
	// 命令 goroutine からの読みと data race にしない）。
	w.mu.Lock()
	w.ctx = ctx
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		err := w.st.WatchWake(ctx, func(sid string) {
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				w.handleWake(ctx, sid)
			}()
		})
		if err != nil && ctx.Err() == nil {
			// ctx 生存中の終了は異常（keepSubscribed は本来 ctx 終了まで
			// 戻らない）。ログだけ残す＝agent 本体（一覧同期）は道連れに
			// しない。
			w.lg.Printf("webterm: WatchWake 異常終了: %v", err)
		}
	}()
}

// handleWake は wake 1 件を処理する（cm Agent.handleWake と同順・同規律）。
func (w *webTerm) handleWake(ctx context.Context, sid string) {
	// ①強制失効済なら source 接続しない（防御多重。tick 側 dormant と同じ
	// 規律: 解除済み端末が Web に蘇らない）。
	if w.st.IsSelfRevoked(ctx) {
		return
	}
	// ②sid 毎 1 本: 既にデータ線が開いている sid の wake は無視。
	// 注意: WatchWake は初回 snapshot でも既在 doc で発火する（契約）ので、
	// agent 再起動直後に古い wake が来ても、この dedup と bridge 側の
	// 失敗即終了で無害に収束する。
	//
	// respawn（restart-proxy 遠隔命令）用に bridge 専用の子 ctx と完了
	// チャネルを持つ。defer の実行順（LIFO）が生命線:
	//   conn.Close（後で登録）→ active 削除＋close(done) → bcancel
	// ＝done を見た respawn が再 spawn する時点で active は必ず空いている。
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel()
	done := make(chan struct{})
	w.mu.Lock()
	if w.active[sid] != nil {
		w.mu.Unlock()
		bcancel()
		w.lg.Printf("webterm: 多重 wake 無視 sid=%q（bridge 稼働中）", sid)
		return
	}
	w.active[sid] = &bridgeRun{cancel: bcancel, done: done}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.active, sid)
		w.mu.Unlock()
		close(done)
	}()

	// ③公開 /session 認可のための短命 source グラント（SA を持つ正規 agent
	// だけが書ける＝relay が CheckRelayGrant で検証）。cm parity: エラーは
	// 無視して dial へ進む（grant 不備なら relay が 403 で落とす＝fail-closed
	// は relay 側が担保）。
	_ = w.st.PutRelayGrant(bctx, sid, "source", sourceGrantTTL)

	// ④データ線: relay へ source として WSS dial → bridge へ渡す。
	// dial 契約は cm relay.Dial の byte 同一コピー（relayclient.go）:
	// URL = baseURL + "/session?sid=" + sid + "&role=source"（sid は
	// エスケープしない・DialOptions{} 既定）→ NetConn(MessageBinary)。
	// conn の寿命は ctx に束縛（cancel＝SIGTERM で read/write が死ぬ）。
	conn, err := relayclient.Dial(bctx, w.relayURL, sid, "source")
	if err != nil {
		w.lg.Printf("webterm: relay dial 失敗 sid=%q: %v", sid, err)
		return
	}
	defer conn.Close()

	// relay sid → herdr pane_id の対応付けは wake ハンドラ（ここ）の責務
	//（bridge.Bridge.Sid の契約）。派生 sid `<pane_id>#inj`（DESIGN・
	// リモート pane 注入の viewer 用）は exact-match の suffix 剥がしのみ
	//（ヒューリスティック禁止）。通常の Web /term は sid=pane_id そのまま。
	paneID := sid
	if base, found := strings.CutSuffix(sid, "#inj"); found {
		paneID = base
	}

	// pane の実在検証は bridge の責務: 不在 pane なら observe spawn が失敗
	// し backoff リトライ→conn 切断で戻る（cm では ResolveSock が agent 側
	// ゲートだったが、drover の pane 解決は observe 側に集約＝二重に持たない）。
	//
	// 契約 = bridge.New(sid, conn, herdrClient) → Run(ctx)（着地形で確定）:
	//   - conn は dial 済み source 役バイトストリーム（io.Closer なら Run
	//     終了/quiescence 自切断で bridge が close。ここの defer は二重で無害）
	//   - quiescence（無通信 30s 自切断＝cm idlePump 相当）は bridge の
	//     Idle 既定値が担い、Run は nil（正常）で戻る＝次の wake 待ちへ
	//   - ctx cancel（SIGTERM）で必ず戻り observe サブプロセスを残さない
	w.lg.Printf("webterm: bridge 開始 sid=%q", sid)
	b := bridge.New(paneID, conn, w.hcli)
	// quiescence（無通信自切断）は DROVER_IDLE で調整可（0 なら bridge 既定
	// 30s＝cm 本番 IdleClose と同値）。e2e が短い idle で実切断を検証する。
	b.Idle = w.idle
	b.Logf = func(format string, args ...any) {
		w.lg.Printf("webterm: bridge sid=%q: "+format, append([]any{sid}, args...)...)
	}
	// bctx（bridge 専用）で回す: SIGTERM（親 cancel 連鎖）でも respawn
	// （bcancel 単独）でも必ず戻る。respawn 由来の切断はエラー扱いしない。
	if err := b.Run(bctx); err != nil && bctx.Err() == nil {
		w.lg.Printf("webterm: bridge 終了 sid=%q: %v", sid, err)
		return
	}
	w.lg.Printf("webterm: bridge 終了 sid=%q（正常）", sid)
}

// respawn は restart-proxy 遠隔命令の実体: 当該 sid の**稼働中** bridge を
// 停止（observe subprocess も掃除される）し、同 sid で新しい bridge を
// 張り直す（cm の proxy --resume 再起動に対応する drover 版）。
//
//   - 稼働 bridge が無い sid は error（呼び手 CommandRunner が status=error
//     で Ack＝pending 滞留させない。ヒューリスティックな探索はしない＝
//     active map の exact-match のみ）
//   - 新 bridge は relay へ source として dial し「初回 RESIZE magic 待ち」
//     から始まる。viewer 側は relay の同 sid takeover semantics で再接続
//     すれば新 full frame を受ける（誰も来なければ quiescence が畳む）
func (w *webTerm) respawn(sid string) error {
	w.mu.Lock()
	h := w.active[sid]
	ctx := w.ctx
	w.mu.Unlock()
	if h == nil {
		return fmt.Errorf("sid %q の稼働 bridge が無い（Web ターミナル未接続の sid は respawn 不能）", sid)
	}
	w.lg.Printf("webterm: respawn 要求 sid=%q（旧 bridge を停止→張り直し）", sid)
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(respawnStopWait):
		return fmt.Errorf("sid %q の旧 bridge が %s 以内に停止しない（respawn 中断）", sid, respawnStopWait)
	}
	if ctx == nil || ctx.Err() != nil {
		return fmt.Errorf("agent 停止中のため respawn しない")
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.handleWake(ctx, sid)
	}()
	return nil
}

// drain は SIGTERM 後の graceful 猶予: ctx cancel で WatchWake と全 bridge が
// 戻るのを bounded に待つ。timeout 時はログを残して諦める（プロセス終了で
// goroutine は消える。ここで無期限に待つと「SIGTERM で終了しない agent」と
// いう更に悪い故障モードになる）。
func (w *webTerm) drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		w.lg.Printf("webterm: 停止待ち timeout（%s）＝bridge が ctx cancel に応答していない疑い", timeout)
	}
}
