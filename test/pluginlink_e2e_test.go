//go:build !windows

// pluginlink_e2e_test — Phase 4「プラグイン化」の plugin link gate（常設・実環境）:
//
//	実 herdr 隔離サーバ（HOME/XDG_CONFIG_HOME/XDG_STATE_HOME 隔離）＋
//	[[build]] 実体（scripts/build.sh）で作った実 bin/herdr-drover で
//
//	  herdr plugin link <このリポジトリ root> → plugin_linked（警告ゼロ）
//	  → plugin list --json で id/enabled/actions/events を exact-match 認識
//	  → workspace create → [[events]] pane.created hook → bin/herdr-drover nudge
//	    → fake pidfile（隔離 HOME・自 PID）→ **実 SIGUSR1 が自プロセスへ届く**
//	  → plugin action invoke（実仕様抽出: `herdr plugin action invoke <action_id>
//	    --plugin <plugin_id>`）→ 同じく実 SIGUSR1
//	  → workspace close → workspace.closed hook（pane.closed が workspace close
//	    では発火しない実測穴を塞ぐ購読が効いている物証）
//	  → 各段を herdr plugin log list の実行監査（event/action_id・exit_code=0・
//	    stdout の nudged pid）で機械確認
//
// を検証する。Firestore は不要（nudge はシグナル送出のみ）＝herdr さえあれば
// 常に走る。herdr 不在の環境のみ Skip（鉄則2）。
//
// 実仕様抽出の根拠（隔離 herdr 0.7.4 実測・2026-07-17）:
//   - `herdr plugin link <path>` → {"result":{"type":"plugin_linked","plugin":{...}}}
//   - `herdr plugin list --json` → {"result":{"type":"plugin_list","plugins":[...]}}
//     plugins[]: plugin_id/enabled/version/actions[]{id}/events[]{on}
//   - `herdr plugin action list` → actions[]{plugin_id,action_id}（qualified 形は
//     CLI には現れない＝invoke は action_id + --plugin で指定する）
//   - `herdr plugin action invoke nudge --plugin 4noha.drover` →
//     {"result":{"type":"plugin_action_invoked","log":{"status":"running",...}}}
//   - `herdr plugin log list --plugin <id>` → logs[]: event（hook 時・ドット名
//     "pane.created"）/action_id（action 時）/status "succeeded"/exit_code/stdout
//   - hook/action の子プロセスは herdr server の env（HOME 含む）を継承する＝
//     server を HOME 隔離で起動すれば nudge の pidfile（~/.herdr-drover/
//     agent.pid）が fake に向く（実測: stdout "nudged: pid <trap pid>"）
//
// 本テストが FAIL することの確認（新設 gate が空緑でない物証・実施済み）:
//   - manifest から workspace.closed を外す → events exact-match で即 FAIL
//   - pane.created の command を ["sh","-c","exit 1"] に差替 → SIGUSR1 不達＋
//     監査 status!=succeeded で FAIL
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

const pluginID = "4noha.drover"

// herdrCLI は隔離サーバの env で herdr CLI を実行し stdout/stderr を返す
// （exit 非 0 は即 FAIL）。plugin 系サブコマンドは全て socket 経由＝
// HERDR_SOCKET_PATH の隔離がそのまま効く。
func herdrCLI(t *testing.T, srv *testServer, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(srv.bin, args...)
	cmd.Env = srv.env
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("herdr %s: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, so.String(), se.String())
	}
	return so.String(), se.String()
}

// cliResult は CLI の {"id":...,"result":{...}} 封筒から result を out へ decode
// する（herdr 0.7.4 実測スキーマ）。
func cliResult(t *testing.T, stdout string, out any) {
	t.Helper()
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || len(env.Result) == 0 {
		t.Fatalf("CLI 封筒の parse 失敗: %v\nstdout: %s", err, stdout)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		t.Fatalf("result の parse 失敗: %v\nresult: %s", err, env.Result)
	}
}

// pluginLogEntry は herdr plugin log list の 1 実行監査（実測スキーマ。
// event hook は event=ドット名・action は action_id を持つ）。
type pluginLogEntry struct {
	Event    string `json:"event"`
	ActionID string `json:"action_id"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// waitPluginLog は述語に合う実行監査 log が succeeded/exit 0 で現れるまで
// 待って返す（合図の SIGUSR1 とは独立の機械的物証）。
func waitPluginLog(t *testing.T, srv *testServer, what string,
	match func(pluginLogEntry) bool) pluginLogEntry {
	t.Helper()
	var found pluginLogEntry
	waitFor(t, 20*time.Second, what, func() (bool, error) {
		so, _ := herdrCLI(t, srv, "plugin", "log", "list", "--plugin", pluginID, "--limit", "50")
		var res struct {
			Logs []pluginLogEntry `json:"logs"`
		}
		cliResult(t, so, &res)
		for _, l := range res.Logs {
			if match(l) {
				found = l
				return true, nil
			}
		}
		return false, fmt.Errorf("logs=%+v", res.Logs)
	})
	if found.Status != "succeeded" || found.ExitCode == nil || *found.ExitCode != 0 {
		t.Fatalf("%s の実行監査が成功でない: %+v", what, found)
	}
	return found
}

func drainSignals(ch <-chan os.Signal) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func waitSIGUSR1(t *testing.T, ch <-chan os.Signal, what string) {
	t.Helper()
	select {
	case <-ch:
		// 実 SIGUSR1 到達（nudge → syscall.Kill → カーネル → 自プロセス）
	case <-time.After(20 * time.Second):
		t.Fatalf("SIGUSR1 が届かない: %s", what)
	}
}

func TestE2EPluginLinkGate(t *testing.T) {
	root := moduleRoot(t)

	// --- [[build]] 実体で bin/herdr-drover を現ソースから作る（stale bin での
	// 偽緑/偽赤を防ぐ。出力は .gitignore 済み bin/ のみ＝repo 非汚染）---
	build := exec.Command("sh", "scripts/build.sh")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("scripts/build.sh 失敗: %v\n%s", err, out)
	}

	// --- 実シグナル受信器（nudge_test.go と同じ規律: Notify を送出より先に）---
	got := make(chan os.Signal, 8)
	signal.Notify(got, syscall.SIGUSR1)
	defer signal.Stop(got)

	// --- 隔離 herdr。HOME/XDG_STATE_HOME も隔離＝hook の nudge が読む
	// pidfile（$HOME/.herdr-drover/agent.pid）を fake（自 PID）へ向ける。
	// hook 子プロセスが server env を継承することは実測済み（冒頭コメント）＝
	// 本テスト自体がその継承の回帰検知にもなる。---
	hookHome, err := os.MkdirTemp("/tmp", "hdh")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(hookHome) })
	if err := os.MkdirAll(filepath.Join(hookHome, "xstate"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv, hc := startHerdr(t,
		"HOME="+hookHome,
		"XDG_STATE_HOME="+filepath.Join(hookHome, "xstate"))

	// fake pidfile: 自 PID（生存確認 pidAlive を通り、SIGUSR1 が自分へ届く）。
	pidDir := filepath.Join(hookHome, ".herdr-drover")
	if err := os.MkdirAll(pidDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "agent.pid"),
		[]byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	nudgedMark := fmt.Sprintf("nudged: pid %d", os.Getpid())

	// --- plugin link（このリポジトリ root をそのまま）＝manifest 検証を herdr
	// 本体に通す。platforms 宣言済みなので警告ゼロのはず（出たら manifest
	// 退行＝ここで捕まえる）。---
	so, se := herdrCLI(t, srv, "plugin", "link", root)
	var linked struct {
		Type   string `json:"type"`
		Plugin struct {
			PluginID string `json:"plugin_id"`
		} `json:"plugin"`
	}
	cliResult(t, so, &linked)
	if linked.Type != "plugin_linked" || linked.Plugin.PluginID != pluginID {
		t.Fatalf("plugin link の応答が想定外: %s", so)
	}
	for _, s := range []string{so, se} {
		if strings.Contains(strings.ToLower(s), "warn") {
			t.Fatalf("plugin link が警告を出した（manifest 退行）: %s", s)
		}
	}

	// --- plugin list --json: 認識内容を exact-match（manifest ドリフト検知）---
	so, _ = herdrCLI(t, srv, "plugin", "list", "--json")
	var list struct {
		Plugins []struct {
			PluginID string `json:"plugin_id"`
			Enabled  bool   `json:"enabled"`
			Version  string `json:"version"`
			Actions  []struct {
				ID string `json:"id"`
			} `json:"actions"`
			Events []struct {
				On string `json:"on"`
			} `json:"events"`
		} `json:"plugins"`
	}
	cliResult(t, so, &list)
	if len(list.Plugins) != 1 || list.Plugins[0].PluginID != pluginID || !list.Plugins[0].Enabled {
		t.Fatalf("plugin が認識されていない: %s", so)
	}
	p := list.Plugins[0]
	var acts, evs []string
	for _, a := range p.Actions {
		acts = append(acts, a.ID)
	}
	for _, e := range p.Events {
		evs = append(evs, e.On)
	}
	sort.Strings(acts)
	sort.Strings(evs)
	// actions 3 種（manifest と exact 一致）
	if want := []string{"install", "nudge", "status"}; !equalStrings(acts, want) {
		t.Fatalf("actions が manifest と不一致: got=%v want=%v", acts, want)
	}
	// events 6 種（pane.closed が workspace close で発火しない実測穴を
	// workspace.closed/tab.closed/pane.exited で塞ぐ構成そのもの）
	if want := []string{"pane.agent_status_changed", "pane.closed", "pane.created",
		"pane.exited", "tab.closed", "workspace.closed"}; !equalStrings(evs, want) {
		t.Fatalf("events が manifest と不一致: got=%v want=%v", evs, want)
	}

	// --- [[events]] pane.created → nudge → 実 SIGUSR1 ---
	drainSignals(got)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	waitSIGUSR1(t, got, "pane.created hook → nudge")
	l := waitPluginLog(t, srv, "pane.created hook の実行監査", func(l pluginLogEntry) bool {
		return l.Event == "pane.created"
	})
	if !strings.Contains(l.Stdout, nudgedMark) {
		t.Fatalf("hook の nudge が fake pidfile（自 PID）へ向いていない: %+v", l)
	}

	// --- action invoke（実仕様: action_id + --plugin）→ nudge → 実 SIGUSR1 ---
	drainSignals(got)
	so, _ = herdrCLI(t, srv, "plugin", "action", "invoke", "nudge", "--plugin", pluginID)
	var invoked struct {
		Type string `json:"type"`
	}
	cliResult(t, so, &invoked)
	if invoked.Type != "plugin_action_invoked" {
		t.Fatalf("action invoke の応答が想定外: %s", so)
	}
	waitSIGUSR1(t, got, "action nudge → SIGUSR1")
	l = waitPluginLog(t, srv, "action nudge の実行監査", func(l pluginLogEntry) bool {
		return l.ActionID == "nudge" && l.Event == ""
	})
	if !strings.Contains(l.Stdout, nudgedMark) {
		t.Fatalf("action の nudge が fake pidfile（自 PID）へ向いていない: %+v", l)
	}

	// --- workspace close → workspace.closed hook（消滅取りこぼし穴の塞ぎが
	// 実際に効く物証）→ 実 SIGUSR1 ---
	drainSignals(got)
	if err := hc.WorkspaceClose(ws.Workspace.WorkspaceID); err != nil {
		t.Fatalf("workspace.close: %v", err)
	}
	waitSIGUSR1(t, got, "workspace.closed hook → nudge")
	l = waitPluginLog(t, srv, "workspace.closed hook の実行監査", func(l pluginLogEntry) bool {
		return l.Event == "workspace.closed"
	})
	if !strings.Contains(l.Stdout, nudgedMark) {
		t.Fatalf("workspace.closed の nudge が fake pidfile へ向いていない: %+v", l)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
