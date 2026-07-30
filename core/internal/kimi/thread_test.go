package kimi

import (
	"encoding/json"
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
//   - session/new reports a configOptions set (model enumeration), the shape
//     kimi 0.30 returns.
func fakeKimiScript(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	path := filepath.Join(t.TempDir(), "fake-kimi")
	script := `#!/usr/bin/env python3
import json, sys

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
            "agentCapabilities": {"promptCapabilities": {"image": True}},
            "agentInfo": {"name": "fake-kimi", "version": "0"}}})
    elif method in ("session/new", "session/load", "session/resume"):
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
            send({"jsonrpc": "2.0", "id": fid, "result": {"configOptions": []}})
    elif method == "session/cancel":
        if pending_prompt is not None:
            send({"jsonrpc": "2.0", "id": pending_prompt,
                  "result": {"stopReason": "cancelled"}})
            pending_prompt = None
    elif method == "session/prompt":
        text = "".join(b.get("text", "") for b in f["params"]["prompt"]
                       if b.get("type") == "text")
        if "wait-cancel" in text:
            upd({"sessionUpdate": "agent_message_chunk",
                 "content": {"type": "text", "text": "working"}})
            upd({"sessionUpdate": "tool_call", "toolCallId": "tc-wait",
                 "title": "Bash", "kind": "execute", "status": "in_progress",
                 "rawInput": {"command": "sleep 600"}})
            pending_prompt = fid
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
	seq := col.snapshot()
	wantTypes := []string{"system", "assistant", "assistant", "assistant",
		"assistant", "user", "result"}
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
	perm := func(_, toolName string, input json.RawMessage) bool {
		gotTool = toolName
		_ = json.Unmarshal(input, &gotInput)
		return true
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
