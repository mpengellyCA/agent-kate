package compact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Summary is the persisted output of one compaction pass — enough to seed a
// fresh claude session on the next resume in place of replaying the original
// transcript.
type Summary struct {
	ThreadID  string    `json:"threadId"`
	SessionID string    `json:"sessionId"` // the source session this summarises
	Strategy  Strategy  `json:"strategy"`  // which strategy produced it
	Stripped  bool      `json:"stripped"`  // strip flag was on for LLM-based compactors
	Created   time.Time `json:"created"`
	Turns     int       `json:"turns"`     // number of (user, assistant) turns in the source
	Body      string    `json:"body"`      // markdown summary text — fed to the next session
}

// Store persists summaries to disk, one JSON file per thread.
type Store struct {
	dir string
}

// NewStore opens (or creates) a summary store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// DefaultDir is where summaries live, alongside the thread store.
func DefaultDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", "summaries")
}

// Put writes a summary to disk atomically.
func (s *Store) Put(sum Summary) error {
	if sum.ThreadID == "" {
		return fmt.Errorf("summary has no thread id")
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	dst := s.path(sum.ThreadID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Get loads a summary by thread id. Returns nil with no error if none exists.
func (s *Store) Get(threadID string) (*Summary, error) {
	b, err := os.ReadFile(s.path(threadID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sum Summary
	if err := json.Unmarshal(b, &sum); err != nil {
		return nil, fmt.Errorf("summary %s: %w", s.path(threadID), err)
	}
	return &sum, nil
}

// Remove deletes a thread's summary; not finding one is not an error.
func (s *Store) Remove(threadID string) error {
	err := os.Remove(s.path(threadID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) path(threadID string) string {
	return filepath.Join(s.dir, threadID+".json")
}
