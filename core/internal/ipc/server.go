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
)

// maxFrameBytes caps a single inbound JSON-RPC line. Agent stream-json events
// can be sizable; 16 MiB is generous headroom.
const maxFrameBytes = 16 * 1024 * 1024

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

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

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
	c := &conn{netConn: netConn, w: bufio.NewWriter(netConn)}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	n := len(s.conns)
	s.mu.Unlock()
	s.log.Info("ui connected", "clients", n)

	go func() {
		defer func() {
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
			go s.dispatch(ctx, c, frame)
		}
		if err := sc.Err(); err != nil {
			s.log.Warn("connection read error", "err", err)
		}
	}()
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
	c.send(&resp, s.log)
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
		c.send(&f, s.log)
	}
}

// conn is one connected UI client. send is serialised by mu so response and
// notification writers do not interleave.
type conn struct {
	netConn net.Conn
	mu      sync.Mutex
	w       *bufio.Writer
}

func (c *conn) send(f *Frame, log *slog.Logger) {
	b, err := json.Marshal(f)
	if err != nil {
		log.Warn("frame marshal failed", "err", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		log.Warn("frame write failed", "err", err)
		return
	}
	if err := c.w.Flush(); err != nil {
		log.Warn("frame flush failed", "err", err)
	}
}
