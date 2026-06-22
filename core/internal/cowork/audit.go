package cowork

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// AuditKind classifies an audit entry.
type AuditKind string

const (
	AuditGrant  AuditKind = "grant"  // a grant was created
	AuditDeny   AuditKind = "deny"   // an authorization was denied
	AuditRevoke AuditKind = "revoke" // a grant was revoked
	AuditAction AuditKind = "action" // a granted capability was exercised
	AuditKill   AuditKind = "kill"   // kill-switch engaged
	AuditRearm  AuditKind = "rearm"  // kill-switch re-armed
	AuditTamper AuditKind = "tamper" // chain verification failed on load
)

// AuditEntry is one append-only, hash-chained record. Hash =
// sha256(canonical(entry with Hash="")). PrevHash links to the previous entry, so
// truncation or in-place mutation is detectable on load. ArtifactHash records a
// sha256 of any captured content (never the content itself).
type AuditEntry struct {
	Seq          int64      `json:"seq"`
	At           time.Time  `json:"at"`
	Kind         AuditKind  `json:"kind"`
	ThreadID     string     `json:"threadId,omitempty"`
	Capability   Capability `json:"capability,omitempty"`
	Target       *Target    `json:"target,omitempty"`
	GrantID      string     `json:"grantId,omitempty"`
	Detail       string     `json:"detail,omitempty"`
	ArtifactHash string     `json:"artifactHash,omitempty"`
	PrevHash     string     `json:"prevHash"`
	Hash         string     `json:"hash"`
}

// Audit is the append-only audit log. Writes are flock-serialized across processes.
type Audit struct {
	mu       sync.Mutex
	path     string
	seq      int64
	head     string
	tampered bool
}

// LoadAudit opens (or prepares) the audit log and verifies the existing hash chain.
// On a verification failure it sets tampered=true (the Authority then fails closed);
// the caller should surface this to the user. A missing file is a clean genesis.
func LoadAudit(path string) (*Audit, error) {
	a := &Audit{path: path}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return a, nil
		}
		return nil, fmt.Errorf("cowork: open audit: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var prev string
	var lastSeq int64
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			a.tampered = true
			break
		}
		if e.PrevHash != prev || e.Hash != hashEntry(e) {
			a.tampered = true
			break
		}
		prev = e.Hash
		lastSeq = e.Seq
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cowork: scan audit: %w", err)
	}
	a.head = prev
	a.seq = lastSeq
	return a, nil
}

// Tampered reports whether the chain failed verification on load.
func (a *Audit) Tampered() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tampered
}

// hashEntry computes the canonical hash of e with its Hash field cleared.
func hashEntry(e AuditEntry) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Append writes a new chained entry. Seq, At, PrevHash and Hash are filled in here;
// the caller supplies the semantic fields. The true chain head is re-read from the
// file UNDER the flock before linking, so a second akcore process sharing the data dir
// (or a stale in-memory head) cannot fork the chain: flock serializes the write, and
// re-reading guarantees PrevHash points at whatever is actually last on disk.
func (a *Audit) Append(e AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	// Re-sync the head/seq to the on-disk tail (another process may have appended
	// since we loaded). An unreadable/empty tail is a clean genesis (head="").
	if head, seq, ok, rerr := lastChainState(f); rerr != nil {
		return rerr
	} else if ok {
		a.head, a.seq = head, seq
	} else {
		a.head, a.seq = "", 0
	}

	a.seq++
	e.Seq = a.seq
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.PrevHash = a.head
	e.Hash = hashEntry(e)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	a.head = e.Hash
	return nil
}

// lastChainState reads the last complete entry from f (its Hash and Seq) so a new append
// can link onto the actual on-disk tail. It reads only a trailing window — entries are
// small, so the last line always lies within it. ok is false for an empty/garbled tail.
func lastChainState(f *os.File) (head string, seq int64, ok bool, err error) {
	st, err := f.Stat()
	if err != nil {
		return "", 0, false, err
	}
	size := st.Size()
	if size == 0 {
		return "", 0, false, nil
	}
	const window = 64 * 1024
	start := int64(0)
	if size > window {
		start = size - window
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", 0, false, err
	}
	for _, ln := range bytesReverseLines(buf) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		var e AuditEntry
		if json.Unmarshal(ln, &e) != nil {
			return "", 0, false, nil // garbled tail: treat as genesis rather than guess
		}
		return e.Hash, e.Seq, true, nil
	}
	return "", 0, false, nil
}

// bytesReverseLines splits on '\n' and returns the lines newest-first.
func bytesReverseLines(b []byte) [][]byte {
	parts := bytes.Split(b, []byte{'\n'})
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return parts
}

// Tail returns up to limit entries (oldest→newest) with Seq > sinceSeq, optionally
// filtered by thread.
func (a *Audit) Tail(threadID string, sinceSeq int64, limit int) ([]AuditEntry, int64, error) {
	a.mu.Lock()
	path := a.path
	head := a.seq
	a.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, head, nil
		}
		return nil, head, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []AuditEntry
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Seq <= sinceSeq {
			continue
		}
		if threadID != "" && e.ThreadID != threadID {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, head, sc.Err()
}
