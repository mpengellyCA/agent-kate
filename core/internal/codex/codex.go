// Package codex supervises Codex CLI app-server processes for Agent Kate.
//
// Codex's durable, interactive interface is `codex app-server --stdio`, not
// `codex exec`: the app server owns a persisted thread and accepts many turns
// over JSON-RPC.  This package gives it the same small supervisor surface used
// by the other harnesses and translates its item notifications into the
// Claude-shaped events consumed by Agent Kate's existing renderer.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/fsperm"
	"agentkate/internal/safe"
)

// EventFunc receives batches in the same Claude-shaped stream-json dialect as
// agent.EventFunc.  Raw app-server frames are deliberately not leaked: they
// are a versioned protocol, while these events are the stable UI contract.
type EventFunc func(threadID string, events []json.RawMessage)

// PermissionFunc sends an app-server approval request through Agent Kate's
// shared human-approval flow. Returning false is a denial; it is deliberately
// the safe default when no UI is available.
type PermissionFunc func(threadID, toolName string, input json.RawMessage) bool

// StartOptions describes one Codex thread.  Config is passed verbatim to the
// app-server's thread/start (or resume/fork) config field, for the adapter to
// use for Codex-specific configuration without growing this neutral package.
type StartOptions struct {
	ID                    string
	WorkDir               string
	Prompt                string
	Attachments           []agent.Attachment
	SessionID             string
	Resume                bool
	ForkSession           bool
	Model                 string
	Effort                string
	ApprovalPolicy        string // untrusted, on-request, never
	Sandbox               string // read-only, workspace-write, danger-full-access
	DeveloperInstructions string
	Config                map[string]any
	Env                   map[string]string
}

// Model is one app-server model/list entry relevant to a picker.
type Model struct {
	ID      string
	Name    string
	Efforts []string
}

// Thread is one app-server process containing one active Codex thread.
type Thread struct {
	ID      string
	WorkDir string

	mu             sync.Mutex
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	rpc            *rpcClient
	sessionID      string
	model          string
	effort         string
	approvalPolicy string
	sandbox        string
	alive          bool
	stopping       bool
	activeTurnID   string
	// completedTurns bridges the JSON-RPC response/notification race: app-server
	// can emit turn/completed immediately after the turn/start response, and the
	// read loop may observe that notification before Send has recorded its id as
	// active. The first later Send-side check consumes the marker instead of
	// resurrecting a completed turn as permanently busy.
	completedTurns map[string]bool
	interrupted    bool
	logFile        *os.File
	text           map[string]*strings.Builder // item id -> streaming text
	stdoutDrained  chan struct{}
	stderrDrained  chan struct{}
}

func (t *Thread) SessionID() string { t.mu.Lock(); defer t.mu.Unlock(); return t.sessionID }
func (t *Thread) Model() string     { t.mu.Lock(); defer t.mu.Unlock(); return t.model }
func (t *Thread) Effort() string    { t.mu.Lock(); defer t.mu.Unlock(); return t.effort }
func (t *Thread) ApprovalPolicy() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.approvalPolicy
}

// Supervisor owns app-server children.  One child per Agent Kate thread is
// intentional: it gives every thread its own process environment and makes
// Stop/interrupt semantics match the existing backends.
type Supervisor struct {
	codexBin   string
	log        *slog.Logger
	emit       EventFunc
	eventDir   string
	permission PermissionFunc

	// Kept test-overridable, matching the other resident supervisors: an
	// interrupt is only useful if a child that stopped reading stdin cannot
	// hold the escape path hostage.
	interruptBackstopDelay time.Duration
	interruptKillDelay     time.Duration

	mu      sync.Mutex
	threads map[string]*Thread
}

// SetPermissionFunc wires this harness into the core's one human approval
// broker. It is set at startup, before any threads can be launched.
func (s *Supervisor) SetPermissionFunc(f PermissionFunc) { s.permission = f }

// NewSupervisor constructs a supervisor.  Empty codexBin resolves to codex on
// PATH; its translated event log is kept privately beneath XDG_DATA_HOME.
func NewSupervisor(codexBin string, log *slog.Logger, emit EventFunc) *Supervisor {
	if codexBin == "" {
		codexBin = "codex"
	}
	return &Supervisor{codexBin: codexBin, log: log, emit: emit,
		eventDir: DefaultEventDir(), threads: make(map[string]*Thread),
		interruptBackstopDelay: 3 * time.Second, interruptKillDelay: 2 * time.Second}
}

func DefaultEventDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "agentkate", "codex-events")
}

func (s *Supervisor) eventLogPath(id string) string { return filepath.Join(s.eventDir, id+".jsonl") }

// ReadTranscript returns the adapter's durable translated history.  A missing
// log is normal for a thread created outside Agent Kate.
func (s *Supervisor) ReadTranscript(id string) ([]json.RawMessage, error) {
	f, err := os.Open(s.eventLogPath(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		if json.Valid(b) {
			out = append(out, append(json.RawMessage(nil), b...))
		}
	}
	return out, sc.Err()
}

func (s *Supervisor) DeleteTranscript(id string) error {
	if id == "" {
		return nil
	}
	err := os.Remove(s.eventLogPath(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Start initializes the JSON-RPC connection then creates, resumes, or forks a
// Codex thread.  Its opening message is sent as the first turn after the
// server confirms the thread, so every prompt uses the same turn/start path.
func (s *Supervisor) Start(opts StartOptions) (*Thread, error) {
	if opts.WorkDir == "" {
		return nil, errors.New("codex start: work directory is required")
	}
	id := opts.ID
	if id == "" {
		id = agent.NewThreadID()
	}
	cmd := exec.Command(s.codexBin, "app-server", "--stdio")
	cmd.Dir = opts.WorkDir
	cmd.Env = agent.ApplyEnvOverlay(os.Environ(), opts.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex stdin pipe: %w", err)
	}
	// Do not use Cmd.StdoutPipe here. Cmd.Wait closes an StdoutPipe itself,
	// which can discard a final app-server frame racing process exit. We own
	// these pipes so reap can wait for both readers before it writes `exited`
	// or closes Codex's translated transcript (the F24/F51 tail contract).
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("codex stdout pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("codex stderr pipe: %w", err)
	}
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutW.Close()
		_ = stderr.Close()
		_ = stderrW.Close()
		return nil, fmt.Errorf("start %s app-server: %w", s.codexBin, err)
	}
	// The child inherited its own descriptors. Closing ours means EOF reaches
	// the readers once the child exits, without having Cmd.Wait tear the read
	// side out from underneath a last JSON-RPC frame.
	_ = stdoutW.Close()
	_ = stderrW.Close()

	t := &Thread{ID: id, WorkDir: opts.WorkDir, cmd: cmd, stdin: stdin, alive: true,
		text: make(map[string]*strings.Builder), completedTurns: make(map[string]bool),
		stdoutDrained: make(chan struct{}), stderrDrained: make(chan struct{})}
	t.rpc = newRPCClient(stdin, s.log)
	t.rpc.onNotification = func(method string, params json.RawMessage) { s.onNotification(t, method, params) }
	t.rpc.onRequest = func(requestID json.RawMessage, method string, params json.RawMessage) {
		s.onRequest(t, requestID, method, params)
	}
	go func() { defer close(t.stdoutDrained); t.rpc.readLoop(stdout) }()
	go func() { defer close(t.stderrDrained); s.pumpStderr(t, stderr) }()

	// No event emitted before this succeeds: callers turn an error into their
	// normal launch failure lifecycle event.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var init map[string]any
	if err := t.rpc.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "Agent Kate", "version": "1"},
		"capabilities": map[string]any{},
	}, &init); err != nil {
		s.discardFailedStart(t)
		return nil, fmt.Errorf("codex initialize: %w", err)
	}
	if err := t.rpc.notify("initialized", map[string]any{}); err != nil {
		s.discardFailedStart(t)
		return nil, fmt.Errorf("codex initialized: %w", err)
	}

	params := startParams(opts)
	method := "thread/start"
	if opts.Resume {
		method = "thread/resume"
		params["threadId"] = opts.SessionID
		if opts.ForkSession {
			method = "thread/fork"
		}
	}
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"thread"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoningEffort"`
		ApprovalPolicy  string `json:"approvalPolicy"`
	}
	if err := t.rpc.call(ctx, method, params, &response); err != nil {
		s.discardFailedStart(t)
		return nil, fmt.Errorf("codex %s: %w", method, err)
	}
	if response.Thread.ID == "" {
		s.discardFailedStart(t)
		return nil, fmt.Errorf("codex %s: response has no thread id", method)
	}
	t.mu.Lock()
	t.sessionID = response.Thread.ID
	t.model = first(response.Model, response.Thread.Model, opts.Model)
	// Reasoning effort is a turn/start override in the current app-server
	// protocol, not a thread/start field. Preserve the requested value here so
	// the opening Send applies it even when thread/start reports the account's
	// previous default effort.
	t.effort = first(opts.Effort, response.ReasoningEffort)
	t.approvalPolicy = first(response.ApprovalPolicy, opts.ApprovalPolicy)
	t.sandbox = first(opts.Sandbox, "workspace-write")
	t.mu.Unlock()
	if err := s.openLog(t, opts.Resume); err != nil {
		s.logf("codex event log unavailable", "thread", id, "err", err)
	}

	s.mu.Lock()
	if _, exists := s.threads[id]; exists {
		s.mu.Unlock()
		s.discardFailedStart(t)
		return nil, fmt.Errorf("thread %q already running", id)
	}
	s.threads[id] = t
	s.mu.Unlock()
	s.emitEvent(t, event(map[string]any{"type": "system", "subtype": "init", "session_id": response.Thread.ID, "model": t.Model()}))
	go s.reap(t)
	if opts.Prompt != "" || len(opts.Attachments) > 0 {
		if err := s.Send(id, opts.Prompt, opts.Attachments); err != nil {
			s.Stop(id)
			return nil, err
		}
	}
	return t, nil
}

func startParams(opts StartOptions) map[string]any {
	p := map[string]any{"cwd": opts.WorkDir}
	if opts.Model != "" {
		p["model"] = opts.Model
	}
	if opts.ApprovalPolicy != "" {
		p["approvalPolicy"] = opts.ApprovalPolicy
	}
	if opts.Sandbox != "" {
		p["sandbox"] = opts.Sandbox
	}
	if opts.DeveloperInstructions != "" {
		p["developerInstructions"] = opts.DeveloperInstructions
	}
	if len(opts.Config) > 0 {
		p["config"] = opts.Config
	}
	return p
}

func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// Send sends a normal user turn. Text attachments are inlined; Codex's
// app-server image input needs a local path/URL, so data-only image attachments
// are described textually instead of pretending they were attached.
func (s *Supervisor) Send(id, text string, atts []agent.Attachment) error {
	t, err := s.thread(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	stopping := t.stopping
	sessionID, model, effort, approvalPolicy, sandbox :=
		t.sessionID, t.model, t.effort, t.approvalPolicy, t.sandbox
	t.mu.Unlock()
	if stopping {
		return fmt.Errorf("codex thread %q is stopping", id)
	}
	input := []map[string]any{{"type": "text", "text": text}}
	for _, a := range atts {
		switch a.Kind {
		case "text":
			input = append(input, map[string]any{"type": "text", "text": fmt.Sprintf("Attached file %q:\n%s", a.Name, a.Text)})
		case "image":
			if a.Path != "" {
				input = append(input, map[string]any{"type": "localImage", "path": a.Path})
			} else {
				input = append(input, map[string]any{"type": "text", "text": fmt.Sprintf("An image attachment named %q was supplied.", a.Name)})
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	// The current app-server API applies model, effort and approval-policy
	// overrides on turn/start, and retains them for later turns. This is also
	// how a picker change made while a prior turn was active takes effect: it
	// is queued here for the next action rather than sent to the obsolete
	// thread/settings/update endpoint.
	params := map[string]any{"threadId": sessionID, "input": input}
	if model != "" {
		params["model"] = model
	}
	if effort != "" {
		params["effort"] = effort
	}
	if approvalPolicy != "" {
		params["approvalPolicy"] = approvalPolicy
	}
	if sandbox != "" {
		params["sandbox"] = sandbox
	}
	if err := t.rpc.call(ctx, "turn/start", params, &out); err != nil {
		return fmt.Errorf("codex turn/start: %w", err)
	}
	t.mu.Lock()
	if t.completedTurns != nil && t.completedTurns[out.Turn.ID] {
		delete(t.completedTurns, out.Turn.ID)
	} else {
		t.activeTurnID = out.Turn.ID
	}
	t.mu.Unlock()
	return nil
}

func (s *Supervisor) Interrupt(id string) error {
	t, err := s.thread(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	turnID, sessionID := t.activeTurnID, t.sessionID
	pgid := 0
	if t.cmd != nil && t.cmd.Process != nil {
		pgid = t.cmd.Process.Pid
	}
	t.mu.Unlock()
	if turnID == "" {
		return nil
	}
	// Arm escalation BEFORE attempting the JSON-RPC write. A stopped child can
	// have a full stdin pipe, so putting this recovery behind call() would make
	// Escape as wedged as the agent (F9/F52 sibling rule).
	safe.Go("codex.interruptBackstop", func() {
		time.Sleep(s.interruptBackstopDelay)
		if !s.cancelPending(id, t, turnID) {
			return
		}
		t.mu.Lock()
		t.interrupted = true
		t.mu.Unlock()
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		}
		time.Sleep(s.interruptKillDelay)
		if s.cancelPending(id, t, turnID) && pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	})
	// The write/response must never park the UI call. writeFrame is bounded and
	// latched, while this goroutine only records failure for diagnostics; the
	// signal backstop owns recovery.
	safe.Go("codex.interruptRequest", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := t.rpc.call(ctx, "turn/interrupt", map[string]any{"threadId": sessionID, "turnId": turnID}, nil); err != nil {
			s.logf("codex turn/interrupt failed; relying on signal backstop", "thread", id, "err", err)
		}
	})
	return nil
}

// cancelPending makes an interrupt backstop specific to the exact old thread
// and turn it was armed for. A resumed thread may reuse Agent Kate's id; a
// stale process-group signal must never hit that newer process.
func (s *Supervisor) cancelPending(id string, want *Thread, turnID string) bool {
	s.mu.Lock()
	t := s.threads[id]
	s.mu.Unlock()
	if t != want {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive && t.activeTurnID == turnID && turnID != ""
}

func (s *Supervisor) Stop(id string) error {
	t, err := s.thread(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if t.stopping {
		t.mu.Unlock()
		return nil
	}
	t.stopping = true
	busy := t.activeTurnID != ""
	t.mu.Unlock()
	if !busy {
		return t.kill()
	}
	// A busy process gets its in-band cancellation/result chance before stdin
	// closes. This preserves the final transcript turn on an orderly stop; the
	// deadline still makes shutdown progress if the CLI never acknowledges.
	_ = s.Interrupt(id)
	safe.Go("codex.stopAfterInterrupt", func() {
		deadline := time.Now().Add(s.interruptBackstopDelay + s.interruptKillDelay + time.Second)
		for time.Now().Before(deadline) {
			t.mu.Lock()
			done := t.activeTurnID == "" || !t.alive
			t.mu.Unlock()
			if done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = t.kill()
	})
	return nil
}
func (s *Supervisor) Running(id string) bool {
	s.mu.Lock()
	t := s.threads[id]
	s.mu.Unlock()
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.alive
}
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
}

// SetOption queues a new app-server setting for the next turn. Codex 0.146
// exposes model, effort and approvalPolicy on turn/start (where they become
// defaults for later turns); its generated protocol schema deliberately has no
// thread/settings/update request. Keeping this state in the resident thread
// makes Agent Kate's existing live picker mean exactly "next action", rather
// than claiming an update the server will reject.
func (s *Supervisor) SetOption(id, option, value string) (string, error) {
	t, err := s.thread(id)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	stopping, alive := t.stopping, t.alive
	t.mu.Unlock()
	if stopping || !alive {
		return "", fmt.Errorf("codex thread %q is not accepting option changes", id)
	}
	if value == "" {
		return "", fmt.Errorf("codex option %q needs a concrete value", option)
	}
	t.mu.Lock()
	switch option {
	case "model":
		t.model = value
	case "effort":
		t.effort = value
	case "permissionMode":
		t.approvalPolicy = value
	case "sandboxMode":
		if value != "read-only" && value != "workspace-write" && value != "danger-full-access" {
			t.mu.Unlock()
			return "", fmt.Errorf("unknown Codex sandbox mode %q", value)
		}
		t.sandbox = value
	default:
		t.mu.Unlock()
		return "", fmt.Errorf("unknown codex option %q", option)
	}
	t.mu.Unlock()
	return value, nil
}

// DiscoverModels returns the app-server's live model catalogue.
func (s *Supervisor) DiscoverModels(ctx context.Context) ([]Model, error) {
	// Do not call Start: that would create a real persisted Codex thread just
	// to populate the model picker.
	cmd := exec.Command(s.codexBin, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	rpc := newRPCClient(stdin, s.log)
	go rpc.readLoop(stdout)
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := rpc.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "Agent Kate", "version": "1"},
		"capabilities": map[string]any{},
	}, nil); err != nil {
		return nil, fmt.Errorf("codex initialize: %w", err)
	}
	if err := rpc.notify("initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("codex initialized: %w", err)
	}
	var result struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Efforts     []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
		NextCursor string `json:"nextCursor"`
	}
	// App-server model/list is cursor-paginated. The contemporary catalogue is
	// small, but honouring the continuation is what keeps an account with many
	// configured providers from silently losing models in Agent Kate.
	const maxModelPages = 20
	out := make([]Model, 0)
	cursor := ""
	for page := 0; page < maxModelPages; page++ {
		result = struct {
			Data []struct {
				ID          string `json:"id"`
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
				Efforts     []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}{}
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := rpc.call(ctx, "model/list", params, &result); err != nil {
			return nil, err
		}
		for _, m := range result.Data {
			efforts := make([]string, 0, len(m.Efforts))
			for _, e := range m.Efforts {
				if e.ReasoningEffort != "" {
					efforts = append(efforts, e.ReasoningEffort)
				}
			}
			id := first(m.ID, m.Model)
			if id != "" {
				out = append(out, Model{ID: id, Name: first(m.DisplayName, id), Efforts: efforts})
			}
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return out, nil
}

func (s *Supervisor) thread(id string) (*Thread, error) {
	s.mu.Lock()
	t := s.threads[id]
	s.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("codex thread %q is not running", id)
	}
	return t, nil
}

// discardFailedStart always reaps a child that failed before it was admitted
// to the live-thread map. Otherwise an initialize/session failure would leak a
// zombie app-server outside StopAll's ownership.
func (s *Supervisor) discardFailedStart(t *Thread) {
	_ = t.kill()
	safe.Go("codex.failedStartReap", func() { _ = t.cmd.Wait() })
}

func (t *Thread) kill() error {
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return nil
	}
	t.stopping = true
	cmd := t.cmd
	t.mu.Unlock()
	_ = t.stdin.Close()
	if cmd.Process != nil {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	return nil
}

func (s *Supervisor) reap(t *Thread) {
	err := t.cmd.Wait()
	// Wait first, then let the owned output readers reach genuine EOF. The
	// order is load-bearing: closing the translated JSONL log before a tail
	// `item/completed` / `turn/completed` frame arrives loses the only replay
	// Agent Kate owns for this Codex thread (F51).
	end := time.Now().Add(5 * time.Second)
	for _, drained := range []chan struct{}{t.stdoutDrained, t.stderrDrained} {
		if drained == nil {
			continue
		}
		select {
		case <-drained:
		case <-time.After(time.Until(end)):
			s.logf("codex output pump still running at reap; proceeding without its tail", "thread", t.ID)
		}
	}
	t.rpc.failAll(errStreamClosed)
	t.mu.Lock()
	t.alive = false
	stopping := t.stopping
	interrupted := t.interrupted
	t.mu.Unlock()
	s.mu.Lock()
	if s.threads[t.ID] == t {
		delete(s.threads, t.ID)
	}
	s.mu.Unlock()
	detail := "exited cleanly"
	if err != nil && !stopping {
		detail = err.Error()
	}
	phase := "exited"
	if interrupted {
		phase = "interrupted"
	} else if !stopping && err != nil {
		phase = "error"
	}
	// Lifecycle is itself part of the durable transcript. Emit it before
	// closing the log, after both output readers have stopped.
	s.emitEvent(t, event(map[string]any{"type": "_lifecycle", "phase": phase, "detail": detail}))
	t.mu.Lock()
	if t.logFile != nil {
		_ = t.logFile.Close()
		t.logFile = nil
	}
	t.mu.Unlock()
}

func (s *Supervisor) openLog(t *Thread, appendLog bool) error {
	if err := fsperm.MkdirAll(s.eventDir); err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendLog {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(s.eventLogPath(t.ID), flags, fsperm.FileMode)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.logFile = f
	t.mu.Unlock()
	return nil
}

func (s *Supervisor) emitEvent(t *Thread, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	t.mu.Lock()
	if t.logFile != nil {
		_, _ = t.logFile.Write(append(raw, '\n'))
	}
	t.mu.Unlock()
	if s.emit != nil {
		s.emit(t.ID, []json.RawMessage{raw})
	}
}
func (s *Supervisor) logf(msg string, args ...any) {
	if s.log != nil {
		s.log.Warn(msg, args...)
	}
}

func (s *Supervisor) pumpStderr(t *Thread, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 256*1024)
	for sc.Scan() {
		s.emitEvent(t, event(codexStderrEvent(sc.Text())))
	}
}

// codexStderrEvent turns Codex's terminal-oriented tracing output into a
// durable transcript diagnostic. In particular, a tool-router failure is a
// failed tool call, not Agent Kate or Bash output; retaining that distinction
// lets the UI label it truthfully. Unknown stderr remains visible as a Codex
// CLI diagnostic rather than being discarded.
func codexStderrEvent(line string) map[string]any {
	text := stripTerminalControls(line)
	ev := map[string]any{
		"type":     "_stderr",
		"source":   "Codex CLI",
		"severity": "error", // stderr retained its historical error treatment
		"text":     text,
	}
	severity, component, message := parseCodexDiagnostic(text)
	if severity != "" {
		ev["severity"] = severity
	}
	if component != "" {
		ev["component"] = component
	}
	if message != "" {
		ev["text"] = message
	}
	if tool := toolRouterFailure(component, message); tool != "" {
		ev["tool"] = tool
		// The tool is already named in the heading, so leave the useful reason
		// in the body rather than making people read it twice.
		ev["text"] = strings.TrimSpace(strings.TrimPrefix(message, tool))
	}
	return ev
}

// stripTerminalControls removes the ANSI CSI/OSC sequences Codex's tracing
// subscriber puts on stderr. It operates on bytes so ordinary UTF-8 passes
// through unchanged while the C0 controls that have no meaning in a transcript
// are dropped.
func stripTerminalControls(text string) string {
	var clean strings.Builder
	clean.Grow(len(text))
	for i := 0; i < len(text); {
		if text[i] == 0x1b {
			i++
			if i == len(text) {
				break
			}
			switch text[i] {
			case '[': // CSI: parameter/intermediate bytes, then a final byte
				i++
				for i < len(text) {
					c := text[i]
					i++
					if c >= 0x40 && c <= 0x7e {
						break
					}
				}
			case ']': // OSC, terminated by BEL or ST (ESC backslash)
				i++
				for i < len(text) {
					if text[i] == 0x07 {
						i++
						break
					}
					if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			default: // two-byte ESC command
				i++
			}
			continue
		}
		if text[i] >= 0x20 || text[i] == '\t' {
			clean.WriteByte(text[i])
		}
		i++
	}
	return clean.String()
}

// parseCodexDiagnostic understands both rust-tracing forms Codex has used:
// "TIME ERROR component: message" and "TIME ERROR [component] message".
// It intentionally returns empty values for an unfamiliar line, which remains
// a plainly labelled harness diagnostic instead of being misclassified.
func parseCodexDiagnostic(text string) (severity, component, message string) {
	fields := strings.Fields(text)
	for i, field := range fields {
		if i > 2 {
			break
		}
		switch strings.ToLower(field) {
		case "trace", "debug", "info", "warn", "warning", "error":
			severity = strings.ToLower(field)
			if severity == "warning" {
				severity = "warn"
			}
			if i+1 >= len(fields) {
				return severity, "", ""
			}
			component = strings.TrimSuffix(fields[i+1], ":")
			component = strings.Trim(component, "[]")
			if component == "" {
				return severity, "", ""
			}
			message = strings.TrimSpace(strings.Join(fields[i+2:], " "))
			message = strings.TrimPrefix(message, "error=")
			return severity, component, message
		}
	}
	return "", "", ""
}

func toolRouterFailure(component, message string) string {
	if !strings.Contains(component, "tools::router") &&
		!strings.Contains(component, "tools.router") {
		return ""
	}
	words := strings.Fields(message)
	if len(words) < 2 {
		return ""
	}
	failed := strings.TrimSuffix(words[1], ":") == "failed" ||
		(len(words) >= 3 && words[1] == "verification" &&
			strings.TrimSuffix(words[2], ":") == "failed")
	if !failed || !isToolName(words[0]) {
		return ""
	}
	return words[0]
}

func isToolName(name string) bool {
	for i, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '-' || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return name != ""
}

func (s *Supervisor) onNotification(t *Thread, method string, raw json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Delta    string `json:"delta"`
		ItemID   string `json:"itemId"`
		Item     struct {
			ID               string          `json:"id"`
			Type             string          `json:"type"`
			Text             string          `json:"text"`
			Command          string          `json:"command"`
			Status           string          `json:"status"`
			AggregatedOutput string          `json:"aggregatedOutput"`
			Tool             string          `json:"tool"`
			Server           string          `json:"server"`
			Arguments        json.RawMessage `json:"arguments"`
		} `json:"item"`
		Turn struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
		TokenUsage struct {
			Total  int64 `json:"total"`
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	switch method {
	case "item/agentMessage/delta":
		t.mu.Lock()
		itemID := first(p.ItemID, p.Item.ID)
		b := t.text[itemID]
		if b == nil {
			b = &strings.Builder{}
			t.text[itemID] = b
		}
		b.WriteString(p.Delta)
		t.mu.Unlock()
	case "item/completed":
		s.handleItemCompleted(t, p.Item)
	case "turn/completed":
		turnID := first(p.TurnID, p.Turn.ID)
		t.mu.Lock()
		if turnID == "" || turnID == t.activeTurnID {
			t.activeTurnID = ""
		} else {
			// See Thread.completedTurns. Keep the guard bounded even if a
			// malformed app-server floods unknown completion notifications.
			if t.completedTurns == nil {
				t.completedTurns = make(map[string]bool)
			}
			if len(t.completedTurns) >= 64 {
				clear(t.completedTurns)
			}
			t.completedTurns[turnID] = true
		}
		t.mu.Unlock()
		result := map[string]any{"type": "result", "subtype": "success", "session_id": t.SessionID()}
		if p.Turn.Status != "" && p.Turn.Status != "completed" {
			result["subtype"] = "error"
			result["error"] = p.Turn.Status
		}
		s.emitEvent(t, event(result))
	case "thread/tokenUsage/updated":
		if p.TokenUsage.Total > 0 {
			s.emitEvent(t, event(map[string]any{"type": "_context", "usedTokens": p.TokenUsage.Total}))
		}
	case "thread/compacted":
		s.emitEvent(t, event(map[string]any{"type": "_notice", "text": "Codex compacted this thread."}))
	}
}

// onRequest maps the two approval shapes that can execute or modify files to
// the same human broker used by the Claude and Kimi harnesses. The other
// app-server request families (permission-profile expansion, questions and
// dynamic client tools) are not capabilities Agent Kate offers yet; replying
// with a JSON-RPC error makes that refusal explicit instead of deadlocking the
// turn or silently granting it.
func (s *Supervisor) onRequest(t *Thread, id json.RawMessage, method string, input json.RawMessage) {
	tool, approval := "", false
	switch method {
	case "item/commandExecution/requestApproval":
		tool, approval = "Bash", true
	case "item/fileChange/requestApproval":
		tool, approval = "Edit", true
	default:
		_ = t.rpc.respondError(id, -32601, "this Codex request is not supported by Agent Kate")
		return
	}
	allow := false
	if approval && s.permission != nil {
		allow = s.permission(t.ID, tool, input)
	}
	if method == "item/commandExecution/requestApproval" {
		decision := "decline"
		if allow {
			decision = "accept"
		}
		_ = t.rpc.respond(id, map[string]any{"decision": decision})
		return
	}
	if allow {
		_ = t.rpc.respond(id, map[string]any{"decision": "approved"})
		return
	}
	_ = t.rpc.respond(id, map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": "Denied by Agent Kate"}}})
}

func (s *Supervisor) handleItemCompleted(t *Thread, item struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	Status           string          `json:"status"`
	AggregatedOutput string          `json:"aggregatedOutput"`
	Tool             string          `json:"tool"`
	Server           string          `json:"server"`
	Arguments        json.RawMessage `json:"arguments"`
}) {
	switch item.Type {
	case "agentMessage":
		t.mu.Lock()
		b := t.text[item.ID]
		delete(t.text, item.ID)
		text := item.Text
		if b != nil && b.Len() > 0 {
			text = b.String()
		}
		t.mu.Unlock()
		if text != "" {
			s.emitEvent(t, event(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}}))
		}
	case "commandExecution":
		input, _ := json.Marshal(map[string]any{"command": item.Command})
		s.emitEvent(t, event(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": item.ID, "name": "Bash", "input": json.RawMessage(input)}}}}))
		if item.AggregatedOutput != "" {
			s.emitEvent(t, event(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": item.ID, "content": item.AggregatedOutput}}}}))
		}
	case "mcpToolCall", "dynamicToolCall":
		name := item.Tool
		if item.Server != "" {
			name = item.Server + "__" + name
		}
		s.emitEvent(t, event(map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": item.ID, "name": name, "input": item.Arguments}}}}))
	}
}

func event(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
