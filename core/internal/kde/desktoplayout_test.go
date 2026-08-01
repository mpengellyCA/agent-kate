package kde

import "testing"

// The pointer mirror asks one question of this type: "could the cursor be AT this point?"
// Answering it with the union of every screen says yes for the dead space a staggered or
// L-shaped layout leaves inside that union — space the compositor never puts the cursor
// in. A yes there is a mirror that points where the cursor is not, which is the whole
// failure class DesktopLayout exists to close.
func TestDesktopLayoutContainsIsPerScreenNotUnion(t *testing.T) {
	// Two screens, staggered vertically: a 1920x1080 primary at the origin and a smaller
	// 1280x1024 secondary hanging off its right edge, 800px down. Their union is
	// (0,0)-(3200,1824), which includes (2500,200) — above the secondary, right of the
	// primary — where no screen is.
	layout := DesktopLayout{
		Union:   DesktopRect{X: 0, Y: 0, Width: 3200, Height: 1824},
		Screens: []DesktopRect{{X: 0, Y: 0, Width: 1920, Height: 1080}, {X: 1920, Y: 800, Width: 1280, Height: 1024}},
	}
	if !layout.Union.Contains(2500, 200) {
		t.Fatal("precondition: the union must contain the dead point (else this proves nothing)")
	}
	if layout.Contains(2500, 200) {
		t.Fatal("a point inside the union but on NO screen must not be reported reachable")
	}
	for _, p := range []struct{ x, y int }{{0, 0}, {1919, 1079}, {1920, 800}, {3199, 1823}} {
		if !layout.Contains(p.x, p.y) {
			t.Fatalf("(%d,%d) is on a screen and must be reported reachable", p.x, p.y)
		}
	}
	// Outside everything, both tests agree.
	if layout.Contains(3200, 0) || layout.Contains(-1, 0) {
		t.Fatal("points off every screen must never be reachable")
	}
}

func TestDesktopLayoutFallsBackToTheUnionWithoutAScreenList(t *testing.T) {
	// KWin would not enumerate screens: the union is all we have, and it must still work
	// (this is the pre-existing behaviour, not a regression to fail closed on).
	layout := DesktopLayout{Union: DesktopRect{X: 0, Y: 0, Width: 1920, Height: 1080}}
	if !layout.Contains(10, 10) {
		t.Fatal("with no screen list the union must still answer containment")
	}
	if layout.Contains(1920, 10) {
		t.Fatal("outside the union is outside")
	}
}

func TestDesktopLayoutWithNoExtentContainsNothing(t *testing.T) {
	// "Unknown" must never read as "yes": every caller of this fails closed on it.
	var unknown DesktopLayout
	if unknown.Valid() || unknown.Contains(0, 0) {
		t.Fatal("an unknown layout must be invalid and contain nothing")
	}
	// A screen list without a valid union is still unknown — the union is the sanity check.
	partial := DesktopLayout{Screens: []DesktopRect{{X: 0, Y: 0, Width: 800, Height: 600}}}
	if partial.Contains(10, 10) {
		t.Fatal("without a usable union the layout must contain nothing")
	}
}
