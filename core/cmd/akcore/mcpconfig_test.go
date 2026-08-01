package main

// Where a bridge secret is allowed to travel (audit F13).
//
// The secret proves a connection is a thread's bridge, so every channel it
// rides has to be one an agent cannot casually read: env, not argv; a 0600 file
// in a directory of ours, not a world-listable /tmp glob.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPConfigCarriesSecretsInEnvOnly(t *testing.T) {
	// A runtime dir of our own, so the test does not depend on the box's.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	path, err := writeMCPConfig("/usr/bin/akcore", "/run/ak.sock", "t-1",
		"/w", "COOP-SECRET", "COWORK-SECRET")
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	want := map[string]string{"cooperation": "COOP-SECRET", "cowork": "COWORK-SECRET"}
	for name, secret := range want {
		srv, ok := cfg.MCPServers[name]
		if !ok {
			t.Fatalf("no %q server in the config", name)
		}
		if got := srv.Env[bridgeSecretEnvVar]; got != secret {
			t.Errorf("%s env secret = %q, want %q", name, got, secret)
		}
		// One secret per bridge process: the OTHER bridge's must not be here.
		for other, otherSecret := range want {
			if other != name && srv.Env[bridgeSecretEnvVar] == otherSecret {
				t.Errorf("%s carries %s's secret — a shared secret makes the "+
					"second bridge look like a replay of the first", name, other)
			}
		}
		// argv is world-readable via /proc/<pid>/cmdline.
		if strings.Contains(strings.Join(srv.Args, " "), secret) {
			t.Errorf("%s carries its secret in argv: %v", name, srv.Args)
		}
	}

	// The file itself: 0600, inside a 0700 directory of ours, and NOT in the
	// world-listable root of the temp dir (the old `/tmp/agentkate-mcp-*.json`
	// published one path per live thread to every user on the box).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", fi.Mode().Perm())
	}
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("config directory mode = %v, want 0700", di.Mode().Perm())
	}
	if dir != filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "agentkate", "mcp") {
		t.Errorf("config written to %s, want the private runtime directory", dir)
	}
}

// TestMCPConfigDirRefusesASymlink pins the fail-closed half:
// MkdirAll is happy to reuse a directory someone else planted, so the check is
// on the directory we ended up with, not on the call that made it. (A symlink
// stands in for the foreign directory — the same Lstat check catches both, and
// a test cannot create a file owned by another uid.)
func TestMCPConfigDirRefusesASymlink(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	if err := os.MkdirAll(filepath.Join(base, "agentkate"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(base, "agentkate", "mcp")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := mcpConfigDir(); err == nil {
		t.Error("mcpConfigDir followed a symlink out of its own directory")
	}
}
