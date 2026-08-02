// Package schedule owns Agent Kate's timed and automatic actions — work that
// happens because a clock said so rather than because a human pressed
// something.
//
// # THE RULE THIS PACKAGE EXISTS TO ENFORCE
//
//	A scheduled or automatic action never carries more authority than the
//	human granted the thread.
//
// This is the whole reason the feature lives here instead of in a systemd
// timer. The workaround it replaces — a user unit running
// `claude --resume … --permission-mode bypassPermissions` when the rate window
// reopens — is a self-escalation: work resumes with MORE authority than the
// human granted, at an hour when nobody is watching. A permission classifier is
// right to block that, and the correct answer is not to route around it but to
// make the capability exist WITH the gate.
//
// Concretely, and structurally rather than by convention:
//
//   - Nothing in this package can express a permission mode. A Wake carries a
//     thread id, a time, and an opaque Fingerprint the caller minted and only
//     the caller can interpret. There is no field, parameter or option through
//     which a mode, a flag or a launch option could travel, so the scheduler
//     cannot widen a thread's authority even by mistake.
//   - Firing is a callback into the ordinary resume path, which replays the
//     thread's own persisted record. The scheduler chooses WHEN, never WHAT
//     WITH.
//   - The Fingerprint is how "the human changed their mind" is honoured: the
//     caller mints it from the authority that must not have moved, and compares
//     it again at fire time. A mismatch is a SKIP, reported with a reason — an
//     automatic action that quietly runs under different authority than it was
//     armed under is exactly the failure this rule forbids.
//
// Both properties are pinned by tests (see cmd/akcore/ratewake_test.go:
// TestScheduledResumeUsesRecordPermissionMode and
// TestNoSchedulerPathCanSetBypassPermissions), because plan 28 requires this
// rule to be enforced by a test rather than a comment.
//
// Phase 2 (rate-window auto-resume) is what is implemented today: the timer
// store and the general scheduler loop are Phase 1, still to come.
package schedule

import (
	"encoding/json"
	"strings"
	"time"
)

// Window is one `rate_limit_event` observation: the account's usage state as
// the engine last reported it. Every engine emits one per turn, so this arrives
// constantly and unconditionally — no flag, no probe.
type Window struct {
	Status   string    // "allowed" / "allowed_warning" / "rejected" / …
	Type     string    // the window the limit covers, e.g. "five_hour"
	ResetsAt time.Time // zero when the event carried no usable timestamp
}

// Blocked reports whether this window actually STOPS work, which is the only
// state worth arming a resume for.
//
// "allowed_warning" is deliberately NOT blocked: it means the account is
// approaching its limit while still being served, so the thread is working, not
// parked. Arming a resume for a thread that never stopped would resume a turn
// nobody was waiting for.
func (w Window) Blocked() bool {
	switch strings.TrimSpace(w.Status) {
	case "", "allowed", "allowed_warning":
		return false
	}
	return true
}

// Serving reports whether the account is being served again — the signal that
// cancels an outstanding wake for that thread, since a thread whose events say
// "allowed" is evidently not stalled.
func (w Window) Serving() bool {
	switch strings.TrimSpace(w.Status) {
	case "allowed", "allowed_warning":
		return true
	}
	return false
}

// Wake is one armed automatic resume.
//
// Fingerprint is OPAQUE to this package (see the rule above): the caller mints
// it from whatever authority must not have changed between arming and firing,
// and compares it itself when the wake fires. The scheduler only carries it.
type Wake struct {
	ThreadID    string
	At          time.Time
	Fingerprint string
}

// ParseWindow reads a `rate_limit_info` object off the wire. It is here, and
// not at the call site, because the timestamp has two shapes in the wild — unix
// seconds on some builds, ISO-8601 on others — and a wrong reset time would
// arm a resume at the wrong hour. An unreadable object returns false: no
// window, no wake, rather than a guess.
func ParseWindow(raw json.RawMessage) (Window, bool) {
	if len(raw) == 0 {
		return Window{}, false
	}
	var info struct {
		Status        string          `json:"status"`
		RateLimitType string          `json:"rateLimitType"`
		ResetsAt      json.RawMessage `json:"resetsAt"`
	}
	if json.Unmarshal(raw, &info) != nil || strings.TrimSpace(info.Status) == "" {
		return Window{}, false
	}
	return Window{
		Status:   info.Status,
		Type:     info.RateLimitType,
		ResetsAt: parseResetsAt(info.ResetsAt),
	}, true
}

// parseResetsAt accepts both wire shapes and returns the zero time for anything
// it cannot read — "we do not know when this clears" is a fact the caller must
// be able to see, and an invented time is worse than none.
func parseResetsAt(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var secs float64
	if json.Unmarshal(raw, &secs) == nil && secs > 0 {
		return time.Unix(int64(secs), 0).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
