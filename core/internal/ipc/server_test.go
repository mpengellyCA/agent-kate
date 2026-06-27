package ipc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it is true or the deadline elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// TestDispatchBoundIsPerConn verifies that a single connection cannot make the
// server spawn more than maxInFlightPerConn concurrent dispatch goroutines: once
// that many handlers are in flight the reader backpressures (stops dispatching)
// until a handler completes and frees a slot.
func TestDispatchBoundIsPerConn(t *testing.T) {
	sock := t.TempDir() + "/ipc.sock"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(sock, log)

	var entered atomic.Int32
	release := make(chan struct{})
	srv.Handle("block", func(_ context.Context, _ json.RawMessage) (any, error) {
		entered.Add(1)
		<-release // hold the dispatch goroutine (and its semaphore slot)
		return map[string]any{"ok": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	// Dial once the listener is up.
	var c net.Conn
	if !waitFor(t, 2*time.Second, func() bool {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		c = conn
		return true
	}) {
		t.Fatal("server never accepted a connection")
	}
	defer c.Close()

	// Fire more requests than the per-conn cap. The reader will dispatch the
	// first maxInFlightPerConn (each blocks in the handler) and then block
	// acquiring a slot for the rest.
	const extra = 2
	total := maxInFlightPerConn + extra
	for i := 0; i < total; i++ {
		id := json.RawMessage(strconv.Itoa(i))
		b, err := json.Marshal(Frame{JSONRPC: "2.0", ID: &id, Method: "block"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(append(b, '\n')); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}

	// Exactly maxInFlightPerConn handlers should start; the rest are held back.
	if !waitFor(t, 2*time.Second, func() bool {
		return entered.Load() == int32(maxInFlightPerConn)
	}) {
		t.Fatalf("in-flight handlers = %d, want %d", entered.Load(), maxInFlightPerConn)
	}
	// The bound holds: extras cannot be dispatched while slots stay occupied.
	// (Deterministic — no slot frees until we close release, so this can't race.)
	time.Sleep(50 * time.Millisecond)
	if got := entered.Load(); got != int32(maxInFlightPerConn) {
		t.Fatalf("bound breached: in-flight = %d, want %d", got, maxInFlightPerConn)
	}

	// Freeing the held handlers lets the backpressured extras through.
	close(release)
	if !waitFor(t, 2*time.Second, func() bool {
		return entered.Load() == int32(total)
	}) {
		t.Fatalf("after release in-flight = %d, want %d", entered.Load(), total)
	}
}
