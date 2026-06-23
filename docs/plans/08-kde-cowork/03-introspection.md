# 03 — Introspection: KWin window list + workspace events + AT-SPI2 tree

The **read-only sensing** layer of Cowork. Two Go-side D-Bus clients give Agent Kate a structured
picture of the live desktop: *which windows exist and where* (KWin scripting, Tier R0) and *what UI
elements are inside them* (AT-SPI2 accessibility tree, Tier R1). Both run entirely in `akcore` — no
Wayland surface, no portal, no FD passing — per **INV-1** ("KWin scripting and AT-SPI2 can be called
directly from `akcore` via D-Bus"). Capture (pixels) and control (input/actions) are siblings
`02-capture.md` / `04-control.md`; this slice deliberately stops at *reading metadata + text*.

This file owns `core/internal/kde/{kwin.go,atspi.go}` and the introspection state in
`core/internal/cowork`. It depends on `01-consent-spine.md` for `Authorize()` and the Grant store,
and feeds the window model + coordinate mapping that `04-control.md` and `05-sandbox.md` consume.

---

## Current state

- **No D-Bus anywhere.** `core/go.mod` (module `agentkate`, go 1.26) has **no D-Bus dependency**;
  we add `github.com/godbus/dbus/v5 v5.1.0` (context-pack §1 build). All introspection is greenfield.
- **Handler registration is static.** `registerHandlers(d handlerDeps)` (`main.go:437`) wires every
  `srv.Handle("ns.method", fn)` up-front; **the handler map is not goroutine-safe after `Serve`**
  (context-pack §1) — so `cowork.*` RPCs are registered at boot like `coop.*`
  (`main.go:1114-1206`), never dynamically.
- **`handlerDeps` is the injection seam** (`main.go:420-434`): `srv, sup, coop, threads, broker,
  sessions, …`. We add one field `cowork *cowork.State` (the introspection + grant facade), built in
  `main()` next to `coopState := coop.NewState()` (`main.go:183`), and torn down in
  `gracefulShutdown` (`main.go:302-323`) alongside `gitCache.Close()`.
- **Notify is broadcast + lossy.** `srv.Notify(method, params)` fans a notification to every client
  (`server.go:197-216`); under backpressure **notifications are shed oldest-first, responses never**
  (`enqueue`, `server.go:252-290`). ⇒ **window/a11y event streams MUST be coalesced and the UI MUST
  re-derive from a snapshot RPC** (INV-6). The canonical pattern is the agent **coalescer**
  (`agent.go:158-236`): a `sync.Mutex` + `pending []T` + a single `time.AfterFunc(window, …)` timer,
  flush on boundary-or-timer, and **`safe.Go` the timer body** (`agent.go:197-199`) so a panic in the
  flush is recovered. We mirror it verbatim.
- **State model to mirror:** `coop.State` (`coop.go:47-66`) — a `sync.Mutex` guarding internal maps,
  every reader **copies out** (`ListOpenFiles` clones into a fresh slice, `coop.go:85-94`), never
  returns internal pointers. Our `cowork.State` follows this exactly.
- **Every goroutine is `safe.Go("pkg.site", fn)`** (`safe.go:22`) — recover+log; a bare `go` panic
  crashes the daemon. D-Bus signal-listener loops and coalescer timers all route through it.
- **MCP tool = two edits** (context-pack §1, confirmed `mcp.go:132-442` switch + `446-571` defs): a
  `case` in `runTool` that does one `b.client.Call("cowork.method", args, &res)` and returns a
  `string`; a `{name,description,inputSchema}` in the tool catalogue. The bridge has `b.client`
  (CoreClient), `b.thread`, `b.workspace` (`mcp.go:135`). New `cowork` MCP server is **separate +
  off-by-default** (INV-5) — its own entry in `writeMCPConfig` + `--allowedTools mcp__cowork`.
- **Consent spine is a sibling dependency.** `01-consent-spine.md` owns `cowork.State.Authorize(ctx,
  threadID, capability, target, tier) (Grant, error)` and the persisted Grant store
  (`cowork-consents.json`, INV-2). This slice **calls** it; it does not define it. R0/R1 here →
  `window_list` / `a11y_read` capabilities (INV-3).

---

## Proposed design

### 1. KWin window enumeration (`kde/kwin.go`)

KWin scripting has **no return channel**: `loadScript(path,pluginName)→scriptId` then
`Script.run()` executes the JS, but `run()` returns nothing useful — the script must *push* its
result out (REFERENCE-skill §Layer 2). Two ways to get data back:

- **(A) D-Bus service callback.** akcore registers a bus name `io.agentkate.Cowork` with an exported
  object `/WindowList` / interface `io.agentkate.Cowork.WindowList` and method `Report(payload: s)`.
  The script `callDBus("io.agentkate.Cowork","/WindowList","io.agentkate.Cowork.WindowList","Report",
  JSON.stringify(result))` (skill JS, lines 98-99). akcore's `Report` handler delivers the JSON to a
  waiting Go channel keyed by a nonce embedded in the script.
- **(B) Temp-file round-trip.** Script writes JSON to a well-known path; Go polls/reads it.

**Recommend (A) for v1.** It is the cleaner, race-free, latency-bounded path *and* it is **mandatory
for the event stream** (§2) — a persistent event script has no file to "finish writing", it must call
back live. Owning the bus name once serves both the one-shot list and the firehose, so there is no
reason to build (B) at all. Cost analysis of owning `io.agentkate.Cowork`:

- godbus: `conn.RequestName("io.agentkate.Cowork", dbus.NameFlagDoNotQueue)` once at `kde.New()`;
  export the object with `conn.Export(receiver, path, iface)` + `conn.ExportMethodTable`. One bus
  name on the **session** bus, akcore-owned, agent-unwritable (the agent has no D-Bus access at all —
  it only sees MCP tools), satisfying INV-4(f) "consent surfaces outside the agent's injectable
  scope." Risk: name collision if two akcore instances run — append the pid:
  `io.agentkate.Cowork.akcore_<pid>` and bake that exact name into the injected JS string. (B) is the
  documented fallback if `RequestName` fails (rare; sandbox/headless), behind one `if`.

**Script lifecycle — one-shot vs persistent.** KWin scripts **persist until KWin restarts** unless
explicitly stopped (REFERENCE-skill Gotchas). So:

- **One-shot enumeration:** `loadScript(tmp, "ak_winlist_<nonce>")` → `Script.run()` → the JS reports
  via `Report` → Go reads the channel → **`Script.stop()`** on `/Scripting/Script<id>` →
  `os.Remove(tmp)`. The nonce in the `pluginName` and in the JS payload ties the report to *this*
  invocation (concurrent lists don't cross-talk). A 3 s timeout (D-Bus call + report) bounds it.
- **Persistent event script:** loaded **once** at `kde.New()`, never stopped until shutdown (§2).

We deliberately **do not use `kpackagetool6`** (installs a persistent package on disk, survives
restart, pollutes the user's KWin config — bad hygiene for an opt-in feature). Instead: a
**loaded-script with teardown-on-shutdown** — `loadScript` the event JS at startup, remember its
scriptId, and `Script.stop()` + bus-name release in `gracefulShutdown`. If akcore crashes the orphan
script's `callDBus` to a now-dead bus name simply no-ops (KWin logs, continues) — harmless, and the
next start reloads fresh. This is the right tradeoff: zero persistent footprint, self-healing.

**Window model** (from the KWin 6.x property table, REFERENCE-skill lines 103-106; selected for what
control/capture actually need). JSON tags are the wire contract:

```go
type Window struct {
    InternalID    string `json:"internalId"`    // QUuid string — STABLE handle across events
    Caption       string `json:"caption"`       // title bar text (may carry secrets — see Risks)
    ResourceClass string `json:"resourceClass"` // e.g. "firefox" — the app identity
    ResourceName  string `json:"resourceName"`
    PID           int    `json:"pid"`           // bridges to AT-SPI pid filter (§3) + portal restore
    Active        bool   `json:"active"`
    Minimized     bool   `json:"minimized"`
    FullScreen    bool   `json:"fullScreen"`
    OnAllDesktops bool   `json:"onAllDesktops"`
    SkipTaskbar   bool   `json:"skipTaskbar"`
    Desktops      []string `json:"desktops"`    // virtual-desktop ids the window is on
    Activities    []string `json:"activities"`
    X, Y, W, H    int    `json:"x","y","width","height"` // FRAME geom, screen coords (KWin global)
    StackingOrder int    `json:"stackingOrder"` // z-order index
    Managed       bool   `json:"managed"`       // false for OSDs/docks — filter noise
    SpecialWindow bool   `json:"specialWindow"`
}
```

`InternalID` (the QUuid) is the **stable identity** used everywhere downstream — events reference it,
`desktop_screenshot(target)` resolves it to a KWin window handle, control targets it. `PID` is the
**join key** to AT-SPI. `X/Y/W/H` is the **coordinate anchor** for a11y reconciliation (§3).

**Snapshot RPC `cowork.listWindows`.** Handler: `Authorize(thread, window_list, "", R0)` →
on grant, `kde.ListWindows(ctx)` (runs the one-shot script, 3 s timeout) → returns `{windows:
[]Window}`. Filter out `!Managed` / `SpecialWindow` / AK's own windows by default (INV-4(f)).
Results also seed `cowork.State`'s window cache so the event coalescer (§2) can diff against it.

### 2. Workspace event streaming (`kde/kwin.go` + `cowork/state.go`)

A **single persistent KWin script** loaded at `kde.New()` connects the workspace signals
(REFERENCE-skill lines 109-110) and `callDBus`-es a compact delta on each:

```javascript
function emit(kind, win) {
  callDBus("io.agentkate.Cowork.akcore_<PID>", "/Events",
    "io.agentkate.Cowork.Events", "WindowEvent",
    JSON.stringify({kind: kind, id: win ? win.internalId.toString() : "",
                    pid: win ? win.pid : 0}));
}
workspace.windowAdded.connect(w   => emit("added", w));
workspace.windowRemoved.connect(w => emit("removed", w));
workspace.windowActivated.connect(w => emit("activated", w));
workspace.currentDesktopChanged.connect((o,w) => emit("desktopChanged", null));
workspace.currentActivityChanged.connect(id    => emit("activityChanged", null));
workspace.screensChanged.connect(()           => emit("screensChanged", null));
```

The `WindowEvent` D-Bus method on akcore's `/Events` object hands each delta to **`cowork.State`'s
coalescer**, a verbatim port of `agent.go:158-236`:

- `mu sync.Mutex` + `pending []WindowEvent` + a single `timer *time.Timer`.
- `add(ev)`: dedup the *immediately-preceding* event if byte-equal (collapses repeated
  `activated`/`screensChanged` storms — exactly the `agent.go:179-186` rule); else append and, if no
  timer, `timer = time.AfterFunc(coalesceWindow, func(){ safe.Go("cowork.coalesceFlush", flush) })`.
- `coalesceWindow = 25 * time.Millisecond` (INV-6 ≥25 ms, matches `agent.go`).
- `flush()`: under lock, take the batch, then `srv.Notify("cowork.windowEvent", {events:[…]})`.
- **No boundary-flush** for events (unlike agent results) — pure time-coalesced, since every window
  event is equivalent in urgency; this maximises batching under a desktop-drag firehose.

The notification payload carries **only deltas** (`{kind,id,pid}`), never full window structs — the
UI/agent **re-derives full state by calling `cowork.listWindows`** when it receives a batch (INV-6
"paired with a snapshot RPC"). This keeps `cowork.windowEvent` tiny and shed-safe: if the UI drops a
coalesced batch under backpressure, the next snapshot is authoritative. The agent does *not* receive
a push stream (MCP is request/response) — it polls `desktop_list_windows`; the notification exists for
the **UI's** live window picker only.

**Install/cleanup.** Loaded-script (not `kpackagetool6`) as argued in §1. At `kde.New()`:
`RequestName` → export `/WindowList`,`/Events` → `loadScript(eventTmp,"ak_events")` → `run()` → store
`eventScriptID`. In `gracefulShutdown`: `Script.stop(eventScriptID)`, `os.Remove(eventTmp)`,
`conn.ReleaseName(...)`, `conn.Close()`. Re-load is idempotent across restarts.

### 3. AT-SPI2 accessibility tree (`kde/atspi.go`)

**Bootstrap (REFERENCE-skill Layer 3):** (1) on the *session* bus,
`org.a11y.Bus@/org/a11y/bus.GetAddress()→s`; (2) check `org.a11y.Status.IsEnabled` (property on the
same object) — if false, the tree is empty, return a clear "accessibility not enabled" error rather
than a confusing empty result; (3) open a **second godbus connection** with `dbus.Dial(addr)`
(+`conn.Auth(nil)` + `conn.Hello()`) to the a11y bus — kept open and reused, **not** reconnected per
call; (4) registry root `org.a11y.atspi.Accessible@/org/a11y/atspi/accessible/root.GetChildren()→
a(so)` = one `(bus_name, object_path)` per running app.

**`QT_ACCESSIBILITY=1`.** Qt apps only populate AT-SPI when this is set in *their* environment, so it
governs whether the desktop's apps are introspectable at all — it is **not** something akcore can set
for other processes. For Agent Kate's *own* window to be introspectable (and for dogfooding), set it
in the **UI process env** at launch — `ui/src/main.cpp` `qputenv("QT_ACCESSIBILITY","1")` before
`QApplication` construction. For third-party apps we **rely on the user's session** already exporting
a11y (KDE sets `QT_ACCESSIBILITY=1`/`GTK` ATK when "Accessibility" is on, or the app opts in). Document
this as a known limitation: apps launched without it are invisible to `desktop_read_a11y_tree`.

**Bounded walk.** Accessibility trees are huge (a browser DOM-mapped tree can be 10k+ nodes) and each
`GetChildren`/property read is a **blocking D-Bus round-trip** — an unbounded recursive walk is a
latency and memory bomb. Bounds:

- **`pid` filter (primary scope-narrowing).** The single most important bound: resolve the target
  app from `pid` via `GetApplication`/app attributes and walk **only that app's subtree**. The agent
  almost always wants one app (the browser, the editor), joined from the `Window.PID` (§1). A whole-
  desktop walk is opt-in and harder-bounded.
- **`maxDepth` (default 12, hard cap 25).** REFERENCE-skill's snippet uses 3; that's too shallow for
  real UIs (a button can be 8 levels deep). 12 covers most app trees; 25 is the absolute ceiling.
- **`maxNodes` (default 1500, hard cap 5000).** Stop the walk when reached; mark the result
  `truncated:true` so the agent knows it's partial and can `desktop_find_element` to drill instead.
- **breadth cap per node (default 200 children).** Containers (giant lists/tables) blow up otherwise;
  emit a `childrenTruncated` marker.
- **Lazy expansion.** `desktop_read_a11y_tree` returns a **shallow** tree (depth-bounded) for
  orientation; `desktop_find_element(role,name)` does a *targeted* bounded BFS (stops at first match
  or maxNodes); `desktop_read_element_text(node)` fetches one node's text on demand. The agent
  composes these — it never needs the whole tree at once.
- **Per-call timeout (default 5 s, ctx-bounded).** Each D-Bus call gets a context deadline; a hung
  app aborts the walk with a partial result, never wedging the handler goroutine.
- **Caching.** Cache the `(busName→pid)` map and the a11y-bus connection for the session; cache a
  *recent tree snapshot* per pid for a short TTL (~2 s) so `find_element` after `read_a11y_tree`
  reuses it. Invalidate on any `cowork.windowEvent` for that pid. Mirror the gitstatus cache's
  copy-out discipline.

**Node model:**

```go
type A11yNode struct {
    Ref       string   `json:"ref"`        // "<busName>\x1f<objectPath>" — opaque handle for follow-up tools
    Name      string   `json:"name"`       // Accessible.Name (REDACTED if password — see Risks)
    Role      uint32   `json:"role"`       // AtspiRole enum (skill table lines 158-162)
    RoleName  string   `json:"roleName"`   // human label e.g. "push button"
    States    []string `json:"states"`     // decoded GetState() bitmask: FOCUSED, ENABLED, PASSWORD, …
    Extents   Rect     `json:"extents"`    // SCREEN-ABSOLUTE after reconciliation (§coords)
    HasText   bool     `json:"hasText"`    // implements Text iface — read via desktop_read_element_text
    Actions   []string `json:"actions"`    // GetName(i) per action ("click","toggle") — feeds 04-control
    ChildCount int     `json:"childCount"`
    Children  []A11yNode `json:"children,omitempty"` // populated only within depth/node bounds
    Truncated bool     `json:"truncated,omitempty"`  // bound hit below this node
}
type Rect struct{ X, Y, W, H int }
```

`Ref` (busName + objectPath, `\x1f`-joined) is the **stable cross-tool handle**:
`read_element_text(ref)` and `04-control`'s `desktop_click_element(ref)` both resolve it back to an
`(bus_name, object_path)` D-Bus object. We **do not** trust an agent-supplied ref blindly — we
re-validate it resolves and re-check the pid/grant before acting (defense for control).

**Coordinate reconciliation (the Wayland gotcha, REFERENCE-skill line 194 / context-pack §4.5).**
On Wayland `GetExtents(coord_type=0 /*screen*/)` returns **window-local** coords (the compositor hides
global positions). So:

1. Call `Component.GetExtents(coord_type=1 /*window-local*/)` → `(lx, ly, w, h)` — reliable.
2. Find the owning KWin window for this app's `pid` from the §1 window cache → its frame `(X, Y)`.
3. **Screen-absolute = `(X + lx, Y + ly, w, h)`.** Account for the window-frame inset if the a11y
   origin is the client area vs the frame — measured empirically in the coord-reconciliation spike.
4. If no KWin window matches the pid (e.g. headless app), return window-local extents with a
   `coordSpace:"window-local"` flag rather than wrong absolutes.

This mapping is the **contract `04-control.md` depends on** to translate "click this element" into a
screen-absolute point for `RemoteDesktop` input injection. We expose it as a pure function
`kde.ToScreenCoords(localRect, pid) (Rect, space string)` so control reuses it.

### 4. Consent integration

Every introspection RPC gates through the consent spine (`01-consent-spine.md`) **before** any D-Bus:

- `cowork.listWindows` → `Authorize(thread, capability=window_list, target="", tier=R0)`. R0 =
  session-scoped, standard prompt (INV-3). Note the KDE layer itself needs **no** prompt (KWin
  scripting is consent-free) — the **AK consent gate is still mandatory** (INV-3 emphasis): the user,
  not KWin, decides whether *the agent* sees the window list.
- `cowork.snapshotA11y` / `cowork.findElement` / `cowork.readElementText` →
  `Authorize(thread, capability=a11y_read, target=<pid or "desktop">, tier=R1)`. R1 = per-target,
  timed/session scope, **audited each read** (INV-3, INV-4(d)). The `target` narrows the grant to one
  app's pid so a grant to read the browser doesn't leak the password manager.
- On deny/timeout the handler returns a definitive `ipc` error (fail-closed); the MCP tool surfaces a
  plain "the user declined" string. The grant is checked **inside the Go handler**, not just in
  Claude's prompt funnel (INV-3 — imperative gate).

**Secret leakage — the R1 redaction posture (INV-4(e)).** A11y trees leak secrets structurally:
password fields expose their content as `Text`, and even `Name`/`Caption` can carry tokens. Defenses,
applied in `atspi.go` *before* data leaves Go:

- **STATE-based exclusion.** Decode `GetState()`; if the node has `ATSPI_STATE_PASSWORD` (or role
  `PASSWORD_TEXT`), **never** read its `Text` — emit `name:"[redacted password field]"`, `hasText:
  false`. `desktop_read_element_text` hard-refuses a password node regardless of agent request.
- **Sensitive-window exclusion.** A configurable denylist of `resourceClass` (password managers,
  banking apps, the user's own keyring prompt) excluded from a11y reads entirely; AK's own windows
  always excluded (INV-4(f)).
- **Treat read text as untrusted/injectable** (INV-4(e)): the a11y text the agent reads can itself
  carry prompt-injection; that's a downstream concern but we tag a11y results so the agent system
  prompt can frame them as untrusted desktop content.
- **Audit every read** with `{thread, capability, target pid, nodeCount, ts}` to the cowork audit log
  (INV-4(d)).

### 5. Go wiring (`kde` package + `cowork.State` + `main.go`)

- **`kde.Client`** owns two godbus connections: `session *dbus.Conn` (KWin + bus-name ownership) and
  `a11y *dbus.Conn` (lazily dialed on first a11y use, then cached). Constructed once in `main()`;
  `kde.New(log)` does `dbus.ConnectSessionBus()`, `RequestName`, exports, loads the event script.
- **Signal listeners via `safe.Go`.** The persistent event script pushes to akcore's exported
  `WindowEvent`/`Report` methods — those run on godbus's internal handler goroutines, which we keep
  trivial (just forward into a buffered channel). A single `safe.Go("kde.eventPump", …)` drains the
  channel into the coalescer. **Never hold `cowork.State.mu` across a D-Bus call** (context-pack §1) —
  the listener only mutates in-memory state under the lock; the blocking `ListWindows` D-Bus work
  happens lock-free, copy-out on completion.
- **`cowork.State`** (mirrors `coop.State`): `mu sync.Mutex` guarding `windows map[string]Window`
  (keyed by InternalID), the event `coalescer`, the a11y tree cache, plus the grant store from
  `01-consent-spine.md`. Readers copy-out. `Snapshot()`/`ListWindows()` clone before returning.
- **Teardown in `gracefulShutdown`** (`main.go:302-323`, after `gitCache.Close()`):
  `cowork.Close()` → stop the event script, release the bus name, close both D-Bus conns, stop the
  coalescer timer. Idempotent (guarded), like the existing `sync.Once` shutdown.
- **Bounded everything:** the event channel is buffered (cap 256) and **drops oldest on full** (the
  coalescer re-syncs from snapshot anyway); the a11y tree cache is size-capped; walks are node-bounded.

### 6. MCP tools + RPCs + notifications

| MCP tool (`mcp__cowork`) | Core RPC | Capability/tier | Notes |
|---|---|---|---|
| `desktop_list_windows` | `cowork.listWindows` | window_list / R0 | session grant; returns `[]Window` |
| `desktop_read_a11y_tree(pid?)` | `cowork.snapshotA11y` | a11y_read / R1 | bounded shallow tree; `pid` narrows scope+grant |
| `desktop_find_element(role,name)` | `cowork.findElement` | a11y_read / R1 | targeted bounded BFS; returns matching `[]A11yNode` refs |
| `desktop_read_element_text(node)` | `cowork.readElementText` | a11y_read / R1 | one node's full text; refuses password nodes |

- **`cowork.listWindows`** `→ {windows:[]Window}` — runs the one-shot KWin script.
- **`cowork.snapshotA11y`** `{pid?, maxDepth?, maxNodes?} → {root:A11yNode, truncated:bool}`.
- **`cowork.findElement`** `{pid?, role?, name?} → {matches:[]A11yNode}` (ref + extents per match).
- **`cowork.readElementText`** `{ref} → {text:string, redacted:bool}`.
- **Notification `cowork.windowEvent`** `{events:[{kind,id,pid}]}` — coalesced ≥25 ms, UI-facing,
  re-derive via `cowork.listWindows`.

MCP wiring is the standard two edits in `mcp.go` (a `case` per tool calling `b.client.Call`, a
catalogue entry), under the new off-by-default `cowork` MCP server (INV-5). Each tool case formats the
struct result into a readable string for the agent (e.g. windows as `caption [class] pid=… (w×h @x,y)`
lines), like the existing `list_open_files` case (`mcp.go:137-151`).

---

## Implementation steps

1. **Dep + skeleton.** Add `github.com/godbus/dbus/v5 v5.1.0`; `go mod tidy` (verify go 1.26 build).
   Create `core/internal/kde/{kwin.go,atspi.go}` and `core/internal/cowork/state.go` (stub `State`,
   coalescer copied from `agent.go:158-236`).
2. **KWin one-shot list (v1 walking skeleton, INV-7).** `kde.New` → `ConnectSessionBus`,
   `RequestName io.agentkate.Cowork.akcore_<pid>`, export `/WindowList`. `ListWindows`: write the
   enumeration JS (nonce-stamped), `loadScript`/`run`, await `Report` on a channel (3 s timeout),
   `Script.stop`, parse `[]Window`. Unit-test the JSON parse against a captured KWin payload.
3. **Snapshot RPC + tool (no consent yet, to prove the seam).** `cowork.listWindows` handler →
   `kde.ListWindows`; `desktop_list_windows` MCP tool; new `cowork` MCP server in `writeMCPConfig`.
   This is INV-7's "single no-consent `desktop_list_windows`" milestone.
4. **Consent gate.** Once `01-consent-spine.md` lands `Authorize`, wrap step 3's handler in
   `Authorize(window_list, R0)` and add the audit entry.
5. **Event stream.** Export `/Events`; persistent event JS at `kde.New`; `eventPump` `safe.Go` →
   coalescer → `cowork.windowEvent`. Wire `cowork.Close()` into `gracefulShutdown`.
6. **AT-SPI bootstrap + bounded walk.** `getA11yBusAddress`, `IsEnabled` check, dial second conn,
   registry `GetChildren`, pid-filtered depth/breadth/node-bounded walk → `A11yNode`. Set
   `QT_ACCESSIBILITY=1` in `ui/src/main.cpp`.
7. **Coord reconciliation** `kde.ToScreenCoords`; join a11y nodes to the §1 window cache.
8. **a11y RPCs + tools** (`snapshotA11y`/`findElement`/`readElementText`) gated on `a11y_read` R1
   with STATE-based password redaction + sensitive-window denylist.
9. **Tests** (context-pack §1 mandate): grant/deny/expiry on each RPC; coalescer batching/dedup
   (copy from agent coalescer tests); walk bounds (depth/node/breadth truncation); password redaction;
   coord-mapping math.

---

## Risks / considerations

- **Bus-name ownership collision.** Two akcore instances racing `io.agentkate.Cowork` → mitigate
  with `.akcore_<pid>` suffix baked into the JS string; `NameFlagDoNotQueue` + fall back to temp-file
  round-trip (option B) if `RequestName` returns not-primary-owner. Cross-ref `01-consent-spine.md`
  if it also wants a bus name — share one `kde.Client` connection.
- **KWin script orphaning on crash.** A loaded event script outlives a crashed akcore until KWin
  restarts; its `callDBus` to the dead name no-ops. Acceptable (self-healing on next start), but the
  one-shot list path **must always `Script.stop`** or scripts accumulate. Defensive: on `kde.New`,
  best-effort enumerate+stop any leftover `ak_*` scripts.
- **a11y not enabled / Qt apps blank.** `QT_ACCESSIBILITY` is per-process; we can't force it on
  third-party apps. Surface a clear "this app does not expose accessibility" rather than empty —
  manage agent expectations. Spike to confirm which common apps (Firefox, Chrome, LibreOffice, VS
  Code) export usable trees on Wayland Plasma 6.
- **Coordinate drift.** Window moves between the a11y walk and the coord join → stale absolutes.
  Mitigate by re-reading the target window's geometry *at reconciliation time* (cheap) and tagging
  results with a capture timestamp; `04-control.md` must re-verify geometry immediately before a click.
- **Secret leakage via a11y (INV-4e).** Covered by STATE/role redaction + sensitive-window denylist +
  audit, but **the denylist is the weak link** — keep it user-editable in the Cowork panel
  (`06-ui-panel.md`) and default-conservative. Treat all read text as injectable untrusted input.
- **Event firehose under window-drag.** `windowActivated`/`screensChanged` can fire dozens/sec; the
  25 ms coalescer + immediate-predecessor dedup + bounded channel keep the bus calm. Validate the
  notification never starves responses (it can't — `enqueue` sheds notifications first, `server.go`).
- **Blocking D-Bus in a handler.** Every introspection RPC dispatches on its own `safe.Go` goroutine
  (`server.go:142`) and carries a context timeout — a hung KWin/a11y call fails that one call, never
  the daemon. **Never hold `cowork.State.mu` across a D-Bus round-trip** (context-pack §1).
- **Cross-slice:** the `Window` model + `ToScreenCoords` are consumed by `04-control.md` (click→coords)
  and `05-sandbox.md` (which VD a window is on); `01-consent-spine.md` owns `Authorize` + audit;
  `06-ui-panel.md` renders the live window list (driven by `cowork.windowEvent`) and the a11y denylist.

---

## Acceptance

- `desktop_list_windows` returns the live window set (caption/class/pid/geom) within ~1 s, gated by a
  `window_list` R0 session grant; denying it returns a clear refusal and no D-Bus call fires.
- The persistent KWin event script loads at boot and is `Script.stop`-ed + bus-name-released on
  shutdown; no `ak_*` scripts leak across a clean restart.
- `cowork.windowEvent` notifications are coalesced (≥25 ms, observed batching under a window drag) and
  the UI window list stays correct purely by re-deriving from `cowork.listWindows`.
- `desktop_read_a11y_tree(pid)` returns a depth/node-bounded tree for one app, `truncated` flagged
  when bounds hit; a password field reads back redacted with `hasText:false`; the read is audited.
- `desktop_find_element` and `desktop_read_element_text` resolve a node `ref` and return its
  screen-absolute extents (window-local + KWin geometry) and text, gated by an `a11y_read` R1 grant.
- A11y RPCs fail-closed on deny/timeout; no a11y data leaves Go without a live grant.
- Tests cover grant/deny/expiry, coalescer batching/dedup, walk bounds, password redaction, and the
  coordinate-mapping math.
