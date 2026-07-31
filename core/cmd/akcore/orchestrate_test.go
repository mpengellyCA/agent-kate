package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/coop"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/permission"
	"agentkate/internal/session"
)

func testSessions(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// TestInSubtree pins the ownership rule behind send_agent/close_agent: a
// caller owns itself and everything transitively parented under it — nothing
// else.
func TestInSubtree(t *testing.T) {
	sessions := testSessions(t)
	put := func(id, parent string) {
		if err := sessions.Put(session.Record{
			ThreadID: id, Project: "/p", ParentThreadID: parent,
			Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	put("t-ctrl", "")
	put("t-w1", "t-ctrl")
	put("t-w2", "t-w1") // a worker's own sub-worker
	put("t-other", "")

	d := handlerDeps{sessions: sessions}
	for _, tc := range []struct {
		caller, target string
		want           bool
	}{
		{"t-ctrl", "t-ctrl", true},  // self
		{"t-ctrl", "t-w1", true},    // direct worker
		{"t-ctrl", "t-w2", true},    // transitive worker
		{"t-w1", "t-w2", true},      // sub-controller owns its own worker
		{"t-w1", "t-ctrl", false},   // a worker does NOT own its controller
		{"t-ctrl", "t-other", false},
		{"t-ctrl", "t-missing", false},
	} {
		if got := d.inSubtree(tc.caller, tc.target); got != tc.want {
			t.Errorf("inSubtree(%s, %s) = %v, want %v",
				tc.caller, tc.target, got, tc.want)
		}
	}
}

// TestUnappliedOptions pins launch_agent's applied-truth contract: whatever
// was requested but not applied is named; matches and empty requests are not.
func TestUnappliedOptions(t *testing.T) {
	launched := harness.Launched{
		Model: "kimi-code/k2", Effort: "high", PermissionMode: "default",
	}
	got := unappliedOptions(map[string]string{
		"model":          "kimi-code/k3", // downgraded by the handshake
		"effort":         "high",         // applied as requested
		"permissionMode": "",             // not requested at all
	}, launched)
	if len(got) != 1 {
		t.Fatalf("unapplied = %v, want exactly the model downgrade", got)
	}
	if got[0]["option"] != "model" || got[0]["requested"] != "kimi-code/k3" ||
		got[0]["applied"] != "kimi-code/k2" {
		t.Fatalf("unapplied[0] = %v", got[0])
	}
	if got := unappliedOptions(map[string]string{"model": "opus"},
		harness.Launched{Model: "opus"}); len(got) != 0 {
		t.Fatalf("matching request reported unapplied: %v", got)
	}
}

// fakeHarness is a registrable no-process harness for exercising the
// launchWorker path: it "applies" whatever effort was asked but downgrades
// every requested model to "fake-small", the way a real handshake might.
type fakeHarness struct {
	mu   sync.Mutex
	last harness.StartSpec
	// personaApplied makes the fake stand in for a harness that DOES support
	// the plan 16 P3 persona channels; the zero value stands in for one that
	// does not and reports nothing about them.
	personaApplied bool
}

func (f *fakeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{ID: "fake", DisplayName: "Fake Engine", MintsSessionID: true}
}

func (f *fakeHarness) Launch(spec harness.StartSpec) (harness.Launched, error) {
	f.mu.Lock()
	f.last = spec
	persona := f.personaApplied
	f.mu.Unlock()
	out := harness.Launched{
		SessionID: spec.SessionID, Model: "fake-small",
		Effort: spec.Effort, PermissionMode: "default",
	}
	if persona {
		out.SystemPromptApplied = spec.SystemPrompt != ""
		for _, p := range spec.Agents {
			out.Agents = append(out.Agents,
				harness.AppliedAgent{Name: p.Name, Applied: true})
		}
	}
	return out, nil
}

// spec returns the StartSpec of the last Launch.
func (f *fakeHarness) spec() harness.StartSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeHarness) Send(string, string, []agent.Attachment) error { return nil }
func (f *fakeHarness) Interrupt(string) error                        { return nil }
func (f *fakeHarness) Stop(string) error                             { return nil }
func (f *fakeHarness) Running(string) bool                           { return false }
func (f *fakeHarness) StopAll()                                      {}
func (f *fakeHarness) ReadTranscript(string, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeHarness) SetOption(string, string, string) (string, error) { return "", nil }
func (f *fakeHarness) DiscoverOptions() ([]harness.DiscoveredOption, error) {
	return nil, nil
}
func (f *fakeHarness) BrowseSessions() ([]harness.BrowsableSession, error) { return nil, nil }

// serveIPC starts srv on sock and blocks until the socket exists.
func serveIPC(t *testing.T, srv *ipc.Server, sock string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("server socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// permAutoResponder connects a raw notification listener that answers every
// permission.requested by resolving the broker with the current allow flag,
// counting the asks — the test's stand-in for the human in the UI.
func permAutoResponder(t *testing.T, sock string, broker *permission.Broker,
	allow *atomic.Bool, asks *atomic.Int32) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("responder dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
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
			if f.Method == "" { // the barrier's reply
				if !barriered {
					barriered = true
					close(accepted)
				}
				continue
			}
			if f.Method != "permission.requested" {
				continue
			}
			var p struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(f.Params, &p) != nil || p.RequestID == "" {
				continue
			}
			asks.Add(1)
			broker.Resolve(p.RequestID, permission.Decision{Allow: allow.Load()})
		}
	}()
	// One round trip before returning: permission.requested is a fire-and-forget
	// broadcast to the connections the server has REGISTERED, so an ask racing
	// this connection's accept would be delivered to nobody and the test would
	// hang for the whole permission timeout. Any method answers — a
	// method-not-found reply proves the accept happened.
	if _, err := conn.Write([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"responder.barrier"}` + "\n")); err != nil {
		t.Fatalf("responder barrier: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("responder connection was never accepted by the server")
	}
}

// orchTestCore spins a real IPC server with the orchestration handlers over
// real (empty) supervisors plus a registered fakeHarness, and returns a
// connected client.
func orchTestCore(t *testing.T, sessions *session.Store, turns *agent.TurnTracker,
	fakes ...*fakeHarness) *ipc.Client {
	t.Helper()
	fake := &fakeHarness{}
	if len(fakes) > 0 {
		fake = fakes[0]
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "orch.sock")
	srv := ipc.NewServer(sock, log)

	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, "", ""))
	harnesses.Register(fake)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	d := handlerDeps{
		srv: srv, sup: sup, harnesses: harnesses,
		turns: turns, orchGrants: newOrchGrants(),
		threads: newThreadRegistry(), gitCache: gitCache,
		sessions: sessions, log: log,
	}
	registerOrchestrationHandlers(d)

	serveIPC(t, srv, sock)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestAgentWaitRPC drives agent.wait end to end over the bus: immediate
// return for an idle dormant thread, the timeout status while a turn is in
// flight, wake-up on the result event, and rejection of unknown threads.
func TestAgentWaitRPC(t *testing.T) {
	sessions := testSessions(t)
	turns := agent.NewTurnTracker()
	if err := sessions.Put(session.Record{
		ThreadID: "t-idle", Project: "/p", Created: time.Now(),
		Status: session.StatusDormant,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	client := orchTestCore(t, sessions, turns)

	var res struct {
		Status   string `json:"status"`
		LastText string `json:"lastText"`
	}
	// Dormant + no turn in flight: returns at once; no process = "exited".
	if err := client.Call("agent.wait",
		map[string]any{"threadId": "t-idle"}, &res); err != nil {
		t.Fatalf("agent.wait: %v", err)
	}
	if res.Status != "exited" {
		t.Fatalf("status = %q, want exited", res.Status)
	}

	// A queued turn holds the wait until its result lands.
	turns.TurnQueued("t-idle")
	if err := client.Call("agent.wait",
		map[string]any{"threadId": "t-idle", "timeoutSec": 1}, &res); err != nil {
		t.Fatalf("agent.wait(timeout): %v", err)
	}
	if res.Status != "timeout" {
		t.Fatalf("status = %q, want timeout", res.Status)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := client.Call("agent.wait",
			map[string]any{"threadId": "t-idle", "timeoutSec": 30}, &res); err != nil {
			t.Errorf("agent.wait: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	turns.Observe("t-idle", json.RawMessage(
		`{"type":"result","subtype":"success","result":"worker says hi"}`))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent.wait did not wake on the result event")
	}
	if res.Status != "exited" || res.LastText != "worker says hi" {
		t.Fatalf("after result: %+v", res)
	}

	if err := client.Call("agent.wait",
		map[string]any{"threadId": "t-nope"}, &res); err == nil {
		t.Fatal("agent.wait accepted an unknown thread")
	}
}

// TestLaunchWorkerValidation covers agent.launchWorker's parameter gates —
// the paths that must fail BEFORE any process could be spawned.
func TestLaunchWorkerValidation(t *testing.T) {
	sessions := testSessions(t)
	if err := sessions.Put(session.Record{
		ThreadID: "t-parent", Project: "/nonexistent", Created: time.Now(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	client := orchTestCore(t, sessions, agent.NewTurnTracker())

	for name, params := range map[string]map[string]any{
		"missing prompt":  {"parentThreadId": "t-parent"},
		"missing parent":  {"prompt": "do things"},
		"unknown parent":  {"parentThreadId": "t-ghost", "prompt": "x"},
		"unknown backend": {"parentThreadId": "t-parent", "prompt": "x", "backend": "codex"},
		"bad isolation":   {"parentThreadId": "t-parent", "prompt": "x", "isolation": "container"},
	} {
		if err := client.Call("agent.launchWorker", params, nil); err == nil {
			t.Errorf("launchWorker accepted %s", name)
		}
	}
}

// TestAuthorizeAgentTarget pins the cross-subtree approval gate: subtree
// targets never ask; a cross-subtree target asks exactly once per (caller,
// target, action) and caches only approvals — a denial is re-asked; pruning a
// thread's grants makes the next call ask again.
func TestAuthorizeAgentTarget(t *testing.T) {
	sessions := testSessions(t)
	for id, parent := range map[string]string{
		"t-a": "", "t-b": "", "t-c": "t-a",
	} {
		if err := sessions.Put(session.Record{
			ThreadID: id, Project: "/p", ParentThreadID: parent, Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "auth.sock")
	srv := ipc.NewServer(sock, log)
	broker := permission.New()
	d := handlerDeps{
		srv: srv, broker: broker, orchGrants: newOrchGrants(),
		sessions: sessions, log: log,
	}
	serveIPC(t, srv, sock)
	var allow atomic.Bool
	var asks atomic.Int32
	permAutoResponder(t, sock, broker, &allow, &asks)

	allow.Store(true)
	// Own subtree: no ask.
	if err := d.authorizeAgentTarget("t-a", "t-c", "send_agent", nil); err != nil {
		t.Fatalf("subtree target: %v", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("subtree target asked the human (%d asks)", asks.Load())
	}
	// Cross-subtree: exactly one ask, then the grant is reused.
	if err := d.authorizeAgentTarget("t-a", "t-b", "send_agent", nil); err != nil {
		t.Fatalf("approved cross target: %v", err)
	}
	if err := d.authorizeAgentTarget("t-a", "t-b", "send_agent", nil); err != nil {
		t.Fatalf("second call after approval: %v", err)
	}
	if asks.Load() != 1 {
		t.Fatalf("approve-once broken: %d asks, want 1", asks.Load())
	}
	// A grant does NOT cover a different action on the same pair.
	if err := d.authorizeAgentTarget("t-a", "t-b", "close_agent", nil); err != nil {
		t.Fatalf("close after send approval: %v", err)
	}
	if asks.Load() != 2 {
		t.Fatalf("action scoping broken: %d asks, want 2", asks.Load())
	}
	// Denials are never cached: every denied call re-asks.
	allow.Store(false)
	if err := d.authorizeAgentTarget("t-b", "t-a", "discard_agent", nil); err == nil {
		t.Fatal("denied discard reported success")
	}
	if err := d.authorizeAgentTarget("t-b", "t-a", "discard_agent", nil); err == nil {
		t.Fatal("second denied discard reported success")
	}
	if asks.Load() != 4 {
		t.Fatalf("denial caching broken: %d asks, want 4", asks.Load())
	}
	// Pruning either party's grants forces a fresh ask.
	d.orchGrants.forgetThread("t-b")
	allow.Store(true)
	if err := d.authorizeAgentTarget("t-a", "t-b", "send_agent", nil); err != nil {
		t.Fatalf("post-prune send: %v", err)
	}
	if asks.Load() != 5 {
		t.Fatalf("forgetThread did not prune the grant: %d asks, want 5", asks.Load())
	}
}

// TestDiscardGoesThroughGate proves agent.discard is gated for agent-driven
// calls (deny blocks it BEFORE any destructive step; approval falls through
// to the normal unknown-thread validation) and stays ungated for the UI.
func TestDiscardGoesThroughGate(t *testing.T) {
	sessions := testSessions(t)
	for _, id := range []string{"t-a", "t-b"} {
		if err := sessions.Put(session.Record{
			ThreadID: id, Project: "/p", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "gate.sock")
	srv := ipc.NewServer(sock, log)
	broker := permission.New()
	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, "", ""))
	d := handlerDeps{
		srv: srv, sup: sup, harnesses: harnesses,
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		coop: coop.NewState(), threads: newThreadRegistry(),
		broker: broker, sessions: sessions, log: log,
	}
	registerHandlers(d) // the real handler set, gate included
	serveIPC(t, srv, sock)
	var allow atomic.Bool
	var asks atomic.Int32
	permAutoResponder(t, sock, broker, &allow, &asks)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Denied: the discard fails at the gate, before any lookup or removal.
	allow.Store(false)
	err = client.Call("agent.discard",
		map[string]any{"threadId": "t-b", "fromThreadId": "t-a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("denied discard: err = %v, want not-approved", err)
	}
	if _, ok := sessions.Get("t-b"); !ok {
		t.Fatal("denied discard removed the record")
	}
	// Approved: the gate passes and the handler proceeds to its normal
	// validation ("unknown thread" — t-b has no registered worktree here).
	allow.Store(true)
	err = client.Call("agent.discard",
		map[string]any{"threadId": "t-b", "fromThreadId": "t-a"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown thread") {
		t.Fatalf("approved discard: err = %v, want unknown-thread", err)
	}
	if asks.Load() != 2 {
		t.Fatalf("asks = %d, want 2 (deny + approve)", asks.Load())
	}
	// UI-driven (no fromThreadId): never gated, straight to validation.
	err = client.Call("agent.discard", map[string]any{"threadId": "t-b"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown thread") {
		t.Fatalf("UI discard: err = %v", err)
	}
	if asks.Load() != 2 {
		t.Fatalf("UI discard asked the human (%d asks)", asks.Load())
	}
}

// TestLaunchWorkerAppliedTruth drives agent.launchWorker end to end over a
// fake harness: the reply carries what was actually applied, the unapplied
// diff names the downgraded model, and the records gain the parent linkage.
func TestLaunchWorkerAppliedTruth(t *testing.T) {
	sessions := testSessions(t)
	proj := t.TempDir()
	if err := sessions.Put(session.Record{
		ThreadID: "t-parent", Project: proj, Created: time.Now(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	client := orchTestCore(t, sessions, agent.NewTurnTracker())

	var res struct {
		ThreadID  string            `json:"threadId"`
		SessionID string            `json:"sessionId"`
		Backend   string            `json:"backend"`
		Isolated  bool              `json:"isolated"`
		Applied   map[string]string `json:"applied"`
		Unapplied []map[string]string
	}
	if err := client.Call("agent.launchWorker", map[string]any{
		"parentThreadId": "t-parent",
		"backend":        "fake",
		"model":          "fake-big",
		"effort":         "high",
		"prompt":         "do the thing",
		"title":          "fake worker",
		"isolation":      "workspace",
	}, &res); err != nil {
		t.Fatalf("launchWorker: %v", err)
	}
	if res.Backend != "fake" || res.SessionID == "" || res.Isolated {
		t.Fatalf("reply = %+v", res)
	}
	if res.Applied["model"] != "fake-small" || res.Applied["effort"] != "high" {
		t.Fatalf("applied = %v", res.Applied)
	}
	if len(res.Unapplied) != 1 || res.Unapplied[0]["option"] != "model" ||
		res.Unapplied[0]["requested"] != "fake-big" ||
		res.Unapplied[0]["applied"] != "fake-small" {
		t.Fatalf("unapplied = %v", res.Unapplied)
	}
	worker, ok := sessions.Get(res.ThreadID)
	if !ok {
		t.Fatal("worker record missing")
	}
	if worker.ParentThreadID != "t-parent" || worker.Role != session.RoleWorker ||
		worker.Model != "fake-small" || worker.Title != "fake worker" {
		t.Fatalf("worker record = %+v", worker)
	}
	if parent, _ := sessions.Get("t-parent"); parent.Role != session.RoleController {
		t.Fatalf("parent role = %q, want controller", parent.Role)
	}
}

// TestLaunchWorkerPersonaAppliedTruth drives the persona channels (plan 16 P3)
// through the whole launch path: agent.launchWorker params → agentStartParams
// → StartSpec (the harness sees them verbatim) → Launched → the applied-truth
// the bridge renders. A harness that applies neither must have both NAMED as
// unapplied; one that applies both must report them and name nothing.
func TestLaunchWorkerPersonaAppliedTruth(t *testing.T) {
	profiles := []map[string]any{{
		"name": "reviewer", "description": "Reviews code",
		"prompt": "You review.", "tools": []string{"Read"}, "model": "fake-small",
	}}
	launch := func(t *testing.T, fake *fakeHarness) (map[string]any, harness.StartSpec) {
		t.Helper()
		sessions := testSessions(t)
		if err := sessions.Put(session.Record{
			ThreadID: "t-parent", Project: t.TempDir(), Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		client := orchTestCore(t, sessions, agent.NewTurnTracker(), fake)
		var res map[string]any
		if err := client.Call("agent.launchWorker", map[string]any{
			"parentThreadId": "t-parent",
			"backend":        "fake",
			"prompt":         "do the thing",
			"isolation":      "workspace",
			"systemPrompt":   "You are the arena's scout.",
			"agents":         profiles,
		}, &res); err != nil {
			t.Fatalf("launchWorker: %v", err)
		}
		return res, fake.spec()
	}

	t.Run("unsupported", func(t *testing.T) {
		res, spec := launch(t, &fakeHarness{})
		// The harness must SEE the request even when it cannot honor it.
		if spec.SystemPrompt != "You are the arena's scout." || len(spec.Agents) != 1 ||
			spec.Agents[0].Name != "reviewer" || spec.Agents[0].Model != "fake-small" ||
			len(spec.Agents[0].Tools) != 1 || spec.Agents[0].Tools[0] != "Read" {
			t.Fatalf("StartSpec persona = %q / %+v", spec.SystemPrompt, spec.Agents)
		}
		if res["systemPromptApplied"] != false || res["appliedAgents"] != nil {
			t.Errorf("applied persona reported for a harness without it: %v", res)
		}
		var options []string
		for _, u := range res["unapplied"].([]any) {
			options = append(options, u.(map[string]any)["option"].(string))
		}
		if len(options) != 2 || options[0] != "system_prompt" ||
			options[1] != "agents[reviewer]" {
			t.Fatalf("unapplied = %v, want the system prompt and the profile", options)
		}
	})

	t.Run("supported", func(t *testing.T) {
		res, _ := launch(t, &fakeHarness{personaApplied: true})
		if res["systemPromptApplied"] != true {
			t.Errorf("systemPromptApplied = %v", res["systemPromptApplied"])
		}
		applied, _ := res["appliedAgents"].([]any)
		if len(applied) != 1 || applied[0] != "reviewer" {
			t.Errorf("appliedAgents = %v", res["appliedAgents"])
		}
		if u, _ := res["unapplied"].([]any); len(u) != 0 {
			t.Errorf("fully applied persona reported as unapplied: %v", u)
		}
	})
}

// stubCore registers canned RPC handlers and returns a bridge wired to them,
// so the bridge tools can be exercised without any real agent processes.
func stubCore(t *testing.T, handlers map[string]ipc.Handler) *mcpBridge {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "stub.sock")
	srv := ipc.NewServer(sock, log)
	for method, h := range handlers {
		srv.Handle(method, h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stub socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &mcpBridge{client: client, thread: "t-ctrl", workspace: "/ws", log: log}
}

// TestBridgeOrchestrationTools exercises the four bridge tools against a stub
// core: argument validation, the caller-identity plumbing (fromThreadId /
// parentThreadId is always the bridge's own thread), applied-truth rendering
// and the self-close refusal.
func TestBridgeOrchestrationTools(t *testing.T) {
	var launchParams, sendParams, closeParams, discardParams map[string]any
	b := stubCore(t, map[string]ipc.Handler{
		"agent.list": func(_ context.Context, _ json.RawMessage) (any, error) {
			return map[string]any{"threads": []map[string]any{{
				"threadId": "t-w", "status": "dormant",
				"branch": "agentkate/t-w", "path": "/p", "isolated": true,
			}}}, nil
		},
		"agent.discard": func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &discardParams)
			return map[string]any{"ok": true}, nil
		},
		"agent.launchWorker": func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &launchParams)
			return map[string]any{
				"threadId": "t-w", "backend": "kimi", "isolated": true,
				"branch":  "agentkate/t-w",
				"applied": map[string]string{"model": "kimi-code/k2"},
				"unapplied": []map[string]string{{
					"option": "model", "requested": "kimi-code/k3", "applied": "kimi-code/k2",
				}},
			}, nil
		},
		"agent.send": func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &sendParams)
			return map[string]any{"ok": true}, nil
		},
		"agent.wait": func(_ context.Context, raw json.RawMessage) (any, error) {
			return map[string]any{"status": "idle", "lastText": "KIMIPONG"}, nil
		},
		"agent.stopClose": func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &closeParams)
			return map[string]any{"ok": true}, nil
		},
	})

	if _, err := b.runTool("launch_agent", json.RawMessage(`{}`)); err == nil {
		t.Error("launch_agent accepted an empty prompt")
	}
	out, err := b.runTool("launch_agent", json.RawMessage(
		`{"backend":"kimi","model":"kimi-code/k3","prompt":"say KIMIPONG","wait":true}`))
	if err != nil {
		t.Fatalf("launch_agent: %v", err)
	}
	if launchParams["parentThreadId"] != "t-ctrl" {
		t.Errorf("launch parentThreadId = %v, want the bridge's own thread", launchParams["parentThreadId"])
	}
	for _, want := range []string{"Launched worker t-w", "NOT APPLIED", "kimi-code/k2", "KIMIPONG"} {
		if !strings.Contains(out, want) {
			t.Errorf("launch_agent output missing %q:\n%s", want, out)
		}
	}

	out, err = b.runTool("send_agent", json.RawMessage(
		`{"thread_id":"t-w","message":"and again"}`))
	if err != nil {
		t.Fatalf("send_agent: %v", err)
	}
	if sendParams["fromThreadId"] != "t-ctrl" || sendParams["threadId"] != "t-w" {
		t.Errorf("send params = %v", sendParams)
	}
	if !strings.Contains(out, "wait_agent") {
		t.Errorf("fire-and-forget send should point at wait_agent: %s", out)
	}

	out, err = b.runTool("wait_agent", json.RawMessage(`{"thread_id":"t-w"}`))
	if err != nil {
		t.Fatalf("wait_agent: %v", err)
	}
	if !strings.Contains(out, "idle") || !strings.Contains(out, "KIMIPONG") {
		t.Errorf("wait_agent output: %s", out)
	}
	if _, err := b.runTool("wait_agent", json.RawMessage(`{}`)); err == nil {
		t.Error("wait_agent accepted a missing thread_id")
	}

	// Self-targeting is refused bridge-side for all three verbs — a self
	// send/wait would deadlock the caller's own turn until timeout.
	if _, err := b.runTool("close_agent", json.RawMessage(`{"thread_id":"t-ctrl"}`)); err == nil ||
		!strings.Contains(err.Error(), "cannot close itself") {
		t.Errorf("self-close: err = %v, want refusal", err)
	}
	if _, err := b.runTool("send_agent",
		json.RawMessage(`{"thread_id":"t-ctrl","message":"hi"}`)); err == nil ||
		!strings.Contains(err.Error(), "cannot message itself") {
		t.Errorf("self-send: err = %v, want refusal", err)
	}
	if _, err := b.runTool("wait_agent",
		json.RawMessage(`{"thread_id":"t-ctrl"}`)); err == nil ||
		!strings.Contains(err.Error(), "cannot wait on itself") {
		t.Errorf("self-wait: err = %v, want refusal", err)
	}

	// Malformed arguments surface the JSON error, not a misleading
	// missing-field message.
	for tool, bad := range map[string]string{
		"launch_agent": `{"prompt":123}`,
		"send_agent":   `{"thread_id":"t-w","message":"x","wait":"yes"}`,
		"wait_agent":   `{"thread_id":42}`,
		"close_agent":  `{"thread_id":[]}`,
	} {
		if _, err := b.runTool(tool, json.RawMessage(bad)); err == nil ||
			!strings.Contains(err.Error(), "malformed arguments") {
			t.Errorf("%s(%s): err = %v, want malformed-arguments", tool, bad, err)
		}
	}

	// discard_agent carries the caller's identity so the core can gate it.
	if _, err := b.runTool("discard_agent", json.RawMessage(`{"thread_id":"t-w"}`)); err != nil {
		t.Fatalf("discard_agent: %v", err)
	}
	if discardParams["fromThreadId"] != "t-ctrl" || discardParams["threadId"] != "t-w" {
		t.Errorf("discard params = %v", discardParams)
	}

	out, err = b.runTool("close_agent", json.RawMessage(`{"thread_id":"t-w"}`))
	if err != nil {
		t.Fatalf("close_agent: %v", err)
	}
	if closeParams["fromThreadId"] != "t-ctrl" {
		t.Errorf("close params = %v", closeParams)
	}
	if !strings.Contains(out, "archived") {
		t.Errorf("close_agent output: %s", out)
	}
}

// TestOrchestrationToolsAdvertised keeps the catalogue honest: all four
// orchestration verbs are advertised alongside list_agents/discard_agent.
func TestOrchestrationToolsAdvertised(t *testing.T) {
	names := map[string]bool{}
	for _, def := range toolDefs() {
		names[def["name"].(string)] = true
	}
	for _, want := range []string{
		"launch_agent", "send_agent", "wait_agent", "close_agent",
		"list_agents", "discard_agent",
	} {
		if !names[want] {
			t.Errorf("tool %s not advertised", want)
		}
	}
}
