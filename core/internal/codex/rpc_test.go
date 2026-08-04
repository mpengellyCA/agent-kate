package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRPCClientCallUsesNewlineDelimitedJSONRPC(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	client := newRPCClient(clientConn, nil)
	go client.readLoop(clientConn)

	done := make(chan error, 1)
	var got struct {
		OK bool `json:"ok"`
	}
	go func() {
		done <- client.call(context.Background(), "initialize",
			map[string]string{"client": "Agent Kate"}, &got)
	}()

	line, err := bufio.NewReader(serverConn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var request struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if request.JSONRPC != "2.0" || request.ID != 1 || request.Method != "initialize" {
		t.Fatalf("request = %#v, want JSON-RPC initialize id 1", request)
	}
	if _, err := serverConn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n")); err != nil {
		t.Fatalf("write response: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("call: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("call did not receive its response")
	}
	if !got.OK {
		t.Fatal("decoded response was not returned")
	}
}

func TestAppServerAgentMessageBecomesAssistantAndResult(t *testing.T) {
	var got []json.RawMessage
	s := NewSupervisor("codex", nil, func(_ string, events []json.RawMessage) {
		got = append(got, events...)
	})
	thread := &Thread{ID: "agent-kate-thread", sessionID: "codex-thread", text: make(map[string]*strings.Builder)}

	s.onNotification(thread, "item/agentMessage/delta", json.RawMessage(`{"itemId":"message-1","delta":"Hello"}`))
	s.onNotification(thread, "item/agentMessage/delta", json.RawMessage(`{"itemId":"message-1","delta":", Kate"}`))
	s.onNotification(thread, "item/completed", json.RawMessage(`{"item":{"id":"message-1","type":"agentMessage"}}`))
	s.onNotification(thread, "turn/completed", json.RawMessage(`{"turnId":"turn-1","turn":{"status":"completed"}}`))

	if len(got) != 6 {
		t.Fatalf("emitted %d events, want stream start/deltas/stop plus assistant and result", len(got))
	}
	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(got[4], &assistant); err != nil {
		t.Fatalf("decode assistant event: %v", err)
	}
	if assistant.Type != "assistant" || len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "Hello, Kate" {
		t.Fatalf("assistant event = %s", got[4])
	}
	var result struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(got[5], &result); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if result.Type != "result" || result.SessionID != "codex-thread" {
		t.Fatalf("result event = %s", got[5])
	}
}

func TestCommandApprovalIsBrokeredAndAnswered(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var askedTool string
	s := NewSupervisor("codex", nil, nil)
	s.SetPermissionFunc(func(threadID, toolName string, input json.RawMessage) bool {
		if threadID != "agent-kate-thread" || len(input) == 0 {
			t.Fatal("broker received incomplete approval request")
		}
		askedTool = toolName
		return true
	})
	thread := &Thread{ID: "agent-kate-thread", rpc: newRPCClient(clientConn, nil)}
	done := make(chan struct{})
	go func() {
		s.onRequest(thread, json.RawMessage("17"), "item/commandExecution/requestApproval", json.RawMessage(`{"command":"git status"}`))
		close(done)
	}()

	line, err := bufio.NewReader(serverConn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read approval response: %v", err)
	}
	<-done
	if askedTool != "Bash" {
		t.Fatalf("broker tool = %q, want Bash", askedTool)
	}
	var response struct {
		ID     int `json:"id"`
		Result struct {
			Decision string `json:"decision"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if response.ID != 17 || response.Result.Decision != "accept" {
		t.Fatalf("approval response = %s", line)
	}
}

func TestUserInputQuestionIsBrokeredAndMappedBackToCodexIDs(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var gotInput struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	s := NewSupervisor("codex", nil, nil)
	s.SetQuestionFunc(func(threadID, toolName string, input json.RawMessage) (bool, json.RawMessage) {
		if threadID != "agent-kate-thread" || toolName != "AskUserQuestion" {
			t.Fatalf("question route = %q/%q", threadID, toolName)
		}
		if err := json.Unmarshal(input, &gotInput); err != nil {
			t.Fatalf("neutral question input: %v", err)
		}
		return true, json.RawMessage(`{"answers":{"Deploy?":"Staging"}}`)
	})
	thread := &Thread{ID: "agent-kate-thread", rpc: newRPCClient(clientConn, nil)}
	done := make(chan struct{})
	go func() {
		s.onRequest(thread, json.RawMessage("18"), "item/tool/requestUserInput", json.RawMessage(`{"questions":[{"id":"deploy-target","question":"Deploy?","options":[{"label":"Staging","description":"Safe"},{"label":"Production","description":"Live"}]}]}`))
		close(done)
	}()

	line, err := bufio.NewReader(serverConn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read user-input response: %v", err)
	}
	<-done
	if len(gotInput.Questions) != 1 || gotInput.Questions[0].Question != "Deploy?" || len(gotInput.Questions[0].Options) != 2 {
		t.Fatalf("neutral question = %#v", gotInput)
	}
	var response struct {
		ID     int `json:"id"`
		Result struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("decode user-input response: %v", err)
	}
	answer, ok := response.Result.Answers["deploy-target"]
	if response.ID != 18 || !ok || len(answer.Answers) != 1 || answer.Answers[0] != "Staging" {
		t.Fatalf("user-input response = %s", line)
	}
}

func TestUserInputQuestionBridgesFreeTextShape(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	s := NewSupervisor("codex", nil, nil)
	s.SetQuestionFunc(func(_ string, _ string, input json.RawMessage) (bool, json.RawMessage) {
		if !strings.Contains(string(input), `"allowOther":true`) {
			t.Fatalf("free-text capability was dropped: %s", input)
		}
		return true, json.RawMessage(`{"answers":{"Why?":"A custom reason"}}`)
	})
	thread := &Thread{ID: "agent-kate-thread", rpc: newRPCClient(clientConn, nil)}
	done := make(chan struct{})
	go func() {
		s.onRequest(thread, json.RawMessage("19"), "item/tool/requestUserInput", json.RawMessage(`{"questions":[{"id":"reason","question":"Why?","isOther":true,"options":[{"label":"A","description":""}]}]}`))
		close(done)
	}()

	line, err := bufio.NewReader(serverConn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read user-input response: %v", err)
	}
	<-done
	if strings.Contains(string(line), `"error"`) || !strings.Contains(string(line), "A custom reason") {
		t.Fatalf("free-text response = %s", line)
	}
}

func TestCompactionNotificationOnlyCompletesOwnedCompaction(t *testing.T) {
	var got []json.RawMessage
	s := NewSupervisor("codex", nil, func(_ string, events []json.RawMessage) { got = append(got, events...) })
	thread := &Thread{ID: "agent-kate-thread", sessionID: "codex-thread"}

	// A delayed notification after a cancelled call, or an unsolicited server
	// notification, is not a terminal user turn.
	s.onNotification(thread, "thread/compacted", json.RawMessage(`{"threadId":"codex-thread"}`))
	if len(got) != 0 {
		t.Fatalf("unowned compaction emitted events: %s", got)
	}

	done := make(chan struct{})
	thread.compacting, thread.compactDone = true, done
	s.onNotification(thread, "thread/compacted", json.RawMessage(`{"threadId":"codex-thread"}`))
	select {
	case <-done:
	default:
		t.Fatal("owned compaction did not complete")
	}
	if len(got) != 2 {
		t.Fatalf("owned compaction emitted %d events, want boundary and result", len(got))
	}
	s.onNotification(thread, "thread/compacted", json.RawMessage(`{"threadId":"codex-thread"}`))
	if len(got) != 2 {
		t.Fatalf("duplicate compaction emitted events: %d", len(got))
	}
}

func TestFailedCommandWithoutOutputStillFinalizesToolCard(t *testing.T) {
	var got []json.RawMessage
	s := NewSupervisor("codex", nil, func(_ string, events []json.RawMessage) { got = append(got, events...) })
	thread := &Thread{ID: "agent-kate-thread", started: map[string]string{"cmd-1": "commandExecution"}}
	s.onNotification(thread, "item/completed", json.RawMessage(`{"item":{"id":"cmd-1","type":"commandExecution","command":"false","status":"failed"}}`))
	if len(got) != 1 || !strings.Contains(string(got[0]), `"is_error":true`) {
		t.Fatalf("failed command result = %s", got)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestFailedWriteLatchesTheRPCStreamBroken(t *testing.T) {
	broken := errors.New("broken pipe")
	client := newRPCClient(failingWriter{err: broken}, nil)
	if err := client.call(context.Background(), "turn/start", map[string]any{}, nil); !errors.Is(err, broken) {
		t.Fatalf("first write = %v, want broken pipe", err)
	}
	if err := client.call(context.Background(), "turn/interrupt", map[string]any{}, nil); !errors.Is(err, errWriteBroken) {
		t.Fatalf("second write = %v, want broken-stream error", err)
	}
}
