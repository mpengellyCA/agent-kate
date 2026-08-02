package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"agentkate/internal/cowork"
	"agentkate/internal/kde"
)

// --- F3 (main): a TIMED injection must be supervised for its whole span ---------------
//
// resolveInjectTarget is a point-in-time check taken before consent; a script may then
// play for up to 30 s (injectMaxSpanMs / timelineMaxSpanMs) while RemoteDesktop keeps
// injecting. injectFocusAbortReason is the per-activation verdict that cancels the
// remainder. Every case below must ABORT except the one where focus is provably still on
// the window the grant was issued for.

func grantedTarget() cowork.Target {
	return cowork.Target{
		Kind: cowork.TargetWindow, WindowID: "w-firefox",
		ResourceClass: "firefox", Label: "Docs — Firefox",
	}
}

func TestInjectFocusWatchAllowsTheGrantedWindow(t *testing.T) {
	auth := selfAuthority(t)
	ev := kde.ActiveWindowEvent{InternalID: "w-firefox", Caption: "Docs — Firefox", ResourceClass: "firefox", PID: 7}
	if r := injectFocusAbortReason(auth, grantedTarget(), ev); r != "" {
		t.Fatalf("focus still on the granted window must not abort, got %q", r)
	}
}

func TestInjectFocusWatchAbortsOnOwnWindowByClass(t *testing.T) {
	auth := selfAuthority(t)
	// The attack: a consent prompt raised by the very script that is playing takes focus.
	ev := kde.ActiveWindowEvent{
		InternalID: "w-ak", Caption: "Agent Kate — desktop CONTROL request",
		ResourceClass: "org.kde.agentkate", PID: 99,
	}
	r := injectFocusAbortReason(auth, grantedTarget(), ev)
	if r == "" {
		t.Fatal("an Agent Kate window taking focus mid-script must abort the remainder")
	}
	if !strings.Contains(r, "Agent Kate") {
		t.Fatalf("the abort reason must name the self-target case, got %q", r)
	}
}

func TestInjectFocusWatchAbortsOnOwnWindowByPID(t *testing.T) {
	auth := selfAuthority(t)
	// KWin reports no resourceClass: the PID is the only evidence and must be decisive.
	ev := kde.ActiveWindowEvent{InternalID: "w-ak2", ResourceClass: "", PID: 4242}
	if r := injectFocusAbortReason(auth, grantedTarget(), ev); r == "" {
		t.Fatal("PID evidence alone must abort a running script")
	}
}

func TestInjectFocusWatchAbortsOnAnyOtherWindow(t *testing.T) {
	auth := selfAuthority(t)
	ev := kde.ActiveWindowEvent{InternalID: "w-term", Caption: "Konsole", ResourceClass: "konsole", PID: 11}
	if r := injectFocusAbortReason(auth, grantedTarget(), ev); r == "" {
		t.Fatal("focus moving to a window the grant does not cover must abort")
	}
}

func TestInjectFocusWatchFailsClosed(t *testing.T) {
	auth := selfAuthority(t)
	cases := map[string]struct {
		auth *cowork.Authority
		ev   kde.ActiveWindowEvent
	}{
		"script error":       {auth, kde.ActiveWindowEvent{Error: "workspace is gone"}},
		"unidentifiable":     {auth, kde.ActiveWindowEvent{InternalID: "", ResourceClass: "", PID: 0}},
		"no self identity":   {nil, kde.ActiveWindowEvent{InternalID: "w-firefox", PID: 7}},
		"malformed activate": {auth, kde.ActiveWindowEvent{Error: "malformed activation report"}},
	}
	for name, c := range cases {
		if r := injectFocusAbortReason(c.auth, grantedTarget(), c.ev); r == "" {
			t.Fatalf("%s: an unverifiable focus must abort (fail closed), got no reason", name)
		}
	}
}

// --- which batches need the watch at all ---------------------------------------------

func TestOpsSpanMsIgnoresTheFirstOpsDelay(t *testing.T) {
	// The player starts its timer at 0, so op[0]'s delay is never waited on. A batch whose
	// only delay is on op[0] runs synchronously and needs no supervision.
	ops := []map[string]any{
		{"t": "key", "keysym": 97, "delayMs": 5000},
		{"t": "key", "keysym": 97},
	}
	if got := opsSpanMs(ops); got != 0 {
		t.Fatalf("op[0] delay must not count toward the span, got %d", got)
	}
	ops[1]["delayMs"] = 250
	if got := opsSpanMs(ops); got != 250 {
		t.Fatalf("span = 250, got %d", got)
	}
}

func TestOpsSpanMsSumsRealDelays(t *testing.T) {
	ops := []map[string]any{
		{"t": "key"}, {"t": "key", "delayMs": 100}, {"t": "move", "delayMs": 900},
	}
	if got := opsSpanMs(ops); got != 1000 {
		t.Fatalf("span = 1000, got %d", got)
	}
}

func TestOpsHaveKeyOnlyForKeystrokes(t *testing.T) {
	pointerOnly := []map[string]any{{"t": "move"}, {"t": "button"}, {"t": "axis"}}
	if opsHaveKey(pointerOnly) {
		t.Fatal("a pointer-only batch has no keyboard target to supervise")
	}
	mixed := append(append([]map[string]any{}, pointerOnly...), map[string]any{"t": "key"})
	if !opsHaveKey(mixed) {
		t.Fatal("a batch containing one key op must be treated as typing")
	}
}

// A timed TYPING batch is the case the resident watch was built for, but it is no longer
// the only supervised one.
//
// SECURITY (audit F50): the handlers used to gate supervision on `HasKey && span > 0`. A
// span-0 batch is played synchronously by the UI — but on FIRST use it can sit QUEUED
// behind the RemoteDesktop portal handshake, so by the time it actually plays, the
// post-consent focus re-verification is stale and nothing is watching.
//
// injectSupervisorKind is that decision, and both handlers call it (through
// startInjectSupervision). WHETHER a batch is supervised turns on `typing` alone; the span
// picks only the MECHANISM. Any span condition on the first question — however spelled —
// makes an immediate typing batch come back supervisorNone here.
func TestEveryTypingBatchIsSupervised(t *testing.T) {
	instantTyping := []map[string]any{{"t": "key"}, {"t": "key"}}
	if !opsHaveKey(instantTyping) || opsSpanMs(instantTyping) != 0 {
		t.Fatal("precondition: this batch types with no span")
	}
	if k := injectSupervisorKind(opsHaveKey(instantTyping), opsSpanMs(instantTyping)); k == supervisorNone {
		t.Fatal("an IMMEDIATE typing batch must still be supervised — queued behind the portal handshake it plays against a stale focus check (audit F50)")
	}

	timedTyping := []map[string]any{{"t": "key"}, {"t": "key", "delayMs": 3000}}
	if !opsHaveKey(timedTyping) || opsSpanMs(timedTyping) == 0 {
		t.Fatal("precondition: this batch types and spans wall-clock time")
	}
	cases := []struct {
		name   string
		typing bool
		span   int
		want   string
	}{
		// The mechanism split (regression A2): 30 s of wall clock earns a resident KWin
		// activation watch; a single keystroke must not have to install one, so it polls.
		{"timed typing", true, 3000, supervisorWatch},
		{"immediate typing", true, 0, supervisorPoll},
		// A batch that types nothing has no keyboard target to supervise: its guard is
		// geometric and does not depend on focus at all.
		{"timed pointer-only", false, 5000, supervisorNone},
		{"immediate pointer-only", false, 0, supervisorNone},
	}
	for _, c := range cases {
		if got := injectSupervisorKind(c.typing, c.span); got != c.want {
			t.Fatalf("%s: supervisor = %q, want %q", c.name, got, c.want)
		}
	}
}

// The poll supervisor is the mechanism an IMMEDIATE typing batch gets, and it must reach
// the same verdicts as the resident watch — including on every "we could not tell".
//
// SECURITY (audit F50 / regression A2): establishing it cannot fail, which is the point —
// a single keystroke is no longer refused because KWin exposes no activation signal. So
// fail-closed has to live in the readings, and each unreadable case has to abort with the
// reason that fits it, not merely abort by accident.
func TestPollSupervisorReadingsMatchTheWatch(t *testing.T) {
	auth := selfAuthority(t)
	tgt := grantedTarget()
	granted := kde.Window{
		InternalID: "w-firefox", Caption: "Docs — Firefox", ResourceClass: "firefox",
		PID: 7, Active: true,
	}

	if r := injectFocusAbortReason(auth, tgt, activeWindowEventFrom([]kde.Window{granted}, nil)); r != "" {
		t.Fatalf("focus still on the granted window must not abort, got %q", r)
	}

	// KWin unreadable: not "focus moved", but "we can no longer verify" — fail closed.
	r := injectFocusAbortReason(auth, tgt, activeWindowEventFrom(nil, errors.New("kwin script failed")))
	if r == "" {
		t.Fatal("a window list that cannot be read must abort the batch")
	}
	if !strings.Contains(r, "could no longer be verified") {
		t.Fatalf("an unreadable window list must abort as an UNVERIFIABLE focus, got %q", r)
	}

	if r := injectFocusAbortReason(auth, tgt, activeWindowEventFrom([]kde.Window{}, nil)); r == "" {
		t.Fatal("no identifiable focused window must abort the batch")
	}
	// A minimized window is not the focused window, however KWin flags it.
	minimized := granted
	minimized.Minimized = true
	if r := injectFocusAbortReason(auth, tgt, activeWindowEventFrom([]kde.Window{minimized}, nil)); r == "" {
		t.Fatal("a minimized target is not proof the keystrokes still land on it")
	}
	// The sharp case: our own window took focus mid-batch.
	ak := kde.Window{InternalID: "w-ak", ResourceClass: "org.kde.agentkate", PID: 99, Active: true}
	if r := injectFocusAbortReason(auth, tgt, activeWindowEventFrom([]kde.Window{ak}, nil)); !strings.Contains(r, "Agent Kate") {
		t.Fatalf("an Agent Kate window taking focus must abort as the self-target case, got %q", r)
	}
	// An ordinary other window.
	other := kde.Window{InternalID: "w-term", Caption: "Konsole", ResourceClass: "konsole", PID: 11, Active: true}
	if r := injectFocusAbortReason(auth, tgt, activeWindowEventFrom([]kde.Window{other}, nil)); r == "" {
		t.Fatal("focus moving to a window the grant does not cover must abort")
	}
}

// Belt and braces on top of the behavioural cases above: both typing handlers must reach
// supervision through the one decision, so a future edit cannot re-gate one of them alone.
func TestBothTypingHandlersUseTheSupervisionDecision(t *testing.T) {
	src, err := os.ReadFile("cowork.go")
	if err != nil {
		t.Fatalf("read cowork.go: %v", err)
	}
	if n := strings.Count(string(src), "startInjectSupervision(cw, target,"); n != 2 {
		t.Fatalf("expected both typing paths (injectInput, playInput) to establish supervision, found %d", n)
	}
}
