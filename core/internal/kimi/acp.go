// Package kimi supervises headless Kimi Code CLI processes as Agent Kate agent
// threads, alongside the Claude Code backend in package agent. Each thread is
// one `kimi acp` child process speaking ACP (Agent Client Protocol —
// newline-delimited JSON-RPC 2.0 over stdio); this package translates all ACP
// traffic into the Claude-shaped stream-json events the UI already renders, so
// the relay, transcript and rendering paths stay backend-agnostic.
package kimi

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

	"agentkate/internal/safe"
)

// ACP / JSON-RPC error codes used when answering agent→client requests.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// errStreamClosed fails every pending call when the child's stdout closes —
// the process died (or is dying), so no request will ever be answered.
var errStreamClosed = errors.New("kimi acp stream closed")

// acpError is the error half of a JSON-RPC response.
type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *acpError) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

// acpFrame is one newline-delimited JSON-RPC message in either direction.
// Requests carry method+id, notifications method only, responses id only.
type acpFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

// acpClient is the JSON-RPC client half of one `kimi acp` child. Outgoing
// requests are id-allocated here; responses are dispatched to the registered
// callback on the read-loop goroutine, so a prompt's completion is always
// observed after the session/update notifications that preceded it (the
// translator's event ordering depends on that).
type acpClient struct {
	log *slog.Logger

	wmu sync.Mutex // serialises writes to the child's stdin
	w   io.Writer

	mu      sync.Mutex
	nextID  int64
	pending map[string]func(acpFrame) // response id (raw JSON) → callback
	closed  bool

	// onNotification handles agent→client notifications (session/update), in
	// stream order, on the read-loop goroutine.
	onNotification func(method string, params json.RawMessage)
	// onRequest handles agent→client requests (session/request_permission,
	// fs/*). It runs on its own goroutine — a permission prompt can wait
	// minutes for the human and must not stall the update stream.
	onRequest func(f acpFrame)
}

func newACPClient(w io.Writer, log *slog.Logger) *acpClient {
	return &acpClient{
		log:     log,
		w:       w,
		pending: make(map[string]func(acpFrame)),
	}
}

// readLoop dispatches frames from the child's stdout until the stream ends,
// then fails every pending call so no caller blocks forever.
func (c *acpClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f acpFrame
		if json.Unmarshal(line, &f) != nil {
			c.log.Debug("kimi acp: undecodable frame", "line", string(line))
			continue
		}
		switch {
		case f.Method != "" && len(f.ID) > 0:
			frame := f
			safe.Go("kimi.acpRequest", func() { c.onRequest(frame) })
		case f.Method != "":
			c.onNotification(f.Method, f.Params)
		default:
			c.dispatchResponse(f)
		}
	}
	c.failAll(errStreamClosed)
}

func (c *acpClient) dispatchResponse(f acpFrame) {
	key := string(f.ID)
	c.mu.Lock()
	cb, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if ok {
		cb(f)
	}
}

// failAll completes every pending call with err. Called when stdout closes.
func (c *acpClient) failAll(err error) {
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]func(acpFrame))
	c.mu.Unlock()
	for _, cb := range pending {
		cb(acpFrame{Error: &acpError{Code: -32000, Message: err.Error()}})
	}
}

// isStreamClosed reports whether a call failure came from the stream closing
// (process exit) rather than from a real RPC-level rejection.
func isStreamClosed(err error) bool {
	var ae *acpError
	return errors.As(err, &ae) && ae.Message == errStreamClosed.Error()
}

// send registers cb for the response and writes the request, returning the
// pending-callback key so a synchronous caller can unregister it on timeout.
// The callback runs on the read-loop goroutine.
func (c *acpClient) send(method string, params any, cb func(acpFrame)) (string, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", errStreamClosed
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	c.pending[key] = cb
	c.mu.Unlock()

	if err := c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return key, err
	}
	return key, nil
}

// call performs a synchronous request, waiting for the response or ctx.
func (c *acpClient) call(ctx context.Context, method string, params, out any) error {
	ch := make(chan acpFrame, 1)
	key, err := c.send(method, params, func(f acpFrame) { ch <- f })
	if err != nil {
		return err
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
		// Unregister the callback so a peer that never answers doesn't leak a
		// pending-map entry for the process's lifetime.
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// notify sends a notification (no id, no response) — session/cancel.
func (c *acpClient) notify(method string, params any) error {
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// respond answers an agent→client request, echoing its id verbatim (kimi
// allocates its own ids, numeric, for reverse requests).
func (c *acpClient) respond(id json.RawMessage, result any) {
	_ = c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func (c *acpClient) respondError(id json.RawMessage, code int, msg string) {
	_ = c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   &acpError{Code: code, Message: msg},
	})
}

func (c *acpClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}
