package codex

import (
	"bufio"
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
			_ = os.WriteFile(logPath, append(append([]byte(nil), sc.Bytes()...), '\n'), 0o600)
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
		case "thread/start", "thread/resume", "thread/fork":
			response(map[string]any{"thread": map[string]any{"id": "codex-thread-1", "model": "gpt-test"}, "model": "gpt-test", "reasoningEffort": "medium", "approvalPolicy": "on-request", "sandbox": "workspace-write"})
		case "turn/start":
			response(map[string]any{"turn": map[string]any{"id": "turn-1"}})
			notify("item/agentMessage/delta", map[string]any{"threadId": "codex-thread-1", "turnId": "turn-1", "itemId": "msg-1", "delta": "hello "})
			notify("item/agentMessage/delta", map[string]any{"threadId": "codex-thread-1", "turnId": "turn-1", "itemId": "msg-1", "delta": "world"})
			notify("item/completed", map[string]any{"threadId": "codex-thread-1", "turnId": "turn-1", "completedAtMs": 1, "item": map[string]any{"id": "msg-1", "type": "agentMessage", "text": "hello world"}})
			notify("turn/completed", map[string]any{"threadId": "codex-thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed"}})
		case "turn/interrupt", "thread/settings/update":
			response(map[string]any{})
		case "model/list":
			response(map[string]any{"data": []map[string]any{{"model": "gpt-test", "displayName": "Test model", "supportedReasoningEfforts": []map[string]string{{"reasoningEffort": "low"}, {"reasoningEffort": "medium"}}}}})
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

func TestSupervisorStartsTurnsAndTranslatesEvents(t *testing.T) {
	var c collected
	s := fakeSupervisor(t, &c)
	thread, err := s.Start(StartOptions{ID: "ak-1", WorkDir: t.TempDir(), Prompt: "say hello", Model: "gpt-test", Effort: "medium", ApprovalPolicy: "on-request", Sandbox: "workspace-write"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop(thread.ID)
	if thread.SessionID() != "codex-thread-1" || thread.Model() != "gpt-test" || thread.Effort() != "medium" {
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
