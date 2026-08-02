package kde

import (
	"testing"
	"time"
)

// The activation watch is a RESIDENT script that reports many times on one nonce. The
// one-shot rendezvous registerNonce hands out (cap 1, with Report dropping rather than
// blocking) would have swallowed every activation after the first — i.e. exactly the
// focus change a running injection has to be aborted for. Pin the buffered variant.
func TestRegisterNonceBufKeepsRepeatedReports(t *testing.T) {
	c := &Client{reports: map[string]chan string{}}
	ch := c.registerNonceBuf("n1", 8)
	defer c.unregisterNonce("n1")

	r := (*reporter)(c)
	for _, p := range []string{"a", "b", "c"} {
		if err := r.Report("n1", p); err != nil {
			t.Fatalf("Report: %v", err)
		}
	}
	for _, want := range []string{"a", "b", "c"} {
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("report %q was dropped — a resident watcher cannot miss activations", want)
		}
	}
}

func TestRegisterNonceStaysOneShot(t *testing.T) {
	// The one-shot callers (ListWindows, ActivateWindow) must keep their existing
	// drop-duplicates behaviour: a second reply is noise, not a second answer.
	c := &Client{reports: map[string]chan string{}}
	ch := c.registerNonce("n2")
	defer c.unregisterNonce("n2")
	r := (*reporter)(c)
	_ = r.Report("n2", "first")
	_ = r.Report("n2", "second")
	if got := <-ch; got != "first" {
		t.Fatalf("got %q, want %q", got, "first")
	}
	select {
	case got := <-ch:
		t.Fatalf("one-shot nonce delivered a second payload %q", got)
	default:
	}
}

// A watch dropped without Stop() used to park its pump on <-raw forever;
// unregisterNonce now CLOSES the rendezvous so a parked receiver wakes and
// exits (audit F65). This is the pump's exit seam, tested at the channel level
// because the pump itself needs a live KWin to construct. Fails if the close
// is removed: the receive never fires and the timeout trips.
func TestUnregisterNonceReleasesParkedReceiver(t *testing.T) {
	c := &Client{reports: map[string]chan string{}}
	ch := c.registerNonceBuf("n3", 8)

	woke := make(chan struct{})
	go func() {
		for range ch { // drains until the channel is CLOSED
		}
		close(woke)
	}()

	c.unregisterNonce("n3")
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("unregisterNonce did not close the rendezvous; a pump parked on it leaks")
	}
	// A late Report against the closed rendezvous must be a no-op, not a
	// send-on-closed-channel panic; and a second unregister must be idempotent.
	if err := (*reporter)(c).Report("n3", "late"); err != nil {
		t.Fatalf("Report after unregister: %v", err)
	}
	c.unregisterNonce("n3")
}

// FAIL CLOSED: with no session bus there is no watch, and the caller must be told so
// rather than handed a channel that never fires (which would read as "focus never
// changed" and let a 30 s script run unsupervised).
func TestWatchActiveWindowFailsClosedWithoutBus(t *testing.T) {
	var c *Client
	if _, err := c.WatchActiveWindow(10 * time.Millisecond); err == nil {
		t.Fatal("a nil client must refuse to establish an activation watch")
	}
	closed := &Client{reports: map[string]chan string{}}
	if _, err := closed.WatchActiveWindow(10 * time.Millisecond); err == nil {
		t.Fatal("a client with no connection must refuse to establish an activation watch")
	}
}
