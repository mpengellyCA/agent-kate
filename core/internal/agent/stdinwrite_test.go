package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// wedgedThread builds a Thread whose stdin is a pipe nobody drains, registered
// in a supervisor. It models a `claude` that has stopped reading stdin — the
// audit F9 trigger — without spawning a process.
func wedgedThread(t *testing.T) (*Supervisor, *Thread, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })

	s := NewSupervisor("claude", slog.New(slog.NewTextHandler(io.Discard, nil)), func(string, []json.RawMessage) {})
	// Shrink the interrupt escalation window so the graceful-stop sequencing
	// (abort → bounded wait → close stdin) completes inside the test.
	s.interruptBackstopDelay = 50 * time.Millisecond
	s.interruptKillDelay = 50 * time.Millisecond
	th := &Thread{
		ID:            "wedged",
		stdin:         w,
		cmd:           &exec.Cmd{}, // no Process: the kill backstop is a no-op
		alive:         true,
		turnsInFlight: 1, // a turn is in flight, so Interrupt is not a no-op
	}
	s.mu.Lock()
	s.threads[th.ID] = th
	s.mu.Unlock()
	return s, th, r
}

// TestWedgedStdinDoesNotBlockRecoveryPaths pins audit F9: a Send larger than
// the pipe buffer against a child that has stopped draining stdin must not park
// the thread mutex, because every recovery path the human can reach (Interrupt,
// Stop, "is it running?") needs that mutex. Before the fix the write happened
// under t.mu and the thread became unkillable from the UI.
func TestWedgedStdinDoesNotBlockRecoveryPaths(t *testing.T) {
	s, th, _ := wedgedThread(t)

	// A message far larger than the 64 KiB pipe buffer: the write blocks.
	big := strings.Repeat("x", 512*1024)
	sendErr := make(chan error, 1)
	go func() { sendErr <- s.Send(th.ID, big, nil) }()

	// Give the writer time to fill the pipe and block.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-sendErr:
		t.Fatalf("Send did not block on a full pipe (err=%v); test cannot prove anything", err)
	default:
	}

	// Every mutex-taking path must still answer promptly.
	done := make(chan string, 4)
	go func() { s.Running(th.ID); done <- "Running" }()
	go func() { s.abortPending(th.ID); done <- "abortPending" }()
	go func() { _ = s.Interrupt(th.ID); done <- "Interrupt" }()
	go func() { _ = s.Stop(th.ID); done <- "Stop" }()
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("a recovery path blocked behind the wedged stdin write")
		}
	}

	// Stop closed stdin, which is what releases the blocked write: the Send must
	// come back with an error rather than hanging for the process's lifetime.
	select {
	case err := <-sendErr:
		if err == nil {
			t.Fatal("Send reported success though its frame never landed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send never returned after stdin was closed")
	}

	// The failed turn must not be left counted as in flight, or the thread
	// reads busy forever in agent.wait and the Jobs panel.
	th.mu.Lock()
	inFlight := th.turnsInFlight
	th.mu.Unlock()
	if inFlight != 1 { // the 1 the fixture seeded; the failed Send's own must be rolled back
		t.Fatalf("turnsInFlight = %d, want 1 (the failed Send must roll its own back)", inFlight)
	}
}

// TestWriteFrameLatchesAfterFailure pins the framing guarantee: once a write
// has failed (possibly part-written), later frames must be refused rather than
// appended to a torn line the CLI's parser would choke on.
func TestWriteFrameLatchesAfterFailure(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // writes now fail with EPIPE
	th := &Thread{ID: "broken", stdin: w, cmd: &exec.Cmd{}, alive: true}

	if err := th.writeFrame([]byte("{}\n")); err == nil {
		t.Fatal("write to a closed pipe reported success")
	}
	err = th.writeFrame([]byte("{}\n"))
	if err == nil {
		t.Fatal("second write was attempted on a stream with unknown framing")
	}
	if err != errStdinBroken {
		t.Fatalf("second write failed with %v, want errStdinBroken", err)
	}
}
