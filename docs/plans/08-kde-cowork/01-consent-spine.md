# 01 — Consent Spine (durable, capability-scoped, revocable consent)

The security foundation every other Cowork slice hangs off: a Go-owned grant store, an
`Authorize()` gate, an interactive prompt flow reusing the broker rendezvous, risk-tiered prompt
strength, an append-only audit log, and a global kill-switch. This file is the **canonical
RPC / grant / audit contract** — slices 02–06 conform to the names and shapes fixed here.
Conforms to INV-1..INV-7 (deviations called out inline).

## Current state

- **Permission broker is the only consent primitive today** and it is the wrong shape:
  `permission.Broker` (`core/internal/permission/broker.go:23-67`) is **per-call, ephemeral,
  in-memory, default-deny-on-timeout**. `Open()` mints `perm-<hex>` + buffered(1) `chan Decision`
  (`:35-45`); `Resolve(id, Decision)` (`:49-60`) delivers once and deletes; `Close(id)` (`:63-67`)
  drops on timeout. **No persistence, no scopes, no targets, no remember, no revocation.** We reuse
  its *rendezvous shape* (INV-2) but not the type.
- **The rendezvous round-trip is the precedent to mirror.** `permission.request` handler
  (`core/cmd/akcore/main.go:1218-1245`): `broker.Open()` → `srv.Notify("permission.requested", …)`
  → **blocks `select` on the channel vs `time.After(8*time.Minute)`**; on timeout `broker.Close(id)`
  + returns `{allow:false}`. `permission.respond` (`:1247-1259`) calls `broker.Resolve`. The MCP
  bridge caller waits **10 min** (`mcp.go:416-418` `CallTimeout("permission.request", …,
  10*time.Minute)`), so **core self-denies at 8 < 10** to guarantee a definitive answer before the
  caller times out — the **8<10 precedent** we replicate. Fail-closed on transport error
  (`mcp.go:419` returns `behavior:"deny"`).
- **Claude's `--permission-mode` is the dominant gate today, not us.** Agents spawn with
  `--permission-mode acceptEdits` (`agent.go:285`), `--allowedTools mcp__cooperation` force-allowed
  (`agent.go:288`), and gated tools funnel through `--permission-prompt-tool
  mcp__cooperation__request_permission` (`agent.go:314`). **A `--permission-mode bypassPermissions`
  would skip that funnel entirely** — so any R2 control gate **must live imperatively inside the Go
  tool**, never rely on the prompt funnel (INV-3).
- **Atomic persistence pattern to mirror exactly** (`session.go`): `flush()` (`:169-190`) =
  `MkdirAll(dir,0o755)` → `WriteFile(path+".tmp", b, 0o644)` → `os.Rename(tmp, path)`; **caller holds
  `s.mu`**; load tolerates `os.IsNotExist` as empty (`:90-96`); `Archive()` writes the archive file
  **BEFORE** dropping the live record (`:341-374`, "write-first" safety). Store struct is
  `{path, mu sync.Mutex, recs map}` with **copy-out on read** (`List` returns a fresh slice `:265-276`).
- **`safe.Go("pkg.site", fn)` for every goroutine** (`safe/safe.go:22-35`) — recover+log; a bare
  `go` panic kills the daemon. `time.AfterFunc` bodies must be wrapped too.
- **`srv.Notify(method, params)` broadcasts to every client; notifications are lossy under
  backpressure** (context pack §1; `server.go:197-216, 252-290`). Responses (with id) block until
  delivered. ⇒ **`grantsChanged` and `killSwitch` must be re-derivable via a pull RPC**
  (`cowork.listGrants`) — never the sole source of truth.
- **Handlers register statically in `registerHandlers`** (`main.go:437+`) via `srv.Handle(...)`;
  the handler map is **not goroutine-safe after Serve** (context pack §1). All Cowork RPCs register
  up-front. Subsystems reach handlers via `handlerDeps` (`main.go:419-434`) — we add one field.
- **MCP config** is per-thread (`writeMCPConfig`, `main.go:2778-2806`) and currently advertises only
  `cooperation`. INV-5 wants a **separate opt-in `cowork` MCP server**, off by default.

## Proposed design

### 1. Grant data model (`core/internal/cowork/grants.go`)

```go
type Capability string
const (
    CapWindowList    Capability = "window_list"     // R0
    CapA11yRead      Capability = "a11y_read"        // R1
    CapScreenshot    Capability = "screenshot"       // R1
    CapScreencast    Capability = "screencast"       // R1
    CapA11yAction    Capability = "a11y_action"      // R2
    CapInputInject   Capability = "input_inject"     // R2
    CapRemoteDesktop Capability = "remote_desktop"   // R2
    CapVDSandbox     Capability = "vd_sandbox"       // R0 (own isolated desktop)
)

type Scope string
const (
    ScopeOnce         Scope = "once"          // single Authorize() hit, then auto-revoked
    ScopeSession      Scope = "session"       // until core restart
    ScopeTimed        Scope = "timed"         // until ExpiresAt
    ScopeUntilRevoked Scope = "until_revoked" // durable across restarts
)

// TargetKind classifies what a grant narrows to. Exactly one field of Target is
// authoritative per kind; the rest are descriptive (for prompt/audit rendering).
type TargetKind string
const (
    TargetWindow    TargetKind = "window"   // KWin internalId (QUuid string)
    TargetApp       TargetKind = "app"      // resourceClass, all windows of an app
    TargetScreen    TargetKind = "screen"   // screen name / index
    TargetRegion    TargetKind = "region"   // a screen rect
    TargetVDesktop  TargetKind = "vdesktop" // virtual-desktop id
    TargetSandbox   TargetKind = "sandbox"  // a vd_sandbox session id
    TargetAny       TargetKind = "any"      // capability has no spatial target (window_list)
)

type Target struct {
    Kind          TargetKind `json:"kind"`
    WindowID      string     `json:"windowId,omitempty"`      // KWin internalId
    ResourceClass string     `json:"resourceClass,omitempty"` // app id
    Screen        string     `json:"screen,omitempty"`
    Region        *Rect      `json:"region,omitempty"`
    VDesktopID    string     `json:"vdesktopId,omitempty"`
    SandboxID     string     `json:"sandboxId,omitempty"`
    Label         string     `json:"label,omitempty"`         // human caption for prompt/audit
}
type Rect struct{ X, Y, W, H int `json:"x" "y" "w" "h"` } // (split into 4 json tags)

type Grant struct {
    ID         string     `json:"id"`          // "grant-<hex>"
    ThreadID   string     `json:"threadId"`    // owning thread; grants are per-thread (INV-2)
    Capability Capability `json:"capability"`
    Target     Target     `json:"target"`
    Scope      Scope      `json:"scope"`
    RiskTier   string     `json:"riskTier"`    // "R0"|"R1"|"R2" snapshot at grant time
    GrantedAt  time.Time  `json:"grantedAt"`
    ExpiresAt  *time.Time `json:"expiresAt,omitempty"`  // set iff Scope==timed
    RevokedAt  *time.Time `json:"revokedAt,omitempty"`  // set on revoke/kill/expiry-sweep
    RevokeReason string   `json:"revokeReason,omitempty"` // "user"|"kill_switch"|"expired"|"restart"
    // Policy fields (slices may extend; baseline below):
    UsesRemaining *int    `json:"usesRemaining,omitempty"` // for Scope==once: 1; decremented to 0
    Redact        bool    `json:"redact,omitempty"`        // R1: redaction posture requested (slice 02/03)
    GrantedBy     string  `json:"grantedBy,omitempty"`     // "user" (only legal value; never agent)
}
```

**JSON file shape** (`cowork-consents.json`):
```json
{ "schemaVersion": 1, "grants": [ {Grant…} ] }
```

Notes: `RiskTier` is **snapshotted** so a later tier-table change cannot silently downgrade an old
grant. Revoked/expired grants are kept in-file (tombstoned with `RevokedAt`) until a bounded
compaction sweep prunes them — keeping a short revocation history aids the audit/UI without
unbounded growth (cap **2000** entries; prune oldest revoked beyond that).

### 2. Grant store (`core/internal/cowork/grants.go` cont.)

```go
type Store struct {
    path string
    mu   sync.Mutex
    grants map[string]Grant // id -> Grant (live + recently-tombstoned)
    onChange func()         // set by wiring → triggers cowork.grantsChanged notify (debounced)
}
func DefaultGrantsPath() string // $XDG_DATA_HOME/agentkate/cowork-consents.json (mirror session.DefaultPath)
func NewStore(path string) (*Store, error)
```

- `load()` mirrors `session.load` (`:89-114`): tolerate `os.IsNotExist`; unmarshal
  `{schemaVersion, grants}`; **run `migrate(schemaVersion)`** then **`applyRestartSemantics()`**.
- `flush()` is a **verbatim copy** of the temp+rename pattern (`session.go:169-190`), caller holds
  `s.mu`. File mode `0o600` (consent is more sensitive than threads' `0o644` — defense vs other
  local users) and dir `0o700`.
- **Concurrency:** every public method `Lock/defer Unlock`; reads **copy-out** (clone `Grant`,
  return value not pointer); **never hold `s.mu` across the broker `select` or any D-Bus call**.

**Schema versioning / migration:** `schemaVersion` int at file root. `migrate()` is a switch from
the on-disk version up to `currentSchema=1`; unknown future version → **refuse to load, fail
closed** (do not silently drop grants we can't interpret — log + start empty rather than corrupt).

**Restart semantics (`applyRestartSemantics`, run once at load):**

| Scope | Survives restart? | Action at load |
|---|---|---|
| `until_revoked` | **yes** | kept live (if not expired/revoked) |
| `timed` | **no** | tombstone `RevokedAt=now, reason="restart"` (a live capture cannot survive a restart anyway — INV; portal restore_tokens are single-use) |
| `session` | **no** | tombstone, reason `"restart"` |
| `once` | **no** | tombstone, reason `"restart"` |

Rationale (recommend for v1): **only `until_revoked` persists; everything else auto-revokes on
restart and any live capture/portal/screencast is torn down** by the kill-on-boot pass (§6 teardown
hooks fire for any session referencing a now-dead grant). This is the safe default the threat model
(INV-4) demands — a crash must not silently re-arm desktop control.

Public API:
```go
func (s *Store) Add(g Grant) error                       // validates, sets GrantedAt, persists, onChange
func (s *Store) Match(threadID string, cap Capability, t Target) (Grant, bool) // live-grant lookup (§3)
func (s *Store) List(threadID string) []Grant            // copy-out; "" = all threads (UI/kill)
func (s *Store) Revoke(id, reason string) (Grant, bool)  // tombstone + persist + onChange
func (s *Store) RevokeAll(reason string) []Grant         // kill-switch / per-thread bulk; returns revoked set
func (s *Store) RevokeThread(threadID, reason string) []Grant
func (s *Store) SweepExpired(now time.Time) []Grant      // called by a ticker; tombstones timed grants past ExpiresAt
func (s *Store) consumeOnce(id string)                   // decrement UsesRemaining, tombstone at 0 (called by Authorize on a once hit)
```

A `safe.Go`-launched ticker (`time.NewTicker(30s)`) calls `SweepExpired`; each tombstoned grant
triggers teardown of any live session bound to it (§6) and `onChange`.

### 3. Consent check + interactive prompt (`core/internal/cowork/consent.go`)

The single function every Cowork tool calls before acting:

```go
type Decision struct {
    Allow    bool
    GrantID  string // the matched-or-newly-created grant
    Reason   string // "matched"|"granted"|"denied"|"timeout"|"killed"|"r2_blocked"
}

func (a *Authority) Authorize(ctx context.Context, threadID string, cap Capability, t Target) (Decision, error)
```

`Authority` bundles `*Store`, `*permission.Broker`-shaped rendezvous (a **dedicated grant broker**,
not the per-tool one — separate type `grantBroker` to keep audiences distinct), the `*audit.Log`,
the `onChange`/notify hooks, and a `killed atomic.Bool` (§6). Algorithm:

1. **Kill check.** If `a.killed.Load()` → audit `denied/killed`, return `{Allow:false,
   Reason:"killed"}`. (Re-checked again after any prompt, before returning allow.)
2. **R2 imperative gate (INV-3).** `tier := TierOf(cap)`. If `tier == "R2"` **and** the matched
   grant (if any) is not a fresh per-action grant, force the interactive prompt — **R2 never
   matches a remembered grant by default** (scope is `once` per-action). This gate is in Go, runs
   regardless of `--permission-mode`. (Power-user opt-in for a timed R2 grant is a **v3** policy
   knob, default-off, behind a distinct double-confirm UI.)
3. **Store match.** `g, ok := store.Match(threadID, cap, t)`. `Match` returns a grant iff: same
   `ThreadID`, same `Capability`, `RevokedAt==nil`, not expired (`ExpiresAt==nil || now<*ExpiresAt`),
   `UsesRemaining` nil-or->0, and **target covers** `t` (window-id equal; or grant target is `app`
   and `t.ResourceClass` matches; or grant `screen` covers a `region` within it; `TargetAny` for
   `window_list`). On hit: audit `matched`, if `Scope==once` call `consumeOnce`, return
   `{Allow:true, GrantID:g.ID, Reason:"matched"}`.
4. **Miss → interactive prompt.** `reqID, ch := grantBroker.Open()`. Emit
   `srv.Notify("cowork.grantRequested", GrantRequest{…})` (shape §3a). **Block on**:
   ```go
   select {
   case d := <-ch:        // user responded via cowork.respondGrant
   case <-time.After(promptTimeout(tier)): grantBroker.Close(reqID); deny "timeout"
   case <-a.killChan:     // kill-switch fired while prompting → deny "killed"
   case <-ctx.Done():     deny ctx.Err()
   }
   ```
5. **On user allow:** build a `Grant` from the request + the user-chosen scope (UI returns scope),
   `store.Add(g)`, audit `granted`, return `{Allow:true, GrantID:g.ID, Reason:"granted"}`. On deny:
   audit `denied`, return `{Allow:false, Reason:"denied"}`.

**Timeout budget (8<10 precedent, generalized).** The MCP `cowork` bridge waits **`callBudget`**
(default 10 min) for `cowork.<verb>` tool RPCs; the tool handler's `Authorize` self-denies at
`promptTimeout(tier)` strictly less than `callBudget`. Recommend `promptTimeout`: R0/R1 = **5 min**,
R2 = **3 min** (control prompts should not linger). All < 10 min bridge budget ⇒ caller always gets
a definitive answer. Fail-closed on any transport/marshal error (mirror `mcp.go:419`).

**3a. RPC + notification contract (the canonical surface — INV-5 names):**

| Method (RPC, core serves) | Params | Result |
|---|---|---|
| `cowork.requestGrant` | `{threadId, capability, target:Target, suggestedScope}` — issued **by the cowork tool handler internally**, not the agent; exposed as an RPC so the UI's "pre-grant" affordance can also open one | `{requestId}` (then resolves async via `cowork.grantRequested`/`cowork.respondGrant`) |
| `cowork.respondGrant` | `{requestId, allow:bool, scope, expiresInSec?, redact?}` (UI→core) | `{ok:true}` |
| `cowork.listGrants` | `{threadId?}` ("" = all) | `{grants:[Grant], killed:bool}` |
| `cowork.revokeGrant` | `{id, reason?}` | `{ok, grant:Grant}` |
| `cowork.revokeThread` | `{threadId, reason?}` | `{revoked:[id]}` |
| `cowork.killSwitch` | `{on:bool, reason?}` (idempotent; `on:false` re-arms) | `{ok, revoked:[id]}` |
| `cowork.listAudit` | `{threadId?, sinceSeq?, limit?}` (§5) | `{entries:[AuditEntry], nextSeq}` |

| Notification (core→UI broadcast) | Params |
|---|---|
| `cowork.grantRequested` | `GrantRequest{requestId, threadId, threadTitle, capability, riskTier, target:Target, actionPreview, suggestedScope, defaultScope, expiresAtHint?}` |
| `cowork.grantsChanged` | `{threadId?}` — a hint; UI re-pulls `cowork.listGrants` (lossy-safe, INV §1) |
| `cowork.killSwitch` | `{on:bool, reason, at}` — UI flips the global banner/state |

`actionPreview` is the **concrete** thing about to happen (INV-4a): e.g. for `input_inject` the
literal keystrokes/coords/target-window caption; for `screenshot` the window caption + region.
Slices 02/04 populate it; the spine just carries and audits it.

`GrantRequest` ⇄ `cowork.respondGrant` is matched by `requestId`; `respondGrant` calls
`grantBroker.Resolve(requestId, grantDecision{Allow, Scope, ExpiresIn, Redact})`. Unknown
`requestId` (already timed-out/resolved) → `{ok:true}` no-op (mirror `Resolve`'s bool-ignored
contract).

### 4. Risk tiers → prompt strength + the R2 imperative gate

`TierOf(cap)` is a fixed table (R0/R1/R2 per INV-3; matches the const comments above). It drives
three things the UI honors via `riskTier` on `GrantRequest`:

| Tier | Caps | Default scope offered | Prompt strength (UI, slice 06) | Auto-remember? |
|---|---|---|---|---|
| **R0** | `window_list`, `vd_sandbox` | `session` | standard banner (reuse `m_permBar` style) | session ok |
| **R1** | `a11y_read`, `screenshot`, `screencast` | `timed` (default 15 min) or per-target `session` | banner + per-target detail + redaction toggle | timed/session ok |
| **R2** | `a11y_action`, `input_inject`, `remote_desktop` | **`once` (per-action)** | **distinct `KMessageBox::warningContinueCancelList` + `Dangerous`**, action listed verbatim, Cancel-defaulted | **never** |

**The imperative R2 gate (INV-3, non-negotiable).** Inside every R2 Cowork tool handler, *before*
any portal/AT-SPI action, the handler calls `Authorize(...)` and `Authorize` itself enforces step 2
above: **R2 forces a fresh per-action prompt and ignores remembered grants by default**. This is a
plain `if tier==R2 { …prompt… }` in Go — it executes even under `--permission-mode
bypassPermissions`, because the gate is not in the prompt funnel.

**Relationship to the existing `--permission-prompt-tool request_permission` funnel (recommend):**
**Bypass it; gate purely server-side.** The `cowork` MCP server's tools are exposed via
`--allowedTools mcp__cowork` (force-allowed at the Claude layer, like `cooperation` today,
`agent.go:288`) so Claude does **not** route them through `request_permission`. Instead, **every
`desktop_*` tool's Go handler calls `Authorize()` itself**. Rationale: (a) the old funnel is
disabled by `bypassPermissions`, which is unacceptable for R2; (b) the funnel only knows a tool
*name*, not the capability+target+action-preview we must show; (c) one gate, server-authoritative,
audited — no double-prompt. The funnel stays the gate for ordinary `cooperation`/Bash tools;
`cowork` is gated by the spine. (Documented deviation from "reuse the existing funnel" — justified
by INV-3/INV-4.)

### 5. Audit log (`core/internal/cowork/audit.go`)

Append-only, atomic, tamper-evident-ish. Location: `$XDG_DATA_HOME/agentkate/cowork-audit.jsonl`
(**JSONL**, one entry per line — append is a single `O_APPEND` write, cheap, crash-safe; no
read-modify-write so no rename needed per entry). Mode `0o600`, core-owned.

```go
type AuditEntry struct {
    Seq        uint64     `json:"seq"`        // monotonic, gap-free per process; persisted high-water
    At         time.Time  `json:"at"`         // UTC
    Kind       string     `json:"kind"`       // "grant"|"revoke"|"deny"|"action"|"kill"|"restart_sweep"
    ThreadID   string     `json:"threadId"`
    Capability Capability `json:"capability,omitempty"`
    Target     Target     `json:"target,omitempty"`
    GrantID    string     `json:"grantId,omitempty"`
    Scope      Scope      `json:"scope,omitempty"`
    Reason     string     `json:"reason,omitempty"`
    Preview    string     `json:"preview,omitempty"`   // the actionPreview for control/capture
    ArtifactHash string   `json:"artifactHash,omitempty"` // sha256 hex of captured frame/text (R1/R2)
    PrevHash   string     `json:"prevHash,omitempty"`  // sha256 of previous line (hash chain)
    Hash       string     `json:"hash,omitempty"`      // sha256(canonical(entry-without-Hash))
}
```

**Tamper-evidence (hash chain):** each entry's `Hash = sha256(PrevHash ‖ canonicalJSON(entry sans
Hash))`; `PrevHash` is the previous line's `Hash`. A break in the chain (verifiable by replay) flags
tampering/truncation. Not cryptographically signed (no key store in v1) — "tamper-evident-ish" per
the brief. The chain head (last `Hash`, `Seq`) is held in memory under the audit mutex.

**Every executed action is audited** (INV-4d): the Cowork tool handler, *after* a successful
`Authorize` and *after* performing the capture/control, calls
`audit.Action(threadID, cap, target, preview, artifactHash)`. For captures, slices 02/03 compute the
**frame/text hash** (sha256 of the PNG bytes or extracted text) and pass it in — the audit stores
**only the hash, never the pixels/text** (INV-4e). Grants/denies/revokes/kills are audited by the
spine itself at their decision points.

**How the UI reads it:** `cowork.listAudit{threadId?, sinceSeq?, limit?}` → core tails the JSONL
(bounded read, newest-`limit` lines, default 200) and returns `{entries, nextSeq}`. The
ActiveGrants/audit view (slice 06) pulls on open and on each `cowork.grantsChanged`. No streaming
firehose — pull-based (INV §1, INV-6).

**Growth:** rotate at e.g. 8 MiB → `cowork-audit.jsonl.1` (single backup), atomic rename; the head
hash carries across rotation so the chain is continuous within a process. (Cross-rotation
verification is best-effort.)

### 6. Kill-switch (`core/internal/cowork/consent.go` — `Authority`)

```go
func (a *Authority) Kill(reason string) []Grant   // global: revoke ALL grants, ALL threads
func (a *Authority) Rearm()                        // clears killed; grants must be re-requested
func (a *Authority) Killed() bool
```

- `Kill` sets `a.killed.Store(true)`, **closes `a.killChan`** (so any in-flight `Authorize` prompts
  unblock → deny "killed"), calls `store.RevokeAll("kill_switch")`, audits a `kill` entry, fires
  `onChange` + emits `cowork.killSwitch{on:true,…}`. Returns the revoked set.
- **Teardown of live sessions:** the spine holds a registry of **teardown hooks** keyed by grantID:
  `RegisterTeardown(grantID string, fn func())`. Capture/control/sandbox slices (02/04/05) register
  a teardown when they start a portal/screencast/remote-desktop/sandbox session bound to a grant.
  On any revoke (single, thread, kill, expiry, restart-sweep) the spine invokes the hook **via
  `safe.Go("cowork.teardown", fn)`** (off the mutex). The UI process owns the actual portal session
  (INV-1) — so for UI-side sessions the teardown hook emits a `cowork.killSwitch`/`cowork.grantsChanged`
  the UI reacts to by closing its PipeWire stream + cancelling the portal session. **Go-side
  teardown (KWin script `stop()`, AT-SPI handles) runs directly.**
- **Per-thread vs global:** `cowork.revokeThread{threadId}` revokes + tears down only that thread's
  grants (multi-agent blast-radius containment, INV-4g) and does **not** set the global `killed`
  flag. `cowork.killSwitch{on:true}` is global and *also* arms the flag so **new** `Authorize` calls
  fail until `Rearm()`. `on:false` re-arms (audited as `kill`/reason `rearm`).
- **Kill at boot:** `applyRestartSemantics` runs the same revoke+teardown for all non-`until_revoked`
  grants (§2 table) — the on-restart kill of live capture.

### 7. Self-approval & anti-escalation enforcement (concrete points)

- **Consent files core-owned, never agent-writable (INV-4f).** `cowork-consents.json` and
  `cowork-audit.jsonl` live under `$XDG_DATA_HOME/agentkate/`, written **only** by the Go core,
  mode `0o600`, dir `0o700`. **The agent has no MCP tool that reads or writes them** — there is no
  `desktop_write_grant`; grants are minted **only** through the `respondGrant` path which is served
  to the **UI**, and `respondGrant`'s `grantBroker.Resolve` is reachable only from a UI connection
  (the MCP bridge cannot call `cowork.respondGrant` — it's not in the bridge's verb set). Enforcement
  point: `respondGrant` handler asserts the caller is a UI client, not a bridge (v1: the bridge
  simply never invokes it; v2: tag connections and reject bridge-origin `respondGrant`).
- **`GrantedBy` is always `"user"`.** `Store.Add` **rejects** any grant whose path didn't originate
  from a `respondGrant`/UI flow (the only constructor that sets it). Agents cannot self-grant.
- **AK's own windows excluded from `input_inject` / `a11y_action` / `remote_desktop` targets
  (INV-4f).** The spine exposes `IsSelfTarget(t Target) bool` checked inside `Authorize` for all R2
  caps: deny (`Reason:"self_target"`, audited) if the target window's `resourceClass`/`pid` matches
  Agent Kate's own UI process (matched against AK's `resourceClass` `org.kde.agentkate` and the UI
  QProcess pid the core already supervises). This prevents the agent injecting into the consent
  dialog to self-approve. (Slice 04 supplies the live AK-window identity; the spine owns the policy.)
- **Captured pixels/text are untrusted and cannot grant (INV-4e).** Capture results flow back to the
  agent as tool output only; **no code path lets tool output reach `respondGrant` or `Store.Add`**.
  Audit stores hashes, not content. This is structural — there is simply no edge from capture output
  to the grant store.
- **`--permission-mode` cannot disable the R2 gate (§4).** Enforcement point: the gate is the
  imperative `if tier==R2` inside each Go handler + `Authority.killed`/`IsSelfTarget` checks, none
  of which consult the Claude permission mode.

### 8. Tests (`core/internal/cowork/*_test.go`)

- `grants_test.go`: **grant** (Add→Match hit), **deny** (no grant → Match miss), **target
  narrowing** (window grant doesn't match a different window; app grant matches all its windows),
  **expiry** (timed grant past `ExpiresAt` → Match miss; `SweepExpired` tombstones it),
  **revocation** (Revoke → Match miss + tombstone present), **once-consumption** (second Match miss).
- `consent_test.go`: **timeout** (no `respondGrant` within `promptTimeout` → `{Allow:false,
  Reason:"timeout"}`, and self-deny strictly before a simulated 10-min bridge budget),
  **concurrent-resolve** (two goroutines `Authorize` same cap/target while one user `respondGrant`
  fires — exactly one creates the grant, no deadlock, mutex never held across the select),
  **R2 forces prompt even with a matching session grant**, **self-target deny**, **kill-during-prompt**
  (Kill closes killChan → in-flight Authorize denies "killed").
- `restart_test.go`: write a store with one of each scope, reopen → only `until_revoked` survives;
  `timed`/`session`/`once` tombstoned reason `"restart"`; teardown hooks for live grants fired.
- `audit_test.go`: append N entries → hash chain verifies; mutate one line → verification fails;
  `listAudit` returns newest-`limit`; rotation preserves chain head.
- `killswitch_test.go`: Kill revokes all threads' grants, fires every registered teardown exactly
  once, sets `Killed()`, blocks subsequent `Authorize`; `Rearm()` re-enables.

## Implementation steps

1. **Go package skeleton.** Create `core/internal/cowork/{grants.go,consent.go,audit.go}` with the
   types above; `grantBroker` (copy `permission.Broker` shape, payload `grantDecision`). Unit-test
   grants + audit first (no IPC).
2. **Wire `Authority` into `handlerDeps`.** Add `cowork *cowork.Authority` to `handlerDeps`
   (`main.go:419-434`); construct in `main()` beside `sessions`/`broker`; pass `onChange` =
   debounced `srv.Notify("cowork.grantsChanged", …)`; set `srv` for `cowork.grantRequested`.
3. **Register RPCs** in `registerHandlers` (`main.go:437+`): the seven methods in §3a, each
   `Unmarshal → Authority/Store call → result`, with the `Authorize` blocking select carrying its
   `promptTimeout`. Launch the `SweepExpired` ticker via `safe.Go`.
4. **Restart + shutdown.** Call `applyRestartSemantics` in `NewStore`/load; add a
   `Authority.Close()` to `gracefulShutdown` (`main.go:302-323`) that tombstones session/once/timed
   grants and runs every teardown hook (mirrors `gitCache.Close()` placement).
5. **MCP `cowork` server (off by default, INV-5).** Extend `writeMCPConfig` (`main.go:2778`) to add
   a `cowork` entry **only when the thread/workspace opts in** (a `record.CoworkEnabled` flag, new
   `session.Record` field, default false); add `mcp__cowork` to `--allowedTools` only then. The
   `desktop_*` tool handlers (slices 02–05) each call `Authorize` before acting — this slice
   provides the function and the gate; it ships **no** `desktop_*` tool itself.
6. **UI contract handoff.** Slice 06 builds the ConsentDialog (subscribes `cowork.grantRequested`,
   sends `cowork.respondGrant`), ActiveGrantsView (`cowork.listGrants` + `cowork.grantsChanged`),
   audit view (`cowork.listAudit`), and the kill-switch button (`cowork.killSwitch`). This file fixes
   their wire shapes.

## Risks / considerations

- **Lossy `grantsChanged`/`killSwitch` notifications.** They are best-effort (INV §1). **Mitigation:**
  the UI **always** re-derives from `cowork.listGrants` (which returns `killed`); the notification is
  a hint, never authority. A dropped `killSwitch` notify still leaves `killed=true` in core, so new
  `Authorize` calls deny regardless of UI state.
- **Two brokers, two timeouts.** The grant broker is separate from `permission.Broker`. **Mitigation:**
  identical rendezvous shape, but distinct ids (`grant-req-<hex>` vs `perm-<hex>`) so a stray
  `permission.respond` can never resolve a grant and vice-versa.
- **Self-target identity is supplied by slice 04.** If AK's window identity is wrong/stale, the
  self-injection guard could be bypassed. **Mitigation:** match on **both** `resourceClass` and the
  supervised UI pid; fail closed (treat unknown identity as "could be self" for R2 → deny) until the
  identity is resolved. Cross-ref **04-control.md**.
- **Audit JSONL unbounded without rotation.** **Mitigation:** 8 MiB rotation + single backup; the
  store-tombstone cap (2000) bounds `consents.json`. Cross-ref growth-bounding precedent (Tier-3
  commit `dc02c92`).
- **R2 per-action consent fatigue** vs **R2 safety** (INV-4b vs INV-3). **Mitigation:** v1 keeps R2
  strictly per-action; a **v3** opt-in timed R2 grant behind double-confirm is the pressure valve —
  default-off, never auto-suggested. Cross-ref **04-control.md**, **06-ui-panel.md**.
- **Migration of a future schema we can't read** → refuse-to-load could surprise users. **Mitigation:**
  on unknown version, **don't delete the file**; log loudly, start with an empty live set, leave the
  file for a newer build to read. Never auto-rewrite a higher version down.
- **Teardown hook for UI-side portals is indirect** (core can't close the UI's PipeWire stream).
  **Mitigation:** the kill/revoke notify carries the affected grant ids; the UI is contractually
  required to tear down on receipt; core additionally invalidates the grant so a re-`captureStill`
  re-prompts. Cross-ref **02-capture.md**, **INV-1**.

## Acceptance

- A Cowork tool calling `Authorize(threadId, screenshot, {window:X})` with no grant raises a
  `cowork.grantRequested`; on UI `cowork.respondGrant{allow:true,scope:timed,expiresInSec:900}` the
  call returns allow, a `Grant` is persisted, and `cowork.listGrants` shows it.
- A second `Authorize` for the same target within the window returns allow **without** a new prompt;
  after `ExpiresAt` it re-prompts; `SweepExpired` tombstones it.
- An R2 `Authorize` (`input_inject`) **always** prompts even with a prior matching grant and even
  under `--permission-mode bypassPermissions`; injecting into an AK-own window is denied
  (`self_target`), audited.
- `cowork.killSwitch{on:true}` revokes every grant across every thread, fires every registered
  teardown once, sets `listGrants.killed=true`, and blocks subsequent `Authorize` until
  `killSwitch{on:false}`.
- Restart: only `until_revoked` grants survive; `session`/`timed`/`once` are tombstoned
  (reason `restart`) and their teardown hooks ran; no live capture is re-armed.
- The audit JSONL records a grant, every executed capture/action (with `artifactHash`, never
  content), every revoke/kill; its hash chain verifies; mutating a line breaks verification.
- `cowork-consents.json` / `cowork-audit.jsonl` are `0o600`, core-written; no MCP tool can read or
  write them; no capture-output path reaches the grant store.
- Tests in §8 pass under `go test ./core/internal/cowork/...` including `-race`.

### Sizing (S ≈ <½ day, M ≈ 1–2 days, L ≈ 3–5 days) + v1/v2/v3

- **v1 (M–L):** grant model + store (atomic, restart semantics, migration) + `Authorize` + grant
  broker rendezvous + the 7 RPCs/3 notifications + audit JSONL (hash chain) + kill-switch +
  self-target/anti-escalation guards + tests. Proves the gate for `desktop_list_windows` (R0) and a
  consent-gated `desktop_screenshot` (R1) per INV-7.
- **v2 (M):** per-thread `revokeThread` UI surface, audit rotation, redaction-posture plumbing for
  R1, connection-origin assertion on `respondGrant` (reject bridge-origin), audit cross-rotation
  chain verification.
- **v3 (S–M):** opt-in **timed R2** grant behind double-confirm (consent-fatigue valve), pre-auth
  integration with `flatpak permission-set kde-authorized` for power users, signed (keyed) audit.
