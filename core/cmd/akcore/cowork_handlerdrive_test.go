package main

// --- F25/F26 WIRING, fourth report: the handlers themselves, end to end -----------------
//
// Every previous round pinned the MECHANISM. cowork_handlerwiring_test.go drives the real
// guard→fire decision (cursorAction.run) over ops built by the very builders the handlers
// call, and it is a good test — but it constructs the cursorAction itself. So it cannot see
// whether cowork.pointerClick still routes through one. Three verifiers in a row deleted a
// handler's guard and watched the suite stay green, because nothing in the suite ever went
// through a handler.
//
// These tests do. They stand up a real ipc.Server with the real registerCoworkHandlers, a
// real cowork.Service, and a real agent-bridge connection that has identified for a
// Cowork-enabled thread — the strongest caller identity a prompt-injected agent can hold —
// and then call the six methods over the wire. The desktop they act on is a fixture
// (coworkListWindows) containing one Agent Kate window, so no KDE session bus is involved
// and the guard decides on real geometry rather than failing closed for want of a
// compositor. What is asserted is the OUTCOME an attacker would care about:
//
//   - the refusal is the GEOMETRIC one — it names Agent Kate — not a fail-closed error
//     that happens to look the same from the outside, and
//   - nothing that acts at a pointer position reached the portal.
//
// Both halves matter. Deleting runPointerAction's cursorAction leaves the handler dispatching
// through runCursorPortal with no hold, which refuses — the right answer for the wrong
// reason, and a test that only checked "an error came back" would certify a guard it cannot
// see. Deleting the hold check as well lets the ops through, and the portal count catches it.
//
// The legitimate half is asserted in the same breath, because a guard that refuses
// everything is not a fix: an ordinary click on an ordinary window must still fire.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentkate/internal/cowork"
	"agentkate/internal/ipc"
	"agentkate/internal/kde"
	"agentkate/internal/session"
)

// The fixture desktop: our own window at [100,500) x [100,400), and an ordinary one far
// away. akPoint is inside ours; farPoint is inside neither.
var (
	akPoint  = point{300, 200}
	farPoint = point{1400, 1000}
)

func driveWindows() []kde.Window {
	return []kde.Window{
		{InternalID: "w-ak", Caption: "Agent Kate — desktop CONTROL request",
			ResourceClass: "org.kde.agentkate", PID: 4242,
			X: 100, Y: 100, Width: 400, Height: 300},
		{InternalID: "w-firefox", Caption: "Docs — Firefox", ResourceClass: "firefox",
			PID: 7, Active: true, X: 1000, Y: 800, Width: 900, Height: 700},
	}
}

// driveCore is one running core: the cowork handlers, an agent bridge that may call them,
// and a stand-in UI that records — and answers — every portal request.
type driveCore struct {
	t      *testing.T
	bridge *ipc.Client
	ui     *ipc.Client
	cw     *cowork.Service

	mu     sync.Mutex
	portal []map[string]any // every cowork.portalRequest payload the UI saw
	grants chan map[string]any
}

// portalActs returns the portal requests whose op stream ACTS at a pointer position (a
// button or a wheel). A pure move is not one: motion has no side effect and is deliberately
// unguarded, so counting it would make every "nothing fired" assertion vacuous.
func (c *driveCore) portalActs() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, p := range c.portal {
		for _, op := range payloadOps(p) {
			if t, _ := op["t"].(string); t == "btn" || t == "axis_discrete" {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func (c *driveCore) portalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.portal)
}

func payloadOps(p map[string]any) []map[string]any {
	raw, _ := p["ops"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, o := range raw {
		if m, ok := o.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// call runs one cowork RPC as the agent bridge and returns the error verbatim.
func (c *driveCore) call(method string, params map[string]any) error {
	c.t.Helper()
	return c.bridge.CallTimeout(method, params, nil, 20*time.Second)
}

// newDriveCore boots the real handlers over a real socket. Pre-authorized capabilities are
// switched ON via the policy toggles, which is the WORST case for these guards: it is the
// posture in which no human sees a prompt, so the geometric guard is the only thing left.
func newDriveCore(t *testing.T, preAuth ...cowork.Capability) *driveCore {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	sock := filepath.Join(dir, "drive.sock")
	srv := ipc.NewServer(sock, log)

	// A zero-value kde.Client makes Available() true (the desktop is "there") while every
	// real KWin call fails — the window list the guards read comes from the fixture below.
	cw, _, err := cowork.New(filepath.Join(dir, "grants.json"), filepath.Join(dir, "audit.jsonl"),
		filepath.Join(dir, "policy.json"), &kde.Client{}, coworkNotifier{srv}, log)
	if err != nil {
		t.Fatalf("cowork.New: %v", err)
	}
	cw.SetSelfIdentity([]string{"org.kde.agentkate"}, []int{4242})
	for _, c := range preAuth {
		if err := cw.SetPolicy(c, true); err != nil {
			t.Fatalf("SetPolicy(%s): %v", c, err)
		}
	}

	sessions := testSessions(t)
	if err := sessions.Put(session.Record{ThreadID: "t-cw", Project: "/p",
		Created: time.Now(), CoworkEnabled: true}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	registerCoworkHandlers(handlerDeps{srv: srv, cowork: cw, sessions: sessions, log: log})
	// The two identity handshakes, registered here rather than pulled in from
	// registerHandlers so this file depends on nothing another agent may be editing.
	srv.Handle("test.asUI", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !srv.MarkUI(ctx) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest, "the UI role is taken")
		}
		return map[string]any{"ok": true}, nil
	})
	srv.Handle("test.asBridge", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &p)
		if ok, reason := srv.IdentifyBridge(ctx, p.ThreadID); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest, reason)
		}
		return map[string]any{"ok": true}, nil
	})
	serveIPC(t, srv, sock)

	// The fixture desktop, for the duration of this test only.
	prev := coworkListWindows
	coworkListWindows = func(*cowork.Service, time.Duration) ([]kde.Window, error) {
		return driveWindows(), nil
	}
	t.Cleanup(func() { coworkListWindows = prev })

	c := &driveCore{t: t, cw: cw, grants: make(chan map[string]any, 8)}

	uiConn, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial ui: %v", err)
	}
	t.Cleanup(func() { _ = uiConn.Close() })
	c.ui = uiConn
	uiConn.OnNotify(func(method string, params json.RawMessage) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		switch method {
		case "cowork.portalRequest":
			c.mu.Lock()
			c.portal = append(c.portal, p)
			c.mu.Unlock()
			// Answer from another goroutine: this callback runs on the client's read loop,
			// and the reply to our own call would arrive on it.
			go c.answerPortal(p)
		case "cowork.grantRequested":
			select {
			case c.grants <- p:
			default:
			}
		}
	})
	if err := uiConn.CallTimeout("test.asUI", map[string]any{}, nil, 5*time.Second); err != nil {
		t.Fatalf("asUI: %v", err)
	}

	bridgeConn, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	t.Cleanup(func() { _ = bridgeConn.Close() })
	c.bridge = bridgeConn
	if err := bridgeConn.CallTimeout("test.asBridge",
		map[string]any{"threadId": "t-cw"}, nil, 5*time.Second); err != nil {
		t.Fatalf("asBridge: %v", err)
	}
	return c
}

// answerPortal plays the UI's half: it reports every op applied and, when the stream ended
// on an absolute move, that the cursor provably landed there — which is what commits the
// core's pointer mirror (opsLandedAsAimed).
func (c *driveCore) answerPortal(p map[string]any) {
	ops := payloadOps(p)
	res := map[string]any{
		"corrId":     p["corrId"],
		"kind":       p["kind"],
		"ok":         true,
		"opsApplied": len(ops),
	}
	for _, op := range ops {
		if t, _ := op["t"].(string); t == "move" {
			res["ptrKnown"] = true
			res["ptrX"] = int(op["x"].(float64))
			res["ptrY"] = int(op["y"].(float64))
		}
	}
	_ = c.ui.CallTimeout("cowork.portalResult", res, nil, 5*time.Second)
}

// mustRefuse: the call came back refused BY THE GUARD, and nothing that acts at a pointer
// position was released.
//
// The second clause is the one that makes this mutation-sensitive. runCursorPortal's
// cursor-hold assertion is a structural BACKSTOP: a handler that stopped routing through
// cursorAction dispatches with no hold and is refused there, so "an error came back" is
// satisfied by a handler with no guard at all. That refusal is a wiring failure wearing a
// refusal's clothes, it names itself (`internal: no cursor hold`), and it fails here.
func (c *driveCore) mustRefuse(what string, err error, wantReason string) {
	c.t.Helper()
	if err == nil {
		c.t.Fatalf("%s: was NOT refused", what)
	}
	if strings.Contains(err.Error(), "no cursor hold") {
		c.t.Fatalf("%s: refused by the structural backstop, not by the guard — this handler "+
			"is no longer routing through the guard→fire section: %v", what, err)
	}
	if wantReason != "" && !strings.Contains(err.Error(), wantReason) {
		c.t.Fatalf("%s: refused for the wrong reason (want %q): %v", what, wantReason, err)
	}
	if acts := c.portalActs(); len(acts) != 0 {
		c.t.Fatalf("%s: the refused action still reached the portal: %v", what, acts)
	}
}

// mustRefuseSelfTarget is mustRefuse where the GEOMETRIC self-target guard is the decider,
// so the refusal must name Agent Kate.
func (c *driveCore) mustRefuseSelfTarget(what string, err error) {
	c.t.Helper()
	c.mustRefuse(what, err, "Agent Kate")
}

func (c *driveCore) mustSucceed(what string, err error) {
	c.t.Helper()
	if err != nil {
		c.t.Fatalf("%s: a legitimate action must not be refused: %v", what, err)
	}
}

// --- the six handlers -------------------------------------------------------------------

// cowork.pointerClick — a positioned click names its own target.
func TestHandlerPointerClickRefusesOurOwnWindow(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl)
	c.mustRefuseSelfTarget("cowork.pointerClick on our own window", c.call("cowork.pointerClick",
		map[string]any{"threadId": "t-cw", "x": akPoint.X, "y": akPoint.Y, "button": "left"}))

	// …and the honest use still works, through the same handler and the same guard.
	c.mustSucceed("cowork.pointerClick on an ordinary window", c.call("cowork.pointerClick",
		map[string]any{"threadId": "t-cw", "x": farPoint.X, "y": farPoint.Y, "button": "left"}))
	if len(c.portalActs()) != 1 {
		t.Fatalf("the cleared click must reach the portal exactly once, got %d", len(c.portalActs()))
	}
}

// cowork.scroll — positioned, and bare at the cursor.
func TestHandlerScrollRefusesOurOwnWindow(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl)
	c.mustRefuseSelfTarget("cowork.scroll positioned over our own window", c.call("cowork.scroll",
		map[string]any{"threadId": "t-cw", "x": akPoint.X, "y": akPoint.Y, "dy": 3}))

	// Park the cursor on us with a move (motion is deliberately unguarded), then scroll
	// bare: the guard point is the mirror, and the mirror now says we are under it.
	c.mustSucceed("move onto our own window", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": akPoint.X, "y": akPoint.Y}))
	c.mustRefuseSelfTarget("cowork.scroll bare while the cursor sits on us",
		c.call("cowork.scroll", map[string]any{"threadId": "t-cw", "dy": 3}))

	// Moved away, the same bare scroll is fine.
	c.mustSucceed("move away", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": farPoint.X, "y": farPoint.Y}))
	c.mustSucceed("bare scroll at a clean position",
		c.call("cowork.scroll", map[string]any{"threadId": "t-cw", "dy": 3}))
	if len(c.portalActs()) != 1 {
		t.Fatalf("exactly the cleared scroll may reach the portal, got %d", len(c.portalActs()))
	}
}

// cowork.pointerDrag — BOTH endpoints act (press at from, release at to).
func TestHandlerPointerDragRefusesEitherEndpointOnUs(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl)
	c.mustRefuseSelfTarget("drag RELEASING on our own window", c.call("cowork.pointerDrag",
		map[string]any{"threadId": "t-cw", "fromX": farPoint.X, "fromY": farPoint.Y,
			"toX": akPoint.X, "toY": akPoint.Y}))
	c.mustRefuseSelfTarget("drag GRABBING on our own window", c.call("cowork.pointerDrag",
		map[string]any{"threadId": "t-cw", "fromX": akPoint.X, "fromY": akPoint.Y,
			"toX": farPoint.X, "toY": farPoint.Y}))

	c.mustSucceed("drag between two ordinary points", c.call("cowork.pointerDrag",
		map[string]any{"threadId": "t-cw", "fromX": farPoint.X, "fromY": farPoint.Y,
			"toX": farPoint.X + 50, "toY": farPoint.Y + 50}))
	if len(c.portalActs()) != 1 {
		t.Fatalf("only the cleared drag may reach the portal, got %d", len(c.portalActs()))
	}
}

// cowork.movePointer and cowork.movePointerRelative — motion only, deliberately unguarded:
// passing over our windows has no side effect, and refusing it would break mouse-look and
// every ordinary approach to a target. What must hold is that they still go through the
// section (so the mirror is committed under the lock and the next bare click is guarded
// against a position that is real), which is what the portal round-trip here proves.
func TestHandlerMovePointerCrossesOurWindowAndStillCommitsTheMirror(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl)
	c.mustSucceed("cowork.movePointer onto our own window", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": akPoint.X, "y": akPoint.Y}))
	if c.portalCount() != 1 {
		t.Fatalf("the move must reach the portal, got %d requests", c.portalCount())
	}
	// The mirror was committed under the lock: the bare click that follows is refused
	// BECAUSE the move was recorded, which is the only observable proof it went through the
	// section rather than round it. (input_inject is not pre-authorized here, so the refusal
	// arrives before any prompt — the advisory copy of the bare-click check.)
	c.mustRefuseSelfTarget("a bare click after the move", c.call("cowork.injectInput",
		map[string]any{"threadId": "t-cw", "events": []map[string]any{{"type": "button", "button": "left"}}}))
}

func TestHandlerMovePointerRelativeFiresAndAccountsForTheWalk(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl, cowork.CapInputInject)
	c.mustSucceed("establish a position", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": farPoint.X, "y": farPoint.Y}))
	// Mouse-look with no absolute landing point must not be blocked…
	c.mustSucceed("cowork.movePointerRelative", c.call("cowork.movePointerRelative",
		map[string]any{"threadId": "t-cw", "dx": float64(akPoint.X - farPoint.X),
			"dy": float64(akPoint.Y - farPoint.Y), "steps": 4}))
	if c.portalCount() != 2 {
		t.Fatalf("both moves must reach the portal, got %d", c.portalCount())
	}
	// …and the walk must be CARRIED into the mirror, so the bare click that follows it
	// cannot be cleared against where the cursor used to be (audit F3). With the desktop
	// bounds unknown — no compositor here — the delta cannot be accounted for at all, and
	// the mirror is DESTROYED rather than left standing; either way the click is refused
	// and nothing is released.
	c.mustRefuse("a bare click after walking onto us", c.call("cowork.injectInput",
		map[string]any{"threadId": "t-cw", "events": []map[string]any{{"type": "button", "button": "left"}}}),
		"RELATIVE nudge")
}

// cowork.injectInput — a bare button fires wherever the cursor already is, so its guard
// point is the pointer mirror.
//
// The advisory copy of that check runs BEFORE the consent wait and the authoritative one
// runs inside the guard→fire section, and this test is arranged so that only the
// authoritative one can refuse: the mirror is clean when the handler starts, and it is
// moved onto Agent Kate's window WHILE the handler is blocked on the human — the exact
// interleaving audit F26 is about, and the reason the second check exists at all.
func TestHandlerInjectInputBareClickIsGuardedInsideTheConsentSection(t *testing.T) {
	// pointer_control is pre-authorized (so the move needs no prompt); input_inject is NOT,
	// so the inject call blocks on a grant request we control the timing of.
	c := newDriveCore(t, cowork.CapPointerControl)
	c.mustSucceed("park the cursor somewhere clean", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": farPoint.X, "y": farPoint.Y}))

	done := make(chan error, 1)
	go func() {
		done <- c.call("cowork.injectInput", map[string]any{"threadId": "t-cw",
			"events": []map[string]any{{"type": "button", "button": "left"}}})
	}()

	// The advisory check has passed by the time the prompt is raised.
	var req map[string]any
	select {
	case req = <-c.grants:
	case <-time.After(10 * time.Second):
		t.Fatal("cowork.injectInput never asked for consent")
	}

	// Now the cursor moves onto us — another action crossing the consent wait.
	c.mustSucceed("a second action parks the shared cursor on us", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": akPoint.X, "y": akPoint.Y}))

	// The human says yes. Only the in-section check can refuse from here.
	if err := c.ui.CallTimeout("cowork.respondGrant", map[string]any{
		"requestId": req["requestId"], "allow": true, "scope": "once",
	}, nil, 5*time.Second); err != nil {
		t.Fatalf("respondGrant: %v", err)
	}

	select {
	case err := <-done:
		// The seed re-proof: the position the advisory check cleared, and that the human
		// was prompted about, is no longer where the cursor is. This is the refusal a
		// verifier deleted in round 2 with the suite still green.
		c.mustRefuse("injectInput bare click after the cursor was parked on us", err,
			"between the safety check and this action")
	case <-time.After(20 * time.Second):
		t.Fatal("cowork.injectInput never returned")
	}
}

// The other half of the same handler: with the mirror UNMOVED across the consent wait, the
// in-section check re-proves the seed, finds it still where it was — and then the geometric
// guard has to refuse it, because that position is on Agent Kate's own window. This is the
// case where the advisory copy and the authoritative copy agree, and it pins that the
// authoritative one reads geometry and not just the mirror.
func TestHandlerInjectInputBareClickOnUsIsRefusedGeometrically(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl, cowork.CapInputInject)
	c.mustSucceed("park the cursor on our own window", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": akPoint.X, "y": akPoint.Y}))
	c.mustRefuseSelfTarget("injectInput bare click over our own window",
		c.call("cowork.injectInput", map[string]any{"threadId": "t-cw",
			"events": []map[string]any{{"type": "button", "button": "left"}}}))
}

// …and the same handler on a clean position still types and clicks, which is what makes the
// refusal above a guard rather than a wall.
func TestHandlerInjectInputStillWorksOnAnOrdinaryWindow(t *testing.T) {
	c := newDriveCore(t, cowork.CapPointerControl, cowork.CapInputInject)
	c.mustSucceed("park the cursor on an ordinary window", c.call("cowork.movePointer",
		map[string]any{"threadId": "t-cw", "x": farPoint.X, "y": farPoint.Y}))
	c.mustSucceed("a bare click at a cleared position", c.call("cowork.injectInput",
		map[string]any{"threadId": "t-cw", "targetWindowId": "w-firefox",
			"events": []map[string]any{{"type": "button", "button": "left"}}}))
	if len(c.portalActs()) != 1 {
		t.Fatalf("the cleared click must reach the portal exactly once, got %d", len(c.portalActs()))
	}
}

// The seam these tests hang on is production code: if coworkListWindows ever stops being
// what the guards read, every assertion above becomes vacuous without failing.
func TestCoworkGeometryReadsTheWindowSeam(t *testing.T) {
	dir := t.TempDir()
	cw, _, err := cowork.New(filepath.Join(dir, "g.json"), filepath.Join(dir, "a.jsonl"),
		filepath.Join(dir, "p.json"), nil, nil, nil)
	if err != nil {
		t.Fatalf("cowork.New: %v", err)
	}
	cw.SetSelfIdentity([]string{"org.kde.agentkate"}, []int{4242})

	prev := coworkListWindows
	coworkListWindows = func(*cowork.Service, time.Duration) ([]kde.Window, error) {
		return driveWindows(), nil
	}
	defer func() { coworkListWindows = prev }()

	geom := coworkGeometry{cw}
	rects, err := geom.Windows()
	if err != nil {
		t.Fatalf("Windows: %v", err)
	}
	if len(rects) != 2 {
		t.Fatalf("the guard must see the whole desktop, got %+v", rects)
	}
	if !geom.IsSelfPoint(akPoint.X, akPoint.Y, rects) {
		t.Fatal("the fixture point inside our own window must be recognised as ours")
	}
	if geom.IsSelfPoint(farPoint.X, farPoint.Y, rects) {
		t.Fatal("the fixture point on an ordinary window must not be")
	}
	// And a compositor that cannot be read is an error, never an empty desktop: an empty
	// list would clear every point on screen.
	coworkListWindows = func(*cowork.Service, time.Duration) ([]kde.Window, error) {
		return nil, fmt.Errorf("kwin gone")
	}
	if _, err := geom.Windows(); err == nil {
		t.Fatal("an unreadable window list must surface as an error")
	}
}
