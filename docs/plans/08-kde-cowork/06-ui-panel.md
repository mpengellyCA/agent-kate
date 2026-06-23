# 06 — Cowork UI: panel, consent dialogs, active grants, live preview, kill-switch

The user-facing half of Cowork: a KDE-native QtWidgets side panel that renders share status,
runs the consent dialogs, shows the live preview and the active-grants surface, and carries the
global kill-switch. This slice also owns the **C++ portal side** (INV-1): all XDG portal sessions,
the `parent_window` Wayland handle, and PipeWire stream consumption live in the UI process. The Go
core remains the consent authority; the UI is a *view + portal executor*, never the source of truth
for what is allowed.

## Current state

- **Panel registration.** `MainWindow::registerPanel(key, icon, label, widget, defaultStrip)`
  (`ui/src/MainWindow.cpp:1009-1038`) places a `QWidget` on a `SideBar` strip and auto-persists its
  position to KConfig (`View/panels/<key>/strip`). Call sites at `:198-243`; the **`Cooperation`
  stub** at `:213-218` is the closest analog. Panel keys are `QString` members on `MainWindow`
  (`MainWindow.h:132`, e.g. `m_keyCoop`). **There is no `m_keyCowork` yet** — we add one.
- **IPC.** `CoreClient::call(method, params, cb, context)` is async with a **lifetime-guarded
  callback** — pass the panel as `context` so a late reply after the panel dies is dropped
  (`CoreClient.h:39`, `.cpp:111-125`). Server→UI events arrive as the Qt signal
  `notification(QString method, QJsonObject params)` (`CoreClient.h:47`, `.cpp:177-181`); panels
  `connect` and filter by method string — **there is no subscribe RPC**, it is broadcast + filter.
  `CoreClient` also emits `connected()` / `disconnected()` (`CoreClient.h:45-46`); only one
  `connected` wiring exists today (`MainWindow.cpp:1305`).
- **Notifications are lossy under backpressure** (context pack §1): notifications are shed
  oldest-first when the writer queue fills. **The panel MUST be able to re-derive its whole state
  from a pull RPC** (`cowork.listGrants` / a `cowork.snapshot`) and treat `cowork.grantsChanged`
  only as a "refetch" nudge.
- **Consent UI house pattern.** Destructive confirmation uses
  **`KMessageBox::warningContinueCancelList` + `KMessageBox::Dangerous`**, itemized, Cancel-defaulted
  (`CleanupDialog.cpp:480-491`). The per-tool **approval banner** `m_permBar` is a styled `QFrame`
  with a rich-text `QLabel` + Approve/Deny buttons, one-at-a-time via `showNextPermission()`
  (`AgentPanel.cpp:501-516, 2442-2484`); the `AskUserQuestion` form is built dynamically
  (`:2486-2558`). **This banner is rubber-stampable** — fine for R0/R1, **forbidden for R2**.
- **Native colour.** `KColorScheme scheme(QPalette::Active, KColorScheme::View)` →
  `scheme.foreground(KColorScheme::NegativeText|NeutralText|PositiveText).color()`
  (`CleanupDialog.cpp:28-47`). **No raw RGB.** `KMessageWidget` already used for inline notices
  (`EditorArea.cpp`, `AgentPanel.cpp`).
- **Settings idiom.** `KSharedConfig::openConfig()->group("Agent").writeEntry(key, value)` inline at
  the call site (`AgentPanel.cpp:621-695`). Precedent that governs us: the UI **refuses to persist
  `bypassPermissions`** and remaps it to a safe default (`AgentPanel.cpp:620-640`) — *never make the
  unsafe choice sticky.*
- **Build.** UI links only `Qt6::{Core,Gui,Widgets,Network}` + KF6 (`CMakeLists.txt:10-106`).
  `Qt6::DBus`, `Qt6::WaylandClient` (+`Qt6::GuiPrivate`), `KPipeWire`, and `Qt6::QuickWidgets` are
  **present on the system but unlinked** (verified: `/usr/lib/libKPipeWire.so.6.7.0`,
  `/usr/lib/cmake/KPipeWire/KPipeWireConfig.cmake`, `Qt6QuickWidgets 6.11.1`,
  `Qt6WaylandClientConfig.cmake`, `Qt6GuiConfig.cmake`). The `.desktop`
  (`ui/org.kde.agentkate.desktop`) has **no `X-KDE-DBUS-Restricted-Interfaces`** line.
- **KPipeWire offers a non-QtQuick C++ stream.** `PipeWireSourceStream`
  (`/usr/include/KPipeWire/pipewiresourcestream.h`) exposes
  `createStream(quint64 objectSerial, int fd)`, `setAllowDmaBuf(bool)`, and the signal
  `frameReceived(const PipeWireFrame&)` where `frame.dataFrame->toImage()` yields a `QImage`. The
  QtQuick `PipeWireSourceItem` (`org.kde.pipewire` QML module) is *not* the only consumer — **we can
  render frames into a plain `QWidget` with no QtQuick at all.** This is decisive for the preview
  spike below.
- **`windowHandle()`** is a `QWidget` method (`QWidget::windowHandle()→QWindow*`); not yet used in
  `ui/src` (grep clean). `MainWindow` is a `KMainWindow` so the top-level `QWindow` is reachable.

## Proposed design

### A. Panel layout — `CoworkPanel` (QWidget, right strip)

Registered like the Cooperation stub:

```cpp
registerPanel(m_keyCowork, QIcon::fromTheme(QStringLiteral("video-display")),
              i18n("Cowork"), new CoworkPanel(m_core, this), QStringLiteral("right"));
```

`CoworkPanel(CoreClient *core, QWidget *parent)` mirrors the existing panel ctor shape
(`AgentPanel`, `WorktreeDashboard`, `LogViewer` all take `CoreClient*`). A top-to-bottom
`QVBoxLayout` with a `QScrollArea` body and a **pinned bottom kill-switch bar** (always visible,
never scrolled away). Sections, each a `QGroupBox` or titled separator:

1. **(a) Status / capability toggles** — a one-line `KMessageWidget` summarising the live posture
   ("No desktop access shared" / "2 active grants · 1 live screencast"), colour-keyed via
   `KColorScheme` (Positive when nothing is shared, Neutral/Negative when control is granted). Below
   it, per-capability **master toggles** that act as *UI affordances only* (collapse the cards, set
   the default scope for the next prompt) — they are **NOT** the consent gate. A read-only badge row
   shows which tiers are currently active.
2. **(b) "Share…" controls** — four `QPushButton`s: *Share a window…*, *Share a screen…*, *Share
   whole desktop…*, *Open sandbox desktop…*. These start a portal session **proactively** (user
   opens the share rather than waiting for the agent to ask). Each calls into `CoworkPortal`
   (§C) → on success calls `cowork.requestGrant`/registers the resulting token with core. A small
   `QComboBox` selects default scope (`once` / `session` / `timed` / `until_revoked`) seeded from
   KConfig (§E).
3. **(c) Live preview area** — a `CoworkPreview` widget (§D). Hidden until a screencast/screenshot
   stream exists; shows the consumed PipeWire frames, a "● LIVE" badge, the target label, and a
   *Stop preview* button. Last preview size persists to KConfig.
4. **(d) Active Grants list** — `ActiveGrantsView` (`QTreeView` + a small model) listing one row per
   live grant: **thread · capability · target · scope · expiry (live countdown)** with a per-row
   **Revoke** button (delegate or trailing action). Populated from `cowork.listGrants`; refreshed on
   `cowork.grantsChanged`. Expiry is rendered with a 1 Hz `QTimer` that only ticks while the panel is
   visible (respect the project's anti-polling norm — see Risks).
5. **(e) Audit log view** — a read-only `QTreeView`/`QPlainTextEdit` of the append-only audit feed
   (grant/deny/revoke/executed-action with target + timestamp). Pulled from a `cowork.listAudit`
   RPC (paginated, newest-first); appended live from `cowork.grantsChanged`/`cowork.windowEvent`
   where they carry audit entries. Read-only — the UI never edits the audit.
6. **(f) Kill-switch** — pinned bottom bar, a single prominent
   `QPushButton(i18n("Stop ALL desktop access"))` with `QIcon::fromTheme("network-disconnect")` and
   a `KColorScheme::NegativeText` accent. One click → confirm via
   `KMessageBox::warningContinueCancelList` (itemising every active grant that will be cut) →
   `cowork.revokeGrant{all:true}` (or a dedicated `cowork.killSwitch`) **and** synchronously tears
   down every UI-side portal session (§G). Always works even if core is mid-flight — local portal
   teardown does not wait on an RPC reply.

`CoworkPanel::onNotification(method, params)` is wired:
```cpp
connect(m_core, &CoreClient::notification, this, &CoworkPanel::onNotification,
        Qt::QueuedConnection);
```
and dispatches by method string: `cowork.grantsChanged` → refetch grants+status;
`cowork.grantRequested` → open the consent flow (§B); `cowork.sessionChanged` → update preview/share
state; `cowork.windowEvent` → (optional) live target labels; `cowork.killSwitch` → tear down all UI
portals + clear preview. **Filter every handler by `threadId` where present**, exactly as
`AgentPanel::onPermissionRequested` does (`AgentPanel.cpp:2425-2428`).

### B. Consent dialogs — the grant-request flow

Choreography: core emits `cowork.grantRequested{requestId, threadId, capability, tier, target,
action?, scope, expiresAt?}` → UI shows the tier-appropriate consent surface → UI calls
`cowork.respondGrant{requestId, allow, scope?, expiresAt?}`. Core remains the authority and applies
fail-closed timeout (reuse the broker's 8-min self-deny rendezvous, context pack §1).

**Display contract (INV-4 / Agent 4):** every prompt shows the **concrete** action, never a vague
tool name. The params carry it; the UI renders:
- window/screen target → the **window caption + app (resourceClass)**, optionally a thumbnail;
- `input_inject` → the **literal keysyms / button + absolute coords**;
- `a11y_action` → the **element role + name + the action verb** ("click", "toggle").

Three surfaces, by tier:

- **R0 (`window_list`) — standard banner.** Reuse a `CoworkPanel`-local inline banner modelled on
  `m_permBar` (a `QFrame` + rich-text `QLabel` + Allow/Deny), one-at-a-time via a local
  `showNextRequest()` queue mirroring `AgentPanel::showNextPermission`. Cheap, session-scoped.
- **R1 (`a11y_read`, `screenshot`, `screencast`) — Dangerous itemized dialog.**
  `KMessageBox::warningContinueCancelList(parent, i18n("Allow Agent Kate to <b>read</b> …?"),
  itemList, i18n("Share screen content"), KGuiItem(i18n("Allow")), KStandardGuiItem::cancel(),
  QString(), KMessageBox::Options(KMessageBox::Notify | KMessageBox::Dangerous))` — `itemList`
  enumerates exactly what gets captured (target window, scope, expiry, "captured pixels are treated
  as untrusted input"). Cancel-defaulted. Same call shape as `CleanupDialog.cpp:480-491`.
- **R2 (`a11y_action`, `input_inject`, `remote_desktop`) — DISTINCT high-risk dialog (NEW class).**
  See §B.1. **Never** the `m_permBar`-style banner — that is rubber-stampable and INV-3/INV-4 forbid
  it for control.

#### B.1 — `ControlConsentDialog` (the R2 widget) — design decision

A dedicated **modal `QDialog`** subclass, visually and interactively distinct from every R0/R1
surface, satisfying Agent 4's R2-widget requirement:

- **Distinct visual treatment.** A `KMessageWidget(KMessageWidget::Error)` header banner ("Agent
  Kate is asking to **control** your desktop"), a negative-keyed border via `KColorScheme`, and a
  large warning icon (`dialog-warning`). No resemblance to the calm Approve/Deny banner.
- **The concrete action, rendered literally.** A monospace read-only block showing the exact
  payload: for `input_inject`, the ordered keysym/button + absolute `(x,y)` list and the **target
  window caption** the cursor will land in; for `a11y_action`, `role · name · verb`. Plus scope +
  expiry.
- **Typed-phrase confirmation (anti-rubber-stamp).** The *Allow* button is disabled until the user
  types a short challenge phrase (`i18nc(...,"allow control")`) into a `QLineEdit` — a deliberate
  speed-bump that defeats reflexive clicking and consent fatigue. (Alternative considered: a
  press-and-hold button. **Recommend the typed phrase for v1** — accessible, screen-reader-friendly,
  no custom timer widget, and it forces the user to *read* the action.)
- **Per-action by default, no "remember".** No checkbox to persist. The dialog cannot raise scope
  beyond what core offered, and **never** offers `until_revoked` for R2.
- **AK-self exclusion.** If the target window resolves to an Agent Kate window, the dialog refuses
  outright (INV-4(f): exclude AK's own windows from `input_inject`). Core enforces this too; the UI
  fails visibly.
- Returns `QDialog::Accepted/Rejected`; the panel maps that to
  `cowork.respondGrant{allow, scope:"once"}`.

This is the **only** R2 path; `input_inject`/`a11y_action` requests are routed here regardless of
how the agent framed them.

### C. `CoworkPortal` (a.k.a. `PortalSession`) — the C++ portal class I own (for Agents 2 & 4)

A `QObject` owning **all** XDG portal D-Bus calls and the `parent_window` handle. This is the
**class boundary** Agent 2 specified core would drive via the `cowork.portalRequest` notification.

- **`parent_window` plumbing.** `QString exportedHandle()`:
  - Wayland: take `MainWindow`'s top-level `QWindow* w = window()->windowHandle()`; use the
    `QtWaylandClient` private API (xdg-foreign `export_toplevel`) to obtain a handle string and
    return `"wayland:" + handle`. Requires `Qt6::WaylandClient` + `Qt6::GuiPrivate` and a
    `#include <qpa/...>` private header (guarded by `#ifdef`).
  - X11 fallback: `"x11:" + hex(winId())`.
  - Headless/unknown: empty string (portal still works, just unparented — acceptable v1).
  The handle is recomputed per portal call (xdg-foreign exports are per-session).
- **Methods (serve the core round-trip, INV-1/INV-5):**
  - `void runScreenshot(target, opts, cb)` — `org.freedesktop.portal.Screenshot` →
    PNG/`restore_token`, returned to core via `cowork.portalResult`.
  - `void runScreenCast(target, persistMode, restoreToken, cb)` — full
    `CreateSession→SelectSources→Start→OpenPipeWireRemote` sequence; keeps the **PipeWire FD UI-side**
    (never crosses the bus), feeds it to `CoworkPreview` (§D), and returns only `{node_id,
    restore_token}` to core.
  - `void runRemoteDesktop(...)` / `injectInput(events)` — services **Agent 4's** injection requests
    entirely UI-side: `CreateSession→SelectDevices→[SelectSources]→Start→ConnectToEIS|Notify*`. Core
    sends the *intent*; the UI runs the portal so the input is bound to the UI's authorized session.
    The R2 `ControlConsentDialog` gates this **before** the portal fires.
  - `void closeSession(handle)` / `void closeAll()` — teardown (§G).
- **Round-trip wiring.** `CoworkPanel` subscribes to `cowork.portalRequest`; on receipt it invokes
  the matching `CoworkPortal` method and replies with `cowork.portalResult{requestId, ok, token?,
  nodeId?, error?}` via `m_core->call(..., this)`. The 16 MiB frame cap bounds any inline PNG
  (Agent 2 owns sizing/scaling); the UI prefers returning a `node_id`/token and serving **stills on
  demand** (`captureStill`) per INV-6.
- **Why the UI owns this:** FDs cannot cross our JSON-RPC bus (context pack §1; REFERENCE-skill NB),
  and portals require a UI `parent_window`. Frames/FDs **never** reach Go. Coordinate with Agent 2:
  *they* define the `cowork.portalRequest` params + `cowork.portalResult` schema; *I* implement the
  C++ executor and guarantee FD/frame containment.

### D. Live preview embedding — **Spike**, with a recommendation

`PipeWireSourceItem` is QtQuick; the app has **no QtQuick** today. Three options:

1. **Manual frame rendering in a plain QWidget (no QtQuick).** Use `PipeWireSourceStream` directly:
   `setAllowDmaBuf(false)` (force SHM so we get CPU frames), `createStream(nodeId, fd)`, connect
   `frameReceived` → `frame.dataFrame->toImage()` → paint the `QImage` (scaled, throttled to ~15 fps)
   in a custom `CoworkPreview : QWidget::paintEvent`. **No QtQuick, no QML, links only `KPipeWire`.**
2. **`QQuickWidget` bridge** hosting the `org.kde.pipewire` `PipeWireSourceItem`. Pulls in
   `Qt6::Quick`+`Qt6::Qml`+`Qt6::QuickWidgets`, a QML file, and a QtQuick render thread into a pure
   QtWidgets app — heavier, GPU-path "free" (dmabuf), but a large new surface for a v1.
3. **External-viewer fallback** — open the stream in `Spectacle`/an external recorder; no in-app
   preview. Lowest effort, worst UX.

**Recommend option 1 for v1 — flag as Spike `SPIKE-PREVIEW`.** It keeps the strict no-QtQuick rule,
links one new lib, and `PipeWireSourceStream`'s public `frameReceived`/`toImage` API is exactly the
contract we need. Risks to resolve in the spike: (a) SHM vs dmabuf — `setAllowDmaBuf(false)` should
force a CPU `QImage`, but confirm the KDE portal honours it; if it only emits dmabuf, fall back to
`DmaBufHandler`→`QImage` (needs EGL, more plumbing) or option 2. (b) frame throttle + scaling so the
preview never becomes a CPU hog. (c) lifecycle: stop the stream on hide/close. If the spike shows SHM
is unreliable, **option 2 (`QQuickWidget`) is the documented fallback** (QuickWidgets 6.11.1 is on
the box). Do not overbuild: v1 preview is best-effort and can ship behind option 3 if the spike slips.

### E. KDE-native + settings (affordances only)

- **Strings:** `i18n`/`i18nc` everywhere; disambiguate the typed-phrase challenge with `i18nc`.
- **Colour:** `KColorScheme` only; status badges reuse the `CleanupDialog::stateColor` idiom.
- **Inline notices:** `KMessageWidget` for the status line + the R2 header.
- **Icons:** theme icons (`video-display`, `network-disconnect`, `dialog-warning`, `view-history`).
- **KConfig `Cowork` group — UI affordances ONLY (never the consent authority):**
  `defaultScopeR0/R1` (string), `lastPreviewSize` (QSize), `previewFps` (int), `auditPageSize`.
  **Following `AgentPanel.cpp:620-640`, refuse to persist any value that would weaken consent** — no
  "always allow", no remembered R2, no default `until_revoked` for control. The grant store
  (core-owned, `cowork-consents.json`) is the only authority; the UI re-reads it on every
  `grantsChanged`.

### F. Build + `.desktop`

`ui/CMakeLists.txt` — add to **both** `find_package` and `target_link_libraries`:
```cmake
find_package(Qt6 6.6 REQUIRED COMPONENTS Core Gui Widgets Network DBus WaylandClient)
find_package(KPipeWire REQUIRED)           # /usr/lib/cmake/KPipeWire, no pkgconfig
# (only if SPIKE-PREVIEW lands on option 2: ... Quick Qml QuickWidgets)
target_link_libraries(agentkate PRIVATE
    ... Qt6::DBus Qt6::WaylandClient Qt6::GuiPrivate KPipeWire::KPipeWire)
```
Add the new sources: `src/cowork/CoworkPanel.cpp`, `CoworkPortal.cpp`, `ControlConsentDialog.cpp`,
`ActiveGrantsView.cpp`, `CoworkPreview.cpp`, `CoworkAuditView.cpp` (AUTOMOC handles `Q_OBJECT`).

`ui/org.kde.agentkate.desktop` — add:
```
X-KDE-DBUS-Restricted-Interfaces=org.kde.KWin.ScreenShot2
```
**Caveat (context pack §1):** only effective for the *installed* app — dogfooding from the build dir
won't have it; ScreenShot2 returns `NoAuthorized`. Test the fast-path against `make install`'d
`~/.local/share/applications/`; portal Screenshot (dialog path) works either way and is the v1 path.

### G. Lifecycle / teardown (no portal outlives the window)

`CoworkPortal` tracks every open session handle. Teardown is triggered by **all** of:
- **Panel close / destroy** — `CoworkPanel::~CoworkPanel` → `m_portal->closeAll()`.
- **`CoreClient::disconnected`** — connect (the second-ever use of this signal,
  `MainWindow.cpp:1305` is the first) → `closeAll()` + clear preview + grey out the panel; the agent
  is gone, no session should linger.
- **Kill-switch** — synchronous local `closeAll()` *before* awaiting the core RPC.
- **`cowork.killSwitch` / `cowork.sessionChanged(ended)`** notifications → close the matching
  session(s).
Every callback uses the panel as `context` so a late reply can't touch a torn-down portal
(`CoreClient`'s lifetime guard). Stop the `PipeWireSourceStream` and the 1 Hz expiry timer on hide.

## Implementation steps

1. **Skeleton panel (no portals).** Add `m_keyCowork` to `MainWindow.h`; register `CoworkPanel`
   (`MainWindow.cpp` near `:213`). Build the layout sections (a)–(f) with the kill-switch wired to a
   confirm dialog + `cowork.revokeGrant{all:true}`. Wire `onNotification` (queued) and
   `cowork.listGrants`/`listAudit` pull RPCs. Renders empty/idle. **[S]**
2. **Active grants + audit.** `ActiveGrantsView` model + Revoke; `CoworkAuditView`. Re-derive from
   pull RPC on `grantsChanged`; live expiry countdown. **[M]**
3. **Consent surfaces.** R0 banner + R1 `KMessageBox` Dangerous dialog wired to
   `cowork.grantRequested`→`cowork.respondGrant`, with the concrete-action display contract. **[M]**
4. **`ControlConsentDialog` (R2).** New distinct dialog with typed-phrase confirm + literal action
   render + AK-self exclusion. Unit-cover the gate (allow only on exact phrase). **[M]**
5. **`CoworkPortal` + parent_window.** Wayland xdg-foreign export (`GuiPrivate`/`WaylandClient`) +
   X11 fallback; `cowork.portalRequest`→`cowork.portalResult` round-trip for Screenshot first
   (the INV-7 walking-skeleton seam), then ScreenCast/RemoteDesktop. **[L]**
6. **`SPIKE-PREVIEW`.** `PipeWireSourceStream`→`QImage`→`CoworkPreview` (option 1); confirm SHM
   path; fall back to `QQuickWidget` (option 2) if needed. **[M, spike-gated]**
7. **Lifecycle + build.** `closeAll()` on close/disconnect/kill-switch; CMake + `.desktop` edits;
   KConfig `Cowork` affordances with the "never persist the unsafe choice" guard. **[S]**

## Risks / considerations

- **Notification loss (lossy bus).** Treat `cowork.grantsChanged` as a *refetch trigger* only; the
  panel must always re-derive from `cowork.listGrants`. Never accumulate state purely from the event
  stream. *(Cross-ref: 01-consent-spine snapshot RPC; INV-6.)*
- **Rubber-stamping / consent fatigue.** R2 deliberately uses a typed-phrase speed-bump and is
  per-action; R0/R1 stay scoped so the *safe* path isn't the annoying one (INV-4(b)). Do not let any
  KConfig affordance turn R2 into one-click.
- **parent_window fragility.** xdg-foreign needs Qt private headers — guard with `#ifdef` and
  degrade to an unparented portal (still consent-gated) rather than failing the share. Re-export per
  call (handles are per-session).
- **FD/frame containment (INV-1).** The PipeWire FD and all frames stay UI-side; only
  `node_id`/`restore_token`/PNG-stills cross to Go. Restore tokens are **single-use** — rotate on
  every reuse (REFERENCE-skill). *(Cross-ref: 02-capture.)*
- **ScreenShot2 authorization.** Fast no-dialog path needs the installed `.desktop`; ship the portal
  dialog path as the v1 default so build-dir dogfooding works. *(Cross-ref: 02-capture.)*
- **Plasma 6.5.x ignores virtual input on portal dialogs** — the first R2/screencast consent may
  need a physical click; document in-app, don't try to automate the consent dialog (that would defeat
  the threat model). *(REFERENCE-skill gotchas.)*
- **Timer/polling discipline.** The 1 Hz expiry timer and any preview repaint run **only while the
  panel is visible** (the codebase recently killed boot-time inotify/timer storms — commit `343a30c`,
  `dc02c92`). Stop on hide.
- **AK self-approval loop (INV-4(f)).** The consent UI and `cowork-consents.json` are outside the
  agent's injectable scope; `ControlConsentDialog` refuses AK-window targets; core double-enforces.
- **Multi-agent blast radius.** Grants are per-thread (filter by `threadId`); the kill-switch is
  global and cuts every thread + every UI portal at once (INV-4(g)).

## Acceptance

- A **Cowork** panel appears on the right strip with sections (a)–(f); position persists across
  restarts.
- `cowork.grantRequested` for R0/R1 raises the banner / Dangerous dialog showing the **concrete**
  target/action; responding calls `cowork.respondGrant` with the chosen scope.
- An `input_inject`/`a11y_action` request raises `ControlConsentDialog` (distinct visual + typed
  phrase), never the `m_permBar`-style banner; *Allow* is disabled until the phrase matches; an
  AK-window target is refused.
- Active Grants lists every live grant (thread·capability·target·scope·expiry) with a working
  one-click Revoke; the list re-derives from `cowork.listGrants` after a dropped notification.
- The kill-switch confirms, cuts all grants, and tears down every UI portal session synchronously;
  no portal session survives panel close or `CoreClient::disconnected`.
- Live preview renders PipeWire frames in a QtWidgets surface (or the spike concludes option 2/3 with
  a documented reason); no QtQuick is introduced unless the spike forces option 2.
- `ui/CMakeLists.txt` links `Qt6::DBus`/`WaylandClient`/`GuiPrivate`/`KPipeWire`; the `.desktop`
  carries `X-KDE-DBUS-Restricted-Interfaces`; no raw RGB, all strings `i18n`'d.
