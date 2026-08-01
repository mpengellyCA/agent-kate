package agent

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

// fakeClaudeScript writes a stand-in `claude` that speaks just enough
// stream-json for the supervisor tests (pattern: the fake kimi script in
// internal/kimi/thread_test.go; cmd/akcore's shell fake models only the
// stdin-close exit — this one also models turns and the in-band interrupt):
//
//   - on start: emits the init system event;
//   - a user message containing "hold" opens a turn that never completes on
//     its own: a tool_use is emitted and the result is held until an
//     interrupt control_request arrives (modelling a long-running tool);
//   - any other user message answers with one text event and a result;
//   - control_request/interrupt is acked with a control_response, and a held
//     turn's (aborted) result is then emitted — the behaviour a spike against
//     claude 2.1.185 confirmed;
//   - control_request set_model / set_permission_mode answer success, except
//     a value of "bad" which answers the error shape claude 2.1.220 uses for
//     an unrecognised model / mode; other subtypes answer "Unsupported
//     control request subtype" (also the real CLI's behaviour);
//   - EOF on stdin exits 0 — the CLI's clean "input closed" exit.
func fakeClaudeScript(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := `#!/usr/bin/env python3
import json, sys

def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

send({"type": "system", "subtype": "init", "session_id": "sess-fake",
      "model": "fake-model"})
held = False
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    t = msg.get("type")
    if t == "user":
        content = msg.get("message", {}).get("content", [])
        text = "".join(b.get("text", "") for b in content
                       if isinstance(b, dict) and b.get("type") == "text")
        if "hold" in text:
            send({"type": "assistant", "message": {"role": "assistant", "content": [
                {"type": "tool_use", "id": "tu-hold", "name": "Bash",
                 "input": {"command": "sleep 600"}}]}})
            held = True
        else:
            send({"type": "assistant", "message": {"role": "assistant", "content": [
                {"type": "text", "text": "ok"}]}})
            send({"type": "result", "subtype": "success", "is_error": False,
                  "session_id": "sess-fake"})
    elif t == "control_request":
        req = msg.get("request", {})
        sub = req.get("subtype")
        rid = msg.get("request_id")
        if sub == "interrupt":
            send({"type": "control_response", "response": {
                "request_id": rid, "subtype": "success"}})
            if held:
                send({"type": "result", "subtype": "error_during_execution",
                      "is_error": True, "session_id": "sess-fake"})
                held = False
        elif sub == "get_context_usage":
            # Nested envelope: the answer sits under response.response, with
            # totalTokens/maxTokens/categories[{name,tokens}]. This is the
            # shape claude 2.1.220 was captured sending (see control_test.go
            # for the verbatim capture); the figures here are the double's.
            send({"type": "control_response", "response": {
                "request_id": rid, "subtype": "success",
                "response": {"totalTokens": 41000, "maxTokens": 200000,
                             "categories": [
                                 {"name": "System prompt", "tokens": 3000},
                                 {"name": "MCP tools", "tokens": 8000},
                                 {"name": "Messages", "tokens": 30000}]}}})
        elif sub == "list_models":
            # Same nesting, with the captured entry vocabulary:
            # value / displayName / supportsEffort / supportedEffortLevels.
            send({"type": "control_response", "response": {
                "request_id": rid, "subtype": "success",
                "response": {"models": [
                    {"value": "opus", "displayName": "Opus 5",
                     "supportsEffort": True,
                     "supportedEffortLevels": ["low", "medium", "high", "max"]},
                    {"value": "haiku", "displayName": "Haiku 4.5",
                     "supportsEffort": True,
                     "supportedEffortLevels": ["low", "medium"]}]}}})
        elif sub in ("set_max_thinking_tokens", "reload_skills"):
            send({"type": "control_response", "response": {
                "request_id": rid, "subtype": "success"}})
        elif sub in ("set_model", "set_permission_mode"):
            value = req.get("model") or req.get("mode") or ""
            if value == "bad":
                send({"type": "control_response", "response": {
                    "request_id": rid, "subtype": "error",
                    "error": "not a recognized value: " + value}})
            else:
                send({"type": "control_response", "response": {
                    "request_id": rid, "subtype": "success"}})
        else:
            send({"type": "control_response", "response": {
                "request_id": rid, "subtype": "error",
                "error": "Unsupported control request subtype: " + str(sub)}})
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestGracefulStopMidTurn is the P1 regression test: Stop on a busy thread
// must abort the turn in-band, let the aborted turn's result land, and only
// then close stdin — so the CLI exits cleanly instead of being SIGKILLed
// mid-turn by the backstop (which could truncate the session JSONL).
func TestGracefulStopMidTurn(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-stop1", WorkDir: t.TempDir(), Prompt: "hold this"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "in-flight tool call", hasToolUse)

	if err := sup.Stop(th.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))

	// The aborted turn's result must have been emitted, and BEFORE the exit —
	// the whole point of the graceful sequencing.
	ri := col.indexOf(isResult)
	ei := col.indexOf(isLifecycle("exited"))
	if ri < 0 {
		t.Fatal("no result event for the aborted turn — stdin was closed without interrupting")
	}
	if ri > ei {
		t.Errorf("result (index %d) arrived after the exit event (index %d)", ri, ei)
	}
	// A graceful stop ends in a clean exit, not a backstop kill and not a
	// user-interrupt report.
	var exitEv map[string]any
	_ = json.Unmarshal(col.snapshot()[ei], &exitEv)
	if got := exitEv["detail"]; got != "exited cleanly" {
		t.Errorf("exit detail = %v, want %q", got, "exited cleanly")
	}
	// The stop suppresses the turn_aborted lifecycle event: the exit note is
	// the one the UI should show.
	if i := col.indexOf(isLifecycle("turn_aborted")); i >= 0 {
		t.Errorf("turn_aborted emitted during a stop (index %d); it should be suppressed", i)
	}
	if sup.Running(th.ID) {
		t.Error("thread still running after stop + exited lifecycle")
	}
}

// TestStopIdle covers the classic path: an idle thread's Stop closes stdin and
// the process exits cleanly, with no interrupt frame involved (the fake would
// answer one with a spurious result; the single result asserted here is the
// turn's own).
func TestStopIdle(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-stop2", WorkDir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "turn result", isResult)
	if err := sup.Stop(th.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))
	results := 0
	for _, raw := range col.snapshot() {
		var ev map[string]any
		if json.Unmarshal(raw, &ev) == nil && isResult(ev) {
			results++
		}
	}
	if results != 1 {
		t.Errorf("saw %d results; an idle stop must not interrupt (want 1)", results)
	}
}

// TestSendWhileStoppingRejected: once a Stop is in flight, new messages are
// rejected deterministically instead of racing the stdin close and vanishing.
func TestSendWhileStoppingRejected(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-stop3", WorkDir: t.TempDir(), Prompt: "hold this"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "in-flight tool call", hasToolUse)
	if err := sup.Stop(th.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := sup.Send(th.ID, "one more thing", nil); err == nil {
		t.Error("send during a stop succeeded; it should be rejected")
	}
	col.waitFor(t, "exited lifecycle", isLifecycle("exited"))
}

// TestInterruptIdle: an interrupt with no turn in flight — and an interrupt
// after a turn has already completed — must be a no-op. Neither may arm the
// signal backstop, which would kill the healthy resident process a few
// seconds later (the hazard the kimi supervisor already guards against).
func TestInterruptIdle(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)
	sup.interruptBackstopDelay = 50 * time.Millisecond
	sup.interruptKillDelay = 20 * time.Millisecond

	th, err := sup.Start(StartOptions{ID: "t-int1", WorkDir: t.TempDir()})
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

// TestSetModelAndPermissionMode covers the mid-session control requests: a
// success resolves the call, and the CLI's rejection text is returned
// verbatim so the UI can show it and revert the picker.
func TestSetModelAndPermissionMode(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-ctl1", WorkDir: t.TempDir(), Prompt: "hello"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "turn result", isResult)

	if err := sup.SetModel(th.ID, "claude-sonnet-4-6"); err != nil {
		t.Errorf("SetModel: %v", err)
	}
	if err := sup.SetPermissionMode(th.ID, "plan"); err != nil {
		t.Errorf("SetPermissionMode: %v", err)
	}
	if err := sup.SetModel(th.ID, "bad"); err == nil {
		t.Error("SetModel(bad) succeeded; the CLI's rejection should propagate")
	} else if !strings.Contains(err.Error(), "not a recognized value") {
		t.Errorf("SetModel(bad) error = %q, want the CLI's text", err)
	}
	sup.StopAll()

	if err := sup.SetModel(th.ID, "claude-sonnet-4-6"); err == nil {
		t.Error("SetModel on a stopped thread succeeded; it should be rejected")
	}
}

// TestInterruptMidTurn: the pre-existing in-band interrupt contract still
// holds with turn tracking in place — the aborted turn emits its result plus
// a turn_aborted lifecycle event, the process stays resident, and a follow-up
// message runs a normal turn on the same stdin.
func TestInterruptMidTurn(t *testing.T) {
	claudeBin := fakeClaudeScript(t)
	col := &eventCollector{}
	sup := NewSupervisor(claudeBin, testLogger(), col.add)

	th, err := sup.Start(StartOptions{ID: "t-int2", WorkDir: t.TempDir(), Prompt: "hold this"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	col.waitFor(t, "in-flight tool call", hasToolUse)
	if err := sup.Interrupt(th.ID); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	col.waitFor(t, "turn_aborted lifecycle", isLifecycle("turn_aborted"))
	if !sup.Running(th.ID) {
		t.Fatal("thread died on interrupt; it should stay resident")
	}
	if err := sup.Send(th.ID, "again", nil); err != nil {
		t.Fatalf("follow-up send: %v", err)
	}
	col.waitFor(t, "follow-up result", func(ev map[string]any) bool {
		return ev["type"] == "result" && ev["subtype"] == "success"
	})
	sup.StopAll()
}
