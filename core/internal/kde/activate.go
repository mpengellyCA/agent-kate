package kde

import (
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
)

// activateWindowJS focuses the window whose internalId matches, then reports back.
// %s order: id, then (busName, nonce) x3 for the ok / notfound / error callbacks.
const activateWindowJS = `(function(){
  try {
    var list = workspace.windowList ? workspace.windowList() : (workspace.stackingOrder || []);
    for (var i = 0; i < list.length; i++) {
      var w = list[i];
      if (w && ("" + w.internalId) === "%s") {
        workspace.activeWindow = w;
        callDBus("%s","/WindowList","io.agentkate.Cowork.WindowList","Report","%s","ok");
        return;
      }
    }
    callDBus("%s","/WindowList","io.agentkate.Cowork.WindowList","Report","%s","notfound");
  } catch (e) {
    callDBus("%s","/WindowList","io.agentkate.Cowork.WindowList","Report","%s","error:" + e);
  }
})();`

// ActivateWindow raises and focuses the window with the given KWin internalId, so a
// subsequent keystroke injection lands on it. Reuses the same one-shot-script +
// callback mechanism as ListWindows.
func (c *Client) ActivateWindow(internalID string, timeout time.Duration) error {
	if c == nil || c.conn == nil {
		return fmt.Errorf("kde: client closed")
	}
	nonce := randHex(8)
	ch := c.registerNonce(nonce)
	defer c.unregisterNonce(nonce)

	js := fmt.Sprintf(activateWindowJS, internalID, c.busName, nonce, c.busName, nonce, c.busName, nonce)
	tmp, err := writeTempScript("ak_kwin_activate_", js)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	scripting := c.conn.Object("org.kde.KWin", dbus.ObjectPath("/Scripting"))
	var id int32
	if err := scripting.Call("org.kde.kwin.Scripting.loadScript", 0, tmp, "akcore_activate_"+nonce).Store(&id); err != nil {
		return fmt.Errorf("kde: loadScript: %w", err)
	}
	scriptObj := c.conn.Object("org.kde.KWin", dbus.ObjectPath(fmt.Sprintf("/Scripting/Script%d", id)))
	stop := func() { _ = scriptObj.Call("org.kde.kwin.Script.stop", 0).Err }

	if runErr := scriptObj.Call("org.kde.kwin.Script.run", 0).Err; runErr != nil {
		if e2 := scripting.Call("org.kde.kwin.Scripting.start", 0).Err; e2 != nil {
			stop()
			return fmt.Errorf("kde: run script failed (%v) and start failed (%v)", runErr, e2)
		}
	}

	select {
	case payload := <-ch:
		stop()
		switch payload {
		case "ok":
			return nil
		case "notfound":
			return fmt.Errorf("kde: window %s not found", internalID)
		default:
			return fmt.Errorf("kde: activate window: %s", payload)
		}
	case <-time.After(timeout):
		stop()
		return fmt.Errorf("kde: activate window timed out")
	}
}
