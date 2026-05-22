package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
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
}

// Dial connects to a core listening on socketPath.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, w: bufio.NewWriter(conn)}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Frame
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.ID == nil {
			continue // notifications are not consumed by the bridge
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
