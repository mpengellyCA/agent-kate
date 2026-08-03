package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeviceStoreDoesNotCreateCredentialsUntilMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote", "devices.json")
	store, err := LoadDeviceStore(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read-only load created %s: %v", filepath.Dir(path), err)
	}
	if _, _, err := store.Mint("phone"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"devices.json", "devices.json.lock"} {
		fi, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatalf("missing durable %s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %04o, want 0600", name, fi.Mode().Perm())
		}
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("remote data dir = %v / %v, want 0700", fi, err)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".devices.json.tmp-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary device files left behind: %v (%v)", matches, err)
	}
}

func TestCertFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	if _, _, err := ensureCert(dir, "127.0.0.1", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{certFileName, keyFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %04o, want 0600", name, fi.Mode().Perm())
		}
	}
}

func TestAuditRotationBoundsRetentionAndAnchorsArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-audit.jsonl")
	audit, err := LoadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	for {
		if err := audit.Append(AuditEntry{Kind: AuditSend, Detail: strings.Repeat("x", 128*1024)}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() >= maxAuditBytes {
			break
		}
	}
	if err := audit.Append(AuditEntry{Kind: AuditStop, Detail: "after rotation"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= maxAuditBytes {
		t.Fatalf("live audit still %d bytes", fi.Size())
	}
	reloaded, err := LoadAudit(path)
	if err != nil || reloaded.Tampered() {
		t.Fatalf("rotated audit did not reload cleanly: audit=%v err=%v", reloaded, err)
	}
	entries, _, err := reloaded.Tail(0, 10)
	if err != nil || len(entries) < 2 || entries[0].Kind != AuditRotate {
		t.Fatalf("rotation audit entries = %#v, err=%v", entries, err)
	}
	archive := path + auditArchiveSuffix
	archived, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archived)
	if got, want := entries[0].ArtifactHash, hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("archive hash = %q, want %q", got, want)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("archive permissions = %v / %v, want 0600", fi, err)
	}
}
