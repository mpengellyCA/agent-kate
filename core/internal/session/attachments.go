// Attachment sidecars record, per agent thread, the compact provenance of every
// file the human attached to an outgoing message: its display name, kind, origin
// path, media type and an outside-workspace flag. The on-disk Claude Code
// transcript keeps only the *inlined* content (a text block for a text file, a
// base64 image block for an image), so the original file name/path is otherwise
// unrecoverable on resume. The UI reads this sidecar when it replays a thread so
// it can redraw the "You" card's named, clickable attachment chips.
//
// Deliberately compact: it never stores the attachment body (dataB64 / text),
// only metadata, so a thread with big attachments does not double its on-disk
// footprint. Turns are appended in send order; the UI pairs the Nth user turn it
// replays from the transcript with the Nth entry here.
package session

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"agentkate/internal/fsperm"
)

// AttachmentMeta is the compact, body-free description of one attached file.
type AttachmentMeta struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "image" or "text"
	Path      string `json:"path,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Outside   bool   `json:"outside,omitempty"`
	// CachePath is the UI's durable copy of an image's bytes, recorded so a
	// replayed chip can still draw its thumbnail when the origin path was a
	// temp file that has since been deleted. Still a path, not bytes — this
	// sidecar stays body-free.
	CachePath string `json:"cachePath,omitempty"`
}

// AttachmentTurn is the attachment metadata for one outgoing user message. Text
// is the message the user typed (may be empty for an attachment-only message);
// it lets the UI redraw the You card body exactly as it was sent.
type AttachmentTurn struct {
	Text        string           `json:"text"`
	Attachments []AttachmentMeta `json:"attachments"`
}

// AttachmentStore persists per-thread attachment sidecars under a directory,
// one JSON file per thread (attachments/<threadID>.json holding an ordered
// array of AttachmentTurn). A single mutex serialises appends across threads —
// writes are infrequent (one per sent message) so contention is a non-issue.
type AttachmentStore struct {
	dir string
	mu  sync.Mutex
}

// DefaultAttachmentDir is where sidecars live unless overridden — beside the
// thread store, so the whole of Agent Kate's per-thread state sits together.
func DefaultAttachmentDir() string {
	return filepath.Join(filepath.Dir(DefaultPath()), "attachments")
}

// NewAttachmentStore opens (lazily) the sidecar directory at dir. The directory
// is still created on first write, not here, so a session that never attaches
// anything never creates it.
//
// It does MIGRATE an existing directory's permissions: sidecars written by
// earlier builds are 0644 in a 0755 directory and name every file the human
// ever attached, with its full path. Deferring that to the next write would
// leave a thread nobody attaches to again exposed forever.
//
// A migration failure is logged, not fatal, and does not stop the store from
// opening: the data root above it is 0700 by then (session.Store.harden), so
// the sidecars are already unreachable by other users, and refusing to open the
// store would cost every attachment chip in the UI over a defence-in-depth
// layer. New writes below are 0600 regardless.
func NewAttachmentStore(dir string) *AttachmentStore {
	if n, err := fsperm.HardenTree(dir); err != nil {
		slog.Warn("could not tighten attachment sidecar permissions", "dir", dir, "err", err)
	} else {
		fsperm.LogMigration(dir, n)
	}
	return &AttachmentStore{dir: dir}
}

func (s *AttachmentStore) pathFor(threadID string) string {
	return filepath.Join(s.dir, threadID+".json")
}

// Append records one sent message's attachment metadata for a thread. A turn
// with no attachments is skipped: the sidecar only exists to recover chips, and
// the transcript already carries the plain user text for replay. Failures are
// returned but callers treat them as non-fatal (chips degrade to absent).
func (s *AttachmentStore) Append(threadID string, turn AttachmentTurn) error {
	if threadID == "" || len(turn.Attachments) == 0 {
		return nil
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

// Load returns a thread's recorded attachment turns in send order, or an empty
// slice if none were ever recorded (a fresh thread, or one with no attachments).
func (s *AttachmentStore) Load(threadID string) ([]AttachmentTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(threadID)
}

// Remove drops a thread's sidecar (used when a thread is forgotten). Missing is
// not an error.
func (s *AttachmentStore) Remove(threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.pathFor(threadID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// load reads (unlocked — callers hold the mutex) the sidecar for a thread.
func (s *AttachmentStore) load(threadID string) ([]AttachmentTurn, error) {
	if threadID == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.pathFor(threadID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var turns []AttachmentTurn
	if err := json.Unmarshal(b, &turns); err != nil {
		return nil, err
	}
	return turns, nil
}

// write atomically replaces a thread's sidecar (unlocked — callers hold the mutex).
func (s *AttachmentStore) write(threadID string, turns []AttachmentTurn) error {
	b, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return err
	}
	// 0700/0600: a sidecar lists the name, origin path and workspace-outside
	// flag of every file the human attached to this thread. Same data class as
	// the transcript itself, same discipline.
	if err := fsperm.MkdirAll(s.dir); err != nil {
		return err
	}
	tmp := s.pathFor(threadID) + ".tmp"
	if err := fsperm.WriteFile(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, s.pathFor(threadID))
}
