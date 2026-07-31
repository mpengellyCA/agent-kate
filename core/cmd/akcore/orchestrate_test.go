package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
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

// orchTestCore spins a real IPC server with the orchestration handlers over
// real (empty) supervisors, and returns a connected client.
func orchTestCore(t *testing.T, sessions *session.Store, turns *agent.TurnTracker) *ipc.Client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "orch.sock")
	srv := ipc.NewServer(sock, log)

	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, "", ""))
	d := handlerDeps{
		srv: srv, sup: sup, harnesses: harnesses,
		turns: turns, orchGrants: newOrchGrants(),
		sessions: sessions, log: log,
	}
	registerOrchestrationHandlers(d)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	var launchParams, sendParams, closeParams map[string]any
	b := stubCore(t, map[string]ipc.Handler{
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

	if _, err := b.runTool("close_agent", json.RawMessage(`{"thread_id":"t-ctrl"}`)); err == nil ||
		!strings.Contains(err.Error(), "cannot close itself") {
		t.Errorf("self-close: err = %v, want refusal", err)
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
