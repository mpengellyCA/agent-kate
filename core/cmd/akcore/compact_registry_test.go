package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// compactFake records the CompactSpec it is handed, so the tests can assert
// that the handlers hand the HARNESS the whole request instead of building a
// CLI invocation themselves (plan 16 P6).
type compactFake struct {
	*fakeHarness
	mu      sync.Mutex
	spec    harness.CompactSpec
	calls   int
	capable bool
	running bool
}

func (c *compactFake) Capabilities() harness.Capabilities {
	caps := c.fakeHarness.Capabilities()
	caps.Compaction = c.capable
	caps.DisplayName = "Fake Engine"
	return caps
}

func (c *compactFake) Running(string) bool { return c.running }

func (c *compactFake) Compact(_ context.Context, spec harness.CompactSpec) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.spec = spec
	if !c.capable {
		return "", harness.Unsupported("Compaction", c.Capabilities())
	}
	return "SUMMARY-FROM-THE-HARNESS", nil
}

func (c *compactFake) lastSpec() harness.CompactSpec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spec
}

func compactTestCore(t *testing.T, fake *compactFake, sessions *session.Store) *ipc.Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "compact.sock")
	srv := ipc.NewServer(sock, log)
	summaries, err := compact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("summary store: %v", err)
	}
	harnesses := harness.NewRegistry("fake")
	harnesses.Register(fake)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	d := handlerDeps{
		srv: srv, harnesses: harnesses,
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		threads: newThreadRegistry(), gitCache: gitCache,
		sessions: sessions, summaries: summaries, log: log,
	}
	registerHandlers(d)
	serveIPC(t, srv, sock)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func seedCompactRecord(t *testing.T, sessions *session.Store, backend string) session.Record {
	t.Helper()
	rec := session.Record{
		ThreadID:  "t-compact",
		SessionID: "sess-1",
		Project:   "/p",
		Backend:   backend,
		Worktree:  worktree.Worktree{ThreadID: "t-compact", Path: t.TempDir()},
		Created:   time.Now(),
	}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return rec
}

// TestCompactNowRoutesThroughTheHarness: both mechanisms reach the thread's
// harness as a CompactSpec — the hot one as an in-session turn, the cold one
// with everything a fresh pass needs. Before P6 the cold path built a `claude
// --print --resume` subprocess in the handler, which no other engine could
// ever satisfy.
func TestCompactNowRoutesThroughTheHarness(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	fake := &compactFake{fakeHarness: &fakeHarness{}, capable: true, running: true}
	client := compactTestCore(t, fake, sessions)

	// Hot: in-session, no session id or model needed.
	if err := client.Call("agent.compactNow",
		map[string]any{"threadId": rec.ThreadID, "model": "hot"}, nil); err != nil {
		t.Fatalf("hot compactNow: %v", err)
	}
	spec := fake.lastSpec()
	if !spec.Hot || spec.ThreadID != rec.ThreadID || spec.Prompt != compact.CompactPrompt {
		t.Fatalf("hot spec = %+v", spec)
	}

	// Cold: the harness gets the session, the worktree and the strategy's model.
	if err := client.Call("agent.compactNow",
		map[string]any{"threadId": rec.ThreadID, "model": "sonnet"}, nil); err != nil {
		t.Fatalf("cold compactNow: %v", err)
	}
	spec = fake.lastSpec()
	if spec.Hot {
		t.Error("cold compaction asked for the hot mechanism")
	}
	if spec.SessionID != rec.SessionID || spec.WorkDir != rec.Worktree.Path ||
		spec.Model == "" || spec.Prompt != compact.CompactPrompt {
		t.Fatalf("cold spec = %+v", spec)
	}
	if fake.calls != 2 {
		t.Fatalf("harness Compact called %d times, want 2", fake.calls)
	}
}

// TestCompactNowRefusedWithoutTheCapability: the gate answers with the shared
// wording and the harness is never asked — a summary no model wrote would be
// worse than none.
func TestCompactNowRefusedWithoutTheCapability(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	fake := &compactFake{fakeHarness: &fakeHarness{}, capable: false, running: true}
	client := compactTestCore(t, fake, sessions)

	err := client.Call("agent.compactNow",
		map[string]any{"threadId": rec.ThreadID, "model": "sonnet"}, nil)
	if err == nil {
		t.Fatal("compactNow ran on a harness with no compaction support")
	}
	if !strings.Contains(err.Error(), "not supported by Fake Engine agents") {
		t.Errorf("error %q does not use the shared capability wording", err)
	}
	if fake.calls != 0 {
		t.Errorf("the harness was asked to compact anyway (%d calls)", fake.calls)
	}
}

// TestUnsupportedWordingMatchesTheRPCGate keeps the two exits identical: an
// adapter's own refusal and the RPC-layer gate must read the same, or the same
// missing capability describes itself two different ways.
func TestUnsupportedWordingMatchesTheRPCGate(t *testing.T) {
	caps := harness.Capabilities{ID: "x", DisplayName: "Some Engine"}
	if got, want := harness.Unsupported("Compaction", caps).Error(),
		unsupportedDetail("Compaction", caps); got != want {
		t.Errorf("harness.Unsupported = %q, RPC gate = %q", got, want)
	}
}

// TestExitCompactSkipsHarnessesThatCannot: the shutdown drain must not spawn a
// pass for a thread whose engine has no compaction — before P6 it spawned a
// claude subprocess for EVERY cold-strategy record, whatever ran the thread.
func TestExitCompactSkipsHarnessesThatCannot(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	rec.CompactStrategy = string(compact.ExitSonnetCold)
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	summaries, err := compact.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("summary store: %v", err)
	}
	fake := &compactFake{fakeHarness: &fakeHarness{}, capable: false}
	harnesses := harness.NewRegistry("fake")
	harnesses.Register(fake)

	tracker := &exitCompactTracker{ctx: t.Context(), harnesses: harnesses}
	tracker.spawn(log, sessions, summaries, rec, compact.ExitSonnetCold)
	tracker.drain(func() {}, 2*time.Second, log)
	if fake.calls != 0 {
		t.Errorf("spawned a compaction for an engine that cannot compact (%d calls)", fake.calls)
	}
	if sum, _ := summaries.Get(rec.ThreadID); sum != nil {
		t.Error("a summary was stored for a thread that was never compacted")
	}
}
