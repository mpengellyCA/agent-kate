package schedule

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// The rate-window waker is the machinery behind "resumes at 14:37". These pin
// the four properties the feature is worthless (or harmful) without: it arms
// only on a real block, it staggers so a reopened window is not re-exhausted by
// the whole fleet at once, it stops promising once the account is serving
// again, and a wake fires EXACTLY once even when the machine slept through it.

// --- a clock the test drives ------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock   *fakeClock
	delay   time.Duration
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *fakeClock) newTimer(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, delay: d, fn: f}
	c.timers = append(c.timers, t)
	return t
}

// deliver runs every live timer callback, as the OS would when the deadlines
// pass. Callable twice: that is the point of TestAWakeFiresExactlyOnce.
func (c *fakeClock) deliver() {
	c.mu.Lock()
	live := make([]*fakeTimer, 0, len(c.timers))
	for _, t := range c.timers {
		if !t.stopped {
			live = append(live, t)
		}
	}
	c.mu.Unlock()
	for _, t := range live {
		t.fn()
	}
}

func newTestWaker(t *testing.T) (*RateWaker, *fakeClock, *[]Wake) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	fired := []Wake{}
	w := NewRateWaker(Config{
		Now:      func() time.Time { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.now },
		NewTimer: clock.newTimer,
		Stagger:  30 * time.Second,
		Fire: func(k Wake) {
			mu.Lock()
			fired = append(fired, k)
			mu.Unlock()
		},
	})
	return w, clock, &fired
}

func blocked(resets time.Time) Window {
	return Window{Status: "rejected", Type: "five_hour", ResetsAt: resets}
}

// Three threads stall on the SAME account-wide window. They must not all wake
// at the same instant: the reopened window would be re-exhausted immediately
// and the fleet would stall again, which is the thundering herd the stagger
// exists to prevent.
func TestRateLimitArmsStaggeredWakes(t *testing.T) {
	w, clock, _ := newTestWaker(t)
	resets := clock.now.Add(90 * time.Minute)

	for _, id := range []string{"t-1", "t-2", "t-3"} {
		if _, ok := w.Note(id, blocked(resets), "default"); !ok {
			t.Fatalf("%s: a blocked window with a known reset time armed nothing", id)
		}
	}

	armed := w.Armed()
	if len(armed) != 3 {
		t.Fatalf("armed %d wakes for 3 stalled threads: %+v", len(armed), armed)
	}
	seen := map[time.Time]string{}
	for _, k := range armed {
		if k.At.Before(resets) {
			t.Errorf("%s wakes at %v, BEFORE the window resets at %v — it would be "+
				"refused again immediately", k.ThreadID, k.At, resets)
		}
		if other, clash := seen[k.At]; clash {
			t.Errorf("%s and %s both wake at %v; the stagger did not space them",
				other, k.ThreadID, k.At)
		}
		seen[k.At] = k.ThreadID
	}
}

// The window reopened early, or overage was switched on: the engine says
// "allowed" and there is nothing left to wait for.
func TestAllowedStatusCancelsWakes(t *testing.T) {
	w, clock, fired := newTestWaker(t)
	resets := clock.now.Add(time.Hour)
	w.Note("t-1", blocked(resets), "default")
	if len(w.Armed()) != 1 {
		t.Fatal("nothing armed to cancel")
	}

	w.Note("t-1", Window{Status: "allowed", Type: "five_hour"}, "default")

	if got := w.Armed(); len(got) != 0 {
		t.Fatalf("a serving window left %d wake(s) armed: %+v", len(got), got)
	}
	clock.deliver()
	if len(*fired) != 0 {
		t.Fatalf("a cancelled wake still fired: %+v", *fired)
	}
}

// "allowed_warning" means the account is close to its limit while STILL being
// served. The thread is working, not parked — arming a resume for it would
// resume a turn nobody was waiting for.
func TestApproachingTheLimitArmsNothing(t *testing.T) {
	w, clock, _ := newTestWaker(t)
	if _, ok := w.Note("t-1", Window{
		Status: "allowed_warning", Type: "five_hour", ResetsAt: clock.now.Add(time.Hour),
	}, "default"); ok {
		t.Fatal("armed a resume for a thread that was never stopped")
	}
	if got := w.Armed(); len(got) != 0 {
		t.Fatalf("armed %+v for a warning", got)
	}
}

// The laptop slept through the reset and the OS delivers the timer more than
// once (or a re-note races the maturity). One wake, one resume — not one per
// interval slept through. This is the classic bug in this feature.
func TestAWakeFiresExactlyOnce(t *testing.T) {
	w, clock, fired := newTestWaker(t)
	w.Note("t-1", blocked(clock.now.Add(30*time.Minute)), "default")

	clock.deliver()
	clock.deliver()

	if len(*fired) != 1 {
		t.Fatalf("a single wake fired %d times: %+v", len(*fired), *fired)
	}
	if (*fired)[0].ThreadID != "t-1" {
		t.Fatalf("fired the wrong thread: %+v", (*fired)[0])
	}
	if got := w.Armed(); len(got) != 0 {
		t.Fatalf("a fired wake stayed armed: %+v", got)
	}
}

// A reset time that already passed is due now, not never: the window the engine
// refused us on has since reopened.
func TestAPassedResetIsDueImmediately(t *testing.T) {
	w, clock, _ := newTestWaker(t)
	k, ok := w.Note("t-1", blocked(clock.now.Add(-2*time.Hour)), "default")
	if !ok {
		t.Fatal("a window that has already reset armed nothing to resume")
	}
	if k.At.Before(clock.now) {
		t.Fatalf("armed in the past: %v < %v", k.At, clock.now)
	}
}

// No usable reset time means we do not know when this clears. Say nothing: the
// UI's honesty rule is that it never claims a resume time we do not have.
func TestNoResetTimeArmsNothing(t *testing.T) {
	w, _, _ := newTestWaker(t)
	if _, ok := w.Note("t-1", Window{Status: "rejected", Type: "five_hour"}, "default"); ok {
		t.Fatal("armed a wake with no reset time — at what hour?")
	}
}

// The repeat events of a parked thread must not push its own wake further out
// on every turn, or a stalled fleet's resume time would drift forever.
func TestRepeatedReportsKeepTheSameSlot(t *testing.T) {
	w, clock, _ := newTestWaker(t)
	resets := clock.now.Add(time.Hour)
	first, _ := w.Note("t-1", blocked(resets), "default")
	again, _ := w.Note("t-1", blocked(resets), "default")
	if !first.At.Equal(again.At) {
		t.Fatalf("a repeat report moved the wake from %v to %v", first.At, again.At)
	}
	if got := w.Armed(); len(got) != 1 {
		t.Fatalf("a repeat report armed a second wake: %+v", got)
	}
}

// Shutdown disarms everything: a wake maturing into a half-torn-down core would
// relaunch a thread the user just stopped.
func TestStopDisarmsEverything(t *testing.T) {
	w, clock, fired := newTestWaker(t)
	w.Note("t-1", blocked(clock.now.Add(time.Hour)), "default")
	w.Stop()
	clock.deliver()
	if len(*fired) != 0 {
		t.Fatalf("a wake fired after Stop: %+v", *fired)
	}
}

// resetsAt is unix seconds on some builds and ISO-8601 on others. Reading it
// wrong would arm the resume at the wrong hour, so both shapes are pinned —
// and anything unreadable yields no time at all rather than a guess.
func TestParseWindowAcceptsBothTimestampShapes(t *testing.T) {
	want := time.Unix(1785000000, 0).UTC()
	numeric, ok := ParseWindow(json.RawMessage(
		`{"status":"rejected","rateLimitType":"five_hour","resetsAt":1785000000}`))
	if !ok || !numeric.ResetsAt.Equal(want) {
		t.Fatalf("unix seconds parsed as %v (ok=%v)", numeric.ResetsAt, ok)
	}
	iso, ok := ParseWindow(json.RawMessage(
		`{"status":"rejected","rateLimitType":"five_hour","resetsAt":"` +
			want.Format(time.RFC3339) + `"}`))
	if !ok || !iso.ResetsAt.Equal(want) {
		t.Fatalf("ISO-8601 parsed as %v (ok=%v)", iso.ResetsAt, ok)
	}
	junk, ok := ParseWindow(json.RawMessage(`{"status":"rejected","resetsAt":"soon-ish"}`))
	if !ok {
		t.Fatal("an unreadable timestamp discarded the whole window")
	}
	if !junk.ResetsAt.IsZero() {
		t.Fatalf("invented a reset time %v from unparseable text", junk.ResetsAt)
	}
	if _, ok := ParseWindow(json.RawMessage(`{}`)); ok {
		t.Fatal("an empty rate_limit_info reported a window")
	}
}
