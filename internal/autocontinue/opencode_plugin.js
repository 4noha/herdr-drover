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
      // v2 のバス（実測 2.2.0）は {type, location:{directory}, data:{…}}、SDK 型は
      // {type, properties:{…}}。どちらでも受ける。
      const p = ev?.properties ?? ev?.data ?? {};
      switch (t) {
        case "session.created":
        case "session.updated": {
          const info = p.info;
          if (info?.id && info.parentID) children.set(info.id, info.parentID);
          if (p.sessionID && p.parentID) children.set(p.sessionID, p.parentID); // 実バス形
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
        // ---- opencode 2.2.0 の内部バス（実測）。SDK 形の message.* は来ない。
        case "session.inbox.enqueued": {
          // data {sessionID, inboxID, item:{type:"user", payload:{text,...}}}
          const item = p.item;
          if (!p.sessionID || item?.type !== "user") return;
          const m = message(get(p.sessionID), "inbox:" + (p.inboxID || Math.random()));
          m.role = "user";
          m.parts.set("text", String(item.payload?.text ?? ""));
          return;
        }
        case "session.step.started": {
          // data {sessionID, assistantMessageID, ...} → この assistant 発話を現在位置に
          if (!p.sessionID || !p.assistantMessageID) return;
          message(get(p.sessionID), p.assistantMessageID).role = "assistant";
          return;
        }
        case "session.text.ended": {
          // data {sessionID, assistantMessageID, ordinal, text}（text ブロックの全文）
          if (!p.sessionID || !p.assistantMessageID) return;
          const m = message(get(p.sessionID), p.assistantMessageID);
          m.role = "assistant";
          m.parts.set(String(p.ordinal ?? m.parts.size), String(p.text ?? ""));
          return;
        }
        case "session.execution.failed":
        case "session.execution.interrupted": {
          // 失敗／中断（Esc 等）は最後の assistant をエラー扱い＝続行判定しない
          const s = sessions.get(p.sessionID);
          if (!s) return;
          for (const m of [...s.values()].reverse()) {
            if (m.role === "assistant") {
              m.error = true;
              break;
            }
          }
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
      if (s)
        for (const m of s.values()) {
          const keys = [...m.parts.keys()].sort((a, b) => (Number(a) || 0) - (Number(b) || 0));
          list.push({ role: m.role, error: m.error, text: keys.map((k) => m.parts.get(k)).join("\n") });
        }
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
    // SDK 形（session.idle + message.*）と 2.2.0 実バス形（session.execution.* など）の両方
    "session.created", "session.updated", "session.deleted", "session.idle",
    "message.updated", "message.part.updated", "message.removed",
    "session.inbox.enqueued", "session.step.started", "session.text.ended",
    "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted",
  ];
  const TURN_END = new Set(["session.idle", "session.execution.succeeded"]);
  const promptSession = async (sid, text) => {
    // 2.2.0: /api/session/{id}/prompt = {prompt:{text}, delivery}。古い形 {parts} へフォールバック。
    try {
      await api.session.prompt({ sessionID: sid, prompt: { text }, delivery: "queue" });
    } catch (e1) {
      try {
        await api.session.prompt({ sessionID: sid, parts: [{ type: "text", text }] });
      } catch (e2) {
        log(`session.prompt 失敗 sid=${sid}: ${e1} / ${e2}`);
      }
    }
  };
  const onIdle = async (sid, locDir) => {
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
      const reason = await decide({ sessionID: sid, cwd: sess?.directory || locDir, summary: tracker.summary(sid) });
      if (reason) {
        // prompt はターン完了まで返ることがあるので待たない（イベント処理を塞がない）。
        void promptSession(sid, reason);
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
      // types フィルタは付けない（フィルタの形がバージョンで違っても取りこぼさない）。
      // 全イベントを受けてここで絞る。
      events = api.event.subscribe({ signal: ac.signal });
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
    const seenTypes = new Set();
    try {
      for await (const raw of events) {
        // {type, properties} / {type, data} / {event:{…}} のどれでも受ける。
        const ev = raw && typeof raw === "object" && !raw.type && raw.event ? raw.event : raw;
        if (ev?.type && !seenTypes.has(ev.type)) {
          // 初出の型だけ生データを残す（型名とキーの実測＝将来の差分検知用）。
          seenTypes.add(ev.type);
          log(`event type=${ev.type} ${JSON.stringify(raw).slice(0, 240)}`);
        }
        if (!ev || !TYPES.includes(ev.type)) continue;
        tracker.apply(ev);
        if (TURN_END.has(ev.type)) {
          const p = ev.properties ?? ev.data ?? {};
          const sid = p.sessionID ?? p.id;
          const sm = tracker.summary(sid);
          log(`${ev.type} sid=${sid} child=${tracker.isChild(sid)} assistant_len=${(sm.lastAssistant || "").length} active=${sm.stopHookActive} err=${sm.lastAssistantError}`);
          onIdle(sid, ev.location?.directory);
        }
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
