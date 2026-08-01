package main

import (
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

// A timed TYPING batch is exactly the combination the watch exists for; the guard in the
// handlers is `HasKey && span > 0`, so pin both halves of that predicate.
func TestTimedTypingIsTheSupervisedCase(t *testing.T) {
	timedTyping := []map[string]any{{"t": "key"}, {"t": "key", "delayMs": 3000}}
	if !(opsHaveKey(timedTyping) && opsSpanMs(timedTyping) > 0) {
		t.Fatal("a delayed keystroke batch must require the activation watch")
	}
	instantTyping := []map[string]any{{"t": "key"}, {"t": "key"}}
	if opsSpanMs(instantTyping) > 0 {
		t.Fatal("an instant typing batch leaves no window for focus to move and must not need the watch")
	}
}
