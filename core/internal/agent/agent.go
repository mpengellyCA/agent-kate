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
		claudeBin: claudeBin,
		log:       log,
		emit:      emit,
		threads:   make(map[string]*Thread),
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
	sessionID   string      // captured from stream-json, for future --resume
	mcpConfig   string      // temp --mcp-config file to clean up on exit
	meter       *toolMeter  // measures tool_result sizes for token-cost telemetry
	usage       *usageMeter // measures per-turn LLM token usage and billed cost
	alive       bool
	pgid        int         // process-group id (== leader pid); signalled by Interrupt
	interrupted bool        // set by Interrupt so reap() reports a user-interrupt
	hotCompact  *hotCompact // when non-nil, the next assistant turn is captured for a summary

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
		sessionID: opts.SessionID,
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

	s.mu.Lock()
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
	if _, err := t.stdin.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("write to agent: %w", err)
	}
	return nil
}

// Stop ends a thread: it closes the input stream and force-kills the process
// if it does not exit promptly.
func (s *Supervisor) Stop(threadID string) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	t.mu.Lock()
	alive := t.alive
	proc := t.cmd.Process
	_ = t.stdin.Close()
	t.mu.Unlock()
	if !alive {
		return nil
	}
	// reap() handles the clean exit; this is only a backstop.
	safe.Go("agent.stopKillBackstop", func() {
		time.Sleep(5 * time.Second)
		t.mu.Lock()
		stillAlive := t.alive
		t.mu.Unlock()
		if stillAlive && proc != nil {
			_ = proc.Kill()
		}
	})
	return nil
}

// Interrupt halts a thread's in-flight turn *immediately* and leaves the
// session resumable. Unlike Stop — the graceful "finish this turn, then quit"
// path used by StopAll and panel close — this aborts generation mid-response so
// no further tokens are billed.
//
// Primary path is in-band: it writes Claude Code's stream-json interrupt
// control_request, then closes stdin so the aborted process exits cleanly.
// A spike against claude 2.1.172 (see docs/plans/04-stop-agent.md) confirmed the
// CLI acks the frame and stops generating within ~1ms, then exits ~180ms after
// stdin close with no signal needed; the persisted session resumes via --resume.
//
// Signals are the reliable fallback: if the process is still alive after a short
// grace, we SIGINT the whole process group (claude + spawned tools), escalating
// to SIGKILL. reap() reports this as a user-interrupt, not a crash.
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
	t.interrupted = true
	pgid := t.pgid
	proc := t.cmd.Process
	// In-band abort, then EOF to let the now-idle process exit cleanly.
	_, werr := t.stdin.Write(append(frame, '\n'))
	_ = t.stdin.Close()
	t.mu.Unlock()
	if werr != nil {
		s.log.Warn("interrupt frame write failed; relying on signal backstop",
			"thread", threadID, "err", werr)
	}

	// Signal backstop: escalate only if the in-band abort + EOF didn't land.
	// Much shorter than Stop's 5s grace because the intent here is immediate.
	safe.Go("agent.interruptBackstop", func() {
		time.Sleep(2 * time.Second)
		if !s.Running(threadID) {
			return
		}
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		}
		time.Sleep(2 * time.Second)
		if !s.Running(threadID) {
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

// StopAll terminates every running thread (used at core shutdown) and blocks
// until each thread's reap() has run to completion. Stop() only closes stdin
// and schedules a 5s backstop kill, so without this join the process could
// exit before a thread's "exited" lifecycle event fires — which is exactly
// what spawns the cold-exit compaction. Waiting here guarantees every such
// compaction has at least been spawned (and registered with its WaitGroup)
// before the caller moves on to drain them.
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
		// Capture the session id wherever it appears, for future --resume.
		var probe struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.SessionID != "" {
			t.mu.Lock()
			t.sessionID = probe.SessionID
			t.mu.Unlock()
		}
		// If a Hot-Opus compaction is in flight, accumulate the assistant
		// text from this event and complete the channel on the next result.
		observeHotCompact(t, raw)
		// Telemetry: where context tokens go (tool outputs) and what we are
		// billed for (per-turn usage). Both observe; neither alters the stream.
		t.meter.Observe(raw)
		t.usage.Observe(raw)
		boundary, dedup := classifyEvent(raw)
		t.co.add(json.RawMessage(raw), boundary, dedup)
	}
	// Drain whatever is still buffered when the stream ends so a trailing
	// partial batch is never stranded behind its timer.
	t.co.flush()
}

// classifyEvent inspects one stream-json event to decide whether it should
// force an immediate coalescer flush (a semantic boundary) and whether it is a
// candidate for byte-identical dedup within a batch.
//
// Boundaries are kept conservative: a `result` (turn end) and any assistant
// turn carrying a tool_use block (the UI renders a tool card and may gate on
// it) flush right away so latency-sensitive UI never waits on the timer.
// Plain assistant text events are dedup candidates because `claude --verbose`
// can repeat an identical partial snapshot; only exact duplicates are dropped,
// so no content is ever lost.
func classifyEvent(raw json.RawMessage) (boundary, dedup bool) {
	var head struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return false, false
	}
	switch head.Type {
	case "result":
		return true, false
	case "assistant":
		for _, blk := range head.Message.Content {
			if blk.Type == "tool_use" {
				return true, false
			}
		}
		return false, true
	}
	return false, false
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
