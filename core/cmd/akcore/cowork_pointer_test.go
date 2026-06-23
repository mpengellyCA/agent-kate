package main

import (
	"math"
	"math/rand"
	"testing"

	"agentkate/internal/kde"
)

func fixedRNG() *rand.Rand { return rand.New(rand.NewSource(1)) }

func TestElementCenterGlobal(t *testing.T) {
	win := kde.Window{X: 1440, Y: 769, Width: 2560, Height: 1408}

	// Wayland: AT-SPI bounds are surface-relative (outside the window's global rect) —
	// must be translated by the window origin into global pixels.
	cx, cy := elementCenterGlobal(kde.Rect{X: 1003, Y: 363, W: 419, H: 40}, win, true)
	if cx != 1440+1212 || cy != 769+383 {
		t.Fatalf("surface-relative translate: got (%d,%d) want (%d,%d)", cx, cy, 1440+1212, 769+383)
	}

	// X11: AT-SPI bounds already global (inside the window's rect) — left unchanged.
	cx, cy = elementCenterGlobal(kde.Rect{X: 2443, Y: 1132, W: 419, H: 40}, win, true)
	if cx != 2443+209 || cy != 1132+20 {
		t.Fatalf("already-global unchanged: got (%d,%d) want (%d,%d)", cx, cy, 2443+209, 1132+20)
	}

	// No owning window resolved — leave the center as-is (best effort).
	cx, cy = elementCenterGlobal(kde.Rect{X: 100, Y: 200, W: 10, H: 20}, kde.Window{}, false)
	if cx != 105 || cy != 210 {
		t.Fatalf("no-window passthrough: got (%d,%d) want (105,210)", cx, cy)
	}
}

func lastOp(ops []map[string]any) map[string]any { return ops[len(ops)-1] }

func TestExpandMoveInstant(t *testing.T) {
	// No known start ⇒ single teleport, no delay.
	ops := expandMove(point{}, false, point{100, 200}, PointerProfile{Speed: 1600, Accuracy: 1}, fixedRNG())
	if len(ops) != 1 {
		t.Fatalf("no-start move: want 1 op, got %d", len(ops))
	}
	if ops[0]["x"] != 100 || ops[0]["y"] != 200 {
		t.Fatalf("teleport landed at (%v,%v), want (100,200)", ops[0]["x"], ops[0]["y"])
	}
	if _, ok := ops[0]["delayMs"]; ok {
		t.Fatalf("instant op should carry no delayMs")
	}
	// Speed 0 ⇒ instant even with a known start.
	ops = expandMove(point{0, 0}, true, point{500, 500}, PointerProfile{Speed: 0, Accuracy: 1}, fixedRNG())
	if len(ops) != 1 {
		t.Fatalf("speed-0 move: want 1 op, got %d", len(ops))
	}
}

func TestExpandMoveLandsExact(t *testing.T) {
	// Both straight (accuracy 1) and human (accuracy 0.3) must land EXACTLY on target.
	for _, acc := range []float64{1.0, 0.5, 0.0} {
		to := point{1320, 540}
		ops := expandMove(point{10, 10}, true, to, PointerProfile{Speed: 1600, Accuracy: acc}, fixedRNG())
		last := lastOp(ops)
		if last["x"] != to.X || last["y"] != to.Y {
			t.Fatalf("accuracy %.1f: final op (%v,%v) must land exactly on (%d,%d)", acc, last["x"], last["y"], to.X, to.Y)
		}
		if len(ops) < 2 {
			t.Fatalf("accuracy %.1f: expected a multi-step path, got %d ops", acc, len(ops))
		}
	}
}

func TestExpandMoveSpeedSetsStepCount(t *testing.T) {
	from, to := point{0, 0}, point{2000, 0}
	fast := expandMove(from, true, to, PointerProfile{Speed: 4000, Accuracy: 1}, fixedRNG())
	slow := expandMove(from, true, to, PointerProfile{Speed: 800, Accuracy: 1}, fixedRNG())
	if !(len(slow) > len(fast)) {
		t.Fatalf("slower speed should produce more steps: slow=%d fast=%d", len(slow), len(fast))
	}
}

func TestExpandMoveJitterBounded(t *testing.T) {
	// Human motion may wander off the straight line, but every step stays within a
	// bounded envelope of jitter+overshoot — and the click still lands exactly (above).
	from, to := point{0, 0}, point{1000, 0}
	ops := expandMove(from, true, to, PointerProfile{Speed: 1600, Accuracy: 0}, fixedRNG())
	limit := pointerMaxJitter + pointerOvershoot + 1.5
	for i, op := range ops {
		x := float64(op["x"].(int))
		y := float64(op["y"].(int))
		// distance off the y=0 line, and overshoot past the endpoints along x
		if math.Abs(y) > limit {
			t.Fatalf("op %d strayed %.1fpx off-axis (limit %.1f)", i, math.Abs(y), limit)
		}
		if x < -limit || x > float64(to.X)+limit {
			t.Fatalf("op %d x=%.0f outside [%.1f, %.1f]", i, x, -limit, float64(to.X)+limit)
		}
	}
}

func TestClampProfileBounds(t *testing.T) {
	bounds := PointerProfile{Speed: 1600, Accuracy: 1, SettleMs: 30}
	// Agent asks to go faster than the user allows ⇒ capped.
	got := clampProfile(PointerProfile{Speed: 9000, Accuracy: 1}, bounds)
	if got.Speed != 1600 {
		t.Fatalf("speed cap: got %.0f want 1600", got.Speed)
	}
	// Instant (0) collapses to the user's cap when one is set.
	got = clampProfile(PointerProfile{Speed: 0, Accuracy: 1}, bounds)
	if got.Speed != 1600 {
		t.Fatalf("instant under a cap: got %.0f want 1600", got.Speed)
	}
	// No cap ⇒ instant stays instant.
	got = clampProfile(PointerProfile{Speed: 0, Accuracy: 1}, PointerProfile{})
	if got.Speed != 0 {
		t.Fatalf("instant with no cap: got %.0f want 0", got.Speed)
	}
	// Accuracy/settle clamped to sane ranges.
	got = clampProfile(PointerProfile{Speed: 100, Accuracy: 5, SettleMs: -10}, PointerProfile{})
	if got.Accuracy != 1 || got.SettleMs != 0 {
		t.Fatalf("range clamp: got accuracy=%.1f settle=%d", got.Accuracy, got.SettleMs)
	}
}

func TestPointerStateResolvePatch(t *testing.T) {
	s := newPointerState()
	s.setBounds(&pointerProfilePatch{Speed: f64(2000), Accuracy: f64(1), SettleMs: iptr(20)})
	// A partial per-call patch changes only speed, keeping the standing accuracy/settle.
	got := s.resolve("t1", &pointerProfilePatch{Speed: f64(900)})
	if got.Speed != 900 || got.Accuracy != 1 || got.SettleMs != 20 {
		t.Fatalf("partial patch: got %+v", got)
	}
	// A session default for the thread overrides the bounds for later calls.
	s.setThreadProfile("t1", &pointerProfilePatch{Accuracy: f64(0.5)})
	got = s.resolve("t1", nil)
	if got.Accuracy != 0.5 {
		t.Fatalf("thread default: got accuracy %.2f want 0.5", got.Accuracy)
	}
}

func TestPointerLastPositionMirror(t *testing.T) {
	s := newPointerState()
	if _, ok := s.last("t1"); ok {
		t.Fatalf("no last position should be known initially")
	}
	prof := PointerProfile{Speed: 1600, Accuracy: 1}
	// moveOps must NOT mutate the mirror — the handler commits only after the portal op
	// succeeds (review H1), so a denied/failed move can't desync the bare-click guard.
	first := s.moveOps("t1", 300, 300, prof, fixedRNG())
	if len(first) != 1 {
		t.Fatalf("first move (unknown start) should teleport, got %d ops", len(first))
	}
	if _, ok := s.last("t1"); ok {
		t.Fatalf("moveOps must not record the position; the caller commits on success")
	}
	// Once the caller commits the position, a later move has a start ⇒ a real path.
	s.setLast("t1", point{300, 300})
	second := s.moveOps("t1", 900, 300, prof, fixedRNG())
	if len(second) < 2 {
		t.Fatalf("second move should be a path, got %d ops", len(second))
	}
}

func TestButtonCodeTable(t *testing.T) {
	cases := map[string]uint32{
		"": 0x110, "left": 0x110, "right": 0x111, "middle": 0x112, "back": 0x113, "forward": 0x114,
	}
	for name, want := range cases {
		got, err := buttonCodeFor(name)
		if err != nil || got != want {
			t.Fatalf("buttonCodeFor(%q) = 0x%x, %v; want 0x%x", name, got, err, want)
		}
		if rt := buttonName(want); name != "" && rt != name {
			t.Fatalf("buttonName(0x%x) = %q; want %q", want, rt, name)
		}
	}
	if _, err := buttonCodeFor("scroll"); err == nil {
		t.Fatalf("unknown button should error")
	}
}

func TestClickOpsComposition(t *testing.T) {
	move := []map[string]any{{"t": "move", "x": 10, "y": 10}}
	ops := clickOps(move, 0x110, 2, 40)
	// move + (press,release) x2 = 5 ops
	if len(ops) != 5 {
		t.Fatalf("double-click: want 5 ops, got %d", len(ops))
	}
	press := ops[1]
	if press["t"] != "btn" || press["state"].(uint32) != 1 {
		t.Fatalf("op[1] should be a press, got %+v", press)
	}
	if press["delayMs"] != 40 {
		t.Fatalf("settle delay should ride the first press, got %v", press["delayMs"])
	}
	// The second press must NOT carry the settle delay again.
	if _, ok := ops[3]["delayMs"]; ok {
		t.Fatalf("only the first press carries settle delay")
	}
}

func TestScrollOpAxes(t *testing.T) {
	v := scrollOp(0, -5)
	if v["axis"] != 0 || v["steps"] != -5 || v["t"] != "axis_discrete" {
		t.Fatalf("vertical scroll op malformed: %+v", v)
	}
	h := scrollOp(1, 3)
	if h["axis"] != 1 || h["steps"] != 3 {
		t.Fatalf("horizontal scroll op malformed: %+v", h)
	}
}

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }
