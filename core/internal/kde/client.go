// Package kde holds the raw D-Bus clients the Cowork feature uses to reach the
// live KDE Plasma session: KWin scripting (window enumeration, virtual desktops)
// and, later, AT-SPI2 and portal coordination. It contains ZERO consent logic —
// that lives in core/internal/cowork. See docs/plans/08-kde-cowork/ (07 §2).
//
// One process, one session-bus connection: Client owns a single *dbus.Conn shared
// by every KDE-facing subsystem. The AT-SPI second bus is dialed lazily (v2).
package kde

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	windowListPath  = dbus.ObjectPath("/WindowList")
	windowListIface = "io.agentkate.Cowork.WindowList"
)

// Client owns the shared godbus session-bus connection and the callback service
// that KWin scripts call back into via callDBus(). All methods are safe for
// concurrent use.
type Client struct {
	log     *slog.Logger
	conn    *dbus.Conn
	busName string

	mu      sync.Mutex
	reports map[string]chan string // nonce -> first Report payload (one-shot rendezvous)

	atspiMu   sync.Mutex
	atspiConn *dbus.Conn // the AT-SPI accessibility bus, dialed lazily (see atspi.go)
}

// New connects to the session bus, exports the window-list callback receiver, and
// claims a per-pid bus name so KWin scripts can reach us. It does not require KWin
// to be present; ListWindows surfaces a missing/unresponsive compositor as an error.
func New(log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("kde: connect session bus: %w", err)
	}
	c := &Client{log: log, conn: conn, reports: map[string]chan string{}}
	c.busName = fmt.Sprintf("io.agentkate.Cowork.akcore_%d", os.Getpid())

	// Export the callback receiver BEFORE claiming the name.
	if err := conn.Export((*reporter)(c), windowListPath, windowListIface); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("kde: export window-list receiver: %w", err)
	}
	reply, err := conn.RequestName(c.busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("kde: request name %s: %w", c.busName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		_ = conn.Close()
		return nil, fmt.Errorf("kde: bus name %s not acquired (reply=%d)", c.busName, reply)
	}
	c.log.Info("kde client ready", "busName", c.busName)
	return c, nil
}

// BusName is the session-bus name KWin scripts call back into.
func (c *Client) BusName() string { return c.busName }

// Conn exposes the shared connection for sibling subsystems (sandbox, portalcoord).
func (c *Client) Conn() *dbus.Conn { return c.conn }

// Close releases the bus name and closes the connection. Safe to call once.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	c.atspiMu.Lock()
	if c.atspiConn != nil {
		_ = c.atspiConn.Close()
		c.atspiConn = nil
	}
	c.atspiMu.Unlock()
	_, _ = c.conn.ReleaseName(c.busName)
	err := c.conn.Close()
	c.conn = nil
	return err
}

// reporter is the D-Bus object KWin scripts invoke via callDBus(... "Report", ...).
// It shares storage with Client (type reporter Client) so Report reaches the same
// rendezvous map; only Report is exported on the windowListIface interface.
type reporter Client

// Report receives one window-list JSON payload keyed by the per-call nonce.
func (r *reporter) Report(nonce string, payload string) *dbus.Error {
	c := (*Client)(r)
	c.mu.Lock()
	ch := c.reports[nonce]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- payload:
		default: // already delivered; ignore duplicates
		}
	}
	return nil
}

func (c *Client) registerNonce(nonce string) chan string {
	ch := make(chan string, 1)
	c.mu.Lock()
	c.reports[nonce] = ch
	c.mu.Unlock()
	return ch
}

func (c *Client) unregisterNonce(nonce string) {
	c.mu.Lock()
	delete(c.reports, nonce)
	c.mu.Unlock()
}
