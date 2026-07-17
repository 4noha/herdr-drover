// bridge.go は relay の source 側データ線（cm ワイヤ）と herdr pane を
// 突き合わせる本体。
//
//	conn（relay WSS / テストでは fake conn）
//	  ← observe subprocess の terminal.frame（ANSI bytes をそのまま転送）
//	  → CMWireParser: RESIZE=observe respawn／SCROLL=v1 無視／
//	    IMAGE=parse-and-drop／その他=pane への入力（input.go の実測決定木）
//
// stream 源が「herdr 同梱 CLI の observe サブプロセス」なのは DESIGN の
// 確定事項（同一バイナリの client を使う＝PROTOCOL_VERSION 完全一致問題が
// 構造的に消滅・AGPL 衛生は socket/CLI 越しのデータ交換のみ）。
//
// フレームは実測（隔離 herdr 0.7.4）どおり全て DECSET 2026 括り＋?25l の
// server-rendered ANSI なので、そのまま流せばブラウザ側 sync.js／2026
// honor 端末で atomic に描画される（bridge は画面解釈をしない＝cm 不変条件
// 「忠実な VT エミュレート＋viewport 再描画だけ」の herdr 版）。
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

const (
	// DefaultMaxRows/Cols は observe サイズの clamp 既定値（knob）。
	// 根拠（隔離 herdr 0.7.4 実測）: observe の full frame は空白セルも
	// 明示塗りで約 20B/セル×cols×rows が内容と無関係に発生し、160×500 は
	// raw 1.6MB/base64 2.1MB。しかも pane 実 PTY より大きい行は上寄せ空白
	// padding（scrollback 非含有）＝帯域の純損失。一方 observe が PTY より
	// 小さいと左上クリップで下部プロンプトが消えるため、実測 PTY
	// （23-50 行級）を必ず上回りつつブラウザ実寸（〜100 行級）を収める
	// 120 行を既定とする。cm Web の 500 行級の巨大 RESIZE はここで潰す。
	DefaultMaxRows = 120
	DefaultMaxCols = 320

	// minRows/minCols は下限 clamp。0 や 1 桁の異常値で observe を起動
	// してもクリップ画面しか得られず、respawn ストームの種になるだけ。
	minRows = 2
	minCols = 10

	// observe respawn の backoff（cm state.watch keepSubscribed と同値:
	// 200ms→×2→上限 5s。フレーム受信実績があれば初期値へ戻す）。
	backoffMin = 200 * time.Millisecond
	backoffMax = 5 * time.Second

	// DefaultIdle は quiescence 自切断の既定（cm 本番の IdleClose=30s、
	// cm cmd/claude-master/main.go:717 と同値）。無通信 30s でデータ線を
	// 自分から閉じ、次の wake まで解放する＝near-$0 設計の要。
	DefaultIdle = 30 * time.Second

	// writeTimeout は conn への frame 書込の上限（cm renderClientLocked の
	// 2s write deadline と同値・同クラスの対処）。viewer が読まない（TCP
	// backpressure）と writer が Conn.Write で無期限ブロックし、RESIZE 時の
	// proc.stop() が <-p.done で凍結する実再現バグの根治。timeout は
	// connErr（データ線死）として扱う＝遅すぎる viewer は切断（cm と同じ）。
	writeTimeout = 2 * time.Second

	// stopGrace は proc.stop() が writer 終了を待つ上限（writeTimeout＋余裕）。
	// SetWriteDeadline 非対応 conn（素の io.ReadWriter）でも凍結しないための
	// 保険で、超過時は conn を強制 close して pending Write を解く。
	stopGrace = writeTimeout + 3*time.Second

	// stderrTailCap は observe/control サブプロセスの stderr 末尾保持量。
	// spawn 即死の真因（socket 不達等）は stderr にしか出ない（実測）ため
	// io.Discard で捨てない（cm tmux.go new-window silent fail の教訓）。
	stderrTailCap = 4 * 1024
)

// winSize は viewer 由来の表示サイズ（clamp 済み）。
type winSize struct{ rows, cols int }

// Bridge は 1 セッション（1 pane × 1 relay conn）のブリッジ。
// New（または struct リテラル）で作り knob を上書きしてから Run を 1 回
// 呼ぶ使い捨て（再利用しない）。
type Bridge struct {
	// Sid は herdr の pane_id（例 "w1:p1"）＝observe/入力の対象。
	// relay 側の sid（派生 sid <pane_id>#inj を含む）との対応付けは
	// 呼び出し側（wake ハンドラ）の責務。
	Sid string
	// Conn は relay の source 側データ線（relayclient.Dial の戻り値等）。
	// テストでは net.Pipe 等の fake conn。io.Closer も実装していれば
	// Run 終了時と quiescence 自切断時に close する（cm BridgeSourceIdle
	// の defer ws.Close() と同じ意味論＝conn reader goroutine の解放も兼ねる）。
	Conn io.ReadWriter
	// Herdr は ndjson API client（SocketPath を observe/control サブ
	// プロセスの HERDR_SOCKET_PATH に引き継ぐ）。
	Herdr *herdrapi.Client

	// HerdrBin は herdr CLI のバイナリ（既定 "herdr"＝PATH 解決）。
	HerdrBin string
	// MaxRows/MaxCols は observe サイズ clamp の knob（0 なら既定値）。
	MaxRows, MaxCols int
	// Idle は quiescence 自切断（両方向とも Idle の間 1 バイトも流れなければ
	// conn を閉じて Run が nil で戻る）。0 なら DefaultIdle(30s)、負なら
	// 無効（テスト用）。cm relay.idlePump の「どちら向きでもバイトが流れる
	// たび bump・ticker(idle/2) で判定」と同じ意味論。
	Idle time.Duration
	// Logf は診断ログ（nil なら log.Printf）。
	Logf func(format string, args ...any)

	// last は最終通信時刻（UnixNano）。conn の read/write 両方向で bump。
	last atomic.Int64
	// idleClosed は quiescence 自切断を自分で行った印（conn close 起因の
	// read エラーを「正常切断」へ写像するため）。
	idleClosed atomic.Bool

	// cur は現行 observe サブプロセス（テストの respawn／リーク検査用に
	// mu 越しで観測する）。
	mu  sync.Mutex
	cur *observeProc
}

// New は必須 3 フィールドだけ埋めるコンビニエンス（webterm 配線は struct
// リテラル直書き＝どちらでもよい）。knob（HerdrBin/MaxRows/MaxCols/Idle/
// Logf）は戻り値に設定してから Run を呼ぶ。
func New(sid string, conn io.ReadWriter, herdr *herdrapi.Client) *Bridge {
	return &Bridge{Sid: sid, Conn: conn, Herdr: herdr}
}

func (b *Bridge) logf(format string, args ...any) {
	if b.Logf != nil {
		b.Logf(format, args...)
		return
	}
	log.Printf("bridge[%s]: "+format, append([]any{b.Sid}, args...)...)
}

func (b *Bridge) herdrBin() string {
	if b.HerdrBin != "" {
		return b.HerdrBin
	}
	return "herdr"
}

// procEnv は observe/control サブプロセス用の環境。HERDR_SOCKET_PATH を
// API client と同じ socket に固定する（隔離テスト・複数サーバ環境で
// 「別サーバの pane を観る」事故を防ぐ）。os/exec は重複キーは後勝ち。
func (b *Bridge) procEnv() []string {
	return append(os.Environ(), "HERDR_SOCKET_PATH="+b.Herdr.SocketPath)
}

// clamp は viewer の RESIZE 要求を observe サイズへ丸める（knob 既定値の
// 根拠は DefaultMaxRows のコメント参照）。丸めが起きた時は必ずログに残す:
// cm Web の term.js は接続時に 500 行級×160 の固定 RESIZE を送るため既定
// clamp を必ず踏む＝Phase 2 実ブラウザゲートが表示品質を判定する際、
// 「要求と実サイズの乖離」が一次情報として要る（沈黙丸めは誤診の温床）。
func (b *Bridge) clamp(rows, cols int) winSize {
	maxR, maxC := b.MaxRows, b.MaxCols
	if maxR <= 0 {
		maxR = DefaultMaxRows
	}
	if maxC <= 0 {
		maxC = DefaultMaxCols
	}
	reqR, reqC := rows, cols
	if rows < minRows {
		rows = minRows
	}
	if cols < minCols {
		cols = minCols
	}
	if rows > maxR {
		rows = maxR
	}
	if cols > maxC {
		cols = maxC
	}
	if rows != reqR || cols != reqC {
		b.logf("RESIZE clamp: 要求 %dx%d → 実 %dx%d (knob MaxRows/MaxCols)", reqC, reqR, cols, rows)
	}
	return winSize{rows: rows, cols: cols}
}

// Run はブリッジ本体（webterm 配線の契約メソッド）。初回 RESIZE を待って
// observe を spawn し、
//   - RESIZE 再受信 → observe respawn（新サイズ・新 full frame）
//   - observe 死亡 → backoff respawn（フレーム実績があれば backoff リセット）
//   - 無通信 Idle（既定 30s）→ quiescence 自切断（nil で戻る＝次 wake 待ちへ）
//   - conn 切断／ctx cancel → observe を確実に kill して復帰
//
// で回る。戻り値 nil は正常切断（quiescence・viewer 切断）。サブプロセスを
// 残して戻ることはない（プロセスリーク禁止）。
func (b *Bridge) Run(ctx context.Context) error {
	if b.Sid == "" || b.Conn == nil || b.Herdr == nil {
		return errors.New("bridge: Sid/Conn/Herdr は必須")
	}
	// cm BridgeSourceIdle の defer ws.Close() 相当。conn を閉じることで
	// readConn goroutine の Read も確実に解け、goroutine リークしない。
	defer b.closeConn()

	// quiescence 監視（cm relay.idlePump と同じ「両方向 bump＋idle/2 tick」）。
	// 発火は conn close で表現する＝readConn の Read が解けて connDone 経由で
	// 全経路が畳まれる（発火専用チャネルを select に増やすより単純で、
	// 「close された conn へは書けない」も自動で成立する）。
	b.last.Store(time.Now().UnixNano())
	idle := b.Idle
	if idle == 0 {
		idle = DefaultIdle
	}
	watchStop := make(chan struct{})
	defer close(watchStop)
	if idle > 0 {
		go func() {
			t := time.NewTicker(idle / 2)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if time.Since(time.Unix(0, b.last.Load())) >= idle {
						b.logf("quiescence: %s 無通信＝データ線を自切断", idle)
						b.idleClosed.Store(true)
						b.closeConn()
						return
					}
				case <-watchStop:
					return
				}
			}
		}()
	}

	resizeCh := make(chan winSize, 1)
	connDone := make(chan error, 1)
	go b.readConn(resizeCh, connDone)

	// 初回 RESIZE 待ち（cm viewer は接続直後に必ず端末実寸の RESIZE を
	// 送る＝client.go/term.js 実コードの抽出。来ないまま conn が死んだら
	// そのまま終了）。
	var size winSize
	select {
	case size = <-resizeCh:
	case err := <-connDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	backoff := backoffMin
	for {
		// quiescence 自切断後は respawn しない（閉じた conn へ書くだけの
		// 無駄 spawn を実再現で観測済み）。readConn の Read が conn close で
		// 解けて connDone が来るのを待って畳む。
		if b.idleClosed.Load() {
			select {
			case err := <-connDone:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		proc, err := b.spawnObserve(size)
		if err != nil {
			// spawn 自体の失敗（herdr バイナリ不在等）も backoff リトライ
			//（恒久エラーでも conn の idle 自切断が上限を与える）。
			b.logf("observe spawn 失敗: %v（%s 後に再試行）", err, backoff)
			select {
			case <-time.After(backoff):
				backoff = nextBackoff(backoff)
				continue
			case size = <-resizeCh:
				continue
			case err := <-connDone:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		b.setProc(proc)

		select {
		case newSize := <-resizeCh:
			// RESIZE → respawn。旧 observe の writer が完全に止まってから
			// 新 observe を起動する（旧新の frame が conn 上で交錯すると
			// 断片スプライス画面になる＝cm relay takeover 修正と同じ規律）。
			proc.stop()
			if proc.connErr != nil {
				// stop 中に writer が書込失敗（write deadline 超過含む）＝
				// データ線死。respawn しても書き損ね続けるだけなので終える
				// （proc.done 分岐と同じ判定。stalled viewer の実再現バグで
				// ここを欠くと閉じた線へ respawn し続ける）。
				b.setProc(nil)
				if b.idleClosed.Load() {
					return nil
				}
				return proc.connErr
			}
			size = newSize
			backoff = backoffMin
			continue
		case <-proc.done:
			proc.stop() // 書き手は既に終了・Wait で回収のみ
			if proc.connErr != nil {
				// conn へ書けない＝データ線が死んでいる。respawn しても
				// 無限に書き損ね続けるだけなので bridge を終える。
				// 自分の quiescence 自切断に起因する書込失敗は正常扱い。
				b.setProc(nil)
				if b.idleClosed.Load() {
					return nil
				}
				return proc.connErr
			}
			if proc.gotFrame.Load() {
				backoff = backoffMin // 実績あり＝一過性の死とみなす
			}
			b.logf("observe 終了（%s 後に respawn）", backoff)
			select {
			case <-time.After(backoff):
				backoff = nextBackoff(backoff)
			case size = <-resizeCh:
				backoff = backoffMin
			case err := <-connDone:
				b.setProc(nil)
				return err
			case <-ctx.Done():
				b.setProc(nil)
				return ctx.Err()
			}
			continue
		case err := <-connDone:
			proc.stop()
			b.setProc(nil)
			return err
		case <-ctx.Done():
			proc.stop()
			b.setProc(nil)
			return ctx.Err()
		}
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// closeConn は Conn が io.Closer なら閉じる（多重 close 安全は下層に依存:
// net.Conn/net.Pipe/websocket.NetConn はいずれも多重 close 可）。
func (b *Bridge) closeConn() {
	if c, ok := b.Conn.(io.Closer); ok {
		_ = c.Close()
	}
}

// bump は quiescence 時計を今へ進める（conn の read/write 両方向から呼ぶ）。
func (b *Bridge) bump() { b.last.Store(time.Now().UnixNano()) }

func (b *Bridge) setProc(p *observeProc) {
	b.mu.Lock()
	b.cur = p
	b.mu.Unlock()
}

// observePID は現行 observe サブプロセスの PID（無ければ 0）。
// respawn／リーク検査テスト用（pgrep のようなシステム全域検索は他プロセス
// と衝突し得るため、正確な自プロセス参照を出す）。
func (b *Bridge) observePID() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil || b.cur.cmd.Process == nil {
		return 0
	}
	return b.cur.cmd.Process.Pid
}

// observeArgs は現行 observe サブプロセスの argv（respawn サイズ検証用）。
func (b *Bridge) observeArgs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return nil
	}
	return append([]string(nil), b.cur.cmd.Args...)
}

// readConn は conn の唯一の読み手。cm ワイヤを剥がして
// RESIZE→resizeCh（最新優先）／入力→sendInput（順序保存のためこの
// goroutine で逐次実行）／SCROLL・IMAGE→消費のみ、を行う。
func (b *Bridge) readConn(resizeCh chan winSize, connDone chan<- error) {
	var p CMWireParser
	// carry は read 境界で割れた UTF-8 rune の先頭断片の繰越し（cmwire の
	// 末尾孤立 0xff と同じ規律）。断片のまま sendInput へ渡すと utf8.Valid が
	// 両断片とも false → control fallback の attach 副作用（実 PTY resize）
	// を踏む（実再現済み）。キー入力（xterm.js onData）は UTF-8 完結だが、
	// relay の 32KB chunk 中継が rune を割り得る＝残りは直後に届くので
	// 繰越しの遅延は実質ゼロ。
	var carry []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := b.Conn.Read(buf)
		if n > 0 {
			b.bump() // viewer→pane 方向の通信あり＝quiescence リセット
			for _, ev := range p.Feed(buf[:n]) {
				switch ev.Kind {
				case EvResize:
					pushLatest(resizeCh, b.clamp(ev.Rows, ev.Cols))
				case EvInput:
					data := ev.Input
					if len(carry) > 0 {
						data = append(carry, data...)
					}
					data, carry = splitIncompleteRune(data)
					if len(data) == 0 {
						continue // 断片のみ＝次 read で再結合
					}
					if serr := b.sendInput(data); serr != nil {
						// 入力欠落は無言だと「壊れた窓」誤診の温床
						//（cm nav-mode 教訓）＝必ずログに残す。
						b.logf("入力送出失敗 (%dB): %v", len(data), serr)
					}
				case EvScroll:
					// v1 は無視（DESIGN: herdr terminal.scroll は共有
					// runtime 状態＝ローカル表示にも影響するため非対応）。
				case EvImage:
					// parse-and-drop（パーサが payload 消費済み。漏れると
					// 画像バイトが打鍵として pane に流れる）。
					b.logf("IMAGE フレームを破棄 (%dB ext=%d)：v1 非対応", ev.ImageLen, ev.ImageExt)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || b.idleClosed.Load() {
				// io.EOF=viewer/relay 側の正常 close。idleClosed=自分の
				// quiescence 自切断（close 起因の read エラーは異常ではない）。
				err = nil
			}
			connDone <- err
			return
		}
	}
}

// pushLatest は容量 1 の resizeCh へ「最新値優先」で入れる（未消費の古い
// 値は捨てる）。RESIZE 連打で respawn を積み上げないため。書き手は
// readConn の 1 goroutine のみ＝livelock しない。
func pushLatest(ch chan winSize, v winSize) {
	for {
		select {
		case ch <- v:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}

// tailBuf は末尾 stderrTailCap バイトだけ保持する io.Writer（exec の pipe
// pump goroutine が書き、プロセス回収後に読む。mutex は -race 安全のため）。
type tailBuf struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > stderrTailCap {
		t.b = append(t.b[:0:0], t.b[len(t.b)-stderrTailCap:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// observeProc は observe サブプロセス 1 世代。
type observeProc struct {
	cmd  *exec.Cmd
	done chan struct{} // frame writer goroutine の終了（stdout EOF / conn 書込不能）
	// connErr は conn への書込失敗（done close 後にのみ読む）。
	connErr error
	// gotFrame は 1 フレームでも conn へ流せた実績（backoff リセット判定）。
	gotFrame atomic.Bool
	stopOnce sync.Once
	// stderr は末尾リングバッファ（真因診断用。cmd.Wait 後に読む）。
	stderr *tailBuf
	// logf は診断ログ（stop 時の stderr/exit 報告に使う）。
	logf func(format string, args ...any)
	// onStuck は writer が stopGrace 内に終わらない時の脱出弁
	//（Bridge.closeConn＝pending Write を強制的に解く）。
	onStuck func()
}

// stop は kill → writer 終了待ち → Wait（回収）。既に死んでいても安全で、
// どの経路からも「サブプロセスとその読み手が完全に居なくなった」ことを
// 保証してから戻る（respawn 時の frame 交錯防止・リーク防止）。
//
// writer 終了待ちは stopGrace で上限を付ける: viewer が読まないと writer が
// Conn.Write でブロックし stop が凍結する実再現バグがあった。通常は per-frame
// write deadline（writeTimeout）が先に解くが、SetWriteDeadline 非対応 conn の
// 保険として conn 強制 close で Write を解いてから待ち直す。
func (p *observeProc) stop() {
	p.stopOnce.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.done:
		case <-time.After(stopGrace):
			p.logf("observe writer が %s 内に止まらない＝conn を強制 close して解く", stopGrace)
			if p.onStuck != nil {
				p.onStuck()
			}
			<-p.done
		}
		werr := p.cmd.Wait()
		// spawn 即死の真因（socket 不達・argv 非互換・version 差等）は stderr
		// にしか出ない（実測）。沈黙させると respawn ストーム時に一次情報が
		// 消える（cm tmux.go new-window silent fail の教訓）＝必ず残す。
		if s := p.stderr.String(); s != "" {
			p.logf("observe stderr (exit=%v): %q", werr, s)
		}
	})
}

// observeEnvelope は observe stdout の ndjson 1 行（実測スキーマ）。
// Bytes は base64 文字列だが encoding/json が []byte へ直接復号する。
type observeEnvelope struct {
	Type   string          `json:"type"` // "terminal.frame" | "terminal.closed"
	Seq    int64           `json:"seq"`
	Full   bool            `json:"full"`
	Width  int             `json:"width"`
	Height int             `json:"height"`
	Bytes  []byte          `json:"bytes"`
	Reason json.RawMessage `json:"reason"`
}

// spawnObserve は `herdr terminal session observe <pane_id> --cols C --rows R`
// を起動し、stdout の ndjson envelope を decode して ANSI bytes を conn へ
// 流す writer goroutine を張る。
func (b *Bridge) spawnObserve(size winSize) (*observeProc, error) {
	cmd := exec.Command(b.herdrBin(),
		"terminal", "session", "observe", b.Sid,
		"--cols", fmt.Sprintf("%d", size.cols),
		"--rows", fmt.Sprintf("%d", size.rows))
	cmd.Env = b.procEnv()
	st := &tailBuf{}
	cmd.Stderr = st
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &observeProc{cmd: cmd, done: make(chan struct{}),
		stderr: st, logf: b.logf, onStuck: b.closeConn}
	b.logf("observe spawn pid=%d size=%dx%d", cmd.Process.Pid, size.cols, size.rows)

	// conn が deadline 対応（websocket.NetConn／net.Pipe とも net.Conn 実装）
	// なら per-frame の write deadline を張る。非対応 conn は stop() の
	// stopGrace 保険に委ねる。
	wd, _ := b.Conn.(interface{ SetWriteDeadline(time.Time) error })

	go func() {
		defer close(p.done)
		// フレームは 160×500 級で base64 2.1MB に達する（実測）ため、
		// bufio.Scanner（既定 64KB 上限）ではなく ReadBytes を使う。
		r := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				var env observeEnvelope
				if jerr := json.Unmarshal(line, &env); jerr != nil {
					b.logf("observe 行 decode 失敗: %v (%.120q)", jerr, line)
				} else {
					switch env.Type {
					case "terminal.frame":
						if len(env.Bytes) > 0 {
							// 書込に上限（writeTimeout）。stalled viewer で
							// ここが無期限ブロックすると RESIZE 経路の
							// proc.stop() が凍結する（実再現済み）。timeout
							// は connErr＝データ線死として bridge を終える
							// （cm renderClientLocked 2s deadline と同じ）。
							if wd != nil {
								_ = wd.SetWriteDeadline(time.Now().Add(writeTimeout))
							}
							if _, werr := b.Conn.Write(env.Bytes); werr != nil {
								p.connErr = fmt.Errorf("conn 書込失敗: %w", werr)
								return
							}
							b.bump() // pane→viewer 方向の通信あり＝quiescence リセット
							p.gotFrame.Store(true)
						}
					case "terminal.closed":
						// pane 消滅・server 停止等。この後 stdout EOF →
						// respawn 判断は Run 側（backoff）。
						b.logf("observe closed: %s", env.Reason)
					default:
						// 未知 type は無視（前方互換。hidden CLI のため
						// version 差の吸収余地を残す）。
					}
				}
			}
			if rerr != nil {
				return // EOF/子プロセス死
			}
		}
	}()
	return p, nil
}
