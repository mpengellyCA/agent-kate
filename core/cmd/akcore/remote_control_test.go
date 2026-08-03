package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"agentkate/internal/permission"
)

func TestRemoteControlAttachIsCredentialSideEffectFree(t *testing.T) {
	root := t.TempDir()
	c := newRemoteControl(context.Background(), slog.Default())
	c.dataDir = root
	c.attach(handlerDeps{broker: permission.New()})
	if c.server() == nil {
		t.Fatal("remote control did not construct its unbound server")
	}
	if c.server().Running() {
		t.Fatal("attach unexpectedly opened a listener")
	}
	for _, name := range []string{"remote-devices.json", "remote-audit.jsonl", "remote-cert.pem", "remote-key.pem"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("attach created %s: %v", name, err)
		}
	}
}
