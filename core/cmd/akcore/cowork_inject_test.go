package main

import "testing"

// opEq reports whether op has exactly the given t/code/state and (optionally) a delayMs.
// code is matched against "keysym" for key ops and "button" for btn ops. wantDelay<0
// means "delayMs must be absent"; wantDelay>=0 means it must equal wantDelay.
func assertOp(t *testing.T, idx int, op map[string]any, wantT string, wantCode uint32, wantState uint32, wantDelay int) {
	t.Helper()
	if op["t"] != wantT {
		t.Fatalf("op[%d] t = %v, want %q", idx, op["t"], wantT)
	}
	codeKey := "keysym"
	if wantT == "btn" {
		codeKey = "button"
	}
	if got, ok := op[codeKey].(uint32); !ok || got != wantCode {
		t.Fatalf("op[%d] %s = %v, want 0x%x", idx, codeKey, op[codeKey], wantCode)
	}
	if got, ok := op["state"].(uint32); !ok || got != wantState {
		t.Fatalf("op[%d] state = %v, want %d", idx, op["state"], wantState)
	}
	gotDelay, has := op["delayMs"]
	if wantDelay < 0 {
		if has {
			t.Fatalf("op[%d] should carry no delayMs, got %v", idx, gotDelay)
		}
		return
	}
	if !has || gotDelay.(int) != wantDelay {
		t.Fatalf("op[%d] delayMs = %v, want %d", idx, gotDelay, wantDelay)
	}
}

func TestBuildInjectOpsSimpleTapFastPath(t *testing.T) {
	ops, desc, err := buildInjectOps([]injectEvent{{Type: "key", Key: "space"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("simple tap: want 2 ops, got %d", len(ops))
	}
	// Exactly [{key,space,1},{key,space,0}] with NO delayMs (fast path preserved).
	assertOp(t, 0, ops[0], "key", 0x0020, 1, -1)
	assertOp(t, 1, ops[1], "key", 0x0020, 0, -1)
	if desc != "press space" {
		t.Fatalf("desc = %q, want %q", desc, "press space")
	}
}

func TestBuildInjectOpsHalfEvents(t *testing.T) {
	// key_down emits exactly one lone down op; balanced by a key_up below.
	down, _, err := buildInjectOps([]injectEvent{{Type: "key_down", Key: "a"}, {Type: "key_up", Key: "a"}})
	if err != nil {
		t.Fatalf("key_down/up: unexpected error: %v", err)
	}
	if len(down) != 2 {
		t.Fatalf("key_down+key_up: want 2 ops, got %d", len(down))
	}
	assertOp(t, 0, down[0], "key", uint32('a'), 1, -1)
	assertOp(t, 1, down[1], "key", uint32('a'), 0, -1)
}

func TestBuildInjectOpsTapHoldAndAfterDelays(t *testing.T) {
	// holdMs rides the UP op; afterMs rides the DOWN op.
	ops, _, err := buildInjectOps([]injectEvent{{Type: "key", Key: "space", HoldMs: 120, AfterMs: 50}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("want 2 ops, got %d", len(ops))
	}
	assertOp(t, 0, ops[0], "key", 0x0020, 1, 50)  // afterMs on DOWN
	assertOp(t, 1, ops[1], "key", 0x0020, 0, 120) // holdMs on UP
}

func TestBuildInjectOpsModifierChord(t *testing.T) {
	ops, desc, err := buildInjectOps([]injectEvent{
		{Type: "key_down", Key: "ctrl"},
		{Type: "button", Button: "left"},
		{Type: "key_up", Key: "ctrl"},
	})
	if err != nil {
		t.Fatalf("balanced chord should not error: %v", err)
	}
	// ctrl-down, left-down, left-up, ctrl-up
	if len(ops) != 4 {
		t.Fatalf("chord: want 4 ops, got %d", len(ops))
	}
	assertOp(t, 0, ops[0], "key", 0xffe3, 1, -1) // Control_L down
	assertOp(t, 1, ops[1], "btn", 0x110, 1, -1)  // BTN_LEFT down
	assertOp(t, 2, ops[2], "btn", 0x110, 0, -1)  // BTN_LEFT up
	assertOp(t, 3, ops[3], "key", 0xffe3, 0, -1) // Control_L up
	if desc != "hold ctrl, left-click, release ctrl" {
		t.Fatalf("desc = %q", desc)
	}
}

func TestBuildInjectOpsUnbalancedHold(t *testing.T) {
	// A key_down with no matching key_up must error (held-set non-empty at end).
	_, _, err := buildInjectOps([]injectEvent{{Type: "key_down", Key: "w"}})
	if err == nil {
		t.Fatalf("unbalanced key_down should error")
	}
}

func TestBuildInjectOpsUnbalancedRelease(t *testing.T) {
	// A key_up for something never held must error.
	_, _, err := buildInjectOps([]injectEvent{{Type: "key_up", Key: "w"}})
	if err == nil {
		t.Fatalf("unbalanced key_up should error")
	}
}

func TestBuildInjectOpsButtonHalfEventsBalanced(t *testing.T) {
	// button_down / button_up balance as a pair and lower to lone btn ops.
	ops, desc, err := buildInjectOps([]injectEvent{
		{Type: "button_down", Button: "left"},
		{Type: "button_up", Button: "left"},
	})
	if err != nil {
		t.Fatalf("balanced button hold should not error: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("want 2 ops, got %d", len(ops))
	}
	assertOp(t, 0, ops[0], "btn", 0x110, 1, -1)
	assertOp(t, 1, ops[1], "btn", 0x110, 0, -1)
	if desc != "left-press, left-release" {
		t.Fatalf("desc = %q", desc)
	}
	// An unmatched button_down must error.
	if _, _, err := buildInjectOps([]injectEvent{{Type: "button_down", Button: "left"}}); err == nil {
		t.Fatalf("unbalanced button_down should error")
	}
}

func TestBuildInjectOpsHoldClamp(t *testing.T) {
	// holdMs over the cap is clamped to injectMaxHoldMs and rides the UP op.
	ops, _, err := buildInjectOps([]injectEvent{{Type: "key", Key: "space", HoldMs: 999999}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := ops[1]["delayMs"]; got != injectMaxHoldMs {
		t.Fatalf("hold clamp: delayMs = %v, want %d", got, injectMaxHoldMs)
	}
}

func TestBuildInjectOpsSpanClamp(t *testing.T) {
	// Cumulative emitted span must never exceed injectMaxSpanMs. Two big afterMs gaps
	// (each at the per-event cap) sum past 30s; the second is clamped to the remainder.
	ops, _, err := buildInjectOps([]injectEvent{
		{Type: "key_down", Key: "a", AfterMs: injectMaxAfterMs},
		{Type: "key_up", Key: "a", AfterMs: injectMaxAfterMs},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	total := 0
	for _, op := range ops {
		if d, ok := op["delayMs"].(int); ok {
			total += d
		}
	}
	if total > injectMaxSpanMs {
		t.Fatalf("total span %d exceeds cap %d", total, injectMaxSpanMs)
	}
	if total != injectMaxSpanMs {
		t.Fatalf("expected span to saturate to %d, got %d", injectMaxSpanMs, total)
	}
}

func TestInjectHasButton(t *testing.T) {
	cases := []struct {
		events []injectEvent
		want   bool
	}{
		{[]injectEvent{{Type: "button_down", Button: "left"}}, true},
		{[]injectEvent{{Type: "button_up", Button: "left"}}, true},
		{[]injectEvent{{Type: "button", Button: "left"}}, true},
		{[]injectEvent{{Type: "click"}}, true},
		{[]injectEvent{{Type: "key", Key: "space"}}, false},
		{[]injectEvent{{Type: "key_down", Key: "ctrl"}}, false},
	}
	for i, c := range cases {
		if got := injectHasButton(c.events); got != c.want {
			t.Fatalf("case %d: injectHasButton = %v, want %v", i, got, c.want)
		}
	}
}
