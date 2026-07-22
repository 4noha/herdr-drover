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

	// ⚠ この pane は reconcile が管理する（リモートセッション消滅で pane.close）。
	// attach は **設定/Firestore/relay のどの失敗でも exit しない**（exit すると
	// pane が死に→次周の reconcile が再作成→runaway churn になる。実障害で確認）。
	// ctx cancel（SIGTERM / pane close で herdr が kill）だけで戻る＝生存し続けて
	// backoff 再試行する。
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		attachCycle(ctx, injSid, remotePC, holder, out)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > 5*time.Second {
			backoff = 500 * time.Millisecond // 正常に使えていた接続の切断は素早く復帰
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// attachCycle は 1 サイクル（設定解決→state→grant/wake→dial→pump）。どの段階の
// 失敗でも **exit せず戻る**（cmdAttach の backoff ループが再試行＝pane は生存）。
// 設定は primary クラウド（reconcile が pane env に GCP_PROJECT 等を注入する。
// env が無くても config.json / clouds.json から解決を試みる）。
func attachCycle(ctx context.Context, injSid, remotePC string, holder *connHolder, out *os.File) {
	cfg, err := resolveConfig()
	if err != nil {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s: 設定解決失敗（再試行）: %v\r\n", remotePC, err)
		return
	}
	clouds := cfg.LoadClouds()
	if len(clouds) == 0 {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s: 接続先クラウド未設定（再試行）\r\n", remotePC)
		return
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
		return
	}
	defer st.Close()

	attachOnce(cctx, st, cl.RelayURL, injSid, remotePC, holder, out)
}

// attachOnce は 1 接続の生存（grant→wake→dial→frame/input pump）。conn 切断か
// ctx 終了で戻る。エラーは画面に控えめに出し、上位の backoff ループが再接続する。
func attachOnce(ctx context.Context, st *state.Client, relayURL, injSid, remotePC string, holder *connHolder, out *os.File) {
	// viewer 許可を先に置き、リモート agent を起こす（source を bridge させる）。
	gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
	_ = st.PutRelayGrant(gctx, injSid, "viewer", injGrantTTL)
	_ = st.Wake(gctx, remotePC, injSid)
	gcancel()

	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()
	// ⚠ relayclient.DialViewerFrom へ渡す ctx は dial だけでなく websocket.NetConn
	// の生存期間全体を縛る（coder/websocket の仕様＝ctx cancel で以後の
	// read/write も死ぬ）。dial 局面だけを打ち切るタイムアウト付き ctx を渡すと
	// dial 成功後も期限切れで正常な接続まで切ってしまう回帰になるため、
	// ここは dctx（attachOnce 生存期間そのもの）をそのまま渡す。dial 自体が
	// ネットワーク不通で長時間ブロックする問題は、下の quiescence 読取り
	// タイムアウトとは別に残るが、pane close による手動 kick（今回の実運用対処）
	// で回復できる範囲。
	//
	// source PC を spc で渡す＝relay の KeyFor が slave source PC の時だけ
	// slaveSessionKey(spc,injSid) で viewer を Accept し、slave の #inj source と
	// ペアする（master source PC では spc 無視＝従来と同一 wire）。
	conn, err := relayclient.DialViewerFrom(dctx, relayURL, injSid, remotePC)
	if err != nil {
		fmt.Fprintf(out, "\x1b[2J\x1b[H↗ %s / %s: relay 接続失敗（再試行）: %v\r\n", remotePC, injSid, err)
		return
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

	pumpFrames(conn, out, DefaultIdle)
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
func pumpFrames(conn deadlineConn, out io.Writer, idle time.Duration) {
	if idle > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(idle))
	}
	buf := make([]byte, 32*1024)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return
			}
			if idle > 0 {
				_ = conn.SetReadDeadline(time.Now().Add(idle))
			}
		}
		if rerr != nil {
			return
		}
	}
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
