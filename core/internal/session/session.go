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

	"agentkate/internal/fsperm"
	"agentkate/internal/harness"
	"agentkate/internal/worktree"
)

// Thread status values.
const (
	StatusRunning  = "running"  // a claude process is live for this thread
	StatusDormant  = "dormant"  // persisted but not running; resumable
	StatusArchived = "archived" // moved out of the live roster; reversible
)

// Orchestration roles for Record.Role (empty = ordinary human-launched thread).
const (
	RoleController = "controller" // launches workers via the Cooperation bridge (or was born one: mode.apply)
	RoleWorker     = "worker"     // launched by another thread (ParentThreadID)
)

// Agent backends. BackendClaude is empty so records written before the field
// existed — all Claude threads — decode correctly.
const (
	BackendClaude = ""     // Claude Code (the `claude` CLI); the default
	BackendKimi   = "kimi" // Kimi Code (the `kimi acp` CLI)
)

// Record is the persisted metadata for one agent thread — enough to resume it.
type Record struct {
	ThreadID       string            `json:"threadId"`
	SessionID      string            `json:"sessionId"`         // agent session, for resume (Claude Code or kimi)
	Project        string            `json:"project"`           // workspace path
	Backend        string            `json:"backend,omitempty"` // "" = Claude Code, "kimi" = Kimi Code; set once at start
	Worktree       worktree.Worktree `json:"worktree"`
	PermissionMode string            `json:"permissionMode"`
	Effort         string            `json:"effort"`         // claude --effort level; "" = Claude Code default
	Model          string            `json:"model"`          // claude --model id; "" = Claude Code default
	Title          string            `json:"title"`          // short summary of the opening prompt
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

	// Orchestration linkage (plan 16). A worker launched by another agent via
	// the Cooperation bridge's launch_agent carries its launcher's thread id in
	// ParentThreadID; Role marks the thread's place in that tree ("controller"
	// launched workers, "worker" was launched by one, "" is an ordinary
	// human-launched thread). Both empty outside orchestration.
	ParentThreadID string `json:"parentThreadId,omitempty"`
	Role           string `json:"role,omitempty"`

	// Persona (plan 16 P3), stored as APPLIED at launch — never as requested.
	// A resume, promote or fork re-passes exactly what the harness confirmed
	// it took, so replay stays reality: a harness that applied nothing (kimi)
	// persists nothing and keeps reporting nothing. A profile the harness took
	// with per-field losses is stored as requested, so the resume re-runs the
	// identical translation and lands in the identical place. Both absent on
	// records written before P3, which then resume exactly as they did before.
	SystemPrompt string                 `json:"systemPrompt,omitempty"`
	Agents       []harness.AgentProfile `json:"agents,omitempty"`

	// Env is the per-thread process-environment overlay this thread was
	// launched with (plan 16 P6). Persisted as REQUESTED, not as applied:
	// unlike the persona channels there is nothing to negotiate — the spawn
	// either sets a variable or it does not. It has to survive, because it is
	// what points a CLI at its per-thread state (KIMI_CODE_HOME): a resume
	// without it would look for the session in a different home. Absent on
	// records written before P6, which then resume exactly as they did before.
	Env map[string]string `json:"env,omitempty"`

	// The P6 launch-option sweep, persisted AS REQUESTED (there is no
	// negotiated value to record — a harness either expressed the list or
	// reported it unapplied, and a resume re-runs that same translation). They
	// must survive: a resume that forgot DisallowedTools would hand the thread
	// back a tool the human took away.
	FallbackModels  []string `json:"fallbackModels,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	AddDirs         []string `json:"addDirs,omitempty"`

	// The control-channel sweep, persisted for the same reason: a resume that
	// forgot StrictMCPConfig would hand the thread back the global MCP servers
	// the human isolated it from, and one that forgot MaxBudgetUSD would drop
	// the spend ceiling.
	StrictMCPConfig bool    `json:"strictMcpConfig,omitempty"`
	MaxBudgetUSD    float64 `json:"maxBudgetUsd,omitempty"`

	// CoworkEnabled opts this thread into the KDE Plasma Cowork MCP server
	// (desktop see/control). Off by default; only the UI may flip it
	// (cowork.setEnabled). See docs/plans/08-kde-cowork/.
	CoworkEnabled bool `json:"coworkEnabled,omitempty"`

	// Third-party API provider routing (non-secret snapshot). When
	// ProviderBaseURL is set, this thread ran the `claude` harness against a
	// third-party Anthropic-compatible endpoint (Fireworks, OpenRouter, …)
	// rather than Anthropic's own. The API token is DELIBERATELY not stored: it
	// is re-resolved at resume from ProviderEnvVar, or re-supplied by the UI.
	// Empty ProviderBaseURL means Claude direct. See docs/plans/11-third-party-providers.md.
	ProviderID      string            `json:"providerId,omitempty"`
	ProviderName    string            `json:"providerName,omitempty"`
	ProviderBaseURL string            `json:"providerBaseUrl,omitempty"`
	ProviderEnvVar  string            `json:"providerEnvVar,omitempty"`
	ProviderModels  map[string]string `json:"providerModels,omitempty"`
}

// Store is the on-disk set of thread records, mirrored in memory.
type Store struct {
	path string
	mu   sync.Mutex
	recs map[string]Record // threadID -> Record

	// archMu serializes all access to threads-archive.json and guards
	// archCache. It exists so Archive/Restore/ListArchived can do their
	// whole-file archive I/O WITHOUT holding s.mu (F62) — s.mu is on every
	// hot path (the relay's throttled sessions.Update lands on each turn
	// result) and must never wait on a 4 MB read-modify-write. Lock order:
	// archMu may be taken first and held while s.mu is acquired, never the
	// reverse.
	archMu    sync.Mutex
	archCache []ArchiveRecord // resolved-Env mirror of the archive file; nil = not loaded
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
//
// Opening also MIGRATES permissions (see harden): every build before this one
// created the data directory 0755 and its files 0644, so the records a user
// already has are the ones that need tightening most.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path, recs: make(map[string]Record)}
	if err := s.harden(); err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// harden tightens the thread store and the data directory that holds it.
//
// The directory is Agent Kate's data root — the thread store sits at its top —
// so tightening it to 0700 also puts every sibling store behind an
// unlistable directory: summaries, attachment sidecars, the kimi event logs,
// modes.json. Each of those tightens its own files as well; this is the
// umbrella that holds even if one of them is missed.
//
// FAIL CLOSED: an error here fails the store open (and so the daemon start)
// rather than proceeding with a store we could not confirm is private. The
// only realistic causes are a path we do not own or a store dir replaced by a
// symlink, both of which deserve a loud stop.
func (s *Store) harden() error {
	dir := filepath.Dir(s.path)
	tightened, err := fsperm.HardenDir(dir)
	if err != nil {
		return fmt.Errorf("thread store: %w", err)
	}
	n := 0
	if tightened {
		n++
	}
	for _, p := range []string{s.path, s.path + ".tmp", s.archivePath(), s.archivePath() + ".tmp"} {
		t, err := fsperm.HardenFile(p)
		if err != nil {
			return fmt.Errorf("thread store: %w", err)
		}
		if t {
			n++
		}
	}
	fsperm.LogMigration(dir, n)
	return nil
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
		// Credential-shaped Env values are never on disk (see flush); re-resolve
		// them from the environment akcore itself runs in, exactly as the
		// provider token is re-resolved from ProviderEnvVar at launch.
		r.Env = resolveEnvFromProcess(r.Env)
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
		// The single choke point where records become bytes on disk: strip
		// credential-shaped Env values here so no caller can forget to. The
		// in-memory record keeps the real value for this process's own
		// launches; only the file gets the marker.
		r.Env = redactEnvForPersist(r.Env)
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })

	b, err := json.MarshalIndent(struct {
		Threads []Record `json:"threads"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := fsperm.MkdirAll(filepath.Dir(s.path)); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := fsperm.WriteFile(tmp, b); err != nil {
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
//
// The returned records carry a REDACTED Env: credential-shaped entries are the
// EnvNotStored marker, exactly as on disk. List is the roster accessor — it
// feeds the UI (session.listThreads hands its result straight over the socket),
// the shutdown sweep, the cleanup candidate scan and the worker-cap count, and
// not one of those needs a credential. Get is the operational accessor and is
// NOT redacted; every launch path goes through it (agents.go start/resume/
// clone all read rec via Get and then session.LaunchEnv).
//
// The redaction lives here rather than in the handler on purpose: the disk
// store already redacts at its own boundary (flush/writeArchive), and a
// boundary that is enforced by every caller remembering to call a helper is not
// a boundary. Adding a new List caller cannot leak a credential by omission.
func (s *Store) List(project string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		if project == "" || r.Project == project {
			r.Env = RedactEnvForWire(r.Env)
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

// loadArchive returns the archive list, taking archMu itself. Convenience for
// callers outside the Archive/Restore critical sections (and tests).
func (s *Store) loadArchive() ([]ArchiveRecord, error) {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	return s.loadArchiveLocked()
}

// loadArchiveLocked serves the archive list from archCache, reading the file
// only on the first call. A missing file is an empty archive, not an error.
// Caller holds s.archMu — never s.mu — and must treat the returned slice as
// read-only (build a new slice to change the archive).
func (s *Store) loadArchiveLocked() ([]ArchiveRecord, error) {
	if s.archCache != nil {
		return s.archCache, nil
	}
	b, err := os.ReadFile(s.archivePath())
	if err != nil {
		if os.IsNotExist(err) {
			s.archCache = []ArchiveRecord{}
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
	for i := range file.Threads {
		// Same re-resolution as the live store: a restored thread relaunches
		// with the credential the environment supplies now, never one the
		// archive kept in cleartext.
		file.Threads[i].Env = resolveEnvFromProcess(file.Threads[i].Env)
	}
	if file.Threads == nil {
		file.Threads = []ArchiveRecord{}
	}
	s.archCache = file.Threads
	return file.Threads, nil
}

// Archive retention (audit F10). Nothing ever pruned threads-archive.json:
// every stop-and-close and every cleanup appended a full Record, forever, in a
// file the UI reads whole on every cleanup.listArchived. Both caps keep the
// NEWEST entries — the archive exists so a recently closed thread can be
// restored, and a two-year-old one is not what anyone reaches for.
//
// The byte cap is the one that actually binds: a Record carries a SystemPrompt
// and an Env overlay, so record count alone is a poor proxy for file size.
const (
	maxArchiveRecords = 500
	maxArchiveBytes   = 4 << 20
)

// writeArchive atomically writes the archive list, taking archMu itself.
// Convenience for callers outside the Archive/Restore critical sections (and
// tests).
func (s *Store) writeArchive(list []ArchiveRecord) error {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	return s.writeArchiveLocked(list)
}

// writeArchiveLocked atomically writes the archive list to disk, newest-first
// and bounded by the retention caps above, and mirrors exactly what survived
// into archCache (with RESOLVED Env — the cache is the in-memory view, the
// redaction below is for the file only). Caller holds s.archMu, never s.mu.
func (s *Store) writeArchiveLocked(list []ArchiveRecord) error {
	sort.Slice(list, func(i, j int) bool { return list[i].ArchivedAt.After(list[j].ArchivedAt) })
	if len(list) > maxArchiveRecords {
		list = list[:maxArchiveRecords]
	}
	// Redact on the way out, like flush: an archive is FOREVER — a credential
	// that leaked into an env overlay would otherwise outlive the thread that
	// carried it, in a file nobody ever looks at again.
	out := make([]ArchiveRecord, 0, len(list))
	for _, ar := range list {
		ar.Env = redactEnvForPersist(ar.Env)
		out = append(out, ar)
	}
	b, kept, err := marshalArchiveWithin(out, maxArchiveBytes)
	if err != nil {
		return err
	}
	path := s.archivePath()
	if err := fsperm.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	if archiveIOTestHook != nil {
		archiveIOTestHook()
	}
	tmp := path + ".tmp"
	if err := fsperm.WriteFile(tmp, b); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	s.archCache = list[:kept]
	return nil
}

// archiveIOTestHook, when non-nil, runs inside the archive-file critical
// section — s.archMu held, s.mu NOT held. Test seam for F62: a test parks the
// archive write here and proves the store stays responsive meanwhile.
var archiveIOTestHook func()

// marshalArchiveWithin encodes the archive, dropping the OLDEST entries (the
// tail — the list arrives newest-first) until the encoding fits the byte cap.
// It reports how many entries survived so the caller's cache can mirror the
// file exactly.
//
// Iterative rather than estimated: records vary by orders of magnitude in size
// (a bare thread vs one carrying a long system prompt), so a per-record average
// would either over- or under-shoot. Each round drops a tenth, so it converges
// in a handful of passes over a list already capped at maxArchiveRecords. The
// newest entry is never dropped: an archive that cannot hold even one record
// would make Archive lose the very thread it was asked to preserve.
func marshalArchiveWithin(list []ArchiveRecord, maxBytes int) (b []byte, kept int, err error) {
	for {
		b, err := json.MarshalIndent(struct {
			Threads []ArchiveRecord `json:"threads"`
		}{list}, "", "  ")
		if err != nil {
			return nil, 0, err
		}
		if len(b) <= maxBytes || len(list) <= 1 {
			return b, len(list), nil
		}
		drop := len(list) / 10
		if drop < 1 {
			drop = 1
		}
		list = list[:len(list)-drop]
	}
}

// Archive moves a thread out of the live store into the archive file. It is the
// reversible alternative to Remove: the full Record is preserved and the Claude
// transcript on disk is intentionally NOT deleted, so the conversation stays
// recoverable.
//
// SAFETY: the archive file is written FIRST and only then is the live record
// dropped. If the archive write fails, the live record is left intact so the
// caller (the cleanup handler) never loses the record before git removal.
//
// LOCKING (F62): s.mu is held only for the record snapshot and the final
// delete+flush — the whole-file archive read-modify-write happens under
// archMu alone, so hot-path store callers (the relay's per-turn
// sessions.Update) never stall behind it. archMu spans through the delete so
// a concurrent Restore of the same thread cannot interleave between the
// archive write and the live removal.
func (s *Store) Archive(threadID, reason string) error {
	s.mu.Lock()
	rec, ok := s.recs[threadID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown thread %s", threadID)
	}

	s.archMu.Lock()
	defer s.archMu.Unlock()
	existing, err := s.loadArchiveLocked()
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
	if err := s.writeArchiveLocked(out); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, threadID)
	return s.flush()
}

// ListArchived returns archived records newest-first.
//
// Env is REDACTED here for the same reason as List: cleanup.listArchived hands
// the result straight over the socket. loadArchive re-resolves EnvNotStored
// markers from akcore's OWN environment so a Restore relaunches with a live
// credential — which means an un-redacted ListArchived would hand every socket
// peer the daemon's credentials, values that were never even on disk. Restore
// and Archive use loadArchive directly and keep the resolved values.
func (s *Store) ListArchived() []ArchiveRecord {
	s.archMu.Lock()
	list, err := s.loadArchiveLocked()
	s.archMu.Unlock()
	if err != nil {
		return nil
	}
	// Copy before redacting: list may be the cache itself, and the cache keeps
	// the resolved values Restore needs.
	out := make([]ArchiveRecord, len(list))
	copy(out, list)
	for i := range out {
		out[i].Env = RedactEnvForWire(out[i].Env)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchivedAt.After(out[j].ArchivedAt) })
	return out
}

// Restore best-effort moves an archived record back into the live store as a
// dormant, non-isolated thread (its worktree is gone after cleanup, so it can
// only be resumed in the workspace). The archive entry is removed once the live
// record is written.
// LOCKING (F62): like Archive, the archive-file work runs under archMu alone;
// s.mu is taken only around the live-map insert+flush (archMu → s.mu is the
// one permitted nesting).
func (s *Store) Restore(threadID string) error {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	list, err := s.loadArchiveLocked()
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
	if err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.recs[rec.ThreadID] = rec
		return s.flush()
	}(); err != nil {
		return err
	}
	return s.writeArchiveLocked(remaining)
}

// NewID returns a fresh random UUID (v4) — a valid `claude --session-id`.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
