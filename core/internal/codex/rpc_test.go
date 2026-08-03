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

	if len(got) != 2 {
		t.Fatalf("emitted %d events, want assistant and result", len(got))
	}
	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(got[0], &assistant); err != nil {
		t.Fatalf("decode assistant event: %v", err)
	}
	if assistant.Type != "assistant" || len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "Hello, Kate" {
		t.Fatalf("assistant event = %s", got[0])
	}
	var result struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(got[1], &result); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if result.Type != "result" || result.SessionID != "codex-thread" {
		t.Fatalf("result event = %s", got[1])
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
