package main

// The pass-2 half of the caller-binding boundary (audit F34/F35/F36,
// 2026-08-01). Pass 1 built the gates — requireUI for handlers only the human's
// window may reach, requireCallerThread/authorizeAgentTarget for the ones an
// agent may — and then applied them unevenly: the transcript PULL endpoints
// kept no check at all while their push twins closed (F34), wait_agent stayed
// the one orchestration handler that never asked who was reading (F35), and a
// whole cluster of privileged handlers (stop, interrupt, the git mutations, the
// destructive removals, the skill and extension installers) went ungated next
// to siblings that were gated (F36).
//
// These tests pin WHO may call each one. They are deliberately written from the
// attacker's connection: an identified agent bridge, which is the strongest
// caller identity a prompt-injected agent can obtain.

import (
	"encoding/json"
	"errors"
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

// pass2Core is bindingTestCore with a caller-chosen roster, so a test can seed
// a parent/worker pair (inside the caller's subtree) alongside a stranger
// thread (outside it) and tell the two authorisation outcomes apart.
func pass2Core(t *testing.T, records []session.Record) (sock string,
	secrets *bridgeSecrets, broker *permission.Broker, srv *ipc.Server) {
	t.Helper()
	sessions := testSessions(t)
	for _, r := range records {
		if r.Project == "" {
			r.Project = "/p"
		}
		if r.Created.IsZero() {
			r.Created = time.Now()
		}
		if err := sessions.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.ThreadID, err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock = filepath.Join(t.TempDir(), "pass2.sock")
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
	// app.shutdown is registered by runCore, not registerHandlers, so without
	// this it is invisible to every test in this file AND to the registry
	// inventory below — which is precisely how the most powerful RPC the core
	// serves (stop every agent, exit the process) stayed ungated. No-op
	// closures: the gate refuses before either runs, and a test that got past
	// the gate must not take the process down with it.
	registerShutdownHandler(srv, func(progress shutdownProgressFn) {
		progress("done", "", 0, 0)
	}, func() {})
	serveIPC(t, srv, sock)
	return sock, secrets, broker, srv
}

// uiOnlyHandlers is the inventory F34 and F36 close, with parameters chosen so
// that a caller which DID pass the gate stops at the handler's own validation
// rather than doing anything real.
var uiOnlyHandlers = map[string]map[string]any{
	// F34 — the transcript pull endpoints, twins of the push channels F6 closed.
	"agent.transcript": {"threadId": "t-ghost"},
	"session.preview":  {"sessionId": ""},
	// F36 — the privileged cluster.
	"session.forget":           {"sessionId": ""},
	"agent.stop":               {"threadId": "t-ghost"},
	"agent.interrupt":          {"threadId": "t-ghost"},
	"agent.commit":             {"threadId": "t-ghost", "message": "x"},
	"agent.openPR":             {"threadId": "t-ghost", "title": "x"},
	"agent.land":               {"threadId": "t-ghost"},
	"agent.rename":             {"threadId": "t-ghost", "title": "x"},
	"agent.setTags":            {"threadId": "t-ghost", "tags": []string{"x"}},
	"agent.addTag":             {"threadId": "t-ghost", "tag": "x"},
	"agent.removeTag":          {"threadId": "t-ghost", "tag": "x"},
	"git.commit":               {"threadId": "t-ghost", "message": "x"},
	"git.openPR":               {"threadId": "t-ghost", "title": "x"},
	"git.land":                 {"threadId": "t-ghost"},
	"git.discardChanges":       {"threadId": "t-ghost"},
	"git.removeWorktree":       {"threadId": "t-ghost"},
	"git.abortMerge":           {"threadId": "t-ghost"},
	"git.finalizeMerge":        {"threadId": "t-ghost"},
	"git.openConflictTool":     {"threadId": "t-ghost"},
	"cleanup.archiveAndRemove": {"threadId": ""},
	"vsix.install":             {"extensionId": ""},
	"vsix.uninstall":           {"extensionId": ""},
	"skills.install":           {"name": "", "target": ""},
	"skills.uninstall":         {"name": "", "target": ""},
	"skills.create":            {"name": "", "description": ""},
	// Pass 3 — the equivalents F34/F36 gated one verb of and left open in
	// another. Each is UI-only by grep and by data class; see the SECURITY
	// comments at their registrations.
	"search.code":              {"query": "x", "root": "/"},
	"session.browse":           {},
	"session.listThreads":      {},
	"cleanup.restore":          {"threadId": ""},
	"agent.setCompactStrategy": {"threadId": "t-ghost", "strategy": ""},
	"app.shutdown":             {},
	// Pass 4 — the nineteen the pass-3 inventory carried as DEFERRED. Every one
	// is the same data class F34 gated one verb of: file content, the metadata
	// around it, or per-thread state, answered for a caller-chosen thread or
	// repo root with no caller binding. Surveyed: all nineteen are called only
	// from ui/src. See the SECURITY comments at their registrations.
	"agent.diff":                {"threadId": "t-ghost"},
	"agent.summaryStatus":       {"threadId": "t-ghost"},
	"agent.compactNow":          {"threadId": "t-ghost", "model": "local"},
	"agent.suggestTags":         {"project": "/nowhere"},
	"agent.subagentTranscripts": {"threadId": "t-ghost"},
	"skills.read":               {"name": ""},
	"git.file":                  {"path": ""},
	"git.diff":                  {"threadId": "t-ghost"},
	"git.blame":                 {"path": ""},
	"git.log":                   {"threadId": "t-ghost"},
	"git.branches":              {"threadId": "t-ghost"},
	"git.commit.detail":         {"threadId": "t-ghost", "sha": ""},
	"git.commit.diff":           {"threadId": "t-ghost", "sha": ""},
	"git.snapshot":              {},
	"git.prDraft":               {"threadId": "t-ghost"},
	"git.suggestCommitMessage":  {"threadId": "t-ghost"},
	"git.workspaceMergeStatus":  {"threadId": "t-ghost"},
	"cleanup.analyze":           {"project": "/nowhere"},
	"cleanup.listArchived":      {},
}

// TestUIOnlyHandlersRefuseAnAgentBridge: none of the inventory above may be
// driven from an agent's own connection, however it names itself. The refusal
// must be the GATE's — a handler that merely happened to fail its parameter
// validation would prove nothing, so the message is asserted, not just the
// error.
func TestUIOnlyHandlersRefuseAnAgentBridge(t *testing.T) {
	sock, secrets, _, _ := pass2Core(t, []session.Record{{ThreadID: "t-a"}})

	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	asBridge(t, secrets, bridge, "t-a")

	// A connection that never identified as anything at all: the fail-closed
	// case, and the one a local process outside Agent Kate would use.
	stranger, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = stranger.Close() })

	for method, params := range uiOnlyHandlers {
		for who, client := range map[string]*ipc.Client{
			"an agent bridge": bridge, "an unidentified connection": stranger,
		} {
			err := client.CallTimeout(method, params, nil, 10*time.Second)
			if err == nil {
				t.Errorf("%s from %s: succeeded; it must be UI-only", method, who)
				continue
			}
			if !strings.Contains(err.Error(), "only be performed from the Agent Kate window") {
				t.Errorf("%s from %s: err = %v, want the UI-only refusal",
					method, who, err)
				continue
			}
			// ...and it says so under a code that means it (audit F36 pass 3).
			// These refusals used to carry codeCoworkDenied (-32010), the
			// DESKTOP CONSENT code, on thirty handlers with nothing to do with
			// the desktop: git mutations, thread removals, transcript reads,
			// installers, the shutdown RPC. The code is part of the wire
			// contract — a client that branches on it would branch on a lie —
			// so the message is not the only thing that has to be right.
			var rpcErr *ipc.RPCError
			if !errors.As(err, &rpcErr) {
				t.Errorf("%s from %s: err = %T, want an *ipc.RPCError", method, who, err)
				continue
			}
			if rpcErr.Code != codeUIOnly {
				t.Errorf("%s from %s: error code = %d, want codeUIOnly (%d); "+
					"%d is Cowork's consent code and this is not a Cowork refusal",
					method, who, rpcErr.Code, codeUIOnly, codeCoworkDenied)
			}
		}
	}
}

// TestUIOnlyHandlersAdmitTheHumansWindow is the regression half, and the half
// that decides whether these gates SURVIVE: a security fix that breaks the
// ordinary user gets reverted, so "the human's own window still does its work"
// is not a nicety, it is the condition of the gate existing at all.
//
// It used to prove far less than its name promised. Every assertion was an
// "unknown thread"/"is required" error, i.e. the handler's own validation — so
// it passed unchanged with requireUIWindow deleted from all nine handlers, and
// it never once saw a UI-only handler DO anything. This version drives the
// gated handlers against a REAL thread and requires them to SUCCEED. A gate
// that admits nobody, a gate wired to the wrong predicate, a handshake that
// stops granting the role: all of them now fail here, loudly, in the direction
// the user would feel.
//
// No permission responder: none of these paths asks the human, and the short
// timeout turns any accidental prompt into a fast failure rather than an
// eight-minute one.
func TestUIOnlyHandlersAdmitTheHumansWindow(t *testing.T) {
	sock, _, _, _ := pass2Core(t, []session.Record{{ThreadID: "t-a"}})
	ui, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ui.Close() })
	if err := ui.Call("handshake", map[string]any{}, nil); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// Real work on a real thread, one from each family the gates cover: the
	// roster reads, the per-thread mutations, the compaction setting, the
	// session browser, the code search, and the shutdown RPC itself (wired to
	// no-op closures by pass2Core). Each must come back with NO error.
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"agent.transcript", map[string]any{"threadId": "t-a"}},
		{"session.listThreads", map[string]any{}},
		{"session.browse", map[string]any{}},
		{"agent.rename", map[string]any{"threadId": "t-a", "title": "renamed"}},
		{"agent.setTags", map[string]any{"threadId": "t-a", "tags": []string{"one"}}},
		{"agent.addTag", map[string]any{"threadId": "t-a", "tag": "two"}},
		{"agent.removeTag", map[string]any{"threadId": "t-a", "tag": "two"}},
		{"agent.setCompactStrategy", map[string]any{"threadId": "t-a", "strategy": ""}},
		{"search.code", map[string]any{"query": "nothing-matches-this", "root": t.TempDir()}},
		// Pass 4's read family: the gate is only affordable because the UI is
		// the sole caller, so the UI doing its work is the condition of the
		// gate surviving. These three answer with no thread state at all, so a
		// gate wired to the wrong predicate shows up here as an outright
		// refusal rather than as somebody's parameter validation.
		{"git.snapshot", map[string]any{}},
		{"cleanup.listArchived", map[string]any{}},
		{"agent.suggestTags", map[string]any{"project": "/nowhere"}},
		{"app.shutdown", map[string]any{}},
	} {
		if err := ui.CallTimeout(tc.method, tc.params, nil, 20*time.Second); err != nil {
			t.Errorf("%s from the human's own window: %v (it must WORK for the UI)",
				tc.method, err)
		}
	}
	// And the mutations really landed — a gate that quietly swallowed the call
	// and answered "ok" would pass everything above.
	var list struct {
		Threads []session.Record `json:"threads"`
	}
	if err := ui.CallTimeout("session.listThreads", map[string]any{}, &list,
		10*time.Second); err != nil {
		t.Fatalf("session.listThreads: %v", err)
	}
	if len(list.Threads) != 1 || list.Threads[0].Title != "renamed" {
		t.Fatalf("after agent.rename the roster reads %+v, want one thread titled %q",
			list.Threads, "renamed")
	}
	if len(list.Threads[0].Tags) != 1 || list.Threads[0].Tags[0] != "one" {
		t.Errorf("after setTags/addTag/removeTag the tags are %v, want [one]",
			list.Threads[0].Tags)
	}

	// The handlers whose own validation is the only reachable outcome keep
	// their weaker assertion — but it is still the gate's refusal that must be
	// ABSENT, which is the property this test is named for.
	for method, want := range map[string]string{
		"session.preview":          "sessionId is required",
		"session.forget":           "sessionId is required",
		"agent.commit":             "unknown thread",
		"git.discardChanges":       "unknown thread",
		"git.removeWorktree":       "unknown thread",
		"cleanup.archiveAndRemove": "threadId is required",
		"cleanup.restore":          "",
		// The rest of pass 4's read family, driven with the ghost thread the
		// refusal half uses: what matters here is that the UI-only refusal is
		// ABSENT and the handler's own "unknown thread" / "path is required"
		// comes back instead.
		"agent.diff":                "unknown thread",
		"agent.summaryStatus":       "unknown thread",
		"agent.compactNow":          "unknown thread",
		"agent.subagentTranscripts": "unknown thread",
		"git.diff":                  "unknown thread",
		"git.prDraft":               "unknown thread",
		"git.suggestCommitMessage":  "unknown thread",
		"git.workspaceMergeStatus":  "unknown thread",
		"git.log":                   "unknown thread",
		"git.branches":              "unknown thread",
		"git.commit.detail":         "unknown thread",
		"git.commit.diff":           "unknown thread",
		"git.file":                  "path is required",
		"git.blame":                 "path is required",
		"cleanup.analyze":           "",
	} {
		err := ui.CallTimeout(method, uiOnlyHandlers[method], nil, 10*time.Second)
		if err == nil {
			continue // it did real work; better still
		}
		if strings.Contains(err.Error(), uiOnlyRefusal) {
			t.Errorf("%s refused the UI itself: %v", method, err)
			continue
		}
		if want != "" && !strings.Contains(err.Error(), want) {
			t.Errorf("%s from the UI: err = %v, want the handler's own validation (%q)",
				method, err, want)
		}
	}
}

// TestPermissionRefusesPromptlyWithNoUIToAsk (F35 pass 3): askHumanPermission
// pushed its notification and then parked on the 8-minute timer whether or not
// any window had received it. With no UI connected that is eight minutes of a
// bridge connection held on a question nothing will ever display, ending in the
// same refusal it could have given at once.
//
// It fails closed either way, so this is not a hole — it is an eight-minute
// hang where a one-second "no" is correct, and a hang is how a fail-closed
// design gets quietly reverted for being unusable. The assertion is therefore
// on the CLOCK as much as on the answer.
//
// Deliberately no permAutoResponder: that helper claims the UI role, and this
// test is about there being nobody at all.
func TestPermissionRefusesPromptlyWithNoUIToAsk(t *testing.T) {
	sock, secrets, _, _ := pass2Core(t, []session.Record{
		{ThreadID: "t-a"},
		{ThreadID: "t-human", Status: session.StatusDormant}, // outside t-a's subtree
	})
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	asBridge(t, secrets, client, "t-a")

	start := time.Now()
	var res struct {
		Status string `json:"status"`
	}
	// 30s is far below permissionTimeout (8 minutes) and far above the ~1ms
	// this should take, so a regression shows up as a CLIENT timeout with a
	// completely different message rather than as a slow pass.
	err = client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-human", "fromThreadId": "t-a", "timeoutSec": 1,
	}, &res, 30*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("cross-subtree wait with no UI connected: succeeded; it must refuse")
	}
	if !strings.Contains(err.Error(), "no Agent Kate window is connected") {
		t.Errorf("err = %v, want a refusal that NAMES the missing window", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the refusal took %s; with nobody to ask it must be immediate, "+
			"not a wait on the human's timeout", elapsed)
	}
}

// TestPermissionRequestBindsTheAskingThread (F36): the threadId on
// permission.request decides whose PANEL the dialog opens in and whose name
// the human reads on it. Unbound, one thread could raise a prompt in another
// thread's panel — the human approving a Bash line believing it belongs to the
// agent they are watching — or park a request against a thread that will never
// answer it.
func TestPermissionRequestBindsTheAskingThread(t *testing.T) {
	sock, secrets, broker, srv := pass2Core(t, []session.Record{
		{ThreadID: "t-a"}, {ThreadID: "t-b"},
	})
	// A standing "yes" from the human, so that anything which DOES reach the
	// broker is visible as an ask rather than as a hang.
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

	// Asking in someone else's name.
	err = client.CallTimeout("permission.request", map[string]any{
		"threadId": "t-b", "toolName": "Bash",
		"input": map[string]any{"command": "rm -rf ~"},
	}, nil, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "may not request permission for thread t-b") {
		t.Errorf("spoofed permission.request: err = %v, want a binding refusal", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("a spoofed request reached the human (%d asks)", asks.Load())
	}

	// A connection with no identity at all reaches it for nobody.
	stranger, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = stranger.Close() })
	err = stranger.CallTimeout("permission.request", map[string]any{
		"threadId": "t-a", "toolName": "Bash", "input": map[string]any{},
	}, nil, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "may not request permission for thread t-a") {
		t.Errorf("unidentified permission.request: err = %v, want a binding refusal", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("an unidentified request reached the human (%d asks)", asks.Load())
	}

	// The real thing still works: a bridge asking in its OWN name.
	var res struct {
		Allow bool `json:"allow"`
	}
	if err := client.CallTimeout("permission.request", map[string]any{
		"threadId": "t-a", "toolName": "Bash", "input": map[string]any{},
	}, &res, 30*time.Second); err != nil {
		t.Fatalf("own-thread permission.request: %v", err)
	}
	if !res.Allow || asks.Load() != 1 {
		t.Fatalf("own-thread request: allow=%v asks=%d, want allow with exactly one ask",
			res.Allow, asks.Load())
	}
}

// TestWaitAgentBindsTheCaller (F35): wait_agent hands back the target thread's
// last assistant message, which makes it a cross-agent READ — the class
// mcpactivity.go goes out of its way not to open. Its siblings agent.send and
// agent.stopClose have bound their caller since F13; this pins that agent.wait
// now does too.
func TestWaitAgentBindsTheCaller(t *testing.T) {
	sock, secrets, broker, srv := pass2Core(t, []session.Record{
		{ThreadID: "t-a"},                                    // the caller
		{ThreadID: "t-w", ParentThreadID: "t-a"},             // its own worker
		{ThreadID: "t-human", Status: session.StatusDormant}, // a stranger's thread
	})
	var allow atomic.Bool
	var asks atomic.Int32
	permAutoResponder(t, srv, sock, broker, &allow, &asks)

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	asBridge(t, secrets, client, "t-a")

	var res struct {
		Status   string `json:"status"`
		LastText string `json:"lastText"`
	}

	// Naming the victim as itself — the forgery the binding exists to stop.
	err = client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-human", "fromThreadId": "t-human", "timeoutSec": 1,
	}, &res, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "may not act for thread t-human") {
		t.Errorf("impersonating wait: err = %v, want a binding refusal", err)
	}
	// Naming nobody, which authorizeAgentTarget reads as "the UI" and waves
	// through — the quieter half of the same hole.
	err = client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-human", "timeoutSec": 1,
	}, &res, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "fromThreadId is required") {
		t.Errorf("anonymous wait: err = %v, want a binding refusal", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("a forged wait reached the human (%d asks)", asks.Load())
	}

	// Honestly named, but outside its subtree: one human decision, and the
	// human says no — nothing of t-human's conversation comes back.
	allow.Store(false)
	err = client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-human", "fromThreadId": "t-a", "timeoutSec": 1,
	}, &res, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "was not approved by the human") {
		t.Errorf("declined cross-subtree wait: err = %v, want the refusal", err)
	}
	if asks.Load() != 1 {
		t.Fatalf("cross-subtree wait asked the human %d times, want exactly 1", asks.Load())
	}

	// Its OWN worker needs no approval — the gate is a caller check, not a new
	// prohibition on orchestration.
	if err := client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-w", "fromThreadId": "t-a", "timeoutSec": 1,
	}, &res, 10*time.Second); err != nil {
		t.Fatalf("waiting on its own worker: %v", err)
	}
	if res.Status != "exited" {
		t.Errorf("own-worker wait status = %q, want exited", res.Status)
	}
	if asks.Load() != 1 {
		t.Errorf("waiting on its own worker asked the human (%d asks total)", asks.Load())
	}

	// And an unknown id is a typo, answered without troubling anyone.
	err = client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-nope", "fromThreadId": "t-a", "timeoutSec": 1,
	}, &res, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unknown thread") {
		t.Errorf("wait on an unknown thread: err = %v", err)
	}
	if asks.Load() != 1 {
		t.Errorf("an unknown thread was put to the human (%d asks total)", asks.Load())
	}
}

// TestCompositeSendAndWaitCostsOneApproval (F35 pass 3): send_agent(wait:true)
// is one operation to the model and one operation to the human, and it must be
// one DECISION.
//
// Round 1 made it two. The bridge asked for the `send_agent` grant, delivered
// the message, and then called agent.wait — which asked again, for `wait_agent`,
// because a grant keys on (from, target, ACTION). Two prompts for one action
// teaches the human to click through them, and the second one arrived AFTER
// delivery: approve the send, decline the wait, and the message is in the
// stranger's inbox while the reply is dropped on the floor. Worst of both
// answers, from a human who thought they had said no.
//
// The fix is NOT to weaken wait_agent — the standalone case is the hole F35
// closed, and TestWaitAgentBindsTheCaller still pins it. It is to declare the
// wait up front, so the human is asked once, before anything is delivered, for
// what the call is really going to do.
func TestCompositeSendAndWaitCostsOneApproval(t *testing.T) {
	sock, secrets, broker, srv := pass2Core(t, []session.Record{
		{ThreadID: "t-a"},
		{ThreadID: "t-x", Status: session.StatusDormant}, // outside t-a's subtree
		{ThreadID: "t-y", Status: session.StatusDormant}, // ditto, for the contrast
		{ThreadID: "t-z", Status: session.StatusDormant}, // ditto, for the refusal
	})
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

	// The composite: one declared send-and-wait. The send itself fails (t-x has
	// no live process), which is beside the point — the AUTHORISATION is what
	// this measures, and it happens before delivery.
	_ = client.CallTimeout("agent.send", map[string]any{
		"threadId": "t-x", "fromThreadId": "t-a",
		"text": "status?", "awaitReply": true,
	}, nil, 30*time.Second)
	if got := asks.Load(); got != 1 {
		t.Fatalf("the declared send+wait asked the human %d times, want 1", got)
	}
	// ...and the paired wait rides the SAME decision.
	var res struct {
		Status string `json:"status"`
	}
	if err := client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-x", "fromThreadId": "t-a", "timeoutSec": 1,
	}, &res, 30*time.Second); err != nil {
		t.Fatalf("the wait half of an approved send+wait: %v", err)
	}
	if got := asks.Load(); got != 1 {
		t.Errorf("send_agent(wait:true) cost %d human decisions, want exactly 1", got)
	}

	// The contrast, which is what keeps this from being a weakening of
	// wait_agent: an UNDECLARED send buys nothing but the send. A later wait on
	// the same target is a separate read and asks separately.
	before := asks.Load()
	_ = client.CallTimeout("agent.send", map[string]any{
		"threadId": "t-y", "fromThreadId": "t-a", "text": "fyi",
	}, nil, 30*time.Second)
	if got := asks.Load() - before; got != 1 {
		t.Fatalf("a plain send asked %d times, want 1", got)
	}
	if err := client.CallTimeout("agent.wait", map[string]any{
		"threadId": "t-y", "fromThreadId": "t-a", "timeoutSec": 1,
	}, &res, 30*time.Second); err != nil {
		t.Fatalf("wait after a plain send: %v", err)
	}
	if got := asks.Load() - before; got != 2 {
		t.Errorf("a plain send followed by a wait cost %d decisions, want 2 — "+
			"the send grant must not silently authorise a read", got)
	}

	// And when the human says NO to the composite, nothing is delivered: the
	// single prompt comes before the send, not between the send and the wait.
	allow.Store(false)
	before = asks.Load()
	err = client.CallTimeout("agent.send", map[string]any{
		"threadId": "t-z", "fromThreadId": "t-a",
		"text": "do this", "awaitReply": true,
	}, nil, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "was not approved by the human") {
		t.Errorf("declined send+wait: err = %v, want the refusal BEFORE delivery", err)
	}
	if got := asks.Load() - before; got != 1 {
		t.Errorf("the declined composite asked %d times, want 1", got)
	}
}
