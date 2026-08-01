package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"agentkate/internal/safe"
)

// callTimeout bounds how long a Client.Call waits for a response.
const callTimeout = 30 * time.Second

// Client is a JSON-RPC 2.0 client over a Unix domain socket — the Go-side
// counterpart of the C++ CoreClient, used by the MCP bridge to reach the core.
type Client struct {
	conn net.Conn
	w    *bufio.Writer
	wmu  sync.Mutex

	nextID  atomic.Int64
	pending sync.Map // int64 id -> chan *Frame

	// onNotify receives server-initiated notifications (no id). Nil by
	// default: a client that never registers one simply drops them, which is
	// what every caller did before core→bridge pushes existed.
	notifyMu sync.RWMutex
	onNotify func(method string, params json.RawMessage)
}

// Dial connects to a core listening on socketPath.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, w: bufio.NewWriter(conn)}
	safe.Go("ipc.client.readLoop", func() { c.readLoop() })
	return c, nil
}

// readLoop drains inbound frames until the connection fails. It uses the shared
// resilient frameReader rather than a bufio.Scanner: a Scanner treats an
// over-long frame as terminal, so one huge core→bridge result (a
// cowork.screenshot, say) silently ended this loop while the Client stayed
// alive — every later Call then blocked for its full timeout. An oversize frame
// is logged and skipped instead; only a real read error ends the loop, and it
// fails the pending calls fast.
func (c *Client) readLoop() {
	fr := newFrameReader(c.conn)
	for {
		line, oversize, err := fr.readFrame()
		if oversize > 0 {
			slog.Warn("oversize frame from core discarded",
				"bytes", oversize, "cap", maxFrameBytes)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Warn("core connection read error", "err", err)
			}
			break
		}
		if oversize > 0 || len(line) == 0 {
			continue
		}
		// Frame's json.RawMessage fields copy on unmarshal, so the decoded frame
		// does not alias the reader's reusable buffer.
		var f Frame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.ID == nil {
			// Server-initiated notification. Delivered on this read loop, so a
			// handler must not block: the bridge's only handler writes one MCP
			// line to stdout.
			c.notifyMu.RLock()
			h := c.onNotify
			c.notifyMu.RUnlock()
			if h != nil {
				h(f.Method, f.Params)
			}
			continue
		}
		var id int64
		if json.Unmarshal(*f.ID, &id) != nil {
			continue
		}
		if ch, ok := c.pending.LoadAndDelete(id); ok {
			resp := f
			ch.(chan *Frame) <- &resp
		}
	}
	// Connection closed: fail any in-flight calls.
	c.pending.Range(func(key, value any) bool {
		c.pending.Delete(key)
		close(value.(chan *Frame))
		return true
	})
}

// OnNotify registers the handler for server-initiated notifications. Call it
// before the first notification can arrive (i.e. right after Dial).
func (c *Client) OnNotify(h func(method string, params json.RawMessage)) {
	c.notifyMu.Lock()
	c.onNotify = h
	c.notifyMu.Unlock()
}

// Call issues a request and waits up to the default timeout for the response.
func (c *Client) Call(method string, params any, out any) error {
	return c.CallTimeout(method, params, out, callTimeout)
}

// CallTimeout is Call with an explicit timeout — used for permission requests,
// which may park for as long as the human takes to decide.
func (c *Client) CallTimeout(method string, params any, out any, timeout time.Duration) error {
	id := c.nextID.Add(1)
	pb, err := json.Marshal(params)
	if err != nil {
		return err
	}
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	frame, err := json.Marshal(Frame{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: pb})
	if err != nil {
		return err
	}

	ch := make(chan *Frame, 1)
	c.pending.Store(id, ch)

	c.wmu.Lock()
	_, werr := c.w.Write(append(frame, '\n'))
	if werr == nil {
		werr = c.w.Flush()
	}
	c.wmu.Unlock()
	if werr != nil {
		c.pending.Delete(id)
		return werr
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return errors.New("core connection closed")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-time.After(timeout):
		c.pending.Delete(id)
		return fmt.Errorf("call %q timed out", method)
	}
}

// Notify sends a notification (no response expected).
func (c *Client) Notify(method string, params any) error {
	pb, err := json.Marshal(params)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(Frame{JSONRPC: "2.0", Method: method, Params: pb})
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(append(frame, '\n')); err != nil {
		return err
	}
	return c.w.Flush()
}

// Close shuts down the connection.
func (c *Client) Close() error { return c.conn.Close() }
