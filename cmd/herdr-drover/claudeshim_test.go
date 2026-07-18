package main

// claude シムの検証（鉄則: 合成で緑にしない。herdr 不在環境は Skip）。
//
// harness は status_test.go の startHerdrForTest（= client_test.go と同じ実測
// レシピ: 短い /tmp dir・XDG_CONFIG_HOME 隔離・停止は自 socket への server
// stop → 自分が spawn した PID のみ kill。裸の pkill herdr は恒久禁止）。
//
// attach の実 TTY e2e は herdr terminal attach が TTY 必須（pipe だと panic
// exit=101 の実測）のため Gate フェーズの pty ハーネスで行い、ここでは exec
// seam に渡る引数列を機械検証する（隔離テストレシピの規律）。

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// installStubClaude は一時 dir に stub claude（marker echo + sleep の sh
// スクリプト）を置き PATH 先頭へ prepend する。実 claude を起動すると隔離
// 不能な副作用（~/.claude 書込等）が出るため、シムの「PATH 解決→argv 注入→
// agent.start」経路だけを実 herdr で通す。
func installStubClaude(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\necho HD_SHIM_STUB\nexec sleep 300\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("stub claude 作成: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stub
}

// chdirPhysical は物理パス（symlink 解決済み）の一時 dir へ chdir する。
// cwd 一致は文字列比較＝/tmp→/private/tmp・/var→/private/var の symlink 経路
// だと「同じ dir なのに不一致」になる（cm start の既知 quirk と同根）。
// EvalSymlinks で先に物理パスへ寄せて、この級の偽陰性を構造的に排除する。
func chdirPhysical(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	t.Chdir(dir)
	return dir
}

// swapSeams は stdinIsTTY / execAttach seam を差し替える（t.Cleanup で復元）。
// TTY 判定は go test 実行環境に依存させず決定論にする。
func swapSeams(t *testing.T, tty bool, execFn func(string, []string, []string) error) {
	t.Helper()
	oldTTY, oldExec := stdinIsTTY, execAttach
	stdinIsTTY = func() bool { return tty }
	if execFn != nil {
		execAttach = execFn
	}
	t.Cleanup(func() { stdinIsTTY, execAttach = oldTTY, oldExec })
}

// swapViewer は runViewer seam（TTY attach 経路のロックフリー・ローカル
// observe ビューア）を差し替える（t.Cleanup で復元）。実 raw-mode e2e は
// localview_test.go の pty ハーネスが担い、ここでは「どの pane_id に接続
// したか」だけを機械検証する（実 TTY 非依存＝決定論）。
func swapViewer(t *testing.T, fn func(*herdrapi.Client, string) error) {
	t.Helper()
	old := runViewer
	runViewer = fn
	t.Cleanup(func() { runViewer = old })
}

// claudeAgents は実サーバの agent.list をシムと同じ純関数（claudeCandidates）
// で絞る＝テストと本体で判定基準が乖離しない。
func claudeAgents(t *testing.T, api *herdrapi.Client, cwd string) []herdrapi.AgentInfo {
	t.Helper()
	agents, err := api.AgentList()
	if err != nil {
		t.Fatalf("agent.list: %v", err)
	}
	return claudeCandidates(agents, cwd)
}

// waitClaudeAgents は cwd 一致 claude agent が n 件になるまで待つ
// （agent.start 直後の agent.list は反映が遅延し得る＝client_test.go の実測
// に合わせた poll。次の run が古い list を読む race をここで塞ぐ）。
func waitClaudeAgents(t *testing.T, api *herdrapi.Client, cwd string, n int) []herdrapi.AgentInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last []herdrapi.AgentInfo
	for time.Now().Before(deadline) {
		last = claudeAgents(t, api, cwd)
		if len(last) == n {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("cwd 一致 claude agent が %d 件にならない（現在 %d 件: %+v）", n, len(last), last)
	return nil
}

// ============ 実 herdr: 新規／既存 attach 抑止／args 明示新規／複数非 TTY ============

func TestClaudeShimRealHerdrLifecycle(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	t.Setenv("HOME", t.TempDir()) // 着地ルール（~/.herdr-drover/workspaces.json）隔離
	installStubClaude(t)
	work := chdirPhysical(t)
	swapSeams(t, false, nil) // 非 TTY（CI/pipe 経路）

	api := herdrapi.New(sock)

	// --- 非 TTY 新規: stub claude が cwd 一致 agent として出現・出力に pane_id ---
	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	ags := waitClaudeAgents(t, api, work, 1)
	ag := ags[0]
	if ag.Name != "claude" || ag.Cwd != work {
		t.Fatalf("agent.list の exact-match が崩れている: %+v (want cwd=%s)", ag, work)
	}
	if !strings.Contains(out, "pane_id="+ag.PaneID) || !strings.Contains(out, "terminal_id="+ag.TerminalID) {
		t.Fatalf("非 TTY 出力に pane_id/terminal_id が無い:\n%s", out)
	}

	// --- 非 TTY 既存: 同 cwd 再実行 → 新規を作らず既存を報告（件数不変） ---
	code, out2, errb2 := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out2, errb2)
	}
	if !strings.Contains(out2, "pane_id="+ag.PaneID) {
		t.Fatalf("既存 pane_id (%s) を報告していない:\n%s", ag.PaneID, out2)
	}
	// 「作られていない」ことの確認は反映遅延の猶予を置いてから件数を見る
	//（即読みだと dup 作成を見逃す偽陰性になり得る）。
	time.Sleep(1 * time.Second)
	if got := len(claudeAgents(t, api, work)); got != 1 {
		t.Fatalf("同 cwd 再実行で dup が作られた: %d 件", got)
	}

	// --- args 非空（TTY）: 既存があっても常に新規（件数 +1）---
	// 非 TTY×args は herdr 非経由の素 claude 透過に変わった
	//（TestClaudeShimNonTTYArgsExecsClaudeDirectly）ため、このステップは
	// TTY seam＋viewer fake で「新規 pane＋ロックフリー接続」経路を検証する。
	var viewerPane string
	viewerCalled := 0
	swapSeams(t, true, nil) // TTY 判定のみ true（execAttach 実体は attach 経路で不使用）
	swapViewer(t, func(_ *herdrapi.Client, paneID string) error {
		viewerCalled++
		viewerPane = paneID
		return nil
	})
	code, out3, errb3 := runCapture(t, "claude", "--hd-shim-arg-test")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out3, errb3)
	}
	if !strings.Contains(out3, "新規起動") {
		t.Fatalf("args 非空で新規起動の報告が無い:\n%s", out3)
	}
	if viewerCalled != 1 {
		t.Fatalf("TTY 新規後のロックフリー接続（runViewer）が呼ばれていない（called=%d）", viewerCalled)
	}
	swapSeams(t, false, nil) // 以降のステップは非 TTY 経路に戻す
	ags2 := waitClaudeAgents(t, api, work, 2)
	// runViewer は observe/pane.send_text の対象 pane_id を受ける（terminal_id
	// ではない）。新規 claude-2 の PaneID と一致することを確認する。
	var claude2 herdrapi.AgentInfo
	for _, a := range ags2 {
		if a.Name == "claude-2" {
			claude2 = a
		}
	}
	if viewerPane == "" || viewerPane != claude2.PaneID {
		t.Fatalf("runViewer に渡った pane_id=%q が新規 claude-2 の PaneID=%q と不一致", viewerPane, claude2.PaneID)
	}
	// agent 名一意制約（実測 agent_name_taken）への自動採番: 2 本目は
	// encode 形 claude-2 になっている（encode/decode round-trip の実サーバ確認）
	names := map[string]bool{}
	for _, a := range ags2 {
		names[a.Name] = true
	}
	if !names["claude"] || !names["claude-2"] {
		t.Fatalf("自動採番の encode 形が想定外: %v", names)
	}

	// --- 複数候補 × 非 TTY: 先頭を報告して非 attach 終了・件数不変（backstop）---
	code, out4, errb4 := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out4, errb4)
	}
	if !strings.Contains(out4, "attach しません") || !strings.Contains(out4, "pane_id=") {
		t.Fatalf("複数候補×非 TTY の先頭報告が無い:\n%s", out4)
	}
	time.Sleep(1 * time.Second)
	if got := len(claudeAgents(t, api, work)); got != 2 {
		t.Fatalf("複数候補×非 TTY で件数が変わった: %d 件", got)
	}
}

// ============ 実 herdr: viewer seam（TTY ロックフリー接続経路） ============

// TestClaudeShimViewerSeamTTY は TTY 分岐がロックフリー・ローカル observe
// ビューア（runViewer）へ、observe/pane.send_text の対象 pane_id と「シムと
// 同じサーバ socket を持つ api」を渡すことを機械検証する。旧 attach 経路
// （herdr terminal attach <terminal_id>＝TerminalAttach でリサイズロックを
// 張る）から observe（ロック非取得・pane_id 対象）へ切り替えた回帰点。
func TestClaudeShimViewerSeamTTY(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	stub := installStubClaude(t)
	work := chdirPhysical(t)

	api := herdrapi.New(sock)
	// 既存 1 件を API 直で用意（接続経路だけを切り出して検証）。
	ag, err := api.AgentStart("claude", []string{stub}, &herdrapi.AgentStartOptions{Cwd: work})
	if err != nil {
		t.Fatalf("agent.start: %v", err)
	}
	waitClaudeAgents(t, api, work, 1)

	var gotAPI *herdrapi.Client
	var gotPane string
	called := 0
	swapSeams(t, true, nil)
	swapViewer(t, func(a *herdrapi.Client, paneID string) error {
		called++
		gotAPI, gotPane = a, paneID
		return nil
	})

	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if called != 1 {
		t.Fatalf("runViewer 呼び出し回数=%d want 1", called)
	}
	// observe/send_text は pane_id 対象（terminal_id ではない）。既存 agent の
	// PaneID が渡ること＝旧 attach（terminal_id）からの切替の要。
	if gotPane != ag.PaneID {
		t.Fatalf("runViewer pane_id=%q want %q（terminal_id=%q を渡していないか）", gotPane, ag.PaneID, ag.TerminalID)
	}
	// api.SocketPath がシムの socket と同一＝ビューアがシムと同じサーバの
	// pane を観る根拠（旧 exec の env 継承と同役割）。
	if gotAPI == nil || gotAPI.SocketPath != sock {
		t.Fatalf("runViewer api.SocketPath がシムと不一致: got %v want %s", gotAPI, sock)
	}
	// 接続ヒント（detach 案内）は raw-mode/alt-screen へ入る前に出ている必要がある
	if !strings.Contains(out, "Ctrl+B q") {
		t.Fatalf("detach ヒントが無い:\n%s", out)
	}
}

// ============ 実 herdr: server 自動起動 ============

func TestClaudeShimAutoStartsServer(t *testing.T) {
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed; skipping real-server test")
	}
	// 短い /tmp dir（sun_path 104B 制約）＋ XDG 隔離（実 session.json 不可侵）。
	// このテストはシム自身が server を spawn するため、t.Setenv した env を
	// 子プロセスへ継承させる（ensureHerdrServer は cmd.Env=nil＝継承）。
	dir, err := os.MkdirTemp("/tmp", "hd")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(xdg, 0o700); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	sock := filepath.Join(dir, "h.sock")
	t.Setenv("HERDR_SOCKET_PATH", sock)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir()) // 着地ルール（~/.herdr-drover/workspaces.json）隔離
	installStubClaude(t)
	swapSeams(t, false, nil)
	chdirPhysical(t)

	// 停止: 自 socket への graceful stop → 自分が spawn した PID のみ kill の
	// backstop（他プロセス不可侵の恒久規律）。env は明示（t.Setenv の復元
	// cleanup との実行順に依存しない）。
	lastSpawnedServerPID = 0
	env := append(os.Environ(), "HERDR_SOCKET_PATH="+sock, "XDG_CONFIG_HOME="+xdg)
	t.Cleanup(func() {
		stop := exec.Command(bin, "server", "stop")
		stop.Env = env
		_ = stop.Run()
		if pid := lastSpawnedServerPID; pid > 0 {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && syscall.Kill(pid, 0) == nil {
				time.Sleep(100 * time.Millisecond)
			}
			if syscall.Kill(pid, 0) == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		os.RemoveAll(dir)
	})

	// server 無しの状態からシム実行 → 自動起動＋ping 成功＋新規 pane まで通る
	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if lastSpawnedServerPID == 0 {
		t.Fatalf("自動起動経路を通っていない（socket %s に既存 server?）", sock)
	}
	if !strings.Contains(errb, "自動起動") {
		t.Fatalf("自動起動の報告が stderr に無い:\n%s", errb)
	}
	if _, err := herdrapi.New(sock).Ping(); err != nil {
		t.Fatalf("自動起動した server へ ping 失敗: %v", err)
	}
	if !strings.Contains(out, "pane_id=") {
		t.Fatalf("非 TTY 出力に pane_id が無い:\n%s", out)
	}
}

// ============ 実デバイス: stdinIsTTY（/dev/null は TTY でない） ============

// swapStdin は os.Stdin を差し替える（stdinIsTTY は呼出し時点の os.Stdin を
// 読むため、実デバイスを与えて本物の判定関数を検証できる）。
func swapStdin(t *testing.T, f *os.File) {
	t.Helper()
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old })
}

// openPtySlaveForTest は python3 の pty.openpty で実 pty を確保し slave を開く
// （隔離テストレシピ: pty は python3 ハーネス・creack/pty 等の依存追加はしない）。
// master は python プロセスが保持し続ける（macOS は master が閉じると slave が
// 死ぬため sleep で生かしておき t.Cleanup で kill）。
func openPtySlaveForTest(t *testing.T) *os.File {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed; pty slave 真ケースは Gate e2e が担保")
	}
	cmd := exec.Command("python3", "-c",
		"import pty,os,time; m,s=pty.openpty(); print(os.ttyname(s),flush=True); time.sleep(60)")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("python3 pty 起動: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("pty slave パス読取: %v", err)
	}
	slave, err := os.OpenFile(strings.TrimSpace(line), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("pty slave open: %v", err)
	}
	t.Cleanup(func() { slave.Close() })
	return slave
}

// stdinIsTTY の実デバイス回帰。核心は /dev/null: os/exec の Stdin=nil は
// /dev/null になり（cron/launchd/nohup の最頻自動化経路）、char device 判定の
// 旧コードは true を返して attach へ進み herdr terminal attach が ratatui init
// panic（exit=101・実測）していた。termios 判定なら
// /dev/null=false・pipe=false・pty slave=true（本ファイル冒頭の実測と一致）。
func TestStdinIsTTYRealDevices(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer null.Close()
	swapStdin(t, null)
	if stdinIsTTY() {
		t.Fatalf("/dev/null（char device・非 TTY）で stdinIsTTY が true（旧 CharDevice 判定の実バグ）")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	swapStdin(t, r)
	if stdinIsTTY() {
		t.Fatalf("pipe で stdinIsTTY が true")
	}

	slave := openPtySlaveForTest(t)
	swapStdin(t, slave)
	if !stdinIsTTY() {
		t.Fatalf("実 pty slave で stdinIsTTY が false（TTY 真ケース退行）")
	}
}

// ============ 実 herdr: symlink 経路 cwd で dup を作らない ============

// cwd の論理パス（symlink 経由・os.Getwd の PWD kludge が返す）と herdr が
// 登録する物理パスの不一致で、実行のたび dup セッションを量産していた退行の
// 回帰（旧コード FAIL 確認済み）。t.Chdir は PWD env も link に設定するため
// os.Getwd は論理パスを返す＝/tmp（→/private/tmp）から実行する実使用と同型。
func TestClaudeShimSymlinkCwdNoDup(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	t.Setenv("HOME", t.TempDir()) // 着地ルール（~/.herdr-drover/workspaces.json）隔離
	installStubClaude(t)
	swapSeams(t, false, nil)

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Chdir(link) // PWD=link（論理パス）で os.Getwd が link を返す状態を再現

	api := herdrapi.New(sock)

	// 1 回目: 新規。登録される agent cwd は物理パス real（herdr が物理化する
	// 一次事実＝test/claudeshim_e2e_test.go 冒頭の実測）。
	code, out, errb := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	waitClaudeAgents(t, api, real, 1)

	// 2 回目: 論理パス cwd のまま再実行 → 既存へ接続し dup を作らない。
	// 旧コード（EvalSymlinks 正規化なし）は cwd=link と登録済み real の文字列
	// 不一致で毎回「新規起動」＝黙って dup を量産していた。
	code, out2, errb2 := runCapture(t, "claude")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out2, errb2)
	}
	if !strings.Contains(out2, "既存 claude セッションへ接続します") {
		t.Fatalf("symlink cwd の再実行が既存へ接続していない:\n%s", out2)
	}
	time.Sleep(1 * time.Second)
	if got := len(claudeAgents(t, api, real)); got != 1 {
		t.Fatalf("symlink cwd の再実行で dup が作られた: %d 件", got)
	}
}

// ============ 実 herdr: 非 TTY×引数あり → herdr 非経由で素の claude へ透過 ============

// `echo prompt | claude -p …`（alias 経由の自動化）で pane を作り捨てて stdin/
// stdout 契約を破っていた退行の回帰（旧コード FAIL 確認済み）。非 TTY×引数
// ありは pane を作らず素の claude へプロセス置換＝pipe 契約を透過する。
func TestClaudeShimNonTTYArgsExecsClaudeDirectly(t *testing.T) {
	sock := startHerdrForTest(t)
	t.Setenv("HERDR_SOCKET_PATH", sock)
	stub := installStubClaude(t)
	work := chdirPhysical(t)

	var gotArgv0 string
	var gotArgv []string
	called := 0
	swapSeams(t, false, func(argv0 string, argv []string, env []string) error {
		called++
		gotArgv0, gotArgv = argv0, argv
		return nil
	})
	api := herdrapi.New(sock)

	code, out, errb := runCapture(t, "claude", "-p", "hello")
	if code != 0 {
		t.Fatalf("exit=%d\nstdout=%s\nstderr=%s", code, out, errb)
	}
	if called != 1 {
		t.Fatalf("素の claude への exec 透過が起きていない（called=%d。旧コードは pane 作り捨て）:\n%s", called, out)
	}
	if gotArgv0 != stub {
		t.Fatalf("argv0=%q want %q（実 claude 絶対パス）", gotArgv0, stub)
	}
	want := []string{stub, "-p", "hello"}
	if !reflect.DeepEqual(gotArgv, want) {
		t.Fatalf("exec 引数列: got %v want %v", gotArgv, want)
	}
	if strings.Contains(out, "新規起動") {
		t.Fatalf("非 TTY×引数ありで pane を作っている:\n%s", out)
	}
	// pane を作り捨てていない（反映遅延の猶予後に件数 0 を確認）。
	time.Sleep(1 * time.Second)
	if got := len(claudeAgents(t, api, work)); got != 0 {
		t.Fatalf("非 TTY×引数ありで pane が作られた: %d 件", got)
	}
}

// ============ 純関数: exact-match 候補絞り ============

func TestClaudeCandidatesExactMatch(t *testing.T) {
	agents := []herdrapi.AgentInfo{
		{Name: "claude", Cwd: "/w/a", PaneID: "w1:p1"},
		{Name: "claude", Cwd: "/w/a/sub", PaneID: "w1:p2"}, // 子孫 cwd は候補外（exact のみ）
		{Name: "claude", Cwd: "/w/b", PaneID: "w1:p3"},     // 別 cwd
		{Name: "hdprobe", Cwd: "/w/a", PaneID: "w1:p4"},    // 別 name
		{Name: "Claude", Cwd: "/w/a", PaneID: "w1:p5"},     // 大文字は exact-match 外
		{Name: "claude-2", Cwd: "/w/a", PaneID: "w1:p6"},   // 自動採番の encode 形は候補
		{Name: "claude-x", Cwd: "/w/a", PaneID: "w1:p7"},   // encode 形でない＝候補外
		{Name: "claudette", Cwd: "/w/a", PaneID: "w1:p8"},  // 前方一致は拾わない
	}
	got := claudeCandidates(agents, "/w/a")
	if len(got) != 2 || got[0].PaneID != "w1:p1" || got[1].PaneID != "w1:p6" {
		t.Fatalf("exact-match 候補が想定外: %+v", got)
	}
	if got := claudeCandidates(nil, "/w/a"); len(got) != 0 {
		t.Fatalf("空 list で候補が出た: %+v", got)
	}
}

// isClaudeAgentName は encode（claude / claude-N 採番）と round-trip する
// decode。構造一致のみ＝前方一致ヒューリスティックでないことを固定する。
func TestIsClaudeAgentName(t *testing.T) {
	cases := map[string]bool{
		"claude":    true,
		"claude-2":  true,
		"claude-64": true,
		"Claude":    false, // 大文字
		"claude-":   false, // 数字なし
		"claude-x":  false, // 非数字
		"claude-2x": false,
		"claudette": false, // 前方一致は不可
		"claude 2":  false,
		"":          false,
		// decode は encode の像と厳密一致（真部分集合でない decode は往復でない）:
		// encode は i=1→"claude"・i>=2→"claude-i" しか生成しない。
		"claude-0":  false, // encode が生成しない
		"claude-1":  false, // i=1 は "claude"（"claude-1" は生成されない）
		"claude-02": false, // 先頭ゼロは %d が生成しない
	}
	for name, want := range cases {
		if got := isClaudeAgentName(name); got != want {
			t.Fatalf("isClaudeAgentName(%q)=%v want %v", name, got, want)
		}
	}
}

// ============ 純関数: picker 入力解釈（cm start と同 UX） ============

func TestPickerChoice(t *testing.T) {
	cases := []struct {
		in      string
		n       int
		idx     int
		new     bool
		wantErr bool
	}{
		{"\n", 3, 0, false, false},   // Enter=先頭
		{"", 3, 0, false, false},     // EOF 気味の空も先頭
		{"  \n", 3, 0, false, false}, // 空白のみは Enter 扱い
		{"n\n", 3, 0, true, false},   // n=新規
		{"0\n", 3, 0, true, false},   // 0=新規
		{"1\n", 3, 0, false, false},  // 番号指定（1 始まり）
		{"3\n", 3, 2, false, false},
		{"4\n", 3, 0, false, true},   // 範囲外
		{"-1\n", 3, 0, false, true},  // 負数
		{"abc\n", 3, 0, false, true}, // 非数値
		{"N\n", 3, 0, false, true},   // 大文字 N は exact-match 外（受けない）
	}
	for _, c := range cases {
		idx, startNew, err := pickerChoice(c.in, c.n)
		if c.wantErr {
			if err == nil {
				t.Fatalf("pickerChoice(%q,%d): エラーにならない", c.in, c.n)
			}
			continue
		}
		if err != nil {
			t.Fatalf("pickerChoice(%q,%d): %v", c.in, c.n, err)
		}
		if idx != c.idx || startNew != c.new {
			t.Fatalf("pickerChoice(%q,%d)=(%d,%v) want (%d,%v)", c.in, c.n, idx, startNew, c.idx, c.new)
		}
	}
}
