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

func (h *connHolder) write(p []byte) error {
	h.mu.Lock()
	c := h.c
	h.mu.Unlock()
	if c == nil {
		return nil // 未接続中の入力は破棄（次の接続から届く）
	}
	_, err := c.Write(p)
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
	buf := make([]byte, 32*1024)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return
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
