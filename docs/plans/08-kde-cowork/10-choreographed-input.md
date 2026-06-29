# 10 — Choreographed input: timed key/button holds, sequences & a unified input timeline

The Cowork agent can today **tap keys** and **click/scroll** at absolute points with a configurable
movement profile (plan 09). But every keystroke is an *atomic tap* — `buildInjectOps`
(`core/cmd/akcore/cowork_inject.go:90`) expands each `key`/`button` event into a back-to-back
press+release with **no hold time, no delay between events, and no way to leave a key held while doing
something else**. That makes the things real programs need impossible: holding `W` to walk while aiming
the mouse, holding `Shift` while dragging a selection, a precise `Ctrl`-down → click → `Ctrl`-up to
open-in-new-tab, charged attacks, modifier chords, double-taps with a controlled gap, or any
frame-bucketed combo. This slice adds **time** to Cowork input: per-event **hold durations**, **inter-event
delays**, **separate key/button down & up primitives**, and a single **choreography timeline** that
interleaves keyboard *and* pointer events on one clock so the agent can script motion with better-than-human
precision and repeatability.

This is a pure capability *deepening* of plan 09 — same RemoteDesktop session, same consent spine
(`01-consent-spine.md`), same R2 gating and anti-escalation discipline (`04-control.md`,
`09-true-cursor-control.md` §6–§7). It adds **no new portal API**: it reuses machinery already shipped.

## The key insight — the plumbing already does this; only the core collapses it

The UI playback layer was built for plan 09's timed pointer motion and already supports everything this
slice needs. The gap is entirely in the Go core's op expansion + the MCP schema.

- **Separate down/up is already the wire format.** Ops carry a `state` field: `runOneOp` dispatches
  `{"t":"key","keysym":K,"state":1|0}` and `{"t":"btn","button":B,"state":1|0}` straight to
  `notifyKeysym`/`notifyButton` (`ui/src/cowork/CoworkPortal.cpp:1096–1101`). A lone `state:1` is a real,
  held press. `buildInjectOps` simply never emits a lone down — it always pairs `state:1` with an
  immediate `state:0` (`cowork_inject.go:100–103`).
- **Held inputs are already tracked and auto-released.** `notifyKeysym`/`notifyButton` insert/remove from
  `m_heldKeys`/`m_heldButtons` on state (`CoworkPortal.cpp:918–920, 936–938`); `releaseHeld()` synthesises
  an up-event for everything still down on any teardown — kill-switch, idle timeout, session rebuild
  (`CoworkPortal.cpp:1200–1216`). So a script that leaves a key held and then dies cannot wedge KWin in a
  stuck grab. This safety net is the precondition that makes holds safe to expose.
- **Per-op timing already plays back on a QTimer.** `runInjectOps` has a synchronous fast path (no
  `delayMs`) and a **timed path**: any op carrying `delayMs>0` makes the whole batch play one-op-per-tick,
  the pause applied *before* each op (`CoworkPortal.cpp:1124–1181`). The pointer profile already produces
  these timed batches (`cowork_pointer.go:expandMove`, `delayMs` per step). A keyboard timeline is the
  same shape — the core just has to emit it.
- **`stopPlayback()` already aborts a timed batch and replies** (`CoworkPortal.cpp:1183–1198`), and
  `flushInjectQueue` preserves ordering across queued batches (`CoworkPortal.cpp:1218+`). A long
  choreography is therefore already abortable mid-flight by the kill-switch.

So: **no UI/portal/D-Bus changes for the core mechanism.** The work is (1) a richer event vocabulary +
timeline compiler in Go, (2) the MCP surface, (3) the safety re-checks that a *time-extended* script
introduces (held-input bounds, execute-time self-target re-verification), and (4) UI affordances
(a "script running / stop" affordance is mostly already there via the kill-switch).

## Scope

A single agent call submits an ordered **timeline** of events on one millisecond clock. Event kinds:

- **`key` (tap)** — down+up, with optional `holdMs` (how long held before release).
- **`key_down` / `key_up`** — explicit half-events, so a key can stay held across *other* events
  (the thing that makes "hold W while aiming" expressible).
- **`button` / `button_down` / `button_up`** — same three forms for pointer buttons
  (`left/right/middle/back/forward`, codes already in `buttonCodeFor`, `cowork_inject.go:42`).
- **`move` (x,y[,profile])** — absolute pointer motion, reusing plan 09's profile expansion.
- **`scroll` (dx,dy)** / **`click` (x,y,button,count)** — convenience composites from plan 09.
- **`wait` (ms)** — an explicit pause (sugar; equivalent to `afterMs` on the next event).

Per-event timing knobs (the "maximum per-frame configurability" ask):

- **`afterMs`** — gap to wait *after the previous event fired* before this one fires (relative model).
- **`atMs`** — absolute offset on the timeline's clock (absolute model). The compiler sorts by `atMs`
  and derives the inter-op deltas. `atMs` and `afterMs` are mutually exclusive per event.
- **`holdMs`** — for `key`/`button` taps: dwell between down and up.
- **`repeat` / `repeatEveryMs`** — optional sugar: fire this event N times at a fixed cadence (auto-repeat
  for held-walk / machine-gun-tap patterns) without authoring N entries.
- Optional **`fps` + `frame`** addressing at the script level: declare `fps` once and place events by
  integer `frame`; the compiler converts `frame → atMs = round(frame * 1000 / fps)`. This is the
  frame-choreography ergonomic — author a combo as "frame 0 down, frame 6 up" and it compiles to ms.

True simultaneity (two events at the *exact same instant*) is **not** achievable — each event is one
serial D-Bus `Notify*` call. Events sharing an `atMs`/`frame` fire back-to-back with ~0 gap (as close as
the bus dispatches). **Overlap, however, is fully expressible** via held half-events: `key_down(W)` at
t=0, `move`/`click`/other taps in between, `key_up(W)` at t=800 — W is genuinely held the whole time
because nothing released it. That overlap is what games and editors actually need, and it is the design's
center of gravity.

## Proposed design

### 1. Event model & timeline compiler (Go, pure, unit-tested)

Extend `injectEvent` (`cowork_inject.go:13`) — or add a parallel `timelineEvent` for the new tool — with:

```
type timelineEvent struct {
    Type    string  // key|key_down|key_up|button|button_down|button_up|move|click|scroll|wait
    Key     string  // for key*
    Button  string  // for button*/click
    X, Y    int     // for move/click/scroll target
    DX, DY  int     // for scroll
    Count   int     // for click (double = 2)
    HoldMs  int     // for key/button taps: dwell between down and up
    AfterMs *int    // relative gap before this event (nil → 0)
    AtMs    *int    // absolute timeline offset (mutually exclusive with AfterMs)
    Frame   *int    // absolute frame index (compiled via script fps; exclusive with AtMs/AfterMs)
    Repeat       int // optional auto-repeat count (default 1)
    RepeatEveryMs int
    Profile *pointerProfilePatch // per-move override (plan 09)
}
```

A new `buildTimelineOps(script)` lowers the timeline to the **existing** op list the UI already plays:

1. **Resolve each event's fire-time** on one clock. Absolute (`atMs`/`frame`) and relative (`afterMs`)
   are normalised to an absolute `fireAtMs`; events are stably sorted by `fireAtMs` (ties keep authoring
   order). Reject a script whose times are non-monotonic *after* expansion only if it can't be serialised
   (it always can — we serialise by sorted fire-time).
2. **Lower each event to half-ops with `state`:**
   - `key_down`→`{t:key,keysym,state:1}`, `key_up`→`{t:key,keysym,state:0}`.
   - `key` (tap)→ down at `fireAtMs`, up at `fireAtMs+holdMs` (default holdMs small, e.g. 0–8ms). The up
     is a *separate scheduled event*, so other events between down and up interleave naturally.
   - buttons mirror keys; `move`/`click`/`scroll` reuse `expandMove`/`clickOps`/`scrollOp`
     (`cowork_pointer.go:173–261`) — a profiled `move` expands to its own sub-timeline of `delayMs` steps,
     offset by the event's `fireAtMs`.
   - `repeat`→ N copies at `repeatEveryMs` spacing.
3. **Convert absolute fire-times to the UI's per-op `delayMs` deltas.** After the full op list is ordered
   by absolute time, set each op's `delayMs = thisFireAt − prevFireAt` (the existing "pause before this
   op" semantics, `CoworkPortal.cpp:1175–1180`). The first op's lead-in delay is its `fireAtMs`.
   Profiled-move sub-steps keep their internal `delayMs`; the timeline only sets the *lead-in* to the
   first sub-step.
4. **Emit a compact human description** for consent/audit (not every op): e.g.
   `"hold W 800ms; at +200ms tap space ×2; move→(1320,540); left-click"`.

This keeps the **entire timing engine in the UI unchanged** — `buildTimelineOps` produces exactly the
`delayMs`-bearing op list `runInjectOps` already knows how to play.

### 2. Held-input safety — the new wrinkle a *timed* script introduces

Holds and long timelines change the threat surface vs. plan 09's instantaneous batches. Mandatory rules,
enforced core-side in `buildTimelineOps` / the RPC handler:

- **Every `*_down` must have a matching `*_up` within the script.** Reject (R `CodeInvalidParams`) a
  timeline that ends with a key/button still logically held. (`releaseHeld()` is the *safety net* for
  crashes/teardown, not a license to leak holds by design.) The compiler tracks a held-set as it lowers
  and verifies it's empty at the end.
- **Bounded hold duration & total script duration.** Cap any single hold (e.g. ≤ 10 s) and the whole
  timeline span (e.g. ≤ 30 s) — both clamped, with the limit surfaced in the tool description. A long hold
  is the kind of thing that, if the agent miscomputes, parks a modifier down; the cap + the mandatory
  matching-up + `releaseHeld` are three independent backstops.
- **Kill-switch aborts mid-script and flushes holds.** Already true: the kill-switch path tears the
  session down → `stopPlayback()` (replies the in-flight batch false) → `releaseHeld()` (synthesises ups).
  Verify the timed-playback batch is the one in flight and that abort latency is ≤ one event-loop tick.
  Add a test that kills a script mid-hold and asserts the key is released.
- **Idle teardown.** A script that *waits* keeps the session active; ensure the 60 s `m_idleTimer` is
  reset by playback ticks (or paused for the script's duration) so a long-but-legitimate choreography
  isn't torn down under itself, while a truly idle session still reaps.

### 3. Anti-escalation — execute-time self-target re-verification

Plan 09 §7's geometric self-target guard refuses clicks/scrolls landing inside an Agent Kate window,
re-fetching live KWin geometry at submit time (`guardPointerTargets`, `cowork_pointer.go:283`). A
*timeline* extends the window between submit and fire — a click scheduled at t=4s could land where an AK
window has since moved. Two-part defence:

1. **Guard every click/scroll target in the script at submit time** (as today) — fail closed if geometry
   is unreadable. This catches the static case.
2. **Re-verify click/scroll targets at fire time.** Because windows move during a multi-second script,
   add an execute-time point-in-AK-rect re-check *before* each button-press/scroll op fires, against
   geometry fetched at (or cached very near) fire time. The cleanest seam: have the core attach the
   intended absolute target to each press op, and the UI (which already owns live position) skip+report
   any op whose target is now inside an AK window — or, simpler and matching the existing split, keep all
   geometry logic in Go by having the core re-fetch and re-guard immediately before dispatching each timed
   press. **Decide in implementation; default to a bounded total script duration (§2) + submit-time guard
   + the bare-click position-mirror guard, and add fire-time re-check only if the duration cap proves too
   loose.** Document the residual window explicitly.
3. **The bare-click mirror guard still applies.** As moves in the timeline execute, thread the last
   commanded position through `pstate.setLast` (only after success, per `cowork_pointer.go:226–233` / plan
   09 review H1) so a later bare button event in the same script is verifiable.

### 4. MCP surface

Add one tool on the opt-in `cowork` MCP server; keep `desktop_inject_input` as the simple escape hatch.

| MCP tool | Core RPC | Purpose |
|---|---|---|
| `desktop_play_input(events[], fps?, targetWindowId?, profile?)` | `cowork.playInput` | run a timed choreography of interleaved key/button/move/scroll events |

`desktop_inject_input` also gains **optional** `holdMs` / `afterMs` on its events and accepts the
`key_down`/`key_up`/`button_down`/`button_up` types (backward-compatible: omitting them is today's atomic
behaviour), so simple holds don't require the full timeline tool. The directive in the tool description:
prefer element/`DoAction` paths for ordinary UI; use `desktop_play_input` for games, editors, and any task
needing held keys, combos, or precisely-timed sequences; coordinates are absolute desktop pixels (same
space as `desktop_list_elements`/`desktop_screenshot`); state the duration/precision limits (§6).

The MCP schema must describe the event object richly (the current `desktop_inject_input` schema uses
`items:{type:object}` with the shape only in prose, `mcp_cowork.go:188` — give the new tool a real
per-field schema so agents author valid timelines without trial-and-error).

### 5. Capability / consent model — reuse, don't add

- A keyboard-only timeline gates on **`input_inject`** (R2, existing `CapInputInject`). A timeline
  containing `move`/`click`/`scroll`/buttons additionally requires **`pointer_control`** (R2, plan 09 §6).
  A mixed script requires **both** toggles on (or an approval covering both) — compute the required
  capability set from the lowered ops and authorise accordingly.
- Same imperative Go gate as `04-control.md` §3 / plan 09: with the toggle(s) on, no per-action prompt;
  off → one consent prompt showing the **compact choreography description** (§1.4) so the user sees
  "hold W 800ms; tap space ×2; left-click at (1320,540)" before approving — not an opaque "play script".
- **Audit** one entry per script with the full compact description, threadId, grantId, timestamp — plus
  a refusal entry for any guard rejection. Don't audit every interpolated step (matches plan 09).

### 6. Honesty about precision — the realistic ceiling

The agent gets **millisecond-scheduled, repeatable** input, not hardware-deterministic frame-perfect
input. Be explicit in the tool description and here:

- Playback is driven by `QTimer` ticks and **one async fire-and-forget D-Bus `Notify*` per op**
  (`CoworkPortal.cpp:notifyKeysym` uses `asyncCall`). Scheduling resolution is ~1 ms but **not real-time**:
  expect a few ms of jitter under load, and a throughput ceiling near ~100–120 ops/s (one call each).
- This is **ample for frame-bucketed choreography at 30–60 fps** (16–33 ms/frame): holds, combos,
  double-taps, modifier-drags, charged inputs, and overlapping key+mouse all land in the right frame
  bucket reliably. It is **not** suitable for sub-frame-deterministic, microsecond-tight inputs (e.g.
  frame-perfect fighting-game links measured in single milliseconds).
- **Future swap if true frame-perfection is needed:** the libei/EIS path noted in `04-control.md` §2
  carries *timestamped* events the compositor schedules, behind the same serialisable op contract — the
  timeline compiler's output wouldn't change, only the UI-side transport. Call this out as the upgrade
  path; do not build it here.

### 7. Phasing

- **Phase A — held keys & explicit half-events.** `key_down`/`key_up`/`button_down`/`button_up` + `holdMs`
  + `afterMs` on `desktop_inject_input`; the held-set validation (matching up) and hold/total-duration
  caps; mandatory abort+release tests. Unlocks "hold W to walk", "Ctrl-down → click → Ctrl-up",
  modifier-drags. **[M]**
- **Phase B — the timeline tool.** `desktop_play_input` + `buildTimelineOps` (absolute/relative/`atMs`
  scheduling, sort→delta lowering), `move`/`click`/`scroll` events interleaved with keys, `repeat` sugar,
  compact description, dual-capability gating, submit-time self-target guard over all click points. **[L]**
- **Phase C — frame addressing & refinements.** `fps`+`frame` ergonomic; per-move `profile` overrides in
  the timeline; execute-time self-target re-verification (or the documented duration-cap decision, §3);
  UI "input script running — Stop" affordance distinct from the global kill-switch if useful. **[M]**

## Implementation steps

1. **Go core (`cowork_inject.go`)**: add the `key_down`/`key_up`/`button_down`/`button_up` event types and
   optional `holdMs`/`afterMs` to `buildInjectOps` (emit lone `state:1`/`state:0` ops; thread `delayMs`);
   add the held-set balance check. Keep existing atomic behaviour for events without the new fields.
2. **Go core (new `cowork_timeline.go`)**: `timelineEvent`, `buildTimelineOps` (pure: fire-time resolution
   for `afterMs`/`atMs`/`frame`+`fps`, stable sort, lowering to half-ops, `delayMs` deltas, `repeat`
   expansion, compact description), reusing `expandMove`/`clickOps`/`scrollOp` for pointer events.
   Hold/total-duration clamps. **Unit-test heavily** (this is the core of the slice).
3. **Go core (`cowork.go`)**: `cowork.playInput` handler — parse, `buildTimelineOps`, compute the required
   capability set (input_inject and/or pointer_control), `Authorize` with the compact description,
   submit-time `guardPointerTargets` over all click/scroll points, focus target window, `runPortal("inject",
   …)`, audit. Extend `cowork.injectInput` for the Phase-A fields. Thread the position mirror on move
   success (`pstate.setLast`).
4. **MCP (`mcp_cowork.go`)**: add `desktop_play_input` with a **fully-specified per-field event schema** +
   dispatch; extend `desktop_inject_input`'s description/schema for the half-event/`holdMs` fields;
   directive-style descriptions incl. the precision-ceiling note (§6).
5. **UI (`CoworkPortal`)**: verify — likely **no change needed** — that the timed path plays interleaved
   key+pointer ops correctly; confirm `m_idleTimer` doesn't reap mid-`wait`; confirm kill-switch abort
   releases mid-hold. Add a fire-time self-target hook only if §3 decision requires it.
6. **UI (`CoworkPanel`)**: optional "input script running — Stop" indicator; reuse existing capability
   labels (no new capability).
7. **Tests**: `buildTimelineOps` golden cases (overlapping hold while tapping; absolute vs relative vs
   frame scheduling agree; `repeat`; holdMs→down/up spacing; final delta lowering); held-set imbalance
   rejected; duration caps; kill-switch-mid-hold releases the key (gated `AK_KDE_LIVE` live test that holds
   a key in a scratch window and asserts release on abort); dual-capability gating (mixed script needs both
   toggles); self-target refusal for a click point inside an AK window.

## Risks / spikes

- **SPIKE-1 — held key survives across intervening ops on KWin?** Confirm a `key_down(W)` stays asserted
  through subsequent unrelated `Notify*` calls until `key_up(W)` (it should — held state is the
  compositor's, and `m_heldKeys` mirrors it). Prototype: hold a key, move the mouse, release; verify the
  app saw a continuous hold. This is the load-bearing assumption.
- **SPIKE-2 — timer jitter at cadence.** Measure actual inter-op timing under load for a 60 fps script;
  confirm jitter stays within a frame bucket. Informs the precision wording (§6) and whether a small
  scheduling lead-compensation is worth it.
- **Execute-time self-target window** (§3) — the residual gap between submit and a late click in a long
  script. Decide: duration cap alone vs. fire-time re-check. Must be settled and re-audited in review; fail
  closed on unreadable geometry.
- **Stuck-modifier safety** — a malformed or aborted script leaving a modifier down would be hostile to the
  user's session. Three backstops (mandatory matching-up, duration cap, `releaseHeld` on any teardown);
  test all three.
- **Throughput ceiling** — a dense timeline can exceed ~100 ops/s; clamp/queue and surface a warning rather
  than silently dropping events (no silent truncation).
- **Idle teardown vs. long waits** (§2) — get the interaction right so legitimate long scripts aren't
  reaped and genuinely idle sessions still are.

## Acceptance

- The agent can submit a timeline that **holds a key for a set duration while other events fire**
  (e.g. hold `W` 800 ms; at +200 ms tap `space` twice; move to an absolute point; left-click) and it plays
  back in order with the held key continuously asserted — proven by a gated live test and a manual run in a
  real game/editor.
- **Separate `key_down`/`key_up`** (and button equivalents) are expressible; `Ctrl`-down → click →
  `Ctrl`-up opens a link in a new tab via the held modifier.
- Timing is configurable **per event** via `holdMs`, `afterMs`, and absolute `atMs`/`frame`(+`fps`); the
  three scheduling models lower to the same op stream (unit-tested), and a `repeat` event expands to evenly
  spaced taps.
- A script that ends with a key still held is **rejected**; a script aborted mid-hold (kill-switch)
  **releases the key within one event-loop tick** (tested).
- A mixed key+pointer script requires **both** `input_inject` and `pointer_control` toggles (or an approval
  covering both); a keyboard-only script requires only `input_inject`. Any click/scroll point inside an
  Agent Kate window is **refused and audited**.
- Each script produces **one compact audit entry** describing the choreography (action sequence with
  timings, target window, threadId, grantId, timestamp); refusals are audited too.
- The tool description **states the precision ceiling** (ms-scheduled, frame-bucket reliable, not
  sub-frame-deterministic) so agents don't assume hardware-perfect timing.
