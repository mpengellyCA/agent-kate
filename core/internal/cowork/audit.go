package cowork

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	AuditRotate AuditKind = "rotate" // log rotated; genesis of the new chain
)

// Audit-log retention (audit F10). The log is append-only and nothing ever
// pruned it, so a long-lived install grew it without bound — a disk-fill DoS on
// the user, and a load-time verification pass that got slower forever.
//
// Rotation cannot simply keep the tail: the chain is verified from a genesis
// whose PrevHash is empty, so a truncated head would read as TAMPERING and fail
// the Authority closed — a retention policy that trips the alarm it is meant to
// keep quiet. Instead the whole segment is copied aside and the live file
// restarts with an AuditRotate entry that RECORDS the rotation in-chain: the
// archived segment's name, its entry count, its last seq, its chain head, and a
// sha256 of the segment itself. The archive is therefore still cryptographically
// anchored from the live chain, and the live chain verifies cleanly from its
// own genesis.
//
// One archived segment is retained (overwritten by the next rotation), so the
// pair is bounded at ~2× maxAuditBytes rather than growing forever.
const (
	maxAuditBytes      = 8 << 20
	auditArchiveSuffix = ".1"
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

	// Retention runs INSIDE the flock, before the head is re-synced, so the
	// rotation entry it writes is the tail this append then links onto.
	// Deliberately not fatal: a rotation we could not complete must never cost
	// us the entry we were asked to record.
	if rerr := a.rotateIfNeededLocked(f); rerr != nil {
		slog.Warn("cowork: audit rotation skipped", "path", a.path, "err", rerr)
	}

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

// rotateIfNeededLocked copies the log aside and restarts it when it has grown
// past maxAuditBytes. The caller holds a.mu AND the flock on f, and f is open
// O_APPEND on the live path.
//
// Deliberately copy + truncate IN PLACE rather than rename: another process may
// already be blocked on the flock holding an fd to this inode, and a rename
// would send its append to the archived segment — a fork in the chain that the
// next load would report as tampering. Truncating the inode everyone is queued
// on keeps them all writing to the same, now-empty, file.
//
// The archive is written BEFORE the live file is truncated, so a failure at any
// step leaves the log exactly as it was.
func (a *Audit) rotateIfNeededLocked(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size < maxAuditBytes {
		return nil
	}

	// Read the outgoing segment's identity first: its chain head (what the new
	// genesis will point back at) and a hash of its bytes (what makes the
	// archive verifiable from the live chain).
	head, lastSeq, _, err := lastChainState(f)
	if err != nil {
		return err
	}

	archive := a.path + auditArchiveSuffix
	tmp := archive + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	sum := sha256.New()
	entries, cerr := io.Copy(io.MultiWriter(dst, sum), io.NewSectionReader(f, 0, size))
	closeErr := dst.Close()
	if cerr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if cerr != nil {
			return cerr
		}
		return closeErr
	}
	if entries != size {
		_ = os.Remove(tmp)
		return fmt.Errorf("cowork: audit archive short write (%d of %d bytes)", entries, size)
	}
	if err := os.Rename(tmp, archive); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	// Point of no return: the segment is safely on disk, so the live file may
	// now be emptied. O_APPEND means every subsequent write still lands at the
	// end, whatever any other process's file offset happens to be.
	if err := f.Truncate(0); err != nil {
		return err
	}
	a.head, a.seq = "", 0

	rot := AuditEntry{
		Seq:  1,
		At:   time.Now(),
		Kind: AuditRotate,
		Detail: fmt.Sprintf(
			"audit log rotated at %d bytes; entries 1..%d moved to %s (previous chain head %s)",
			size, lastSeq, filepath.Base(archive), head),
		ArtifactHash: hex.EncodeToString(sum.Sum(nil)),
		PrevHash:     "", // genesis of the new chain — see maxAuditBytes
	}
	rot.Hash = hashEntry(rot)
	line, err := json.Marshal(rot)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	a.head, a.seq = rot.Hash, rot.Seq
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
