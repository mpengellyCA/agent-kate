package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"

	"agentkate/internal/kde"
)

// --- choreographed input: the timeline compiler (cowork plan 10) ----------------
//
// The positioned pointer tools (cowork_pointer.go) and the low-level injectInput path
// (cowork_inject.go) each fire ONE thing at a time. The timeline compiler is the layer
// above: it takes a *score* — an ordered list of keyboard/pointer events, each pinned in
// time (relative gap, absolute offset, or a frame index against a script FPS) — and lowers
// it into the single flat, delayMs-bearing op list the UI replays. Holds can overlap taps
// (hold W while tapping space), events can repeat on a cadence, and three scheduling
// dialects (afterMs / atMs / frame+fps) all compile to the same stream.
//
// The hard part is time. Every leaf lowering (expandMove/clickOps/scrollOp) already emits
// ops with INTERNAL delayMs (12 ms between move steps, settleMs on a click press). We
// resolve each event's absolute fire-time, lower it to a sub-op list, place that sub-list
// on the absolute timeline (absolutize), then re-derive a single global delta pass so the
// whole thing is one monotonic stream. The UI ignores op[0]'s delayMs (its timer starts at
// 0), so we normalize the earliest op to fire at t=0.
//
// SECURITY (audit F25): a sub-op list occupies WALL CLOCK — a profiled move expands to up
// to 240 ops × 12 ms ≈ 2.9 s of flight — and every sub-list is pinned at its own event's
// fire-time and then globally re-sorted. So two pointer events scheduled close together
// interleave, and the compiler's threaded cursor (which advances in EVENT order) stops
// describing where the cursor actually is in STREAM order. That is not cosmetic: a bare
// button's guard point is the threaded position, so the guard clears one point while the
// button fires at another, and the mirror the handler commits afterwards is a fiction.
// Hence the invariant enforced below: pointer events may not overlap. Keyboard events stay
// exempt — overlapping holds are the whole point of a timeline.

// timelineEvent is one authored event in the score.
type timelineEvent struct {
	// Type is the event kind:
	//   key|key_down|key_up         — keyboard tap / half-events (hold across other events)
	//   button|button_down|button_up — pointer button at the CURRENT cursor (bare)
	//   move                         — positioned absolute motion (no side effect, not guarded)
	//   move_rel                     — raw relative dx/dy delta (mouse-look; no guard point,
	//                                  but it DOES carry the position mirror — see below)
	//   click                        — positioned click (move→press→release ×Count)
	//   scroll                       — wheel, optionally positioned (X,Y) else at the cursor
	//   wait                         — pure delay (advances the cursor, emits nothing)
	Type   string `json:"type"`
	Key    string `json:"key"`
	Button string `json:"button"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	DX     int    `json:"dx"` // scroll notches (scroll) OR relative pixel delta (move_rel)
	DY     int    `json:"dy"` // scroll notches (scroll) OR relative pixel delta (move_rel)
	Count  int    `json:"count"`
	HoldMs int    `json:"holdMs"` // tap dwell between down and up (clamped, never rejected)

	// Scheduling — at most ONE of these may be set per event (mutually exclusive):
	AfterMs *int `json:"afterMs"` // relative gap before this event (nil → 0); measured from the running cursor
	AtMs    *int `json:"atMs"`    // absolute timeline offset
	Frame   *int `json:"frame"`   // absolute frame index, compiled against the script FPS

	Repeat        int `json:"repeat"`        // produce this many copies (max(1,Repeat)); the cadence is RepeatEveryMs
	RepeatEveryMs int `json:"repeatEveryMs"` // ms between repeat copies

	Profile *pointerProfilePatch `json:"profile"` // per-move profile override (move/click/scroll)
}

// timelineScript is the whole score plus the FPS used to resolve any Frame addressing.
type timelineScript struct {
	Events []timelineEvent
	FPS    float64
	// Bounds is the SCREEN LAYOUT the compositor clamps the pointer into, used to decide
	// whether a move_rel's accumulated position is still knowable (audit F3 — see
	// pointerState.applyRelative). Containment is per screen, not against the union box:
	// a staggered multi-monitor union contains dead space the cursor can never occupy. An
	// invalid/zero layout means "unknown", and every relative event then INVALIDATES the
	// mirror instead of advancing it.
	Bounds kde.DesktopLayout
}

// timelinePlan is the compiled result: the flat op list for the UI plus everything the
// handler needs to authorize, guard, audit, and update its pointer mirror.
type timelinePlan struct {
	Ops        []map[string]any // the final delayMs-bearing op list for the UI
	Desc       string           // compact human description for consent/audit
	GuardPts   []point          // every click/scroll target point, to self-target-guard at submit
	HasKey     bool             // any keyboard event present → needs input_inject capability
	HasPointer bool             // any move/click/scroll/button present → needs pointer_control
	FinalPos   point            // last commanded pointer position (for the handler to setLast)
	HaveFinal  bool
	// RelLost means a move_rel left the pointer somewhere this compiler could not
	// account for. The handler must then DESTROY the thread's mirror rather than leave
	// the pre-script position standing (audit F3).
	RelLost bool
	// UsedSeedPos means at least one guard point was taken from the SEEDED mirror position
	// (a bare button/scroll before the script commanded any absolute motion), rather than
	// from a coordinate the script itself names. Such a plan is only meaningful while the
	// mirror still holds that position — and the mirror is global, so another thread can
	// move the cursor while this call waits on consent. The handler re-proves the seed
	// under the fire lock before firing (audit F26).
	UsedSeedPos bool
}

// Duration caps. A tap's dwell is a pure duration → safe to CLAMP. But an explicit
// down→up hold or the total span overrunning the ceiling is a structural mistake the
// design forbids us to paper over: we return an error rather than silently truncate.
const (
	timelineMaxHoldMs = 10000 // tap dwell ceiling (10s) — clamped
	timelineMaxSpanMs = 30000 // total wall-clock span across the timeline (30s) — hard error
	timelineMaxRepeat = 10000 // copies a single event may expand to — hard error above this
)

// timedOp is one lowered op pinned to an absolute timeline offset (ms). The final delta
// pass converts atMs back into per-op delayMs.
type timedOp struct {
	op   map[string]any
	atMs int
}

// intOf reads an int out of an op map value. The ops we build carry plain int delayMs, so
// only the int case matters in practice; the rest are defensive.
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// absolutize pins a leaf sub-op list onto the absolute timeline starting at fireAt. The
// FIRST sub op fires exactly at fireAt — its own internal delayMs is dropped (it is cadence
// and the UI ignores op[0]'s delay anyway). Each later op fires at prev + its delayMs.
// delayMs is STRIPPED from every op here; the global delta pass re-derives it.
func absolutize(sub []map[string]any, fireAt int) []timedOp {
	out := make([]timedOp, 0, len(sub))
	cur := fireAt
	for i, op := range sub {
		if i > 0 {
			cur += intOf(op["delayMs"])
		}
		delete(op, "delayMs")
		out = append(out, timedOp{op: op, atMs: cur})
	}
	return out
}

// resolvedEvent is an authored event with its base fire-time computed (and any repeat
// already expanded into one copy per fire-time). seq preserves authoring order so the
// stable sort can break fire-time ties deterministically.
type resolvedEvent struct {
	ev     timelineEvent
	fireAt int
	seq    int
}

// buildTimelineOps lowers a score into the flat op stream the UI replays.
//
// start/haveStart seed the pointer mirror (the last commanded position) so the first move
// can expand a real path and bare buttons/scrolls can be verified. resolveProfile returns
// the effective, already-user-bounds-clamped PointerProfile for a move, given the event's
// optional per-move patch (the handler passes a closure over its pointerState; tests pass a
// trivial closure). rng seeds the (pure) path expansion.
//
// See the algorithm comment blocks below; the shape is: resolve fire-times (expanding
// repeat/wait) → stable-sort by time → lower in time order threading the cursor, holds, and
// the absolute timeline → global stable-sort + delta lowering → span check.
func buildTimelineOps(
	script timelineScript,
	start point, haveStart bool,
	resolveProfile func(*pointerProfilePatch) PointerProfile,
	rng *rand.Rand,
) (timelinePlan, error) {
	var plan timelinePlan

	// --- 1. Resolve fire-times in authoring order, expanding repeat and wait. ---------
	var resolved []resolvedEvent
	cursor := 0 // running authoring clock (ms)
	seq := 0
	// Predicted end of the pointer's flight, threaded in authoring order. A RELATIVE gap
	// (afterMs, or none at all) is measured from the moment the previous event's pointer
	// action finishes rather than from the moment it starts, so the natural `[click A,
	// click B]` / `[ctrl-down, click, ctrl-up]` score chains instead of colliding with the
	// no-overlap invariant in step 3 (and so a modifier is released AFTER the click it was
	// held for, which the old start-relative chaining got wrong). It is only a scheduling
	// hint — an explicit atMs/frame is never moved, and soundness is enforced in step 3
	// against the REAL lowered ops.
	predPos, predHave := start, haveStart
	predBusy := 0
	for i, ev := range script.Events {
		// Mutual exclusivity: at most one scheduling dialect per event.
		set := 0
		if ev.AfterMs != nil {
			set++
		}
		if ev.AtMs != nil {
			set++
		}
		if ev.Frame != nil {
			set++
		}
		if set > 1 {
			return plan, fmt.Errorf("event %d sets more than one of afterMs/atMs/frame (they are mutually exclusive)", i)
		}

		// A wait is a pure delay: advance the cursor by its AfterMs (its duration), emit
		// nothing, and move on. Clamp the gap non-negative.
		if strings.EqualFold(ev.Type, "wait") {
			gap := 0
			if ev.AfterMs != nil {
				gap = *ev.AfterMs
			}
			if gap < 0 {
				gap = 0
			}
			if predBusy > cursor {
				cursor = predBusy // "wait 500 after the move" means after it ARRIVES
			}
			cursor += gap
			continue
		}

		// Base fire-time from whichever dialect is in play.
		var fireAt int
		switch {
		case ev.Frame != nil:
			if script.FPS <= 0 {
				return plan, fmt.Errorf("event %d uses frame addressing but the script has no fps (frame addressing needs fps)", i)
			}
			fireAt = int(math.Round(float64(*ev.Frame) * 1000 / script.FPS))
		case ev.AtMs != nil:
			fireAt = *ev.AtMs
		default:
			gap := 0
			if ev.AfterMs != nil {
				gap = *ev.AfterMs
			}
			base := cursor
			if predBusy > base {
				base = predBusy
			}
			fireAt = base + gap
		}
		// A following relative event measures from here.
		cursor = fireAt

		// Expand repeat: n copies. After a repeat the cursor sits on the LAST copy's time
		// so a following relative event chains off it.
		n := ev.Repeat
		if n < 1 {
			n = 1
		}
		if n > timelineMaxRepeat {
			return plan, fmt.Errorf("event %d repeat=%d exceeds the %d-copy ceiling", i, ev.Repeat, timelineMaxRepeat)
		}
		// A repeat cadence is a forward gap; a negative one would march copies backward in
		// time, so clamp it to 0. The whole repeat's extent can't exceed the timeline ceiling
		// either — checking that via DIVISION (never multiplying n-1 by step) also forecloses
		// the int overflow that an enormous repeatEveryMs would otherwise sneak into
		// fireAt+c*step below, where a wrapped-negative offset could defeat the span cap.
		step := ev.RepeatEveryMs
		if step < 0 {
			step = 0
		}
		if n > 1 && step > 0 && n-1 > timelineMaxSpanMs/step {
			return plan, fmt.Errorf("event %d repeat extent (%d copies every %dms) exceeds the %dms ceiling", i, n, step, timelineMaxSpanMs)
		}
		// SECURITY (audit F25, part 2): copies CHAIN off one another instead of piling onto
		// the first copy's flight. `{click x,y repeat:3 repeatEveryMs:100}` used to place
		// every copy 100 ms apart from a common base, so copies 2 and 3 were scheduled while
		// copy 1's ~600 ms path was still in the air — and the no-overlap invariant below,
		// correctly, refused the whole script. Repeated clicking at one spot is an ordinary
		// script (the tool docs advertise it), and the invariant is not what needed relaxing:
		// the SCHEDULE was wrong. A repeat cadence is a relative gap, and relative gaps in
		// this compiler are already measured from the END of the previous pointer action
		// (predBusy above) — copies now follow the same rule, so only the FIRST copy carries
		// the travel and the rest fire, in place, at the requested cadence or as soon after
		// as the previous copy's own ops have finished. `repeatEveryMs:0` therefore means "as
		// fast as the stream allows", not "all at once on top of each other".
		//
		// This does NOT reopen F25: a copy is a byte-identical event at an identical point,
		// so nothing here can schedule motion under another event's button — and anything
		// that is NOT a copy (a differently-targeted event, an absolutely-scheduled event
		// landing mid-burst) still meets the invariant unchanged.
		copyPos, copyHave := predPos, predHave
		copyEnd, lastAt := 0, fireAt
		for c := 0; c < n; c++ {
			at := fireAt + c*step
			if c > 0 && at < copyEnd {
				at = copyEnd
			}
			resolved = append(resolved, resolvedEvent{ev: ev, fireAt: at, seq: seq})
			seq++
			// Advance the flight prediction over THIS copy (see predPos above). A pointer
			// event's predicted duration is exactly opsSpanMs of the ops it lowers to, so a
			// copy placed at copyEnd meets the invariant rather than tripping it.
			var dur int
			dur, copyPos, copyHave = predictPointerEvent(ev, copyPos, copyHave, script.Bounds, resolveProfile)
			copyEnd, lastAt = at+dur, at
		}
		cursor = lastAt
		predPos, predHave = copyPos, copyHave
		if copyEnd > predBusy {
			predBusy = copyEnd
		}
	}

	// --- 2. Stable-sort resolved events by fire-time (ties keep authoring order). ------
	sort.SliceStable(resolved, func(a, b int) bool {
		return resolved[a].fireAt < resolved[b].fireAt
	})

	// --- 3. Lower in time order, threading pointer position, holds, and the timeline. --
	lastPos := start
	haveLast := haveStart
	var timed []timedOp

	// Held-set: keysyms/button codes currently down (from *_down half-events) awaiting a
	// matching release. Keyed kind+code so a keysym and a button code can't collide. We
	// also remember each hold's down fire-time to enforce the explicit-hold cap.
	held := map[string]bool{}
	heldSince := map[string]int{}
	keyKey := func(ks uint32) string { return fmt.Sprintf("k%d", ks) }
	btnKey := func(bc uint32) string { return fmt.Sprintf("b%d", bc) }

	var descParts []string
	prof := PointerProfile{} // reused per move/click/scroll

	// SECURITY (audit F25): the no-overlap invariant. busyUntil is the absolute time the
	// last-lowered pointer event's ops finish; a pointer event scheduled before that would
	// interleave with it on the globally re-sorted stream, and then neither its guard point
	// nor the threaded final position describes where the cursor really is when it fires.
	// So it is a compile error, not a caveat: overlapping absolute motion is never
	// semantically meaningful, and refusing here is what keeps event order == flight order
	// (which is what makes every GuardPt, and FinalPos, sound). We track the end of the
	// whole sub-op list, not just of the motion inside it: a later event that fires between
	// an earlier click's last move op and its press would displace the cursor out from
	// under that press. movedAbs / UsedSeedPos record whether a guard point came from the
	// SEEDED mirror rather than from a coordinate the script names (audit F26).
	busyUntil, haveBusy := 0, false
	movedAbs := false

	for _, r := range resolved {
		ev := r.ev
		fireAt := r.fireAt
		var sub []map[string]any

		if isTimelinePointerEvent(ev.Type) && haveBusy && fireAt < busyUntil {
			return plan, fmt.Errorf("pointer event %q scheduled at %dms would fire while the previous pointer action is still running (it finishes at %dms) — there is only ONE cursor, so pointer events may not overlap, and a click fired while the cursor is still in flight lands somewhere that cannot be verified. Repair it any of these ways: (1) drop this event's atMs/frame and use afterMs instead (or omit scheduling entirely) — a RELATIVE gap is measured from the END of the previous pointer action, so [click A, click B] chains by itself; (2) keep absolute scheduling but move this event to atMs >= %d; (3) raise profile.speed so the previous move lands sooner; (4) if you meant a burst at one spot, fold it into that event's count/repeat rather than scheduling a second event on top of it",
				strings.ToLower(ev.Type), fireAt, busyUntil, busyUntil)
		}

		switch strings.ToLower(ev.Type) {
		case "key", "":
			ks, err := keysymFor(ev.Key)
			if err != nil {
				return plan, err
			}
			plan.HasKey = true
			hold := ev.HoldMs
			if hold < 0 {
				hold = 0
			}
			if hold > timelineMaxHoldMs {
				hold = timelineMaxHoldMs
			}
			down := map[string]any{"t": "key", "keysym": ks, "state": uint32(1)}
			up := map[string]any{"t": "key", "keysym": ks, "state": uint32(0)}
			if hold > 0 {
				up["delayMs"] = hold
			}
			sub = []map[string]any{down, up}
			descParts = append(descParts, tapDesc(strings.ToLower(strings.TrimSpace(ev.Key)), ev, hold))

		case "key_down":
			ks, err := keysymFor(ev.Key)
			if err != nil {
				return plan, err
			}
			plan.HasKey = true
			held[keyKey(ks)] = true
			heldSince[keyKey(ks)] = fireAt
			sub = []map[string]any{{"t": "key", "keysym": ks, "state": uint32(1)}}
			descParts = append(descParts, "hold "+strings.ToLower(strings.TrimSpace(ev.Key)))

		case "key_up":
			ks, err := keysymFor(ev.Key)
			if err != nil {
				return plan, err
			}
			plan.HasKey = true
			k := keyKey(ks)
			if !held[k] {
				return plan, fmt.Errorf("key_up for %q which is not held (every key_up needs a prior key_down)", ev.Key)
			}
			if h := fireAt - heldSince[k]; h > timelineMaxHoldMs {
				return plan, fmt.Errorf("key %q held %dms exceeds the %dms hold cap", ev.Key, h, timelineMaxHoldMs)
			}
			delete(held, k)
			delete(heldSince, k)
			sub = []map[string]any{{"t": "key", "keysym": ks, "state": uint32(0)}}
			descParts = append(descParts, "release "+strings.ToLower(strings.TrimSpace(ev.Key)))

		case "button", "button_down", "button_up":
			// Bare button at the current cursor. Fail closed exactly like the bare-click
			// guard: if we don't know where the cursor is, we can't verify the target.
			bc, err := buttonCodeFor(ev.Button)
			if err != nil {
				return plan, err
			}
			plan.HasPointer = true
			if !haveLast {
				if plan.RelLost {
					return plan, fmt.Errorf("%s at the current pointer, but an earlier relative nudge left the cursor where this script can no longer verify it (the compositor may have clamped it at a screen edge) — use a positioned click, or a `move` event to re-establish a known position", strings.ToLower(ev.Type))
				}
				return plan, fmt.Errorf("%s at the current pointer, but no position is known yet — move first or pass a positioned click", strings.ToLower(ev.Type))
			}
			plan.GuardPts = append(plan.GuardPts, lastPos)
			if !movedAbs {
				plan.UsedSeedPos = true
			}

			switch strings.ToLower(ev.Type) {
			case "button":
				hold := ev.HoldMs
				if hold < 0 {
					hold = 0
				}
				if hold > timelineMaxHoldMs {
					hold = timelineMaxHoldMs
				}
				down := btnOp(bc, 1)
				up := btnOp(bc, 0)
				if hold > 0 {
					up["delayMs"] = hold
				}
				sub = []map[string]any{down, up}
				descParts = append(descParts, tapDesc(buttonName(bc)+"-click", ev, hold))
			case "button_down":
				held[btnKey(bc)] = true
				heldSince[btnKey(bc)] = fireAt
				sub = []map[string]any{btnOp(bc, 1)}
				descParts = append(descParts, buttonName(bc)+"-press")
			case "button_up":
				k := btnKey(bc)
				if !held[k] {
					return plan, fmt.Errorf("button_up for %q which is not held (every button_up needs a prior button_down)", buttonName(bc))
				}
				if h := fireAt - heldSince[k]; h > timelineMaxHoldMs {
					return plan, fmt.Errorf("button %q held %dms exceeds the %dms hold cap", buttonName(bc), h, timelineMaxHoldMs)
				}
				delete(held, k)
				delete(heldSince, k)
				sub = []map[string]any{btnOp(bc, 0)}
				descParts = append(descParts, buttonName(bc)+"-release")
			}

		case "move":
			plan.HasPointer = true
			prof = resolveProfile(ev.Profile)
			to := point{ev.X, ev.Y}
			sub = expandMove(lastPos, haveLast, to, prof, rng)
			lastPos = to
			haveLast = true
			movedAbs = true
			descParts = append(descParts, fmt.Sprintf("move→(%d,%d)", to.X, to.Y))
			// Motion over AK windows is allowed — NOT a guard point.

		case "move_rel":
			plan.HasPointer = true
			// Raw relative delta (dx,dy) — mouse-look for a pointer-grabbing game. It has no
			// absolute landing point, so it is NOT a guard point and needs no screencast.
			//
			// SECURITY (audit F3): it DOES move the mirror. Leaving lastPos standing let a
			// script nudge the real cursor onto an Agent Kate window and then fire a bare
			// `button` event whose guard point was the position the cursor had before the
			// nudge. The delta is accumulated while the result provably stays inside the
			// desktop; anything else (unknown bounds, or a walk into the compositor's edge
			// clamp) invalidates the position, and the bare button/scroll cases below then
			// refuse the whole script.
			sub = []map[string]any{relMoveOp(float64(ev.DX), float64(ev.DY))}
			if haveLast {
				to := point{
					X: lastPos.X + int(math.Round(clampRelDelta(float64(ev.DX)))),
					Y: lastPos.Y + int(math.Round(clampRelDelta(float64(ev.DY)))),
				}
				if script.Bounds.Contains(to.X, to.Y) {
					lastPos = to
				} else {
					haveLast = false
					plan.RelLost = true
				}
			} else {
				plan.RelLost = true
			}
			descParts = append(descParts, fmt.Sprintf("nudge(%+d,%+d)", ev.DX, ev.DY))

		case "click":
			bc, err := buttonCodeFor(ev.Button)
			if err != nil {
				return plan, err
			}
			plan.HasPointer = true
			prof = resolveProfile(ev.Profile)
			to := point{ev.X, ev.Y}
			mv := expandMove(lastPos, haveLast, to, prof, rng)
			sub = clickOps(mv, bc, ev.Count, prof.SettleMs)
			lastPos = to
			haveLast = true
			movedAbs = true
			plan.GuardPts = append(plan.GuardPts, to)
			descParts = append(descParts, clickDesc(buttonName(bc), ev, to))

		case "scroll":
			plan.HasPointer = true
			positioned := ev.X != 0 || ev.Y != 0
			if positioned {
				prof = resolveProfile(ev.Profile)
				to := point{ev.X, ev.Y}
				sub = append(sub, expandMove(lastPos, haveLast, to, prof, rng)...)
				lastPos = to
				haveLast = true
				movedAbs = true
				plan.GuardPts = append(plan.GuardPts, to)
			} else {
				// Scroll at the current cursor — fail closed if we can't verify where it lands
				// (mirror the bare-click rule).
				if !haveLast {
					if plan.RelLost {
						return plan, fmt.Errorf("scroll at the current pointer, but an earlier relative nudge left the cursor where this script can no longer verify it — pass x,y, or re-establish a known position with a `move` event")
					}
					return plan, fmt.Errorf("scroll at the current pointer, but no position is known yet — pass x,y or move first")
				}
				plan.GuardPts = append(plan.GuardPts, lastPos)
				if !movedAbs {
					plan.UsedSeedPos = true
				}
			}
			// Mirror cowork.scroll: positive dy = down (axis 0), positive dx = right (axis 1).
			if ev.DY != 0 {
				sub = append(sub, scrollOp(0, ev.DY))
			}
			if ev.DX != 0 {
				sub = append(sub, scrollOp(1, ev.DX))
			}
			descParts = append(descParts, scrollDesc(ev, positioned, lastPos))

		default:
			return plan, fmt.Errorf("unknown timeline event type %q", ev.Type)
		}

		// The sub-list's own span has to be read BEFORE absolutize, which strips delayMs.
		if isTimelinePointerEvent(ev.Type) {
			busyUntil, haveBusy = fireAt+opsSpanMs(sub), true
		}
		timed = append(timed, absolutize(sub, fireAt)...)
	}

	// A pointer script that never commanded an absolute target derives BOTH its guard
	// points and its final position from the seeded mirror (a bare button at the cursor,
	// a move_rel accumulation, or a plan that only presses buttons) — so the whole plan is
	// seed-dependent, not just the guard points marked above (audit F26). With NO seed
	// there is nothing to depend on: a mouse-look burst from an unknown position stays
	// legal, it just commits no position (HaveFinal false / RelLost).
	if plan.HasPointer && !movedAbs && haveStart {
		plan.UsedSeedPos = true
	}

	// --- 4. Every hold must be released by the end. ------------------------------------
	if n := len(held); n > 0 {
		return plan, fmt.Errorf("timeline ends with %d key/button still held (every key_down needs a matching key_up)", n)
	}

	// --- 5. Global ordering + delta lowering. ------------------------------------------
	// Stable so ties keep insertion order: that preserves down-before-up and authoring
	// order at equal times (a tap's down/up share their relative positions; the move steps
	// keep their cadence order).
	sort.SliceStable(timed, func(a, b int) bool { return timed[a].atMs < timed[b].atMs })

	if len(timed) == 0 {
		return plan, fmt.Errorf("empty timeline")
	}

	base := timed[0].atMs
	span := timed[len(timed)-1].atMs - base
	if span > timelineMaxSpanMs {
		return plan, fmt.Errorf("timeline span %dms exceeds the %dms ceiling", span, timelineMaxSpanMs)
	}

	ops := make([]map[string]any, 0, len(timed))
	prev := base
	for i, t := range timed {
		if i > 0 {
			if d := t.atMs - prev; d > 0 {
				t.op["delayMs"] = d
			}
		}
		prev = t.atMs
		ops = append(ops, t.op)
	}
	plan.Ops = ops

	// --- 6. Defence in depth: derive the final position from the COMPILED stream. ------
	// SECURITY (audit F25): the handler commits FinalPos as the mirror the NEXT call's
	// bare-click guard will trust, so it may never be the compiler's intention — it must be
	// what the op stream actually does. Under the no-overlap invariant the two agree by
	// construction; walking the stream proves it instead of assuming it, and a disagreement
	// refuses the script rather than publishing a position the cursor may not be at.
	derived, haveDerived := start, haveStart
	for _, t := range timed {
		switch kind, _ := t.op["t"].(string); kind {
		case "move":
			x, okx := t.op["x"].(int)
			y, oky := t.op["y"].(int)
			if !okx || !oky {
				return plan, fmt.Errorf("internal: a compiled move op carries no readable target")
			}
			derived, haveDerived = point{x, y}, true
		case "move_rel":
			if !haveDerived {
				continue
			}
			dx, _ := t.op["dx"].(float64)
			dy, _ := t.op["dy"].(float64)
			to := point{derived.X + int(math.Round(dx)), derived.Y + int(math.Round(dy))}
			if script.Bounds.Contains(to.X, to.Y) {
				derived = to
			} else {
				haveDerived = false
			}
		}
	}
	if haveDerived != haveLast || (haveDerived && derived != lastPos) {
		return plan, fmt.Errorf("internal: the compiled stream leaves the pointer at %v (known=%v) but the timeline threaded %v (known=%v) — refusing rather than recording a position the cursor may not be at",
			derived, haveDerived, lastPos, haveLast)
	}

	// The same treatment for the GUARD points. The compiler collected them while threading
	// its own cursor in EVENT order; effectPoints re-derives them by walking the ops the UI
	// will actually play, which is also what the fire-time section guards against
	// (cursorAction.run). Under the no-overlap invariant the two agree by construction — so
	// a disagreement means the invariant has a hole, and the honest answer is to refuse the
	// script rather than guard points it does not act on (audit F25, defence in depth).
	derivedPts, ptsOK := effectPoints(plan.Ops, start, haveStart, script.Bounds)
	if !ptsOK {
		return plan, fmt.Errorf("internal: the compiled stream fires a button or wheel at a position this script cannot account for — refusing rather than releasing ops whose landing point cannot be checked")
	}
	if !samePointSet(derivedPts, plan.GuardPts) {
		return plan, fmt.Errorf("internal: the compiled stream acts at %v but the timeline guarded %v — refusing rather than checking points the script does not act on",
			derivedPts, plan.GuardPts)
	}

	// --- 7. Description, final position, return. --------------------------------------
	plan.Desc = strings.Join(descParts, "; ")
	plan.FinalPos = lastPos
	plan.HaveFinal = haveLast
	return plan, nil
}

// isTimelinePointerEvent reports whether an event type commands the pointer — the events
// the no-overlap invariant applies to (audit F25). Keyboard events are deliberately absent:
// overlapping key holds are a feature, and keystrokes follow focus, not the cursor.
func isTimelinePointerEvent(t string) bool {
	switch strings.ToLower(t) {
	case "move", "move_rel", "click", "scroll", "button", "button_down", "button_up":
		return true
	}
	return false
}

// predictPointerEvent estimates how long one event keeps the pointer busy and where it
// leaves it, threading in AUTHORING order. It mirrors the lowering below closely enough to
// schedule relative gaps from the end of a motion (see predPos in step 1) — it is not a
// guard, and it does not need to be exact: the no-overlap invariant is enforced in step 3
// against the ops the lowering really produced, so an under-prediction costs a refusal, not
// a bypass.
func predictPointerEvent(ev timelineEvent, from point, haveFrom bool, bounds kde.DesktopLayout,
	resolveProfile func(*pointerProfilePatch) PointerProfile) (int, point, bool) {
	switch strings.ToLower(ev.Type) {
	case "move":
		prof := resolveProfile(ev.Profile)
		to := point{ev.X, ev.Y}
		return movePathMs(from, haveFrom, to, prof), to, true
	case "click":
		prof := resolveProfile(ev.Profile)
		to := point{ev.X, ev.Y}
		return movePathMs(from, haveFrom, to, prof) + prof.SettleMs, to, true
	case "scroll":
		if ev.X == 0 && ev.Y == 0 {
			return 0, from, haveFrom // at the cursor: no motion
		}
		prof := resolveProfile(ev.Profile)
		to := point{ev.X, ev.Y}
		return movePathMs(from, haveFrom, to, prof), to, true
	case "move_rel":
		if !haveFrom {
			return 0, point{}, false
		}
		to := point{
			X: from.X + int(math.Round(clampRelDelta(float64(ev.DX)))),
			Y: from.Y + int(math.Round(clampRelDelta(float64(ev.DY)))),
		}
		if !bounds.Contains(to.X, to.Y) {
			return 0, point{}, false
		}
		return 0, to, true
	case "button":
		hold := ev.HoldMs
		if hold < 0 {
			hold = 0
		}
		if hold > timelineMaxHoldMs {
			hold = timelineMaxHoldMs
		}
		return hold, from, haveFrom
	}
	return 0, from, haveFrom
}

// tapDesc summarizes a tap (key or button), folding in repeat count and hold dwell.
func tapDesc(label string, ev timelineEvent, hold int) string {
	s := "tap " + label
	if ev.Repeat > 1 {
		s += fmt.Sprintf(" ×%d", ev.Repeat)
	}
	if hold > 0 {
		s = fmt.Sprintf("hold %s %dms", label, hold)
	}
	return s
}

// clickDesc summarizes a positioned click at a target, folding in click count.
func clickDesc(button string, ev timelineEvent, to point) string {
	c := ev.Count
	if c < 1 {
		c = 1
	}
	verb := button + "-click"
	if c == 2 {
		verb = "double " + verb
	} else if c > 2 {
		verb = fmt.Sprintf("%d× %s", c, verb)
	}
	return fmt.Sprintf("%s→(%d,%d)", verb, to.X, to.Y)
}

// scrollDesc summarizes a scroll, naming the position when it is positioned.
func scrollDesc(ev timelineEvent, positioned bool, at point) string {
	where := "at cursor"
	if positioned {
		where = fmt.Sprintf("at (%d,%d)", at.X, at.Y)
	}
	return fmt.Sprintf("scroll v%d/h%d %s", ev.DY, ev.DX, where)
}
