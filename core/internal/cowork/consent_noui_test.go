package cowork

import (
	"context"
	"strings"
	"testing"
	"time"
)

// zeroNotifier models a bus with NO UI connection at all: the frame is queued
// to nobody. Distinct from silentNotifier (delivered, human never answers) —
// F53's refusal keys on zero deliveries, never on silence.
type zeroNotifier struct{}

func (zeroNotifier) Notify(string, any)       {}
func (zeroNotifier) NotifyUI(string, any) int { return 0 }

// TestAuthorizeRefusesPromptlyWithNoUIToAsk (audit F35 pass 3 / F53): Authorize
// pushed cowork.grantRequested and then parked on the consent timer whether or
// not any window had received it. With no UI connected that held the caller —
// a bridge handler goroutine — for minutes on a question nothing will ever
// display, ending in the same refusal it could have given at once.
//
// It fails closed either way, so the assertion is on the CLOCK as much as on
// the answer: the prompt timeout is restored to a human scale here precisely
// so a regression shows up as this test's own 5-second guard firing rather
// than as a slow pass.
func TestAuthorizeRefusesPromptlyWithNoUIToAsk(t *testing.T) {
	svc := newTestService(t, zeroNotifier{})
	svc.Authority.promptTimeoutR0R1 = 30 * time.Second

	start := time.Now()
	done := make(chan Decision, 1)
	go func() {
		d, err := svc.Authorize(context.Background(), AuthRequest{
			ThreadID: "t", Capability: CapScreenshot,
			Target: Target{Kind: TargetWindow, WindowID: "w", ResourceClass: "org.mozilla.firefox"}})
		if err != nil {
			t.Errorf("Authorize: %v", err)
		}
		done <- d
	}()

	var d Decision
	select {
	case d = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Authorize parked on the consent timeout with no UI connected to ask")
	}
	if d.Allow {
		t.Fatal("no window connected must fail closed, not allow")
	}
	if !strings.Contains(d.Reason, "no Agent Kate window is connected") {
		t.Errorf("Reason = %q, want a refusal that NAMES the missing window", d.Reason)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the refusal took %s; with nobody to ask it must be immediate", elapsed)
	}
	// The abandoned broker request must not linger as an open prompt: it would
	// trip the capture-while-prompt-open guard for every later screenshot.
	if n := svc.Authority.broker.open(); n != 0 {
		t.Errorf("%d consent prompt(s) left open after the no-UI refusal", n)
	}
}
