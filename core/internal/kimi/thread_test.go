package kimi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKimiScript writes a stand-in `kimi` binary that speaks just enough ACP
// for the thread tests (pattern: the fake claude script in
// cmd/akcore/compaction_shutdown_test.go). It implements initialize,
// session/new + session/resume, session/set_config_option, session/prompt,
// session/cancel and the session/request_permission reverse request:
//
//   - any other prompt streams a thought delta, a plan update, two text
//     deltas, a tool_call (rawInput inline), its completed tool_call_update,
//     then answers stopReason "end_turn";
//   - a prompt containing "wait-cancel" streams one delta and holds the
//     response until session/cancel arrives, then answers "cancelled";
//   - a prompt containing "perm" asks permission (reverse request, numeric id
//     0 — kimi's style) and echoes the selected optionId in its reply text;
//   - a prompt containing "ask-question" asks an AskUserQuestion the same way,
//     with the q0_opt_<i> / q0_skip option set kimi mints for a question, and
//     echoes the selected optionId too;
//   - session/new reports a configOptions set (model enumeration), the shape
//     kimi 0.30 returns.
func fakeKimiScript(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	path := filepath.Join(t.TempDir(), "fake-kimi")
	script := `#!/usr/bin/env python3
import json, sys, os, signal

sid = "session_fake-0001"
pending_prompt = None  # prompt id held until session/cancel
perm_prompt = None     # prompt id held until the permission response

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

def upd(u):
    send({"jsonrpc": "2.0", "method": "session/update",
          "params": {"sessionId": sid, "update": u}})

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    f = json.loads(line)
    method = f.get("method")
    fid = f.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": fid, "result": {
            "protocolVersion": 1,
            "agentCapabilities": {"promptCapabilities": {"image": True},
                                  "loadSession": True},
            "authMethods": [{"id": "oauth", "name": "Sign in with Moonshot",
                             "description": "opens a browser"}],
            "agentInfo": {"name": "fake-kimi", "version": "0"}}})
    elif method == "session/load":
        # ACP replay: the history streams as notifications DURING the call.
        upd({"sessionUpdate": "user_message_chunk",
             "content": {"type": "text", "text": "what does this repo do?"}})
        upd({"sessionUpdate": "agent_message_chunk",
             "content": {"type": "text", "text": "It is a test fixture."}})
        send({"jsonrpc": "2.0", "id": fid,
              "result": {"sessionId": f["params"]["sessionId"], "configOptions": []}})
    elif method in ("session/new", "session/resume"):
        send({"jsonrpc": "2.0", "id": fid,
              "result": {"sessionId": sid, "configOptions": [
                  {"type": "select", "id": "model", "name": "Model",
                   "category": "model", "currentValue": "kimi-code/k3",
                   "options": [
                       {"value": "kimi-code/k3", "name": "K3"},
                       {"value": "kimi-code/kimi-for-coding", "name": "K2.7 Coding"}]}]}})
    elif method == "session/set_config_option":
        if f["params"].get("value") == "bad":
            send({"jsonrpc": "2.0", "id": fid,
                  "error": {"code": -32602, "message": "unknown value: bad"}})
        else:
            # The authoritative post-change config, as kimi answers it: the
            # "thinking" option is deliberately downgraded to show the caller
            # must believe the response rather than its own request.
            applied = f["params"]["value"]
            if f["params"]["configId"] == "thinking" and applied == "max":
                applied = "high"
            send({"jsonrpc": "2.0", "id": fid, "result": {"configOptions": [
                {"id": f["params"]["configId"], "currentValue": applied}]}})
    elif method == "session/list":
        # One session left behind by a DiscoverOptions/ListSessions probe (its
        # cwd is this very throwaway probe dir), one real session — ListSessions
        # must drop the probe one by its temp-dir prefix.
        send({"jsonrpc": "2.0", "id": fid, "result": {"sessions": [
            {"sessionId": "sess-probe", "cwd": os.getcwd(),
             "title": "probe leftover", "updatedAt": "2026-07-30T10:00:00Z"},
            {"sessionId": "sess-real", "cwd": "/home/fake/project",
             "title": "real work", "updatedAt": "2026-07-30T11:00:00Z"}]}})
    elif method == "session/cancel":
        if pending_prompt is not None:
            send({"jsonrpc": "2.0", "id": pending_prompt,
                  "result": {"stopReason": "cancelled"}})
            pending_prompt = None
    elif method == "session/prompt":
        text = "".join(b.get("text", "") for b in f["params"]["prompt"]
                       if b.get("type") == "text")
        if text.strip() == "/usage":
            upd({"sessionUpdate": "agent_message_chunk",
                 "content": {"type": "text",
                             "text": "Context: 4,096 / 128,000 tokens (3%)"}})
            send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
        elif text.strip() == "/compact":
            upd({"sessionUpdate": "agent_message_chunk",
                 "content": {"type": "text", "text": "Compacted 12 messages."}})
            send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
        elif "flip-mode" in text:
            # kimi's own ExitPlanMode transition: it changes mode and only
            # tells us afterwards.
            upd({"sessionUpdate": "config_option_update",
                 "configId": "mode", "value": "default"})
            send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
        elif "wait-cancel" in text:
            upd({"sessionUpdate": "agent_message_chunk",
                 "content": {"type": "text", "text": "working"}})
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc-wait",
                 "title": "Bash", "kind": "execute", "status": "in_progress",
                 "rawInput": {"command": "sleep 600"}})
            pending_prompt = fid
        elif "hang-turn" in text:
            # A turn that never acks the cancel and ignores SIGINT, forcing the
            # backstop to escalate all the way to SIGKILL.
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc-hang",
                 "title": "Bash", "kind": "execute", "status": "in_progress",
                 "rawInput": {"command": "sleep 600"}})
            # No response, no pending_prompt: session/cancel is ignored too.
        elif "die-mid-turn" in text:
            # The process exits mid-turn without ever answering the prompt — the
            # supervisor must NOT synthesise a result for a turn that isn't coming.
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc-die",
                 "title": "Bash", "kind": "execute", "status": "in_progress",
                 "rawInput": {"command": "boom"}})
            sys.stdout.flush()
            os._exit(1)
        elif "ask-question" in text:
            # kimi's AskUserQuestion: no session/request_question exists, so it
            # arrives as a permission request whose OPTIONS are the answers.
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc-ask",
                 "title": "AskUserQuestion", "kind": "other", "status": "pending",
                 "rawInput": {"questions": [{"question": "Tabs or spaces?"}]}})
            send({"jsonrpc": "2.0", "id": 0, "method": "session/request_permission",
                  "params": {"sessionId": sid,
                             "toolCall": {"toolCallId": "tc-ask",
                                          "title": "AskUserQuestion", "kind": "other"},
                             "options": [
                                 {"optionId": "q0_opt_0", "name": "Tabs",
                                  "kind": "allow_once"},
                                 {"optionId": "q0_opt_1", "name": "Spaces",
                                  "kind": "allow_once"},
                                 {"optionId": "q0_skip", "name": "Skip",
                                  "kind": "reject_once"}]}})
            perm_prompt = fid
        elif "perm" in text:
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc-perm",
                 "title": "Bash", "kind": "execute", "status": "pending",
                 "rawInput": {"command": "rm -rf /"}})
            send({"jsonrpc": "2.0", "id": 0, "method": "session/request_permission",
                  "params": {"sessionId": sid,
                             "toolCall": {"toolCallId": "tc-perm", "title": "Bash"},
                             "options": [
                                 {"optionId": "approve_once", "kind": "allow_once"},
                                 {"optionId": "reject", "kind": "reject_once"}]}})
            perm_prompt = fid
        else:
            upd({"sessionUpdate": "agent_thought_chunk",
                 "content": {"type": "text", "text": "pondering"}})
            upd({"sessionUpdate": "plan", "entries": [
                {"content": "list files", "priority": "high", "status": "in_progress"}]})
            upd({"sessionUpdate": "agent_message_chunk",
                 "content": {"type": "text", "text": "Hello "}})
            upd({"sessionUpdate": "agent_message_chunk",
                 "content": {"type": "text", "text": "world"}})
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc1", "title": "Bash",
                 "kind": "execute", "status": "pending",
                 "rawInput": {"command": "ls"}})
            upd({"sessionUpdate": "tool_call_update", "toolCallId": "tc1",
                 "status": "completed", "rawOutput": "file.txt"})
            send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
    elif method is None and perm_prompt is not None:
        # the response to our session/request_permission reverse request
        outcome = (f.get("result") or {}).get("outcome") or {}
        upd({"sessionUpdate": "agent_message_chunk",
             "content": {"type": "text", "text": "perm:" + str(outcome.get("optionId"))}})
        send({"jsonrpc": "2.0", "id": perm_prompt, "result": {"stopReason": "end_turn"}})
        perm_prompt = None
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kimi: %v", err)
	}
	return path
}

// eventCollector gathers every emitted event, flattened, in order.
type eventCollector struct {
	mu     sync.Mutex
	events []json.RawMessage
}

func (c *eventCollector) add(_ string, events []json.RawMessage) {
	c.mu.Lock()
	c.events = append(c.events, events...)
	c.mu.Unlock()
}

func (c *eventCollector) snapshot() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]json.RawMessage(nil), c.events...)
}

// waitFor blocks until an event matching pred has been emitted.
func (c *eventCollector) waitFor(t *testing.T, what string, pred func(ev map[string]any) bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, raw := range c.snapshot() {
			var ev map[string]any
			if json.Unmarshal(raw, &ev) == nil && pred(ev) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	var sb strings.Builder
	for _, raw := range c.snapshot() {
		sb.WriteString("  " + string(raw) + "\n")
	}
	t.Fatalf("timed out waiting for %s; events so far:\n%s", what, sb.String())
}

// indexOf returns the position of the first event matching pred, or -1.
func (c *eventCollector) indexOf(pred func(ev map[string]any) bool) int {
	for i, raw := range c.snapshot() {
		var ev map[string]any
		if json.Unmarshal(raw, &ev) == nil && pred(ev) {
			return i
		}
	}
	return -1
}

func isLifecycle(phase string) func(map[string]any) bool {
	return func(ev map[string]any) bool {
		return ev["type"] == "_lifecycle" && ev["phase"] == phase
	}
}

func isResult(ev map[string]any) bool { return ev["type"] == "result" }

// isType matches any event of the given synthetic type.
func isType(typ string) func(map[string]any) bool {
	return func(ev map[string]any) bool { return ev["type"] == typ }
}

// hasToolUse matches an assistant event carrying a tool_use block — the
// sync point a test can wait on while a turn is mid-flight.
func hasToolUse(ev map[string]any) bool {
	if ev["type"] != "assistant" {
		return false
	}
	msg, _ := ev["message"].(map[string]any)
	content, _ := msg["content"].([]any)
	for _, b := range content {
		blk, _ := b.(map[string]any)
		if blk["type"] == "tool_use" {
			return true
		}
	}
	return false
}

func assistantText(want string) func(map[string]any) bool {
	return func(ev map[string]any) bool {
		if ev["type"] != "assistant" {
			return false
		}
		msg, _ := ev["message"].(map[string]any)
		content, _ := msg["content"].([]any)
		for _, b := range content {
			blk, _ := b.(map[string]any)
			if blk["type"] == "text" && blk["text"] == want {
				return true
			}
		}
		return false
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestKimiThreadTurn drives a full turn through the fake kimi and checks the
// translated event sequence: init (with config options), the thinking card,
// the plan checklist, one assistant text card, the tool card, its result, and
// the turn result.
func TestKimiThreadTurn(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	eventDir := t.TempDir()
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, eventDir)

	th, err := sup.Start(StartOptions{
		ID:      "t-kimi1",
		WorkDir: t.TempDir(),
		Prompt:  "hello",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := th.SessionID(); got != "session_fake-0001" {
		t.Errorf("SessionID = %q, want session_fake-0001", got)
	}

	col.waitFor(t, "turn result", isResult)
	// The completed turn triggers the silent `/usage` probe; wait for its
	// readout so the sequence below is the settled one.
	col.waitFor(t, "usage readout", isType("_context"))
	seq := col.snapshot()
	wantTypes := []string{"system", "assistant", "assistant", "assistant",
		"assistant", "user", "result", "_context"}
	if len(seq) != len(wantTypes) {
		t.Fatalf("emitted %d events, want %d: %s", len(seq), len(wantTypes), seq)
	}
	for i, wt := range wantTypes {
		var ev map[string]any
		_ = json.Unmarshal(seq[i], &ev)
		if ev["type"] != wt {
			t.Errorf("event %d type = %v, want %s (%s)", i, ev["type"], wt, seq[i])
		}
	}
	// The init event carries the handshake's config options (model list).
	var initHead struct {
		ConfigOptions []ConfigOption `json:"configOptions"`
	}
	_ = json.Unmarshal(seq[0], &initHead)
	if len(initHead.ConfigOptions) != 1 || initHead.ConfigOptions[0].ID != "model" ||
		len(initHead.ConfigOptions[0].Options) != 2 {
		t.Errorf("init configOptions = %+v, want the model enumeration", initHead.ConfigOptions)
	}
	// The thought delta arrives as a thinking block, the plan as a TodoWrite
	// tool_use — the shapes the UI's thinking and checklist cards consume.
	var thinkEv struct {
		Message struct {
			Content []struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(seq[1], &thinkEv)
	if len(thinkEv.Message.Content) != 1 || thinkEv.Message.Content[0].Type != "thinking" ||
		thinkEv.Message.Content[0].Thinking != "pondering" {
		t.Errorf("event 1 = %s, want a thinking block \"pondering\"", seq[1])
	}
	var planEv struct {
		Message struct {
			Content []struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				Input struct {
					Todos []map[string]any `json:"todos"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(seq[2], &planEv)
	if len(planEv.Message.Content) != 1 || planEv.Message.Content[0].Name != "TodoWrite" ||
		len(planEv.Message.Content[0].Input.Todos) != 1 {
		t.Errorf("event 2 = %s, want a TodoWrite tool_use with one todo", seq[2])
	}

	// Every translated event lands in the thread's event log — its transcript.
	if err := sup.Stop(th.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))
	if sup.Running(th.ID) {
		t.Error("thread still running after stop + exited lifecycle")
	}
	logged, err := ReadTranscript(eventDir, th.ID)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(logged) < len(wantTypes) {
		t.Fatalf("event log has %d events, want at least %d", len(logged), len(wantTypes))
	}
	var init map[string]any
	_ = json.Unmarshal(logged[0], &init)
	if init["type"] != "system" || init["session_id"] != "session_fake-0001" {
		t.Errorf("first logged event = %s, want the init system event", logged[0])
	}
}

// TestKimiThreadInterrupt covers session/cancel: the turn's prompt response
// comes back stopReason "cancelled", which must surface as a result plus a
// turn_aborted lifecycle event, with the process staying resident.
func TestKimiThreadInterrupt(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi2", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sup.Send(th.ID, "wait-cancel", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	col.waitFor(t, "in-flight tool call", hasToolUse)
	if err := sup.Interrupt(th.ID); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	col.waitFor(t, "turn_aborted lifecycle", isLifecycle("turn_aborted"))
	if !sup.Running(th.ID) {
		t.Fatal("thread died on interrupt; it should stay resident")
	}
	// The session is still hot: a follow-up goes down the same process.
	if err := sup.Send(th.ID, "again", nil); err != nil {
		t.Fatalf("follow-up send: %v", err)
	}
	col.waitFor(t, "follow-up text", assistantText("Hello world"))
	sup.StopAll()
}

// TestKimiPermissionBridge covers the session/request_permission reverse
// request: the supervisor's PermissionFunc is consulted with the mapped tool
// name and parsed input, and an allow maps back onto the allow_once option.
func TestKimiPermissionBridge(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}

	var gotTool string
	var gotInput map[string]any
	perm := func(_, toolName string, input json.RawMessage) (bool, json.RawMessage) {
		gotTool = toolName
		_ = json.Unmarshal(input, &gotInput)
		return true, nil
	}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, perm, t.TempDir())

	if _, err := sup.Start(StartOptions{ID: "t-kimi3", WorkDir: t.TempDir(), Prompt: "please perm"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "perm echo", assistantText("perm:approve_once"))
	if gotTool != "Bash" {
		t.Errorf("permission toolName = %q, want Bash", gotTool)
	}
	if gotInput["command"] != "rm -rf /" {
		t.Errorf("permission input = %v, want command rm -rf /", gotInput)
	}
	sup.StopAll()
}

// TestKimiQuestionAnswersTheChosenOption is the regression pin for the live
// bug: kimi bridges AskUserQuestion over session/request_permission, so
// answering by option KIND picked whichever answer happened to be listed first
// — the user's choice never reached the CLI. The human's pick must be the
// optionId that goes back over the wire.
func TestKimiQuestionAnswersTheChosenOption(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}

	var gotTool string
	var gotInput map[string]any
	perm := func(_, toolName string, input json.RawMessage) (bool, json.RawMessage) {
		gotTool = toolName
		_ = json.Unmarshal(input, &gotInput)
		// The UI answers the SECOND choice — the one an allow_once kind match
		// would never have selected.
		updated, _ := json.Marshal(map[string]any{
			"answers": map[string]any{"Tabs or spaces?": "Spaces"},
		})
		return true, updated
	}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, perm, t.TempDir())

	if _, err := sup.Start(StartOptions{
		ID: "t-kimi-q1", WorkDir: t.TempDir(), Prompt: "please ask-question",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "the chosen option on the wire", assistantText("perm:q0_opt_1"))

	// The prompt the human saw carried the REAL answers, not an allow/deny.
	if gotTool != "AskUserQuestion" {
		t.Errorf("question toolName = %q, want AskUserQuestion", gotTool)
	}
	questions, _ := gotInput["questions"].([]any)
	if len(questions) != 1 {
		t.Fatalf("question input = %v, want one question", gotInput)
	}
	q, _ := questions[0].(map[string]any)
	if q["question"] != "Tabs or spaces?" {
		t.Errorf("question text = %v, want the CLI's own question", q["question"])
	}
	var labels []string
	opts, _ := q["options"].([]any)
	for _, o := range opts {
		om, _ := o.(map[string]any)
		labels = append(labels, om["label"].(string))
	}
	if strings.Join(labels, ",") != "Tabs,Spaces,Skip" {
		t.Errorf("offered options = %v, want the CLI's own answers", labels)
	}
	sup.StopAll()
}

// TestKimiQuestionUnansweredSkips: a question the human dismissed (or answered
// with something no option matches) must go back as the CLI's own skip option
// — never as a guessed answer.
func TestKimiQuestionUnansweredSkips(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}

	perm := func(_, _ string, _ json.RawMessage) (bool, json.RawMessage) {
		return false, nil
	}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, perm, t.TempDir())

	if _, err := sup.Start(StartOptions{
		ID: "t-kimi-q2", WorkDir: t.TempDir(), Prompt: "please ask-question",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "the skip option on the wire", assistantText("perm:q0_skip"))
	sup.StopAll()
}

// TestKimiAbandonedInternalReplyLeavesTheLiveTurnAlone pins the ownership rule
// for a late reply: an internal turn (`/usage`) that was abandoned exactly as
// its reply arrived must not decrement the in-flight count a human send has
// since claimed, nor end that human turn with a result event of its own.
func TestKimiAbandonedInternalReplyLeavesTheLiveTurnAlone(t *testing.T) {
	col := &eventCollector{}
	s := NewSupervisor("", testLogger(), col.add, nil, t.TempDir())

	th := &Thread{ID: "t-race", tr: newTranslator("sess-race", "", nil)}
	it := newInternalTurn("usage", true)
	// Exactly what abandonInternal leaves behind…
	it.abandoned = true
	// …with a human send having already claimed the in-flight slot.
	th.activePrompts = 1

	s.onPromptDone(th, acpFrame{Result: json.RawMessage(`{"stopReason":"end_turn"}`)}, it)

	if th.activePrompts != 1 {
		t.Errorf("activePrompts = %d, want 1 — the human turn's slot was released",
			th.activePrompts)
	}
	if idx := col.indexOf(isResult); idx >= 0 {
		t.Errorf("a spurious turn result was emitted for the human's turn: %s",
			col.snapshot()[idx])
	}
}

// newBareThread builds a Thread with just enough wiring for the state-machine
// tests below: a registered id (so Supervisor.cancelPending can find it), a
// translator, and a client whose writes go nowhere — session/cancel notifies
// are fire-and-forget, and no reply is ever needed.
func newBareThread(s *Supervisor, id string) *Thread {
	t := &Thread{
		ID:        id,
		alive:     true,
		tr:        newTranslator("sess-"+id, "", nil),
		client:    newACPClient(io.Discard, testLogger()),
		sessionID: "sess-" + id,
	}
	s.mu.Lock()
	s.threads[id] = t
	s.mu.Unlock()
	return t
}

// TestKimiAbandonClearsThePendingCancel pins the cancel-flag lifecycle across an
// abandoned internal turn. t.cancelling is what cancelPending consults, and the
// interrupt backstop escalates to SIGINT/SIGKILL while it is true. abandonInternal
// releases the prompt slot, so it must settle the flag with it — otherwise an
// Interrupt landing during an in-flight `/usage` or `/compact` leaves the thread
// believing a cancel is forever pending: the very next turn's backstop fires on a
// perfectly healthy process. The late reply for the abandoned turn must settle it
// too, since it takes the early-return path that skips the release.
func TestKimiAbandonClearsThePendingCancel(t *testing.T) {
	t.Run("abandon", func(t *testing.T) {
		col := &eventCollector{}
		s := NewSupervisor("", testLogger(), col.add, nil, t.TempDir())
		th := newBareThread(s, "t-abandon-cancel")

		it := newInternalTurn("usage", true)
		th.internal = it
		th.activePrompts = 1
		th.cancelling = true // an Interrupt landed while the probe was in flight

		s.abandonInternal(th, it)

		if s.cancelPending(th.ID) {
			t.Error("cancelPending still true after the turn it would cancel was abandoned")
		}
		th.mu.Lock()
		internal, active := th.internal, th.activePrompts
		th.mu.Unlock()
		if internal != nil || active != 0 {
			t.Fatalf("after abandon: internal=%v activePrompts=%d, want nil/0", internal, active)
		}

		// And the thread takes a new prompt: sendInternal gates on exactly the
		// state above, so a clean answer here is the proof it is usable again.
		if _, err := s.sendInternal(th, "usage", usageCommand, true); err != nil {
			t.Fatalf("the thread refused a new prompt after abandon: %v", err)
		}
	})

	t.Run("late reply", func(t *testing.T) {
		col := &eventCollector{}
		s := NewSupervisor("", testLogger(), col.add, nil, t.TempDir())
		th := newBareThread(s, "t-late-cancel")

		it := newInternalTurn("usage", true)
		it.abandoned = true // abandonInternal already released the slot
		th.cancelling = true

		s.onPromptDone(th, acpFrame{Result: json.RawMessage(`{"stopReason":"cancelled"}`)}, it)

		if s.cancelPending(th.ID) {
			t.Error("cancelPending still true after the abandoned turn's own reply arrived")
		}
		if _, err := s.sendInternal(th, "usage", usageCommand, true); err != nil {
			t.Fatalf("the thread refused a new prompt after the late reply: %v", err)
		}
	})

	// The mirror image: a cancel that belongs to a turn STILL in flight (a
	// human send that claimed the slot while the probe was unwinding) must
	// survive the abandoned probe's late reply — clearing it there would disarm
	// the backstop for a turn that genuinely has not acked its cancel.
	t.Run("live turn keeps its cancel", func(t *testing.T) {
		col := &eventCollector{}
		s := NewSupervisor("", testLogger(), col.add, nil, t.TempDir())
		th := newBareThread(s, "t-live-cancel")

		it := newInternalTurn("usage", true)
		it.abandoned = true
		th.activePrompts = 1 // the human's send
		th.cancelling = true // and the Interrupt aimed at it

		s.onPromptDone(th, acpFrame{Result: json.RawMessage(`{"stopReason":"cancelled"}`)}, it)

		if !s.cancelPending(th.ID) {
			t.Error("the live turn's pending cancel was cleared by an unrelated late reply")
		}
	})
}

// TestKimiDropLatchIsLiftedOnlyByItsOwner pins the drop-latch ownership rule.
// The latch is thread-global, but two abandoned turns can be unwinding at once
// (a probe pre-empted by a send, then that send's own compact abandoned). The
// FIRST turn's straggling reply must not lift the SECOND's latch: that turn is
// still streaming, and its chunks would spill into the human's transcript as a
// card nobody asked for.
func TestKimiDropLatchIsLiftedOnlyByItsOwner(t *testing.T) {
	col := &eventCollector{}
	s := NewSupervisor("", testLogger(), col.add, nil, t.TempDir())
	th := newBareThread(s, "t-drop-owner")

	first := newInternalTurn("usage", true)
	second := newInternalTurn("compact", false)

	// Both abandoned, second last — so the latch is second's.
	th.internal, th.activePrompts = first, 1
	s.abandonInternal(th, first)
	th.mu.Lock()
	th.internal, th.activePrompts = second, 1
	th.mu.Unlock()
	s.abandonInternal(th, second)

	// The first turn's reply finally lands. It owns no latch any more.
	s.onPromptDone(th, acpFrame{Result: json.RawMessage(`{"stopReason":"cancelled"}`)}, first)

	// A chunk buffers rather than emitting, so the latch is read off what
	// endTurn flushes: latched, the tail never made it into the buffer at all.
	th.tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "tail of the second turn"},
	}))
	if got := countAssistant(th.tr.endTurn()); got != 0 {
		t.Fatalf("the still-abandoned turn's tail escaped the latch: %d assistant cards", got)
	}

	// The second turn's own reply is what lifts it.
	s.onPromptDone(th, acpFrame{Result: json.RawMessage(`{"stopReason":"cancelled"}`)}, second)
	th.tr.update(upd("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": "the human's answer"},
	}))
	if got := countAssistant(th.tr.endTurn()); got != 1 {
		t.Errorf("assistant cards after the owning turn's reply = %d, want 1 —"+
			" the latch was never lifted by the turn that armed it", got)
	}
}

// countAssistant counts assistant cards in a translated batch.
func countAssistant(events []json.RawMessage) int {
	n := 0
	for _, raw := range events {
		var ev map[string]any
		if json.Unmarshal(raw, &ev) == nil && ev["type"] == "assistant" {
			n++
		}
	}
	return n
}

// TestKimiResume covers session/resume: the thread re-attaches its prior kimi
// session, emits a fresh init event for it, and APPENDS to the existing
// translated-event log — the log is the transcript the UI replays, so a
// resume must never truncate the history before it.
func TestKimiResume(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	eventDir := t.TempDir()
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, eventDir)

	// First life: a fresh thread runs one turn, then stops.
	th, err := sup.Start(StartOptions{ID: "t-kimi4", WorkDir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "turn result", isResult)
	if err := sup.Stop(th.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))
	before, err := ReadTranscript(eventDir, th.ID)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("no transcript after the first life")
	}

	// Second life: resume re-attaches the same kimi session.
	th2, err := sup.Start(StartOptions{
		ID:        "t-kimi4",
		WorkDir:   t.TempDir(),
		SessionID: "session_fake-0001",
		Resume:    true,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := th2.SessionID(); got != "session_fake-0001" {
		t.Errorf("SessionID = %q, want session_fake-0001", got)
	}
	col.waitFor(t, "post-resume init event", func(ev map[string]any) bool {
		return ev["type"] == "system" && ev["subtype"] == "init" &&
			ev["session_id"] == "session_fake-0001"
	})

	// The prior history survived and the resume's init event was appended.
	after, err := ReadTranscript(eventDir, th.ID)
	if err != nil {
		t.Fatalf("ReadTranscript after resume: %v", err)
	}
	if len(after) <= len(before) {
		t.Fatalf("transcript did not grow across resume: %d -> %d events", len(before), len(after))
	}
	if string(after[0]) != string(before[0]) {
		t.Errorf("resume rewrote the transcript head:\n before: %s\n after:  %s",
			before[0], after[0])
	}
	sup.StopAll()
}

// TestKimiGracefulStopMidTurn is the P1 regression test for the kimi side:
// Stop on a busy thread must cancel the turn via session/cancel, let the
// cancelled turn's result land, and only then close stdin — instead of
// cutting the turn off and relying on the kill backstop. The turn_aborted
// lifecycle event is suppressed during a stop (the exit note is the one the
// UI should show), and new Sends are rejected once the stop is in flight.
func TestKimiGracefulStopMidTurn(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi6", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sup.Send(th.ID, "wait-cancel", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	col.waitFor(t, "in-flight tool call", hasToolUse)

	if err := sup.Stop(th.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := sup.Send(th.ID, "one more thing", nil); err == nil {
		t.Error("send during a stop succeeded; it should be rejected")
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))

	ri := col.indexOf(isResult)
	ei := col.indexOf(isLifecycle("exited"))
	if ri < 0 {
		t.Fatal("no result event for the cancelled turn — stdin was closed without cancelling")
	}
	if ri > ei {
		t.Errorf("result (index %d) arrived after the exit event (index %d)", ri, ei)
	}
	var exitEv map[string]any
	_ = json.Unmarshal(col.snapshot()[ei], &exitEv)
	if got := exitEv["detail"]; got != "exited cleanly" {
		t.Errorf("exit detail = %v, want %q", got, "exited cleanly")
	}
	if i := col.indexOf(isLifecycle("turn_aborted")); i >= 0 {
		t.Errorf("turn_aborted emitted during a stop (index %d); it should be suppressed", i)
	}
	if sup.Running(th.ID) {
		t.Error("thread still running after stop + exited lifecycle")
	}
}

// TestKimiSetConfigOption covers mid-session config changes (model /
// thinking / mode): success resolves, and the CLI's rejection propagates so
// the UI can show it and revert the picker.
func TestKimiSetConfigOption(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi7", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sup.SetConfigOption(th.ID, "model", "kimi-code/kimi-for-coding"); err != nil {
		t.Errorf("SetConfigOption(model): %v", err)
	}
	if err := sup.SetConfigOption(th.ID, "mode", "yolo"); err != nil {
		t.Errorf("SetConfigOption(mode): %v", err)
	}
	if err := sup.SetConfigOption(th.ID, "model", "bad"); err == nil {
		t.Error("SetConfigOption(bad) succeeded; the CLI's rejection should propagate")
	}
	sup.StopAll()
	if err := sup.SetConfigOption(th.ID, "model", "kimi-code/k3"); err == nil {
		t.Error("SetConfigOption on a stopped thread succeeded; it should be rejected")
	}
}

// TestKimiInterruptEscalation covers the signal backstop on a hung turn: a
// turn that never acks session/cancel and ignores SIGINT must be SIGKILLed, so
// reap() reports phase "interrupted" and the thread resumes afterwards. This is
// the riskiest path in the package and TestKimiInterruptIdle only covers the
// no-op cases.
func TestKimiInterruptEscalation(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())
	sup.cancelBackstopDelay = 100 * time.Millisecond
	sup.cancelKillDelay = 100 * time.Millisecond

	th, err := sup.Start(StartOptions{ID: "t-esc", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sup.Send(th.ID, "hang-turn", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	col.waitFor(t, "in-flight tool call", hasToolUse)
	if err := sup.Interrupt(th.ID); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	// The cancel is never acked and SIGINT is ignored, so the backstop escalates
	// to SIGKILL; reap() then reports a user-interrupt, not a plain exit.
	col.waitFor(t, "interrupted lifecycle", isLifecycle("interrupted"))
	deadline := time.Now().Add(5 * time.Second)
	for sup.Running(th.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sup.Running(th.ID) {
		t.Fatal("thread still running after the interrupt backstop escalated")
	}
	// A killed-by-interrupt thread stays resumable on its id (the exit was
	// deliberate, the session survives).
	if _, err := sup.Start(StartOptions{
		ID: "t-esc", WorkDir: t.TempDir(),
		SessionID: "session_fake-0001", Resume: true,
	}); err != nil {
		t.Fatalf("resume after interrupt: %v", err)
	}
	col.waitFor(t, "post-resume init", func(ev map[string]any) bool {
		return ev["type"] == "system" && ev["subtype"] == "init"
	})
	sup.StopAll()
}

// TestKimiMidTurnDeath covers a process that exits mid-turn without answering
// the prompt: onPromptDone sees the stream close (isStreamClosed) and must NOT
// synthesise a result event for a turn that will never complete — reap reports
// the plain exit instead.
func TestKimiMidTurnDeath(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-die", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sup.Send(th.ID, "die-mid-turn", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))
	if i := col.indexOf(isResult); i >= 0 {
		t.Errorf("synthetic result at index %d for a turn that never completed", i)
	}
	if sup.Running(th.ID) {
		t.Error("thread still running after mid-turn death")
	}
	sup.StopAll()
}

// TestKimiListSessionsFiltersProbes covers session/list's probe filtering: a
// session left in kimi's store by a one-shot probe (cwd under the temp
// probe-dir prefix) is dropped; a real session is kept.
func TestKimiListSessionsFiltersProbes(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	sup := NewSupervisor(kimiBin, testLogger(),
		func(string, []json.RawMessage) {}, nil, t.TempDir())

	sessions, err := sup.ListSessions("")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions returned %d sessions, want 1 (the probe leftover filtered): %+v",
			len(sessions), sessions)
	}
	if sessions[0].SessionID != "sess-real" || sessions[0].Cwd != "/home/fake/project" {
		t.Errorf("kept session = %+v, want the non-probe sess-real", sessions[0])
	}
}

// TestKimiDiscoverOptions covers the one-shot config-option probe and its cache:
// the model enumeration comes back, and a second call serves it without a
// re-probe.
func TestKimiDiscoverOptions(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	sup := NewSupervisor(kimiBin, testLogger(),
		func(string, []json.RawMessage) {}, nil, t.TempDir())

	opts, err := sup.DiscoverOptions()
	if err != nil {
		t.Fatalf("DiscoverOptions: %v", err)
	}
	if len(opts) != 1 || opts[0].ID != "model" || len(opts[0].Options) != 2 {
		t.Fatalf("DiscoverOptions = %+v, want the model enumeration", opts)
	}
	opts2, err := sup.DiscoverOptions()
	if err != nil || len(opts2) != 1 {
		t.Fatalf("cached DiscoverOptions = %+v, %v", opts2, err)
	}
}

// TestKimiInterruptIdle: an interrupt with no turn in flight — and an
// interrupt after a turn has already completed — must be a no-op. Neither may
// arm the signal backstop, which would kill the healthy resident process a
// few seconds later.
func TestKimiInterruptIdle(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())
	sup.cancelBackstopDelay = 50 * time.Millisecond
	sup.cancelKillDelay = 20 * time.Millisecond

	th, err := sup.Start(StartOptions{ID: "t-kimi5", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := sup.Interrupt(th.ID); err != nil {
		t.Fatalf("idle interrupt: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // ample time for a mis-armed backstop
	if !sup.Running(th.ID) {
		t.Fatal("idle interrupt killed the process; it must be a no-op")
	}

	// The thread still takes a normal turn, and interrupting AFTER that turn
	// completed (a cancel racing natural completion) is again a no-op.
	if err := sup.Send(th.ID, "hello", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	col.waitFor(t, "turn result", isResult)
	if err := sup.Interrupt(th.ID); err != nil {
		t.Fatalf("post-turn interrupt: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if !sup.Running(th.ID) {
		t.Fatal("post-turn interrupt killed the process; it must be a no-op")
	}
	sup.StopAll()
}

// TestKimiConfigOptionUpdateFlipsRecordedMode covers the stale-mode bug: kimi
// changes mode on its own (its ExitPlanMode transition) and only announces it
// afterwards. The announcement must reach the UI as an `_options` event AND
// revise what the thread reports as its applied mode, so a later resume
// replays the mode the session actually ended on.
func TestKimiConfigOptionUpdateFlipsRecordedMode(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{
		ID: "t-kimi-mode", WorkDir: t.TempDir(), Mode: "plan", Prompt: "flip-mode",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	col.waitFor(t, "options event", isType("_options"))
	if got := th.Mode(); got != "default" {
		t.Errorf("Mode() = %q, want default — the CLI's own flip must be recorded", got)
	}
	idx := col.indexOf(isType("_options"))
	var ev struct {
		Changed       []string       `json:"changed"`
		ConfigOptions []ConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(col.snapshot()[idx], &ev); err != nil {
		t.Fatalf("decode _options: %v", err)
	}
	if len(ev.Changed) != 1 || ev.Changed[0] != "mode" {
		t.Errorf("changed = %v, want [mode]", ev.Changed)
	}
	found := false
	for _, co := range ev.ConfigOptions {
		if co.ID == "mode" {
			found = true
			if co.CurrentValue != "default" {
				t.Errorf("mode currentValue = %q, want default", co.CurrentValue)
			}
		}
	}
	if !found {
		t.Errorf("_options carried no mode option: %+v", ev.ConfigOptions)
	}
}

// TestKimiSetConfigOptionBelievesTheResponse: session/set_config_option
// returns the authoritative post-change config, which need not be the value
// asked for. The harness must record — and announce — what came back.
func TestKimiSetConfigOptionBelievesTheResponse(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-cfg", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	// The fake downgrades "max" to "high", the way kimi downgrades a value it
	// will not run.
	if err := sup.SetConfigOption(th.ID, "thinking", "max"); err != nil {
		t.Fatalf("SetConfigOption: %v", err)
	}
	col.waitFor(t, "options event", isType("_options"))
	if got := th.Thinking(); got != "high" {
		t.Errorf("Thinking() = %q, want high — the response is authoritative", got)
	}
}

// TestKimiStartLoadsHistoryForAnUnknownSession: browse-resuming a session
// created outside Agent Kate has no transcript of ours to replay, so the CLI
// replays it via session/load — and the history lands AFTER the init event.
func TestKimiStartLoadsHistoryForAnUnknownSession(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{
		ID: "t-kimi-load", WorkDir: t.TempDir(),
		SessionID: "session_elsewhere", Resume: true,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	col.waitFor(t, "replayed agent message", assistantText("It is a test fixture."))
	seq := col.snapshot()
	initAt := col.indexOf(isType("system"))
	userAt := col.indexOf(isType("user"))
	agentAt := col.indexOf(assistantText("It is a test fixture."))
	if initAt != 0 {
		t.Fatalf("init event at %d, want first: %s", initAt, seq)
	}
	if userAt < initAt || agentAt < userAt {
		t.Errorf("replay out of order: init=%d user=%d agent=%d\n%s",
			initAt, userAt, agentAt, seq)
	}
	if th.SessionID() != "session_elsewhere" {
		t.Errorf("SessionID = %q, want session_elsewhere", th.SessionID())
	}
}

// A resume of a thread whose transcript we already hold must NOT ask the CLI
// to replay — that would double every card the UI already has.
func TestKimiResumeWithOwnTranscriptSkipsLoad(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	eventDir := t.TempDir()
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, eventDir)

	// Pre-existing transcript for this thread.
	if err := os.WriteFile(filepath.Join(eventDir, "t-kimi-resume.jsonl"),
		[]byte("{\"type\":\"system\"}\n"), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	th, err := sup.Start(StartOptions{
		ID: "t-kimi-resume", WorkDir: t.TempDir(),
		SessionID: "session_fake-0001", Resume: true,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	col.waitFor(t, "init event", isType("system"))
	// Give any replay a chance to show up before declaring there was none.
	time.Sleep(150 * time.Millisecond)
	if idx := col.indexOf(assistantText("It is a test fixture.")); idx >= 0 {
		t.Errorf("session/load replayed on a thread that already has a transcript: %s",
			col.snapshot())
	}
}

// TestKimiCompact drives the in-session compaction: `/compact` goes out as an
// ordinary prompt turn, the turn is visible in the transcript, and the CLI's
// reply comes back to the caller.
func TestKimiCompact(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-compact", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	text, err := sup.Compact(ctx, th.ID)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if text != "Compacted 12 messages." {
		t.Errorf("Compact text = %q, want the CLI's reply", text)
	}
	// The human asked for it, so it reads as a turn.
	col.waitFor(t, "compaction reply card", assistantText("Compacted 12 messages."))
	// Compaction is refused on a thread that isn't there.
	if _, err := sup.Compact(ctx, "t-nope"); err == nil {
		t.Error("Compact on an unknown thread succeeded")
	}
}

// The silent `/usage` probe leaves no trace of its own turn: only the
// `_context` readout, never a card or a second result event.
func TestKimiUsageProbeIsSilent(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{
		ID: "t-kimi-usage", WorkDir: t.TempDir(), Prompt: "hello",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	col.waitFor(t, "usage readout", isType("_context"))
	results, cards := 0, 0
	for _, raw := range col.snapshot() {
		var ev map[string]any
		_ = json.Unmarshal(raw, &ev)
		if ev["type"] == "result" {
			results++
		}
		if assistantText("Context: 4,096 / 128,000 tokens (3%)")(ev) {
			cards++
		}
	}
	if results != 1 {
		t.Errorf("result events = %d, want 1 (the human's turn only)", results)
	}
	if cards != 0 {
		t.Errorf("the /usage reply was rendered as %d card(s); it must be silent", cards)
	}
	// The next turn's result event carries the model's context WINDOW and
	// nothing else usage-shaped: the fill itself already travelled as
	// `_context`, and per-turn token fields would be a billing lie (audit F19b).
	if err := sup.Send(th.ID, "again", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	col.waitFor(t, "second result carrying the context window", func(ev map[string]any) bool {
		if ev["type"] != "result" {
			return false
		}
		if _, billed := ev["usage"]; billed {
			t.Errorf("kimi result event carries a per-turn usage block: %v", ev)
		}
		mu, _ := ev["modelUsage"].(map[string]any)
		for _, v := range mu {
			per, _ := v.(map[string]any)
			return per["contextWindow"] == float64(128000)
		}
		return false
	})
}

// An unauthenticated CLI answers the handshake with an opaque rejection; the
// error the caller sees must name the sign-in method and what to run.
func TestKimiAuthFailureIsActionable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-kimi-noauth")
	script := `#!/usr/bin/env python3
import json, sys
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    f = json.loads(line)
    if f.get("method") == "initialize":
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": f["id"], "result": {
            "protocolVersion": 1, "agentCapabilities": {},
            "authMethods": [{"id": "oauth", "name": "Sign in with Moonshot",
                             "description": "opens a browser"}]}}) + "\n")
    else:
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": f["id"], "error": {
            "code": -32000, "message": "authentication required"}}) + "\n")
    sys.stdout.flush()
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake kimi: %v", err)
	}
	sup := NewSupervisor(path, testLogger(), func(string, []json.RawMessage) {}, nil, t.TempDir())
	_, err := sup.Start(StartOptions{ID: "t-kimi-auth", WorkDir: t.TempDir()})
	if err == nil {
		t.Fatal("start succeeded against an unauthenticated CLI")
	}
	msg := err.Error()
	for _, want := range []string{"not signed in", "Sign in with Moonshot", "opens a browser"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}
}

// commandsUpdate builds an available_commands_update notification announcing
// exactly the named slash commands.
func commandsUpdate(names ...string) json.RawMessage {
	cmds := make([]any, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, map[string]any{"name": n, "description": n})
	}
	return upd("available_commands_update", map[string]any{"availableCommands": cmds})
}

// TestKimiSlashCommandsAreGatedOnTheAnnouncedVocabulary: `/compact` and
// `/usage` are only commands because the CLI says so. Sent to a build that
// doesn't list them they would be plain prompt text — a real model turn whose
// prose reply the caller would store as a "summary" of a session that was
// never compacted.
func TestKimiSlashCommandsAreGatedOnTheAnnouncedVocabulary(t *testing.T) {
	kimiBin := fakeKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-cmds", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	// Nothing announced yet: silence does not prove absence, so the blind
	// send stays allowed and the pre-announcement behaviour is unchanged.
	if !th.hasCommand("/compact") || !th.hasCommand("usage") {
		t.Fatal("an un-announced vocabulary must not gate anything")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A list that omits both: now absence IS known, and both are refused.
	sup.onNotification(th, "session/update", commandsUpdate("init", "help"))
	if th.hasCommand("compact") || th.hasCommand("/usage") {
		t.Fatal("commands absent from the announced list were reported present")
	}
	if _, err := sup.Compact(ctx, th.ID); err == nil {
		t.Error("Compact sent /compact to a session that does not offer it")
	} else if !strings.Contains(err.Error(), compactCommand) {
		t.Errorf("refusal %q does not name the missing command", err)
	}

	// Announced: the command goes through and the CLI's reply comes back.
	sup.onNotification(th, "session/update", commandsUpdate("compact", "usage"))
	text, err := sup.Compact(ctx, th.ID)
	if err != nil {
		t.Fatalf("Compact after the command was announced: %v", err)
	}
	if text != "Compacted 12 messages." {
		t.Errorf("Compact text = %q, want the CLI's reply", text)
	}
}

// hangingKimiScript answers the handshake and ordinary prompts, but never
// answers `/compact` and ignores session/cancel — the hung internal turn that
// used to wedge a thread for good.
func hangingKimiScript(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	path := filepath.Join(t.TempDir(), "fake-kimi-hang")
	script := `#!/usr/bin/env python3
import json, sys

sid = "session_hang-0001"

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    f = json.loads(line)
    method = f.get("method")
    fid = f.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": fid, "result": {
            "protocolVersion": 1, "agentCapabilities": {}, "authMethods": [],
            "agentInfo": {"name": "hang-kimi", "version": "0"}}})
    elif method in ("session/new", "session/resume"):
        send({"jsonrpc": "2.0", "id": fid,
              "result": {"sessionId": sid, "configOptions": []}})
    elif method == "session/prompt":
        text = "".join(b.get("text", "") for b in f["params"]["prompt"]
                       if b.get("type") == "text")
        if text.strip() in ("/compact", "/usage"):
            continue  # never answered, and session/cancel is ignored too
        send({"jsonrpc": "2.0", "method": "session/update",
              "params": {"sessionId": sid, "update": {
                  "sessionUpdate": "agent_message_chunk",
                  "content": {"type": "text", "text": "still here"}}}})
        send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write hanging kimi: %v", err)
	}
	return path
}

// TestKimiTimedOutInternalTurnLeavesTheThreadUsable: an internal turn that
// never gets a reply must release its bookkeeping when its context expires.
// Before the fix, runInternal's ctx.Done() arm returned with t.internal still
// set and activePrompts still raised, so the thread was wedged: every later
// Send paid the full internal-turn timeout, and sendInternal refused forever.
func TestKimiTimedOutInternalTurnLeavesTheThreadUsable(t *testing.T) {
	kimiBin := hangingKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-hang", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := sup.Compact(ctx, th.ID); err == nil {
		t.Fatal("Compact returned success from a CLI that never replied")
	}

	// The bookkeeping is unwound: no internal turn, no phantom prompt in
	// flight — the state sendInternal and Send both gate on.
	th.mu.Lock()
	internal, active := th.internal, th.activePrompts
	th.mu.Unlock()
	if internal != nil || active != 0 {
		t.Fatalf("after the timeout: internal=%v activePrompts=%d, want nil/0",
			internal, active)
	}

	// And the thread really works: the send lands promptly rather than
	// blocking on a turn nobody is waiting for any more.
	done := make(chan error, 1)
	go func() { done <- sup.Send(th.ID, "hello", nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send after the abandoned internal turn: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked after an abandoned internal turn; the thread is wedged")
	}
	col.waitFor(t, "the reply to the send that followed", assistantText("still here"))
}

// TestKimiUserSendPreemptsAnInternalTurn: a human message must never queue
// behind Agent Kate's own bookkeeping. With a `/usage` probe hung in flight,
// Send cancels it and proceeds inside the short pre-empt grace instead of
// waiting out the 30-second internal-turn timeout.
func TestKimiUserSendPreemptsAnInternalTurn(t *testing.T) {
	kimiBin := hangingKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-preempt", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	// Park an internal turn in flight; the fake never answers it.
	if _, err := sup.sendInternal(th, "usage", usageCommand, true); err != nil {
		t.Fatalf("sendInternal: %v", err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- sup.Send(th.ID, "hello", nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	case <-time.After(internalTurnTimeout):
		t.Fatal("Send waited out the internal-turn timeout instead of pre-empting")
	}
	if elapsed := time.Since(start); elapsed > preemptGrace+3*time.Second {
		t.Errorf("Send took %v; a discardable internal turn must not cost that", elapsed)
	}
	col.waitFor(t, "the user's own reply", assistantText("still here"))
}

// usageHangsKimiScript answers `/compact` but never answers the `/usage` probe
// that follows every turn — the shape that made "Summarize now" fail with a
// busy refusal right after a turn.
func usageHangsKimiScript(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	path := filepath.Join(t.TempDir(), "fake-kimi-usage-hang")
	script := `#!/usr/bin/env python3
import json, sys

sid = "session_usagehang-0001"

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    f = json.loads(line)
    method = f.get("method")
    fid = f.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": fid, "result": {
            "protocolVersion": 1, "agentCapabilities": {}, "authMethods": [],
            "agentInfo": {"name": "usage-hang-kimi", "version": "0"}}})
    elif method in ("session/new", "session/resume"):
        send({"jsonrpc": "2.0", "id": fid,
              "result": {"sessionId": sid, "configOptions": []}})
    elif method == "session/prompt":
        text = "".join(b.get("text", "") for b in f["params"]["prompt"]
                       if b.get("type") == "text").strip()
        if text == "/usage":
            continue  # never answered, and session/cancel is ignored too
        send({"jsonrpc": "2.0", "method": "session/update",
              "params": {"sessionId": sid, "update": {
                  "sessionUpdate": "agent_message_chunk",
                  "content": {"type": "text", "text": "Compacted 12 messages."}}}})
        send({"jsonrpc": "2.0", "id": fid, "result": {"stopReason": "end_turn"}})
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write usage-hang kimi: %v", err)
	}
	return path
}

// TestKimiCompactPreemptsTheUsageProbe: `/usage` runs after EVERY turn, and an
// internal turn is only started on an idle thread — so without pre-emption the
// human's "Summarize now" right after a turn is refused as busy. Compaction is
// something the human asked for; the bookkeeping gives way to it exactly as it
// does for a send.
func TestKimiCompactPreemptsTheUsageProbe(t *testing.T) {
	kimiBin := usageHangsKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-compact-preempt", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	// Park the probe in flight, as a just-finished turn would.
	if _, err := sup.sendInternal(th, "usage", usageCommand, true); err != nil {
		t.Fatalf("sendInternal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), internalTurnTimeout)
	defer cancel()
	text, err := sup.Compact(ctx, th.ID)
	if err != nil {
		t.Fatalf("Compact behind a hung usage probe: %v", err)
	}
	if text != "Compacted 12 messages." {
		t.Errorf("compact reply = %q, want the CLI's own", text)
	}
}

// TestKimiSendClaimsTheTurnAgainstARacingProbe: preemptInternal must run
// without the thread lock (it waits on the turn's unwind), which leaves a
// window between it returning and Send claiming the turn slot. The post-turn
// `/usage` probe runs on its own goroutine and can start a fresh internal turn
// inside that window — and then TWO session/prompt requests are in flight,
// which kimi rejects. Send therefore re-checks t.internal under the same lock
// hold that increments activePrompts, and pre-empts again if one appeared.
//
// The onPreempt hook opens that window deterministically; it is otherwise
// unhittable. Before the fix Send sailed past the intruder, leaving t.internal
// set while the human's turn ran.
func TestKimiSendClaimsTheTurnAgainstARacingProbe(t *testing.T) {
	kimiBin := hangingKimiScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(kimiBin, testLogger(), col.add, nil, t.TempDir())

	th, err := sup.Start(StartOptions{ID: "t-kimi-claim", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sup.Stop(th.ID)

	// Exactly once, sneak a `/usage` probe into the claim window. The fake
	// never answers it and ignores session/cancel, so only a real pre-empt
	// (abandon after the grace) can clear it.
	var once sync.Once
	sup.onPreempt = func(tt *Thread) {
		once.Do(func() {
			if _, err := sup.sendInternal(tt, "usage", usageCommand, true); err != nil {
				t.Errorf("could not park the racing probe: %v", err)
			}
		})
	}

	if err := sup.Send(th.ID, "hello", nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	th.mu.Lock()
	internal := th.internal
	th.mu.Unlock()
	if internal != nil {
		t.Errorf("Send wrote its prompt with a %q internal turn still holding the slot",
			internal.kind)
	}
	// And the human's turn really ran.
	col.waitFor(t, "the user's own reply", assistantText("still here"))
}

// TestKimiUsageProbeIsThrottled: the routine post-turn `/usage` probe is a full
// extra ACP round-trip that the human's next send then has to pre-empt (up to
// preemptGrace of stall on a message already sent). It is therefore rate
// limited — but never when the figure has moved discontinuously, which is what
// Compact's force=true covers.
func TestKimiUsageProbeIsThrottled(t *testing.T) {
	sup := &Supervisor{log: testLogger()}
	// Not alive: every probe this test lets through fails fast inside
	// sendInternal, so only the throttle gate is under test.
	th := &Thread{ID: "t-throttle", commands: map[string]bool{"usage": true}}

	probedAt := func() time.Time {
		th.mu.Lock()
		defer th.mu.Unlock()
		return th.usageProbedAt
	}

	sup.refreshUsage(th, false)
	first := probedAt()
	if first.IsZero() {
		t.Fatal("the first probe of a session must not be throttled")
	}

	sup.refreshUsage(th, false)
	if !probedAt().Equal(first) {
		t.Error("a second probe inside usageProbeInterval must be skipped")
	}

	sup.refreshUsage(th, true)
	forced := probedAt()
	if forced.Equal(first) {
		t.Error("force (a compaction just moved the figure) must bypass the throttle")
	}

	// Once the readout is stale the routine probe runs again.
	stale := time.Now().Add(-usageProbeInterval - time.Second)
	th.mu.Lock()
	th.usageProbedAt = stale
	th.mu.Unlock()
	sup.refreshUsage(th, false)
	if probedAt().Equal(stale) {
		t.Error("a readout older than usageProbeInterval must be refreshed")
	}

	// A CLI that never announced /usage is never probed at all, throttle or no.
	quiet := &Thread{ID: "t-nousage", commands: map[string]bool{"compact": true}}
	sup.refreshUsage(quiet, true)
	quiet.mu.Lock()
	defer quiet.mu.Unlock()
	if !quiet.usageProbedAt.IsZero() {
		t.Error("probed a CLI that offers no /usage command")
	}
}
