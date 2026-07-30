package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/safe"
)

// EventFunc receives a batch of translated, Claude-shaped events for a thread.
// Identical in shape to agent.EventFunc so the run loop relays both backends
// through one code path.
type EventFunc func(threadID string, events []json.RawMessage)

// PermissionFunc asks the human to approve a gated tool call and returns the
// decision. The run loop wires it to the same broker + permission.requested
// notification the Claude MCP bridge uses, so the UI flow is identical.
type PermissionFunc func(threadID, toolName string, input json.RawMessage) (allow bool)

// MCPServer is one ACP stdio MCP server entry for session/new. Agent Kate
// passes its Cooperation bridge (`akcore mcp ...`) this way — kimi forwards
// stdio MCP servers natively.
type MCPServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []MCPEnv `json:"env"`
}

// MCPEnv is one environment variable in an ACP MCP server entry.
type MCPEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StartOptions configures a new kimi thread. It mirrors agent.StartOptions
// where the backends overlap; provider/cowork/compaction options don't exist
// here because the kimi backend doesn't support them (rejected at agent.start).
type StartOptions struct {
	ID          string             // thread id; generated if empty
	WorkDir     string             // working directory for the agent (a workspace or worktree)
	Prompt      string             // initial user message
	Attachments []agent.Attachment // files attached to the opening message
	SessionID   string             // with Resume: the kimi session to re-attach
	Resume      bool               // true: session/resume an existing kimi session
	Model       string             // kimi model alias (config option "model"); "" = CLI default
	MCPServers  []MCPServer        // forwarded to session/new (the Cooperation bridge)
}

// Thread is one running `kimi acp` process. Its outward surface mirrors
// agent.Thread's role in the run loop: Send/Interrupt/Stop via the supervisor,
// SessionID for persistence.
type Thread struct {
	ID      string
	WorkDir string

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	client      *acpClient
	tr          *translator
	sessionID   string
	alive       bool
	pgid        int  // process-group id (== leader pid); signalled by the interrupt backstop
	interrupted bool // set when the interrupt backstop had to kill; reap() reports a user-interrupt
	cancelling  bool // a session/cancel is in flight; the last prompt completion clears it
	stopping    bool // a Stop is in flight; suppresses turn_aborted and rejects new Sends
	// activePrompts counts session/prompt requests awaiting their response.
	// Normally 0 or 1 (ACP allows one turn per session; kimi rejects overlap),
	// but a rejected overlapping prompt completes independently and must not
	// clear the real turn's in-flight state — hence a counter, not a bool.
	// Interrupt is a no-op at 0: nothing to cancel means nothing to backstop.
	activePrompts int
	logFile     *os.File
}

// SessionID returns the kimi session id assigned by session/new (empty until
// the handshake completes).
func (t *Thread) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Supervisor owns the set of running kimi threads. It mirrors agent.Supervisor
// method-for-method so the handlers can route by backend.
type Supervisor struct {
	kimiBin  string
	log      *slog.Logger
	emit     EventFunc
	perm     PermissionFunc
	eventDir string

	mu      sync.Mutex
	threads map[string]*Thread

	// Interrupt-backstop timings: how long an unacknowledged session/cancel may
	// stay pending before the process group is SIGINTed, and how long after
	// that before SIGKILL. Overridable in tests; the defaults suit real kimi.
	cancelBackstopDelay time.Duration
	cancelKillDelay     time.Duration

	// reapWG tracks every in-flight reap() goroutine; StopAll waits on it so
	// each thread's "exited" lifecycle event has been delivered before shutdown
	// proceeds (same guarantee agent.Supervisor makes).
	reapWG sync.WaitGroup
}

// NewSupervisor creates a kimi supervisor. An empty kimiBin defaults to "kimi"
// (resolved via PATH); an empty eventDir defaults to DefaultEventDir(). perm
// may be nil — permission requests are then always cancelled (denied).
func NewSupervisor(kimiBin string, log *slog.Logger, emit EventFunc, perm PermissionFunc, eventDir string) *Supervisor {
	if kimiBin == "" {
		kimiBin = "kimi"
	}
	if eventDir == "" {
		eventDir = DefaultEventDir()
	}
	return &Supervisor{
		kimiBin:             kimiBin,
		log:                 log,
		emit:                emit,
		perm:                perm,
		eventDir:            eventDir,
		cancelBackstopDelay: 3 * time.Second,
		cancelKillDelay:     2 * time.Second,
		threads:             make(map[string]*Thread),
	}
}

// DefaultEventDir is where per-thread translated-event logs live. Kimi has no
// core-parseable transcript of its own, so these JSONL files are what
// agent.transcript serves for kimi threads.
func DefaultEventDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", "kimi-events")
}

// eventLogPath returns the per-thread translated-event log.
func (s *Supervisor) eventLogPath(threadID string) string {
	return filepath.Join(s.eventDir, threadID+".jsonl")
}

// ReadTranscript returns the translated events logged for a thread from this
// supervisor's own event directory — use this rather than the package-level
// ReadTranscript so a non-default eventDir can never diverge from the handler.
func (s *Supervisor) ReadTranscript(threadID string) ([]json.RawMessage, error) {
	return ReadTranscript(s.eventDir, threadID)
}

// ReadTranscript returns the translated events logged for a kimi thread, in
// order, so the UI can replay the conversation when reopening a dormant
// thread — the kimi counterpart of session.ReadTranscript. Returns nil with
// no error if there is no log yet.
func ReadTranscript(eventDir, threadID string) ([]json.RawMessage, error) {
	if eventDir == "" {
		eventDir = DefaultEventDir()
	}
	f, err := os.Open(filepath.Join(eventDir, threadID+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []json.RawMessage
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		out = append(out, append(json.RawMessage(nil), line...))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Start launches a new kimi thread: spawn `kimi acp`, run the ACP handshake
// (initialize → session/new, or session/resume for a dormant thread), then
// send the opening prompt.
func (s *Supervisor) Start(opts StartOptions) (*Thread, error) {
	cmd := exec.Command(s.kimiBin, "acp")
	cmd.Dir = opts.WorkDir
	// Own process group, like the Claude threads, so the interrupt backstop
	// can signal kimi plus anything it spawned.
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
		id = agent.NewThreadID()
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s acp: %w", s.kimiBin, err)
	}

	t := &Thread{
		ID:      id,
		WorkDir: opts.WorkDir,
		cmd:     cmd,
		stdin:   stdin,
		alive:   true,
		pgid:    cmd.Process.Pid,
	}
	t.client = newACPClient(stdin, s.log)
	t.client.onNotification = func(method string, params json.RawMessage) {
		s.onNotification(t, method, params)
	}
	t.client.onRequest = func(f acpFrame) { s.onAgentRequest(t, f) }
	safe.Go("kimi.acpRead", func() { t.client.readLoop(stdout) })

	// The translated-event log is the thread's transcript; a start failure is
	// logged, never fatal — replay simply degrades to empty. A resumed thread
	// APPENDS: the existing log is the history the UI replays, so truncating
	// here would erase the conversation up to the resume.
	logFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if opts.Resume {
		logFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	if err := os.MkdirAll(s.eventDir, 0o755); err == nil {
		if lf, err := os.OpenFile(s.eventLogPath(id), logFlags, 0o644); err == nil {
			t.logFile = lf
		} else {
			s.log.Warn("kimi event log unavailable", "thread", id, "err", err)
		}
	}

	// The ACP handshake runs synchronously so a failure (kimi missing, not
	// logged in, bad session id) is reported through Start's error return —
	// the caller turns it into an "error" lifecycle event, exactly like a
	// failed Claude spawn. On failure the child is killed and reaped inline:
	// no thread was ever registered, so no "exited" lifecycle event follows.
	if err := s.handshake(t, opts); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.logFile != nil {
			_ = t.logFile.Close()
			t.logFile = nil
		}
		return nil, err
	}

	s.mu.Lock()
	s.threads[t.ID] = t
	s.mu.Unlock()

	s.log.Info("kimi process spawned", "thread", t.ID, "dir", opts.WorkDir,
		"pid", cmd.Process.Pid, "session", t.SessionID())

	safe.Go("kimi.pumpStderr", func() { s.pumpStderr(t, stderr) })
	s.reapWG.Add(1)
	safe.Go("kimi.reap", func() { s.reap(t) })

	// Session start, shaped like claude's init system event — the run loop
	// persists the session id from it and the UI shows the model line.
	s.emitEvents(t, []json.RawMessage{t.tr.initEvent()})

	// A fresh thread gets its opening turn now. A resumed thread has none —
	// it waits for the user's next message (same contract as agent.Start).
	if opts.Prompt != "" || len(opts.Attachments) > 0 {
		if err := s.Send(t.ID, opts.Prompt, opts.Attachments); err != nil {
			s.log.Warn("failed to send initial prompt", "thread", t.ID, "err", err)
		}
	}
	return t, nil
}

// handshake performs initialize → session/new (or session/resume) and applies
// the requested model.
func (s *Supervisor) handshake(t *Thread, opts StartOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := t.client.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		// fs/terminal capabilities are deliberately NOT advertised: kimi then
		// does its own file I/O and shell execution locally instead of
		// reverse-RPCing them back to us.
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "agentkate", "version": "1"},
	}, nil); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}

	mcpServers := opts.MCPServers
	if mcpServers == nil {
		mcpServers = []MCPServer{} // kimi rejects a null mcpServers (-32602)
	}
	sessionParams := map[string]any{
		"cwd":        opts.WorkDir,
		"mcpServers": mcpServers,
	}
	method := "session/new"
	if opts.Resume {
		// session/resume re-attaches WITHOUT replaying history (session/load
		// would) — the UI already replays the transcript itself from the
		// translated-event log, so an ACP-side replay would double every card.
		method = "session/resume"
		sessionParams["sessionId"] = opts.SessionID
	}
	// The full config-option set (model / thinking / mode enumerations, each
	// with values and display names) rides into the translator's init event so
	// the UI can offer real pickers instead of free-text fields.
	var sessionRes struct {
		SessionID     string         `json:"sessionId"`
		ConfigOptions []ConfigOption `json:"configOptions"`
	}
	if err := t.client.call(ctx, method, sessionParams, &sessionRes); err != nil {
		return fmt.Errorf("acp %s: %w", method, err)
	}
	// session/new returns the fresh id; session/resume returns only
	// configOptions, so the id we re-attached to is the one we passed.
	sessionID := sessionRes.SessionID
	if sessionID == "" {
		sessionID = opts.SessionID
	}
	if sessionID == "" {
		return fmt.Errorf("acp %s: no sessionId in response", method)
	}
	// Locked: the read loop is already running and reads sessionID/tr under
	// t.mu (a notification can arrive while the handshake is still applying
	// the model below).
	t.mu.Lock()
	t.sessionID = sessionID
	t.mu.Unlock()

	// Model: empty leaves the CLI's configured default; otherwise set it via
	// the unified config-option dispatcher. A bad alias is a warning, not a
	// failed start — the thread can still run on the default model.
	model := opts.Model
	if model != "" {
		if err := t.client.call(ctx, "session/set_config_option", map[string]any{
			"sessionId": sessionID,
			"configId":  "model",
			"value":     model,
		}, nil); err != nil {
			s.log.Warn("could not set kimi model; using the CLI default",
				"thread", t.ID, "model", model, "err", err)
			model = ""
		}
	}
	for i, co := range sessionRes.ConfigOptions {
		if co.ID != "model" {
			continue
		}
		if model == "" {
			model = co.CurrentValue
		} else {
			// Keep the init event's option set consistent with the model we
			// just applied, so the UI's picker shows the real current value.
			sessionRes.ConfigOptions[i].CurrentValue = model
		}
	}
	t.mu.Lock()
	t.tr = newTranslator(sessionID, model, sessionRes.ConfigOptions)
	t.mu.Unlock()
	return nil
}

// buildPromptContent assembles ACP prompt content blocks from the message text
// and any attachments. Text attachments are inlined exactly as the Claude
// backend inlines them (agent.buildUserContent); images ride as ACP image
// blocks (promptCapabilities.image is true).
func buildPromptContent(text string, attachments []agent.Attachment) []map[string]any {
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
				"type":     "image",
				"data":     a.DataB64,
				"mimeType": a.MediaType,
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return content
}

// Send delivers a user message — text plus any attachments — to a running
// thread. It returns once the prompt request is written; the turn's streamed
// updates and its completion arrive asynchronously.
func (s *Supervisor) Send(threadID, text string, attachments []agent.Attachment) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	t.mu.Lock()
	alive := t.alive
	stopping := t.stopping
	sid := t.sessionID
	if alive && !stopping {
		t.activePrompts++ // Interrupt has something to cancel from here on
	}
	t.mu.Unlock()
	if !alive {
		return fmt.Errorf("thread %q is not running", threadID)
	}
	if stopping {
		return fmt.Errorf("thread %q is stopping", threadID)
	}
	err := t.client.send("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    buildPromptContent(text, attachments),
	}, func(f acpFrame) { s.onPromptDone(t, f) })
	if err != nil {
		t.mu.Lock()
		if t.activePrompts > 0 {
			t.activePrompts--
		}
		t.mu.Unlock()
	}
	return err
}

// onPromptDone completes a turn: the prompt response's stopReason becomes the
// result event. Runs on the ACP read-loop goroutine, after every session/update
// for the turn has already been translated.
func (s *Supervisor) onPromptDone(t *Thread, f acpFrame) {
	// This prompt is over, however it ended — success, error or stream close.
	// Clearing cancelling once no prompt remains in flight (not just on
	// stopReason "cancelled") is what keeps the interrupt backstop from
	// SIGINTing a healthy process when a cancel races the turn's natural
	// completion: ACP treats a cancel of a finished turn as a no-op, so no
	// "cancelled" stop reason ever arrives.
	t.mu.Lock()
	if t.activePrompts > 0 {
		t.activePrompts--
	}
	if t.activePrompts == 0 {
		t.cancelling = false
	}
	tr := t.tr
	stopping := t.stopping
	t.mu.Unlock()

	if f.Error != nil {
		// A stream close means the process died — reap() reports the exit, so
		// don't synthesize a turn result for a turn that isn't coming.
		if isStreamClosed(f.Error) {
			return
		}
		s.log.Warn("kimi prompt failed", "thread", t.ID, "err", f.Error)
		s.emitEvents(t, []json.RawMessage{
			marshalEvent(map[string]any{"type": "_stderr", "text": "prompt: " + f.Error.Error()}),
			marshalEvent(map[string]any{
				"type":       "result",
				"subtype":    "error",
				"is_error":   true,
				"session_id": t.SessionID(),
			}),
		})
		return
	}
	var res struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(f.Result, &res)
	s.emitEvents(t, tr.endTurn())
	// During a graceful Stop the turn_aborted lifecycle event is suppressed:
	// the exited event that follows tells the UI everything, and an
	// "interrupted — ready for your next message" note on a thread that is
	// shutting down would mislead.
	if res.StopReason == "cancelled" && !stopping {
		s.emitLifecycle(t, "turn_aborted", "interrupted — session kept, ready for your next message")
	}
}

// Interrupt cancels the in-flight turn via session/cancel while keeping the
// process resident: the next Send goes to the same session with no resume
// cost — the ACP counterpart of the Claude backend's in-band interrupt. When
// the prompt response returns stopReason "cancelled", onPromptDone emits the
// turn_aborted lifecycle event that resets the UI to idle.
//
// Like the Claude path, a signal backstop covers a hung turn that never acks
// the cancel: SIGINT the process group, escalating to SIGKILL. That kills the
// process, so reap() reports a user-interrupt and the thread goes dormant.
func (s *Supervisor) Interrupt(threadID string) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return nil
	}
	if t.activePrompts == 0 {
		// Nothing in flight to cancel. Sending a cancel anyway would arm the
		// backstop with no prompt response coming to disarm it, and a few
		// seconds later it would kill a perfectly idle resident process.
		t.mu.Unlock()
		return nil
	}
	t.cancelling = true
	sid := t.sessionID
	pgid := t.pgid
	proc := t.cmd.Process
	t.mu.Unlock()

	if err := t.client.notify("session/cancel", map[string]any{"sessionId": sid}); err != nil {
		s.log.Warn("session/cancel write failed; relying on signal backstop",
			"thread", threadID, "err", err)
	}

	safe.Go("kimi.interruptBackstop", func() {
		time.Sleep(s.cancelBackstopDelay)
		if !s.cancelPending(threadID) {
			return // clean cancel — process stays alive
		}
		s.log.Info("cancel not acked; escalating to signals", "thread", threadID)
		t.mu.Lock()
		t.interrupted = true // reap() will report a user-interrupt now
		t.mu.Unlock()
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		}
		time.Sleep(s.cancelKillDelay)
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

// cancelPending reports whether a thread still has an unacknowledged cancel in
// flight (the escalation backstop's trigger condition).
func (s *Supervisor) cancelPending(threadID string) bool {
	t := s.thread(threadID)
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive && t.cancelling
}

// Stop ends a thread. On an idle thread it closes the input stream — kimi
// exits when its stdin ends — with a kill backstop for a process that doesn't.
// On a busy thread (a prompt in flight) closing stdin alone is not graceful:
// the turn would be cut off mid-stream. So a busy Stop first cancels the turn
// via session/cancel (the same interrupt the interactive CLI's Esc maps to),
// waits — bounded — for the prompt response to land, and only then closes
// stdin. Stop returns immediately in both cases; the graceful sequencing runs
// on a background goroutine — the same contract agent.Supervisor.Stop makes.
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
	busy := t.activePrompts > 0
	t.mu.Unlock()
	if busy {
		safe.Go("kimi.gracefulStop", func() { s.abortThenClose(t) })
	} else {
		s.closeStdin(t)
	}
	return nil
}

// closeStdin closes the thread's input stream so kimi exits, and arms the kill
// backstop for a process that doesn't. reap() handles the clean exit; the
// backstop only fires if the process lingers.
func (s *Supervisor) closeStdin(t *Thread) {
	t.mu.Lock()
	proc := t.cmd.Process
	_ = t.stdin.Close()
	t.mu.Unlock()
	safe.Go("kimi.stopKillBackstop", func() {
		time.Sleep(5 * time.Second)
		t.mu.Lock()
		stillAlive := t.alive
		t.mu.Unlock()
		if stillAlive && proc != nil {
			_ = proc.Kill()
		}
	})
}

// abortThenClose is the busy half of Stop: cancel the in-flight turn, wait for
// its prompt response (ACP's turn boundary), then close stdin. The wait is
// bounded just past Interrupt's own escalation window: if the cancel never
// acks (a hung turn), the interrupt backstop has already SIGINT/SIGKILLed the
// process by then and the close is a no-op.
func (s *Supervisor) abortThenClose(t *Thread) {
	if err := s.Interrupt(t.ID); err != nil {
		s.log.Warn("stop: session/cancel failed; closing stdin directly",
			"thread", t.ID, "err", err)
		s.closeStdin(t)
		return
	}
	deadline := time.Now().Add(s.cancelBackstopDelay + s.cancelKillDelay + time.Second)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		done := !t.alive || t.activePrompts == 0
		t.mu.Unlock()
		if done {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.closeStdin(t)
}

// StopAll terminates every running thread (used at core shutdown) and blocks
// until each thread's reap() has run to completion — the same shutdown
// ordering guarantee agent.Supervisor.StopAll makes.
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

func (s *Supervisor) thread(id string) *Thread {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[id]
}

// onNotification translates a session/update notification and emits the
// resulting events. Unknown notifications are ignored.
func (s *Supervisor) onNotification(t *Thread, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	t.mu.Lock()
	tr := t.tr
	t.mu.Unlock()
	if tr == nil {
		return // pre-handshake chatter (available_commands etc.)
	}
	s.emitEvents(t, tr.update(params))
}

// onAgentRequest answers an agent→client request. Only
// session/request_permission is supported; fs/* and terminal/* were never
// advertised, so kimi performs file I/O and shell execution locally.
func (s *Supervisor) onAgentRequest(t *Thread, f acpFrame) {
	if f.Method != "session/request_permission" {
		t.client.respondError(f.ID, codeMethodNotFound, "method not found: "+f.Method)
		return
	}
	var p struct {
		ToolCall struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(f.Params, &p); err != nil {
		t.client.respondError(f.ID, codeInvalidParams, err.Error())
		return
	}

	t.mu.Lock()
	tr := t.tr
	t.mu.Unlock()
	name, input := "", json.RawMessage("{}")
	if tr != nil {
		if n, in := tr.toolForPermission(p.ToolCall.ToolCallID); n != "" {
			name, input = n, in
		}
	}
	if name == "" {
		name = toolName(p.ToolCall.Title, p.ToolCall.Kind)
	}

	allow := false
	if s.perm != nil {
		allow = s.perm(t.ID, name, input)
	}

	// Map the boolean decision onto kimi's option set by KIND — option ids are
	// kimi-specific ("approve_once", "reject"), the kinds are spec-stable.
	want, fallback := "reject_once", "reject_always"
	if allow {
		want, fallback = "allow_once", "allow_always"
	}
	optionID := ""
	for _, o := range p.Options {
		if o.Kind == want {
			optionID = o.OptionID
			break
		}
		if o.Kind == fallback && optionID == "" {
			optionID = o.OptionID
		}
	}
	if optionID == "" {
		t.client.respond(f.ID, map[string]any{
			"outcome": map[string]any{"outcome": "cancelled"},
		})
		return
	}
	t.client.respond(f.ID, map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": optionID},
	})
}

func (s *Supervisor) pumpStderr(t *Thread, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 32*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		s.log.Debug("kimi stderr", "thread", t.ID, "line", line)
		s.emitSynthetic(t, "_stderr", line)
	}
}

func (s *Supervisor) reap(t *Thread) {
	defer s.reapWG.Done()
	err := t.cmd.Wait()

	t.mu.Lock()
	t.alive = false
	interrupted := t.interrupted
	t.mu.Unlock()

	s.mu.Lock()
	delete(s.threads, t.ID)
	s.mu.Unlock()

	phase := "exited"
	detail := "exited cleanly"
	switch {
	case interrupted:
		phase = "interrupted"
		detail = "interrupted by user (resumable)"
	case err != nil:
		detail = "exited: " + err.Error()
	}
	s.log.Info("kimi thread ended", "thread", t.ID, "phase", phase, "detail", detail)
	// Emit before closing the log so the exit note lands in the transcript,
	// then nil the handle so any straggler (a late stderr line) skips the
	// write instead of hitting a closed file.
	s.emitLifecycle(t, phase, detail)
	t.mu.Lock()
	if t.logFile != nil {
		_ = t.logFile.Close()
		t.logFile = nil
	}
	t.mu.Unlock()
}

// emitEvents logs and relays a batch of translated events. Every translated
// event is appended to the thread's JSONL log (its transcript) before relay.
func (s *Supervisor) emitEvents(t *Thread, events []json.RawMessage) {
	if len(events) == 0 {
		return
	}
	t.mu.Lock()
	if t.logFile != nil {
		for _, ev := range events {
			_, _ = t.logFile.Write(append(ev, '\n'))
		}
	}
	t.mu.Unlock()
	s.emit(t.ID, events)
}

func (s *Supervisor) emitLifecycle(t *Thread, phase, detail string) {
	s.emitEvents(t, []json.RawMessage{
		marshalEvent(map[string]any{"type": "_lifecycle", "phase": phase, "detail": detail}),
	})
}

func (s *Supervisor) emitSynthetic(t *Thread, typ, text string) {
	s.emitEvents(t, []json.RawMessage{
		marshalEvent(map[string]any{"type": typ, "text": text}),
	})
}
