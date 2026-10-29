package autocontinue

// opencode プラグイン（opencode_plugin.js）のロジックを実 node で検証する。
// SDK client は fake（session.get / messages / prompt を記録）、stop-hook は
// fake の sh スクリプト（stdin を記録して block を返す）。検証項目:
//   - 子セッション（parentID あり）の idle は無視
//   - messages から最終 assistant 発話・直近の人間入力・stop_hook_active を正しく組む
//   - block なら session.prompt に reason が渡る／出力なしなら prompt しない
//   - 最終 assistant がエラー（中断）なら何もしない

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpencodePluginRender(t *testing.T) {
	out := opencodeRender("/Users/x/.herdr-drover/bin/herdr-drover")
	if !strings.Contains(out, `"/Users/x/.herdr-drover/bin/herdr-drover" ||`) || strings.Contains(out, "__HERDR_DROVER_BIN__") {
		t.Fatalf("bin が埋まっていない:\n%.400s", out)
	}
	if !strings.Contains(out, opencodePluginMarker) {
		t.Fatal("マーカーが無い")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "opencode", "plugins", "herdr-drover-autocontinue.js")
	if ch, err := Install(p, "opencode", "/opt/hd"); err != nil || !ch {
		t.Fatalf("install: %t %v", ch, err)
	}
	if ch, err := Install(p, "opencode", "/opt/hd"); err != nil || ch {
		t.Fatalf("冪等: %t %v", ch, err)
	}
	if _, ok, err := Installed(p, "opencode"); !ok || err != nil {
		t.Fatalf("installed: %t %v", ok, err)
	}
	// 他人のファイルは触らない。
	other := filepath.Join(dir, "other.js")
	os.WriteFile(other, []byte("export default {}"), 0o644)
	if _, err := Install(other, "opencode", "/opt/hd"); err == nil {
		t.Fatal("他人のファイルを上書きした")
	}
	if _, err := Uninstall(other, "opencode"); err == nil {
		t.Fatal("他人のファイルを消した")
	}
	if ch, err := Uninstall(p, "opencode"); err != nil || !ch {
		t.Fatalf("uninstall: %t %v", ch, err)
	}
	if _, ok, _ := Installed(p, "opencode"); ok {
		t.Fatal("uninstall 後も installed")
	}
}

func TestOpencodePluginWithNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node が無い（プラグインの実 node 検証はスキップ）")
	}
	dir := t.TempDir()
	plugin := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(plugin, []byte(opencodeRender("/nonexistent")), 0o644); err != nil {
		t.Fatal(err)
	}
	// fake stop-hook: stdin を記録し、FAKE_DECISION に応じて出力。
	hook := filepath.Join(dir, "fakehook.sh")
	rec := filepath.Join(dir, "stdin.jsonl")
	os.WriteFile(hook, []byte("#!/bin/sh\ncat >> '"+rec+"'; echo >> '"+rec+"'\n[ \"$FAKE_DECISION\" = block ] && printf '%s\\n' '{\"decision\":\"block\",\"reason\":\"[semi-auto 1/3] つづけて\"}'\nexit 0\n"), 0o755)
	harness := filepath.Join(dir, "run.mjs")
	os.WriteFile(harness, []byte(`
import plugin, { collect } from "`+plugin+`";
const calls = { get: [], messages: [], prompt: [] };
const sessions = {
  "ses_root": { id: "ses_root", directory: "/u/proj" },
  "ses_child": { id: "ses_child", directory: "/u/proj", parentID: "ses_root" },
  "ses_abort": { id: "ses_abort", directory: "/u/proj" },
};
const messages = {
  "ses_root": [
    { info: { role: "user" }, parts: [{ type: "text", text: "改善を続けてください" }] },
    { info: { role: "assistant" }, parts: [{ type: "text", text: "やりました。" }] },
    { info: { role: "user" }, parts: [{ type: "text", text: "[semi-auto 1/3] 改善を続けてください" }] },
    { info: { role: "assistant" }, parts: [{ type: "step-start" }, { type: "text", text: "途中です。次は検証です。" }] },
  ],
  "ses_abort": [
    { info: { role: "user" }, parts: [{ type: "text", text: "x" }] },
    { info: { role: "assistant", error: { name: "MessageAbortedError" } }, parts: [{ type: "text", text: "途中" }] },
  ],
};
const client = {
  session: {
    get: async ({ path }) => { calls.get.push(path.id); return { data: sessions[path.id] }; },
    messages: async ({ path }) => { calls.messages.push(path.id); return { data: messages[path.id] || [] }; },
    prompt: async ({ path, body }) => { calls.prompt.push({ id: path.id, text: body.parts[0].text }); return { data: {} }; },
  },
  app: { log: async () => ({}) },
};
const hooks = await plugin.server({ client, directory: "/u/server" });
await hooks.event({ event: { type: "session.updated", properties: { info: { id: "ses_child", parentID: "ses_root" } } } });
await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_child" } } });
await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_abort" } } });
await hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_root" } } });
await new Promise((r) => setTimeout(r, 50));
const c = collect(messages["ses_root"]);
console.log(JSON.stringify({ calls, collect: c }));
`), 0o644)
	run := func(decision string) map[string]any {
		cmd := exec.Command(node, harness)
		cmd.Env = append(os.Environ(), "HERDR_DROVER_BIN="+hook, "FAKE_DECISION="+decision)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}
		var res map[string]any
		last := ""
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(l, "{") {
				last = l
			}
		}
		if err := json.Unmarshal([]byte(last), &res); err != nil {
			t.Fatalf("出力解釈: %v\n%s", err, out)
		}
		return res
	}
	os.Remove(rec)
	res := run("block")
	calls := res["calls"].(map[string]any)
	// 子セッションは get すら呼ばず無視、abort は messages まで見て何もしない。
	if gets := calls["get"].([]any); len(gets) != 2 || gets[0] != "ses_abort" || gets[1] != "ses_root" {
		t.Fatalf("get calls: %v", gets)
	}
	prompts := calls["prompt"].([]any)
	if len(prompts) != 1 || prompts[0].(map[string]any)["id"] != "ses_root" || prompts[0].(map[string]any)["text"] != "[semi-auto 1/3] つづけて" {
		t.Fatalf("prompt calls: %v", prompts)
	}
	col := res["collect"].(map[string]any)
	if col["lastAssistant"] != "途中です。次は検証です。" || col["stopHookActive"] != true || col["lastHuman"] != "改善を続けてください" {
		t.Fatalf("collect: %v", col)
	}
	// fake hook が受けた stdin（1 回だけ・root 分）。
	b, _ := os.ReadFile(rec)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("hook 呼出回数=%d want 1:\n%s", len(lines), b)
	}
	var in Input
	if err := json.Unmarshal([]byte(lines[0]), &in); err != nil {
		t.Fatal(err)
	}
	if in.SessionID != "ses_root" || in.Cwd != "/u/proj" || !in.StopHookActive || in.LastAssistantMessage == nil ||
		*in.LastAssistantMessage != "途中です。次は検証です。" || in.LastUserMessage == nil || *in.LastUserMessage != "改善を続けてください" {
		t.Fatalf("hook stdin: %+v", in)
	}
	// 出力なし（停止）なら prompt しない。
	os.Remove(rec)
	res = run("none")
	if n := len(res["calls"].(map[string]any)["prompt"].([]any)); n != 0 {
		t.Fatalf("停止側なのに prompt %d 回", n)
	}
}

// opencode 経路: transcript が無い時は last_user_message を直近の人間入力として使う。
func TestHookUsesLastUserMessageWithoutTranscript(t *testing.T) {
	h, st := newTestHook(t, fakeJudge("CONTINUE", nil))
	st.SaveConfig(Config{Enabled: boolp(true)})
	h.Agent = "opencode"
	in := Input{SessionID: "ses_1", Cwd: "/u/proj", LastAssistantMessage: strp("途中です"), LastUserMessage: strp("最後まで自律的にすすめて")}
	res, err := h.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/10] 最後まで自律的にすすめて" || !res.Grant {
		t.Fatalf("res=%+v out=%+v", res, res.Output)
	}
}
