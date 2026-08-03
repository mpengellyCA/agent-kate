package permission

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingObserver struct {
	mu       sync.Mutex
	opened   []Request
	resolved []Resolution
}

func (o *recordingObserver) PermissionOpened(req Request) {
	o.mu.Lock()
	o.opened = append(o.opened, req)
	o.mu.Unlock()
}

func (o *recordingObserver) PermissionResolved(res Resolution) {
	o.mu.Lock()
	o.resolved = append(o.resolved, res)
	o.mu.Unlock()
}

func (o *recordingObserver) snapshot() ([]Request, []Resolution) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Request(nil), o.opened...), append([]Resolution(nil), o.resolved...)
}

func TestCancelThreadWakesOnlyThatThreadsPromptsWithRefusal(t *testing.T) {
	b := New()
	_, first := b.Open("thread-a", "Bash", "Approve a shell command", time.Minute)
	_, second := b.Open("thread-a", "Bash", "Approve a shell command", time.Minute)
	_, other := b.Open("thread-b", "Bash", "Approve a shell command", time.Minute)

	if got := b.CancelThread("thread-a", Interrupted); got != 2 {
		t.Fatalf("CancelThread count = %d, want 2", got)
	}
	for i, ch := range []chan Decision{first, second} {
		select {
		case dec := <-ch:
			if dec.Allow || len(dec.UpdatedInput) != 0 {
				t.Fatalf("cancelled decision %d = %+v, want fail-closed zero decision", i, dec)
			}
		case <-time.After(time.Second):
			t.Fatalf("cancelled prompt %d did not wake", i)
		}
	}
	select {
	case <-other:
		t.Fatal("CancelThread(thread-a) woke thread-b prompt")
	default:
	}
	if got := b.CancelThread("thread-a", Interrupted); got != 0 {
		t.Fatalf("second CancelThread count = %d, want 0", got)
	}
}

func TestBrokerPublishesSameSafeTerminalShapeForEveryOutcome(t *testing.T) {
	b := New()
	observer := &recordingObserver{}
	b.SetObserver(observer)

	resolved, resolvedCh := b.Open("thread-a", "Bash", "Approve a shell command", time.Minute)
	timedOut, _ := b.Open("thread-b", "Bash", "Approve a shell command", time.Minute)
	interrupted, interruptedCh := b.Open("thread-c", "Bash", "Approve a shell command", time.Minute)
	exited, exitedCh := b.Open("thread-d", "Bash", "Approve a shell command", time.Minute)

	if _, ok := b.Resolve(resolved.ID, Decision{Allow: true, ResolvedBy: "desktop"}); !ok {
		t.Fatal("Resolve returned false")
	}
	if dec := <-resolvedCh; !dec.Allow {
		t.Fatal("resolved decision did not reach waiter")
	}
	b.Close(timedOut.ID, TimedOut)
	b.CancelThread("thread-c", Interrupted)
	b.CancelThread("thread-d", Exited)
	if dec := <-interruptedCh; dec.Allow {
		t.Fatal("interrupted prompt was approved")
	}
	if dec := <-exitedCh; dec.Allow {
		t.Fatal("exited prompt was approved")
	}

	opened, terminals := observer.snapshot()
	if len(opened) != 4 || len(terminals) != 4 {
		t.Fatalf("observer saw %d opens and %d terminals, want 4 each", len(opened), len(terminals))
	}
	got := map[string]TerminalReason{}
	for _, terminal := range terminals {
		if terminal.Request.Summary != "Approve a shell command" {
			t.Fatalf("terminal leaked or changed summary: %#v", terminal.Request)
		}
		got[terminal.Request.ID] = terminal.Reason
	}
	for id, want := range map[string]TerminalReason{
		resolved.ID: Resolved, timedOut.ID: TimedOut, interrupted.ID: Interrupted, exited.ID: Exited,
	} {
		if got[id] != want {
			t.Errorf("terminal reason for %s = %q, want %q", id, got[id], want)
		}
	}
}

func TestRemoteSafeSummaryAndDetailNeverRetainOrdinaryToolInput(t *testing.T) {
	secret := "token=super-secret-value"
	input := json.RawMessage(`{"command":"curl https://example.test/?token=super-secret-value","nested":{"password":"super-secret-value"}}`)
	if got := Summary("Bash"); strings.Contains(got, secret) || strings.Contains(got, "curl") {
		t.Fatalf("summary leaked tool input: %q", got)
	}
	if got := RenderableDetail("Bash", input); got.Plan != "" || len(got.Questions) != 0 {
		t.Fatalf("ordinary tool got remote detail: %#v", got)
	}

	plan := RenderableDetail("ExitPlanMode", json.RawMessage(`{"plan":"# Safe plan","token":"super-secret-value"}`))
	if plan.Plan != "# Safe plan" || len(plan.Questions) != 0 {
		t.Fatalf("plan allowlist = %#v", plan)
	}
	questions := RenderableDetail("AskUserQuestion", json.RawMessage(`{"questions":[{"question":"Continue?","options":[{"label":"Yes"}]}],"token":"super-secret-value"}`))
	if string(questions.Questions) != `[{"question":"Continue?","options":[{"label":"Yes"}]}]` || questions.Plan != "" {
		t.Fatalf("question allowlist = %#v", questions)
	}
}

func TestDesktopOnlyCoworkBrokerEntryIsNeverMirrored(t *testing.T) {
	b := New()
	observer := &recordingObserver{}
	b.SetObserver(observer)
	id, ch := b.OpenLocal()
	if _, ok := b.Resolve(id, Decision{Allow: true, ResolvedBy: "desktop"}); !ok {
		t.Fatal("could not resolve desktop-only prompt")
	}
	if decision := <-ch; !decision.Allow {
		t.Fatal("desktop-only decision did not reach local waiter")
	}
	opened, terminal := observer.snapshot()
	if len(opened) != 0 || len(terminal) != 0 {
		t.Fatalf("Cowork consent reached remote observer: opens=%#v terminal=%#v", opened, terminal)
	}
}

func TestPendingRemoteOmitsDesktopOnlyRequests(t *testing.T) {
	b := New()
	localID, _ := b.OpenLocal()
	remoteReq, _ := b.Open("thread-remote", "Bash", "Approve a shell command", time.Minute)
	got := b.PendingRemote()
	if len(got) != 1 || got[0].ID != remoteReq.ID {
		t.Fatalf("PendingRemote = %#v, want only %s", got, remoteReq.ID)
	}
	if _, ok := b.GetRemote(localID); ok {
		t.Fatal("desktop-only request was visible to the remote surface")
	}
}
