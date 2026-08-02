package main

import (
	"strings"
	"testing"

	"agentkate/internal/cowork"
	"agentkate/internal/kde"
)

// --- F3 (pointer half): relative motion may not strand the position mirror ------------
//
// The attack these cover: move the pointer somewhere harmless with the ABSOLUTE tool (so
// the mirror says "safe"), then walk the REAL cursor onto an Agent Kate window with
// relative nudges, then fire a bare click. The bare-click guard is geometric and reads the
// mirror, so a mirror that does not move with the cursor clears a click that lands on our
// own consent dialog.

// akWindowRects is the live-geometry view guardPointerTargets builds from KWin: one Agent
// Kate window at (100,100)-(500,400), one unrelated window elsewhere.
func akWindowRects() []cowork.WindowRect {
	return []cowork.WindowRect{
		{X: 100, Y: 100, W: 400, H: 300, PID: 4242, ResourceClass: "org.kde.agentkate"},
		{X: 900, Y: 700, W: 600, H: 300, PID: 7, ResourceClass: "firefox"},
	}
}

// testBounds is a single 1920x1080 screen: union and per-screen containment agree.
func testBounds() kde.DesktopLayout {
	r := kde.DesktopRect{X: 0, Y: 0, Width: 1920, Height: 1080}
	return kde.DesktopLayout{Union: r, Screens: []kde.DesktopRect{r}}
}

func TestRelativeWalkOntoSelfWindowIsSeenByTheGeometricGuard(t *testing.T) {
	auth := selfAuthority(t)
	rects := akWindowRects()
	ps := newPointerState()
	const thread = "t-walk"

	// Park the cursor somewhere provably safe with the absolute tool.
	ps.setLast(thread, point{1800, 900})
	if start, ok := ps.last(); !ok || auth.IsSelfPoint(start.X, start.Y, rects) {
		t.Fatalf("precondition: the start position must be known and outside Agent Kate")
	}

	// Walk the real cursor onto the Agent Kate window with a relative nudge. Before the
	// fix the mirror stayed at (1800,900) and the guard cleared the click.
	got, known := ps.applyRelative(thread, -1500, -700, testBounds())
	if !known {
		t.Fatal("an in-bounds relative move must keep the position known, not drop it")
	}
	if got != (point{300, 200}) {
		t.Fatalf("mirror did not accumulate the delta: got %+v, want (300,200)", got)
	}
	mirror, ok := ps.last()
	if !ok || mirror != (point{300, 200}) {
		t.Fatalf("stored mirror is %v/%v, want (300,200)", mirror, ok)
	}
	// This is the assertion that matters: the point the bare-click guard now checks is
	// inside an Agent Kate window, so guardPointerTargets refuses.
	if !auth.IsSelfPoint(mirror.X, mirror.Y, rects) {
		t.Fatal("a bare click after the relative walk must be refused — the mirror is not tracking the cursor")
	}
}

func TestLegitimateRelativeMoveThenActionElsewhereStillWorks(t *testing.T) {
	auth := selfAuthority(t)
	rects := akWindowRects()
	ps := newPointerState()
	const thread = "t-game"

	ps.setLast(thread, point{1000, 900})
	// Mouse-look sized nudges: the position stays known and stays outside our windows,
	// so a bare click (shooting, in the gaming case) still goes through.
	for i := 0; i < 4; i++ {
		if _, known := ps.applyRelative(thread, 40, -10, testBounds()); !known {
			t.Fatalf("nudge %d must keep a known position", i)
		}
	}
	mirror, ok := ps.last()
	if !ok || mirror != (point{1160, 860}) {
		t.Fatalf("mirror after four nudges is %v/%v, want (1160,860)", mirror, ok)
	}
	if auth.IsSelfPoint(mirror.X, mirror.Y, rects) {
		t.Fatal("a legitimate relative move away from Agent Kate must remain clickable")
	}
	if ps.mirrorLoss() == mirrorLostRelative {
		t.Fatal("an accounted-for relative move must not mark the mirror as lost")
	}
}

func TestRelativeMoveIntoTheScreenEdgeFailsClosed(t *testing.T) {
	ps := newPointerState()
	const thread = "t-edge"
	ps.setLast(thread, point{10, 10})

	// The compositor clamps at the edge, so the accumulated (-490,10) is fiction. Refuse
	// rather than trust it — and refuse rather than leave the pre-nudge position standing.
	if _, known := ps.applyRelative(thread, -500, 0, testBounds()); known {
		t.Fatal("a delta that runs off the desktop must not leave a known position")
	}
	if _, ok := ps.last(); ok {
		t.Fatal("the mirror must be destroyed, not left at the pre-nudge position")
	}
	if !(ps.mirrorLoss() == mirrorLostRelative) {
		t.Fatal("the refusal must be attributable to the relative move")
	}
	if msg := bareClickRefusal(mirrorLostRelative); !strings.Contains(strings.ToLower(msg), "relative") {
		t.Fatalf("the refusal should name the cause: %q", msg)
	}

	// An absolute move is the documented way back.
	ps.setLast(thread, point{400, 400})
	if p, ok := ps.last(); !ok || p != (point{400, 400}) {
		t.Fatal("an absolute move must re-establish a known position")
	}
	if ps.mirrorLoss() == mirrorLostRelative {
		t.Fatal("an absolute move must clear the relative-loss mark")
	}
}

func TestRelativeMoveWithUnknownDesktopBoundsFailsClosed(t *testing.T) {
	ps := newPointerState()
	const thread = "t-nobounds"
	ps.setLast(thread, point{500, 500})
	// KWin could not tell us the desktop extent: accumulation cannot be bounded, so the
	// mirror goes rather than being trusted.
	if _, known := ps.applyRelative(thread, 10, 10, kde.DesktopLayout{}); known {
		t.Fatal("unknown desktop bounds must invalidate the mirror, not accumulate blindly")
	}
	if _, ok := ps.last(); ok {
		t.Fatal("the mirror must not survive an unbounded relative move")
	}
}

func TestRelativeMoveWithNoKnownStartStaysUnknown(t *testing.T) {
	ps := newPointerState()
	const thread = "t-cold"
	if _, known := ps.applyRelative(thread, 5, 5, testBounds()); known {
		t.Fatal("a relative move cannot establish a position out of nothing")
	}
	if _, ok := ps.last(); ok {
		t.Fatal("no position may be invented")
	}
	if !(ps.mirrorLoss() == mirrorLostRelative) {
		t.Fatal("the refusal must still be attributed to relative motion")
	}
}

// --- the same rule inside a compiled timeline ----------------------------------------

func TestTimelineRelativeNudgeMovesTheGuardPoint(t *testing.T) {
	auth := selfAuthority(t)
	rects := akWindowRects()
	a, b := 0, 20
	script := timelineScript{
		Events: []timelineEvent{
			{Type: "move_rel", DX: -1500, DY: -700, AtMs: &a},
			{Type: "button", Button: "left", AtMs: &b},
		},
		Bounds: testBounds(),
	}
	plan, err := buildTimelineOps(script, point{1800, 900}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(plan.GuardPts) != 1 {
		t.Fatalf("want one guard point for the bare button, got %v", plan.GuardPts)
	}
	if plan.GuardPts[0] != (point{300, 200}) {
		t.Fatalf("the bare button must be guarded at the ACCUMULATED position, got %+v", plan.GuardPts[0])
	}
	if !auth.IsSelfPoint(plan.GuardPts[0].X, plan.GuardPts[0].Y, rects) {
		t.Fatal("a script that nudges onto Agent Kate and clicks must be refused by the submit-time guard")
	}
	if !plan.HaveFinal || plan.FinalPos != (point{300, 200}) {
		t.Fatalf("the script's final position must be the accumulated one, got %+v/%v", plan.FinalPos, plan.HaveFinal)
	}
}

func TestTimelineRelativeNudgeOffScreenRefusesTheBareButton(t *testing.T) {
	a, b := 0, 20
	script := timelineScript{
		Events: []timelineEvent{
			{Type: "move_rel", DX: -500, DY: 0, AtMs: &a},
			{Type: "button", Button: "left", AtMs: &b},
		},
		Bounds: testBounds(),
	}
	_, err := buildTimelineOps(script, point{10, 10}, true, trivialProfile(), fixedRNG())
	if err == nil {
		t.Fatal("a bare button after an unaccountable nudge must be refused")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "relative") {
		t.Fatalf("the refusal should name the relative nudge: %v", err)
	}
}

func TestTimelineRelativeNudgeWithoutBoundsInvalidatesTheMirror(t *testing.T) {
	a := 0
	// No Bounds → unknown desktop extent → the position must not survive the script.
	script := timelineScript{Events: []timelineEvent{{Type: "move_rel", DX: 10, DY: 10, AtMs: &a}}}
	plan, err := buildTimelineOps(script, point{500, 500}, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.HaveFinal {
		t.Fatal("an unbounded nudge must not commit a final position")
	}
	if !plan.RelLost {
		t.Fatal("the handler must be told to destroy the mirror (RelLost)")
	}
}

// --- F3 (keyboard residual): a window with NO identity evidence is not a target -------

func TestResolveInjectTargetRefusesWindowWithNoIdentityEvidence(t *testing.T) {
	auth := selfAuthority(t)
	// KWin reports neither an owning pid nor a resource class. IsSelfWindow returning
	// false here means "no evidence", not "verified other" — exactly the shape an
	// unidentifiable Agent Kate dialog takes.
	blank := []kde.Window{{InternalID: "w-blank", Caption: "", ResourceClass: "", PID: 0, Active: true}}
	if _, err := resolveInjectTargetFrom(auth, blank, nil, ""); err == nil {
		t.Fatal("a focused window with no pid and no class must not be a keyboard target")
	}
	if _, err := resolveInjectTargetFrom(auth, blank, nil, "w-blank"); err == nil {
		t.Fatal("naming the unidentifiable window explicitly must not help")
	}

	// A window with no id cannot be focus-verified after consent, so it is refused too.
	noID := []kde.Window{{InternalID: "", Caption: "Docs", ResourceClass: "firefox", PID: 7, Active: true}}
	if _, err := resolveInjectTargetFrom(auth, noID, nil, ""); err == nil {
		t.Fatal("a window with no internal id cannot be re-verified after consent and must be refused")
	}

	// Positive controls: EITHER piece of evidence is enough for a foreign window.
	classOnly := []kde.Window{{InternalID: "w-c", Caption: "Docs", ResourceClass: "firefox", PID: 0, Active: true}}
	if _, err := resolveInjectTargetFrom(auth, classOnly, nil, ""); err != nil {
		t.Fatalf("a foreign window identified by class must still be typeable: %v", err)
	}
	pidOnly := []kde.Window{{InternalID: "w-p", Caption: "Docs", ResourceClass: "", PID: 7, Active: true}}
	if _, err := resolveInjectTargetFrom(auth, pidOnly, nil, ""); err != nil {
		t.Fatalf("a foreign window identified by pid must still be typeable: %v", err)
	}
}
