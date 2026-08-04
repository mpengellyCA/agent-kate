// Package extensions provides the catalogue vocabulary shared by loose Skills
// and Claude plugins.  It deliberately wraps (rather than replaces) skills so
// the long-lived skills.* RPC surface keeps its exact behaviour during the
// migration.
package extensions

import (
	"path/filepath"

	"agentkate/internal/skills"
)

const (
	KindSkill   = "skill"
	KindCommand = "command"
	KindAgent   = "agent"
	KindHook    = "hook"
	KindMCP     = "mcp"
)

// Component is the independently selectable part of an Extension. TokenCost
// is supplied only by the upstream CLI; we never infer a cost from text.
type Component struct {
	ID          string `json:"id"`
	Bundle      string `json:"bundle"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	TokenCost   int    `json:"tokenCost,omitempty"`
}

type Extension struct {
	Name        string      `json:"name"`
	Version     string      `json:"version,omitempty"`
	Source      string      `json:"source"`
	Marketplace string      `json:"marketplace,omitempty"`
	Enabled     bool        `json:"enabled"`
	Components  []Component `json:"components"`
	// Harness identifies the native registry that owns this extension. Empty
	// means the Agent Kate loose-skill catalogue, whose file links are shared
	// across harnesses; native plugin bundles are never implied portable.
	Harness string `json:"harness,omitempty"`
}

// ConversionStatus is deliberately a result, not a promise. A native plugin
// can contain mixed components, so one outcome for the whole bundle would
// hide unsafe or unsupported pieces.
type ConversionStatus string

const (
	ConversionPortable ConversionStatus = "portable"
	ConversionReview   ConversionStatus = "review"
	ConversionBlocked  ConversionStatus = "blocked"
)

type ConversionComponent struct {
	Kind   string           `json:"kind"`
	Name   string           `json:"name"`
	Status ConversionStatus `json:"status"`
	Reason string           `json:"reason"`
}

type ConversionPreview struct {
	SourceHarness string                `json:"sourceHarness"`
	TargetHarness string                `json:"targetHarness"`
	Extension     string                `json:"extension"`
	Components    []ConversionComponent `json:"components"`
}

// PreviewConversion is intentionally non-mutating. Skills use the established
// file-link/native-root path; every other plugin component must be adapted by
// a target-harness-specific exporter before it can be installed.
func PreviewConversion(source Extension, targetHarness string) ConversionPreview {
	preview := ConversionPreview{SourceHarness: source.Harness, TargetHarness: targetHarness, Extension: source.Name}
	for _, component := range source.Components {
		out := ConversionComponent{Kind: component.Kind, Name: component.Name, Status: ConversionBlocked,
			Reason: "this component has no verified exporter for " + targetHarness}
		switch component.Kind {
		case KindSkill:
			out.Status = ConversionPortable
			out.Reason = "SKILL.md can be delivered through Agent Kate's file links and Codex skill roots"
		case KindMCP:
			out.Status = ConversionReview
			out.Reason = "MCP definitions need target-side transport, environment, and approval review"
		case KindCommand, KindAgent, KindHook:
			out.Reason = "native command, agent, and hook semantics are not portable by file link"
		}
		preview.Components = append(preview.Components, out)
	}
	return preview
}

// FromSkills exposes the existing central Skills catalogue as degenerate,
// one-component extensions.  Keeping this conversion pure is important: a
// malformed external plugin can never alter how skills are discovered or
// installed.
func FromSkills(entries []skills.Skill) []Extension {
	out := make([]Extension, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name
		out = append(out, Extension{
			Name: name, Source: "catalog", Enabled: true,
			Components: []Component{{
				ID: "catalog/skill/" + name, Bundle: "", Kind: KindSkill,
				Name: name, Description: entry.Description, Path: entry.Path,
			}},
		})
	}
	return out
}

// ComponentPath returns the conventional source path for a plugin component.
// It is intentionally a helper, not a validator: plugin metadata is only a
// display catalogue in phases 1-2 and is never loaded by Agent Kate.
func ComponentPath(root, kind, name string) string {
	return filepath.Join(root, kind, name)
}
