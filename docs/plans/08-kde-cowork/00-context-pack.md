# 08 — KDE Plasma Cowork: Context Pack (shared substrate)

> **Read this first.** This is the curated, grounded substrate for every design/optimize/review
> agent working on the KDE Plasma Cowork feature. It fixes the facts (from codebase recon) and
> the **shared design invariants** so parallel design work stays coherent. Do not re-explore the
> codebase for things stated here with `file:line` — trust them, and only dig when you need
> detail beyond what's captured.

---

## 0. What we're building

A **Cowork** capability: let the user share parts of their KDE Plasma desktop with Agent Kate
(an AI agent) — see a window / a screen / the whole desktop / an isolated virtual-desktop
sandbox — and optionally let the agent *control* it (drive a browser, LibreOffice, etc.), with
**every access gated by explicit, auditable, revocable user permission.** Three API layers:

1. **XDG Desktop Portal** — consent-gated screen capture (`Screenshot`, `ScreenCast`) and remote
   desktop input (`RemoteDesktop`), via PipeWire. *(Skill doc Layer 1.)*
2. **KWin D-Bus scripting** — no-prompt window enumeration + workspace/virtual-desktop events.
   *(Skill doc Layer 2.)*
3. **AT-SPI2** — structured accessibility tree for UI introspection + widget actions.
   *(Skill doc Layer 3.)*

Reference skill: `KDEPlasmaCoworkSKILL.md` (in the task prompt). It has the D-Bus method
signatures, options vardicts, AtspiRole table, and Go snippets — treat it as the API reference.

---

## 1. Architecture facts (grounded)

### Two processes, one JSON-RPC bus
- **`agentkate`** — C++/Qt6/KF6 UI (`ui/`). Thin: renders + input. Owns the core's lifecycle as a
  `QProcess` child. **The only process with a Wayland surface.** Qt 6.11, KF6.
- **`akcore`** — Go core (`core/`). Owns the agent supervisor, the `agentkate-cooperation` MCP
  stdio bridge, worktrees, sessions, LSP. Go 1.26.
- **Bus:** newline-delimited **JSON-RPC 2.0** over a **Unix domain socket**
  (`$XDG_RUNTIME_DIR/agentkate-<pid>.sock`). Envelope `ipc.Frame{jsonrpc,id?,method?,params?,result?,error?}`
  (`core/internal/ipc/protocol.go:13-20`). Frame cap **16 MiB** (`server.go:19`).
- **⛔ NO file-descriptor passing.** Plain `net.Conn` + `bufio` JSON only — no `SCM_RIGHTS`, no
  ancillary data, anywhere (`server.go:75,133-141`, `client.go:32`). Confirmed both ends.

### MCP tool surface (how the agent calls anything)
- The MCP server is a **per-thread stdio bridge subprocess** (`akcore mcp …`), not in-process.
  `claude` is spawned with `--mcp-config <tmp>` → `mcpServers.cooperation` → the bridge
  (`core/cmd/akcore/main.go:2778-2806`, `core/internal/agent/agent.go:288,309-316`).
- **Add a tool = two edits in `core/cmd/akcore/mcp.go`:** (1) append a `{name,description,inputSchema}`
  map to `toolDefs()` (`mcp.go:446-571`); (2) add a `case` to the `switch name` in `runTool`
  (`mcp.go:132-442`) that unmarshals args, does work (usually one `b.client.Call("ns.method", …)`
  into core), and returns a plain `string`. Result shape fixed by `toolResult` (`mcp.go:573-578`).
- **Tools are auto-metered** by `toolMeter`/`usageMeter` observing the stream-json (`toolmeter.go`,
  wired `agent.go:551-552`). New tools need **zero** metering wiring; optional one-line
  `summarizeInput` case for nicer telemetry.

### IPC handlers (core side)
- Register **up-front** in `registerHandlers` (`main.go:437+`) via `srv.Handle("ns.method", fn)`
  where `Handler = func(ctx, json.RawMessage)(any,error)` (`server.go:32`). **The handler map is
  NOT goroutine-safe after `Serve` starts** — all registration is static, no lazy/dynamic tools.
- Each inbound frame dispatches on its **own** `safe.Go("ipc.dispatch",…)` goroutine
  (`server.go:142`) — blocking handlers are fine but **must carry a timeout**.
- `handlerDeps` struct (`main.go:420-434`) is how handlers reach subsystems.

### Server→UI push (events)
- `srv.Notify(method, params)` **broadcasts a notification to every connected client**
  (`server.go:197-216`). Examples: `permission.requested`, `agent.event`, `git.invalidated`,
  `vsix.installProgress`.
- **Best-effort + lossy under backpressure:** per-conn writer goroutine drains a bounded
  `out chan` (depth `outboundBuffer=1024`, 30 s write deadline). **Responses (with id) block until
  delivered; notifications (no id) are shed oldest-first when the queue is full** (`enqueue`,
  `server.go:252-290`). ⇒ **High-rate events MUST be coalesced/throttled and the UI MUST be able
  to re-derive state from a pull-based snapshot RPC.** Precedent: agent events coalesce on a 25 ms
  window (`agent.go:158-236`); vsix throttles to 250 ms/1%.
- **UI side:** `CoreClient` emits Qt signal `notification(QString method, QJsonObject params)`
  (`ui/src/ipc/CoreClient.h:47`, `.cpp:177-181`). Panels `connect` to it and filter by method
  string (often `Qt::QueuedConnection`). **No subscribe RPC** — it's broadcast + client-side filter.

### UI RPC + panels
- `CoreClient::call(method, params, cb, context)` — async; exactly one of `result`/`error` is set;
  **`context` QObject lifetime-guards the callback** (drops it if the context dies — the Tier-0 UAF
  fix). (`CoreClient.h:39`, `.cpp:111-125`.)
- **Add a side panel** via `MainWindow::registerPanel(key, icon, label, widget, defaultStrip)`
  (`MainWindow.cpp:1009-1038`); call sites `:198-243`. Subclass `QWidget`, take `CoreClient*`, add
  `.cpp` to `ui/CMakeLists.txt`. The "Cooperation" stub at `:213-218` is the closest analog. Panel
  strip/raise state auto-persists to KConfig.
- **Dialogs/consent UI:** subclass `QDialog`; for destructive/consent confirmation the house
  pattern is **`KMessageBox::warningContinueCancelList` + `KMessageBox::Dangerous`** (itemized,
  Cancel-defaulted) — `CleanupDialog.cpp:480-491`. Native colors via `KColorScheme`, never raw RGB.
- **Settings:** `KSharedConfig::openConfig()->group("X").read/writeEntry`, inline at call site
  (`~/.config/agentkaterc`). Precedent that matters: the UI **refuses to persist
  `bypassPermissions`** and remaps it to a safe default (`AgentPanel.cpp:620-640`) — "never make the
  unsafe choice sticky."

### Permission broker (the spine we extend — but it's the wrong shape as-is)
- `permission.Broker` (`core/internal/permission/broker.go`): **per-call, ephemeral, in-memory,
  default-deny-on-timeout.** `Open()`→`perm-<hex>` id + buffered(1) `chan Decision`; `Resolve(id,
  Decision{Allow,UpdatedInput})`; `Close(id)`. No persistence, no scopes, no "remember", no
  allowlist.
- **Flow:** `claude` calls MCP `request_permission` (wired via `--permission-prompt-tool
  mcp__cooperation__request_permission`, `agent.go:314`) → bridge `CallTimeout("permission.request",
  …, 10 min)` (`mcp.go:389-438`) → core handler `broker.Open()` +
  `srv.Notify("permission.requested",…)` + **blocks 8 min** (`main.go:1218-1245`) → UI banner
  `m_permBar` Approve/Deny (`AgentPanel.cpp:2390-2484`, one-at-a-time `showNextPermission`) →
  `permission.respond{requestId,allow,updatedInput?}` → `broker.Resolve`. **Core self-denies at 8 min
  < bridge 10 min** so the caller always gets a definitive answer. Fail-closed on transport error.
- **Important:** the broker is NOT the dominant gate today — Claude Code's own `--permission-mode`
  (default `acceptEdits`) decides whether a tool even escalates; Cooperation MCP is force-allowed via
  `--allowedTools mcp__cooperation`. So Agent Kate has no server-side policy table yet; it only
  renders what Claude escalates.

### Persistence + concurrency norms
- **Atomic persistence:** temp-file + `os.Rename`, `MkdirAll` first, archive-before-delete. Session
  store `session.Record` → `$XDG_DATA_HOME/agentkate/threads.json` (`session.go`), already persists a
  per-thread `PermissionMode`. Natural home pattern for durable grants.
- **`safe.Go("pkg.site", fn)` for EVERY goroutine** (`safe/safe.go:22`) — recover+log; a bare `go`
  panic crashes the daemon. Timer/`AfterFunc` bodies too.
- Mutex-guard every shared map; **copy-out** on read (clone, never return internal pointers). Bound
  every queue. Don't hold a mutex across a blocking D-Bus call. Explicit `Close`/teardown in
  `gracefulShutdown` (`main.go:302-323`).
- New consent logic **must have tests** (grant/deny/expiry/revocation/timeout/concurrent-resolve).

### Build / packaging
- UI: append `.cpp` to `ui/CMakeLists.txt` `add_executable` list; AUTOMOC handles `Q_OBJECT`. Add
  Qt/KF6 modules to **both** `find_package` and `target_link_libraries`. Today links only
  `Qt6::{Core,Gui,Widgets,Network}` — **need to add `Qt6::DBus` (required), `Qt6::WaylandClient`
  (+`Qt6::GuiPrivate`) for the surface handle, and (for live preview) `KPipeWire`.** All present on
  system, none linked.
- Core: `go.mod` (module `agentkate`, go 1.26) has **no D-Bus lib** — add `github.com/godbus/dbus/v5`
  (verify go 1.26 compat). `go mod tidy`; no CMake change for Go sources.
- **`.desktop` file** `ui/org.kde.agentkate.desktop` must add
  `X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2` (only effective for the *installed*
  app — dogfood-from-build-dir won't have it; test against an installed build). Installs to
  `${KDE_INSTALL_APPDIR}`.

### Plan-document house style (match exactly)
- `docs/plans/NN-slug.md`, indexed in `docs/plans/README.md`. Ours: **`docs/plans/08-kde-cowork/`**
  (a directory — write multiple plan files + a sub-`README.md`, add one row to the top README).
- Per-file shape: `# NN — Title` → 1–3 sentence framing → **`## Current state`** (every claim with
  `file:line`, **bold** the load-bearing fact, note precedents) → **`## Proposed design`** (numbered
  concrete steps naming exact symbols; offer alternatives with an explicit **"Recommend X for v1"**;
  flag **Spikes**) → **`## Implementation steps`** (ordered, cross-process) → **`## Risks /
  considerations`** (bold lead-ins, concrete mitigations, cross-ref sibling plans) → **`##
  Acceptance`** (observable bullets).
- **Size key (verbatim):** `S ≈ <½ day, M ≈ 1–2 days, L ≈ 3–5 days`. Use M–L for spanning items.

---

## 2. SHARED DESIGN INVARIANTS (Director-set — all design agents MUST conform)

These are decisions fixed up-front so six parallel design slices compose into one coherent system.
If a slice has a strong reason to deviate, it must say so explicitly and justify it.

### INV-1 — Process boundary
- **UI (Qt6) owns:** all XDG portal sessions (`Screenshot`, `ScreenCast`, `RemoteDesktop`), the
  `parent_window` Wayland surface handle (via `MainWindow::windowHandle()` + xdg-foreign export),
  PipeWire stream consumption (KPipeWire), and live preview rendering. Frames/FDs **never** cross to
  Go.
- **Core (Go) owns:** the **consent authority** (grant store + policy + audit), KWin D-Bus scripting
  (window list + events — Go can call D-Bus directly), AT-SPI2 tree read + actions (Go D-Bus), and
  the MCP tool surface. The agent talks *only* to Go tools.
- **The seam:** when a tool needs a portal capability (capture/control), Go asks the UI (via a new
  RPC the UI serves, or a notification + response) to run the portal session and return only
  **serializable artifacts**: `restore_token` (string), PipeWire `node_id` (int), or a
  PNG-on-demand (base64/bytes within the 16 MiB frame cap). The UI may keep the live stream entirely
  to itself and hand the agent **encoded still frames** on request.

### INV-2 — Consent is a NEW durable subsystem, not the old broker
- Build `core/internal/cowork` (consent + state) and `core/internal/kde` (raw D-Bus/AT-SPI/portal-
  coordination clients). **Reuse the broker's request/notify/respond *rendezvous shape* for
  interactive prompts; add a separate, persisted, capability-scoped, revocable Grant store** —
  authoritative in Go, atomic-written to `$XDG_DATA_HOME/agentkate/cowork-consents.json`. The UI is
  never the source of truth for what's allowed.
- **Grant model (baseline — slices may extend):** `Grant{ id, threadId, capability, target, scope,
  grantedAt, expiresAt?, revokedAt? }` where `capability ∈ {window_list, a11y_read, a11y_action,
  screenshot, screencast, input_inject, remote_desktop, vd_sandbox}`, `target` narrows to a
  window/app/screen/region/virtual-desktop, `scope ∈ {once, session, timed, until_revoked}`. Grants
  are **per-thread**, never global.

### INV-3 — Risk tiers (drives prompt strength + default scope)
- **Tier R0 — passive metadata** (`window_list`): low-sensitivity but still leaks app usage →
  session-scoped consent, standard prompt.
- **Tier R1 — read content** (`a11y_read`, `screenshot`, `screencast`): can capture secrets →
  per-target consent, timed/session scope, redaction posture, audit each capture.
- **Tier R2 — control** (`a11y_action`, `input_inject`, `remote_desktop`): arbitrary action as the
  user → **default-off, strong/distinct confirmation UI, per-action by default, never auto-remembered,
  cannot be enabled by `--permission-mode`** (gate lives imperatively inside the Go tool, not just in
  Claude's prompt funnel).
- Tier escalation is one-way per grant; a `screencast` grant never implies `input_inject`.

### INV-4 — Defense in depth against the threat model
Every slice must address, where relevant: (a) prompt-injection→desktop escalation (the prompt shows
the **concrete** action — keystrokes/coords/target window — not a vague tool name); (b) consent
fatigue (risk-tiered: scoped/timed grants for reads, per-action for control; never make the safe path
the annoying one); (c) scope creep (independent per-capability, per-target grants); (d) **audit +
revocation** (append-only audit log of every grant + every executed action with target; a live
"active grants" surface with one-click revoke; a global **kill-switch** that cuts all desktop access
across all threads); (e) secret capture (sensitive-window/password-field exclusion, never persist raw
frames beyond the live session, treat captured pixels/text as untrusted input that can itself carry
injection); (f) self-approval loops (the AK consent UI and consent files are **outside** the agent's
injectable input scope — exclude AK's own windows from `input_inject` targets; consent files owned by
core, never agent-writable); (g) multi-agent blast radius (per-thread grants; kill-switch is global).

### INV-5 — Naming + wiring conventions (so slices don't collide)
- **Separate, opt-in MCP server** `cowork` (distinct from `cooperation`), **off by default**, enabled
  per-workspace/thread. New entry in `writeMCPConfig` + matching `--allowedTools mcp__cowork` token.
  Rationale: security isolation + default-off matches the threat model. (Slices may argue for folding
  into `cooperation` if they show it's safe.)
- **MCP tool names:** `desktop_*` verb-first — e.g. `desktop_list_windows`, `desktop_screenshot`,
  `desktop_read_a11y_tree`, `desktop_find_element`, `desktop_click_element`, `desktop_start_screencast`,
  `desktop_inject_input`, `desktop_open_sandbox`. (See §3 for the full provisional surface — the
  Optimize phase finalizes it.)
- **Core RPC methods:** `cowork.<verb>` — e.g. `cowork.listWindows`, `cowork.snapshotA11y`,
  `cowork.requestGrant`, `cowork.respondGrant`, `cowork.listGrants`, `cowork.revokeGrant`,
  `cowork.startScreencast` (UI-serviced), `cowork.captureStill`, `cowork.injectInput`.
- **Notifications (core→UI):** `cowork.grantRequested`, `cowork.grantsChanged`, `cowork.windowEvent`,
  `cowork.sessionChanged`, `cowork.killSwitch`.
- **UI→core RPCs the UI *serves* are not possible** (UI is the client). For "core asks UI to run a
  portal," use: core emits a `cowork.portalRequest` **notification**; the UI runs the portal and
  replies with a normal `cowork.portalResult` **call** carrying the token/node-id. (Optimize phase
  validates this round-trip pattern.)

### INV-6 — Streaming discipline
No raw frames over the bus. Window/a11y event firehoses are coalesced (≥25 ms window, like
`agent.go`) and paired with a snapshot RPC. Live video stays UI-side (PipeWire→KPipeWire→preview
widget); the agent gets **stills on demand** (`desktop_screenshot` / `captureStill`) unless a future
phase adds a deliberate, throttled frame-pull.

### INV-7 — Phasing bias
Prefer a **walking-skeleton v1** that proves the riskiest seams cheaply (build-system + a single
no-consent `desktop_list_windows` via KWin D-Bus from Go; then a consent-gated `desktop_screenshot`
via portal with the UI parent_window round-trip). Defer screencast streaming, remote-desktop/EIS, and
the VD sandbox to later phases. Every slice should label its parts S/M/L and propose a v1/v2/v3 split.

---

## 3. Provisional MCP tool surface (Optimize phase finalizes)

| Tool | Layer | Capability/tier | Consent |
|---|---|---|---|
| `desktop_list_windows` | KWin D-Bus (Go) | window_list / R0 | session grant |
| `desktop_read_a11y_tree(pid?)` | AT-SPI2 (Go) | a11y_read / R1 | session/timed grant |
| `desktop_find_element(role,name)` | AT-SPI2 (Go) | a11y_read / R1 | session/timed grant |
| `desktop_read_element_text(node)` | AT-SPI2 (Go) | a11y_read / R1 | session/timed grant |
| `desktop_screenshot(target?)` | portal/KWin ScreenShot2 (UI) | screenshot / R1 | per-target / timed |
| `desktop_start_screencast(target)` | portal ScreenCast + PipeWire (UI) | screencast / R1 | timed/session, preview |
| `desktop_stop_screencast(token)` | portal (UI) | — | — |
| `desktop_click_element(node)` | AT-SPI2 Action (Go) | a11y_action / R2 | per-action |
| `desktop_inject_input(events)` | portal RemoteDesktop/EIS (UI) | input_inject / R2 | per-action, distinct UI |
| `desktop_open_sandbox()` / `desktop_use_sandbox` | KWin VD (Go) | vd_sandbox | session grant |

---

## 4. Open seams the design must resolve (carried from recon)
1. **Core↔UI portal round-trip** (INV-5): exact RPC/notification choreography for "core needs the UI
   to run a portal session and return a token." Validate it's deadlock-free and lifetime-safe.
2. **Frame delivery:** PNG-on-demand sizing vs the 16 MiB cap; scaling/region to stay under it.
3. **Grant store schema + migration + atomic write + restart semantics** (live grants auto-revoke on
   restart? portal `restore_token` re-use is single-use — rotate).
4. **KWin script lifecycle:** one-shot enumerations must `stop()` the loaded script; persistent event
   scripts need packaging (`kpackagetool6`) or a long-lived loaded script with cleanup on shutdown.
5. **AT-SPI Wayland coords** are window-local; reconcile with KWin window geometry for screen-absolute.
6. **EIS vs Notify* input path** (Plasma 6.3+ libei) — pick one; EIS is lower-latency but more plumbing.
7. **Plasma version gotchas** (skill doc): 6.5.x portal dialogs ignore virtual input (need physical
   click or pre-auth); `flatpak permission-set kde-authorized …` pre-authorization path for power users.

---

## 5. File map (where things land)
- New Go: `core/internal/kde/{kwin.go,atspi.go,portalcoord.go}`, `core/internal/cowork/{state.go,
  consent.go,audit.go,grants.go}`. Tools in `core/cmd/akcore/mcp.go`; RPCs/notify in `main.go`
  (`registerHandlers`, `handlerDeps`, `gracefulShutdown`). Dep: `github.com/godbus/dbus/v5`.
- New C++: `ui/src/cowork/{CoworkPanel,PortalSession,ConsentDialog,ActiveGrantsView,…}.{h,cpp}`;
  register panel in `MainWindow.cpp`; add `Qt6::DBus`/`WaylandClient`/`KPipeWire` to `ui/CMakeLists.txt`.
- `.desktop`: add `X-KDE-DBUS-Restricted-Interfaces`.
- Plans: `docs/plans/08-kde-cowork/*.md` + sub-README + top-README row.

---

## 6. Design deliverables (this directory)
- `00-context-pack.md` — this file.
- `01-consent-spine.md` — consent/grant/audit/revocation/kill-switch + RPC surface (foundation).
- `02-capture.md` — screenshot + screencast + PipeWire + parent_window round-trip (UI-side).
- `03-introspection.md` — KWin window list + events + AT-SPI2 tree (Go-side D-Bus).
- `04-control.md` — RemoteDesktop input injection + AT-SPI actions (highest risk).
- `05-sandbox.md` — KWin virtual-desktop sandbox confinement.
- `06-ui-panel.md` — Cowork panel, consent dialogs, active-grants, KDE-native.
- (Optimize/Refine produce) `07-wiring-and-roadmap.md` + the final `README.md` sub-index.
