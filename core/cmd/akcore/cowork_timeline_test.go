package main

import (
	"testing"
)

// trivialProfile is the fixed effective profile the timeline tests resolve every move
// against (the handler would clamp to the user's bounds; tests just pin it). resolveProfile
// closes over it and ignores the per-move patch — the patch plumbing is exercised by the
// pointer tests, not here.
func trivialProfile() func(*pointerProfilePatch) PointerProfile {
	return func(*pointerProfilePatch) PointerProfile {
		return PointerProfile{Speed: 1600, Accuracy: 1, SettleMs: 30}
	}
}

// opStream renders the op list to a comparable string per op: t|key/button|state|delayMs.
// It is the canonical fingerprint the "three dialects agree" test compares.
func opStream(ops []map[string]any) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		t, _ := op["t"].(string)
		var detail string
		switch t {
		case "key":
			detail = uintStr(op["keysym"]) + ":" + uintStr(op["state"])
		case "btn":
			detail = uintStr(op["button"]) + ":" + uintStr(op["state"])
		case "move":
			detail = intStr(op["x"]) + "," + intStr(op["y"])
		case "axis_discrete":
			detail = intStr(op["axis"]) + ":" + intStr(op["steps"])
		}
		out = append(out, t+"|"+detail+"|d"+intStr(op["delayMs"]))
	}
	return out
}

func uintStr(v any) string {
	if u, ok := v.(uint32); ok {
		return uitoa(int(u))
	}
	return "?"
}
func intStr(v any) string {
	if v == nil {
		return "-"
	}
	return uitoa(intOf(v))
}
func uitoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func mustKeysym(t *testing.T, name string) uint32 {
	t.Helper()
	ks, err := keysymFor(name)
	if err != nil {
		t.Fatalf("keysymFor(%q): %v", name, err)
	}
	return ks
}

//  1. A hold must stay down continuously across an interleaved tap: w-down FIRST,
//     space down/up in the middle, w-up LAST.
func TestTimelineOverlappingHold(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key_down", Key: "w"},                    // @0
		{Type: "key", Key: "space", AfterMs: iptr(200)}, // @200, hold 0
		{Type: "key_up", Key: "w", AfterMs: iptr(200)},  // @400 (after space)
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	w := mustKeysym(t, "w")
	sp := mustKeysym(t, "space")
	if len(plan.Ops) != 4 {
		t.Fatalf("want 4 ops (w-down, space-down, space-up, w-up), got %d: %v", len(plan.Ops), opStream(plan.Ops))
	}
	// w-down first.
	if plan.Ops[0]["keysym"] != w || plan.Ops[0]["state"].(uint32) != 1 {
		t.Fatalf("op[0] should be w-down, got %+v", plan.Ops[0])
	}
	// space tap in the middle.
	if plan.Ops[1]["keysym"] != sp || plan.Ops[1]["state"].(uint32) != 1 {
		t.Fatalf("op[1] should be space-down, got %+v", plan.Ops[1])
	}
	if plan.Ops[2]["keysym"] != sp || plan.Ops[2]["state"].(uint32) != 0 {
		t.Fatalf("op[2] should be space-up, got %+v", plan.Ops[2])
	}
	// w-up LAST.
	if plan.Ops[3]["keysym"] != w || plan.Ops[3]["state"].(uint32) != 0 {
		t.Fatalf("op[3] should be w-up (release after the tap), got %+v", plan.Ops[3])
	}
	if !plan.HasKey || plan.HasPointer {
		t.Fatalf("keyboard-only timeline: HasKey=%v HasPointer=%v", plan.HasKey, plan.HasPointer)
	}
}

//  2. The same logical score authored via afterMs, atMs, and frame+fps must compile to an
//     identical op stream.
func TestTimelineSchedulingDialectsAgree(t *testing.T) {
	// tap a@0, tap b@500, tap c@1000.
	viaAfter := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a"},
		{Type: "key", Key: "b", AfterMs: iptr(500)},
		{Type: "key", Key: "c", AfterMs: iptr(500)},
	}}
	viaAt := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", AtMs: iptr(0)},
		{Type: "key", Key: "b", AtMs: iptr(500)},
		{Type: "key", Key: "c", AtMs: iptr(1000)},
	}}
	// fps=2 ⇒ frame 0,1,2 → 0,500,1000 ms.
	viaFrame := timelineScript{FPS: 2, Events: []timelineEvent{
		{Type: "key", Key: "a", Frame: iptr(0)},
		{Type: "key", Key: "b", Frame: iptr(1)},
		{Type: "key", Key: "c", Frame: iptr(2)},
	}}

	pa, err := buildTimelineOps(viaAfter, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("afterMs: %v", err)
	}
	pt, err := buildTimelineOps(viaAt, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("atMs: %v", err)
	}
	pf, err := buildTimelineOps(viaFrame, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("frame: %v", err)
	}

	sa, st, sf := opStream(pa.Ops), opStream(pt.Ops), opStream(pf.Ops)
	if !eqStrings(sa, st) {
		t.Fatalf("afterMs vs atMs differ:\n  after=%v\n  at=%v", sa, st)
	}
	if !eqStrings(sa, sf) {
		t.Fatalf("afterMs vs frame differ:\n  after=%v\n  frame=%v", sa, sf)
	}
	// Spot-check the cadence: b-down and c-down each carry a 500ms gap.
	if pa.Ops[2]["delayMs"] != 500 || pa.Ops[4]["delayMs"] != 500 {
		t.Fatalf("expected 500ms gaps before b and c: %v", sa)
	}
}

// 3. holdMs rides the up op as delayMs (down/up adjacent in the stream).
func TestTimelineHoldMsSpacing(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "space", HoldMs: 120},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plan.Ops) != 2 {
		t.Fatalf("want 2 ops, got %d", len(plan.Ops))
	}
	if _, ok := plan.Ops[0]["delayMs"]; ok {
		t.Fatalf("first op must carry no delayMs (UI ignores op[0] delay)")
	}
	if plan.Ops[1]["delayMs"] != 120 {
		t.Fatalf("up op should carry 120ms hold, got %v", plan.Ops[1]["delayMs"])
	}
}

// 4. repeat: 3× a tap ⇒ 6 ops; the down ops are spaced repeatEveryMs apart.
func TestTimelineRepeat(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "space", Repeat: 3, RepeatEveryMs: 100},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plan.Ops) != 6 {
		t.Fatalf("3 taps ⇒ want 6 ops, got %d: %v", len(plan.Ops), opStream(plan.Ops))
	}
	// Ops are: down0, up0, down1, up1, down2, up2. With hold 0, each down lands 100ms after
	// the previous down (the up shares the down's time, so its delta is 0 and the next down
	// carries the 100ms gap).
	if _, ok := plan.Ops[0]["delayMs"]; ok {
		t.Fatalf("op[0] must have no delay")
	}
	if plan.Ops[2]["delayMs"] != 100 {
		t.Fatalf("second down should be +100ms, got %v", plan.Ops[2]["delayMs"])
	}
	if plan.Ops[4]["delayMs"] != 100 {
		t.Fatalf("third down should be +100ms, got %v", plan.Ops[4]["delayMs"])
	}
	// Cumulative: down times are 0, 100, 200.
	cum := 0
	downTimes := []int{}
	for _, op := range plan.Ops {
		cum += intOf(op["delayMs"])
		if op["state"].(uint32) == 1 {
			downTimes = append(downTimes, cum)
		}
	}
	if len(downTimes) != 3 || downTimes[0] != 0 || downTimes[1] != 100 || downTimes[2] != 200 {
		t.Fatalf("down cadence should be 0,100,200; got %v", downTimes)
	}
}

// 5. Final delta lowering: events at atMs 0/300/800 ⇒ first ops carry deltas 0/300/500.
func TestTimelineDeltaLowering(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", AtMs: iptr(0)},
		{Type: "key", Key: "b", AtMs: iptr(300)},
		{Type: "key", Key: "c", AtMs: iptr(800)},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 6 ops: a-down/up @0, b-down/up @300, c-down/up @800.
	if len(plan.Ops) != 6 {
		t.Fatalf("want 6 ops, got %d", len(plan.Ops))
	}
	if _, ok := plan.Ops[0]["delayMs"]; ok {
		t.Fatalf("first op must have no delayMs")
	}
	// a-up shares a's time ⇒ no delay. b-down carries 300. c-down carries 500.
	if _, ok := plan.Ops[1]["delayMs"]; ok {
		t.Fatalf("a-up shares a-down's time, should carry no delay")
	}
	if plan.Ops[2]["delayMs"] != 300 {
		t.Fatalf("b-down should carry 300ms, got %v", plan.Ops[2]["delayMs"])
	}
	if plan.Ops[4]["delayMs"] != 500 {
		t.Fatalf("c-down should carry 500ms (800-300), got %v", plan.Ops[4]["delayMs"])
	}
}

// 6. Held-set imbalance is rejected in both directions.
func TestTimelineHeldImbalance(t *testing.T) {
	if _, err := buildTimelineOps(timelineScript{Events: []timelineEvent{
		{Type: "key_down", Key: "w"},
	}}, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("dangling key_down should error")
	}
	if _, err := buildTimelineOps(timelineScript{Events: []timelineEvent{
		{Type: "key_up", Key: "w"},
	}}, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("orphan key_up should error")
	}
}

// 7. Span cap: an event past 30s ⇒ error (not silent truncation).
func TestTimelineSpanCap(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", AtMs: iptr(0)},
		{Type: "key", Key: "b", AtMs: iptr(31000)},
	}}
	if _, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("span > 30s should error")
	}
}

// 8. Explicit hold cap: a down→up held 11s ⇒ error.
func TestTimelineExplicitHoldCap(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key_down", Key: "w", AtMs: iptr(0)},
		{Type: "key_up", Key: "w", AtMs: iptr(11000)},
	}}
	if _, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("explicit 11s hold should error")
	}
}

// 9. Guard points: a click adds its target; a move does not.
func TestTimelineGuardPoints(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 200, Y: 200},
		{Type: "click", X: 1320, Y: 540},
	}}
	plan, err := buildTimelineOps(script, point{0, 0}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plan.GuardPts) != 1 {
		t.Fatalf("only the click is a guard point, got %d: %v", len(plan.GuardPts), plan.GuardPts)
	}
	if plan.GuardPts[0] != (point{1320, 540}) {
		t.Fatalf("guard point should be the click target, got %v", plan.GuardPts[0])
	}
	if !plan.HaveFinal || plan.FinalPos != (point{1320, 540}) {
		t.Fatalf("final position should be the click target, got %v (have=%v)", plan.FinalPos, plan.HaveFinal)
	}
}

//  10. Bare button fail-closed: with no known start it errors; with a known start it is
//     allowed and guards the start position.
func TestTimelineBareButtonFailClosed(t *testing.T) {
	bare := timelineScript{Events: []timelineEvent{{Type: "button", Button: "left"}}}
	if _, err := buildTimelineOps(bare, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("bare button with unknown cursor should fail closed")
	}
	plan, err := buildTimelineOps(bare, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("bare button with known cursor should be allowed: %v", err)
	}
	if len(plan.GuardPts) != 1 || plan.GuardPts[0] != (point{500, 500}) {
		t.Fatalf("bare button should guard the known cursor position, got %v", plan.GuardPts)
	}
	if !plan.HasPointer {
		t.Fatalf("bare button is a pointer event ⇒ HasPointer")
	}
}

// 11. Capability flags: keyboard-only ⇒ HasKey only; mixed ⇒ both.
func TestTimelineCapabilityFlags(t *testing.T) {
	kbd := timelineScript{Events: []timelineEvent{{Type: "key", Key: "a"}}}
	pk, err := buildTimelineOps(kbd, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("kbd: %v", err)
	}
	if !pk.HasKey || pk.HasPointer {
		t.Fatalf("keyboard-only: HasKey=%v HasPointer=%v", pk.HasKey, pk.HasPointer)
	}

	mixed := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a"},
		{Type: "click", X: 100, Y: 100, AfterMs: iptr(50)},
	}}
	pm, err := buildTimelineOps(mixed, point{0, 0}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if !pm.HasKey || !pm.HasPointer {
		t.Fatalf("mixed: HasKey=%v HasPointer=%v", pm.HasKey, pm.HasPointer)
	}
}

// 12. Mutual exclusivity: setting both atMs and afterMs ⇒ error.
func TestTimelineMutualExclusivity(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", AtMs: iptr(100), AfterMs: iptr(50)},
	}}
	if _, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("event with both atMs and afterMs should error")
	}
}

// Extra: empty timeline and a wait advancing the clock.
func TestTimelineEmptyAndWait(t *testing.T) {
	if _, err := buildTimelineOps(timelineScript{}, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("empty timeline should error")
	}
	// wait pushes the following relative tap out by its duration.
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a"},
		{Type: "wait", AfterMs: iptr(400)},
		{Type: "key", Key: "b"},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if len(plan.Ops) != 4 {
		t.Fatalf("wait emits no ops; want 4 (a-down/up, b-down/up), got %d", len(plan.Ops))
	}
	// b-down should land 400ms after a (a's up shares a's time).
	if plan.Ops[2]["delayMs"] != 400 {
		t.Fatalf("b-down should be +400ms after the wait, got %v", plan.Ops[2]["delayMs"])
	}
}

// Extra: a positioned scroll guards its target and moves there; a scroll at an unknown
// cursor fails closed; scroll axes mirror cowork.scroll (dy→axis0, dx→axis1).
func TestTimelineScroll(t *testing.T) {
	// Bare scroll, unknown cursor ⇒ fail closed.
	if _, err := buildTimelineOps(timelineScript{Events: []timelineEvent{
		{Type: "scroll", DY: 3},
	}}, point{}, false, trivialProfile(), fixedRNG()); err == nil {
		t.Fatalf("bare scroll with unknown cursor should fail closed")
	}
	// Positioned scroll ⇒ guards the target, emits a move then the axis ops.
	plan, err := buildTimelineOps(timelineScript{Events: []timelineEvent{
		{Type: "scroll", X: 800, Y: 600, DY: 3, DX: -2},
	}}, point{0, 0}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("positioned scroll: %v", err)
	}
	if len(plan.GuardPts) != 1 || plan.GuardPts[0] != (point{800, 600}) {
		t.Fatalf("positioned scroll should guard its target, got %v", plan.GuardPts)
	}
	// Last two ops are the vertical then horizontal axis ops.
	last := plan.Ops[len(plan.Ops)-1]
	prev := plan.Ops[len(plan.Ops)-2]
	if prev["t"] != "axis_discrete" || prev["axis"] != 0 || prev["steps"] != 3 {
		t.Fatalf("vertical scroll op (axis0, dy=3) malformed: %+v", prev)
	}
	if last["t"] != "axis_discrete" || last["axis"] != 1 || last["steps"] != -2 {
		t.Fatalf("horizontal scroll op (axis1, dx=-2) malformed: %+v", last)
	}
	if plan.FinalPos != (point{800, 600}) {
		t.Fatalf("scroll should commit the move target as final pos, got %v", plan.FinalPos)
	}
}

// --- tiny local helpers (kept here so the file is self-contained) ------------------

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTimelineMoveRelative(t *testing.T) {
	a, b := 0, 50
	script := timelineScript{Events: []timelineEvent{
		{Type: "move_rel", DX: 120, DY: -30, AtMs: &a},
		{Type: "move_rel", DX: 120, DY: 0, AtMs: &b},
	}}
	// haveStart=false on purpose: relative motion needs no known start, unlike scroll/click.
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !plan.HasPointer || plan.HasKey {
		t.Fatalf("move_rel should set HasPointer only: pointer=%v key=%v", plan.HasPointer, plan.HasKey)
	}
	// No absolute landing point ⇒ never a self-target guard point, never a tracked position.
	if len(plan.GuardPts) != 0 {
		t.Fatalf("move_rel must add no guard points, got %d", len(plan.GuardPts))
	}
	if plan.HaveFinal {
		t.Fatalf("move_rel must not establish a tracked absolute position")
	}
	if len(plan.Ops) != 2 {
		t.Fatalf("want 2 move_rel ops, got %d: %v", len(plan.Ops), opStream(plan.Ops))
	}
	if plan.Ops[0]["t"] != "move_rel" || plan.Ops[0]["dx"].(float64) != 120 || plan.Ops[0]["dy"].(float64) != -30 {
		t.Fatalf("op[0] malformed: %+v", plan.Ops[0])
	}
	if _, ok := plan.Ops[0]["x"]; ok {
		t.Fatalf("move_rel op must not carry an absolute x: %+v", plan.Ops[0])
	}
	// Second nudge fires 50ms after the first (global delta lowering).
	if plan.Ops[1]["delayMs"] != 50 {
		t.Fatalf("op[1] should be +50ms, got %v", plan.Ops[1]["delayMs"])
	}
}

func TestTimelineMoveRelativeOverlapWithHeldKey(t *testing.T) {
	// The strafe-and-turn shape: hold W while nudging the view right, then release.
	d0, d1, d2, d3 := 0, 20, 40, 60
	script := timelineScript{Events: []timelineEvent{
		{Type: "key_down", Key: "w", AtMs: &d0},
		{Type: "move_rel", DX: 80, DY: 0, AtMs: &d1},
		{Type: "move_rel", DX: 80, DY: 0, AtMs: &d2},
		{Type: "key_up", Key: "w", AtMs: &d3},
	}}
	plan, err := buildTimelineOps(script, point{}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !plan.HasKey || !plan.HasPointer {
		t.Fatalf("mixed script should need both caps: key=%v pointer=%v", plan.HasKey, plan.HasPointer)
	}
	// key down, two nudges, key up = 4 ops in time order.
	if len(plan.Ops) != 4 {
		t.Fatalf("want 4 ops, got %d: %v", len(plan.Ops), opStream(plan.Ops))
	}
	if plan.Ops[0]["t"] != "key" || plan.Ops[0]["state"].(uint32) != 1 {
		t.Fatalf("op[0] should be the W key-down: %+v", plan.Ops[0])
	}
	if plan.Ops[3]["t"] != "key" || plan.Ops[3]["state"].(uint32) != 0 {
		t.Fatalf("op[3] should be the W key-up: %+v", plan.Ops[3])
	}
}
