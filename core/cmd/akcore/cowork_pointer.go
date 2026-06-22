package main

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"agentkate/internal/cowork"
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
type pointerState struct {
	mu        sync.Mutex
	bounds    PointerProfile
	perThread map[string]PointerProfile
	lastPos   map[string]point
}

func newPointerState() *pointerState {
	return &pointerState{
		bounds:    defaultPointerProfile(),
		perThread: map[string]PointerProfile{},
		lastPos:   map[string]point{},
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

func (s *pointerState) setLast(thread string, p point) {
	s.mu.Lock()
	s.lastPos[thread] = p
	s.mu.Unlock()
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
