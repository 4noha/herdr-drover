package main

// claude シム — cm（claude-master）`start` の C 案を herdr 世界で再現する。
//
// `herdr-drover claude [args...]` を alias claude にすると:
//   - herdr server が居なければ detached 自動起動（ping まで最大 10s 待ち）
//   - 引数なし: cwd 完全一致の既存 claude セッションへ attach（exact-match のみ
//     ＝ヒューリスティック分類禁止の鉄則）。複数は picker、無ければ新規
//   - 新規は**常に新しい Tab（claude pane 1 枚）**として生まれる（ルールの
//     有無に関わらず既存 Tab を split しない。旧 agent.start は必ず既存 tab の
//     focused pane を split する実測＝「既存 Tab の表示を邪魔する」ユーザー
//     実観測の根治）。着地先 workspace は ~/.herdr-drover/workspaces.json の
//     ルール（internal/wsmap: exact > 最長 prefix > default）で解決し、
//     ルール無しは現（focused）workspace
//   - 引数あり（TTY）: 常に新規 Tab（cm 規律「user 明示指定は尊重」）
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
	"encoding/json"
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
	"github.com/4noha/herdr-drover/internal/wsmap"
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
			return attachOrReport(api, cands[0], stdout)
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
				return attachOrReport(api, cands[idx], stdout)
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
	return attachOrReport(api, *ag, stdout)
}

// maxClaudeAgents は自動採番の上限（1 サーバに同時に持つ claude agent 数の
// 実用上限。超えたら明示エラー＝黙って失敗しない）。
const maxClaudeAgents = 64

// startClaudeAgent は新規 claude を**新しい Tab（claude pane 1 枚）**として
// 起動する。確定 UX 仕様: claude 起動で既存 Tab を split すると表示を邪魔する
// （実観測）＝ルールの有無に関わらず split しない。
//
// 経路は Probe で確定した唯一の「指定 workspace に新 tab＋argv 直接実行 pane
// 1 枚」正規 API layout.apply（agent.start は常に既存 tab の focused pane を
// split し新 tab を作る経路が存在しない・tab.create は shell pane が余る＝
// どちらも実測で棄却）。手順:
//  1. 着地ルール（~/.herdr-drover/workspaces.json）を読む。壊れていたら
//     **tab を作る前に** loud に停止（silent fallback 禁止）
//  2. workspace 解決: ルール一致 → label から wsmap.ResolveWorkspaceID
//     （不在 label は focus 非奪取で自動作成）／ルール無し → 現 workspace
//  3. layout.apply で新 Tab 生成（tab label は cwd 末尾・focus 非奪取）
//  4. agent.rename で agent 名を付与（実測: layout.apply pane への後付け命名
//     可・応答 agent_info に terminal_id 込みの AgentInfo が返る）
//
// agent 名の一意制約（実測 agent_name_taken）への対応は従来と同じ
// 「encode/decode の構造往復」: encode は claude → claude-2 → … を順に試し、
// decode（isClaudeAgentName）がその形だけを exact に受ける。
// ⚠旧 agent.start は名前予約が pane 生成と原子的だったが、本経路は pane 生成
// →命名の 2 段になる。同時 2 シムは双方 Tab を作り得るが、これは従来から
// 許容している可視の race（picker が扱う正規状態）と同型＝silent 破壊はない。
func startClaudeAgent(api *herdrapi.Client, argv []string, cwd string) (*herdrapi.AgentInfo, error) {
	rules, err := wsmap.Load()
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// wsmap.Load が home を解決できた直後なので実質起こらないが、~ ルール
		// を silent に不一致へ落とさないため loud に返す。
		return nil, fmt.Errorf("home 解決: %w", err)
	}

	var wsID string
	if label := rules.Resolve(cwd, home); label != "" {
		wsID, err = wsmap.ResolveWorkspaceID(api, label)
	} else {
		wsID, err = currentWorkspaceID(api)
	}
	if err != nil {
		return nil, err
	}

	paneID, err := applyClaudeTab(api, wsID, filepath.Base(cwd), argv, cwd)
	if err != nil {
		return nil, err
	}
	return nameClaudePane(api, paneID)
}

// currentWorkspaceID は「現 workspace」を決定的に選ぶ:
// focused（実測では常にちょうど 1 つ）→ 万一 focused が無ければ number 最小
// → workspace ゼロ（fresh server）は workspace.create で 1 つ作る（実測:
// 最初の workspace は必然 focused になり、label は herdr が cwd から導出）。
func currentWorkspaceID(api *herdrapi.Client) (string, error) {
	raw, err := api.Call("workspace.list", nil)
	if err != nil {
		return "", fmt.Errorf("workspace.list: %w", err)
	}
	var list struct {
		Workspaces []herdrapi.WorkspaceInfo `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("workspace_list decode: %w", err)
	}
	var best *herdrapi.WorkspaceInfo
	for i := range list.Workspaces {
		ws := &list.Workspaces[i]
		if best == nil ||
			(ws.Focused && !best.Focused) ||
			(ws.Focused == best.Focused && ws.Number < best.Number) {
			best = ws
		}
	}
	if best != nil {
		return best.WorkspaceID, nil
	}
	created, err := api.WorkspaceCreate()
	if err != nil {
		return "", fmt.Errorf("workspace.create: %w", err)
	}
	return created.Workspace.WorkspaceID, nil
}

// applyClaudeTab は layout.apply で「wsID に新 tab＋argv 直接実行 pane 1 枚」
// を生成し root pane_id を返す。focus:false＝既存 Tab の表示を邪魔しない
// （Probe 実測: workspace focus も tab focus も奪わない）。
// 実採取応答: {"type":"layout_apply","layout":{"workspace_id","tab_id",
// "zoomed","focused_pane_id","root":{"type","pane_id","cwd","command"}}}
func applyClaudeTab(api *herdrapi.Client, wsID, tabLabel string, argv []string, cwd string) (string, error) {
	type layoutPane struct {
		Type    string   `json:"type"`
		Cwd     string   `json:"cwd"`
		Command []string `json:"command"`
	}
	raw, err := api.Call("layout.apply", struct {
		WorkspaceID string     `json:"workspace_id"`
		TabLabel    string     `json:"tab_label"`
		Focus       bool       `json:"focus"`
		Root        layoutPane `json:"root"`
	}{wsID, tabLabel, false, layoutPane{Type: "pane", Cwd: cwd, Command: argv}})
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
		return "", fmt.Errorf("layout.apply 応答に root pane_id が無い（wire 変化?）")
	}
	return out.Layout.Root.PaneID, nil
}

// nameClaudePane は生成済み pane へ agent 名を付与する（agent.rename の
// target は pane_id 可＝実測）。応答 {"type":"agent_info","agent":{...}} は
// terminal_id 込みの AgentInfo＝attach にそのまま使える。
func nameClaudePane(api *herdrapi.Client, paneID string) (*herdrapi.AgentInfo, error) {
	for i := 1; i <= maxClaudeAgents; i++ {
		name := "claude"
		if i > 1 {
			name = fmt.Sprintf("claude-%d", i)
		}
		raw, err := api.Call("agent.rename", struct {
			Target string `json:"target"`
			Name   string `json:"name"`
		}{paneID, name})
		if err == nil {
			var out struct {
				Agent herdrapi.AgentInfo `json:"agent"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("agent_info decode: %w", err)
			}
			return &out.Agent, nil
		}
		var apiErr *herdrapi.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "agent_name_taken" {
			continue // 実測コードの exact-match でのみ次の採番へ
		}
		// Tab は生成済みなので pane_id を残す（黙って孤児にしない）。
		return nil, fmt.Errorf("agent.rename %s: %w", paneID, err)
	}
	return nil, fmt.Errorf("claude agent 名が %d 個まで全て使用中（herdr の agent 名は一意制約。pane %s は作成済み）", maxClaudeAgents, paneID)
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

// attachOrReport は TTY なら自動 min ローカルビューア（runViewer→runLocalView）
// で pane を映して操作させ、非 TTY なら pane_id/terminal_id を表示して正常終了
// する（CI/スクリプト安全）。
//
// ⚠旧実装は `herdr terminal attach <terminal_id>` へ exec 置換していたが、
// これは接続モード TerminalAttach で pane を direct_attach_resize_locks に
// 登録し、**メインアプリがその pane をリサイズできなくなる**（起動元端末の
// サイズに固定＝普段使いの herdr で下部入力が切れる。herdr 0.7.4 ソース
// src/ui/panes.rs／server/headless.rs で確定・ユーザー実測で裏取り）。逆に
// 常時 observe だと起動元端末が grid より小さいとき下部がクリップされて見えない
// （実測）。localview は「自動 min」＝起動元端末が grid より小さいときだけ
// control で pane を起動元実寸へ縮小＋ロック（メインは余白）、それ以外は observe
// でリサイズ権限をメインに残す（localview.go 冒頭の根拠）。入力は両モードとも
// pane.send_text。observe/control とも pane_id 対象（terminal_id ではない＝
// bridge と同契約）なので ag.PaneID を渡す。
func attachOrReport(api *herdrapi.Client, ag herdrapi.AgentInfo, stdout io.Writer) error {
	if !stdinIsTTY() {
		fmt.Fprintf(stdout, "pane_id=%s terminal_id=%s\n", ag.PaneID, ag.TerminalID)
		return nil
	}
	// ヒントは raw mode/alt-screen へ入る前に出す（ビューア開始後は復帰まで
	// この画面へは出せない）。
	fmt.Fprintf(stdout, "接続します（ロックフリー・リサイズ権限はメインアプリに残る／detach は Ctrl+B q）\n")
	return runViewer(api, ag.PaneID)
}
