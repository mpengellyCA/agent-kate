package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ClaudePlugins is the deliberately thin boundary around Claude's plugin CLI.
// All methods return CLI errors to the RPC layer; a failed marketplace must be
// represented there as a named failed source, never as a daemon failure.
type ClaudePlugins struct {
	bin string
	run func(context.Context, string, ...string) ([]byte, error)
}

func NewClaudePlugins(bin string) *ClaudePlugins {
	if strings.TrimSpace(bin) == "" {
		bin = "claude"
	}
	return &ClaudePlugins{bin: bin, run: func(ctx context.Context, bin string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, bin, args...).CombinedOutput()
	}}
}

func (p *ClaudePlugins) ListInstalled(ctx context.Context) ([]Extension, error) {
	return p.list(ctx, false)
}

func (p *ClaudePlugins) ListAvailable(ctx context.Context) ([]Extension, error) {
	return p.list(ctx, true)
}

func (p *ClaudePlugins) list(ctx context.Context, available bool) ([]Extension, error) {
	args := []string{"plugin", "list", "--json"}
	if available {
		args = append(args, "--available")
	}
	raw, err := p.run(ctx, p.bin, args...)
	if err != nil {
		return nil, fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(raw)))
	}
	return parsePluginList(raw), nil
}

// Details parses only a conservative subset of the human-readable details
// output. Metadata is display-only: failure yields a zero-cost empty inventory
// rather than an error or guessed context budget.
func (p *ClaudePlugins) Details(ctx context.Context, name string) (Extension, error) {
	if err := validatePluginName(name); err != nil {
		return Extension{}, err
	}
	raw, err := p.run(ctx, p.bin, "plugin", "details", name)
	if err != nil {
		return Extension{}, fmt.Errorf("claude plugin details %q: %w: %s", name, err, strings.TrimSpace(string(raw)))
	}
	return parsePluginDetails(name, string(raw)), nil
}

func (p *ClaudePlugins) Marketplaces(ctx context.Context) ([]string, error) {
	raw, err := p.run(ctx, p.bin, "plugin", "marketplace", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("claude plugin marketplace list: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		return names, nil
	}
	var wrapped struct {
		Marketplaces []struct {
			Name string `json:"name"`
		} `json:"marketplaces"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("invalid marketplace JSON: %w", err)
	}
	for _, m := range wrapped.Marketplaces {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names, nil
}

func (p *ClaudePlugins) AddMarketplace(ctx context.Context, source string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("marketplace source is required")
	}
	raw, err := p.run(ctx, p.bin, "plugin", "marketplace", "add", source)
	if err != nil {
		return fmt.Errorf("claude plugin marketplace add: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (p *ClaudePlugins) RemoveMarketplace(ctx context.Context, name string) error {
	if err := validatePluginName(name); err != nil {
		return err
	}
	raw, err := p.run(ctx, p.bin, "plugin", "marketplace", "remove", name)
	if err != nil {
		return fmt.Errorf("claude plugin marketplace remove: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (p *ClaudePlugins) command(ctx context.Context, action, name string) error {
	if err := validatePluginName(name); err != nil {
		return err
	}
	raw, err := p.run(ctx, p.bin, "plugin", action, name)
	if err != nil {
		return fmt.Errorf("claude plugin %s %q: %w: %s", action, name, err, strings.TrimSpace(string(raw)))
	}
	return nil
}
func (p *ClaudePlugins) Install(ctx context.Context, name string) error {
	return p.command(ctx, "install", name)
}
func (p *ClaudePlugins) Uninstall(ctx context.Context, name string) error {
	return p.command(ctx, "uninstall", name)
}
func (p *ClaudePlugins) Enable(ctx context.Context, name string) error {
	return p.command(ctx, "enable", name)
}
func (p *ClaudePlugins) Disable(ctx context.Context, name string) error {
	return p.command(ctx, "disable", name)
}

func parsePluginList(raw []byte) []Extension {
	var entries []struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Marketplace string `json:"marketplace"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		var wrapped struct {
			Plugins []struct {
				Name        string `json:"name"`
				Version     string `json:"version"`
				Marketplace string `json:"marketplace"`
				Enabled     *bool  `json:"enabled"`
			} `json:"plugins"`
		}
		if json.Unmarshal(raw, &wrapped) != nil {
			return []Extension{}
		}
		entries = wrapped.Plugins
	}
	out := make([]Extension, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.Name) == "" {
			continue
		}
		enabled := true
		if e.Enabled != nil {
			enabled = *e.Enabled
		}
		out = append(out, Extension{Name: e.Name, Version: e.Version, Source: "marketplace", Marketplace: e.Marketplace, Enabled: enabled})
	}
	return out
}

var heading = regexp.MustCompile(`(?i)^\s*(skills?|commands?|agents?|hooks?|mcp(?:\s+servers?)?)\s*:?\s*$`)
var cost = regexp.MustCompile(`(?i)(?:token\s*(?:cost|estimate|budget)?|projected\s+tokens?)\D{0,12}([0-9][0-9,]*)`)

func parsePluginDetails(name, raw string) Extension {
	e := Extension{Name: name, Source: "marketplace", Enabled: true}
	section := ""
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 1024), 256*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if m := heading.FindStringSubmatch(line); m != nil {
			section = kindForHeading(m[1])
			continue
		}
		if m := cost.FindStringSubmatch(line); m != nil && len(e.Components) > 0 {
			// Parsing errors intentionally retain 0: cost cannot become load-bearing.
			var n int
			_, err := fmt.Sscanf(strings.ReplaceAll(m[1], ",", ""), "%d", &n)
			if err == nil && n >= 0 {
				e.Components[len(e.Components)-1].TokenCost = n
			}
			continue
		}
		if section == "" || line == "" {
			continue
		}
		item := strings.TrimSpace(strings.TrimLeft(line, "-*• \t"))
		if item == "" || strings.Contains(item, ":") && !strings.Contains(item, " - ") {
			continue
		}
		namePart, desc, _ := strings.Cut(item, " - ")
		namePart = strings.TrimSpace(namePart)
		if namePart == "" || strings.ContainsAny(namePart, "[]{}") {
			continue
		}
		e.Components = append(e.Components, Component{ID: name + "/" + section + "/" + namePart, Bundle: name, Kind: section, Name: namePart, Description: strings.TrimSpace(desc)})
	}
	return e
}

func kindForHeading(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(s, "skill"):
		return KindSkill
	case strings.HasPrefix(s, "command"):
		return KindCommand
	case strings.HasPrefix(s, "agent"):
		return KindAgent
	case strings.HasPrefix(s, "hook"):
		return KindHook
	default:
		return KindMCP
	}
}
func validatePluginName(name string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\x00\r\n") {
		return fmt.Errorf("plugin name is required and may not contain control characters")
	}
	return nil
}
