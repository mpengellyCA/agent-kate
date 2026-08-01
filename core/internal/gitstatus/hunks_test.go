package gitstatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHunksSkipOversizeWorkingTreeFile pins audit F11 on the core side: the
// size check must happen BEFORE the read. A huge file (an agent-generated log,
// a dropped ISO) must cost no allocation at all — the gutter simply has no
// markers for it — rather than being pulled into memory and then diffed.
func TestHunksSkipOversizeWorkingTreeFile(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	big := filepath.Join(repo, "big.log")
	if err := os.WriteFile(big, []byte(strings.Repeat("line\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sparse file: apparent size over the cap, no disk cost.
	f, err := os.OpenFile(big, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxHunkFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	hunks, err := computeFileHunks(wtFor(repo), "big.log")
	if err != nil {
		t.Fatalf("an oversize file must be skipped, not fail: %v", err)
	}
	if len(hunks) != 0 {
		t.Fatalf("got %d hunks for a file over the cap, want none", len(hunks))
	}

	// A normal file still diffs, so the cap has not disabled the feature.
	small := filepath.Join(repo, "f.txt")
	if err := os.WriteFile(small, []byte("x\nCHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hunks, err = computeFileHunks(wtFor(repo), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) == 0 {
		t.Fatal("an edited small file produced no hunks")
	}
}

// A non-regular path (fifo, device, directory) must never be read either: a
// read on a fifo blocks forever, which would park the git cache goroutine.
func TestHunksSkipNonRegularFiles(t *testing.T) {
	repo, _ := initLinearRepo(t, 1)
	if err := os.Mkdir(filepath.Join(repo, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	hunks, err := computeFileHunks(wtFor(repo), "adir")
	if err != nil || len(hunks) != 0 {
		t.Fatalf("hunks=%v err=%v, want none/nil for a directory", hunks, err)
	}
}
