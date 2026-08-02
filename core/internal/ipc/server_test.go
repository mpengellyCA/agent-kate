package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// privateSocketDir returns a 0700 directory to bind test sockets in. Serve
// refuses a group/world-accessible socket directory (audit F20a) and t.TempDir
// hands out 0755 subdirectories, so tests must model the real deployment:
// $XDG_RUNTIME_DIR, which is 0700.
func privateSocketDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() + "/run"
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

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
	sock := privateSocketDir(t) + "/ipc.sock"
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

// startServer runs srv until the test ends and waits for its socket to accept.
func startServer(t *testing.T, srv *Server, sock string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	if !waitFor(t, 2*time.Second, func() bool {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}) {
		t.Fatal("server never accepted a connection")
	}
}

// dialServer starts srv on a temp socket and returns a connected client.
func dialServer(t *testing.T, srv *Server, sock string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()

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
	t.Cleanup(func() { c.Close() })
	return c
}

// writeOversizeFrame writes a single line longer than maxFrameBytes, starting
// with the given prefix (so a test can control whether an id is recoverable).
func writeOversizeFrame(t *testing.T, c net.Conn, prefix string) {
	t.Helper()
	// Bound every write: a regression that stops draining the oversize line must
	// fail the test, not hang it until the package timeout.
	if err := c.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.SetWriteDeadline(time.Time{}) }()
	if _, err := c.Write([]byte(prefix)); err != nil {
		t.Fatalf("write oversize prefix: %v", err)
	}
	pad := make([]byte, 1<<20)
	for i := range pad {
		pad[i] = 'x'
	}
	for written := 0; written <= maxFrameBytes; written += len(pad) {
		if _, err := c.Write(pad); err != nil {
			t.Fatalf("write oversize padding: %v", err)
		}
	}
	if _, err := c.Write([]byte("\"}\n")); err != nil {
		t.Fatalf("terminate oversize frame: %v", err)
	}
}

// TestOversizeFrameIsSurvivable is the regression test for the failure mode in
// which one over-long line ended the read loop, dropped the only client, and so
// stopped the entire core: the connection must stay usable and the sender must
// get an error reply naming the cap.
func TestOversizeFrameIsSurvivable(t *testing.T) {
	sock := privateSocketDir(t) + "/ipc.sock"
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Handle("echo", func(_ context.Context, p json.RawMessage) (any, error) {
		return map[string]any{"got": string(p)}, nil
	})
	var gone atomic.Bool
	srv.OnAllClientsGone(func() { gone.Store(true) })

	c := dialServer(t, srv, sock)
	writeOversizeFrame(t, c, `{"jsonrpc":"2.0","id":7,"method":"echo","params":"`)

	dec := json.NewDecoder(c)
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	var errResp Frame
	if err := dec.Decode(&errResp); err != nil {
		t.Fatalf("no reply to the oversize frame: %v", err)
	}
	if errResp.ID == nil || string(*errResp.ID) != "7" {
		t.Fatalf("error reply id = %v, want 7", errResp.ID)
	}
	if errResp.Error == nil || !strings.Contains(errResp.Error.Message, "frame too large") {
		t.Fatalf("error reply = %+v, want a frame-too-large error", errResp.Error)
	}
	if !strings.Contains(errResp.Error.Message, "cap 16 MiB") {
		t.Fatalf("error message %q does not name the cap", errResp.Error.Message)
	}

	// The connection must still serve ordinary traffic.
	id := json.RawMessage("8")
	b, err := json.Marshal(Frame{JSONRPC: "2.0", ID: &id, Method: "echo", Params: json.RawMessage(`"hi"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatalf("write follow-up request: %v", err)
	}
	var ok Frame
	if err := dec.Decode(&ok); err != nil {
		t.Fatalf("no reply to the follow-up request: %v", err)
	}
	if ok.ID == nil || string(*ok.ID) != "8" {
		t.Fatalf("follow-up reply id = %v, want 8", ok.ID)
	}
	if ok.Error != nil {
		t.Fatalf("follow-up failed: %+v", ok.Error)
	}
	if !strings.Contains(string(ok.Result), `\"hi\"`) {
		t.Fatalf("follow-up result = %s", ok.Result)
	}
	if gone.Load() {
		t.Fatal("the client was disconnected by the oversize frame")
	}
}

// TestOversizeFrameWithoutRecoverableID covers the case where the discarded
// prefix carries no id: nothing is sent back, but the connection survives.
func TestOversizeFrameWithoutRecoverableID(t *testing.T) {
	sock := privateSocketDir(t) + "/ipc.sock"
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Handle("echo", func(_ context.Context, p json.RawMessage) (any, error) {
		return map[string]any{"got": string(p)}, nil
	})

	c := dialServer(t, srv, sock)
	writeOversizeFrame(t, c, `{"jsonrpc":"2.0","method":"echo","params":"`)

	id := json.RawMessage("1")
	b, err := json.Marshal(Frame{JSONRPC: "2.0", ID: &id, Method: "echo", Params: json.RawMessage(`"after"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatalf("write follow-up request: %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// The next frame on the wire must be the follow-up's reply — proof that no
	// error frame was invented for the id-less oversize line.
	var resp Frame
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		t.Fatalf("no reply after the id-less oversize frame: %v", err)
	}
	if resp.ID == nil || string(*resp.ID) != "1" || resp.Error != nil {
		t.Fatalf("unexpected reply: id=%v err=%+v", resp.ID, resp.Error)
	}
}

// TestFrameIDExtraction pins the lexical id recovery used on truncated prefixes.
func TestFrameIDExtraction(t *testing.T) {
	cases := []struct {
		raw  string
		want string // "" means no id
	}{
		{`{"jsonrpc":"2.0","id":42,"method":"x","params":{"blob":"aaa`, "42"},
		{`{"jsonrpc":"2.0","id": "abc-1" ,"method":"x"`, `"abc-1"`},
		{`{"jsonrpc":"2.0","method":"x","params":{"blob":"aaa`, ""},
		{`{"jsonrpc":"2.0","id":`, ""},
		// A probe cut can sever the id mid-token; replying with the fragment
		// would resolve an unrelated in-flight request.
		{`{"jsonrpc":"2.0","id":123456`, ""},
		{`{"jsonrpc":"2.0","id":"abc`, ""},
		// An id inside params is the payload's, not the request's.
		{`{"jsonrpc":"2.0","method":"x","params":{"id":99,"blob":"aaa`, ""},
		{`{"jsonrpc":"2.0","id":5,"method":"x","params":{"id":99,"blob":"aaa`, "5"},
	}
	for _, tc := range cases {
		got := frameID([]byte(tc.raw))
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("frameID(%q) = %s, want none", tc.raw, *got)
		case tc.want != "" && got == nil:
			t.Errorf("frameID(%q) = none, want %s", tc.raw, tc.want)
		case tc.want != "" && string(*got) != tc.want:
			t.Errorf("frameID(%q) = %s, want %s", tc.raw, *got, tc.want)
		}
	}
}

// nonRepeatingPayload builds n JSON-safe bytes with no repeating block, so a
// byte-for-byte comparison catches a mis-stitched chunk boundary that a run of
// identical padding would hide.
func nonRepeatingPayload(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	b := make([]byte, n)
	x := uint64(0x2545F4914F6CDD1D)
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = alphabet[x&63]
	}
	return string(b)
}

// TestLargeValidFrameRoundTrip covers the accumulation path: a frame far larger
// than readBufferBytes but under the cap is stitched across many ErrBufferFull
// reads and must arrive intact, byte for byte, in both directions.
func TestLargeValidFrameRoundTrip(t *testing.T) {
	sock := privateSocketDir(t) + "/ipc.sock"
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Handle("echo", func(_ context.Context, p json.RawMessage) (any, error) {
		var s string
		if err := json.Unmarshal(p, &s); err != nil {
			return nil, err
		}
		return s, nil
	})

	c := dialServer(t, srv, sock)
	payload := nonRepeatingPayload(5 << 20)

	id := json.RawMessage("11")
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Frame{JSONRPC: "2.0", ID: &id, Method: "echo", Params: pb})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatalf("write large frame: %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}

	var resp Frame
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		t.Fatalf("no reply to the large frame: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("large frame rejected: %+v", resp.Error)
	}
	if resp.ID == nil || string(*resp.ID) != "11" {
		t.Fatalf("reply id = %v, want 11", resp.ID)
	}
	var got string
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("echoed length = %d, want %d", len(got), len(payload))
	}
	if got != payload {
		for i := range got {
			if got[i] != payload[i] {
				t.Fatalf("payload differs at byte %d: got %q want %q", i, got[i], payload[i])
			}
		}
	}
}

// TestReadFrameBoundaries drives readFrame directly over the edges the socket
// tests cannot reach cheaply: the exact cap, CRLF, and an unterminated line.
func TestReadFrameBoundaries(t *testing.T) {
	// The newline counts toward the cap, so the largest accepted payload is
	// maxFrameBytes-1 and one byte more is rejected.
	t.Run("at cap", func(t *testing.T) {
		body := strings.Repeat("a", maxFrameBytes-1)
		fr := newFrameReader(strings.NewReader(body + "\n"))
		line, over, err := fr.readFrame()
		if err != nil || over != 0 {
			t.Fatalf("readFrame = (%d bytes, over %d, err %v), want accepted", len(line), over, err)
		}
		if len(line) != maxFrameBytes-1 {
			t.Fatalf("line length = %d, want %d", len(line), maxFrameBytes-1)
		}
	})
	t.Run("one past cap", func(t *testing.T) {
		body := strings.Repeat("a", maxFrameBytes)
		fr := newFrameReader(strings.NewReader(body + "\n"))
		line, over, err := fr.readFrame()
		if err != nil {
			t.Fatalf("readFrame err = %v", err)
		}
		if over != maxFrameBytes {
			t.Fatalf("oversize = %d, want %d", over, maxFrameBytes)
		}
		if len(line) != idProbeBytes {
			t.Fatalf("retained head = %d bytes, want %d", len(line), idProbeBytes)
		}
	})
	t.Run("crlf and reuse", func(t *testing.T) {
		fr := newFrameReader(strings.NewReader("one\r\ntwo\n\nthree"))
		for _, want := range []string{"one", "two", ""} {
			line, over, err := fr.readFrame()
			if err != nil || over != 0 {
				t.Fatalf("readFrame = (%q, %d, %v)", line, over, err)
			}
			if string(line) != want {
				t.Fatalf("line = %q, want %q", line, want)
			}
		}
		// Unterminated trailing line: returned whole, EOF only on the next call.
		line, over, err := fr.readFrame()
		if err != nil || over != 0 || string(line) != "three" {
			t.Fatalf("trailing line = (%q, %d, %v), want (\"three\", 0, nil)", line, over, err)
		}
		if _, _, err := fr.readFrame(); !errors.Is(err, io.EOF) {
			t.Fatalf("after trailing line err = %v, want EOF", err)
		}
	})
}

// TestClientSurvivesOversizeFrame is the client-side mirror of the server test:
// an over-cap core→bridge response (a huge cowork.screenshot result) must not
// end the read loop, or every later Call would block for its full timeout.
func TestClientSurvivesOversizeFrame(t *testing.T) {
	sock := privateSocketDir(t) + "/ipc.sock"
	srv := NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Handle("big", func(_ context.Context, _ json.RawMessage) (any, error) {
		return strings.Repeat("y", maxFrameBytes+4096), nil
	})
	srv.Handle("small", func(_ context.Context, _ json.RawMessage) (any, error) {
		return "pong", nil
	})
	startServer(t, srv, sock)

	cl, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { cl.Close() })

	// The oversize reply is discarded, so this call can only end in its timeout.
	var sink string
	if err := cl.CallTimeout("big", nil, &sink, 2*time.Second); err == nil {
		t.Fatal("oversize reply was accepted, want a timeout")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("big call err = %v, want a timeout", err)
	}

	// The client must still be usable: proof the read loop survived.
	var out string
	if err := cl.CallTimeout("small", nil, &out, 5*time.Second); err != nil {
		t.Fatalf("follow-up call after oversize frame: %v", err)
	}
	if out != "pong" {
		t.Fatalf("follow-up result = %q, want pong", out)
	}
}

// TestHandlerContextCancelledOnDisconnect pins the disconnect-release contract
// (audit F17): a handler parked in a long wait (agent.wait parks for up to an
// hour) must be released when the connection that asked for it goes away, not
// left to run out its deadline. Without a per-connection context, every
// stop/restart of a waiting controller bridge leaks one parked goroutine.
func TestHandlerContextCancelledOnDisconnect(t *testing.T) {
	sock := privateSocketDir(t) + "/ipc.sock"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(sock, log)

	entered := make(chan struct{})
	released := make(chan error, 1)
	srv.Handle("park", func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(entered)
		select {
		case <-ctx.Done():
			released <- ctx.Err()
		case <-time.After(10 * time.Second): // stand-in for the 1 h wait deadline
			released <- nil
		}
		return map[string]any{"ok": true}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

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

	if _, err := c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"park","params":{}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}

	// The caller vanishes mid-wait.
	c.Close()

	select {
	case err := <-released:
		if err == nil {
			t.Fatal("handler ran to its own deadline instead of being released on disconnect")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("released with %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not released when its connection dropped")
	}

	// The server context is still live: a disconnect must not cancel it.
	if ctx.Err() != nil {
		t.Fatalf("server context cancelled by a client disconnect: %v", ctx.Err())
	}
}

// TestServeRefusesWorldAccessibleSocketDir pins audit F20a: binding the socket
// in a directory other users can traverse (the /tmp fallback when
// XDG_RUNTIME_DIR is unset) lets another user pre-create the path — the core
// then cannot remove it and refuses to start — and leaves the socket carrying
// umask permissions for the window between Listen and Chmod. Serve must refuse
// such a directory outright rather than bind and hope.
func TestServeRefusesWorldAccessibleSocketDir(t *testing.T) {
	open := t.TempDir() + "/shared"
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	srv := NewServer(open+"/ipc.sock", slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := srv.Serve(context.Background())
	if err == nil {
		t.Fatal("Serve accepted a world-traversable socket directory")
	}
	if !strings.Contains(err.Error(), "accessible") {
		t.Fatalf("unexpected error %v", err)
	}
	if _, statErr := os.Stat(open + "/ipc.sock"); statErr == nil {
		t.Fatal("Serve bound the socket anyway")
	}

	// And the missing-directory case must fail closed too, not be waved through.
	srv2 := NewServer(t.TempDir()+"/gone/ipc.sock", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := srv2.Serve(context.Background()); err == nil {
		t.Fatal("Serve accepted a socket directory it could not stat")
	}
}

// --- audit F66: "all clients gone" means "all UI clients gone" --------------

// allGoneServer starts a server exposing role-claim handlers and returns it
// with a fired-flag for onAllGone.
func allGoneServer(t *testing.T) (srv *Server, sock string, gone *atomic.Int32) {
	t.Helper()
	sock = privateSocketDir(t) + "/ipc.sock"
	srv = NewServer(sock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gone = &atomic.Int32{}
	srv.OnAllClientsGone(func() { gone.Add(1) })
	srv.Handle("ui", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if !srv.MarkUI(ctx) {
			return nil, Errorf(CodeInvalidRequest, "UI role refused")
		}
		return map[string]any{"ok": true}, nil
	})
	srv.Handle("bridge", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if ok, reason := srv.IdentifyBridge(ctx, "t-1"); !ok {
			return nil, Errorf(CodeInvalidRequest, reason)
		}
		return map[string]any{"ok": true}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	if !waitFor(t, 2*time.Second, func() bool {
		c, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}) {
		t.Fatal("server never came up")
	}
	return srv, sock, gone
}

// The UI leaving fires onAllGone even while an agent bridge is still
// connected: the core must not outlive its window just because a resident
// engine keeps its bridge open. Fails if the role filter is removed — the
// live bridge then keeps len(s.conns) above zero and nothing fires.
func TestAllGoneFiresOnLastUIDespiteLiveBridge(t *testing.T) {
	_, sock, gone := allGoneServer(t)

	bridge, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial (bridge): %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	if err := bridge.Call("bridge", map[string]any{}, nil); err != nil {
		t.Fatalf("identify bridge: %v", err)
	}

	ui, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial (ui): %v", err)
	}
	if err := ui.Call("ui", map[string]any{}, nil); err != nil {
		t.Fatalf("mark ui: %v", err)
	}

	_ = ui.Close()
	if !waitFor(t, 2*time.Second, func() bool { return gone.Load() == 1 }) {
		t.Fatal("the UI disconnected and onAllGone never fired — the live bridge kept the core alive")
	}
}

// Bridge-only churn before any UI has ever connected must NOT fire onAllGone:
// a startup phase that re-attaches agents ahead of the window would otherwise
// shut the core down at birth. Fails if the uiSeen latch (or the role filter)
// is removed — the last bridge disconnecting then counts as "all gone".
func TestAllGoneIgnoresBridgeChurnBeforeAnyUI(t *testing.T) {
	_, sock, gone := allGoneServer(t)

	bridge, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial (bridge): %v", err)
	}
	if err := bridge.Call("bridge", map[string]any{}, nil); err != nil {
		t.Fatalf("identify bridge: %v", err)
	}
	_ = bridge.Close()

	// Also a connection that never claimed any role at all.
	anon, err := Dial(sock)
	if err != nil {
		t.Fatalf("dial (anon): %v", err)
	}
	_ = anon.Close()

	if waitFor(t, 300*time.Millisecond, func() bool { return gone.Load() != 0 }) {
		t.Fatal("onAllGone fired during a bridge-only startup phase, before any UI connected")
	}
}
