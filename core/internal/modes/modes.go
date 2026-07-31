// Package modes stores user-editable ensembles: named controller/worker
// recipes an agent-orchestrated session can be launched from (plan 16
// Feature 3).
//
// An ensemble is deliberately NOT an execution plan. It is a controller thread
// plus a roster of worker roles the controller may launch on demand through the
// Cooperation bridge's launch_agent, and a master prompt that tells it how. The
// core does no scheduling: applying an ensemble creates exactly one thread (the
// controller) and hands it the roster as its menu.
//
// The store merges built-in ensembles with the user's own on load. A user entry
// wins over a built-in of the same name, and deleting a built-in records a
// suppression rather than pretending it is gone — the built-in list is code, so
// suppression is the only way a deletion can survive an upgrade.
package modes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Participant is one member of an ensemble — the controller, or one worker
// role the controller may spawn. Every field is harness-neutral vocabulary,
// exactly as agent.start / launch_agent take it: the store never resolves or
// validates model ids, because the vocabulary belongs to the harness (and
// changes with every CLI release).
type Participant struct {
	// Role is the worker's name in the roster ("coder", "scout"); the
	// controller's is ignored. It reaches the controller as the label it picks
	// a worker by, so it is free text.
	Role    string `json:"role,omitempty"`
	Backend string `json:"backend"` // registry id ("claude", "kimi"); "" = default
	Model   string `json:"model,omitempty"`
	// PermissionMode / Effort / Isolation are the same vocabularies agent.start
	// takes; empty means "the harness's default", never a substituted value.
	PermissionMode string `json:"permissionMode,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Isolation      string `json:"isolation,omitempty"`
	// Notes is a one-line hint about what this role is for. It is rendered into
	// the master prompt's roster table, so it is the controller's only clue
	// about when to pick this worker.
	Notes string `json:"notes,omitempty"`
}

// Mode is one ensemble.
type Mode struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Controller  Participant   `json:"controller"`
	Workers     []Participant `json:"workers,omitempty"`
	// MasterPrompt is the controller's opening message, with {{ensemble_name}},
	// {{workspace}} and {{worker_roster}} placeholders. Empty = DefaultMasterPrompt.
	MasterPrompt string    `json:"masterPrompt,omitempty"`
	Updated      time.Time `json:"updated,omitempty"`

	// BuiltIn is computed on read (never persisted): true when this ensemble
	// ships with Agent Kate and the user has not overridden it. The UI uses it
	// to label the entry; deleting one suppresses it instead of erasing it.
	BuiltIn bool `json:"builtIn,omitempty"`
}

// file is the on-disk shape of modes.json.
type file struct {
	Modes []Mode `json:"modes"`
	// Suppressed names built-in ensembles the user deleted. Without this a
	// deleted built-in would return on the next launch.
	Suppressed []string `json:"suppressed,omitempty"`
}

// Store is the on-disk ensemble set, mirrored in memory. Safe for concurrent
// use; every mutation rewrites the file atomically (same pattern as the
// session store).
type Store struct {
	path       string
	mu         sync.Mutex
	user       map[string]Mode // user-defined and user-overridden, by name
	suppressed map[string]bool // deleted built-ins, by name
}

// DefaultPath is where the ensemble store lives unless overridden.
func DefaultPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", "modes.json")
}

// NewStore opens (or starts) the store at path.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:       path,
		user:       make(map[string]Mode),
		suppressed: make(map[string]bool),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run: built-ins only
		}
		return err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("ensemble store %s: %w", s.path, err)
	}
	for _, m := range f.Modes {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		m.BuiltIn = false // an on-disk entry is the user's, whatever it shadows
		s.user[m.Name] = m
	}
	for _, name := range f.Suppressed {
		s.suppressed[name] = true
	}
	return nil
}

// flush writes the user's entries atomically. Caller holds s.mu.
func (s *Store) flush() error {
	f := file{Modes: make([]Mode, 0, len(s.user))}
	for _, m := range s.user {
		m.BuiltIn = false
		f.Modes = append(f.Modes, m)
	}
	sort.Slice(f.Modes, func(i, j int) bool { return f.Modes[i].Name < f.Modes[j].Name })
	for name := range s.suppressed {
		f.Suppressed = append(f.Suppressed, name)
	}
	sort.Strings(f.Suppressed)

	b, err := json.MarshalIndent(f, "", "  ")
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

// List returns every available ensemble — built-ins merged with the user's,
// user entries winning on a name collision and suppressed built-ins omitted —
// sorted by name.
func (s *Store) List() []Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Mode, 0, len(s.user)+len(BuiltIns()))
	for _, m := range BuiltIns() {
		if s.suppressed[m.Name] {
			continue
		}
		if _, overridden := s.user[m.Name]; overridden {
			continue
		}
		m.BuiltIn = true
		out = append(out, m)
	}
	for _, m := range s.user {
		m.BuiltIn = false
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get resolves one ensemble by name, with the same precedence as List.
func (s *Store) Get(name string) (Mode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.user[name]; ok {
		m.BuiltIn = false
		return m, true
	}
	if s.suppressed[name] {
		return Mode{}, false
	}
	for _, m := range BuiltIns() {
		if m.Name == name {
			m.BuiltIn = true
			return m, true
		}
	}
	return Mode{}, false
}

// Save inserts or replaces an ensemble. Saving under a built-in's name shadows
// that built-in (and un-suppresses it, since the user is clearly not done with
// the name). Returns the stored copy.
func (s *Store) Save(m Mode) (Mode, error) {
	m.Name = strings.TrimSpace(m.Name)
	if err := Validate(m); err != nil {
		return Mode{}, err
	}
	m.BuiltIn = false
	m.Updated = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user[m.Name] = m
	delete(s.suppressed, m.Name)
	if err := s.flush(); err != nil {
		return Mode{}, err
	}
	return m, nil
}

// Delete removes an ensemble. A user entry is erased; a built-in (which lives
// in code) is suppressed so it stays gone across upgrades. Deleting a user
// entry that shadows a built-in reveals the built-in again — the natural
// reading of "undo my edit".
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, wasUser := s.user[name]
	delete(s.user, name)
	if !wasUser {
		for _, b := range BuiltIns() {
			if b.Name == name {
				s.suppressed[name] = true
				break
			}
		}
	}
	return s.flush()
}

// Validate rejects an ensemble the apply path could not act on. Model ids and
// permission modes are NOT checked: those vocabularies belong to the harness,
// and a stale allow-list here would reject models that started working today.
func Validate(m Mode) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("ensemble name is required")
	}
	for i, w := range m.Workers {
		if strings.TrimSpace(w.Role) == "" {
			return fmt.Errorf("worker %d needs a role name", i+1)
		}
	}
	return nil
}
