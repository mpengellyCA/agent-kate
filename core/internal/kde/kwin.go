package kde

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// Window is the enumerated KWin window model (plan 03/07 §1.5). Geometry is in
// compositor coordinates. Desktops are virtual-desktop ids (QUuid strings) — note
// KWin's window.desktops is VirtualDesktop[] objects, so the script emits .id per
// element (REFERENCE §CORRECTIONS / 08 §D6).
type Window struct {
	InternalID    string   `json:"internalId"`
	Caption       string   `json:"caption"`
	ResourceClass string   `json:"resourceClass"`
	ResourceName  string   `json:"resourceName"`
	PID           int      `json:"pid"`
	Active        bool     `json:"active"`
	Minimized     bool     `json:"minimized"`
	FullScreen    bool     `json:"fullScreen"`
	OnAllDesktops bool     `json:"onAllDesktops"`
	SkipTaskbar   bool     `json:"skipTaskbar"`
	SpecialWindow bool     `json:"specialWindow"`
	Desktops      []string `json:"desktops"`
	X             int      `json:"x"`
	Y             int      `json:"y"`
	Width         int      `json:"width"`
	Height        int      `json:"height"`
}

// windowListJS enumerates workspace.stackingOrder (KWin 6.x) and reports the result
// back to akcore via callDBus. %s placeholders: bus name, then nonce. Written
// defensively because KWin's QJSEngine is unforgiving and a throw aborts silently.
const windowListJS = `(function(){
  try {
    var out = [];
    var list = workspace.stackingOrder || [];
    for (var i = 0; i < list.length; i++) {
      var w = list[i];
      if (!w) continue;
      var desks = [];
      if (w.desktops) {
        for (var j = 0; j < w.desktops.length; j++) {
          if (w.desktops[j]) desks.push("" + w.desktops[j].id);
        }
      }
      var g = w.frameGeometry || {};
      out.push({
        internalId:    w.internalId ? ("" + w.internalId) : "",
        caption:       w.caption ? ("" + w.caption) : "",
        resourceClass: w.resourceClass ? ("" + w.resourceClass) : "",
        resourceName:  w.resourceName ? ("" + w.resourceName) : "",
        pid:           w.pid || 0,
        active:        !!w.active,
        minimized:     !!w.minimized,
        fullScreen:    !!w.fullScreen,
        onAllDesktops: !!w.onAllDesktops,
        skipTaskbar:   !!w.skipTaskbar,
        specialWindow: !!w.specialWindow,
        desktops:      desks,
        x:      Math.round(g.x || 0),
        y:      Math.round(g.y || 0),
        width:  Math.round(g.width || 0),
        height: Math.round(g.height || 0)
      });
    }
    callDBus("%s", "/WindowList", "io.agentkate.Cowork.WindowList", "Report",
             "%s", JSON.stringify(out));
  } catch (e) {
    callDBus("%s", "/WindowList", "io.agentkate.Cowork.WindowList", "Report",
             "%s", JSON.stringify({error: "" + e}));
  }
})();`

// ListWindows loads a one-shot KWin script that enumerates the live window stack and
// reports it back over the session bus. It returns when the callback arrives or the
// timeout elapses, then stops/unloads the script (no residue across KWin restarts).
func (c *Client) ListWindows(timeout time.Duration) ([]Window, error) {
	payload, err := c.runOneShotScript("winlist", windowListJS, timeout)
	if err != nil {
		return nil, err
	}
	var ws []Window
	if err := json.Unmarshal([]byte(payload), &ws); err != nil {
		return nil, fmt.Errorf("kde: decode window list: %w", err)
	}
	return ws, nil
}

// DesktopRect is an absolute desktop rectangle in the same compositor pixels Window geometry
// and the pointer tools use.
type DesktopRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Valid reports whether the rectangle has a usable extent. A zero/negative one means
// "we could not learn the desktop's size", which every caller must treat as unknown.
func (r DesktopRect) Valid() bool { return r.Width > 0 && r.Height > 0 }

// Contains is the half-open [X, X+Width) x [Y, Y+Height) test.
func (r DesktopRect) Contains(x, y int) bool {
	return r.Valid() && x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height
}

// DesktopLayout is the compositor's screen arrangement: the union box the pointer cannot
// leave, PLUS each screen's own rectangle when KWin could enumerate them.
//
// SECURITY: the union alone is not the reachable area. Screens laid out in an L (or at
// different heights, or with a gap) leave dead corners inside the union that no screen
// covers — and the compositor never puts the cursor there. A containment test against the
// union therefore CLEARS accumulated positions the cursor cannot possibly hold, which is
// exactly the "the mirror says somewhere the cursor is not" failure the mirror exists to
// prevent. Contains() prefers per-screen containment and only falls back to the union when
// the screen list is unavailable.
type DesktopLayout struct {
	Union   DesktopRect   `json:"union"`
	Screens []DesktopRect `json:"screens"`
}

// Valid reports whether the layout has a usable extent at all.
func (l DesktopLayout) Valid() bool { return l.Union.Valid() }

// Contains reports whether (x,y) is a position the pointer can actually occupy: inside
// some screen when the screen list is known, else inside the union. An invalid layout
// contains nothing — callers fail closed on it.
func (l DesktopLayout) Contains(x, y int) bool {
	if !l.Union.Valid() {
		return false
	}
	if len(l.Screens) == 0 {
		return l.Union.Contains(x, y)
	}
	for _, s := range l.Screens {
		if s.Contains(x, y) {
			return true
		}
	}
	return false
}

// desktopBoundsJS reports the union of every screen — the rectangle the compositor
// clamps the pointer into. Written against three KWin 6 spellings in decreasing order
// of precision because a wrong answer here is a security answer: the relative-pointer
// mirror uses it to decide whether an accumulated position could have been clamped
// (see cowork_pointer.go). Anything it cannot determine comes back as a zero extent,
// which callers must treat as "unknown" and fail closed on.
const desktopBoundsJS = `(function(){
  try {
    var x = 0, y = 0, w = 0, h = 0;
    var g = workspace.virtualScreenGeometry;
    if (g && g.width > 0 && g.height > 0) {
      x = Math.round(g.x || 0); y = Math.round(g.y || 0);
      w = Math.round(g.width);  h = Math.round(g.height);
    }
    // Per-screen rectangles are collected ALWAYS (not only as a union fallback): the
    // union of an L-shaped or unequal-height layout contains dead space no screen covers,
    // and the pointer mirror must test containment per screen. An empty list means KWin
    // would not tell us, and callers fall back to the union.
    var screens = [];
    if (workspace.screens && workspace.screens.length > 0) {
      var x0 = null, y0 = null, x1 = null, y1 = null;
      for (var i = 0; i < workspace.screens.length; i++) {
        var s = workspace.screens[i];
        if (!s) continue;
        var r = s.geometry;
        if (!r || !(r.width > 0) || !(r.height > 0)) continue;
        var rx = Math.round(r.x || 0), ry = Math.round(r.y || 0);
        var rw = Math.round(r.width), rh = Math.round(r.height);
        screens.push({x: rx, y: ry, width: rw, height: rh});
        if (x0 === null || rx < x0) x0 = rx;
        if (y0 === null || ry < y0) y0 = ry;
        if (x1 === null || rx + rw > x1) x1 = rx + rw;
        if (y1 === null || ry + rh > y1) y1 = ry + rh;
      }
      if (!(w > 0 && h > 0) && x0 !== null) { x = x0; y = y0; w = x1 - x0; h = y1 - y0; }
    }
    if (!(w > 0 && h > 0)) {
      var sz = workspace.virtualScreenSize;
      if (sz && sz.width > 0 && sz.height > 0) {
        x = 0; y = 0; w = Math.round(sz.width); h = Math.round(sz.height);
      }
    }
    if (!(w > 0 && h > 0)) {
      if (workspace.workspaceWidth > 0 && workspace.workspaceHeight > 0) {
        x = 0; y = 0;
        w = Math.round(workspace.workspaceWidth);
        h = Math.round(workspace.workspaceHeight);
      }
    }
    callDBus("%s", "/WindowList", "io.agentkate.Cowork.WindowList", "Report",
             "%s", JSON.stringify({x: x, y: y, width: w, height: h, screens: screens}));
  } catch (e) {
    callDBus("%s", "/WindowList", "io.agentkate.Cowork.WindowList", "Report",
             "%s", JSON.stringify({error: "" + e}));
  }
})();`

// DesktopBounds returns the compositor's screen layout: the union of all screens (the box
// the pointer cannot leave) plus each screen's own rectangle when KWin enumerated them.
// An error, or a zero-extent union, means "unknown": callers must not guess (the
// relative-pointer mirror invalidates itself instead).
func (c *Client) DesktopBounds(timeout time.Duration) (DesktopLayout, error) {
	payload, err := c.runOneShotScript("bounds", desktopBoundsJS, timeout)
	if err != nil {
		return DesktopLayout{}, err
	}
	var raw struct {
		DesktopRect
		Screens []DesktopRect `json:"screens"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return DesktopLayout{}, fmt.Errorf("kde: decode desktop bounds: %w", err)
	}
	if !raw.DesktopRect.Valid() {
		return DesktopLayout{}, fmt.Errorf("kde: KWin reported no usable desktop geometry")
	}
	// A screen with no usable extent tells us nothing and must not shrink the reachable
	// area to "nothing but the others" by accident — drop it. If that empties the list,
	// Contains falls back to the union, which is the pre-existing (looser) behaviour.
	layout := DesktopLayout{Union: raw.DesktopRect}
	for _, s := range raw.Screens {
		if s.Valid() {
			layout.Screens = append(layout.Screens, s)
		}
	}
	return layout, nil
}

// runOneShotScript loads a one-shot KWin script whose JS takes four %s placeholders
// (bus name + nonce, twice: the success and the error callback), runs it, and returns
// the single JSON payload it reports back. The script is stopped AND unloaded on every
// path — KWin keeps a Script object alive per loadScript, so skipping that leaks one
// per call. A JS-level throw arrives as {"error": "..."} and is surfaced as an error.
func (c *Client) runOneShotScript(kind, js string, timeout time.Duration) (string, error) {
	if c == nil || c.conn == nil {
		return "", fmt.Errorf("kde: client closed")
	}
	nonce := randHex(8)
	ch := c.registerNonce(nonce)
	defer c.unregisterNonce(nonce)

	src := fmt.Sprintf(js, c.busName, nonce, c.busName, nonce)
	tmp, err := writeTempScript("ak_kwin_"+kind+"_", src)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	scripting := c.conn.Object("org.kde.KWin", dbus.ObjectPath("/Scripting"))
	var id int32
	pluginName := "akcore_" + kind + "_" + nonce
	if err := scripting.Call("org.kde.kwin.Scripting.loadScript", 0, tmp, pluginName).Store(&id); err != nil {
		return "", fmt.Errorf("kde: loadScript (is KWin running?): %w", err)
	}

	scriptObj := c.conn.Object("org.kde.KWin", dbus.ObjectPath(fmt.Sprintf("/Scripting/Script%d", id)))
	stop := func() {
		_ = scriptObj.Call("org.kde.kwin.Script.stop", 0).Err
		_ = scripting.Call("org.kde.kwin.Scripting.unloadScript", 0, pluginName).Err
	}

	if runErr := scriptObj.Call("org.kde.kwin.Script.run", 0).Err; runErr != nil {
		// Fallback: start() runs all pending scripts (older/edge KWin object-path layouts).
		if startErr := scripting.Call("org.kde.kwin.Scripting.start", 0).Err; startErr != nil {
			stop()
			return "", fmt.Errorf("kde: run script failed (%v) and start failed (%v)", runErr, startErr)
		}
	}

	select {
	case payload := <-ch:
		stop()
		// The error path of the JS reports {"error": "..."}.
		var probe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &probe) == nil && probe.Error != "" {
			return "", fmt.Errorf("kde: KWin script error: %s", probe.Error)
		}
		return payload, nil
	case <-time.After(timeout):
		stop()
		return "", fmt.Errorf("kde: timed out after %s waiting for KWin %s callback", timeout, kind)
	}
}

func writeTempScript(prefix, content string) (string, error) {
	f, err := os.CreateTemp("", prefix+"*.js")
	if err != nil {
		return "", fmt.Errorf("kde: temp script: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("kde: write temp script: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal-grade; fall back to a fixed-but-unique-ish token.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
