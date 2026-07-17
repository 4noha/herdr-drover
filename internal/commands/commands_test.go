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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/cloud/state"
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
	}
	go func() { _ = cr.Run(ctx) }()
	time.Sleep(1500 * time.Millisecond) // listener attach

	push := func(cmd, sid string) string {
		id, err := st.PushCommand(ctx, pc, cmd, sid, "owner@example.com")
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

	for cmd, wantDetail := range map[string]string{
		"restart-agent": "restart 未配線",
		"self-update":   "update 未配線",
		"restart-proxy": "restart-proxy 未配線",
	} {
		id, err := st.PushCommand(ctx, pc, cmd, "w1:p1", "owner@example.com")
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

	id, err := st1.PushCommand(ctx, pc, "restart-agent", "", "owner@example.com")
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
