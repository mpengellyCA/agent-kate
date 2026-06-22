package cowork

import "testing"

func TestPointerControlTierAndValid(t *testing.T) {
	if got := TierOf(CapPointerControl); got != TierR2 {
		t.Fatalf("pointer_control must be R2, got %q", got)
	}
	if !CapPointerControl.Valid() {
		t.Fatal("pointer_control must be a valid capability")
	}
	found := false
	for _, c := range AllToggleable() {
		if c == CapPointerControl {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("pointer_control must appear in AllToggleable()")
	}
}

func TestIsSelfPoint(t *testing.T) {
	a := newTestService(t, &fakeNotifier{}).Authority
	a.SetSelfIdentity([]string{"org.kde.agentkate"}, []int{4242})

	akByPID := WindowRect{X: 0, Y: 0, W: 100, H: 100, PID: 4242, ResourceClass: "konsole"}
	akByClass := WindowRect{X: 200, Y: 200, W: 100, H: 100, PID: 9, ResourceClass: "org.kde.agentkate"}
	other := WindowRect{X: 400, Y: 400, W: 100, H: 100, PID: 7, ResourceClass: "firefox"}
	all := []WindowRect{akByPID, akByClass, other}

	if !a.IsSelfPoint(50, 50, all) {
		t.Fatal("point inside an AK-owned window (PID match) must be self")
	}
	if !a.IsSelfPoint(250, 250, all) {
		t.Fatal("point inside an AK-owned window (class match) must be self")
	}
	if a.IsSelfPoint(450, 450, all) {
		t.Fatal("point inside a non-AK window must NOT be self")
	}
	if a.IsSelfPoint(1000, 1000, all) {
		t.Fatal("point outside all windows must NOT be self")
	}
	if a.IsSelfPoint(50, 50, nil) {
		t.Fatal("empty window list must NOT be self")
	}
	// Half-open rect: (X+W, Y) is outside, (X, Y) is inside.
	if a.IsSelfPoint(100, 0, all) {
		t.Fatal("point at (X+W, Y) must be outside (half-open)")
	}
	if !a.IsSelfPoint(0, 0, all) {
		t.Fatal("point at (X, Y) must be inside")
	}
}
