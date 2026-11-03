//go:build !windows

package commands

// 実 Firestore エミュレータ＋fake seam（実 launchctl/自己置換/os.Exit を
// しない）で「command doc 書込 → claim（transaction・二重実行防止）→
// dispatch → Ack 監査 read-back」の e2e を検証する。合成サーバなし＝
// 実 WatchCommands/claimCommand/AckCommand 経路を通す（cm
// internal/cloud/agent/commands_test.go のパターン移植＋drover 差分:
// self-update は DoExit・restart-proxy は DoProxy seam・二重 runner での
// claim 一意性）。エミュレータ不在（gcloud/Java21+ 無し）の環境は Skip。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
)

const projectID = "demo-hd-commands"

// ===== Firestore エミュレータ（test/e2e_test.go の確定パターン） =====

func java21Bin() string {
	cands := []string{
		"/opt/homebrew/opt/openjdk/bin",
		"/opt/homebrew/opt/openjdk@25/bin",
		"/opt/homebrew/opt/openjdk@21/bin",
	}
	for _, d := range cands {
		j := d + "/java"
		if fi, err := os.Stat(j); err == nil && !fi.IsDir() {
			out, _ := exec.Command(j, "-version").CombinedOutput()
			s := string(out)
			for _, v := range []string{"\"21", "\"22", "\"23", "\"24", "\"25", "\"26"} {
				if strings.Contains(s, v) {
					return d
				}
			}
		}
	}
	return ""
}

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var emuCmd *exec.Cmd

func TestMain(m *testing.M) {
	jbin := java21Bin()
	if _, err := exec.LookPath("gcloud"); err == nil && jbin != "" {
		port := freePort()
		host := fmt.Sprintf("127.0.0.1:%d", port)
		emuCmd = exec.Command("gcloud", "beta", "emulators", "firestore", "start",
			"--host-port="+host, "--quiet")
		emuCmd.Env = append(os.Environ(),
			"PATH="+jbin+":"+os.Getenv("PATH"),
			"CLOUDSDK_CORE_DISABLE_PROMPTS=1")
		emuCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := emuCmd.Start(); err == nil {
			for i := 0; i < 80; i++ { // 最大 40s
				if c, err := http.Get("http://" + host + "/"); err == nil {
					c.Body.Close()
					os.Setenv("FIRESTORE_EMULATOR_HOST", host)
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
				_ = syscall.Kill(-emuCmd.Process.Pid, syscall.SIGKILL)
				emuCmd = nil
			}
		} else {
			emuCmd = nil
		}
	}
	code := m.Run()
	if emuCmd != nil {
		_ = syscall.Kill(-emuCmd.Process.Pid, syscall.SIGKILL)
	}
	os.Exit(code)
}

func newState(t *testing.T, pc string) *state.Client {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	st, err := state.New(context.Background(), projectID, pc)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// waitCmdStatus は Ack 監査（status=done|error）の read-back。
func waitCmdStatus(t *testing.T, st *state.Client, pc, id string,
	to time.Duration) state.Command {
	t.Helper()
	dl := time.Now().Add(to)
	for time.Now().Before(dl) {
		cs, _ := st.RecentCommands(context.Background(), pc, 20)
		for _, c := range cs {
			if c.ID == id && (c.Status == "done" || c.Status == "error") {
				return c
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	t.Fatalf("命令 %s が done/error に至らない（dispatch 不成立）", id)
	return state.Command{}
}

// ===== dispatch・Ack 先行・revocation（cm パターン＋drover 写像） =====

func TestCommandRunnerDispatchAndRevocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover"
	st := newState(t, pc)

	var restarts, updates, exits, respawns atomic.Int32
	var claudeRestarts, claudeUpdates, updateAlls atomic.Int32
	var ackBeforeUpdateAllExit atomic.Bool
	var claudeSid atomic.Value // 最後に渡された sid（"" 込みで検証したいので Value）
	claudeSid.Store("")
	var revoked atomic.Bool
	// Ack 先行順序の機械検証: 破壊的 seam（DoRestart / self-update の DoExit）の
	// **実行時点**で当該命令の監査 status が既に done であることを、実エミュ
	// レータの read-back で確認する（kickstart -k / exit で agent が即死しても
	// 監査が done で残る、という Ack 先行設計の実証。後 Ack だと本番では永遠に
	// running 滞留＝この 2 フラグが false になり FAIL する。commands.go の
	// Ack と seam の呼び順を入れ替えた旧形コードで FAIL することを確認済み）。
	// 各 cmd 種は seam 発火時点で 1 命令しか存在しない（テストは逐次 push＋
	// 最終 status 待ち）ので Cmd 名での特定は exact。
	var ackBeforeRestart, ackBeforeExit atomic.Bool
	statusOfCmd := func(cmd string) string {
		cs, _ := st.RecentCommands(context.Background(), pc, 20)
		for _, c := range cs {
			if c.Cmd == cmd {
				return c.Status
			}
		}
		return ""
	}
	cr := &CommandRunner{
		St:      st,
		Revoked: func(context.Context) bool { return revoked.Load() },
		DoRestart: func(context.Context) error {
			if statusOfCmd("restart-agent") == "done" {
				ackBeforeRestart.Store(true)
			}
			restarts.Add(1)
			return nil
		},
		DoUpdate: func(context.Context) (string, bool, error) {
			updates.Add(1)
			return "v9.9.9", true, nil
		},
		DoExit: func() {
			if statusOfCmd("self-update") == "done" {
				ackBeforeExit.Store(true)
			}
			if statusOfCmd("update-all") == "done" {
				ackBeforeUpdateAllExit.Store(true)
			}
			exits.Add(1)
		},
		// respawn 写像: 稼働 bridge がある sid だけ成功（webterm.respawn の
		// 「未知 sid は error」契約を seam で模す。実 bridge 経路は
		// test/command_e2e_test.go の実環境 gate が担保）。
		DoProxy: func(_ context.Context, sid string) error {
			if sid == "w1:p1" {
				respawns.Add(1)
				return nil
			}
			return fmt.Errorf("sid %q の稼働 bridge が無い", sid)
		},
		// restart-claude 写像: agent 自身は死なない＝Ack は実行**後**で、
		// 実行結果の要約が detail に載る（何件やって何件 skip したかを監査で追う）。
		DoRestartClaude: func(_ context.Context, sid, agent string) (string, error) {
			claudeSid.Store(sid)
			if sid == "w9:p9" {
				return "", fmt.Errorf("sid %q は再起動対象のローカル claude pane ではない", sid)
			}
			claudeRestarts.Add(1)
			return "再起動 2 件: claude,claude-2 / skip claude-3(working)", nil
		},
		// update-all 写像: 逐次実行の要約を返し、成功時は restart=true。
		// 呼び手（handle）は Ack を打ってから DoExit する契約。
		DoUpdateAll: func(context.Context) (string, bool, error) {
			updateAlls.Add(1)
			return "claude: 更新 2.1.219 → 2.1.220 / 再起動 1 件: claude / drover: 更新 v9.9.9", true, nil
		},
		// update-claude 写像: claude 本体更新＋再起動の要約が detail に載る。
		DoUpdateClaude: func(_ context.Context, sid, agent string) (string, error) {
			claudeSid.Store(sid)
			claudeUpdates.Add(1)
			return "更新 2.1.214 → 2.1.219 / 再起動 2 件: claude,claude-2", nil
		},
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond) // listener attach

	push := func(cmd, sid string) string {
		id, err := st.PushCommand(ctx, pc, cmd, sid, "", "owner@example.com")
		if err != nil {
			t.Fatalf("PushCommand(%s): %v", cmd, err)
		}
		return id
	}

	// restart-agent: DoRestart 発火・Ack done・**Ack が実行より先**（破壊的
	// 命令の監査消失防止＝cm 規律）
	c := waitCmdStatus(t, st, pc, push("restart-agent", ""), 8*time.Second)
	if c.Status != "done" || restarts.Load() != 1 {
		t.Fatalf("restart-agent: status=%s restarts=%d", c.Status, restarts.Load())
	}
	if !ackBeforeRestart.Load() {
		t.Fatalf("restart-agent の Ack が実行（DoRestart）に先行していない")
	}

	// self-update: DoUpdate 発火・tag が監査に残る・更新後 DoExit も発火
	// （drover は単一 agent＝exit→launchd KeepAlive 再起動。cm の
	// DoRestart 相当）。
	c = waitCmdStatus(t, st, pc, push("self-update", ""), 8*time.Second)
	if c.Status != "done" || updates.Load() != 1 ||
		!strings.Contains(c.Detail, "v9.9.9") || exits.Load() != 1 {
		t.Fatalf("self-update: %+v updates=%d exits=%d",
			c, updates.Load(), exits.Load())
	}
	if !ackBeforeExit.Load() {
		t.Fatalf("self-update の Ack が再起動（DoExit）に先行していない")
	}

	// restart-proxy: 稼働 bridge がある sid → respawn 発火・done
	c = waitCmdStatus(t, st, pc, push("restart-proxy", "w1:p1"), 8*time.Second)
	if c.Status != "done" || respawns.Load() != 1 ||
		!strings.Contains(c.Detail, "respawn") {
		t.Fatalf("restart-proxy(既知 sid): %+v respawns=%d", c, respawns.Load())
	}

	// restart-proxy: 未知 sid → status=error で Ack（pending 滞留させない）
	c = waitCmdStatus(t, st, pc, push("restart-proxy", "w9:p9"), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "稼働 bridge が無い") {
		t.Fatalf("restart-proxy(未知 sid) が error にならない: %+v", c)
	}
	if respawns.Load() != 1 {
		t.Fatalf("未知 sid で respawn が発火: %d", respawns.Load())
	}

	// restart-claude: sid 空＝その PC のローカル claude pane 全部。要約が
	// そのまま監査 detail に載る（skip 理由まで残す＝silent skip 禁止）。
	c = waitCmdStatus(t, st, pc, push("restart-claude", ""), 8*time.Second)
	if c.Status != "done" || claudeRestarts.Load() != 1 ||
		!strings.Contains(c.Detail, "再起動 2 件") ||
		!strings.Contains(c.Detail, "skip claude-3(working)") {
		t.Fatalf("restart-claude(sid 空): %+v claudeRestarts=%d", c, claudeRestarts.Load())
	}
	if got := claudeSid.Load().(string); got != "" {
		t.Fatalf("sid 空の命令で seam に %q が渡った（全件指定が壊れている）", got)
	}

	// restart-claude: sid 指定はその sid が seam へ exact で渡る
	c = waitCmdStatus(t, st, pc, push("restart-claude", "w1:p1"), 8*time.Second)
	if c.Status != "done" || claudeRestarts.Load() != 2 {
		t.Fatalf("restart-claude(sid 指定): %+v claudeRestarts=%d", c, claudeRestarts.Load())
	}
	if got := claudeSid.Load().(string); got != "w1:p1" {
		t.Fatalf("seam へ渡った sid = %q（w1:p1 のはず）", got)
	}

	// restart-claude: 対象外 sid → status=error で Ack（pending 滞留させない）
	c = waitCmdStatus(t, st, pc, push("restart-claude", "w9:p9"), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "ローカル claude pane ではない") {
		t.Fatalf("restart-claude(対象外 sid) が error にならない: %+v", c)
	}
	if claudeRestarts.Load() != 2 {
		t.Fatalf("対象外 sid で再起動が発火: %d", claudeRestarts.Load())
	}

	// update-claude: claude 本体更新＋再起動。版の前後と再起動結果が監査に残る。
	c = waitCmdStatus(t, st, pc, push("update-claude", ""), 8*time.Second)
	if c.Status != "done" || claudeUpdates.Load() != 1 ||
		!strings.Contains(c.Detail, "更新 2.1.214 → 2.1.219") ||
		!strings.Contains(c.Detail, "再起動 2 件") {
		t.Fatalf("update-claude: %+v claudeUpdates=%d", c, claudeUpdates.Load())
	}
	// self-update（herdr-drover 自身）とは別経路＝互いに発火しないこと。
	if updates.Load() != 1 {
		t.Fatalf("update-claude が self-update を巻き込んだ: %d", updates.Load())
	}
	if claudeRestarts.Load() != 2 {
		t.Fatalf("update-claude が restart-claude seam を二重に呼んだ: %d", claudeRestarts.Load())
	}

	// update-all: 両段の要約が監査に残り、**Ack が exit より先**（後 Ack だと
	// プロセスが死んで running のまま滞留する＝既存の破壊的命令と同じ規律）。
	exitsBefore := exits.Load()
	c = waitCmdStatus(t, st, pc, push("update-all", ""), 12*time.Second)
	if c.Status != "done" || updateAlls.Load() != 1 {
		t.Fatalf("update-all: %+v updateAlls=%d", c, updateAlls.Load())
	}
	if !strings.Contains(c.Detail, "claude:") || !strings.Contains(c.Detail, "drover:") ||
		!strings.Contains(c.Detail, "再起動（exit）を実行") {
		t.Fatalf("update-all の detail が両段＋再起動を示していない: %+v", c)
	}
	if exits.Load() != exitsBefore+1 {
		t.Fatalf("update-all が exit を呼んでいない: %d→%d", exitsBefore, exits.Load())
	}
	if !ackBeforeUpdateAllExit.Load() {
		t.Fatalf("update-all の Ack が exit に先行していない")
	}

	// revocation: 失効中は実行拒否（DoRestart 増えない・error revoked）
	revoked.Store(true)
	c = waitCmdStatus(t, st, pc, push("restart-agent", ""), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "revoked") {
		t.Fatalf("失効中に実行拒否されない: %+v", c)
	}
	if restarts.Load() != 1 {
		t.Fatalf("失効中なのに DoRestart が発火: %d", restarts.Load())
	}
}

// 未配線（nil seam）は error Ack＝滞留させない（Web ターミナル無効構成の
// restart-proxy と、配線漏れの restart/update を同じ規律で監査に落とす）。
func TestCommandRunnerUnwiredAcksError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover-unwired"
	st := newState(t, pc)
	cr := &CommandRunner{
		St:      st,
		Revoked: func(context.Context) bool { return false },
		// DoRestart/DoUpdate/DoProxy 全て nil
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond)

	// 旧名で投入しても **新名として処理される**（NormalizeCommand）ので、
	// 未配線 Ack の文言は新名で出る。旧 PC からの投入を受け続けるための仕組み。
	for cmd, wantDetail := range map[string]string{
		"restart-agent":         "restart 未配線",
		"self-update":           "update 未配線",
		"restart-proxy":         "restart-proxy 未配線",
		"restart-claude":        "restart-agent-session 未配線", // 旧名 → 新名
		"update-claude":         "update-agent-cli 未配線",      // 旧名 → 新名
		"restart-agent-session": "restart-agent-session 未配線",
		"update-agent-cli":      "update-agent-cli 未配線",
		"update-all":            "update-all 未配線",
		"read-pane":             "read-pane 未配線",
		"send-input":            "send-input 未配線",
		"interrupt":             "interrupt 未配線",
		"semi-auto":             "semi-auto 未配線",
	} {
		id, err := st.PushCommand(ctx, pc, cmd, "w1:p1", "", "owner@example.com")
		if err != nil {
			t.Fatalf("PushCommand(%s): %v", cmd, err)
		}
		c := waitCmdStatus(t, st, pc, id, 8*time.Second)
		if c.Status != "error" || !strings.Contains(c.Detail, wantDetail) {
			t.Fatalf("%s 未配線の Ack が想定外: %+v", cmd, c)
		}
	}
}

// claim（pending→running transaction）の二重実行防止: 同一 pc を 2 つの
// runner（別 Firestore クライアント）が購読しても、1 命令は**ちょうど 1 回**
// だけ実行される。cm の claim 設計（Snapshot 再配信・複数 agent 耐性）の
// 実エミュレータ検証。
func TestCommandClaimPreventsDoubleExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover-claim"
	st1 := newState(t, pc)
	st2 := newState(t, pc)

	var n1, n2 atomic.Int32
	mk := func(st *state.Client, n *atomic.Int32) *CommandRunner {
		return &CommandRunner{
			St:      st,
			Revoked: func(context.Context) bool { return false },
			DoRestart: func(context.Context) error {
				n.Add(1)
				return nil
			},
		}
	}
	go func() { _ = mk(st1, &n1).Run(ctx) }()
	go func() { _ = mk(st2, &n2).Run(ctx) }()
	time.Sleep(1500 * time.Millisecond) // 両 listener attach

	id, err := st1.PushCommand(ctx, pc, "restart-agent", "", "", "owner@example.com")
	if err != nil {
		t.Fatalf("PushCommand: %v", err)
	}
	c := waitCmdStatus(t, st1, pc, id, 8*time.Second)
	if c.Status != "done" {
		t.Fatalf("status=%s", c.Status)
	}
	// 敗者側の遅延実行が無いことを確定させる猶予後に総和を検査。
	time.Sleep(2 * time.Second)
	if total := n1.Load() + n2.Load(); total != 1 {
		t.Fatalf("二重実行防止が破れている: 実行 %d 回（n1=%d n2=%d）",
			total, n1.Load(), n2.Load())
	}
}

// 旧命令名は新名へ写像され、agent は "claude" に固定される（旧名は claude 専用
// だった＝推測ではなく事実）。**どう解釈したかは必ず Ack detail に残す**。
// まだ更新していない PC の Web/CLI から旧名が飛んでくるので、この経路は生き続ける。
func TestLegacyCommandNameMapsToClaude(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover-legacy"
	st := newState(t, pc)
	var mu sync.Mutex
	var gotSID, gotAgent string
	cr := &CommandRunner{
		St:      st,
		Revoked: func(context.Context) bool { return false },
		DoRestartClaude: func(_ context.Context, sid, agent string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			gotSID, gotAgent = sid, agent
			return "再起動 1 件", nil
		},
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond)

	id, err := st.PushCommand(ctx, pc, "restart-claude", "w1:p1", "", "owner@example.com")
	if err != nil {
		t.Fatalf("PushCommand: %v", err)
	}
	c := waitCmdStatus(t, st, pc, id, 8*time.Second)
	if c.Status != "done" {
		t.Fatalf("Ack = %+v", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAgent != "claude" {
		t.Errorf("旧名 restart-claude の agent = %q, want claude", gotAgent)
	}
	if gotSID != "w1:p1" {
		t.Errorf("sid = %q", gotSID)
	}
	if !strings.Contains(c.Detail, "旧名") {
		t.Errorf("旧名として解釈したことが監査に残っていない: %q", c.Detail)
	}
}

// 新名 + agent 指定はそのまま透過する。
func TestNewCommandNameCarriesAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover-newname"
	st := newState(t, pc)
	var mu sync.Mutex
	var gotAgent string
	cr := &CommandRunner{
		St:      st,
		Revoked: func(context.Context) bool { return false },
		DoRestartClaude: func(_ context.Context, sid, agent string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			gotAgent = agent
			return "再起動 1 件", nil
		},
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond)

	id, err := st.PushCommand(ctx, pc, "restart-agent-session", "", "codex", "owner@example.com")
	if err != nil {
		t.Fatalf("PushCommand: %v", err)
	}
	if c := waitCmdStatus(t, st, pc, id, 8*time.Second); c.Status != "done" {
		t.Fatalf("Ack = %+v", c)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAgent != "codex" {
		t.Errorf("agent = %q, want codex", gotAgent)
	}
}

// 画面 I/O 命令（Phase 2）: read-pane は expires_at 不要（非破壊）、
// send-input/interrupt は expires_at 必須（スリープ復帰後の遅延実行事故を
// 防ぐ・commands.go checkExpiry）。args が exact にそのまま seam へ渡ること、
// 期限切れ/未指定では seam 自体が発火しないこと（監査にも exact な理由が
// 残ること）を検証する。
func TestCommandRunnerScreenIOCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover-screenio"
	st := newState(t, pc)

	var reads, sends, interrupts atomic.Int32
	var lastReadArgs, lastSendArgs atomic.Value

	cr := &CommandRunner{
		St:      st,
		Revoked: func(context.Context) bool { return false },
		DoReadPane: func(_ context.Context, id, sid string, args map[string]string) (string, error) {
			reads.Add(1)
			lastReadArgs.Store(args)
			if sid == "w9:p9" {
				return "", fmt.Errorf("pane が見つからない")
			}
			return fmt.Sprintf("画面取得完了（id=%s, 42 bytes）", id), nil
		},
		DoSendInput: func(_ context.Context, sid string, args map[string]string) (string, error) {
			sends.Add(1)
			lastSendArgs.Store(args)
			if sid == "w9:p9" {
				return "", fmt.Errorf("working 中のため拒否")
			}
			return "入力完了", nil
		},
		DoInterrupt: func(_ context.Context, sid string) (string, error) {
			interrupts.Add(1)
			if sid == "w9:p9" {
				return "", fmt.Errorf("pane が見つからない")
			}
			return "中断完了", nil
		},
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond)

	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)
	past := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339Nano)

	push := func(cmd, sid string, args map[string]string) string {
		id, err := st.PushCommandArgs(ctx, pc, cmd, sid, "", "owner@example.com", "mcp", args)
		if err != nil {
			t.Fatalf("PushCommandArgs(%s): %v", cmd, err)
		}
		return id
	}

	// read-pane: expires_at 不要（非破壊）。args が exact に渡り、成功時は
	// 要約が detail に載る。
	c := waitCmdStatus(t, st, pc, push("read-pane", "w1:p1", map[string]string{"source": "visible", "lines": "50"}), 8*time.Second)
	if c.Status != "done" || reads.Load() != 1 || !strings.Contains(c.Detail, "bytes") {
		t.Fatalf("read-pane: %+v reads=%d", c, reads.Load())
	}
	if got, _ := lastReadArgs.Load().(map[string]string); got["source"] != "visible" || got["lines"] != "50" {
		t.Fatalf("read-pane に渡った args が想定外: %+v", got)
	}

	// read-pane: seam エラー → status=error（pending 滞留させない）
	c = waitCmdStatus(t, st, pc, push("read-pane", "w9:p9", nil), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "見つからない") {
		t.Fatalf("read-pane(エラー sid): %+v", c)
	}

	// send-input: expires_at 無し → seam 自体を呼ばず error（期限切れ事故防止の
	// ゲートが nil seam チェックの後・DoSendInput 呼び出しの前にあることの検証）。
	c = waitCmdStatus(t, st, pc, push("send-input", "w1:p1", map[string]string{"text": "hello"}), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "expires_at が必須") {
		t.Fatalf("send-input(expires_at 無し): %+v", c)
	}
	if sends.Load() != 0 {
		t.Fatalf("expires_at 無しで seam が発火: %d", sends.Load())
	}

	// send-input: 期限切れ → error、seam は呼ばれない
	c = waitCmdStatus(t, st, pc, push("send-input", "w1:p1", map[string]string{"text": "hello", "expires_at": past}), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "expired") {
		t.Fatalf("send-input(期限切れ): %+v", c)
	}
	if sends.Load() != 0 {
		t.Fatalf("期限切れで seam が発火: %d", sends.Load())
	}

	// send-input: 有効期限内 → seam 発火・args が exact に渡る・done
	c = waitCmdStatus(t, st, pc, push("send-input", "w1:p1", map[string]string{"text": "hello", "expires_at": future}), 8*time.Second)
	if c.Status != "done" || sends.Load() != 1 || c.Detail != "入力完了" {
		t.Fatalf("send-input(有効): %+v sends=%d", c, sends.Load())
	}
	if got, _ := lastSendArgs.Load().(map[string]string); got["text"] != "hello" {
		t.Fatalf("send-input に渡った args が想定外: %+v", got)
	}

	// interrupt: 同じ expires_at ゲートを共有する（コピペミスの回帰検知）。
	c = waitCmdStatus(t, st, pc, push("interrupt", "w1:p1", nil), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "expires_at が必須") {
		t.Fatalf("interrupt(expires_at 無し): %+v", c)
	}
	if interrupts.Load() != 0 {
		t.Fatalf("expires_at 無しで interrupt seam が発火: %d", interrupts.Load())
	}
	c = waitCmdStatus(t, st, pc, push("interrupt", "w1:p1", map[string]string{"expires_at": future}), 8*time.Second)
	if c.Status != "done" || interrupts.Load() != 1 {
		t.Fatalf("interrupt(有効): %+v interrupts=%d", c, interrupts.Load())
	}

	// interrupt: seam エラー → status=error
	c = waitCmdStatus(t, st, pc, push("interrupt", "w9:p9", map[string]string{"expires_at": future}), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "見つからない") {
		t.Fatalf("interrupt(エラー sid): %+v", c)
	}
}

// semi-auto（セミオート続行の pane 単位 ON/OFF）: sid 必須（空は error Ack・seam 不発火）、
// args["on"] がそのまま seam へ渡り、戻りの summary が Ack detail に載る。
func TestCommandRunnerSemiAuto(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc := "cmd-drover-semiauto"
	st := newState(t, pc)

	var calls atomic.Int32
	var lastSID, lastOn atomic.Value
	cr := &CommandRunner{
		St:      st,
		Revoked: func(context.Context) bool { return false },
		DoSemiAuto: func(_ context.Context, sid string, args map[string]string) (string, error) {
			calls.Add(1)
			lastSID.Store(sid)
			lastOn.Store(args["on"])
			if sid == "w9:p9" {
				return "", fmt.Errorf("pane が見つからない")
			}
			return "semi-auto ON pane=" + sid, nil
		},
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond)
	push := func(sid string, args map[string]string) string {
		id, err := st.PushCommandArgs(ctx, pc, "semi-auto", sid, "", "owner@example.com", "web", args)
		if err != nil {
			t.Fatalf("PushCommandArgs: %v", err)
		}
		return id
	}
	// sid 空 → error、seam は呼ばれない。
	c := waitCmdStatus(t, st, pc, push("", map[string]string{"on": "1"}), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "sid 必須") || calls.Load() != 0 {
		t.Fatalf("sid 空: %+v calls=%d", c, calls.Load())
	}
	// 正常: args["on"] が渡り、summary が detail。
	c = waitCmdStatus(t, st, pc, push("w1:p5", map[string]string{"on": "1"}), 8*time.Second)
	if c.Status != "done" || c.Detail != "semi-auto ON pane=w1:p5" || calls.Load() != 1 {
		t.Fatalf("正常: %+v calls=%d", c, calls.Load())
	}
	if lastSID.Load() != "w1:p5" || lastOn.Load() != "1" {
		t.Fatalf("seam 引数: sid=%v on=%v", lastSID.Load(), lastOn.Load())
	}
	// 失敗は error Ack に理由が残る。
	c = waitCmdStatus(t, st, pc, push("w9:p9", nil), 8*time.Second)
	if c.Status != "error" || !strings.Contains(c.Detail, "見つからない") {
		t.Fatalf("失敗: %+v", c)
	}
}
