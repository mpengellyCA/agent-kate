package main

import (
	"context"
	"errors"
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
	// hotOnly models an engine that compacts the LIVE session in place (kimi):
	// Compaction is on, ColdCompact is off.
	hotOnly bool
	// inPlace makes the hot mechanism report ErrCompactedInPlace — success
	// with no summary text, the outcome Harness.Compact's signature cannot
	// express on its own.
	inPlace bool
	// hotErr / hotEmpty model the two ways a hot compaction can fail after the
	// handler has already marked the turn queued.
	hotErr   bool
	hotEmpty bool
}

func (c *compactFake) Capabilities() harness.Capabilities {
	caps := c.fakeHarness.Capabilities()
	caps.Compaction = c.capable
	caps.ColdCompact = c.capable && !c.hotOnly
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
	if c.inPlace {
		return "", ErrCompactedInPlace
	}
	if c.hotErr && spec.Hot {
		return "", errors.New("the engine refused the compaction")
	}
	if c.hotEmpty && spec.Hot {
		return "", nil
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

// TestHotCompactFailureReleasesTheTurn: the handler marks the compact turn
// queued before it runs, so every failure path has to release it. A leaked
// queued turn leaves the thread permanently busy for agent.wait and the Jobs
// panel — with no result event ever coming to clear it.
func TestHotCompactFailureReleasesTheTurn(t *testing.T) {
	for name, fake := range map[string]*compactFake{
		"the engine refused": {
			fakeHarness: &fakeHarness{}, capable: true, running: true, hotErr: true,
		},
		"the engine answered nothing": {
			fakeHarness: &fakeHarness{}, capable: true, running: true, hotEmpty: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			sessions := testSessions(t)
			rec := seedCompactRecord(t, sessions, "fake")
			client := compactTestCore(t, fake, sessions)

			if err := client.Call("agent.compactNow",
				map[string]any{"threadId": rec.ThreadID, "model": "hot"}, nil); err == nil {
				t.Fatal("a failed hot compaction reported success")
			}
			var out struct {
				Status string `json:"status"`
			}
			if err := client.Call("agent.wait",
				map[string]any{"threadId": rec.ThreadID, "timeoutSec": 1}, &out); err != nil {
				t.Fatalf("agent.wait: %v", err)
			}
			if out.Status != "idle" {
				t.Errorf("thread is %q after a failed compaction, want idle", out.Status)
			}
		})
	}
}

// TestSetCompactStrategyRefusesColdOnAHotOnlyEngine: the capability gate has to
// be per-strategy, not per-feature. Every strategy but the hot one runs after
// the process is gone, so storing one on an engine that compacts only live
// sessions arms a compaction that can never fire.
func TestSetCompactStrategyRefusesColdOnAHotOnlyEngine(t *testing.T) {
	sessions := testSessions(t)
	rec := seedCompactRecord(t, sessions, "fake")
	fake := &compactFake{
		fakeHarness: &fakeHarness{}, capable: true, running: true, hotOnly: true,
	}
	client := compactTestCore(t, fake, sessions)

	for _, strategy := range []string{
		string(compact.ExitSonnetCold), string(compact.ResumeSonnetCold),
		string(compact.ResumeLocal),
	} {
		err := client.Call("agent.setCompactStrategy",
			map[string]any{"threadId": rec.ThreadID, "strategy": strategy}, nil)
		if err == nil {
			t.Fatalf("strategy %q was accepted on a hot-only engine", strategy)
		}
		if !strings.Contains(err.Error(), string(compact.ExitOpusHot)) {
			t.Errorf("refusal %q does not name the strategy that IS supported", err)
		}
	}
	// The hot strategy is the one it can run, and it is still accepted.
	if err := client.Call("agent.setCompactStrategy",
		map[string]any{"threadId": rec.ThreadID, "strategy": string(compact.ExitOpusHot)},
		nil); err != nil {
		t.Fatalf("hot strategy on a hot-only engine: %v", err)
	}
	if got, _ := sessions.Get(rec.ThreadID); got.CompactStrategy != string(compact.ExitOpusHot) {
		t.Errorf("stored strategy = %q, want the hot one", got.CompactStrategy)
	}
}

// TestHotCompactReleasesTheBusySlotOnEveryFailure pins audit F24's fix.
//
// runHotCompactIfConfigured marks the thread busy (TurnQueued) BEFORE asking the
// harness to compact, so a waiter that races the start does not see a false
// idle. Every way the compaction can end without a turn actually running has to
// give that slot back. It used to give it back on none of them, and the comment
// justifying that ("the imminent exit lifecycle clears the count") only holds
// when the exit happens — if the following agentStop fails, the thread reads
// busy for the rest of the core's run: wait_agent parks on it and the Jobs panel
// shows work that is not running.
//
// The probe is TurnTracker.Wait with a zero-ish timeout: it returns timedOut
// only while the in-flight count is above zero, which is exactly "the thread
// still reads busy".
func TestHotCompactReleasesTheBusySlotOnEveryFailure(t *testing.T) {
	cases := []struct {
		name string
		// A factory, not a value: compactFake carries a mutex, so the table may
		// not hold one to copy out of.
		fake func() *compactFake
		// ranATurn is true when the compaction turn really executed, so its own
		// result event is what clears the count — the handler must NOT release
		// again or it would steal a slot from a concurrent turn.
		ranATurn bool
	}{
		// The harness never got the prompt to the agent (thread not running, the
		// send failed, the deadline passed): no result event is coming, so the
		// handler owns the release.
		{
			name: "harness refused",
			fake: func() *compactFake {
				return &compactFake{fakeHarness: &fakeHarness{},
					capable: true, running: true, hotErr: true}
			},
		},
		// These two DID run the turn — the agent answered, the answer was just
		// unusable (blank) or kept in the engine's own session (in place). Their
		// result event already cleared the count.
		{
			name: "empty summary body",
			fake: func() *compactFake {
				return &compactFake{fakeHarness: &fakeHarness{},
					capable: true, running: true, hotEmpty: true}
			},
			ranATurn: true,
		},
		{
			name: "compacted in place",
			fake: func() *compactFake {
				return &compactFake{fakeHarness: &fakeHarness{},
					capable: true, running: true, hotOnly: true, inPlace: true}
			},
			ranATurn: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake()
			sessions := testSessions(t)
			rec := seedCompactRecord(t, sessions, "fake")
			if err := sessions.Update(rec.ThreadID, func(r *session.Record) {
				r.CompactStrategy = string(compact.ExitOpusHot)
			}); err != nil {
				t.Fatalf("set strategy: %v", err)
			}
			harnesses := harness.NewRegistry("fake")
			harnesses.Register(fake)
			summaries, err := compact.NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("summary store: %v", err)
			}
			turns := agent.NewTurnTracker()
			d := handlerDeps{
				harnesses: harnesses, turns: turns, sessions: sessions,
				summaries: summaries,
				log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			if tc.ranATurn {
				// Model what really happens on the wire: the compaction turn's
				// result event reaches the relay and clears the count. If the
				// handler ALSO released, this thread would go idle one turn early.
				turns.TurnQueued(rec.ThreadID) // a concurrent, still-running turn
			}
			runHotCompactIfConfigured(d, rec.ThreadID)
			if tc.ranATurn {
				turns.Observe(rec.ThreadID, []byte(`{"type":"result"}`))
			}

			_, timedOut := turns.Wait(t.Context(), rec.ThreadID, 200*time.Millisecond)
			if tc.ranATurn {
				if !timedOut {
					t.Fatal("the handler released a slot the result event already " +
						"released — a concurrent turn was marked idle early")
				}
				return
			}
			if timedOut {
				t.Fatal("the thread still reads busy after a failed hot compaction; " +
					"wait_agent and the Jobs panel would hang on it")
			}
		})
	}
}
