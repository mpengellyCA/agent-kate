package compact

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "summaries"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sum := Summary{
		ThreadID:  "t-abc",
		SessionID: "deadbeef-uuid",
		Strategy:  ResumeLocal,
		Created:   time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Turns:     7,
		Body:      "## Conversation\n\n**User:** hi\n",
	}
	if err := s.Put(sum); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get("t-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for existing summary")
	}
	if got.ThreadID != sum.ThreadID || got.SessionID != sum.SessionID ||
		got.Strategy != sum.Strategy || got.Turns != sum.Turns ||
		got.Body != sum.Body {
		t.Errorf("round-trip mismatch:\nwant: %#v\ngot:  %#v", sum, *got)
	}
}

func TestStoreGetMissingIsNil(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	got, err := s.Get("t-nope")
	if err != nil {
		t.Errorf("Get on missing thread should not error, got %v", err)
	}
	if got != nil {
		t.Errorf("Get on missing thread should return nil, got %#v", *got)
	}
}

func TestStoreRemove(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Summary{ThreadID: "t-x", Body: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Remove("t-x"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := s.Remove("t-x"); err != nil {
		t.Errorf("Remove on missing should be a no-op, got %v", err)
	}
	if got, _ := s.Get("t-x"); got != nil {
		t.Errorf("Get after Remove should be nil, got %#v", *got)
	}
}

func TestStorePutRejectsEmptyThreadID(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Summary{Body: "x"}); err == nil {
		t.Error("Put with empty ThreadID should fail")
	}
}
