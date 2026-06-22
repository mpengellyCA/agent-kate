// Package session persists agent-thread metadata so a thread can be resumed
// later — after the process stops, or after Agent Kate itself restarts.
//
// Each Agent Kate thread maps to one Claude Code session (a UUID). Claude Code
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
	StatusRunning  = "running"  // a claude process is live for this thread
	StatusDormant  = "dormant"  // persisted but not running; resumable
	StatusArchived = "archived" // moved out of the live roster; reversible
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
	Tags           []string          `json:"tags,omitempty"` // user/auto labels for roster organization
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

	// CoworkEnabled opts this thread into the KDE Plasma Cowork MCP server
	// (desktop see/control). Off by default; only the UI may flip it
	// (cowork.setEnabled). See docs/plans/08-kde-cowork/.
	CoworkEnabled bool `json:"coworkEnabled,omitempty"`
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
	// Backfill the project-scoped Worktree.Number for records from before
	// the field existed. Assign sequentially in Created order so older
	// agents keep the lower numbers a user would expect.
	if s.backfillNumbers() {
		_ = s.flush()
	}
	return nil
}

// backfillNumbers stamps Worktree.Number on every record that lacks one,
// numbering each project's records 1..N in Created order. Returns true if
// anything changed. Caller holds s.mu (or is in single-threaded load()).
func (s *Store) backfillNumbers() bool {
	byProject := make(map[string][]Record)
	for _, r := range s.recs {
		byProject[r.Project] = append(byProject[r.Project], r)
	}
	changed := false
	for _, list := range byProject {
		sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
		used := make(map[int]bool)
		for _, r := range list {
			if r.Worktree.Number > 0 {
				used[r.Worktree.Number] = true
			}
		}
		next := 1
		for _, r := range list {
			if r.Worktree.Number > 0 {
				continue
			}
			for used[next] {
				next++
			}
			r.Worktree.Number = next
			used[next] = true
			s.recs[r.ThreadID] = r
			changed = true
		}
	}
	return changed
}

// NextNumber returns the next project-scoped agent number to assign. Numbers
// monotonically increase per project; gaps left by removed records are not
// re-used (predictable history beats compactness).
func (s *Store) NextNumber(project string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	max := 0
	for _, r := range s.recs {
		if r.Project != project {
			continue
		}
		if r.Worktree.Number > max {
			max = r.Worktree.Number
		}
	}
	return max + 1
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

// Update mutates an existing record in place and persists, advancing Updated to
// now — use it for changes that count as activity. It is a no-op if no record
// has that thread id.
func (s *Store) Update(threadID string, fn func(*Record)) error {
	return s.update(threadID, fn, true)
}

// UpdateQuiet mutates and persists like Update but leaves Updated untouched. Use
// it for background/lifecycle bookkeeping that isn't user activity — status
// transitions on exit/resume, worktree promotion, compaction summaries — so the
// Updated timestamp stays a faithful "last active" clock rather than being bumped
// every launch and shutdown.
func (s *Store) UpdateQuiet(threadID string, fn func(*Record)) error {
	return s.update(threadID, fn, false)
}

func (s *Store) update(threadID string, fn func(*Record), bump bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[threadID]
	if !ok {
		return nil
	}
	fn(&rec)
	if bump {
		rec.Updated = time.Now()
	}
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

// ArchiveRecord is a Record that has been moved out of the live roster into the
// archive file. It keeps the entire original Record (so a Restore is lossless)
// plus when and why it was archived.
type ArchiveRecord struct {
	Record
	ArchivedAt time.Time `json:"archivedAt"`
	Reason     string    `json:"reason"`
}

// archivePath returns the sibling archive file beside the live store.
func (s *Store) archivePath() string {
	dir := filepath.Dir(s.path)
	return filepath.Join(dir, "threads-archive.json")
}

// loadArchive reads the archive file. A missing file is an empty archive, not
// an error. The caller need not hold s.mu — the archive file is independent of
// the in-memory live map.
func (s *Store) loadArchive() ([]ArchiveRecord, error) {
	b, err := os.ReadFile(s.archivePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var file struct {
		Threads []ArchiveRecord `json:"threads"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, fmt.Errorf("archive store %s: %w", s.archivePath(), err)
	}
	return file.Threads, nil
}

// writeArchive atomically writes the archive list to disk.
func (s *Store) writeArchive(list []ArchiveRecord) error {
	sort.Slice(list, func(i, j int) bool { return list[i].ArchivedAt.After(list[j].ArchivedAt) })
	b, err := json.MarshalIndent(struct {
		Threads []ArchiveRecord `json:"threads"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	path := s.archivePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Archive moves a thread out of the live store into the archive file. It is the
// reversible alternative to Remove: the full Record is preserved and the Claude
// transcript on disk is intentionally NOT deleted, so the conversation stays
// recoverable.
//
// SAFETY: the archive file is written FIRST and only then is the live record
// dropped. If the archive write fails, the live record is left intact so the
// caller (the cleanup handler) never loses the record before git removal.
func (s *Store) Archive(threadID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[threadID]
	if !ok {
		return fmt.Errorf("unknown thread %s", threadID)
	}

	existing, err := s.loadArchive()
	if err != nil {
		return err
	}
	// Replace any prior archive entry for the same thread (re-archive after a
	// restore) rather than duplicating it.
	out := make([]ArchiveRecord, 0, len(existing)+1)
	for _, a := range existing {
		if a.ThreadID == threadID {
			continue
		}
		out = append(out, a)
	}
	rec.Status = StatusArchived
	out = append(out, ArchiveRecord{
		Record:     rec,
		ArchivedAt: time.Now().UTC(),
		Reason:     reason,
	})
	// Archive file written BEFORE the live record is removed.
	if err := s.writeArchive(out); err != nil {
		return err
	}
	delete(s.recs, threadID)
	return s.flush()
}

// ListArchived returns archived records newest-first.
func (s *Store) ListArchived() []ArchiveRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadArchive()
	if err != nil {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ArchivedAt.After(list[j].ArchivedAt) })
	if list == nil {
		return []ArchiveRecord{}
	}
	return list
}

// Restore best-effort moves an archived record back into the live store as a
// dormant, non-isolated thread (its worktree is gone after cleanup, so it can
// only be resumed in the workspace). The archive entry is removed once the live
// record is written.
func (s *Store) Restore(threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.loadArchive()
	if err != nil {
		return err
	}
	var found *ArchiveRecord
	remaining := make([]ArchiveRecord, 0, len(list))
	for i := range list {
		if list[i].ThreadID == threadID && found == nil {
			a := list[i]
			found = &a
			continue
		}
		remaining = append(remaining, list[i])
	}
	if found == nil {
		return fmt.Errorf("no archived thread %s", threadID)
	}
	rec := found.Record
	rec.Status = StatusDormant
	// The dedicated worktree was removed during cleanup; the thread can only
	// come back in the workspace itself.
	rec.Worktree.Isolated = false
	rec.Worktree.Path = rec.Project
	rec.Worktree.Branch = ""
	rec.Updated = time.Now()
	s.recs[rec.ThreadID] = rec
	if err := s.flush(); err != nil {
		return err
	}
	return s.writeArchive(remaining)
}

// NewID returns a fresh random UUID (v4) — a valid `claude --session-id`.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
