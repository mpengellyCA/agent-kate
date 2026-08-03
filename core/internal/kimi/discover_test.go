package kimi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProbeEngineAuthHonorsContext makes a wedged `kimi acp` probe prove that
// its caller owns the deadline. Health passes its per-check deadline here;
// before withProbeContext, this waited for a private 20-second Background
// timeout instead.
func TestProbeEngineAuthHonorsContext(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "kimi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	sup := NewSupervisor(bin, testLogger(), nil, nil, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := sup.ProbeEngineAuth(ctx)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("ProbeEngineAuth returned an unexpected error: %v", err)
	}
	if got.State != "unknown" {
		t.Errorf("state = %q, want unknown after deadline", got.State)
	}
	if elapsed > time.Second {
		t.Fatalf("probe ignored its context for %s", elapsed)
	}
}
