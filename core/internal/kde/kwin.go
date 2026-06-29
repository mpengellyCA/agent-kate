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
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("kde: client closed")
	}
	nonce := randHex(8)
	ch := c.registerNonce(nonce)
	defer c.unregisterNonce(nonce)

	js := fmt.Sprintf(windowListJS, c.busName, nonce, c.busName, nonce)
	tmp, err := writeTempScript("ak_kwin_winlist_", js)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)

	scripting := c.conn.Object("org.kde.KWin", dbus.ObjectPath("/Scripting"))
	var id int32
	pluginName := "akcore_winlist_" + nonce
	if err := scripting.Call("org.kde.kwin.Scripting.loadScript", 0, tmp, pluginName).Store(&id); err != nil {
		return nil, fmt.Errorf("kde: loadScript (is KWin running?): %w", err)
	}

	scriptObj := c.conn.Object("org.kde.KWin", dbus.ObjectPath(fmt.Sprintf("/Scripting/Script%d", id)))
	// stop() halts the script AND unloads it — KWin keeps a Script object alive per
	// loadScript until unloadScript, so without this each call leaks one in KWin.
	stop := func() {
		_ = scriptObj.Call("org.kde.kwin.Script.stop", 0).Err
		_ = scripting.Call("org.kde.kwin.Scripting.unloadScript", 0, pluginName).Err
	}

	if runErr := scriptObj.Call("org.kde.kwin.Script.run", 0).Err; runErr != nil {
		// Fallback: start() runs all pending scripts (older/edge KWin object-path layouts).
		if startErr := scripting.Call("org.kde.kwin.Scripting.start", 0).Err; startErr != nil {
			stop()
			return nil, fmt.Errorf("kde: run script failed (%v) and start failed (%v)", runErr, startErr)
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
			return nil, fmt.Errorf("kde: KWin script error: %s", probe.Error)
		}
		var ws []Window
		if err := json.Unmarshal([]byte(payload), &ws); err != nil {
			return nil, fmt.Errorf("kde: decode window list: %w", err)
		}
		return ws, nil
	case <-time.After(timeout):
		stop()
		return nil, fmt.Errorf("kde: timed out after %s waiting for KWin window-list callback", timeout)
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
