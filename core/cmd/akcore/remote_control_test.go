package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"agentkate/internal/permission"
	"agentkate/internal/remote"
)

func TestRemoteControlAttachIsCredentialSideEffectFree(t *testing.T) {
	root := t.TempDir()
	c := newRemoteControl(context.Background(), slog.Default())
	c.dataDir = root
	c.attach(handlerDeps{broker: permission.New()})
	if c.server() == nil {
		t.Fatal("remote control did not construct its unbound server")
	}
	if c.server().Running() {
		t.Fatal("attach unexpectedly opened a listener")
	}
	for _, name := range []string{"remote-devices.json", "remote-audit.jsonl", "remote-cert.pem", "remote-key.pem"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("attach created %s: %v", name, err)
		}
	}
}

func TestHumanEchoStoreBridgesReconnectThenConsumesHarnessCopy(t *testing.T) {
	var store humanEchoStore
	at := time.Now().UTC().Truncate(time.Millisecond)
	store.add("thread-1", remote.TranscriptEvent{Kind: "user", Text: "continue", At: at})

	// A reconnect before the harness has committed its JSONL user record still
	// receives the accepted, redacted user turn.
	got := store.merge("thread-1", nil)
	if len(got) != 1 || got[0].Kind != "user" || got[0].Text != "continue" {
		t.Fatalf("bridged transcript = %#v", got)
	}

	// Once the harness provides the equivalent turn, clients that already saw
	// the synthetic SSE event do not see a duplicate and the memory bridge is
	// retired.
	harnessCopy := []remote.TranscriptEvent{{Kind: "user", Text: "continue", At: at.Add(time.Second)}}
	if got := store.consumeObserved("thread-1", harnessCopy); len(got) != 0 {
		t.Fatalf("observed duplicate was published: %#v", got)
	}
	if got := store.merge("thread-1", nil); len(got) != 0 {
		t.Fatalf("consumed echo remained in store: %#v", got)
	}
}

func TestHumanEchoStoreMatchesQueuedDuplicatesOneForOne(t *testing.T) {
	var store humanEchoStore
	at := time.Now().UTC().Truncate(time.Millisecond)
	store.add("thread-1", remote.TranscriptEvent{Kind: "user", Text: "same follow-up", At: at})
	store.add("thread-1", remote.TranscriptEvent{Kind: "user", Text: "same follow-up", At: at.Add(time.Second)})

	// A queued send may not be written to the harness transcript until many
	// minutes after acceptance. It still consumes only its matching first echo.
	first := []remote.TranscriptEvent{{Kind: "user", Text: "same follow-up", At: at.Add(20 * time.Minute)}}
	if got := store.consumeObserved("thread-1", first); len(got) != 0 {
		t.Fatalf("first accepted echo was duplicated: %#v", got)
	}
	if got := store.merge("thread-1", nil); len(got) != 1 || got[0].Text != "same follow-up" {
		t.Fatalf("second identical queued echo was consumed too: %#v", got)
	}
}

func TestHumanEchoMergePreservesCanonicalOrderWithLegacyUntimestampedEvents(t *testing.T) {
	var store humanEchoStore
	at := time.Now().UTC().Truncate(time.Millisecond)
	store.add("thread-1", remote.TranscriptEvent{Kind: "user", Text: "queued follow-up", At: at})

	// Codex logs written before timestamp normalization contain assistant and
	// tool rows without At. A reload may merge one pending user echo into this
	// page; the merge must not reorder the persisted conversation around it.
	canonical := []remote.TranscriptEvent{
		{Kind: "user", Text: "opening prompt", At: at.Add(-2 * time.Minute)},
		{Kind: "assistant", Text: "first answer"},
		{Kind: "tool", ToolName: "Bash", Summary: "Approve a shell command"},
		{Kind: "assistant", Text: "second answer"},
	}
	got := store.merge("thread-1", canonical)
	if len(got) != len(canonical)+1 {
		t.Fatalf("merged transcript = %#v", got)
	}
	for i, event := range canonical {
		if !reflect.DeepEqual(got[i], event) {
			t.Fatalf("canonical event %d moved: got %#v, want %#v", i, got[i], event)
		}
	}
	if got[len(got)-1].Kind != "user" || got[len(got)-1].Text != "queued follow-up" {
		t.Fatalf("pending echo not kept at its safe tail position: %#v", got)
	}
}
