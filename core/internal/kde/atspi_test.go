package kde

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestElementIDRoundTrip(t *testing.T) {
	cases := []atspiRef{
		{Name: ":1.42", Path: "/org/a11y/atspi/accessible/2147483648"},
		{Name: "org.a11y.atspi.Registry", Path: atspiRootPath},
		{Name: ":1.0", Path: "/org/a11y/atspi/accessible/root"},
	}
	for _, want := range cases {
		id := encodeElementID(want)
		got, err := decodeElementID(id)
		if err != nil {
			t.Fatalf("decode(%q) error: %v", id, err)
		}
		if got.Name != want.Name || got.Path != want.Path {
			t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
		}
	}
}

func TestDecodeElementIDRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not-base64!!", "Zm9v" /* "foo", no separator */} {
		if _, err := decodeElementID(bad); err == nil {
			t.Errorf("decodeElementID(%q) = nil error, want error", bad)
		}
	}
}

func TestStateHas(t *testing.T) {
	// Bit 25 (SHOWING) and bit 30 (VISIBLE) live in word 0; bit 32 lives in word 1.
	st := []uint32{(1 << stShowing) | (1 << stVisible), 1 << (32 - 32)}
	if !stateHas(st, stShowing) || !stateHas(st, stVisible) {
		t.Error("expected SHOWING and VISIBLE set")
	}
	if stateHas(st, stSensitive) {
		t.Error("did not expect SENSITIVE set")
	}
	if !stateHas(st, 32) {
		t.Error("expected bit 32 (word 1, offset 0) set")
	}
	if stateHas(nil, stShowing) {
		t.Error("nil state must report nothing set")
	}
	if stateHas(st, 999) {
		t.Error("out-of-range bit must be false, not panic")
	}
}

func TestRectsIntersect(t *testing.T) {
	// window at 100,100 200x200
	wx, wy, ww, wh := 100, 100, 200, 200
	if !rectsIntersect(150, 150, 10, 10, wx, wy, ww, wh) {
		t.Error("inside rect should intersect")
	}
	if rectsIntersect(0, 0, 10, 10, wx, wy, ww, wh) {
		t.Error("disjoint rect should not intersect")
	}
	if rectsIntersect(300, 100, 10, 10, wx, wy, ww, wh) {
		t.Error("edge-adjacent (touching x=300) should not intersect")
	}
}

func TestRoleSetsDisjointOnKeyRoles(t *testing.T) {
	// A node's role drives whether we treat it as a click target or a text field;
	// the two sets must not overlap or a field would be mis-activated.
	for r := range atspiActionableRoles {
		if atspiEditableRoles[r] {
			t.Errorf("role %q is in both actionable and editable sets", r)
		}
	}
}

// compile-time guard that atspiRef matches the AT-SPI (so) wire shape.
var _ = atspiRef{Name: "", Path: dbus.ObjectPath("")}
