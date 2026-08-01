package kimi

// Live smoke test against the real `kimi` CLI. Skipped unless KIMI_LIVE_SMOKE
// points at a kimi binary (e.g. KIMI_LIVE_SMOKE=~/.kimi-code/bin/kimi); it
// spends a small amount of real model tokens, so it never runs by default.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestLiveDiscoverAndList exercises the token-free probes — session/new's
// config-option enumeration (DiscoverOptions) and session/list (ListSessions) —
// against the real kimi CLI. Neither sends a prompt, so no model inference (or
// token spend) occurs and the test passes even when the model backend is
// rate-limited.
func TestLiveDiscoverAndList(t *testing.T) {
	kimiBin := liveKimiBin(t)
	sup := NewSupervisor(kimiBin, testLogger(), nil, nil, t.TempDir())

	opts, err := sup.DiscoverOptions()
	if err != nil {
		t.Fatalf("DiscoverOptions: %v", err)
	}
	var haveModel bool
	for _, o := range opts {
		if o.ID == "model" && len(o.Options) > 0 {
			haveModel = true
		}
	}
	if !haveModel {
		t.Errorf("DiscoverOptions returned no model enumeration: %+v", opts)
	}
	// Cached: a second call must serve the same set without re-probing.
	if opts2, err := sup.DiscoverOptions(); err != nil || len(opts2) != len(opts) {
		t.Errorf("second DiscoverOptions = %d opts, err %v; want %d cached",
			len(opts2), err, len(opts))
	}

	// session/list must succeed (may be empty) and must not surface the
	// throwaway sessions the probes above left behind.
	sessions, err := sup.ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, s := range sessions {
		if strings.HasPrefix(s.Cwd, filepath.Join(os.TempDir(), probeDirPrefix)) {
			t.Errorf("ListSessions leaked a probe session: %q", s.Cwd)
		}
	}
}

// TestLiveSmoke drives a real `kimi acp` child through a prompt that needs a
// gated Bash tool call (exercising the permission bridge), then a resume.
func TestLiveSmoke(t *testing.T) {
	kimiBin := liveKimiBin(t)
	col := &eventCollector{}

	permCalls := 0
	perm := func(_, toolName string, _ json.RawMessage) (bool, json.RawMessage) {
		permCalls++
		if toolName != "Bash" {
			t.Errorf("permission requested for %q, want Bash", toolName)
		}
		return true, nil
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
