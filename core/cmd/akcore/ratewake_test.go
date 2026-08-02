package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentkate/internal/schedule"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// Plan 28 §Phase 2 — rate-window auto-resume, and the rule that makes it
// legitimate:
//
//	A scheduled or automatic action never carries more authority than the
//	human granted the thread.
//
// The feature exists because the alternative people actually reach for is a
// systemd timer running `claude --resume … --permission-mode bypassPermissions`
// when the window reopens — work resuming with MORE authority than the human
// granted, at an hour when nobody is watching. The plan requires this rule to
// be enforced by a test rather than a comment. These are that test: one
// behavioural (the resumed launch carries the record's own mode) and one
// source-level (no path in the scheduler surface can even express another one).

// parkedRecord is a dormant thread that stalled on the account's usage window:
// a real worktree on disk, a session to resume, and one permission mode.
func parkedRecord(t *testing.T, threadID, mode string) session.Record {
	t.Helper()
	dir := t.TempDir()
	return session.Record{
		ThreadID: threadID, SessionID: "s-" + threadID, Project: dir,
		Worktree:       worktree.Worktree{ThreadID: threadID, Path: dir},
		Backend:        "fake",
		PermissionMode: mode,
		Created:        time.Now(),
		Status:         session.StatusDormant,
	}
}

// THE RULE, behaviourally. A thread the human left on "plan" resumes on "plan".
// Nothing in the wake carries a mode, so the only mode available to the resume
// is the one on the record — and this test fails the moment that stops being
// true.
func TestScheduledResumeUsesRecordPermissionMode(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	rec := parkedRecord(t, "t-parked", "plan")
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fireRateWake(d, schedule.Wake{
		ThreadID:    rec.ThreadID,
		At:          time.Now(),
		Fingerprint: authorityFingerprint(rec),
	})

	spec := fake.spec()
	if spec.ThreadID != rec.ThreadID {
		t.Fatalf("the wake did not resume the thread at all (spec %+v)", spec)
	}
	if spec.PermissionMode != "plan" {
		t.Fatalf("an unattended resume launched with permissionMode %q, but the "+
			"human granted this thread %q — a scheduled action must never carry "+
			"more authority than the human granted", spec.PermissionMode, "plan")
	}
	if !spec.Resume || spec.SessionID != rec.SessionID {
		t.Errorf("the auto-resume did not re-attach the thread's own session: %+v", spec)
	}
}

// The default mode is just as much a decision as a restrictive one: a resume
// that "helpfully" upgraded a default thread would be the same escalation.
func TestScheduledResumeKeepsTheDefaultMode(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	rec := parkedRecord(t, "t-default", "default")
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fireRateWake(d, schedule.Wake{ThreadID: rec.ThreadID, Fingerprint: authorityFingerprint(rec)})

	if got := fake.spec().PermissionMode; got != "default" {
		t.Fatalf("auto-resume launched with %q, not the record's %q", got, "default")
	}
}

// The authority moved while the thread was parked. The wake armed under the old
// answer is not the wake the human would arm now, so it must not fire — and it
// must SAY it did not, rather than leaving someone who was told "resumes at
// 14:37" staring at an agent that never moved.
func TestScheduledResumeSkipsWhenThePermissionModeChanged(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	rec := parkedRecord(t, "t-moved", "plan")
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Armed while the thread was on "default"; the human has since moved it.
	wake := schedule.Wake{ThreadID: rec.ThreadID, Fingerprint: "default"}

	reason := rateWakeSkipReason(d, wake)
	if reason == "" {
		t.Fatal("a resume armed under a different permission mode fired anyway")
	}
	if !strings.Contains(reason, "when to ask") {
		t.Errorf("the skip reason does not tell the user what changed: %q", reason)
	}

	fireRateWake(d, wake)
	if spec := fake.spec(); spec.ThreadID != "" {
		t.Fatalf("the skipped wake launched something anyway: %+v", spec)
	}
}

// The other three ways a wake comes due on something it can no longer resume.
// Each has to be explained, not swallowed.
func TestScheduledResumeExplainsEverySkip(t *testing.T) {
	sessions := testSessions(t)
	d, _ := personaDeps(t, sessions)

	if got := rateWakeSkipReason(d, schedule.Wake{ThreadID: "t-ghost"}); got == "" {
		t.Error("a wake for a thread that no longer exists reported no reason")
	} else if !strings.Contains(got, "no longer") {
		t.Errorf("unhelpful reason for a vanished thread: %q", got)
	}

	noSession := parkedRecord(t, "t-nosession", "default")
	noSession.SessionID = ""
	if err := sessions.Put(noSession); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := rateWakeSkipReason(d, schedule.Wake{
		ThreadID: noSession.ThreadID, Fingerprint: authorityFingerprint(noSession),
	}); !strings.Contains(got, "session") {
		t.Errorf("a thread with no session to resume reported %q", got)
	}

	wallet := parkedRecord(t, "t-wallet", "default")
	wallet.ProviderBaseURL = "https://api.example.invalid"
	wallet.ProviderEnvVar = "AK_TEST_UNSET_PROVIDER_KEY"
	if err := sessions.Put(wallet); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := rateWakeSkipReason(d, schedule.Wake{
		ThreadID: wallet.ThreadID, Fingerprint: authorityFingerprint(wallet),
	}); !strings.Contains(got, "wallet") {
		t.Errorf("a wallet-held key resumed unattended (or reported %q) — there is "+
			"no window open at 3am to unlock it", got)
	}
}

// The arm-time authority snapshot comes from the RECORD, never from the event.
// If it came from the wire, an engine (or anything that can shape an event)
// would choose what a later resume is compared against.
func TestArmingSnapshotsTheRecordsOwnMode(t *testing.T) {
	sessions := testSessions(t)
	d, _ := personaDeps(t, sessions)
	rec := parkedRecord(t, "t-arm", "acceptEdits")
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	waker := schedule.NewRateWaker(schedule.Config{})
	defer waker.Stop()
	info := fmt.Sprintf(`{"status":"rejected","rateLimitType":"five_hour","resetsAt":%d}`,
		time.Now().Add(time.Hour).Unix())

	noteRateLimit(d, waker, rec.ThreadID, json.RawMessage(info))

	armed := waker.Armed()
	if len(armed) != 1 {
		t.Fatalf("a rejected window armed %d wakes: %+v", len(armed), armed)
	}
	if armed[0].Fingerprint != "acceptEdits" {
		t.Fatalf("armed against fingerprint %q, not the record's mode %q",
			armed[0].Fingerprint, "acceptEdits")
	}
}

// THE RULE, source-level — the half a behavioural test cannot give you, because
// the escalation this feature exists to avoid is one line away at all times.
//
// The scheduler surface may not WRITE a permission mode anywhere, and the word
// the workaround uses may not appear in it at all. Grepping is not enough (a
// comment would trip it and a differently spelled assignment would not), so
// this parses the files and looks at what the code actually does.
func TestNoSchedulerPathCanSetBypassPermissions(t *testing.T) {
	files := schedulerSurface(t)
	if len(files) < 3 {
		t.Fatalf("the scheduler surface is %d files; this test is looking in the "+
			"wrong place and would pass on an empty set", len(files))
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				// No literal may name a permission mode — this is the string
				// the systemd workaround passes, and it must be unspellable
				// here.
				if node.Kind == token.STRING &&
					strings.Contains(strings.ToLower(node.Value), "bypass") {
					t.Errorf("%s:%d: the scheduler surface names %s — a scheduled "+
						"action must never carry more authority than the human granted",
						filepath.Base(path), fset.Position(node.Pos()).Line, node.Value)
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && isModeField(sel.Sel.Name) {
						t.Errorf("%s:%d: the scheduler surface assigns %s — the "+
							"resumed thread's authority must come from its record, "+
							"never from the schedule", filepath.Base(path),
							fset.Position(node.Pos()).Line, sel.Sel.Name)
					}
				}
			case *ast.KeyValueExpr:
				// A composite literal (a StartSpec, say) setting the mode.
				if key, ok := node.Key.(*ast.Ident); ok && isModeField(key.Name) {
					t.Errorf("%s:%d: the scheduler surface builds a value with %s set",
						filepath.Base(path), fset.Position(node.Pos()).Line, key.Name)
				}
			}
			return true
		})
	}
}

func isModeField(name string) bool {
	return name == "PermissionMode" || name == "permissionMode"
}

// schedulerSurface is every non-test source file a scheduled action runs
// through before it reaches the ordinary resume path.
func schedulerSurface(t *testing.T) []string {
	t.Helper()
	out := []string{filepath.Join(".", "ratewake.go")}
	pkg := filepath.Join("..", "..", "internal", "schedule")
	entries, err := os.ReadDir(pkg)
	if err != nil {
		t.Fatalf("the schedule package is not where this test looks (%s): %v", pkg, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(pkg, name))
	}
	for _, path := range out {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("scheduler surface file missing: %s", path)
		}
	}
	return out
}
