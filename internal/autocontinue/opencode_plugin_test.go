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
import plugin from "`+plugin+`";
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
const mode = process.env.MODE;
const calls = { get: [], messages: [], prompt: [] };
if (mode === "v1") {
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
} else {
  // v2: event.subscribe は AsyncIterable。messages はイベントから組み立てさせる。
  const queue = [];
  let wake = null;
  let closed = false;
  const push = (ev) => { queue.push(ev); if (wake) { const w = wake; wake = null; w(); } };
  const iterable = {
    [Symbol.asyncIterator]() {
      return { next: async () => {
        while (queue.length === 0 && !closed) await new Promise((r) => (wake = r));
        if (queue.length === 0) return { done: true, value: undefined };
        return { done: false, value: queue.shift() };
      } };
    },
  };
  const api = {
    event: { subscribe: ({ types, signal }) => { calls.types = types; signal?.addEventListener("abort", () => { closed = true; push(undefined); }); return iterable; } },
    session: {
      get: async ({ sessionID }) => { calls.get.push(sessionID); const { directory, ...rest } = sessions[sessionID]; return rest; },
      // 2.2.0 の形 {sessionID, text, delivery} だけ受け、他の形は SchemaError 風に拒否する。
      prompt: async ({ sessionID, text, delivery, prompt, parts }) => { if (typeof text !== "string" || prompt || parts) throw new Error("SchemaError(Missing key\n  text)"); calls.prompt.push({ id: sessionID, text, delivery }); return {}; },
    },
  };
  const dispose = await plugin.setup(api);
  // 共有 service はプロジェクトごとに setup を重ねて呼ぶ（実測 3 回）。2 本目はループを張らない。
  const dispose2 = await plugin.setup({ ...api, event: { subscribe: () => { throw new Error("second loop must not subscribe"); } } });
  // opencode 2.2.0 実測のバス形: {id, created, type, location:{directory}, data:{…}}。
  // 発話は session.inbox.enqueued（user）/ session.step.started + session.text.ended（assistant）、
  // ターン終了は session.execution.succeeded、中断は session.execution.interrupted。
  // execution.* には location が付かない（実測）。session.get も directory を返さない前提で、
  // 他イベントの location から cwd が引けることを検証する。
  const bus = (type, data) => ({ id: "evt_" + Math.random(), created: Date.now(), type, ...(type.startsWith("session.execution.") ? {} : { location: { directory: "/u/proj" } }), data });
  let n = 0;
  const feed = (sid, list) => {
    for (const m of list) {
      const id = "msg_" + (++n);
      if (m.info.role === "user") {
        push(bus("session.inbox.enqueued", { sessionID: sid, inboxID: "inb_" + n, item: { type: "user", payload: { text: m.parts.map((p) => p.text || "").join("") }, delivery: "queue" } }));
      } else {
        push(bus("session.step.started", { sessionID: sid, assistantMessageID: id }));
        let k = 0;
        for (const p of m.parts) if (p.type === "text") push(bus("session.text.ended", { sessionID: sid, assistantMessageID: id, ordinal: k++, text: p.text }));
        push(bus(m.info.error ? "session.execution.interrupted" : "session.step.ended", { sessionID: sid, assistantMessageID: id, ...(m.info.error ? { reason: "user" } : {}) }));
      }
    }
  };
  push(bus("integration.updated", {}));
  push(bus("session.created", { sessionID: "ses_child", parentID: "ses_root" }));
  feed("ses_root", messages["ses_root"]);
  feed("ses_abort", messages["ses_abort"]);
  push(bus("session.execution.succeeded", { sessionID: "ses_child" }));
  push(bus("session.execution.succeeded", { sessionID: "ses_abort" }));
  push(bus("session.execution.succeeded", { sessionID: "ses_root" }));
  await new Promise((r) => setTimeout(r, 300));
  dispose2();
  dispose();
  // 1 本目を dispose した後は新しい setup がループを張れる（subscribe が呼ばれる）。
  let resubscribed = false;
  const d3 = await plugin.setup({ ...api, event: { subscribe: () => { resubscribed = true; return iterable; } } });
  await new Promise((r) => setTimeout(r, 20));
  calls.resubscribed = resubscribed;
  d3();
}
await new Promise((r) => setTimeout(r, 100));
console.log(JSON.stringify({ calls }));
`), 0o644)
	run := func(mode, decision string) map[string]any {
		cmd := exec.Command(node, harness)
		cmd.Env = append(os.Environ(), "HERDR_DROVER_BIN="+hook, "FAKE_DECISION="+decision, "MODE="+mode,
			"HERDR_DROVER_AUTO_PLUGIN_LOG="+filepath.Join(dir, "plugin.log"))
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
	for _, mode := range []string{"v1", "v2"} {
		os.Remove(rec)
		res := run(mode, "block")
		calls := res["calls"].(map[string]any)
		// 子セッションは get すら呼ばず無視、abort は見て何もしない、root は prompt。
		gets := calls["get"].([]any)
		if len(gets) != 2 || gets[0] != "ses_abort" || gets[1] != "ses_root" {
			t.Fatalf("[%s] get calls: %v", mode, gets)
		}
		prompts := calls["prompt"].([]any)
		if len(prompts) != 1 || prompts[0].(map[string]any)["id"] != "ses_root" || prompts[0].(map[string]any)["text"] != "[semi-auto 1/3] つづけて" {
			t.Fatalf("[%s] prompt calls: %v", mode, prompts)
		}
		if mode == "v2" {
			if calls["resubscribed"] != true {
				t.Fatalf("[v2] dispose 後の再 setup でループが張られていない: %v", calls["resubscribed"])
			}
			b, _ := os.ReadFile(filepath.Join(dir, "plugin.log"))
			if !strings.Contains(string(b), "event loop 開始") || !strings.Contains(string(b), "session.execution.succeeded sid=ses_root") {
				t.Fatalf("[v2] プラグインログ:\n%s", b)
			}
		}
		// fake hook が受けた stdin（1 回だけ・root 分）。
		b, _ := os.ReadFile(rec)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != 1 {
			t.Fatalf("[%s] hook 呼出回数=%d want 1:\n%s", mode, len(lines), b)
		}
		var in Input
		if err := json.Unmarshal([]byte(lines[0]), &in); err != nil {
			t.Fatal(err)
		}
		if in.SessionID != "ses_root" || in.Cwd != "/u/proj" || !in.StopHookActive || in.LastAssistantMessage == nil ||
			*in.LastAssistantMessage != "途中です。次は検証です。" || in.LastUserMessage == nil || *in.LastUserMessage != "改善を続けてください" {
			t.Fatalf("[%s] hook stdin: %+v", mode, in)
		}
		// 出力なし（停止）なら prompt しない。
		os.Remove(rec)
		res = run(mode, "none")
		if n := len(res["calls"].(map[string]any)["prompt"].([]any)); n != 0 {
			t.Fatalf("[%s] 停止側なのに prompt %d 回", mode, n)
		}
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
