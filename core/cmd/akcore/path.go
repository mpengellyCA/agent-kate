package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// augmentPath prepends common user-install bin directories to PATH so that
// CLIs installed under the user's home (notably `claude`, plus `gh`, `git`,
// `kdiff3`) resolve when AgentKate is launched from a .desktop entry. KDE/
// Plasma sessions inherit the systemd-user PATH, which often omits the dirs
// shell rc files add — so a terminal-launched dev build finds `claude` while
// the installed app does not.
//
// Only directories that exist on disk are added, and PATH order is preserved
// for entries already present.
func augmentPath() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	candidates := []string{
		filepath.Join(home, ".local", "bin"),       // pipx, ~/.local installs
		filepath.Join(home, ".npm-global", "bin"),  // common `npm config set prefix` target
		filepath.Join(home, ".bun", "bin"),         // Bun
		filepath.Join(home, ".volta", "bin"),       // Volta
		filepath.Join(home, ".cargo", "bin"),       // Rust
		filepath.Join(home, "go", "bin"),           // Go binaries
		"/usr/local/bin",
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates,
			"/opt/homebrew/bin", // Apple Silicon Homebrew
			"/opt/homebrew/sbin",
		)
	}

	current := os.Getenv("PATH")
	existing := make(map[string]struct{})
	for _, p := range filepath.SplitList(current) {
		existing[p] = struct{}{}
	}

	var add []string
	for _, dir := range candidates {
		if _, seen := existing[dir]; seen {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		add = append(add, dir)
	}
	if len(add) == 0 {
		return
	}

	parts := append(add, current)
	_ = os.Setenv("PATH", strings.Join(parts, string(filepath.ListSeparator)))
}
