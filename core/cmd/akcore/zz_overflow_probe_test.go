package main

import (
	"math"
	"testing"
)

// Probe: does a huge repeat*step or a Frame near math.MaxInt overflow the span check?
func TestProbeRepeatOverflow(t *testing.T) {
	// huge repeat with a step — does it blow up memory or bypass the span cap?
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", Repeat: 100000000, RepeatEveryMs: 1},
	}}
	_, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	t.Logf("huge repeat err=%v", err)
	if err == nil {
		t.Errorf("huge repeat produced no error (allocated 100M ops?)")
	}
}

func TestProbeFrameOverflow(t *testing.T) {
	big := int(math.MaxInt32)
	script := timelineScript{FPS: 0.001, Events: []timelineEvent{
		{Type: "key", Key: "a", Frame: &big},
	}}
	_, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	t.Logf("frame overflow err=%v", err)
}

func TestProbeNegativeAtMs(t *testing.T) {
	// out-of-order atMs: later event with smaller atMs
	a, b := 5000, 1000
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", AtMs: &a},
		{Type: "key", Key: "b", AtMs: &b},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	for i, op := range plan.Ops {
		if d, ok := op["delayMs"].(int); ok && d < 0 {
			t.Errorf("op[%d] has negative delayMs=%d", i, d)
		}
		t.Logf("op[%d]=%v", i, op)
	}
}

func TestProbeNegativeAtMsSingle(t *testing.T) {
	neg := -5000
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", AtMs: &neg},
		{Type: "key", Key: "b", AtMs: &neg},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	t.Logf("negative atMs err=%v ops=%v", err, plan.Ops)
}

// Probe: an enormous repeatEveryMs must not overflow fireAt+c*step into a wrapped-negative
// offset that defeats the span cap — it should be rejected by the extent guard instead.
func TestProbeRepeatStepOverflow(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", Repeat: 5, RepeatEveryMs: math.MaxInt64 / 3},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	t.Logf("huge step err=%v", err)
	if err == nil {
		t.Errorf("huge repeatEveryMs produced no error (overflow slipped past the span cap?)")
	}
	for i, op := range plan.Ops {
		if d, ok := op["delayMs"].(int); ok && d < 0 {
			t.Errorf("op[%d] has negative delayMs=%d", i, d)
		}
	}
}

// Probe: a negative cadence is clamped to 0 (copies stack at one instant), never marching
// copies backward in time. No error, no negative delays.
func TestProbeRepeatNegativeStep(t *testing.T) {
	script := timelineScript{Events: []timelineEvent{
		{Type: "key", Key: "a", Repeat: 3, RepeatEveryMs: -1000},
	}}
	plan, err := buildTimelineOps(script, point{}, false, trivialProfile(), fixedRNG())
	if err != nil {
		t.Fatalf("negative step should clamp, not error: %v", err)
	}
	if len(plan.Ops) != 6 {
		t.Fatalf("3 taps ⇒ 6 ops, got %d", len(plan.Ops))
	}
	for i, op := range plan.Ops {
		if d, ok := op["delayMs"].(int); ok && d < 0 {
			t.Errorf("op[%d] has negative delayMs=%d", i, d)
		}
	}
}
