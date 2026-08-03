package harness

import (
	"context"
	"testing"
)

// Health keeps the package's do-nothing fake implementing the interface now
// that Health is on it (plan 26): a fake engine honestly answers Unknown.
func (f fakeHarness) Health(context.Context) (Health, error) {
	return Health{EngineID: f.id, State: HealthUnknown}, nil
}

// TestHealthWorstStateWins pins THE roll-up every adapter shares: one bad
// check makes the whole engine bad, warn + ok is warn — and each case is
// asserted against the exact state, so an inverted (best-of) roll-up fails on
// the very first row rather than passing by coincidence.
func TestHealthWorstStateWins(t *testing.T) {
	c := func(state string) Check { return Check{Name: "c", State: state} }
	for _, tc := range []struct {
		name   string
		checks []Check
		want   string
	}{
		{"bad beats everything", []Check{c(HealthOK), c(HealthBad), c(HealthOK)}, HealthBad},
		{"warn plus ok is warn", []Check{c(HealthWarn), c(HealthOK)}, HealthWarn},
		{"all ok is ok", []Check{c(HealthOK), c(HealthOK)}, HealthOK},
		// A warn outranks an unknown: "something IS off" must not be diluted
		// by a probe that merely failed to answer...
		{"warn beats unknown", []Check{c(HealthUnknown), c(HealthWarn)}, HealthWarn},
		// ...but an unknown still outranks ok: a card whose doctor hung must
		// not show a green light on the strength of the checks that DID run.
		{"unknown beats ok", []Check{c(HealthOK), c(HealthUnknown)}, HealthUnknown},
		{"bad beats unknown", []Check{c(HealthUnknown), c(HealthBad)}, HealthBad},
		// No checks is no verdict; an unrecognised state is not trusted.
		{"empty is unknown", nil, HealthUnknown},
		{"unrecognised state reads as unknown", []Check{c("excellent")}, HealthUnknown},
	} {
		if got := WorstState(tc.checks); got != tc.want {
			t.Errorf("%s: WorstState = %q, want %q", tc.name, got, tc.want)
		}
	}
}
