package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agentkate/internal/agent"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
)

// bridgeCallSites extracts every RPC method the MCP bridges call, straight from
// their source — the one list that cannot drift.
func bridgeCallSites(t *testing.T) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`client\.Call(?:Timeout)?\(\s*"([^"]+)"`)
	out := map[string]string{}
	for _, file := range []string{"mcp.go", "mcp_cowork.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = file
		}
	}
	if len(out) < 20 {
		t.Fatalf("only %d bridge call sites found — the scan regex is stale", len(out))
	}
	return out
}

// TestMCPToolMapCoversBridgeCallSites keeps the activity feed's method→tool map
// complete in BOTH directions: no bridge call reports under a raw RPC name, and
// no map entry names a method the bridge cannot make.
func TestMCPToolMapCoversBridgeCallSites(t *testing.T) {
	sites := bridgeCallSites(t)
	for method, file := range sites {
		if mcpQuietMethods[method] {
			continue // identity handshake, deliberately not tool traffic
		}
		if _, ok := mcpToolNames[method]; !ok {
			t.Errorf("%s calls %q but mcpToolNames has no tool for it", file, method)
		}
	}
	for method := range mcpToolNames {
		if _, ok := sites[method]; !ok {
			t.Errorf("mcpToolNames maps %q, which no bridge calls any more", method)
		}
	}

	// Every mapped name must be a tool the agent can actually see advertised —
	// a typo here would render as a tool that does not exist.
	advertised := map[string]bool{}
	for _, defs := range [][]map[string]any{toolDefs(), coworkToolDefs()} {
		for _, def := range defs {
			advertised[def["name"].(string)] = true
		}
	}
	for method, tool := range mcpToolNames {
		if !advertised[tool] {
			t.Errorf("%s maps to %q, which is not an advertised tool", method, tool)
		}
	}
}

// TestMCPArgsSummary pins the per-tool digests, the cap, and — the part that
// matters most — what the feed must never carry: prompt bodies, typed text, or
// a gated tool's input.
func TestMCPArgsSummary(t *testing.T) {
	for name, tc := range map[string]struct {
		tool, params, want string
	}{
		"note is one line": {"post_note",
			`{"author":"t-a","text":"first line\nsecond line"}`, "first line"},
		"claim shows the path": {"claim_file",
			`{"path":"src/main.go","owner":"t-a"}`, "src/main.go"},
		"review shows the summary": {"request_review",
			`{"thread":"t-a","summary":"rewired the relay"}`, "rewired the relay"},
		"launch shows engine and title": {"launch_agent",
			`{"parentThreadId":"t-a","backend":"kimi","model":"kimi-code/k3",` +
				`"title":"pong worker","prompt":"the whole secret briefing"}`,
			"kimi/kimi-code/k3: pong worker"},
		"launch without a backend names the caller's engine": {"launch_agent",
			`{"parentThreadId":"t-a","prompt":"go"}`, "(caller's engine)"},
		"send shows target and first line": {"send_agent",
			`{"threadId":"t-w","text":"do this\nand that","fromThreadId":"t-a"}`,
			"t-w: do this"},
		"wait shows the target": {"wait_agent",
			`{"threadId":"t-w","timeoutSec":30}`, "t-w"},
		"close shows the target": {"close_agent",
			`{"threadId":"t-w","fromThreadId":"t-a"}`, "t-w"},
		"discard shows the target": {"discard_agent",
			`{"threadId":"t-w","fromThreadId":"t-a"}`, "t-w"},
		"permission names the gated tool only": {"request_permission",
			`{"threadId":"t-a","toolName":"Bash","input":{"command":"deploy --token=hunter2"}}`,
			"Bash"},
		"set_text names the element only": {"desktop_set_text",
			`{"threadId":"t-a","elementId":"el-7","text":"hunter2"}`, "el-7"},
		"click shows the point": {"desktop_click",
			`{"threadId":"t-a","x":100,"y":250}`, "100,250"},
		"scroll shows the delta": {"desktop_scroll",
			`{"threadId":"t-a","dx":0,"dy":-3}`, "+0,-3"},
		"drag shows both ends": {"desktop_drag",
			`{"threadId":"t-a","fromX":1,"fromY":2,"toX":3,"toY":4}`, "1,2 → 3,4"},
		"injection shows the event count": {"desktop_inject_input",
			`{"threadId":"t-a","events":[{"type":"key","key":"a"},{"type":"key","key":"b"}]}`,
			"2 event(s)"},
		"parameterless tools have no digest": {"read_notes", `{}`, ""},
		"unmapped methods have no digest":    {"some.new.rpc", `{"secret":"x"}`, ""},
		"missing params never panic":         {"post_note", ``, ""},
	} {
		if got := mcpArgsSummary(tc.tool, json.RawMessage(tc.params)); got != tc.want {
			t.Errorf("%s: mcpArgsSummary(%s) = %q, want %q", name, tc.tool, got, tc.want)
		}
	}

	// The payloads that must never ride along: a worker's briefing, a gated
	// tool's input, text typed into a field, a message's later lines.
	for _, tc := range []struct{ tool, params, forbidden string }{
		{"launch_agent",
			`{"backend":"kimi","title":"t","prompt":"the whole secret briefing"}`,
			"secret briefing"},
		{"request_permission",
			`{"toolName":"Bash","input":{"command":"deploy --token=hunter2"}}`, "hunter2"},
		{"desktop_set_text", `{"elementId":"el-7","text":"hunter2"}`, "hunter2"},
		{"send_agent", `{"threadId":"t-w","text":"do this\nand the secret part"}`,
			"secret part"},
	} {
		if got := mcpArgsSummary(tc.tool, json.RawMessage(tc.params)); strings.Contains(got, tc.forbidden) {
			t.Errorf("%s leaked %q: %q", tc.tool, tc.forbidden, got)
		}
	}
}

// TestMCPArgsSummaryCaps proves a long argument cannot flood the feed and that
// the cut never splits a multi-byte rune.
func TestMCPArgsSummaryCaps(t *testing.T) {
	long, err := json.Marshal(map[string]string{"text": strings.Repeat("ünïcödé ", 200)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := mcpArgsSummary("post_note", long)
	if len(got) > mcpSummaryCap+4 { // + the ellipsis rune
		t.Errorf("summary not capped: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("capped summary should be marked elided: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("cap split a rune: %q", got)
	}
}

// rawProbe is a raw socket connection that records everything the core sends
// it, split into notifications and replies. Raw rather than an ipc.Client
// because the client discards notifications — and whether a notification
// reaches a given connection is exactly what these tests are about.
type rawProbe struct {
	conn    net.Conn
	acts    chan map[string]any // mcp.activity payloads
	notes   chan string         // every OTHER notification's method
	replies chan ipc.Frame
}

func newRawProbe(t *testing.T, sock string) *rawProbe {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p := &rawProbe{
		conn:    conn,
		acts:    make(chan map[string]any, 16),
		notes:   make(chan string, 16),
		replies: make(chan ipc.Frame, 16),
	}
	go func() {
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var f ipc.Frame
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			switch {
			case f.Method == "mcp.activity":
				var m map[string]any
				_ = json.Unmarshal(f.Params, &m)
				p.acts <- m
			case f.Method != "":
				p.notes <- f.Method
			default:
				p.replies <- f
			}
		}
	}()
	return p
}

// call sends a request and returns its reply frame.
func (p *rawProbe) call(t *testing.T, id int, method, params string) ipc.Frame {
	t.Helper()
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`,
		id, method, params)
	if _, err := p.conn.Write([]byte(frame + "\n")); err != nil {
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

// asUI identifies this connection as the UI, so it receives UI-only feeds.
func (p *rawProbe) asUI(t *testing.T) *rawProbe {
	t.Helper()
	p.call(t, 1, "handshake", "{}")
	return p
}

// asBridge identifies this connection as thread's agent bridge.
func (p *rawProbe) asBridge(t *testing.T, threadID string) *rawProbe {
	t.Helper()
	if f := p.call(t, 1, "bridge.identify",
		fmt.Sprintf(`{"threadId":%q}`, threadID)); f.Error != nil {
		t.Fatalf("bridge.identify: %v", f.Error)
	}
	return p
}

// next returns the next activity, or fails if none arrives.
func (p *rawProbe) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-p.acts:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no mcp.activity arrived")
		return nil
	}
}

// quiet fails if any activity arrives in the settle window.
func (p *rawProbe) quiet(t *testing.T, why string) {
	t.Helper()
	select {
	case m := <-p.acts:
		t.Fatalf("%s: unexpected mcp.activity %v", why, m)
	case <-time.After(250 * time.Millisecond):
	}
}

// TestBridgeDispatchEmitsActivity drives the feed end to end over a real IPC
// server: a bridge's calls produce exactly one mcp.activity each (with the
// bound thread, the tool name, the digest and the outcome), its identity
// handshake produces none, and identical traffic from the UI produces none.
func TestBridgeDispatchEmitsActivity(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "activity.sock")
	srv := ipc.NewServer(sock, log)
	srv.Handle("handshake", func(ctx context.Context, _ json.RawMessage) (any, error) {
		srv.MarkUI(ctx)
		return map[string]any{"name": "akcore"}, nil
	})
	srv.Handle("coop.postNote", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"id": 1}, nil
	})
	srv.Handle("coop.claimFile", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, ipc.Errorf(ipc.CodeInvalidParams, "already claimed by t-other")
	})
	srv.Handle("coop.readNotes", func(_ context.Context, _ json.RawMessage) (any, error) {
		panic("the board exploded")
	})
	registerMCPActivity(handlerDeps{srv: srv, log: log})
	serveIPC(t, srv, sock)

	ui := newRawProbe(t, sock).asUI(t)

	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("bridge dial: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })

	// The identity handshake is plumbing, not tool traffic.
	if err := bridge.Call("bridge.identify",
		map[string]any{"threadId": "t-agent"}, nil); err != nil {
		t.Fatalf("bridge.identify: %v", err)
	}
	ui.quiet(t, "bridge.identify")

	if err := bridge.Call("coop.postNote",
		map[string]any{"author": "t-agent", "text": "claiming the parser\nthen editing"},
		nil); err != nil {
		t.Fatalf("coop.postNote: %v", err)
	}
	act := ui.next(t)
	if act["threadId"] != "t-agent" || act["tool"] != "post_note" {
		t.Fatalf("activity = %v", act)
	}
	if act["argsSummary"] != "claiming the parser" {
		t.Errorf("argsSummary = %v", act["argsSummary"])
	}
	if ok, _ := act["ok"].(bool); !ok {
		t.Errorf("ok = %v, want true", act["ok"])
	}
	if _, has := act["error"]; has {
		t.Errorf("successful call carried an error: %v", act["error"])
	}
	if _, isNum := act["durationMs"].(float64); !isNum {
		t.Errorf("durationMs = %v (%T)", act["durationMs"], act["durationMs"])
	}
	// Exactly one notification per request.
	ui.quiet(t, "second notification for one request")

	// A failed call is reported as a failure, with the reason.
	if err := bridge.Call("coop.claimFile",
		map[string]any{"path": "src/parser.go", "owner": "t-agent"}, nil); err == nil {
		t.Fatal("coop.claimFile should have failed")
	}
	act = ui.next(t)
	if act["tool"] != "claim_file" || act["argsSummary"] != "src/parser.go" {
		t.Fatalf("activity = %v", act)
	}
	if ok, _ := act["ok"].(bool); ok {
		t.Errorf("failed call reported ok")
	}
	if errText, _ := act["error"].(string); !strings.Contains(errText, "already claimed") {
		t.Errorf("error = %q", errText)
	}

	// A panicking handler still answers and is still reported: without the
	// dispatch recover the goroutine unwinds past both the reply and the feed,
	// so the bridge hangs to its timeout and the most interesting event of all
	// goes unrecorded.
	if err := bridge.Call("coop.readNotes", map[string]any{}, nil); err == nil ||
		!strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panicking handler: err = %v, want a panic error response", err)
	}
	act = ui.next(t)
	if act["tool"] != "read_notes" {
		t.Fatalf("activity = %v", act)
	}
	if ok, _ := act["ok"].(bool); ok {
		t.Errorf("panicking call reported ok")
	}
	if errText, _ := act["error"].(string); !strings.Contains(errText, "panicked") {
		t.Errorf("error = %q, want the panic", errText)
	}

	// The same call from the UI is not MCP traffic and is never broadcast.
	ui.call(t, 2, "coop.postNote", `{"author":"human","text":"from the UI"}`)
	ui.quiet(t, "UI-role traffic")

	// The feed is UI-only in the other direction too: a bridge gets its
	// response and nothing else — never another agent's activity.
	spy := newRawProbe(t, sock).asBridge(t, "t-spy")
	spy.quiet(t, "bridge.identify on the spy")
	if f := spy.call(t, 2, "coop.postNote", `{"author":"t-spy","text":"mine"}`); f.Error != nil {
		t.Fatalf("spy postNote: %v", f.Error)
	}
	spy.quiet(t, "bridge received its own activity")
	select {
	case m := <-spy.notes:
		t.Errorf("bridge received notification %q", m)
	case <-time.After(250 * time.Millisecond):
	}
	// ... and the UI did see that bridge's call.
	if act = ui.next(t); act["threadId"] != "t-spy" {
		t.Errorf("UI missed the spy bridge's activity: %v", act)
	}

	// A UI connection may not claim a bridge identity.
	if f := ui.call(t, 3, "bridge.identify", `{"threadId":"t-agent"}`); f.Error == nil {
		t.Error("bridge.identify from a UI connection was accepted")
	}
	// An empty thread id is refused outright.
	if f := newRawProbe(t, sock).call(t, 1, "bridge.identify", `{}`); f.Error == nil {
		t.Error("bridge.identify accepted an empty threadId")
	}
}

// TestBridgeCannotBecomeUI pins the role's one-way-ness. A bridge that calls
// `handshake` must stay a bridge: otherwise it would pass RequireUI (answering
// its own grant prompts) and start receiving every other agent's mcp.activity.
func TestBridgeCannotBecomeUI(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock := filepath.Join(t.TempDir(), "roles.sock")
	srv := ipc.NewServer(sock, log)
	roles := make(chan string, 4)
	srv.Handle("handshake", func(ctx context.Context, _ json.RawMessage) (any, error) {
		srv.MarkUI(ctx)
		ref := ipc.ConnFromContext(ctx)
		roles <- ref.Role()
		return map[string]any{"name": "akcore", "isUI": srv.RequireUI(ctx)}, nil
	})
	srv.Handle("coop.postNote", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"id": 1}, nil
	})
	registerMCPActivity(handlerDeps{srv: srv, log: log})
	serveIPC(t, srv, sock)

	ui := newRawProbe(t, sock).asUI(t)
	if role := <-roles; role != "ui" {
		t.Fatalf("real UI role = %q", role)
	}

	bridge := newRawProbe(t, sock).asBridge(t, "t-agent")
	f := bridge.call(t, 2, "handshake", "{}")
	if role := <-roles; role != "bridge" {
		t.Errorf("role after a bridge's handshake = %q, want it to stay bridge", role)
	}
	var res struct {
		IsUI bool `json:"isUI"`
	}
	if f.Error == nil {
		_ = json.Unmarshal(f.Result, &res)
	}
	if res.IsUI {
		t.Error("a bridge passed RequireUI after calling handshake")
	}

	// It also does not join the UI-only feed: its own call is broadcast to the
	// real UI and to nobody else.
	if f := bridge.call(t, 3, "coop.postNote", `{"author":"t-agent","text":"hi"}`); f.Error != nil {
		t.Fatalf("postNote: %v", f.Error)
	}
	bridge.quiet(t, "bridge that tried to become the UI")
	if act := ui.next(t); act["threadId"] != "t-agent" || act["tool"] != "post_note" {
		t.Errorf("UI activity = %v", act)
	}
}

// TestBridgeErrorsCarryNoSecrets guards the other half of the redaction
// boundary: an mcp.activity's `error` is the handler's error string verbatim,
// so no handler a bridge can reach may echo a secret-bearing argument into it.
func TestBridgeErrorsCarryNoSecrets(t *testing.T) {
	const secret = "S3CRET-PAYLOAD-MARKER"
	sessions := testSessions(t)
	if err := sessions.Put(session.Record{
		ThreadID: "t-parent", Project: t.TempDir(), Created: time.Now(),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	client := orchTestCore(t, sessions, agent.NewTurnTracker())

	// Every fixture puts the marker in a secret-bearing field and forces the
	// handler down an error path.
	for name, call := range map[string]struct {
		method string
		params map[string]any
	}{
		"launch: unknown parent": {"agent.launchWorker", map[string]any{
			"parentThreadId": "t-ghost", "prompt": secret, "title": secret}},
		"launch: unknown backend": {"agent.launchWorker", map[string]any{
			"parentThreadId": "t-parent", "prompt": secret, "backend": "codex"}},
		"launch: bad isolation": {"agent.launchWorker", map[string]any{
			"parentThreadId": "t-parent", "prompt": secret, "isolation": secret}},
		"launch: empty prompt": {"agent.launchWorker", map[string]any{
			"parentThreadId": "t-parent", "prompt": "", "title": secret}},
		"wait: unknown thread": {"agent.wait", map[string]any{
			"threadId": "t-ghost", "text": secret}},
	} {
		err := client.Call(call.method, call.params, nil)
		if err == nil {
			t.Errorf("%s: expected an error path, got success", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: handler error echoed a secret-bearing argument: %q",
				name, err.Error())
		}
	}
}
