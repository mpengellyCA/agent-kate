package cowork

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
// the caller supplies the semantic fields.
func (a *Audit) Append(e AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	e.Seq = a.seq
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.PrevHash = a.head
	e.Hash = hashEntry(e)

	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	a.head = e.Hash
	return nil
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
