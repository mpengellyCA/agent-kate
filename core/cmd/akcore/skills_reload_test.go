package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
)

// slowReloadFake models the case that used to hurt: every running thread takes
// the full control-channel wait before answering (a wedged CLI answers not at
// all, and the supervisor's timeout is what actually elapses).
type slowReloadFake struct {
	*fakeHarness
	delay time.Duration
	calls atomic.Int32
}

func (f *slowReloadFake) Running(string) bool { return true }

func (f *slowReloadFake) ReloadSkills(string) error {
	f.calls.Add(1)
	time.Sleep(f.delay)
	return nil
}

// TestReloadSkillsEverywhereFansOut pins the shape of the broadcast: N running
// threads must cost about ONE reload wait, not N of them. Serially, this took
// threads*delay inside the skills.install RPC handler.
func TestReloadSkillsEverywhereFansOut(t *testing.T) {
	const (
		threads = 6
		delay   = 200 * time.Millisecond
	)
	sessions := testSessions(t)
	for i := range threads {
		if err := sessions.Put(session.Record{
			ThreadID: "t-" + string(rune('a'+i)), Project: "/p", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	fake := &slowReloadFake{fakeHarness: &fakeHarness{}, delay: delay}
	harnesses := harness.NewRegistry("fake")
	harnesses.Register(fake)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := handlerDeps{
		sessions:  sessions,
		harnesses: harnesses,
		srv:       ipc.NewServer(filepath.Join(t.TempDir(), "skills.sock"), log),
		log:       log,
	}

	start := time.Now()
	reloaded := reloadSkillsEverywhere(d, "installed a skill")
	elapsed := time.Since(start)

	if len(reloaded) != threads {
		t.Fatalf("reloaded %d threads, want %d", len(reloaded), threads)
	}
	if got := fake.calls.Load(); got != threads {
		t.Errorf("ReloadSkills called %d times, want %d", got, threads)
	}
	// Generous ceiling: the point is that it is a small multiple of one wait,
	// not of six. Serial would be at least threads*delay.
	if limit := delay * 3; elapsed > limit {
		t.Errorf("broadcast took %v, over the %v fan-out ceiling (serialised?)", elapsed, limit)
	}
}
