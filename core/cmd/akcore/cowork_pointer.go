package main

import (
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
// the last commanded pointer position per thread (the m_ptr mirror — the UI tracks its
// own, but the core needs a start point to expand a path). All access is mutex-guarded.
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
	lastPos   map[string]point
	// lostWhy records WHY a thread's mirror was DESTROYED (one of the mirrorLost*
	// constants), so the refusal can say what happened — and steer the agent to the fix —
	// instead of claiming the pointer was never moved. Absent = never established.
	lostWhy map[string]string

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
		lastPos:   map[string]point{},
		lostWhy:   map[string]string{},
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

func (s *pointerState) last(thread string) (point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.lastPos[thread]
	return p, ok
}

// setLast records an EXACT commanded position (absolute move/click/drag/scroll). It also
// clears the relative-drift mark: an absolute move re-establishes a known position, which
// is the documented way back from a mirror that relative motion invalidated.
func (s *pointerState) setLast(thread string, p point) {
	s.mu.Lock()
	s.lastPos[thread] = p
	delete(s.lostWhy, thread)
	s.mu.Unlock()
}

// invalidate destroys the mirror for a thread: after this, every guard that depends on
// knowing where the cursor is refuses until an absolute move re-establishes it. why is one
// of the mirrorLost* constants and only shapes the refusal wording.
func (s *pointerState) invalidate(thread, why string) {
	s.mu.Lock()
	delete(s.lastPos, thread)
	s.lostWhy[thread] = why
	s.mu.Unlock()
}

// mirrorLoss reports why the mirror is missing: one of the mirrorLost* constants, or ""
// when it was simply never established (no positioned action this session).
func (s *pointerState) mirrorLoss(thread string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lostWhy[thread]
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
	from, ok := s.lastPos[thread]
	if !ok {
		// Nothing to drift: the mirror was already unknown, and a relative move cannot
		// establish one. Mark it as relative loss anyway so the refusal names the cause.
		s.lostWhy[thread] = mirrorLostRelative
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
		delete(s.lastPos, thread)
		s.lostWhy[thread] = mirrorLostRelative
		return point{}, false
	}
	s.lastPos[thread] = to
	delete(s.lostWhy, thread)
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

// moveOps expands a move to (x,y) from the thread's last commanded position. It does
// NOT record the new position — the caller commits it with setLast only after the portal
// op succeeds, so a denied/failed action never desyncs the mirror the bare-click guard
// trusts (plan 09 §7 / review H1).
func (s *pointerState) moveOps(thread string, x, y int, prof PointerProfile, rng *rand.Rand) []map[string]any {
	from, ok := s.last(thread)
	return expandMove(from, ok, point{x, y}, prof, rng)
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
func (s *pointerState) dragOps(thread string, from, to point, prof PointerProfile, rng *rand.Rand) []map[string]any {
	start, ok := s.last(thread)
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

// guardPointerTargets is the geometric self-target guard (plan 09 §7): it refuses if any
// action point (a click/scroll location) falls inside an Agent-Kate-owned window. It
// re-fetches live KWin geometry at execute time (windows move) and fails CLOSED if that
// geometry cannot be read — an unverifiable target is never clicked.
func guardPointerTargets(cw *cowork.Service, pts []point) error {
	wins, err := cw.KDE().ListWindows(4 * time.Second)
	if err != nil {
		return fmt.Errorf("refused: cannot read window geometry to verify the pointer target is not Agent Kate's own UI")
	}
	rects := make([]cowork.WindowRect, 0, len(wins))
	for _, w := range wins {
		rects = append(rects, cowork.WindowRect{
			X: w.X, Y: w.Y, W: w.Width, H: w.Height, PID: w.PID, ResourceClass: w.ResourceClass,
		})
	}
	for _, p := range pts {
		if cw.IsSelfPoint(p.X, p.Y, rects) {
			return fmt.Errorf("refused: (%d,%d) is inside an Agent Kate window — the agent may not point at or click its own controls", p.X, p.Y)
		}
	}
	return nil
}
