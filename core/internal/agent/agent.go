// Package agent supervises headless Claude Code processes — the "agent threads"
// that do the work in Agent Kate's harness. Each thread is one `claude` process
// run in streaming-JSON mode so its conversation can be observed live and
// continued with follow-up messages.
package agent

import (
	"bufio"
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
	"time"
)

// EventFunc receives one event for a thread. An event is either a raw
// stream-json object emitted by `claude`, or a synthetic object injected by
// this package — synthetic types begin with an underscore (_stderr,
// _lifecycle) so the UI can tell them apart.
type EventFunc func(threadID string, event json.RawMessage)

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

// NewSupervisor creates a supervisor. emit is invoked for every thread event.
// An empty claudeBin defaults to "claude" (resolved via PATH).
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

	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	sessionID  string      // captured from stream-json, for future --resume
	mcpConfig  string      // temp --mcp-config file to clean up on exit
	meter      *toolMeter  // measures tool_result sizes for token-cost telemetry
	usage      *usageMeter // measures per-turn LLM token usage and billed cost
	alive      bool
	hotCompact *hotCompact // when non-nil, the next assistant turn is captured for a summary
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
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
		// The Cooperation MCP is always allowed so every agent can see and
		// coordinate with its collaborators without a permission prompt.
		"--allowedTools", "mcp__cooperation",
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

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.claudeBin, err)
	}
	t.alive = true

	s.mu.Lock()
	s.threads[t.ID] = t
	s.mu.Unlock()

	// The "started" lifecycle event is emitted by the orchestration layer
	// once the thread id is known to the UI; here we only log.
	s.log.Info("agent process spawned", "thread", t.ID, "dir", opts.WorkDir, "pid", cmd.Process.Pid)

	go s.pumpStdout(t, stdout)
	go s.pumpStderr(t, stderr)
	s.reapWG.Add(1)
	go s.reap(t)

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
	go func() {
		time.Sleep(5 * time.Second)
		t.mu.Lock()
		stillAlive := t.alive
		t.mu.Unlock()
		if stillAlive && proc != nil {
			_ = proc.Kill()
		}
	}()
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
		s.emit(t.ID, json.RawMessage(raw))
	}
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
		s.emitSynthetic(t.ID, "_stderr", line)
	}
}

func (s *Supervisor) reap(t *Thread) {
	defer s.reapWG.Done()
	err := t.cmd.Wait()

	t.mu.Lock()
	t.alive = false
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

	detail := "exited cleanly"
	if err != nil {
		detail = "exited: " + err.Error()
	}
	s.log.Info("agent thread ended", "thread", t.ID, "detail", detail)
	s.emitLifecycle(t.ID, "exited", detail)
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

func (s *Supervisor) emitLifecycle(threadID, phase, detail string) {
	s.emitObj(threadID, map[string]any{"type": "_lifecycle", "phase": phase, "detail": detail})
}

func (s *Supervisor) emitSynthetic(threadID, typ, text string) {
	s.emitObj(threadID, map[string]any{"type": typ, "text": text})
}

func (s *Supervisor) emitObj(threadID string, obj map[string]any) {
	if b, err := json.Marshal(obj); err == nil {
		s.emit(threadID, json.RawMessage(b))
	}
}
