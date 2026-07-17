package main

// claude シム — cm（claude-master）`start` の C 案を herdr 世界で再現する。
//
// `herdr-drover claude [args...]` を alias claude にすると:
//   - herdr server が居なければ detached 自動起動（ping まで最大 10s 待ち）
//   - 引数なし: cwd 完全一致の既存 claude セッションへ attach（exact-match のみ
//     ＝ヒューリスティック分類禁止の鉄則）。複数は picker、無ければ新規
//   - 引数あり（TTY）: 常に新規 pane（cm 規律「user 明示指定は尊重」）
//   - 引数あり（非 TTY）: herdr を経由せず素の claude へプロセス置換
//     ＝`echo prompt | claude -p …` の pipe stdin/stdout 契約を透過する
//     （pane を作り捨てて pane_id だけ返すと自動化が silent に壊れる）
//   - 引数なし×非 TTY（CI/pipe）: attach せず pane_id/terminal_id を表示して
//     終了＝自動化スクリプトから呼ばれても dup を作らない backstop
//
// attach は syscall.Exec で `herdr terminal attach <terminal_id>` へプロセス
// 置換する（env 継承＝HERDR_SOCKET_PATH 透過）。exec は seam（execAttach）に
// してテストから「exec に渡る引数列」を機械検証する（herdr terminal attach は
// TTY 必須で pipe だと panic exit=101 の実測があるため、実 TTY e2e は Gate
// フェーズの pty ハーネス側で行う＝隔離テストレシピの規律）。

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// serverStartTimeout は自動起動した herdr server の ping 応答待ち上限。
// 実測の起動時間は ~1s（client_test.go の harness と同じ根拠）＝10s は十分。
const serverStartTimeout = 10 * time.Second

// テスト用 seam。exec はプロセス置換＝テストから直接は検証不能なので、
// 「渡る引数列」を fake で捕まえる（合成の別経路を作らないための最小 seam）。
var (
	// execAttach は syscall.Exec の seam。成功時は戻らない。attach への置換と
	// 非 TTY×引数ありの素 claude 透過（cmdClaude 冒頭）の両方が使う共通 seam。
	execAttach = func(argv0 string, argv []string, env []string) error {
		return syscall.Exec(argv0, argv, env)
	}
	// stdinIsTTY は stdin の TTY 判定。⚠旧 ModeCharDevice 判定は /dev/null
	//（char device）でも真になる実バグだった: os/exec の Stdin=nil は
	// /dev/null＝cron/launchd/nohup の最頻自動化経路で attach へ進み、
	// herdr terminal attach が ratatui init panic（exit=101）していた。
	// isatty(3) と同じ termios 取得（tcgetattr 相当 ioctl）の成否なら
	// /dev/null=false・pipe=false・pty slave=true（実測。ioctl 番号は
	// claudeshim_tty_{darwin,linux}.go の OS-split 定数＝依存追加なしで
	// golang.org/x/term 不使用の規律を維持）。
	stdinIsTTY = func() bool {
		// バッファは termios 実サイズ（darwin 72B / linux 36B）より十分大きく。
		var termios [128]byte
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL, os.Stdin.Fd(), uintptr(ioctlReadTermios),
			uintptr(unsafe.Pointer(&termios[0])))
		return errno == 0
	}
)

// lastSpawnedServerPID は直近に自動起動した herdr server の PID（0=未 spawn）。
// テストの停止 backstop（「自分が spawn した PID のみ kill」の恒久規律＝裸の
// pkill herdr で他者サーバを殺した実インシデントの再発防止）から参照する。
var lastSpawnedServerPID int

// cmdClaude は claude シム本体。args は実 claude へそのまま渡す引数列。
func cmdClaude(args []string, stdout, stderr io.Writer) error {
	claudeAbs, err := lookupClaude()
	if err != nil {
		return err
	}

	// 非 TTY×引数あり: herdr を経由せず素の claude へプロセス置換する。
	// pane 経由にすると `echo prompt | claude -p …` の stdin が pane に届かず
	// stdout も返らないまま exit 0＝自動化が silent に壊れ、呼ばれるたび
	// pane が作り捨てられる（旧コードの実バグ・回帰テストで FAIL 確認済み）。
	// server 自動起動より前に判定＝cron 等から herdr server を勝手に建てない。
	if len(args) > 0 && !stdinIsTTY() {
		return execAttach(claudeAbs, append([]string{claudeAbs}, args...), os.Environ())
	}

	api := herdrapi.New("")
	if err := ensureHerdrServer(api, stderr); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd 取得: %w", err)
	}
	// 物理パスへ正規化する。os.Getwd は PWD env が同 inode を指すと論理パス
	//（symlink 経路: /tmp→/private/tmp 等）を返すが、herdr は agent cwd を
	// 物理パスで登録する（実測一次事実）＝生値の文字列比較だと毎回不一致で
	// dup を量産する（cm start の既知 quirk の同根・回帰テストで FAIL 確認済み。
	// AgentStart へ渡す cwd も同じ値＝候補一致と登録が常に揃う）。
	if p, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = p
	}

	// 引数なしのときだけ既存セッション探索。引数ありは常に新規
	//（cm 規律: user の明示指定を cwd 一致 attach で横取りしない）。
	// ⚠既知の race（許容・実測に基づく判断）: agent.start 直後の agent.list は
	// 反映が遅延し得るため、同 cwd で同時に 2 本走ると双方 0 候補→双方新規に
	// なり得る。同 cwd 複数 pane は picker が扱う正規状態（引数あり新規も同じ
	// 状態を作る）＝silent 破壊でなく可視・herdr に per-cwd の原子的
	// create-if-absent は無い（agent 名一意制約は global）ため、名前へ cwd を
	// 符号化する等の回避は「引数あり=常に新規」の設計と両立しない。
	if len(args) == 0 {
		agents, err := api.AgentList()
		if err != nil {
			return fmt.Errorf("agent.list: %w", err)
		}
		cands := claudeCandidates(agents, cwd)
		switch {
		case len(cands) == 1:
			fmt.Fprintf(stdout, "cwd 一致の既存 claude セッションへ接続します\n")
			return attachOrReport(cands[0], stdout)
		case len(cands) > 1:
			if !stdinIsTTY() {
				// 非 TTY は picker を出せない。勝手に attach も新規もせず
				// 先頭情報だけ報告して終了（dup を作らない backstop）。
				c := cands[0]
				fmt.Fprintf(stdout, "cwd 一致の claude セッションが %d 件あります（非 TTY のため attach しません）\n", len(cands))
				fmt.Fprintf(stdout, "pane_id=%s terminal_id=%s cwd=%s\n", c.PaneID, c.TerminalID, c.Cwd)
				return nil
			}
			idx, startNew, err := runPicker(cands, stdout)
			if err != nil {
				return err
			}
			if !startNew {
				return attachOrReport(cands[idx], stdout)
			}
			// startNew: 下の新規経路へ落ちる
		}
	}

	// 新規: 実 claude の絶対パスを argv[0] に渡す。exec.LookPath は shell
	// alias の影響を受けず、herdr server 側の PATH 差異にも免疫になる。
	ag, err := startClaudeAgent(api, append([]string{claudeAbs}, args...), cwd)
	if err != nil {
		return fmt.Errorf("agent.start: %w", err)
	}
	fmt.Fprintf(stdout, "claude セッションを新規起動しました\n")
	return attachOrReport(*ag, stdout)
}

// maxClaudeAgents は自動採番の上限（1 サーバに同時に持つ claude agent 数の
// 実用上限。超えたら明示エラー＝黙って失敗しない）。
const maxClaudeAgents = 64

// startClaudeAgent は agent 名の一意制約に対応した agent.start。
// 実測（herdr 0.7.4）: 同名 agent が居ると agent.start は
// {"code":"agent_name_taken"} で拒否される（CLI help も「unique agent
// names」と明記）＝固定名 "claude" では 2 本目が起動できない。
// 対応は cm M8f marker と同じ「encode/decode の構造往復」: encode は
// claude → claude-2 → claude-3 … を順に試し、decode（isClaudeAgentName）が
// その形だけを exact に受ける。ヒューリスティック分類ではない。
func startClaudeAgent(api *herdrapi.Client, argv []string, cwd string) (*herdrapi.AgentInfo, error) {
	for i := 1; i <= maxClaudeAgents; i++ {
		name := "claude"
		if i > 1 {
			name = fmt.Sprintf("claude-%d", i)
		}
		ag, err := api.AgentStart(name, argv, &herdrapi.AgentStartOptions{Cwd: cwd})
		if err == nil {
			return ag, nil
		}
		var apiErr *herdrapi.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "agent_name_taken" {
			continue // 実測コードの exact-match でのみ次の採番へ
		}
		return nil, err
	}
	return nil, fmt.Errorf("claude agent 名が %d 個まで全て使用中（herdr の agent 名は一意制約）", maxClaudeAgents)
}

// isClaudeAgentName は decode 側: encode（startClaudeAgent）が実際に生成する
// 形と厳密往復する。encode は i=1→"claude"・i>=2→fmt.Sprintf("claude-%d", i)
// しか作らない＝decode も "claude" か "claude-N"（N>=2・%d は先頭ゼロを
// 生成しない）のみ真。"claude-0"/"claude-1"/"claude-02" を受けていた旧実装は
// decode が encode の真部分集合でなく「構造往復」の主張と不一致だった
// （レビュー指摘・回帰テストで FAIL 確認済み）。
func isClaudeAgentName(name string) bool {
	if name == "claude" {
		return true
	}
	rest, ok := strings.CutPrefix(name, "claude-")
	if !ok || rest == "" || rest[0] == '0' {
		return false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 2 {
		return false
	}
	return true
}

// ensureHerdrServer は ping 失敗時に herdr server を detached 自動起動する
// （setsid・stdio devnull＝cm spawnDetachedProxy と同型。シム終了後も server
// が親 terminal の SIGHUP 連鎖に巻き込まれないため）。
//
// ライフサイクル（設計判断・README にも明記）: ここで spawn する server は
// 「ユーザーの herdr server の代理起動」であって drover の管理下に置かない
// ＝drover は止めない・監督しない。停止はユーザーが `herdr server stop`。
// 同時 2 シムの二重 spawn は herdr 自身の単一インスタンス制御に委ねる
// （実測 0.7.4: 既存 server が居る socket で 2 本目は "herdr server is
// already running" exit 1 で即終了・socket 強奪なし＝flock 等の追加排他は
// 不要。双方の ping 待ちループは勝者の server で成立する）。
func ensureHerdrServer(api *herdrapi.Client, stderr io.Writer) error {
	if _, err := api.Ping(); err == nil {
		return nil
	}
	herdrAbs, err := exec.LookPath("herdr")
	if err != nil {
		return fmt.Errorf("herdr が PATH に見つからない（https://herdr.dev から導入）: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("devnull open: %w", err)
	}
	cmd := exec.Command(herdrAbs, "server")
	cmd.Stdin = devnull
	cmd.Stdout = devnull
	cmd.Stderr = devnull
	// env は継承（cmd.Env=nil）＝HERDR_SOCKET_PATH/XDG_CONFIG_HOME 透過。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		devnull.Close()
		return fmt.Errorf("herdr server 自動起動失敗: %w", err)
	}
	devnull.Close()
	lastSpawnedServerPID = cmd.Process.Pid
	fmt.Fprintf(stderr, "herdr server を自動起動しました (pid %d)。応答待ち…\n", cmd.Process.Pid)
	_ = cmd.Process.Release()

	deadline := time.Now().Add(serverStartTimeout)
	for {
		if _, err := api.Ping(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("herdr server が %s 以内に応答しない（socket=%s）", serverStartTimeout, api.SocketPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// lookupClaude は実 claude バイナリを PATH から絶対パスで解決する。
// shell の alias claude='…' は exec.LookPath に影響しない（実環境の claude が
// cm シム alias でも実バイナリ ~/.local/bin/claude が解決される）。
func lookupClaude() (string, error) {
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("claude が PATH に見つからない（alias は exec に効かない＝実バイナリを PATH へ）: %w", err)
	}
	return filepath.Abs(p)
}

// claudeCandidates は cwd 完全一致かつ agent 名が本シムの encode 形
// （isClaudeAgentName）の exact-match のみを候補にする純関数
// （ヒューリスティック分類禁止。cwd の部分一致・子孫一致はしない）。
//
// identity は name 完全一致＋cwd 完全一致（鉄則③の定義そのもの）。DESIGN.md
// の「metadata＋launch_argv 二重符号化」は Phase 3 リモート pane 注入の
// identity 規律（drover 注入 pane を任意 pane 群から見分ける別問題）であり
// 本シムへは適用しない: agent.list の AgentInfo に tokens は載らない（実採取
// types.go）＝pane.list との突合せが要る複雑化に加え、既存シムセッションが
// metadata 無しで孤児化し upgrade 直後に dup を量産する退行になる。
func claudeCandidates(agents []herdrapi.AgentInfo, cwd string) []herdrapi.AgentInfo {
	var out []herdrapi.AgentInfo
	for _, a := range agents {
		if isClaudeAgentName(a.Name) && a.Cwd == cwd {
			out = append(out, a)
		}
	}
	return out
}

// pickerChoice は picker 1 行入力の解釈（純関数＝テーブルテスト対象）。
// cm start と同 UX: Enter=先頭 / "n" か "0"=新規 / 1..n=番号指定。
// 戻り値: (候補 index, 新規フラグ, エラー)。
func pickerChoice(line string, n int) (int, bool, error) {
	s := strings.TrimSpace(line)
	if s == "" {
		return 0, false, nil
	}
	if s == "n" || s == "0" {
		return 0, true, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > n {
		return 0, false, fmt.Errorf("1〜%d の番号か n/0（新規）を入力してください", n)
	}
	return v - 1, false, nil
}

// runPicker は TTY で複数候補時の対話選択。入力解釈は pickerChoice（純関数）に
// 委譲し、この関数は表示と読み取りだけを担う（テスト対象を純関数へ寄せる）。
func runPicker(cands []herdrapi.AgentInfo, stdout io.Writer) (int, bool, error) {
	fmt.Fprintf(stdout, "cwd 一致の claude セッションが %d 件あります:\n", len(cands))
	for i, c := range cands {
		fmt.Fprintf(stdout, "  [%d] %s pane=%s terminal=%s status=%s\n", i+1, c.Name, c.PaneID, c.TerminalID, c.AgentStatus)
	}
	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprintf(stdout, "番号を選択（Enter=1 / n か 0=新規）: ")
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return 0, false, fmt.Errorf("picker 入力読取: %w", err)
		}
		idx, startNew, perr := pickerChoice(line, len(cands))
		if perr != nil {
			fmt.Fprintf(stdout, "%v\n", perr)
			continue
		}
		return idx, startNew, nil
	}
}

// attachOrReport は TTY なら herdr terminal attach へプロセス置換、非 TTY なら
// pane_id/terminal_id を表示して正常終了する（CI/スクリプト安全）。
func attachOrReport(ag herdrapi.AgentInfo, stdout io.Writer) error {
	if !stdinIsTTY() {
		fmt.Fprintf(stdout, "pane_id=%s terminal_id=%s\n", ag.PaneID, ag.TerminalID)
		return nil
	}
	herdrAbs, err := exec.LookPath("herdr")
	if err != nil {
		return fmt.Errorf("herdr が PATH に見つからない: %w", err)
	}
	// detach ヒントは exec 前に出す（置換後はこちらから何も出せない）。
	fmt.Fprintf(stdout, "attach します（detach は Ctrl+B q）\n")
	// env 継承で HERDR_SOCKET_PATH が透過＝シムと同じサーバへ attach する。
	return execAttach(herdrAbs, []string{herdrAbs, "terminal", "attach", ag.TerminalID}, os.Environ())
}
