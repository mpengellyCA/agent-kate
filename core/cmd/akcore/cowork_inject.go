package main

import (
	"fmt"
	"strings"
)

// injectEvent is one high-level input event the agent asks for. Keyboard keys are
// named ("space", "k", "playpause", or a single printable character); pointer
// buttons are "left"/"right"/"middle". Pointer motion / absolute clicks are deferred
// (they need a paired ScreenCast stream — plan 04 §2 / review D5).
type injectEvent struct {
	Type   string `json:"type"`   // "key" | "button"
	Key    string `json:"key"`    // for type=key
	Button string `json:"button"` // for type=button
}

// X keysyms for the named special keys we support (video control + navigation).
var keysymNames = map[string]uint32{
	"space": 0x0020, "enter": 0xff0d, "return": 0xff0d, "tab": 0xff09,
	"escape": 0xff1b, "esc": 0xff1b, "backspace": 0xff08, "delete": 0xffff,
	"up": 0xff52, "down": 0xff54, "left": 0xff51, "right": 0xff53,
	"home": 0xff50, "end": 0xff57, "pageup": 0xff55, "pagedown": 0xff56,
	"playpause": 0x1008ff14, "play": 0x1008ff14, "pause": 0x1008ff31,
	"stop": 0x1008ff15, "next": 0x1008ff17, "prev": 0x1008ff16, "previous": 0x1008ff16,
	"volumeup": 0x1008ff13, "volumedown": 0x1008ff11, "mute": 0x1008ff12,
}

func keysymFor(name string) (uint32, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	if k, ok := keysymNames[n]; ok {
		return k, nil
	}
	r := []rune(n)
	if len(r) == 1 && r[0] >= 0x20 && r[0] < 0x7f {
		return uint32(r[0]), nil // printable ASCII keysym == its codepoint
	}
	return 0, fmt.Errorf("unknown key %q", name)
}

func buttonCodeFor(name string) (uint32, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "left", "":
		return 0x110, nil // BTN_LEFT
	case "right":
		return 0x111, nil
	case "middle":
		return 0x112, nil
	}
	return 0, fmt.Errorf("unknown button %q", name)
}

// buildInjectOps expands the high-level events into an ordered op list for the UI
// (each "tap" becomes a press + release) and a human-readable description for the
// consent prompt + audit log.
func buildInjectOps(events []injectEvent) ([]map[string]any, string, error) {
	var ops []map[string]any
	var desc []string
	for _, e := range events {
		switch strings.ToLower(e.Type) {
		case "key", "":
			ks, err := keysymFor(e.Key)
			if err != nil {
				return nil, "", err
			}
			ops = append(ops,
				map[string]any{"t": "key", "keysym": ks, "state": uint32(1)},
				map[string]any{"t": "key", "keysym": ks, "state": uint32(0)},
			)
			desc = append(desc, "press "+strings.ToLower(strings.TrimSpace(e.Key)))
		case "button", "click":
			bc, err := buttonCodeFor(e.Button)
			if err != nil {
				return nil, "", err
			}
			ops = append(ops,
				map[string]any{"t": "btn", "button": bc, "state": uint32(1)},
				map[string]any{"t": "btn", "button": bc, "state": uint32(0)},
			)
			b := strings.ToLower(strings.TrimSpace(e.Button))
			if b == "" {
				b = "left"
			}
			desc = append(desc, b+"-click")
		default:
			return nil, "", fmt.Errorf("unknown event type %q", e.Type)
		}
	}
	if len(ops) == 0 {
		return nil, "", fmt.Errorf("no input events")
	}
	return ops, strings.Join(desc, ", "), nil
}
