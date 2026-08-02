package kde

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"agentkate/internal/safe"
)

// ActiveWindowEvent is one KWin activation report: the window that just became
// active, as KWin sees it. Error is non-empty when the script itself failed (the
// subscription is then dead and the consumer must FAIL CLOSED).
type ActiveWindowEvent struct {
	InternalID    string `json:"internalId"`
	Caption       string `json:"caption"`
	ResourceClass string `json:"resourceClass"`
	PID           int    `json:"pid"`
	Error         string `json:"error,omitempty"`
}

// ActiveWindowWatch is a live subscription to KWin's window-activation signal.
// C delivers one event per activation, starting with the window active at the moment
// the watch was established. C is CLOSED when the watch is stopped or the underlying
// script dies, so a consumer ranging over it always terminates.
//
// Why this exists (audit F3): a timed injection may span up to 30 s, during which the
// focused window can change under a keystroke stream that follows focus. Point-in-time
// verification at submit is not enough; the target has to be re-verified for the whole
// span. Polling ListWindows would mean loading a KWin script every few hundred ms, so
// the watch loads exactly one resident script and lets KWin push.
type ActiveWindowWatch struct {
	C <-chan ActiveWindowEvent

	once sync.Once
	stop func()
}

// Stop unloads the KWin script and closes C. Safe to call more than once.
func (w *ActiveWindowWatch) Stop() {
	if w == nil || w.stop == nil {
		return
	}
	w.once.Do(w.stop)
}

// activeWindowWatchJS connects to KWin's activation signal and reports every change
// back over callDBus, plus one immediate report of the currently active window (which
// is what proves to the Go side that the subscription is really live). %s order:
// busName, nonce — repeated for the error path.
const activeWindowWatchJS = `(function(){
  var BUS = "%s", NONCE = "%s";
  function rep(o) {
    try {
      callDBus(BUS, "/WindowList", "io.agentkate.Cowork.WindowList", "Report",
               NONCE, JSON.stringify(o));
    } catch (e) {}
  }
  function pack(w) {
    if (!w) return {internalId: "", caption: "", resourceClass: "", pid: 0};
    return {
      internalId:    w.internalId ? ("" + w.internalId) : "",
      caption:       w.caption ? ("" + w.caption) : "",
      resourceClass: w.resourceClass ? ("" + w.resourceClass) : "",
      pid:           w.pid || 0
    };
  }
  try {
    var sig = null;
    if (workspace.windowActivated && workspace.windowActivated.connect) {
      sig = workspace.windowActivated;           // KWin 6
    } else if (workspace.clientActivated && workspace.clientActivated.connect) {
      sig = workspace.clientActivated;           // KWin 5
    }
    if (!sig) {
      rep({error: "this compositor exposes no window-activation signal"});
      return;
    }
    sig.connect(function(w) { rep(pack(w)); });
    rep(pack(workspace.activeWindow || workspace.activeClient));
  } catch (e) {
    rep({error: "" + e});
  }
})();`

// WatchActiveWindow loads a resident KWin script that reports every window activation.
// It returns only once the script has reported the CURRENT active window — so a
// successful return means the subscription is provably live, and every failure mode
// (no KWin, no signal, script threw, no first report inside timeout) is an error the
// caller must treat as "cannot verify" rather than "nothing changed".
//
// The caller MUST call Stop() on the returned watch; until then KWin keeps the script
// (and its signal connection) loaded.
func (c *Client) WatchActiveWindow(timeout time.Duration) (*ActiveWindowWatch, error) {
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("kde: client closed")
	}
	nonce := randHex(8)
	// Headroom: Report drops rather than blocks, and a burst of alt-tabbing must not
	// silently lose the one activation that matters.
	raw := c.registerNonceBuf(nonce, 32)

	js := fmt.Sprintf(activeWindowWatchJS, c.busName, nonce)
	tmp, err := writeTempScript("ak_kwin_actwatch_", js)
	if err != nil {
		c.unregisterNonce(nonce)
		return nil, err
	}

	scripting := c.conn.Object("org.kde.KWin", dbus.ObjectPath("/Scripting"))
	var id int32
	pluginName := "akcore_actwatch_" + nonce
	if err := scripting.Call("org.kde.kwin.Scripting.loadScript", 0, tmp, pluginName).Store(&id); err != nil {
		_ = os.Remove(tmp)
		c.unregisterNonce(nonce)
		return nil, fmt.Errorf("kde: loadScript (is KWin running?): %w", err)
	}
	scriptObj := c.conn.Object("org.kde.KWin", dbus.ObjectPath(fmt.Sprintf("/Scripting/Script%d", id)))
	unload := func() {
		_ = scriptObj.Call("org.kde.kwin.Script.stop", 0).Err
		_ = scripting.Call("org.kde.kwin.Scripting.unloadScript", 0, pluginName).Err
		_ = os.Remove(tmp)
		c.unregisterNonce(nonce)
	}

	if runErr := scriptObj.Call("org.kde.kwin.Script.run", 0).Err; runErr != nil {
		if startErr := scripting.Call("org.kde.kwin.Scripting.start", 0).Err; startErr != nil {
			unload()
			return nil, fmt.Errorf("kde: run script failed (%v) and start failed (%v)", runErr, startErr)
		}
	}

	// The first report is the handshake: it proves the signal was connected. Anything
	// else (timeout, script error) is a failure to establish the watch.
	var first ActiveWindowEvent
	select {
	case payload := <-raw:
		if err := json.Unmarshal([]byte(payload), &first); err != nil {
			unload()
			return nil, fmt.Errorf("kde: decode activation report: %w", err)
		}
		if first.Error != "" {
			unload()
			return nil, fmt.Errorf("kde: window-activation watch: %s", first.Error)
		}
	case <-time.After(timeout):
		unload()
		return nil, fmt.Errorf("kde: timed out after %s establishing the window-activation watch", timeout)
	}

	out := make(chan ActiveWindowEvent, 32)
	done := make(chan struct{})
	w := &ActiveWindowWatch{C: out}
	w.stop = func() {
		close(done) // ends the pump; the pump closes out and unloads the script
	}

	// safe.Go, not a bare goroutine: one panic in a pump must not take down the
	// daemon and orphan every agent (the project rule this file was the last
	// exception to — audit F65).
	safe.Go("kde.actwatch", func() {
		defer close(out)
		defer unload()
		// Replay the handshake event so the consumer sees the starting state without a
		// separate API. Stop() during this send is handled by the same select.
		select {
		case out <- first:
		case <-done:
			return
		}
		for {
			select {
			case <-done:
				return
			case payload, ok := <-raw:
				if !ok {
					// unregisterNonce closed the rendezvous: the watch was torn
					// down without Stop(); exit rather than park forever.
					return
				}
				var ev ActiveWindowEvent
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					ev = ActiveWindowEvent{Error: "malformed activation report"}
				}
				select {
				case out <- ev:
				case <-done:
					return
				}
			}
		}
	})
	return w, nil
}
