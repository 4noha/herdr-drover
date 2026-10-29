package main

// auto（セミオート続行）の CLI と、通知抑止（push / webhook が autocontinue の
// マークを見て working→idle の確定を保留する）を検証する。
// TestLiveClaudeStopHook は実 Claude Code（claude -p・ユーザーの認証）を使う
// opt-in（HERDR_DROVER_LIVE_CLAUDE=1）。それ以外は実 claude を起動しない。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/webhook"
	"github.com/4noha/herdr-drover/internal/autocontinue"
)

// push: 抑止中は working→idle を確定せず（通知なし・prev は working のまま）、
// マークが消えた後の tick で通常どおり 1 回だけ通知する。抑止が解けずに
// working へ戻れば（自動続行）何も飛ばない。
func TestTaskNotifierDefersWhileSuppressed(t *testing.T) {
	ts, sendCount, _ := fakeFCM(t, nil)
	tn := newTestNotifier(ts)
	store := &fakePushStore{tokens: []string{"tok-1"}}
	ctx := context.Background()
	sup := map[string]bool{}
	tn.suppressed = func(key string) bool { return sup[key] }

	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "claude")})
	// Stop フック判定中（pending マーク）に idle を観測 → 保留。
	sup["w1:p1"] = true
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	if sendCount["tok-1"] != 0 {
		t.Fatalf("抑止中に通知: %d", sendCount["tok-1"])
	}
	if tn.prev["w1:p1"] != "working" {
		t.Fatalf("保留中は prev=working のはず: %q", tn.prev["w1:p1"])
	}
	// 自動続行＝working に戻る → 遷移なし。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "claude")})
	sup["w1:p1"] = false
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "claude")})
	if sendCount["tok-1"] != 0 {
		t.Fatalf("自動続行で通知: %d", sendCount["tok-1"])
	}
	// 本当に止まった（フックが停止側＝マーク消滅）→ 通常どおり 1 回。
	sup["w1:p1"] = true
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	sup["w1:p1"] = false
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("マーク消滅後に通知 1 回のはず: %d", sendCount["tok-1"])
	}
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("idle 維持で再通知: %d", sendCount["tok-1"])
	}
}

// webhook も同じ規律（実 https 受信で確認）。
func TestWebhookNotifierDefersWhileSuppressed(t *testing.T) {
	wr := newWebhookReceiver(t)
	wn := newWebhookNotifier("mac-studio-herdr", log.New(io.Discard, "", 0))
	wn.cachedCfg = webhook.Config{Enabled: true, URL: wr.srv.URL}
	wn.cachedAt = time.Now()
	ctx := context.Background()
	store := &fakeWebhookStore{cfg: wn.cachedCfg}
	sup := map[string]bool{}
	wn.suppressed = func(key string) bool { return sup[key] }

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "working", "claude", "proj")})
	sup["w1:p1"] = true
	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "idle", "claude", "proj")})
	wr.assertStableCount(t, 0)
	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "working", "claude", "proj")})
	sup["w1:p1"] = false
	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "working", "claude", "proj")})
	wr.assertStableCount(t, 0)
	sup["w1:p1"] = true
	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "done", "claude", "proj")})
	wr.assertStableCount(t, 0)
	sup["w1:p1"] = false
	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "done", "claude", "proj")})
	wr.waitCount(t, 1)
	if p := wr.last(); p.OldStatus != "working" || p.NewStatus != "done" {
		t.Fatalf("payload: %+v", p)
	}
}

// 実マークファイル経由（SuppressFunc）でも同じ: フックが pending を書く→保留、
// 消す→確定。
func TestSuppressFuncWithRealMarks(t *testing.T) {
	dir := t.TempDir()
	st := &autocontinue.Store{Dir: dir}
	ts, sendCount, _ := fakeFCM(t, nil)
	tn := newTestNotifier(ts)
	tn.suppressed = autocontinue.SuppressFunc(dir, time.Now)
	store := &fakePushStore{tokens: []string{"tok-1"}}
	ctx := context.Background()
	tn.check(ctx, store, []map[string]any{sess("w1:p4F", "working", "claude")})
	st.SetMark("w1:p4F", autocontinue.Mark{At: time.Now(), Pending: true})
	tn.check(ctx, store, []map[string]any{sess("w1:p4F", "idle", "claude")})
	if sendCount["tok-1"] != 0 {
		t.Fatal("pending マーク中に通知")
	}
	st.ClearMark("w1:p4F")
	tn.check(ctx, store, []map[string]any{sess("w1:p4F", "idle", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("マーク消滅後に通知 1 回のはず: %d", sendCount["tok-1"])
	}
}

func TestAutoCLI(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	dir := filepath.Join(home, "auto")
	t.Setenv(autocontinue.DirEnv, dir)
	t.Setenv("HERDR_PANE_ID", "w1:p4F")
	runOK := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, code, errb.String())
		}
		return out.String()
	}
	// 既存 settings.json（他フックあり）を用意して install。
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"x"}]}]},"model":"opus"}`), 0o600)
	out := runOK("auto", "install", "--bin", "/opt/hd/herdr-drover")
	if !strings.Contains(out, "claude の Stop フックを") || !strings.Contains(out, "codex の Stop フックを") || !strings.Contains(out, "/hooks") {
		t.Fatalf("install 出力: %s", out)
	}
	for _, a := range []string{"claude", "codex"} {
		p, _ := autocontinue.SettingsPath(home, a)
		if _, ok, err := autocontinue.Installed(p, a); !ok || err != nil {
			t.Fatalf("%s 未設置 err=%v", a, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(b), `"model": "opus"`) || !strings.Contains(string(b), `"SessionStart"`) {
		t.Fatalf("既存設定が壊れた: %s", b)
	}
	if out := runOK("auto", "install", "--bin", "/opt/hd/herdr-drover"); !strings.Contains(out, "設置済み") {
		t.Fatalf("冪等 install: %s", out)
	}
	// on（今いる pane）→ pane ファイル。status に出る。
	out = runOK("auto", "on", "--phrase", "改善を続けてください")
	if !strings.Contains(out, "pane w1:p4F を enabled=true") {
		t.Fatalf("on: %s", out)
	}
	st := &autocontinue.Store{Dir: dir}
	p, found, _ := st.LoadPane("w1:p4F")
	if !found || p.Enabled == nil || !*p.Enabled || p.Phrase != "改善を続けてください" {
		t.Fatalf("pane 設定: found=%t %+v", found, p)
	}
	out = runOK("auto", "status")
	for _, want := range []string{"w1:p4F", "enabled=true", "改善を続けてください", "設置済", "この pane (w1:p4F): enabled=true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status に %q が無い:\n%s", want, out)
		}
	}
	// --all --grant-cap。
	runOK("auto", "on", "--all", "--cap", "2", "--grant-cap", "7")
	cfg, _ := st.LoadConfig()
	if cfg.Enabled == nil || !*cfg.Enabled || cfg.Cap != 2 || cfg.GrantCap != 7 {
		t.Fatalf("config: %+v", cfg)
	}
	// off / 明示 pane。
	runOK("auto", "off", "--pane", "w2:p1", "--grant")
	p, found, _ = st.LoadPane("w2:p1")
	if !found || *p.Enabled || !p.Grant {
		t.Fatalf("off pane: %+v", p)
	}
	// uninstall → 両方除去・他は温存。
	out = runOK("auto", "uninstall")
	if !strings.Contains(out, "claude の Stop フックを") || !strings.Contains(out, "除去") {
		t.Fatalf("uninstall: %s", out)
	}
	b, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if strings.Contains(string(b), "stop-hook") || !strings.Contains(string(b), `"SessionStart"`) {
		t.Fatalf("uninstall 後: %s", b)
	}
	// toggle: pane 設定を反転（herdr が無い＝通知は失敗報告のみ・切替は成功）。
	t.Setenv("HERDR_PANE_ID", "w1:p4F")
	t.Setenv("HERDR_SOCKET_PATH", filepath.Join(home, "no-such.sock"))
	st.SavePane("w1:p4F", autocontinue.PaneSetting{Enabled: boolPtr(true)})
	out = runOK("auto", "toggle")
	if !strings.Contains(out, "semi-auto OFF: w1:p4F") || !strings.Contains(out, "通知に失敗") {
		t.Fatalf("toggle→OFF: %s", out)
	}
	if p, _, _ := st.LoadPane("w1:p4F"); p.Enabled == nil || *p.Enabled {
		t.Fatalf("toggle 後も enabled: %+v", p)
	}
	// pane 設定が無い pane は全体既定（上で --all on 済み）から反転＝OFF、もう一度で ON。
	out = runOK("auto", "toggle", "--pane", "w7:p1")
	if !strings.Contains(out, "semi-auto OFF: w7:p1") {
		t.Fatalf("toggle（全体 on の pane）→OFF: %s", out)
	}
	out = runOK("auto", "toggle", "--pane", "w7:p1")
	if !strings.Contains(out, "semi-auto ON: w7:p1") || !strings.Contains(out, "Stop フック未設置") {
		t.Fatalf("toggle→ON（フック除去後なので未設置警告が要る）: %s", out)
	}
	b, _ = os.ReadFile(st.LogPath())
	if !strings.Contains(string(b), "toggle pane=w7:p1 → ON") {
		t.Fatalf("toggle がログに無い: %s", b)
	}
	// 使い方エラーは exit 2。
	var out2, errb bytes.Buffer
	if code := run([]string{"auto"}, &out2, &errb); code != 2 {
		t.Fatalf("auto 引数なし exit=%d", code)
	}
	if code := run([]string{"auto", "on", "--all", "extra"}, &out2, &errb); code != 2 {
		t.Fatalf("余分な引数 exit=%d", code)
	}
	t.Setenv("HERDR_PANE_ID", "")
	if code := run([]string{"auto", "on"}, &out2, &errb); code != 2 || !strings.Contains(errb.String(), "pane が分からない") {
		t.Fatalf("pane 不明 exit=%d stderr=%s", code, errb.String())
	}
	// stop-hook は引数不正でも exit 0（エージェントを邪魔しない）。
	if code := run([]string{"stop-hook"}, &out2, &errb); code != 0 {
		t.Fatalf("stop-hook 引数なし exit=%d", code)
	}
}

// 実 Claude Code で Stop フック契約を通す（opt-in: HERDR_DROVER_LIVE_CLAUDE=1）。
// ビルドした herdr-drover 本体をフックに据え、判定器は fake（sh）にして、
// `claude -p --settings <隔離 settings>` が (1) 1 回目の停止で block→続行、
// (2) 2 回目（stop_hook_active=true）で上限（cap=1）→停止、となることを
// transcript とログで確認する。2026-10-29 実測で同じ経路が通ることを確認済み。
func TestLiveClaudeStopHook(t *testing.T) {
	if os.Getenv("HERDR_DROVER_LIVE_CLAUDE") != "1" {
		t.Skip("実 Claude Code を使う live テスト（HERDR_DROVER_LIVE_CLAUDE=1 で実行）")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude が PATH に無い")
	}
	work := t.TempDir()
	bin := filepath.Join(work, "herdr-drover")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	dir := filepath.Join(work, "auto")
	st := &autocontinue.Store{Dir: dir}
	on := true
	st.SaveConfig(autocontinue.Config{Enabled: &on, Cap: 1, JudgeCmd: []string{"sh", "-c", "echo CONTINUE"}})
	settings := filepath.Join(work, "settings.json")
	sj, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": autocontinue.HookCommand(bin, "claude"), "timeout": 30}}}}}})
	os.WriteFile(settings, sj, 0o600)

	cmd := exec.Command(claude, "-p", "単語 PART1 だけを答えて止まってください。", "--model", "haiku",
		"--settings", settings, "--output-format", "json")
	cmd.Dir = work
	cmd.Stdin = strings.NewReader("")
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDECODE") || strings.HasPrefix(kv, "CLAUDE_CODE_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, autocontinue.DirEnv+"="+dir, "HERDR_PANE_ID=wL:p1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("claude -p: %v", err)
	}
	var res struct {
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output: %v %s", err, out)
	}
	b, _ := os.ReadFile(st.LogPath())
	lg := string(b)
	if !strings.Contains(lg, "active=false count=0/1") || !strings.Contains(lg, "→続行 \"[semi-auto 1/1] つづけて\"") ||
		!strings.Contains(lg, "active=true count=1/1") || !strings.Contains(lg, "上限到達→停止") {
		t.Fatalf("autocontinue.log が期待の 2 行でない:\n%s\nresult=%q", lg, res.Result)
	}
	if ok, _ := st.Suppressed("wL:p1", time.Now()); ok {
		t.Fatal("停止後にマークが残っている")
	}
	t.Logf("result=%q session=%s\n%s", res.Result, res.SessionID, lg)
}

func boolPtr(b bool) *bool { return &b }
