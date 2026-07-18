//go:build unix

package main

// localview.go — 自動 min ローカルビューア（observe / control 自動切替）。
//
// 単一 pane を「メインアプリ（herdr TUI＝App クライアント）」と「外から起動した
// ローカル端末」の2ビューアで同時に下部まで見せるには、pane grid を**両者の
// 小さい方**に合わせるしかない（単一 PTY は片方にしか厳密一致できない＝大きい
// 側は余白）。herdr 0.7.4 の実挙動（ソース確定）を踏まえ、ローカル端末が grid
// より小さいときだけ最小限ロックする:
//
//   - App は描画のたび、ロックが無ければ pane を自分のレイアウトサイズへ強制
//     resize する（src/ui/panes.rs: `if !direct_attach_resize_locks.contains`）。
//     ＝ロック無しでは pane grid は常に App 追従。
//   - `herdr terminal session observe <pane> --cols C --rows R`（TerminalObserve）
//     はロックを一切張らず、render_terminal_virtual で観測側サイズへ仮想描画
//     （pane 実サイズ不変）。ただし観測 < grid だと grid 上端から観測行数ぶんを
//     描く**上寄せクリップ**で、pane 下部（claude の入力ボックス）が範囲外に
//     なる（scrollback は履歴＝上方向にしか動かず下部に到達不能）。
//   - `herdr terminal session control <pane> --takeover? --cols C --rows R`
//     （隠し CLI・ControlTerminal＝attach と同じロック経路）は pane を明示サイズ
//     C×R へ resize＋direct_attach_resize_locks へ登録し、observe と同一の
//     terminal.frame ストリームを返す。
//
// よって自動 min:
//   - localRows >= gridRows: **observe**（ロック非取得）。App がリサイズ権限を
//     保持＝メイン優先。ローカルが大きければ余白。
//   - localRows <  gridRows: **control** で pane を local 実寸へ縮小＋ロック。
//     ローカルで下部入力まで見える／メイン（＝より大きい）は pane を余白付きで
//     表示（App は locked＝縮んだ grid を上書きしない）。ロックは「ローカルが
//     小さい側＝メインが余白で全部見える」ときだけ張る＝ロックが有害な
//     「メイン < ローカル」では張らない（observe）＝旧 attach 固定の弊害を回避。
//
// grid 行は pane.get の scroll.viewport_rows（非ロック時に真の App サイズ）。桁は
// API 非公開のため control には起動元端末の実桁を渡す（外部が両次元で小さい
// 一般ケースは完全 fit・混在次元のみ幅方向で余白/クリップ＝後述の残課題）。
// SIGWINCH で mode を再評価し respawn（observe 中は gridRows も再取得＝メインを
// 途中でリサイズした場合の threshold 追随）。入力は両モードとも ndjson API の
// pane.send_text（byte-perfect・input.go の実測決定木の primary）で注入する。
//
// ⚠残課題（ユーザー承認済み・「稀な動的リサイズ」）: control ロック中は pane.get
// が自分のロックサイズを返し App の真のサイズを読めない＝ロック後にメインを
// ローカルより小さく縮めるとメイン側が下部クリップし得る（detach で解消）。
// ⚠control fallback（bridge.sendInput の非 UTF-8 経路）は使わず、非 UTF-8 バイト
// （キーボードからは実質発生しない）は破棄する＝入力経路を両モードで単純化。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/4noha/herdr-drover/internal/bridge"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"golang.org/x/sys/unix"
)

// runViewer は attachOrReport の TTY 分岐が使うビューア seam。既定は
// ロックフリー・ローカル observe ビューア。テストは paneID の到達だけを
// 機械検証する（実 TTY raw-mode e2e は localview_test.go の pty ハーネス）。
var runViewer = func(api *herdrapi.Client, paneID string) error {
	herdrBin, err := exec.LookPath("herdr")
	if err != nil {
		return fmt.Errorf("herdr が PATH に見つからない: %w", err)
	}
	return runLocalView(api, paneID, herdrBin, os.Stdin, os.Stdout, os.Stderr)
}

// runLocalView は observe（表示）＋pane.send_text（入力）でローカル端末に
// pane を映して操作させる。in は raw mode にする TTY（キー入力）、out は
// フレーム描画先、msg は入力送出失敗などの診断（稀）。
//
// 戻り値 nil は正常終了（detach または pane 消滅）。サブプロセス（observe）を
// 残して戻ることはない（リーク禁止＝defer gen.stop）。
func runLocalView(api *herdrapi.Client, paneID, herdrBin string, in, out, msg *os.File) error {
	inFD := int(in.Fd())
	outFD := int(out.Fd())

	old, err := enterRaw(inFD)
	if err != nil {
		return fmt.Errorf("raw mode 失敗（TTY でない?）: %w", err)
	}
	defer restoreRaw(inFD, old)

	// alt-screen へ退避（フレームは全画面 clear+絶対座標描画＝シェルの
	// scrollback を汚さないため）。復帰時は alt-screen を抜けカーソルを可視へ
	// （フレームが ?25l を出しているため復帰しないと以後カーソルが消える）。
	_, _ = io.WriteString(out, "\x1b[?1049h")
	defer func() { _, _ = io.WriteString(out, "\x1b[?25h\x1b[?1049l") }()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	detach := make(chan struct{})
	go readStdinLoop(in, api, paneID, detach, msg)

	// grid（App が決めた pane 実サイズ）を起動時に確定＝自動 min の threshold。
	// 取得不能（0）は pickStreamMode が安全側の observe を返す。
	gridRows := paneGridRows(api, paneID)

	rows, cols := termSize(outFD)
	mode := pickStreamMode(rows, gridRows)
	gen, err := spawnStreamGen(herdrBin, mode, paneID, api.SocketPath, rows, cols, out)
	if err != nil {
		return fmt.Errorf("%s 起動失敗: %w", mode, err)
	}
	defer func() { gen.stop() }()

	for {
		select {
		case <-winch:
			drainSignals(winch)
			// observe 中（非ロック）は pane.get が真の App サイズを返す＝メインを
			// 途中でリサイズした場合に備え threshold を更新（control 中は自分の
			// ロックサイズが返るので更新しない＝stale を承知で維持＝残課題）。
			if mode == streamObserve {
				if g := paneGridRows(api, paneID); g > 0 {
					gridRows = g
				}
			}
			// 旧世代を完全停止（frame 交錯防止・リーク防止・control ならロック
			// 解除）してから新ローカルサイズで mode を再評価し respawn。
			gen.stop()
			rows, cols = termSize(outFD)
			mode = pickStreamMode(rows, gridRows)
			var e error
			gen, e = spawnStreamGen(herdrBin, mode, paneID, api.SocketPath, rows, cols, out)
			if e != nil {
				return fmt.Errorf("%s respawn 失敗: %w", mode, e)
			}
		case <-gen.done:
			// observe が自然終了＝pane 消滅／server 停止。stderr に理由があれば
			// 復帰後（alt-screen を抜けた後）に見せる。
			reason := gen.stderrTail()
			gen.stop() // Wait で回収（done は既に閉じている）
			if reason != "" {
				fmt.Fprintf(msg, "\r\nセッション終了 (%s)\r\n", reason)
			}
			return nil
		case <-detach:
			return nil
		}
	}
}

// enterRaw は fd を cfmakeraw 相当の raw mode にし、復元用の旧 termios を返す。
func enterRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		return nil, err
	}
	raw := makeRawTermios(*old)
	if err := unix.IoctlSetTermios(fd, setTermiosReq, &raw); err != nil {
		return nil, err
	}
	return old, nil
}

func restoreRaw(fd int, old *unix.Termios) {
	if old != nil {
		_ = unix.IoctlSetTermios(fd, setTermiosReq, old)
	}
}

// makeRawTermios は cfmakeraw(3) 相当のフラグ操作。x/sys/unix の定数は各 OS で
// 正しい型（darwin=uint64 / linux=uint32 のフラグ）へ解決されるため OS-split
// 不要（get/set の ioctl 要求番号だけ localview_ioctl_{darwin,linux}.go）。
func makeRawTermios(t unix.Termios) unix.Termios {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return t
}

// termSize は fd の端末サイズ（行・桁）。取得不能や 0 は 24x80 へ fallback
// （observe は 1 桁級の異常サイズだとクリップ画面しか返さないため）。
func termSize(fd int) (rows, cols int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// drainSignals は保留中の同一シグナルを吸う（連続 SIGWINCH で respawn を
// 積み上げない＝1 回の resize burst を 1 respawn に畳む）。
func drainSignals(ch <-chan os.Signal) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// streamObserve / streamControl は spawnStreamGen の mode（herdr の
// `terminal session <mode>` サブコマンド名にそのまま渡る）。
const (
	streamObserve = "observe"
	streamControl = "control"
)

// pickStreamMode は自動 min の決定的分岐（純関数＝テスト対象）。ローカル端末が
// grid（App が決めた pane 行数）より縦に小さいときだけ control（pane を local
// 実寸へ縮小＋ロック＝ローカルで下部入力まで見える／メインは余白）。それ以外は
// observe（ロック非取得＝リサイズ権限を App に残す／ローカルが大きければ余白）。
// gridRows<=0（grid 不明）は安全側の observe＝旧挙動と同じでロックを張らない。
func pickStreamMode(localRows, gridRows int) string {
	if gridRows > 0 && localRows < gridRows {
		return streamControl
	}
	return streamObserve
}

// paneGridRows は pane の現 grid 行数（App が決めた実サイズ）。取得不能は 0。
// ⚠control 接続保持中は pane.get が自分のロックサイズを返す＝呼び出し側は
// observe（非ロック）時のみ threshold 更新に使うこと。
func paneGridRows(api *herdrapi.Client, paneID string) int {
	if p, e := api.PaneGet(paneID); e == nil && p.Scroll.ViewportRows > 0 {
		return p.Scroll.ViewportRows
	}
	return 0
}

// obsGen は observe/control サブプロセス 1 世代。frame writer goroutine が stdout
// の ndjson terminal.frame の ANSI バイトをローカル out へ直書きする。
type obsGen struct {
	cmd    *exec.Cmd
	done   chan struct{} // frame writer goroutine の終了（stdout EOF / 子プロセス死）
	once   sync.Once
	stderr *capBuf
	// stdinKeep は control モードでのみ非 nil。control は stdin EOF で自動 Detach
	// （ロック解除）するため、書込端を開いたまま保持して EOF させない。stop で閉じる。
	stdinKeep *os.File
}

// spawnStreamGen は `herdr terminal session <mode> <pane> --cols C --rows R` を
// 起動する（mode = observe｜control）。observe は read-only・ロック非取得。
// control は pane を C×R へ resize＋ロック（direct_attach_resize_locks）＝自動
// min の縮小側。どちらも terminal.frame を同一 envelope で流す（表示コードは共通）。
func spawnStreamGen(herdrBin, mode, paneID, socketPath string, rows, cols int, out io.Writer) (*obsGen, error) {
	cmd := exec.Command(herdrBin,
		"terminal", "session", mode, paneID,
		"--cols", strconv.Itoa(cols),
		"--rows", strconv.Itoa(rows))
	// HERDR_SOCKET_PATH を API client と同じ socket へ固定（隔離テスト・複数
	// サーバ環境で「別サーバの pane を観る」事故を防ぐ＝bridge.procEnv と同旨）。
	cmd.Env = append(os.Environ(), "HERDR_SOCKET_PATH="+socketPath)
	sb := &capBuf{limit: 4 * 1024}
	cmd.Stderr = sb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stdinKeep *os.File
	if mode == streamControl {
		// control の入力スレッドは stdin.lines() を読み、EOF で ClientMessage::
		// Detach を送る＝ロック解除。書込端(pw)を開いたまま保持して EOF を防ぐ
		// （このパイプへは何も書かない＝入力は別途 pane.send_text で両モード共通）。
		pr, pw, perr := os.Pipe()
		if perr != nil {
			return nil, perr
		}
		cmd.Stdin = pr
		stdinKeep = pw
		defer pr.Close() // 子が Start で dup 済み＝親側は閉じてよい（pw は保持）
	}
	if err := cmd.Start(); err != nil {
		if stdinKeep != nil {
			stdinKeep.Close()
		}
		return nil, err
	}
	g := &obsGen{cmd: cmd, done: make(chan struct{}), stderr: sb, stdinKeep: stdinKeep}
	go func() {
		defer close(g.done)
		// フレームは端末実寸級（〜100 行）でも数十 KB／ローカル socket ゆえ
		// Web の 2MB clamp は不要だが、行の途中で切らないため大きめバッファ。
		r := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				var env struct {
					Type  string `json:"type"`
					Bytes []byte `json:"bytes"` // base64 → []byte（encoding/json）
				}
				if json.Unmarshal(line, &env) == nil &&
					env.Type == "terminal.frame" && len(env.Bytes) > 0 {
					// ローカル TTY への書込は素の Write（remote conn の
					// stalled-viewer backpressure は無い＝write deadline 不要）。
					if _, werr := out.Write(env.Bytes); werr != nil {
						return
					}
				}
				// terminal.closed 等は無視＝この後 stdout EOF で done が閉じ、
				// runLocalView が「セッション終了」として畳む。
			}
			if rerr != nil {
				return
			}
		}
	}()
	return g, nil
}

// stop は kill → 保持 stdin を閉じ → writer 終了待ち → Wait（回収）。sync.Once
// で多重呼び出し安全（winch respawn／自然終了／defer の全経路から呼ばれる）。
func (g *obsGen) stop() {
	g.once.Do(func() {
		if g.cmd.Process != nil {
			_ = g.cmd.Process.Kill()
		}
		if g.stdinKeep != nil {
			// control の保持書込端を閉じる（Kill 済でも fd リークを残さない）。
			_ = g.stdinKeep.Close()
		}
		<-g.done
		_ = g.cmd.Wait()
	})
}

func (g *obsGen) stderrTail() string { return g.stderr.String() }

// readStdinLoop は raw TTY の入力を pane.send_text（ロックフリー）で注入する。
// prefix（Ctrl-B q = detach）を applyPrefix で処理し、rune 割れは bridge の
// SplitIncompleteRune で繰り越す（read 境界が multibyte を割っても壊さない）。
func readStdinLoop(in io.Reader, api *herdrapi.Client, paneID string, detach chan<- struct{}, msg io.Writer) {
	buf := make([]byte, 4096)
	var carry []byte
	prefix := false
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			fwd, det := applyPrefix(&prefix, buf[:n])
			if len(fwd) > 0 {
				data := fwd
				if len(carry) > 0 {
					data = append(carry, fwd...)
				}
				var send []byte
				send, carry = bridge.SplitIncompleteRune(data)
				if len(send) > 0 {
					if utf8.Valid(send) {
						if e := api.PaneSendText(paneID, string(send)); e != nil {
							// cm nav-mode 教訓: 入力欠落を沈黙させない（「壊れた窓」
							// 誤診の温床）。frame は全画面再描画で直に上書きされる。
							fmt.Fprintf(msg, "\r\n[入力送出失敗: %v]\r\n", e)
						}
					} else {
						// 非 UTF-8（キーボードからは実質発生しない）。control
						// fallback はロックを張るためロックフリー保証優先で破棄。
						fmt.Fprintf(msg, "\r\n[非 UTF-8 入力 %dB を破棄（ロックフリー維持）]\r\n", len(send))
					}
				}
			}
			if det {
				close(detach)
				return
			}
		}
		if rerr != nil {
			return // stdin EOF/error＝入力ループ終了（表示は observe 死/detach まで継続）
		}
	}
}

// applyPrefix は Ctrl-B（0x02）プレフィックスを処理し、pane へ転送すべき
// バイト列と detach 要求を返す純関数（テーブルテスト対象）。*prefix は read
// 境界を跨ぐ状態（前 read の末尾が Ctrl-B だった）。
//
//	Ctrl-B q     → detach（以降のバイトは捨てる）
//	Ctrl-B Ctrl-B → リテラル Ctrl-B 1 個を転送
//	Ctrl-B <他>  → Ctrl-B と <他> を両方転送（キーを飲み込まない）
//	末尾 Ctrl-B  → 次 read へ保留（*prefix=true）
func applyPrefix(prefix *bool, data []byte) (forward []byte, detach bool) {
	const ctrlB = 0x02
	var out []byte
	for i := 0; i < len(data); i++ {
		b := data[i]
		if *prefix {
			*prefix = false
			switch b {
			case 'q':
				return out, true
			case ctrlB:
				out = append(out, ctrlB)
			default:
				out = append(out, ctrlB, b)
			}
			continue
		}
		if b == ctrlB {
			*prefix = true
			continue
		}
		out = append(out, b)
	}
	return out, false
}

// capBuf は末尾 limit バイトだけ保持する io.Writer（observe stderr の診断用。
// -race 安全のため mutex）。bridge.tailBuf の localview 版（unexported 同士を
// 跨いで使わない）。
type capBuf struct {
	mu    sync.Mutex
	b     []byte
	limit int
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.b = append(c.b, p...)
	if c.limit > 0 && len(c.b) > c.limit {
		c.b = append(c.b[:0:0], c.b[len(c.b)-c.limit:]...)
	}
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.b)
}
