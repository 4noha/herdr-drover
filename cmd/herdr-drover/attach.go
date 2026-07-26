//go:build unix

package main

// attach — リモート pane 注入の viewer client（Phase 3・↗窓相当）。reconcile が
// ローカル herdr の注入 pane 内で `herdr-drover attach <pc> <sid>` として起動する。
// primary クラウドの relay へ **viewer** として接続し、リモート PC のセッション
// 画面（server-rendered ANSI フレーム）を自 stdout（＝pane PTY）へ流し、キー入力を
// cm-wire で WSS へ送る。
//
// grant フロー（drover-cloud/relay ServeHTTP は source/viewer 両方を Grant 検証。
// Web viewer は web の認証 /ws→Accept 直叩きで grant を迂回するが、native viewer は
// public /session を通るので自分で viewer grant を書く）:
//   1. PutRelayGrant(<sid>#inj, "viewer")            ← 自分の viewer 許可
//   2. Wake(remotePC, <sid>#inj)                     ← リモート agent を起こす
//      → リモート webterm.handleWake が #inj を剥がして pane <sid> を observe し
//        PutRelayGrant(<sid>#inj,"source")＋source dial（webterm.go 側で対応）
//   3. relayclient.Dial(relay, <sid>#inj, "viewer")  ← relay が source⇄viewer 突合
//
// 派生 sid `<sid>#inj` を使う理由: relay は 1 sid=viewer 1 本＝Web /term と同 sid
// だと相互蹴り出しストームになる。#inj で分離（herdr は多重 observer 可＝リモート側
// の bridge 並走は無料）。conn 切断（リモート idle quiescence 含む）は **exit せず
// backoff 再接続**（cm socket-client の「切断=窓死亡」欠陥の修正＝DESIGN）。pane 自体の
// 生死は reconcile が管理する（リモートセッション消滅で pane.close＝attach は kill される）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/4noha/drover-cloud/relayclient"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// injGrantTTL は viewer grant の寿命（webterm の source grant sourceGrantTTL と
// 同値＝relay grant 検証窓を対称に保つ）。
const injGrantTTL = 60 * time.Second

// DefaultIdle は attachOnce の quiescence 読取りタイムアウト（internal/bridge
// の DefaultIdle=30s と同値。無通信 30s で「切断」とみなし backoff 再接続へ渡す）。
const DefaultIdle = 30 * time.Second

// inputWriteTimeout は connHolder.write（stdin→relay conn）1 回の書込上限
// （internal/bridge.writeTimeout と同値・同クラスの対処）。実運用フィードバック
// で「TCP は ESTABLISHED のまま何を送っても pane に届かない」症状が繰り返し
// 観測された。原因は本ファイルの stdin reader が**プロセス生存中 1 goroutine
// のみ**（上のコメント参照：複数 reader のキー奪い合いを避けるための設計）で
// 逐次 write する構造にあり、relay 側（webterm の viewer accept）が読まなく
// なると conn.Write が無期限ブロックし、この 1 goroutine が固まったまま以後の
// 入力が一切処理されなくなる（bridge.go 側は同じ問題に writeTimeout で対処済み
// だったが、viewer 側の本パスには対応漏れがあった）。タイムアウトで打ち切り、
// conn を close して attachOnce 側の backoff 再接続に委ねる。
const inputWriteTimeout = 2 * time.Second

// dialTimeout は attachOnce の relay dial（DialViewerFrom）に許す上限。
// 実障害（2026-07-23・ノート PC の Wi-Fi 切替）: dial 自体がネットワーク不通の
// まま応答なく長時間ブロックし得た。grant/wake の 10s（injGrantTTL 手前の
// gctx）より少し長く取り、正常なネットワークでの dial（実測 1s 未満）を
// 誤検知しない範囲で切る。
const dialTimeout = 15 * time.Second

// watchLifecycleTick は watchLifecycle のポーリング周期。3s は Go doc の
// スリープ検知パターン（wall clock diff）と NIC diff の両方に対して低負荷
// （net.InterfaceAddrs は syscall 1 回で sub-ms）で、検知遅延は 3〜6s。
const watchLifecycleTick = 3 * time.Second

// watchLifecycleClockJump はスリープ復帰と判定する wall clock ジャンプ幅。
// Go の time.Time の monotonic clock は macOS/Linux 共に S3/S4 sleep 中に停止する
// （time パッケージ公式 doc 記載）ため、time.Now().Round(0) で monotonic を剥がして
// wall clock で比較する。>15s は NTP step（通常 ms オーダー）を十分マージンで
// 超えつつ、典型 sleep（秒〜時間）を確実に捕える閾値。pumpFrames の DefaultIdle=30s
// より短く proactive に切断＝quiescence 到達前に再接続を開始できる。
const watchLifecycleClockJump = 15 * time.Second

// watchLifecycleCooldown はスリープ復帰検知と NIC 変化検知の共有 cooldown。
// (1) sleep/wake は「wall clock jump→数秒後に NIC associate 完了」の 2 段発火に
// なりがちで、両者を 30s 窓で束ねて 1 回だけの再接続に集約する。
// (2) Wi-Fi 切替は「旧 set→空→新 set」の 2 段遷移で NIC diff が 2 回来るが、
// 同じ cooldown で 2 回目を抑止する。
// (3) pumpFrames の DefaultIdle=30s と同じ長さにして、正常な quiescence 再接続
// の頻度以上には切らないよう保つ。
const watchLifecycleCooldown = 30 * time.Second

// watchLifecycle は cmdAttach の goroutine として常駐し、スリープ復帰・NIC 変化を
// 検知したら holder.forceClose と wakeCh 送信で「今すぐ再接続」を促す。
//   - ctx は cmdAttach の signal.NotifyContext（プロセス寿命）。attachOnce の dctx
//     に紐付けると backoff 中は停止＝主たる発生窓（backoff 30s に伸びた後）で検知
//     漏れになるため cmdAttach スコープが正。
//   - nowFn/fingerprintFn/tickCh は単体テスト用の DI seam（fake clock / fake NIC を
//     注入して sleep 実測なしで検証）。nil ならデフォルト（time.Now・nicFingerprint・
//     time.NewTicker）。baselineReady は「baseline 取得完了」の合図（テスト側で
//     nowFn/fingerprintFn を変える前に待つためのバリア。nil なら合図しない）。
//   - 検知時のアクションは holder.forceClose（現 conn を能動的に切断＝pumpFrames が
//     Read err で return→attachOnce 終了→cmdAttach の backoff）と、wakeCh への
//     非ブロッキング送信（backoff sleep 中なら select を早期に解く）の 2 経路を毎回
//     ペアで行う。attachOnce 実行中は前者が効き、backoff sleep 中は後者が効き、
//     両分岐が同時成立しても副作用なし。
//
// NIC transient 空の扱い: 中間 tick で fingerprint が "" を返す（syscall 一時失敗
// 等）ときは **lastFP を書き換えない**。空を経由した a→""→b の遷移で真の変化 (a→b)
// を吸収しないため（敵対的レビュー指摘に対処）。空↔非空の初回遷移（起動直後の
// baseline が空だった場合の実 NIC 検出）は 1 回だけ許容し以後は lastFP を非空で保つ。
func watchLifecycle(
	ctx context.Context,
	holder *connHolder,
	wakeCh chan<- struct{},
	nowFn func() time.Time,
	fingerprintFn func() string,
	tickCh <-chan time.Time,
	notify func(reason string),
	baselineReady chan<- struct{},
) {
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().Round(0) }
	}
	if fingerprintFn == nil {
		fingerprintFn = nicFingerprint
	}
	if tickCh == nil {
		t := time.NewTicker(watchLifecycleTick)
		defer t.Stop()
		tickCh = t.C
	}

	// baseline を初回 tick 前に取る（起動直後の誤発火を防ぐ＝初回 tick で「変化した」
	// 判定になるのを避けるため）。
	last := nowFn()
	lastFP := fingerprintFn()
	if baselineReady != nil {
		close(baselineReady) // テストが nowFn/fingerprintFn を変える前の同期点
	}
	// cooldownUntil はスリープ復帰と NIC 変化が共有する。zero value は epoch=常時
	// 発火可能。発火時刻 + watchLifecycleCooldown へ更新する。
	var cooldownUntil time.Time

	kick := func(reason string) {
		if notify != nil {
			notify(reason)
		}
		holder.forceClose()
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tickCh:
			now := nowFn()
			fp := fingerprintFn()
			// wall clock の**後退**（NTP の大幅補正・VM スナップショット復元）。
			// cooldownUntil は旧時計基準の絶対時刻なので、後退すると未来に取り
			// 残されて以後の検知を長時間塞ぐ。基準を捨てて即時発火可能へ戻す
			// （後退そのものは環境変化ではないので kick はしない）。
			if now.Before(last) {
				cooldownUntil = time.Time{}
			}
			jumped := now.Sub(last) > watchLifecycleClockJump
			// changed 判定は「非空同士の差分」に限定。fp=="" (net.InterfaceAddrs
			// 一時失敗) は lastFP を書き換えず保持＝次に非空が戻った時に本来の
			// diff が生きるようにする（transient 空を挟んだ real change の吸収を防ぐ）。
			changed := fp != "" && lastFP != "" && fp != lastFP
			last = now
			if fp != "" && lastFP == "" {
				// 初回 baseline が空だったケースの最初の非空取得＝現在値を新 baseline
				// として採用のみで発火しない（起動直後の誤発火防止）。以後 lastFP は
				// 非空で維持されるので transient 空を挟んでも本フォールバックは走らない。
				lastFP = fp
			}

			if !jumped && !changed {
				continue
			}
			if now.Before(cooldownUntil) {
				// cooldown 中の変化は最新値で追随のみ（cooldown 明けに 1 回だけ
				// 発火するよう lastFP は更新して次周期以降の diff を正しくする）。
				if changed {
					lastFP = fp
				}
				continue
			}
			cooldownUntil = now.Add(watchLifecycleCooldown)
			switch {
			case jumped && changed:
				kick("スリープ復帰＋NIC 変化")
			case jumped:
				kick("スリープ復帰")
			default:
				kick("NIC 変化")
			}
			if changed {
				lastFP = fp
			}
		}
	}
}

// connHolder は「現在の接続」を保持し、常駐 stdin reader が接続切替を跨いで現接続
// へ書けるようにする（reader を cycle ごとに作らない＝キーストローク奪い合い防止）。
// 未接続(nil)中の入力は破棄する（次の接続確立後の入力から届く）。
type connHolder struct {
	mu sync.Mutex
	c  net.Conn
}

func (h *connHolder) set(c net.Conn) {
	h.mu.Lock()
	h.c = c
	h.mu.Unlock()
}

// write は現接続へ書く。inputWriteTimeout 超過は無期限ブロックの回避策として
// conn を close する（close 後の Write はエラーで即返るため、以後の write 呼出
// がここで再ブロックすることはない）。attachOnce 側の pumpFrames が conn の
// Read エラーで close を検知して backoff 再接続する（この関数自身は再接続しない
// ＝責務分離）。
func (h *connHolder) write(p []byte) error {
	h.mu.Lock()
	c := h.c
	h.mu.Unlock()
	if c == nil {
		return nil // 未接続中の入力は破棄（次の接続から届く）
	}
	_ = c.SetWriteDeadline(time.Now().Add(inputWriteTimeout))
	_, err := c.Write(p)
	if err != nil {
		_ = c.Close()
	}
	return err
}

// forceClose は現接続を能動的に close する（watchLifecycle 経由でスリープ復帰・
// NIC 変化を検知した時に呼ばれる。pumpFrames が Read err で return→attachOnce
// 終了→cmdAttach の backoff ループが新 conn で再 dial、の既存経路にそのまま乗る）。
// set(nil) は close を伴わない参照外しなので意味論が違う＝別メソッドで分ける。
// 二重 Close は net.Conn の冪等契約（および attachOnce の defer conn.Close と
// SIGWINCH goroutine の `<-dctx.Done() で Close`）で吸収される＝戻り値は無視。
// c==nil（未接続中＝backoff sleep 中）は no-op。
func (h *connHolder) forceClose() {
	h.mu.Lock()
	c := h.c
	h.c = nil
	h.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// dialWithTimeout は dial（ブロッキング呼び出し）を別 goroutine で走らせ、
// timeout 以内に完了しなければ dial 側の ctx を cancel（cancelDialCtx）して
// タイムアウトエラーを返す純粋な制御フロー（attachOnce から切り出し・fake dial
// 関数で単体テスト可能にする）。実障害（2026-07-23・ノート PC の Wi-Fi 切替）:
// relayclient.DialViewerFrom へ渡す ctx は dial だけでなく websocket.NetConn の
// 生存期間全体を縛る（coder/websocket の仕様）ため、dial 自体にタイムアウト付き
// ctx を直接渡すと dial 成功後も期限切れで正常な接続まで切れる回帰になる。ここで
// 外側から select でタイムアウトを掛け、dial に渡す ctx 自体（呼び手が保持する
// dctx）は変えない。タイムアウト後に dial goroutine が遅れて成功しても、その
// conn は即 Close する（呼び手はもう待っていない＝リーク防止）。
func dialWithTimeout(timeout time.Duration, cancelDialCtx func(), dial func() (net.Conn, error)) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, err := dial()
		done <- result{c, err}
	}()
	select {
	case r := <-done:
		return r.conn, r.err
	case <-time.After(timeout):
		cancelDialCtx()
		go func() {
			if r := <-done; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, fmt.Errorf("relay 接続タイムアウト（%s）", timeout)
	}
}

// cmdAttach は `herdr-drover attach <pc> <sid>`。ctx 終了（SIGTERM/pane close で
// herdr が kill）まで backoff 再接続し続ける。
func cmdAttach(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("%w: herdr-drover attach <pc> <sid>（reconcile が注入 pane 内で起動する内部コマンド）", errUsage)
	}
	remotePC, sid := args[0], args[1]
	injSid := sid + "#inj"

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	out, _ := stdout.(*os.File)
	if out == nil {
		out = os.Stdout
	}
	fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s / %s に接続中...\r\n", remotePC, sid)

	// この pane 自身に注入 identity token を再表明する（自己治癒）。herdr サーバ
	// 再起動で pane の report_metadata token は消える（実 herdr 0.7.4 で実測）が、
	// pane の argv（attach pc sid）と HERDR_PANE_ID は復元されるので、復元後に
	// 起動したここで token を貼り直せば reconcile が cur で認識でき **重複を作らない**
	// （token 無し pane を一括掃除すると注入 ws の構造 root pane まで殺すため掃除は
	// しない設計＝reconcile.go 参照）。初回は reconcile の post-create token と二重だが
	// exact 値ゆえ冪等。best-effort（失敗しても relay 接続は続ける）。
	if pid := os.Getenv("HERDR_PANE_ID"); pid != "" {
		_ = herdrapi.New("").PaneReportMetadata(pid, injSource, herdrapi.ReportMetadata{
			Tokens: map[string]string{herdrapi.InjTokenPC: remotePC, herdrapi.InjTokenSID: sid},
		})
	}

	// stdin reader は **プロセス生存中 1 本だけ**（cycle ごとに spawn すると前
	// reader が os.Stdin.Read でブロックしたまま残り、複数 reader がキーストローク
	// を奪い合って取りこぼす＝敵対的レビューで確認）。現接続へ転送し、未接続中の
	// 入力は破棄する。プロセス終了（pane close で herdr が kill）で goroutine も消える。
	// pane PTY を raw モードにする。canonical（行バッファ）のままだと owner が
	// ↗窓 で打鍵しても Enter まで os.Stdin.Read が返らず、リモートへ入力が届か
	// ない（Web は xterm.js が raw 相当なので効く＝↗窓 だけ owner→remote 入力が
	// 効かなかった実バグ・master/slave 問わず）。出力（frames→out）は raw 不要
	// なので表示は動いていた。cfmakeraw で OPOST も落ち frame の生 ANSI 透過も
	// 正しくなる（localview と同一 helper・同パッケージ）。TTY でなければ skip。
	inFD := int(os.Stdin.Fd())
	if old, rerr := enterRaw(inFD); rerr == nil {
		defer restoreRaw(inFD, old)
	}
	holder := &connHolder{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				_ = holder.write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()

	// wakeCh はスリープ復帰・NIC 変化検知時に watchLifecycle が非ブロッキング送信
	// する（capacity 1 で最新のみ保持）。backoff sleep 中の select を早期に解いて
	// 「復帰直後の再接続を最大 backoff-1s ではなく即時に」する。attachOnce 実行中は
	// holder.forceClose 側が pumpFrames Read err を誘発するので wakeCh は buffer に
	// 溜まって次の backoff 局面で 1 回消費されるだけ＝副作用なし。
	wakeCh := make(chan struct{}, 1)
	go watchLifecycle(ctx, holder, wakeCh, nil, nil, nil, func(reason string) {
		fmt.Fprintf(out, "\r\n↗ %s: %s — 再接続します\r\n", remotePC, reason)
	}, nil)

	// ⚠ この pane は reconcile が管理する（リモートセッション消滅で pane.close）。
	// attach は **設定/Firestore/relay のどの失敗でも exit しない**（exit すると
	// pane が死に→次周の reconcile が再作成→runaway churn になる。実障害で確認）。
	// ctx cancel（SIGTERM / pane close で herdr が kill）だけで戻る＝生存し続けて
	// backoff 再試行する。
	//
	// スリープ復帰 / NIC 変化検知（watchLifecycle）は holder.forceClose→pumpFrames
	// Read err→attachOnce return→backoff sleep 中なら wakeCh で早期脱出、の経路で
	// 自動復旧する。watchLifecycle は cmdAttach ctx 寿命＝backoff 中も生存
	// （dctx に紐付けると backoff 30s に伸びた区間で検知漏れ＝あえて cmdAttach 側）。
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		idleClosed := attachCycle(ctx, injSid, remotePC, holder, out)
		if ctx.Err() != nil {
			return nil
		}
		backoff = attachBackoffReset(backoff, time.Since(start), idleClosed)
		select {
		case <-ctx.Done():
			return nil
		case <-wakeCh:
			// スリープ復帰 / NIC 変化検知＝ネットワーク条件が変わったので backoff
			// 状態自体をリセット（前回まで dial 失敗続きで 30s まで伸びていても、
			// 環境変化後の最初の再試行は即時に始めるべき）。
			backoff = 500 * time.Millisecond
			continue
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// attachBackoffReset は 1 サイクル後の backoff を決める純関数（BUG-3 の thrash 対策）。
//
// ⚠**idle 切断（無通信 quiescence）は backoff をリセットしない**。リモート pane が
// idle だと source は 30s 無通信で bridge を quiescence 自切断し、viewer 側 pumpFrames
// も idle 超過で戻る。旧実装は「サイクルが >5s 続いた＝健全な接続だった」とみなして
// backoff を 500ms へリセットし即再接続→即再 Wake していたが、これは**pane が idle な
// だけ**で、毎周期 source に observe を再 spawn させる thrash になる（実測: observe
// spawn 59,329 回 / agent.log 16.8MB）。idle 切断時はリセットせず backoff を伸ばし、
// 再接続を最大でも cap 間隔まで疎にする（＝quiescence の「無通信なら畳む」意図を活かす）。
//
// 実データが流れていた接続（idleClosed=false）が >5s 続いてから不意に切れた場合だけ、
// ネットワーク瞬断からの素早い復帰のためリセットする。接続前段の即失敗（<5s）は
// リセットせず backoff を伸ばす（source 不達を連打しない＝従来どおり）。
func attachBackoffReset(cur, cycle time.Duration, idleClosed bool) time.Duration {
	if !idleClosed && cycle > 5*time.Second {
		return 500 * time.Millisecond // 実データ接続の不意の切断＝素早く復帰
	}
	return cur
}

// attachCycle は 1 サイクル（設定解決→state→grant/wake→dial→pump）。どの段階の
// 失敗でも **exit せず戻る**（cmdAttach の backoff ループが再試行＝pane は生存）。
// 設定は primary クラウド（reconcile が pane env に GCP_PROJECT 等を注入する。
// env が無くても config.json / clouds.json から解決を試みる）。
// 戻り値 idleClosed は attachOnce から伝播する（無通信 quiescence 切断＝BUG-3）。
// 接続前段の失敗（設定/クラウド/Firestore）は idle ではない＝false。
func attachCycle(ctx context.Context, injSid, remotePC string, holder *connHolder, out *os.File) (idleClosed bool) {
	cfg, err := resolveConfig()
	if err != nil {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s: 設定解決失敗（再試行）: %v\r\n", remotePC, err)
		return false
	}
	clouds := cfg.LoadClouds()
	if len(clouds) == 0 {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s: 接続先クラウド未設定（再試行）\r\n", remotePC)
		return false
	}
	cl := clouds[0] // primary（reconcile と同じクラウドで他 PC を見ている）

	creds := cl.SAKeyPath
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" {
		creds = ""
	}
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	st, err := state.NewWithCredentials(cctx, cl.Project, cl.PCName, creds)
	if err != nil {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s: Firestore 接続失敗（再試行）: %v\r\n", remotePC, err)
		return false
	}
	defer st.Close()

	return attachOnce(cctx, st, cl.RelayURL, injSid, remotePC, holder, out)
}

// attachOnce は 1 接続の生存（grant→wake→dial→frame/input pump）。conn 切断か
// ctx 終了で戻る。エラーは画面に控えめに出し、上位の backoff ループが再接続する。
// 戻り値 idleClosed は「無通信 quiescence（idle 超過）で pumpFrames が戻った」印
// （BUG-3: cmdAttach の backoff 判断に使う）。dial 前段の失敗は false。
func attachOnce(ctx context.Context, st *state.Client, relayURL, injSid, remotePC string, holder *connHolder, out *os.File) (idleClosed bool) {
	// viewer 許可を先に置き、リモート agent を起こす（source を bridge させる）。
	gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
	_ = st.PutRelayGrant(gctx, injSid, "viewer", injGrantTTL)
	_ = st.Wake(gctx, remotePC, injSid)
	gcancel()

	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()
	// source PC を spc で渡す＝relay の KeyFor が slave source PC の時だけ
	// slaveSessionKey(spc,injSid) で viewer を Accept し、slave の #inj source と
	// ペアする（master source PC では spc 無視＝従来と同一 wire）。
	conn, err := dialWithTimeout(dialTimeout, dcancel, func() (net.Conn, error) {
		return relayclient.DialViewerFrom(dctx, relayURL, injSid, remotePC)
	})
	if err != nil {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s / %s: %v（再試行）\r\n", remotePC, injSid, err)
		return false
	}
	defer conn.Close()

	// この接続を holder に載せ、常駐 stdin reader（cmdAttach）が入力を転送できる
	// ようにする。切断で外す（未接続中の入力は破棄＝次接続から届く）。cycle ごとに
	// stdin reader を作ると前 reader が残ってキーを奪い合う＝敵対的レビューで確認。
	holder.set(conn)
	defer holder.set(nil)

	// 初回 RESIZE（cm-wire magic）で observe サイズを pane 実寸へ。以後 SIGWINCH
	// で再送。RESIZE を送らないと bridge は初回 full frame を出さない（実測仕様）。
	rows, cols := termSize(int(out.Fd()))
	_, _ = conn.Write(resizeMagic(rows, cols))

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// SIGWINCH: 最新サイズを RESIZE 再送（bridge が observe respawn＝新 full frame）。
	go func() {
		for {
			select {
			case <-dctx.Done():
				return
			case <-winch:
				drainSignals(winch)
				r, c := termSize(int(out.Fd()))
				if _, werr := conn.Write(resizeMagic(r, c)); werr != nil {
					return
				}
			}
		}
	}()

	// 表示: conn（remote 画面フレーム）→ stdout（pane PTY）。conn 切断で戻る＝
	// この attachOnce が終了し backoff ループが再接続する。
	go func() { <-dctx.Done(); conn.Close() }()

	return pumpFrames(conn, out, DefaultIdle)
}

// deadlineConn は SetReadDeadline を持つ conn（net.Conn のうち pumpFrames が
// 使う最小 seam。websocket.NetConn の実装を直接 import せず fake conn で
// テストできるようにする）。
type deadlineConn interface {
	io.Reader
	SetReadDeadline(t time.Time) error
}

// pumpFrames は conn（remote 画面フレーム）→ out（pane PTY）を転送し続ける。
// quiescence 読取り監視（internal/bridge.Bridge の quiescence と同じ意味論の
// viewer 側版）: ネットワーク切断（Wi-Fi 切替・VPN 再接続等）が起きると
// conn.Read は TCP の OS 既定タイムアウト（実測 数十分オーダー）までブロック
// し続け、上位の backoff ループへ一切戻らない（実障害で確認済み＝移動で
// ネットワークが切れた後、手動で pane を close するまで自動復旧しなかった）。
// websocket.NetConn は SetReadDeadline に対応する（ブロック中の Read も
// deadline 到達で解ける＝doc.go 保証）ため、フレーム受信ごとに deadline を
// idle 先へ延ばし、無通信 idle 超過で Read がタイムアウトしたら戻る＝
// cmdAttach の backoff ループが再接続を試みる。idle<=0 は監視無効（テスト用）。
// 戻り値 idleClosed は「無通信 quiescence（idle 超過の read deadline）で戻った」印。
// 実データ切断（conn 破断）と区別して cmdAttach の backoff 判断に使う（BUG-3:
// idle 切断を『健全接続の切断』と誤判定して即再 Wake すると source が observe を
// 再 spawn し続ける thrash になる）。idle<=0（監視無効）では常に false。
func pumpFrames(conn deadlineConn, out io.Writer, idle time.Duration) (idleClosed bool) {
	if idle > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(idle))
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return false
			}
			if idle > 0 {
				_ = conn.SetReadDeadline(time.Now().Add(idle))
			}
		}
		if rerr != nil {
			return idle > 0 && isIdleTimeout(rerr)
		}
	}
}

// isIdleTimeout は err が read deadline（quiescence idle 超過）由来かを判定する。
// websocket.NetConn の deadline エラーは os.ErrDeadlineExceeded を満たすか
// net.Error.Timeout()==true になる（実装差を両方受ける）。実データ切断（EOF・
// conn reset 等）は false＝素早い再接続に回す。
func isIdleTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// resizeMagic は cm-wire の RESIZE（0xff 0xff + rows u16BE + cols u16BE）。
// webterm_e2e の resizeFrame・internal/bridge CMWireParser と同一形式。
func resizeMagic(rows, cols int) []byte {
	if rows < 0 {
		rows = 0
	}
	if cols < 0 {
		cols = 0
	}
	return []byte{
		0xff, 0xff,
		byte(rows >> 8), byte(rows),
		byte(cols >> 8), byte(cols),
	}
}
