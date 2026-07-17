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

// webTerm は Web ターミナル経路の agent 側状態（WatchWake＋sid 毎 bridge）。
type webTerm struct {
	relayURL string
	idle     time.Duration // quiescence 自切断（0=bridge 既定 30s。DROVER_IDLE 由来）
	st       *state.Client
	hcli     *herdrapi.Client
	lg       *log.Logger

	mu     sync.Mutex
	active map[string]bool // 同 sid の二重ブリッジ防止（cm agent と同じ）
	wg     sync.WaitGroup  // WatchWake＋全 bridge の停止待ち（drain 用）
}

func newWebTerm(relayURL string, idle time.Duration, st *state.Client, hcli *herdrapi.Client, lg *log.Logger) *webTerm {
	return &webTerm{
		relayURL: relayURL,
		idle:     idle,
		st:       st,
		hcli:     hcli,
		lg:       lg,
		active:   map[string]bool{},
	}
}

// start は制御線（WatchWake）を goroutine で起動する。wake ごとに
// handleWake を goroutine 起動（watcher は止めない＝cm Agent.Run と同型）。
// WatchWake は内部で再購読 backoff を持ち ctx 終了まで戻らない（watch.go）。
func (w *webTerm) start(ctx context.Context) {
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
	w.mu.Lock()
	if w.active[sid] {
		w.mu.Unlock()
		w.lg.Printf("webterm: 多重 wake 無視 sid=%q（bridge 稼働中）", sid)
		return
	}
	w.active[sid] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.active, sid)
		w.mu.Unlock()
	}()

	// ③公開 /session 認可のための短命 source グラント（SA を持つ正規 agent
	// だけが書ける＝relay が CheckRelayGrant で検証）。cm parity: エラーは
	// 無視して dial へ進む（grant 不備なら relay が 403 で落とす＝fail-closed
	// は relay 側が担保）。
	_ = w.st.PutRelayGrant(ctx, sid, "source", sourceGrantTTL)

	// ④データ線: relay へ source として WSS dial → bridge へ渡す。
	// dial 契約は cm relay.Dial の byte 同一コピー（relayclient.go）:
	// URL = baseURL + "/session?sid=" + sid + "&role=source"（sid は
	// エスケープしない・DialOptions{} 既定）→ NetConn(MessageBinary)。
	// conn の寿命は ctx に束縛（cancel＝SIGTERM で read/write が死ぬ）。
	conn, err := relayclient.Dial(ctx, w.relayURL, sid, "source")
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
	if err := b.Run(ctx); err != nil && ctx.Err() == nil {
		w.lg.Printf("webterm: bridge 終了 sid=%q: %v", sid, err)
		return
	}
	w.lg.Printf("webterm: bridge 終了 sid=%q（正常）", sid)
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
