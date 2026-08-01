package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/safe"
)

// maxFrameBytes caps a single inbound JSON-RPC line. Agent stream-json events
// can be sizable; 16 MiB is generous headroom.
const maxFrameBytes = 16 * 1024 * 1024

// readBufferBytes is the per-connection read buffer. Lines longer than it are
// still read whole (readFrame accumulates across ErrBufferFull); it only sets
// how many syscalls a large frame costs.
const readBufferBytes = 64 * 1024

// idProbeBytes is how much of an oversize frame's head is retained to recover
// the request id for the error reply. The id sits in the frame's first object
// keys; keeping more would defeat the point of discarding the frame.
const idProbeBytes = 4096

// retainedBufferBytes caps the accumulation buffer a frameReader keeps between
// frames. Reuse exists to spare the steady state its allocations; a one-off
// giant frame must not pin its whole array on an otherwise idle connection.
const retainedBufferBytes = 1 << 20

// outboundBuffer is the depth of each connection's outbound frame queue. A slow
// UI client may fall this far behind before backpressure kicks in.
const outboundBuffer = 1024

// writeDeadline bounds a single write+flush so a dead-but-not-closed client
// cannot park the writer goroutine forever.
const writeDeadline = 30 * time.Second

// maxInFlightPerConn caps the number of in-flight dispatch goroutines for a
// single connection. Each inbound frame is handled on its own goroutine; a
// client that stalls draining its responses makes every handler block in
// enqueue, so without a cap the reader would spawn goroutines without bound — a
// goroutine/memory leak systemd-oomd eventually trips on. When the cap is hit
// the reader blocks, applying backpressure to this connection alone.
//
// The bound is deliberately per-connection, not global. Long-blocking handlers
// (permission.request waits up to 8 minutes for the human's permission.respond)
// are released by a frame on a DIFFERENT connection — the UI — so a per-conn cap
// can never deadlock: the UI reader keeps flowing and resolves the waiters. A
// global cap could be exhausted by agent bridges and starve that very release.
const maxInFlightPerConn = 256

// Handler processes one request and returns a result to be JSON-marshalled, or
// an error. An *RPCError is sent verbatim; any other error becomes a
// CodeInternalError.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// BridgeActivityFunc observes one completed request from an agent's MCP bridge
// (plan 16 P2's `mcp.activity` feed). threadID is the connection's bound thread,
// params the raw request params, dur the handler's own runtime, and errText the
// failure message ("" on success). It runs on the dispatch goroutine after the
// reply has been queued, so it must not block.
type BridgeActivityFunc func(threadID, method string, params json.RawMessage,
	dur time.Duration, errText string)

// Server is a JSON-RPC 2.0 server over a Unix domain socket. Multiple UI
// clients may connect at once; Notify broadcasts to all of them.
type Server struct {
	socketPath string
	log        *slog.Logger

	mu          sync.RWMutex
	handlers    map[string]Handler
	conns       map[*conn]struct{}
	onAllGone   func()
	primaryConn *conn // the first UI to handshake; runs portal sessions (Cowork keystone)
	// onBridgeActivity is set once before Serve (registerMCPActivity) and read
	// without locking thereafter, like handlers.
	onBridgeActivity BridgeActivityFunc
}

// NewServer creates a server bound to socketPath. Call Handle to register
// methods, then Serve.
func NewServer(socketPath string, log *slog.Logger) *Server {
	return &Server{
		socketPath: socketPath,
		log:        log,
		handlers:   make(map[string]Handler),
		conns:      make(map[*conn]struct{}),
	}
}

// Handle registers a handler for a method name. Call before Serve.
func (s *Server) Handle(method string, h Handler) {
	s.handlers[method] = h
}

// OnBridgeActivity registers the observer for completed agent-bridge requests.
// Call before Serve. Only requests (not notifications) on connections tagged as
// bridges are reported.
func (s *Server) OnBridgeActivity(f BridgeActivityFunc) { s.onBridgeActivity = f }

// OnAllClientsGone registers a callback fired whenever the last connected UI
// client disconnects. The core uses this to shut itself down rather than be
// orphaned when its UI exits. Call before Serve.
func (s *Server) OnAllClientsGone(f func()) {
	s.onAllGone = f
}

// assertPrivateDir refuses any socket directory another user could reach. It is
// what makes the unavoidable window between Listen and Chmod harmless: the
// socket may briefly carry umask permissions, but nobody else can traverse the
// directory to find it — and nobody else can pre-create a squatting socket
// there either (audit F20a).
//
// Deliberately fails CLOSED: every condition it cannot evaluate (stat error,
// a platform without a Unix stat, a symlinked directory whose target may move
// under us) is treated as unsafe, because "we could not check" and "it is
// private" must never be the same answer.
func assertPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("socket directory %s: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket directory %s is a symlink; refusing to bind through it", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket directory %s is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("socket directory %s: cannot determine ownership", dir)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("socket directory %s is owned by uid %d, not %d", dir, st.Uid, os.Getuid())
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("socket directory %s is group/world accessible (mode %04o)", dir, perm)
	}
	return nil
}

// Serve listens on the socket and blocks until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	// The socket's privacy rests on its directory, so check that first — before
	// removing anything or binding anything inside it.
	if err := assertPrivateDir(filepath.Dir(s.socketPath)); err != nil {
		return err
	}
	// A stale socket from a previous crash would make Listen fail.
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	defer os.Remove(s.socketPath)
	// Defense in depth: restrict the socket to the owning user. The directory is
	// verified 0700 and ours above, so this does not stop a same-uid process
	// (see 08 §F Option A) and the Listen→Chmod window is not reachable by
	// anyone else; it is belt-and-braces for a directory mode changed later.
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		s.log.Warn("ipc socket chmod failed", "err", err)
	}
	s.log.Info("ipc listening", "socket", s.socketPath)

	safe.Go("ipc.listenerCloser", func() {
		<-ctx.Done()
		ln.Close()
	})

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // graceful shutdown
			}
			s.log.Warn("accept failed", "err", err)
			continue
		}
		s.serveConn(ctx, c)
	}
}

func (s *Server) serveConn(ctx context.Context, netConn net.Conn) {
	// Every handler dispatched on this connection runs under a context that is
	// cancelled when the connection goes away, not only when the whole server
	// shuts down. Long-blocking handlers (agent.wait parks for up to an hour,
	// permission.request for minutes) are contractually required to release
	// when their caller's bridge dies — see agent/turnwait.go's disconnect
	// contract. Without this, each stop/restart of a waiting controller leaks a
	// parked goroutine for the whole deadline.
	connCtx, cancelConn := context.WithCancel(ctx)
	c := &conn{
		netConn: netConn,
		w:       bufio.NewWriter(netConn),
		out:     make(chan *Frame, outboundBuffer),
		done:    make(chan struct{}),
		sem:     make(chan struct{}, maxInFlightPerConn),
		log:     s.log,
	}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	n := len(s.conns)
	s.mu.Unlock()
	s.log.Info("ui connected", "clients", n)

	// One dedicated writer goroutine per connection drains the outbound queue
	// and performs the blocking write+flush in isolation, so a slow client can
	// never wedge the producers (per-agent pumps, the fs-watcher loop).
	safe.Go("ipc.connWriter", func() { c.writeLoop() })

	safe.Go("ipc.serveConn", func() {
		defer func() {
			// Release every handler still parked on this connection BEFORE
			// anything else: a waiter whose caller is gone has nobody left to
			// answer.
			cancelConn()
			c.close() // stops the writer goroutine; idempotent
			s.mu.Lock()
			delete(s.conns, c)
			if s.primaryConn == c {
				s.primaryConn = nil // a Cowork portal request will now fail closed until a UI reconnects
			}
			remaining := len(s.conns)
			s.mu.Unlock()
			netConn.Close()
			s.log.Info("ui disconnected", "clients", remaining)
			if remaining == 0 && s.onAllGone != nil {
				s.onAllGone()
			}
		}()

		fr := newFrameReader(netConn)
		for {
			frame, oversize, err := fr.readFrame()
			if oversize > 0 {
				s.log.Warn("oversize inbound frame discarded",
					"bytes", oversize, "cap", maxFrameBytes)
				// A caller blocked in Client.Call would otherwise wait out its
				// whole timeout, so answer it when the retained prefix still
				// carries a usable id.
				if id := frameID(frame); id != nil {
					c.enqueue(&Frame{JSONRPC: "2.0", ID: id,
						Error: Errorf(CodeInvalidRequest, fmt.Sprintf(
							"frame too large (%d bytes, cap %d MiB)",
							oversize, maxFrameBytes/(1024*1024)))})
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					s.log.Warn("connection read error", "err", err)
				}
				return
			}
			if oversize > 0 || len(frame) == 0 {
				continue
			}
			// Bound in-flight dispatch goroutines for this connection: a client
			// that stalls draining responses makes each handler block in enqueue,
			// so without this the reader would spawn them without bound. Acquiring
			// blocks the reader once the cap is hit (backpressure on this conn
			// only); the token is released when the handler returns. See
			// maxInFlightPerConn for why this is per-connection, not global.
			select {
			case <-c.done:
				return
			case c.sem <- struct{}{}:
			}
			// frame aliases the reader's reusable buffer, which the next
			// readFrame overwrites while this dispatch is still running.
			msg := append([]byte(nil), frame...)
			safe.Go("ipc.dispatch", func() {
				defer func() { <-c.sem }()
				s.dispatch(connCtx, c, msg)
			})
		}
	})
}

// idPattern finds the request id in a truncated frame prefix. It is deliberately
// lexical, not a JSON parse: the prefix of an oversize frame is not valid JSON.
// Matching the first `"id": <number|string>` is right for frames the client
// emits (id precedes params), and a miss simply means no error reply.
var idPattern = regexp.MustCompile(`"id"\s*:\s*(\d+|"[^"]*")`)

// paramsKey bounds the id search: an "id" inside params belongs to the payload,
// not to JSON-RPC, and answering with it would resolve an unrelated in-flight
// request.
var paramsKey = []byte(`"params"`)

// frameID extracts the JSON-RPC id from raw frame bytes, or nil if absent.
//
// raw may be a probe-truncated prefix, so the recovery is deliberately
// conservative in two ways — a wrong id resolves someone else's call, which is
// worse than no reply at all:
//   - only the bytes before the first "params" key are searched, so a nested id
//     inside the payload can never be mistaken for the request id;
//   - a match that ends exactly at the end of raw is refused, because the probe
//     cut may have severed a number mid-digits or a string mid-value.
func frameID(raw []byte) *json.RawMessage {
	head := raw
	if i := bytes.Index(head, paramsKey); i >= 0 {
		head = head[:i]
	}
	m := idPattern.FindSubmatchIndex(head)
	if m == nil {
		return nil
	}
	if m[3] == len(raw) {
		return nil // possibly truncated mid-token
	}
	id := json.RawMessage(append([]byte(nil), head[m[2]:m[3]]...))
	return &id
}

// frameReader reads newline-delimited frames from one connection. The
// accumulation buffer is reused across frames: a fresh append-doubling buffer
// per frame is pure garbage on a stream of large agent events.
type frameReader struct {
	r     *bufio.Reader
	buf   []byte // reused accumulator; reset to [:0] per frame
	probe []byte // small, separate head buffer for oversize frames
}

func newFrameReader(rd io.Reader) *frameReader {
	return &frameReader{r: bufio.NewReaderSize(rd, readBufferBytes)}
}

// readFrame reads one newline-delimited frame, enforcing maxFrameBytes without
// killing the connection — the reason this is not a bufio.Scanner. A Scanner
// treats an over-long token as terminal (ErrTooLong ends the loop), so a single
// bad frame would drop the UI connection and, it being the last client, stop the
// whole core.
//
// On an oversize line the rest of the line is drained up to the next '\n' and
// oversize is the discarded line's length in bytes; the returned bytes are only
// the leading idProbeBytes, retained so the caller can answer the sender.
// Otherwise oversize is 0 and the returned bytes are the line, EOL stripped.
//
// The returned slice aliases the reader's own buffer and is invalidated by the
// next readFrame call: a caller that hands it to another goroutine must copy it.
func (fr *frameReader) readFrame() (line []byte, oversize int, err error) {
	fr.buf = fr.buf[:0]
	if cap(fr.buf) > retainedBufferBytes {
		fr.buf = nil // release a one-off giant frame's array
	}
	total := 0
	over := false
	eol := false
	for {
		chunk, rerr := fr.r.ReadSlice('\n')
		total += len(chunk)
		switch {
		case over:
			// Draining the remainder of the line: the head is already capped at
			// idProbeBytes and nothing past it is retained, only counted.
		case total > maxFrameBytes:
			over = true
			if len(fr.buf) > idProbeBytes {
				fr.buf = fr.buf[:idProbeBytes]
			} else if room := idProbeBytes - len(fr.buf); room > 0 {
				fr.buf = append(fr.buf, chunk[:min(room, len(chunk))]...)
			}
		default:
			fr.buf = append(fr.buf, chunk...)
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue // line longer than the read buffer; keep going
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) && total > 0 {
				break // unterminated trailing line; EOF is reported on the next call
			}
			return nil, 0, rerr
		}
		eol = true
		break
	}
	if over {
		if eol {
			total--
		}
		// Hand back a small copy and drop the accumulator, so the up-to-16-MiB
		// array a rejected frame grew is not pinned by a 4 KiB reslice.
		fr.probe = append(fr.probe[:0], fr.buf...)
		fr.buf = nil
		return fr.probe, total, nil
	}
	if eol {
		fr.buf = fr.buf[:len(fr.buf)-1]
		if n := len(fr.buf); n > 0 && fr.buf[n-1] == '\r' {
			fr.buf = fr.buf[:n-1]
		}
	}
	return fr.buf, 0, nil
}

// callHandler runs one handler with its own recover, so a panicking handler
// becomes an ordinary error response instead of a silent hang. safe.Go already
// keeps a panic from killing the process, but it unwinds the whole dispatch
// goroutine — past the reply enqueue and past the activity hook — leaving the
// caller (an agent's MCP bridge) blocked until its own timeout and the
// mcp.activity feed missing the most interesting event of all.
func callHandler(ctx context.Context, log *slog.Logger, h Handler,
	method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("rpc handler panic recovered", "method", method,
				"panic", r, "stack", string(debug.Stack()))
			result = nil
			err = Errorf(CodeInternalError,
				fmt.Sprintf("handler for %s panicked: %v", method, r))
		}
	}()
	return h(ctx, params)
}

func (s *Server) dispatch(ctx context.Context, c *conn, raw []byte) {
	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		s.log.Warn("bad inbound frame", "err", err)
		return
	}
	if f.Method == "" {
		return // responses to the UI are not expected; ignore
	}

	s.mu.RLock()
	h, ok := s.handlers[f.Method]
	s.mu.RUnlock()

	// Carry the calling connection's identity (role/threadId/primary) so the Cowork
	// consent handlers can enforce UI-only RPCs and per-thread binding. Other
	// handlers ignore it (Cowork keystone, 08 §C).
	ctx = context.WithValue(ctx, connCtxKey{}, c)

	// Notification (no id): run the handler, discard the result.
	if f.ID == nil {
		if ok {
			if _, err := h(ctx, f.Params); err != nil {
				s.log.Warn("notification handler failed", "method", f.Method, "err", err)
			}
		}
		return
	}

	resp := Frame{JSONRPC: "2.0", ID: f.ID}
	started := time.Now()
	switch {
	case !ok:
		resp.Error = Errorf(CodeMethodNotFound, "method not found: "+f.Method)
	default:
		result, err := callHandler(ctx, s.log, h, f.Method, f.Params)
		if err != nil {
			var rpcErr *RPCError
			if errors.As(err, &rpcErr) {
				resp.Error = rpcErr
			} else {
				resp.Error = Errorf(CodeInternalError, err.Error())
			}
		} else if b, mErr := json.Marshal(result); mErr != nil {
			resp.Error = Errorf(CodeInternalError, mErr.Error())
		} else {
			resp.Result = b
		}
	}
	dur := time.Since(started)
	c.enqueue(&resp)

	// Every request an agent's MCP bridge completes is one MCP tool call the
	// human should be able to watch (plan 16 §4a). Reported after the reply is
	// queued so the observer never delays the agent, and only for bridges — UI
	// traffic is not MCP traffic.
	if s.onBridgeActivity != nil && c.getRole() == "bridge" {
		errText := ""
		if resp.Error != nil {
			errText = resp.Error.Message
		}
		s.onBridgeActivity(c.getThreadID(), f.Method, f.Params, dur, errText)
	}
}

// Notify broadcasts a notification to every connected UI client.
func (s *Server) Notify(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		s.log.Warn("notify marshal failed", "method", method, "err", err)
		return
	}
	raw := json.RawMessage(b)
	f := Frame{JSONRPC: "2.0", Method: method, Params: raw}

	s.mu.RLock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.RUnlock()

	for _, c := range conns {
		c.enqueue(&f)
	}
}

// NotifyUI broadcasts a notification to the connected UI clients only —
// connections that identified themselves as the UI. An agent bridge never sees
// it, so a feed about one agent's activity cannot reach another agent.
func (s *Server) NotifyUI(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		s.log.Warn("notify-ui marshal failed", "method", method, "err", err)
		return
	}
	f := Frame{JSONRPC: "2.0", Method: method, Params: json.RawMessage(b)}

	s.mu.RLock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		if c.getRole() == "ui" {
			conns = append(conns, c)
		}
	}
	s.mu.RUnlock()

	for _, c := range conns {
		c.enqueue(&f)
	}
}

// NotifyBridge sends a notification to the MCP bridge connections bound to one
// agent thread — the reverse direction of the bridge's own Call, and the only
// core→bridge push. It is how a live capability change (Cowork being switched
// on mid-session) reaches the bridge so it can re-advertise its tool set; the
// UI and other threads' bridges never see it.
func (s *Server) NotifyBridge(threadID, method string, params any) {
	if threadID == "" {
		return
	}
	b, err := json.Marshal(params)
	if err != nil {
		s.log.Warn("notify-bridge marshal failed", "method", method, "err", err)
		return
	}
	f := Frame{JSONRPC: "2.0", Method: method, Params: json.RawMessage(b)}

	s.mu.RLock()
	conns := make([]*conn, 0, 2)
	for c := range s.conns {
		if c.getRole() == "bridge" && c.getThreadID() == threadID {
			conns = append(conns, c)
		}
	}
	s.mu.RUnlock()

	for _, c := range conns {
		c.enqueue(&f)
	}
}

// conn is one connected UI client. Frames are produced concurrently (per-agent
// pumps, the fs-watcher loop, request handlers) but written by a single
// dedicated goroutine draining out, so producers never block on socket I/O and
// frames for this connection never interleave.
type conn struct {
	netConn net.Conn
	w       *bufio.Writer
	log     *slog.Logger

	out  chan *Frame   // bounded outbound queue, drained by writeLoop
	done chan struct{} // closed once to stop the writer; guarded by closeOnce
	sem  chan struct{} // bounds in-flight dispatch goroutines (maxInFlightPerConn)

	closeOnce sync.Once

	// Cowork keystone identity (08 §C). idMu guards these; they are set once at
	// handshake/attach and read by Cowork consent handlers via ConnFromContext.
	idMu      sync.RWMutex
	role      string // "" (unknown) | "ui" | "bridge"
	connTID   string // for bridge conns: the bound thread id (trust-on-first-use)
	isPrimary bool   // the one UI that runs portal sessions
}

func (c *conn) getRole() string {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	return c.role
}

func (c *conn) getThreadID() string {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	return c.connTID
}

func (c *conn) getPrimary() bool {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	return c.isPrimary
}

// enqueue queues f for delivery to this client. It never blocks a shared
// producer on a slow client.
//
// Backpressure policy distinguishes the two frame kinds:
//   - Responses (non-nil ID) are awaited by a blocking Client.Call and MUST be
//     delivered. They are sent with a guaranteed (blocking) send that only gives
//     up if the connection is closing; the writer's deadline disconnects a dead
//     client, which unblocks the send. Responses originate on the per-request
//     dispatch goroutine, so blocking there delays only that one reply, never the
//     shared per-agent/fs-watcher producers.
//   - Notifications (nil ID) are coalescing/best-effort — UI state is re-derived
//     from snapshots — so when the queue is full we shed the OLDEST notification
//     to keep the client as current as possible. We NEVER shed a response: if the
//     head of the queue is a response, we put it back and drop this incoming
//     notification instead.
//
// enqueue is safe to call after the connection has begun closing: every send is
// guarded by a select on done, so there is no send-on-closed-channel panic and
// no goroutine leak.
func (c *conn) enqueue(f *Frame) {
	if f.ID != nil {
		// Response: must deliver. Block (bounded by writer drain / deadline).
		select {
		case <-c.done:
		case c.out <- f:
		}
		return
	}
	for {
		select {
		case <-c.done:
			return // connection closing; drop silently
		case c.out <- f:
			return
		default:
		}
		// Queue full: shed the oldest *notification*, then retry.
		select {
		case <-c.done:
			return
		case old := <-c.out:
			if old.ID != nil {
				// Oldest is a response we must keep. A slot is now free (we just
				// popped it), so put it back — and drop this incoming
				// notification instead of the response.
				select {
				case <-c.done:
				case c.out <- old:
				}
				return
			}
			c.log.Warn("ipc outbound queue full, dropping oldest notification")
		default:
			// Drained concurrently between the two selects; loop and retry the
			// enqueue (which will now usually succeed).
		}
	}
}

// writeLoop is the single writer goroutine for this connection. It drains out,
// marshals each frame, and performs the blocking write+flush under a deadline.
// Any write/flush/marshal error (including a deadline timeout from a dead client
// that never closed its socket) tears the connection down.
func (c *conn) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case f := <-c.out:
			if !c.writeFrame(f) {
				// Unblock serveConn's reader (which owns lifecycle cleanup) and
				// stop this writer.
				c.netConn.Close()
				c.close()
				return
			}
		}
	}
}

// writeFrame marshals and writes a single frame under a write deadline.
// It returns false on any error, signalling the connection should be dropped.
func (c *conn) writeFrame(f *Frame) bool {
	b, err := json.Marshal(f)
	if err != nil {
		c.log.Warn("frame marshal failed", "err", err)
		return true // a bad frame must not kill the connection
	}
	// Bound the write so a dead-but-not-closed client cannot park us forever.
	if err := c.netConn.SetWriteDeadline(time.Now().Add(writeDeadline)); err != nil {
		c.log.Warn("set write deadline failed", "err", err)
		return false
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		c.log.Warn("frame write failed", "err", err)
		return false
	}
	if err := c.w.Flush(); err != nil {
		c.log.Warn("frame flush failed", "err", err)
		return false
	}
	return true
}

// close signals the writer goroutine to stop. It is idempotent and safe to call
// from either the reader's cleanup path or the writer itself.
func (c *conn) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// --- Cowork keystone: per-connection identity (08 §C) ----------------------------
//
// The Cowork consent model must distinguish the UI (which may answer grant prompts,
// press the kill-switch, run portals) from an agent's MCP bridge (which may only
// REQUEST consent-gated capabilities, never grant them) and bind each bridge to a
// single thread so it cannot spend another thread's grant. Identity is injected into
// the request context in dispatch; handlers retrieve it via ConnFromContext.
//
// Same-uid honesty: a malicious local process can still forge a handshake on the raw
// socket (Option A, 08 §F). This gate stops the realistic prompt-injection path (the
// agent acting through its own bridge) and cross-thread grant theft, not a determined
// raw-socket forger — for which the audit log + kill-switch provide detection.

type connCtxKey struct{}

// ConnRef exposes the calling connection's identity to handlers that need it.
type ConnRef struct{ c *conn }

// ConnFromContext returns the calling connection's identity, or nil if absent
// (e.g. an internally-synthesized call).
func ConnFromContext(ctx context.Context) *ConnRef {
	if c, ok := ctx.Value(connCtxKey{}).(*conn); ok && c != nil {
		return &ConnRef{c}
	}
	return nil
}

// Role is "" (not yet identified), "ui", or "bridge".
func (r *ConnRef) Role() string { return r.c.getRole() }

// ThreadID is the bound thread for a bridge connection ("" otherwise).
func (r *ConnRef) ThreadID() string { return r.c.getThreadID() }

// IsPrimaryUI reports whether this is the UI that runs portal sessions.
func (r *ConnRef) IsPrimaryUI() bool { return r.c.getPrimary() }

// Token identifies THIS connection to bookkeeping that has to outlive the call
// — the bridge-secret ledger, which must be able to tell "the bridge that
// redeemed this secret is calling again" from "a second connection is replaying
// it". A ConnRef is minted per call, so it cannot serve as that key itself.
//
// The token is comparable (it wraps the connection pointer) and it keeps the
// connection object alive for as long as a holder keeps it, which is what makes
// the identity stable: nothing else can ever be handed the same pointer while
// the token exists, so there is no ABA.
func (r *ConnRef) Token() ConnToken { return ConnToken{r.c} }

// ConnToken is an opaque, comparable handle to one connection. The zero value
// belongs to no connection and is never Live — the fail-closed direction for a
// caller that has no connection identity at all.
type ConnToken struct{ c *conn }

// Live reports whether the connection is still connected. It reads the same
// `done` channel serveConn's teardown closes, so a holder that has gone away
// stops holding anything the moment its socket drops — which is what lets a
// legitimate bridge reconnect into the slot it used to hold.
func (t ConnToken) Live() bool {
	if t.c == nil {
		return false
	}
	select {
	case <-t.c.done:
		return false
	default:
		return true
	}
}

// MarkUI tags this connection as the UI. The first UI to call becomes primary
// (runs portal sessions). Called from the existing `handshake` handler. It
// reports whether the connection now holds the UI role, so the handshake can
// answer with a refusal rather than leaving a client to discover it has no
// authority one rejected call at a time.
//
// Two connections are REFUSED the role:
//
//   - one that already identified as an agent bridge — the mirror of
//     IdentifyBridge's refusal of a UI connection. Without it a bridge could
//     re-identify by calling `handshake`, and thereby pass RequireUI (answering
//     its own grant prompts) and receive the UI-only mcp.activity feed for every
//     other agent in the arena. Role is one-way per connection in both
//     directions.
//   - a SECOND connection asking for the role while another live connection
//     already holds it (audit F13). Agent Kate runs one UI; a second claimant is
//     either a mistake or a same-uid process forging the role, and both are
//     better refused than admitted to the human's feeds. This does NOT
//     authenticate the first claimant — the role is still self-asserted on first
//     contact, and a process that connects before the real UI still wins the
//     slot. It narrows the window rather than closing it; see
//     docs/security-model.md §2 for the known gap and the fix that closes it.
//
// A disconnect clears the role with the connection, so the real UI reconnecting
// (or a respawned core) always finds the slot free.
func (s *Server) MarkUI(ctx context.Context) bool {
	ref := ConnFromContext(ctx)
	if ref == nil {
		return false
	}
	c := ref.c
	// ONE atomic decision, held under s.mu from the scan to the assignment.
	//
	// The earlier version scanned for an existing UI under s.mu, RELEASED it,
	// and only then took c.idMu to write the role — so two connections
	// handshaking at once could each scan while the other was still roleless
	// and both come away holding the role, which is exactly what the refusal
	// exists to prevent. The comment claimed atomicity the code did not have.
	//
	// Lock ORDER is s.mu then idMu — the same order NotifyUI uses (it reads
	// roles under s.mu.RLock, and getRole takes idMu.RLock); nothing takes idMu
	// first, so holding both cannot deadlock. c.idMu is taken directly rather
	// than via getRole/getters, since those would re-enter it.
	s.mu.Lock()
	defer s.mu.Unlock()
	for other := range s.conns {
		if other == c {
			continue
		}
		if other.getRole() == "ui" {
			s.log.Warn("refusing a second UI role claim: a client already holds it")
			return false
		}
	}
	c.idMu.Lock()
	defer c.idMu.Unlock()
	if c.role == "bridge" {
		s.log.Warn("refusing to mark an agent bridge connection as the UI",
			"thread", c.connTID)
		return false
	}
	c.role = "ui"
	// The first UI to hold the role runs portal sessions. Claimed only after
	// the role is granted, so a refused claimant can never leave itself parked
	// as the primary — the unwind the old two-phase version needed.
	if s.primaryConn == nil {
		s.primaryConn = c
		c.isPrimary = true
	}
	return true
}

// IdentifyBridge tags this connection as the agent bridge for threadID. It is
// the ONLY way a connection acquires the bridge role, and the caller must have
// authenticated the claim first — in akcore that is `bridge.identify`, which
// checks the per-launch secret akcore handed to that thread's bridge before it
// calls this (see cmd/akcore/bridgeauth.go).
//
// This used to be trust-on-first-use, and it was reached from every handler
// that wanted "the bridge for this thread": an unidentified connection could
// name ANY thread and take its identity, with the thread's whole authority
// attached. Binding now happens once, at the authenticated door, and every
// other handler asserts through RequireBridge instead.
//
// A UI connection may never act as a bridge (the mirror of MarkUI's refusal of
// a bridge), and a bridge may not switch threads. Re-identifying to the SAME
// thread is idempotent: a thread legitimately runs more than one bridge (the
// Cooperation and Cowork servers), each on its own connection, and a bridge may
// re-send its handshake.
func (s *Server) IdentifyBridge(ctx context.Context, threadID string) (ok bool, reason string) {
	ref := ConnFromContext(ctx)
	if ref == nil {
		return false, "no connection identity"
	}
	c := ref.c
	c.idMu.Lock()
	defer c.idMu.Unlock()
	if c.role == "ui" {
		return false, "UI connection may not invoke agent capabilities"
	}
	if c.role == "" {
		c.role = "bridge"
		c.connTID = threadID
		return true, ""
	}
	if c.connTID != threadID {
		return false, "thread mismatch: connection is bound to a different thread"
	}
	return true, ""
}

// RequireBridge asserts that this connection has ALREADY identified as
// threadID's agent bridge. It never binds: a connection that skipped the
// authenticated handshake is refused, which is what stops a handler from
// becoming a second, unauthenticated door into a thread's identity.
func (s *Server) RequireBridge(ctx context.Context, threadID string) (ok bool, reason string) {
	ref := ConnFromContext(ctx)
	if ref == nil {
		return false, "no connection identity"
	}
	c := ref.c
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	switch {
	case c.role == "ui":
		return false, "UI connection may not invoke agent capabilities"
	case c.role != "bridge":
		return false, "this connection has not identified as an agent bridge"
	case c.connTID != threadID:
		return false, "thread mismatch: connection is bound to a different thread"
	}
	return true, ""
}

// RequireUI reports whether the caller is the UI (for grant responses, kill-switch,
// revoke, enable — never an agent).
func (s *Server) RequireUI(ctx context.Context) bool {
	ref := ConnFromContext(ctx)
	return ref != nil && ref.Role() == "ui"
}

// NotifyPrimaryUI sends a notification only to the primary UI (portal requests).
// Returns false if there is no primary UI connected (caller should fail closed).
func (s *Server) NotifyPrimaryUI(method string, params any) bool {
	b, err := json.Marshal(params)
	if err != nil {
		s.log.Warn("notify-primary marshal failed", "method", method, "err", err)
		return false
	}
	s.mu.RLock()
	c := s.primaryConn
	s.mu.RUnlock()
	if c == nil {
		return false
	}
	c.enqueue(&Frame{JSONRPC: "2.0", Method: method, Params: json.RawMessage(b)})
	return true
}
