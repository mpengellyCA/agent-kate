package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentkate/internal/cowork"
	"agentkate/internal/ipc"
)

// --- F26 part 2: the guard→fire section over the SHARED cursor -------------------------
//
// These drive cursorAction.run — the decision every cursor-affecting handler routes
// through (cowork.movePointer/movePointerRelative/click/scroll/pointerDrag via
// runPointerAction, cowork.pointerClickElement, cowork.injectInput's bare click, and
// cowork.playInput) — and assert the OUTCOME: refused or fired, and whether anything
// reached the portal. The handler closures themselves need a live KDE bus and a UI portal,
// so the decision lives here where it can be exercised; the earlier tests in this area
// asserted on setters and on source text, which is why deleting the whole serialization
// left them green.

// fakeGeometry is a cursorGeometry over a fixed window list and the REAL self-identity
// matcher, so the production guard body (pointerTargetsClear) runs with no KDE session bus
// behind it. This is what lets these tests drive the decision the handlers drive, instead
// of a re-implementation of it.
type fakeGeometry struct {
	auth  *cowork.Authority
	rects []cowork.WindowRect
	err   error
	// before, when set, runs on every window read — used to observe the section.
	before func()
}

func (g *fakeGeometry) Windows() ([]cowork.WindowRect, error) {
	if g.before != nil {
		g.before()
	}
	if g.err != nil {
		return nil, g.err
	}
	return g.rects, nil
}

func (g *fakeGeometry) IsSelfPoint(x, y int, wins []cowork.WindowRect) bool {
	return g.auth.IsSelfPoint(x, y, wins)
}

// akGeometry is the standard fixture: one Agent Kate window at (100,100)-(500,400) and one
// unrelated window, matched by the production authority.
func akGeometry(t *testing.T) *fakeGeometry {
	t.Helper()
	return &fakeGeometry{auth: selfAuthority(t), rects: akWindowRects()}
}

// benignOps is a stream that acts nowhere — a pure move — for the tests whose subject is
// the SECTION rather than the guard. cursorAction.run refuses an action that dispatches
// without handing over the ops it is about to dispatch, so there is no such thing as a
// cursor action with nothing in it.
func benignOps() []map[string]any {
	return []map[string]any{{"t": "move", "x": 1800, "y": 900}}
}

// swapFireWait shortens the contention timeout for one test and restores it.
func swapFireWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := fireWaitMax
	fireWaitMax = d
	t.Cleanup(func() { fireWaitMax = old })
}

// holdSection enters the section on another goroutine and keeps it until the returned
// release is called. It reports back once it is really inside.
func holdSection(t *testing.T, ps *pointerState, thread string) (release func()) {
	t.Helper()
	inside := make(chan struct{})
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = cursorAction{
			Ops: benignOps(),
			Fire: func(*cursorHold, []map[string]any) (bool, error) {
				close(inside)
				<-done
				return true, nil
			},
		}.run(context.Background(), ps, thread)
	}()
	<-inside
	return func() {
		close(done)
		<-finished
	}
}

// The property: no other thread may touch the cursor between a guard and its fire. Eight
// threads run the full guard→fire sequence at once; if the section is not real, one
// action's guard interleaves with another's fire and `overlaps` is non-zero.
func TestCursorSectionSerializesGuardToFire(t *testing.T) {
	ps := newPointerState()
	var live, overlaps int32

	// The geometry read IS the guard's only I/O, so counting entries to it counts guards.
	geom := akGeometry(t)
	geom.before = func() {
		if atomic.AddInt32(&live, 1) != 1 {
			atomic.AddInt32(&overlaps, 1)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// A real positioned click, at a point outside every Agent Kate window.
	ops := clickOps([]map[string]any{{"t": "move", "x": 1800, "y": 900}}, 0x110, 1, 0)

	act := func(thread string) error {
		return cursorAction{
			Geometry: geom,
			Ops:      ops,
			Fire: func(h *cursorHold, _ []map[string]any) (bool, error) {
				if !h.valid() {
					return false, fmt.Errorf("ops were released without a live cursor hold")
				}
				time.Sleep(2 * time.Millisecond)
				if atomic.AddInt32(&live, -1) != 0 {
					atomic.AddInt32(&overlaps, 1)
				}
				return true, nil
			},
		}.run(context.Background(), ps, thread)
	}

	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) { errs <- act(fmt.Sprintf("thread-%d", i)) }(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("an uncontended pointer action must succeed: %v", err)
		}
	}
	if n := atomic.LoadInt32(&overlaps); n != 0 {
		t.Fatalf("%d interleavings between one action's guard and another's fire — with one shared cursor that is the F26 race", n)
	}
}

// A security fix that stalls the arena gets reverted, so it is not a fix: waiting for the
// section is bounded, and what comes back must not read like a policy refusal.
func TestCursorSectionRefusesRatherThanBlockingForever(t *testing.T) {
	swapFireWait(t, 60*time.Millisecond)
	ps := newPointerState()
	release := holdSection(t, ps, "thread-A")
	defer release()

	var fired int32
	start := time.Now()
	err := cursorAction{
		Ops:  benignOps(),
		Fire: func(*cursorHold, []map[string]any) (bool, error) { atomic.AddInt32(&fired, 1); return true, nil },
	}.run(context.Background(), ps, "thread-B")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a pointer action that could not take the shared cursor must come back with a reason, not proceed")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the wait must be bounded; it took %s", elapsed)
	}
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Fatalf("nothing may be released to the portal without the section, fired=%d", n)
	}
	// Contention, not a denial — an agent that reads "busy" as "forbidden" changes target
	// or gives up, when the right move is to retry the identical call.
	var busy cursorBusy
	if !errors.As(err, &busy) {
		t.Fatalf("a contended acquire must be a cursorBusy, got %T: %v", err, err)
	}
	var refusal cursorRefusal
	if errors.As(err, &refusal) {
		t.Fatal("contention must not be reported as a guard refusal")
	}
	rpc, ok := cursorActionError(err).(*ipc.RPCError)
	if !ok {
		t.Fatalf("cursorActionError must produce an RPC error, got %T", cursorActionError(err))
	}
	if rpc.Code != codeCoworkBusy {
		t.Fatalf("contention must use codeCoworkBusy (%d), got %d", codeCoworkBusy, rpc.Code)
	}
	if strings.Contains(strings.ToLower(rpc.Message), "refused") {
		t.Fatalf("the busy message must not read as a refusal: %q", rpc.Message)
	}
	for _, want := range []string{"busy", "another agent", "NOTHING WAS DONE", "try again"} {
		if !strings.Contains(rpc.Message, want) {
			t.Fatalf("the busy message must contain %q so the agent knows to retry: %q", want, rpc.Message)
		}
	}
}

// sync.Mutex.Lock ignores ctx: a cancelled call used to stay parked on the cursor anyway.
func TestCursorSectionWaitIsCancellable(t *testing.T) {
	// fireWaitMax stays at its production value, so only the cancellation can end this wait.
	ps := newPointerState()
	release := holdSection(t, ps, "thread-A")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := cursorAction{
		Ops: benignOps(),
		Fire: func(*cursorHold, []map[string]any) (bool, error) {
			t.Error("a cancelled action must not fire")
			return true, nil
		},
	}.run(ctx, ps, "thread-B")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled wait must return")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("cancellation must end the wait promptly; it took %s (fireWaitMax is %s)", elapsed, fireWaitMax)
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("the reason should say the call was cancelled while waiting: %v", err)
	}
}

// A bare click/button/scroll's guard evidence IS the mirror, so it is only meaningful while
// the mirror still says what it said at compile time. The consent wait that precedes the
// section is exactly where another thread's pointer action lands.
func TestSeedIsReprovenInsideTheSection(t *testing.T) {
	ps := newPointerState()
	seed := point{1800, 900}
	ps.setLast("thread-B", seed)

	// Thread A is inside the section and, on its way out (still inside), parks the real
	// cursor somewhere else — the mirror B was checked against is now stale.
	inside := make(chan struct{})
	letGo := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = cursorAction{
			Ops: benignOps(),
			Fire: func(*cursorHold, []map[string]any) (bool, error) {
				close(inside)
				<-letGo
				return true, nil
			},
			Commit: func(pointerPlay) { ps.setLast("thread-A", point{300, 200}) },
		}.run(context.Background(), ps, "thread-A")
	}()
	<-inside
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(letGo)
	}()

	var fired int32
	err := cursorAction{
		Ops:  benignOps(),
		Seed: &seed,
		Fire: func(*cursorHold, []map[string]any) (bool, error) { atomic.AddInt32(&fired, 1); return true, nil },
	}.run(context.Background(), ps, "thread-B")
	<-finished

	if err == nil {
		t.Fatal("a bare action whose seed another thread moved while it waited must be refused")
	}
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Fatalf("the refused action must not have reached the portal, fired=%d", n)
	}
	var refusal cursorRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a stale seed is a refusal, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "1800") || !strings.Contains(err.Error(), "300") {
		t.Fatalf("the refusal should name both positions so the agent can re-aim: %v", err)
	}
	if !strings.Contains(err.Error(), "another agent") {
		t.Fatalf("the agent must be told ANOTHER agent took the cursor, or the refusal reads like a bug: %v", err)
	}
}

// A refusal decided inside the section is audited, fires nothing, and — because nothing
// moved — leaves the mirror exactly as it was.
func TestGuardRefusalFiresNothingAndLeavesTheMirrorStanding(t *testing.T) {
	ps := newPointerState()
	ps.setLast("t-1", point{1800, 900})
	var audited []string
	var plays []pointerPlay
	var fired int32

	// A real positioned click whose target is inside the Agent Kate window (100,100)-(500,400).
	err := cursorAction{
		Geometry: akGeometry(t),
		Ops:      clickOps([]map[string]any{{"t": "move", "x": 300, "y": 200}}, 0x110, 1, 0),
		Fire:     func(*cursorHold, []map[string]any) (bool, error) { atomic.AddInt32(&fired, 1); return true, nil },
		Commit:   func(p pointerPlay) { plays = append(plays, p) },
		Refused:  func(e error) { audited = append(audited, e.Error()) },
	}.run(context.Background(), ps, "t-1")

	if err == nil {
		t.Fatal("a geometric refusal must not succeed")
	}
	if !strings.Contains(err.Error(), "Agent Kate") {
		t.Fatalf("the refusal must name the self-target case: %v", err)
	}
	if n := atomic.LoadInt32(&fired); n != 0 {
		t.Fatalf("a refused action must not reach the portal, fired=%d", n)
	}
	if len(audited) != 1 {
		t.Fatalf("a refusal decided inside the section must be audited exactly once, got %d", len(audited))
	}
	if len(plays) != 1 || plays[0].played {
		t.Fatalf("the commit must see played=false so the mirror is left alone, got %+v", plays)
	}
	if p, ok := ps.last(); !ok || p != (point{1800, 900}) {
		t.Fatalf("nothing ran, so the mirror must still read (1800,900): got %v ok=%v", p, ok)
	}
}

// The wiring enforcement, not just a convention: a geometric verdict reached outside the
// section is already stale, so a call site that never took the section has no hold to pass
// and is refused. This is what makes deleting the section from a handler FAIL rather than
// silently race.
func TestGeometricGuardRefusesWithoutTheCursorSection(t *testing.T) {
	geom := akGeometry(t)
	pts := []point{{1800, 900}} // a point the guard would otherwise CLEAR
	err := guardPointerTargets(geom, nil, pts)
	if err == nil {
		t.Fatal("a geometric check taken outside the cursor section must fail closed")
	}
	if !strings.Contains(err.Error(), "cursor hold") {
		t.Fatalf("the refusal must be about the missing section, got %v", err)
	}
	ps := newPointerState()
	hold, release, aerr := ps.acquireFire(context.Background(), "t-1")
	if aerr != nil {
		t.Fatalf("uncontended acquire: %v", aerr)
	}
	// Held, it clears; released, it must not.
	if err := guardPointerTargets(geom, hold, pts); err != nil {
		t.Fatalf("a clean point under a live hold must clear: %v", err)
	}
	release()
	if err := guardPointerTargets(geom, hold, pts); err == nil || !strings.Contains(err.Error(), "cursor hold") {
		t.Fatalf("a RELEASED hold must not clear a geometric check either, got %v", err)
	}
	// And no geometry at all is a refusal, never a pass.
	if err := guardPointerTargets(nil, hold, pts); err == nil {
		t.Fatal("a guard with no geometry source must fail closed")
	}
}

func TestCursorOpsCannotReachThePortalWithoutAHold(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := handlerDeps{
		srv:    ipc.NewServer(filepath.Join(t.TempDir(), "cursor.sock"), log),
		cowork: selfService(t),
		log:    log,
	}
	ops := []map[string]any{{"t": "move", "x": 10, "y": 10}}
	// Same shape as above: with no UI attached the dispatch would fail anyway, so the test
	// asserts it was stopped for the RIGHT reason — before anything was sent.
	_, err := runCursorPortal(d, context.Background(), nil, "t-1", ops, time.Second, nil)
	if err == nil {
		t.Fatal("cursor-affecting ops must not reach the portal without a live cursor hold")
	}
	rpc, ok := err.(*ipc.RPCError)
	if !ok || rpc.Code != codeCoworkDenied {
		t.Fatalf("a missing hold is a denial, got %T %v", err, err)
	}
	if !strings.Contains(rpc.Message, "cursor hold") {
		t.Fatalf("the ops must be stopped by the missing section, not by a downstream failure: %q", rpc.Message)
	}
}

// Belt and braces on top of the behavioural tests above: acquireFire is the only door into
// the section, and cursorAction.run is the only caller — so no handler can take the cursor
// while skipping run's seed re-proof, guard, and commit-inside-the-section ordering.
func TestTheCursorSectionHasExactlyOneDoor(t *testing.T) {
	for _, f := range []string{"cowork.go", "cowork_pointer.go", "cowork_timeline.go", "mcp_cowork.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		n := strings.Count(string(src), "acquireFire(")
		want := 0
		if f == "cowork_pointer.go" {
			want = 2 // the definition and cursorAction.run's single call
		}
		if n != want {
			t.Fatalf("%s: found %d acquireFire references, want %d — every cursor action must enter through cursorAction.run", f, n, want)
		}
	}
}
