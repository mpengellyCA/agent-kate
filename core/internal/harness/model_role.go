package harness

import "strings"

// ModelRole is a semantic placement in the model picker, not a model alias.
// A model remains selectable by its native ID and is always shown with the
// exact display name supplied by its harness.
type ModelRole string

const (
	ModelRoleBalanced ModelRole = "balanced"
	ModelRoleDeep     ModelRole = "deep"
	ModelRoleFast     ModelRole = "fast"
)

func (r ModelRole) Valid() bool {
	return r == ModelRoleBalanced || r == ModelRoleDeep || r == ModelRoleFast
}

// DeriveModelRole returns a role only when a native catalogue's ID or display
// name explicitly contains one of Agent Kate's verified family tokens.  It
// intentionally does not guess from version numbers, provider names, effort
// support, or marketing adjectives: e.g. a bare "gpt-5.x" entry is left
// ungrouped until its harness exposes an authoritative family label.
//
// The aliases themselves are not sent to a harness and never replace its
// display label. Multiple family claims are ambiguous and therefore untyped.
func DeriveModelRole(id, displayName string) ModelRole {
	roles := map[ModelRole]bool{}
	for _, source := range []string{id, displayName} {
		for _, token := range roleTokens(source) {
			switch token {
			case "sonnet", "luna":
				roles[ModelRoleBalanced] = true
			case "opus", "terra":
				roles[ModelRoleDeep] = true
			case "fable", "sol":
				roles[ModelRoleFast] = true
			}
		}
	}
	if len(roles) != 1 {
		return ""
	}
	for role := range roles {
		return role
	}
	return ""
}

func roleTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}
