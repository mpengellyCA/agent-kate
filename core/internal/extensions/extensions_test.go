package extensions

import (
	"context"
	"errors"
	"testing"

	"agentkate/internal/skills"
)

func TestFromSkillsPreservesLooseSkill(t *testing.T) {
	got := FromSkills([]skills.Skill{{Name: "review", Description: "Review changes", Path: "/tmp/review"}})
	if len(got) != 1 || got[0].Source != "catalog" || len(got[0].Components) != 1 {
		t.Fatalf("FromSkills = %#v", got)
	}
	c := got[0].Components[0]
	if c.ID != "catalog/skill/review" || c.Kind != KindSkill || c.TokenCost != 0 {
		t.Fatalf("component = %#v", c)
	}
}

func TestParsePluginDetails(t *testing.T) {
	got := parsePluginDetails("acme-tools", "Skills:\n- review - Review a diff\n  Token cost: 1,200\nCommands:\n- /ship - Ship it\n")
	if len(got.Components) != 2 {
		t.Fatalf("components = %#v", got.Components)
	}
	if got.Components[0].Kind != KindSkill || got.Components[0].TokenCost != 1200 {
		t.Fatalf("skill = %#v", got.Components[0])
	}
	if got.Components[1].Kind != KindCommand || got.Components[1].TokenCost != 0 {
		t.Fatalf("command = %#v", got.Components[1])
	}
}

func TestMalformedPluginDetailsIsNonLoadBearing(t *testing.T) {
	got := parsePluginDetails("acme", "\x00 definitely not a plugin details response\nToken cost: bananas")
	if len(got.Components) != 0 {
		t.Fatalf("malformed details produced components: %#v", got.Components)
	}
	if got.Name != "acme" {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestPluginListMalformedIsEmpty(t *testing.T) {
	if got := parsePluginList([]byte("not json")); len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestDetailsFailureReturnsCLIError(t *testing.T) {
	p := NewClaudePlugins("claude")
	p.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("not found"), errors.New("exit 1")
	}
	if _, err := p.Details(context.Background(), "nope"); err == nil {
		t.Fatal("Details succeeded")
	}
}
