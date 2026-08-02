package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
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

// SkillReload declared, since the broadcast now consults the CAPABILITY as well
// as the interface (audit F50) — a harness that reloads must say so.
func (f *slowReloadFake) Capabilities() harness.Capabilities {
	return harness.Capabilities{ID: "fake", DisplayName: "Fake Engine", SkillReload: true}
}

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

// --- the engines that CANNOT reload (audit F50) ------------------------------

// staticSkillsFake stands in for kimi: it runs threads, and it reads its skill
// directories exactly once, at session start. It has no ReloadSkills method and
// declares SkillReload false — the two halves of "this engine cannot do it".
type staticSkillsFake struct {
	*fakeHarness
	ids map[string]bool
}

func (f *staticSkillsFake) Capabilities() harness.Capabilities {
	return harness.Capabilities{ID: "static", DisplayName: "Static Engine"}
}
func (f *staticSkillsFake) Running(id string) bool { return f.ids[id] }

// reloadingFake is its opposite number: running, capable, and declaring it.
type reloadingFake struct {
	*fakeHarness
	ids   map[string]bool
	calls atomic.Int32
}

func (f *reloadingFake) Capabilities() harness.Capabilities {
	return harness.Capabilities{ID: "reloading", DisplayName: "Reloading Engine",
		SkillReload: true}
}
func (f *reloadingFake) Running(id string) bool { return f.ids[id] }
func (f *reloadingFake) ReloadSkills(string) error {
	f.calls.Add(1)
	return nil
}

// uiNoticeSink connects as the UI and collects the _lifecycle "notice" details
// per thread. emitLifecycle uses NotifyUI, so a plain connection would see
// nothing — the role claim is part of what is being exercised.
func uiNoticeSink(t *testing.T, srv *ipc.Server, sock string) func() map[string][]string {
	t.Helper()
	srv.Handle("test.markUI", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !srv.MarkUI(ctx) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest, "UI role refused")
		}
		return map[string]any{"ok": true}, nil
	})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("sink dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var mu sync.Mutex
	notices := map[string][]string{}
	accepted := make(chan struct{})
	go func() {
		barriered := false
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var f ipc.Frame
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			if f.Method == "" {
				if !barriered {
					barriered = true
					close(accepted)
				}
				continue
			}
			if f.Method != "agent.event" {
				continue
			}
			var p agentEventParams
			if json.Unmarshal(f.Params, &p) != nil {
				continue
			}
			for _, raw := range p.Events {
				var ev struct {
					Type   string `json:"type"`
					Phase  string `json:"phase"`
					Detail string `json:"detail"`
				}
				if json.Unmarshal(raw, &ev) != nil || ev.Type != "_lifecycle" ||
					ev.Phase != "notice" {
					continue
				}
				mu.Lock()
				notices[p.ThreadID] = append(notices[p.ThreadID], ev.Detail)
				mu.Unlock()
			}
		}
	}()
	// One round trip before returning, and it is the role claim: a notice
	// racing this connection's accept would be delivered to nobody.
	if _, err := conn.Write([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"test.markUI","params":{}}` + "\n")); err != nil {
		t.Fatalf("sink markUI: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("sink never got its role claim answered")
	}
	return func() map[string][]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]string{}
		for k, v := range notices {
			out[k] = append([]string(nil), v...)
		}
		return out
	}
}

// TestReloadSkillsTellsTheThreadsItCannotReload (audit F50): a running thread on
// an engine with no reload mechanism used to be dropped twice over — once while
// the target list was built, and again by the notice loop, which only ran over
// threads whose reload SUCCEEDED. So the human installed a skill, watched Agent
// Kate confirm it, and had no way to learn that the agent they were about to
// hand the work to would never see it. A capability gap the human is told about
// costs them a restart; one they are not told about costs them the work.
func TestReloadSkillsTellsTheThreadsItCannotReload(t *testing.T) {
	sessions := testSessions(t)
	for id, backend := range map[string]string{
		"t-reloads": "reloading",
		"t-static":  "static",
		"t-dormant": "static", // not running: no notice, nothing to tell
	} {
		if err := sessions.Put(session.Record{
			ThreadID: id, Backend: backend, Project: "/p", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	reloading := &reloadingFake{fakeHarness: &fakeHarness{},
		ids: map[string]bool{"t-reloads": true}}
	static := &staticSkillsFake{fakeHarness: &fakeHarness{},
		ids: map[string]bool{"t-static": true}}
	harnesses := harness.NewRegistry("reloading")
	harnesses.Register(reloading)
	harnesses.Register(static)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "reload.sock")
	srv := ipc.NewServer(sock, log)
	serveIPC(t, srv, sock)
	read := uiNoticeSink(t, srv, sock)

	d := handlerDeps{sessions: sessions, harnesses: harnesses, srv: srv, log: log}
	reloaded := reloadSkillsEverywhere(d, "installed a skill")

	if len(reloaded) != 1 || reloaded[0] != "t-reloads" {
		t.Fatalf("reloaded = %v, want just [t-reloads]", reloaded)
	}
	if got := reloading.calls.Load(); got != 1 {
		t.Errorf("ReloadSkills called %d times, want 1", got)
	}

	// Notices are queued on the connection's writer, so give the sink a moment.
	var notices map[string][]string
	deadline := time.Now().Add(5 * time.Second)
	for {
		notices = read()
		if len(notices["t-reloads"]) > 0 && len(notices["t-static"]) > 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(notices["t-reloads"]) != 1 ||
		!strings.Contains(notices["t-reloads"][0], "skills reloaded") {
		t.Errorf("t-reloads notices = %v, want one 'skills reloaded' line",
			notices["t-reloads"])
	}
	// THE FINDING: this used to be empty.
	if len(notices["t-static"]) != 1 {
		t.Fatalf("t-static notices = %v, want exactly one — a running agent that "+
			"cannot pick the skill up must SAY so, not be skipped in silence",
			notices["t-static"])
	}
	said := notices["t-static"][0]
	for _, want := range []string{"Static Engine", "restart"} {
		if !strings.Contains(said, want) {
			t.Errorf("t-static notice = %q, want it to name %q so the human knows "+
				"which agent and what to do about it", said, want)
		}
	}
	if len(notices["t-dormant"]) != 0 {
		t.Errorf("t-dormant (not running) was told %v; a dormant thread reads its "+
			"skills when it next starts, so there is nothing to say",
			notices["t-dormant"])
	}
}

// --- the AND, enforced in both directions (audit F50 pass 4) -----------------

// The broadcast asks for BOTH halves: the ReloadSkills method (how the reload
// is called) and Capabilities().SkillReload (what the harness declares, and
// what ui/src/state/HarnessTraits.cpp mirrors). Pass 3 wrote the AND and tested
// only the case where the two agree — so `hasMethod || declared`,
// `hasMethod` alone and `declared` alone all passed its fixtures, and the
// condition was decoration.
//
// These two fakes are the halves disagreeing. Both must fail CLOSED, in the
// direction the human feels: no reload, and a notice saying so — because the
// alternative on the left is a thread that reloads while the UI's traits table
// says it cannot, and on the right a thread dropped in the silence F50 is
// about.

// undeclaredReloadFake CAN reload and does not say so. `declared` alone would
// skip it correctly; `hasMethod` alone would reload it behind the UI's back.
type undeclaredReloadFake struct {
	*fakeHarness
	ids   map[string]bool
	calls atomic.Int32
}

func (f *undeclaredReloadFake) Capabilities() harness.Capabilities {
	return harness.Capabilities{ID: "undeclared", DisplayName: "Undeclared Engine"}
}
func (f *undeclaredReloadFake) Running(id string) bool { return f.ids[id] }
func (f *undeclaredReloadFake) ReloadSkills(string) error {
	f.calls.Add(1)
	return nil
}

// declaredNoMethodFake SAYS it can reload and has no method to do it with.
// `hasMethod` alone would skip it correctly; `declared` alone would put it in
// the target list, where the type assertion that built the list has already
// failed — i.e. it would not compile as written, which is exactly why this half
// needs a fixture rather than an argument.
type declaredNoMethodFake struct {
	*fakeHarness
	ids map[string]bool
}

func (f *declaredNoMethodFake) Capabilities() harness.Capabilities {
	return harness.Capabilities{ID: "declaredonly", DisplayName: "Declared-Only Engine",
		SkillReload: true}
}
func (f *declaredNoMethodFake) Running(id string) bool { return f.ids[id] }

func TestReloadSkillsNeedsBothHalvesOfTheCapability(t *testing.T) {
	sessions := testSessions(t)
	for id, backend := range map[string]string{
		"t-undeclared":   "undeclared",   // method, no declaration
		"t-declaredonly": "declaredonly", // declaration, no method
		"t-reloads":      "reloading",    // both — the control
	} {
		if err := sessions.Put(session.Record{
			ThreadID: id, Backend: backend, Project: "/p", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	undeclared := &undeclaredReloadFake{fakeHarness: &fakeHarness{},
		ids: map[string]bool{"t-undeclared": true}}
	declaredOnly := &declaredNoMethodFake{fakeHarness: &fakeHarness{},
		ids: map[string]bool{"t-declaredonly": true}}
	reloading := &reloadingFake{fakeHarness: &fakeHarness{},
		ids: map[string]bool{"t-reloads": true}}
	harnesses := harness.NewRegistry("reloading")
	harnesses.Register(reloading)
	harnesses.Register(undeclared)
	harnesses.Register(declaredOnly)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "reload-and.sock")
	srv := ipc.NewServer(sock, log)
	serveIPC(t, srv, sock)
	read := uiNoticeSink(t, srv, sock)

	d := handlerDeps{sessions: sessions, harnesses: harnesses, srv: srv, log: log}
	reloaded := reloadSkillsEverywhere(d, "installed a skill")

	// Only the thread whose halves AGREE is reloaded.
	if len(reloaded) != 1 || reloaded[0] != "t-reloads" {
		t.Fatalf("reloaded = %v, want just [t-reloads] — a harness whose method "+
			"and capability disagree must not be reloaded", reloaded)
	}
	// The half that matters most: a harness that can reload but never declared
	// it is NOT called. Otherwise the reload happens while every surface derived
	// from the capability — the UI's traits table, the notice below — says it
	// did not.
	if got := undeclared.calls.Load(); got != 0 {
		t.Errorf("an undeclared harness was reloaded %d times; the declaration is "+
			"what the rest of the app reads", got)
	}

	var notices map[string][]string
	deadline := time.Now().Add(5 * time.Second)
	for {
		notices = read()
		if len(notices["t-undeclared"]) > 0 && len(notices["t-declaredonly"]) > 0 &&
			len(notices["t-reloads"]) > 0 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Both disagreeing threads say something, in their own panel: failing closed
	// silently would leave the human handing work to an agent that never saw the
	// skill, which is the whole of F50.
	for id, engine := range map[string]string{
		"t-undeclared":   "Undeclared Engine",
		"t-declaredonly": "Declared-Only Engine",
	} {
		if len(notices[id]) != 1 {
			t.Fatalf("%s notices = %v, want exactly one", id, notices[id])
		}
		for _, want := range []string{engine, "restart"} {
			if !strings.Contains(notices[id][0], want) {
				t.Errorf("%s notice = %q, want it to name %q", id, notices[id][0], want)
			}
		}
	}
	if len(notices["t-reloads"]) != 1 ||
		!strings.Contains(notices["t-reloads"][0], "skills reloaded") {
		t.Errorf("t-reloads notices = %v, want one 'skills reloaded' line",
			notices["t-reloads"])
	}
}
