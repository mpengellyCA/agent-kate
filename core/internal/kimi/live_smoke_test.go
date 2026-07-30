package kimi

// Live smoke test against the real `kimi` CLI. Skipped unless KIMI_LIVE_SMOKE
// points at a kimi binary (e.g. KIMI_LIVE_SMOKE=~/.kimi-code/bin/kimi); it
// spends a small amount of real model tokens, so it never runs by default.

import (
	"encoding/json"
	"os"
	"testing"
)

func liveKimiBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KIMI_LIVE_SMOKE")
	if bin == "" {
		t.Skip("KIMI_LIVE_SMOKE not set; skipping live kimi smoke test")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("kimi binary %q not found: %v", bin, err)
	}
	return bin
}

// TestLiveSmoke drives a real `kimi acp` child through a prompt that needs a
// gated Bash tool call (exercising the permission bridge), then a resume.
func TestLiveSmoke(t *testing.T) {
	kimiBin := liveKimiBin(t)
	col := &eventCollector{}

	permCalls := 0
	perm := func(_, toolName string, _ json.RawMessage) bool {
		permCalls++
		if toolName != "Bash" {
			t.Errorf("permission requested for %q, want Bash", toolName)
		}
		return true
	}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, perm, t.TempDir())

	th, err := sup.Start(StartOptions{
		ID:      "t-live",
		WorkDir: t.TempDir(),
		Prompt:  "Run `echo live-smoke-ok` using the shell, then reply with exactly: DONE",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if th.SessionID() == "" {
		t.Fatal("no session id after start")
	}
	col.waitFor(t, "Bash tool card", hasToolUse)
	col.waitFor(t, "turn result", isResult)
	if permCalls == 0 {
		t.Error("permission bridge never consulted for the gated Bash call")
	}
	sessionID := th.SessionID()
	sup.StopAll()

	// Resume the same kimi session (session/resume — no history replay).
	col2 := &eventCollector{}
	sup2 := NewSupervisor(kimiBin, testLogger(), col2.add, nil, t.TempDir())
	th2, err := sup2.Start(StartOptions{
		ID:        "t-live",
		WorkDir:   t.TempDir(),
		SessionID: sessionID,
		Resume:    true,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if th2.SessionID() != sessionID {
		t.Errorf("resumed session = %q, want %q", th2.SessionID(), sessionID)
	}
	sup2.StopAll()
}
