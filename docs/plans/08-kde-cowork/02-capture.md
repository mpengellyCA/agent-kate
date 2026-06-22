# 02 — Capture layer: screenshots, screencast, and the core↔UI portal round-trip

The UI-side capture slice. The agent never touches a Wayland surface, a portal, a PipeWire FD, or a
raw frame; it calls Go `cowork.*` RPCs, and Go borrows the UI to run portal sessions over the
JSON-RPC bus. This file designs that borrow (the **portal round-trip**), the **screenshot** still
path (portal `Screenshot` vs KWin `ScreenShot2`), the **screencast** live path (portal `ScreenCast`
→ KPipeWire preview, stills-on-demand), and the consent/lifecycle/teardown glue. Conforms to INV-1
(portals/PipeWire/parent_window live UI-side), INV-5 (round-trip via `cowork.portalRequest`
notification + `cowork.portalResult` call), and INV-6 (no raw frames on the bus).

## Current state

- **The bus cannot pass FDs and caps a frame at 16 MiB.** Plain `net.Conn` + newline-JSON, no
  `SCM_RIGHTS` (`core/internal/ipc/server.go:19,75,133-141`). Confirmed both ends. ⇒ a PipeWire FD
  or a ScreenShot2 pipe FD **physically cannot** cross to whichever process didn't open it; capture
  must complete in the process that holds the FD, and only encoded bytes/tokens/ids cross.
- **The UI is the only process with a Wayland surface** (`00-context-pack.md` §1). Portal
  `parent_window` *requires* a real surface handle. ⇒ all `Screenshot`/`ScreenCast`/`RemoteDesktop`
  CreateSession/Start calls must originate UI-side. Go has no surface, so a Go-side portal call would
  pass `parent_window=""` and the KDE portal would either reject it or anchor the dialog wrong.
- **The round-trip precedent already exists and works.** `permission.request`
  (`core/cmd/akcore/main.go:1218-1245`) is the canonical rendezvous: a handler calls
  `broker.Open() → (id, chan)`, `srv.Notify("permission.requested", …)` **broadcasts** to every UI
  (`server.go:197-216`), the handler **blocks on the channel** with an 8-min timeout, and a *separate*
  inbound CALL `permission.respond` (`main.go:1247-1259`) runs `broker.Resolve(id, …)` to wake it.
  **The portal round-trip is structurally identical** — I reuse the broker *shape* with a dedicated
  `portalBroker` (the consent broker stays Agent 1's). Each dispatch is its own goroutine
  (`server.go:142`), so a blocked portal handler never wedges other traffic.
- **UI `CoreClient` is a one-way client with a lifetime-guarded callback.** `call(method, params,
  cb, context)` — async, exactly one of result/error, **callback DROPPED if `context` QObject dies
  before the reply** (`ui/src/ipc/CoreClient.h:39`, `.cpp:111-125,158-174`). `notification(QString
  method, QJsonObject params)` Qt signal (`CoreClient.h:47`, `.cpp:177-181`); panels `connect` and
  filter by method string. **There is no UI-served RPC** — the UI is always the JSON-RPC *client*, so
  "core asks UI" must be modelled as notification-out + call-back-in (INV-5).
- **Notifications are lossy under backpressure; responses are not.** `enqueue` sheds the oldest
  notification when the 1024-deep queue fills, but a response (non-nil id) blocks until delivered
  (`server.go:252-290`). ⇒ the `cowork.portalRequest` *notification* could in principle be dropped;
  the design must tolerate that (correlation id + UI-side idempotency + Go-side timeout, never assume
  exactly-once delivery). A large PNG returned in a `cowork.portalResult` *call* is a request frame
  (non-nil id) so it is delivered reliably **as long as it fits the 16 MiB cap** — sizing is on me.
- **Adding a tool = two edits in `mcp.go`** (`toolDefs()` map + `runTool` switch case), result via
  `toolResult(text, isErr)` (`core/cmd/akcore/mcp.go:573-578`); the case does one `b.client.Call(...)`
  and returns a string. Auto-metered. **Adding an RPC = `srv.Handle("cowork.x", fn)` up-front in
  `registerHandlers`** (`main.go:437+`); the map is **not** goroutine-safe post-`Serve` — all static.
- **Build deps present, none linked.** `Qt6::DBus`, `Qt6::WaylandClient`(+`Qt6::GuiPrivate`), and
  `KPipeWire` are installed (verified: `/usr/lib/cmake/{Qt6WaylandClient,Qt6GuiPrivate,KPipeWire}`,
  `libKPipeWire.so.6.7.0`) but `ui/CMakeLists.txt:10,86-106` links only `Qt6::{Core,Gui,Widgets,
  Network}` + KF6. `.desktop` (`ui/org.kde.agentkate.desktop`) has **no**
  `X-KDE-DBUS-Restricted-Interfaces`. Go `go.mod` has **no godbus** (verified: no dbus dep).
- **Host is Plasma 6.7.0** (verified `plasmashell --version`) — *past* the 6.5.x portal-dialog
  virtual-input bug. The design still notes the 6.5.x gotcha for portability.

## Proposed design

### 1. `parent_window` handle acquisition (UI)

A `WindowHandle` helper (`ui/src/cowork/WindowHandle.{h,cpp}`) turns `MainWindow`'s top-level
`QWindow*` into a portal `parent_window` string, branching on the platform:

- **X11:** trivial — `x11:` + hex of the X window id. `QWindow::winId()` is the XID; format
  `QStringLiteral("x11:%1").arg(win->winId(), 0, 16)`. Synchronous, no async export.
- **Wayland:** portals need an **xdg-foreign** exported handle, `wayland:<handle>`. Qt 6.6+ exposes
  this via `QNativeInterface::Private::QWaylandWindow::surfaceRole`? — no; the supported path is
  `qGuiApp->nativeInterface<QNativeInterface::QWaylandApplication>()` for the display and the
  **`xdg_exporter` / `zxdg_exporter_v2`** protocol. Practical route: use the Qt-private
  `QWaylandWindow::requestXdgExportToken()` / the `xdg-foreign` wrapper (needs `Qt6::WaylandClient` +
  `Qt6::GuiPrivate`). Export is **asynchronous** (round-trips the compositor), so `WindowHandle`
  exposes `void handleAsync(QWindow*, std::function<void(QString)> cb)` and caches the token
  (xdg-foreign handles are reusable for the surface's lifetime; re-export on surface
  recreate/`screenChanged`). **Spike:** confirm the exact Qt 6.11 private API name for export — Qt
  has historically gated this behind `QtWaylandClientPrivate`; if unstable, fall back to the
  portal-provided `parent_window=""` (KDE anchors to the active window) for v1 and treat a correct
  handle as a v2 polish. The portal tolerates an empty handle; it just can't perfectly parent the
  dialog.
- **A `PortalSession` (a.k.a. `CoworkPortal`) QObject** (`ui/src/cowork/PortalSession.{h,cpp}`) owns
  all portal D-Bus via `Qt6::DBus` (`QDBusConnection::sessionBus()`, async `QDBusPendingCallWatcher`
  per method, `Response` signal subscription on the returned `Request` object path). One
  `PortalSession` per live screencast; screenshots are one-shot and don't need a persistent session.
  `PortalSession` holds the `parent_window` string, the `restore_token`, the session handle path, and
  the PipeWire node id. It is parented to the Cowork panel (Agent 6) so teardown is automatic on
  panel/window destruction.

**Recommend for v1:** ship the X11 branch fully and the Wayland branch with the
`parent_window=""` fallback if the private export API proves fiddly; do the proper xdg-foreign export
in v2. Screenshots (esp. the no-dialog ScreenShot2 path) don't strictly need `parent_window`, so v1
screenshots are unaffected.

### 2. The core↔UI portal round-trip (INV-5) — deadlock-free, lifetime-safe

This is the heart of the slice. **Choreography for one capture** (e.g. a portal screenshot the
agent requested):

1. Agent calls MCP `desktop_screenshot(target?)` → bridge `runTool` case does **one**
   `b.client.CallTimeout("cowork.screenshot", args, &res, 130s)` into core (longer than the portal's
   own 120 s timeout from the skill snippet so the bridge always gets a definitive answer, mirroring
   the permission 8-min-<-10-min staggering at `main.go:1241`).
2. Core handler `cowork.screenshot` (in `registerHandlers`): **(a)** calls Agent 1's
   `consent.Authorize(threadId, capability=screenshot, target, scope)` — if denied, return an
   `*RPCError` immediately (fail-closed, no UI borrow). **(b)** If allowed and the chosen backend is
   UI-side (portal, or ScreenShot2-from-UI), open a `portalBroker.Open() → (corrId, chan)`, then
   `srv.Notify("cowork.portalRequest", {corrId, op:"screenshot", target, interactive, parentHint})`
   (broadcast), then `select { case res := <-chan: …; case <-time.After(125s): portalBroker.Close;
   return RPCError("portal timeout") }`. **The handler runs on its own dispatch goroutine** so this
   block isolates one request.
3. **UI side:** the Cowork controller (Agent 6's panel, or a headless `CoworkController` QObject if
   no panel is open) `connect`s to `CoreClient::notification`, filters `method ==
   "cowork.portalRequest"`, dedupes on `corrId` (idempotency — a re-delivered notification is
   ignored if `corrId` already in-flight/done). It runs the `PortalSession` op asynchronously
   (acquire `parent_window`, call portal, read FD, encode PNG). **All portal callbacks are
   lifetime-guarded:** every `CoreClient::call(..., context)` passes the controller QObject as
   `context`, so if the panel/window closes mid-capture the late reply is dropped, not a UAF
   (`CoreClient.cpp:169-174`).
4. UI replies with a **normal CALL** `cowork.portalResult({corrId, ok, restoreToken?, nodeId?,
   pngB64?, mime, width, height, error?})`. Because this is a request frame (non-nil id) it is
   delivered reliably under backpressure (`server.go:253-260`) — the PNG isn't shed. Core's
   `cowork.portalResult` handler runs `portalBroker.Resolve(corrId, result)`, waking the blocked
   `cowork.screenshot` handler, which then returns the artifact to the bridge → tool result.

**Deadlock-free proof.** Two independent goroutines, no shared lock held across the wait. The
`cowork.screenshot` handler blocks on a *buffered(1)* channel; the waker (`cowork.portalResult`) is a
*different* inbound frame on a *different* dispatch goroutine (`server.go:142`), never the same one.
The UI never blocks waiting on core inside this flow — `CoreClient::call` is fire-and-callback, and
the UI's portal D-Bus calls are async (`QDBusPendingCallWatcher`), so the UI event loop keeps
spinning. No cycle: core→UI is a *notification* (best-effort, non-blocking enqueue for the producer);
UI→core is an async call. The only blocking wait is the handler on its channel, bounded by a
timeout. Even if the notification is **shed** (queue full) or **no UI is connected**, the handler
just times out at 125 s and returns a fail-closed error — never hangs.

**Lifetime-safe proof.** (a) Core: `portalBroker.Resolve` deletes-then-sends under mutex, channel is
buffered(1), so a late `cowork.portalResult` after timeout finds no entry and is a harmless no-op
(same as `permission.Broker.Resolve` returning false, `broker.go:49-60`). (b) UI: the callback
context-guard drops replies whose `PortalSession`/controller died (`CoreClient.cpp:169`). (c) The
`PortalSession` is parented to a QObject; window-close destroys it, aborting in-flight `QDBusPending`
watchers.

**Correlation id:** `corrId = "cap-" + 6 random bytes hex` minted by `portalBroker.Open()` (reuse the
broker id scheme, `broker.go:36-38`). Echoed in the notification and required in the result.

**Timeout & fail-closed ladder (staggered, longest outermost):** bridge `CallTimeout` 130 s > core
handler wait 125 s > portal-internal timeout 120 s (skill snippet) > UI op budget ~115 s. **No UI
connected** ⇒ `srv.Notify` enumerates zero conns (`server.go:206-211`), nobody replies, handler times
out → `RPCError("cowork: no UI available to run portal")` → tool returns an error string the agent
sees. **Multiple UIs connected** ⇒ broadcast reaches all; the *first* `cowork.portalResult` with a
given `corrId` wins (Resolve deletes the entry; later ones no-op). To avoid two UIs both popping a
portal dialog, v1 designates the **primary UI** (the one that owns the akcore QProcess; it sets a
`primary:true` flag at handshake) as the sole portal runner — secondary/attached UIs ignore
`cowork.portalRequest`. (Multi-UI is rare today; this is a cheap guard.)

### 3. Screenshot path — backend choice, sizing, delivery

Two backends, decided **per tier** and per process:

- **Portal `Screenshot`** (`org.freedesktop.portal.Screenshot.Screenshot(parent, {interactive})`):
  native, consent-dialog-mediated, returns a `uri` to a saved PNG (the portal writes a file under
  `$XDG_*`). Pros: user sees exactly what's shared, KDE-native, no `.desktop` decl. Cons: a dialog
  per shot (consent fatigue), slower. **Use when interactive=true is desired** (first-time / picker).
- **KWin `ScreenShot2`** (`org.kde.KWin.ScreenShot2.CaptureActiveWindow/CaptureWindow/CaptureArea/
  CaptureScreen`, returns a **pipe FD** of raw RGBA): fast (~30–70 ms), **no dialog**. Needs the
  `.desktop` `X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2` decl, else `NoAuthorized`.
  **Use for repeat captures inside an already-consented grant** (the consent gate is our own Go
  `Authorize`, not a KWin dialog).

**Which process calls ScreenShot2?** The decision pivots on two process-local facts: **(1) the pipe
FD is local to the caller** — whoever calls `ScreenShot2` must read that FD in-process; it can't be
relayed. **(2) the `.desktop` restricted-interface authorization is keyed to the installed app id**
(`StartupWMClass`/desktop file name = the app KWin sees making the D-Bus call). KWin checks the
*caller's* desktop-file authorization. **Both point to the UI:** the UI is the app whose `.desktop`
file we install and whose process identity KWin authorizes; and the UI can read the FD and encode
without crossing the bus. **If Go called ScreenShot2 it would be `NoAuthorized`** (Go has no installed
`.desktop` entry; akcore is a child binary, not a registered application) **and** the FD would be
stuck in Go — fine for encoding there, but the authorization blocker is fatal. **Recommendation: the
UI calls ScreenShot2** (and portal `Screenshot`), encodes, and returns bytes via
`cowork.portalResult`. This keeps INV-1 clean (one process owns all capture) and dodges the
authorization split-brain. *(Deviation note: the skill's MCP-mapping table loosely tags screenshots
"desktop-decl" without fixing the process; INV-1 already mandates UI-side, so no real deviation —
just making the FD/authorization reasoning explicit.)*

**Per-tier recommendation:**
- **v1 (R1 screenshot, default):** UI runs **ScreenShot2** for speed once a `screenshot` grant
  exists, **portal `Screenshot` (interactive)** for the very first capture in a session so the user
  visibly consents to *which* surface. (i.e. portal = consent ceremony, ScreenShot2 = the workhorse.)
- The `desktop_screenshot(target?)` arg picks `CaptureActiveWindow` (no target), `CaptureWindow`
  (target = KWin internalId from `desktop_list_windows`), `CaptureScreen` (target = screen), or
  `CaptureArea(x,y,w,h)` (target = region).

**Frame-size budget (the 16 MiB cap is the hard constraint).** A 4K screen RGBA frame is
3840×2160×4 ≈ **33.2 MiB raw** — already over cap *before* base64's +33 %. So the UI **must** downscale
and encode before returning:
- **Default max dimension 1568 px** on the longest edge (matches common vision-model tiling and keeps
  detail), `QImage::scaled(…, Qt::KeepAspectRatio, Qt::SmoothTransformation)`. A 1568×882 image is
  ~1.4 M px.
- **Encoding:** **PNG for window/UI captures** (sharp text, lossless, typically 200 KB–1.5 MB at this
  size) is the v1 default; **JPEG q≈85 for full-screen/photographic** content (smaller, ~150–600 KB).
  Either way, well under cap even after base64. The tool/RPC accepts an optional `maxDim` and
  `format` to let the agent trade detail vs size.
- **Hard guard:** the UI checks encoded size; if `> 8 MiB` (half the cap, leaving JSON+base64
  headroom) it re-encodes at a smaller `maxDim`/lower quality and, failing that, returns an error
  rather than a frame the bus would reject (a >16 MiB inbound line would be silently dropped by the
  scanner buffer, `server.go:134`). Region captures are naturally small.

**How the PNG reaches the tool result — base64-in-JSON vs temp-file path.** **Recommend base64
in-JSON for v1** (`cowork.portalResult.pngB64`), because (a) it round-trips entirely over the
existing bus with no shared-filesystem assumption, (b) the agent's MCP result can embed an
*image content block* (`{type:"image", source:{type:"base64", media_type, data}}`) so the model
*sees* the screenshot natively — far more useful than a path string. The `desktop_screenshot` tool
returns an MCP image content block (extend `toolResult` to allow an image block, or add a small
`toolImageResult` helper) so the screenshot lands in the model's context directly. **v2 option:** for
very large or many captures, write to a core-owned temp dir
(`$XDG_RUNTIME_DIR/agentkate/captures/<corrId>.png`, 0600, **auto-deleted after the tool returns and
on session end** — never persisted past the live session per INV-4/INV-5) and return the *path*; but
since the model can't read an arbitrary host path through MCP, base64-image is strictly better for the
common case. Keep base64 default.

### 4. Screencast path — portal `ScreenCast` → KPipeWire preview, stills-on-demand

Live cast is **entirely UI-side**; the agent never gets a frame firehose (INV-6). Flow:

1. Agent calls `desktop_start_screencast(target)` → `cowork.startScreencast` handler → `Authorize`
   (capability=`screencast`) → portal round-trip op `op:"startScreencast"`.
2. **UI `PortalSession.startScreencast`:** `ScreenCast.CreateSession` → `SelectSources({types,
   multiple:false, persist_mode:2, restore_token:<saved?>})` → `Start(session, parent_window, {})` →
   on `Response`, read `streams` (node ids) and the **new `restore_token`** → `OpenPipeWireRemote`
   → **PipeWire FD stays in the UI**, handed to **KPipeWire** (`PipeWireSourceItem`/`PipeWireRecord`)
   for the preview widget (built by **Design Agent 6** — this slice only specifies the contract: the
   UI gets a `node_id` + FD and renders a live thumbnail). The UI keeps a `PortalSession` alive,
   tracked by its `restore_token` and node id.
3. UI replies `cowork.portalResult({corrId, ok, nodeId, castToken})` where **`castToken` is the
   Go-facing handle** the agent uses to stop or capture from this cast — *not* the portal restore
   token. Core stores `cast{castToken → corrId/threadId/nodeId, restoreToken}` in `cowork` state and
   returns `castToken` to the agent.
4. **Agent gets stills on demand, not frames.** `desktop_screenshot` (or an internal
   `cowork.captureStill(castToken)`) against an active cast: the UI grabs the **current frame from
   the live KPipeWire stream** (it already has the buffer), encodes a PNG per §3 sizing, returns
   base64. This is the recommended way the agent "watches" — pull a still when it needs to look, no
   continuous decode/encode/transmit load, no bus saturation. **No frame-pull firehose in v1.**

**`persist_mode` / `restore_token` rotation (single-use!).** Each `Start` returns a **new**
restore token; the old one is spent (skill: "single-use … rotate"). The UI **must** capture the new
token from every `Start` Response and ship it to core, which **atomically persists it** (Agent 1's
grant store, alongside the `screencast` grant: `restoreToken` field, temp-file+rename per house norm).
Reusing a spent token yields a fresh consent prompt. Rotation rule: on each restart/restore, save the
returned token; never reuse a token twice; on revoke, discard the token (next cast re-prompts).
`persist_mode:2` (persistent) lets a re-cast in the same session skip the dialog *if* we feed back the
saved token — the consent UX win that makes repeated casts bearable.

### 5. Consent integration (calls into Agent 1's `Authorize()`)

Every UI-side capture is gated **in Go, before any portal borrow**, by Agent 1's consent authority
(INV-2). The mapping:

| Tool / RPC | capability | target | scope (default) | tier |
|---|---|---|---|---|
| `desktop_screenshot` → `cowork.screenshot` | `screenshot` | active-window / window id / screen / region | `timed` (e.g. 10 min) or `once` for first shot | R1 |
| `desktop_start_screencast` → `cowork.startScreencast` | `screencast` | window / monitor / virtual | `session` or `timed`, preview-on | R1 |
| `cowork.captureStill` (internal, against a cast) | rides the existing `screencast` grant | the cast's target | — | R1 |
| `desktop_stop_screencast` → `cowork.stopScreencast` | — (teardown, no new grant) | the cast | — | — |

- **Authorize call:** `grant, err := d.consent.Authorize(ctx, threadId, cowork.Capability{...},
  target, scope)`. Deny ⇒ `*RPCError`, no notification emitted (the user never even sees a portal
  dialog — fail-closed at the Go gate). Allow ⇒ proceed to the portal round-trip. Note the **double
  gate** for portal screenshots: our Go `Authorize` *plus* the portal's own dialog (if interactive) —
  belt and suspenders; for ScreenShot2 (no dialog) our Go gate is the *only* gate, which is exactly
  why the capability must be granted first.
- **Redaction posture (INV-4e).** (a) **Exclude AK's own windows** from screenshot/cast targets by
  default — the consent UI and grant files must not be capturable into the agent's context
  (self-injection guard). (b) **Sensitive-window exclusion:** before a ScreenShot2/portal capture,
  check the target window against a denylist (password managers, the system auth agent, windows whose
  KWin `resourceClass` is on a sensitive list, and any window flagged `skipTaskbar` + a known-secret
  class); if the active window is sensitive, **refuse the capture** with a clear error rather than
  leaking it. (c) **Never persist raw frames past the live session:** PNG bytes live only in the
  in-flight RPC result; any temp file (v2) is 0600, auto-unlinked after the tool returns and on
  session/kill-switch. KPipeWire buffers are released on cast stop.
- Treat captured pixels as **untrusted input** (they can carry prompt-injection text rendered on
  screen) — that's an agent-context concern flagged here, enforced upstream.

### 6. Lifecycle / teardown

- **Stop on UI disconnect:** when the last UI disconnects (`OnAllClientsGone`, `server.go:65,128`),
  core marks all live casts dead and clears in-flight `portalBroker` entries (Resolve-with-error or
  Close) so no handler hangs; the UI process dying already drops its PipeWire FDs.
- **Kill-switch (INV-4d):** Agent 1's global kill-switch fires `cowork.killSwitch` (broadcast); the
  UI controller tears down **every** `PortalSession` (stop PipeWire, close portal sessions, drop
  preview widgets), Go discards all cast handles + restore tokens. Idempotent.
- **Window close / panel close:** `PortalSession` is QObject-parented to the panel; destruction
  aborts `QDBusPending` watchers and stops streams. The lifetime-guard then drops any late
  `cowork.portalResult` callback.
- **`desktop_stop_screencast(token)` → `cowork.stopScreencast`:** portal round-trip `op:"stopCast"`
  → UI stops the KPipeWire stream + closes that `ScreenCast` session; Go forgets the handle.
- **Restore-token rotation on teardown:** on revoke/kill-switch, discard saved restore tokens so the
  next cast re-prompts (no silent re-grant via a stale persistent token).
- **Clean portal sessions:** always `Close` the portal session object path on stop; never leak
  `/org/freedesktop/portal/desktop/session/...` handles. On core `gracefulShutdown`
  (`main.go:302-323`), broadcast `cowork.killSwitch` first so the UI tears down before the socket dies.

### 7. Plasma gotchas

- **Plasma 6.5.x portal dialogs ignore virtual input** (ydotool/VNC can't click the consent dialog).
  Our host is 6.7.0 (fixed), but for portability: on 6.5.x, the **first** consent needs a *physical*
  click; thereafter the persistent `restore_token` skips the dialog. Document the
  pre-authorization escape hatch: `flatpak permission-set kde-authorized screencast "" yes` (or by
  app-id) writes the `kde-authorized` permission-store table so the portal stops prompting — for
  power-users/CI only, surfaced as an opt-in note, never auto-run by AK (auto-running it would defeat
  consent).
- **ScreenShot2 `NoAuthorized`** until the installed `.desktop` carries
  `X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2` — only effective for an *installed*
  build (dogfood-from-build-dir won't have it). v1 acceptance must test an installed build; the
  portal `Screenshot` path is the build-dir fallback that needs no decl.
- **Empty `parent_window`** is tolerated by the KDE portal (anchors to active window) — the v1
  Wayland fallback if xdg-foreign export is fiddly.

### 8. MCP tools + core RPCs + notifications (introduced by this slice)

**MCP tools (`mcp.go` — `toolDefs` + `runTool`):**
- `desktop_screenshot(target?, maxDim?, format?)` → `cowork.screenshot` → returns an MCP **image
  content block** (base64 PNG/JPEG).
- `desktop_start_screencast(target)` → `cowork.startScreencast` → returns `castToken` (string).
- `desktop_stop_screencast(token)` → `cowork.stopScreencast` → returns ok.

**Core RPCs (`registerHandlers`):**
- `cowork.screenshot` (agent→core; does `Authorize` + portal round-trip; returns image artifact).
- `cowork.startScreencast` / `cowork.stopScreencast` (agent→core; round-trip).
- `cowork.captureStill` (internal/agent→core; still from an active cast).
- **`cowork.portalResult`** (UI→core CALL; resolves the `portalBroker` — the *only* new inbound RPC
  the UI initiates for this slice).

**Notifications (core→UI):**
- **`cowork.portalRequest`** `{corrId, op, target, interactive?, parentHint?, restoreToken?}` — the
  borrow request.
- `cowork.killSwitch` (shared with Agent 1; this slice consumes it).

## Implementation steps

1. **Build wiring (S).** `ui/CMakeLists.txt`: add `Qt6::DBus`, `Qt6::WaylandClient`, `Qt6::GuiPrivate`,
   `KPipeWire` to `find_package` + `target_link_libraries`; add `src/cowork/*.cpp`. Go: `go get
   github.com/godbus/dbus/v5@v5.1.0` (verify go 1.26), `go mod tidy`. `.desktop`: add
   `X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2`.
2. **Round-trip skeleton (M).** Core: a `portalBroker` (clone of `permission.Broker` shape) in
   `core/internal/kde/portalcoord.go`; register `cowork.portalResult` + a trivial `cowork.screenshot`
   that does the Notify/block/timeout dance and returns a stub. UI: `CoworkController` QObject
   connecting `notification`, dispatching on `cowork.portalRequest`, replying `cowork.portalResult` —
   first with a hard-coded 1×1 PNG to prove the loop end-to-end (no portal yet).
3. **`WindowHandle` + `PortalSession` (M).** X11 branch + Wayland export (or `""` fallback); portal
   D-Bus plumbing (`QDBusPendingCallWatcher`, `Response` signal).
4. **Screenshot real (M).** ScreenShot2 FD read + encode + sizing/guard; portal `Screenshot`
   interactive path; consent gate via Agent 1 `Authorize`; sensitive/AK-window exclusion; MCP image
   content block.
5. **Screencast (L).** Portal `ScreenCast` sequence, KPipeWire FD hand-off to Agent 6's preview,
   `castToken` mapping, restore-token rotation + persistence, `captureStill`, `stop`.
6. **Lifecycle (S–M).** `OnAllClientsGone`, `cowork.killSwitch`, panel-parented teardown,
   `gracefulShutdown` broadcast, token rotation on revoke.
7. **Tests (M).** Go: round-trip resolve/timeout/no-UI/double-resolve/concurrent (mirror the broker
   test set, INV-2). UI: notification-dispatch + lifetime-guard drop on context death.

## Risks / considerations

- **Wayland xdg-foreign export API churn** — Qt private. *Mitigation:* `parent_window=""` fallback for
  v1; treat proper export as v2. **Spike** the exact Qt 6.11 symbol first.
- **16 MiB cap vs large frames** — a careless full-4K PNG overflows the bus and is silently dropped.
  *Mitigation:* mandatory downscale to 1568 px + 8 MiB pre-flight guard + error-rather-than-overflow.
- **Notification loss / no-UI** — `cowork.portalRequest` can be shed or land on zero conns.
  *Mitigation:* the whole flow is timeout-bounded and fail-closed; never assume delivery. See Agent 1
  for the symmetric consent-prompt loss handling.
- **ScreenShot2 authorization is install-only** — build-dir dogfooding gets `NoAuthorized`.
  *Mitigation:* portal `Screenshot` fallback; acceptance tests run an installed build.
- **Restore-token single-use** — reuse re-prompts or fails. *Mitigation:* capture the new token from
  every `Start`, persist atomically, never reuse, discard on revoke. Cross-ref `01-consent-spine.md`
  grant store schema.
- **Two-UI double-dialog** — *Mitigation:* primary-UI-only portal runner in v1.
- **Captured pixels as injection vector** — flagged for the agent-context layer; this slice enforces
  AK-window + sensitive-window exclusion at capture time.

## Acceptance

- With the `cowork` MCP server enabled and a `screenshot` grant, `desktop_screenshot` returns a
  PNG the model *sees* (image content block), ≤ ~1.5 MB, of the requested window/screen/region.
- Denied/absent grant ⇒ tool returns a clear error and **no portal dialog ever appears**.
- No UI connected ⇒ the tool times out and errors within ~130 s; the core never hangs and serves
  other RPCs throughout.
- `desktop_start_screencast` opens a portal dialog (or restores via saved token), shows a live
  preview in the Cowork panel, returns a `castToken`; subsequent `desktop_screenshot` against the
  cast returns current-frame stills; **no continuous frames cross the bus.**
- `desktop_stop_screencast` / kill-switch / window-close all tear down the PipeWire stream + portal
  session and rotate/discard the restore token; no leaked session paths, no late-callback crashes.
- ScreenShot2 works on an installed build (with the `.desktop` decl); portal `Screenshot` works from
  the build dir.

## Sizing & phasing

- **parent_window + round-trip skeleton:** M — **v1** (riskiest seam; prove it cheap per INV-7).
- **Screenshot (ScreenShot2 + portal, sizing, consent gate, image block):** M — **v1**.
- **Lifecycle/teardown/kill-switch:** S–M — **v1**.
- **Wayland proper xdg-foreign export:** S — **v2** (v1 uses `""` fallback).
- **Screencast (portal ScreenCast + KPipeWire preview + token rotation + captureStill):** L —
  **v2** (Agent 6 builds the preview widget; this slice provides node-id/FD + still-grab contract).
- **Temp-file path delivery / region-tiling / multi-still batching:** S — **v3** (base64-image
  covers the common case first).
- **Deliberate throttled frame-pull (beyond on-demand stills):** M — **v3**, only if a use-case
  demands it; default stays stills-on-demand per INV-6.
