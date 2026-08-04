package harness

import "testing"

func TestDeriveModelRoleOnlyClassifiesExplicitFamilies(t *testing.T) {
	for _, tc := range []struct {
		id, name string
		want     ModelRole
	}{
		{"claude-sonnet-5", "Claude Sonnet 5", ModelRoleBalanced},
		{"claude-opus-5", "Claude Opus 5", ModelRoleDeep},
		{"fable", "Fable", ModelRoleFast},
		{"luna-2", "Luna", ModelRoleBalanced},
		{"terra-2", "Terra", ModelRoleDeep},
		{"sol-2", "Sol", ModelRoleFast},
		{"gpt-5.4", "GPT-5.4", ""},
		{"sonnet-opus-experiment", "Experimental", ""},
		{"solstice", "Solstice", ""},
	} {
		if got := DeriveModelRole(tc.id, tc.name); got != tc.want {
			t.Errorf("DeriveModelRole(%q, %q) = %q, want %q", tc.id, tc.name, got, tc.want)
		}
	}
}
