package kimi

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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"agentkate/internal/agent"
	"agentkate/internal/fsperm"
	"agentkate/internal/harness"
	"agentkate/internal/safe"
)

// EventFunc receives a batch of translated, Claude-shaped events for a thread.
// Identical in shape to agent.EventFunc so the run loop relays both backends
// through one code path.
type EventFunc func(threadID string, events []json.RawMessage)

// PermissionFunc asks the human to approve a gated tool call and returns the
// decision. The run loop wires it to the same broker + permission.requested
// notification the Claude MCP bridge uses, so the UI flow is identical.
// updatedInput carries whatever the human sent back with an allow — for a
// bridged AskUserQuestion (see answerQuestion) that is the answered question,
// which is the only way the CHOSEN answer reaches the CLI. Empty otherwise.
type PermissionFunc func(threadID, toolName string, input json.RawMessage) (allow bool, updatedInput json.RawMessage)

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
	Thinking    string             // kimi thinking level (config option "thinking"); "" = CLI default
	Mode        string             // kimi approval mode (config option "mode"); "" = CLI default
	MCPServers  []MCPServer        // forwarded to session/new (the Cooperation bridge)
	// Env overlays the child's environment. `kimi acp` takes no harness-shaping
	// flags at all (plan 15's probe), so environment is the ONLY per-thread
	// lever this CLI has — KIMI_CODE_HOME, for one, moves a thread's whole
	// kimi state (sessions, config, agents) somewhere of the caller's
	// choosing. See harness.StartSpec.Env for why no agent can set it.
	Env map[string]string
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
	// Config the handshake actually applied. Kimi downgrades a CLI-rejected
	// model/thinking/mode to its default, so these can differ from StartOptions
	// — the record must hold what is really running, so resume replays reality.
	appliedModel    string
	appliedThinking string
	appliedMode     string
	// stderr accumulated before the session went live (stderrLive), kept as a
	// bounded tail for a handshake-failure diagnostic; once live, new lines
	// surface as _stderr cards instead.
	stderrTail []string
	stderrLive bool
	// activePrompts counts session/prompt requests awaiting their response.
	// Normally 0 or 1 (ACP allows one turn per session; kimi rejects overlap),
	// but a rejected overlapping prompt completes independently and must not
	// clear the real turn's in-flight state — hence a counter, not a bool.
	// Interrupt is a no-op at 0: nothing to cancel means nothing to backstop.
	activePrompts int
	// pendingCmds holds the latest available_commands_update that arrived
	// BEFORE the handshake finished (kimi announces its command list during
	// session setup, when the translator doesn't exist yet). Start replays it
	// right after the init event so the UI's slash autocomplete is fed.
	pendingCmds json.RawMessage
	// commands is the CLI's current slash-command vocabulary, taken from the
	// latest available_commands_update. nil means the CLI has not announced
	// one yet, which is NOT the same as "no commands" — see hasCommand.
	commands map[string]bool
	// loading / pendingReplay buffer the history session/load streams as
	// notifications DURING the load call. They are held back so the whole
	// replayed conversation lands after the session's init event rather than
	// before it.
	loading       bool
	pendingReplay []json.RawMessage
	// internal is the Agent-Kate-issued turn in flight (`/compact`, `/usage`),
	// if any. Its prompt response belongs to us, not to the human, so
	// onPromptDone routes it here instead of ending the visible turn.
	internal *internalTurn
	// usageProbedAt is when the last `/usage` probe was STARTED, and throttles
	// the next one — see refreshUsage.
	usageProbedAt time.Time
	// authMethods is what initialize advertised. Kept so a session/new failure
	// on an unauthenticated CLI can say which sign-in to run instead of
	// surfacing the raw RPC error.
	authMethods []AuthMethod
	logFile     *os.File
	// stdoutDrained / stderrDrained are closed when the ACP reader and the
	// stderr pump return. reap() waits on them after cmd.Wait, before it emits
	// "exited" and closes the event log.
	stdoutDrained chan struct{}
	stderrDrained chan struct{}
	// logBytes is the event log's size as far as this thread knows (its size at
	// open, plus everything written since). It drives the retention trim — see
	// maxEventLogBytes.
	logBytes int64
}

// Event-log retention (audit F10). The per-thread JSONL log is append-only and
// a resumed thread appends to the SAME file, so a long-lived thread's log grows
// without bound — nothing ever deleted it. Past this size the log is trimmed to
// its most recent trimEventLogTo bytes on a line boundary: the replay only ever
// serves the tail anyway (harness.MaxReplayBytes), so the discarded prefix was
// already unreachable from the UI. The agent's own context is untouched by
// this; the CLI keeps its own session store.
const (
	maxEventLogBytes  = 32 << 20
	trimEventLogTo    = 8 << 20
	trimLineScanLimit = 1 << 20 // give up looking for a line boundary past this
)

// trimEventLogLocked rewrites the thread's event log down to its last
// trimEventLogTo bytes, starting at the first line boundary at or after the cut
// so no partial event survives. Caller holds t.mu and t.logFile is open.
//
// Failure is never fatal: on any error the log is left exactly as it was (and
// keeps growing) rather than risking a half-written transcript — losing the
// conversation is far worse than a large file.
func (s *Supervisor) trimEventLogLocked(t *Thread) {
	path := s.eventLogPath(t.ID)
	src, err := os.Open(path)
	if err != nil {
		s.log.Warn("kimi event log trim skipped", "thread", t.ID, "err", err)
		return
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil || st.Size() <= trimEventLogTo {
		return
	}
	if _, err := src.Seek(st.Size()-trimEventLogTo, io.SeekStart); err != nil {
		return
	}
	// Advance to the next newline so the file starts on a whole event.
	br := bufio.NewReaderSize(src, 64*1024)
	if _, err := br.ReadBytes('\n'); err != nil {
		return // no line boundary in the tail: leave the log alone
	}

	tmp := path + ".trim"
	_ = os.Remove(tmp)
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		s.log.Warn("kimi event log trim skipped", "thread", t.ID, "err", err)
		return
	}
	n, cerr := io.Copy(dst, br)
	closeErr := dst.Close()
	if cerr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		s.log.Warn("kimi event log trim failed", "thread", t.ID, "err", cerr)
		return
	}
	// Swap only once the replacement is complete on disk.
	_ = t.logFile.Close()
	t.logFile = nil
	if err := os.Rename(tmp, path); err != nil {
		s.log.Warn("kimi event log trim failed", "thread", t.ID, "err", err)
		_ = os.Remove(tmp)
	}
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// The thread keeps running; it just stops recording. Loud, not fatal.
		s.log.Warn("kimi event log unavailable after trim", "thread", t.ID, "err", err)
		return
	}
	t.logFile = lf
	t.logBytes = n
	s.log.Info("kimi event log trimmed", "thread", t.ID, "bytes", n)
}

// AuthMethod is one sign-in method from the ACP initialize response.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// internalTurn is one prompt turn Agent Kate sends on its own behalf — the
// slash commands the CLI answers locally. Silent turns leave no transcript
// trace at all; the caller waits on done and reads the CLI's reply text.
type internalTurn struct {
	kind   string // "compact" | "usage" — for logging
	silent bool

	done chan struct{}
	once sync.Once
	mu   sync.Mutex
	text string
	err  error

	// abandoned marks a turn whose bookkeeping was already unwound (it hung,
	// or a user send pre-empted it). Guarded by the owning Thread's mu, not
	// this one, because it is read and written alongside t.activePrompts.
	// A late response for an abandoned turn is dropped: decrementing then
	// would clear a LATER turn's in-flight state.
	abandoned bool

	// id is a process-unique, monotonic tag. It identifies which abandoned turn
	// armed the translator's drop latch, so a straggling reply can only lift
	// the latch it armed itself (see clearDropFor).
	id uint64
}

// internalTurnSeq hands out internalTurn.id. Package-global and atomic: ids
// only need to be unique, never ordered against anything but themselves, and
// starting at 1 keeps 0 meaning "no owner".
var internalTurnSeq atomic.Uint64

func newInternalTurn(kind string, silent bool) *internalTurn {
	return &internalTurn{
		kind:   kind,
		silent: silent,
		done:   make(chan struct{}),
		id:     internalTurnSeq.Add(1),
	}
}

// finish completes the turn exactly once — a second call (say the reap racing
// the prompt response) is a no-op.
func (it *internalTurn) finish(text string, err error) {
	it.once.Do(func() {
		it.mu.Lock()
		it.text, it.err = text, err
		it.mu.Unlock()
		close(it.done)
	})
}

func (it *internalTurn) result() (string, error) {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.text, it.err
}

// SessionID returns the kimi session id assigned by session/new (empty until
// the handshake completes).
func (t *Thread) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Model, Thinking and Mode report the config the handshake actually applied
// (a rejected request is downgraded to the CLI default), so the caller records
// reality rather than the request.
func (t *Thread) Model() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.appliedModel
}

func (t *Thread) Thinking() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.appliedThinking
}

func (t *Thread) Mode() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.appliedMode
}

// setCommandsLocked records the CLI's slash-command vocabulary. Names are
// stored bare (kimi announces "compact", the composer shows "/compact"), so
// membership tests read the same either way. Caller holds t.mu.
func (t *Thread) setCommandsLocked(names []string) {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[strings.TrimPrefix(strings.TrimSpace(n), "/")] = true
	}
	t.commands = m
}

// releasePromptSlotLocked gives back one in-flight prompt slot and settles the
// cancel flag with it. Every path that frees a slot must go through here:
// t.cancelling is what cancelPending consults for the interrupt backstop, and
// once nothing is in flight there is by definition no cancel left outstanding.
// Releasing the slot WITHOUT clearing it (abandonInternal used to) left the
// thread believing a cancel was pending forever — an Interrupt during an
// in-flight `/usage` or `/compact` poisoned every later turn. Caller holds t.mu.
func (t *Thread) releasePromptSlotLocked() {
	if t.activePrompts > 0 {
		t.activePrompts--
	}
	if t.activePrompts == 0 {
		t.cancelling = false
	}
}

// hasCommand reports whether the CLI offers a slash command. A thread that has
// never received an available_commands_update answers true for everything:
// silence does not prove absence, and gating on it would break a CLI that
// simply never announces. Once a list HAS arrived it is authoritative — a
// command missing from it would otherwise be delivered to the model as
// literal prompt text, which for `/compact` means a wasted turn and a reply
// the caller would mistake for a summary.
func (t *Thread) hasCommand(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.commands == nil {
		return true
	}
	return t.commands[strings.TrimPrefix(name, "/")]
}

// stderrTailString returns the buffered pre-handshake stderr as a single line,
// for a handshake-failure diagnostic.
func (t *Thread) stderrTailString() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.stderrTail, "; ")
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

	// drainGrace bounds how long reap waits, after cmd.Wait returns, for the
	// output readers to reach EOF — a reader kept alive by a leaked grandchild
	// must not leak the reaper (and with it the thread's "exited" event).
	// Overridable in tests.
	drainGrace time.Duration

	// reapWG tracks every in-flight reap() goroutine; StopAll waits on it so
	// each thread's "exited" lifecycle event has been delivered before shutdown
	// proceeds (same guarantee agent.Supervisor makes).
	reapWG sync.WaitGroup

	// One-shot DiscoverOptions probe cache: the CLI's config-option vocabulary
	// is stable for the process lifetime, so one successful probe serves every
	// later call. Guarded by its own mutex (held across the probe — see
	// DiscoverOptions) so a slow probe never blocks the thread map.
	discoverMu     sync.Mutex
	discovered     bool
	discoveredOpts []ConfigOption

	// onPreempt, when set, runs inside Send's pre-empt/claim loop between
	// preemptInternal returning and the lock being taken. That gap is exactly
	// the race the loop exists to close, and it is otherwise unhittable from a
	// test. Set only by tests; nil in every real supervisor.
	onPreempt func(*Thread)
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
	// Migrate an event store an earlier build created world-readable. These
	// logs are whole kimi transcripts — the most sensitive thing Agent Kate
	// keeps — and the mode constants on the write path below only apply to
	// files and directories they CREATE, so without this pass every log
	// already on disk keeps its 0644 forever. Logged, never fatal: a store
	// that cannot be tightened must still be usable, and the message is what
	// makes the exposure visible.
	if n, err := fsperm.HardenTree(eventDir); err != nil {
		if log != nil {
			log.Warn("could not tighten permissions on the kimi event store", "store", eventDir, "err", err)
		}
	} else if n > 0 {
		fsperm.LogMigration(eventDir, n)
	}
	return &Supervisor{
		kimiBin:             kimiBin,
		log:                 log,
		emit:                emit,
		perm:                perm,
		eventDir:            eventDir,
		cancelBackstopDelay: 3 * time.Second,
		cancelKillDelay:     2 * time.Second,
		drainGrace:          5 * time.Second,
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

// DeleteTranscript removes a thread's event log (and any half-finished trim
// beside it). Called when a thread is DESTROYED — discard, or cleanup's
// archive-and-remove — because nothing else ever deleted these files: they are
// full kimi transcripts, they were appended to on every resume, and they
// outlived the threads they belonged to forever (audit F10).
//
// Unlike the claude transcript, which is the CLI's own file and is deliberately
// left on disk so an archived thread stays recoverable, this log is ours and is
// meaningless once the thread it replays is gone.
//
// A missing file is success: the caller is asking for the log to be gone.
func (s *Supervisor) DeleteTranscript(threadID string) error {
	if threadID == "" {
		return nil
	}
	path := s.eventLogPath(threadID)
	_ = os.Remove(path + ".trim")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadTranscript returns the translated events logged for a kimi thread, in
// order, so the UI can replay the conversation when reopening a dormant
// thread — the kimi counterpart of session.ReadTranscript. Returns nil with
// no error if there is no log yet.
//
// The read is BOUNDED (audit F10): only the most recent events that fit the
// replay caps are kept, and memory never exceeds them however long the log has
// grown. When older events are dropped the returned slice opens with a
// truncation notice, so a shortened history is visible rather than silent.
//
// A trailing line that is not valid JSON is skipped, not relayed: the log is
// appended without fsync, so a crash can leave a torn last line, and shipping
// that fragment to the UI as an event is a parse error in the panel for a
// problem that belongs here.
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
	// Budget leaves room for the notice event the handler-side cap would
	// otherwise have to add on top (harness.CapTranscript is then a no-op).
	maxEvents := harness.MaxReplayEvents - 1
	maxBytes := harness.MaxReplayBytes - 4096

	var (
		ring    []json.RawMessage // most recent events, oldest first
		bytesIn int
		omitted int
		torn    int
	)
	drop := func() {
		bytesIn -= len(ring[0])
		ring = ring[1:]
		omitted++
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			torn++
			continue
		}
		ring = append(ring, append(json.RawMessage(nil), line...))
		bytesIn += len(line)
		for len(ring) > maxEvents || (bytesIn > maxBytes && len(ring) > 1) {
			drop()
		}
	}
	if err := sc.Err(); err != nil {
		// A line longer than the scanner cap ends the read. Everything up to it
		// is still a usable history, so report what we have rather than nothing
		// — but say so, because the tail is missing.
		if !errors.Is(err, bufio.ErrTooLong) {
			return nil, err
		}
		torn++
	}
	if torn > 0 {
		slog.Warn("kimi transcript: skipped unreadable lines",
			"thread", threadID, "lines", torn)
	}
	if omitted == 0 {
		return ring, nil
	}
	if notice := harness.TruncationNotice(omitted); notice != nil {
		return append([]json.RawMessage{notice}, ring...), nil
	}
	return ring, nil
}

// Start launches a new kimi thread: spawn `kimi acp`, run the ACP handshake
// (initialize → session/new, or session/resume for a dormant thread), then
// send the opening prompt.
func (s *Supervisor) Start(opts StartOptions) (*Thread, error) {
	cmd := exec.Command(s.kimiBin, "acp")
	cmd.Dir = opts.WorkDir
	// kimi used to spawn with the core's environment verbatim; a per-thread
	// overlay is the only way to shape an `acp` child (plan 16 P6).
	cmd.Env = agent.ApplyEnvOverlay(os.Environ(), opts.Env)
	// Own process group, like the Claude threads, so the interrupt backstop
	// can signal kimi plus anything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	// Deliberately os.Pipe + cmd.Stdout, NOT cmd.StdoutPipe (audit F24):
	// cmd.Wait closes the pipes it created, so a reap that wins the race
	// against the ACP reader discards whatever is still in the pipe — the last
	// frames of the final turn. A pipe we own is closed by us, at real EOF.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		stdout.Close()
		stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// readingOut/readingErr are set once the reader goroutines own the
	// respective read end (each closes its own at EOF). Until then every
	// failure exit must close it here, or a failed spawn leaks descriptors.
	readingOut, readingErr := false, false
	defer func() {
		stdoutW.Close()
		stderrW.Close()
		if !readingOut {
			stdout.Close()
		}
		if !readingErr {
			stderr.Close()
		}
	}()

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
	// reap() waits on stdoutDrained after cmd.Wait(): the last frames of the
	// final turn may still be in the pipe when the process dies, and the exited
	// event must not close the event log under the reader translating them
	// (audits F24/F51).
	t.stdoutDrained = make(chan struct{})
	readingOut = true
	safe.Go("kimi.acpRead", func() {
		defer close(t.stdoutDrained)
		defer stdout.Close()
		t.client.readLoop(stdout)
	})

	// A resume with no transcript of our own is a session Agent Kate never
	// ran — browse-resumed from the CLI's own store. There is nothing to
	// replay locally, so ask the CLI to replay it instead (session/load).
	// Measured BEFORE the log is opened, since opening creates it.
	loadHistory := opts.Resume && opts.SessionID != "" && !s.haveTranscript(id)

	// The translated-event log is the thread's transcript; a start failure is
	// logged, never fatal — replay simply degrades to empty. A resumed thread
	// APPENDS: the existing log is the history the UI replays, so truncating
	// here would erase the conversation up to the resume.
	logFlags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if opts.Resume {
		logFlags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	if err := fsperm.MkdirAll(s.eventDir); err == nil {
		if lf, err := os.OpenFile(s.eventLogPath(id), logFlags, fsperm.FileMode); err == nil {
			t.logFile = lf
			// OpenFile applies its mode only when it CREATES the file, so a
			// resumed thread appending to a log an earlier build left 0644
			// would keep that mode for the life of the thread.
			if _, herr := fsperm.HardenFile(s.eventLogPath(id)); herr != nil && s.log != nil {
				s.log.Warn("could not tighten permissions on the kimi event log",
					"path", s.eventLogPath(id), "err", herr)
			}
			// Seed the retention counter from what is already on disk: a resumed
			// thread appends to an existing log, and the trim must account for
			// every earlier session, not just this one.
			if st, serr := lf.Stat(); serr == nil {
				t.logBytes = st.Size()
			}
		} else {
			s.log.Warn("kimi event log unavailable", "thread", id, "err", err)
		}
	}

	// Drain stderr from the moment the child starts — before the handshake, not
	// after — so a chatty startup (verbose logging, a noisy shell rc) can't fill
	// the OS pipe buffer and stall the handshake into a spurious timeout. Until
	// the session is live the pump buffers lines as a tail for a failure
	// diagnostic; afterwards it emits them as _stderr cards.
	stderrDone := make(chan struct{})
	t.stderrDrained = stderrDone
	readingErr = true
	safe.Go("kimi.pumpStderr", func() {
		defer stderr.Close()
		s.pumpStderr(t, stderr, stderrDone)
	})

	// teardown kills the process group (not just the leader — a handshake that
	// failed after session/new may already have spawned children), lets the
	// stderr pump drain the closing pipe, then reaps. Used by both failure
	// exits below; no thread was ever registered, so no "exited" event follows.
	teardown := func() {
		_ = stdin.Close()
		if t.pgid > 0 {
			_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
		select {
		case <-stderrDone:
		case <-time.After(2 * time.Second):
		}
		_ = cmd.Wait()
		t.mu.Lock()
		if t.logFile != nil {
			_ = t.logFile.Close()
			t.logFile = nil
		}
		t.mu.Unlock()
	}

	// The ACP handshake runs synchronously so a failure (kimi missing, not
	// logged in, bad session id) is reported through Start's error return —
	// the caller turns it into an "error" lifecycle event, exactly like a
	// failed Claude spawn. The drained stderr tail rides along so the user sees
	// why (the RPC error alone rarely says).
	if err := s.handshake(t, opts, loadHistory); err != nil {
		teardown()
		if tail := t.stderrTailString(); tail != "" {
			err = fmt.Errorf("%w (kimi stderr: %s)", err, tail)
		}
		return nil, err
	}

	// Register atomically: a double resume (say a double-clicked Resume) can
	// race two Starts to here, and a blind overwrite would strand the loser's
	// live process — the winner's reap() would delete the map entry and
	// deregister it. Refuse the duplicate so the race loser fails cleanly.
	s.mu.Lock()
	if _, dup := s.threads[t.ID]; dup {
		s.mu.Unlock()
		teardown()
		return nil, fmt.Errorf("thread %q already running", t.ID)
	}
	s.threads[t.ID] = t
	s.mu.Unlock()

	s.log.Info("kimi process spawned", "thread", t.ID, "dir", opts.WorkDir,
		"pid", cmd.Process.Pid, "session", t.SessionID())

	s.reapWG.Add(1)
	safe.Go("kimi.reap", func() { s.reap(t) })

	// Session start, shaped like claude's init system event — the run loop
	// persists the session id from it and the UI shows the model line.
	s.emitEvents(t, []json.RawMessage{t.tr.initEvent()})

	// The session is live: from here, stderr lines surface as _stderr cards
	// (in order, after the init event) rather than feeding the failure tail.
	t.mu.Lock()
	t.stderrLive = true
	t.mu.Unlock()

	// Replay a command list announced during the handshake (see pendingCmds).
	t.mu.Lock()
	pendingCmds := t.pendingCmds
	t.pendingCmds = nil
	replay := t.pendingReplay
	t.pendingReplay = nil
	tr := t.tr
	t.mu.Unlock()
	if pendingCmds != nil {
		s.emitEvents(t, tr.update(pendingCmds))
	}
	// A session/load handshake replayed the CLI's own history; ship it now, in
	// order, after the init event (see Thread.loading).
	if len(replay) > 0 {
		s.log.Info("replayed kimi history from the CLI's store",
			"thread", t.ID, "session", t.SessionID(), "events", len(replay))
		s.emitEvents(t, replay)
	}

	// A fresh thread gets its opening turn now. A resumed thread has none —
	// it waits for the user's next message (same contract as agent.Start).
	if opts.Prompt != "" || len(opts.Attachments) > 0 {
		if err := s.Send(t.ID, opts.Prompt, opts.Attachments); err != nil {
			s.log.Warn("failed to send initial prompt", "thread", t.ID, "err", err)
		}
	}
	return t, nil
}

// haveTranscript reports whether this thread already has translated events of
// its own on disk — the history the UI replays without any help from the CLI.
func (s *Supervisor) haveTranscript(threadID string) bool {
	st, err := os.Stat(s.eventLogPath(threadID))
	return err == nil && st.Size() > 0
}

// handshake performs initialize → session/new (or session/resume, or
// session/load when there is history only the CLI has) and applies the
// requested model.
func (s *Supervisor) handshake(t *Thread, opts StartOptions, loadHistory bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The request body is shared with the one-shot probes (discover.go) so the
	// CLI always sees the same client capabilities.
	var initRes struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
		AuthMethods []AuthMethod `json:"authMethods"`
	}
	if err := t.client.call(ctx, "initialize", initializeParams(), &initRes); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	t.mu.Lock()
	t.authMethods = initRes.AuthMethods
	t.mu.Unlock()

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
		// session/resume re-attaches WITHOUT replaying history — normally
		// exactly right, since the UI replays the transcript itself from the
		// translated-event log and an ACP-side replay would double every card.
		// A session Agent Kate never ran (browse-resumed from the CLI's own
		// store) has no such log, and that is the one case where the CLI's
		// replay is the only history there is: session/load then.
		method = "session/resume"
		if loadHistory && initRes.AgentCapabilities.LoadSession {
			method = "session/load"
		}
		sessionParams["sessionId"] = opts.SessionID
	}
	// The full config-option set (model / thinking / mode enumerations, each
	// with values and display names) rides into the translator's init event so
	// the UI can offer real pickers instead of free-text fields.
	var sessionRes struct {
		SessionID     string         `json:"sessionId"`
		ConfigOptions []ConfigOption `json:"configOptions"`
	}
	if method == "session/load" {
		// The replay streams as session/update notifications DURING the call,
		// so the translator has to exist before it is made. Its model and
		// option set arrive with the response and are filled in below.
		t.mu.Lock()
		t.sessionID = opts.SessionID
		t.tr = newTranslator(opts.SessionID, "", nil)
		t.loading = true
		t.mu.Unlock()
	}
	err := t.client.call(ctx, method, sessionParams, &sessionRes)
	if err != nil && method == "session/load" {
		// A CLI that advertises loadSession but refuses this session still has
		// a working re-attach; take it and accept an empty transcript rather
		// than failing the resume outright.
		s.log.Warn("kimi session/load failed; falling back to session/resume",
			"thread", t.ID, "session", opts.SessionID, "err", err)
		t.mu.Lock()
		t.loading = false
		t.pendingReplay = nil
		t.tr = nil
		t.mu.Unlock()
		method = "session/resume"
		err = t.client.call(ctx, method, sessionParams, &sessionRes)
	}
	if err != nil {
		return s.sessionError(t, method, err)
	}
	if t.loadingDone() {
		// Everything the replay streamed is buffered; close the last message
		// off, since a replay has no prompt response to end its final turn.
		t.mu.Lock()
		tr := t.tr
		t.mu.Unlock()
		t.appendReplay(tr.flush())
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

	// configValue reads an option's CLI default straight from the handshake's
	// set — the value that applies when a request is empty or gets rejected.
	configValue := func(id string) string {
		for _, co := range sessionRes.ConfigOptions {
			if co.ID == id {
				return co.CurrentValue
			}
		}
		return ""
	}
	// setOption applies one config option and returns the value that actually
	// took effect: the request when the CLI accepted it, else the CLI default —
	// for an empty request, or a rejected one. Kimi downgrades a rejected value
	// silently, so reporting the request as applied would make the record claim
	// a model/mode the agent is not running; the caller records what this
	// returns instead. A rejection is a warning, never a failed start. The init
	// event's option set is kept consistent so the UI picker shows the real
	// current value.
	setOption := func(id, value string) string {
		if value != "" {
			// The response is the authoritative post-change config; fold it in
			// so an accepted-but-adjusted value is recorded as what it became.
			var res json.RawMessage
			if err := t.client.call(ctx, "session/set_config_option", map[string]any{
				"sessionId": sessionID,
				"configId":  id,
				"value":     value,
			}, &res); err != nil {
				s.log.Warn("could not set kimi config option; using the CLI default",
					"thread", t.ID, "option", id, "value", value, "err", err)
				value = ""
			} else {
				for _, up := range decodeConfigOptions(res) {
					if up.ID != id || up.CurrentValue == "" {
						continue
					}
					if up.CurrentValue != value {
						s.log.Info("kimi adjusted a config option",
							"thread", t.ID, "option", id,
							"requested", value, "applied", up.CurrentValue)
					}
					value = up.CurrentValue
				}
			}
		}
		if value == "" {
			value = configValue(id)
		}
		for i := range sessionRes.ConfigOptions {
			if sessionRes.ConfigOptions[i].ID == id {
				sessionRes.ConfigOptions[i].CurrentValue = value
			}
		}
		return value
	}
	model := setOption("model", opts.Model)
	thinking := setOption("thinking", opts.Thinking)
	mode := setOption("mode", opts.Mode)

	t.mu.Lock()
	t.appliedModel = model
	t.appliedThinking = thinking
	t.appliedMode = mode
	if t.tr != nil {
		// The session/load path built the translator early (see above); give
		// it the model and option set the response finally supplied.
		t.tr.setSession(model, sessionRes.ConfigOptions)
	} else {
		t.tr = newTranslator(sessionID, model, sessionRes.ConfigOptions)
	}
	t.mu.Unlock()
	return nil
}

// loadingDone clears the replay-buffering flag and reports whether it was set.
func (t *Thread) loadingDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := t.loading
	t.loading = false
	return was
}

// appendReplay buffers translated history for Start to emit after the init
// event.
func (t *Thread) appendReplay(events []json.RawMessage) {
	if len(events) == 0 {
		return
	}
	t.mu.Lock()
	t.pendingReplay = append(t.pendingReplay, events...)
	t.mu.Unlock()
}

// sessionError turns a failed session/new|resume|load into the clearest error
// available. An unauthenticated CLI answers with an opaque RPC rejection, so
// when initialize advertised sign-in methods and the failure looks like an auth
// refusal, the message names them and says what to run — the difference
// between "acp session/new: acp error -32000: authentication required" and an
// instruction the user can act on.
func (s *Supervisor) sessionError(t *Thread, method string, err error) error {
	t.mu.Lock()
	methods := t.authMethods
	t.mu.Unlock()
	if len(methods) == 0 || !looksLikeAuthFailure(err) {
		return fmt.Errorf("acp %s: %w", method, err)
	}
	names := make([]string, 0, len(methods))
	for _, m := range methods {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		if m.Description != "" {
			label += " — " + m.Description
		}
		names = append(names, label)
	}
	return fmt.Errorf("%s is not signed in (%s %v). Sign in first: run `%s` in a "+
		"terminal and complete one of: %s",
		s.kimiBin, method, err, s.kimiBin, strings.Join(names, "; "))
}

// authRequiredCode is ACP's "the agent needs authentication" JSON-RPC code.
const authRequiredCode = -32000

// looksLikeAuthFailure reports whether an RPC rejection is about sign-in.
// The code is the reliable signal; the wording is the backstop for a CLI that
// reports it as a generic internal error.
func looksLikeAuthFailure(err error) bool {
	var ae *acpError
	if errors.As(err, &ae) {
		// -32000 doubles as our own stream-closed marker; a dead process is
		// not an auth problem.
		if ae.Message == errStreamClosed.Error() {
			return false
		}
		if ae.Code == authRequiredCode {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"auth", "not logged in", "log in", "login", "sign in", "signed in",
		"unauthenticated", "unauthorized", "api key", "credential",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
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
	// A `/usage` probe fired by the previous turn may still be in flight, and
	// kimi rejects an overlapping prompt. It is discardable bookkeeping, so it
	// is cancelled rather than waited out — the human's message must not queue
	// behind it.
	//
	// preemptInternal necessarily runs WITHOUT the lock (it waits on the
	// turn's unwind), which leaves a window between it returning and this
	// goroutine claiming the turn slot. The post-turn `/usage` probe runs off
	// its own goroutine and can start a fresh internal turn inside that
	// window; both prompts would then be in flight and kimi would reject the
	// human's. So the claim re-checks t.internal under the SAME lock hold that
	// increments activePrompts, and pre-empts again if one appeared.
	var (
		alive, stopping bool
		sid             string
		tr              *translator
		claimed         bool
	)
	for round := 0; round < preemptRounds && !claimed; round++ {
		s.preemptInternal(t, preemptGrace)
		if s.onPreempt != nil {
			s.onPreempt(t)
		}
		t.mu.Lock()
		alive, stopping, sid, tr = t.alive, t.stopping, t.sessionID, t.tr
		switch {
		case !alive || stopping:
			claimed = true // nothing to claim; the error below is the answer
		case t.internal != nil:
			// Lost the race — go round and cancel the newcomer too.
		default:
			t.activePrompts++ // Interrupt has something to cancel from here on
			claimed = true
		}
		t.mu.Unlock()
	}
	if !alive {
		return fmt.Errorf("thread %q is not running", threadID)
	}
	if stopping {
		return fmt.Errorf("thread %q is stopping", threadID)
	}
	if !claimed {
		// Only reachable if a new internal turn appeared on every round, which
		// would mean something is spawning them in a loop. Refusing is far
		// better than writing a prompt kimi is certain to reject.
		return fmt.Errorf("thread %q is busy with its own bookkeeping turn", threadID)
	}
	// From here the feed belongs to the human's turn, so any drop latch a
	// pre-empted probe left behind has to go — even if that probe never
	// answered the cancel (see abandonCapture).
	if tr != nil {
		tr.clearDrop()
	}
	_, err := t.client.send("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    buildPromptContent(text, attachments),
	}, func(f acpFrame) { s.onPromptDone(t, f, nil) })
	if err != nil {
		t.mu.Lock()
		t.releasePromptSlotLocked()
		t.mu.Unlock()
	}
	return err
}

// The slash commands the CLI answers itself, sent as ordinary prompt text.
// Both are local: `/compact` rewrites the session's own context and `/usage`
// reports it back, neither spending a model call.
const (
	compactCommand = "/compact"
	usageCommand   = "/usage"
)

// internalTurnTimeout bounds a locally-answered slash command. Both are
// in-process work for the CLI, so anything approaching this is a hang.
const internalTurnTimeout = 30 * time.Second

// sendInternal issues one Agent-Kate-owned prompt turn and returns a handle to
// wait on. ACP allows a single turn per session and kimi rejects an overlapping
// prompt, so an internal turn is only ever started on an idle thread — the
// caller's request is refused rather than queued, since these are all
// best-effort bookkeeping.
func (s *Supervisor) sendInternal(t *Thread, kind, prompt string, silent bool) (*internalTurn, error) {
	t.mu.Lock()
	switch {
	case !t.alive:
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q is not running", t.ID)
	case t.stopping:
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q is stopping", t.ID)
	case t.internal != nil:
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q is already running a %s turn", t.ID, t.internal.kind)
	case t.activePrompts > 0:
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q is busy; %s needs an idle turn", t.ID, kind)
	case t.tr == nil:
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q has no session yet", t.ID)
	}
	it := newInternalTurn(kind, silent)
	t.internal = it
	t.activePrompts++
	tr := t.tr
	sid := t.sessionID
	t.mu.Unlock()

	tr.beginCapture(silent)
	_, err := t.client.send("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	}, func(f acpFrame) { s.onPromptDone(t, f, it) })
	if err != nil {
		t.mu.Lock()
		t.releasePromptSlotLocked()
		if t.internal == it {
			t.internal = nil
		}
		t.mu.Unlock()
		tr.endCapture()
		return nil, err
	}
	return it, nil
}

// runInternal sends an internal turn and waits for its reply text.
func (s *Supervisor) runInternal(ctx context.Context, t *Thread, kind, prompt string, silent bool) (string, error) {
	it, err := s.sendInternal(t, kind, prompt, silent)
	if err != nil {
		return "", err
	}
	select {
	case <-it.done:
		return it.result()
	case <-ctx.Done():
		// Giving up on the wait is not enough: the turn still holds
		// t.internal and an activePrompts slot, and leaving them set would
		// make every later Send pay the full timeout again — one hung
		// bookkeeping turn would wedge the thread for good.
		s.abandonInternal(t, it)
		return "", ctx.Err()
	}
}

// abandonInternal unwinds a stuck or pre-empted internal turn so the thread is
// immediately usable again, exactly as sendInternal's error path does: the
// in-flight counter is released, the capture closed, and the turn's waiter
// released. The CLI is told to drop the turn too — it would otherwise keep
// working on a prompt nobody awaits and reject the user's next one.
func (s *Supervisor) abandonInternal(t *Thread, it *internalTurn) {
	t.mu.Lock()
	if it.abandoned || t.internal != it {
		t.mu.Unlock()
		return // already completed or already unwound
	}
	it.abandoned = true
	t.internal = nil
	t.releasePromptSlotLocked()
	tr := t.tr
	sid := t.sessionID
	t.mu.Unlock()

	if tr != nil {
		// Closing the capture is not enough: the CLI has NOT stopped talking
		// yet, and a silent probe's remaining chunks would land in the human's
		// transcript as an assistant card. Latch them away until the abandoned
		// prompt's own reply arrives (onPromptDone clears it) — and only that
		// reply, hence the id: a second abandon may latch after this one.
		tr.abandonCapture(it.id)
	}
	// A plain notify, not Interrupt: this must not arm the signal backstop,
	// which would kill a healthy process over a bookkeeping turn.
	_ = t.client.notify("session/cancel", map[string]any{"sessionId": sid})
	s.log.Debug("abandoned kimi internal turn", "thread", t.ID, "kind", it.kind)
	it.finish("", fmt.Errorf("the %s turn was abandoned before the CLI replied", it.kind))
}

// preemptInternal clears the way for a turn the HUMAN asked for (a send, or
// "Summarize now"). An Agent-Kate-owned turn is always discardable bookkeeping
// (`/usage`, or a `/compact` the user has now moved past), so it is cancelled
// rather than waited out: the user's request must not sit behind a 30-second
// timeout, nor be refused as busy. The cancel usually lands in
// milliseconds; if the CLI does not ack within grace, the turn is abandoned
// locally so the send proceeds regardless.
func (s *Supervisor) preemptInternal(t *Thread, grace time.Duration) {
	t.mu.Lock()
	it := t.internal
	sid := t.sessionID
	t.mu.Unlock()
	if it == nil {
		return
	}
	_ = t.client.notify("session/cancel", map[string]any{"sessionId": sid})
	select {
	case <-it.done:
	case <-time.After(grace):
		s.abandonInternal(t, it)
	}
}

// preemptGrace bounds how long a user's send waits for a cancelled internal
// turn to unwind. Kept short: these turns are answered locally by the CLI, so
// anything past this is a hang, and the human should never feel it.
const preemptGrace = 2 * time.Second

// preemptRounds bounds Send's pre-empt/claim retry loop. One retry covers the
// real race (a probe that started while the previous pre-empt was unwinding);
// the rest are slack, and the ceiling is what stops a pathological producer of
// internal turns from spinning a user's send forever.
const preemptRounds = 3

// Compact runs kimi's own in-session compaction: `/compact` sent as an ordinary
// prompt turn, which the CLI intercepts and answers by rewriting the session's
// context in place. Verified against kimi 0.30 — there is no ACP compaction
// method, but the slash command really does compact.
//
// The returned string is whatever the CLI wrote back. It is deliberately NOT a
// summary in the Claude sense: claude's hot compaction asks the MODEL to write
// a summary and the caller stores that text to seed a fresh session, whereas
// kimi keeps the compacted context inside its own session and prints at most a
// status line. See kimiHarness.Compact for what the adapter does with that
// difference.
func (s *Supervisor) Compact(ctx context.Context, threadID string) (string, error) {
	t := s.thread(threadID)
	if t == nil {
		return "", fmt.Errorf("unknown thread %q", threadID)
	}
	// The command is only a command if the CLI says it is. Sent to a build
	// that doesn't offer it, `/compact` is just prompt text: the model would
	// spend a real turn on it and answer with prose the caller would store as
	// a summary of a session that was never compacted.
	if !t.hasCommand(compactCommand) {
		return "", fmt.Errorf("this Kimi Code session offers no %s command", compactCommand)
	}
	// The `/usage` probe that follows EVERY turn may still be in flight, and an
	// internal turn is only started on an idle thread — without this,
	// "Summarize now" right after a turn is refused as busy.
	s.preemptInternal(t, preemptGrace)
	// Not silent: the human asked for this one, so the turn belongs in the
	// transcript exactly as the claude backend's hot compaction is.
	text, err := s.runInternal(ctx, t, "compact", compactCommand, false)
	if err != nil {
		return "", err
	}
	// The context just shrank — the one moment the figure moves the wrong way,
	// and the readout on screen is now wrong. Never throttled.
	s.refreshUsage(t, true)
	return strings.TrimSpace(text), nil
}

// usageProbeInterval throttles the routine post-turn `/usage` probe.
//
// The probe is free of inference, but it is NOT free: it is a full extra ACP
// round-trip per turn, and because it lands on an idle thread the human's next
// send has to pre-empt it — up to preemptGrace (2s) of stall on a message they
// have already hit send on. A rapid back-and-forth pays that on every turn
// while the readout barely moves.
//
// Correctness is unaffected. The figure only ever grows within a session
// (context accumulates), so a throttled readout under-reports slightly rather
// than showing something false, and the one moment where it can move
// DISCONTINUOUSLY — a compaction — passes force and is never throttled.
// Compact is the only force:true caller; there is deliberately no "refresh the
// meter" RPC, because the probe costs the turn slot the human's next send would
// have to pre-empt, and the post-turn probe already keeps the meter honest.
const usageProbeInterval = 30 * time.Second

// refreshUsage asks the CLI for its context/token readout and publishes it.
// Silent: it is Agent Kate's bookkeeping, not a turn the human took, and it
// must leave no card behind. Best-effort throughout: a busy thread, an
// unparseable answer or a CLI that has no `/usage` all end in nothing
// published rather than an error anyone sees.
//
// force skips the throttle, for the callers whose whole point is that the
// number just changed out from under the readout.
func (s *Supervisor) refreshUsage(t *Thread, force bool) {
	// Same reasoning as Compact: a CLI that doesn't offer `/usage` would treat
	// it as a prompt and burn a real turn on it. Skipping silently is right —
	// the readout is a nicety, and nobody asked for it.
	if !t.hasCommand(usageCommand) {
		return
	}
	now := time.Now()
	t.mu.Lock()
	if !force && !t.usageProbedAt.IsZero() && now.Sub(t.usageProbedAt) < usageProbeInterval {
		t.mu.Unlock()
		return
	}
	// Stamped on START, not completion: the cost this bounds is the probe
	// occupying the turn slot, which begins now.
	t.usageProbedAt = now
	t.mu.Unlock()
	safe.Go("kimi.refreshUsage", func() {
		ctx, cancel := context.WithTimeout(context.Background(), internalTurnTimeout)
		defer cancel()
		text, err := s.runInternal(ctx, t, "usage", usageCommand, true)
		if err != nil {
			s.log.Debug("kimi usage probe skipped", "thread", t.ID, "err", err)
			return
		}
		u, ok := parseUsage(text)
		if !ok {
			s.log.Debug("kimi usage output not recognised", "thread", t.ID, "text", text)
			return
		}
		t.mu.Lock()
		tr := t.tr
		t.mu.Unlock()
		if tr == nil {
			return
		}
		if ev := tr.setUsage(u); ev != nil {
			s.emitEvents(t, []json.RawMessage{ev})
		}
	})
}

// onPromptDone completes a turn: the prompt response's stopReason becomes the
// result event. Runs on the ACP read-loop goroutine, after every session/update
// for the turn has already been translated.
//
// owner names the internal turn this reply belongs to (nil for a human turn).
// It is checked against the thread's current one under the SAME lock hold that
// releases the in-flight slot: an internal turn abandoned exactly as its reply
// arrived has had its bookkeeping unwound already, and a human send may have
// claimed the slot in between — decrementing again would end that turn.
func (s *Supervisor) onPromptDone(t *Thread, f acpFrame, owner *internalTurn) {
	// This prompt is over, however it ended — success, error or stream close.
	// Clearing cancelling once no prompt remains in flight (not just on
	// stopReason "cancelled") is what keeps the interrupt backstop from
	// SIGINTing a healthy process when a cancel races the turn's natural
	// completion: ACP treats a cancel of a finished turn as a no-op, so no
	// "cancelled" stop reason ever arrives.
	t.mu.Lock()
	if owner != nil && (owner.abandoned || t.internal != owner) {
		// The slot was released by abandonInternal, but the cancel flag still
		// has to settle against what is in flight NOW: a cancel that raced this
		// abandoned turn has nothing left to acknowledge it, and leaving
		// t.cancelling set would keep cancelPending true for good.
		if t.activePrompts == 0 {
			t.cancelling = false
		}
		tr := t.tr
		t.mu.Unlock()
		// The CLI has stopped streaming for the abandoned turn, so whatever it
		// still had to say has now been said (and dropped). Lift only THIS
		// turn's latch: a later abandoned turn may still be streaming behind it.
		if tr != nil {
			tr.clearDropFor(owner.id)
		}
		return
	}
	t.releasePromptSlotLocked()
	tr := t.tr
	stopping := t.stopping
	it := t.internal
	t.internal = nil
	t.mu.Unlock()

	if it != nil {
		// An Agent-Kate-owned turn (`/compact`, `/usage`): its reply goes to
		// the caller that asked for it. A silent one's events were dropped as
		// they streamed, so endTurn has nothing left to ship.
		var events []json.RawMessage
		if f.Error == nil {
			events = tr.endTurn()
		}
		text := tr.endCapture()
		s.emitEvents(t, events)
		var err error
		if f.Error != nil && !isStreamClosed(f.Error) {
			err = fmt.Errorf("%s: %w", it.kind, f.Error)
		} else if f.Error != nil {
			err = f.Error
		}
		it.finish(text, err)
		return
	}

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
	// The turn just changed how full the context is; read it back. This is the
	// routine path, so it is throttled (usageProbeInterval) rather than run on
	// every single turn — and not run at all while shutting down, where the
	// only thing left to do is exit.
	if !stopping {
		s.refreshUsage(t, false)
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

	// The backstop is armed BEFORE the cancel is sent, and the notify itself
	// runs asynchronously (audit F52, mirroring the claude backend's F9 fix):
	// Interrupt is the UI's escape hatch, so it must return promptly even when
	// the child has stopped draining stdin — in that state the pipe is full and
	// the notify blocks until its write deadline. The backstop is exactly the
	// recovery for "the cancel never landed", so it must not sit behind that
	// write.
	safe.Go("kimi.interruptBackstop", func() {
		time.Sleep(s.cancelBackstopDelay)
		// Signal only the exact thread this backstop was armed for. If the
		// original was reaped during the sleep and its id reused by a resumed
		// thread, s.thread(threadID) is now a different *Thread — the captured
		// pgid is stale and must not be signalled (pgid reuse could hit an
		// unrelated process).
		if s.thread(threadID) != t || !s.cancelPending(threadID) {
			return // clean cancel, or a different thread now owns this id
		}
		s.log.Info("cancel not acked; escalating to signals", "thread", threadID)
		t.mu.Lock()
		t.interrupted = true // reap() will report a user-interrupt now
		t.mu.Unlock()
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		}
		time.Sleep(s.cancelKillDelay)
		if s.thread(threadID) != t || !s.Running(threadID) {
			return
		}
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else if proc != nil {
			_ = proc.Kill()
		}
	})

	safe.Go("kimi.interruptCancel", func() {
		if err := t.client.notify("session/cancel", map[string]any{"sessionId": sid}); err != nil {
			s.log.Warn("session/cancel write failed; relying on signal backstop",
				"thread", threadID, "err", err)
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
	pgid := t.pgid
	_ = t.stdin.Close()
	t.mu.Unlock()
	safe.Go("kimi.stopKillBackstop", func() {
		time.Sleep(5 * time.Second)
		t.mu.Lock()
		stillAlive := t.alive
		t.mu.Unlock()
		if !stillAlive {
			return
		}
		// Kill the whole group (kimi plus anything it spawned), like the
		// interrupt backstop and the probe teardown — not just the leader.
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else if proc != nil {
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
	kind := sessionUpdateKind(params)
	t.mu.Lock()
	// Retain the vocabulary whichever side of the handshake it arrives on:
	// Compact and the usage probe check membership before sending a slash
	// command the CLI might not answer.
	if kind == "available_commands_update" {
		if names := commandNames(params); names != nil {
			t.setCommandsLocked(names)
		}
	}
	tr := t.tr
	if tr == nil {
		// Pre-handshake chatter. The command list is worth keeping — kimi
		// announces it during session setup, and the UI's slash autocomplete
		// wants it — so stash the latest for Start to replay post-handshake.
		if kind == "available_commands_update" {
			t.pendingCmds = append(json.RawMessage(nil), params...)
		}
		t.mu.Unlock()
		return
	}
	loading := t.loading
	t.mu.Unlock()

	events := tr.update(params)
	if kind == "config_option_update" {
		// The CLI just told us what it is really running (its own ExitPlanMode
		// flip lands here too). Keep the thread's record in step, so a resume
		// replays the mode the session ended on rather than the one it started
		// with.
		s.syncApplied(t, tr)
	}
	if loading {
		// session/load replay: buffer until the init event has gone out.
		t.appendReplay(events)
		return
	}
	s.emitEvents(t, events)
}

// sessionUpdateKind peeks at a session/update notification's kind without
// decoding the rest of it.
func sessionUpdateKind(params json.RawMessage) string {
	var probe struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &probe) != nil {
		return ""
	}
	return probe.Update.SessionUpdate
}

// syncApplied copies the translator's current config values onto the thread,
// so Model/Thinking/Mode report what the CLI is running now rather than what
// the handshake asked for.
func (s *Supervisor) syncApplied(t *Thread, tr *translator) {
	model := tr.configValue("model")
	thinking := tr.configValue("thinking")
	mode := tr.configValue("mode")
	t.mu.Lock()
	defer t.mu.Unlock()
	if model != "" {
		t.appliedModel = model
	}
	if thinking != "" {
		t.appliedThinking = thinking
	}
	if mode != "" {
		t.appliedMode = mode
	}
}

// SetConfigOption applies one session config option (model / thinking / mode)
// to a running thread mid-session, via the same session/set_config_option the
// handshake uses. The CLI's rejection (e.g. an unknown value) is returned.
func (s *Supervisor) SetConfigOption(threadID, configID, value string) error {
	t := s.thread(threadID)
	if t == nil {
		return fmt.Errorf("unknown thread %q", threadID)
	}
	t.mu.Lock()
	alive := t.alive
	stopping := t.stopping
	sid := t.sessionID
	t.mu.Unlock()
	if !alive {
		return fmt.Errorf("thread %q is not running", threadID)
	}
	if stopping {
		return fmt.Errorf("thread %q is stopping", threadID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The response carries the authoritative post-change config — kimi may
	// have applied something other than what was asked for. Take its word for
	// it instead of assuming the request landed verbatim.
	var res json.RawMessage
	if err := t.client.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sid,
		"configId":  configID,
		"value":     value,
	}, &res); err != nil {
		return err
	}
	s.applyConfigResponse(t, res)
	return nil
}

// applyConfigResponse folds an authoritative config snapshot into the session
// and announces it, so every consumer converges on what the CLI actually did.
// Shared by the set_config_option response and anything else that returns one.
func (s *Supervisor) applyConfigResponse(t *Thread, res json.RawMessage) {
	opts := decodeConfigOptions(res)
	if len(opts) == 0 {
		return
	}
	t.mu.Lock()
	tr := t.tr
	t.mu.Unlock()
	if tr == nil {
		return
	}
	if ev := tr.applyConfigOptions(opts); ev != nil {
		s.emitEvents(t, []json.RawMessage{ev})
	}
	s.syncApplied(t, tr)
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
		Options []permOption `json:"options"`
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

	// Not every request on this channel is a permission: kimi has no
	// session/request_question, so AskUserQuestion arrives here too, with the
	// user's ANSWERS as the options. Answering that by kind would pick one at
	// random — it needs the human's actual choice.
	if isQuestionRequest(p.Options) {
		s.answerQuestion(t, f, name, input, p.Options)
		return
	}

	allow := false
	if s.perm != nil {
		allow, _ = s.perm(t.ID, name, input)
	}

	optionID := selectPermissionOption(allow, p.Options)
	if optionID == "" {
		// Fail closed, and say so: kimi answers a cancelled outcome with
		// "Tool <name> was not run because the approval request was
		// cancelled", so the turn continues — but from the panel the human's
		// click would otherwise have done nothing, with no explanation.
		t.client.respond(f.ID, map[string]any{
			"outcome": map[string]any{"outcome": "cancelled"},
		})
		s.emitLifecycle(t, "notice", scopeRefusalNote(allow, name))
		return
	}
	t.client.respond(f.ID, map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": optionID},
	})
}

// selectPermissionOption maps the human's one-off decision onto kimi's option
// set by KIND — option ids are kimi-specific ("approve_once", "reject"), the
// kinds are spec-stable. It returns "" when the once-scoped kind is missing,
// which the caller answers as `cancelled`.
//
// SECURITY (audit F27): the match is scope-EXACT, never a fallback across
// scope. The UI offers exactly one-off Approve/Deny, so an Approve answered
// with `allow_always` would install a standing grant the human was never
// offered and no surface mentions — kimi maps its approve_always id to
// `{decision: approved, scope: session}`, i.e. a session-runtime allow rule
// for every later call matching the same matcher. `reject_always` is the
// mirror: one Deny permanently silencing a prompt class. Refusing the turn is
// the fail-closed answer; widening the human's authority is not.
//
// As of kimi 0.30.0 this is defence in depth, not a live escape: every prompt
// the CLI mints carries an allow_once. Its CANONICAL_OPTIONS are
// approve_once/allow_once, approve_always/allow_always, reject/reject_once,
// and the plan_review branch is one allow_once per plan option (or a single
// plan_approve) plus the reject_once exits Revise / Reject and Exit — no
// reject_always kind exists anywhere in the shipped bundle. This guards the
// day that changes, which we would otherwise never notice.
func selectPermissionOption(allow bool, opts []permOption) string {
	want := "reject_once"
	if allow {
		want = "allow_once"
	}
	for _, o := range opts {
		if o.Kind == want {
			return o.OptionID
		}
	}
	return ""
}

// scopeRefusalNote explains a cancelled permission in the panel feed: the
// human clicked, nothing ran, and the reason is a property of the CLI's option
// set rather than anything they did.
func scopeRefusalNote(allow bool, tool string) string {
	if allow {
		return "permission cancelled: this agent offered no one-off approval for " +
			"the tool it named " + quoteUntrusted(tool) + " — only a standing " +
			"always-grant, which Agent Kate never takes on your behalf. " +
			"Nothing was run."
	}
	return "permission cancelled: this agent offered no one-off refusal for " +
		"the tool it named " + quoteUntrusted(tool) + " — only a standing " +
		"never-rule, which Agent Kate never records on your behalf. " +
		"Nothing was run."
}

// maxUntrustedNoteRunes bounds a model-supplied fragment inside a feed row: a
// tool "name" is a CLI/model-supplied title and has no length contract.
const maxUntrustedNoteRunes = 80

// zeroWidthJoiner glues emoji into one cluster (U+200D).
const zeroWidthJoiner = '‍'

// openQuote / closeQuote are the delimiters Agent Kate puts around a fragment
// it did not write. Named, because quoteUntrusted's whole guarantee is that
// neither of these two runes can occur inside the span.
const (
	openQuote  = '“'
	closeQuote = '”'
)

// quoteLookalikes are the runes that RENDER as one of the delimiters above
// without being it. They cannot forge the span — quoteUntrusted folds the
// delimiters themselves — but a reader deciding where Agent Kate's voice stops
// and the agent's begins is reading pixels, not code points, so a fragment
// containing U+275E ❞ ends the quotation as far as the human is concerned.
// Folded to a plain '"', which reads as the agent's own punctuation.
//
// The list is best-effort and says so (see quoteUntrusted): it is the
// double-quote confusables of U+201C/U+201D, not a proof. What is NOT
// best-effort is the delimiter fold and the printable allowlist below.
var quoteLookalikes = map[rune]bool{
	'“': true, '”': true, // “ ” — the delimiters themselves
	'„': true, '‟': true, // „ ‟
	'″': true, '‶': true, // ″ ‶
	'〃': true,                       // 〃
	'〝': true, '〞': true, '〟': true, // 〝 〞 〟
	'＂': true,            // ＂
	'❝': true, '❞': true, // ❝ ❞
	'ʺ': true, '˝': true, 'ˮ': true, // ʺ ˝ ˮ
	'˶': true, '״': true, '⹂': true, // ˶ ״ ⹂
	'\U0001F676': true, '\U0001F677': true, '\U0001F678': true, // 🙶 🙷 🙸
}

// quoteUntrusted wraps CLI/model-supplied text for display inside one of Agent
// Kate's own notes.
//
// SECURITY (audit F27, tightened pass 4): these notes render in the panel's
// system voice — the dim `sys` row Agent Kate uses to speak for itself. The
// fragment reaches that row as escaped plain text (ui/src/AgentPanel.cpp calls
// toHtmlEscaped() on the _lifecycle detail), so this is not injection; it is
// impersonation. Undelimited, a model-chosen tool "name" reads as the app's own
// words, and the surrounding sentence attributes it ("the tool it named") so
// the reader knows whose text it is.
//
// Two guarantees, and they are not the same strength — the earlier version of
// this comment claimed one blanket "cannot be forged", which was true of the
// delimiters and not of what the human sees:
//
//   - GUARANTEED: the outer pair is ours. Both delimiter runes are folded
//     inside the fragment, so the span cannot be closed from within it, and no
//     text after a fake close can be read as Agent Kate speaking again.
//   - BEST-EFFORT: the span LOOKS like one span. Visual confusables of the
//     delimiters (quoteLookalikes) are folded too, but Unicode confusables are
//     an open set and a font can always be found that blurs another pair.
//
// Everything that is not printable is folded to a space, as an allowlist rather
// than a blocklist: unicode.IsGraphic admits only L/M/N/P/S/Zs, which excludes
// control characters (Cc — a newline in a "name" would manufacture a second
// line in a one-line row), FORMAT characters (Cf — bidi overrides such as
// U+202E, which reorder the surrounding sentence on screen; zero-width spaces
// and joiners, which hide word boundaries), line and paragraph separators (Zl,
// Zp), surrogates and private-use runes. A blocklist here would have to be
// re-checked against every Unicode release; the allowlist is closed by
// construction and only ever gets safer.
func quoteUntrusted(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case quoteLookalikes[r]:
			return '"'
		case !unicode.IsGraphic(r):
			return ' '
		}
		return r
	}, s)
	return string(openQuote) + clipRunes(s, maxUntrustedNoteRunes) + string(closeQuote)
}

// clipRunes truncates to at most max runes without splitting a grapheme
// cluster. A naive rune cut strands a combining mark on its own, or halves a
// ZWJ emoji sequence or a two-codepoint flag, which renders as garbage in the
// feed row.
//
// Its only caller is quoteUntrusted, which since pass 4 folds every non-graphic
// rune — the ZWJ and the variation selectors included — so the joiner arms
// below can no longer fire on that path. They are kept, and tested directly:
// this is a general-purpose clip, and the next caller may not pre-fold.
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := max
	// Never cut where the first DROPPED rune continues the one kept before it.
	for cut > 0 && continuesCluster(r[cut]) {
		cut--
	}
	// Never leave a dangling joiner, or half a regional-indicator pair.
	for cut > 0 && (r[cut-1] == zeroWidthJoiner || (isRegionalIndicator(r[cut-1]) && trailingRegionals(r[:cut])%2 == 1)) {
		cut--
	}
	return string(r[:cut]) + "…"
}

// continuesCluster reports whether r attaches to the character before it:
// combining marks, zero-width joiner, variation selectors, emoji skin-tone
// modifiers and tag characters.
func continuesCluster(r rune) bool {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc):
		return true
	case r == zeroWidthJoiner:
		return true
	case r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef:
		return true
	case r >= 0x1f3fb && r <= 0x1f3ff:
		return true
	case r >= 0xe0020 && r <= 0xe007f:
		return true
	}
	return false
}

func isRegionalIndicator(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }

// trailingRegionals counts the regional indicators ending r — a flag is a pair
// of them, so an odd run means the cut lands inside one.
func trailingRegionals(r []rune) int {
	n := 0
	for i := len(r) - 1; i >= 0 && isRegionalIndicator(r[i]); i-- {
		n++
	}
	return n
}

// permOption is one choice offered by session/request_permission. For a real
// permission the ids are kimi-specific and the KINDS carry the meaning; for a
// question (see answerQuestion) it is the other way round — every answer is
// kind "allow_once" and only the id says which answer it is.
type permOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// askUserQuestionTool is the tool name a bridged question is presented under.
// It is claude's, deliberately: the UI already renders a question form for it,
// so the same card serves both engines.
const askUserQuestionTool = "AskUserQuestion"

// questionOptionID matches the option ids kimi mints when it bridges the
// AskUserQuestion tool onto session/request_permission: one q<n>_opt_<i> per
// answer, plus a q<n>_skip. Probed against kimi 0.30.0.
var questionOptionID = regexp.MustCompile(`^q\d+_opt_\d+$`)

// isQuestionRequest reports whether a permission request is really a question.
// The option-id namespace is the tell — the title is not always present, and
// the kinds are indistinguishable from an ordinary allow/reject pair.
func isQuestionRequest(opts []permOption) bool {
	for _, o := range opts {
		if questionOptionID.MatchString(o.OptionID) {
			return true
		}
	}
	return false
}

// answerQuestion puts a bridged AskUserQuestion to the human with its REAL
// answers and sends back the one they picked. The prompt travels down the
// existing permission channel, shaped as claude's AskUserQuestion input
// ({questions:[{question, options:[{label}]}]}) so the UI's question form
// renders it unchanged; the answer comes back as updatedInput and is mapped to
// an option id by label.
//
// Only labels cross the ACP bridge — kimi drops each option's description and
// degrades multi-select before we see it — so nothing richer can be offered.
// The CLI's own skip option is kept in the list: dismissing is a first-class
// answer, and it is the only way to decline without inventing one.
//
// SECURITY (audit F27): always-scoped options are dropped, so no answer on
// this path can install a standing rule. The question card renders a plain
// label with no hint that picking it decides every future prompt of the class,
// and the label is model-chosen, so a "Cancel" wired to `reject_always` would
// buy a standing refusal with a click that reads as declining once. The filter
// is a denylist of the two always kinds rather than an allowlist of the once
// kinds: a future CLI that renames or omits the kind on its answers must still
// be able to ask a question.
func (s *Supervisor) answerQuestion(t *Thread, f acpFrame, name string, rawInput json.RawMessage, opts []permOption) {
	labels := make([]any, 0, len(opts))
	byLabel := make(map[string]string, len(opts))
	for _, o := range opts {
		if isAlwaysScoped(o.Kind) {
			continue
		}
		label := o.Name
		if label == "" {
			label = o.OptionID
		}
		labels = append(labels, map[string]any{"label": label})
		if _, dup := byLabel[label]; !dup {
			byLabel[label] = o.OptionID
		}
	}
	if len(labels) == 0 {
		// Every answer was a standing decision: there is nothing we can put to
		// the human that they are able to answer once, so do not prompt at all.
		t.client.respond(f.ID, map[string]any{
			"outcome": map[string]any{"outcome": "cancelled"},
		})
		s.emitLifecycle(t, "notice", scopeRefusalNote(false, name))
		return
	}
	question := questionText(rawInput)
	input, _ := json.Marshal(map[string]any{
		"questions": []any{map[string]any{
			"question":    question,
			"multiSelect": false,
			"options":     labels,
		}},
	})

	allow, updated := false, json.RawMessage(nil)
	if s.perm != nil {
		allow, updated = s.perm(t.ID, askUserQuestionTool, input)
	}
	optionID := ""
	if allow {
		optionID = byLabel[answeredLabel(updated, question)]
	}
	if optionID == "" {
		// Dismissed, or an answer that matches no option: say so with the
		// CLI's own skip rather than guessing which answer was meant.
		optionID = skipOptionID(opts)
	}
	if optionID == "" {
		// No once-scoped way to decline: cancelling is the fail-closed answer,
		// and the human is told why their dismissal produced nothing (the
		// permission path above does the same).
		t.client.respond(f.ID, map[string]any{
			"outcome": map[string]any{"outcome": "cancelled"},
		})
		s.emitLifecycle(t, "notice", scopeRefusalNote(false, name))
		return
	}
	t.client.respond(f.ID, map[string]any{
		"outcome": map[string]any{"outcome": "selected", "optionId": optionID},
	})
}

// questionText recovers what the agent actually asked from the AskUserQuestion
// tool call's raw input. The permission request itself carries only the
// answers, so a CLI that streamed no usable input leaves the generic prompt.
func questionText(rawInput json.RawMessage) string {
	var in struct {
		Question  string `json:"question"`
		Header    string `json:"header"`
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
		} `json:"questions"`
	}
	if json.Unmarshal(rawInput, &in) == nil {
		for _, q := range []string{
			in.Question, in.Header,
		} {
			if s := strings.TrimSpace(q); s != "" {
				return s
			}
		}
		if len(in.Questions) > 0 {
			for _, q := range []string{in.Questions[0].Question, in.Questions[0].Header} {
				if s := strings.TrimSpace(q); s != "" {
					return s
				}
			}
		}
	}
	return "The agent is asking a question — pick an answer."
}

// answeredLabel pulls the human's choice out of the answered input the UI
// sends back: {"answers": {"<question>": "<label>"}} (a one-element array for
// a multi-select UI). The question key is matched first; a single answer under
// any other key is taken as that answer, since there is only ever one question
// on this path.
func answeredLabel(updated json.RawMessage, question string) string {
	var in struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if json.Unmarshal(updated, &in) != nil || len(in.Answers) == 0 {
		return ""
	}
	raw, ok := in.Answers[question]
	if !ok {
		if len(in.Answers) != 1 {
			return ""
		}
		for _, v := range in.Answers {
			raw = v
		}
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		return many[0]
	}
	return ""
}

// skipOptionID returns the option that declines to answer — kimi's q<n>_skip,
// identified by its reject kind so a renamed id still resolves. It returns ""
// when no once-scoped refusal is on offer, which the caller answers as
// `cancelled`.
//
// SECURITY (audit F27): the match is scope-EXACT, the mirror of
// selectPermissionOption. This is the option Agent Kate picks on the human's
// behalf when they dismiss a question or answer something no option matches —
// a `strings.HasPrefix(o.Kind, "reject")` test resolves `reject_always` just
// as happily, so a dismissal, the most passive act in the UI, would record a
// standing refusal for the whole prompt class that the human was never offered
// and no surface mentions. Declining once is what they asked for; declining
// forever is not.
func skipOptionID(opts []permOption) string {
	for _, o := range opts {
		if o.Kind == "reject_once" {
			return o.OptionID
		}
	}
	return ""
}

// isAlwaysScoped reports whether an option kind records a STANDING decision —
// kimi maps both onto a session-runtime rule that answers every later matching
// prompt without asking. Exact kinds, never a prefix: "always" is the whole
// property being tested for (audit F27).
func isAlwaysScoped(kind string) bool {
	return kind == "allow_always" || kind == "reject_always"
}

// maxStderrTailLines bounds the pre-handshake stderr buffer kept for a failure
// diagnostic — a hung startup could otherwise stream unbounded.
const maxStderrTailLines = 20

func (s *Supervisor) pumpStderr(t *Thread, r io.Reader, done chan<- struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 32*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		s.log.Debug("kimi stderr", "thread", t.ID, "line", line)
		t.mu.Lock()
		live := t.stderrLive
		if !live {
			// Pre-handshake noise: buffer a bounded tail rather than emitting
			// out-of-order cards before the session's init event.
			t.stderrTail = append(t.stderrTail, line)
			if len(t.stderrTail) > maxStderrTailLines {
				t.stderrTail = t.stderrTail[len(t.stderrTail)-maxStderrTailLines:]
			}
		}
		t.mu.Unlock()
		if live {
			s.emitSynthetic(t, "_stderr", line)
		}
	}
}

func (s *Supervisor) reap(t *Thread) {
	defer s.reapWG.Done()
	err := t.cmd.Wait()
	// The pipes are ours (os.Pipe, not cmd.StdoutPipe), so Wait did NOT close
	// them under the readers — that close is what could discard the tail of
	// the stream, i.e. the final turn's frames (audit F24). Give the readers
	// their moment to reach real EOF so those frames land in the event log —
	// kimi's only transcript — BEFORE the "exited" lifecycle event closes it,
	// not after (audit F51: waiting here at thread start instead burnt the
	// whole grace against channels that only close at EOF, leaving zero drain
	// wait at actual exit).
	// One absolute deadline shared by both waits (a single Timer would be
	// consumed by the first and could never fire for the second).
	end := time.Now().Add(s.drainGrace)
	for _, drained := range []chan struct{}{t.stdoutDrained, t.stderrDrained} {
		if drained == nil {
			continue
		}
		select {
		case <-drained:
		case <-time.After(time.Until(end)):
			s.log.Warn("output reader still running at reap; proceeding without its tail",
				"thread", t.ID)
		}
	}

	t.mu.Lock()
	t.alive = false
	interrupted := t.interrupted
	// Release any Agent-Kate-owned turn still waiting: its reply isn't coming.
	if t.internal != nil {
		t.internal.finish("", fmt.Errorf("agent exited before the %s turn completed",
			t.internal.kind))
		t.internal = nil
	}
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
			n, _ := t.logFile.Write(append(ev, '\n'))
			t.logBytes += int64(n)
		}
		// Retention: an append-only log that nothing ever deletes is a slow
		// disk-fill on the user (audit F10).
		if t.logBytes > maxEventLogBytes {
			s.trimEventLogLocked(t)
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
