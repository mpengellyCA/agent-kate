package agent

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestTurnTrackerSnapshotAndBusyDoNotAllocateUnknownState(t *testing.T) {
	tt := NewTurnTracker()
	if tt.Busy("unknown") {
		t.Fatal("unknown thread reported busy")
	}
	if got := tt.Snapshot(); len(got) != 0 {
		t.Fatalf("empty tracker snapshot = %#v", got)
	}
	tt.mu.Lock()
	n := len(tt.threads)
	tt.mu.Unlock()
	if n != 0 {
		t.Fatalf("read-only busy check allocated %d entries", n)
	}
}

func TestTurnTrackerPublishesOnlyBusyEdgesOutsideItsLock(t *testing.T) {
	tt := NewTurnTracker()
	var mu sync.Mutex
	var edges []bool
	done := make(chan struct{}, 4)
	tt.SetOnChange(func(threadID string, busy bool) {
		if threadID != "t-1" {
			t.Errorf("edge thread = %q", threadID)
		}
		// Re-entering proves publication is outside tt.mu.
		if got := tt.Busy(threadID); got != busy {
			t.Errorf("Busy during callback = %v, want %v", got, busy)
		}
		mu.Lock()
		edges = append(edges, busy)
		mu.Unlock()
		done <- struct{}{}
	})

	tt.TurnQueued("t-1")
	tt.TurnQueued("t-1") // still busy: no second edge
	tt.Observe("t-1", json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"text","text":"one token"}]}}`))
	tt.Observe("t-1", json.RawMessage(`{"type":"result"}`)) // still busy
	tt.Observe("t-1", json.RawMessage(`{"type":"result"}`)) // idle edge

	for want := 0; want < 2; want++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for busy edge %d", want)
		}
	}
	mu.Lock()
	got := append([]bool(nil), edges...)
	mu.Unlock()
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("busy edges = %v, want [true false]", got)
	}
}

func TestTurnTrackerReapStillPublishesFinalIdleEdge(t *testing.T) {
	tt := NewTurnTracker()
	edges := make(chan bool, 2)
	tt.SetOnChange(func(_ string, busy bool) { edges <- busy })
	tt.TurnQueued("t-gone")
	<-edges
	tt.ObserveLifecycle("t-gone", "exited")
	select {
	case busy := <-edges:
		if busy {
			t.Fatal("terminal lifecycle published busy")
		}
	case <-time.After(time.Second):
		t.Fatal("terminal lifecycle did not publish idle edge")
	}
	if tt.Busy("t-gone") {
		t.Fatal("reaped thread remains busy")
	}
}
