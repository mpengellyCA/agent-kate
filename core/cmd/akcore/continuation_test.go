package main

import (
	"sync"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/session"
)

type continuationHarness struct {
	*fakeHarness
	mu   sync.Mutex
	sent []string
}

func (h *continuationHarness) Descriptor() harness.HarnessDescriptor {
	return harness.HarnessDescriptor{
		ContractVersion: harness.ContractVersion, ID: "continue-test", DisplayName: "Continuation test",
		Health:  harness.HealthOK,
		Interop: harness.InteroperabilityMatrix{Continuation: harness.InteropManaged},
	}
}

func (h *continuationHarness) Send(_ string, text string, _ []agent.Attachment) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, text)
	return nil
}

func (h *continuationHarness) sends() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sent...)
}

func TestBoundedContinuationUsesOnlyResultBudget(t *testing.T) {
	store := testSessions(t)
	rec := session.Record{ThreadID: "t-continue", Project: "/p", Backend: "continue-test", Created: time.Now(),
		Continuation: session.ContinuationPolicy{Enabled: true, MaxTurns: 2}}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	h := &continuationHarness{fakeHarness: &fakeHarness{}}
	registry := harness.NewRegistry("continue-test")
	registry.Register(h)
	turns := agent.NewTurnTracker()
	d := handlerDeps{sessions: store, harnesses: registry, turns: turns, humanQueue: newHumanSendQueue()}

	// A generic idle/failure event must not start work. Only a completed result
	// calls the controller in the production relay.
	d.continueAfterResult("t-continue")
	d.humanQueue.drainOne(d, "t-continue")
	d.continueAfterResult("t-continue")
	d.humanQueue.drainOne(d, "t-continue")
	d.continueAfterResult("t-continue")
	got := h.sends()
	if len(got) != 2 || got[0] != continuationPrompt || got[1] != continuationPrompt {
		t.Fatalf("automatic sends = %#v", got)
	}
	updated, _ := store.Get("t-continue")
	if updated.Continuation.TurnsUsed != 2 {
		t.Fatalf("used turns = %d, want 2", updated.Continuation.TurnsUsed)
	}
}

func TestNormaliseContinuationRequiresBoundedOptIn(t *testing.T) {
	got, err := normaliseContinuation(session.ContinuationPolicy{Enabled: true})
	if err != nil || got.MaxTurns != defaultContinuationMaxTurns || got.TurnsUsed != 0 {
		t.Fatalf("default policy = %+v, %v", got, err)
	}
	if _, err := normaliseContinuation(session.ContinuationPolicy{Enabled: true, MaxTurns: maxContinuationMaxTurns + 1}); err == nil {
		t.Fatal("unbounded max turns was accepted")
	}
	got, err = normaliseContinuation(session.ContinuationPolicy{Enabled: false, MaxTurns: 99, TurnsUsed: 3})
	if err != nil || got.Enabled || got.MaxTurns != 0 || got.TurnsUsed != 0 {
		t.Fatalf("disabled policy = %+v, %v", got, err)
	}
}
