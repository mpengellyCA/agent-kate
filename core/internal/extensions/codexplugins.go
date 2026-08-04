package extensions

import (
	"context"

	"agentkate/internal/codex"
)

// CodexPlugins is a read-only view of Codex's native plugin registry. Plugin
// install/update policy remains Codex-owned until Agent Kate can surface its
// full approval and marketplace flows; a registry must never pretend that
// reading a plugin makes it safe to install or convert it elsewhere.
type CodexPlugins struct {
	supervisor *codex.Supervisor
}

func NewCodexPlugins(supervisor *codex.Supervisor) *CodexPlugins {
	return &CodexPlugins{supervisor: supervisor}
}

func (p *CodexPlugins) ListInstalled(ctx context.Context) ([]Extension, error) {
	entries, err := p.supervisor.DiscoverInstalledPlugins(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Extension, 0, len(entries))
	for _, entry := range entries {
		if !entry.Installed {
			continue
		}
		out = append(out, Extension{Name: entry.Name, Version: entry.Version,
			Source: "native", Marketplace: entry.Marketplace, Enabled: entry.Enabled,
			Harness: "codex"})
	}
	return out, nil
}

func (p *CodexPlugins) Install(ctx context.Context, name string) error {
	return p.supervisor.InstallPlugin(ctx, name)
}

func (p *CodexPlugins) Uninstall(ctx context.Context, name string) error {
	return p.supervisor.UninstallPlugin(ctx, name)
}
