package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/permission"
	"agentkate/internal/remote"
	"agentkate/internal/session"
)

func TestProjectRemoteTranscriptRedactsToolAndAttachmentBodies(t *testing.T) {
	secret := "do-not-leak-remote-secret"
	raw := []json.RawMessage{
		json.RawMessage(fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"text","text":%q},{"type":"text","text":%q}]}}`,
			"hello from the human", "Attached file `x.txt`:\n```\n"+secret+"\n```")),
		json.RawMessage(fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q},{"type":"tool_use","name":"Bash","input":{"command":%q}}]}}`,
			"I will check it.", "echo "+secret)),
		json.RawMessage(fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","content":%q}]}}`, secret)),
		json.RawMessage(fmt.Sprintf(`{"type":"_lifecycle","phase":"exited","detail":%q}`, secret)),
	}

	got, truncated := projectRemoteTranscript(raw, 20, 4096)
	if truncated {
		t.Fatal("small safe projection was unexpectedly truncated")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), `"command":`) {
		t.Fatalf("remote projection leaked raw tool or attachment data: %s", encoded)
	}
	if len(got) != 4 {
		t.Fatalf("events = %#v, want human, assistant, tool, lifecycle", got)
	}
	if got[0].Kind != "user" || got[0].Text != "hello from the human" {
		t.Fatalf("user projection = %#v", got[0])
	}
	if got[1].Kind != "assistant" || got[1].Text != "I will check it." {
		t.Fatalf("assistant projection = %#v", got[1])
	}
	if got[2].Kind != "tool" || got[2].ToolName != "Bash" || got[2].Summary != "Approve a shell command" {
		t.Fatalf("tool projection = %#v", got[2])
	}
	if got[3].Kind != "lifecycle" || got[3].Text != "Agent exited" {
		t.Fatalf("lifecycle projection = %#v", got[3])
	}
}

func TestProjectRemoteTranscriptCapsOnlyTheRemoteDTO(t *testing.T) {
	long := strings.Repeat("x", 1024)
	raw := []json.RawMessage{json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + long + `"}]}}`)}
	got, truncated := projectRemoteTranscript(raw, 1, 64)
	if !truncated || len(got) != 1 || len(got[0].Text) > 64 {
		t.Fatalf("cap result truncated=%v events=%#v", truncated, got)
	}
}

func TestProjectRemoteTranscriptLabelsAttachmentOnlyUserTurn(t *testing.T) {
	secret := "attachment-body-must-not-leave-the-desktop"
	raw := []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"text","text":%q}]}}`,
		"Attached file `private.md`:\n```\n"+secret+"\n```"))}
	got, truncated := projectRemoteTranscript(raw, 20, 4096)
	if truncated || len(got) != 1 || got[0].Kind != "user" || got[0].Text != "Attached 1 file(s)" {
		t.Fatalf("attachment-only projection = %#v, truncated=%v", got, truncated)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "private.md") {
		t.Fatalf("attachment-only projection leaked file details: %s", encoded)
	}
}

func TestRemoteBackendUsesHarnessDescriptorAndLinkage(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := session.Record{
		AgentRef:          harness.AgentRef{ThreadID: "t-1", HarnessID: "fake"},
		EffectiveSettings: harness.AgentSettings{Model: "linked-model"},
		Project:           "/home/person/projects/agent-kate",
		Title:             "linked title",
		Status:            session.StatusRunning,
		Updated:           time.Now(),
		// This stale legacy projection must not choose the remote adapter's
		// behaviour or descriptor label.
		Backend: "not-a-real-backend",
	}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	h := &fakeHarness{}
	registry := harness.NewRegistry("fake")
	registry.Register(h)
	backend := remoteBackend{d: handlerDeps{sessions: store, harnesses: registry, turns: agent.NewTurnTracker()}}
	rows, err := backend.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].Backend != "fake" || rows[0].EngineName != "Fake Engine" || rows[0].Model != "linked-model" {
		t.Fatalf("row did not use descriptor/linkage: %#v", rows[0])
	}
	if rows[0].Project != "agent-kate" {
		t.Fatalf("absolute project path leaked: %#v", rows[0])
	}
}

func TestRemotePermissionDetailAndResponseExcludeLocalBrokerEntries(t *testing.T) {
	b := permission.New()
	backend := remoteBackend{d: handlerDeps{broker: b}}
	localID, _ := b.OpenLocal()
	if _, err := backend.PermissionDetail(context.Background(), localID); !errors.Is(err, remote.ErrUnknownRequest) {
		t.Fatalf("local request detail error = %v, want unknown", err)
	}

	secret := "do-not-leak-tool-input"
	request, _ := b.OpenWithDetail("t-1", "Bash", permission.Summary("Bash"), permission.Detail{}, time.Minute)
	detail, err := backend.PermissionDetail(context.Background(), request.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if strings.Contains(string(encoded), secret) || detail.Plan != "" || len(detail.Questions) != 0 {
		t.Fatalf("ordinary permission detail leaked render data: %s", encoded)
	}
	if err := backend.RespondPermission(context.Background(), remote.Principal{DeviceID: "d-1", DeviceName: "Phone"}, remote.PermissionAnswer{RequestID: localID, Allow: true}); !errors.Is(err, remote.ErrUnknownRequest) {
		t.Fatalf("local response error = %v, want unknown", err)
	}
}
