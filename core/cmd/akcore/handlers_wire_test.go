package main

import (
	"testing"
	"time"

	"agentkate/internal/harness"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

func TestSessionRecordWireBridgesLinkageAndRosterShapes(t *testing.T) {
	record := session.Record{
		SchemaVersion: 1,
		AgentRef: harness.AgentRef{
			ThreadID:        "thread-1",
			HarnessID:       "claude",
			NativeSessionID: "native-1",
		},
		ThreadID:  "thread-1",
		SessionID: "native-1",
		Backend:   "claude",
		Project:   "/workspace/project",
		Title:     "restore me",
		Status:    session.StatusDormant,
		Worktree: worktree.Worktree{
			Path:     "/workspace/project/.agentkate/thread-1",
			Branch:   "agentkate/thread-1",
			Isolated: true,
			Number:   7,
		},
		Created: time.Now(),
	}

	wire := sessionRecordWire(record)
	if got := wire["threadId"]; got != "thread-1" {
		t.Fatalf("flat threadId = %#v, want thread-1", got)
	}
	if got := wire["backend"]; got != "claude" {
		t.Fatalf("flat backend = %#v, want claude", got)
	}
	if got := wire["agentRef"].(map[string]any)["threadId"]; got != "thread-1" {
		t.Fatalf("nested agentRef.threadId = %#v, want thread-1", got)
	}
	if got := wire["branch"]; got != "agentkate/thread-1" {
		t.Fatalf("flat branch = %#v, want agentkate/thread-1", got)
	}
}

func TestArchiveRecordWireKeepsRestoreMetadata(t *testing.T) {
	record := session.Record{ThreadID: "thread-arch", Title: "closed"}
	wire := archiveRecordWire(session.ArchiveRecord{
		Record:     record,
		ArchivedAt: time.Unix(123, 0).UTC(),
		Reason:     "stop & close",
	})
	if wire["threadId"] != "thread-arch" {
		t.Fatalf("archive threadId = %#v, want thread-arch", wire["threadId"])
	}
	if wire["reason"] != "stop & close" {
		t.Fatalf("archive reason = %#v, want stop & close", wire["reason"])
	}
	if _, ok := wire["archivedAt"]; !ok {
		t.Fatal("archive archivedAt missing")
	}
}
