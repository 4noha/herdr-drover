// installed by herdr-drover (`herdr-drover auto install --agent opencode`)
// managed by herdr-drover; reinstalling overwrites this file. HERDR_DROVER_AUTOCONTINUE=1
//
// opencode には Stop フックが無いので、server プラグインが `session.idle` を受けて
// `herdr-drover stop-hook opencode` を呼び、続行なら session.prompt で投入する。
// pane の解決は hook 側（cwd → herdr agent.list）。judge・上限・ログも hook 側＝
// このファイルは「最終発話と直近の人間入力を集めて渡し、答えを投入する」だけ。
//
// 2 系統のローダーに対応する（opencode 2.2.0 実測）:
//   - v2（2.x）: default.setup(api) の Promise API。api.event.subscribe({types,signal}) は
//     **AsyncIterable**、api.session.get({sessionID}) / api.session.prompt({sessionID,parts})。
//     messages 取得 API が無いので message.updated / message.part.updated から発話を
//     自前で追跡する。default.server は無視される。
//   - v1（1.x）: default.server(input) → hooks（client.session.messages / prompt）。
//
// ⚠ 共有 background service の中で走る（全セッションのイベントが来る）。重い処理は
// しない・例外は握りつぶして opencode を止めない。診断は自前のログファイルへ。
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const MARK = "[semi-auto";
const BIN =
  process.env.HERDR_DROVER_BIN ||
  "__HERDR_DROVER_BIN__" ||
  path.join(os.homedir(), ".herdr-drover", "bin", "herdr-drover");
const HOOK_TIMEOUT_MS = 55_000;
const MAX_MSGS_PER_SESSION = 8;
const LOG = process.env.HERDR_DROVER_AUTO_PLUGIN_LOG ||
  path.join(process.env.HERDR_DROVER_AUTO_DIR || path.join(os.homedir(), ".herdr-drover", "autocontinue"), "opencode-plugin.log");

function log(msg) {
  try {
    fs.mkdirSync(path.dirname(LOG), { recursive: true });
    fs.appendFileSync(LOG, `${new Date().toISOString()} ${msg}\n`);
  } catch {}
}

// SDK v1 の応答は { data, error }（openapi-fetch 形）か素の値のどちらでも受ける。
function unwrap(r) {
  if (r && typeof r === "object" && ("data" in r || "error" in r)) {
    return r.error ? undefined : r.data;
  }
  return r;
}

function runHook(input, bin = BIN) {
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

// summarize は時系列の [{role, error, text}] から hook への入力材料を出す。
function summarize(list) {
  let lastAssistant = null;
  let lastAssistantError = false;
  let lastUser = null;
  let lastHuman = null;
  for (const m of list) {
    if (m.role === "assistant") {
      if (m.text.trim()) {
        lastAssistant = m.text;
        lastAssistantError = !!m.error;
      }
    } else if (m.role === "user") {
      lastUser = m.text;
      if (!m.text.startsWith(MARK)) lastHuman = m.text;
    }
  }
  return {
    lastAssistant,
    lastAssistantError,
    stopHookActive: typeof lastUser === "string" && lastUser.startsWith(MARK),
    lastHuman,
  };
}

function textOfParts(parts) {
  return (parts || [])
    .filter((p) => p && p.type === "text" && typeof p.text === "string")
    .map((p) => p.text)
    .join("\n");
}

// decide は hook を呼び、続行なら reason を返す（それ以外は null）。
async function decide({ sessionID, cwd, summary }) {
  if (!summary.lastAssistant || summary.lastAssistantError) return null; // 発話なし／中断
  const out = await runHook({
    session_id: sessionID,
    cwd: cwd || "",
    hook_event_name: "Stop",
    stop_hook_active: summary.stopHookActive,
    last_assistant_message: summary.lastAssistant,
    last_user_message: summary.lastHuman,
  });
  if (out && out.error) log(`hook error sid=${sessionID}: ${out.error}`);
  if (out && out.decision === "block" && typeof out.reason === "string" && out.reason) return out.reason;
  return null;
}

// ---- v2: イベントから発話を追跡する ------------------------------------------

function makeTracker(maxMsgs = MAX_MSGS_PER_SESSION) {
  const sessions = new Map(); // sid → Map<messageID, {role, error, parts: Map<partID, text>}>
  const children = new Map(); // child sid → parent sid
  const get = (sid) => {
    let s = sessions.get(sid);
    if (!s) {
      s = new Map();
      sessions.set(sid, s);
    }
    return s;
  };
  const message = (s, id) => {
    let m = s.get(id);
    if (!m) {
      m = { role: undefined, error: false, parts: new Map() };
      s.set(id, m);
      while (s.size > maxMsgs) s.delete(s.keys().next().value);
    }
    return m;
  };
  return {
    apply(ev) {
      const t = ev?.type;
      const p = ev?.properties ?? {};
      switch (t) {
        case "session.created":
        case "session.updated": {
          const info = p.info;
          if (info?.id && info.parentID) children.set(info.id, info.parentID);
          return;
        }
        case "session.deleted": {
          const id = p.info?.id ?? p.sessionID;
          if (id) {
            sessions.delete(id);
            children.delete(id);
          }
          return;
        }
        case "message.updated": {
          const info = p.info;
          if (!info?.sessionID || !info.id) return;
          const m = message(get(info.sessionID), info.id);
          m.role = info.role;
          m.error = !!info.error;
          return;
        }
        case "message.part.updated": {
          const part = p.part;
          if (!part?.sessionID || !part.messageID || part.type !== "text") return;
          message(get(part.sessionID), part.messageID).parts.set(part.id, part.text ?? "");
          return;
        }
        case "message.removed": {
          const s = sessions.get(p.sessionID);
          if (s && p.messageID) s.delete(p.messageID);
          return;
        }
        default:
          return;
      }
    },
    isChild: (sid) => children.has(sid),
    markChild: (sid, parent) => children.set(sid, parent),
    summary(sid) {
      const s = sessions.get(sid);
      const list = [];
      if (s) for (const m of s.values()) list.push({ role: m.role, error: m.error, text: [...m.parts.values()].join("\n") });
      return summarize(list);
    },
  };
}

async function setupV2(api) {
  if (typeof api?.event?.subscribe !== "function" || typeof api?.session?.prompt !== "function") {
    log(`setup(v2): API 不足 event=${typeof api?.event?.subscribe} session.prompt=${typeof api?.session?.prompt}`);
    return;
  }
  const ac = new AbortController();
  const tracker = makeTracker();
  const busy = new Set();
  const TYPES = [
    "session.created", "session.updated", "session.deleted", "session.idle",
    "message.updated", "message.part.updated", "message.removed",
  ];
  const onIdle = async (sid) => {
    if (!sid || tracker.isChild(sid) || busy.has(sid)) return;
    busy.add(sid);
    try {
      let sess;
      try {
        sess = unwrap(await api.session.get({ sessionID: sid }));
      } catch (e) {
        log(`session.get 失敗 sid=${sid}: ${e}`);
      }
      if (sess?.parentID) {
        tracker.markChild(sid, sess.parentID);
        return;
      }
      const reason = await decide({ sessionID: sid, cwd: sess?.directory, summary: tracker.summary(sid) });
      if (reason) {
        // prompt はターン完了まで返らないので待たない（イベント処理を塞がない）。
        Promise.resolve(api.session.prompt({ sessionID: sid, parts: [{ type: "text", text: reason }] })).catch((e) =>
          log(`session.prompt 失敗 sid=${sid}: ${e}`),
        );
      }
    } catch (e) {
      log(`idle 処理失敗 sid=${sid}: ${e}`);
    } finally {
      busy.delete(sid);
    }
  };
  (async () => {
    let events;
    try {
      events = api.event.subscribe({ types: TYPES, signal: ac.signal });
      if (events && typeof events.then === "function") events = await events;
    } catch (e) {
      log(`event.subscribe 失敗: ${e}`);
      return;
    }
    if (!events || typeof events[Symbol.asyncIterator] !== "function") {
      log(`event.subscribe の戻りが AsyncIterable でない: ${typeof events}`);
      return;
    }
    log("setup(v2): event loop 開始");
    try {
      for await (const ev of events) {
        tracker.apply(ev);
        if (ev?.type === "session.idle") onIdle(ev.properties?.sessionID);
      }
    } catch (e) {
      if (!ac.signal.aborted) log(`event loop 終了: ${e}`);
    }
  })();
  return () => ac.abort();
}

// ---- v1: hooks オブジェクト（client.session.messages が使える） -----------------

const serverV1 = async ({ client, directory }) => {
  const children = new Map();
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
        const list = msgs.map((m) => ({ role: m?.info?.role ?? m?.role, error: !!m?.info?.error, text: textOfParts(m?.parts) }));
        const reason = await decide({ sessionID: sid, cwd: sess?.directory || directory, summary: summarize(list) });
        if (reason) {
          client.session
            .prompt({ path: { id: sid }, body: { parts: [{ type: "text", text: reason }] } })
            .catch((e) => log(`session.prompt 失敗 sid=${sid}: ${e}`));
        }
      } catch (e) {
        log(`idle 処理失敗(v1) sid=${sid}: ${e}`);
      } finally {
        busy.delete(sid);
      }
    },
  };
};

export default {
  id: "herdr-drover.autocontinue",
  server: serverV1,
  setup: setupV2,
};
