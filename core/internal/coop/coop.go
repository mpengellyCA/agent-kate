// Package coop holds the shared cooperation state that the Cooperation MCP
// exposes to agents: which files are open, who is focused where, advisory file
// claims, a notes board, and review requests — so agents and the human stay
// aware of one another.
package coop

import (
	"sort"
	"sync"
	"time"
)

// maxBoardEntries caps how many notes and review requests the board keeps in
// memory. When the count exceeds the cap the oldest entries are evicted
// (oldest-first); the nextNote/nextRev counters keep climbing, so callers that
// track the latest-seen ID are unaffected. Without this cap both slices grow for
// the whole process lifetime — on a long-running arena that is effectively
// unbounded, one of the leaks systemd-oomd eventually trips on.
const maxBoardEntries = 500

// OpenFile is one file currently open in the arena.
type OpenFile struct {
	Path  string `json:"path"`
	Owner string `json:"owner"` // "human" or an agent thread id
}

// Note is one message on the cooperation board.
type Note struct {
	ID     int       `json:"id"`
	Author string    `json:"author"`
	Text   string    `json:"text"`
	Time   time.Time `json:"time"`
}

// Presence is what an owner is currently focused on.
type Presence struct {
	Owner       string `json:"owner"`
	FocusedFile string `json:"focusedFile"`
}

// Claim is an advisory soft lock on a file held by an owner.
type Claim struct {
	Path  string `json:"path"`
	Owner string `json:"owner"`
}

// Review is an agent's request for the human to look at its work.
type Review struct {
	ID      int       `json:"id"`
	Thread  string    `json:"thread"`
	Summary string    `json:"summary"`
	Time    time.Time `json:"time"`
}

// State is the live cooperation state. All methods are safe for concurrent use.
type State struct {
	mu        sync.Mutex
	openFiles map[string]OpenFile // keyed by path
	notes     []Note
	nextNote  int
	presence  map[string]Presence // keyed by owner
	claims    map[string]string   // path -> owner
	reviews   []Review
	nextRev   int
}

// NewState returns an empty cooperation state.
func NewState() *State {
	return &State{
		openFiles: make(map[string]OpenFile),
		presence:  make(map[string]Presence),
		claims:    make(map[string]string),
	}
}

// SetOpenFiles replaces the set of files recorded as open under owner.
func (s *State) SetOpenFiles(owner string, paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, f := range s.openFiles {
		if f.Owner == owner {
			delete(s.openFiles, p)
		}
	}
	for _, p := range paths {
		if p != "" {
			s.openFiles[p] = OpenFile{Path: p, Owner: owner}
		}
	}
}

// ListOpenFiles returns every open file, sorted by path.
func (s *State) ListOpenFiles() []OpenFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]OpenFile, 0, len(s.openFiles))
	for _, f := range s.openFiles {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// PostNote appends a note and returns it.
func (s *State) PostNote(author, text string) Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextNote++
	n := Note{ID: s.nextNote, Author: author, Text: text, Time: time.Now()}
	s.notes = append(s.notes, n)
	if len(s.notes) > maxBoardEntries {
		s.notes = s.notes[len(s.notes)-maxBoardEntries:]
	}
	return n
}

// ReadNotes returns a copy of every note, oldest first.
func (s *State) ReadNotes() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Note, len(s.notes))
	copy(out, s.notes)
	return out
}

// SetPresence records what owner is currently focused on.
func (s *State) SetPresence(owner, focusedFile string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presence[owner] = Presence{Owner: owner, FocusedFile: focusedFile}
}

// ListPresence returns every owner's focus, sorted by owner.
func (s *State) ListPresence() []Presence {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Presence, 0, len(s.presence))
	for _, p := range s.presence {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out
}

// ClaimFile records an advisory claim on path. It returns false and the
// current holder if the path is already claimed by a different owner.
func (s *State) ClaimFile(path, owner string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if holder, ok := s.claims[path]; ok && holder != owner {
		return false, holder
	}
	s.claims[path] = owner
	return true, owner
}

// ReleaseFile drops owner's claim on path.
func (s *State) ReleaseFile(path, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if holder, ok := s.claims[path]; ok && holder == owner {
		delete(s.claims, path)
	}
}

// ListClaims returns every active claim, sorted by path.
func (s *State) ListClaims() []Claim {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Claim, 0, len(s.claims))
	for p, o := range s.claims {
		out = append(out, Claim{Path: p, Owner: o})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// AddReview records an agent's review request and returns it.
func (s *State) AddReview(thread, summary string) Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRev++
	r := Review{ID: s.nextRev, Thread: thread, Summary: summary, Time: time.Now()}
	s.reviews = append(s.reviews, r)
	if len(s.reviews) > maxBoardEntries {
		s.reviews = s.reviews[len(s.reviews)-maxBoardEntries:]
	}
	return r
}

// ListReviews returns a copy of every review request, oldest first.
func (s *State) ListReviews() []Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Review, len(s.reviews))
	copy(out, s.reviews)
	return out
}

// ClearOwner drops an owner's presence, claims and open files — used when an
// agent thread exits so its locks do not linger.
func (s *State) ClearOwner(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.presence, owner)
	for p, o := range s.claims {
		if o == owner {
			delete(s.claims, p)
		}
	}
	for p, f := range s.openFiles {
		if f.Owner == owner {
			delete(s.openFiles, p)
		}
	}
}
