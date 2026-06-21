package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"agentkate/internal/safe"
)

// maxFrameBytes caps a single inbound JSON-RPC line. Agent stream-json events
// can be sizable; 16 MiB is generous headroom.
const maxFrameBytes = 16 * 1024 * 1024

// outboundBuffer is the depth of each connection's outbound frame queue. A slow
// UI client may fall this far behind before backpressure kicks in.
const outboundBuffer = 1024

// writeDeadline bounds a single write+flush so a dead-but-not-closed client
// cannot park the writer goroutine forever.
const writeDeadline = 30 * time.Second

// Handler processes one request and returns a result to be JSON-marshalled, or
// an error. An *RPCError is sent verbatim; any other error becomes a
// CodeInternalError.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Server is a JSON-RPC 2.0 server over a Unix domain socket. Multiple UI
// clients may connect at once; Notify broadcasts to all of them.
type Server struct {
	socketPath string
	log        *slog.Logger

	mu        sync.RWMutex
	handlers  map[string]Handler
	conns     map[*conn]struct{}
	onAllGone func()
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

// OnAllClientsGone registers a callback fired whenever the last connected UI
// client disconnects. The core uses this to shut itself down rather than be
// orphaned when its UI exits. Call before Serve.
func (s *Server) OnAllClientsGone(f func()) {
	s.onAllGone = f
}

// Serve listens on the socket and blocks until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	// A stale socket from a previous crash would make Listen fail.
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	defer os.Remove(s.socketPath)
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
	c := &conn{
		netConn: netConn,
		w:       bufio.NewWriter(netConn),
		out:     make(chan *Frame, outboundBuffer),
		done:    make(chan struct{}),
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
			c.close() // stops the writer goroutine; idempotent
			s.mu.Lock()
			delete(s.conns, c)
			remaining := len(s.conns)
			s.mu.Unlock()
			netConn.Close()
			s.log.Info("ui disconnected", "clients", remaining)
			if remaining == 0 && s.onAllGone != nil {
				s.onAllGone()
			}
		}()

		sc := bufio.NewScanner(netConn)
		sc.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			// Scanner reuses its buffer; copy before handing to a goroutine.
			frame := append([]byte(nil), line...)
			safe.Go("ipc.dispatch", func() { s.dispatch(ctx, c, frame) })
		}
		if err := sc.Err(); err != nil {
			s.log.Warn("connection read error", "err", err)
		}
	})
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
	switch {
	case !ok:
		resp.Error = Errorf(CodeMethodNotFound, "method not found: "+f.Method)
	default:
		result, err := h(ctx, f.Params)
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
	c.enqueue(&resp)
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

	closeOnce sync.Once
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
