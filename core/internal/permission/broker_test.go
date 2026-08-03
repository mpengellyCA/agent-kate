package permission

import (
	"testing"
	"time"
)

func TestCancelThreadWakesOnlyThatThreadsPromptsWithRefusal(t *testing.T) {
	b := New()
	_, first := b.OpenForThread("thread-a")
	_, second := b.OpenForThread("thread-a")
	_, other := b.OpenForThread("thread-b")

	if got := b.CancelThread("thread-a"); got != 2 {
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
	if got := b.CancelThread("thread-a"); got != 0 {
		t.Fatalf("second CancelThread count = %d, want 0", got)
	}
}

func TestResolveRemovesThreadOwnership(t *testing.T) {
	b := New()
	id, ch := b.OpenForThread("thread-a")
	if !b.Resolve(id, Decision{Allow: true}) {
		t.Fatal("Resolve returned false")
	}
	<-ch
	if got := b.CancelThread("thread-a"); got != 0 {
		t.Fatalf("CancelThread after Resolve = %d, want 0", got)
	}
}
