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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

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

	// One-shot cache of the CLI's long-option vocabulary (see cliflags.go), so
	// the two version-sensitive launch flags can be omitted on a binary that
	// does not know them instead of killing the spawn. Its own mutex: the probe
	// runs a subprocess and must not be serialised behind thread bookkeeping.
	flagMu    sync.Mutex
	flagCache flagProbeResult

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
	Kind      string `json:"kind"` // "image" or "text"
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
	// CachePath is the UI's own durable copy of an image attachment's bytes.
	// Also never sent to the model. Path may point at a temp file the capture
	// tool has already reaped, in which case the replayed chip has nothing to
	// draw its thumbnail from; this survives that.
	CachePath string `json:"cachePath,omitempty"`
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
	Provider       *Provider    // optional third-party API routing; nil/empty BaseURL = Claude direct
	SystemPrompt   string       // claude --append-system-prompt; empty = none
	AgentsJSON     string       // claude --agents payload, pre-rendered by the adapter; empty = none
	// systemPromptFile is set by Start, never by a caller: the path of the
	// 0600 temp file holding SystemPrompt. When it is set the persona travels
	// as --append-system-prompt-file instead of as an argv element, keeping it
	// out of /proc/<pid>/cmdline (audit F23). Unexported so the field cannot be
	// used to make `claude` read an arbitrary file chosen elsewhere.
	systemPromptFile string
	// Env overlays the child's environment (applied AFTER provider routing, so
	// a caller cannot silently redirect a routed thread's endpoint by ordering).
	// See harness.StartSpec.Env for why this never comes from an agent.
	Env map[string]string
	// The plan 16 P6 sweep, straight through from StartSpec.
	FallbackModels  []string // claude --fallback-model (comma-joined)
	DisallowedTools []string // claude --disallowedTools
	AddDirs         []string // claude --add-dir
	// The control-channel sweep. All three verified present in
	// `claude -p --help` on 2.1.220.
	StrictMCPConfig bool    // claude --strict-mcp-config: ignore the user's global MCP servers
	MaxBudgetUSD    float64 // claude --max-budget-usd; 0 = uncapped
	Title           string  // claude --name: the session label `claude agents` lists
}

// buildStartArgs assembles the `claude` argv for one thread. Split out of
// Start so the flag plumbing is testable without spawning the CLI — the
// process-shaping decisions (env, process group, pipes) stay in Start.
//
// flags is the installed CLI's probed long-option vocabulary (cliflags.go), and
// comes first because it describes the binary rather than the thread; nil means
// "unprobed", which treats every flag as supported.
func buildStartArgs(flags cliFlags, opts StartOptions) []string {
	mode := opts.PermissionMode
	if mode == "" {
		mode = "acceptEdits"
	}
	// Both MCP servers are always allowed. The Cowork server is wired in for
	// every thread so it can be switched on mid-session (it advertises no tools
	// until then), and a server-prefix allow-list covers tools that appear
	// later — verified against claude 2.1.220, where a tool revealed by
	// tools/list_changed was callable under this same allow entry. Consent for
	// individual desktop actions is enforced server-side (the cowork consent
	// authority), NOT by this allow-list — so --permission-mode cannot bypass
	// it, and neither can an allow entry for a thread that never opted in.
	allowedTools := "mcp__cooperation,mcp__cowork"
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-mode", mode,
		"--allowedTools", allowedTools,
	}
	// Token-by-token output: the CLI adds `stream_event` lines carrying the
	// raw Anthropic SSE shape (message_start / content_block_start /
	// content_block_delta / …) alongside the authoritative `assistant`
	// event, which still arrives afterwards. The UI paints the deltas into
	// a provisional row and replaces it when the authoritative event lands,
	// so nothing downstream — including replay of a stored transcript,
	// which holds no stream_events — changes shape.
	//
	// GATED, unlike the rest of this fixed prefix: it is a recent flag, and an
	// older claude aborts on an unknown option, so appending it unconditionally
	// would make the newest CLI a hard requirement for every launch. Omitted,
	// the stream simply degrades to whole-message rendering.
	if flags.supports(flagIncludePartialMessages) {
		args = append(args, flagIncludePartialMessages)
	}
	// Subagent output is forwarded onto this stream tagged with
	// parent_tool_use_id instead of being visible only by tailing the
	// subagent's transcript file after the fact. Gated for the same reason;
	// omitted, subagent turns remain readable through the on-disk subagent
	// transcripts.
	if flags.supports(flagForwardSubagentText) {
		args = append(args, flagForwardSubagentText)
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
	// Persona text rides ALONGSIDE Claude Code's own system prompt (verified
	// in print mode against claude 2.1.220); --system-prompt would replace it,
	// hiding the tool and skill injections, so it is deliberately not used.
	//
	// The file form is preferred when the CLI has it (Start writes a 0600 temp
	// file): argv is world-readable through /proc/<pid>/cmdline for the life of
	// the process, and a persona can carry instructions and context the human
	// would not publish to every local user (audit F23). The inline form
	// remains the fallback for a CLI that does not advertise the file flag —
	// dropping the persona instead would silently change what the agent is.
	if strings.TrimSpace(opts.SystemPrompt) != "" {
		if opts.systemPromptFile != "" {
			args = append(args, flagAppendSystemPromptFile, opts.systemPromptFile)
		} else {
			args = append(args, "--append-system-prompt", opts.SystemPrompt)
		}
	}
	// NOT moved off argv: --agents has no file form on claude 2.1.220
	// (`--agents-file` is rejected as an unknown option — probed live). Custom
	// subagent definitions therefore remain visible in /proc/<pid>/cmdline;
	// documented in docs/security-model.md.
	// Custom subagent definitions for this session, already rendered into the
	// CLI's JSON-object shape by the adapter (harness_claude.go owns that
	// vocabulary, including which fields the binary honors).
	if opts.AgentsJSON != "" {
		args = append(args, "--agents", opts.AgentsJSON)
	}
	// The launch-option sweep (plan 16 P6), all verified present on claude
	// 2.1.220. --fallback-model takes ONE comma-separated value; the other two
	// are variadic (`<tools...>`, `<directories...>`), which is why each value
	// is passed as its own flag occurrence: a variadic flag greedily eats the
	// argv that follows it. (Our prompt travels over stdin as stream-json, not
	// as a trailing positional, so nothing downstream is at risk either way —
	// but one value per occurrence keeps that true if that ever changes.)
	if len(opts.FallbackModels) > 0 {
		args = append(args, "--fallback-model", strings.Join(opts.FallbackModels, ","))
	}
	for _, tool := range opts.DisallowedTools {
		if strings.TrimSpace(tool) != "" {
			args = append(args, "--disallowedTools", tool)
		}
	}
	for _, dir := range opts.AddDirs {
		if strings.TrimSpace(dir) != "" {
			args = append(args, "--add-dir", dir)
		}
	}
	// Isolate the thread from whatever MCP servers the human has configured
	// globally: only the servers we pass with --mcp-config exist. Off by
	// default, because the global set is usually the point.
	if opts.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	// A hard spend ceiling for this session. The CLI ends the turn with an
	// error result once it trips, which the panel already surfaces.
	if opts.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd",
			strconv.FormatFloat(opts.MaxBudgetUSD, 'f', -1, 64))
	}
	// The thread's own title, so a session started here is identifiable in
	// `claude agents` rather than showing up as an anonymous print-mode run.
	// truncateName trims for itself and comes back empty only for a title that
	// was blank to begin with, which must not become `--name ""`.
	if name := truncateName(opts.Title); name != "" {
		args = append(args, "--name", name)
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
			"--permission-prompt-tool", "mcp__cooperation__request_permission")
	}
	return args
}

// writePersonaFile stages a thread's persona text in an owner-only temp file
// for --append-system-prompt-file, and returns its path.
//
// Why a file at all: argv is public. /proc/<pid>/cmdline is world-readable on
// Linux, so `ps` shows every local user the whole persona for the life of the
// process — and a persona is exactly where a human puts private standing
// instructions and context (audit F23). The file is 0600 in the private temp
// dir and is unlinked when the thread is reaped.
//
// FAIL CLOSED: any error is returned, and Start turns it into a failed launch.
// The two silent alternatives are both worse — falling back to argv would make
// the private path an illusion that disappears under load, and launching
// without the persona would hand the human a different agent than the one they
// configured, with nothing on screen to say so.
//
// os.CreateTemp is what creates the file, so the mode is 0600 from the first
// byte (never 0644-then-chmod, which would be a window, however short). The
// explicit Chmod is belt-and-braces for a future CreateTemp with a different
// default; it costs one syscall per launch.
func writePersonaFile(threadID, prompt string) (string, error) {
	f, err := os.CreateTemp("", "agentkate-persona-"+threadID+"-*.txt")
	if err != nil {
		return "", fmt.Errorf("persona file: %w", err)
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("persona file: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.WriteString(prompt); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("persona file: %w", err)
	}
	return path, nil
}

// maxNameBytes bounds --name. Titles are summarised prompts, which can run
// long; the CLI has no documented limit, so this keeps the label readable in
// `claude agents` and the argv element small.
const maxNameBytes = 120

// truncateName trims a title and clips it to maxNameBytes on a rune boundary.
//
// The result is empty ONLY when the title was blank (empty or all whitespace),
// which callers must treat as "no name" rather than a name of "". The clip
// path cannot produce it: the leading TrimSpace guarantees a non-space first
// rune, so neither the UTF-8 back-off (which never eats a valid leading rune)
// nor the trailing trim can consume the whole clip.
func truncateName(title string) string {
	title = strings.TrimSpace(title)
	if len(title) <= maxNameBytes {
		return title
	}
	clipped := title[:maxNameBytes]
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return strings.TrimSpace(clipped)
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
	personaFile string      // temp 0600 --append-system-prompt-file to clean up on exit
	meter       *toolMeter  // measures tool_result sizes for token-cost telemetry
	usage       *usageMeter // measures per-turn LLM token usage and billed cost
	alive       bool
	pgid        int  // process-group id (== leader pid); signalled by Interrupt
	interrupted bool // set by Interrupt so reap() reports a user-interrupt if the process dies
	aborting    bool // set by Interrupt while an in-band abort is pending; cleared on the aborted turn's result
	stopping    bool // a Stop is in flight; suppresses turn_aborted and rejects new Sends
	// stdoutDrained / stderrDrained are closed when the respective output pump
	// returns. reap() waits on them before cmd.Wait, which closes the pipes.
	stdoutDrained chan struct{}
	stderrDrained chan struct{}
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
	controls map[string]chan controlOutcome
	// ctxProbePending guards the post-turn get_context_usage probe: one in
	// flight at a time, so a burst of quick turns cannot stack up probes (each
	// of which holds a controls entry and a goroutine for up to controlTimeout).
	ctxProbePending bool
	hotCompact      *hotCompact // when non-nil, the next assistant turn is captured for a summary

	co *coalescer // batches this thread's events before they reach the emit callback

	// wmu serializes writes to the child's stdin and is NEVER held together
	// with mu. Writing under mu was the wedge in audit F9: a message larger
	// than the 64 KiB pipe buffer (any base64 image attachment) blocks until
	// the CLI drains it, and a `claude` that stops reading stdin therefore
	// parked mu forever — Interrupt, Stop/closeStdin, abortPending and
	// pumpStdout all serialize behind it, leaving the thread unkillable from
	// the UI. State is decided under mu; the write happens after mu is
	// released. This mirrors the kimi backend's dedicated write mutex.
	wmu sync.Mutex
	// writeBroken latches once a stdin write fails or times out. The frame that
	// failed may have been written in part, so the stream's framing is no longer
	// trustworthy: every later write fails fast instead of appending a fragment
	// to a torn line. Guarded by wmu.
	writeBroken bool
}

// stdinWriteTimeout bounds a single frame write to the child's stdin. The pipe
// is pollable, so a deadline turns "wedged CLI" from an unbounded park into a
// bounded, reportable failure. Generous enough that a large attachment on a
// healthy-but-busy CLI never trips it.
const stdinWriteTimeout = 30 * time.Second

// deadlineWriter is the subset of *os.File a pipe from exec.Cmd.StdinPipe
// satisfies. Type-asserted rather than required: if a future writer cannot take
// a deadline we still write (correctness first), we just lose the bound.
type deadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

// errStdinBroken is returned once the stdin stream's framing can no longer be
// trusted (see Thread.writeBroken).
var errStdinBroken = errors.New("agent stdin is no longer writable (a previous frame failed mid-write)")

// writeFrame writes one newline-terminated frame to the child's stdin under
// wmu, with mu NOT held. t.stdin is assigned once at construction and never
// reassigned, so reading it here without mu is safe.
func (t *Thread) writeFrame(frame []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.writeBroken {
		return errStdinBroken
	}
	if dw, ok := t.stdin.(deadlineWriter); ok {
		if err := dw.SetWriteDeadline(time.Now().Add(stdinWriteTimeout)); err == nil {
			defer func() { _ = dw.SetWriteDeadline(time.Time{}) }()
		}
	}
	if _, err := t.stdin.Write(frame); err != nil {
		// Includes the deadline case (os.ErrDeadlineExceeded): a partial frame
		// is on the wire, so refuse every later write rather than corrupt the
		// CLI's parser with a fragment.
		t.writeBroken = true
		return err
	}
	return nil
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
// means the subtype succeeded. payload carries the whole `response` object, so
// the read-only subtypes (get_context_usage, list_models) can be answered with
// data rather than only a verdict.
type controlOutcome struct {
	err     string
	payload json.RawMessage
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
	// Probe the binary's option vocabulary before shaping the argv: cached after
	// the first launch, and the only thing it can change is whether the two
	// optional streaming flags are appended.
	flags := s.supportedFlags()
	// Set once the Thread is registered and owns its temp files; until then the
	// deferred cleanups below unlink them on every error return.
	spawned := false
	// Persona text goes to a private file rather than into argv when the CLI
	// can read it from one (audit F23). writePersonaFile fails closed: if it
	// cannot create an owner-only file it returns an error and the launch
	// fails, rather than quietly falling back to the world-readable argv form
	// or quietly dropping the persona. A CLI without the flag is a different
	// case — nothing failed, the capability simply is not there — and keeps the
	// inline form (see buildStartArgs).
	if strings.TrimSpace(opts.SystemPrompt) != "" && flags.supports(flagAppendSystemPromptFile) {
		path, err := writePersonaFile(opts.ID, opts.SystemPrompt)
		if err != nil {
			return nil, err
		}
		opts.systemPromptFile = path
		// Every failure path below returns before the Thread exists to own the
		// file; on success reap() unlinks it, like the --mcp-config file.
		defer func() {
			if !spawned {
				_ = os.Remove(opts.systemPromptFile)
			}
		}()
	}
	cmd := exec.Command(s.claudeBin, buildStartArgs(flags, opts)...)
	cmd.Dir = opts.WorkDir
	// Route this child at a third-party Anthropic-compatible endpoint when a
	// provider is selected; buildEnv scrubs any inherited Anthropic credentials
	// first so a real Claude key is never forwarded to someone else's base URL.
	// Claude-direct threads get os.Environ() back unchanged.
	env, err := buildEnv(os.Environ(), opts.Provider)
	if err != nil {
		return nil, err
	}
	// The caller's per-thread overlay goes on last (e.g. a CLI-state home dir).
	cmd.Env = ApplyEnvOverlay(env, opts.Env)
	// Put the agent in its own process group so Interrupt() can signal the whole
	// group (claude + any tools / MCP subprocesses it spawns) rather than
	// orphaning children. The group id equals the leader's pid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	// Deliberately os.Pipe + cmd.Stdout, NOT cmd.StdoutPipe (audit F24):
	// cmd.Wait closes the pipes it created as part of reaping, so a Wait that
	// wins the race against the pump discards whatever is still in the pipe —
	// and that tail is the turn's final `result` event. A pipe we own is closed
	// by us, at real EOF. The child's copies of the write ends are closed right
	// after Start, so EOF still arrives the moment the process (and anything it
	// leaked the fds to) is gone.
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
	// pumping is set once the reader goroutines own the read ends and will
	// close them at EOF. Until then every failure exit must close them here, or
	// each failed spawn leaks two descriptors.
	pumping := false
	defer func() {
		// The parent has no use for the write ends once the child holds them.
		stdoutW.Close()
		stderrW.Close()
		if !pumping {
			stdout.Close()
			stderr.Close()
		}
	}()

	id := opts.ID
	if id == "" {
		id = NewThreadID()
	}
	t := &Thread{
		ID:          id,
		WorkDir:     opts.WorkDir,
		cmd:         cmd,
		stdin:       stdin,
		mcpConfig:   opts.MCPConfig,
		personaFile: opts.systemPromptFile,
		meter:       newToolMeter(s.log, id),
		usage:       newUsageMeter(s.log, id),
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
	// From here the Thread owns the persona file; reap() unlinks it.
	spawned = true

	// The "started" lifecycle event is emitted by the orchestration layer
	// once the thread id is known to the UI; here we only log.
	provider := ""
	if opts.Provider.Routed() {
		provider = opts.Provider.ID // id/base URL only — never the token
	}
	s.log.Info("agent process spawned", "thread", t.ID, "dir", opts.WorkDir, "pid", cmd.Process.Pid, "provider", provider)

	// reap() waits on these before cmd.Wait(): os/exec closes the pipes as part
	// of Wait, so calling it while a pump is still reading can discard the tail
	// of the stream — and the tail is the turn's `result` event (audit F24).
	t.stdoutDrained = make(chan struct{})
	t.stderrDrained = make(chan struct{})
	pumping = true
	safe.Go("agent.pumpStdout", func() {
		defer close(t.stdoutDrained)
		defer stdout.Close()
		s.pumpStdout(t, stdout)
	})
	safe.Go("agent.pumpStderr", func() {
		defer close(t.stderrDrained)
		defer stderr.Close()
		s.pumpStderr(t, stderr)
	})
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
	// Decide under mu, write outside it (audit F9): buildUserContent routinely
	// produces messages far larger than the pipe buffer, and blocking on that
	// write while holding mu is what made a wedged CLI unkillable.
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return fmt.Errorf("thread %q is not running", threadID)
	}
	if t.stopping {
		t.mu.Unlock()
		return fmt.Errorf("thread %q is stopping", threadID)
	}
	// Count the turn BEFORE the write: the CLI can emit its result the instant
	// the frame lands, and pumpStdout's decrement must never run first.
	t.turnsInFlight++
	t.mu.Unlock()

	if err := t.writeFrame(append(msg, '\n')); err != nil {
		t.mu.Lock()
		if t.turnsInFlight > 0 {
			t.turnsInFlight-- // the turn never started
		}
		t.mu.Unlock()
		return fmt.Errorf("write to agent: %w", err)
	}
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
	t.mu.Unlock()
	// In-band abort. stdin stays OPEN so the process stays resident for the next
	// message.
	//
	// The write is ASYNCHRONOUS and holds no thread lock (audit F9). Interrupt
	// is the UI's escape hatch, so it must return promptly even when the child
	// has stopped draining stdin: in that state the pipe is full and this write
	// would block behind whatever large Send filled it. The signal backstop
	// below is exactly the recovery for "the frame never landed", so an
	// in-flight write is allowed to lose the race with it.
	safe.Go("agent.interruptFrame", func() {
		if werr := t.writeFrame(append(frame, '\n')); werr != nil {
			s.log.Warn("interrupt frame write failed; relying on signal backstop",
				"thread", threadID, "err", werr)
		}
	})

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

// ClaudeModel is one entry of the CLI's live model vocabulary as reported by
// `claude -p /model`. Value is the token passed to `--model` (an alias like
// "opus" that resolves to the newest model, a "[1m]" variant, or a full id);
// Name is a display label.
type ClaudeModel struct {
	Value string
	Name  string
	// Efforts are the reasoning-effort tiers this model supports, as reported
	// by the list_models control request. Empty means the CLI said nothing —
	// which callers must read as "every tier", never as "none".
	Efforts []string
}

// DiscoverModels enumerates the CLI's live model vocabulary, cheapest source
// first:
//
//  1. the list_models control request on a thread the human already has open —
//     free, and it carries per-model effort support;
//  2. the same control request against a throwaway stream-json session (no turn
//     is started, so nothing is billed);
//  3. the legacy `claude -p /model` prose parse, for a CLI too old to know the
//     subtype.
//
// Best-effort throughout: every failure falls through, and an exhausted chain
// returns an empty slice rather than an error, so callers leave a prior cache
// intact instead of blanking the picker.
func (s *Supervisor) DiscoverModels(ctx context.Context) ([]ClaudeModel, error) {
	if id := s.anyRunningThread(); id != "" {
		if models, err := s.listModels(id); err == nil && len(models) > 0 {
			return models, nil
		} else if err != nil {
			s.log.Debug("list_models on a live thread failed; probing", "thread", id, "err", err)
		}
	}
	if models, err := s.listModelsProbe(ctx); err == nil && len(models) > 0 {
		return models, nil
	} else if err != nil {
		s.log.Debug("list_models probe failed; falling back to /model prose", "err", err)
	}
	cmd := exec.CommandContext(ctx, s.claudeBin, "-p", "/model")
	out, err := cmd.CombinedOutput()
	if err != nil {
		// An unauthenticated or missing CLI yields nothing to cache, not a hard
		// error that would blank the picker.
		return nil, nil
	}
	return parseClaudeModelList(string(out)), nil
}

// parseClaudeModelList extracts the alias vocabulary from `claude -p /model`
// output, e.g.:
//
//	Current model: Opus 5 (1M context) (default)
//	Usage: /model <name>. Available: sonnet, opus, haiku, fable, best,
//	  sonnet[1m], opus[1m], fable[1m], opusplan, default, or a full model ID.
func parseClaudeModelList(out string) []ClaudeModel {
	const marker = "Available:"
	i := strings.Index(out, marker)
	if i < 0 {
		return nil
	}
	list := out[i+len(marker):]
	if dot := strings.IndexByte(list, '.'); dot >= 0 {
		list = list[:dot] // the sentence ends at the first period after the list
	}
	current := parseCurrentModel(out)
	var models []ClaudeModel
	seen := map[string]bool{}
	for _, tok := range strings.Split(list, ",") {
		v := strings.TrimSpace(tok)
		// Drop the trailing "or a full model ID" prose, whether comma-separated
		// or glued onto the last alias.
		if low := strings.ToLower(v); strings.HasPrefix(low, "or a full model") {
			continue
		} else if idx := strings.Index(low, "or a full model"); idx >= 0 {
			v = strings.TrimSpace(v[:idx])
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		name := prettyModelAlias(v)
		if strings.EqualFold(v, "default") && current != "" {
			name = "Default (" + current + ")"
		}
		models = append(models, ClaudeModel{Value: v, Name: name})
	}
	return models
}

// parseCurrentModel pulls the short name from a "Current model: <name> (…)" line.
func parseCurrentModel(out string) string {
	const marker = "Current model:"
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	line := out[i+len(marker):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	if p := strings.IndexByte(line, '('); p >= 0 {
		line = line[:p] // drop "(1M context)" / "(default)" annotations
	}
	return strings.TrimSpace(line)
}

// prettyModelAlias turns "opus" → "Opus" and "opus[1m]" → "Opus (1M)".
func prettyModelAlias(v string) string {
	base, oneM := v, false
	if strings.HasSuffix(strings.ToLower(v), "[1m]") {
		base, oneM = v[:len(v)-4], true
	}
	if base != "" {
		base = strings.ToUpper(base[:1]) + base[1:]
	}
	if oneM {
		base += " (1M)"
	}
	return base
}

// controlTimeout bounds how long a control request may wait for its response.
// The CLI answers set_model / set_permission_mode immediately (~ms), so a
// timeout means the process is wedged or dying.
const controlTimeout = 10 * time.Second

// reloadSkillsTimeout is deliberately shorter: reload_skills is broadcast to
// every running thread at once when a skill is installed, so a wedged CLI's
// share of the wait is paid by an interactive RPC rather than by a background
// probe. Re-reading a skill directory is local work — a CLI that has not
// answered in this long is not going to.
const reloadSkillsTimeout = 3 * time.Second

// controlTimeoutFor reports how long one control subtype may wait.
func controlTimeoutFor(subtype string) time.Duration {
	if subtype == "reload_skills" {
		return reloadSkillsTimeout
	}
	return controlTimeout
}

// sendControl writes one control_request and waits (bounded) for its
// control_response, returning the CLI's error verbatim if it rejected the
// request.
func (s *Supervisor) sendControl(threadID, subtype string, fields map[string]any) error {
	_, err := s.sendControlResult(threadID, subtype, fields)
	return err
}

// sendControlResult is sendControl with the answer kept: it returns the whole
// `response` object of the matching control_response, which the read-only
// subtypes (get_context_usage, list_models) carry their data in.
func (s *Supervisor) sendControlResult(threadID, subtype string, fields map[string]any) (json.RawMessage, error) {
	t := s.thread(threadID)
	if t == nil {
		return nil, fmt.Errorf("unknown thread %q", threadID)
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
		return nil, err
	}
	ch := make(chan controlOutcome, 1)
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q is not running", threadID)
	}
	if t.stopping {
		t.mu.Unlock()
		return nil, fmt.Errorf("thread %q is stopping", threadID)
	}
	if t.controls == nil {
		t.controls = make(map[string]chan controlOutcome)
	}
	t.controls[reqID] = ch
	t.mu.Unlock()
	// Register first, write with mu released (audit F9). Registering before the
	// write also removes the race where the response arrives before the waiter
	// exists.
	if werr := t.writeFrame(append(frame, '\n')); werr != nil {
		t.mu.Lock()
		delete(t.controls, reqID)
		t.mu.Unlock()
		return nil, fmt.Errorf("write to agent: %w", werr)
	}

	select {
	case out := <-ch:
		if out.err != "" {
			return nil, fmt.Errorf("%s", out.err)
		}
		return out.payload, nil
	case <-time.After(controlTimeoutFor(subtype)):
		t.mu.Lock()
		delete(t.controls, reqID)
		t.mu.Unlock()
		return nil, fmt.Errorf("%s: no response from the agent", subtype)
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
		boundary, dedup, subagent, typ := classifyEvent(raw)
		// Telemetry: where context tokens go (tool outputs) and what we are
		// billed for (per-turn usage). Both observe; neither alters the stream.
		// Subagent-forwarded events (--forward-subagent-text, tagged with
		// parent_tool_use_id) are excluded: their tokens are the subagent's own
		// and are already billed inside the parent's Task tool_result, so
		// metering them here double-counts, and their tool_use/tool_result ids
		// belong to a stream this meter never sees both halves of. They still
		// reach the UI, which renders them as the subagent's transcript.
		if !subagent {
			t.meter.Observe(raw)
			t.usage.Observe(raw)
		}
		t.co.add(json.RawMessage(raw), boundary, dedup)
		// A `result` ends a turn: decrement the in-flight count and complete a
		// pending in-band interrupt. Done AFTER the result event itself is
		// buffered so the turn_aborted lifecycle event orders behind it. A
		// subagent's own result never ends the parent's turn.
		if typ == "result" && !subagent {
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
// force an immediate coalescer flush (a semantic boundary), whether it is a
// candidate for byte-identical dedup within a batch, and whether it was
// forwarded from a subagent, and reports the event's type so the caller can
// track turn ends and control responses without re-parsing.
//
// subagent is true when the event carries a parent_tool_use_id — the tag
// --forward-subagent-text puts on everything a child session emits. Such
// events are display-only for us: they are not the parent thread's usage, not
// its tool calls, and not its turn boundaries.
//
// Boundaries are kept conservative: a `result` (turn end) and any assistant
// turn carrying a tool_use block (the UI renders a tool card and may gate on
// it) flush right away so latency-sensitive UI never waits on the timer.
// Plain assistant text events are dedup candidates because `claude --verbose`
// can repeat an identical partial snapshot; only exact duplicates are dropped,
// so no content is ever lost.
func classifyEvent(raw json.RawMessage) (boundary, dedup, subagent bool, typ string) {
	var head struct {
		Type            string `json:"type"`
		ParentToolUseID string `json:"parent_tool_use_id"`
		Message         struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return false, false, false, ""
	}
	subagent = head.ParentToolUseID != ""
	switch head.Type {
	case "result":
		return true, false, subagent, head.Type
	case "assistant":
		for _, blk := range head.Message.Content {
			if blk.Type == "tool_use" {
				return true, false, subagent, head.Type
			}
		}
		return false, true, subagent, head.Type
	}
	// Everything else — notably `stream_event`, the token-by-token deltas from
	// --include-partial-messages — is neither a boundary nor a dedup
	// candidate. Deltas in particular must never be deduped: two
	// byte-identical " the" deltas in one batch are both real text, and they
	// are exactly what the coalescer exists to batch, with the authoritative
	// `assistant` event that follows carrying the semantics the UI gates on.
	return false, false, subagent, head.Type
}

// observeControlResponse completes the waiter for one control_response. The
// response's request_id keys the controls map; responses nobody registered
// for (e.g. an interrupt ack) are ignored.
func observeControlResponse(t *Thread, raw json.RawMessage) {
	var head struct {
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(raw, &head) != nil || len(head.Response) == 0 {
		return
	}
	var meta struct {
		RequestID string `json:"request_id"`
		Subtype   string `json:"subtype"`
		Error     string `json:"error"`
	}
	if json.Unmarshal(head.Response, &meta) != nil || meta.RequestID == "" {
		return
	}
	t.mu.Lock()
	ch := t.controls[meta.RequestID]
	delete(t.controls, meta.RequestID)
	t.mu.Unlock()
	if ch == nil {
		return
	}
	out := controlOutcome{payload: head.Response}
	if meta.Subtype == "error" {
		out.err = meta.Error
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
		Type string `json:"type"`
		// The --forward-subagent-text tag. A helper session's events are
		// interleaved with the parent's on this one stdout, so without this
		// filter a Task the compaction prompt happened to spawn would write
		// ITS prose into the summary and, worse, END the capture on its own
		// `result` — completing the compaction with a truncated, foreign
		// summary. Same exclusion classifyEvent, the turn-end accounting and
		// the meters apply, for the same reason.
		ParentToolUseID string `json:"parent_tool_use_id"`
		Message         struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return
	}
	if head.ParentToolUseID != "" {
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
	// The turn is over and the CLI is idle: ask it what the context actually
	// holds now, and ship the answer as a `_context` event. This is the only
	// moment the figure is both stable and interesting.
	if !stopping {
		s.reportContextUsage(t)
	}
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

// drainGrace bounds how long reap waits for the output pumps once the process
// is gone. EOF normally arrives immediately; a pump can only outlive the
// process if a grandchild the CLI leaked still holds the pipe, and waiting on
// that forever would leak the reaper — and with it the thread's "exited" event.
const drainGrace = 5 * time.Second

func (s *Supervisor) reap(t *Thread) {
	defer s.reapWG.Done()
	err := t.cmd.Wait()
	// The pipes are ours (os.Pipe, not cmd.StdoutPipe), so Wait did NOT close
	// them under the pumps — that close is what could discard the tail of the
	// stream, i.e. the turn's final `result` event (audit F24). Give the pumps
	// their moment to reach real EOF so their last events are emitted BEFORE
	// the "exited" lifecycle event, not after it.
	// One absolute deadline shared by both waits (a single Timer would be
	// consumed by the first and could never fire for the second).
	end := time.Now().Add(drainGrace)
	for _, drained := range []chan struct{}{t.stdoutDrained, t.stderrDrained} {
		if drained == nil {
			continue
		}
		select {
		case <-drained:
		case <-time.After(time.Until(end)):
			s.log.Warn("output pump still running at reap; proceeding without its tail",
				"thread", t.ID)
		}
	}

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
	if t.personaFile != "" {
		_ = os.Remove(t.personaFile)
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
