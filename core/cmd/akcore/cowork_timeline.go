package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
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

// timelineEvent is one authored event in the score.
type timelineEvent struct {
	// Type is the event kind:
	//   key|key_down|key_up         — keyboard tap / half-events (hold across other events)
	//   button|button_down|button_up — pointer button at the CURRENT cursor (bare)
	//   move                         — positioned absolute motion (no side effect, not guarded)
	//   move_rel                     — raw relative dx/dy delta (mouse-look; not guarded/mirrored)
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
			fireAt = cursor + gap
		}
		// A following relative event measures from here.
		cursor = fireAt

		// Expand repeat: n copies at fireAt + i*step. After a repeat the cursor sits on the
		// LAST copy's time so a following relative event chains off it.
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
		for c := 0; c < n; c++ {
			at := fireAt + c*step
			resolved = append(resolved, resolvedEvent{ev: ev, fireAt: at, seq: seq})
			seq++
		}
		if n > 1 {
			cursor = fireAt + (n-1)*step
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

	for _, r := range resolved {
		ev := r.ev
		fireAt := r.fireAt
		var sub []map[string]any

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
				return plan, fmt.Errorf("%s at the current pointer, but no position is known yet — move first or pass a positioned click", strings.ToLower(ev.Type))
			}
			plan.GuardPts = append(plan.GuardPts, lastPos)

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
			descParts = append(descParts, fmt.Sprintf("move→(%d,%d)", to.X, to.Y))
			// Motion over AK windows is allowed — NOT a guard point.

		case "move_rel":
			plan.HasPointer = true
			// Raw relative delta (dx,dy) — mouse-look for a pointer-grabbing game. It has no
			// absolute landing point, so it is NOT a guard point, needs no screencast, and
			// deliberately leaves lastPos/haveLast untouched (a grab makes the true cursor
			// position unknowable). Sequence several with afterMs/atMs/frame for a turn.
			sub = []map[string]any{relMoveOp(float64(ev.DX), float64(ev.DY))}
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
				plan.GuardPts = append(plan.GuardPts, to)
			} else {
				// Scroll at the current cursor — fail closed if we can't verify where it lands
				// (mirror the bare-click rule).
				if !haveLast {
					return plan, fmt.Errorf("scroll at the current pointer, but no position is known yet — pass x,y or move first")
				}
				plan.GuardPts = append(plan.GuardPts, lastPos)
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

		timed = append(timed, absolutize(sub, fireAt)...)
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

	// --- 6/7. Description, final position, return. ------------------------------------
	plan.Desc = strings.Join(descParts, "; ")
	plan.FinalPos = lastPos
	plan.HaveFinal = haveLast
	return plan, nil
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
