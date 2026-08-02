package main

import (
	"encoding/json"
	"os"

	"agentkate/internal/schedule"
	"agentkate/internal/session"
)

// Rate-window auto-resume: the core side of plan 28 §Phase 2.
//
// A thread that hits the account's usage limit used to stop until a human came
// back and re-poked it, even though the engine had just told us the exact
// second the window reopens. This file is the "and then resume it" half:
// `rate_limit_event` goes into the waker (run.go), the waker arms a staggered
// wake at `resetsAt`, and when it matures fireRateWake resumes the thread
// through the ORDINARY resume path — the same resumeThread the human's Resume
// button drives, replaying the record's own persisted options.
//
// THE RULE (see internal/schedule's package doc): a scheduled or automatic
// action never carries more authority than the human granted the thread. This
// file is where it would be broken, so read it with that in mind:
//
//   - It never builds a harness.StartSpec. resumeThread builds it from the
//     record, so the thread resumes under its OWN persisted permission mode and
//     there is no expression here through which another one could be supplied.
//     Pinned by TestScheduledResumeUsesRecordPermissionMode.
//   - It never writes a permission mode, and the string "bypassPermissions"
//     appears nowhere in the scheduler surface. Pinned, source-level, by
//     TestNoSchedulerPathCanSetBypassPermissions.
//   - If the authority moved between arming and firing, it does NOT resume. It
//     skips, and says so.
//
// And the honesty half: every outcome — armed, cancelled, fired, skipped —
// is reported to the window. A resume that quietly does not happen would be
// worse than never promising one.

// authorityFingerprint is the opaque snapshot the waker carries from arm time
// to fire time. It is the thread's permission mode: the one property of a
// resumed turn that decides how much the agent may do without asking. If the
// human moves it while the thread is parked, the wake that was armed under the
// old answer is not the wake the human would arm now, so it is skipped rather
// than fired under an authority nobody agreed to at that moment.
func authorityFingerprint(rec session.Record) string {
	return rec.PermissionMode
}

// newRateWaker builds the waker for a running core. deps is captured by
// reference (runCore declares it before this is called and assigns it after),
// so the callbacks see the fully wired handlers.
func newRateWaker(deps *handlerDeps) *schedule.RateWaker {
	return schedule.NewRateWaker(schedule.Config{
		Fire: func(w schedule.Wake) { fireRateWake(*deps, w) },
		OnChange: func(w schedule.Wake, state, reason string) {
			notifyRateWake(*deps, w, state, reason)
		},
	})
}

// noteRateLimit feeds one `rate_limit_event` to the waker. Called from the
// event relay, on the hot path, so it does nothing but parse and hand over.
func noteRateLimit(d handlerDeps, waker *schedule.RateWaker, threadID string, info json.RawMessage) {
	if waker == nil {
		return
	}
	win, ok := schedule.ParseWindow(info)
	if !ok {
		return
	}
	fingerprint := ""
	if rec, found := d.sessions.Get(threadID); found {
		fingerprint = authorityFingerprint(rec)
	}
	waker.Note(threadID, win, fingerprint)
}

// rateWakeSkipReason is the whole decision: "" means resume, anything else is
// the sentence the human reads instead.
//
// It is split from the emission for the same reason AgentNotifier splits
// evaluate* from emitAlert — so the rule can be asserted in a test without a
// window, a socket or a notification server on the other end.
func rateWakeSkipReason(d handlerDeps, w schedule.Wake) string {
	rec, ok := d.sessions.Get(w.ThreadID)
	if !ok {
		// Closed, archived or discarded while it was parked.
		return "the agent is no longer here"
	}
	if d.agentRunning(w.ThreadID) {
		// The human (or a manual Resume) got there first.
		return "it is already running again"
	}
	if authorityFingerprint(rec) != w.Fingerprint {
		// THE RULE. The authority the resume was armed under is not the
		// authority the thread carries now, so this resume is not the one the
		// human agreed to. Refuse it and say so; they can resume it themselves
		// in one click, in the open.
		return "its \"when to ask\" setting changed after the resume was scheduled — " +
			"resume it yourself when you are ready"
	}
	if _, hok := d.harnesses.Get(rec.Backend); !hok {
		return "its engine is no longer registered"
	}
	if rec.SessionID == "" {
		return "it has no session to resume"
	}
	return missingProviderCredential(rec)
}

// fireRateWake performs one matured wake, or explains why it did not.
//
// A skip is ANNOUNCED. The alternative — failing silently — would leave a user
// who was told "resumes at 14:37" staring at an agent that never moved, which
// is a worse outcome than never having promised.
func fireRateWake(d handlerDeps, w schedule.Wake) {
	if reason := rateWakeSkipReason(d, w); reason != "" {
		notifyRateWake(d, w, schedule.StateSkipped, reason)
		return
	}
	rec, ok := d.sessions.Get(w.ThreadID)
	if !ok {
		return // raced with a discard between the check and here
	}
	h, hok := d.harnesses.Get(rec.Backend)
	if !hok {
		return
	}
	notifyRateWake(d, w, schedule.StateFired, "")
	// The ordinary resume path, with the record exactly as persisted. Note what
	// is NOT here: no StartSpec, no options, no mode. resumeThread replays the
	// thread's own authority because that is the only authority it has access
	// to (agents.go:194).
	resumeThread(d, h, rec, nil)
}

// missingProviderCredential reports why a third-party-routed thread cannot be
// resumed unattended, or "" when it can.
//
// A provider's API token is deliberately never persisted on the record: it is
// re-resolved from the environment at launch, or re-supplied by the window from
// KWallet. An automatic resume has no window to ask, so a wallet-held key is a
// case we must decline out loud rather than fail on a launch error the user has
// to decode.
func missingProviderCredential(rec session.Record) string {
	p := providerFromRecord(rec)
	if !p.Routed() || p.EnvVar == "" {
		return ""
	}
	if rec.Env[p.EnvVar] != "" || os.Getenv(p.EnvVar) != "" {
		return ""
	}
	return "its API key is held in the wallet, which only the Agent Kate window can unlock"
}

// notifyRateWake tells the human's window what the schedule just did.
//
// It rides the thread's own event channel as a `_ratewake` event, in the same
// batch shape as every other event, so the panel that already renders this
// thread's usage-limit chip renders this too — one wire contract, and the note
// lands in the conversation it belongs to. NotifyUI for the same reason the
// relay uses it: a thread's events are that thread's business and the human's.
func notifyRateWake(d handlerDeps, w schedule.Wake, state, reason string) {
	if d.srv == nil {
		return
	}
	ev := map[string]any{"type": "_ratewake", "state": state}
	if !w.At.IsZero() {
		ev["at"] = w.At.Unix()
	}
	if reason != "" {
		ev["reason"] = reason
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	d.srv.NotifyUI("agent.event", agentEventParams{
		ThreadID: w.ThreadID,
		Events:   []json.RawMessage{b},
	})
}
