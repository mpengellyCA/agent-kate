package cowork

import (
	"path/filepath"
	"testing"
)

// TestAuditConcurrentInstancesNoFork reproduces the corruption that two akcore
// processes (or a debug probe) sharing one data dir used to cause: each kept its own
// in-memory chain head, so interleaved appends linked onto a stale head and forked the
// hash chain, which then failed verification ("audit chain tampered"). With the
// per-append head re-sync under the flock, the on-disk chain must stay linear.
func TestAuditConcurrentInstancesNoFork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	a1, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := LoadAudit(path) // a second, independently-loaded log == a second process
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		if err := a1.Append(AuditEntry{Kind: AuditAction, Detail: "a1"}); err != nil {
			t.Fatal(err)
		}
		if err := a2.Append(AuditEntry{Kind: AuditAction, Detail: "a2"}); err != nil {
			t.Fatal(err)
		}
	}

	reloaded, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Tampered() {
		t.Fatalf("chain forked despite the concurrent-safe append")
	}
	entries, _, err := reloaded.Tail("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10 {
		t.Fatalf("want 10 entries, got %d", len(entries))
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq not contiguous: entry %d has seq %d", i, e.Seq)
		}
	}
}
