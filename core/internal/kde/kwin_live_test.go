package kde

import (
	"os"
	"testing"
	"time"
)

// TestListWindowsLive proves the SPIKE-CALLBACK round-trip: akcore RequestName +
// Export, KWin loadScript+run, the script's callDBus reaching our Report method,
// and decoding the window model. It touches the developer's LIVE KWin, so it is
// gated behind AK_KDE_LIVE=1 (and a reachable session bus) and never runs in CI.
//
//	AK_KDE_LIVE=1 go test ./internal/kde/ -run TestListWindowsLive -v
func TestListWindowsLive(t *testing.T) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Skip("no session bus (DBUS_SESSION_BUS_ADDRESS unset)")
	}
	if os.Getenv("AK_KDE_LIVE") == "" {
		t.Skip("set AK_KDE_LIVE=1 to run the live KWin integration test")
	}

	c, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	wins, err := c.ListWindows(8 * time.Second)
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	t.Logf("KWin reported %d windows", len(wins))
	for i, w := range wins {
		if i >= 12 {
			t.Logf("  ... (%d more)", len(wins)-12)
			break
		}
		t.Logf("  [%d] class=%-22q pid=%-7d geom=%dx%d+%d+%d desktops=%v caption=%q",
			i, w.ResourceClass, w.PID, w.Width, w.Height, w.X, w.Y, w.Desktops, w.Caption)
	}
}
