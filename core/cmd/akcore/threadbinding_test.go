package main

// The per-thread handlers' caller binding (audit F13 / §4, 2026-08-01).
//
// agent.send, agent.stopClose and agent.discard each take a `fromThreadId` and
// gate on it (authorizeAgentTarget). Until this round the parameter WAS the
// authority: the handlers discarded the connection identity, so a bridge could
// name any thread as itself — or omit the field, which authorizeAgentTarget
// reads as "the UI" and waves through with no human ask at all. These tests pin
// that the id is now bound to the connection that sent it.

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
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

// bindingTestCore is the real handler set over real (empty) supervisors, with
// two unrelated threads: t-a (the caller) and t-b (a thread in nobody's
// subtree). It returns the socket path, the ledger and the deps' broker so a
// test can count how often the human was asked.
func bindingTestCore(t *testing.T) (sock string, secrets *bridgeSecrets,
	broker *permission.Broker, srv *ipc.Server) {
	t.Helper()
	sessions := testSessions(t)
	for _, id := range []string{"t-a", "t-b"} {
		if err := sessions.Put(session.Record{
			ThreadID: id, Project: "/p", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock = filepath.Join(t.TempDir(), "binding.sock")
	srv = ipc.NewServer(sock, log)
	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, "", ""))
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	broker = permission.New()
	secrets = newBridgeSecrets()
	registerHandlers(handlerDeps{
		srv: srv, harnesses: harnesses, broker: broker,
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		coop: coop.NewState(), threads: newThreadRegistry(),
		gitCache: gitCache, sessions: sessions, log: log,
		bridgeSecrets: secrets,
	})
	serveIPC(t, srv, sock)
	return sock, secrets, broker, srv
}

// TestPerThreadHandlersBindTheCaller is the cross-thread refusal: a bridge
// bound to t-a cannot drive t-b through any of the three handlers, whether it
// claims to BE t-b or claims nothing at all — and the human is never asked,
// because an unbound claim is not a request, it is a forgery.
func TestPerThreadHandlersBindTheCaller(t *testing.T) {
	sock, secrets, broker, srv := bindingTestCore(t)
	// A standing "yes" from the human. The gate must refuse these calls WITHOUT
	// reaching it: an approval the human would have granted is not the point —
	// the point is that nobody asked on t-b's behalf.
	var allow atomic.Bool
	var asks atomic.Int32
	allow.Store(true)
	permAutoResponder(t, srv, sock, broker, &allow, &asks)

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	asBridge(t, secrets, client, "t-a")

	for _, method := range []string{"agent.send", "agent.stopClose", "agent.discard"} {
		// Claiming to be the thread it is attacking: with the id unbound this
		// passed as t-b acting on itself — in its own subtree, so not even an
		// approval prompt stood in the way.
		err := client.Call(method, map[string]any{
			"threadId": "t-b", "fromThreadId": "t-b", "text": "do as I say",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "may not act for thread t-b") {
			t.Errorf("%s from a bridge impersonating t-b: err = %v, want a binding refusal",
				method, err)
		}
		// Omitting the caller entirely: authorizeAgentTarget reads "" as the UI.
		err = client.Call(method, map[string]any{
			"threadId": "t-b", "text": "do as I say",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "fromThreadId is required") {
			t.Errorf("%s from a bridge with no fromThreadId: err = %v, want a refusal",
				method, err)
		}
	}
	if asks.Load() != 0 {
		t.Errorf("the human was asked %d times about calls that were forgeries", asks.Load())
	}

	// The same connection speaking for ITS OWN thread still works: the binding
	// is a caller check, not a new prohibition. It falls through the gate to the
	// handler's normal validation (no live process for t-a here).
	err = client.Call("agent.send", map[string]any{
		"threadId": "t-a", "fromThreadId": "t-a", "text": "note to self",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown thread") {
		t.Errorf("a bridge sending to its own thread: err = %v, want the normal validation", err)
	}
	if asks.Load() != 0 {
		t.Errorf("a self-directed send asked the human (%d asks)", asks.Load())
	}

	// And a connection that never identified at all reaches none of them.
	stranger, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = stranger.Close() })
	for _, method := range []string{"agent.send", "agent.stopClose", "agent.discard"} {
		err := stranger.Call(method, map[string]any{
			"threadId": "t-b", "fromThreadId": "t-a", "text": "hi",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "may not act for thread t-a") {
			t.Errorf("%s from an unidentified connection: err = %v, want a refusal", method, err)
		}
	}
}

// TestUIActsWithoutNamingAThread is the other half: the human's own window
// drives every thread, sends no fromThreadId, and is never gated — the binding
// must not have turned the UI into a caller that has to name itself.
//
// No permission responder here on purpose: if a UI call were gated it would
// block on a prompt nobody answers, and the short CallTimeout below turns that
// into a fast failure instead of an eight-minute one.
func TestUIActsWithoutNamingAThread(t *testing.T) {
	sock, _, _, _ := bindingTestCore(t)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Call("handshake", map[string]any{}, nil); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	// Straight to the handler's own validation, from the gate's point of view a
	// non-event: no caller named, no approval asked.
	err = client.CallTimeout("agent.discard",
		map[string]any{"threadId": "t-b"}, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unknown thread") {
		t.Fatalf("UI discard: err = %v, want the normal validation", err)
	}
	err = client.CallTimeout("agent.send",
		map[string]any{"threadId": "t-b", "text": "hello"}, nil, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unknown thread") {
		t.Fatalf("UI send: err = %v, want the normal validation", err)
	}
}
