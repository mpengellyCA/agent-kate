package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"agentkate/internal/cowork"
	"agentkate/internal/kde"
)

// point is an absolute desktop pixel coordinate (same space as desktop_list_elements
// bounds and desktop_screenshot pixels).
type point struct{ X, Y int }

// PointerProfile shapes how a move op is expanded into a path (plan 09 §4). The core
// expands a single logical move into a list of intermediate move ops with inter-op
// delays; the UI just plays them. Speed sets the step count/delay, accuracy shapes the
// path. The expansion is pure (seeded RNG) so it is unit-testable and audit-loggable —
// we log the target + profile, never every interpolated step.
type PointerProfile struct {
	Speed    float64 `json:"speed"`    // pixels/second; <=0 means "instant" (a single jump)
	Accuracy float64 `json:"accuracy"` // 0..1; 1 = straight line, lands exact; <1 = human-like
	SettleMs int     `json:"settleMs"` // pause after arrival before a click
}

// Movement-profile constants. stepDt caps the smooth-motion frequency: one D-Bus
// Notify* per step, so we never exceed ~120 ops/s (plan 09 §4).
const (
	pointerStepDt    = 0.012 // 12 ms between interpolated steps
	pointerMaxSteps  = 240   // hard cap on a single path's op count
	pointerMaxJitter = 6.0   // px of per-step jitter at accuracy 0
	pointerOvershoot = 14.0  // px of bounded overshoot-then-correct at accuracy 0
)

func defaultPointerProfile() PointerProfile {
	return PointerProfile{Speed: 1600, Accuracy: 1.0, SettleMs: 30}
}

// clampProfile applies the absolute safety limits, then clamps to the user-set bounds
// (the Cowork panel control): the agent may never command motion faster than the user
// allowed — "instant" (0) is treated as the fastest and capped to the user's speed when
// a cap is set. Accuracy/settle are clamped to sane ranges only.
func clampProfile(p, bounds PointerProfile) PointerProfile {
	out := p
	if out.Speed < 0 {
		out.Speed = 0
	}
	if out.Speed > 20000 {
		out.Speed = 20000
	}
	if bounds.Speed > 0 {
		// instant (0) or anything above the user's cap collapses to the cap.
		if out.Speed == 0 || out.Speed > bounds.Speed {
			out.Speed = bounds.Speed
		}
	}
	if out.Accuracy <= 0 {
		out.Accuracy = 0
	}
	if out.Accuracy > 1 {
		out.Accuracy = 1
	}
	if out.SettleMs < 0 {
		out.SettleMs = 0
	}
	if out.SettleMs > 2000 {
		out.SettleMs = 2000
	}
	return out
}

// pointerState holds the per-thread session-default profiles, the user-set bounds, and
// the last commanded pointer position (the m_ptr mirror — the UI tracks its own, but the
// core needs a start point to expand a path). All access is mutex-guarded.
//
// SECURITY (audit F3, pointer half): the mirror is not bookkeeping, it is the ONLY
// evidence the bare-click / bare-scroll guards have about where a button will fire. A
// mirror that says "safe spot" while the true cursor sits on Agent Kate's Allow button
// is worse than no mirror at all, so every path that moves the pointer must either
// commit an exact position (setLast) or destroy the mirror (invalidate). Relative motion
// goes through applyRelative, which does one or the other.
type pointerState struct {
	mu        sync.Mutex
	bounds    PointerProfile
	perThread map[string]PointerProfile

	// SECURITY (audit F26): ONE mirror for the whole core, never one per thread. The
	// cursor is a single global resource, so per-thread evidence about it was unsound:
	// thread A parked the real pointer on an Agent Kate window (motion is deliberately
	// unguarded, and A's own mirror stayed honest about it) while thread B's untouched
	// mirror still read B's earlier clean position — B's bare click then passed its
	// geometric guard and fired at A's parked point. Every thread's pointer action now
	// updates or destroys the same evidence. The availability cost (threads invalidate
	// each other) is deliberate; the refusals already steer the agent to re-establish a
	// position with an absolute move.
	lastPos  point
	haveLast bool
	// lostWhy records WHY the mirror was DESTROYED (one of the mirrorLost* constants), so
	// the refusal can say what happened — and steer the agent to the fix — instead of
	// claiming the pointer was never moved. Empty = never established.
	lostWhy string
	// lastBy is the thread whose action last moved (or destroyed) the mirror. Diagnostics
	// only — it is deliberately NOT a key: see the block above.
	lastBy string

	// fire serializes the window between a pointer guard and the portal reply (audit F26,
	// part 2). Without it two threads interleave between the geometric check and the use
	// of the cursor, so a check that passed describes a position the other thread has
	// already changed. It is NEVER held across cw.Authorize, which can wait minutes on a
	// human.
	//
	// It is a 1-slot channel and not a sync.Mutex because the section is held across a
	// portal round-trip of up to 60 s (a timeline may legitimately play for 30). Mutex.Lock
	// is neither bounded nor context-aware, so one agent's long script parked EVERY other
	// agent's pointer call behind it with no cancellation and no diagnostic — a self-
	// inflicted denial of service in a multi-agent arena, and the kind of collateral that
	// gets a security fix reverted. Waiting is now bounded (fireWaitMax), cancellable by
	// the caller's ctx, and refused with a contention message that is deliberately NOT a
	// guard refusal (see fireBusyErr).
	fire chan struct{}
	// fireBy is the thread inside the section, read only to word a contention message.
	// Guarded by mu — it says nothing about who may enter, only who is already in.
	fireBy string

	// Cached desktop layout (the screens the compositor clamps the pointer into). Short
	// TTL: screens change rarely, and a stale answer only ever costs an extra invalidation.
	deskBounds  kde.DesktopLayout
	deskAt      time.Time
	deskFetched bool
}

// desktopBoundsTTL is how long a KWin desktop-geometry answer is reused. Short enough
// that unplugging a monitor is picked up within a couple of relative moves, long enough
// that a mouse-look burst does not load a KWin script per nudge.
const desktopBoundsTTL = 3 * time.Second

// The two ways a mirror is destroyed rather than merely unset. Both refuse the same
// actions; they differ only in what the agent is told to do next.
const (
	// mirrorLostRelative: a relative nudge moved the cursor somewhere we cannot account
	// for (unknown screen layout, or a walk into the compositor's edge clamp).
	mirrorLostRelative = "relative"
	// mirrorLostUnproven: an ABSOLUTE action did not provably land where it was aimed —
	// the UI dropped the move (no captured screen contains the point) or the playback
	// failed part-way, stranding the cursor somewhere along the path.
	mirrorLostUnproven = "unproven"
)

func newPointerState() *pointerState {
	return &pointerState{
		bounds:    defaultPointerProfile(),
		perThread: map[string]PointerProfile{},
		fire:      make(chan struct{}, 1),
	}
}

// pointerProfilePatch is a partial profile from the agent/UI: a nil field means "leave
// unchanged", so {"speed":3000} adjusts only the speed and keeps the standing accuracy.
type pointerProfilePatch struct {
	Speed    *float64 `json:"speed"`
	Accuracy *float64 `json:"accuracy"`
	SettleMs *int     `json:"settleMs"`
}

func (p PointerProfile) applyPatch(patch *pointerProfilePatch) PointerProfile {
	if patch == nil {
		return p
	}
	if patch.Speed != nil {
		p.Speed = *patch.Speed
	}
	if patch.Accuracy != nil {
		p.Accuracy = *patch.Accuracy
	}
	if patch.SettleMs != nil {
		p.SettleMs = *patch.SettleMs
	}
	return p
}

// baseLocked returns the standing profile for a thread (its session default if set, else
// the user bounds). Caller holds s.mu.
func (s *pointerState) baseLocked(thread string) PointerProfile {
	if p, ok := s.perThread[thread]; ok {
		return p
	}
	return s.bounds
}

// setBounds merges a patch onto the user-set bounds (the Cowork panel control). Bounds
// are sanity-clamped but not clamped to themselves.
func (s *pointerState) setBounds(patch *pointerProfilePatch) PointerProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bounds = clampProfile(s.bounds.applyPatch(patch), PointerProfile{})
	return s.bounds
}

// setThreadProfile merges a patch onto the thread's standing profile (the agent's
// desktop_set_pointer_profile session default) and clamps to the user bounds.
func (s *pointerState) setThreadProfile(thread string, patch *pointerProfilePatch) PointerProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	eff := clampProfile(s.baseLocked(thread).applyPatch(patch), s.bounds)
	s.perThread[thread] = eff
	return eff
}

// resolve returns the effective, clamped profile for one call: the thread's standing
// profile with any per-call patch applied, clamped to the user bounds.
func (s *pointerState) resolve(thread string, patch *pointerProfilePatch) PointerProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clampProfile(s.baseLocked(thread).applyPatch(patch), s.bounds)
}

// last reads the global mirror. It takes no thread: the answer is about the one cursor
// every thread shares (audit F26), and a per-thread read is exactly the bug.
func (s *pointerState) last() (point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPos, s.haveLast
}

// setLast records an EXACT commanded position (absolute move/click/drag/scroll). It also
// clears the relative-drift mark: an absolute move re-establishes a known position, which
// is the documented way back from a mirror that relative motion invalidated. thread is
// recorded for diagnostics only — the position itself is global.
func (s *pointerState) setLast(thread string, p point) {
	s.mu.Lock()
	s.lastPos, s.haveLast = p, true
	s.lostWhy = ""
	s.lastBy = thread
	s.mu.Unlock()
}

// invalidate destroys the mirror: after this, every guard that depends on knowing where
// the cursor is refuses — for EVERY thread, because there is only one cursor — until an
// absolute move re-establishes it. why is one of the mirrorLost* constants and only shapes
// the refusal wording.
func (s *pointerState) invalidate(thread, why string) {
	s.mu.Lock()
	s.lastPos, s.haveLast = point{}, false
	s.lostWhy = why
	s.lastBy = thread
	s.mu.Unlock()
}

// mirrorLoss reports why the mirror is missing: one of the mirrorLost* constants, or ""
// when it was simply never established (no positioned action yet).
func (s *pointerState) mirrorLoss() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lostWhy
}

// fireWaitMax bounds how long one pointer action waits for the shared guard→fire section.
// It is deliberately shorter than the longest legitimate hold (a 30 s timeline behind a
// 60 s portal timeout): past this point the honest answer to the waiting agent is "the
// cursor is busy, retry", not an unbounded stall it cannot observe or cancel.
// (A var, not a const, only so the contention tests can shorten it — production never
// writes it.)
var fireWaitMax = 10 * time.Second

// cursorHold is proof that its bearer is inside the guard→fire section over the shared
// cursor. Only acquireFire mints one, and release invalidates it, so a geometric guard —
// or a dispatch of cursor-affecting ops — cannot be run from a call site that never took
// the section: it has no hold to pass, and nil FAILS CLOSED (audit F26). A hold never
// escapes the goroutine that took it.
type cursorHold struct {
	ps       *pointerState
	released bool
}

func (h *cursorHold) valid() bool { return h != nil && h.ps != nil && !h.released }

// acquireFire takes the guard→fire critical section over the shared cursor and returns a
// hold plus its release. It is NEVER called across cw.Authorize, which can wait minutes on
// a human.
//
// SECURITY (audit F26): the section is what makes a geometric check describe the cursor at
// the moment the ops are released. Waiting for it is bounded and cancellable so that
// keeping that property cannot deadlock the arena — see the `fire` field comment.
func (s *pointerState) acquireFire(ctx context.Context, thread string) (*cursorHold, func(), error) {
	enter := func() (*cursorHold, func(), error) {
		s.mu.Lock()
		s.fireBy = thread
		s.mu.Unlock()
		h := &cursorHold{ps: s}
		return h, func() {
			h.released = true
			s.mu.Lock()
			s.fireBy = ""
			s.mu.Unlock()
			<-s.fire
		}, nil
	}
	// Uncontended fast path: no timer, no allocation beyond the hold.
	select {
	case s.fire <- struct{}{}:
		return enter()
	default:
	}
	t := time.NewTimer(fireWaitMax)
	defer t.Stop()
	select {
	case s.fire <- struct{}{}:
		return enter()
	case <-ctx.Done():
		return nil, nil, s.fireBusyErr(thread, "the call was cancelled while it waited its turn")
	case <-t.C:
		return nil, nil, s.fireBusyErr(thread, fmt.Sprintf("gave up after %s", fireWaitMax))
	}
}

// fireBusyErr words a CONTENTION failure. It must not read like a guard refusal: an agent
// that mistakes "the cursor is busy" for "that target is forbidden" learns the wrong lesson
// and either gives up on a legitimate action or retries a different, worse shape. So it
// says busy (never "refused"), states that nothing was checked and nothing ran, and asks
// for a plain retry.
func (s *pointerState) fireBusyErr(thread, why string) error {
	s.mu.Lock()
	by := s.fireBy
	s.mu.Unlock()
	who := "another agent"
	if by != "" && by == thread {
		who = "another call on this same thread"
	}
	return cursorBusy{fmt.Errorf("busy: %s is currently controlling the shared pointer, so this action could not take its turn (%s). Nothing was checked and NOTHING WAS DONE — this is not a refusal and the target is not forbidden; simply try again in a moment", who, why)}
}

// cursorBusy marks a CONTENTION failure over the shared cursor: no check ran and no op was
// released. Typed so the handler maps it to codeCoworkBusy and never to a denial.
type cursorBusy struct{ err error }

func (e cursorBusy) Error() string { return e.err.Error() }
func (e cursorBusy) Unwrap() error { return e.err }

// cursorAction is the guard→fire decision of one pointer-affecting action, lifted out of
// the handler closures so it can be driven — and its refuse-vs-fire outcome asserted — in a
// unit test that needs no portal and no KDE bus (audit F26 wiring). Every handler that
// touches the shared cursor routes through run; acquireFire has no other caller.
//
// SECURITY (audit F25/F26 wiring, third report): it is DECLARATIVE on purpose. Each handler
// used to hand in its own Guard closure and its own guard points, so "this handler forgot to
// guard" and "this handler has nothing to guard (a pure move)" were the same program — and
// the verifier could delete injectInput's guard, and then runPointerAction's, with the whole
// suite still green. There is no guard closure to delete now: the caller hands over the OPS
// IT IS ABOUT TO RELEASE, and run derives the points those ops act at (effectPoints), proves
// the mirror, checks the geometry and only then fires. A handler cannot opt out of the guard
// without also not releasing any ops.
type cursorAction struct {
	// Geometry is the live-window source the geometric self-target guard reads. nil FAILS
	// CLOSED for any action whose ops act somewhere.
	Geometry cursorGeometry
	// Ops is the exact op stream Fire is about to release. The guard points are DERIVED
	// from it, never declared alongside it.
	Ops []map[string]any
	// Seed, when non-nil, is the mirror position this action was COMPILED against (a bare
	// click/button/scroll fires wherever the cursor IS, and the consent prompt the human
	// answered names that point). It is re-proven INSIDE the section, because the consent
	// wait that precedes run is exactly where another thread's pointer action lands. It is
	// REQUIRED — nil fails closed — whenever Ops act before naming an absolute point.
	Seed *point
	// Bounds is the screen layout relative motion inside Ops is accounted against. An
	// invalid layout means "unknown", so any op acting after a nudge fails closed.
	Bounds kde.DesktopLayout
	// Fire releases ops to the UI portal and reports whether the UI PROVED they landed as
	// aimed. Reached only once the section is held and every check above passed.
	//
	// It is HANDED the ops rather than closing over its own copy, and that is not a style
	// choice: a Fire that carried its own list could be dispatched while a different — or an
	// empty — list was the one the section checked, which is the same "guarded one thing,
	// did another" shape as the deleted guards this design exists to make impossible. What
	// is guarded and what is released are now the same slice.
	Fire func(hold *cursorHold, ops []map[string]any) (landed bool, err error)
	// Commit records what became of the ops, still inside the section — that is what stops
	// another thread from reading the mirror in the gap between the portal's reply and the
	// update. It is called even when the action was refused (played=false: nothing moved,
	// so the mirror stands). nil for actions that cannot move the pointer.
	Commit func(pointerPlay)
	// Refused audits a refusal decided inside the section. Contention is NOT routed here:
	// a busy cursor is an availability outcome, not a security one, and logging it as a
	// refusal would bury the real ones. nil to skip.
	Refused func(error)
}

// cursorGeometry is the live geometry the self-target guard reads: the compositor's current
// window rectangles, plus Agent Kate's own identity to match them against. *cowork.Service
// is the production implementation (coworkGeometry, cowork.go); a test supplies a fake,
// which is what lets the REAL guard→fire decision be driven with no KDE session bus — the
// reason the previous rounds' tests re-implemented the decision instead of driving it.
type cursorGeometry interface {
	Windows() ([]cowork.WindowRect, error)
	IsSelfPoint(x, y int, wins []cowork.WindowRect) bool
}

// cursorRefusal marks an error that the section itself decided (stale seed, geometric
// guard). Handlers map it to codeCoworkDenied; a contention error, which is not one of
// these, maps to codeCoworkBusy.
type cursorRefusal struct{ err error }

func (e cursorRefusal) Error() string { return e.err.Error() }
func (e cursorRefusal) Unwrap() error { return e.err }

func (a cursorAction) run(ctx context.Context, ps *pointerState, thread string) error {
	hold, release, err := ps.acquireFire(ctx, thread)
	if err != nil {
		// Contention: nothing was checked, nothing ran, the mirror is untouched. Returning
		// before Commit is deliberate — there is no play to record.
		return err
	}
	defer release()

	var play pointerPlay
	if a.Commit != nil {
		defer func() { a.Commit(play) }()
	}
	refuse := func(err error) error {
		if a.Refused != nil {
			a.Refused(err)
		}
		return cursorRefusal{err}
	}
	// The mirror, read ONCE and inside the section: it is both the subject of the seed
	// re-proof and the position every derived guard point is threaded from.
	at, haveAt := ps.last()
	if needSeed := opsNeedSeed(a.Ops); a.Seed != nil || needSeed {
		if a.Seed == nil {
			// FAIL CLOSED: these ops act wherever the cursor already is, and nothing told the
			// section which position the human's prompt and this call's checks were about.
			return refuse(fmt.Errorf("refused: this action fires at the pointer's current position but was never checked against a known one (internal: no seed) — use desktop_click(x,y), which targets and guards an exact point"))
		}
		if err := ps.seedStillHolds(thread, *a.Seed); err != nil {
			return refuse(err)
		}
	}
	// Every point these ops will actually ACT at, derived from the ops themselves.
	pts, ok := effectPoints(a.Ops, at, haveAt, a.Bounds)
	if !ok {
		return refuse(fmt.Errorf("%s", bareClickRefusal(ps.mirrorLoss())))
	}
	if len(pts) > 0 {
		if err := guardPointerTargets(a.Geometry, hold, pts); err != nil {
			return refuse(err)
		}
	}
	if a.Fire == nil {
		return nil
	}
	// FAIL CLOSED on an empty stream: an action that dispatches must have handed the section
	// the ops it is about to dispatch, so "nothing to check" and "nothing to send" are the
	// same state. Without this, blanking Ops in a handler would leave the guard with nothing
	// to derive and the portal with everything to play.
	if len(a.Ops) == 0 {
		return refuse(fmt.Errorf("refused: a pointer action must hand the safety check the ops it is about to run (internal: no ops)"))
	}
	// From here the ops are in the UI's hands: whatever comes back, the cursor may have
	// moved (a failure can strand it part-way along an interpolated path).
	play.played = true
	landed, err := a.Fire(hold, a.Ops)
	play.landed = landed
	return err
}

// seedStillHolds re-proves, under the fire lock, that the mirror is still the position an
// action's guard evidence was compiled against. A bare click/button/scroll fires wherever
// the cursor IS, so its guard point is only meaningful while the mirror still says what it
// said at compile time — and with a global mirror another thread may have moved it (or
// destroyed it) while this call waited on consent (audit F26).
//
// thread is the caller, used only to tell it whether ANOTHER agent took the cursor: that is
// the availability cost of a global mirror, and a refusal the agent cannot explain reads
// like a bug. The other thread is never named.
func (s *pointerState) seedStillHolds(thread string, seed point) error {
	s.mu.Lock()
	now, ok, why, by := s.lastPos, s.haveLast, s.lostWhy, s.lastBy
	s.mu.Unlock()
	who := "this agent"
	if by != "" && by != thread {
		who = "another agent"
	}
	if !ok {
		return fmt.Errorf("refused: the pointer position this action was checked against was lost before it could run (%s, by %s) — the cursor is shared, so re-establish a known position with desktop_move_pointer(x,y), or use desktop_click(x,y), which targets and guards an exact point",
			orDefault(why, "a pointer action"), who)
	}
	if now != seed {
		return fmt.Errorf("refused: the pointer moved from (%d,%d) to (%d,%d) between the safety check and this action (%s moved it) — the cursor is shared with every other agent, so where a bare click would land can no longer be verified. Re-check the position, or use desktop_click(x,y), which targets and guards an exact point",
			seed.X, seed.Y, now.X, now.Y, who)
	}
	return nil
}

// applyRelative folds a relative delta into the mirror.
//
// SECURITY (audit F3): relative motion USED to leave the mirror untouched, on the theory
// that a pointer grab makes the true position unknowable. That was a bypass, not a
// caveat: the stale position stayed valid, so an agent could walk the real cursor onto an
// Agent Kate window with move_rel and then fire a bare click that the geometric guard
// cleared against the position the cursor left minutes ago.
//
// So: accumulate (a), and fail closed (b) the moment accumulation stops being sound.
// The compositor applies dx/dy exactly while the result stays inside the desktop; at the
// edge it CLAMPS, and past that point the accumulated value is fiction. Anything we cannot
// prove landed inside the known desktop box — unknown bounds included — destroys the
// mirror instead of updating it. Returns the resulting position and whether it is known.
// bounds is the SCREEN LAYOUT, not a single box: containment is proven per screen, because
// the union of a staggered/L-shaped multi-monitor layout contains dead space the cursor can
// never occupy, and accepting a point there would be the very "mirror points where the
// cursor is not" state this guards against (round 11, MED).
func (s *pointerState) applyRelative(thread string, dx, dy float64, bounds kde.DesktopLayout) (point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from, ok := s.lastPos, s.haveLast
	if !ok {
		// Nothing to drift: the mirror was already unknown, and a relative move cannot
		// establish one. Mark it as relative loss anyway so the refusal names the cause.
		s.lostWhy, s.lastBy = mirrorLostRelative, thread
		return point{}, false
	}
	to := point{
		X: from.X + int(math.Round(clampRelDelta(dx))),
		Y: from.Y + int(math.Round(clampRelDelta(dy))),
	}
	if !bounds.Contains(to.X, to.Y) {
		// Either we never learned the desktop's extent, or the cursor ran into an edge (a
		// screen edge, or the dead space between mis-aligned screens) and the compositor
		// clamped it. Both mean the accumulated point is not where the cursor is.
		s.lastPos, s.haveLast = point{}, false
		s.lostWhy, s.lastBy = mirrorLostRelative, thread
		return point{}, false
	}
	s.lastPos, s.haveLast = to, true
	s.lostWhy, s.lastBy = "", thread
	return to, true
}

// desktopBounds returns the compositor's screen layout, cached for desktopBoundsTTL.
// A failure is reported as an invalid layout — never a guess — so applyRelative fails
// closed rather than trusting an accumulation it cannot bound.
func (s *pointerState) desktopBounds(cw *cowork.Service) kde.DesktopLayout {
	s.mu.Lock()
	if s.deskFetched && time.Since(s.deskAt) < desktopBoundsTTL {
		r := s.deskBounds
		s.mu.Unlock()
		return r
	}
	s.mu.Unlock()

	// Off-lock: this is a KWin round trip and must never block profile/mirror readers.
	var r kde.DesktopLayout
	if cw != nil {
		if got, err := cw.KDE().DesktopBounds(4 * time.Second); err == nil {
			r = got
		}
	}
	s.mu.Lock()
	s.deskBounds = r
	s.deskAt = time.Now()
	s.deskFetched = true
	s.mu.Unlock()
	return r
}

func newPointerRNG() *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}

// expandMove turns a logical move from→to into a path of move ops per the profile. The
// final op ALWAYS lands exactly on `to` regardless of accuracy (jitter/overshoot never
// change where the click happens — only the path). With no known start (haveFrom=false)
// or instant speed, it is a single teleport op.
func expandMove(from point, haveFrom bool, to point, prof PointerProfile, rng *rand.Rand) []map[string]any {
	if !haveFrom || prof.Speed <= 0 {
		return []map[string]any{{"t": "move", "x": to.X, "y": to.Y}}
	}
	dx := float64(to.X - from.X)
	dy := float64(to.Y - from.Y)
	dist := math.Hypot(dx, dy)
	if dist < 1 {
		return []map[string]any{{"t": "move", "x": to.X, "y": to.Y}}
	}
	steps := int(math.Round(dist / (prof.Speed * pointerStepDt)))
	if steps < 1 {
		steps = 1
	}
	if steps > pointerMaxSteps {
		steps = pointerMaxSteps
	}
	human := prof.Accuracy < 1
	jitterAmp := (1 - prof.Accuracy) * pointerMaxJitter
	overshoot := (1 - prof.Accuracy) * pointerOvershoot
	delay := int(pointerStepDt * 1000)
	moveOp := func(x, y int) map[string]any {
		return map[string]any{"t": "move", "x": x, "y": y, "delayMs": delay}
	}

	ops := make([]map[string]any, 0, steps+2)
	// Interpolated approach: steps-1 intermediate points (the exact target is appended
	// last). Linear for accuracy 1; smoothstep-eased + jittered for human motion.
	for i := 1; i < steps; i++ {
		t := float64(i) / float64(steps)
		et := t
		if human {
			et = t * t * (3 - 2*t) // smoothstep ease-in/out
		}
		px := float64(from.X) + dx*et
		py := float64(from.Y) + dy*et
		if human {
			px += (rng.Float64()*2 - 1) * jitterAmp
			py += (rng.Float64()*2 - 1) * jitterAmp
		}
		ops = append(ops, moveOp(int(math.Round(px)), int(math.Round(py))))
	}
	// Human-like overshoot: nudge just past the target along the travel direction, then
	// the exact correction below lands the cursor on the target.
	if human && overshoot >= 1 {
		ux, uy := dx/dist, dy/dist
		ops = append(ops, moveOp(int(math.Round(float64(to.X)+ux*overshoot)), int(math.Round(float64(to.Y)+uy*overshoot))))
	}
	// Final op: land EXACTLY on the target (never jittered) regardless of accuracy.
	ops = append(ops, moveOp(to.X, to.Y))
	return ops
}

// moveOps expands a move to (x,y) from the last commanded position. It does NOT record
// the new position — the caller commits it with setLast only after the portal op
// succeeds, so a denied/failed action never desyncs the mirror the bare-click guard
// trusts (plan 09 §7 / review H1).
func (s *pointerState) moveOps(x, y int, prof PointerProfile, rng *rand.Rand) []map[string]any {
	from, ok := s.last()
	return expandMove(from, ok, point{x, y}, prof, rng)
}

// movePathMs is how long the path expandMove emits keeps the cursor in flight. The op
// COUNT is independent of the jitter rng, so a throwaway seed gives the exact answer the
// real expansion will produce — the timeline compiler uses it to schedule a relative gap
// from the end of a motion rather than its start (audit F25), and nothing but scheduling
// depends on it (the overlap invariant itself is enforced against the real lowered ops).
func movePathMs(from point, haveFrom bool, to point, prof PointerProfile) int {
	ops := expandMove(from, haveFrom, to, prof, rand.New(rand.NewSource(1)))
	return opsSpanMs(ops)
}

// btnOp builds one pointer-button op (state 1 = press, 0 = release).
func btnOp(code uint32, state uint32) map[string]any {
	return map[string]any{"t": "btn", "button": code, "state": state}
}

// clickOps composes move→press→release (×count). The first press carries the settle
// delay (a pause after arrival before the button fires, plan 09 §4).
func clickOps(moveOps []map[string]any, code uint32, count, settleMs int) []map[string]any {
	if count < 1 {
		count = 1
	}
	ops := append([]map[string]any{}, moveOps...)
	for c := 0; c < count; c++ {
		press := btnOp(code, 1)
		if c == 0 && settleMs > 0 {
			press["delayMs"] = settleMs
		}
		ops = append(ops, press, btnOp(code, 0))
	}
	return ops
}

// scrollOp builds a discrete wheel op: axis 0 = vertical, 1 = horizontal; steps signed
// (sign = direction).
func scrollOp(axis, steps int) map[string]any {
	return map[string]any{"t": "axis_discrete", "axis": axis, "steps": steps}
}

// relMaxDelta bounds a single relative-motion delta per axis. Mouse-look turns are modest
// (even a full in-game spin is a few thousand px), so this caps abuse without clipping a
// real turn.
const relMaxDelta = 10000.0

// clampRelDelta bounds one axis of a relative delta to [-relMaxDelta, relMaxDelta] and
// scrubs NaN/Inf to 0 — a non-finite delta would wedge the compositor's pointer.
func clampRelDelta(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v > relMaxDelta {
		return relMaxDelta
	}
	if v < -relMaxDelta {
		return -relMaxDelta
	}
	return v
}

// relMoveOp builds one RELATIVE pointer-motion op: a raw dx/dy delta with no absolute
// target. Pointer-grabbing games read these as mouse-look. Unlike absolute "move" it needs
// no screencast stream and is not itself a guard point (it has no landing point) — but it
// DOES move the mirror, via applyRelative, because a relative move that left the mirror
// standing was a self-target bypass (audit F3; see applyRelative).
func relMoveOp(dx, dy float64) map[string]any {
	return map[string]any{"t": "move_rel", "dx": clampRelDelta(dx), "dy": clampRelDelta(dy)}
}

// relMoveOps splits a relative delta into `steps` (>=1, capped) evenly-timed sub-deltas so
// a large turn plays as smooth motion the game integrates frame-by-frame rather than one
// teleporting jump. The first op carries no delay (the UI ignores op[0]'s delay anyway).
func relMoveOps(dx, dy float64, steps int) []map[string]any {
	if steps < 1 {
		steps = 1
	}
	if steps > pointerMaxSteps {
		steps = pointerMaxSteps
	}
	dx, dy = clampRelDelta(dx), clampRelDelta(dy)
	delay := int(pointerStepDt * 1000)
	sx, sy := dx/float64(steps), dy/float64(steps)
	ops := make([]map[string]any, 0, steps)
	for i := 0; i < steps; i++ {
		op := relMoveOp(sx, sy)
		if i > 0 {
			op["delayMs"] = delay
		}
		ops = append(ops, op)
	}
	return ops
}

// dragOps composes move(from)→press(left)→move(to, stepped)→release(left). It threads
// the intermediate position (the cursor is at `from` once the first leg lands) without
// mutating the mirror — the caller commits the final position (to) after success.
func (s *pointerState) dragOps(from, to point, prof PointerProfile, rng *rand.Rand) []map[string]any {
	start, ok := s.last()
	ops := expandMove(start, ok, from, prof, rng)
	press := btnOp(0x110, 1)
	if prof.SettleMs > 0 {
		press["delayMs"] = prof.SettleMs
	}
	ops = append(ops, press)
	ops = append(ops, expandMove(from, true, to, prof, rng)...)
	ops = append(ops, btnOp(0x110, 0))
	return ops
}

// --- playback evidence: did the batch actually land where it was aimed? ---------------
//
// SECURITY (audit F3, absolute half): the mirror used to be committed on "the portal call
// returned without an error", which is not the same claim. The UI DROPS an absolute move
// whose point lies inside no captured screen (there is no stream node to address it to),
// and it can fail MID-PLAY, stranding the cursor somewhere along an interpolated path that
// is allowed to cross Agent Kate's own windows. Both leave the true cursor somewhere other
// than the commanded point while the mirror records the commanded point — the same bypass
// as stale relative motion, reachable with no relative motion at all.
//
// So the UI now reports what it actually played (kde.PortalResult.OpsDropped / PtrKnown /
// PtrX / PtrY) and the core believes THAT. Anything short of proof destroys the mirror.

// lastAbsMove returns the target of the last absolute "move" op in a batch — the position
// the cursor is supposed to end up at. Every expansion (expandMove, clickOps, dragOps, the
// timeline compiler) ends its motion with an EXACT, un-jittered move to the target, so this
// is the point the UI's report must match. Ops with no absolute move (bare clicks, scrolls
// at the cursor, keystrokes, pure relative motion) return false: there is nothing to prove.
func lastAbsMove(ops []map[string]any) (point, bool) {
	for i := len(ops) - 1; i >= 0; i-- {
		if t, _ := ops[i]["t"].(string); t == "move" {
			x, okx := ops[i]["x"].(int)
			y, oky := ops[i]["y"].(int)
			if !okx || !oky {
				return point{}, false // unreadable target — treat as unproven
			}
			return point{x, y}, true
		}
	}
	return point{}, false
}

// opsLandedAsAimed reports whether the UI's reply PROVES the batch reached the position it
// aimed for. Fails closed: a dropped op, an unproven pointer, or a landing point that is
// not the one requested all read as "no".
func opsLandedAsAimed(ops []map[string]any, res kde.PortalResult) bool {
	if res.OpsDropped > 0 {
		return false
	}
	want, ok := lastAbsMove(ops)
	if !ok {
		return true // the batch never claimed to move the pointer to a point
	}
	return res.PtrKnown && res.PtrX == want.X && res.PtrY == want.Y
}

// pointerPlay is what actually became of one batch of pointer ops, as far as the core can
// prove it: `played` means the ops were handed to the UI portal (so the cursor MAY have
// moved, including part-way), `landed` means the UI proved they finished on the requested
// point.
type pointerPlay struct {
	played bool
	landed bool
}

// commitPointer updates the mirror after an action that aimed to leave the cursor at want.
// The three outcomes are deliberately distinct:
//   - proven landing        → commit the exact point (the mirror is evidence, and this is it)
//   - played but unproven   → DESTROY the mirror; a guess is exactly what the self-target
//     guard may not run on, and the stranded cursor may be sitting
//     on an Agent Kate window
//   - never played (refused before the portal ran) → nothing moved, the mirror stands
func (s *pointerState) commitPointer(thread string, play pointerPlay, want point, haveWant bool) {
	switch {
	case play.landed && haveWant:
		s.setLast(thread, want)
	case play.played:
		s.invalidate(thread, mirrorLostUnproven)
	}
}

// playMirrorOutcome decides what a finished cowork.playInput does to the GLOBAL pointer
// mirror. Pure, so the fail-closed matrix is testable without a portal: commit means
// "record plan.FinalPos", a non-empty lostWhy means "destroy the mirror with that reason",
// and neither means "leave it exactly as it is".
//
// SECURITY (audit F26): a script with NO pointer events cannot have moved the cursor, so it
// must not write the mirror at all. Its FinalPos is merely the seed it was compiled with,
// and with one mirror shared by every thread, another thread may have moved — or destroyed
// — the real position while this call waited on consent. Re-committing the seed would
// resurrect a stale position as fresh evidence, which is the very bypass being closed.
func playMirrorOutcome(plan timelinePlan, landed bool) (commit bool, lostWhy string) {
	if !plan.HasPointer {
		return false, ""
	}
	switch {
	case plan.HaveFinal && landed:
		return true, ""
	case plan.HaveFinal:
		// The UI could not prove the batch finished on the point it aimed for: an aborted or
		// failed timeline strands the cursor mid-path (and an interpolated path is allowed to
		// cross Agent Kate's windows).
		return false, mirrorLostUnproven
	case plan.RelLost:
		// A relative nudge outran what the compiler could account for.
		return false, mirrorLostRelative
	}
	return false, ""
}

// bareClickRefusal is the message for a bare button/scroll whose landing point cannot be
// verified. `why` (a mirrorLost* constant, or "" for "never established") distinguishes the
// ways that happens, because "you never moved the pointer", "the pointer moved somewhere I
// can no longer account for" and "the move you asked for did not land" need different next
// steps from the agent — and none of them must read like a bug.
func bareClickRefusal(why string) string {
	switch why {
	case mirrorLostRelative:
		return "refused: the pointer was last moved by a RELATIVE nudge, so where a bare click would " +
			"land can no longer be verified (the compositor may have clamped it at a screen edge). " +
			"Use desktop_click(x,y), which targets and guards an exact point, or re-establish a known " +
			"position with desktop_move_pointer(x,y) first"
	case mirrorLostUnproven:
		return "refused: the last pointer action did not provably land where it was aimed (the desktop " +
			"could not apply the move — a point off every screen — or the playback stopped part-way), " +
			"so where a bare click would land can no longer be verified. Use desktop_click(x,y), which " +
			"targets and guards an exact point, or re-establish a known position with " +
			"desktop_move_pointer(x,y) to an on-screen point first"
	}
	return "refused: a bare click fires at the cursor's current position, which can't be verified safe — " +
		"use desktop_click(x,y) (it targets and guards an exact point), or move the pointer first with desktop_move_pointer"
}

// --- what an op stream actually does, derived from the stream ------------------------
//
// SECURITY (audit F25/F26 wiring): these two are the reason there is no guard closure left
// in any handler. The guard's real question is "where will the ops I am about to release
// press a button or turn a wheel?", and the ops are the only honest answer to it — a list
// handed in beside them can be wrong, or forgotten, and nothing downstream can tell.

// effectPoints walks an op stream and returns every absolute position at which it will have
// an EFFECT (a pointer button, a wheel notch), threading the cursor from `at` — the live
// mirror, read inside the guard→fire section. Consecutive duplicates are collapsed (a
// double-click and a two-axis scroll act at one place). Pure absolute motion produces NO
// points: passing over Agent Kate's windows has no side effect, and only landing on one
// does.
//
// It FAILS CLOSED (ok=false) whenever an effect op's position cannot be determined: no
// known starting position, a move op with an unreadable target, or a relative nudge that
// walked outside the known desktop (where the compositor clamps and the accumulation stops
// being true).
func effectPoints(ops []map[string]any, at point, haveAt bool, bounds kde.DesktopLayout) ([]point, bool) {
	var pts []point
	for _, op := range ops {
		switch kind, _ := op["t"].(string); kind {
		case "move":
			x, okx := op["x"].(int)
			y, oky := op["y"].(int)
			if !okx || !oky {
				return nil, false
			}
			at, haveAt = point{x, y}, true
		case "move_rel":
			if !haveAt {
				continue
			}
			dx, _ := op["dx"].(float64)
			dy, _ := op["dy"].(float64)
			to := point{at.X + int(math.Round(dx)), at.Y + int(math.Round(dy))}
			if !bounds.Contains(to.X, to.Y) {
				haveAt = false
				continue
			}
			at = to
		case "btn", "axis_discrete":
			if !haveAt {
				return nil, false
			}
			if n := len(pts); n == 0 || pts[n-1] != at {
				pts = append(pts, at)
			}
		}
	}
	return pts, true
}

// opsNeedSeed reports whether the stream ACTS somewhere before it names an absolute point —
// a bare click/button/scroll, or one that follows only relative motion. The position such
// an op fires at is inherited from the mirror, which is global and shared, so the section
// must re-prove that the mirror still says what it said when the ops were compiled and the
// human was prompted (audit F26). A stream that moves somewhere first names its own target
// and needs no seed.
func opsNeedSeed(ops []map[string]any) bool {
	for _, op := range ops {
		switch kind, _ := op["t"].(string); kind {
		case "move":
			return false
		case "btn", "axis_discrete":
			return true
		}
	}
	return false
}

// opsHaveRelative reports whether a stream contains relative motion — the only case whose
// derived positions need the desktop layout, and so the only case worth a KWin round trip.
func opsHaveRelative(ops []map[string]any) bool {
	for _, op := range ops {
		if kind, _ := op["t"].(string); kind == "move_rel" {
			return true
		}
	}
	return false
}

// samePointSet reports whether two point lists cover the same set of positions (order and
// repetition are not meaningful — a repeat fires many times at one place).
func samePointSet(a, b []point) bool {
	set := func(pts []point) map[point]bool {
		m := make(map[point]bool, len(pts))
		for _, p := range pts {
			m[p] = true
		}
		return m
	}
	ma, mb := set(a), set(b)
	if len(ma) != len(mb) {
		return false
	}
	for p := range ma {
		if !mb[p] {
			return false
		}
	}
	return true
}

// guardPointerTargets is the geometric self-target guard (plan 09 §7): it refuses if any
// action point (a click/scroll location) falls inside an Agent-Kate-owned window. It
// re-fetches live KWin geometry at execute time (windows move) and fails CLOSED if that
// geometry cannot be read — an unverifiable target is never clicked.
//
// SECURITY (audit F26): hold proves the caller is inside the guard→fire section. The
// verdict is only worth anything while nothing else can move the cursor, so a call site
// that never took the section has nothing to pass and FAILS CLOSED here rather than
// returning an answer that is already stale. The one deliberately advisory caller — the
// pre-consent courtesy check in cowork.injectInput, which exists so the human is not
// prompted for something that will be refused anyway — calls pointerTargetsClear directly.
func guardPointerTargets(geom cursorGeometry, hold *cursorHold, pts []point) error {
	if !hold.valid() {
		return fmt.Errorf("refused: the pointer target cannot be verified without exclusive control of the shared cursor, so the check would already be stale (internal: no cursor hold)")
	}
	return pointerTargetsClear(geom, pts)
}

// pointerTargetsClear is guardPointerTargets' body without the section proof. Only two
// callers may use it: guardPointerTargets itself, and an ADVISORY pre-consent preview.
func pointerTargetsClear(geom cursorGeometry, pts []point) error {
	if geom == nil {
		return fmt.Errorf("refused: the self-target guard is unavailable, so the pointer target cannot be verified as not Agent Kate's own UI")
	}
	rects, err := geom.Windows()
	if err != nil {
		return fmt.Errorf("refused: cannot read window geometry to verify the pointer target is not Agent Kate's own UI")
	}
	for _, p := range pts {
		if geom.IsSelfPoint(p.X, p.Y, rects) {
			return fmt.Errorf("refused: (%d,%d) is inside an Agent Kate window — the agent may not point at or click its own controls", p.X, p.Y)
		}
	}
	return nil
}
