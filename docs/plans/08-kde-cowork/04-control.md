# 04 — Control layer: AT-SPI semantic actions + RemoteDesktop input injection

The highest-risk slice. Control lets the agent *act as the user*: click a button, type text,
move the pointer. We split it into two mechanisms with very different risk/auditability profiles
and make the **safer one (AT-SPI semantic actions) the default**, the **dangerous one (raw portal
input injection) an explicit escape hatch**. Both are Tier **R2**: default-off, per-action by
default, strong/distinct confirmation UI, never auto-remembered, and gated *imperatively inside the
Go tool* so `--permission-mode` can never enable them. This file owns the two R2 MCP tools, their
`cowork.*` RPCs, the R2 gating flow, the anti-escalation enforcement points, audit, and kill-switch
integration.

## Current state

- **The broker is per-call, ephemeral, default-deny-on-timeout** — `permission.Broker.Open()` →
  `perm-<hex>` + buffered(1) `chan Decision`; `Resolve(id,Decision{Allow,UpdatedInput})`; `Close(id)`
  (`core/internal/permission/broker.go:35-67`). No scopes, no persistence, no distinct risk levels.
  We do **not** reuse the broker for R2 grants — INV-2/INV-3 require the new capability-scoped Grant
  store (Agent 1). We reuse only its **request/notify/respond rendezvous shape** for the per-action
  confirmation prompt.
- **The dominant gate today is Claude Code's `--permission-mode` (default `acceptEdits`)**, not the
  broker (`core/internal/agent/agent.go:285`). Cooperation MCP is force-allowed via
  `--allowedTools mcp__cooperation` (`agent.go:288`); gated tools route to
  `--permission-prompt-tool mcp__cooperation__request_permission` (`agent.go:314`). **This is exactly
  why INV-3 R2 forbids relying on the prompt funnel: a future `--permission-mode bypassPermissions`
  or an `--allowedTools mcp__cowork` token would silently auto-allow our tool.** The R2 gate must be
  an unconditional call inside `runTool`, executed *before* any D-Bus side effect, regardless of what
  Claude decided.
- **MCP tool = two edits in `core/cmd/akcore/mcp.go`** — append to `toolDefs()` (`mcp.go:446-571`),
  add a `case` to `runTool`'s `switch name` (`mcp.go:132-442`) that unmarshals args, calls
  `b.client.Call("ns.method", …)` / `CallTimeout(...)` into core, returns a `string`. Result shape
  fixed by `toolResult` (`mcp.go:573-578`). The bridge handles each tool concurrently on its own
  `safe.Go("mcp.handle",…)` (`mcp.go:80`), so a blocking R2 confirmation does not stall other traffic.
- **Per-tool approval round-trip exists and is the template**: bridge `CallTimeout("permission.request",
  {threadId,toolName,input}, &res, 10*time.Minute)` (`mcp.go:416-418`) → core handler `broker.Open()`
  + `srv.Notify("permission.requested",…)` + **blocks 8 min** then self-denies (`main.go:1218-1245`) →
  UI banner → `permission.respond` → `broker.Resolve` (`main.go:1247-1259`). **Core's 8 min < bridge's
  10 min** so the caller always gets a definitive answer; fail-closed on transport error
  (`mcp.go:419`). We mirror this timing discipline for R2 but with a *distinct* notification +
  *distinct* widget.
- **`srv.Notify(method,params)` broadcasts to every client; notifications are lossy under backpressure**
  (`server.go:197-216,252-290`). R2 confirmations are **request/response with an id** through the same
  blocking-handler mechanism — never a fire-and-forget notification — so they cannot be silently
  dropped. The kill-switch notification (`cowork.killSwitch`) is best-effort but the *authority* is the
  Go-side teardown, not the UI seeing the event.
- **Every goroutine uses `safe.Go("pkg.site",fn)`** (`safe/safe.go:22`); a bare-`go` panic crashes the
  daemon. D-Bus injection calls, the EIS reader, and the session-watchdog timer all run under `safe.Go`.
- **No FD passing on the bus** (`server.go:75,133-141`) — confirmed. This is decisive for the portal
  path: `RemoteDesktop.Start` and `ConnectToEIS` yield a session handle that is **bound to the D-Bus
  connection that created it**, and EIS yields an FD. Therefore the portal `RemoteDesktop` session
  **must live entirely UI-side** (Qt6), and the `Notify*`/EIS calls are issued by the UI. Go never
  touches the portal session; it sends a *serializable injection plan* to the UI and gets back a result.
- **AT-SPI `org.a11y.atspi.Action`** (`REFERENCE-skill.md:153-155`): `GetNActions`, `GetName(i)`,
  `DoAction(i)→bool`, `GetDescription`, `GetKeyBinding`. This is a **same-bus D-Bus method call from Go**
  — no portal, no FD, no UI round-trip. That asymmetry is the whole reason AT-SPI is the safer default.

## Proposed design

### 1. AT-SPI Action path — PRIMARY, lower-risk, auditable (`desktop_click_element`)

A semantic action targets a **named accessibility node** and invokes one of *its own declared
actions* (`DoAction`), not a screen coordinate. Three properties make it the recommended default:

- **Bounded surface.** `DoAction(i)` can only invoke actions the widget already exposes
  (`click`/`toggle`/`expand`/`press`). It cannot type arbitrary text into a password field it didn't
  navigate to, cannot drag, cannot hit a coordinate that happens to overlap a different app. The action
  set is enumerable in advance (`GetNActions`/`GetName(i)`), so the confirmation can show *exactly* what
  will fire.
- **Self-describing & auditable.** We resolve the node to `Role`+`Name`+app, so the audit log and the
  confirmation read **"DoAction 'click' on BUTTON 'Save' in app 'LibreOffice Writer'"** — a human-legible
  record, not "pointer to (1024, 768)". This is the INV-4(a) anti-prompt-injection property: the user
  approves a *concrete, named* action.
- **No portal, no FD, no UI round-trip.** Go calls `org.a11y.atspi.Action.DoAction` directly on the
  a11y bus (Agent 3 owns the a11y connection + node addressing). The whole action is a single in-process
  Go D-Bus call after the gate resolves.

**Node identity (depend on Agent 3).** A `node` is the AT-SPI `(bus_name, object_path)` pair plus a
cached `{role, roleName, name, appName, pid, windowId}` resolved at *find* time. The agent obtains a
node from Agent 3's `desktop_find_element(role,name)` / `desktop_read_a11y_tree` (R1, read). **The R2
action tool re-resolves the node at action time** (re-reads `Name`/`Role`/`State`) and aborts if it no
longer matches the approved description (the widget moved/changed/disappeared) — TOCTOU defense. We
encode the node as an opaque base64 token `{bus_name, object_path, sig}` where `sig` is an HMAC over the
resolved descriptor using a per-process key, so the agent cannot fabricate or mutate a node handle.

**Tool**: `desktop_click_element(node)` (name fixed by INV-5). Despite the name it covers the general
"invoke a chosen action on this element"; `action` defaults to the element's primary/`"click"` action,
optionally `action: "<name>"` selected from the node's enumerated actions. **Recommend** keeping the
single `desktop_click_element` surface for v2 with an optional `action` arg rather than minting
`desktop_toggle`/`desktop_expand` — fewer tools, the action name is already in the gate display.

**RPC**: `cowork.doElementAction{ threadId, node, action? }`. Core flow (imperative gate, §3):

1. Decode + verify the node token; re-resolve via Agent 3's a11y client. Abort on mismatch.
2. **Anti-escalation check (§4)**: reject if the node's `windowId`/`pid`/`bus_name` is an AK window.
3. Build the **concrete action descriptor** (role, name, app, action name, window). 
4. Call Agent 1's `Authorize(ctx, AuthRequest{threadId, capability:"a11y_action", target:{windowId,
   appName}, action:<descriptor>, tier:R2})` — **this blocks** on the R2 confirmation (§3).
5. On allow: `atspi.DoAction(node, actionIndex)`; record audit (§5). On deny/timeout: return a tool
   error string; record the denial.

### 2. RemoteDesktop input injection — ESCAPE HATCH, R2 (`desktop_inject_input`)

When no a11y node exists (canvas apps, games, a webview with a broken a11y tree, drag gestures), the
agent falls back to raw input injection through the **XDG RemoteDesktop portal**, which **lives
UI-side** (INV-1; FD/session can't cross the bus). Sequence (skill `REFERENCE-skill.md:35-42`):
`CreateSession` → `SelectDevices(types: keyboard|pointer)` → `Start(parent_window,…)` →
`NotifyPointerMotionAbsolute(session,stream,x,y)` / `NotifyKeyboardKeysym(session,keysym,state)`.

**Notify\* vs EIS/libei (Plasma 6.3+).** **Recommend `Notify*` for v1 of this path (i.e. v3 overall —
see §8), EIS later.** Rationale:
- `Notify*` is **pure D-Bus** — issued from the UI's existing `QDBusConnection`, no extra native
  plumbing, no FD lifecycle, no `libei` link. It matches the round-trip we already use for capture.
- EIS (`ConnectToEIS`→libei FD) is **lower-latency and the modern path**, but adds: a `libei` dependency
  + cgo/native binding in the UI, an FD-backed event loop, and a second teardown path. Its win
  (sub-frame latency, smooth motion) **only matters for high-frequency streamed input** (drag, scrub,
  games). For v3's agent-driven discrete clicks/keystrokes, one `Notify*` D-Bus call per event is fine.
- **Decision:** ship `Notify*`. Add EIS in a later phase *iff* a use-case needs streamed/smooth input;
  it's an internal swap behind the same `cowork.injectInput` RPC and `desktop_inject_input` tool — the
  event-list contract (§ below) is path-agnostic, so EIS is a non-breaking UI-internal change.

**Event contract.** `desktop_inject_input(events)` / `cowork.injectInput{threadId, targetWindowId,
events}` where `events: [ {type:"keysym", keysym:<u32>, state:"press"|"release"|"tap", char?:"A"} |
{type:"text", text:"..."} | {type:"pointerMove", windowLocalX, windowLocalY} | {type:"pointerButton",
button:"left|right|middle", state:"press|release|click"} ]`. Text is expanded to a keysym sequence
**core-side** (so the audit log holds the literal text *and* the resulting keysyms). Coordinates are
**window-local** in the contract — never raw absolute — which forces every injection to name a target
window and lets coord reconciliation (below) and the AK-window exclusion (§4) operate on a window id.

**Coordinate mapping (depend on Agent 3).** AT-SPI window-local coords (`coord_type=1`) and our
contract's window-local coords must become absolute pointer coords for `NotifyPointerMotionAbsolute`.
Reconciliation = window-local `(x,y)` + KWin window geometry `(winX,winY)` for `targetWindowId`
→ absolute `(winX+x, winY+y)`, then mapped to the portal stream's coordinate space. **Agent 3 owns the
KWin geometry lookup and the window-local→screen reconciliation helper** (`portalcoord.go` per the file
map). The UI receives the *target window id + window-local point*, asks core (or its own cached
geometry mirror) for the absolute mapping, and clamps the result to the target window's rect — an
injected motion can **never** be placed outside the approved window (defence against "move to AK's
consent dialog and click Allow", §4).

**Core↔UI round-trip.** Reuse Agent 2's portal choreography (INV-5): core emits a
**`cowork.portalRequest`** notification carrying the injection plan; the UI runs the RemoteDesktop
session and replies with a normal **`cowork.portalResult`** call. But injection is *not* a one-shot
token fetch like capture — it's an *imperative command that must complete or fail definitively and be
auditable*. So we add a **dedicated `cowork.injectInput` that the UI services**, layered on the same
transport:

- Because the UI is the IPC *client* (it can't be called), the core-asks-UI direction is a
  **notification**: core sends `cowork.portalRequest{kind:"inject", reqId, targetWindowId, absEvents}`
  *after* the R2 gate has resolved allow. The UI's portal session executes the events and replies with
  `cowork.portalResult{reqId, ok, error?}` (a normal call into core). Core's injection handler **blocks
  on a broker-style rendezvous** keyed by `reqId` (same buffered(1) chan + timeout pattern as
  `permission.request`, but a separate map in `core/internal/cowork`) until `portalResult` arrives or a
  short (e.g. 30 s) timeout fires → fail-closed.
- **Session reuse:** the first injection for a thread triggers the portal consent dialog (the OS's own
  RemoteDesktop prompt) UI-side and obtains a `restore_token`; subsequent injections within the grant
  reuse the live session. The portal consent is *in addition to* AK's R2 gate, not a replacement — both
  must pass. Restore tokens are single-use → the UI rotates on each `Start` (carried from open-seam #3).

### 3. R2 gating UI + flow (imperative Go gate)

**The gate is an unconditional statement at the top of each R2 tool's RPC handler, before any side
effect.** It does not depend on Claude having escalated, on `--permission-mode`, or on any
`--allowedTools` token. Pseudocode (core, in `cowork.doElementAction` / `cowork.injectInput`):

```go
// R2 IMPERATIVE GATE — runs regardless of how the tool was invoked.
if d.cowork.KillSwitchEngaged() { return errKilled }                 // §6
desc := buildConcreteAction(req)                                     // exact, human-readable
if d.cowork.IsOwnWindow(req.targetWindowId) { return errSelfTarget } // §4 anti-escalation
grant, err := d.cowork.Authorize(ctx, AuthRequest{                   // Agent 1
    ThreadID: req.threadId, Capability: capCtl, Target: desc.target,
    Tier: R2, Action: desc,                                          // the CONCRETE action
})                                                                    // BLOCKS on R2 confirm UI
if err != nil || !grant.Allow { audit(deny); return errDenied }
audit(allow, desc)                                                   // §5, before executing
execute(req)                                                         // DoAction / inject
audit(executed, desc, result)                                        // §5
```

**Concrete-action display contract (hand to Agent 6).** `Authorize` for tier R2 carries a structured
`ActionDescriptor` that the UI **must render literally** — never a tool name, never truncated:

```
ActionDescriptor {
  mechanism:  "a11y_action" | "input_inject"
  appName:    "LibreOffice Writer"          // resolved, not pid
  windowTitle:"untitled1.odt — LibreOffice"
  // a11y_action:
  elementRole:"BUTTON"; elementName:"Save"; actionName:"click"
  // input_inject (one row per event, fully expanded):
  events: [ "move pointer to (412,318) in window",
            "type text: \"rm -rf ~/project\"  → keysyms: r,m,space,…",
            "press Return" ]
  threadId, requestedAt
}
```

The agent MUST present the concrete action; a vague tool name is a spec violation. For injection the
display shows **every keysym/char/coord**; for typed text it shows the literal string *and* expanded
keysyms (so "type rm -rf" can't hide as an opaque blob).

**Distinct high-risk confirmation widget (Agent 6 requirement).** R2 must **not** reuse the
rubber-stampable Approve/Deny banner (`m_permBar`, `AgentPanel.cpp:2390-2484`). It needs a separate
widget class — `CoworkControlConfirmDialog` (modal `QDialog`, `KMessageBox::Dangerous` styling,
`KColorScheme::NegativeText`, a warning icon, the literal `ActionDescriptor` rows, target window
title shown, and the calling thread id/title). Confirmation friction by mechanism:
- **`input_inject`** and **destructive-looking a11y actions** (text entry, Return into a terminal/url
  bar): **typed-phrase** confirmation — the user types e.g. the app name or a short fixed phrase to
  enable the Allow button (defeats reflexive Enter-mashing). 
- **plain a11y `click`/`toggle`** on a benign-looking widget: a distinct dialog with an explicit
  "Allow this one action" button (no typed phrase) — still per-action, still default-off, but not as
  heavy. The dialog never carries a "remember"/"don't ask again" checkbox for R2.

**Per-action by default; optional timed "drive this app" grant — sandbox only.** Baseline scope for
R2 is `once` (one action). We allow **one** widened scope: a `timed` grant `"let the agent drive
<app> for N minutes"` — **only when `target` is inside the VD sandbox** (Agent 5's
`vd_sandbox`-confined window). Outside the sandbox, R2 is always per-action. The timed grant is itself
created through the distinct R2 dialog with typed-phrase confirmation, is per-thread, auto-expires,
surfaces in the active-grants view with one-click revoke (Agent 1), and **never persists across a core
restart** (live R2 grants auto-revoke on restart — open-seam #3). Rationale: a sandbox window is on an
isolated virtual desktop with no access to AK's own windows or other apps, so the blast radius of a
timed grant is bounded; outside it, the cost of per-action prompting is the price of safety.

### 4. Anti-escalation enforcement points (INV-4 f)

Concrete, enumerated checks — each is a *hard refusal in Go before any side effect*:

1. **AK's own windows are never valid targets.** `cowork.IsOwnWindow(windowId)` is consulted for both
   tools. The AK window id set comes from Agent 3's KWin window list filtered by our own `pid`
   (and child pids) + `resourceClass` (`org.kde.agentkate`) + the a11y application whose `bus_name`
   matches our process. **Depend on Agent 3** to tag windows with `isOwnApp`/expose our pid; we also
   self-compute from `os.Getpid()` and the UI pid (the UI is our `QProcess` parent — core knows the UI
   pid from the socket peer / a handshake) as defence-in-depth so we don't *solely* trust Agent 3's tag.
2. **Injection coords are clamped to the approved target window rect** (§2). Even if a stale geometry
   slips through, an absolute point outside the window is clamped in, so injection cannot "walk" the
   pointer onto an AK dialog or another app.
3. **Injected input can never answer AK's own consent prompts.** Two layers: (a) AK windows are
   excluded as targets (#1); (b) the R2 confirmation dialogs are **separate top-level modal `QDialog`s
   owned by the UI** — the consent decision is read from a Qt widget, not from any text/keysym channel
   the injection path can reach. The injection portal session targets *other* apps' surfaces; it has no
   handle to AK's surface.
4. **The consent/grant store is unreachable by injection.** Grants live in
   `$XDG_DATA_HOME/agentkate/cowork-consents.json`, owned and written by **core** (Agent 1), atomic
   temp+rename, never agent-writable and never a control target (it's a file, not a window). Injection
   can only produce input events to non-AK windows; it has no filesystem path.
5. **Self-approval-loop defense.** The R2 gate (§3) blocks the *calling* tool in the bridge subprocess;
   the human's decision arrives via `cowork.respondGrant` from a human-driven Qt dialog. There is no
   code path by which an agent action resolves its own pending grant: `Authorize` only returns when a
   `respondGrant` matching the `reqId` arrives, and `respondGrant` is served only to the UI's
   human-initiated click, not exposed as an MCP tool.
6. **Kill-switch short-circuits before the gate** (§6) so a global stop wins even mid-prompt.

### 5. Audit (Agent 1's append-only log)

Every R2 attempt writes an audit record via Agent 1's audit log (`core/internal/cowork/audit.go`),
**before execution** (intent) and **after** (outcome), append-only, atomic:

```
AuditEntry {
  ts, threadId, capability ("a11y_action"|"input_inject"),
  decision ("requested"|"allowed"|"denied"|"timed_out"|"executed"|"failed"|"killed"),
  target { windowId, appName, windowTitle },
  literal {                       // the EXACT thing that ran
     a11y:   { role, name, actionName }
     inject: { events:[ "move (412,318)", "text \"rm -rf\"", "Return" ], keysyms:[...] }
  },
  grantId?, error?
}
```

The literal input is recorded verbatim (text *and* keysyms; coords resolved to both window-local and
absolute) so a post-hoc review can reconstruct precisely what the agent did. Read content captured for
context is *not* logged here (R1's concern) — only control actions. The active-grants/audit surface
(Agent 6) renders these as "Agent <thread> clicked 'Save' in LibreOffice at 14:03".

### 6. Kill-switch — immediate teardown

The global kill-switch (Agent 1's `cowork.killSwitch`) must, for the control layer:
1. **Set `KillSwitchEngaged()=true` synchronously** so every in-flight and future R2 gate short-circuits
   (`return errKilled`) before any side effect (§3 first line).
2. **Tear down the RemoteDesktop portal session immediately.** Core emits `cowork.portalRequest{kind:
   "killInject"}`; the UI calls `Session.Close()` on the portal session and drops the `restore_token`,
   stopping any further `Notify*`/EIS events at the source. Because the portal session is the *only* way
   to inject, closing it is a hard stop even if a notification is lost — but we also unblock all pending
   `injectInput` rendezvous with a denied result so nothing hangs.
3. **Revoke all live R2 grants** (timed "drive this app" grants included) via Agent 1's grant store and
   resolve all pending `Authorize` calls as denied. Audit a `"killed"` entry per affected thread.

Kill-switch is global (all threads) per INV-4(g); R2 grants are per-thread, but the switch cuts them
all. Teardown runs under `safe.Go("cowork.killswitch",…)` and holds no mutex across the D-Bus close.

### 7. MCP tools + RPCs (exact names)

| MCP tool (cowork server) | Capability/tier | Core RPC | Mechanism |
|---|---|---|---|
| `desktop_click_element(node, action?)` | a11y_action / R2 | `cowork.doElementAction` | AT-SPI `DoAction` (Go) |
| `desktop_inject_input(events, targetWindowId)` | input_inject / R2 | `cowork.injectInput` | portal RemoteDesktop `Notify*` (UI) |

Both tools are on the **separate, opt-in `cowork` MCP server** (INV-5), off by default, enabled
per-workspace/thread; `writeMCPConfig` (`main.go:2778`) gains a `cowork` entry only when the workspace
opts in, with `--allowedTools mcp__cowork`. **Even with that token present, the R2 imperative gate still
fires** — the token only lets the tool be *called*, not *auto-approved*. Tool `description` strings
state plainly: "Requires explicit per-action human approval every time; cannot be pre-authorized."

Round-trip RPCs/notifications this slice depends on or adds:
- adds: `cowork.doElementAction`, `cowork.injectInput` (core handlers).
- reuses (Agent 2): `cowork.portalRequest` (notify, core→UI) / `cowork.portalResult` (call, UI→core) —
  extended with `kind ∈ {"inject","killInject"}`.
- reuses (Agent 1): `Authorize()`, audit log, grant store, `cowork.grantRequested`/`respondGrant`,
  `cowork.killSwitch`, active-grants surface.
- reuses (Agent 3): a11y node addressing/`DoAction` client, KWin geometry, coord reconciliation,
  `isOwnApp` window tagging.

### 8. Phasing & risk-ordering

- **v1 (this directory's walking skeleton, INV-7):** *no control at all.* Window list + screenshot
  only. Control is explicitly out of v1.
- **v2 — AT-SPI semantic actions, sandbox-confined.** Ship `desktop_click_element` /
  `cowork.doElementAction` with the full R2 gate, but **restrict targets to windows inside Agent 5's VD
  sandbox** at first. Justification for ordering AT-SPI before raw injection: it is lower-risk (bounded
  action set, no portal/FD, no coordinate aiming), more auditable (named element), and exercises the R2
  gate + anti-escalation + audit + kill-switch end-to-end **without** the portal-session lifecycle. Once
  proven in the sandbox, lift to non-sandbox windows (still per-action). **[M]** (gate+audit+anti-esc
  reuse Agent 1; the new code is the action descriptor, node re-resolution/TOCTOU, and the `DoAction`
  call).
- **v3 — raw portal input injection (`Notify*`).** Ship `desktop_inject_input` / `cowork.injectInput`
  with the UI-side RemoteDesktop session, coord reconciliation, window-rect clamping, and the dedicated
  inject round-trip. This is last because it carries the highest blast radius (arbitrary input anywhere
  in the target window, portal session lifecycle, coordinate-aiming bugs) and the most plumbing (UI
  portal session + rendezvous + teardown). **[L]**. EIS swap is a **later, optional [M]** behind the same
  tool/RPC, only if streamed input latency proves necessary.

**Plasma version gotcha (carried, open-seam #7):** on Plasma 6.5.x the portal consent dialogs *ignore
virtual input*; an agent cannot click its own RemoteDesktop consent (good — reinforces #4), but it also
means the *human* must physically click the first portal consent, then the persistent token is reused.
v3 must surface this in the UI ("approve the system dialog with a real click") and fall back to the
pre-authorization path (`flatpak permission-set kde-authorized remote-desktop …`) for power users.

## Implementation steps

1. **(v2) Go:** add `cowork.doElementAction` handler in `registerHandlers` (`main.go:437+`); it builds
   the `ActionDescriptor`, runs the imperative gate (kill-switch → `IsOwnWindow` → `Authorize`), calls
   Agent 3's a11y `DoAction`, audits. Node token encode/verify + re-resolve helper in
   `core/internal/cowork`. `handlerDeps` gains `cowork *cowork.Service`.
2. **(v2) MCP:** add `desktop_click_element` to `toolDefs()` + a `runTool` case that calls
   `cowork.doElementAction` (`mcp.go`). Add the `cowork` MCP server to `writeMCPConfig` behind the
   per-workspace opt-in + `--allowedTools mcp__cowork` in `agent.go`.
3. **(v2) UI (Agent 6):** `CoworkControlConfirmDialog` distinct R2 widget; wire to
   `cowork.grantRequested` for tier R2; typed-phrase variant for inject/destructive a11y.
4. **(v3) Go:** add `cowork.injectInput` handler — gate, coord reconcile (Agent 3) to absolute + clamp,
   text→keysym expansion, then the inject rendezvous (buffered(1) chan keyed by reqId, 30 s timeout)
   driving `cowork.portalRequest{kind:"inject"}`. Add `desktop_inject_input` tool + case.
5. **(v3) UI:** RemoteDesktop portal session manager (`PortalSession` extended): `CreateSession`/
   `SelectDevices`/`Start`, `restore_token` rotation, `Notify*` issuance on `portalRequest{inject}`,
   `Session.Close()` on `portalRequest{killInject}` and on revoke.
6. **Both:** kill-switch integration (`KillSwitchEngaged`, teardown emit, pending-rendezvous drain);
   audit wiring (Agent 1). Tests: gate allow/deny/timeout/concurrent, `IsOwnWindow` refusal, coord
   clamp, node TOCTOU mismatch abort, kill-switch mid-action, restore-token rotation.

## Risks / considerations

- **TOCTOU on the a11y node** — the widget can move/change between find and action. *Mitigation:*
  re-resolve and compare role+name (+rough extents) at action time; abort + audit `"failed"` on
  mismatch. The confirmation shows the descriptor captured at gate time; if it changed, deny.
- **Coordinate-aiming bug injects into the wrong window** — geometry can be stale (window moved during
  the gate). *Mitigation:* clamp to the *current* target-window rect re-fetched at execute time, not the
  gate-time rect; if the target window is gone, abort. Reconciliation correctness is Agent 3's contract
  — cross-ref `03-introspection.md` coord section.
- **Self-approval loop** — the single biggest threat. *Mitigation:* enumerated in §4 (AK windows
  excluded, coords clamped, consent read from Qt widgets not injectable channels, grant store core-owned,
  `respondGrant` not an MCP tool). This must be re-audited in the Optimize/Review phase.
- **Consent fatigue pushes users to the timed grant** — *Mitigation:* timed grant is sandbox-only and
  typed-phrase-gated; outside the sandbox there is no escape from per-action prompting by design.
- **Portal session leak** — a session left open after a thread dies. *Mitigation:* tie session lifetime
  to the grant; revoke-on-thread-exit; kill-switch closes all; live R2 grants auto-revoke on restart.
- **Lossy `cowork.killSwitch` notification** — the UI might miss the event. *Mitigation:* the authority
  is Go-side `KillSwitchEngaged` + the portal close *command*; injection is impossible without the UI's
  portal session, and the gate refuses regardless. Cross-ref `01-consent-spine.md` kill-switch.
- **Plasma 6.5.x virtual-input gotcha** — see §8; v3 must detect Plasma version and message the user.

## Acceptance

- `desktop_inject_input`/`desktop_click_element` are **not callable without the `cowork` MCP server
  enabled**, and even then **every** call surfaces the distinct R2 dialog — proven by a test that sets
  `--permission-mode bypassPermissions` and confirms the gate still fires.
- The R2 dialog shows the **literal** action (element role+name+action, or every keysym/coord) and the
  target window title — never a bare tool name.
- Injection and a11y actions targeting an **AK window are refused** with an audit `"denied"` entry, in a
  test that passes AK's own window id.
- The **kill-switch** closes the RemoteDesktop session and denies all pending/future R2 calls within one
  event loop tick; a test injects mid-action and observes the abort.
- Every executed action produces a verbatim **audit entry** (capability, target window+element, literal
  input, threadId, timestamp).
- Outside the sandbox, R2 scope is **always per-action** (no timed grant offered); inside the sandbox a
  timed "drive this app" grant is available, typed-phrase-gated, auto-expiring, revocable, and
  non-persistent across restart.
