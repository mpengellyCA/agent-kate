// Package agent supervises headless Claude Code processes — the "agent threads"
// that do the work in Agent Kate's harness. Each thread is one `claude` process
// run in streaming-JSON mode so its conversation can be observed live and
// continued with follow-up messages.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/safe"
)

// EventFunc receives a coalesced batch of events for a thread, in the order
// `claude` produced them. Each event is either a raw stream-json object emitted
// by `claude`, or a synthetic object injected by this package — synthetic types
// begin with an underscore (_stderr, _lifecycle) so the UI can tell them apart.
//
// Events are buffered per thread and flushed as one batch either on a short
// timer or when a semantic boundary (a result, a tool_use, or a synthetic
// event) is reached — whichever comes first. Batching the delivery collapses
// the per-line stream-json flood into far fewer notifications without changing
// event content or ordering.
type EventFunc func(threadID string, events []json.RawMessage)

// coalesceWindow is how long a thread's event buffer may sit before a timer
// flush. Short enough to stay imperceptible in the UI, long enough to fold a
// burst of partial assistant deltas into a single notification.
const coalesceWindow = 25 * time.Millisecond

// Supervisor owns the set of running agent threads.
type Supervisor struct {
	claudeBin string
	log       *slog.Logger
	emit      EventFunc

	mu      sync.Mutex
	threads map[string]*Thread

	// Interrupt-backstop timings: how long an unacknowledged in-band abort may
	// stay pending before the process group is SIGINTed, and how long after
	// that before SIGKILL. Overridable in tests; the defaults suit real claude.
	interruptBackstopDelay time.Duration
	interruptKillDelay     time.Duration

	// reapWG tracks every in-flight reap() goroutine. StopAll waits on it so
	// the caller can be sure every thread's "exited" lifecycle event has been
	// delivered — and thus every cold-exit compaction has been spawned —
	// before shutdown proceeds to drain those compactions.
	reapWG sync.WaitGroup
}

// NewSupervisor creates a supervisor. emit is invoked with a coalesced batch of
// thread events (see EventFunc). An empty claudeBin defaults to "claude"
// (resolved via PATH).
func NewSupervisor(claudeBin string, log *slog.Logger, emit EventFunc) *Supervisor {
	if claudeBin == "" {
		claudeBin = "claude"
	}
	return &Supervisor{
		claudeBin:              claudeBin,
		log:                    log,
		emit:                   emit,
		interruptBackstopDelay: 3 * time.Second,
		interruptKillDelay:     2 * time.Second,
		threads:                make(map[string]*Thread),
	}
}

// Attachment is a file the human attached to a message: an image (sent as an
// image content block) or a text file (embedded inline as text).
type Attachment struct {
	Kind      string `json:"kind"`      // "image" or "text"
	Name      string `json:"name"`
	MediaType string `json:"mediaType"` // images, e.g. "image/png"
	Text      string `json:"text"`      // text files: the file content
	DataB64   string `json:"dataB64"`   // images: base64-encoded bytes
	// Path and Outside are UI-side provenance the harness never sends to the
	// model — they ride along so the core can persist a compact per-thread
	// attachment sidecar (name/kind/path/mediaType/outside) that lets the UI
	// re-draw the "You" card's attachment chips after a restart/resume, since
	// the on-disk transcript keeps only the inlined content, not the origin.
	Path    string `json:"path,omitempty"`
	Outside bool   `json:"outside,omitempty"`
}

// StartOptions configures a new agent thread.
type StartOptions struct {
	ID             string       // thread id; generated if empty
	WorkDir        string       // working directory for the agent (a workspace or worktree)
	Prompt         string       // initial user message
	MCPConfig      string       // optional path to a --mcp-config file
	PermissionMode string       // claude --permission-mode; defaults to acceptEdits
	Effort         string       // claude --effort level; empty leaves Claude Code's default
	Model          string       // claude --model id; empty leaves Claude Code's default (smoke tests pin Haiku)
	Attachments    []Attachment // files attached to the opening message
	SessionID      string       // Claude Code session id (a UUID)
	Resume         bool         // true: --resume the session; false: --session-id a new one
	ForkSession    bool         // with Resume: --fork-session — branch a NEW session off the resumed context
	CoworkEnabled  bool         // opt this thread into the KDE Cowork desktop MCP server
	Provider       *Provider    // optional third-party API routing; nil/empty BaseURL = Claude direct
}

// buildUserContent assembles a stream-json user message content array from the
// message text and any attachments.
func buildUserContent(text string, attachments []Attachment) []map[string]any {
	var content []map[string]any
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, a := range attachments {
		switch a.Kind {
		case "text":
			content = append(content, map[string]any{
				"type": "text",
				"text": fmt.Sprintf("Attached file `%s`:\n```\n%s\n```", a.Name, a.Text),
			})
		case "image":
			content = append(content, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": a.MediaType,
					"data":       a.DataB64,
				},
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return content
}

// Thread is one running `claude` process.
type Thread struct {
	ID      string
	WorkDir string

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	mcpConfig   string      // temp --mcp-config file to clean up on exit
	meter       *toolMeter  // measures tool_result sizes for token-cost telemetry
	usage       *usageMeter // measures per-turn LLM token usage and billed cost
	alive       bool
	pgid        int         // process-group id (== leader pid); signalled by Interrupt
	interrupted bool        // set by Interrupt so reap() reports a user-interrupt if the process dies
	aborting    bool        // set by Interrupt while an in-band abort is pending; cleared on the aborted turn's result
	stopping    bool        // a Stop is in flight; suppresses turn_aborted and rejects new Sends
	// turnsInFlight counts user messages written whose `result` event has not
	// arrived yet — the claude counterpart of the kimi supervisor's
	// activePrompts. Every turn is initiated by our own Send (opening prompt,
	// follow-ups, Compact's summary prompt), and the CLI ends each with exactly
	// one result event, so Send increments and the result observer decrements.
	// Interrupt is a no-op at 0: aborting an idle CLI would arm a backstop no
	// result can ever disarm, killing a healthy resident process seconds later.
	turnsInFlight int
	// controls maps an in-flight control_request's request_id to the waiter
	// for its control_response, so SetModel / SetPermissionMode can report
	// the CLI's actual verdict (e.g. "not a recognized model id") instead of
	// fire-and-forgetting. Interrupt deliberately does not wait (its backstop
	// covers the no-ack case).
	controls   map[string]chan controlOutcome
	hotCompact *hotCompact // when non-nil, the next assistant turn is captured for a summary

	co *coalescer // batches this thread's events before they reach the emit callback
}

// coalescer buffers a thread's events and flushes them to the supervisor's
// emit callback as a single batch, preserving production order. A flush fires
// on a short timer (coalesceWindow) or immediately when a semantic boundary
// (result / tool_use / synthetic event) is buffered, whichever comes first.
//
// All buffered events feed one emit call, so ordering within and across flushes
// matches the order add() was called — which, because pumpStdout reads the
// stream sequentially, is the order `claude` produced them.
type coalescer struct {
	threadID string
	emit     EventFunc

	mu      sync.Mutex
	pending []json.RawMessage
	timer   *time.Timer // non-nil while a flush is scheduled
}

func newCoalescer(threadID string, emit EventFunc) *coalescer {
	return &coalescer{threadID: threadID, emit: emit}
}

// add appends one event to the buffer. boundary forces an immediate flush
// (used for result / tool_use / synthetic events); otherwise a timer flush is
// scheduled. dedup, when true, drops this event if a byte-identical event is
// already buffered in the current batch — a provably content-safe way to
// collapse the repeated partial assistant snapshots `claude --verbose` emits.
func (c *coalescer) add(raw json.RawMessage, boundary, dedup bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dedup && len(c.pending) > 0 {
		// Drop only when the IMMEDIATELY-PRECEDING buffered event is byte-equal:
		// that is the exact `claude --verbose` partial-snapshot pattern. Scanning
		// the whole batch could drop a legitimately-repeated, non-adjacent
		// identical event.
		if bytes.Equal(c.pending[len(c.pending)-1], raw) {
			return
		}
	}
	c.pending = append(c.pending, raw)
	if boundary {
		c.flushLocked()
		return
	}
	if c.timer == nil {
		// AfterFunc runs its callback on its own goroutine; route the flush
		// through safe.Go so a panic there is recovered rather than crashing
		// the process.
		c.timer = time.AfterFunc(coalesceWindow, func() {
			safe.Go("agent.coalesceFlush", c.flush)
		})
	}
}

// flush delivers any buffered events. Called by the timer and by pumpStdout
// when the stream ends.
func (c *coalescer) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked()
}

// flushLocked drains the buffer and delivers it to emit. The caller holds
// c.mu, and emit runs while the lock is held: this serialises batch delivery
// so two concurrent producers (pumpStdout, pumpStderr, reap) can never reorder
// their batches. emit never re-enters the coalescer, so holding the lock across
// it cannot deadlock.
func (c *coalescer) flushLocked() {
	batch := c.takeLocked()
	if len(batch) > 0 {
		c.emit(c.threadID, batch)
	}
}

// takeLocked detaches the current buffer and clears the pending state. Caller
// holds c.mu.
func (c *coalescer) takeLocked() []json.RawMessage {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if len(c.pending) == 0 {
		return nil
	}
	batch := c.pending
	c.pending = nil
	return batch
}

// controlOutcome is the CLI's answer to one control_request: an empty err
// means the subtype succeeded.
type controlOutcome struct {
	err string
}

// hotCompact tracks an in-flight Hot-Opus compaction: assistant text is
// accumulated until a `result` event signals turn completion, at which
// point done is closed and all callers in Compact() read the buffered text.
// Multiple callers (e.g. agent.stop + akcore shutdown) share one compaction;
// the starter sends the prompt, everyone else waits on done.
type hotCompact struct {
	text strings.Builder
	done chan struct{}
	err  error
	once sync.Once
}

// finish closes done at most once. Safe to call from observeHotCompact
// (on `result`), from a failed Send in Compact(), or from reap() when the
// agent dies before its summary turn lands.
func (hc *hotCompact) finish(err error) {
	hc.once.Do(func() {
		hc.err = err
		close(hc.done)
	})
}

// NewThreadID returns a fresh, unique agent thread id.
func NewThreadID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "t-" + hex.EncodeToString(b[:])
}

// Start launches a new agent thread and sends it the initial prompt.
//
// The agent is driven through Claude Code's documented headless interface:
// `claude --print` with stream-json on both stdin and stdout. M1 runs it
// directly in WorkDir with --permission-mode acceptEdits — file edits are
// auto-accepted and the Cooperation MCP is allowed, while Bash and network
// tools stay gated. Every tool call is still surfaced to the UI. M2 will move
// each thread into its own git worktree and add per-tool approval.
func (s *Supervisor) Start(opts StartOptions) (*Thread, error) {
	mode := opts.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	// The Cooperation MCP is always allowed; the opt-in Cowork desktop server is
	// added only when the thread enabled it. Consent for individual desktop
	// actions is enforced server-side (the cowork consent authority), NOT by this
	// allow-list — so --permission-mode cannot bypass it.
	allowedTools := "mcp__cooperation"
	if opts.CoworkEnabled {
		allowedTools += ",mcp__cowork"
	}
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
		"--allowedTools", allowedTools,
	}
	// Reasoning effort is optional: an empty value leaves Claude Code on
	// whatever default the user has configured.
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	// Model is optional: empty means inherit Claude Code's configured default
	// (typically Opus). Smoke tests pin Haiku to keep their token spend low.
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	// A fresh thread is pinned to a session id we choose, so it can be resumed
	// later; a resumed thread replays that same Claude Code session.
	if opts.SessionID != "" {
		if opts.Resume {
			args = append(args, "--resume", opts.SessionID)
			// A fork replays the resumed session's context but mints a fresh
			// session id for the new turns, leaving the source session untouched.
			// The init event reports that new id, which the run loop persists.
			if opts.ForkSession {
				args = append(args, "--fork-session")
			}
		} else {
			args = append(args, "--session-id", opts.SessionID)
		}
	}
	if opts.MCPConfig != "" {
		args = append(args,
			"--mcp-config", opts.MCPConfig,
			// Gated tools (Bash and the like) route to our MCP approval tool,
			// which surfaces an approve/deny prompt in the agent panel.
			"--permission-prompt-tool", "mcp__cooperation__request_permission",
		)
	}

	cmd := exec.Command(s.claudeBin, args...)
	cmd.Dir = opts.WorkDir
	// Route this child at a third-party Anthropic-compatible endpoint when a
	// provider is selected; buildEnv scrubs any inherited Anthropic credentials
	// first so a real Claude key is never forwarded to someone else's base URL.
	// Claude-direct threads get os.Environ() back unchanged.
	env, err := buildEnv(os.Environ(), opts.Provider)
	if err != nil {
		return nil, err
	}
	cmd.Env = env
	// Put the agent in its own process group so Interrupt() can signal the whole
	// group (claude + any tools / MCP subprocesses it spawns) rather than
	// orphaning children. The group id equals the leader's pid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	id := opts.ID
	if id == "" {
		id = NewThreadID()
	}
	t := &Thread{
		ID:        id,
		WorkDir:   opts.WorkDir,
		cmd:       cmd,
		stdin:     stdin,
		mcpConfig: opts.MCPConfig,
		meter:     newToolMeter(s.log, id),
		usage:     newUsageMeter(s.log, id),
	}
	t.co = newCoalescer(t.ID, s.emit)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.claudeBin, err)
	}
	t.alive = true
	// With Setpgid the child is the leader of a new group whose id is its pid.
	t.pgid = cmd.Process.Pid

	// Register atomically: a double resume (say a double-clicked Resume) can
	// race two Starts to here, and a blind overwrite would strand the loser's
	// live process — the winner's reap() would delete the map entry and
	// deregister it. Refuse the duplicate so the race loser fails cleanly.
	s.mu.Lock()
	if _, dup := s.threads[t.ID]; dup {
		s.mu.Unlock()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("thread %q already running", t.ID)
	}
	s.threads[t.ID] = t
	s.mu.Unlock()

	// The "started" lifecycle event is emitted by the orchestration layer
	// once the thread id is known to the UI; here we only log.
	provider := ""
	if opts.Provider.Routed() {
		provider = opts.Provider.ID // id/base URL only — never the token
	}
	s.log.Info("agent process spawned", "thread", t.ID, "dir", opts.WorkDir, "pid", cmd.Process.Pid, "provider", provider)

	safe.Go("agent.pumpStdout", func() { s.pumpStdout(t, stdout) })
	safe.Go("agent.pumpStderr", func() { s.pumpStderr(t, stderr) })
	s.reapWG.Add(1)
	safe.Go("agent.reap", func() { s.reap(t) })

	// A fresh thread gets its opening turn now. A resumed thread has no opening
	// prompt — it waits for the user's next message instead.
	if opts.Prompt != "" || len(opts.Attachments) > 0 {
		if err := s.Send(t.ID, opts.Prompt, opts.Attachments); err != nil {
			s.log.Warn("failed to send initial prompt", "thread", t.ID, "err", err)
		}
	}
	return t, nil
}

// Send delivers a follow-up user message — text plus any attachments — to a
// running thread.
func (s *Supervisor) Send(threadID, text string, attachments []Attachment) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	msg, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": buildUserContent(text, attachments),
		},
	})
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.alive {
		return fmt.Errorf("thread %q is not running", threadID)
	}
	if t.stopping {
		return fmt.Errorf("thread %q is stopping", threadID)
	}
	if _, err := t.stdin.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("write to agent: %w", err)
	}
	t.turnsInFlight++
	return nil
}

// Stop ends a thread. On an idle thread it closes the input stream — the CLI
// exits at its next stdin read — with a kill backstop for a process that
// doesn't. On a busy thread (a turn in flight) closing stdin alone is not
// graceful: the CLI keeps generating until the turn ends, so the backstop
// SIGKILLs any turn longer than the grace window mid-write, which can leave
// the session JSONL truncated. So a busy Stop first aborts the turn in-band
// (the same interrupt the CLI's Esc uses), waits — bounded — for the aborted
// turn's result to land, and only then closes stdin. Stop returns immediately
// in both cases; the graceful sequencing runs on a background goroutine.
func (s *Supervisor) Stop(threadID string) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	t.mu.Lock()
	if !t.alive {
		_ = t.stdin.Close()
		t.mu.Unlock()
		return nil
	}
	if t.stopping {
		t.mu.Unlock()
		return nil // a stop is already in flight
	}
	t.stopping = true
	busy := t.turnsInFlight > 0
	t.mu.Unlock()
	if busy {
		safe.Go("agent.gracefulStop", func() { s.abortThenClose(t) })
	} else {
		s.closeStdin(t)
	}
	return nil
}

// closeStdin closes the thread's input stream so the CLI exits at its next
// stdin read, and arms the kill backstop for a process that doesn't. reap()
// handles the clean exit; the backstop only fires if the process lingers.
func (s *Supervisor) closeStdin(t *Thread) {
	t.mu.Lock()
	proc := t.cmd.Process
	_ = t.stdin.Close()
	t.mu.Unlock()
	safe.Go("agent.stopKillBackstop", func() {
		time.Sleep(5 * time.Second)
		t.mu.Lock()
		stillAlive := t.alive
		t.mu.Unlock()
		if stillAlive && proc != nil {
			_ = proc.Kill()
		}
	})
}

// abortThenClose is the busy half of Stop: abort the in-flight turn in-band,
// wait for its result (so the CLI finishes writing the session JSONL), then
// close stdin. The wait is bounded just past Interrupt's own escalation
// window: if the abort never lands (a hung tool), the interrupt backstop has
// already SIGINT/SIGKILLed the process by then and the close is a no-op.
func (s *Supervisor) abortThenClose(t *Thread) {
	if err := s.Interrupt(t.ID); err != nil {
		s.log.Warn("stop: in-band abort failed; closing stdin directly",
			"thread", t.ID, "err", err)
		s.closeStdin(t)
		return
	}
	deadline := time.Now().Add(s.interruptBackstopDelay + s.interruptKillDelay + time.Second)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		done := !t.alive || (!t.aborting && t.turnsInFlight == 0)
		t.mu.Unlock()
		if done {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.closeStdin(t)
}

// Interrupt halts a thread's in-flight turn *immediately* while keeping the
// process resident and the session hot. Unlike Stop — the "wind the thread
// down" path used by StopAll and panel close — this aborts generation
// mid-response so no further tokens are billed, then leaves the CLI running so the
// next Send goes down the same stdin with no resume cost. A no-op on an idle
// thread: with no turn in flight there is nothing to abort.
//
// Primary path is in-band and does NOT close stdin: it writes Claude Code's
// stream-json interrupt control_request. A spike against claude 2.1.185 confirmed
// the CLI acks the frame (control_response) within ~100ms, emits a `result` for
// the aborted turn, stays resident, and answers a subsequent user message from the
// same session with full context. pumpStdout watches for that result and emits a
// `turn_aborted` lifecycle event (process alive) so the UI resets to idle.
//
// Signals are the fallback for a hung tool (e.g. a `sleep 600` bash call the CLI
// can't cancel in-band): if no result lands within a short grace, we SIGINT the
// whole process group (claude + spawned tools), escalating to SIGKILL. That kills
// the process, so reap() reports a user-interrupt and the thread goes dormant;
// the UI's auto-resume-on-send then brings it back with context.
func (s *Supervisor) Interrupt(threadID string) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	frame, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": "ak-interrupt-" + NewThreadID(),
		"request":    map[string]any{"subtype": "interrupt"},
	})
	if err != nil {
		return err
	}

	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return nil
	}
	if t.turnsInFlight == 0 {
		// Nothing in flight to abort. Sending the frame anyway would arm the
		// backstop with no result coming to disarm it, and a few seconds later
		// it would kill a perfectly idle resident process (the same hazard the
		// kimi supervisor guards against).
		t.mu.Unlock()
		return nil
	}
	// Mark the abort pending but do NOT set t.interrupted yet: that flag makes
	// reap() report a user-interrupt, and we only want that if the escalation
	// backstop has to kill the process. A clean in-band abort keeps the process
	// alive, so reap() never runs for it.
	t.aborting = true
	pgid := t.pgid
	proc := t.cmd.Process
	// In-band abort. stdin stays OPEN so the process stays resident for the next
	// message.
	_, werr := t.stdin.Write(append(frame, '\n'))
	t.mu.Unlock()
	if werr != nil {
		s.log.Warn("interrupt frame write failed; relying on signal backstop",
			"thread", threadID, "err", werr)
	}

	// Signal backstop: escalate only if the in-band abort never produced a
	// result (a hung tool the CLI can't cancel). If the abort landed cleanly,
	// pumpStdout has already cleared t.aborting, so we do nothing.
	safe.Go("agent.interruptBackstop", func() {
		time.Sleep(s.interruptBackstopDelay)
		// Signal only the exact thread this backstop was armed for. If the
		// original was reaped during the sleep and its id reused by a resumed
		// thread, s.thread(threadID) is now a different *Thread — the captured
		// pgid is stale and must not be signalled.
		if s.thread(threadID) != t || !s.abortPending(threadID) {
			return // clean in-band abort, or a different thread now owns this id
		}
		s.log.Info("interrupt not acked in-band; escalating to signals", "thread", threadID)
		t.mu.Lock()
		t.interrupted = true // reap() will report a user-interrupt now
		t.mu.Unlock()
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		}
		time.Sleep(s.interruptKillDelay)
		if s.thread(threadID) != t || !s.Running(threadID) {
			return
		}
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else if proc != nil {
			_ = proc.Kill()
		}
	})
	return nil
}

// SetModel switches the model for the thread's NEXT turn via the CLI's
// set_model control request — the same mechanism the interactive /model
// command uses. Verified against claude 2.1.220: the switch is real (the
// following turn runs and bills on the new model) and an unrecognised id is
// rejected with a clear error, which is returned here.
func (s *Supervisor) SetModel(threadID, model string) error {
	return s.sendControl(threadID, "set_model", map[string]any{"model": model})
}

// SetPermissionMode switches the permission mode mid-session via the CLI's
// set_permission_mode control request (valid modes per claude 2.1.220:
// acceptEdits, auto, bypassPermissions, default, dontAsk, plan).
func (s *Supervisor) SetPermissionMode(threadID, mode string) error {
	return s.sendControl(threadID, "set_permission_mode", map[string]any{"mode": mode})
}

// controlTimeout bounds how long a control request may wait for its response.
// The CLI answers set_model / set_permission_mode immediately (~ms), so a
// timeout means the process is wedged or dying.
const controlTimeout = 10 * time.Second

// sendControl writes one control_request and waits (bounded) for its
// control_response, returning the CLI's error verbatim if it rejected the
// request.
func (s *Supervisor) sendControl(threadID, subtype string, fields map[string]any) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	req := map[string]any{"subtype": subtype}
	for k, v := range fields {
		req[k] = v
	}
	reqID := "ak-" + subtype + "-" + NewThreadID()
	frame, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    req,
	})
	if err != nil {
		return err
	}
	ch := make(chan controlOutcome, 1)
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return fmt.Errorf("thread %q is not running", threadID)
	}
	if t.stopping {
		t.mu.Unlock()
		return fmt.Errorf("thread %q is stopping", threadID)
	}
	if t.controls == nil {
		t.controls = make(map[string]chan controlOutcome)
	}
	t.controls[reqID] = ch
	_, werr := t.stdin.Write(append(frame, '\n'))
	if werr != nil {
		delete(t.controls, reqID)
		t.mu.Unlock()
		return fmt.Errorf("write to agent: %w", werr)
	}
	t.mu.Unlock()

	select {
	case out := <-ch:
		if out.err != "" {
			return fmt.Errorf("%s", out.err)
		}
		return nil
	case <-time.After(controlTimeout):
		t.mu.Lock()
		delete(t.controls, reqID)
		t.mu.Unlock()
		return fmt.Errorf("%s: no response from the agent", subtype)
	}
}

// abortPending reports whether a thread still has an unacknowledged in-band
// abort in flight (the escalation backstop's trigger condition).
func (s *Supervisor) abortPending(threadID string) bool {
	t := s.thread(threadID)
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive && t.aborting
}

// StopAll terminates every running thread (used at core shutdown) and blocks
// until each thread's reap() has run to completion. Stop() returns before the
// process has exited (it only sequences the abort/stdin-close and backstops),
// so without this join the core could exit before a thread's "exited"
// lifecycle event fires — which is exactly what spawns the cold-exit
// compaction. Waiting here guarantees every such compaction has at least been
// spawned (and registered with its WaitGroup) before the caller moves on to
// drain them.
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.threads))
	for id := range s.threads {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.Stop(id)
	}
	s.reapWG.Wait()
}

func (s *Supervisor) pumpStdout(t *Thread, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		raw := append([]byte(nil), line...)
		// The live session id (which can change on an in-session compaction) is
		// captured and persisted to the thread's Record in the supervisor relay
		// (run.go), so --resume always targets the latest session.
		//
		// If a Hot-Opus compaction is in flight, accumulate the assistant
		// text from this event and complete the channel on the next result.
		observeHotCompact(t, raw)
		// Telemetry: where context tokens go (tool outputs) and what we are
		// billed for (per-turn usage). Both observe; neither alters the stream.
		t.meter.Observe(raw)
		t.usage.Observe(raw)
		boundary, dedup, typ := classifyEvent(raw)
		t.co.add(json.RawMessage(raw), boundary, dedup)
		// A `result` ends a turn: decrement the in-flight count and complete a
		// pending in-band interrupt. Done AFTER the result event itself is
		// buffered so the turn_aborted lifecycle event orders behind it.
		if typ == "result" {
			s.observeTurnEnd(t)
		}
		// A control_response answers a SetModel / SetPermissionMode waiter.
		if typ == "control_response" {
			observeControlResponse(t, raw)
		}
	}
	// Drain whatever is still buffered when the stream ends so a trailing
	// partial batch is never stranded behind its timer.
	t.co.flush()
}

// classifyEvent inspects one stream-json event to decide whether it should
// force an immediate coalescer flush (a semantic boundary) and whether it is
// a candidate for byte-identical dedup within a batch, and reports the
// event's type so the caller can track turn ends and control responses
// without re-parsing.
//
// Boundaries are kept conservative: a `result` (turn end) and any assistant
// turn carrying a tool_use block (the UI renders a tool card and may gate on
// it) flush right away so latency-sensitive UI never waits on the timer.
// Plain assistant text events are dedup candidates because `claude --verbose`
// can repeat an identical partial snapshot; only exact duplicates are dropped,
// so no content is ever lost.
func classifyEvent(raw json.RawMessage) (boundary, dedup bool, typ string) {
	var head struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return false, false, ""
	}
	switch head.Type {
	case "result":
		return true, false, head.Type
	case "assistant":
		for _, blk := range head.Message.Content {
			if blk.Type == "tool_use" {
				return true, false, head.Type
			}
		}
		return false, true, head.Type
	}
	return false, false, head.Type
}

// observeControlResponse completes the waiter for one control_response. The
// response's request_id keys the controls map; responses nobody registered
// for (e.g. an interrupt ack) are ignored.
func observeControlResponse(t *Thread, raw json.RawMessage) {
	var head struct {
		Response struct {
			RequestID string `json:"request_id"`
			Subtype   string `json:"subtype"`
			Error     string `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &head) != nil || head.Response.RequestID == "" {
		return
	}
	t.mu.Lock()
	ch := t.controls[head.Response.RequestID]
	delete(t.controls, head.Response.RequestID)
	t.mu.Unlock()
	if ch == nil {
		return
	}
	out := controlOutcome{}
	if head.Response.Subtype == "error" {
		out.err = head.Response.Error
		if out.err == "" {
			out.err = "control request rejected"
		}
	}
	ch <- out
}

// observeHotCompact pulls assistant text out of one stream-json event into
// the thread's pending hot-compact buffer (if any), and delivers the
// accumulated text on the channel when a `result` event is seen.
func observeHotCompact(t *Thread, raw json.RawMessage) {
	t.mu.Lock()
	hc := t.hotCompact
	t.mu.Unlock()
	if hc == nil {
		return
	}
	var head struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return
	}
	switch head.Type {
	case "assistant":
		for _, blk := range head.Message.Content {
			if blk.Type == "text" {
				hc.text.WriteString(blk.Text)
			}
		}
	case "result":
		hc.finish(nil)
	}
}

// observeTurnEnd handles a `result` event: the turn it ends is no longer in
// flight, and if an in-band interrupt was pending this result completes it.
// The process stays alive on a clean abort, so instead of a reap we emit a
// `turn_aborted` lifecycle event: the UI resets to idle and the next Send goes
// down the same stdin with no resume cost. During a graceful Stop the
// lifecycle event is suppressed — the exited event that follows tells the UI
// everything, and an "interrupted — ready for your next message" note on a
// thread that is shutting down would mislead.
func (s *Supervisor) observeTurnEnd(t *Thread) {
	t.mu.Lock()
	if t.turnsInFlight > 0 {
		t.turnsInFlight--
	}
	aborted := t.aborting
	t.aborting = false
	stopping := t.stopping
	t.mu.Unlock()
	if !aborted {
		return
	}
	s.log.Info("agent turn aborted in-band; process stays resident", "thread", t.ID)
	if !stopping {
		s.emitLifecycle(t, "turn_aborted", "interrupted — session kept, ready for your next message")
	}
}

// Compact sends a one-shot summarisation prompt to a live thread and waits
// for the next assistant turn's text. Used by the Hot-Opus exit-compact path
// to capture a summary while the model's KV cache is still warm — the
// summary then seeds the thread's next resume.
//
// Concurrent callers share one compaction: the first to arrive sends the
// prompt; the rest just wait on the same result. This makes the agent.stop
// handler and the akcore shutdown path safely composable — both can call
// Compact() and both block until the summary lands (or the agent dies).
//
// Returns ("", err) on a dead thread, a send failure, ctx cancellation,
// or if the agent process exits before the summary turn arrives.
func (s *Supervisor) Compact(ctx context.Context, threadID, prompt string) (string, error) {
	t := s.thread(threadID)
	if t == nil {
		return "", fmt.Errorf("unknown thread %q", threadID)
	}
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return "", fmt.Errorf("thread %q is not running", threadID)
	}
	hc := t.hotCompact
	starter := hc == nil
	if starter {
		hc = &hotCompact{done: make(chan struct{})}
		t.hotCompact = hc
	}
	t.mu.Unlock()

	if starter {
		defer func() {
			t.mu.Lock()
			if t.hotCompact == hc {
				t.hotCompact = nil
			}
			t.mu.Unlock()
		}()
		if err := s.Send(threadID, prompt, nil); err != nil {
			hc.finish(fmt.Errorf("send compact prompt: %w", err))
		}
	}

	select {
	case <-hc.done:
		return hc.text.String(), hc.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Supervisor) pumpStderr(t *Thread, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 32*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		s.log.Debug("agent stderr", "thread", t.ID, "line", line)
		s.emitSynthetic(t, "_stderr", line)
	}
}

func (s *Supervisor) reap(t *Thread) {
	defer s.reapWG.Done()
	err := t.cmd.Wait()

	t.mu.Lock()
	t.alive = false
	interrupted := t.interrupted
	// Unblock any in-flight Compact waiters: the summary turn isn't coming.
	if t.hotCompact != nil {
		t.hotCompact.finish(fmt.Errorf("agent exited before compact completed"))
	}
	// Likewise any control-request waiters — their responses aren't coming.
	for id, ch := range t.controls {
		delete(t.controls, id)
		ch <- controlOutcome{err: "agent exited before answering"}
	}
	if t.mcpConfig != "" {
		_ = os.Remove(t.mcpConfig)
	}
	t.mu.Unlock()

	// Telemetry: per-tool totals (what filled the context) plus per-thread
	// usage (what we were billed for, in case no `result` event arrived).
	t.meter.Summary()
	t.usage.Summary()

	s.mu.Lock()
	delete(s.threads, t.ID)
	s.mu.Unlock()

	// A user interrupt makes claude exit non-zero (aborted_streaming); report it
	// as an interrupt, not a crash, so the UI shows "stopped (resumable)".
	phase := "exited"
	detail := "exited cleanly"
	switch {
	case interrupted:
		phase = "interrupted"
		detail = "interrupted by user (resumable)"
	case err != nil:
		detail = "exited: " + err.Error()
	}
	s.log.Info("agent thread ended", "thread", t.ID, "phase", phase, "detail", detail)
	s.emitLifecycle(t, phase, detail)
}

func (s *Supervisor) thread(id string) *Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[id]
}

// Running reports whether a thread currently has a live process.
func (s *Supervisor) Running(threadID string) bool {
	t := s.thread(threadID)
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive
}

func (s *Supervisor) emitLifecycle(t *Thread, phase, detail string) {
	s.emitObj(t, map[string]any{"type": "_lifecycle", "phase": phase, "detail": detail})
}

func (s *Supervisor) emitSynthetic(t *Thread, typ, text string) {
	s.emitObj(t, map[string]any{"type": typ, "text": text})
}

// emitObj buffers a synthetic event through the thread's coalescer. Synthetic
// events (_stderr, _lifecycle) are low-frequency and order-significant, so each
// is treated as a flush boundary: it ships with whatever stream events preceded
// it, in order, rather than waiting on the timer.
func (s *Supervisor) emitObj(t *Thread, obj map[string]any) {
	if b, err := json.Marshal(obj); err == nil {
		t.co.add(json.RawMessage(b), true, false)
	}
}
