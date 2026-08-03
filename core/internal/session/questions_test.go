package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestQuestionHistoryReplaysAnsweredAndDismissedEvents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "questions")
	s := NewQuestionStore(dir)
	input := json.RawMessage(`{"questions":[{"question":"Tabs or spaces?"}]}`)
	answer := json.RawMessage(`{"answers":{"Tabs or spaces?":"Spaces"}}`)
	if err := s.Append("thread-1", QuestionTurn{Input: input, Answer: answer, Answered: true}); err != nil {
		t.Fatalf("Append answered: %v", err)
	}
	if err := s.Append("thread-1", QuestionTurn{Input: input, Answered: false}); err != nil {
		t.Fatalf("Append dismissed: %v", err)
	}
	events, err := s.Events("thread-1")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	var first, second struct {
		Type     string          `json:"type"`
		Answered bool            `json:"answered"`
		Answer   json.RawMessage `json:"answer"`
	}
	if err := json.Unmarshal(events[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[1], &second); err != nil {
		t.Fatal(err)
	}
	if first.Type != "_question" || !first.Answered || !json.Valid(first.Answer) {
		t.Fatalf("answered event = %s", events[0])
	}
	if second.Type != "_question" || second.Answered {
		t.Fatalf("dismissed event = %s", events[1])
	}
	if info, err := os.Stat(filepath.Join(dir, "thread-1.json")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("history mode = %o, want 600", info.Mode().Perm())
	}
}

func TestQuestionHistoryRejectsMalformedInput(t *testing.T) {
	s := NewQuestionStore(t.TempDir())
	if err := s.Append("thread-1", QuestionTurn{Input: json.RawMessage(`not-json`), Answered: true}); err != nil {
		t.Fatalf("Append malformed = %v, want harmless rejection", err)
	}
	events, err := s.Events("thread-1")
	if err != nil || len(events) != 0 {
		t.Fatalf("malformed input persisted: events=%q err=%v", events, err)
	}
}
