package compact

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "summaries"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sum := Summary{
		ThreadID:  "t-abc",
		SessionID: "deadbeef-uuid",
		Strategy:  ResumeLocal,
		Created:   time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Turns:     7,
		Body:      "## Conversation\n\n**User:** hi\n",
	}
	if err := s.Put(sum); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get("t-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for existing summary")
	}
	if got.ThreadID != sum.ThreadID || got.SessionID != sum.SessionID ||
		got.Strategy != sum.Strategy || got.Turns != sum.Turns ||
		got.Body != sum.Body {
		t.Errorf("round-trip mismatch:\nwant: %#v\ngot:  %#v", sum, *got)
	}
}

func TestStoreGetMissingIsNil(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	got, err := s.Get("t-nope")
	if err != nil {
		t.Errorf("Get on missing thread should not error, got %v", err)
	}
	if got != nil {
		t.Errorf("Get on missing thread should return nil, got %#v", *got)
	}
}

func TestStoreRemove(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Summary{ThreadID: "t-x", Body: "x"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Remove("t-x"); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if err := s.Remove("t-x"); err != nil {
		t.Errorf("Remove on missing should be a no-op, got %v", err)
	}
	if got, _ := s.Get("t-x"); got != nil {
		t.Errorf("Get after Remove should be nil, got %#v", *got)
	}
}

func TestStorePutRejectsEmptyThreadID(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Summary{Body: "x"}); err == nil {
		t.Error("Put with empty ThreadID should fail")
	}
}

// TestStorePrivatePermissions: a summary is a condensed copy of a whole
// conversation. It gets the same owner-only treatment as the transcript it
// summarises — on creation, and by migration for the summaries an earlier
// build already wrote 0644 into a 0755 directory.
func TestStorePrivatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "summaries")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Put(Summary{ThreadID: "t-1", Body: "what the model was shown"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	di, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("summary dir mode = %o, want 700", got)
	}
	fi, err := os.Lstat(filepath.Join(dir, "t-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("summary file mode = %o, want 600", got)
	}
}

func TestStoreMigratesWorldReadableSummaries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "summaries")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "t-old.json")
	if err := os.WriteFile(old, []byte(`{"threadId":"t-old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	di, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o after open, want 700", got)
	}
	fi, err := os.Lstat(old)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("existing summary mode = %o after open, want 600", got)
	}
}

// TestConcurrentPutNeverPublishesASplicedSummary pins audit F24's fix: Put used
// to stage through a FIXED `<thread>.json.tmp`, so two writers for the same
// thread interleaved their bytes on one file and the rename published the
// splice. A summary is the entire memory a resumed session is given, so a
// spliced one is not cosmetic damage.
//
// The bodies are deliberately long and made of a single repeated rune each, so
// any interleaving shows up as a body containing both runes — which is what the
// assertion looks for, rather than merely "it parses".
func TestConcurrentPutNeverPublishesASplicedSummary(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "summaries"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	const (
		writers = 8
		rounds  = 25
		bodyLen = 64 * 1024 // comfortably past one write() worth of buffering
	)
	runes := []string{"a", "b", "c", "d", "e", "f", "g", "h"}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			body := strings.Repeat(runes[w], bodyLen)
			for r := 0; r < rounds; r++ {
				// Every writer targets the SAME thread id: that is the collision.
				if err := s.Put(Summary{ThreadID: "t-hot", Body: body}); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}(w)
	}
	// Read concurrently too: a reader must never observe a half-written file.
	var readWG sync.WaitGroup
	stop := make(chan struct{})
	readWG.Add(1)
	go func() {
		defer readWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := s.Get("t-hot")
			if err != nil {
				t.Errorf("Get saw a torn file: %v", err)
				return
			}
			if got != nil {
				assertUniformBody(t, got.Body, bodyLen)
			}
		}
	}()
	wg.Wait()
	close(stop)
	readWG.Wait()

	final, err := s.Get("t-hot")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final == nil {
		t.Fatal("no summary published")
	}
	assertUniformBody(t, final.Body, bodyLen)

	// No staging litter left behind: every unique tmp is renamed or removed.
	entries, err := os.ReadDir(filepath.Join(s.dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("left a staging file behind: %s", e.Name())
		}
	}
}

// assertUniformBody fails when body is not exactly bodyLen copies of one rune —
// i.e. when two writers' bytes were spliced together.
func assertUniformBody(t *testing.T, body string, bodyLen int) {
	t.Helper()
	if len(body) != bodyLen {
		t.Fatalf("body length = %d, want %d (a spliced write)", len(body), bodyLen)
	}
	first := body[0]
	for i := 0; i < len(body); i++ {
		if body[i] != first {
			t.Fatalf("body mixes %q and %q at offset %d — two writers were spliced",
				first, body[i], i)
		}
	}
}
