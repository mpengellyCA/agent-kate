package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"agentkate/internal/cowork"
	"agentkate/internal/ipc"
)

// --- F25/F26 wiring, third report: the guard is derived from the ops, per handler --------
//
// The gap this closes: every previous round pinned the MECHANISM (the section, the mirror,
// the compiler invariant) and left the WIRING to a source scan, or — worse — to a test that
// re-implemented the handler's guard inside itself. So the verifier could delete
// cowork.injectInput's bare-click guard, and then runPointerAction's Seed re-proof AND its
// geometric guard (covering movePointer, movePointerRelative, pointerClick, scroll and
// pointerDrag), with the whole suite still green.
//
// There is no per-handler guard to delete now. Each handler hands the section the OPS IT IS
// ABOUT TO RELEASE and the section derives, from those ops, every point they will act at
// (effectPoints) — so "this handler forgot to guard" is no longer expressible. What is left
// to pin is that each handler's op SHAPE is guarded the way that handler needs, and that is
// what these do: they build the stream with the very builders the handlers call
// (buildInjectOps, moveOps, relMoveOps, clickOps, scrollOp, dragOps, buildTimelineOps) and
// assert the OUTCOME of the real decision — refused with nothing released, or fired.
//
// Deleting the geometric guard, the seed re-proof, or the btn/wheel case of the derivation
// fails every "refused" case below; deleting the empty-points shortcut fails every "fires"
// case. The mutations are listed in the plan-29 round report.

// sectionOutcome runs the real guard→fire decision over one handler's ops and reports
// whether anything reached the portal.
type sectionOutcome struct {
	err   error
	fired int32
}

func runSection(t *testing.T, ps *pointerState, geom cursorGeometry, ops []map[string]any, seed *point) *sectionOutcome {
	t.Helper()
	out := &sectionOutcome{}
	out.err = cursorAction{
		Geometry: geom,
		Ops:      ops,
		Seed:     seed,
		Bounds:   testBounds(),
		Fire: func(*cursorHold, []map[string]any) (bool, error) {
			atomic.AddInt32(&out.fired, 1)
			return true, nil
		},
	}.run(context.Background(), ps, "t-wiring")
	return out
}

func mustRefuse(t *testing.T, what string, out *sectionOutcome) {
	t.Helper()
	if out.err == nil {
		t.Fatalf("%s: aimed at an Agent Kate window and was NOT refused", what)
	}
	if !strings.Contains(out.err.Error(), "Agent Kate") {
		t.Fatalf("%s: refused for the wrong reason: %v", what, out.err)
	}
	if n := atomic.LoadInt32(&out.fired); n != 0 {
		t.Fatalf("%s: the refused action still reached the portal (%d times)", what, n)
	}
}

func mustFire(t *testing.T, what string, out *sectionOutcome) {
	t.Helper()
	if out.err != nil {
		t.Fatalf("%s: a legitimate action must not be refused: %v", what, out.err)
	}
	if n := atomic.LoadInt32(&out.fired); n != 1 {
		t.Fatalf("%s: expected exactly one dispatch, got %d", what, n)
	}
}

// inAK is a point inside the fixture's Agent Kate window; outside is not.
var (
	inAK    = point{300, 200}
	outside = point{1800, 900}
)

// cowork.injectInput — a bare button at the cursor. Its guard point is the mirror, so the
// mirror is BOTH the derived target and the seed the section re-proves.
func TestWiringInjectInputBareClick(t *testing.T) {
	geom := akGeometry(t)
	ops, _, err := buildInjectOps([]injectEvent{{Type: "button", Button: "left"}})
	if err != nil {
		t.Fatalf("buildInjectOps: %v", err)
	}

	ps := newPointerState()
	ps.setLast("t-wiring", inAK)
	seed := inAK
	mustRefuse(t, "injectInput bare click over our own window", runSection(t, ps, geom, ops, &seed))

	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	seed = outside
	mustFire(t, "injectInput bare click on an ordinary window", runSection(t, ps, geom, ops, &seed))

	// A keys-only batch acts nowhere and needs no seed — it must not be blocked by the
	// pointer guard (it takes no section in the handler at all; this pins the derivation).
	keys, _, err := buildInjectOps([]injectEvent{{Type: "key", Key: "a"}})
	if err != nil {
		t.Fatalf("buildInjectOps(keys): %v", err)
	}
	pts, ok := effectPoints(keys, inAK, true, testBounds())
	if !ok || len(pts) != 0 {
		t.Fatalf("a keys-only batch acts at no pointer position, got %v ok=%v", pts, ok)
	}
	if opsNeedSeed(keys) {
		t.Fatal("a keys-only batch does not fire at the cursor, so it needs no seed")
	}
}

// cowork.movePointer — motion only. Deliberately unguarded: passing over our windows has no
// side effect. The property to pin is that this is a consequence of the ops, not a handler
// opting out.
func TestWiringMovePointer(t *testing.T) {
	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	ops := ps.moveOps(inAK.X, inAK.Y, defaultPointerProfile(), fixedRNG())
	pts, ok := effectPoints(ops, outside, true, testBounds())
	if !ok || len(pts) != 0 {
		t.Fatalf("a pure move acts nowhere, got %v ok=%v", pts, ok)
	}
	mustFire(t, "movePointer across our own window", runSection(t, ps, akGeometry(t), ops, nil))
}

// cowork.movePointerRelative — the same, for raw deltas.
func TestWiringMovePointerRelative(t *testing.T) {
	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	ops := relMoveOps(-400, -300, 4)
	if opsNeedSeed(ops) {
		t.Fatal("relative motion alone acts nowhere, so it must not demand a seed — that would break mouse-look from an unknown position")
	}
	mustFire(t, "movePointerRelative", runSection(t, ps, akGeometry(t), ops, nil))
}

// cowork.pointerClick — a positioned click names its own target.
func TestWiringPointerClick(t *testing.T) {
	geom := akGeometry(t)
	prof := defaultPointerProfile()

	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	onUs := clickOps(ps.moveOps(inAK.X, inAK.Y, prof, fixedRNG()), 0x110, 1, prof.SettleMs)
	mustRefuse(t, "pointerClick on our own window", runSection(t, ps, geom, onUs, nil))

	ps = newPointerState()
	ps.setLast("t-wiring", inAK)
	away := clickOps(ps.moveOps(outside.X, outside.Y, prof, fixedRNG()), 0x110, 1, prof.SettleMs)
	mustFire(t, "pointerClick on an ordinary window", runSection(t, ps, geom, away, nil))

	// Every repeat of a multi-click is guarded, not just the first.
	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	triple := clickOps(ps.moveOps(inAK.X, inAK.Y, prof, fixedRNG()), 0x110, 3, prof.SettleMs)
	mustRefuse(t, "pointerClick count=3 on our own window", runSection(t, ps, geom, triple, nil))
}

// cowork.scroll — positioned, and bare at the cursor (whose point is the mirror).
func TestWiringScroll(t *testing.T) {
	geom := akGeometry(t)

	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	positioned := append(ps.moveOps(inAK.X, inAK.Y, defaultPointerProfile(), fixedRNG()), scrollOp(0, 3))
	mustRefuse(t, "positioned scroll over our own window", runSection(t, ps, geom, positioned, nil))

	ps = newPointerState()
	ps.setLast("t-wiring", inAK)
	seed := inAK
	mustRefuse(t, "bare scroll while the shared cursor sits on us", runSection(t, ps, geom, []map[string]any{scrollOp(0, 3)}, &seed))

	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	seed = outside
	mustFire(t, "bare scroll at a clean position", runSection(t, ps, geom, []map[string]any{scrollOp(0, 3)}, &seed))
}

// cowork.pointerDrag — BOTH endpoints act (press at from, release at to).
func TestWiringPointerDrag(t *testing.T) {
	geom := akGeometry(t)
	prof := defaultPointerProfile()

	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	intoUs := ps.dragOps(outside, inAK, prof, fixedRNG())
	mustRefuse(t, "drag RELEASING on our own window", runSection(t, ps, geom, intoUs, nil))

	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	outOfUs := ps.dragOps(inAK, outside, prof, fixedRNG())
	mustRefuse(t, "drag GRABBING on our own window", runSection(t, ps, geom, outOfUs, nil))

	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	clean := ps.dragOps(point{1000, 800}, outside, prof, fixedRNG())
	mustFire(t, "drag between two ordinary points", runSection(t, ps, geom, clean, nil))
}

// cowork.playInput — the compiled timeline. Its bare button after mouse-look is the case
// that needs the desktop layout to be accounted for at all.
func TestWiringPlayInput(t *testing.T) {
	geom := akGeometry(t)

	onUs, err := buildTimelineOps(timelineScript{Events: []timelineEvent{
		{Type: "click", Button: "left", X: inAK.X, Y: inAK.Y},
	}, Bounds: testBounds()}, outside, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	mustRefuse(t, "playInput click on our own window", runSection(t, ps, geom, onUs.Ops, nil))

	// Mouse-look that walks onto our window, then a bare button: the derivation must follow
	// the nudges, or the button is cleared against where the cursor no longer is (audit F3).
	walk, err := buildTimelineOps(timelineScript{Events: []timelineEvent{
		{Type: "move", X: outside.X, Y: outside.Y},
		{Type: "move_rel", DX: inAK.X - outside.X, DY: inAK.Y - outside.Y},
		{Type: "button", Button: "left"},
	}, Bounds: testBounds()}, outside, true, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("build(walk): %v", err)
	}
	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	mustRefuse(t, "playInput nudging onto us then clicking", runSection(t, ps, geom, walk.Ops, nil))
}

// cowork.pointerClickElement — the a11y-addressed click resolves to a coordinate and is
// guarded like any other.
func TestWiringPointerClickElement(t *testing.T) {
	geom := akGeometry(t)
	prof := defaultPointerProfile()
	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	ops := clickOps(ps.moveOps(inAK.X, inAK.Y, prof, fixedRNG()), 0x110, 1, prof.SettleMs)
	mustRefuse(t, "pointerClickElement resolving into our own window", runSection(t, ps, geom, ops, nil))
}

// The fail-closed corners of the shared decision. Each is a refusal, never a pass.
func TestSectionFailsClosedWithoutEvidence(t *testing.T) {
	bare := []map[string]any{btnOp(0x110, 1), btnOp(0x110, 0)}

	// No seed for a stream that fires at the cursor: the section cannot know which position
	// the human's prompt and the pre-consent checks were about.
	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	out := runSection(t, ps, akGeometry(t), bare, nil)
	if out.err == nil || atomic.LoadInt32(&out.fired) != 0 {
		t.Fatalf("a bare click with no seed must be refused, got err=%v fired=%d", out.err, out.fired)
	}
	if !strings.Contains(out.err.Error(), "no seed") {
		t.Fatalf("the refusal should name the missing evidence: %v", out.err)
	}

	// No mirror at all: nothing to derive the landing point from.
	ps = newPointerState()
	seed := outside
	out = runSection(t, ps, akGeometry(t), bare, &seed)
	if out.err == nil || atomic.LoadInt32(&out.fired) != 0 {
		t.Fatalf("a bare click with no mirror must be refused, got err=%v fired=%d", out.err, out.fired)
	}

	// No geometry source: an unverifiable target is never clicked.
	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	out = runSection(t, ps, nil, bare, &seed)
	if out.err == nil || atomic.LoadInt32(&out.fired) != 0 {
		t.Fatalf("a click with no window geometry must be refused, got err=%v fired=%d", out.err, out.fired)
	}

	// KWin unreadable: same answer.
	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	broken := akGeometry(t)
	broken.err = context.DeadlineExceeded
	out = runSection(t, ps, broken, bare, &seed)
	if out.err == nil || atomic.LoadInt32(&out.fired) != 0 {
		t.Fatalf("an unreadable window list must refuse, got err=%v fired=%d", out.err, out.fired)
	}

	// A relative nudge that walked off the known desktop leaves the button's position
	// unaccountable — refuse rather than guard a fiction.
	ps = newPointerState()
	ps.setLast("t-wiring", outside)
	strayed := []map[string]any{relMoveOp(9000, 0), btnOp(0x110, 1), btnOp(0x110, 0)}
	out = runSection(t, ps, akGeometry(t), strayed, nil)
	if out.err == nil || atomic.LoadInt32(&out.fired) != 0 {
		t.Fatalf("a button after an unaccountable nudge must be refused, got err=%v fired=%d", out.err, out.fired)
	}
}

// --- the family is enumerable with or without a session bus -----------------------------
//
// registerCoworkHandlers used to return early when the desktop service was absent, so on
// every machine without a KDE session bus — every CI runner, every test process — the
// cowork.* methods were not in the registry at all. The authorisation inventory
// (handlers_inventory_test.go) enumerates the registry, so it could not see them, and a new
// UNGATED cowork handler could not break the build.
func TestCoworkHandlersAreEnumerableWithoutASessionBus(t *testing.T) {
	names := func(cw *cowork.Service) []string {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		srv := ipc.NewServer(filepath.Join(t.TempDir(), "cw.sock"), log)
		registerCoworkHandlers(handlerDeps{srv: srv, cowork: cw, log: log})
		var out []string
		for _, m := range srv.Methods() {
			if strings.HasPrefix(m, "cowork.") {
				out = append(out, m)
			}
		}
		return out
	}
	// selfService builds a real Service whose kde client is nil — "the bus is there, the
	// desktop is not", the live-handler case.
	live := names(selfService(t))
	absent := names(nil)
	if !reflect.DeepEqual(live, absent) {
		t.Fatalf("the same method names must be registered either way:\n  with a service: %v\n  without:        %v", live, absent)
	}
	if len(live) < 25 {
		t.Fatalf("only %d cowork methods registered; the family is two dozen and more — the registrar is not seeing them all", len(live))
	}
}

// The section refuses to dispatch a stream it was not given. This is what makes blanking a
// handler's Ops a REFUSAL rather than an unguarded dispatch: what is guarded and what is
// released are the same slice (cursorAction.Fire is handed the ops; it does not carry its
// own copy), so there is no arrangement in which the check sees one list and the portal
// gets another.
func TestSectionRefusesToDispatchOpsItWasNotGiven(t *testing.T) {
	ps := newPointerState()
	ps.setLast("t-wiring", outside)
	var got [][]map[string]any
	err := cursorAction{
		Geometry: akGeometry(t),
		Fire: func(_ *cursorHold, ops []map[string]any) (bool, error) {
			got = append(got, ops)
			return true, nil
		},
	}.run(context.Background(), ps, "t-wiring")
	if err == nil {
		t.Fatal("an action with nothing to check must not dispatch")
	}
	if len(got) != 0 {
		t.Fatalf("nothing may reach the portal, got %v", got)
	}

	// And what Fire receives is exactly what was checked.
	want := clickOps([]map[string]any{{"t": "move", "x": outside.X, "y": outside.Y}}, 0x110, 1, 0)
	got = nil
	if err := (cursorAction{
		Geometry: akGeometry(t),
		Ops:      want,
		Fire: func(_ *cursorHold, ops []map[string]any) (bool, error) {
			got = append(got, ops)
			return true, nil
		},
	}).run(context.Background(), ps, "t-wiring"); err != nil {
		t.Fatalf("a clean click must fire: %v", err)
	}
	if len(got) != 1 || len(got[0]) != len(want) {
		t.Fatalf("Fire must receive the checked stream, got %v", got)
	}
}

// --- the UI-only gates answer with the UI-only code, not a Cowork denial ----------------
//
// The ten cowork gates (getPolicy, setPolicy, killSwitch, listGrants, listAudit,
// revokeGrant, respondGrant, requestGrant, portalResult, setPointerBounds) — and the two in
// cowork_enable.go, which share this function — used to answer codeCoworkDenied (-32010).
// That is the CONSENT code: it says the user, or a policy, refused this action, and it
// invites a client to treat the refusal as something a grant could change. A structural
// "only the human's own window may drive this" is codeUIOnly (-32012, handlers.go).
//
// The sentence is asserted too, because it is unchanged and the UI matches on it — but the
// sentence alone is not the test: the previous shape had exactly the same words.
func TestCoworkUIOnlyGateUsesTheUIOnlyCode(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := handlerDeps{srv: ipc.NewServer(filepath.Join(t.TempDir(), "ui.sock"), log), log: log}

	err := requireUI(d, context.Background())
	if err == nil {
		t.Fatal("a caller that never identified as the UI must be refused")
	}
	rpc, ok := err.(*ipc.RPCError)
	if !ok {
		t.Fatalf("the refusal must be an RPC error, got %T", err)
	}
	if rpc.Code == codeCoworkDenied {
		t.Fatalf("a UI-only refusal is not a Cowork consent decision — code %d is the consent code", rpc.Code)
	}
	if rpc.Code != codeUIOnly {
		t.Fatalf("a UI-only refusal must use codeUIOnly (%d), got %d", codeUIOnly, rpc.Code)
	}
	if rpc.Message != uiOnlyRefusal {
		t.Fatalf("the sentence the UI matches on must not move: %q", rpc.Message)
	}
	// The three codes in this shared space stay distinct.
	if codeCoworkDenied == codeCoworkBusy || codeCoworkBusy == codeUIOnly || codeCoworkDenied == codeUIOnly {
		t.Fatalf("the wire codes must stay distinct: denied=%d busy=%d uiOnly=%d",
			codeCoworkDenied, codeCoworkBusy, codeUIOnly)
	}
}

// samePointSet backs the compiler's cross-check of its own guard points against the ones
// derived from the compiled stream. Order and repetition are not meaningful (a repeat fires
// many times at one place), membership is.
func TestSamePointSet(t *testing.T) {
	a := []point{{1, 1}, {2, 2}}
	if !samePointSet(a, []point{{2, 2}, {1, 1}, {1, 1}}) {
		t.Fatal("order and repetition must not matter")
	}
	if samePointSet(a, []point{{1, 1}}) {
		t.Fatal("a missing point is a difference")
	}
	if samePointSet(a, []point{{1, 1}, {2, 2}, {3, 3}}) {
		t.Fatal("an extra point is a difference")
	}
	if !samePointSet(nil, nil) {
		t.Fatal("two empty lists agree")
	}
	if samePointSet(nil, a) {
		t.Fatal("empty and non-empty do not agree")
	}
}
