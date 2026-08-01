package compact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentkate/internal/fsperm"
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
	Turns     int       `json:"turns"` // number of (user, assistant) turns in the source
	Body      string    `json:"body"`  // markdown summary text — fed to the next session
}

// Store persists summaries to disk, one JSON file per thread.
type Store struct {
	dir string
}

// NewStore opens (or creates) a summary store rooted at dir.
//
// A summary is a condensed copy of a whole conversation — everything the model
// was shown, distilled. It gets the same 0700/0600 discipline as the transcript
// it summarises, and opening MIGRATES a directory an earlier build created
// world-readable.
//
// FAIL CLOSED: a migration that cannot complete fails the open. The store is
// created at daemon start beside the thread store, which fails closed for the
// same reason.
func NewStore(dir string) (*Store, error) {
	if err := fsperm.MkdirAll(dir); err != nil {
		return nil, err
	}
	n, err := fsperm.HardenTree(dir)
	if err != nil {
		return nil, fmt.Errorf("summary store: %w", err)
	}
	fsperm.LogMigration(dir, n)
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
//
// The staging file gets a UNIQUE name (audit F24). It used to be the fixed
// `<thread>.json.tmp`, which made "write tmp, rename over dst" atomic only
// against a reader — not against a second writer. Two Puts for the SAME thread
// do overlap in practice: a hot exit-compaction and the cold-exit compaction
// spawned from the thread-exit lifecycle event run on different goroutines, and
// so does any UI-triggered re-summarise. Interleaved on one tmp path they
// produce a file holding the tail of one summary and the head of another, and
// the rename publishes that as the thread's summary — which is then fed to the
// next session as its whole memory of the conversation. A unique name per call
// gives each writer its own staging file; the rename stays atomic, so the
// loser's content is simply replaced rather than spliced.
//
// The unique file is removed on every error path, so a full disk or a crashed
// marshal cannot leave litter accumulating in the summary directory.
func (s *Store) Put(sum Summary) error {
	if sum.ThreadID == "" {
		return fmt.Errorf("summary has no thread id")
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	dst := s.path(sum.ThreadID)
	// CreateTemp in the store directory (not $TMPDIR) so the rename below stays
	// on one filesystem, and 0600 from birth via fsperm's discipline.
	f, err := os.CreateTemp(s.dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(fsperm.FileMode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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
