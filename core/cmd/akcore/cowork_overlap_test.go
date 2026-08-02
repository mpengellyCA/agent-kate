package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// --- F25: overlapping pointer motion may not defeat the submit-time guard --------------
//
// The bypass these cover, in ONE cowork.playInput call with the pointer_control toggle on:
// each event's sub-ops are pinned at that event's own fire-time and the whole thing is
// re-sorted globally, so a slow move scheduled early is still in flight when a later event
// fires. The compiler threads its cursor in EVENT order, so the later event's guard point
// is the position the cursor was *supposed* to have — while the button actually fires
// wherever the interleaved stream left it. Measured before the fix on the auditor's script:
// guard point (10,10), button fired at (1900,1000), mirror committed (10,10).
//
// The invariant that closes it: pointer events may not overlap. Keyboard events stay
// exempt (see TestTimelineOverlapExemptsKeyboard) — overlapping holds are the feature.

// patchProfile resolves a per-event profile patch onto the default profile, so a script can
// ask for the pathological `profile:{speed:1}` the auditor used (a ~2.9s path).
func patchProfile() func(*pointerProfilePatch) PointerProfile {
	return func(p *pointerProfilePatch) PointerProfile {
		return clampProfile(PointerProfile{Speed: 1600, Accuracy: 1, SettleMs: 30}.applyPatch(p),
			PointerProfile{})
	}
}

func slowMove() *pointerProfilePatch { return &pointerProfilePatch{Speed: f64(1)} }

// The auditor's exact script: a crawling move to A, a second move to B, then a bare button
// far enough out that it looks safely after both. Before the fix it compiled, guarded
// against B and fired at A.
func TestTimelineAuditorOverlapScriptIsRefused(t *testing.T) {
	a, b, c := 0, 500, 3000
	script := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 1900, Y: 1000, AtMs: &a, Profile: slowMove()},
		{Type: "move", X: 10, Y: 10, AtMs: &b},
		{Type: "button", Button: "left", AtMs: &c},
	}}
	_, err := buildTimelineOps(script, point{5, 5}, true, patchProfile(), fixedRNG())
	if err == nil {
		t.Fatal("the overlapping move/move/button script must not compile — its button is guarded against one point and fires at another")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("the refusal should say the previous pointer action is still running: %v", err)
	}
}

func TestTimelineOverlappingMovesRefused(t *testing.T) {
	a, b := 0, 100
	script := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 1900, Y: 1000, AtMs: &a, Profile: slowMove()},
		{Type: "move", X: 10, Y: 10, AtMs: &b},
	}}
	if _, err := buildTimelineOps(script, point{5, 5}, true, patchProfile(), fixedRNG()); err == nil {
		t.Fatal("two absolute moves whose paths overlap must not compile")
	}
	// Control: the SAME two moves, spaced past the first path's flight, compile fine and
	// end where the second one aimed.
	a2, b2 := 0, 4000
	ok := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 1900, Y: 1000, AtMs: &a2, Profile: slowMove()},
		{Type: "move", X: 10, Y: 10, AtMs: &b2},
	}}
	plan, err := buildTimelineOps(ok, point{5, 5}, true, patchProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("non-overlapping moves must still compile: %v", err)
	}
	if !plan.HaveFinal || plan.FinalPos != (point{10, 10}) {
		t.Fatalf("final position = %v/%v, want (10,10)", plan.FinalPos, plan.HaveFinal)
	}
}

func TestTimelineBareButtonDuringMotionRefused(t *testing.T) {
	a, b := 0, 200
	script := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 1900, Y: 1000, AtMs: &a, Profile: slowMove()},
		{Type: "button", Button: "left", AtMs: &b},
	}}
	_, err := buildTimelineOps(script, point{5, 5}, true, patchProfile(), fixedRNG())
	if err == nil {
		t.Fatal("a bare button fired while the cursor is still travelling must not compile — its guard point is not where it lands")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "button") {
		t.Fatalf("the refusal should name the offending event: %v", err)
	}
}

func TestTimelinePositionedClickDuringMotionRefused(t *testing.T) {
	a, b := 0, 200
	script := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 1900, Y: 1000, AtMs: &a, Profile: slowMove()},
		{Type: "click", X: 300, Y: 300, AtMs: &b},
	}}
	if _, err := buildTimelineOps(script, point{5, 5}, true, patchProfile(), fixedRNG()); err == nil {
		t.Fatal("a positioned click that starts mid-flight must not compile — its own path interleaves with the one already running")
	}
	// A scroll is the same class of event and gets the same answer.
	c := 200
	scroll := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 1900, Y: 1000, AtMs: &a, Profile: slowMove()},
		{Type: "scroll", DY: 3, AtMs: &c},
	}}
	if _, err := buildTimelineOps(scroll, point{5, 5}, true, patchProfile(), fixedRNG()); err == nil {
		t.Fatal("a bare scroll mid-flight must not compile either")
	}
}

// --- F25 item 2: a repeat at ONE spot is an ordinary script, not an overlap -------------
//
// The invariant refuses a pointer op scheduled while the cursor is still in flight. A
// repeat used to place every copy at fireAt + c*step from a COMMON base, so
// `{click, repeat:3, repeatEveryMs:100}` scheduled copies 2 and 3 on top of copy 1's ~600 ms
// travel and the whole script — an ordinary one the tool docs advertise — failed to compile.
// The schedule was the bug, not the invariant: copies now chain off one another, exactly as
// a relative gap between two separate events already did.

// countOps tallies the op kinds in a compiled stream.
func countOps(ops []map[string]any) map[string]int {
	n := map[string]int{}
	for _, op := range ops {
		kind, _ := op["t"].(string)
		n[kind]++
	}
	return n
}

func TestTimelineRepeatedClickAtOneSpotCompiles(t *testing.T) {
	for _, every := range []int{0, 1, 100, 400} {
		script := timelineScript{Events: []timelineEvent{
			{Type: "click", Button: "left", X: 100, Y: 100, Repeat: 3, RepeatEveryMs: every},
		}}
		plan, err := buildTimelineOps(script, point{1900, 1000}, true, trivialProfile(), fixedRNG())
		if err != nil {
			t.Fatalf("repeatEveryMs=%d: a repeated click at one point must compile: %v", every, err)
		}
		// Three clicks, all at the one point, and one travel — the repeats add no motion.
		if n := countOps(plan.Ops)["btn"]; n != 6 {
			t.Fatalf("repeatEveryMs=%d: want 3 press/release pairs, got %d btn ops", every, n)
		}
		pts, ok := effectPoints(plan.Ops, point{1900, 1000}, true, testBounds())
		if !ok || len(pts) != 1 || pts[0] != (point{100, 100}) {
			t.Fatalf("repeatEveryMs=%d: every copy must act at (100,100), got %v ok=%v", every, pts, ok)
		}
		if !plan.HaveFinal || plan.FinalPos != (point{100, 100}) {
			t.Fatalf("repeatEveryMs=%d: final position = %v/%v", every, plan.FinalPos, plan.HaveFinal)
		}
	}
}

// A bare button burst at the cursor is the same shape without the travel.
func TestTimelineRepeatedBareButtonCompiles(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "button", Button: "left", HoldMs: 200, Repeat: 4, RepeatEveryMs: 100},
	}}
	plan, err := buildTimelineOps(script, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("a held-button burst must compile (each copy waits for the previous hold): %v", err)
	}
	if n := countOps(plan.Ops)["btn"]; n != 8 {
		t.Fatalf("want 4 press/release pairs, got %d btn ops", n)
	}
	// The copies may not overlap their own holds: 4 copies × 200 ms hold ≥ 600 ms of span.
	if span := opsSpanMs(plan.Ops); span < 600 {
		t.Fatalf("copies must chain past each other's hold, span = %dms", span)
	}
}

// The other direction — the mutation check. A repeat must not become a licence to schedule
// ANY pointer event during in-flight motion: an independent event that lands mid-burst, and
// a repeat whose copies are crossed by a differently-targeted move, are still refused.
func TestTimelineRepeatDoesNotReopenOverlap(t *testing.T) {
	mid := 50
	burstThenStranger := timelineScript{Events: []timelineEvent{
		{Type: "click", Button: "left", X: 100, Y: 100, Repeat: 3, RepeatEveryMs: 100, Profile: slowMove()},
		{Type: "click", Button: "left", X: 1500, Y: 900, AtMs: &mid},
	}}
	if _, err := buildTimelineOps(burstThenStranger, point{1900, 1000}, true, patchProfile(), fixedRNG()); err == nil {
		t.Fatal("a differently-targeted click scheduled inside a repeat burst must still be refused")
	}
	// …and a move crossing the burst, which would displace the cursor out from under the
	// copies' presses.
	crossing := timelineScript{Events: []timelineEvent{
		{Type: "click", Button: "left", X: 100, Y: 100, Repeat: 3, RepeatEveryMs: 100, Profile: slowMove()},
		{Type: "move", X: 800, Y: 800, AtMs: &mid},
	}}
	if _, err := buildTimelineOps(crossing, point{1900, 1000}, true, patchProfile(), fixedRNG()); err == nil {
		t.Fatal("a move scheduled inside a repeat burst must still be refused")
	}
}

// Keyboard repeats keep their EXACT cadence — they are exempt from the invariant and must
// not be pushed around by pointer chaining.
func TestTimelineKeyboardRepeatKeepsItsCadence(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", Repeat: 3, RepeatEveryMs: 120},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("a key repeat must compile: %v", err)
	}
	if span := opsSpanMs(plan.Ops); span != 240 {
		t.Fatalf("three taps 120ms apart span 240ms, got %dms", span)
	}
}

// Keyboard events are DELIBERATELY exempt: a script that holds W while the pointer travels
// (and taps space in the middle) is the whole reason the timeline exists.
func TestTimelineOverlapExemptsKeyboard(t *testing.T) {
	a, b, c, e := 0, 100, 1000, 4000
	script := timelineScript{Events: []timelineEvent{
		{Type: "key_down", Key: "w", AtMs: &a},
		{Type: "move", X: 1900, Y: 1000, AtMs: &b, Profile: slowMove()},
		{Type: "key", Key: "space", AtMs: &c},
		{Type: "key_up", Key: "w", AtMs: &e},
	}}
	plan, err := buildTimelineOps(script, point{5, 5}, true, patchProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("keys overlapping pointer motion must still compile: %v", err)
	}
	if !plan.HasKey || !plan.HasPointer {
		t.Fatalf("mixed script: key=%v pointer=%v", plan.HasKey, plan.HasPointer)
	}
}

// The natural, untimed `[click A, click B]` score must keep working: a relative gap is
// measured from the END of the previous pointer action, so the two chain instead of
// colliding. (Before this change both fired at t=0 and their paths interleaved.)
func TestTimelineRelativeGapsChainAfterMotion(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "click", X: 400, Y: 400},
		{Type: "click", X: 1200, Y: 800},
	}}
	plan, err := buildTimelineOps(script, point{0, 0}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("two untimed clicks must chain, not collide: %v", err)
	}
	if len(plan.GuardPts) != 2 || plan.GuardPts[0] != (point{400, 400}) || plan.GuardPts[1] != (point{1200, 800}) {
		t.Fatalf("both click targets must be guarded, got %v", plan.GuardPts)
	}
	// Flight order must match authoring order: the first click's press/release happen while
	// the cursor is at A, and every move toward B comes afterwards.
	firstBtn, lastMoveBefore := -1, point{}
	for i, op := range plan.Ops {
		kind, _ := op["t"].(string)
		if kind == "move" && firstBtn < 0 {
			x, _ := op["x"].(int)
			y, _ := op["y"].(int)
			lastMoveBefore = point{x, y}
		}
		if kind == "btn" && firstBtn < 0 {
			firstBtn = i
		}
	}
	if firstBtn < 0 {
		t.Fatal("no button op in the stream")
	}
	if lastMoveBefore != (point{400, 400}) {
		t.Fatalf("the first click must fire at A(400,400), not %v", lastMoveBefore)
	}
	if !plan.HaveFinal || plan.FinalPos != (point{1200, 800}) {
		t.Fatalf("final position = %v/%v, want B(1200,800)", plan.FinalPos, plan.HaveFinal)
	}
}

// A held modifier released with a relative gap must be released AFTER the click it was held
// for — the documented "Ctrl-down → click → Ctrl-up opens a link in a new tab" score.
func TestTimelineModifierChordReleasesAfterTheClick(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key_down", Key: "ctrl"},
		{Type: "click", X: 900, Y: 500},
		{Type: "key_up", Key: "ctrl"},
	}}
	plan, err := buildTimelineOps(script, point{0, 0}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("modifier chord must compile: %v", err)
	}
	lastBtn, keyUp := -1, -1
	for i, op := range plan.Ops {
		kind, _ := op["t"].(string)
		if kind == "btn" {
			lastBtn = i
		}
		if kind == "key" && op["state"].(uint32) == 0 {
			keyUp = i
		}
	}
	if lastBtn < 0 || keyUp < 0 {
		t.Fatalf("expected both a button and a key release in the stream: %v", opStream(plan.Ops))
	}
	if keyUp < lastBtn {
		t.Fatalf("the modifier is released at op %d, BEFORE the click at op %d — the chord never happens", keyUp, lastBtn)
	}
}

// The invariant is only acceptable if nothing legitimate breaks under it: overlapping
// absolute motion is never semantically meaningful, but every ordinary choreography — a
// drag, a double-click, a frame-addressed game score, scroll-then-click — must still
// compile, and end where its last event aimed.
func TestTimelineOrdinaryScriptsStillCompile(t *testing.T) {
	cases := map[string]struct {
		script timelineScript
		final  point
	}{
		"drag": {timelineScript{Events: []timelineEvent{
			{Type: "move", X: 300, Y: 300},
			{Type: "button_down", Button: "left"},
			{Type: "move", X: 900, Y: 700},
			{Type: "button_up", Button: "left"},
		}}, point{900, 700}},
		"move then bare click": {timelineScript{Events: []timelineEvent{
			{Type: "move", X: 640, Y: 480},
			{Type: "button", Button: "left"},
		}}, point{640, 480}},
		"double click": {timelineScript{Events: []timelineEvent{
			{Type: "click", X: 640, Y: 480, Count: 2},
		}}, point{640, 480}},
		"bare button repeat": {timelineScript{Events: []timelineEvent{
			{Type: "button", Button: "left", Repeat: 5, RepeatEveryMs: 120},
		}}, point{500, 500}},
		"frame-addressed game score": {timelineScript{FPS: 60, Events: []timelineEvent{
			{Type: "key_down", Key: "w", Frame: iptr(0)},
			{Type: "move_rel", DX: 60, DY: 0, Frame: iptr(6)},
			{Type: "move_rel", DX: 60, DY: 0, Frame: iptr(12)},
			{Type: "button", Button: "left", Frame: iptr(18)},
			{Type: "key_up", Key: "w", Frame: iptr(30)},
		}, Bounds: testBounds()}, point{620, 500}},
		"scroll then click": {timelineScript{Events: []timelineEvent{
			{Type: "scroll", X: 800, Y: 600, DY: 5},
			{Type: "click", X: 800, Y: 620},
		}}, point{800, 620}},
		"type then click": {timelineScript{Events: []timelineEvent{
			{Type: "key", Key: "a"},
			{Type: "key", Key: "b", AfterMs: iptr(50)},
			{Type: "click", X: 1000, Y: 400, AfterMs: iptr(200)},
		}}, point{1000, 400}},
	}
	for name, c := range cases {
		plan, err := buildTimelineOps(c.script, point{500, 500}, true, trivialProfile(), fixedRNG())
		if err != nil {
			t.Errorf("%s: must still compile: %v", name, err)
			continue
		}
		if !plan.HaveFinal || plan.FinalPos != c.final {
			t.Errorf("%s: final position = %v/%v, want %v", name, plan.FinalPos, plan.HaveFinal, c.final)
		}
	}
}

// Defence in depth: FinalPos (what the handler commits as the mirror) must be what the
// COMPILED stream ends on, not merely what the compiler intended.
func TestTimelineFinalPosMatchesTheCompiledStream(t *testing.T) {
	a, b, c := 0, 1500, 3000
	script := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 500, Y: 500, AtMs: &a},
		{Type: "click", X: 1200, Y: 300, AtMs: &b},
		{Type: "key", Key: "a", AtMs: &c},
	}}
	plan, err := buildTimelineOps(script, point{0, 0}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, ok := lastAbsMove(plan.Ops)
	if !ok {
		t.Fatal("the stream should contain an absolute move")
	}
	if !plan.HaveFinal || plan.FinalPos != got {
		t.Fatalf("FinalPos %v/%v disagrees with the stream's last move %v", plan.FinalPos, plan.HaveFinal, got)
	}
}

// UsedSeedPos marks the plans whose guard evidence is the (global) mirror rather than a
// coordinate the script names — those are the ones the handler must re-prove before firing.
func TestTimelineUsedSeedPos(t *testing.T) {
	bare := timelineScript{Events: []timelineEvent{{Type: "button", Button: "left"}}}
	plan, err := buildTimelineOps(bare, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !plan.UsedSeedPos {
		t.Fatal("a bare button guarded at the seeded mirror position must be flagged")
	}
	// A script that establishes its own position first depends on no seed.
	own := timelineScript{Events: []timelineEvent{
		{Type: "move", X: 800, Y: 800},
		{Type: "button", Button: "left"},
	}}
	plan, err = buildTimelineOps(own, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.UsedSeedPos {
		t.Fatal("a script that moves to its own target first does not depend on the seeded mirror")
	}
}

// A pointer script that never names an absolute target derives everything — guard points
// AND final position — from the seeded mirror, so the whole plan must be re-proven before
// it fires.
func TestTimelineSeedDependentPlans(t *testing.T) {
	relOnly := timelineScript{
		Events: []timelineEvent{{Type: "move_rel", DX: 40, DY: 0}},
		Bounds: testBounds(),
	}
	plan, err := buildTimelineOps(relOnly, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !plan.UsedSeedPos {
		t.Fatal("a move_rel accumulates from the seed, so the plan depends on it")
	}
	// With no seed at all there is nothing to re-prove: mouse-look from an unknown position
	// stays legal (it commits no position either — see TestTimelineMoveRelative).
	plan, err = buildTimelineOps(relOnly, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.UsedSeedPos {
		t.Fatal("a nudge with no known start depends on no seed — refusing it would break mouse-look")
	}
	// A script that names its own coordinates does not.
	abs := timelineScript{Events: []timelineEvent{{Type: "click", X: 700, Y: 700}}}
	plan, err = buildTimelineOps(abs, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.UsedSeedPos {
		t.Fatal("a positioned click names its own target and does not depend on the seed")
	}
}

// --- F26: the mirror is global, because the cursor is ---------------------------------

// What a finished playInput may write to the SHARED mirror. The keyboard-only case is the
// one globalisation made dangerous: its FinalPos is just the seed it compiled against, and
// another thread may have moved the real cursor during the consent wait.
func TestPlayMirrorOutcome(t *testing.T) {
	kbd := timelinePlan{HasKey: true, FinalPos: point{1800, 900}, HaveFinal: true}
	if commit, why := playMirrorOutcome(kbd, true); commit || why != "" {
		t.Fatalf("a keyboard-only script must not write the shared mirror, got commit=%v why=%q", commit, why)
	}
	landed := timelinePlan{HasPointer: true, FinalPos: point{700, 700}, HaveFinal: true}
	if commit, why := playMirrorOutcome(landed, true); !commit || why != "" {
		t.Fatalf("a proven landing must commit, got commit=%v why=%q", commit, why)
	}
	if commit, why := playMirrorOutcome(landed, false); commit || why != mirrorLostUnproven {
		t.Fatalf("an unproven landing must destroy the mirror, got commit=%v why=%q", commit, why)
	}
	relLost := timelinePlan{HasPointer: true, RelLost: true}
	if commit, why := playMirrorOutcome(relLost, true); commit || why != mirrorLostRelative {
		t.Fatalf("an unaccountable nudge must destroy the mirror, got commit=%v why=%q", commit, why)
	}
}

// The attack: thread A parks the real cursor over an Agent Kate window (motion is
// deliberately unguarded, so A's mirror is honestly on-target), and thread B then fires a
// bare click. With a per-thread mirror B's guard read B's own earlier clean position and
// cleared the click; the portal fired at A's parked point — our own consent controls.
//
// This drives the REAL decision — cursorAction.run over the ops cowork.injectInput builds
// from the very same agent request — and asserts the OUTCOME: refused, and nothing released
// to the portal. The previous version of this test re-implemented the handler's guard inside
// itself, so deleting the handler's guard left it green (audit F25/F26 wiring, third
// report). There is no guard to re-implement now: run derives the point the ops act at.
func TestCrossThreadParkThenBareClickIsRefused(t *testing.T) {
	geom := akGeometry(t)
	ps := newPointerState()

	// What cowork.injectInput compiles for {"type":"button","button":"left"} — a bare click
	// at wherever the cursor is.
	ops, _, err := buildInjectOps([]injectEvent{{Type: "button", Button: "left"}})
	if err != nil {
		t.Fatalf("buildInjectOps: %v", err)
	}
	var fired []point
	bareClick := func(thread string) error {
		seed, ok := ps.last()
		if !ok {
			return fmt.Errorf("%s", bareClickRefusal(ps.mirrorLoss()))
		}
		return cursorAction{
			Geometry: geom,
			Ops:      ops,
			Seed:     &seed,
			Fire: func(*cursorHold, []map[string]any) (bool, error) {
				at, _ := ps.last()
				fired = append(fired, at)
				return false, nil
			},
		}.run(context.Background(), ps, thread)
	}

	// Thread B establishes a clean position of its own and may click there.
	ps.setLast("thread-B", point{1800, 900})
	if err := bareClick("thread-B"); err != nil {
		t.Fatalf("a bare click over B's own verified position must be allowed: %v", err)
	}
	if len(fired) != 1 || fired[0] != (point{1800, 900}) {
		t.Fatalf("the allowed click should have fired once at (1800,900), got %v", fired)
	}

	// Thread A parks the cursor on the Agent Kate window (allowed: motion has no side
	// effect, and A's move landed provably).
	ps.setLast("thread-A", point{300, 200})

	err = bareClick("thread-B")
	if err == nil {
		t.Fatal("B's bare click must be refused: the one shared cursor is now parked on an Agent Kate window")
	}
	if !strings.Contains(err.Error(), "Agent Kate") {
		t.Fatalf("the refusal must name the self-target case, got %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("the refused click must not have reached the portal; the op stream was %v", fired)
	}
}

func TestMirrorInvalidationIsGlobal(t *testing.T) {
	ps := newPointerState()
	ps.setLast("thread-B", point{1000, 1000})
	// A's move played but could not be proven: the cursor is somewhere nobody can name, so
	// NO thread may fire a bare click — not just A's.
	ps.commitPointer("thread-A", pointerPlay{played: true, landed: false}, point{50, 50}, true)
	if p, ok := ps.last(); ok {
		t.Fatalf("one thread's unproven action must destroy the mirror for everyone, got %v", p)
	}
	if why := ps.mirrorLoss(); why != mirrorLostUnproven {
		t.Fatalf("loss reason = %q, want %q", why, mirrorLostUnproven)
	}
	// And a relative nudge by A that ran off the desktop invalidates it for B too.
	ps.setLast("thread-B", point{10, 10})
	if _, known := ps.applyRelative("thread-A", -500, 0, testBounds()); known {
		t.Fatal("a nudge into the compositor's edge clamp must not stay known")
	}
	if _, ok := ps.last(); ok {
		t.Fatal("the destroyed mirror must be destroyed globally")
	}
}

// seedStillHolds is the guard→fire re-proof: a bare action's evidence is only good while
// the mirror still says what it said when the action was checked.
func TestSeedStillHoldsCatchesTheInterleavedThread(t *testing.T) {
	ps := newPointerState()
	seed := point{1800, 900}
	ps.setLast("thread-B", seed)
	if err := ps.seedStillHolds("thread-B", seed); err != nil {
		t.Fatalf("an untouched mirror must still hold: %v", err)
	}
	// Another thread moves the cursor while this call waits on consent.
	ps.setLast("thread-A", point{300, 200})
	err := ps.seedStillHolds("thread-B", seed)
	if err == nil {
		t.Fatal("a mirror another thread moved must not still hold — that is the F26 interleave")
	}
	if !strings.Contains(err.Error(), "1800") || !strings.Contains(err.Error(), "300") {
		t.Fatalf("the refusal should name both positions: %v", err)
	}
	// Destroyed, not moved: same answer.
	ps.invalidate("thread-A", mirrorLostUnproven)
	if err := ps.seedStillHolds("thread-B", seed); err == nil {
		t.Fatal("a destroyed mirror must not still hold")
	}
}
