package kde

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// AT-SPI2 is the desktop accessibility bus. It lets us enumerate the *actionable*
// widgets of a window (links, buttons, fields, menu items) WITH labels, and fire a
// widget's own default action via Action.DoAction — i.e. "click this link" with no
// pointer movement, no focus games, and no chance of a stuck pointer grab. It is the
// cursor-free interaction path that replaces blind coordinate clicks (plan 08 §v2
// "AT-SPI desktop_click_element"). This file holds ZERO consent logic — that stays
// in core/internal/cowork; here we only speak D-Bus to org.a11y.*.

const (
	atspiRegistryName = "org.a11y.atspi.Registry"
	atspiRootPath     = dbus.ObjectPath("/org/a11y/atspi/accessible/root")

	ifaceAccessible   = "org.a11y.atspi.Accessible"
	ifaceComponent    = "org.a11y.atspi.Component"
	ifaceAction       = "org.a11y.atspi.Action"
	ifaceText         = "org.a11y.atspi.Text"
	ifaceEditableText = "org.a11y.atspi.EditableText"

	atspiCoordScreen = uint32(0) // GetExtents coordinate type: absolute screen pixels
)

// AT-SPI StateType bit positions (atspi-constants.h) packed into the 64-bit state
// bitfield returned as two uint32s.
const (
	stEditable  = 7
	stSensitive = 24
	stShowing   = 25
	stVisible   = 30
)

// Walk bounds — AT-SPI tree traversal is one D-Bus round trip per node, so we cap
// breadth/depth/time hard and report truncation rather than hammering the bus.
const (
	atspiMaxVisited = 6000
	atspiMaxCollect = 200
	atspiMaxDepth   = 40
)

// atspiActionableRoles are roles we surface as directly-activatable. Role names come
// from GetRoleName (stable, human-readable: "push button", "link", …).
var atspiActionableRoles = map[string]bool{
	"push button": true, "toggle button": true, "radio button": true,
	"check box": true, "check menu item": true, "radio menu item": true,
	"menu item": true, "menu": true, "link": true, "list item": true,
	"combo box": true, "page tab": true, "tree item": true, "table cell": true,
	"spin button": true, "slider": true, "button": true,
}

// atspiEditableRoles are text-entry roles; we surface them only when the node is
// actually EDITABLE (so static labels rendered as "text" are excluded).
var atspiEditableRoles = map[string]bool{
	"entry": true, "password text": true, "text": true,
	"paragraph": true, "document text": true,
}

// atspiContentRoles are the text-bearing roles ReadText harvests prose from. They are
// taken as whole blocks (not descended into) so a paragraph's inline runs aren't
// double-counted. Form fields are excluded — reading their contents risks leaking
// secrets and isn't page prose.
var atspiContentRoles = map[string]bool{
	"heading": true, "paragraph": true, "static text": true, "text": true,
	"label": true, "list item": true, "caption": true, "block quote": true,
	"table cell": true, "link": true, "term": true, "definition": true,
}

// Rect is a screen rectangle in absolute pixels (used to spatially filter elements
// to a target window when pid matching is unavailable).
type Rect struct{ X, Y, W, H int }

// AtspiElement is one actionable widget the agent can target by id.
type AtspiElement struct {
	ID       string   `json:"id"`       // opaque token (busName+path), pass back to activate
	Role     string   `json:"role"`     // e.g. "push button", "link", "entry"
	Name     string   `json:"name"`     // accessible label
	Editable bool     `json:"editable"` // a text field SetText can fill
	Actions  []string `json:"actions"`  // available action names (default is index 0)
	X        int      `json:"x"`
	Y        int      `json:"y"`
	W        int      `json:"w"`
	H        int      `json:"h"`
}

// ElementContext is the lightweight metadata the consent layer needs to name an
// element and map it back to a window (for the self-target guard) before activating.
type ElementContext struct {
	Role    string
	Name    string
	PID     int
	Actions []string
}

// atspiRef is the AT-SPI object reference type (so): a connection name + object path.
type atspiRef struct {
	Name string
	Path dbus.ObjectPath
}

// atspiConnect dials the accessibility bus lazily and caches it. The address comes
// from org.a11y.Bus.GetAddress on the session bus; the a11y bus is a *separate*
// dbus-daemon, so it needs its own auth + Hello handshake.
func (c *Client) atspiConnect(ctx context.Context) (*dbus.Conn, error) {
	c.atspiMu.Lock()
	defer c.atspiMu.Unlock()
	if c.atspiConn != nil {
		if c.atspiConn.Connected() {
			return c.atspiConn, nil
		}
		_ = c.atspiConn.Close() // stale (a11y daemon restarted) — drop and re-dial
		c.atspiConn = nil
	}

	var addr string
	obj := c.conn.Object("org.a11y.Bus", dbus.ObjectPath("/org/a11y/bus"))
	if err := obj.CallWithContext(ctx, "org.a11y.Bus.GetAddress", 0).Store(&addr); err != nil {
		return nil, fmt.Errorf("kde: accessibility bus unavailable (is the a11y bus running / accessibility enabled?): %w", err)
	}
	if addr == "" {
		return nil, fmt.Errorf("kde: the accessibility bus returned an empty address")
	}
	conn, err := dbus.Dial(addr)
	if err != nil {
		return nil, fmt.Errorf("kde: dial a11y bus: %w", err)
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("kde: a11y bus auth: %w", err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("kde: a11y bus hello: %w", err)
	}
	c.atspiConn = conn
	return conn, nil
}

// --- low-level accessible accessors (one D-Bus call each) ------------------------

func (c *Client) a11yObj(conn *dbus.Conn, ref atspiRef) dbus.BusObject {
	return conn.Object(ref.Name, ref.Path)
}

func a11yProp(ctx context.Context, obj dbus.BusObject, iface, prop string) (dbus.Variant, error) {
	var v dbus.Variant
	err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, prop).Store(&v)
	return v, err
}

func (c *Client) a11yRoleName(ctx context.Context, conn *dbus.Conn, ref atspiRef) (string, error) {
	var name string
	err := c.a11yObj(conn, ref).CallWithContext(ctx, ifaceAccessible+".GetRoleName", 0).Store(&name)
	return name, err
}

func (c *Client) a11yName(ctx context.Context, conn *dbus.Conn, ref atspiRef) string {
	v, err := a11yProp(ctx, c.a11yObj(conn, ref), ifaceAccessible, "Name")
	if err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return strings.TrimSpace(s)
}

func (c *Client) a11yChildCount(ctx context.Context, conn *dbus.Conn, ref atspiRef) int {
	v, err := a11yProp(ctx, c.a11yObj(conn, ref), ifaceAccessible, "ChildCount")
	if err != nil {
		return 0
	}
	switch n := v.Value().(type) {
	case int32:
		return int(n)
	case int:
		return n
	}
	return 0
}

func (c *Client) a11yChild(ctx context.Context, conn *dbus.Conn, ref atspiRef, i int) (atspiRef, error) {
	var out atspiRef
	err := c.a11yObj(conn, ref).CallWithContext(ctx, ifaceAccessible+".GetChildAtIndex", 0, int32(i)).Store(&out)
	return out, err
}

func (c *Client) a11yState(ctx context.Context, conn *dbus.Conn, ref atspiRef) []uint32 {
	var st []uint32
	if err := c.a11yObj(conn, ref).CallWithContext(ctx, ifaceAccessible+".GetState", 0).Store(&st); err != nil {
		return nil
	}
	return st
}

func stateHas(st []uint32, bit uint) bool {
	idx := bit / 32
	if int(idx) >= len(st) {
		return false
	}
	return st[idx]&(1<<(bit%32)) != 0
}

func (c *Client) a11yExtents(ctx context.Context, conn *dbus.Conn, ref atspiRef) (x, y, w, h int, ok bool) {
	var r struct{ X, Y, W, H int32 }
	if err := c.a11yObj(conn, ref).CallWithContext(ctx, ifaceComponent+".GetExtents", 0, atspiCoordScreen).Store(&r); err != nil {
		return 0, 0, 0, 0, false
	}
	return int(r.X), int(r.Y), int(r.W), int(r.H), true
}

func (c *Client) a11yActions(ctx context.Context, conn *dbus.Conn, ref atspiRef) []string {
	obj := c.a11yObj(conn, ref)
	v, err := a11yProp(ctx, obj, ifaceAction, "NActions")
	if err != nil {
		return nil
	}
	n := 0
	switch x := v.Value().(type) {
	case int32:
		n = int(x)
	case int:
		n = x
	}
	var names []string
	for i := 0; i < n && i < 12; i++ {
		var nm string
		if err := obj.CallWithContext(ctx, ifaceAction+".GetName", 0, int32(i)).Store(&nm); err == nil && nm != "" {
			names = append(names, nm)
		}
	}
	return names
}

// pidOf maps an a11y-bus connection name to the owning process via the bus driver.
func (c *Client) a11yPID(ctx context.Context, conn *dbus.Conn, busName string) int {
	var pid uint32
	err := conn.Object("org.freedesktop.DBus", dbus.ObjectPath("/org/freedesktop/DBus")).
		CallWithContext(ctx, "org.freedesktop.DBus.GetConnectionUnixProcessID", 0, busName).Store(&pid)
	if err != nil {
		return 0
	}
	return int(pid)
}

// --- id encoding -----------------------------------------------------------------

func encodeElementID(ref atspiRef) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ref.Name + "\n" + string(ref.Path)))
}

func decodeElementID(id string) (atspiRef, error) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return atspiRef{}, fmt.Errorf("kde: malformed element id")
	}
	parts := strings.SplitN(string(raw), "\n", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return atspiRef{}, fmt.Errorf("kde: malformed element id")
	}
	return atspiRef{Name: parts[0], Path: dbus.ObjectPath(parts[1])}, nil
}

func rectsIntersect(ax, ay, aw, ah, bx, by, bw, bh int) bool {
	return ax < bx+bw && ax+aw > bx && ay < by+bh && ay+ah > by
}

// --- public API ------------------------------------------------------------------

// ListElements indexes the actionable elements of the application(s) backing the
// target window. It matches by pid; if no a11y application reports that pid (some
// apps expose accessibility from a different process) it falls back to scanning every
// application and keeping only elements whose on-screen bounds fall inside winRect.
// truncated reports whether a cap stopped the walk early.
func (c *Client) ListElements(targetPID int, winRect Rect, max int, timeout time.Duration) (elems []AtspiElement, truncated bool, err error) {
	if max <= 0 || max > atspiMaxCollect {
		max = atspiMaxCollect
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := c.atspiConnect(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { sortElementsReadingOrder(elems) }()

	// Prefer apps whose pid matches the window; else scan all and geometry-filter.
	apps, geomFilter := c.atspiApps(ctx, conn, targetPID)
	geomFilter = geomFilter && winRect.W > 0 && winRect.H > 0

	type queued struct {
		ref   atspiRef
		depth int
	}
	visited := 0
	for _, app := range apps {
		queue := []queued{{app, 0}}
		for len(queue) > 0 {
			if ctx.Err() != nil {
				return elems, true, nil
			}
			if visited >= atspiMaxVisited || len(elems) >= max {
				return elems, true, nil
			}
			node := queue[0]
			queue = queue[1:]
			visited++

			roleName, e := c.a11yRoleName(ctx, conn, node.ref)
			if e != nil {
				continue // defunct / vanished node
			}
			rl := strings.ToLower(roleName)

			candidate := atspiActionableRoles[rl]
			editableRole := atspiEditableRoles[rl]
			if candidate || editableRole {
				// One state fetch decides editability AND the showing/visible/sensitive
				// filter, so a candidate node costs a single extra round trip.
				st := c.a11yState(ctx, conn, node.ref)
				editable := editableRole && stateHas(st, stEditable)
				collect := candidate || editable
				usable := stateHas(st, stShowing) && stateHas(st, stVisible) && stateHas(st, stSensitive)
				if collect && usable {
					x, y, w, h, okE := c.a11yExtents(ctx, conn, node.ref)
					onScreen := okE && w > 0 && h > 0
					if onScreen && (!geomFilter || rectsIntersect(x, y, w, h, winRect.X, winRect.Y, winRect.W, winRect.H)) {
						elems = append(elems, AtspiElement{
							ID:       encodeElementID(node.ref),
							Role:     roleName,
							Name:     c.a11yName(ctx, conn, node.ref),
							Editable: editable,
							Actions:  c.a11yActions(ctx, conn, node.ref),
							X:        x, Y: y, W: w, H: h,
						})
					}
				}
			}

			if node.depth < atspiMaxDepth {
				n := c.a11yChildCount(ctx, conn, node.ref)
				for i := 0; i < n; i++ {
					child, ce := c.a11yChild(ctx, conn, node.ref, i)
					if ce == nil && child.Name != "" {
						queue = append(queue, queued{child, node.depth + 1})
					}
				}
			}
		}
	}
	return elems, truncated, nil
}

// ElementInfo returns the metadata the consent layer needs before activating: role,
// label, available actions, and the owning pid (to map the element back to a window).
func (c *Client) ElementInfo(id string, timeout time.Duration) (ElementContext, error) {
	ref, err := decodeElementID(id)
	if err != nil {
		return ElementContext{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.atspiConnect(ctx)
	if err != nil {
		return ElementContext{}, err
	}
	role, err := c.a11yRoleName(ctx, conn, ref)
	if err != nil {
		return ElementContext{}, fmt.Errorf("kde: the element is no longer available (re-list elements)")
	}
	return ElementContext{
		Role:    role,
		Name:    c.a11yName(ctx, conn, ref),
		PID:     c.a11yPID(ctx, conn, ref.Name),
		Actions: c.a11yActions(ctx, conn, ref),
	}, nil
}

// ActivateElement fires an element's action by name (empty = the default action,
// index 0) directly through AT-SPI — no pointer movement involved.
func (c *Client) ActivateElement(id, action string, timeout time.Duration) error {
	ref, err := decodeElementID(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.atspiConnect(ctx)
	if err != nil {
		return err
	}
	obj := c.a11yObj(conn, ref)

	idx := 0
	if action != "" {
		names := c.a11yActions(ctx, conn, ref)
		found := -1
		for i, nm := range names {
			if strings.EqualFold(strings.TrimSpace(nm), strings.TrimSpace(action)) {
				found = i
				break
			}
		}
		if found < 0 {
			return fmt.Errorf("kde: no action %q on this element (available: %s)", action, strings.Join(names, ", "))
		}
		idx = found
	}

	var ok bool
	if err := obj.CallWithContext(ctx, ifaceAction+".DoAction", 0, int32(idx)).Store(&ok); err != nil {
		return fmt.Errorf("kde: activate element: %w", err)
	}
	if !ok {
		return fmt.Errorf("kde: the element did not accept the action")
	}
	return nil
}

// SetElementText replaces the contents of an editable text element directly (no
// per-character keystrokes, so no risk of dropped/misrouted characters).
func (c *Client) SetElementText(id, text string, timeout time.Duration) error {
	ref, err := decodeElementID(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.atspiConnect(ctx)
	if err != nil {
		return err
	}
	var ok bool
	if err := c.a11yObj(conn, ref).CallWithContext(ctx, ifaceEditableText+".SetTextContents", 0, text).Store(&ok); err != nil {
		return fmt.Errorf("kde: set element text (is it an editable field?): %w", err)
	}
	if !ok {
		return fmt.Errorf("kde: the element rejected the text (it may not be editable)")
	}
	return nil
}

// atspiApps returns the application accessibles to scan for a target window: those
// whose pid matches, or — when none report that pid — every application (with
// geomFilter true so the caller spatially clips results to the window).
func (c *Client) atspiApps(ctx context.Context, conn *dbus.Conn, targetPID int) (apps []atspiRef, geomFilter bool) {
	root := atspiRef{Name: atspiRegistryName, Path: atspiRootPath}
	appCount := c.a11yChildCount(ctx, conn, root)
	for i := 0; i < appCount; i++ {
		app, e := c.a11yChild(ctx, conn, root, i)
		if e != nil || app.Name == "" {
			continue
		}
		if targetPID > 0 && c.a11yPID(ctx, conn, app.Name) == targetPID {
			apps = append(apps, app)
		}
	}
	if len(apps) > 0 {
		return apps, false
	}
	for i := 0; i < appCount; i++ {
		app, e := c.a11yChild(ctx, conn, root, i)
		if e != nil || app.Name == "" {
			continue
		}
		apps = append(apps, app)
	}
	return apps, true
}

// a11yTextOf returns the rendered text of a node — its Text-interface contents if it
// has any, else its accessible Name.
func (c *Client) a11yTextOf(ctx context.Context, conn *dbus.Conn, ref atspiRef) string {
	obj := c.a11yObj(conn, ref)
	if v, err := a11yProp(ctx, obj, ifaceText, "CharacterCount"); err == nil {
		n := 0
		switch x := v.Value().(type) {
		case int32:
			n = int(x)
		case int:
			n = x
		}
		if n > 0 {
			var s string
			if e := obj.CallWithContext(ctx, ifaceText+".GetText", 0, int32(0), int32(n)).Store(&s); e == nil {
				return s
			}
		}
	}
	return c.a11yName(ctx, conn, ref)
}

// ReadText extracts the readable prose of the target window's content from the
// accessibility tree — headings and paragraphs, in document order — so an agent can
// read an article without OCR'ing a downscaled screenshot. Text-bearing roles are
// taken as whole blocks (not descended into) to avoid double-counting inline runs;
// form fields are skipped. truncated reports whether a cap stopped the walk.
func (c *Client) ReadText(targetPID int, winRect Rect, maxChars int, timeout time.Duration) (text string, truncated bool, err error) {
	if maxChars <= 0 || maxChars > 80000 {
		maxChars = 20000
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.atspiConnect(ctx)
	if err != nil {
		return "", false, err
	}

	apps, geomFilter := c.atspiApps(ctx, conn, targetPID)
	geomFilter = geomFilter && winRect.W > 0 && winRect.H > 0

	var b strings.Builder
	visited := 0
	type queued struct {
		ref   atspiRef
		depth int
	}
	for _, app := range apps {
		queue := []queued{{app, 0}}
		for len(queue) > 0 {
			if ctx.Err() != nil || visited >= atspiMaxVisited || b.Len() >= maxChars {
				return strings.TrimSpace(b.String()), true, nil
			}
			node := queue[0]
			queue = queue[1:]
			visited++

			roleName, e := c.a11yRoleName(ctx, conn, node.ref)
			if e != nil {
				continue
			}
			rl := strings.ToLower(roleName)

			if atspiContentRoles[rl] {
				st := c.a11yState(ctx, conn, node.ref)
				if stateHas(st, stShowing) && stateHas(st, stVisible) && !stateHas(st, stEditable) {
					take := true
					if geomFilter {
						x, y, w, h, okE := c.a11yExtents(ctx, conn, node.ref)
						take = okE && rectsIntersect(x, y, w, h, winRect.X, winRect.Y, winRect.W, winRect.H)
					}
					if take {
						t := strings.TrimSpace(c.a11yTextOf(ctx, conn, node.ref))
						if t != "" {
							if strings.HasPrefix(rl, "heading") {
								b.WriteString("\n## " + t + "\n")
							} else {
								b.WriteString(t + "\n")
							}
						}
					}
				}
				continue // a content block is taken whole; don't descend into its runs
			}

			if node.depth < atspiMaxDepth {
				n := c.a11yChildCount(ctx, conn, node.ref)
				for i := 0; i < n; i++ {
					child, ce := c.a11yChild(ctx, conn, node.ref, i)
					if ce == nil && child.Name != "" {
						queue = append(queue, queued{child, node.depth + 1})
					}
				}
			}
		}
	}
	return strings.TrimSpace(b.String()), truncated, nil
}

// sortElementsReadingOrder orders elements roughly top-to-bottom, left-to-right so
// the agent sees them in a predictable layout order.
func sortElementsReadingOrder(elems []AtspiElement) {
	sort.SliceStable(elems, func(i, j int) bool {
		// Bucket rows into 24px bands so minor y jitter doesn't scramble a row.
		bi, bj := elems[i].Y/24, elems[j].Y/24
		if bi != bj {
			return bi < bj
		}
		return elems[i].X < elems[j].X
	})
}
