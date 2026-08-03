package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"agentkate/internal/safe"
)

// The app-server uses newline-delimited JSON-RPC 2.0 over stdio (not LSP
// Content-Length framing).  Keep this tiny client local so the adapter can be
// tested against a shell fake without pulling another protocol dependency in.
var errStreamClosed = errors.New("codex app-server stream closed")
var errWriteBroken = errors.New("codex app-server stdin is no longer writable")

const stdinWriteTimeout = 30 * time.Second

type deadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message) }

type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcClient struct {
	log *slog.Logger
	w   io.Writer
	wmu sync.Mutex
	// Guarded by wmu. A failed write can leave a partial JSON-RPC line on the
	// pipe, so every later writer must fail rather than corrupt the stream.
	writeBroken bool

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcFrame
	closed  bool

	onNotification func(method string, params json.RawMessage)
	// onRequest handles server-initiated JSON-RPC requests such as an app-server
	// command approval. They must not be mistaken for notifications: leaving a
	// request unanswered wedges the active Codex turn.
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
}

func newRPCClient(w io.Writer, log *slog.Logger) *rpcClient {
	return &rpcClient{w: w, log: log, pending: make(map[string]chan rpcFrame)}
}

func (c *rpcClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f rpcFrame
		if err := json.Unmarshal(line, &f); err != nil {
			if c.log != nil {
				c.log.Debug("codex app-server: invalid JSON-RPC frame", "bytes", len(line))
			}
			continue
		}
		if f.Method != "" {
			if len(f.ID) > 0 && string(f.ID) != "null" {
				if c.onRequest != nil {
					requestID := append(json.RawMessage(nil), f.ID...)
					params := append(json.RawMessage(nil), f.Params...)
					method := f.Method
					safe.Go("codex.serverRequest", func() { c.onRequest(requestID, method, params) })
				} else {
					_ = c.respondError(f.ID, -32601, "Agent Kate cannot handle this Codex request")
				}
				continue
			}
			if c.onNotification != nil {
				c.onNotification(f.Method, f.Params)
			}
			continue
		}
		key := string(f.ID)
		c.mu.Lock()
		ch := c.pending[key]
		delete(c.pending, key)
		c.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
	c.failAll(errStreamClosed)
}

func (c *rpcClient) respond(id json.RawMessage, result any) error {
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	if err != nil {
		return err
	}
	return c.writeFrame(append(frame, '\n'))
}

func (c *rpcClient) respondError(id json.RawMessage, code int, message string) error {
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
	if err != nil {
		return err
	}
	return c.writeFrame(append(frame, '\n'))
}

func (c *rpcClient) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errStreamClosed
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan rpcFrame, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.remove(key)
		return err
	}
	err = c.writeFrame(append(frame, '\n'))
	if err != nil {
		c.remove(key)
		return fmt.Errorf("codex rpc write: %w", err)
	}
	select {
	case f := <-ch:
		if f.Error != nil {
			return f.Error
		}
		if out != nil && len(f.Result) > 0 {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.remove(key)
		return ctx.Err()
	}
}

// notify writes a JSON-RPC notification. Codex app-server requires the
// initialized acknowledgement after a successful initialize response; unlike a
// request, it deliberately carries no id and has no response to await.
func (c *rpcClient) notify(method string, params any) error {
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	return c.writeFrame(append(frame, '\n'))
}

// writeFrame mirrors the Claude/Kimi supervisors' F9/F52 hardening. Pipes
// returned by StdinPipe support deadlines; a wedged child can therefore never
// leave every IPC writer parked forever behind wmu.
func (c *rpcClient) writeFrame(frame []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.writeBroken {
		return errWriteBroken
	}
	if dw, ok := c.w.(deadlineWriter); ok {
		if err := dw.SetWriteDeadline(time.Now().Add(stdinWriteTimeout)); err == nil {
			defer func() { _ = dw.SetWriteDeadline(time.Time{}) }()
		}
	}
	if _, err := c.w.Write(frame); err != nil {
		c.writeBroken = true
		return err
	}
	return nil
}

func (c *rpcClient) remove(key string) { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }
func (c *rpcClient) failAll(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan rpcFrame)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcFrame{Error: &rpcError{Code: -32000, Message: err.Error()}}
	}
}
