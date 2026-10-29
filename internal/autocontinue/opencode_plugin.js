// installed by herdr-drover (`herdr-drover auto install --agent opencode`)
// managed by herdr-drover; reinstalling overwrites this file. HERDR_DROVER_AUTOCONTINUE=1
//
// opencode には Stop フックが無いので、server プラグインが `session.idle` を受けて
// `herdr-drover stop-hook opencode` を呼び、続行なら `session.prompt` で投入する。
// pane の解決は hook 側（cwd → herdr agent.list）。judge・上限・ログも hook 側＝
// このファイルは「最終発話と直近の人間入力を集めて渡し、答えを投入する」だけ。
//
// ⚠ 共有 background service の中で走る（全セッションの idle が来る）。重い処理は
// しない・例外は握りつぶして opencode を止めない。
import { spawn } from "node:child_process";
import os from "node:os";
import path from "node:path";

const MARK = "[semi-auto";
const BIN =
  process.env.HERDR_DROVER_BIN ||
  "__HERDR_DROVER_BIN__" ||
  path.join(os.homedir(), ".herdr-drover", "bin", "herdr-drover");
const HOOK_TIMEOUT_MS = 55_000;

// SDK の応答は { data, error }（openapi-fetch 形）か素の値のどちらでも受ける。
function unwrap(r) {
  if (r && typeof r === "object" && ("data" in r || "error" in r)) {
    return r.error ? undefined : r.data;
  }
  return r;
}

function textOf(parts) {
  return (parts || [])
    .filter((p) => p && p.type === "text" && typeof p.text === "string")
    .map((p) => p.text)
    .join("\n");
}

export function runHook(input, bin = BIN) {
  return new Promise((resolve) => {
    let out = "";
    let err = "";
    let child;
    try {
      child = spawn(bin, ["stop-hook", "opencode"], { stdio: ["pipe", "pipe", "pipe"] });
    } catch (e) {
      resolve({ error: String(e) });
      return;
    }
    const timer = setTimeout(() => {
      try {
        child.kill("SIGKILL");
      } catch {}
    }, HOOK_TIMEOUT_MS);
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (err += d));
    child.on("error", (e) => {
      clearTimeout(timer);
      resolve({ error: String(e) });
    });
    child.on("close", () => {
      clearTimeout(timer);
      const t = out.trim();
      if (!t) {
        resolve({ stderr: err });
        return;
      }
      try {
        resolve(JSON.parse(t));
      } catch {
        resolve({ error: "bad json: " + t.slice(0, 200) });
      }
    });
    child.stdin.end(JSON.stringify(input));
  });
}

// collect は messages から「最後の assistant 発話」「直近の人間入力」「直前の入力が
// 自分の続行文か（＝stop_hook_active）」「最後の assistant が中断/エラーか」を出す。
export function collect(msgs) {
  let lastAssistant = null;
  let lastAssistantError = false;
  let lastUser = null;
  let lastHuman = null;
  for (const m of msgs || []) {
    const info = m?.info ?? m ?? {};
    const role = info.role;
    const t = textOf(m?.parts);
    if (role === "assistant") {
      if (t.trim()) {
        lastAssistant = t;
        lastAssistantError = !!info.error;
      }
    } else if (role === "user") {
      lastUser = t;
      if (!t.startsWith(MARK)) lastHuman = t;
    }
  }
  return {
    lastAssistant,
    lastAssistantError,
    stopHookActive: typeof lastUser === "string" && lastUser.startsWith(MARK),
    lastHuman,
  };
}

export const HerdrDroverAutoContinue = async ({ client, directory }) => {
  const children = new Map(); // child session → parent（子セッションの idle は無視）
  const busy = new Set();
  return {
    event: async ({ event }) => {
      const type = event?.type;
      const props = event?.properties ?? {};
      const info = props.info;
      if (info?.id && info.parentID) children.set(info.id, info.parentID);
      if (type !== "session.idle") return;
      const sid = props.sessionID;
      if (!sid || children.has(sid) || busy.has(sid)) return;
      busy.add(sid);
      try {
        const sess = unwrap(await client.session.get({ path: { id: sid } }));
        if (sess?.parentID) {
          children.set(sid, sess.parentID);
          return;
        }
        const msgs = unwrap(await client.session.messages({ path: { id: sid } })) || [];
        const c = collect(msgs);
        if (!c.lastAssistant || c.lastAssistantError) return; // 発話なし／中断は何もしない
        const out = await runHook({
          session_id: sid,
          cwd: sess?.directory || directory || "",
          hook_event_name: "Stop",
          stop_hook_active: c.stopHookActive,
          last_assistant_message: c.lastAssistant,
          last_user_message: c.lastHuman,
        });
        if (out && out.decision === "block" && typeof out.reason === "string" && out.reason) {
          // prompt はターン完了まで返らないので待たない（イベント処理を塞がない）。
          client.session
            .prompt({ path: { id: sid }, body: { parts: [{ type: "text", text: out.reason }] } })
            .catch(() => {});
        }
      } catch (e) {
        try {
          await client.app.log({
            body: { service: "herdr-drover", level: "warn", message: "autocontinue: " + String(e) },
          });
        } catch {}
      } finally {
        busy.delete(sid);
      }
    },
  };
};

export default {
  id: "herdr-drover.autocontinue",
  server: HerdrDroverAutoContinue,
  setup() {},
};
