package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditChainSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-audit.jsonl")
	first, err := LoadAudit(path)
	if err != nil {
		t.Fatalf("LoadAudit: %v", err)
	}
	for _, k := range []AuditKind{AuditPair, AuditAuth, AuditSend} {
		if err := first.Append(AuditEntry{Kind: k, DeviceID: "d-1"}); err != nil {
			t.Fatalf("Append(%v): %v", k, err)
		}
	}

	second, err := LoadAudit(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.Tampered() {
		t.Fatal("a chain we just wrote failed verification")
	}
	if err := second.Append(AuditEntry{Kind: AuditStop, DeviceID: "d-1"}); err != nil {
		t.Fatalf("Append after reload: %v", err)
	}
	entries, head, err := second.Tail(0, 100)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 4 || head != 4 {
		t.Fatalf("got %d entries (head %d), want 4", len(entries), head)
	}
	for i, e := range entries {
		if e.Seq != int64(i+1) {
			t.Errorf("entry %d has seq %d", i, e.Seq)
		}
		if i > 0 && e.PrevHash != entries[i-1].Hash {
			t.Errorf("entry %d does not link to its predecessor", i)
		}
	}
}

// TestAuditDetectsTampering is the "detect, not prevent" posture stated as a
// test. An agent runs at this uid and can reach the file; the chain does not
// stop it editing the record, it stops it doing so unnoticed.
func TestAuditDetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{
			name: "a field edited in place",
			mutate: func(t *testing.T, path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				edited := strings.Replace(string(raw), `"allow=true"`, `"allow=fals"`, 1)
				if edited == string(raw) {
					t.Fatal("test fixture did not contain the field it edits")
				}
				if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
		{
			name: "an entry removed from the middle",
			mutate: func(t *testing.T, path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
				out := append([]string{lines[0]}, lines[2:]...)
				if err := os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
		{
			name: "a line replaced with garbage",
			mutate: func(t *testing.T, path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
				lines[1] = "{not json"
				if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "audit.jsonl")
			a, err := LoadAudit(path)
			if err != nil {
				t.Fatalf("LoadAudit: %v", err)
			}
			for _, d := range []string{"allow=true", "allow=false", "mode=queue"} {
				if err := a.Append(AuditEntry{Kind: AuditPermission, Detail: d}); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			tc.mutate(t, path)

			reloaded, err := LoadAudit(path)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if !reloaded.Tampered() {
				t.Fatal("the chain verified after being edited")
			}
		})
	}
}

func TestAuditFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := LoadAudit(path)
	if err != nil {
		t.Fatalf("LoadAudit: %v", err)
	}
	if err := a.Append(AuditEntry{Kind: AuditPair}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("audit mode = %o, want 600", perm)
	}
}

func TestAuditTailFiltersBySeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := LoadAudit(path)
	if err != nil {
		t.Fatalf("LoadAudit: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := a.Append(AuditEntry{Kind: AuditSend}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	cases := []struct {
		name  string
		since int64
		limit int
		want  int
	}{
		{name: "everything", since: 0, limit: 0, want: 10},
		{name: "after the fifth", since: 5, limit: 0, want: 5},
		{name: "limited to the newest three", since: 0, limit: 3, want: 3},
		{name: "past the end", since: 99, limit: 0, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := a.Tail(tc.since, tc.limit)
			if err != nil {
				t.Fatalf("Tail: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d entries, want %d", len(got), tc.want)
			}
		})
	}
}
