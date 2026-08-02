package search

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeRg puts a stand-in `rg` first on PATH. It emits one valid match event,
// then execs into sleep so the process itself (not a child holding the pipe)
// is what Run's context has to kill — the shape of a real rg pinned on a huge
// tree.
func fakeRg(t *testing.T, sleepSec string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
echo '{"type":"match","data":{"path":{"text":"/p/a.go"},"lines":{"text":"hit\n"},"line_number":3,"submatches":[{"start":0,"end":3}]}}'
exec sleep ` + sleepSec + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A search whose rg never finishes must be killed at the context deadline and
// return cleanly with whatever it had (audit F64). This fails if Run goes back
// to exec.Command: the scanner would then sit on the pipe for the fake rg's
// full sleep, and the elapsed-time assertion trips.
func TestRunKilledByContext(t *testing.T) {
	fakeRg(t, "10")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, err := Run(ctx, Options{Query: "hit", Root: "/p"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s; the rg child was not killed at the deadline", elapsed)
	}
	if res.Total != 1 || len(res.Files) != 1 {
		t.Fatalf("partial result lost: total=%d files=%d", res.Total, len(res.Files))
	}
	if !res.Truncated {
		t.Fatal("a search ended by the deadline must be flagged Truncated")
	}
}

// The happy path through the same fake: a completed rg (sleep 0) is not
// truncated and its match is parsed.
func TestRunCompletesUnderCap(t *testing.T) {
	fakeRg(t, "0")
	res, err := Run(context.Background(), Options{Query: "hit", Root: "/p"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Truncated {
		t.Fatal("a completed search must not be flagged Truncated")
	}
	if res.Total != 1 || len(res.Files) != 1 || res.Files[0].Path != "/p/a.go" {
		t.Fatalf("unexpected result: %+v", res)
	}
}
