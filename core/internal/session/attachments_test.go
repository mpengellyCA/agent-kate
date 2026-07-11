package session

import (
	"path/filepath"
	"testing"
)

func TestAttachmentStoreAppendLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	s := NewAttachmentStore(dir)

	// A fresh thread has no turns.
	turns, err := s.Load("t1")
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("expected no turns, got %d", len(turns))
	}

	// Append two turns; order and metadata must round-trip.
	if err := s.Append("t1", AttachmentTurn{
		Text: "look at this",
		Attachments: []AttachmentMeta{
			{Name: "a.png", Kind: "image", Path: "/tmp/a.png", MediaType: "image/png"},
		},
	}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := s.Append("t1", AttachmentTurn{
		Text: "and this",
		Attachments: []AttachmentMeta{
			{Name: "b.txt", Kind: "text", Path: "/tmp/b.txt", Outside: true},
		},
	}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	turns, err = s.Load("t1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}
	if turns[0].Text != "look at this" || turns[0].Attachments[0].Name != "a.png" ||
		turns[0].Attachments[0].MediaType != "image/png" {
		t.Errorf("turn 0 mismatch: %+v", turns[0])
	}
	if turns[1].Attachments[0].Kind != "text" || !turns[1].Attachments[0].Outside {
		t.Errorf("turn 1 mismatch: %+v", turns[1])
	}
}

func TestAttachmentStoreSkipsEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	s := NewAttachmentStore(dir)

	// A turn with no attachments must not create a sidecar (the transcript already
	// carries the plain user text; the sidecar exists only to recover chips).
	if err := s.Append("t1", AttachmentTurn{Text: "no files here"}); err != nil {
		t.Fatalf("append empty: %v", err)
	}
	turns, err := s.Load("t1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("empty turn must not be recorded, got %d", len(turns))
	}

	// An empty thread id is a no-op too.
	if err := s.Append("", AttachmentTurn{Attachments: []AttachmentMeta{{Name: "x"}}}); err != nil {
		t.Fatalf("append empty id: %v", err)
	}
}

func TestAttachmentStoreRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "attachments")
	s := NewAttachmentStore(dir)
	if err := s.Append("t1", AttachmentTurn{
		Attachments: []AttachmentMeta{{Name: "a.png", Kind: "image"}},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Remove("t1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	turns, _ := s.Load("t1")
	if len(turns) != 0 {
		t.Fatalf("expected sidecar gone, got %d turns", len(turns))
	}
	// Removing a missing sidecar is not an error.
	if err := s.Remove("nope"); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}
