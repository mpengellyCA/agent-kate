package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"agentkate/internal/ipc"
	"agentkate/internal/permission"
)

// TestAskCoworkEnableRefusesPromptlyWithNoUIToAsk (audit F35 pass 3 / F53):
// askCoworkEnable pushed cowork.enableRequested and then parked on the
// 8-minute permission timer whether or not any window had received it. With
// the UI crashed or the core headless, enable_cowork (or a cowork:true
// launch) held a bridge handler goroutine for the whole window, ending in the
// same denial it could have given at once.
//
// It fails closed either way, so the assertion is on the CLOCK as much as on
// the answer: a regression shows up as this test's own 5-second guard firing,
// not as a slow pass. No UI connection is made, deliberately — the point is
// that there is nobody at all.
func TestAskCoworkEnableRefusesPromptlyWithNoUIToAsk(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := ipc.NewServer(filepath.Join(t.TempDir(), "noui.sock"), log)
	d := handlerDeps{srv: srv, broker: permission.New(), log: log}

	start := time.Now()
	done := make(chan bool, 1)
	go func() {
		done <- askCoworkEnable(d, "t-asker", "t-asker", "a worker", "needs the desktop")
	}()

	select {
	case allowed := <-done:
		if allowed {
			t.Fatal("no window connected must fail closed, not grant desktop access")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askCoworkEnable parked on the permission timeout with no UI connected to ask")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the refusal took %s; with nobody to ask it must be immediate", elapsed)
	}
}
