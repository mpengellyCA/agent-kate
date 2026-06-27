package main

import (
	"fmt"
	"strings"
)

// injectEvent is one high-level input event the agent asks for. Keyboard keys are
// named ("space", "k", "playpause", "ctrl", or a single printable character); pointer
// buttons are "left"/"right"/"middle"/"back"/"forward". Positioned pointer motion +
// absolute clicks live in the dedicated pointer tools (cowork_pointer.go, plan 09);
// this low-level path fires buttons wherever the pointer already sits.
//
// Beyond the back-to-back press+release "taps" (Type "key"/"button"), an event can be
// a lone half-event — "key_down"/"key_up"/"button_down"/"button_up" — so the agent can
// hold an input down across other events (e.g. hold Ctrl, click, release Ctrl). Holds
// must be balanced WITHIN a single injectInput call: every *_down needs a matching *_up
// in the same events array (the UI's releaseHeld is a crash safety net, not a license to
// leak holds by design).
type injectEvent struct {
	Type   string `json:"type"`   // "key"|"key_down"|"key_up" | "button"|"click"|"button_down"|"button_up"
	Key    string `json:"key"`    // for key events
	Button string `json:"button"` // for button events
	// HoldMs is the dwell (ms) between the down and up of a TAP ("key"/"button"); it
	// rides the up op as delayMs. Ignored for half-events.
	HoldMs int `json:"holdMs"`
	// AfterMs is the gap (ms) to wait after the previous event fired, before this event
	// fires; it rides this event's first (down) op as delayMs.
	AfterMs int `json:"afterMs"`
}

// Duration caps. We clamp (never reject) so a careless script still runs, just bounded.
const (
	injectMaxHoldMs  = 10000 // per-tap dwell ceiling (10s)
	injectMaxAfterMs = 30000 // per-event lead-in ceiling (30s)
	injectMaxSpanMs  = 30000 // total wall-clock span across the whole batch (30s)
)

// X keysyms for the named special keys we support (video control + navigation), plus
// the modifier aliases needed for chords like "hold Ctrl while clicking". The modifier
// values are the LEFT-hand variants (Control_L/Shift_L/Alt_L/Super_L).
var keysymNames = map[string]uint32{
	"space": 0x0020, "enter": 0xff0d, "return": 0xff0d, "tab": 0xff09,
	"escape": 0xff1b, "esc": 0xff1b, "backspace": 0xff08, "delete": 0xffff,
	"up": 0xff52, "down": 0xff54, "left": 0xff51, "right": 0xff53,
	"home": 0xff50, "end": 0xff57, "pageup": 0xff55, "pagedown": 0xff56,
	"playpause": 0x1008ff14, "play": 0x1008ff14, "pause": 0x1008ff31,
	"stop": 0x1008ff15, "next": 0x1008ff17, "prev": 0x1008ff16, "previous": 0x1008ff16,
	"volumeup": 0x1008ff13, "volumedown": 0x1008ff11, "mute": 0x1008ff12,
	// Modifiers (left-hand variants) so chords can hold them across other events.
	"ctrl": 0xffe3, "control": 0xffe3, // Control_L
	"shift": 0xffe1,                                // Shift_L
	"alt":   0xffe9,                                // Alt_L
	"super": 0xffeb, "meta": 0xffeb, "win": 0xffeb, // Super_L
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
		return 0x111, nil // BTN_RIGHT
	case "middle":
		return 0x112, nil // BTN_MIDDLE
	case "back":
		return 0x113, nil // BTN_SIDE — SPIKE-2: some stacks drive history-back via BTN_BACK (0x116)
	case "forward":
		return 0x114, nil // BTN_EXTRA — SPIKE-2: some stacks drive history-forward via BTN_FORWARD (0x115)
	}
	return 0, fmt.Errorf("unknown button %q", name)
}

// buttonName is the inverse of buttonCodeFor for audit/consent descriptions.
func buttonName(code uint32) string {
	switch code {
	case 0x110:
		return "left"
	case 0x111:
		return "right"
	case 0x112:
		return "middle"
	case 0x113:
		return "back"
	case 0x114:
		return "forward"
	}
	return fmt.Sprintf("button-0x%x", code)
}

// injectHasButton reports whether any event is a pointer button (so the bare-click
// self-target guard applies — keyboard-only batches are exempt). Half-events
// (button_down/button_up) are pointer buttons too, so they count.
func injectHasButton(events []injectEvent) bool {
	for _, e := range events {
		switch strings.ToLower(e.Type) {
		case "button", "click", "button_down", "button_up":
			return true
		}
	}
	return false
}

// clampNonNeg clamps v into [0, max] (negatives become 0).
func clampNonNeg(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// buildInjectOps expands the high-level events into an ordered op list for the UI and a
// human-readable description for the consent prompt + audit log.
//
// Lowering rules:
//   - A tap ("key"/"button") emits down THEN up. The down op carries delayMs=AfterMs and
//     the up op carries delayMs=HoldMs — but only when that value is >0, so a plain tap
//     (HoldMs=0, AfterMs=0) emits exactly two ops with NO delayMs keys, preserving the
//     UI's synchronous fast path.
//   - A half-event ("key_down"/"key_up"/"button_down"/"button_up") emits a single lone op
//     carrying delayMs=AfterMs (only when >0).
//
// Holds must balance within the call: each *_down adds to a held-set, each *_up removes
// it. A key_up for something not held is an error, and a non-empty held-set at the end is
// an error.
//
// Durations are clamped (never rejected): HoldMs ≤ injectMaxHoldMs, AfterMs ≤
// injectMaxAfterMs, and the cumulative emitted span is held to ≤ injectMaxSpanMs.
func buildInjectOps(events []injectEvent) ([]map[string]any, string, error) {
	var ops []map[string]any
	var desc []string

	// Held-set: keysyms and button codes currently down (from half-events) awaiting a
	// matching release. Keyed by a kind+code string so a keysym and a button code can't
	// collide.
	held := map[string]bool{}
	keyKey := func(ks uint32) string { return fmt.Sprintf("k%d", ks) }
	btnKey := func(bc uint32) string { return fmt.Sprintf("b%d", bc) }

	// totalSpan tracks the cumulative delayMs actually emitted; emit() clamps each delay so
	// the running total never exceeds injectMaxSpanMs, then attaches it (only if >0).
	totalSpan := 0
	emit := func(op map[string]any, delayMs int) {
		if delayMs > 0 {
			if rem := injectMaxSpanMs - totalSpan; delayMs > rem {
				delayMs = rem
			}
			if delayMs > 0 {
				op["delayMs"] = delayMs
				totalSpan += delayMs
			}
		}
		ops = append(ops, op)
	}

	for _, e := range events {
		after := clampNonNeg(e.AfterMs, injectMaxAfterMs)
		hold := clampNonNeg(e.HoldMs, injectMaxHoldMs)

		switch strings.ToLower(e.Type) {
		case "key", "":
			ks, err := keysymFor(e.Key)
			if err != nil {
				return nil, "", err
			}
			emit(map[string]any{"t": "key", "keysym": ks, "state": uint32(1)}, after)
			emit(map[string]any{"t": "key", "keysym": ks, "state": uint32(0)}, hold)
			name := strings.ToLower(strings.TrimSpace(e.Key))
			if hold > 0 {
				desc = append(desc, fmt.Sprintf("press %s (hold %dms)", name, hold))
			} else {
				desc = append(desc, "press "+name)
			}

		case "key_down":
			ks, err := keysymFor(e.Key)
			if err != nil {
				return nil, "", err
			}
			emit(map[string]any{"t": "key", "keysym": ks, "state": uint32(1)}, after)
			held[keyKey(ks)] = true
			desc = append(desc, "hold "+strings.ToLower(strings.TrimSpace(e.Key)))

		case "key_up":
			ks, err := keysymFor(e.Key)
			if err != nil {
				return nil, "", err
			}
			k := keyKey(ks)
			if !held[k] {
				return nil, "", fmt.Errorf("key_up for %q which is not held (unbalanced release)", e.Key)
			}
			delete(held, k)
			emit(map[string]any{"t": "key", "keysym": ks, "state": uint32(0)}, after)
			desc = append(desc, "release "+strings.ToLower(strings.TrimSpace(e.Key)))

		case "button", "click":
			bc, err := buttonCodeFor(e.Button)
			if err != nil {
				return nil, "", err
			}
			emit(map[string]any{"t": "btn", "button": bc, "state": uint32(1)}, after)
			emit(map[string]any{"t": "btn", "button": bc, "state": uint32(0)}, hold)
			b := buttonName(bc)
			if hold > 0 {
				desc = append(desc, fmt.Sprintf("%s-click (hold %dms)", b, hold))
			} else {
				desc = append(desc, b+"-click")
			}

		case "button_down":
			bc, err := buttonCodeFor(e.Button)
			if err != nil {
				return nil, "", err
			}
			emit(map[string]any{"t": "btn", "button": bc, "state": uint32(1)}, after)
			held[btnKey(bc)] = true
			desc = append(desc, buttonName(bc)+"-press")

		case "button_up":
			bc, err := buttonCodeFor(e.Button)
			if err != nil {
				return nil, "", err
			}
			k := btnKey(bc)
			if !held[k] {
				return nil, "", fmt.Errorf("button_up for %q which is not held (unbalanced release)", buttonName(bc))
			}
			delete(held, k)
			emit(map[string]any{"t": "btn", "button": bc, "state": uint32(0)}, after)
			desc = append(desc, buttonName(bc)+"-release")

		default:
			return nil, "", fmt.Errorf("unknown event type %q", e.Type)
		}
	}

	if len(ops) == 0 {
		return nil, "", fmt.Errorf("no input events")
	}
	if n := len(held); n > 0 {
		return nil, "", fmt.Errorf("input ends with %d key/button still held (every key_down needs a matching key_up in the same call)", n)
	}
	return ops, strings.Join(desc, ", "), nil
}
