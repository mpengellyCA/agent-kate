package session

// Durable, human-facing records of AskUserQuestion interactions.  The engine
// transcripts either omit the permission bridge entirely (Kimi) or are owned
// by the CLI (Claude), so neither can reliably replay the answer the human
// selected.  This sidecar is intentionally separate from permissions: normal
// tool approvals are security prompts, not conversation history.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"agentkate/internal/fsperm"
)

// QuestionTurn is one completed question interaction.  Input and Answer are
// the protocol shapes already shown to the human, retained only for the
// UI-only transcript replay.  Answered is false for deny, cancellation,
// timeout, or an unusable answer; those outcomes must be visible as dismissed,
// never silently converted into a choice.
type QuestionTurn struct {
	Input    json.RawMessage `json:"input"`
	Answer   json.RawMessage `json:"answer,omitempty"`
	Answered bool            `json:"answered"`
}

// QuestionStore persists one ordered JSON sidecar per agent thread.
type QuestionStore struct {
	dir string
	mu  sync.Mutex
}

// DefaultQuestionDir keeps question history beside the thread store, under the
// same 0700 data root as the transcript and attachment sidecars.
func DefaultQuestionDir() string {
	return filepath.Join(filepath.Dir(DefaultPath()), "questions")
}

func NewQuestionStore(dir string) *QuestionStore {
	// Harden old installations on startup, not only after a later question is
	// answered.  The input can be private project context and is transcript
	// data, so it gets the same 0700/0600 discipline as attachments.
	_, _ = fsperm.HardenTree(dir)
	return &QuestionStore{dir: dir}
}

func (s *QuestionStore) pathFor(threadID string) string {
	return filepath.Join(s.dir, threadID+".json")
}

// Append records a completed question.  Malformed protocol payloads are not
// persisted: replay is an output boundary and must not manufacture arbitrary
// JSON for the UI to parse.
func (s *QuestionStore) Append(threadID string, turn QuestionTurn) error {
	if threadID == "" || !json.Valid(turn.Input) {
		return nil
	}
	if len(turn.Answer) != 0 && !json.Valid(turn.Answer) {
		turn.Answer = nil
		turn.Answered = false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, err := s.load(threadID)
	if err != nil {
		return err
	}
	turns = append(turns, turn)
	return s.write(threadID, turns)
}

// Events returns synthetic transcript events in ask/answer order.  These are
// appended by the UI-only agent.transcript handler after the engine transcript
// is read; they are deliberately never broadcast to another bridge.
func (s *QuestionStore) Events(threadID string) ([]json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns, err := s.load(threadID)
	if err != nil {
		return nil, err
	}
	events := make([]json.RawMessage, 0, len(turns))
	for _, turn := range turns {
		b, err := json.Marshal(map[string]any{
			"type":     "_question",
			"input":    json.RawMessage(turn.Input),
			"answer":   json.RawMessage(turn.Answer),
			"answered": turn.Answered,
		})
		if err == nil {
			events = append(events, b)
		}
	}
	return events, nil
}

// Remove drops a thread's history when its thread and worktree are permanently
// destroyed.  A missing sidecar is already the desired state.
func (s *QuestionStore) Remove(threadID string) error {
	if threadID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.pathFor(threadID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *QuestionStore) load(threadID string) ([]QuestionTurn, error) {
	if threadID == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.pathFor(threadID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var turns []QuestionTurn
	if err := json.Unmarshal(b, &turns); err != nil {
		return nil, err
	}
	return turns, nil
}

func (s *QuestionStore) write(threadID string, turns []QuestionTurn) error {
	b, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return err
	}
	if err := fsperm.MkdirAll(s.dir); err != nil {
		return err
	}
	tmp := s.pathFor(threadID) + ".tmp"
	if err := fsperm.WriteFile(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, s.pathFor(threadID))
}
