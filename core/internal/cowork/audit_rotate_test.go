package cowork

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fillAudit appends entries until the log is at least minBytes on disk.
func fillAudit(t *testing.T, a *Audit, path string, minBytes int64) int {
	t.Helper()
	n := 0
	for {
		if err := a.Append(AuditEntry{
			Kind:   AuditAction,
			Detail: strings.Repeat("d", 4096),
		}); err != nil {
			t.Fatal(err)
		}
		n++
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() >= minBytes {
			return n
		}
	}
}

// Rotation is the whole point of audit F10's "cowork-audit.jsonl grows
// forever" — but a rotation that broke the hash chain would fail the Authority
// CLOSED on every later load, which is worse than the disk it saved. So: the
// live file shrinks, and it still verifies.
func TestAuditRotationBoundsFileAndKeepsChainValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cowork-audit.jsonl")
	a, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}

	fillAudit(t, a, path, maxAuditBytes)
	// One more append crosses the threshold and triggers the rotation.
	if err := a.Append(AuditEntry{Kind: AuditGrant, Detail: "after"}); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() >= maxAuditBytes {
		t.Fatalf("live log is still %d bytes; rotation did not happen", st.Size())
	}

	// THE gate: a fresh load of the rotated log must verify cleanly. A tampered
	// verdict here means retention would fail the Authority closed.
	reloaded, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Tampered() {
		t.Fatal("rotated log reads as TAMPERED; the chain was not preserved")
	}

	// The new chain's genesis records the rotation in-chain, and anchors the
	// archived segment by hash.
	entries := readEntries(t, path)
	if len(entries) < 2 {
		t.Fatalf("expected a rotation genesis plus the new entry, got %d", len(entries))
	}
	rot := entries[0]
	if rot.Kind != AuditRotate {
		t.Fatalf("first entry is %q, want %q", rot.Kind, AuditRotate)
	}
	if rot.PrevHash != "" || rot.Seq != 1 {
		t.Fatalf("rotation entry is not a genesis: seq=%d prev=%q", rot.Seq, rot.PrevHash)
	}
	if entries[1].PrevHash != rot.Hash {
		t.Fatal("the entry after the rotation does not link to it")
	}
	if entries[1].Detail != "after" {
		t.Fatalf("the triggering append was lost; got %q", entries[1].Detail)
	}

	// The archived segment exists, still verifies as its own chain, and matches
	// the hash the live chain recorded for it.
	archive := path + auditArchiveSuffix
	arch, err := LoadAudit(archive)
	if err != nil {
		t.Fatal(err)
	}
	if arch.Tampered() {
		t.Fatal("archived segment does not verify")
	}
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != rot.ArtifactHash {
		t.Fatalf("archived segment hash %s does not match the in-chain record %s",
			got, rot.ArtifactHash)
	}
	if fi, err := os.Stat(archive); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("archived segment is mode %04o, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(archive + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("rotation left its temp file behind")
	}
}

// Two rotations must stay bounded: the second overwrites the first archive
// rather than accumulating segments.
func TestAuditRotationRetainsOneArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cowork-audit.jsonl")
	a, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		fillAudit(t, a, path, maxAuditBytes)
		if err := a.Append(AuditEntry{Kind: AuditGrant}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := filepath.Glob(filepath.Join(dir, "cowork-audit.jsonl*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("data dir holds %v; want the live log plus exactly one archive", names)
	}
	if reloaded, err := LoadAudit(path); err != nil {
		t.Fatal(err)
	} else if reloaded.Tampered() {
		t.Fatal("log reads as TAMPERED after a second rotation")
	}
}

// Below the threshold nothing is touched — no archive, no rotation entry.
func TestAuditNoRotationBelowCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cowork-audit.jsonl")
	a, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := a.Append(AuditEntry{Kind: AuditAction}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + auditArchiveSuffix); !os.IsNotExist(err) {
		t.Fatal("rotated a log that was nowhere near the cap")
	}
	for _, e := range readEntries(t, path) {
		if e.Kind == AuditRotate {
			t.Fatal("wrote a rotation entry without rotating")
		}
	}
}

func readEntries(t *testing.T, path string) []AuditEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("unparseable audit line: %v", err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
