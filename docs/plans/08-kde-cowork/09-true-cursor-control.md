# 09 — True cursor control: absolute pointer, buttons & scroll (RemoteDesktop + ScreenCast)

The Cowork agent can today **click an element's default action** (AT-SPI `DoAction`, no cursor) and
**inject keystrokes + raw button events** through the RemoteDesktop portal — but the button events fire
**wherever the virtual pointer already sits**, because we never move it. This slice adds **real pointer
control**: move the cursor to an absolute screen point and act there — left/right/middle click, the
mouse **back/forward** side buttons, vertical **and horizontal** scroll — with agent-configurable
**speed** and **accuracy** (how the motion is shaped between two points). It unblocks the things AT-SPI
fundamentally cannot do: open a link in a new tab (Ctrl/middle-click), drag, hover-driven UIs, canvas /
`<video>` / map widgets, and any app with a broken or absent accessibility tree.

This is the highest-blast-radius capability in Cowork. It must reuse the consent spine
(`01-consent-spine.md`), the R2 gating discipline and anti-escalation rules of `04-control.md`, and the
hardening already shipped (held-input force-release, idle teardown, the kill-switch path).

## Scope (from the request)

Move + the following actions, all at an absolute coordinate:

- left click (single / double), press-and-hold, release (so drag is expressible)
- right click, middle click
- mouse **back** and **forward** buttons (BTN_SIDE / BTN_EXTRA)
- vertical scroll, **horizontal** scroll
- **speed** and **accuracy** settings the agent can configure (per call and as a session default)

## Current state (what is shipped, and the gap)

- **Input injection lives entirely UI-side** in `ui/src/cowork/CoworkPortal.{h,cpp}` (INV-1: only the Qt
  process has a Wayland surface; the RemoteDesktop session handle/FD is bound to the connection that
  created it and cannot cross the JSON bus). The core sends a serializable op list; the UI runs the
  portal `Notify*` calls and replies. Keep this split — Go never touches the portal session.
- **The live RemoteDesktop session is keyboard-only by default** (commit `b2d1350`,
  `CoworkPortal::deviceTypesFor`/`startRemoteDesktop`): `SelectDevices(types)` is derived from the queued
  ops, so the common keyboard path creates **no virtual pointer** — that was the cursor-freeze fix. A
  button op widens the session to `keyboard|pointer` and rebuilds it. **This slice makes a *positioned*
  pointer the safe, intended usage**, and must preserve the freeze-fix machinery: `m_heldKeys`/
  `m_heldButtons` tracking, `releaseHeld()` on teardown, the 60 s `m_idleTimer`, and session rebuild on
  device-set growth.
- **`NotifyPointerButton(session, {}, button, state)` is already wired** (`runInjectOps` →
  `notifyButton`) with `BTN_LEFT/RIGHT/MIDDLE` (`core/cmd/akcore/cowork_inject.go:buttonCodeFor`). It
  fires at the pointer's current (undefined) position — useless without motion. This slice adds the
  motion and the remaining buttons/axes.
- **`NotifyPointerMotionAbsolute` was deliberately deferred** (see `04-control.md` §2 / the freeze-fix
  notes): on Wayland it takes a **stream id** and **requires a live ScreenCast stream bound to the same
  RemoteDesktop session**. We don't stand up screencast yet. **That is the central piece of work here.**
- **AT-SPI gives us targets for free.** `desktop_list_elements` (`core/internal/kde/atspi.go`) already
  returns each element's **absolute screen bounds** (`GetExtents(coordType=0)`), and `desktop_read_text`
  reads prose. So "click the element with id X" can be implemented as move-to-center + click *without*
  the agent guessing pixels — and modifier-click (Ctrl+click) then yields open-in-new-tab.
- **`kde.PortalResult`** (`core/internal/kde/portalcoord.go`) already carries `NodeID`, `CastToken`,
  `RestoreToken` fields reserved for screencast — wire them here.

## Proposed design

### 1. Session shape — RemoteDesktop **with** ScreenCast on one session

Absolute motion needs a screencast stream. The portal supports requesting input **and** screencast on a
**single** session; the streams come back from `Start`:

```
RemoteDesktop.CreateSession(handle_token, session_handle_token)
RemoteDesktop.SelectDevices(session, {types: KEYBOARD|POINTER})      // 1|2 = 3
ScreenCast.SelectSources(session, {types: MONITOR, multiple: true,   // all outputs
                                   cursor_mode: EMBEDDED|METADATA,
                                   restore_token, persist_mode: 2})
RemoteDesktop.Start(session, parent_window)                          // → results.streams: a(ua{sv})
RemoteDesktop.OpenPipeWireRemote(session) → fd                       // PipeWire fd (see SPIKE-1)
```

- `Start.results.streams` is an array of `(node_id uint, props a{sv})`. Each stream's `props` carry
  `position (x,y)` and `size (w,h)` — the output's place in the global layout. **Cache the stream
  table**; it is the coordinate map (§2).
- `results.restore_token` lets a later session re-Start without re-prompting — store it (the
  `RestoreToken` field already exists) and pass it back on the next `SelectSources`; rotate on each
  `Start`.
- **Lazy + reused like today**: only stand up screencast the first time a *pointer-motion* op is
  requested. Pure keyboard / `DoAction` paths must stay screencast-free (and pointer-free) so we don't
  spin up a capture stream — or trip the cursor-freeze class of bug — for a keystroke.

> **SPIKE-1 (the make-or-break unknown): does `NotifyPointerMotionAbsolute` require an *actively
> consumed* PipeWire stream, or just a started one?** On KWin the stream node exists after `Start`, but
> the compositor may only honour absolute motion while a consumer is connected. Prototype: (a) Start with
> a monitor source, (b) `OpenPipeWireRemote`, (c) create a minimal `pw_stream` that connects to the node
> (we can drop every frame — we never send frames over the bus, INV: no FD/frames cross), (d) call
> `NotifyPointerMotionAbsolute` and confirm the cursor moves. If a connected-but-draining stream is
> enough, we never decode a frame. Resolve this **before** committing to the rest. KPipeWire
> (`KF6::PipeWire` / the `screencast` deferred item) is the natural way to hold the stream cheaply.

### 2. Coordinate model — global desktop coords ↔ stream-local

Everything the agent reasons about is in **absolute desktop pixels**: `desktop_list_elements` bounds and
`desktop_screenshot` pixels are global. `NotifyPointerMotionAbsolute(session, {}, stream, x, y)` wants
coords **within a specific stream** (one monitor). So:

- Build a **stream map** from `Start.results.streams`: `{ nodeId, originX, originY, w, h }` per output.
- `globalToStream(X, Y)` → find the stream whose rect contains `(X,Y)`; return `{ nodeId, X-originX,
  Y-originY }`. Clamp to the stream rect; reject points outside every stream.
- Multi-monitor is therefore first-class: select **all** monitors as sources (`multiple: true`). A move
  that crosses monitors just resolves to a different stream id.

The op contract from core → UI is **absolute global `(x,y)`**; the UI does the stream resolution (it owns
the stream map). This mirrors the existing `ops` round-trip and keeps the agent/core in one coordinate
system.

### 3. Pointer primitives (UI-side `Notify*`)

Add to `CoworkPortal::runInjectOps` op kinds (kept serializable, like `key`/`btn` today):

| op | Portal call | Notes |
|---|---|---|
| `move` (x,y) | `NotifyPointerMotionAbsolute(session, {}, streamId, lx, ly)` | x,y global; resolve via §2. Track as `m_ptr` (last set position). |
| `btn` (button, state) | `NotifyPointerButton(session, {}, code, state)` | already wired; extend `buttonCodeFor`. |
| `axis` (dx, dy) | `NotifyPointerAxis(session, {}, dx, dy)` | smooth scroll; dy=vertical, dx=horizontal (logical px). |
| `axis_discrete` (axis, steps) | `NotifyPointerAxisDiscrete(session, {}, axis, steps)` | axis 0=vertical, 1=horizontal; steps = wheel notches (sign=direction). |

**Button codes** (`linux/input-event-codes.h`, extend `buttonCodeFor` in `cowork_inject.go`):
`left=0x110 BTN_LEFT`, `right=0x111 BTN_RIGHT`, `middle=0x112 BTN_MIDDLE`, **`back=0x113 BTN_SIDE`**,
**`forward=0x114 BTN_EXTRA`**. (Some stacks map back/forward to `BTN_BACK 0x116`/`BTN_FORWARD 0x115`;
**SPIKE-2**: confirm which a browser honours under the KDE portal and pick the pair that drives
history-back/forward.)

Higher-level gestures compose from these op primitives, expanded **core-side** so the audit log holds the
literal sequence:

- **click** = `move(x,y)` → `btn(b,press)` → `btn(b,release)`. **double-click** = two click pairs.
- **drag** = `move(from)` → `btn(left,press)` → `move(to)` (stepped, §4) → `btn(left,release)`.
- **modifier-click / open-in-new-tab** = `key(ctrl,press)` → click → `key(ctrl,release)`, **or**
  `btn(middle,...)`. (Ctrl reuses the keyboard path already in the same session.)
- **scroll** = one or more `axis_discrete`/`axis` ops; horizontal = axis 1 / `dx`.

### 4. Movement profile — agent-configurable speed & accuracy

`NotifyPointerMotionAbsolute` teleports the cursor. To give **speed** and **accuracy**, the **core**
expands a `move` into a *path* of intermediate `move` ops with inter-op delays; the UI just plays them.
Because the core tracks the last commanded position (`m_ptr` mirrored core-side per session), it knows the
start point.

Define a **PointerProfile** the agent sets (session default) and may override per call:

```
PointerProfile {
  speed:    "instant" | <pixels per second>   // default e.g. 1600 px/s; "instant" = single jump
  accuracy: 0.0 .. 1.0                          // 1.0 = straight line, land exact (default)
                                                // <1.0 = human-like: easing + slight overshoot+correct + jitter
  settleMs: <int>                               // pause after arrival before a click (default ~30ms)
}
```

- **speed** sets the step count / delay: `steps = clamp(distance/ (speed*stepDt))`, `stepDt ≈ 8–16 ms`
  (don't exceed ~120 ops/s — one D-Bus call each). `"instant"` = a single `NotifyPointerMotionAbsolute`.
- **accuracy** shapes the path: `1.0` = linear interpolation to the exact target; `<1.0` adds
  ease-in/out, a bounded overshoot then correction, and per-step jitter whose amplitude scales with
  `(1-accuracy)`. The **final op always lands exactly on the target** regardless of accuracy (jitter
  never changes where the click happens — only the path).
- Rationale for exposing both: most automation wants `instant`/`1.0` (fast, deterministic, cheap). The
  human-like profile exists for hover-sensitive UIs and for users who want non-robotic motion. Keep the
  expansion **core-side and pure** so it's unit-testable and audit-loggable (we log the target + profile,
  not every interpolated step).

Tools to set it: `desktop_set_pointer_profile(speed?, accuracy?, settleMs?)` (session default), plus an
optional `profile` arg on the move/click tools. Persist a UI default in the Cowork panel (a "pointer
speed/accuracy" control) so the *user* sets sensible bounds; the agent's per-call values clamp to them.

### 5. MCP tools + RPCs

On the opt-in `cowork` MCP server. Coordinates are absolute desktop pixels (same space as
`desktop_list_elements`/`desktop_screenshot`).

| MCP tool | Core RPC | Purpose |
|---|---|---|
| `desktop_move_pointer(x, y, profile?)` | `cowork.movePointer` | move only |
| `desktop_click(x, y, button?, count?, profile?)` | `cowork.pointerClick` | move + click (left default; right/middle/back/forward; count for double) |
| `desktop_click_element(elementId, button?, profile?)` | `cowork.pointerClickElement` | resolve AT-SPI bounds → click center (the ergonomic path; §8) |
| `desktop_scroll(dx, dy, x?, y?)` | `cowork.scroll` | vertical+horizontal; optional move-first to (x,y) |
| `desktop_drag(fromX,fromY,toX,toY, profile?)` | `cowork.pointerDrag` | press-move-release |
| `desktop_set_pointer_profile(...)` | `cowork.setPointerProfile` | session speed/accuracy default |

Keep `desktop_inject_input` (keyboard + bare buttons) as the low-level escape hatch; the new tools are the
ergonomic surface. Tool descriptions are the directive: prefer `desktop_click_element` and `DoAction`;
use raw `desktop_click(x,y)` only when there's no element (canvas, map, broken a11y); use middle-click or
Ctrl+`desktop_click` to open links in new tabs.

### 6. Capability / consent model

- New capability **`pointer_control`** (Tier **R2** — arbitrary action as the user), in
  `core/internal/cowork/grants.go` (`Valid`/`TierOf`), `policy.go` `AllToggleable`, and a
  `CoworkPanel::capLabel` ("Move & click the pointer"). It gates all of §5's *acting* tools.
- **Move-only** (`desktop_move_pointer`) is the one arguably-R1 action (no click), but because a move is
  the setup for a click and the two are almost always issued together, gate the whole surface under the
  single R2 `pointer_control` to avoid a confusing split. (Open question for the implementer; default to
  one capability.)
- Same flow as `04-control.md` §3: imperative Go gate before any portal op; with the toggle on, no
  per-action prompt (the user pre-authorised standing pointer control); off → prompt. Kill-switch and
  audit always apply.
- **Audit** every act with the concrete target: `"left-click at (1320,540)"` / `"click element 'FIXTURES'
  (link) at (…)"` / `"scroll vertical -5 notches"`.

### 7. Anti-escalation — self-target by **geometry** (the critical new rule)

A free pointer can reach **any** pixel, including Agent Kate's own kill-switch and consent dialogs. The
window-rect clamp that protected injection in `04-control.md` §4 does not apply to free motion, so add a
**coordinate self-target guard**, enforced in Go before the op is sent and re-checked against live
geometry:

1. **Refuse any button press/click whose coordinates fall inside an Agent-Kate-owned window.** AK windows
   = KWin windows whose `pid` is our UI/core pid (or children) or `resourceClass == org.kde.agentkate`
   (reuse `Authority.IsSelfTarget` + the self-pid set from `SetSelfIdentity`, extended to a
   point-in-rect test against the live KWin geometry). This is the geometric analogue of "AK windows are
   never valid targets" and is what stops the agent from moving to its own "Allow" button and clicking it.
2. **Moves may pass over AK windows, but clicks/scrolls on them are refused** — motion alone has no
   side effect; only the click/scroll is dangerous.
3. **Consent decisions remain read from Qt widgets**, but since a real click *could* hit them, rule #1 is
   the actual defense — re-fetch AK window rects at execute time (windows move) and test the *final*
   target point, not a gate-time cached rect.
4. Kill-switch short-circuits before the gate (tears down the RemoteDesktop+ScreenCast session, releases
   held buttons) exactly as today.

This guard is **mandatory** and should have its own test (click aimed at AK's window id/rect → refused +
audited).

### 8. Integration with shipped Cowork

- **Targets from AT-SPI.** `desktop_click_element(elementId)` decodes the element, re-resolves its
  `GetExtents` (TOCTOU re-check, like `ActivateElement`), computes the center, and routes through the
  same move+click path — so the agent clicks "the FIXTURES tab" by id, not by guessing pixels. This is the
  recommended default; raw `desktop_click(x,y)` is the fallback.
- **Open-in-new-tab** (the original failure): `desktop_click_element(id, button:"middle")` or
  Ctrl+`desktop_click` on the link's center. Now expressible.
- **Reuse the freeze-fix machinery** unchanged: held-button release on teardown, idle teardown, device
  rebuild. The pointer is now *positioned* before any button — the unsafe "bare virtual pointer" case the
  fix guarded against no longer occurs in this path, but keep the guards (defence in depth).
- **`desktop_read_text`/`desktop_list_elements`** stay the way to *read*; this slice is purely *act*.

### 9. Kill-switch, teardown, cursor visibility

- Extend `teardownRemoteDesktop` to also stop/close the ScreenCast stream (drop the PipeWire consumer +
  `Session.Close`). Kill-switch and idle-timeout both route through it.
- The **real cursor follows** the virtual pointer on KWin (the compositor renders one cursor), so the
  user always sees where the agent is — good for trust. Verify the cursor doesn't get hidden when the
  screencast stream uses `cursor_mode: METADATA` vs `EMBEDDED` (**SPIKE-3**; pick the mode that keeps the
  hardware cursor visible and following).

### 10. Phasing

- **Phase A — motion + left click + vertical scroll.** Stand up RemoteDesktop+ScreenCast, resolve
  SPIKE-1, implement `move`/`btn`/`axis_discrete`, `globalToStream`, `desktop_move_pointer` /
  `desktop_click` / `desktop_scroll`, the `pointer_control` capability + geometric self-target guard,
  audit, kill-switch teardown. **[L]**
- **Phase B — full button + horizontal scroll + element + drag.** right/middle/back/forward, horizontal
  scroll, `desktop_click_element`, modifier-click, `desktop_drag`. **[M]**
- **Phase C — speed & accuracy profiles.** Core-side path expansion, `desktop_set_pointer_profile`, UI
  default control, per-call override + clamping. **[M]**

## Implementation steps

1. **UI (`CoworkPortal`)**: add ScreenCast to the session — `ScreenCast.SelectSources` before `Start`,
   parse `results.streams` into a stream map, `OpenPipeWireRemote` + a minimal KPipeWire consumer
   (pending SPIKE-1), store/rotate `restore_token`. Add `move`/`axis`/`axis_discrete` op handling +
   `globalToStream`; extend `notifyButton` coverage. Track `m_ptr`.
2. **Go core (`cowork_inject.go`)**: extend `buttonCodeFor` (back/forward); add op builders for
   move/scroll/drag/click; **path expansion** for the movement profile (pure, unit-tested); track the
   per-session last-position mirror.
3. **Go core (`cowork.go`)**: `cowork.movePointer` / `pointerClick` / `pointerClickElement` / `scroll` /
   `pointerDrag` / `setPointerProfile` handlers — imperative R2 gate, **geometric self-target guard**
   (point-in-AK-rect via live KWin geometry), audit, `runPortal` to the UI.
4. **Go core (`grants.go`/`policy.go`)**: add `pointer_control` (R2, Valid/TierOf/AllToggleable).
5. **MCP (`mcp_cowork.go`)**: the §5 tool defs + dispatch; directive-style descriptions.
6. **UI (`CoworkPanel`)**: `capLabel` for `pointer_control`; a pointer speed/accuracy default control.
7. **`portalcoord.go`**: use `NodeID`/`CastToken`/`RestoreToken`; add stream-map fields if needed.
8. **Tests**: profile path-expansion (speed→step count, accuracy→bounded jitter, exact landing);
   `globalToStream` multi-monitor mapping + clamp; geometric self-target refusal; button-code table;
   kill-switch teardown closes the screencast stream. A gated live test (`AK_KDE_LIVE`) like
   `kwin_live_test.go` that moves+clicks against a scratch window.

## Risks / spikes

- **SPIKE-1 — absolute motion needs an active stream?** (§1). The whole slice hinges on this. Resolve
  first with a throwaway prototype.
- **SPIKE-2 — back/forward button codes** (§3): which evdev code drives browser history under the KDE
  portal.
- **SPIKE-3 — cursor visibility** with screencast active (§9): EMBEDDED vs METADATA cursor mode.
- **Self-approval loop via free pointer** (§7) — the single biggest threat; the geometric self-target
  guard is mandatory and must be re-audited in review. Re-fetch AK geometry at execute time (windows
  move); fail closed if geometry is unavailable.
- **Multi-monitor / fractional scaling** — stream `position`/`size` are in the compositor's logical
  pixels; reconcile with KWin geometry units (both should be logical px on Wayland, but verify under a
  scaled output).
- **Portal re-prompt** — first `Start` shows the OS RemoteDesktop+ScreenCast consent; `restore_token`
  avoids re-prompting. On Plasma versions where the portal dialog ignores virtual input (carried gotcha,
  `04-control.md` §8) the *human* must approve it once with a real click.
- **PipeWire dependency weight** — pulls in KPipeWire/libpipewire (the deferred "screencast" item). Keep
  the consumer minimal (connect + drain); never move frames across the JSON bus (INV).
- **Latency** — one D-Bus `Notify*` per step caps smooth-motion frequency (~100/s). Fine for clicks and
  human-speed motion; if true streamed input is ever needed, the EIS/libei path (`04-control.md` §2) is
  the internal swap behind the same op contract.

## Acceptance

- The agent can move the cursor to an absolute point and **left/right/middle/back/forward click** there,
  and scroll **vertically and horizontally**, on a real window — proven by a gated live test and a manual
  run (open a Google-News article in a *new tab* via middle-click / Ctrl+click).
- `desktop_click_element(id)` clicks the named element's center via the pointer (TOCTOU-rechecked).
- **Speed & accuracy** are agent-configurable: `instant` vs a px/s speed changes step count; accuracy
  `<1.0` produces human-like motion but the click **still lands exactly** on the target (unit-tested).
- A click aimed inside any **Agent Kate window is refused** and audited (geometric self-target test).
- `pointer_control` is **off by default**, gated by the R2 imperative gate even with
  `--permission-mode bypassPermissions`, toggleable in the Cowork panel, and cut by the kill-switch
  (which also tears down the screencast stream and releases held buttons) within one event-loop tick.
- Every act produces a verbatim **audit entry** (action, button/axis, absolute target, element if any,
  threadId, timestamp).
```
