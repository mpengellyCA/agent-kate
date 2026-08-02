package schedule

import (
	"sort"
	"sync"
	"time"
)

// RateWaker turns a rate-limit stall from "wait for a human" into "resumes at
// 14:37" (plan 28 §Phase 2).
//
// The one fact it needs already arrives on every turn: `rate_limit_event`
// carries the exact `resetsAt` of the window that just refused the work. Until
// now the machine knew when it could continue and did nothing with it.
//
// Two properties shape the design:
//
//   - Rate limits are ACCOUNT-WIDE, not per-thread. One shared window state,
//     one wake schedule. Wakes are keyed by thread because it is threads that
//     get resumed, but they are STAGGERED: several threads waking at the same
//     instant would re-exhaust the freshly reset window immediately, which is
//     the thundering herd this staggering exists to prevent.
//   - A wake fires ONCE. A laptop that slept through the reset must resume the
//     thread once on wake, not once per interval it slept through — the classic
//     bug in this class of feature.
//
// It never touches a session record, a harness or a permission mode; see the
// package rule. Firing calls back into the caller, which resumes the thread
// through the ordinary path with the thread's own persisted options.
type RateWaker struct {
	mu    sync.Mutex
	armed map[string]*armedWake

	now      func() time.Time
	newTimer func(time.Duration, func()) Timer
	stagger  time.Duration
	maxDelay time.Duration
	fire     func(Wake)
	onChange func(Wake, string, string)
	stopped  bool
}

// Timer is the slice of *time.Timer this package uses, so tests can drive the
// clock instead of waiting on it.
type Timer interface{ Stop() bool }

type armedWake struct {
	wake  Wake
	timer Timer
	fired bool
}

// Wake states reported through Config.OnChange, and the vocabulary the UI
// renders. They are deliberately explicit about the negative cases: a resume
// that silently does not happen is the failure mode this feature would
// otherwise introduce.
const (
	StateArmed     = "armed"     // a resume is scheduled for this thread
	StateCancelled = "cancelled" // it is no longer needed or no longer possible
	StateFired     = "fired"     // the resume is being performed now
	StateSkipped   = "skipped"   // it came due and was deliberately NOT performed
)

// Config wires a RateWaker. Every field has a working default except Fire.
type Config struct {
	// Fire performs the resume. It is called on its own goroutine, once per
	// wake, and must not block the caller's event path.
	Fire func(Wake)
	// OnChange reports every state transition (wake, state, reason) so the
	// human's window can show what is scheduled and, just as importantly, what
	// was skipped and why. Optional.
	OnChange func(w Wake, state, reason string)

	// Now/NewTimer exist for tests; production leaves them nil.
	Now      func() time.Time
	NewTimer func(time.Duration, func()) Timer

	// Stagger spaces consecutive wakes so a reset window is not re-exhausted
	// the instant it opens. Zero uses the default.
	Stagger time.Duration
	// MaxDelay refuses to arm a wake further out than this. A five-hour window
	// never resets more than five hours away, so a longer horizon means a
	// nonsense timestamp, and a promise we would very likely not keep.
	MaxDelay time.Duration
}

const (
	defaultStagger  = 45 * time.Second
	defaultMaxDelay = 24 * time.Hour
)

// NewRateWaker builds a waker. A nil Fire makes every matured wake a no-op,
// which is the fail-closed direction, not a panic.
func NewRateWaker(cfg Config) *RateWaker {
	w := &RateWaker{
		armed:    map[string]*armedWake{},
		now:      cfg.Now,
		newTimer: cfg.NewTimer,
		stagger:  cfg.Stagger,
		maxDelay: cfg.MaxDelay,
		fire:     cfg.Fire,
		onChange: cfg.OnChange,
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.newTimer == nil {
		w.newTimer = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if w.stagger <= 0 {
		w.stagger = defaultStagger
	}
	if w.maxDelay <= 0 {
		w.maxDelay = defaultMaxDelay
	}
	return w
}

// Note folds one thread's latest window report in.
//
// A blocked window arms (or re-times) that thread's wake; a serving window
// cancels it, because a thread whose engine is answering is not parked.
// fingerprint is the caller's opaque authority snapshot — see the package rule.
//
// It returns the armed wake and true when one is now scheduled.
func (r *RateWaker) Note(threadID string, w Window, fingerprint string) (Wake, bool) {
	if r == nil || threadID == "" {
		return Wake{}, false
	}
	if w.Serving() {
		r.Cancel(threadID, "the usage window is serving again")
		return Wake{}, false
	}
	if !w.Blocked() {
		return Wake{}, false
	}

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return Wake{}, false
	}
	now := r.now()
	// A window with no usable reset time gives us nothing to arm on. Say
	// nothing rather than pick an hour: the UI's honesty rule is that we never
	// claim a resume time we do not have.
	if w.ResetsAt.IsZero() {
		r.mu.Unlock()
		return Wake{}, false
	}
	// A reset that has already passed (the machine was asleep through it, or
	// the event is stale) is due NOW — the stagger below still spaces it.
	base := w.ResetsAt
	if base.Before(now) {
		base = now
	}
	// Re-noting a thread that is already armed keeps its existing slot unless
	// the reset time moved later or the authority snapshot changed — otherwise
	// every turn's repeat event would push the whole fleet's wakes further out.
	if prev, ok := r.armed[threadID]; ok && !prev.fired {
		if !prev.wake.At.Before(w.ResetsAt) && prev.wake.Fingerprint == fingerprint {
			existing := prev.wake
			r.mu.Unlock()
			return existing, true
		}
		prev.timer.Stop()
		delete(r.armed, threadID)
	}
	at := r.freeSlotLocked(base)
	if at.Sub(now) > r.maxDelay {
		r.mu.Unlock()
		return Wake{}, false
	}
	wake := Wake{ThreadID: threadID, At: at, Fingerprint: fingerprint}
	entry := &armedWake{wake: wake}
	r.armed[threadID] = entry
	entry.timer = r.newTimer(at.Sub(now), func() { r.mature(threadID) })
	onChange := r.onChange
	r.mu.Unlock()

	if onChange != nil {
		onChange(wake, StateArmed, "")
	}
	return wake, true
}

// freeSlotLocked returns the first time at or after base that no other wake
// already occupies, spaced by the stagger. Caller holds r.mu.
func (r *RateWaker) freeSlotLocked(base time.Time) time.Time {
	at := base
	for i := 0; i < len(r.armed)+1; i++ {
		clash := false
		for _, e := range r.armed {
			if !e.fired && e.wake.At.Equal(at) {
				clash = true
				break
			}
		}
		if !clash {
			return at
		}
		at = at.Add(r.stagger)
	}
	return at
}

// Cancel drops a thread's wake — the window reopened, the human resumed it
// themselves, the agent was closed. Cancelling something that was never armed
// is silent.
func (r *RateWaker) Cancel(threadID, reason string) {
	if r == nil || threadID == "" {
		return
	}
	r.mu.Lock()
	entry, ok := r.armed[threadID]
	if !ok || entry.fired {
		r.mu.Unlock()
		return
	}
	entry.timer.Stop()
	delete(r.armed, threadID)
	wake := entry.wake
	onChange := r.onChange
	r.mu.Unlock()

	if onChange != nil {
		onChange(wake, StateCancelled, reason)
	}
}

// mature runs when a wake comes due. The fired latch is what makes a wake fire
// exactly once even if the timer is delivered more than once (a resumed laptop
// draining timers it slept through).
func (r *RateWaker) mature(threadID string) {
	r.mu.Lock()
	entry, ok := r.armed[threadID]
	if !ok || entry.fired || r.stopped {
		r.mu.Unlock()
		return
	}
	entry.fired = true
	delete(r.armed, threadID)
	wake := entry.wake
	fire := r.fire
	r.mu.Unlock()

	if fire != nil {
		fire(wake)
	}
}

// Armed lists the wakes currently scheduled, soonest first. For the UI, and for
// the tests that pin the staggering.
func (r *RateWaker) Armed() []Wake {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Wake, 0, len(r.armed))
	for _, e := range r.armed {
		if !e.fired {
			out = append(out, e.wake)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ThreadID < out[j].ThreadID
		}
		return out[i].At.Before(out[j].At)
	})
	return out
}

// Stop disarms everything, for shutdown. Nothing fires afterwards.
func (r *RateWaker) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	for id, e := range r.armed {
		e.timer.Stop()
		delete(r.armed, id)
	}
}
