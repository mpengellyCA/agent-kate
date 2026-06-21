package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// fakeClaudeScript writes a stand-in `claude` binary that plays both roles the
// real one does in this flow:
//
//   - As an agent thread (no --resume in argv): block reading stdin and exit
//     cleanly once it's closed — i.e. when the supervisor Stops the thread.
//   - As a cold compactor (--resume in argv, the shape RunLLM uses): sleep
//     briefly to model real compaction latency, print a summary body, exit 0.
//
// The deliberate sleep on the compaction path lets the test prove that the
// shutdown drain actually *waits* for the compaction rather than racing it.
func fakeClaudeScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	script := `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--resume" ]; then
    sleep 0.3
    printf 'COLD-SUMMARY-OK\n'
    exit 0
  fi
done
cat >/dev/null
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// TestColdExitCompactionCompletesOnShutdown is the regression test for the
// fire-and-forget cold-compaction race: a thread configured for a cold-exit
// strategy exits as the core shuts down, and its compaction must finish (and
// its summary land on disk) before the process is allowed to exit.
//
// Before the fix, StopAll did not wait for reap() and the compaction was a
// bare `go runExitCompact(...)`: by the time the drain ran the WaitGroup was
// empty, drain returned instantly, and the summary was never written. The
// asserts below — "no summary the instant StopAll returns" then "summary
// present once drain returns" — fail on that old code and pass on the new.
func TestColdExitCompactionCompletesOnShutdown(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	workDir := t.TempDir()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	summaries, err := compact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("summary store: %v", err)
	}

	threadID := agent.NewThreadID()
	rec := session.Record{
		ThreadID:        threadID,
		SessionID:       "sess-" + threadID,
		CompactStrategy: string(compact.ExitSonnetCold),
		Worktree:        worktree.Worktree{ThreadID: threadID, Path: workDir},
		Status:          session.StatusRunning,
	}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("seed record: %v", err)
	}

	// Wire the cold-exit path exactly as runCore does, but with the fake claude
	// injected so RunLLM spawns the stub instead of a real model.
	coldCompacts := &exitCompactTracker{ctx: t.Context(), claudeBin: claudeBin}
	sup := agent.NewSupervisor(claudeBin, log, func(tid string, events []json.RawMessage) {
		for _, event := range events {
			var probe struct {
				Type  string `json:"type"`
				Phase string `json:"phase"`
			}
			if json.Unmarshal(event, &probe) != nil {
				continue
			}
			if probe.Type == "_lifecycle" && probe.Phase == "exited" {
				if r, ok := sessions.Get(tid); ok {
					strat := compact.Strategy(r.CompactStrategy).Resolve()
					if strat.RunsOnExit() && strat != compact.ExitOpusHot {
						coldCompacts.spawn(log, sessions, summaries, r, strat)
					}
				}
			}
		}
	})

	if _, err := sup.Start(agent.StartOptions{
		ID:        threadID,
		WorkDir:   workDir,
		SessionID: rec.SessionID,
	}); err != nil {
		t.Fatalf("start thread: %v", err)
	}

	// Shutdown sequence, mirroring runCore's tail.
	sup.StopAll()

	// StopAll waited for reap(), so the cold compaction has been spawned — but
	// the fake compactor sleeps 0.3s, so its summary must not exist yet. This
	// is the assertion that fails on the old fire-and-forget code (the summary
	// would simply never appear).
	if sum, _ := summaries.Get(threadID); sum != nil {
		t.Fatal("summary already on disk before drain — fake compactor should still be running")
	}

	cancelled := false
	coldCompacts.drain(func() { cancelled = true }, exitCompactCap, log)
	if cancelled {
		t.Fatal("drain hit the deadline and cancelled the compaction; it should have completed in time")
	}

	sum, err := summaries.Get(threadID)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if sum == nil {
		t.Fatal("cold-exit compaction produced no summary after shutdown drain")
	}
	if got := sum.Body; got != "COLD-SUMMARY-OK" {
		t.Errorf("summary body = %q, want %q", got, "COLD-SUMMARY-OK")
	}
	if sum.Strategy != compact.ExitSonnetCold {
		t.Errorf("summary strategy = %q, want %q", sum.Strategy, compact.ExitSonnetCold)
	}
}

// TestWaitWithDeadlineTimesOut covers the backstop: a compaction that never
// finishes must not wedge quit — drain returns once the cap elapses.
func TestWaitWithDeadlineTimesOut(t *testing.T) {
	tr := &exitCompactTracker{ctx: t.Context()}
	tr.wg.Add(1) // never Done — models a hung claude --resume

	start := time.Now()
	cancelled := false
	tr.drain(func() { cancelled = true }, 50*time.Millisecond, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if !cancelled {
		t.Fatal("drain should have cancelled stragglers after the cap elapsed")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("drain returned in %v, before the cap", elapsed)
	}
	tr.wg.Done() // release the wg.Wait() goroutine drain left blocked
}
