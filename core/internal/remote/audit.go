package remote

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

// auditScanBuffer bounds one line of the audit log. Entries are short by
// construction (no payloads, only who/what/when), so 1 MiB is orders of
// magnitude of headroom and still refuses to allocate unboundedly for a file
// somebody has appended garbage to.
const auditScanBuffer = 1024 * 1024

// auditTailWindow is how far back Append reads to find the true chain head. The
// last line always lies within it for entries of this size.
const auditTailWindow = 64 * 1024

// AuditKind classifies a remote action.
//
// Every kind here is a MUTATION. Reads — the roster, a transcript, a diff, a git
// log — are deliberately absent: a phone glancing at the roster once a minute
// would bury the entries that matter, and "who approved what, from which device,
// when" is the only question this log exists to answer.
type AuditKind string

const (
	AuditPair       AuditKind = "pair"       // a device was paired
	AuditAuth       AuditKind = "auth"       // a token was exchanged for a session
	AuditLogout     AuditKind = "logout"     // a device ended its session
	AuditRevoke     AuditKind = "revoke"     // a device was revoked
	AuditKill       AuditKind = "kill"       // global kill-switch engaged
	AuditRearm      AuditKind = "rearm"      // global kill-switch re-armed
	AuditSend       AuditKind = "send"       // a prompt was sent or queued
	AuditPermission AuditKind = "permission" // a permission prompt was answered
	AuditInterrupt  AuditKind = "interrupt"  // a turn was interrupted
	AuditStop       AuditKind = "stop"       // an agent was stopped
	AuditCapability AuditKind = "capability" // desktop changed a device's developer powers
	AuditTamper     AuditKind = "tamper"     // chain verification failed on load
	AuditRotate     AuditKind = "rotate"     // bounded-retention chain genesis
)

// Keep the live segment and at most one previous segment. Rotation restarts a
// hash chain with an anchored AuditRotate genesis instead of trimming its head,
// because trimming would make a healthy retention policy indistinguishable
// from tampering at the next load.
const (
	maxAuditBytes      = 8 << 20
	auditArchiveSuffix = ".1"
)

// AuditEntry is one append-only, hash-chained record, following
// cowork.AuditEntry exactly: Hash = sha256(canonical(entry with Hash="")) and
// PrevHash links to the previous entry, so truncation or in-place editing is
// detectable on load.
//
// The same "detect, not prevent" honesty applies here as it does for Cowork: an
// agent runs at this uid and can reach this file. The chain does not stop it
// editing the record; it stops it doing so unnoticed.
type AuditEntry struct {
	Seq        int64     `json:"seq"`
	At         time.Time `json:"at"`
	Kind       AuditKind `json:"kind"`
	DeviceID   string    `json:"deviceId,omitempty"`
	DeviceName string    `json:"deviceName,omitempty"`
	ThreadID   string    `json:"threadId,omitempty"`
	RequestID  string    `json:"requestId,omitempty"`
	// Detail is a short, already-redacted description. It must never carry a
	// prompt body, a tool input or typed text — the same rule mcp.activity's
	// argsSummary follows, for the same reason: this file outlives the session.
	Detail  string `json:"detail,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	// ArtifactHash anchors the bounded retained archive from a new chain's
	// rotation entry. It is a hash of audit bytes, never user/tool content.
	ArtifactHash string `json:"artifactHash,omitempty"`
	PrevHash     string `json:"prevHash"`
	Hash         string `json:"hash"`
}

// Audit is the append-only remote-action log. Writes are flock-serialised across
// processes.
type Audit struct {
	mu       sync.Mutex
	path     string
	seq      int64
	head     string
	tampered bool
}

// LoadAudit opens (or prepares) the log and verifies the existing chain. A
// verification failure sets tampered; the caller surfaces it rather than
// pretending the history is intact. A missing file is a clean genesis.
func LoadAudit(path string) (*Audit, error) {
	a := &Audit{path: path}
	b, err := readPrivate(path)
	if err != nil {
		if os.IsNotExist(err) {
			return a, nil
		}
		return nil, fmt.Errorf("remote: open audit: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), auditScanBuffer)
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
		if e.PrevHash != prev || e.Hash != hashAuditEntry(e) {
			a.tampered = true
			break
		}
		prev = e.Hash
		lastSeq = e.Seq
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("remote: scan audit: %w", err)
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

func hashAuditEntry(e AuditEntry) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Append writes a new chained entry. The true head is re-read from the file
// UNDER the flock before linking, so a second akcore sharing the data dir cannot
// fork the chain — flock serialises the write and the re-read guarantees
// PrevHash points at whatever is actually last on disk.
func (a *Audit) Append(e AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	f, err := openPrivate(a.path, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	if err := a.rotateIfNeededLocked(f); err != nil {
		return err
	}

	if head, seq, ok, rerr := lastAuditState(f); rerr != nil {
		return rerr
	} else if ok {
		a.head, a.seq = head, seq
	} else {
		a.head, a.seq = "", 0
	}

	a.seq++
	e.Seq = a.seq
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.PrevHash = a.head
	e.Hash = hashAuditEntry(e)
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

// rotateIfNeededLocked retains one archived segment and records a fresh chain
// genesis that anchors its content. Caller holds a.mu and f's flock.
func (a *Audit) rotateIfNeededLocked(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() < maxAuditBytes {
		return nil
	}
	head, seq, _, err := lastAuditState(f)
	if err != nil {
		return err
	}
	segment := io.NewSectionReader(f, 0, st.Size())
	archive := a.path + auditArchiveSuffix
	if err := copyPrivate(archive, segment); err != nil {
		return err
	}
	archived, err := readPrivate(archive)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archived)
	if err := f.Truncate(0); err != nil {
		return err
	}
	a.head, a.seq = "", 0
	rotate := AuditEntry{
		Seq: 1, At: time.Now().UTC(), Kind: AuditRotate,
		Detail: fmt.Sprintf("audit log rotated at %d bytes; entries 1..%d moved to %s (previous chain head %s)",
			st.Size(), seq, filepath.Base(archive), head),
		ArtifactHash: hex.EncodeToString(sum[:]),
	}
	rotate.Hash = hashAuditEntry(rotate)
	line, err := json.Marshal(rotate)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	a.head, a.seq = rotate.Hash, rotate.Seq
	return nil
}

// lastAuditState reads the last complete entry so a new append links onto the
// actual on-disk tail. A garbled tail is treated as genesis rather than guessed
// at — writing a link to something we could not parse would manufacture a chain
// break that looks exactly like tampering.
func lastAuditState(f *os.File) (head string, seq int64, ok bool, err error) {
	st, err := f.Stat()
	if err != nil {
		return "", 0, false, err
	}
	size := st.Size()
	if size == 0 {
		return "", 0, false, nil
	}
	start := int64(0)
	if size > auditTailWindow {
		start = size - auditTailWindow
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", 0, false, err
	}
	parts := bytes.Split(buf, []byte{'\n'})
	for i := len(parts) - 1; i >= 0; i-- {
		ln := bytes.TrimSpace(parts[i])
		if len(ln) == 0 {
			continue
		}
		var e AuditEntry
		if json.Unmarshal(ln, &e) != nil {
			return "", 0, false, nil
		}
		return e.Hash, e.Seq, true, nil
	}
	return "", 0, false, nil
}

// Tail returns up to limit entries (oldest→newest) with Seq > sinceSeq.
func (a *Audit) Tail(sinceSeq int64, limit int) ([]AuditEntry, int64, error) {
	a.mu.Lock()
	path := a.path
	head := a.seq
	a.mu.Unlock()

	b, err := readPrivate(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, head, nil
		}
		return nil, head, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), auditScanBuffer)
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
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, head, sc.Err()
}
