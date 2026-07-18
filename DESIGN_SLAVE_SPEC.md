# DESIGN_SLAVE_SPEC — LOCKED implementation contract (slave / shared-PC)

**Status: LOCKED. Two implementers (drover-cloud relay/web/state, herdr-drover
agent/enroll) may code against this without further coordination.** Derived from
`DESIGN_SLAVE.md` + verbatim reads of the current source. Every signature below is
copy-paste ready. Anything not pinned here is left to the implementer, but nothing
here may be changed unilaterally.

Repos:
- `drover-cloud` = `/Users/4noha/works/tools/drover-cloud` (relay, web, state, webauth, relayclient).
- `herdr-drover` = `/Users/4noha/works/tools/herdr-drover` (agent, enroll, webterm, commands).

Line refs are against the files as read for this spec.

---

## 0. Design in one paragraph

The shared PC holds **no SA key**. It holds a durable **refresh secret** (opaque,
useless except against the relay) and fetches a short-lived **slave bearer token**
(`uid="slave:<pc>"`, RS256, 1h) from the relay. The relay (which holds the SA) is
the sole trust boundary: it verifies the bearer on every request and **structurally
forbids the slave from ever being a `viewer` or touching another PC's data**. All
Firestore access by the slave is relay-mediated (`/slave/*` endpoints); the slave
never talks to Firestore directly. Owner→slave operation is unchanged (owner viewer
via cookie). Master path (single/multi-cloud, existing enroll, existing `/session`
wire) is **byte-identical**.

---

## 1. Slave token

### 1.1 Claims & signing — REUSE `firebaseCustomToken`, verify with RS256/SA-public

**Decision: RS256, signed by the SA private key via the existing
`web/fbtoken.go:firebaseCustomToken(saJSON, uid, now)` (fbtoken.go:36), verified at
the relay by deriving the RSA public key from the same SA private key the relay
already holds (`s.enrollSA`).**

Justification (this is the P1 decision the design asked to nail):
- `DESIGN_SLAVE.md §4` mandates reusing `firebaseCustomToken` with `uid="slave:<pc>"`.
  That function signs with the SA **private** key (RS256), so the natural verify is
  RS256 with the SA **public** key. The relay holds the full SA JSON (`s.enrollSA`,
  wired at `cmd/relay/main.go:41-48`), so it can derive the public key locally — no
  network round-trip, no Identity Toolkit.
- Choosing RS256 (not HMAC/`webauth.Signer`) keeps **one token shape** that is a
  genuine Firebase custom token. This makes the P4 `firestore.rules` `slave:<pc>`
  defense-in-depth path (§6) actually reachable via Identity Toolkit
  `signInWithCustomToken` if ever needed. An HMAC token could not be. The primary
  data-plane verify is still done by the relay itself (RS256 sig + `uid` + `exp`,
  **ignoring `aud`** — the relay is not Identity Toolkit).
- Cost: one `rsa.VerifyPKCS1v15` per slave request. Slave requests are human-paced
  (wake / grant / refresh) or content-hash gated (push) → negligible, near-$0 intact.

Mint (relay side, NEW; do not overload owner path `apiFBToken` at fbtoken.go:93):

```go
// web/slavetoken.go (NEW)
func mintSlaveToken(saJSON []byte, pc string, now time.Time) (string, error) {
    return firebaseCustomToken(saJSON, "slave:"+pc, now) // fbtoken.go:36, uid free param
}
```

Resulting JWT (unchanged from `firebaseCustomToken`): header `{alg:RS256,typ:JWT}`,
claims `{iss=sub=<sa.client_email>, aud=<Identity Toolkit const>, iat=now,
exp=now+1h, uid="slave:<pc>"}`.

Verify (relay side, NEW — lift the exact split/decode/verify from
`web/fbtoken_test.go:verifyJWT` into production; derive pub from the SA priv using
`fbtoken.go:47-58` logic):

```go
// web/slavetoken.go (NEW). Symmetric to firebaseCustomToken.
// Returns the bound pc ("mac-studio-herdr") on success.
func verifySlaveToken(saJSON []byte, tok string, now time.Time) (pc string, err error) {
    // 1. parse saJSON -> *rsa.PrivateKey (reuse fbtoken.go:37-58), pub := priv.Public().(*rsa.PublicKey)
    // 2. split tok on '.', b64url-decode sig, sha256(parts[0]+"."+parts[1]),
    //    rsa.VerifyPKCS1v15(pub, crypto.SHA256, h, sig)  -> err on mismatch
    // 3. decode claims; require exp present and now.Unix() < exp  -> err "expired"
    // 4. require uid has prefix "slave:"; pc = uid[len("slave:"):]; require pc != ""
    // 5. DO NOT check aud (relay is not Identity Toolkit)
    return pc, nil
}
```

### 1.2 Authorization header form (wire)

Slave presents the bearer on every `/slave/*` (except `/slave/token`) **and** on the
slave `/session` dial as an HTTP header:

```
Authorization: Bearer <jwt>
```

- `coder/websocket` supports this via `DialOptions.HTTPHeader` (see §4.3 `DialAuth`).
- The relay detects "this is a slave request" **by the presence of this header**.
  Absent header ⇒ master/owner path, untouched.

### 1.3 TTL / refresh

- Bearer TTL is fixed at **1h** (hardcoded in `firebaseCustomToken`, fbtoken.go:75).
- The slave obtains/refreshes it from `POST /slave/token` (§2.1) using its durable
  **refresh secret** (§5). Client refreshes **≥5 min before `exp`** and on any `401`
  from a `/slave/*` call (single retry after refresh).
- The refresh secret itself never expires; it is revoked by owner "端末解除" (§3.4).

---

## 2. Relay endpoints

Two guards:

```go
// web/web.go (NEW) — mirror of apiGuard (web.go:225) but for the RS256 bearer.
// Fail-closed if s.enrollSA=="". Does NOT set Content-Type (each handler sets its own).
func (s *Server) slaveGuard(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if s.enrollSA == "" { http.Error(w, `{"error":"slave disabled"}`, 404); return }
        tok := bearer(r) // strip "Bearer " from Authorization
        pc, err := verifySlaveToken([]byte(s.enrollSA), tok, time.Now())
        if err != nil || pc == "" { http.Error(w, `{"error":"unauthorized"}`, 401); return }
        // fail-closed revocation (relay is authority): revoked/{pc} OR slaves/{pc}.revoked
        if s.st.IsRevoked(context.Background(), pc) || s.st.SlaveRevoked(context.Background(), pc) {
            http.Error(w, `{"error":"revoked"}`, 403); return
        }
        h(w, r, pc)
    }
}
```

Registration (all in `web.Server.Handler()`, web.go:104-125; existing routes stay
byte-identical):

```go
mux.HandleFunc("/slave/token",       s.slaveToken)                 // refresh-secret gated
mux.HandleFunc("/slave/register",    s.slaveGuard(s.slaveRegister))
mux.HandleFunc("/slave/push",        s.slaveGuard(s.slavePush))
mux.HandleFunc("/slave/delete",      s.slaveGuard(s.slaveDelete))
mux.HandleFunc("/slave/sessions",    s.slaveGuard(s.slaveSessions))
mux.HandleFunc("/slave/wake",        s.slaveGuard(s.slaveWake))
mux.HandleFunc("/slave/grant",       s.slaveGuard(s.slaveGrant))
mux.HandleFunc("/slave/revoked",     s.slaveGuard(s.slaveRevoked))
// P4/optional (command receipt): mux.HandleFunc("/slave/commands", ...); "/slave/command-ack"
```

All request/response bodies are JSON unless noted. All error bodies `{"error":"..."}`.
Every handler `w.Header().Set("Content-Type","application/json")` itself (slaveGuard
does not, per gotcha about SSE/plain paths).

### 2.1 `POST /slave/token` — mint/refresh bearer (refresh-secret gated, NOT slaveGuard)

| | |
|---|---|
| Auth | refresh secret (body), **no bearer, no cookie** |
| Req | `{"pc":"<host>-herdr","secret":"<hex>"}` |
| 200 | `{"token":"<jwt>","exp":<unix>}` |
| 401 | secret mismatch / pc unknown |
| 403 | `revoked/{pc}` set OR `slaves/{pc}.revoked==true` |
| 404 | `s.enrollSA==""` (slave feature off) |

Relay: read `slaves/{pc}` (§3.1); require `sha256(secret)==secret_hash`; check
revocation (both `IsRevoked(pc)` and the slave doc's `revoked`); mint via
`mintSlaveToken(s.enrollSA, pc, now)`; return with `exp = now+1h`.

### 2.2 `POST /slave/register` — announce PC (slaveGuard, pc from token)

| | |
|---|---|
| Req | `{"agent_version":"v0.x.y"}` |
| 200 | `{"ok":true}` |

Relay: `s.st.RegisterSlavePCVersion(ctx, pc, agent_version)` (§3.2) → writes
`pcs/{pc}` with `agent_kind:"herdr-drover"`, `role:"slave"`, `cm_version`.

### 2.3 `POST /slave/push` — session upsert + sid→pc learning (slaveGuard)

| | |
|---|---|
| Req | `{"sessions":[ {STATUS session map}, ... ]}` |
| 200 | `{"changed":<int>}` |

Relay: `changed, err := s.st.PushStatusFor(ctx, pc, sessions)` (§3.2). This is the
authoritative **sid→pc learning point**: it writes `pcs/{pc}/sessions/{sessionKey(s)}`
(sessionKey==sid, state.go:81) forced under `pc`, and force-stamps nothing that
would let the slave claim another pc (pc is fixed = `token.pc`).
`content_hash` gate is still applied **client-side** (§4.2) so a no-change tick
sends `{"sessions":[]}` / is skipped entirely; relay's `PushStatusFor` applies the
same server-side gate for the sessions it does receive.

### 2.4 `POST /slave/delete` — session tombstone (slaveGuard)

| | |
|---|---|
| Req | `{"key":"<sid>"}` |
| 200 | `{"ok":true}` |

Relay: `s.st.DeleteSessionFor(ctx, pc, key)` (§3.2) → deletes
`pcs/{pc}/sessions/{key}`.

### 2.5 `GET /slave/sessions` — own session keys (slaveGuard) — startup seed only

| | |
|---|---|
| 200 | `{"keys":["sid1","sid2",...]}` |

Relay: `s.st.SessionKeysFor(ctx, pc)` (§3.2). Called once at agent startup to seed
the producer's `prev` set.

### 2.6 `GET /slave/wake` — wake stream (slaveGuard) — **long-poll with cursor**

**Shape: long-poll (chosen over SSE for Cloud-Run/proxy simplicity and per-poll
bearer/revocation recheck).**

| | |
|---|---|
| Query | `?since=<RFC3339Nano ts or empty>` |
| 200 | `{"sid":"<sid>","ts":"<RFC3339Nano>"}` — a wake occurred |
| 204 | no wake within the server hold window (~25s) — client immediately re-polls |
| 403 | pc became revoked mid-hold |

Relay algorithm:
1. If `wake/{pc}.ts > since` already, return it immediately (lossless catch-up).
2. Else watch `wake/{pc}` (`s.st.WatchWakeFor(ctx, pc, cb)`, §3.2) up to ~25s; on
   change return `{sid,ts}`; on timeout return `204`.
3. Client stores returned `ts` as the next `since` and re-polls immediately (on 200)
   or after a short backoff (on 204/error). This reconstructs
   `state.WatchWake`'s `func(sid string)` callback exactly.

### 2.7 `POST /slave/grant` — source grant (slaveGuard) — role forced `source`

| | |
|---|---|
| Req | `{"sid":"<sid>","ttl_seconds":60}` |
| 200 | `{"ok":true}` |
| 403 | sid not owned by `token.pc` (see below) |

Relay:
1. Ownership check: `s.st.SessionOwnedBy(ctx, pc, sid)` (§3.2) — `pcs/{pc}/sessions/{sid}`
   must exist (learned at `/slave/push`). If not → **403** (a slave cannot grant a sid
   it never pushed under its own pc).
2. Clamp `ttl_seconds` to `[1, 300]`.
3. `s.st.PutRelayGrantFor(ctx, pc, sid, "source", ttl)` (§3.2) — writes
   `relaygrants/{sid}:source` with `pc:token.pc`. **role is always `source`;** a
   `viewer` request body is rejected/ignored (relay never writes a slave viewer grant).

### 2.8 `GET /slave/revoked` — self-revocation check (slaveGuard)

| | |
|---|---|
| 200 | `{"revoked":<bool>}` |

Relay: `IsRevoked(pc) || SlaveRevoked(pc)`. Network/lookup failure ⇒ client treats
as `false` (availability-preferring, matches `state.IsRevoked` semantics state.go:432).

### 2.9 `WSS /session` rule change — **the primary defense**

`relay.Server.ServeHTTP` (relay.go:47) is the sole public `/session` entry
(`mux.Handle("/session", rl)`, main.go:28). Add a **new request-aware seam** — do
NOT change `Grant`'s signature (relay.go:33, wired `rl.Grant = st.CheckRelayGrant`
at main.go:40):

```go
// relay/relay.go — NEW field on Server (nil by default = today's behavior)
// Consulted for /session ONLY when an Authorization: Bearer header is present.
// handled=false  => no slave token; fall through to the existing Grant path (byte-identical).
// handled=true   => slave token present; allow decides 200/101 vs 403; effKey is the
//                   pairing key to Accept with (pc-namespaced for slaves, see §2.10).
SlaveGate func(r *http.Request, sid, role string) (handled bool, allow bool, effKey string)
```

`ServeHTTP` becomes (only the authz block relay.go:54-60 changes; L48-53 400-check
and L60 Accept stay):

```go
if s.SlaveGate != nil {
    if handled, allow, effKey := s.SlaveGate(r, sid, role); handled {
        if !allow { http.Error(w, "未認可（slave scope 外）", http.StatusForbidden); return }
        s.Accept(w, r, effKey, role) // effKey = pc-namespaced (§2.10); role guaranteed "source"
        return
    }
}
if s.Grant != nil && !s.Grant(r.Context(), sid, role) { // UNCHANGED master path
    http.Error(w, "未認可（grant 無効）", http.StatusForbidden); return
}
s.Accept(w, r, sid, role)
```

`SlaveGate` closure (constructed in main.go, closing over `enrollSA` + `st`):

```go
func(r *http.Request, sid, role string) (handled, allow bool, effKey string) {
    tok := bearer(r)
    if tok == "" { return false, false, "" }            // not a slave attempt
    pc, err := verifySlaveToken([]byte(enrollSA), tok, time.Now())
    if err != nil || pc == "" { return true, false, "" } // invalid  -> 403
    if role != "source" { return true, false, "" }       // viewer/other -> 403 (THE peeping stop)
    if st.IsRevoked(r.Context(), pc) || st.SlaveRevoked(r.Context(), pc) { return true, false, "" }
    // sid ownership, anchored on the grant the slave wrote via /slave/grant (pc-stamped by relay):
    gpc, ok := st.RelayGrantPC(r.Context(), sid, "source") // §3.2: reads relaygrants/{sid}:source.pc + exp
    if !ok || gpc != pc { return true, false, "" }         // wrong pc / no grant -> 403
    return true, true, slaveSessionKey(pc, sid)            // §2.10
}
```

**Decision table (LOCKED):**

| bearer | role | sid∈token.pc | result |
|---|---|---|---|
| absent | any | — | master/owner path (Grant) — **byte-identical** |
| valid slave | `viewer` | — | **403** |
| valid slave | `source` | no | **403** |
| valid slave | `source` | yes | **101/200** (Accept as source, pc-namespaced key) |
| invalid/expired | any | — | **403** |

The owner→slave **viewer** side never goes through `ServeHTTP`: `web.wsViewer`
(web.go:470) calls `s.rl.Accept` directly (web.go:490), bypassing `SlaveGate`/`Grant`.
Slaves cannot reach `wsViewer` (it requires a `cm_session` owner cookie, which
requires an allowlisted Google login — a slave can never obtain one).

### 2.10 Slave pairing-key namespacing (closes the source-hijack residual)

Because relay pairs source⇄viewer by the raw `sid` string and two PCs can push the
same sid string, a slave that guesses an owner sid could otherwise hijack the source
slot. Fix (LOCKED, part of P1):

```go
// relay side, shared helper
func slaveSessionKey(pc, sid string) string { return pc + "\x00" + sid } // NUL never in sids/urls
```

- **Slave source** `/session`: Accept with `effKey = slaveSessionKey(token.pc, sid)`
  (returned by `SlaveGate`).
- **Owner viewer of a slave session** (`web.wsViewer`): if the target pc is a slave
  (`s.st.PCRole(ctx,pc)=="slave"`, §3.2), Accept with `slaveSessionKey(pc, sid)`;
  otherwise Accept with raw `sid` (master — **byte-identical**).

Master sources and owner-viewers-of-master keep the raw `sid` key ⇒ a slave source
can never land in a master session's pairing slot.

---

## 3. sid→pc ownership & new state methods

### 3.1 Storage & TTL

| datum | doc | written by | TTL | used by |
|---|---|---|---|---|
| **authoritative sid→pc** | `pcs/{pc}/sessions/{sid}` (sessionKey==sid) | `/slave/push`→`PushStatusFor` | session lifetime | `/slave/grant` ownership check |
| **hot-path sid→pc** | `relaygrants/{sid}:source.pc` | `/slave/grant`→`PutRelayGrantFor` | grant ttl (≤300s, slave sends 60s) | `/session` `SlaveGate` (reuses the read `CheckRelayGrant` already does — near-$0) |
| **slave credential** | `slaves/{pc}` = `{pc, secret_hash, created_at, revoked}` | `/enroll` (role=slave) | none (owner-revoked) | `/slave/token` |
| **role marker** | `pcs/{pc}.role="slave"` (+ `agent_kind`) | `/slave/register` | — | `wsViewer` namespacing, owner-side reconcile filtering |

The hot-path grant anchor is safe because only the relay writes grants, `/slave/grant`
force-stamps `pc=token.pc`, and it 403s unless `pcs/{token.pc}/sessions/{sid}` exists.
Restart-safe: grants are re-issued each connection (`webterm.handleWake` calls
`PutRelayGrant` every wake, webterm.go:147) and derive from the durable sessions doc.

### 3.2 New `state.Client` methods (drover-cloud `state/state.go`, `state/commands.go`)

The relay's state client is `pcID="relay"` (main.go:35), so **every ownership method
must take `pc` explicitly** (do not mint a client per pc). Add:

```go
// state/state.go — pc-explicit variants of existing pcID-scoped methods.
func (c *Client) PushStatusFor(ctx context.Context, pc string, sessions []map[string]any) (changed int, err error)
    // body identical to PushStatus (state.go:95) but col := pcs/{pc}/sessions and the
    // parent-doc tail (state.go:131-149) writes {id:pc, updated_at, agent_kind:"herdr-drover"} (MergeAll).
func (c *Client) DeleteSessionFor(ctx context.Context, pc, key string) error       // pcs/{pc}/sessions/{key}.Delete
func (c *Client) SessionKeysFor(ctx context.Context, pc string) ([]string, error)  // ids under pcs/{pc}/sessions
func (c *Client) SessionOwnedBy(ctx context.Context, pc, sid string) bool          // pcs/{pc}/sessions/{sid} exists && err==nil
func (c *Client) PutRelayGrantFor(ctx context.Context, pc, sid, role string, ttl time.Duration) error
    // body identical to PutRelayGrant (state.go:394) but the doc's "pc" field = pc (not c.pcID)
func (c *Client) RelayGrantPC(ctx context.Context, sid, role string) (pc string, ok bool)
    // reads relaygrants/{sid}:{role}; ok only if exists AND not expired (reuse CheckRelayGrant's exp logic);
    // returns the doc's "pc". (Lets SlaveGate reuse the single hot-path read.)
func (c *Client) WatchWakeFor(ctx context.Context, pc string, cb func(sid string)) error // watch wake/{pc} (WatchWake is pcID-scoped)
func (c *Client) RegisterSlavePCVersion(ctx context.Context, pc, agentVersion string) error
    // Set pcs/{pc} = {id:pc, updated_at, agent_kind:"herdr-drover", role:"slave", cm_version:agentVersion}
func (c *Client) PCRole(ctx context.Context, pc string) (string, error)            // pcs/{pc}.role ("" if unset)

// slave credential (slaves/{pc}) — new collection.
func (c *Client) BindSlave(ctx context.Context, pc, secretHash string) (ok bool, err error)
    // TRANSACTION: reject (ok=false) if pcs/{pc} exists AND pcs/{pc}.role != "slave"
    //   (prevents claiming an existing master/unmarked pc — the owner's real PC is protected
    //    because it already registered). Otherwise Set slaves/{pc}={pc,secret_hash,created_at,revoked:false}.
    //   Re-enroll of an existing slave pc overwrites the secret (ok=true).
func (c *Client) SlaveSecretHash(ctx context.Context, pc string) (hash string, ok bool) // read slaves/{pc}.secret_hash
func (c *Client) SlaveRevoked(ctx context.Context, pc string) bool                      // slaves/{pc}.revoked==true (false on miss)
func (c *Client) SetSlaveRevoked(ctx context.Context, pc string, revoked bool) error    // toggle slaves/{pc}.revoked
```

`SetRevoked`/`IsRevoked`/`ClearRevoked` (state.go:410-443) are **unchanged and
reused** — deleting a slave PC from the Web sets `revoked/{pc}` (via existing
`apiDeletePC`, web.go:313) which `slaveGuard`, `/slave/token`, and `SlaveGate` all
honor. `deletePC` (state.go:228) should additionally delete `slaves/{pc}` — add that
one line so "端末解除" fully removes slave credentials.

### 3.3 Master path invariants for state

`PushStatus` (state.go:95), `DeleteSession` (state.go:324), `OwnSessionKeys`
(state.go:337), `PutRelayGrant` (state.go:394), `CheckRelayGrant` (state.go:448),
`Wake`/`WatchWake` (state.go:352/363) — **unchanged**. New `*For` methods are
strictly additive.

### 3.4 Revocation

`slaveGuard`, `/slave/token`, and `SlaveGate` all fail-closed on
`IsRevoked(pc) || SlaveRevoked(pc)`. Owner "端末解除" (`apiDeletePC`) already calls
`SetRevoked`; that alone locks the slave out of `/session` and all `/slave/*`. The
slave agent also self-stops via `IsSelfRevoked` (mapped to `GET /slave/revoked`, §4.1).

---

## 4. herdr-drover relay-mediated state client (agent side)

### 4.1 Interface + selection

Master path stays concrete `*state.Client` (byte-identical). Slave path uses a
relay-mediated `*relayState`. Define one interface both satisfy for the shared seams:

```go
// cmd/herdr-drover/agentstate.go (NEW)
type agentState interface {
    Close() error
    IsSelfRevoked(ctx context.Context) bool
    RegisterPCVersion(ctx context.Context, agentVersion string) error
    // producer seam (session.StateClient, producer.go:39 — already interface-abstracted)
    PushStatus(ctx context.Context, sessions []map[string]any) (int, error)
    DeleteSession(ctx context.Context, key string) error
    OwnSessionKeys(ctx context.Context) ([]string, error)
    // webterm seam
    WatchWake(ctx context.Context, cb func(sid string)) error
    PutRelayGrant(ctx context.Context, sid, role string, ttl time.Duration) error
    // commands seam (P4/optional)
    WatchCommands(ctx context.Context, fn func(state.Command)) error
    AckCommand(ctx context.Context, id, status, detail string) error
}
```

`*state.Client` already has every one of these methods verbatim ⇒ it satisfies
`agentState` with no changes. `*relayState` (NEW, §4.2) implements each by calling a
`/slave/*` endpoint.

**Method → endpoint map (relayState):**

| interface method | relay endpoint | notes |
|---|---|---|
| `Close` | — | no-op, `return nil` |
| `IsSelfRevoked` | `GET /slave/revoked` | network err ⇒ `false` |
| `RegisterPCVersion` | `POST /slave/register` | |
| `PushStatus` | `POST /slave/push` | **client-side content_hash gate** (§4.2); returns `changed` |
| `DeleteSession` | `POST /slave/delete` | |
| `OwnSessionKeys` | `GET /slave/sessions` | startup seed |
| `WatchWake` | `GET /slave/wake` (long-poll loop) | reconnect/backoff internally; calls `cb(sid)` |
| `PutRelayGrant` | `POST /slave/grant` | role always `"source"`; `viewer` ⇒ no-op (never happens) |
| `WatchCommands` | `GET /slave/commands` (P4) | optional; else `return nil` immediately |
| `AckCommand` | `POST /slave/command-ack` (P4) | optional |

Every call attaches `Authorization: Bearer <bearer>`; `relayState` holds a token
cache and refreshes via `POST /slave/token` (§2.1) when `<5min` to `exp` or on a
`401` (single retry).

**Selection & runRemoteInject skip — exact `agent.go` edits:**

Constructor branch at **`agent.go:181`** (`st, err := state.NewWithCredentials(...)`):

```go
var scConcrete *state.Client // non-nil only on master (for runRemoteInject)
var st agentState
if cfg.Role == "slave" {
    rs, e := newRelayState(cl.RelayURL, cl.PCName, cfg /* reads ~/.herdr-drover/slave.json */, lg)
    if e != nil { return fmt.Errorf("slave 初期化失敗: %w", e) }
    st = rs
} else {
    creds := cl.SAKeyPath
    if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" { creds = "" }
    sc, e := state.NewWithCredentials(ctx, cl.Project, cl.PCName, creds) // UNCHANGED
    if e != nil { return fmt.Errorf("Firestore 接続失敗（project=%s）: %w", cl.Project, e) }
    scConcrete, st = sc, sc
}
defer st.Close()
```

`newWebTerm` (agent.go:202) and `CommandRunner{St: st}` (agent.go:219) and
`session.NewProducer(hcli, st)` (agent.go:245) now take `st agentState` (see §4.3 for
the two field-type widenings). `RegisterPCVersion`/`IsSelfRevoked` calls
(agent.go:191/193/253) work through the interface.

Skip reconcile at **`agent.go:211`** (`if primary && cl.RelayURL != "" {`):

```go
if cfg.Role != "slave" && primary && cl.RelayURL != "" {
    go runRemoteInject(ctx, hcli, scConcrete, cl, lg) // reconcile.go:232 keeps concrete *state.Client
    lg.Printf("%sリモート pane 注入 起動（primary）", tag)
}
```

Slave never runs `runRemoteInject` ⇒ never injects others' sessions (the direct cause
of the leak is removed).

### 4.2 Client-side content_hash gate (keeps near-$0)

`relayState.PushStatus` maintains a per-key content hash **identical to
`state.contentHash`** (state.go:62 — exclude `version`/`updated_at`/`content_hash`,
sorted keys). It POSTs only the sessions whose hash changed since last push
(and returns that count as `changed`). Unchanged tick ⇒ empty/no POST ⇒ no relay
Firestore write, no wake ⇒ near-$0 preserved. (This is a straight copy from `state`
— same author, cm→drover copy is allowed; do NOT vendor herdr sources, AGPL rule #4.)

### 4.3 Two field-type widenings (both satisfied by `*state.Client` ⇒ master
byte-identical) + authenticated dial

```go
// webterm.go:59  — widen st, add dialer seam
type webTerm struct {
    ...
    st         agentState                                      // was *state.Client (webterm.go:59)
    dialSource func(ctx context.Context, sid string) (net.Conn, error) // nil on master
    ...
}
// webterm.go:154 dial site becomes:
dial := w.dialSource
if dial == nil { dial = func(c context.Context, sid string) (net.Conn, error) {
    return relayclient.Dial(c, w.relayURL, sid, "source") // MASTER: header-less, byte-identical
}}
conn, err := dial(bctx, sid)

// commands.go:30 — widen St (all calls WatchCommands/AckCommand/IsSelfRevoked are on the interface)
type CommandRunner struct {
    St agentState // was *state.Client (commands.go:30)
    ...
}
```

Slave sets `wt.dialSource` to a token-injecting dialer using a NEW relayclient
variant (master `Dial` at relayclient.go:41 stays header-less):

```go
// relayclient/relayclient.go (NEW) — identical URL to Dial (url.QueryEscape(sid)),
// adds the bearer header. websocket supports DialOptions.HTTPHeader.
func DialAuth(ctx context.Context, baseURL, sid, role, bearer string) (net.Conn, error) {
    u := baseURL + "/session?sid=" + url.QueryEscape(sid) + "&role=" + role
    h := http.Header{}
    if bearer != "" { h.Set("Authorization", "Bearer "+bearer) }
    c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
    if err != nil { return nil, err }
    return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
}
```

Slave wiring: `wt.dialSource = func(c,sid){ tok,_ := rs.bearer(c); return relayclient.DialAuth(c, cl.RelayURL, sid, "source", tok) }`.
`webterm.handleWake` still calls `PutRelayGrant(sid,"source",60s)` (webterm.go:147)
before dialing — on slave that hits `POST /slave/grant`, which the relay needs to
have processed before the `/session` `SlaveGate` grant-anchor read. Order is already
correct (grant then dial).

---

## 5. `enroll --slave`

### 5.1 Client (`herdr-drover cmd/herdr-drover/enroll.go`)

Add `--slave` (no value) to the arg loop (enroll.go:45-57):

```go
case "--slave":
    slave = true
```

Byte-invariant for master: master never passes `--slave`, so `code`/`relay` parse
and the entire existing wire (POST form `code=<code>`, decode, home resolution,
master write block enroll.go:108-176) are untouched.

Branch **after** decode+home resolution, **before** the master `additional`/config
block, so that block is a verbatim copy for master:

```go
if slave {
    return cmdEnrollSlave(b /*decoded resp*/, relay, relayURL, dir, rcfg, stdout)
}
```

Slave POST differs only by adding the pc field (so the relay can bind `slaves/{pc}`):

```
POST <httpBase>/enroll   form: code=<code>&pc=<rcfg.PCID>&role=slave
```

(Master POST stays `code=<code>` only ⇒ byte-identical.)

`cmdEnrollSlave` writes (single-cloud only — NO `additional`/`AppendCloud`/`clouds.json`):

1. **`~/.herdr-drover/config.json`** via `readRawFileConfig`→set→`writeRawFileConfig`
   (config.go:145/161 — preserves unknown keys like `learn_moves`):
   ```json
   {
     "cloud_relay_url": "wss://claude-master-relay-demo01-an.a.run.app",
     "gcp_project": "example-gcp-project",
     "role": "slave"
   }
   ```
   Set `gcp_project`, `cloud_relay_url`; inject `raw["role"] = "slave"`; **delete**
   `google_application_credentials` (v=="" → delete, reusing enroll.go:163-166 loop
   semantics). **Never write `sa.json`** (ignore `b.SAJSON` entirely — defensive even
   if a legacy/misconfigured relay still returns `sa_json`).

2. **`~/.herdr-drover/slave.json`** (0600, via `writeFileAtomic`):
   ```json
   {
     "pc": "n9htqcr6g0-herdr",
     "refresh_secret": "<hex from relay resp .slave_secret>",
     "relay_url": "wss://claude-master-relay-demo01-an.a.run.app",
     "gcp_project": "example-gcp-project"
   }
   ```

3. **Remove any stale SA fan-out** to be truly SA-less (per gotcha — a master→slave
   re-enroll would otherwise leave `LoadClouds().defaultSAKeyPath` (clouds.go:101)
   falling back to `sa.json`): `os.Remove(~/.herdr-drover/sa.json)` and
   `os.Remove(~/.herdr-drover/clouds.json)` (best-effort).

4. **Skip** `ClearRevoked` (enroll.go:181-188) — already a no-op since `saPath==""`.

5. stdout: distinct lines (no `認証鍵` line; add `role = slave`, `pc id`, and a note
   that this PC cannot see the owner's sessions).

### 5.2 config schema (`cmd/herdr-drover/config.go`)

```go
// fileConfig (config.go:118) — additive; master bytes unaffected because master
// config.json is written from a RAW map, not by marshalling fileConfig.
type fileConfig struct {
    GCPProject                   string `json:"gcp_project,omitempty"`
    CloudRelayURL                string `json:"cloud_relay_url,omitempty"`
    GoogleApplicationCredentials string `json:"google_application_credentials,omitempty"`
    PCID                         string `json:"pc_id,omitempty"`
    Role                         string `json:"role,omitempty"`   // NEW
}

// Config (config.go:23) + resolveConfig (config.go:41): env > file > default("" = master)
type Config struct { ...; Role string } // NEW
// in resolveConfig: cfg.Role = os.Getenv("HERDR_ROLE"); if cfg.Role=="" { cfg.Role = fc.Role }
```

`LoadClouds` (clouds.go:64) is **unchanged**: slave config.json keeps `gcp_project`
(non-secret) + `cloud_relay_url`, so it yields a single `Cloud{Project, RelayURL}`
just as today. `defaultSAKeyPath` (clouds.go:101) returns "" for slave once stale
`sa.json` is removed (step 3) ⇒ `SAKeyPath==""`, which the slave branch ignores
anyway (§4.1).

### 5.3 Relay `/api/enroll` (web.go:403) — issue a slave code

```go
func (s *Server) apiEnroll(w http.ResponseWriter, r *http.Request, t webauth.Token) {
    role := r.URL.Query().Get("role") // "" or "master" => existing behavior
    ...
    scope := "enroll"; extra := ""
    if role == "slave" { scope = "enroll-slave"; extra = " --slave" }
    // CreatePairing(HashCode(code), "", scope, 15m)  (was hardcoded "enroll", web.go:412)
    cmd := "herdr-drover enroll " + code + " --relay " + host + extra +
        "\n  # claude-master の場合: ..." // master string byte-identical when extra==""
    ...
}
```
`role==""`/`"master"` ⇒ scope `"enroll"` and the **identical** command string as
today (byte-invariant).

### 5.4 Relay `/enroll` (web.go:429) — consume slave code, withhold SA, issue secret

```go
_, scope, ok, err := s.st.ConsumePairing(ctx, webauth.HashCode(code))
if !ok || (scope != "enroll" && scope != "enroll-slave") { 401; return } // was: scope != "enroll"

if scope == "enroll-slave" {
    pc := r.FormValue("pc")
    if pc == "" { http.Error(w, "pc が必要", 400); return }
    secret, _ := webauth.GenSecret()          // NEW: crypto/rand 32-byte hex (add to webauth)
    okBind, e := s.st.BindSlave(ctx, pc, sha256Hex(secret)) // §3.2 — collision-checked
    if e != nil { 500; return }
    if !okBind { http.Error(w, "この pc 名は既存の端末と衝突します（master を先に登録してください）", 409); return }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]any{
        "role": "slave", "pc": pc,
        "gcp_project": s.gcpProject, "relay_url": relayWSS(r),
        "slave_secret": secret,                // NO sa_json, ever
    })
    return
}
// scope == "enroll" : EXISTING master response, byte-identical (web.go:457-465, includes sa_json)
```

---

## 6. `firestore.rules` (deploy/firestore.rules) — defense-in-depth (P4)

Only the `match /pcs/{pc}/{document=**}` block (firestore.rules:21-24) changes; read
stays `cm-owner`-only, add a slave-write allow scoped to the path pc segment:

```
service cloud.firestore {
  match /databases/{database}/documents {

    match /pcs/{pc}/{document=**} {
      allow read: if request.auth != null && request.auth.uid == "cm-owner";
      // slave:<pc> は自分の pcs/{pc} 配下のみ write（多層防御・主防御は relay）。
      allow write: if request.auth != null
                   && request.auth.uid == "slave:" + pc;
    }

    // wake / pairings / commands / relaygrants / slaves 等はクライアント全不可
    match /{document=**} {
      allow read, write: if false;
    }
  }
}
```

Notes: server SDK (relay + agent) bypasses rules ⇒ this only guards a hypothetical
REST/client-SDK path. It is reachable only because the slave token is a genuine
Firebase custom token (§1.1). The recursive `{document=**}` covers `pcs/{pc}` and its
`sessions/{sid}` subtree. Slave **read stays denied** (no allow rule matches a
`slave:*` uid). Rules deploy separately (firebaserules REST, per project M9 history)
— editing this file has no runtime effect until that release step runs.

---

## 7. Web add-device master/slave choice (minimal)

- **`web/ui.go` `devicesHTML`** (around ui.go:71-73): add a second control next to
  `<button id="addbtn">`. Keep `addbtn`'s id/text unchanged. Minimal:
  ```html
  <button id="addbtn" ...>＋ 端末を追加</button>
  <button id="addbtn-slave" style="... background:#475569 ...">＋ 共用 PC を追加（slave）</button>
  ```
- **`web/static/devices.js` `setupAdd`** (devices.js:219): bind `addbtn-slave` to
  `fetch("/api/enroll?role=slave", {method:"POST", headers:{Accept:"application/json"}})`
  and adjust the help text to mention `--slave` and "SA 鍵は配布されない＝この PC は
  オーナーのセッションを見られません". The existing `addbtn`→`fetch("/api/enroll")`
  path (no role) is **unchanged**.

---

## 8. BACKWARD-COMPAT INVARIANTS (byte-unchanged — acceptance condition, DESIGN §9 rule 5)

The following MUST be byte-identical for master / single-cloud / existing-enroll.
Any slave gate must be a **no-op when no slave token / no `--slave` / role!="slave"**.

1. **`relay.Server.ServeHTTP` master path** (relay.go:47): with **no
   `Authorization: Bearer` header**, `SlaveGate` returns `handled=false` and control
   falls through to the **unchanged** `Grant` check (relay.go:56) and `Accept`. A
   token-less `source`/`viewer` with a valid grant still gets 101/200 exactly as
   today. `Grant`'s signature (relay.go:33) and its wiring `rl.Grant =
   st.CheckRelayGrant` (main.go:40) are **unchanged**.
2. **`relayclient.Dial`** (relayclient.go:41): unchanged, header-less. Master agent /
   CLI viewer dials stay header-less. `DialAuth` is a separate new function.
3. **`web.wsViewer`** (web.go:470): owner viewer path unchanged for **master** pcs
   (raw `sid` Accept). Only slave-pc targets get the namespaced key (§2.10).
4. **`state` master methods**: `PushStatus`, `DeleteSession`, `OwnSessionKeys`,
   `PutRelayGrant`, `CheckRelayGrant`, `Wake`, `WatchWake`, `RegisterPC*`,
   `contentHash`, `sessionKey` — unchanged. All slave support is `*For`/additive.
5. **`/api/enroll` + `/enroll` master response**: with `role==""`/`"master"`, the
   pairing scope is `"enroll"`, the printed command string is identical, and the
   `/enroll` response `{gcp_project, relay_url, sa_json?}` is identical (web.go:457-465).
6. **herdr-drover master enroll** (`cmdEnroll`, enroll.go:43): with no `--slave`, the
   POST form is `code=<code>` only, and the master/`additional` write blocks
   (enroll.go:108-206) are untouched. `fileConfig`/`Config` new `Role` field is
   `omitempty` and config.json is written from a raw map ⇒ master emitted bytes
   unchanged.
7. **herdr-drover master agent** (`runOneCloud`): master builds concrete
   `*state.Client` (agent.go:181 unchanged branch), runs `runRemoteInject`
   (agent.go:211, now `cfg.Role != "slave" && ...` which is true for master), and the
   `webterm` master dial remains header-less `relayclient.Dial`. The `agentState`
   interface is satisfied by `*state.Client` with zero changes to `state`.
8. **`firebaseCustomToken` / `apiFBToken` owner path** (fbtoken.go:36/93): unchanged
   (`uid="cm-owner"`); slave minting is the separate `mintSlaveToken`/`/slave/token`.
9. **Deployed Cloud Run `/session` wire** for `claude-master` and existing
   `herdr-drover` master agents: unchanged (no header, `Grant`-gated).

A regression test MUST assert (1),(5),(6) explicitly (token-less source/viewer +
valid grant ⇒ 101/200; master enroll code/command/response bytes; master config.json
bytes).

---

## 9. Test / gate plan (real relay + real Firestore emulator; no synthetic greens)

All gates follow the existing real-emulator style
(`state_test.go:TestRelayGrantPutCheckExpiry`, state_test.go:452, `newClient`) and
the `-tags manual` real-API style (`web/fbtoken_manual_test.go`,
`webhelpers_test.go`: `New(rl,st,signer,clientID,allowed,gv,gcpProject,enrollSA)`,
`authCookie`, `fakeGV`). Mint a slave JWT for tests with
`firebaseCustomToken([]byte(sa), "slave:pcX", now)` and send it as
`Authorization: Bearer`. **Each gate must FAIL on current code before the fix**
(iron rule #2).

- **P1 (relay authz)** — `relay`/`web` real-relay adversarial e2e:
  1. slave token × role=`viewer` ⇒ **403**.
  2. slave token × role=`source` × sid NOT owned (no `/slave/grant`) ⇒ **403**.
  3. slave token × role=`source` × own sid (after `/slave/push`+`/slave/grant`) ⇒
     **101/200**.
  4. no-credential source/viewer + valid grant ⇒ **101/200** (invariant §8.1).
  5. expired/garbage bearer ⇒ **403**.
  6. `verifySlaveToken` round-trips `mintSlaveToken` (real RSA), rejects tampered
     sig / wrong `uid` prefix / expired `exp`.
- **P2 (data plane)** — real Firestore emulator:
  1. `/slave/push` writes only `pcs/{own}/sessions/*`; a bearer for pcX cannot write
     pcY (path forced by relay).
  2. `/slave/grant` for a sid not in `pcs/{own}/sessions` ⇒ 403.
  3. sid-hijack: pcX pushes sid also owned by pcY, connects `/session` source ⇒
     lands under `slaveSessionKey(pcX,sid)`, does **not** hijack pcY's raw-sid slot
     (§2.10).
  4. content_hash gate: unchanged tick ⇒ 0 relay Firestore writes (near-$0).
  5. revocation: `SetRevoked(pc)` ⇒ `/slave/token`, `/slave/*`, `/session` all 403/refuse.
- **P3 (agent) — THE DECISIVE ADVERSARIAL e2e (must be simultaneously green):**
  1. **owner→slave operate = WORKS**: owner viewer (cookie) opens a slave session,
     wakes the slave, slave sources, owner types → reaches the slave pane
     (`pane.send_text`). Verify via the existing bridge/display path.
  2. **slave viewing owner's sid = 403**: slave agent credentials attempt a
     `viewer` (or `source` for an owner sid) `/session` ⇒ **always 403**; owner's
     pane never appears on the shared PC (reconcile off, agent.go:211).
  3. slave agent runs SA-less (no `sa.json`, `state.NewWithCredentials` never called
     on the slave branch) and still pushes/wakes/grants via `/slave/*`.
- **P4** — `firestore.rules` real read/write: `slave:<pc>` may write `pcs/{pc}/**`,
  may not write `pcs/{other}`, may not read anything; `cm-owner` read still works;
  server SDK unaffected. Web: slave enroll button issues `enroll-slave` scope,
  `/enroll` withholds `sa_json`, returns `slave_secret`.

**Cross-cutting acceptance (DESIGN §7 final):** P3-1 (owner→slave works) AND P3-2
(slave→owner 403) green **at the same time**, on a real relay + real herdr, or the
feature is not adopted.

---

## 10. Ownership split (who builds what)

- **drover-cloud**: §1 (`mintSlaveToken`/`verifySlaveToken`), §2 (all `/slave/*` +
  `SlaveGate` field on `relay.Server` + `ServeHTTP` edit + `slaveSessionKey` +
  `wsViewer` namespacing), §3.2 (all new `state` `*For`/slave methods +
  `webauth.GenSecret`), §5.3/5.4 (`apiEnroll`/`enroll`), §6 (rules), §7 (UI), and the
  `SlaveGate`/route wiring in `cmd/relay/main.go` (hoist nothing of `Grant`; add the
  closure + register `/slave/*` on the web mux which is already mounted at `/`).
- **herdr-drover**: §4 (`agentState`, `relayState`, `relayclient.DialAuth`, webterm/
  commands field widening + dialer seam, `agent.go:181`/`:211` branches), §5.1/5.2
  (`enroll --slave`, config `Role`).

Shared wire contracts that neither side may change unilaterally: the `/slave/*`
JSON shapes (§2), the `Authorization: Bearer` form (§1.2), the slave `/session`
decision table (§2.9), and the enroll slave request/response (§5).
