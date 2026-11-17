#!/usr/bin/env python3
"""live-claude-pane.py — 隔離 herdr server（短い /tmp socket・XDG_CONFIG_HOME 隔離）に
**実 Claude Code** の pane を立てる開発用ハーネス。

  python3 scripts/live-claude-pane.py hold   # 準備できたら隣に live_pane.json を書いて待つ
  DROVER_LIVE_PANE_INFO=scripts/live_pane.json go test ./cmd/herdr-drover -run TestLiveSendInput -v
  （終わったら SIGTERM＝自分が spawn した server を stop・pane の claude だけ kill）

引数なしで実行すると、CLI 経由（send-text → send-keys enter）の送信成否を自前で測る
旧モード（2026-11-17 の調査で使った。判定は Claude Code の transcript）。
実 Claude Code を起動して API を使うので CI では走らせない。信頼ダイアログ（既定が
「No, exit」）は Down→Enter で抜ける。裸の pkill herdr は使わない（CLAUDE.md）。
"""
import json, os, subprocess, sys, time, tempfile, glob, signal, random, string

BASE = tempfile.mkdtemp(prefix="hd", dir="/tmp")          # short path (sun_path 104B)
SOCK = os.path.join(BASE, "h.sock"); XDG = os.path.join(BASE, "xdg"); WORK = os.path.join(BASE, "work")
os.makedirs(XDG); os.makedirs(WORK)
open(os.path.join(WORK, "README.md"), "w").write("# scratch for input test\n")
ENV = dict(os.environ, HERDR_SOCKET_PATH=SOCK, XDG_CONFIG_HOME=XDG)
CLAUDE = "/Users/4noha/.local/bin/claude"
LOG = open(os.path.join(BASE, "repro.log"), "a")
def log(*a):
    s = time.strftime("%H:%M:%S ") + " ".join(str(x) for x in a); print(s, flush=True); LOG.write(s + "\n"); LOG.flush()

def herdr(*args, check=True):
    r = subprocess.run(["herdr", *args], env=ENV, capture_output=True, text=True)
    if r.returncode != 0 and check:
        raise RuntimeError(f"herdr {' '.join(args)} failed: {r.stderr.strip()} {r.stdout.strip()[:300]}")
    try: return json.loads(r.stdout)
    except Exception: return {"raw": r.stdout, "err": r.stderr}

SERVLOG = open(os.path.join(BASE, "server.log"), "w")
server = subprocess.Popen(["herdr", "server"], env=ENV, stdout=SERVLOG, stderr=SERVLOG)
log("server pid", server.pid, "base", BASE)
pane_pid = None
def cleanup(*_):
    try:
        if pane_pid: os.kill(pane_pid, signal.SIGTERM)
    except Exception: pass
    try: subprocess.run(["herdr", "server", "stop"], env=ENV, capture_output=True, timeout=10)
    except Exception: pass
    try: server.wait(timeout=10)
    except Exception:
        server.kill()
    log("cleanup done (server stopped, pid", server.pid, ")")
signal.signal(signal.SIGTERM, lambda *a: (cleanup(), sys.exit(1)))
try:
    for _ in range(120):
        if os.path.exists(SOCK):
            try: herdr("workspace", "list"); break
            except Exception as e: last = str(e)[:200]
        time.sleep(0.25)
    else:
        log("server log:", open(os.path.join(BASE, "server.log")).read()[-800:]); raise SystemExit("server not up: " + str(last))
    log("server up")
    ws = herdr("workspace", "create", "--cwd", WORK)
    log("workspace:", json.dumps(ws.get("result", ws), ensure_ascii=False)[:300])
    pl = herdr("pane", "list")["result"]["panes"]
    log("panes:", [(p["pane_id"], p.get("cwd")) for p in pl])
    pane = pl[-1]["pane_id"] if pl else None
    if pane is None: raise SystemExit("no pane after workspace create")
    time.sleep(1.5)  # let the shell come up
    herdr("pane", "run", pane, CLAUDE)   # real binary by full path (bypasses alias/shim)
    log("pane", pane, "-> claude launched")
    time.sleep(4)
    try:
        pi = herdr("pane", "process-info", "--pane", pane)
        log("process-info:", json.dumps(pi.get("result", pi), ensure_ascii=False)[:400])
        res = pi.get("result", {}).get("process_info", {})
        fps = res.get("foreground_processes") or []
        pane_pid = fps[0].get("pid") if fps else None
    except Exception as e: log("process-info:", str(e)[:160])
    log("pane_pid", pane_pid)

    def screen(lines=45):
        r = subprocess.run(["herdr", "pane", "read", pane, "--source", "visible", "--lines", str(lines), "--format", "text"], env=ENV, capture_output=True, text=True)
        out = r.stdout
        try:
            d = json.loads(out); res = d.get("result", {}); rd = res.get("read", res)
            return rd.get("text") or out
        except Exception:
            return out
    def status():
        try: return herdr("pane", "get", pane)["result"]["pane"].get("agent_status")
        except Exception: return "?"
    def tail_lines(s, n):
        ls = [l for l in s.splitlines() if l.strip()]
        return "\n".join(ls[-n:])
    launched = 1
    for i in range(120):
        s = screen(); t = tail_lines(s, 6)
        if "Enter to confirm" in t:
            log("trust dialog -> Down, Enter"); herdr("pane", "send-keys", pane, "Down"); time.sleep(0.5); herdr("pane", "send-keys", pane, "enter"); time.sleep(3); continue
        if t.rstrip().endswith("%"):
            if launched >= 3: log("screen:\n" + s[-1500:]); raise SystemExit("claude keeps exiting")
            launched += 1; log("claude not running (shell prompt) -> relaunch"); herdr("pane", "run", pane, CLAUDE); time.sleep(5); continue
        st = status()
        if st in ("idle", "done") or "shortcuts" in tail_lines(s, 12):
            break
        time.sleep(1)
    time.sleep(3)
    log("ready. status=", status()); log("screen tail:\n" + "\n".join(screen().splitlines()[-12:]))

    proj = os.path.join(os.path.expanduser("~/.claude/projects"), os.path.realpath(WORK).replace("/", "-"))
    log("transcript dir:", proj)
    def user_msgs_with(nonce):
        n = 0
        for f in glob.glob(os.path.join(proj, "*.jsonl")):
            for line in open(f, encoding="utf-8", errors="replace"):
                if nonce not in line: continue
                try: d = json.loads(line)
                except Exception: continue
                if d.get("type") != "user": continue
                c = d.get("message", {}).get("content")
                txt = c if isinstance(c, str) else " ".join(x.get("text", "") for x in c if isinstance(x, dict) and x.get("type") == "text")
                if nonce in txt: n += 1
        return n
    def assistant_after(nonce, since):
        for f in glob.glob(os.path.join(proj, "*.jsonl")):
            if os.path.getmtime(f) < since - 1: continue
            for line in open(f, encoding="utf-8", errors="replace"):
                if '"type":"assistant"' in line and nonce in line: return True
        return False

    if sys.argv[1:] == ["hold"]:
        info = dict(sock=SOCK, xdg=XDG, pane=pane, work=os.path.realpath(WORK), proj=proj, pane_pid=pane_pid, server_pid=server.pid)
        json.dump(info, open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "live_pane.json"), "w"))
        log("HOLD: live pane ready", json.dumps(info)); 
        while True: time.sleep(5)
    results = []
    def trial(kind, text, gap_ms, retry_enter_after=None):
        nonce = "ZQ" + "".join(random.choices(string.ascii_uppercase + string.digits, k=6))
        body = text.replace("NONCE", nonce)
        t0 = time.time()
        herdr("pane", "send-text", pane, body)
        if gap_ms: time.sleep(gap_ms / 1000)
        herdr("pane", "send-keys", pane, "enter")
        submitted = False; sub_at = None
        for _ in range(40):  # up to 10 s
            if user_msgs_with(nonce) > 0: submitted = True; sub_at = round(time.time() - t0, 2); break
            time.sleep(0.25)
        retried = None
        if not submitted and retry_enter_after is not None:
            time.sleep(retry_enter_after / 1000)
            herdr("pane", "send-keys", pane, "enter")
            for _ in range(40):
                if user_msgs_with(nonce) > 0: retried = "retry-enter OK"; submitted = True; break
                time.sleep(0.25)
            if retried is None: retried = "retry-enter FAILED"
        scr = screen()
        tail = "\n".join(scr.splitlines()[-8:])
        if submitted:
            # wait for assistant reply / idle before next trial
            for _ in range(240):
                if assistant_after(nonce, t0) and status() != "working": break
                time.sleep(0.5)
            time.sleep(1.0)
        else:
            # clear composer: one Ctrl-C clears the input in Claude Code
            herdr("pane", "send-keys", pane, "C-c"); time.sleep(1.5)
            s2 = screen(); 
            if nonce in s2 or "Pasted text" in s2:
                log("composer not cleared; sending C-u"); herdr("pane", "send-keys", pane, "C-u"); time.sleep(1.0)
        rec = dict(kind=kind, bytes=len(body.encode()), gap_ms=gap_ms, submitted=submitted, submit_s=sub_at, retry=retried, status=status())
        results.append(rec); log("TRIAL", json.dumps(rec, ensure_ascii=False)); log("  screen tail:\n" + tail)
        return submitted

    short = "Reply with exactly this token and nothing else: NONCE"
    big_lines = ["Reply with exactly this token and nothing else: NONCE", "", "Context (ignore, do not act on it):"] + [f"line {i:03d} " + "x" * 60 for i in range(22)]
    big = "\n".join(big_lines)          # ~1.5KB multi-line, like Grok Bot's messages
    mid = "Reply with exactly this token and nothing else: NONCE\n\nContext (ignore):\n" + "\n".join(f"line {i} " + "y" * 40 for i in range(3))
    for i in range(3): trial("short gap0", short, 0, retry_enter_after=800)
    for i in range(2): trial("mid<800 ml gap0", mid, 0, retry_enter_after=800)
    for i in range(3): trial("big gap0", big, 0, retry_enter_after=800)
    for i in range(3): trial("short gap300", short, 300)
    for i in range(3): trial("big gap300", big, 300)
    for i in range(2): trial("big gap1000", big, 1000)
    log("SUMMARY"); 
    import collections
    agg = collections.defaultdict(lambda: [0, 0])
    for r in results: agg[r["kind"]][0] += r["submitted"]; agg[r["kind"]][1] += 1
    for k, (ok, n) in agg.items(): log(f"  {k:14} submitted {ok}/{n}")
    json.dump(results, open(os.path.join(BASE, "results.json"), "w"), ensure_ascii=False, indent=1)
    log("results at", os.path.join(BASE, "results.json"))
finally:
    cleanup()
