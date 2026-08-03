package main

// Regression tests for the authority gates added after the kimi-k3 audit
// (F1 launch escalation, F5 UI-only thread creation, F6 permission feed
// isolation, F13 single UI-role binding). Every one of them fails if the gate
// is removed — that is the point of writing them.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os/exec"
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
	"agentkate/internal/worktree"
)

// markClientUI gives an ipc.Client the UI role on the server it is talking to.
// Registered as its own method rather than reusing `handshake` so it works on
// the trimmed handler sets the focused tests build.
func markClientUI(t *testing.T, srv *ipc.Server, client *ipc.Client) {
	t.Helper()
	srv.Handle("test.markUI", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !srv.MarkUI(ctx) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest, "UI role refused")
		}
		return map[string]any{"ok": true}, nil
	})
	if err := client.Call("test.markUI", map[string]any{}, nil); err != nil {
		t.Fatalf("test.markUI: %v", err)
	}
}

// TestPermissivenessOrdering pins the one cross-engine ordering every
// escalation decision is made on. If this table drifts, F1's gate silently
// changes shape.
func TestPermissivenessOrdering(t *testing.T) {
	// Ascending authority, claude's vocabulary.
	claude := []string{"plan", "default", "acceptEdits", "auto", "dontAsk", "bypassPermissions"}
	for i := 1; i < len(claude); i++ {
		if requestedPermissiveness(claude[i]) <= requestedPermissiveness(claude[i-1]) {
			t.Errorf("%s is not ranked above %s", claude[i], claude[i-1])
		}
	}
	// kimi's vocabulary shares three names and adds yolo at the very top.
	if requestedPermissiveness("yolo") != requestedPermissiveness("bypassPermissions") {
		t.Error("yolo and bypassPermissions must sit on the same rung")
	}
	if requestedPermissiveness("yolo") <= requestedPermissiveness("auto") {
		t.Error("yolo must outrank auto")
	}
	// Fail closed both ways: an unknown REQUEST outranks everything (so it is
	// asked about), an unknown or missing HELD mode ranks at the floor (so it
	// delegates nothing).
	if requestedPermissiveness("someFutureMode") <= requestedPermissiveness("bypassPermissions") {
		t.Error("an unknown requested mode must outrank every known mode")
	}
	if heldPermissiveness("someFutureMode") != 0 || heldPermissiveness("") != 0 {
		t.Error("an unreadable held mode must delegate no authority")
	}
	if heldPermissiveness("bypassPermissions") != requestedPermissiveness("bypassPermissions") {
		t.Error("a known mode must rank the same held as requested")
	}
}

// TestLaunchBaselineRank pins what an unspecified permission_mode is worth:
// exactly acceptEdits on a static-vocabulary engine (claude's supervisor
// rewrites it), and the ENGINE'S OWN reported default — not the launcher's
// mode — on a discovered-vocabulary engine (kimi).
func TestLaunchBaselineRank(t *testing.T) {
	static := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "claude"}
	discovered := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "kimi"}

	if got := launchBaselineRank(static, ""); got != rankStaticEngineDefault {
		t.Errorf("static baseline = %d, want %d", got, rankStaticEngineDefault)
	}
	// The engine's real default is used verbatim. A launcher PINNED below it
	// (kimi in "plan") must therefore read as an escalation — the bug the old
	// launcher-mode approximation hid.
	if got := launchBaselineRank(discovered, "default"); got != permissivenessRanks["default"] {
		t.Errorf("discovered baseline = %d, want the engine's own default rank", got)
	}
	if launchBaselineRank(discovered, "default") <= heldPermissiveness("plan") {
		t.Error("a plan-pinned launcher must not out-rank its engine's default")
	}
	// An unreadable (probe failed) or unranked default falls back to the most
	// permissive baseline any engine applies, so it is asked about, not assumed.
	if got := launchBaselineRank(discovered, ""); got != rankStaticEngineDefault {
		t.Errorf("unreadable discovered baseline = %d, want %d", got, rankStaticEngineDefault)
	}
	if got := launchBaselineRank(discovered, "someFutureMode"); got != rankUnknownMode {
		t.Errorf("unranked discovered baseline = %d, want %d", got, rankUnknownMode)
	}
}

// TestEngineDefaultPermissionMode proves the gate reads the value the audit's
// verifier pointed at: the `mode` option's currentValue, which the harness
// already discovers and reports.
func TestEngineDefaultPermissionMode(t *testing.T) {
	discovered := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "kimi"}
	h := &optionFake{fakeHarness: &fakeHarness{}, defaultMode: "auto"}
	if got := engineDefaultPermissionMode(h, discovered); got != "auto" {
		t.Errorf("engine default = %q, want auto", got)
	}
	// A failing catalogue reports nothing so the caller can fail closed.
	broken := &optionFake{fakeHarness: &fakeHarness{}, err: errors.New("no CLI")}
	if got := engineDefaultPermissionMode(broken, discovered); got != "" {
		t.Errorf("failed probe reported %q, want the empty fail-closed answer", got)
	}
}

// optionFake is a fakeHarness with a scripted option-discovery answer.
type optionFake struct {
	*fakeHarness
	defaultMode string
	err         error
}

func (o *optionFake) Catalogue(context.Context, harness.CatalogueScope) (harness.CatalogueSnapshot, error) {
	if o.err != nil {
		return harness.CatalogueSnapshot{}, o.err
	}
	snapshot := harness.CatalogueSnapshot{ContractVersion: harness.ContractVersion, HarnessID: o.Descriptor().ID,
		Settings: []harness.SettingDescriptor{{Key: harness.SettingPermissionMode, DisplayName: "Mode", DefaultValue: o.defaultMode, Timing: harness.TimingLaunch}}}
	snapshot.Revision = harness.CatalogueRevision(snapshot)
	return snapshot, nil
}

// runningFake is a fakeHarness whose threads all report as live, for the
// concurrency-cap tests.
type runningFake struct{ *fakeHarness }

func (runningFake) Running(string) bool { return true }

// --- project fixtures --------------------------------------------------------
//
// The isolation half of the gate asks the worktree layer what a project would
// ACTUALLY do, so these tests need real repositories: the difference between
// "auto means an isolated worktree" and "auto means the human's own files" is a
// property of the repo, not of the request.

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@agentkate.invalid"},
		{"config", "user.name", "Agent Kate Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// repoWithCommit is the ordinary project: isolation "auto" gets a real worktree.
func repoWithCommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", "root")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	return dir
}

// repoNoCommits is the project that makes the requested mode a lie: `git
// worktree add` has nothing to branch from, so worktree.Create degrades "auto"
// to the workspace itself — the human's real files.
func repoNoCommits(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir)
	return dir
}

// authTestDeps builds handlerDeps with a broker and an auto-responding "human",
// and returns the ask counter and the allow flag the responder reads.
func authTestDeps(t *testing.T, sessions *session.Store, h harness.Harness) (handlerDeps, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "auth.sock")
	srv := ipc.NewServer(sock, log)
	broker := permission.New()
	harnesses := harness.NewRegistry(h.Descriptor().ID)
	harnesses.Register(h)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	d := handlerDeps{
		srv: srv, broker: broker, harnesses: harnesses,
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		workerSlots: newWorkerSlots(),
		threads:     newThreadRegistry(), gitCache: gitCache,
		sessions: sessions, log: log,
	}
	serveIPC(t, srv, sock)
	var allow atomic.Bool
	var asks atomic.Int32
	permAutoResponder(t, srv, sock, broker, &allow, &asks)
	return d, &allow, &asks
}

// gate calls the authority gate and releases the reserved slot immediately —
// for the tests that are about the DECISION rather than the reservation.
// TestWorkerLaunchReservationIsAtomic below holds slots instead.
func (d handlerDeps) gate(parent session.Record, caps harness.HarnessDescriptor,
	req workerLaunchRequest) error {
	release, err := d.authorizeWorkerLaunch(parent, caps, "", req)
	release()
	return err
}

// TestWorkerLaunchEscalationNeedsTheHuman is F1's regression test: a launch
// that would hand the worker MORE authority than the launcher holds stops for
// the human, and a refusal means the worker is not launched.
func TestWorkerLaunchEscalationNeedsTheHuman(t *testing.T) {
	sessions := testSessions(t)
	// A project with commits, so isolation "auto" really is an isolated
	// worktree and the only reasons to ask are the ones this test creates.
	parent := session.Record{
		ThreadID: "t-parent", Project: repoWithCommit(t), Backend: "fake",
		PermissionMode: "acceptEdits", Created: time.Now(),
	}
	if err := sessions.Put(parent); err != nil {
		t.Fatalf("Put: %v", err)
	}
	caps := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "fake"}
	fake := &fakeHarness{}
	d, allow, asks := authTestDeps(t, sessions, fake)

	base := workerLaunchRequest{ParentThreadID: "t-parent", Backend: "fake",
		Prompt: "audit the repo"}

	// Same mode, isolated worktree: no human in the loop at all.
	allow.Store(true)
	if err := d.gate(parent, caps, base); err != nil {
		t.Fatalf("an unescalated launch was gated: %v", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("an unescalated launch asked the human (%d asks)", asks.Load())
	}

	// A more permissive mode asks — and a refusal is a refusal.
	allow.Store(false)
	esc := base
	esc.PermissionMode = "bypassPermissions"
	err := d.gate(parent, caps, esc)
	if err == nil {
		t.Fatal("a bypassPermissions worker was approved without the human")
	}
	if !strings.Contains(err.Error(), "NOT APPLIED") {
		t.Errorf("refusal does not use the applied-truth vocabulary: %v", err)
	}
	if asks.Load() != 1 {
		t.Fatalf("escalating launch asks = %d, want 1", asks.Load())
	}

	// Approval lets exactly THAT launch through, and is never cached: the next
	// identical launch asks again.
	allow.Store(true)
	if err := d.gate(parent, caps, esc); err != nil {
		t.Fatalf("approved escalation refused: %v", err)
	}
	if err := d.gate(parent, caps, esc); err != nil {
		t.Fatalf("second approved escalation refused: %v", err)
	}
	if asks.Load() != 3 {
		t.Fatalf("approvals were cached: %d asks, want 3", asks.Load())
	}

	// Workspace isolation is authority on its own, at the launcher's own mode.
	ws := base
	ws.Isolation = "workspace"
	if err := d.gate(parent, caps, ws); err != nil {
		t.Fatalf("approved workspace launch refused: %v", err)
	}
	if asks.Load() != 4 {
		t.Fatalf("workspace isolation did not ask: %d asks, want 4", asks.Load())
	}
	allow.Store(false)
	if err := d.gate(parent, caps, ws); err == nil {
		t.Fatal("a refused workspace launch was allowed")
	}

	// An unknown mode is treated as maximally permissive, so it is asked about
	// rather than waved through.
	future := base
	future.PermissionMode = "someFutureMode"
	if err := d.gate(parent, caps, future); err == nil {
		t.Fatal("an unrecognised mode was waved through")
	}
}

// TestWorkerLaunchCaps proves fan-out is bounded per launcher and per tree, and
// that a cap is a refusal rather than a question the agent can get answered.
func TestWorkerLaunchCaps(t *testing.T) {
	sessions := testSessions(t)
	proj := repoWithCommit(t)
	put := func(id, parent string) {
		if err := sessions.Put(session.Record{
			ThreadID: id, Project: proj, Backend: "fake", ParentThreadID: parent,
			PermissionMode: "acceptEdits", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	put("t-root", "")
	parent, _ := sessions.Get("t-root")
	caps := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "fake"}
	fake := &fakeHarness{}
	d, allow, asks := authTestDeps(t, sessions, runningFake{fake})
	allow.Store(true)

	req := workerLaunchRequest{ParentThreadID: "t-root", Backend: "fake", Prompt: "go"}
	for i := 0; i < maxLiveWorkersPerParent; i++ {
		if err := d.gate(parent, caps, req); err != nil {
			t.Fatalf("launch %d refused below the cap: %v", i, err)
		}
		put("t-w"+string(rune('a'+i)), "t-root")
	}
	err := d.gate(parent, caps, req)
	if err == nil {
		t.Fatal("the per-launcher cap did not refuse")
	}
	if !strings.Contains(err.Error(), "NOT APPLIED") {
		t.Errorf("cap refusal does not use the applied-truth vocabulary: %v", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("a cap refusal asked the human (%d asks) — a cap is not negotiable", asks.Load())
	}

	// A worker launching sub-workers is bound by the TREE cap, so a chain
	// cannot multiply past it. Fill the tree using SEVERAL sub-parents, none of
	// them at its own cap, so what refuses can only be the tree total.
	total := maxLiveWorkersPerParent
	for i := 0; total < maxLiveWorkersPerTree; i++ {
		under := "t-w" + string(rune('a'+i/4)) // ≤4 each: well under the per-parent cap
		put("t-d"+string(rune('a'+i)), under)
		total++
	}
	// t-wh has no sub-workers of its own, so its per-parent count is 0.
	deep, _ := sessions.Get("t-wh")
	deepReq := workerLaunchRequest{ParentThreadID: "t-wh", Backend: "fake", Prompt: "go"}
	err = d.gate(deep, caps, deepReq)
	if err == nil {
		t.Fatal("the per-tree cap did not refuse a sub-worker launch")
	}
	if !strings.Contains(err.Error(), "orchestration tree") {
		t.Errorf("a sub-worker launch was refused by the wrong cap: %v", err)
	}
	if asks.Load() != 0 {
		t.Fatalf("a cap refusal asked the human (%d asks)", asks.Load())
	}
}

// uiOnlyTestCore builds the real handler set on a real bus and returns the
// socket path plus an anonymous (roleless) client, for the caller-role tests.
func uiOnlyTestCore(t *testing.T, sessions *session.Store) (string, *ipc.Client) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "roles.sock")
	srv := ipc.NewServer(sock, log)
	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, "", ""))
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	d := handlerDeps{
		srv: srv, harnesses: harnesses, broker: permission.New(),
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		coop: coop.NewState(), threads: newThreadRegistry(),
		gitCache: gitCache, sessions: sessions, log: log,
	}
	registerHandlers(d)
	serveIPC(t, srv, sock)
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return sock, client
}

// TestThreadCreationIsUIOnly is F5/F6's regression test: the handlers that
// create threads with caller-supplied authority, and the one that answers the
// human's permission prompt, refuse a caller that has not identified as the UI.
func TestThreadCreationIsUIOnly(t *testing.T) {
	sessions := testSessions(t)
	if err := sessions.Put(session.Record{
		ThreadID: "t-dormant", Project: t.TempDir(), SessionID: "s-1",
		Backend: "claude", Created: time.Now(), Status: session.StatusDormant,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, anon := uiOnlyTestCore(t, sessions)

	for name, call := range map[string]struct {
		method string
		params map[string]any
	}{
		"agent.start": {"agent.start", map[string]any{
			"workspacePath": t.TempDir(), "prompt": "do it",
			"coworkEnabled": true, "permissionMode": "bypassPermissions",
			"env": map[string]any{"ANTHROPIC_BASE_URL": "http://evil.invalid"},
		}},
		"agent.resume": {"agent.resume", map[string]any{"threadId": "t-dormant"}},
		"session.attach": {"session.attach", map[string]any{
			"sessionId": "s-2", "project": t.TempDir()}},
		"mode.apply":         {"mode.apply", map[string]any{"name": "x", "workDir": t.TempDir()}},
		"permission.respond": {"permission.respond", map[string]any{"requestId": "r-1", "allow": true}},
		// The three F5 sites the first pass missed. agent.fork CREATES a thread
		// carrying the source's whole authority; agent.promote relaunches one
		// somewhere else; agent.updateSettings can raise a LIVE thread's permission
		// mode, which is the shortest escalation path in the whole surface.
		"agent.fork":    {"agent.fork", map[string]any{"threadId": "t-dormant"}},
		"agent.promote": {"agent.promote", map[string]any{"threadId": "t-dormant"}},
		"agent.updateSettings": {"agent.updateSettings", map[string]any{
			"agentRef": map[string]any{"threadId": "t-dormant"},
			"requested": map[string]any{"permissionMode": "bypassPermissions"}}},
		// ...and the stored recipes that decide what a future thread starts with.
		"mode.save": {"mode.save", map[string]any{
			"mode": map[string]any{"name": "planted"}}},
		"mode.delete": {"mode.delete", map[string]any{"name": "planted"}},
	} {
		if err := anon.Call(call.method, call.params, nil); err == nil {
			t.Errorf("%s accepted a caller with no UI role", name)
		} else if !strings.Contains(err.Error(), "Agent Kate window") {
			t.Errorf("%s refused for the wrong reason: %v", name, err)
		}
	}
	// The roleless caller could not even have created a thread by racing.
	if recs := sessions.List(""); len(recs) != 1 {
		t.Fatalf("a refused caller created %d threads", len(recs)-1)
	}
}

// frameProbe is a raw connection that reports every NOTIFICATION frame the
// server pushes to it, params included — rawProbe keeps only method names, and
// F6 is precisely about what travels in the params.
type frameProbe struct {
	conn    net.Conn
	notes   chan ipc.Frame
	replies chan ipc.Frame
}

func newFrameProbe(t *testing.T, sock string) *frameProbe {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p := &frameProbe{conn: conn,
		notes: make(chan ipc.Frame, 16), replies: make(chan ipc.Frame, 16)}
	go func() {
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var f ipc.Frame
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			if f.Method == "" {
				p.replies <- f
				continue
			}
			p.notes <- f
		}
	}()
	return p
}

func (p *frameProbe) call(t *testing.T, method string) ipc.Frame {
	t.Helper()
	if _, err := p.conn.Write([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{}}` + "\n")); err != nil {
		t.Fatalf("probe write: %v", err)
	}
	select {
	case f := <-p.replies:
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never answered", method)
		return ipc.Frame{}
	}
}

// TestPermissionRequestIsNotBroadcast is F6's other half: the human's prompt —
// which carries the request id AND the raw tool input — reaches the UI and no
// other connection.
func TestPermissionRequestIsNotBroadcast(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "feed.sock")
	srv := ipc.NewServer(sock, log)
	broker := permission.New()
	srv.Handle("test.markUI", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !srv.MarkUI(ctx) {
			return nil, ipc.Errorf(ipc.CodeInvalidRequest, "UI role refused")
		}
		return map[string]any{"ok": true}, nil
	})
	serveIPC(t, srv, sock)

	ui := newFrameProbe(t, sock)
	if f := ui.call(t, "test.markUI"); f.Error != nil {
		t.Fatalf("UI role: %v", f.Error)
	}
	// A second, unidentified connection — a stand-in for another agent's bridge
	// or any same-uid watcher. The reply proves the server accepted it before
	// the ask goes out.
	watcher := newFrameProbe(t, sock)
	watcher.call(t, "watcher.barrier")

	done := make(chan bool, 1)
	go func() {
		_, ok := askHumanPermission(srv, broker, "t-1", "Bash",
			json.RawMessage(`{"command":"cat /home/u/.ssh/id_ed25519"}`))
		done <- ok
	}()

	var ask ipc.Frame
	select {
	case ask = <-ui.notes:
	case <-time.After(3 * time.Second):
		t.Fatal("the UI never received permission.requested")
	}
	if ask.Method != "permission.requested" {
		t.Fatalf("UI saw %q first, want permission.requested", ask.Method)
	}
	select {
	case m := <-watcher.notes:
		t.Fatalf("an unidentified connection received %q with params %s",
			m.Method, string(m.Params))
	case <-time.After(250 * time.Millisecond):
	}

	// Resolve so the ask does not sit out its 8-minute timeout.
	var p struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(ask.Params, &p); err != nil || p.RequestID == "" {
		t.Fatalf("permission.requested carried no requestId: %s", string(ask.Params))
	}
	broker.Resolve(p.RequestID, permission.Decision{Allow: false})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("askHumanPermission never returned")
	}
}

// TestSecondUIRoleClaimRefused is F13's partial mitigation: while one
// connection holds the UI role, a second cannot assert it — and the refusal is
// an error the client can see, not a silent downgrade.
func TestSecondUIRoleClaimRefused(t *testing.T) {
	sock, first := uiOnlyTestCore(t, testSessions(t))
	if err := first.Call("handshake", map[string]any{}, nil); err != nil {
		t.Fatalf("first handshake: %v", err)
	}
	// A second connection to the SAME server.
	second := newFrameProbe(t, sock)
	if f := second.call(t, "handshake"); f.Error == nil {
		t.Fatal("a second connection was granted the UI role")
	}
	// The first client keeps its authority.
	if err := first.Call("permission.respond",
		map[string]any{"requestId": "r-none", "allow": false}, nil); err != nil {
		t.Fatalf("the real UI lost its role: %v", err)
	}
}

// --- what the human actually reads ------------------------------------------

// renderPermSummary is a PORT of the UI's permission-bar summariser
// (ui/src/AgentChatHelpers.cpp: permSummary + mcpSummary, rendered by
// ui/src/AgentPanel.cpp::showNextPermission). It exists so the core can prove
// what its approval payload turns into on screen, which is the only thing the
// human decides on.
//
// Keep it in step with the C++ if that file changes — the two are a contract,
// and F1 stayed exploitable precisely because nobody checked the payload
// against the renderer.
func renderPermSummary(toolName string, input map[string]any) string {
	str := func(k string) string {
		s, _ := input[k].(string)
		return s
	}
	// mcpSummary is consulted only for the arena's own MCP tool names.
	if strings.HasPrefix(toolName, "mcp__cooperation__") ||
		strings.HasPrefix(toolName, "mcp__cowork__") {
		verb := toolName
		if i := strings.Index(verb, "__"); i >= 0 {
			if j := strings.Index(verb[i+2:], "__"); j >= 0 {
				verb = verb[i+2+j+2:]
			}
		}
		if verb == "launch_agent" {
			// The digest that swallowed the escalation prompt: engine + title,
			// nothing else, and "same engine" when the arguments are absent.
			engine := str("backend")
			if m := str("model"); m != "" {
				if engine == "" {
					engine = m
				} else {
					engine += "/" + m
				}
			}
			if engine == "" {
				engine = "same engine"
			}
			if t := str("title"); t != "" {
				return engine + ": " + t
			}
			return engine
		}
	}
	switch toolName {
	case "Bash":
		return str("command")
	case "WebFetch":
		return str("url")
	case "WebSearch":
		return str("query")
	}
	for _, k := range []string{"file_path", "path", "pattern", "description"} {
		if v := str(k); v != "" {
			return v
		}
	}
	b, _ := json.Marshal(input)
	return string(b)
}

// permBarText is what the bar shows: the tool name, then the summary elided at
// 240 characters (AgentPanel.cpp).
func permBarText(toolName string, input map[string]any) string {
	sum := renderPermSummary(toolName, input)
	if len([]rune(sum)) > 240 {
		sum = string([]rune(sum)[:240]) + "…"
	}
	return "Allow the agent to use " + toolName + "? " + sum
}

// TestEscalationPromptRendersTheFacts is F1's "informed human" half: an
// approval dialog the human cannot understand is not a control. It asserts on
// the RENDERED text, not on the payload keys, because the payload was already
// "correct" while the dialog said the words "same engine".
func TestEscalationPromptRendersTheFacts(t *testing.T) {
	sessions := testSessions(t)
	parent := session.Record{
		ThreadID: "t-parent", Project: "/p", Backend: "fake",
		Title: "Refactor the parser", PermissionMode: "acceptEdits",
		DisallowedTools: []string{"Bash"}, Created: time.Now(),
	}
	if err := sessions.Put(parent); err != nil {
		t.Fatalf("Put: %v", err)
	}
	req := workerLaunchRequest{
		ParentThreadID: "t-parent", Backend: "fake",
		PermissionMode: "bypassPermissions", Isolation: "workspace",
		Title:  "worker",
		Prompt: "rewrite every test file so the suite passes\nsecond line",
		// A relaxation on top: the worker sheds the parent's Bash ban.
		DisallowedTools: []string{},
	}
	// The gate's own payload, rendered through the UI's own summariser. The ask
	// itself is covered by TestWorkerLaunchEscalationNeedsTheHuman.
	_, _, relax := inheritRestrictions(parent, req)
	summary := escalationSummary(parent, req, relax, true, worktree.ModeWorkspace)
	rendered := permBarText(launchApprovalTool, map[string]any{"description": summary})

	// Every fact the audit requires, in the words the human sees.
	for _, want := range []string{
		"MORE authority than the asker", // approving creates authority the asker lacks
		"bypassPermissions",             // the mode it wants
		"acceptEdits",                   // the mode it holds
		"YOUR real files",               // what "workspace" isolation MEANS
		"Un-bans tools",                 // the restriction it sheds
		"Bash",
		"t-parent", // who is asking
		"First:",   // the worker's opening line
		"rewrite every test",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the dialog never says %q.\nrendered: %s", want, rendered)
		}
	}
	// ...and the bar never had to elide, so nothing was cut off the end. (An
	// ellipsis INSIDE the summary is fine: the agent's own text is budgeted.)
	if n := len([]rune(summary)); n > 240 {
		t.Errorf("summary is %d characters, so the bar elides a fact:\n%s", n, summary)
	}
	// The regression itself: under the tool name the UI DOES digest, this same
	// prompt renders as two words about the engine. If a future change routes
	// the ask back through it, this fails.
	if got := renderPermSummary("mcp__cooperation__launch_agent",
		map[string]any{"description": summary}); got != "same engine" {
		t.Fatalf("the UI digest changed (%q); re-check launchApprovalTool", got)
	}
	if strings.HasPrefix(launchApprovalTool, "mcp__") {
		t.Error("the approval tool name is digested by the UI again")
	}
}

// TestEscalationSummaryFitsTheBar proves the budget holds against agent-chosen
// text: a worker prompt and a thread title are attacker-controlled, and neither
// may push a fact past the bar's elision point.
func TestEscalationSummaryFitsTheBar(t *testing.T) {
	parent := session.Record{
		ThreadID: "t-parent", PermissionMode: "plan",
		Title: strings.Repeat("padding ", 200),
	}
	req := workerLaunchRequest{
		ParentThreadID: "t-parent", PermissionMode: "bypassPermissions",
		Isolation: "workspace", Prompt: strings.Repeat("filler ", 500),
	}
	got := escalationSummary(parent, req, []string{
		"Un-bans tools: " + strings.Repeat("Bash,", 50) + "."}, true,
		worktree.ModeWorkspace)
	if len(got) > escalationSummaryLimit {
		t.Errorf("summary is %d bytes, over the %d the bar renders:\n%s",
			len(got), escalationSummaryLimit, got)
	}
	for _, want := range []string{"MORE authority than the asker", "bypassPermissions",
		"YOUR real files"} {
		if !strings.Contains(got, want) {
			t.Errorf("a fact was crowded out by agent text: %q missing from %s", want, got)
		}
	}
}

// --- reservations ------------------------------------------------------------

// TestWorkerLaunchReservationIsAtomic is the check-then-act regression: the cap
// counts persisted records, and a worker's record is written long AFTER the
// gate runs, so concurrent launches used to all pass one count. Every launch
// here starts before any of them finishes — exactly the window — and no more
// than the cap may get through.
func TestWorkerLaunchReservationIsAtomic(t *testing.T) {
	sessions := testSessions(t)
	parent := session.Record{
		ThreadID: "t-root", Project: repoWithCommit(t), Backend: "fake",
		PermissionMode: "acceptEdits", Created: time.Now(),
	}
	if err := sessions.Put(parent); err != nil {
		t.Fatalf("Put: %v", err)
	}
	caps := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "fake"}
	d, allow, _ := authTestDeps(t, sessions, runningFake{&fakeHarness{}})
	allow.Store(true)

	const tries = maxLiveWorkersPerParent * 3
	var granted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	releases := make(chan func(), tries)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all of them inside the launch window at once
			release, err := d.authorizeWorkerLaunch(parent, caps, "",
				workerLaunchRequest{ParentThreadID: "t-root", Backend: "fake",
					Prompt: "go"})
			if err == nil {
				granted.Add(1)
				releases <- release // held: no record is persisted in this test
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := granted.Load(); got != maxLiveWorkersPerParent {
		t.Fatalf("%d concurrent launches passed the cap of %d",
			got, maxLiveWorkersPerParent)
	}
	// Releasing frees the slots again — a reservation that leaked would wedge
	// the controller out of launching anything for the rest of the run.
	close(releases)
	for r := range releases {
		r()
		r() // idempotent: a double release must not free somebody else's slot
	}
	if err := d.gate(parent, caps, workerLaunchRequest{
		ParentThreadID: "t-root", Backend: "fake", Prompt: "go"}); err != nil {
		t.Fatalf("released slots were not returned to the pool: %v", err)
	}
}

// --- restrictions -------------------------------------------------------------

// TestWorkerInheritsParentRestrictions is the third F1 bug: a worker's start
// params carried no DisallowedTools/AddDirs at all, so every worker escaped its
// launcher's restrictions by default — and asking for fewer of them was not
// measured as an escalation either.
func TestWorkerInheritsParentRestrictions(t *testing.T) {
	parent := session.Record{
		ThreadID: "t-parent", PermissionMode: "acceptEdits",
		DisallowedTools: []string{"Bash", "WebFetch"},
		AddDirs:         []string{"/ref"},
	}
	// Asking for nothing inherits everything, and is no escalation.
	dis, dirs, relax := inheritRestrictions(parent,
		workerLaunchRequest{ParentThreadID: "t-parent"})
	if len(dis) != 2 || len(dirs) != 1 {
		t.Fatalf("worker did not inherit the parent's restrictions: %v %v", dis, dirs)
	}
	if len(relax) != 0 {
		t.Fatalf("inheritance reported as a relaxation: %v", relax)
	}
	// Dropping one of the parent's denies is a relaxation, and it is named.
	_, _, relax = inheritRestrictions(parent, workerLaunchRequest{
		ParentThreadID: "t-parent", DisallowedTools: []string{"Bash"}})
	if len(relax) != 1 || !strings.Contains(relax[0], "WebFetch") ||
		!strings.Contains(relax[0], "Un-bans") {
		t.Fatalf("a dropped deny was not reported: %v", relax)
	}
	// So is reaching a directory the parent cannot.
	_, _, relax = inheritRestrictions(parent, workerLaunchRequest{
		ParentThreadID: "t-parent", AddDirs: []string{"/ref", "/etc"}})
	if len(relax) != 1 || !strings.Contains(relax[0], "/etc") {
		t.Fatalf("an added root was not reported: %v", relax)
	}
	// A STRICTER worker is not an escalation and needs no approval.
	_, _, relax = inheritRestrictions(parent, workerLaunchRequest{
		ParentThreadID: "t-parent", DisallowedTools: []string{"Bash", "WebFetch", "Write"},
		AddDirs: []string{}})
	if len(relax) != 0 {
		t.Fatalf("a stricter worker was treated as an escalation: %v", relax)
	}
}

// TestEffectiveIsolationIsWhatIsGated is the isolation half of F1, second pass:
// the gate used to key on the literal string "workspace", while worktree.Create
// silently degrades "auto" (and the unspecified default) to the human's REAL
// checkout in any project with no commit to branch from. A worker landed in the
// user's files and the gate stayed quiet, because the request said "auto".
func TestEffectiveIsolationIsWhatIsGated(t *testing.T) {
	// The worktree layer's own answer, which is what the gate now asks for.
	fresh, committed := repoNoCommits(t), repoWithCommit(t)
	for _, tc := range []struct{ repo, mode, want string }{
		{committed, "", worktree.ModeIsolated},
		{committed, worktree.ModeAuto, worktree.ModeIsolated},
		{committed, worktree.ModeWorkspace, worktree.ModeWorkspace},
		// The whole finding: "auto" in a repo with no commits IS the workspace.
		{fresh, "", worktree.ModeWorkspace},
		{fresh, worktree.ModeAuto, worktree.ModeWorkspace},
		// Isolation that is REQUIRED fails rather than degrading, so it is not
		// reported as a workspace launch.
		{fresh, worktree.ModeIsolated, worktree.ModeIsolated},
		// Not a git repository at all — Create runs directly in it.
		{t.TempDir(), worktree.ModeAuto, worktree.ModeWorkspace},
		{"/nonexistent-agentkate-test", worktree.ModeAuto, worktree.ModeWorkspace},
		// An empty root is never answered by git in the caller's own cwd.
		{"", worktree.ModeAuto, worktree.ModeWorkspace},
		{"", "", worktree.ModeWorkspace},
	} {
		if got := worktree.EffectiveIsolation(tc.repo, tc.mode); got != tc.want {
			t.Errorf("EffectiveIsolation(%s, %q) = %q, want %q",
				tc.repo, tc.mode, got, tc.want)
		}
	}

	// ...and the gate asks the human for a fresh-repo "auto" exactly as it does
	// for an explicit "workspace", at the launcher's own permission mode.
	caps := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "fake"}
	for name, tc := range map[string]struct {
		project   string
		isolation string
		wantAsk   bool
	}{
		"auto in a fresh repo is the human's files": {fresh, "", true},
		"auto in a real repo is a worktree":         {committed, "", false},
		"explicit workspace always asks":            {committed, "workspace", true},
		"explicit auto in a fresh repo asks":        {fresh, "auto", true},
	} {
		sessions := testSessions(t)
		parent := session.Record{
			ThreadID: "t-parent", Project: tc.project, Backend: "fake",
			PermissionMode: "acceptEdits", Created: time.Now(),
		}
		if err := sessions.Put(parent); err != nil {
			t.Fatalf("Put: %v", err)
		}
		d, allow, asks := authTestDeps(t, sessions, &fakeHarness{})
		allow.Store(false) // a refusal, so an ask is also a launch that did not happen
		err := d.gate(parent, caps, workerLaunchRequest{
			ParentThreadID: "t-parent", Backend: "fake", Prompt: "go",
			Isolation: tc.isolation})
		if gotAsk := asks.Load() > 0; gotAsk != tc.wantAsk {
			t.Errorf("%s: asked = %v, want %v", name, gotAsk, tc.wantAsk)
		}
		if tc.wantAsk && err == nil {
			t.Errorf("%s: a refused launch was allowed", name)
		}
		if !tc.wantAsk && err != nil {
			t.Errorf("%s: an unescalated launch was gated: %v", name, err)
		}
	}
}

// TestEscalationPromptNamesTheEffectiveIsolation: the human must read what will
// actually happen, not the word the agent used. "auto" that means the real
// checkout has to say so, or the dialog is worse than no dialog.
func TestEscalationPromptNamesTheEffectiveIsolation(t *testing.T) {
	parent := session.Record{ThreadID: "t-parent", PermissionMode: "acceptEdits"}
	req := workerLaunchRequest{ParentThreadID: "t-parent", Prompt: "go", Isolation: "auto"}
	got := escalationSummary(parent, req, nil, false, worktree.ModeWorkspace)
	if !strings.Contains(got, "YOUR real files") {
		t.Errorf("an effective-workspace launch did not say so: %s", got)
	}
	if got := escalationSummary(parent, req, nil, false, worktree.ModeIsolated); !strings.Contains(
		got, "its own worktree") {
		t.Errorf("an isolated launch was described as the workspace: %s", got)
	}
}

// TestRestrictionEntriesAreNormalisedOnce is the second half of the deny-list
// bug: missingFrom compared TRIMMED entries while the caller's UNTRIMMED slice
// went to the launch, so " Bash" passed the escalation check as "Bash" and then
// reached the CLI as a different string that banned nothing.
func TestRestrictionEntriesAreNormalisedOnce(t *testing.T) {
	parent := session.Record{
		ThreadID: "t-parent", PermissionMode: "acceptEdits",
		DisallowedTools: []string{"Bash", "WebFetch"},
		AddDirs:         []string{"/ref"},
	}
	dis, dirs, relax := inheritRestrictions(parent, workerLaunchRequest{
		ParentThreadID:  "t-parent",
		DisallowedTools: []string{" Bash", "WebFetch\t", "", "   "},
		AddDirs:         []string{" /ref "},
	})
	// The check saw no relaxation...
	if len(relax) != 0 {
		t.Fatalf("padded entries read as a relaxation: %v", relax)
	}
	// ...and the LAUNCH gets exactly the values the check was made on.
	if len(dis) != 2 || dis[0] != "Bash" || dis[1] != "WebFetch" {
		t.Errorf("the launch got un-normalised denies: %q", dis)
	}
	if len(dirs) != 1 || dirs[0] != "/ref" {
		t.Errorf("the launch got un-normalised roots: %q", dirs)
	}
	// The distinction normalisation must not erase: nil (inherit) is not the
	// same as an explicitly empty list (shed everything, and be asked about it).
	dis, _, relax = inheritRestrictions(parent, workerLaunchRequest{
		ParentThreadID: "t-parent", DisallowedTools: []string{"  "}})
	if len(relax) != 1 || !strings.Contains(relax[0], "Bash") {
		t.Fatalf("a list of nothing but blanks was treated as inheritance: %v", relax)
	}
	if dis == nil || len(dis) != 0 {
		t.Errorf("an explicitly empty deny-list became %v", dis)
	}
}

// TestRestrictionRelaxationAsksTheHuman drives the same thing through the gate:
// the launcher's mode and isolation are unchanged, so the ONLY reason to stop
// is the restriction it wants to shed.
func TestRestrictionRelaxationAsksTheHuman(t *testing.T) {
	sessions := testSessions(t)
	parent := session.Record{
		ThreadID: "t-parent", Project: repoWithCommit(t), Backend: "fake",
		PermissionMode: "acceptEdits", DisallowedTools: []string{"Bash"},
		Created: time.Now(),
	}
	if err := sessions.Put(parent); err != nil {
		t.Fatalf("Put: %v", err)
	}
	caps := harness.HarnessDescriptor{ContractVersion: harness.ContractVersion, ID: "fake"}
	d, allow, asks := authTestDeps(t, sessions, &fakeHarness{})

	allow.Store(false)
	err := d.gate(parent, caps, workerLaunchRequest{
		ParentThreadID: "t-parent", Backend: "fake", Prompt: "go",
		DisallowedTools: []string{}})
	if err == nil {
		t.Fatal("a worker shedding the launcher's tool ban was launched unasked")
	}
	if asks.Load() != 1 {
		t.Fatalf("asks = %d, want exactly one", asks.Load())
	}
	// The same launch WITHOUT the relaxation sails through untouched.
	if err := d.gate(parent, caps, workerLaunchRequest{
		ParentThreadID: "t-parent", Backend: "fake", Prompt: "go"}); err != nil {
		t.Fatalf("an unescalated launch was gated: %v", err)
	}
	if asks.Load() != 1 {
		t.Fatalf("an inheriting launch asked the human (%d asks)", asks.Load())
	}
}

// TestLaunchWorkerBindsTheCallerToItsParent: the gate measures the requested
// authority against the PARENT thread's — which measures nothing if a caller
// can name any parent it likes. A connection may launch only from the thread it
// is bound to, the same trust-on-first-use binding bridge.identify makes.
func TestLaunchWorkerBindsTheCallerToItsParent(t *testing.T) {
	sessions := testSessions(t)
	for _, id := range []string{"t-mine", "t-theirs"} {
		if err := sessions.Put(session.Record{
			ThreadID: id, Project: t.TempDir(), Backend: "fake",
			PermissionMode: "acceptEdits", Created: time.Now(),
		}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	client, secrets := orchTestCore(t, sessions, agent.NewTurnTracker())

	// Before any identity, the caller may not launch from ANY thread: the bridge
	// role is no longer taken by asking for it (audit F13).
	if err := client.Call("agent.launchWorker", map[string]any{
		"parentThreadId": "t-mine", "backend": "fake", "prompt": "work"}, nil); err == nil {
		t.Fatal("an unidentified connection launched a worker")
	} else if !strings.Contains(err.Error(), "has not identified") {
		t.Errorf("refused for the wrong reason: %v", err)
	}

	// With the secret its bridge was launched with, this connection is t-mine.
	asBridge(t, secrets, client, "t-mine")
	if err := client.Call("agent.launchWorker", map[string]any{
		"parentThreadId": "t-mine", "backend": "fake", "prompt": "work"}, nil); err != nil {
		t.Fatalf("own-thread launch: %v", err)
	}
	// ...so borrowing the OTHER thread's authority is refused outright, and the
	// refusal names the reason.
	err := client.Call("agent.launchWorker", map[string]any{
		"parentThreadId": "t-theirs", "backend": "fake", "prompt": "work"}, nil)
	if err == nil {
		t.Fatal("a caller launched a worker from a thread it does not own")
	}
	if !strings.Contains(err.Error(), "different thread") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	if _, ok := sessions.Get("t-theirs"); !ok {
		t.Fatal("the borrowed parent vanished")
	}
	// Nothing was created for the refused launch: one worker exists, and it is
	// the legitimate one.
	workers := 0
	for _, rec := range sessions.List("") {
		if rec.ParentThreadID != "" {
			workers++
			if rec.ParentThreadID != "t-mine" {
				t.Errorf("a worker was created under %q", rec.ParentThreadID)
			}
		}
	}
	if workers != 1 {
		t.Errorf("worker count = %d, want exactly the approved one", workers)
	}
}

// TestWorkerStartSpecCarriesInheritedRestrictions closes the loop end to end:
// the fix is only real if the values reach the harness's StartSpec, which is
// what the CLI is actually launched with.
func TestWorkerStartSpecCarriesInheritedRestrictions(t *testing.T) {
	sessions := testSessions(t)
	if err := sessions.Put(session.Record{
		ThreadID: "t-parent", Project: t.TempDir(), Backend: "fake",
		PermissionMode: "acceptEdits", Created: time.Now(),
		DisallowedTools: []string{"Bash"}, AddDirs: []string{"/ref"},
		StrictMCPConfig: true, MaxBudgetUSD: 4,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fake := &fakeHarness{}
	client, secrets := orchTestCore(t, sessions, agent.NewTurnTracker(), fake)
	asBridge(t, secrets, client, "t-parent")

	if err := client.Call("agent.launchWorker", map[string]any{
		"parentThreadId": "t-parent", "backend": "fake", "prompt": "work"}, nil); err != nil {
		t.Fatalf("launchWorker: %v", err)
	}
	spec := fake.spec()
	if len(spec.DisallowedTools) != 1 || spec.DisallowedTools[0] != "Bash" {
		t.Errorf("worker escaped the launcher's deny-list: %v", spec.DisallowedTools)
	}
	if len(spec.AddDirs) != 1 || spec.AddDirs[0] != "/ref" {
		t.Errorf("worker did not inherit the launcher's roots: %v", spec.AddDirs)
	}
	if !spec.StrictMCPConfig || spec.MaxBudgetUSD != 4 {
		t.Errorf("worker shed the launcher's MCP isolation or spend ceiling: %+v", spec)
	}
	// The persisted record replays the same restrictions on a later resume.
	for _, rec := range sessions.List("") {
		if rec.ParentThreadID == "" {
			continue
		}
		if len(rec.DisallowedTools) != 1 || len(rec.AddDirs) != 1 {
			t.Errorf("worker record lost the restrictions: %+v", rec)
		}
	}
}
