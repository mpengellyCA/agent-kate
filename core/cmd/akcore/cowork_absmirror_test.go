package main

import (
	"math/rand"
	"strings"
	"testing"

	"agentkate/internal/kde"
)

// --- F3 (absolute half): an absolute action that did not land may not leave a mirror ---
//
// The hole these cover: desktop_move_pointer(x,y) to a point inside NO captured screen.
// The UI has no stream node to address it to, so it drops the op — the cursor never moves
// — but the core used to record the requested point anyway, because the portal call came
// back without an error. Every later guarded action was then validated against a position
// the cursor was not at. That is the same bypass as a stale relative nudge, reachable with
// no relative motion at all.
//
// The rule (round 10, for relative motion; now for absolute too): the mirror is EVIDENCE.
// It is committed only when the UI PROVES the batch landed on the requested point, and is
// destroyed on anything less.

// okReply is the UI's reply for a batch that played in full and landed on (x,y).
func okReply(x, y, applied int) kde.PortalResult {
	return kde.PortalResult{OK: true, Kind: "inject", OpsApplied: applied, PtrKnown: true, PtrX: x, PtrY: y}
}

// droppedReply is the UI's reply for a batch it could not apply in full: the move had no
// containing screen, so the batch was abandoned at that op and the pointer is unproven.
func droppedReply(applied, dropped int) kde.PortalResult {
	return kde.PortalResult{OK: true, Kind: "inject", OpsApplied: applied, OpsDropped: dropped}
}

func moveBatch(from point, to point) []map[string]any {
	return expandMove(from, true, to, PointerProfile{Speed: 1600, Accuracy: 1.0}, rand.New(rand.NewSource(1)))
}

func TestAbsoluteMoveOffEveryScreenInvalidatesTheMirror(t *testing.T) {
	ps := newPointerState()
	const thread = "t-offscreen"
	ps.setLast(thread, point{300, 300})

	// The agent aims at a point no captured screen contains. The UI drops the move (and
	// abandons the rest of the batch), so nothing landed.
	target := point{5000, 5000}
	ops := moveBatch(point{300, 300}, target)
	res := droppedReply(3, len(ops)-3)
	play := pointerPlay{played: true, landed: opsLandedAsAimed(ops, res)}
	if play.landed {
		t.Fatal("a batch the UI reported dropping ops from must never count as landed")
	}
	ps.commitPointer(thread, play, target, true)

	// The mirror must be GONE — not the old point, and above all not the requested one.
	if p, ok := ps.last(); ok {
		t.Fatalf("the mirror survived an unlanded move: %+v", p)
	}
	if why := ps.mirrorLoss(); why != mirrorLostUnproven {
		t.Fatalf("mirror loss reason = %q, want %q", why, mirrorLostUnproven)
	}
	// ...and a following BARE click (the low-level injectInput path) refuses, with a
	// message that explains what happened rather than reading like a bug.
	msg := bareClickRefusal(ps.mirrorLoss())
	if !strings.HasPrefix(msg, "refused:") {
		t.Fatalf("a bare click after an unlanded move must be refused: %q", msg)
	}
	for _, want := range []string{"did not provably land", "desktop_click"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal should mention %q: %q", want, msg)
		}
	}
}

func TestNormalMoveThenClickStillCommitsTheMirror(t *testing.T) {
	auth := selfAuthority(t)
	ps := newPointerState()
	const thread = "t-normal"
	ps.setLast(thread, point{100, 900})

	// An ordinary in-bounds move: the UI plays every op and reports landing on the target.
	to := point{1400, 600}
	ops := moveBatch(point{100, 900}, to)
	res := okReply(to.X, to.Y, len(ops))
	play := pointerPlay{played: true, landed: opsLandedAsAimed(ops, res)}
	if !play.landed {
		t.Fatal("a fully played batch that ends on the requested point must count as landed")
	}
	ps.commitPointer(thread, play, to, true)
	if p, ok := ps.last(); !ok || p != to {
		t.Fatalf("mirror = %+v/%v, want %+v", p, ok, to)
	}
	if why := ps.mirrorLoss(); why != "" {
		t.Fatalf("a landed move must clear the loss mark, got %q", why)
	}
	// The bare-click guard now has a position to check, and it is outside our windows.
	if auth.IsSelfPoint(to.X, to.Y, akWindowRects()) {
		t.Fatal("precondition: the target must be outside Agent Kate's windows")
	}

	// The click that follows lands on the same point and keeps the mirror there.
	clicks := clickOps(moveBatch(to, to), 0x110, 1, 30)
	cres := okReply(to.X, to.Y, len(clicks))
	cplay := pointerPlay{played: true, landed: opsLandedAsAimed(clicks, cres)}
	ps.commitPointer(thread, cplay, to, true)
	if p, ok := ps.last(); !ok || p != to {
		t.Fatalf("mirror after the click = %+v/%v, want %+v", p, ok, to)
	}
}

func TestMidPlayFailureInvalidatesTheMirror(t *testing.T) {
	ps := newPointerState()
	const thread = "t-midplay"
	ps.setLast(thread, point{200, 200})

	// The portal errored (session torn down, focus-abort, timeout) after the ops were
	// handed over: the cursor is stranded somewhere along an interpolated path that is
	// ALLOWED to cross Agent Kate's windows, because motion alone is harmless. Leaving the
	// pre-move mirror standing would clear the next bare click against a fiction.
	ps.commitPointer(thread, pointerPlay{played: true, landed: false}, point{1500, 1500}, true)
	if p, ok := ps.last(); ok {
		t.Fatalf("a mid-play failure must destroy the mirror, got %+v", p)
	}
	if why := ps.mirrorLoss(); why != mirrorLostUnproven {
		t.Fatalf("mirror loss reason = %q, want %q", why, mirrorLostUnproven)
	}
}

func TestRefusalBeforePlaybackLeavesTheMirrorAlone(t *testing.T) {
	ps := newPointerState()
	const thread = "t-denied"
	ps.setLast(thread, point{640, 480})
	// Consent denied / self-target guard refused: the ops never reached the UI, so nothing
	// moved. Destroying the mirror here would be fail-closed theatre that costs the agent a
	// re-established position for an action the desktop never saw.
	ps.commitPointer(thread, pointerPlay{}, point{10, 10}, true)
	if p, ok := ps.last(); !ok || p != (point{640, 480}) {
		t.Fatalf("mirror = %+v/%v, want the untouched (640,480)", p, ok)
	}
}

func TestReplyWithoutOutcomeFieldsFailsClosed(t *testing.T) {
	// A UI that does not report an outcome (an older build, or a reply that lost the
	// fields) must not be taken as proof. PtrKnown=false is the fail-closed default.
	ops := moveBatch(point{0, 0}, point{500, 500})
	if opsLandedAsAimed(ops, kde.PortalResult{OK: true, Kind: "inject"}) {
		t.Fatal("a reply with no pointer evidence must not count as landed")
	}
	// A reply that names a DIFFERENT point than the one asked for is likewise no proof —
	// this is the case where the UI landed on a clamped/other position.
	if opsLandedAsAimed(ops, okReply(499, 500, len(ops))) {
		t.Fatal("landing on another point must not count as landing on the requested one")
	}
}

func TestBatchesWithNoAbsoluteMoveHaveNothingToProve(t *testing.T) {
	// Keystrokes, bare buttons and a scroll at the current cursor never claim to move the
	// pointer to a point, so "did it land" is vacuously true — provided nothing was dropped.
	bare := clickOps(nil, 0x110, 1, 0)
	if !opsLandedAsAimed(bare, kde.PortalResult{OK: true, OpsApplied: len(bare)}) {
		t.Fatal("a batch with no absolute move must not be forced to prove a landing")
	}
	if opsLandedAsAimed(bare, droppedReply(1, 1)) {
		t.Fatal("a dropped op must fail the check even with no absolute move in the batch")
	}
}

func TestLastAbsMoveIsTheExactTargetEvenWithHumanJitter(t *testing.T) {
	// The check compares the UI's report against the batch's LAST move op, so that op must
	// be the exact target: expandMove jitters the path but always lands clean.
	from, to := point{40, 40}, point{1200, 800}
	ops := expandMove(from, true, to, PointerProfile{Speed: 900, Accuracy: 0.2}, rand.New(rand.NewSource(7)))
	got, ok := lastAbsMove(ops)
	if !ok || got != to {
		t.Fatalf("lastAbsMove = %+v/%v, want %+v", got, ok, to)
	}
	// A drag ends with the release AFTER the final move; the target is still found.
	ps := newPointerState()
	ps.setLast("t", from)
	drag := ps.dragOps(from, to, PointerProfile{Speed: 900, Accuracy: 1}, rand.New(rand.NewSource(7)))
	if got, ok := lastAbsMove(drag); !ok || got != to {
		t.Fatalf("lastAbsMove(drag) = %+v/%v, want %+v", got, ok, to)
	}
	if _, ok := lastAbsMove(nil); ok {
		t.Fatal("an empty batch has no absolute move")
	}
}

// --- MED: relative accumulation must be proven per SCREEN, not against the union -------

func TestRelativeMoveIntoMultiMonitorDeadSpaceInvalidates(t *testing.T) {
	// Two staggered screens; (2500,200) is inside their union but on neither screen, so the
	// compositor can never park the cursor there. Accepting it would leave the mirror
	// pointing at a place the cursor is not — the exact state the mirror must never hold.
	layout := kde.DesktopLayout{
		Union: kde.DesktopRect{X: 0, Y: 0, Width: 3200, Height: 1824},
		Screens: []kde.DesktopRect{
			{X: 0, Y: 0, Width: 1920, Height: 1080},
			{X: 1920, Y: 800, Width: 1280, Height: 1024},
		},
	}
	ps := newPointerState()
	const thread = "t-deadspace"
	ps.setLast(thread, point{1000, 200})
	if _, known := ps.applyRelative(thread, 1500, 0, layout); known {
		t.Fatal("a nudge into inter-screen dead space must invalidate the mirror")
	}
	if _, ok := ps.last(); ok {
		t.Fatal("the mirror must not survive a nudge the compositor could not honour")
	}
	if why := ps.mirrorLoss(); why != mirrorLostRelative {
		t.Fatalf("mirror loss reason = %q, want %q", why, mirrorLostRelative)
	}

	// The same-sized nudge that ends ON the second screen still accumulates normally.
	ps.setLast(thread, point{1000, 900})
	got, known := ps.applyRelative(thread, 1500, 0, layout)
	if !known || got != (point{2500, 900}) {
		t.Fatalf("an on-screen nudge must accumulate: got %+v/%v, want (2500,900)", got, known)
	}
}

func TestTimelineRelativeNudgeUsesPerScreenContainment(t *testing.T) {
	// The timeline compiler carries the same rule (it does its own accumulation): a nudge
	// into dead space marks RelLost, and a bare button afterwards refuses the whole script.
	layout := kde.DesktopLayout{
		Union: kde.DesktopRect{X: 0, Y: 0, Width: 3200, Height: 1824},
		Screens: []kde.DesktopRect{
			{X: 0, Y: 0, Width: 1920, Height: 1080},
			{X: 1920, Y: 800, Width: 1280, Height: 1024},
		},
	}
	a, b := 0, 20
	script := timelineScript{
		Events: []timelineEvent{
			{Type: "move_rel", DX: 1500, DY: 0, AtMs: &a},
			{Type: "button", Button: "left", AtMs: &b},
		},
		Bounds: layout,
	}
	_, err := buildTimelineOps(script, point{1000, 200}, true, trivialProfile(), fixedRNG())
	if err == nil {
		t.Fatal("a bare button after a nudge into dead space must refuse the script")
	}
	if !strings.Contains(err.Error(), "relative nudge") {
		t.Fatalf("the refusal should name the cause: %v", err)
	}
}
