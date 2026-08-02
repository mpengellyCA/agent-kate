package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/coop"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/permission"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// slowExitHarness stands in for a supervisor whose Stop returns immediately
// while the process winds down in the background — the real contract of both
// supervisors' graceful stop. Running stays true until exitDelay after Stop;
// exitDelay < 0 models a process that never dies (a SIGKILL survivor).
type slowExitHarness struct {
	*fakeHarness
	mu        sync.Mutex
	threadID  string
	stopAt    time.Time
	exitDelay time.Duration
}

func (h *slowExitHarness) Running(threadID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if threadID != h.threadID {
		return false
	}
	if h.exitDelay < 0 {
		return true
	}
	if !h.stopAt.IsZero() && time.Since(h.stopAt) >= h.exitDelay {
		return false
	}
	return true
}

func (h *slowExitHarness) Stop(threadID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if threadID == h.threadID && h.stopAt.IsZero() {
		h.stopAt = time.Now()
	}
	return nil
}

// discardTestCore wires a served core around one live slow-exit thread and
// returns a UI-role client pointed at it, plus the session store for
// asserting what a refused discard left behind.
func discardTestCore(t *testing.T, fake *slowExitHarness, exitWait time.Duration) (*ipc.Client, *session.Store) {
	t.Helper()
	sessions := testSessions(t)
	if err := sessions.Put(session.Record{
		ThreadID: fake.threadID, Project: "/p", Backend: "fake", Created: time.Now(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "discard.sock")
	srv := ipc.NewServer(sock, log)
	harnesses := harness.NewRegistry("fake")
	harnesses.Register(fake)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	summaries, err := compact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("compact.NewStore: %v", err)
	}
	threads := newThreadRegistry()
	// Isolated=false: worktree.Remove is a no-op on disk, so the test observes
	// the WAIT (and the refusal), not git's own behaviour.
	threads.put(fake.threadID, worktree.Worktree{ThreadID: fake.threadID, Path: "/p"})
	registerHandlers(handlerDeps{
		srv: srv, harnesses: harnesses, broker: permission.New(),
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		coop: coop.NewState(), threads: threads,
		gitCache: gitCache, sessions: sessions, summaries: summaries,
		bridgeSecrets: newBridgeSecrets(), log: log,
		agentExitWait: exitWait,
	})
	serveIPC(t, srv, sock)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	// The UI role: discard with no fromThreadId is the human's own action.
	if err := client.Call("handshake", map[string]any{}, nil); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return client, sessions
}

// TestDiscardWaitsForTheStoppedProcessToExit (audit F54): agent.discard used
// to call worktree.Remove on the very next line after agentStop — but Stop
// returns immediately and a graceful busy-stop takes seconds, so the removal
// deleted a live process's cwd and the "destroyed" agent kept running. The
// discard must not complete until the process is really gone.
func TestDiscardWaitsForTheStoppedProcessToExit(t *testing.T) {
	const exitDelay = 300 * time.Millisecond
	fake := &slowExitHarness{
		fakeHarness: &fakeHarness{}, threadID: "t-slow", exitDelay: exitDelay,
	}
	client, sessions := discardTestCore(t, fake, 5*time.Second)

	start := time.Now()
	if err := client.CallTimeout("agent.discard",
		map[string]any{"threadId": "t-slow"}, nil, 10*time.Second); err != nil {
		t.Fatalf("agent.discard: %v", err)
	}
	if elapsed := time.Since(start); elapsed < exitDelay {
		t.Errorf("discard returned in %s, before the stopped process exited "+
			"(%s) — the worktree was removed under a live agent", elapsed, exitDelay)
	}
	if fake.Running("t-slow") {
		t.Error("discard returned with the agent process still running")
	}
	if _, ok := sessions.Get("t-slow"); ok {
		t.Error("a completed discard must remove the session record")
	}
}

// TestDiscardRefusesWhenTheProcessNeverExits is F54's fail-closed half: a
// process still alive past the exit window survived even the supervisor's
// SIGKILL backstops, and the discard must refuse — leaving the record and
// worktree intact — rather than delete the cwd out from under it.
func TestDiscardRefusesWhenTheProcessNeverExits(t *testing.T) {
	fake := &slowExitHarness{
		fakeHarness: &fakeHarness{}, threadID: "t-stuck", exitDelay: -1,
	}
	client, sessions := discardTestCore(t, fake, 400*time.Millisecond)

	err := client.CallTimeout("agent.discard",
		map[string]any{"threadId": "t-stuck"}, nil, 10*time.Second)
	if err == nil {
		t.Fatal("agent.discard succeeded while the agent process was still alive")
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Errorf("err = %v, want the refusal to say the process did not exit", err)
	}
	// Nothing was torn down: the thread is still discardable once it dies.
	if _, ok := sessions.Get("t-stuck"); !ok {
		t.Error("the refused discard still removed the session record")
	}
}
