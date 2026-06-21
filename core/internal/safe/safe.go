// Package safe provides goroutine launch helpers that contain panics.
//
// A panic in any goroutine that is not recovered crashes the entire
// process. In akcore that means dropping the UI socket and orphaning every
// running agent. Go runs fn in a new goroutine with a deferred recover so a
// panic in one goroutine is logged and contained instead of taking down the
// whole daemon.
package safe

import (
	"log/slog"
	"runtime/debug"
)

// Go runs fn in a new goroutine wrapped in defer/recover. If fn panics, the
// recovered value and a stack trace are logged via the default slog logger
// (tagged with name) and the goroutine returns normally; the panic does NOT
// propagate and does NOT crash the process.
//
// name should be a short, descriptive identifier for the goroutine so panics
// can be traced back to their launch site (e.g. "agent.pumpStdout").
func Go(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goroutine panic recovered",
					"goroutine", name,
					"panic", r,
					"stack", string(debug.Stack()),
				)
			}
		}()
		fn()
	}()
}
