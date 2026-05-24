// Package session persists agent-thread metadata so a thread can be resumed
// later — after the process stops, or after AgentKate itself restarts.
//
// Each AgentKate thread maps to one Claude Code session (a UUID). Claude Code
// stores the conversation transcript on disk and can replay it with
// `claude --resume <session-id>`; this package records just enough beside it
// — the session id, worktree and project — to spawn that resume.
package session

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"agentkate/internal/worktree"
)

// Thread status values.
const (
	StatusRunning = "running" // a claude process is live for this thread
	StatusDormant = "dormant" // persisted but not running; resumable
)

// Record is the persisted metadata for one agent thread — enough to resume it.
type Record struct {
	ThreadID       string            `json:"threadId"`
	SessionID      string            `json:"sessionId"` // Claude Code session, for --resume
	Project        string            `json:"project"`   // workspace path
	Worktree       worktree.Worktree `json:"worktree"`
	PermissionMode string            `json:"permissionMode"`
	Effort         string            `json:"effort"` // claude --effort level; "" = Claude Code default
	Model          string            `json:"model"`  // claude --model id; "" = Claude Code default
	Title          string            `json:"title"`  // short summary of the opening prompt
	Created        time.Time         `json:"created"`
	Updated        time.Time         `json:"updated"`
	Status         string            `json:"status"`

	// Compaction policy and state. The strategy chooses when and how the
	// transcript is condensed so a resume does not pay the full re-cache
	// cost on a long thread; the strip flag is a per-thread sticky that
	// asks LLM-based compactors to first apply local lossless passes.
	// SummaryUpdatedAt is zero when no summary has been produced yet; if
	// LastTurnAt is after it, the summary is stale and a fresh compact is
	// due. Empty CompactStrategy is treated as the compact-package default.
	CompactStrategy  string    `json:"compactStrategy,omitempty"`
	CompactStrip     bool      `json:"compactStrip,omitempty"`
	SummaryUpdatedAt time.Time `json:"summaryUpdatedAt,omitempty"`
	LastTurnAt       time.Time `json:"lastTurnAt,omitempty"`
}

// Store is the on-disk set of thread records, mirrored in memory.
type Store struct {
	path string
	mu   sync.Mutex
	recs map[string]Record // threadID -> Record
}

// DefaultPath is where the thread store lives unless overridden.
func DefaultPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", "threads.json")
}

// NewStore opens (or starts) the store at path. Any record left as "running"
// from a previous run is reset to "dormant" — a freshly started core has no
// live threads.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, recs: make(map[string]Record)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var file struct {
		Threads []Record `json:"threads"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return fmt.Errorf("thread store %s: %w", s.path, err)
	}
	for _, r := range file.Threads {
		r.Status = StatusDormant // nothing is running in a fresh process
		s.recs[r.ThreadID] = r
	}
	return nil
}

// flush writes the store to disk atomically. The caller holds s.mu.
func (s *Store) flush() error {
	list := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })

	b, err := json.MarshalIndent(struct {
		Threads []Record `json:"threads"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Put inserts or replaces a record and persists the store.
func (s *Store) Put(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Updated.IsZero() {
		rec.Updated = time.Now()
	}
	s.recs[rec.ThreadID] = rec
	return s.flush()
}

// Update mutates an existing record in place and persists. It is a no-op if no
// record has that thread id.
func (s *Store) Update(threadID string, fn func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[threadID]
	if !ok {
		return nil
	}
	fn(&rec)
	rec.Updated = time.Now()
	s.recs[threadID] = rec
	return s.flush()
}

// Get returns the record for a thread id.
func (s *Store) Get(threadID string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[threadID]
	return r, ok
}

// GetBySession returns the record that owns a Claude Code session id, so the
// same session is never attached twice.
func (s *Store) GetBySession(sessionID string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.SessionID == sessionID {
			return r, true
		}
	}
	return Record{}, false
}

// Remove deletes a record and persists the store.
func (s *Store) Remove(threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, threadID)
	return s.flush()
}

// List returns records newest-first. When project is non-empty only that
// project's threads are returned.
func (s *Store) List(project string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		if project == "" || r.Project == project {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// NewID returns a fresh random UUID (v4) — a valid `claude --session-id`.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
