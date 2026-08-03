package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFakeAppServer is executed as a subprocess by the tests below.  It is a
// deliberately small app-server: enough protocol to freeze the launch, turn,
// event translation and fork contracts without spending Codex tokens.
func TestFakeAppServer(t *testing.T) {
	if os.Getenv("AK_CODEX_FAKE") != "1" {
		return
	}
	logPath := os.Getenv("AK_CODEX_FAKE_LOG")
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	sc := bufio.NewScanner(os.Stdin)
	turns := 0
	for sc.Scan() {
		var f struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		if logPath != "" {
			log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = log.Write(append(append([]byte(nil), sc.Bytes()...), '\n'))
				_ = log.Close()
			}
		}
		response := func(result any) {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(f.ID), "result": result})
			_, _ = out.Write(append(b, '\n'))
			_ = out.Flush()
		}
		notify := func(method string, p any) {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": p})
			_, _ = out.Write(append(b, '\n'))
			_ = out.Flush()
		}
		switch f.Method {
		case "initialize":
			response(map[string]any{"codexHome": "/fake", "platformFamily": "unix", "platformOs": "linux", "userAgent": "fake"})
		case "initialized":
			// JSON-RPC notification: the real app-server sends no response.
		case "thread/start", "thread/resume", "thread/fork":
			response(map[string]any{"thread": map[string]any{"id": "codex-thread-1", "model": "gpt-test"}, "model": "gpt-test", "reasoningEffort": "medium", "approvalPolicy": "on-request", "sandbox": "workspace-write"})
		case "turn/start":
			turns++
			turnID := fmt.Sprintf("turn-%d", turns)
			notifyID := fmt.Sprintf("msg-%d", turns)
			response(map[string]any{"turn": map[string]any{"id": turnID}})
			notify("item/agentMessage/delta", map[string]any{"threadId": "codex-thread-1", "turnId": turnID, "itemId": notifyID, "delta": "hello "})
			notify("item/agentMessage/delta", map[string]any{"threadId": "codex-thread-1", "turnId": turnID, "itemId": notifyID, "delta": "world"})
			notify("item/completed", map[string]any{"threadId": "codex-thread-1", "turnId": turnID, "completedAtMs": 1, "item": map[string]any{"id": notifyID, "type": "agentMessage", "text": "hello world"}})
			notify("turn/completed", map[string]any{"threadId": "codex-thread-1", "turn": map[string]any{"id": turnID, "status": "completed"}})
		case "turn/interrupt":
			response(map[string]any{})
		case "model/list":
			response(map[string]any{"data": []map[string]any{{"id": "gpt-test", "displayName": "Test model", "supportedReasoningEfforts": []map[string]string{{"reasoningEffort": "low"}, {"reasoningEffort": "medium"}}}}, "nextCursor": nil})
		default:
			response(map[string]any{})
		}
	}
	os.Exit(0)
}

type collected struct {
	mu     sync.Mutex
	values []json.RawMessage
}

func (c *collected) add(_ string, evs []json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values, evs...)
}
func (c *collected) contains(t *testing.T, fn func(map[string]any) bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		values := append([]json.RawMessage(nil), c.values...)
		c.mu.Unlock()
		for _, raw := range values {
			var v map[string]any
			if json.Unmarshal(raw, &v) == nil && fn(v) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func fakeSupervisor(t *testing.T, c *collected) *Supervisor {
	t.Helper()
	t.Setenv("AK_CODEX_FAKE", "1")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	return NewSupervisor(os.Args[0], slog.New(slog.NewTextHandler(io.Discard, nil)), c.add)
}

func recordedRequests(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatal(err)
		}
		out = append(out, frame)
	}
	return out
}

func TestSupervisorStartsTurnsAndTranslatesEvents(t *testing.T) {
	var c collected
	logPath := filepath.Join(t.TempDir(), "rpc.jsonl")
	t.Setenv("AK_CODEX_FAKE_LOG", logPath)
	s := fakeSupervisor(t, &c)
	thread, err := s.Start(StartOptions{ID: "ak-1", WorkDir: t.TempDir(), Prompt: "say hello", Model: "gpt-test", Effort: "high", ApprovalPolicy: "on-request", Sandbox: "workspace-write"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop(thread.ID)
	if thread.SessionID() != "codex-thread-1" || thread.Model() != "gpt-test" || thread.Effort() != "high" {
		t.Fatalf("thread state = session=%q model=%q effort=%q", thread.SessionID(), thread.Model(), thread.Effort())
	}
	if !c.contains(t, func(v map[string]any) bool {
		return v["type"] == "system" && v["subtype"] == "init" && v["session_id"] == "codex-thread-1"
	}) {
		t.Error("missing init event")
	}
	if !c.contains(t, func(v map[string]any) bool {
		if v["type"] != "assistant" {
			return false
		}
		m, _ := v["message"].(map[string]any)
		content, _ := m["content"].([]any)
		return len(content) == 1 && content[0].(map[string]any)["text"] == "hello world"
	}) {
		t.Error("missing translated assistant text")
	}
	if !c.contains(t, func(v map[string]any) bool { return v["type"] == "result" && v["subtype"] == "success" }) {
		t.Error("missing result event")
	}
	got, err := s.ReadTranscript("ak-1")
	if err != nil || len(got) < 3 {
		t.Fatalf("ReadTranscript = %d events, %v", len(got), err)
	}
	// App-server requires initialize to be acknowledged with a notification,
	// and effort is a turn/start setting rather than a thread/start field.
	var sawInitialized, sawInitialEffort bool
	for _, frame := range recordedRequests(t, logPath) {
		if frame["method"] == "initialized" {
			sawInitialized = true
		}
		if frame["method"] != "turn/start" {
			continue
		}
		params, _ := frame["params"].(map[string]any)
		sawInitialEffort = params["effort"] == "high" && params["model"] == "gpt-test"
	}
	if !sawInitialized || !sawInitialEffort {
		t.Fatalf("app-server handshake/turn settings missing: initialized=%v effort=%v",
			sawInitialized, sawInitialEffort)
	}
}

func TestSetOptionQueuesOverridesForTheNextTurn(t *testing.T) {
	var c collected
	logPath := filepath.Join(t.TempDir(), "rpc.jsonl")
	t.Setenv("AK_CODEX_FAKE_LOG", logPath)
	s := fakeSupervisor(t, &c)
	thread, err := s.Start(StartOptions{ID: "ak-options", WorkDir: t.TempDir(), Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	for option, value := range map[string]string{
		"model": "gpt-next", "effort": "xhigh", "permissionMode": "never",
	} {
		if got, err := s.SetOption(thread.ID, option, value); err != nil || got != value {
			t.Fatalf("SetOption(%q) = %q, %v", option, got, err)
		}
	}
	if err := s.Send(thread.ID, "second", nil); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, frame := range recordedRequests(t, logPath) {
		if frame["method"] == "thread/settings/update" {
			t.Fatal("used removed thread/settings/update endpoint")
		}
		if frame["method"] != "turn/start" {
			continue
		}
		params, _ := frame["params"].(map[string]any)
		input, _ := params["input"].([]any)
		if len(input) == 0 {
			continue
		}
		firstInput, _ := input[0].(map[string]any)
		if firstInput["text"] == "second" {
			found = params["model"] == "gpt-next" &&
				params["effort"] == "xhigh" &&
				params["approvalPolicy"] == "never"
		}
	}
	if !found {
		t.Fatal("next Codex turn did not carry the queued model, effort and approval policy")
	}
	if err := s.Stop(thread.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Running(thread.ID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Running(thread.ID) {
		t.Fatal("Codex child did not stop before test cleanup")
	}
}

func TestStartForkUsesThreadFork(t *testing.T) {
	var c collected
	logPath := filepath.Join(t.TempDir(), "rpc.jsonl")
	t.Setenv("AK_CODEX_FAKE_LOG", logPath)
	s := fakeSupervisor(t, &c)
	thread, err := s.Start(StartOptions{ID: "ak-fork", WorkDir: t.TempDir(), Resume: true, ForkSession: true, SessionID: "source-thread"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop(thread.ID)
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"method":"thread/fork"`) || !strings.Contains(string(b), `"threadId":"source-thread"`) {
		t.Fatalf("fork request missing from %s", b)
	}
}

func TestDiscoverModelsDoesNotCreateAThread(t *testing.T) {
	var c collected
	s := fakeSupervisor(t, &c)
	models, err := s.DiscoverModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "gpt-test" || models[0].Name != "Test model" || len(models[0].Efforts) != 2 {
		t.Fatalf("models = %#v", models)
	}
	s.mu.Lock()
	n := len(s.threads)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("model discovery registered %d running thread(s)", n)
	}
}

// A failed client tool reaches Codex app-server stderr through its Rust tracing
// subscriber. It is not Bash output and it is not an Agent Kate failure; freeze
// the translated provenance so the transcript can say exactly that.
func TestToolRouterStderrIsStructuredAndTerminalCodesAreRemoved(t *testing.T) {
	var c collected
	s := &Supervisor{emit: c.add}
	thread := &Thread{ID: "ak-stderr"}
	s.pumpStderr(thread, strings.NewReader(
		"\x1b[2m2026-08-03T16:44:56.107468Z\x1b[0m \x1b[31mERROR\x1b[0m "+
			"\x1b[2mcodex_core::tools::router\x1b[0m\x1b[2m:\x1b[0m "+
			"\x1b[3merror\x1b[0m\x1b[2m=\x1b[0mapply_patch verification failed: "+
			"Failed to find expected lines\n"))

	if !c.contains(t, func(v map[string]any) bool {
		return v["type"] == "_stderr" &&
			v["source"] == "Codex CLI" &&
			v["severity"] == "error" &&
			v["component"] == "codex_core::tools::router" &&
			v["tool"] == "apply_patch" &&
			v["text"] == "verification failed: Failed to find expected lines"
	}) {
		t.Fatalf("tool-router stderr was not translated into the expected diagnostic: %#v", c.values)
	}
	for _, raw := range c.values {
		if strings.Contains(string(raw), "\x1b") {
			t.Fatalf("terminal control escaped into transcript event: %q", raw)
		}
	}
}
