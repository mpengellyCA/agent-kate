# 07 — Wiring & Roadmap (the unified contract)

The **reconciliation layer**. Six parallel slices (01–06) each designed a piece of Cowork against the
shared invariants (`00-context-pack.md`, INV-1..INV-7). This file de-duplicates their wire surfaces
into **one authoritative contract**, fixes the naming/shape collisions they introduced, pins the Go
package layout and the exact `main.go`/`mcp.go`/`agent.go`/CMake/`.desktop` edits, and lays out the
consolidated v1/v2/v3 roadmap, spike register, and trim list. **When this file and a slice disagree,
this file wins** (it post-dates them); each resolution cites the slice it overrides. Conforms to
INV-1..INV-7; deviations are flagged. `01-consent-spine.md` remains canonical for *consent semantics*
— this file only normalizes the *names and signatures* the other slices use to call it.

> **Director ratifications are RECORDED in `## 0. Decisions (ratified)` below** (2026-06-21). v1 scope
> is frozen; implementation proceeds against this contract once the user signs off on the overall plan.

---

## 0. Decisions (RATIFIED by the Director — 2026-06-21)

These resolve the open questions the Optimize phase surfaced. They are binding on implementation.

| # | Decision | Ruling | Rationale |
|---|---|---|---|
| C1 | `Authorize()` scope arg | **Scope is set at GRANT time** (user picks via `respondGrant`); callers pass only `SuggestedScope`; tier is derived by `TierOf(cap)`. Signature: `Authorize(ctx, AuthRequest)`. | The user — not the calling tool — chooses how broad/long a grant is. |
| Q1 | Separate vs folded MCP server | **Separate, opt-in `cowork` server, OFF by default** (`CoworkEnabled` / `--cowork` / `--allowedTools mcp__cowork`). | Security isolation + default-off matches the threat model (INV-5). |
| Q2 | v3 timed-R2 grant | **Allowed ONLY when sandbox-confined.** Outside the sandbox, R2 control stays strictly per-action, never remembered. | A timed control grant is only tolerable when blast radius is bounded to the sandbox VD. |
| Q3 | v1 audit integrity | **Hash-chain (tamper-evident) for v1.** Signed/keyed audit → v3 once a key-management story exists. | The chain catches truncation/mutation (the v1 threat) with no key store to secure. |
| Q4 | `respondGrant` origin assertion | **→ v2.** v1 safety rests on the structural fact that the bridge's MCP verb set never contains `cowork.respondGrant` — the agent literally cannot call it. | Connection-origin tagging is hardening, not a v1 hole. |
| Q5 | `remote_desktop` capability + temp-file PNG delivery | **Both CUT.** `input_inject` is the single v3 injection surface (`remote_desktop` kept only as a reserved, unused enum value); image delivery is base64 MCP image block only. | A synonym capability with no distinct gate is dead weight; an MCP consumer can't read a host file path. |

Also ratified (Optimize trim list, §6): HMAC node-tokens → v2; persistent KWin event script → v2;
per-thread sandbox VDs → v2; EIS → deferred/optional; the sandbox is an **organizational, not a
security, boundary** (the loud consent caveat is a hard v1 requirement).

---

## 1. Unified wire contract

### 1.0 Collisions resolved (read first)

The six slices introduced six divergent shapes for the same things. Resolutions (each overrides the
cited slice):

| # | Collision | Slices | Resolution (authoritative) |
|---|---|---|---|
| C1 | `Authorize()` signature: `(ctx,thread,cap,target)` (01) vs `…,scope)` (02) vs `…,tier)` (03) vs `AuthRequest{}` struct (04) | 01/02/03/04 | **`Authorize(ctx, AuthRequest) (Decision, error)`** with a struct (§1.5). **Scope is NOT a param** — the user picks scope at grant time via `respondGrant`; `Authorize`'s caller passes only a `SuggestedScope` hint. **Tier is NOT a param** — derived internally by `TierOf(cap)`. The struct subsumes 04's already-struct form. Drops 02's `scope` arg and 03's `tier` arg. |
| C2 | Kill-switch: dedicated `cowork.killSwitch` RPC+notify (01) vs `cowork.revokeGrant{all:true}` (06) | 01/06 | **`cowork.killSwitch{on,reason?}` only** (01). Delete 06's `revokeGrant{all:true}`. `revokeGrant` takes a single `{id}`. The kill-switch UI button calls `cowork.killSwitch{on:true}`. |
| C3 | Portal target enum: `TargetKind="vdesktop"` (01) vs `kind=="virtual_desktop"` (05) | 01/05 | **`TargetKind = "vdesktop"`** (01's const). Slice 05 updated to use `TargetVDesktop`/`"vdesktop"`. Also adds `TargetSandbox="sandbox"` for a sandbox-session-scoped grant (01 already has it). |
| C4 | Portal envelope: `{corrId, op}` (02) vs `{reqId, kind}` (04) vs `{requestId}` (06) | 02/04/06 | **ONE schema (§1.4)**: field is **`corrId`**, discriminator is **`kind`** (covers `screenshot`/`screencastStart`/`screencastStop`/`captureStill`/`inject`/`killInject`). Drops 02's `op`, 04/06's `reqId`/`requestId`. |
| C5 | Facade type name: `cowork.State` (03) vs `Authority` (01) vs `cowork.Service` (04) vs `SandboxState` (05) | 01/03/04/05 | **Two distinct types, one injected struct.** `cowork.Authority` = consent (Store+broker+audit+kill, 01). `cowork.State` = introspection cache (windows+coalescer+a11y cache, 03) **and** sandbox state. `handlerDeps` gets **one** field `cowork *cowork.Service` that embeds both `*Authority` and `*State` + the `*kde.Client`. Handlers reach everything through `d.cowork`. |
| C6 | `cowork.injectInput` target: bare `targetWindowId string` (04) vs `Target` (01) | 01/04 | Inject keeps **`targetWindowId string`** on the *RPC* (it needs a window for coord-clamping), but `Authorize`'s `AuthRequest.Target` is built from it as `Target{Kind:"window", WindowID:…}`. No new shape; the handler adapts. |
| C7 | a11y read RPC name: INV-5 says `cowork.snapshotA11y`; 03 uses `cowork.snapshotA11y` but also `cowork.findElement`/`readElementText` | 03 | Keep all three (03). Add to the canonical list. |

### 1.1 MCP tools (`desktop_*`, on the opt-in `cowork` server)

Verb-first per INV-5. Each tool's `runTool` case does **one** `b.client.Call/CallTimeout("cowork.<verb>", …)`.

| MCP tool | Purpose | → Core RPC | Capability / tier | v |
|---|---|---|---|---|
| `desktop_list_windows()` | live window set (caption/class/pid/geom) | `cowork.listWindows` | `window_list` / R0 | **v1** |
| `desktop_screenshot(target?, maxDim?, format?)` | still of window/screen/region as MCP **image block** | `cowork.screenshot` | `screenshot` / R1 | **v1** |
| `desktop_read_a11y_tree(pid?, maxDepth?, maxNodes?)` | bounded shallow a11y tree | `cowork.snapshotA11y` | `a11y_read` / R1 | v2 |
| `desktop_find_element(role?, name?, pid?)` | targeted bounded BFS → node refs | `cowork.findElement` | `a11y_read` / R1 | v2 |
| `desktop_read_element_text(node)` | one node's text (refuses password nodes) | `cowork.readElementText` | `a11y_read` / R1 | v2 |
| `desktop_start_screencast(target)` | begin cast → live preview; returns `castToken` | `cowork.startScreencast` | `screencast` / R1 | v2 |
| `desktop_stop_screencast(token)` | end a cast | `cowork.stopScreencast` | — (teardown) | v2 |
| `desktop_open_sandbox()` | create/find sandbox VD; ensure `vd_sandbox` grant | `cowork.openSandbox` | `vd_sandbox` / R0 | v2 |
| `desktop_use_sandbox()` | switch user's view to the sandbox VD (visible act) | `cowork.useSandbox` | `vd_sandbox` / R0 | v2 |
| `desktop_launch_in_sandbox(app, args?)` | launch app + move onto sandbox VD | `cowork.launchInSandbox` | `vd_sandbox` / R0 | v2 |
| `desktop_click_element(node, action?)` | invoke a named a11y action (sandbox-confined first) | `cowork.doElementAction` | `a11y_action` / **R2** | v2 |
| `desktop_inject_input(events, targetWindowId)` | raw keyboard/pointer via RemoteDesktop portal | `cowork.injectInput` | `input_inject` / **R2** | v3 |

`cowork.captureStill` and `cowork.sandboxWindows` are **internal** RPCs (no direct MCP tool) — the
first rides an active cast for `desktop_screenshot`, the second feeds the UI window list.

### 1.2 Core RPC methods (`cowork.*`, agent/bridge → core unless noted)

| RPC | Params → Result | Owner slice | v |
|---|---|---|---|
| `cowork.listWindows` | `{}` → `{windows:[]Window}` (R0 gate) | 03 | v1 |
| `cowork.screenshot` | `{threadId,target?,maxDim?,format?}` → image artifact (R1 gate + portal round-trip) | 02 | v1 |
| `cowork.requestGrant` | `{threadId,capability,target,suggestedScope}` → `{requestId}` (UI pre-grant affordance) | 01 | v1 |
| `cowork.respondGrant` | `{requestId,allow,scope,expiresInSec?,redact?}` (**UI→core**) → `{ok:true}` | 01 | v1 |
| `cowork.listGrants` | `{threadId?}` → `{grants:[]Grant, killed:bool}` | 01 | v1 |
| `cowork.revokeGrant` | `{id,reason?}` → `{ok,grant:Grant}` (single id only — see C2) | 01 | v1 |
| `cowork.revokeThread` | `{threadId,reason?}` → `{revoked:[]id}` | 01 | v2 |
| `cowork.killSwitch` | `{on:bool,reason?}` → `{ok,revoked:[]id}` (idempotent; `on:false` re-arms) | 01 | v1 |
| `cowork.listAudit` | `{threadId?,sinceSeq?,limit?}` → `{entries:[]AuditEntry,nextSeq}` | 01 | v1 |
| `cowork.portalResult` | `{corrId,kind,ok,…artifacts}` (**UI→core**) → `{ok:true}` (resolves portalBroker) | 02 | v1 |
| `cowork.snapshotA11y` | `{pid?,maxDepth?,maxNodes?}` → `{root:A11yNode,truncated}` (R1) | 03 | v2 |
| `cowork.findElement` | `{pid?,role?,name?}` → `{matches:[]A11yNode}` (R1) | 03 | v2 |
| `cowork.readElementText` | `{ref}` → `{text,redacted}` (R1; password-refusing) | 03 | v2 |
| `cowork.startScreencast` | `{threadId,target}` → `{castToken}` (R1 + portal round-trip) | 02 | v2 |
| `cowork.stopScreencast` | `{token}` → `{ok}` | 02 | v2 |
| `cowork.captureStill` | `{castToken,maxDim?,format?}` → image artifact (rides screencast grant) | 02 | v2 |
| `cowork.openSandbox` | `{threadId}` → `{vdId,vdName,number,created}` | 05 | v2 |
| `cowork.useSandbox` | `{threadId}` → `{ok}` | 05 | v2 |
| `cowork.launchInSandbox` | `{threadId,app,args?}` → `{pid,matched,windowId?}` | 05 | v2 |
| `cowork.sandboxWindows` | `{threadId}` → `{windows:[]Window}` (filters 03's snapshot) | 05 | v2 |
| `cowork.closeSandbox` | `{threadId,closeApps?,removeVd?}` → `{closed,removedVd}` | 05 | v2 |
| `cowork.doElementAction` | `{threadId,node,action?}` → `{ok,result}` (R2 imperative gate) | 04 | v2 |
| `cowork.injectInput` | `{threadId,targetWindowId,events}` → `{ok}` (R2 + inject round-trip) | 04 | v3 |

**Naming note:** `cowork.requestGrant` is invoked *internally* by each tool handler via `Authorize`
(not by the agent over MCP); it is exposed as an RPC only so the UI's proactive "Share…" affordance
can open one. The agent has **no** MCP tool that calls it directly.

### 1.3 Notifications (core → UI broadcast; lossy → all are re-derivable via a pull RPC)

| Notification | Params | Producer | Consumer | Re-derive via | v |
|---|---|---|---|---|---|
| `cowork.grantRequested` | `GrantRequest{requestId,threadId,threadTitle,capability,riskTier,target,actionPreview,suggestedScope,defaultScope,expiresAtHint?}` | core `Authorize` miss | UI consent dialog | (event-driven; re-sent on retry) | v1 |
| `cowork.grantsChanged` | `{threadId?}` (hint only) | core store onChange | UI active-grants | `cowork.listGrants` | v1 |
| `cowork.killSwitch` | `{on:bool,reason,at}` | core `Kill`/`Rearm` | UI banner + portal teardown | `cowork.listGrants.killed` | v1 |
| `cowork.portalRequest` | **§1.4 envelope** | core (any portal-needing handler) | UI primary portal runner | (timeout-bounded; not re-derivable — fail-closed) | v1 |
| `cowork.windowEvent` | `{events:[{kind,id,pid}]}` coalesced ≥25 ms | core KWin event pump | UI live window picker | `cowork.listWindows` | v2 |
| `cowork.sandboxChanged` | `{threadId?}` (hint only) | core sandbox lifecycle | UI sandbox view | `cowork.sandboxWindows` | v2 |
| `cowork.sessionChanged` | `{threadId,state}` (`active`/`ended`) | core session lifecycle | UI preview/share state | `cowork.listGrants` | v2 |

**Cut from INV-5's provisional list:** none added beyond these seven. `cowork.sessionChanged` is
demoted to v2 (v1 needs only grant/kill/portal). The slices' ad-hoc names (`cowork.sandboxWindows` as
a "notification" in 05 — it's a *pull RPC*, fixed above) are normalized here.

### 1.4 The portal round-trip envelope (THE single schema — supersedes 02/04/06)

Core borrows the UI to run a portal (INV-1/INV-5). **One notification out, one call back in**, keyed
by `corrId`. This replaces 02's `{corrId,op}`, 04's `{reqId,kind}`, and 06's `{requestId}`.

**`cowork.portalRequest`** (core → UI **notification**, best-effort):
```jsonc
{
  "corrId":   "cap-<6B hex>",        // minted by portalBroker.Open(); echoed in result
  "kind":     "screenshot" | "screencastStart" | "screencastStop" |
              "captureStill" | "inject" | "killInject",
  "threadId": "<thread>",
  "target":   Target,                // §1.5 (window/app/screen/region/vdesktop/sandbox/any)
  "interactive": bool,               // screenshot: use portal dialog vs ScreenShot2
  "parentHint":  "<caption>",        // optional, for dialog anchoring
  "restoreToken": "<token>",         // screencastStart: feed back saved persistent token (rotated)
  "castToken":   "<handle>",         // screencastStop/captureStill: which live cast
  "absEvents":   [ InjectEvent ],    // inject: window-local events, already gated+allowed
  "maxDim": int, "format": "png"|"jpeg"   // screenshot/captureStill sizing (Agent 2)
}
```

**`cowork.portalResult`** (UI → core **call**, reliable — non-nil id, never shed):
```jsonc
{
  "corrId": "cap-<6B hex>",          // REQUIRED — matches the request
  "kind":   "<same as request>",
  "ok":     bool,
  "error":  "<string>",              // when !ok
  // screenshot / captureStill:
  "pngB64": "<base64>", "mime": "image/png", "width": int, "height": int,
  // screencastStart:
  "nodeId": int, "castToken": "<handle>", "restoreToken": "<rotated token>",
  // inject / screencastStop / killInject: just {ok,error?}
}
```

**Discriminator (`kind`) covers every portal need across all slices:**
- `screenshot` (02) — one-shot still, returns `pngB64`.
- `screencastStart` (02) — `CreateSession→SelectSources→Start→OpenPipeWireRemote`, returns `nodeId`+`castToken`+rotated `restoreToken`; FD stays UI-side.
- `screencastStop` (02) — stop one cast.
- `captureStill` (02) — grab current frame from a live cast, returns `pngB64`.
- `inject` (04) — UI runs RemoteDesktop `Notify*` for the *already-gated-allow* events.
- `killInject` (04) — close the RemoteDesktop session immediately (kill-switch path).

**`corrId`** (not `reqId`/`requestId`) is the universal correlation key, minted by
`portalBroker.Open()` (clone of `permission.Broker`, lives in `kde/portalcoord.go`). Re-delivered
notifications are idempotent (UI dedupes on `corrId`); a late result after timeout is a harmless
no-op (`Resolve` returns false-ignored).

**Staggered-timeout ladder (longest outermost, all fail-closed):**

| Layer | Budget | Site |
|---|---|---|
| MCP bridge `CallTimeout` | **130 s** (capture) / **35 s** (inject) | `mcp.go` runTool case |
| Core handler portalBroker wait | **125 s** / **30 s** | `cowork.screenshot` / `cowork.injectInput` |
| Portal-internal timeout | **120 s** | UI `PortalSession` (skill snippet) |
| UI op budget | **~115 s** | UI portal D-Bus deadline |

Inject uses the short ladder (35>30) because it's an imperative command, not a slow capture.
Consent prompts use the spine's own ladder (bridge 10 min > R0/R1 prompt 5 min > R2 prompt 3 min,
01 §3) — **independent** of this portal ladder. **No UI connected** ⇒ `Notify` reaches zero conns ⇒
handler times out ⇒ `RPCError("no UI available to run portal")`. **Primary-UI-only** runs the portal
(the UI owning the akcore QProcess sets `primary:true` at handshake; secondaries ignore
`cowork.portalRequest`) to avoid double-dialogs.

### 1.5 Canonical types (the enums every slice shares)

```go
// Capability — the complete set; tier is derived, never stored as a param.
type Capability string
const (
    CapWindowList    Capability = "window_list"   // R0
    CapVDSandbox     Capability = "vd_sandbox"     // R0
    CapA11yRead      Capability = "a11y_read"      // R1
    CapScreenshot    Capability = "screenshot"     // R1
    CapScreencast    Capability = "screencast"     // R1
    CapA11yAction    Capability = "a11y_action"    // R2
    CapInputInject   Capability = "input_inject"   // R2
    CapRemoteDesktop Capability = "remote_desktop" // R2 (reserved; input_inject is the v3 surface)
)
func TierOf(c Capability) string // R0|R1|R2 — the fixed table (01 §4)

// TargetKind — MUST cover every target every slice uses. (C3: "vdesktop" wins.)
type TargetKind string
const (
    TargetWindow   TargetKind = "window"   // KWin internalId (QUuid)
    TargetApp      TargetKind = "app"       // resourceClass
    TargetScreen   TargetKind = "screen"
    TargetRegion   TargetKind = "region"    // a screen rect
    TargetVDesktop TargetKind = "vdesktop"  // virtual-desktop id (sandbox lives here)
    TargetSandbox  TargetKind = "sandbox"   // a vd_sandbox SESSION id (distinct from the VD)
    TargetAny      TargetKind = "any"       // window_list — no spatial target
)
// Target struct exactly as 01 §1 (WindowID/ResourceClass/Screen/Region/VDesktopID/SandboxID/Label).

// AuthRequest — the SINGLE Authorize input (C1: subsumes 02's scope arg + 03's tier arg + 04's struct).
type AuthRequest struct {
    ThreadID      string
    Capability    Capability
    Target        Target
    SuggestedScope Scope            // hint for the prompt; the USER picks the real scope via respondGrant
    ActionPreview *ActionDescriptor // R2 only: the concrete literal action (04 §3); nil for R0/R1
}
func (a *Authority) Authorize(ctx context.Context, req AuthRequest) (Decision, error)
```

Every slice's call site rewrites to `Authorize(ctx, AuthRequest{...})`. Sandbox confinement (05) is a
post-`Match` predicate inside `Authorize` when `Target.Kind ∈ {vdesktop,sandbox}` —
`SandboxGuard.Check(target)` re-reads live VD membership (time-of-check==time-of-use). `Grant`,
`Scope`, `Decision`, `AuditEntry`, `A11yNode`, `Window`, `ActionDescriptor` are exactly as their
owning slices define them (01/03/04) — unchanged here.

---

## 2. Go package layout

```
core/internal/kde/                 # raw D-Bus; ZERO consent logic; one shared session bus
  client.go        # kde.Client: owns the godbus session *dbus.Conn, RequestName
                   #   "io.agentkate.Cowork.akcore_<pid>", Export(/WindowList,/Events,/Sandbox),
                   #   lazy a11y *dbus.Conn (second bus). New(log)/Close(). SHARED by kwin+atspi+
                   #   portalcoord+sandbox (C5 — one connection, not four).
  kwin.go          # window enumeration (one-shot script), event script (persistent), VD mutators
                   #   (CreateOrFindSandboxVD/MoveWindowToVD/SetCurrentVD/RemoveVD), ToScreenCoords.
  atspi.go         # AT-SPI bootstrap (a11y bus addr, IsEnabled), bounded walk, DoAction, redaction.
  portalcoord.go   # portalBroker (clone of permission.Broker; payload portalResult). NO portal D-Bus
                   #   here — portals are UI-side; this only correlates the round-trip.

core/internal/cowork/              # consent authority + state; the policy brain
  grants.go        # Grant/Capability/Scope/Target/TargetKind, Store (atomic, restart semantics,
                   #   migration, SweepExpired), 0o600/0o700.
  consent.go       # Authority (Store+grantBroker+audit+killed+teardown registry), Authorize(),
                   #   Kill/Rearm/Killed, TierOf, IsSelfTarget.
  audit.go         # AuditEntry, append-only JSONL hash-chain, rotation, listAudit tail.
  state.go         # State: window cache + event coalescer (port of agent.go:158-236) + a11y tree
                   #   cache. Copy-out on read. (C5: introspection cache, distinct from Authority.)
  sandbox.go       # SandboxState (vdId/launchedPids/eventScriptId), SandboxGuard.Check predicate,
                   #   cowork-sandbox.json persistence.
  service.go       # Service: embeds *Authority + *State + *kde.Client + *SandboxState. The ONE thing
                   #   handlerDeps holds. Close() tears down everything in order.
  *_test.go        # grants/consent/restart/audit/killswitch/coalescer/walk/sandbox-guard.

go.mod: + github.com/godbus/dbus/v5 v5.1.0   (verify go 1.26 build; SPIKE-GODBUS)
```

**The single shared session-bus connection.** `kde.Client.session *dbus.Conn` is opened **once** in
`New()` (`dbus.ConnectSessionBus()`), used by: KWin scripting (03), VD mutators + sandbox scripts
(05), `RequestName`/`Export` for the script-callback round-trip (03/05), and portal *coordination*
(the broker — note portals themselves run UI-side). The **AT-SPI second bus** (`a11y *dbus.Conn`,
dialed from `org.a11y.Bus.GetAddress`) is also owned by `kde.Client`, lazily on first a11y use. Both
close in `Service.Close()`. No slice opens its own connection — slices 03/05 explicitly say "share one
`kde.Client`"; this enforces it.

---

## 3. Exact wiring points

### 3.1 `core/cmd/akcore/main.go`

**`handlerDeps` (`:419-434`)** — add one field (C5):
```go
cowork *cowork.Service   // embeds Authority + State + kde.Client + SandboxState
```
**`main()`** — construct beside `sessions`/`broker` (near `coopState := coop.NewState()`, `:183`):
```go
coworkSvc, err := cowork.New(cowork.DefaultGrantsPath(), log)   // load store, applyRestartSemantics
// onChange = debounced srv.Notify("cowork.grantsChanged", …); grantRequested via srv
coworkSvc.SetNotifier(srv)
safe.Go("cowork.sweepExpired", coworkSvc.RunSweepTicker)        // 30s ticker → SweepExpired
```
**`registerHandlers` (`:437+`)** — add `d.srv.Handle("cowork.…", fn)` lines. **v1 set:**
```go
d.srv.Handle("cowork.listWindows",  …)  // → d.cowork.Authorize(window_list,R0) → kde.ListWindows
d.srv.Handle("cowork.screenshot",   …)  // → Authorize(screenshot,R1) → portal round-trip
d.srv.Handle("cowork.requestGrant", …)
d.srv.Handle("cowork.respondGrant", …)  // UI→core; resolves grantBroker
d.srv.Handle("cowork.listGrants",   …)
d.srv.Handle("cowork.revokeGrant",  …)
d.srv.Handle("cowork.killSwitch",   …)
d.srv.Handle("cowork.listAudit",    …)
d.srv.Handle("cowork.portalResult", …)  // UI→core; resolves portalBroker
```
v2 adds `snapshotA11y/findElement/readElementText/startScreencast/stopScreencast/captureStill/
revokeThread/openSandbox/useSandbox/launchInSandbox/sandboxWindows/closeSandbox/doElementAction`;
v3 adds `injectInput`. **All static** — the handler map is not goroutine-safe post-`Serve`.

**`gracefulShutdown` (`:302-323`)** — after `_ = gitCache.Close()`:
```go
progress("cowork", "", 0, 0)
_ = coworkSvc.Close()   // tombstone session/once/timed grants, run teardown hooks, stop event
                        // script (kde.Client), release bus name, close both *dbus.Conn, stop tickers
```

**`writeMCPConfig` (`:2778`)** — add the SECOND, opt-in MCP server. Signature gains a `coworkEnabled
bool` (read from `record.CoworkEnabled`); only when true:
```go
mcpServers := map[string]any{ "cooperation": {…existing…} }
if coworkEnabled {
    mcpServers["cowork"] = map[string]any{
        "type": "stdio", "command": exePath,
        "args": []string{"mcp", "--socket", socketPath, "--thread", threadID,
                         "--workspace", workspace, "--cowork"},   // --cowork flips the bridge tool set
    }
}
```

### 3.2 `core/internal/session/session.go`

Add to `Record` (`:31-56`): `CoworkEnabled bool \`json:"coworkEnabled,omitempty"\`` — default false
(off by default, INV-5). Set per-thread/workspace from the UI (a new `session.setCowork` RPC or a
field on `agent.start`). Persisted atomically with the rest of the record.

### 3.3 `core/cmd/akcore/mcp.go`

- **`toolDefs()` (`:446-571`)** — append `{name,description,inputSchema}` for the v1 tools
  (`desktop_list_windows`, `desktop_screenshot`), v2/v3 tools later. **Only emitted when the bridge ran
  with `--cowork`** (the bridge filters its catalogue by flag).
- **`runTool` `switch name` (`:132-442`)** — add a `case` per tool: unmarshal args → one
  `b.client.CallTimeout("cowork.<verb>", args, &res, <ladder>)` → return string (or, for
  `desktop_screenshot`, an **MCP image content block** — add a `toolImageResult(b64, mime)` helper
  beside `toolResult` (`:573`) returning `{"content":[{"type":"image","source":{"type":"base64",
  "media_type":…,"data":…}}],"isError":false}`).
- The R2 tool descriptions state: *"Requires explicit per-action human approval every time; cannot be
  pre-authorized."* (04 §7.)

### 3.4 `core/internal/agent/agent.go`

`buildArgs` (`:280-316`): when the thread is cowork-enabled, append `mcp__cowork` to `--allowedTools`
(force-allowed at the Claude layer like `cooperation`, so the spine — not the prompt funnel — is the
gate; 01 §4). The `--mcp-config` already points at the file `writeMCPConfig` produced.
```go
allowed := "mcp__cooperation"
if opts.CoworkEnabled { allowed += ",mcp__cowork" }
args = append(args, "--allowedTools", allowed)
```

### 3.5 `ui/CMakeLists.txt`

`find_package` (`:10`): `... COMPONENTS Core Gui Widgets Network DBus WaylandClient` and a new
`find_package(KPipeWire REQUIRED)`. `target_link_libraries` (`:86-106`): add `Qt6::DBus`
`Qt6::WaylandClient` `Qt6::GuiPrivate` `KPipeWire::KPipeWire`. Add sources:
`src/cowork/{CoworkPanel,CoworkPortal,WindowHandle,ControlConsentDialog,ActiveGrantsView,CoworkPreview,
CoworkAuditView}.cpp` (AUTOMOC handles `Q_OBJECT`). **`Qt6::Quick/Qml/QuickWidgets` only if
SPIKE-PREVIEW lands on option 2** — not in v1.

### 3.6 `ui/org.kde.agentkate.desktop`

Add: `X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2`. **Only effective for the installed
app** — build-dir dogfooding gets `NoAuthorized`; the portal `Screenshot` (dialog) path is the
build-dir fallback and the v1 default. Acceptance tests run a `make install`'d build for the fast path.

### 3.7 `ui/src/MainWindow.{h,cpp}` + `main.cpp`

- `MainWindow.h` (`:132`): add `QString m_keyCowork;`.
- `MainWindow.cpp` (near the Cooperation stub `:213`): `registerPanel(m_keyCowork,
  QIcon::fromTheme("video-display"), i18n("Cowork"), new CoworkPanel(m_core, this),
  QStringLiteral("right"));`.
- `ui/src/main.cpp`: `qputenv("QT_ACCESSIBILITY", "1");` **before** `QApplication` construction (so
  AK's own window is a11y-introspectable for dogfooding; 03 §3).

---

## 4. Consolidated roadmap (every sub-capability → v1/v2/v3 · S/M/L)

> Size key: **S** ≈ <½ day, **M** ≈ 1–2 days, **L** ≈ 3–5 days.

| Sub-capability | Slice | v1 | v2 | v3 | Size |
|---|---|:--:|:--:|:--:|---|
| Build wiring (CMake, godbus dep, `.desktop`, panel reg) | 02/03/06 | ✅ | | | S |
| Opt-in `cowork` MCP server (`writeMCPConfig` + `--cowork` + `--allowedTools` + `CoworkEnabled`) | 01 | ✅ | | | S |
| Grant store + `Authorize` + grant broker + restart semantics + migration | 01 | ✅ | | | L |
| Audit JSONL + hash chain | 01 | ✅ | | | M |
| Kill-switch + teardown registry + self-target/anti-escalation guards | 01 | ✅ | | | M |
| Audit **rotation** + cross-rotation chain verify | 01 | | ✅ | | S |
| `revokeThread` (per-thread blast-radius UI) | 01 | | ✅ | | S |
| `respondGrant` connection-origin assertion (reject bridge-origin) | 01 | | ✅ | | S |
| Signed (keyed) audit | 01 | | | ✅ | M |
| Opt-in **timed R2** grant (consent-fatigue valve, sandbox-only) | 01/04 | | | ✅ | S–M |
| `desktop_list_windows` (KWin one-shot script, **no portal**) | 03 | ✅ | | | M |
| KWin **event stream** + coalescer + `cowork.windowEvent` | 03 | | ✅ | | M |
| AT-SPI bootstrap + bounded walk + `read_a11y_tree`/`find_element`/`read_element_text` | 03 | | ✅ | | L |
| Coord reconciliation (`ToScreenCoords`, window-local→absolute) | 03 | | ✅ | | S |
| Portal round-trip skeleton (`portalBroker` + `portalRequest`/`portalResult` + envelope) | 02 | ✅ | | | M |
| `parent_window`: X11 branch + Wayland `""` fallback | 02/06 | ✅ | | | S |
| `parent_window`: proper Wayland xdg-foreign export | 02/06 | | ✅ | | S |
| `desktop_screenshot` (ScreenShot2 + portal, sizing/guard, image block, redaction) | 02 | ✅ | | | M |
| Lifecycle/teardown (OnAllClientsGone, kill-switch, gracefulShutdown broadcast) | 02 | ✅ | | | S–M |
| `desktop_start/stop_screencast` + KPipeWire preview + `captureStill` + token rotation | 02/06 | | ✅ | | L |
| Live preview widget (`CoworkPreview`, PipeWire→QImage) [SPIKE-PREVIEW] | 06 | | ✅ | | M |
| VD sandbox: open/use/launch + SandboxGuard (membership re-read) + conservative close | 05 | | ✅ | | M |
| Whole-VD single-stream screencast (`types`=Virtual) + KWin window rule | 05 | | ✅ | | M |
| Per-thread sandbox VDs | 05 | | | ✅ | S |
| True isolation (nested `kwin_wayland` / separate user / container) | 05 | | | ✅ | L |
| `desktop_click_element` (AT-SPI `DoAction`, R2 gate, **sandbox-confined first**) | 04 | | ✅ | | M |
| `ControlConsentDialog` (R2 distinct widget, typed-phrase) | 06/04 | ✅* | | | M |
| `desktop_inject_input` (RemoteDesktop `Notify*`, R2, coord-clamp, inject round-trip) | 04 | | | ✅ | L |
| EIS/libei swap behind `cowork.injectInput` | 04 | | | ✅ | M |
| Cowork panel: active-grants + audit view + kill-switch button + status | 06 | ✅ | | | M |
| Consent dialogs: R0 banner + R1 Dangerous dialog | 06 | ✅ | | | M |

`*` **`ControlConsentDialog` ships in v1 as a *shell*** (the distinct R2 widget class, typed-phrase
gate, literal-action renderer, AK-self refusal) so the R2 surface exists and is reviewed early — but
**no R2 tool is wired to it until v2** (`desktop_click_element`). This satisfies INV-7's "R2
ControlConsentDialog shell" without shipping control in v1.

### 4.1 The v1 walking skeleton (AMENDED by the review — see `08-review-findings.md` §G)

> **⚠️ The adversarial review materially revised this scope.** The list below is the *original* frozen
> skeleton; `08-review-findings.md` §G is now authoritative and adds: a **per-connection identity/role
> registry (the keystone)**, **origin-checked UI-only RPCs** (pulled forward from v2), a **capability
> probe + graceful "unavailable"** path, **server-derived grant provenance + audit tamper-detection**,
> **flock for concurrent instances**, and a user-facing **enable control + first-run risk education**.
> Revised v1 sizing: **L→XL (≈5–8 days)**. Read §G before implementing.

v1 proves the riskiest seams cheaply and ships **read + screenshot only, no control**:

1. **Build system + `godbus` dep + opt-in `cowork` MCP server** (`CoworkEnabled` flag, `--cowork`,
   `--allowedTools mcp__cowork`, off by default).
2. **Consent spine**: grant store (atomic, restart semantics, migration), `Authorize`, grant broker
   rendezvous, the 9 v1 RPCs + 4 v1 notifications, audit JSONL with hash chain, kill-switch +
   self-target/anti-escalation guards, full test suite (`-race`).
3. **`desktop_list_windows`** via KWin one-shot D-Bus script from Go — **no portal**, R0-gated.
4. **`desktop_screenshot`** consent-gated (R1) via the **portal round-trip** (core→UI
   `cowork.portalRequest{kind:"screenshot"}` → UI runs ScreenShot2/portal → `cowork.portalResult`),
   returned as an MCP image block; downscale ≤1568 px, 8 MiB pre-flight guard; AK-window + sensitive-
   window exclusion.
5. **Cowork panel**: active-grants list (live, re-derived from `cowork.listGrants`), the global
   kill-switch button (→ `cowork.killSwitch{on:true}`), R0 banner + R1 Dangerous consent dialog,
   status line. `parent_window` = X11 branch + Wayland `""` fallback.
6. **`ControlConsentDialog` shell** (R2 widget class, typed-phrase, literal-action render, AK-self
   refusal) present and reviewable — **wired to no R2 tool yet**.

**Explicitly NOT in v1:** a11y tree, screencast/live preview, sandbox, AT-SPI actions, raw input
injection, EIS, signed audit, per-thread sandbox VDs, audit rotation, window-event stream.

---

## 5. Spike register (consolidated; resolve before the gated phase)

| ID | Spike | Why it matters | Owner slice | Resolve before |
|---|---|---|---|---|
| SPIKE-GODBUS | `github.com/godbus/dbus/v5 v5.1.0` builds clean on **go 1.26** | the whole Go D-Bus layer depends on it | 02/03/05 | **v1** (gate-0) |
| SPIKE-XDGEXPORT | exact Qt 6.11 **xdg-foreign export** private symbol (`QtWaylandClientPrivate`) | proper `parent_window` on Wayland; v1 uses `""` fallback if unstable | 02/06 | v2 (v1 has fallback) |
| SPIKE-PREVIEW | KPipeWire **SHM-vs-dmabuf**: does `setAllowDmaBuf(false)` yield CPU `QImage` frames, or must we use `DmaBufHandler`/EGL or `QQuickWidget` | live preview in a no-QtQuick QtWidgets app | 06 | v2 |
| SPIKE-CALLBACK | KWin `callDBus` ↔ godbus `RequestName`/`Export` round-trip actually delivers (script→`Report`/`WindowEvent`) | the entire window-list + event path uses it | 03 | **v1** (`desktop_list_windows` depends on it) |
| SPIKE-VDARITY | `workspace.createDesktop(position,name)` arity on the running Plasma | sandbox VD creation | 05 | v2 |
| SPIKE-VIRTSRC | ScreenCast `types` bit-4 (**Virtual**) detection (Plasma ≥6.5) + per-window fallback | whole-sandbox single-stream cast | 05 | v2 |
| SPIKE-A11YAPPS | a11y tree availability for **Firefox/Chrome/LibreOffice/VSCode** on Wayland Plasma 6 (which export usable trees) | bounds what `desktop_read_a11y_tree` can ever see | 03 | v2 |
| SPIKE-TOCTOU | node `ref` re-resolution: re-read role+name+extents at action time, abort on mismatch | R2 a11y action TOCTOU defense | 04 | v2 |
| SPIKE-65INPUT | Plasma 6.5.x portal-dialog **virtual-input bug** + the `flatpak permission-set kde-authorized` pre-auth path | first R2/screencast consent on 6.5.x needs a physical click | 02/04 | v3 (host is 6.7; portability note) |
| SPIKE-COORD | a11y window-local origin vs KWin frame inset (empirical) for screen-absolute mapping | injection/click aiming correctness | 03 | v2 |

**v1-blocking spikes: SPIKE-GODBUS and SPIKE-CALLBACK only.** Everything else gates a later phase.

---

## 6. Trim list (defer/cut — keep v1 minimal, INV-7)

| Item | Action | Justification |
|---|---|---|
| HMAC/signed **node-tokens** (04 §1: `sig` over the descriptor) | **→ v2** | v2 a11y actions re-resolve + re-validate the node and re-check pid/grant at action time (TOCTOU defense) — that already defeats fabrication. The HMAC is belt-on-belt; add it when injection (v3) raises the stakes. v1/v2 use an opaque base64 `{bus_name,object_path}` with server-side re-resolution, no key. |
| **Signed (keyed) audit** | **→ v3** | v1 ships the hash chain (tamper-evident, no key store). Keyed signing needs a key-management story that doesn't exist yet; the chain catches truncation/mutation, which is the v1 threat. |
| **Persistent KWin event script** (03 §2) | **→ v2** | v1's `desktop_list_windows` is a one-shot `loadScript→run→stop`. The resident `windowAdded` firehose + coalescer + `cowork.windowEvent` only feeds the UI's live picker — pure v2. Cuts a whole resident-script lifecycle from v1. |
| **`respondGrant` connection-origin assertion** (01 §7) | **→ v2** | v1 relies on the structural fact that the bridge's verb set simply never includes `cowork.respondGrant` (it's not in the MCP tool list) — the agent *cannot* call it. Tagging connections + rejecting bridge-origin is hardening for v2, not a v1 hole. |
| **Per-thread sandbox VDs** (05 §6) | **→ v2/v3** | v1 has no sandbox at all; even when sandbox lands (v2) one shared VD is simplest. Per-thread VDs are a multi-agent-attribution nicety, not a safety requirement (grants are already per-thread). |
| **EIS/libei input path** (04 §2) | **deferred (v3+, optional)** | `Notify*` (pure D-Bus, no cgo/FD/libei) covers agent-driven discrete clicks fine. EIS only wins for streamed/smooth input; ship it iff a use-case demands it, as a non-breaking UI-internal swap. |
| **Temp-file PNG path delivery** (02 §3) | **→ v3 (likely cut)** | The model can't read an arbitrary host path through MCP; base64 image block is strictly more useful for the common case. Only revisit for very-large/batched captures. Lean toward cutting entirely. |
| **KWin window rule for sandbox launch** (05 §3) | **→ v2** | v1 sandbox (when it lands) accepts the brief pre-move flash (capture is membership-gated, so a pre-move frame is never captured). The rule mutates persistent `~/.config/kwinrulesrc` — defer the config-write complexity. |
| **`cowork.sessionChanged` notification** | **→ v2** | v1 needs only grant/kill/portal notifications; session lifecycle surfacing is a v2 UI polish. |
| **Master capability toggles** in the panel (06 §A.1a) | **trim to status-only in v1** | They are "UI affordances only, not the gate" — a v1 status line + active-grants list conveys posture without the toggle machinery. Add toggles in v2 if users want them. |
| **`desktop_read_element_text` as a separate tool** | **keep, but v2** | Folding it into `find_element` was considered; keep it separate (clean per-node text fetch with password-refusal) but it's v2 with the rest of a11y. |

**Recommend cutting entirely (not just deferring):** the **temp-file PNG path** (base64 image block
makes it dead weight for an MCP consumer) and the **`remote_desktop` capability as a distinct enum
value** — `input_inject` is the single v3 control-injection surface; `remote_desktop` adds a synonym
with no distinct gate. Keep it reserved in the enum but ship no tool/RPC for it.

---

## 7. Cross-references

- Consent semantics (grant model, audit, kill-switch, R2 gate) — **`01-consent-spine.md`** (canonical).
- Portal/PipeWire/parent_window — **`02-capture.md`** (UI executor) + **`06-ui-panel.md`** (C++ side).
- Window model + coord reconciliation + a11y — **`03-introspection.md`**.
- R2 control + anti-escalation — **`04-control.md`**.
- Sandbox confinement + honest isolation caveat — **`05-sandbox.md`**.
- Invariants — **`00-context-pack.md`** (INV-1..INV-7).
