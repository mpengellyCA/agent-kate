// Command akcore is the Agent Kate orchestration core. It supervises agent and
// language-server subprocesses and exposes them to the agentkate UI over a
// local JSON-RPC bus. The UI normally spawns this binary itself.
//
// Invoked as `akcore mcp ...` it instead runs the Cooperation MCP stdio bridge
// (see runMCPBridge); the default invocation runs the core.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/coop"
	"agentkate/internal/cowork"
	"agentkate/internal/gitstatus"
	"agentkate/internal/ipc"
	"agentkate/internal/kde"
	"agentkate/internal/permission"
	"agentkate/internal/safe"
	"agentkate/internal/search"
	"agentkate/internal/session"
	"agentkate/internal/skills"
	"agentkate/internal/vsix"
	"agentkate/internal/worktree"
)

// version is the akcore protocol/build version reported in the handshake.
// The default is overridden at build time via -ldflags "-X main.version=..."
// (see CMakeLists.txt) so it tracks MAJOR.MINOR.<commits-on-main>.
var version = "0.1.0"

// defaultSocketPath mirrors the path the UI computes, so a manually launched
// core and UI still meet. The UI normally passes --socket explicitly.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agentkate.sock")
	}
	return filepath.Join(os.TempDir(), "agentkate.sock")
}

func main() {
	// Desktop-launched runs inherit a minimal PATH that often omits user bin
	// dirs like ~/.local/bin where `claude` lives. Augment it before anything
	// else so subprocesses (claude, git, gh, ...) resolve the same way they
	// do under a terminal-launched dev build.
	augmentPath()

	// Subcommand dispatch: `akcore mcp` is the Cooperation MCP stdio bridge.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		runMCPBridge(os.Args[2:])
		return
	}
	runCore()
}

// threadRegistry maps agent thread ids to the worktree each runs in.
type threadRegistry struct {
	mu sync.Mutex
	wt map[string]worktree.Worktree
}

func newThreadRegistry() *threadRegistry {
	return &threadRegistry{wt: make(map[string]worktree.Worktree)}
}

func (r *threadRegistry) put(id string, w worktree.Worktree) {
	r.mu.Lock()
	r.wt[id] = w
	r.mu.Unlock()
}

func (r *threadRegistry) get(id string) (worktree.Worktree, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.wt[id]
	return w, ok
}

func (r *threadRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.wt, id)
	r.mu.Unlock()
}

// analyzeCleanupCandidates classifies every worktree in a project as a removal
// candidate. It analyses each live snapshot (filtered to the project's repo
// root) plus synthesises orphaned candidates from session records whose
// worktree directory no longer exists on disk and which the cache no longer
// tracks. Pure read.
func analyzeCleanupCandidates(d handlerDeps, project string) []gitstatus.CleanupCandidate {
	cands := make([]gitstatus.CleanupCandidate, 0)
	seen := make(map[string]bool)

	for _, snap := range d.gitCache.Snapshots() {
		if project != "" && snap.RepoRoot != project {
			continue
		}
		seen[snap.ThreadID] = true
		wt, ok := d.threads.get(snap.ThreadID)
		if !ok {
			// Reconstruct a worktree from the snapshot when the registry has no
			// entry (e.g. a record re-registered on restart).
			wt = worktree.Worktree{
				ThreadID: snap.ThreadID,
				RepoRoot: snap.RepoRoot,
				Path:     snap.Path,
				Branch:   snap.Branch,
				Base:     snap.Base,
				Isolated: snap.Isolated,
				Number:   snap.Number,
			}
		}
		running := d.sup.Running(snap.ThreadID)
		title, last := "", time.Time{}
		if rec, ok := d.sessions.Get(snap.ThreadID); ok {
			title, last = rec.Title, rec.Updated
		}
		cands = append(cands, gitstatus.AnalyzeCandidate(wt, snap, running, title, last))
	}

	// Session records the live cache no longer tracks. Two kinds surface here:
	// orphaned isolated worktrees (dir gone — removal prunes git bookkeeping),
	// and dormant direct-workspace agents (no live snapshot this session).
	// AnalyzeCandidate classifies each: the former orphaned, the latter
	// record-only. Both are removable; the direct ones only archive the session.
	for _, rec := range d.sessions.List(project) {
		if seen[rec.ThreadID] {
			continue
		}
		running := d.sup.Running(rec.ThreadID)
		cands = append(cands,
			gitstatus.AnalyzeCandidate(rec.Worktree, nil, running, rec.Title, rec.Updated))
	}
	return cands
}

// cleanupBlockerReason turns the first blocker code into a human-readable
// refusal message for the destructive handler.
func cleanupBlockerReason(blockers []string) string {
	if len(blockers) == 0 {
		return "worktree is not removable"
	}
	switch blockers[0] {
	case gitstatus.BlockerRunning:
		return "the agent is still running — stop it first"
	case gitstatus.BlockerNotIsolated:
		return "this is the main workspace, not an isolated worktree"
	case gitstatus.BlockerDetached:
		return "the worktree is detached or has no branch"
	case gitstatus.BlockerSnapshot:
		return "could not read the worktree's git state"
	default:
		return "worktree is not removable (" + blockers[0] + ")"
	}
}

func runCore() {
	socket := flag.String("socket", defaultSocketPath(), "Unix domain socket path for the UI bus")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Info("akcore starting", "version", version, "pid", os.Getpid())

	exePath, err := os.Executable()
	if err != nil {
		log.Error("cannot resolve own path", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := ipc.NewServer(*socket, log)
	coopState := coop.NewState()
	threads := newThreadRegistry()
	broker := permission.New()
	extensions := vsix.NewManager(vsix.DefaultCacheDir())
	skillCatalog := skills.New(skills.DefaultDir())
	gitCache := gitstatus.NewCache(log)
	// Push a git.invalidated notification to every UI client whenever an
	// entry flips clean→dirty (fs watcher events or mutating RPCs). The UI
	// uses this to short-cut its next 1 Hz poll.
	gitCache.OnInvalidate(func(threadID string) {
		srv.Notify("git.invalidated", map[string]any{
			"threadIds": []string{threadID},
		})
	})
	// HEAD-change is the log viewer's signal to refetch. We notify only when a
	// recomputed snapshot shows a different HeadSHA, so editing a tracked file
	// doesn't trigger needless log reloads.
	gitCache.OnHeadChange(func(threadID, headSHA string) {
		srv.Notify("git.log.invalidated", map[string]any{
			"threadId": threadID,
			"headSha":  headSHA,
		})
	})

	sessions, err := session.NewStore(session.DefaultPath())
	if err != nil {
		log.Error("cannot open the thread store", "err", err)
		os.Exit(1)
	}

	summaries, err := compact.NewStore(compact.DefaultDir())
	if err != nil {
		log.Error("cannot open the summary store", "err", err)
		os.Exit(1)
	}

	// Cold-exit compactions are spawned from the thread-exit lifecycle event,
	// which can fire right as the core is shutting down. Track them so the
	// shutdown path can drain them before the process exits (otherwise the
	// goroutine is killed mid-compaction and the summary goes missing). The
	// context is cancelled at the shutdown deadline to kill any straggler
	// `claude --resume` rather than orphan it.
	exitCompactCtx, cancelExitCompacts := context.WithCancel(context.Background())
	defer cancelExitCompacts()
	coldCompacts := &exitCompactTracker{ctx: exitCompactCtx}

	// The agent supervisor relays every thread event to the UI as a
	// notification, so the agent panel can render the conversation live. Events
	// arrive pre-coalesced as an ordered batch; we forward the whole batch in a
	// single notification and run the per-event side effects in order.
	sup := agent.NewSupervisor("", log, func(threadID string, events []json.RawMessage) {
		if len(events) == 0 {
			return
		}
		srv.Notify("agent.event", agentEventParams{ThreadID: threadID, Events: events})
		for _, event := range events {
			var probe struct {
				Type  string `json:"type"`
				Phase string `json:"phase"`
			}
			if json.Unmarshal(event, &probe) != nil {
				continue
			}
			// Bump LastTurnAt on each turn completion. This is the staleness
			// signal for the compaction layer — if the summary is older than the
			// last turn, it needs to be refreshed before resume.
			if probe.Type == "result" {
				_ = sessions.Update(threadID, func(r *session.Record) {
					r.LastTurnAt = time.Now()
				})
				continue
			}
			// When a thread exits, drop its cooperation locks and presence, and
			// mark it dormant so it can be resumed later. If the thread is
			// configured for a cold-exit compaction strategy, fire it in the
			// background — Hot-Opus is a separate pre-reap flow.
			if probe.Type == "_lifecycle" && (probe.Phase == "exited" || probe.Phase == "interrupted") {
				coopState.ClearOwner(threadID)
				_ = sessions.UpdateQuiet(threadID, func(r *session.Record) {
					r.Status = session.StatusDormant
				})
				// Cold-exit compaction runs only on a normal exit; a user
				// interrupt is meant to be immediate, so we don't spend a turn
				// summarising it.
				if probe.Phase == "exited" {
					if rec, ok := sessions.Get(threadID); ok {
						strat := compact.Strategy(rec.CompactStrategy).Resolve()
						if strat.RunsOnExit() && strat != compact.ExitOpusHot {
							coldCompacts.spawn(log, sessions, summaries, rec, strat)
						}
					}
				}
			}
		}
	})

	// --- KDE Plasma Cowork (opt-in desktop see/control; off by default) --------
	// The shared D-Bus client and consent authority are constructed eagerly so the
	// capability probe (cowork.status) is accurate, but no thread can use them until
	// it is opted in (session.Record.CoworkEnabled) and the user grants consent.
	kdeClient, kerr := kde.New(log)
	if kerr != nil {
		log.Warn("cowork: KDE session bus unavailable; desktop features disabled", "err", kerr)
		kdeClient = nil
	}
	coworkSvc, coworkWarn, cerr := cowork.New(cowork.DefaultGrantsPath(), cowork.DefaultAuditPath(), kdeClient, coworkNotifier{srv}, log)
	if cerr != nil {
		log.Error("cowork: consent store init failed; desktop features disabled", "err", cerr)
		coworkSvc = nil
		if kdeClient != nil {
			_ = kdeClient.Close()
		}
	}
	for _, w := range coworkWarn {
		log.Warn("cowork startup warning", "warning", w)
	}
	if coworkSvc != nil {
		coworkSvc.StartSweeper(30 * time.Second)
	}

	deps := handlerDeps{
		srv:        srv,
		sup:        sup,
		coop:       coopState,
		threads:    threads,
		broker:     broker,
		extensions: extensions,
		sessions:   sessions,
		summaries:  summaries,
		skills:     skillCatalog,
		gitCache:   gitCache,
		cowork:     coworkSvc,
		socketPath: *socket,
		exePath:    exePath,
		log:        log,
	}
	registerHandlers(deps)

	// gracefulShutdown runs the ordered teardown: compact live (Hot-Opus)
	// threads, stop every agent, drain any cold-exit compactions, then close the
	// file watchers. It is guarded by a sync.Once so the RPC-driven path (which
	// streams progress to a UI dialog) and the signal/disconnect fallback path
	// can both call it without doing the work twice.
	var shutdownOnce sync.Once
	gracefulShutdown := func(progress shutdownProgressFn) {
		shutdownOnce.Do(func() {
			running := 0
			for _, rec := range sessions.List("") {
				if sup.Running(rec.ThreadID) {
					running++
				}
			}
			progress("preparing", "", 0, running)
			// Hot-Opus compaction runs while the threads are still live and
			// cache-warm; it emits its own per-agent "compacting" progress.
			runHotCompactsAtShutdown(deps, progress)
			progress("stopping", "", 0, running)
			sup.StopAll()
			// Cold-exit compactions were spawned from the exit lifecycle as each
			// thread reaped; drain them (bounded) before we let the process exit.
			progress("draining", "", 0, 0)
			coldCompacts.drain(cancelExitCompacts, exitCompactCap, log)
			progress("watchers", "", 0, 0)
			_ = gitCache.Close()
			// Tear down any live Cowork portal/screencast sessions, revoke
			// non-durable grants, and release the D-Bus connection.
			if coworkSvc != nil {
				progress("cowork", "", 0, 0)
				_ = coworkSvc.Close()
			}
			progress("done", "", 0, running)
		})
	}

	// app.shutdown lets the UI request a graceful, observable teardown while the
	// IPC server is still alive, so it can stream shutdown.progress to a dialog.
	// When done it cancels the serve context, which exits the process; the
	// fallback path below then re-invokes gracefulShutdown as a guarded no-op.
	srv.Handle("app.shutdown", func(_ context.Context, _ json.RawMessage) (any, error) {
		gracefulShutdown(func(phase, detail string, index, total int) {
			srv.Notify("shutdown.progress", map[string]any{
				"phase":  phase,
				"detail": detail,
				"index":  index,
				"total":  total,
			})
		})
		stop()
		return map[string]any{"ok": true}, nil
	})

	// Rehydrate the git cache and thread registry with every persisted thread
	// that still has a worktree on disk, so the dashboard shows dormant threads
	// after a restart and per-thread RPCs (git.log, git.diff, …) resolve before
	// the agent has been re-attached.
	for _, rec := range sessions.List("") {
		if _, err := os.Stat(rec.Worktree.Path); err == nil {
			gitCache.Register(rec.Worktree)
			threads.put(rec.ThreadID, rec.Worktree)
		}
	}

	// Don't outlive the UI: when the last client disconnects, shut down.
	srv.OnAllClientsGone(func() {
		log.Info("ui disconnected; akcore shutting down")
		stop()
	})

	serveErr := srv.Serve(ctx)
	// Fallback teardown for the paths that don't go through app.shutdown (UI
	// disconnect, SIGINT/SIGTERM). Guarded by the same sync.Once, so if the UI
	// already drove a graceful shutdown this is a no-op.
	gracefulShutdown(func(phase, detail string, index, total int) {
		log.Info("shutdown progress", "phase", phase, "index", index, "total", total)
	})
	if serveErr != nil {
		log.Error("ipc server stopped", "err", serveErr)
		os.Exit(1)
	}
	log.Info("akcore stopped")
}

// --- IPC parameter / result types ------------------------------------------

// agentEventParams is the wire shape of an "agent.event" notification. Events
// are delivered as an ordered batch (the core coalesces the per-line
// stream-json flood); the UI iterates Events in order. A batch always carries
// at least one event.
type agentEventParams struct {
	ThreadID string            `json:"threadId"`
	Events   []json.RawMessage `json:"events"`
}

type agentStartParams struct {
	WorkspacePath  string             `json:"workspacePath"`
	Prompt         string             `json:"prompt"`
	PermissionMode string             `json:"permissionMode"`
	Effort         string             `json:"effort"`    // claude --effort level; "" = default
	Model          string             `json:"model"`     // claude --model id; "" = Claude Code default
	Isolation      string             `json:"isolation"` // worktree.Mode*; "" = auto
	Attachments    []agent.Attachment `json:"attachments"`
	CoworkEnabled  bool               `json:"coworkEnabled"` // opt into the KDE Cowork desktop tools
}

type agentSendParams struct {
	ThreadID    string             `json:"threadId"`
	Text        string             `json:"text"`
	Attachments []agent.Attachment `json:"attachments"`
}

type agentStopParams struct {
	ThreadID string `json:"threadId"`
}

type agentDiffParams struct {
	ThreadID string `json:"threadId"`
}

type coopSetOpenFilesParams struct {
	Owner string   `json:"owner"`
	Files []string `json:"files"`
}

type coopPostNoteParams struct {
	Author string `json:"author"`
	Text   string `json:"text"`
}

// handlerDeps bundles everything registerHandlers needs.
type handlerDeps struct {
	srv        *ipc.Server
	sup        *agent.Supervisor
	coop       *coop.State
	threads    *threadRegistry
	broker     *permission.Broker
	extensions *vsix.Manager
	sessions   *session.Store
	summaries  *compact.Store
	skills     *skills.Catalog
	gitCache   *gitstatus.Cache
	cowork     *cowork.Service // nil if KDE/consent init failed; handlers guard
	socketPath string
	exePath    string
	log        *slog.Logger
}

// registerHandlers wires the JSON-RPC methods the core serves.
func registerHandlers(d handlerDeps) {
	d.srv.Handle("handshake", func(ctx context.Context, _ json.RawMessage) (any, error) {
		// Tag this connection as the UI (the first UI becomes the primary that runs
		// Cowork portal sessions) — the Cowork keystone (08 §C).
		d.srv.MarkUI(ctx)
		d.log.Info("handshake received")
		return map[string]any{
			"name":    "akcore",
			"version": version,
			"pid":     os.Getpid(),
		}, nil
	})

	// Cowork (KDE desktop see/control) RPCs. No-op if the service is unavailable.
	registerCoworkHandlers(d)

	// --- agent threads -----------------------------------------------------
	d.srv.Handle("agent.start", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStartParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.WorkspacePath == "" || p.Prompt == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "workspacePath and prompt are required")
		}
		// Start asynchronously so this reply — which carries the threadId —
		// always reaches the UI before any streamed event for the thread.
		threadID := agent.NewThreadID()
		sessionID := session.NewID()
		safe.Go("agent.startThread", func() { startAgentThread(d, threadID, sessionID, p) })
		return map[string]any{"threadId": threadID, "sessionId": sessionID}, nil
	})

	// agent.resume re-launches a dormant thread on its persisted Claude Code
	// session, in the same worktree it ran in before.
	d.srv.Handle("agent.resume", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if rec.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"thread has no Claude Code session to resume")
		}
		safe.Go("agent.resumeThread", func() { resumeAgentThread(d, rec) })
		return map[string]any{"threadId": rec.ThreadID, "sessionId": rec.SessionID}, nil
	})

	// agent.transcript returns the Claude Code session transcript for a
	// thread, as the raw JSONL events. The UI replays these to rebuild the
	// conversation feed when reopening a dormant thread.
	d.srv.Handle("agent.transcript", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if rec.SessionID == "" {
			return map[string]any{"events": []json.RawMessage{}}, nil
		}
		events, err := session.ReadTranscript(rec.SessionID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if events == nil {
			events = []json.RawMessage{}
		}
		return map[string]any{"events": events}, nil
	})

	// agent.promote upgrades a non-isolated thread into a dedicated git
	// worktree, carrying its working-tree changes and Claude Code session over.
	d.srv.Handle("agent.promote", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if rec.Worktree.Isolated {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this thread already runs in an isolated worktree")
		}
		safe.Go("agent.promoteThread", func() { promoteAgentThread(d, rec) })
		return map[string]any{"threadId": rec.ThreadID}, nil
	})

	// session.listThreads returns persisted threads (running and dormant),
	// optionally filtered to one project, so the UI can offer to resume them.
	d.srv.Handle("session.listThreads", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p) // project is optional
		records := d.sessions.List(p.Project)
		if records == nil {
			records = []session.Record{}
		}
		return map[string]any{"threads": records}, nil
	})

	// session.browse lists every Claude Code session transcript on disk, so the
	// user can attach any past conversation — even ones Agent Kate did not start.
	d.srv.Handle("session.browse", func(_ context.Context, _ json.RawMessage) (any, error) {
		found, err := session.Discover()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		known := map[string]bool{}
		for _, r := range d.sessions.List("") {
			known[r.SessionID] = true
		}
		for i := range found {
			found[i].Attached = known[found[i].SessionID]
		}
		if len(found) > 500 {
			found = found[:500] // Discover sorts newest-first
		}
		if found == nil {
			found = []session.Discovered{}
		}
		return map[string]any{"sessions": found}, nil
	})

	// session.attach turns a discovered Claude Code session into a dormant
	// Agent Kate thread, which the UI then resumes like any other.
	d.srv.Handle("session.attach", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"sessionId"`
			Project   string `json:"project"`
			Title     string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" || p.Project == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"sessionId and project are required")
		}
		// Never attach the same Claude Code session twice.
		if rec, ok := d.sessions.GetBySession(p.SessionID); ok {
			return map[string]any{"threadId": rec.ThreadID, "alreadyAttached": true}, nil
		}
		threadID := agent.NewThreadID()
		rec := session.Record{
			ThreadID:  threadID,
			SessionID: p.SessionID,
			Project:   p.Project,
			Worktree: worktree.Worktree{
				ThreadID: threadID,
				RepoRoot: p.Project,
				Path:     p.Project,
				Isolated: false, // resume in the conversation's own directory
			},
			PermissionMode: "acceptEdits",
			Title:          summarizePrompt(p.Title),
			Created:        time.Now(),
			Status:         session.StatusDormant,
		}
		if err := d.sessions.Put(rec); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.log.Info("session attached", "thread", threadID, "session", p.SessionID)
		return map[string]any{"threadId": threadID, "alreadyAttached": false}, nil
	})

	// session.preview streams the last few turns of a discovered session's
	// transcript so the user can confirm what they are about to resume without
	// attaching it first. The whole file is never read into the reply.
	d.srv.Handle("session.preview", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID   string `json:"sessionId"`
			MaxMessages int    `json:"maxMessages"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "sessionId is required")
		}
		msgs, truncated, err := session.PreviewTranscript(p.SessionID, p.MaxMessages)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if msgs == nil {
			msgs = []session.PreviewMessage{}
		}
		return map[string]any{"messages": msgs, "truncated": truncated}, nil
	})

	// session.forget deletes a discovered session's transcript from disk. It
	// refuses to act on a session that is attached as an Agent Kate thread —
	// the user must remove that agent first, so the thread never dangles.
	d.srv.Handle("session.forget", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "sessionId is required")
		}
		if _, ok := d.sessions.GetBySession(p.SessionID); ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this session is attached as an agent; remove the agent first")
		}
		if err := session.DeleteTranscript(p.SessionID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("session forgotten", "session", p.SessionID)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("agent.send", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentSendParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.sup.Send(p.ThreadID, p.Text, p.Attachments); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("agent.stop", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		// Hot-Opus runs against the live session so it has to happen BEFORE
		// the supervisor terminates the process; cold strategies fire from
		// the exit lifecycle handler instead.
		runHotCompactIfConfigured(d, p.ThreadID)
		if err := d.sup.Stop(p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// agent.interrupt is the hard-stop counterpart to agent.stop: it aborts the
	// in-flight turn immediately (no further tokens billed) and leaves the
	// thread dormant-but-resumable. No hot-compaction here — interrupt is meant
	// to be instantaneous, and spending a summary turn would defeat the purpose.
	d.srv.Handle("agent.interrupt", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentStopParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.sup.Interrupt(p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("agent.diff", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p agentDiffParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		diff, err := worktree.Diff(wt)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{
			"diff":     diff,
			"isolated": wt.Isolated,
			"branch":   wt.Branch,
		}, nil
	})

	d.srv.Handle("agent.commit", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.Commit(wt, p.Message); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.Invalidate(p.ThreadID)
		return map[string]any{"ok": true, "branch": wt.Branch}, nil
	})

	d.srv.Handle("agent.openPR", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		url, err := worktree.OpenPR(wt, p.Title)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"url": url}, nil
	})

	// agent.land merges a thread's branch into the workspace's main branch — a
	// local integration, separate from agent.openPR which targets GitHub.
	d.srv.Handle("agent.land", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		target, err := worktree.Land(rec.Worktree)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The land touched both the worktree and the main repo — invalidate
		// every entry so the dashboard reflects the new ahead/behind.
		d.gitCache.InvalidateAll()
		d.log.Info("agent thread landed", "thread", p.ThreadID,
			"branch", rec.Worktree.Branch, "into", target)
		return map[string]any{"branch": rec.Worktree.Branch, "into": target}, nil
	})

	d.srv.Handle("agent.list", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p)
		recs := d.sessions.List(p.Project)
		out := make([]map[string]any, 0, len(recs))
		for _, r := range recs {
			out = append(out, map[string]any{
				"threadId": r.ThreadID,
				"project":  r.Project,
				"title":    r.Title,
				"status":   r.Status,
				"branch":   r.Worktree.Branch,
				"path":     r.Worktree.Path,
				"isolated": r.Worktree.Isolated,
				"number":   r.Worktree.Number,
				"created":  r.Created,
				"updated":  r.Updated,
				"lastTurn": r.LastTurnAt,
				"model":    r.Model,
				"tags":     r.Tags,
			})
		}
		return map[string]any{"threads": out}, nil
	})

	// agent.rename persists a user-chosen title for a thread. No worktree or
	// process is touched — only the session record's Title field is updated, so
	// the new name survives restart (session.listThreads reads it back).
	d.srv.Handle("agent.rename", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Title    string `json:"title"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" || p.Title == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId and title are required")
		}
		if _, ok := d.sessions.Get(p.ThreadID); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Title = p.Title
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// --- tags --------------------------------------------------------------
	// Roster organization labels. The store is a dumb setter; normalization
	// (trim, dedupe, cap length/count) happens here so threads.json never
	// holds malformed tags. Every successful mutation broadcasts
	// agent.tagsChanged so all roster clients converge.

	// agent.setTags replaces a thread's full tag set.
	d.srv.Handle("agent.setTags", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string   `json:"threadId"`
			Tags     []string `json:"tags"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if _, ok := d.sessions.Get(p.ThreadID); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		tags := normalizeTags(p.Tags)
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Tags = tags
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.srv.Notify("agent.tagsChanged", map[string]any{
			"threadId": p.ThreadID, "tags": tags})
		return map[string]any{"ok": true, "tags": tags}, nil
	})

	// agent.addTag adds one tag to a thread, returning the full normalized set.
	d.srv.Handle("agent.addTag", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Tag      string `json:"tag"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		tags := normalizeTags(append(append([]string{}, rec.Tags...), p.Tag))
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Tags = tags
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.srv.Notify("agent.tagsChanged", map[string]any{
			"threadId": p.ThreadID, "tags": tags})
		return map[string]any{"ok": true, "tags": tags}, nil
	})

	// agent.removeTag drops one tag (case-insensitive) from a thread.
	d.srv.Handle("agent.removeTag", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Tag      string `json:"tag"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		drop := strings.ToLower(strings.TrimSpace(p.Tag))
		kept := make([]string, 0, len(rec.Tags))
		for _, t := range rec.Tags {
			if strings.ToLower(strings.TrimSpace(t)) != drop {
				kept = append(kept, t)
			}
		}
		tags := normalizeTags(kept)
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.Tags = tags
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.srv.Notify("agent.tagsChanged", map[string]any{
			"threadId": p.ThreadID, "tags": tags})
		return map[string]any{"ok": true, "tags": tags}, nil
	})

	// agent.suggestTags runs a one-shot Sonnet pass over a project's threads
	// and returns proposed tag assignments. It is read-only — it applies
	// nothing; the UI previews the proposals and applies them via setTags.
	d.srv.Handle("agent.suggestTags", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(raw, &p) // project optional → all threads
		recs := d.sessions.List(p.Project)
		if len(recs) == 0 {
			return map[string]any{"proposals": []any{}}, nil
		}
		sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		proposals, err := session.SuggestTagOrganization(sctx, recs, "", "claude-sonnet-4-6")
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		out := make([]map[string]any, 0, len(proposals))
		for _, pr := range proposals {
			out = append(out, map[string]any{"threadId": pr.ThreadID, "tags": pr.Tags})
		}
		return map[string]any{"proposals": out}, nil
	})

	d.srv.Handle("agent.discard", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		_ = d.sup.Stop(p.ThreadID)
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The worktree is gone, so the thread can never be resumed — forget it.
		_ = d.sessions.Remove(p.ThreadID)
		_ = d.summaries.Remove(p.ThreadID)
		d.gitCache.Forget(p.ThreadID)
		// Tell every UI client to drop this thread from its roster.
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		return map[string]any{"ok": true}, nil
	})

	// --- compaction --------------------------------------------------------
	// Reduces prefix re-cache cost on resume. See package compact.
	d.srv.Handle("agent.setCompactStrategy", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Strategy string `json:"strategy"`
			Strip    bool   `json:"strip"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		s := compact.Strategy(p.Strategy)
		if p.Strategy != "" && !s.Valid() {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown strategy "+p.Strategy)
		}
		if err := d.sessions.Update(p.ThreadID, func(r *session.Record) {
			r.CompactStrategy = p.Strategy
			r.CompactStrip = p.Strip
		}); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// agent.summaryStatus reports whether a thread has a current compacted
	// summary on disk, used by the UI to drive the recovery dialog on resume.
	d.srv.Handle("agent.summaryStatus", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		sum, err := d.summaries.Get(p.ThreadID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		out := map[string]any{
			"hasSummary":  sum != nil,
			"strategy":    rec.CompactStrategy,
			"strip":       rec.CompactStrip,
			"lastTurnAt":  rec.LastTurnAt,
			"updatedAt":   rec.SummaryUpdatedAt,
		}
		if sum != nil {
			out["summaryTurns"] = sum.Turns
			out["summaryCreated"] = sum.Created
		}
		// Stale when there is no summary at all, or the latest user/assistant
		// turn happened after the last compaction.
		out["stale"] = sum == nil ||
			(!rec.LastTurnAt.IsZero() && rec.LastTurnAt.After(rec.SummaryUpdatedAt))
		return out, nil
	})

	// agent.compactNow runs a compaction synchronously with the given model.
	// Used by the resume-time recovery dialog and the explicit "Compact now"
	// UI action. model accepts: "hot" / "opus_hot" (inline on the live
	// thread), "opus", "sonnet", "haiku", "local" (case-insensitive), or a
	// full claude --model id like "claude-sonnet-4-6".
	d.srv.Handle("agent.compactNow", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if rec.SessionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "thread has no Claude Code session yet")
		}

		var sum compact.Summary
		token := strings.ToLower(strings.TrimSpace(p.Model))
		if token == "hot" || token == "opus_hot" || token == "hot_opus" {
			// Hot path: send the compact prompt into the live thread and use
			// its assistant reply as the summary. Requires a running thread.
			if !d.sup.Running(p.ThreadID) {
				return nil, ipc.Errorf(ipc.CodeInvalidParams,
					"Hot Opus compaction requires a running thread; resume it first")
			}
			hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			text, herr := d.sup.Compact(hctx, p.ThreadID, compact.CompactPrompt)
			if herr != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, herr.Error())
			}
			if strings.TrimSpace(text) == "" {
				return nil, ipc.Errorf(ipc.CodeInternalError, "hot compaction returned empty body")
			}
			sum = compact.Summary{
				ThreadID:  p.ThreadID,
				SessionID: rec.SessionID,
				Strategy:  compact.ExitOpusHot,
				Stripped:  rec.CompactStrip,
				Created:   time.Now().UTC(),
				Body:      text,
			}
		} else {
			modelID, strategy, isLocal := resolveCompactModel(p.Model)
			if isLocal {
				events, rerr := session.ReadTranscript(rec.SessionID)
				if rerr != nil {
					return nil, ipc.Errorf(ipc.CodeInternalError, rerr.Error())
				}
				sum = compact.Programmatic(p.ThreadID, rec.SessionID, events)
			} else {
				var err error
				sum, err = compact.RunLLM(ctx, p.ThreadID, strategy, compact.LLMOptions{
					WorkDir:   rec.Worktree.Path,
					SessionID: rec.SessionID,
					Model:     modelID,
					Timeout:   5 * time.Minute,
				})
				if err != nil {
					return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
				}
			}
		}
		if err := d.summaries.Put(sum); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		_ = d.sessions.UpdateQuiet(p.ThreadID, func(r *session.Record) {
			r.SummaryUpdatedAt = sum.Created
		})
		d.log.Info("compaction complete",
			"thread", p.ThreadID, "strategy", sum.Strategy,
			"turns", sum.Turns, "body_bytes", len(sum.Body))
		return map[string]any{
			"ok":        true,
			"strategy":  string(sum.Strategy),
			"turns":     sum.Turns,
			"bodyBytes": len(sum.Body),
		}, nil
	})

	// --- cooperation state (shared with the Cooperation MCP) ---------------
	d.srv.Handle("coop.setOpenFiles", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p coopSetOpenFilesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		d.coop.SetOpenFiles(p.Owner, p.Files)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.listOpenFiles", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"files": d.coop.ListOpenFiles()}, nil
	})

	d.srv.Handle("coop.postNote", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p coopPostNoteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Author == "" {
			p.Author = "human"
		}
		return d.coop.PostNote(p.Author, p.Text), nil
	})

	d.srv.Handle("coop.readNotes", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"notes": d.coop.ReadNotes()}, nil
	})

	d.srv.Handle("coop.setPresence", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner       string `json:"owner"`
			FocusedFile string `json:"focusedFile"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		d.coop.SetPresence(p.Owner, p.FocusedFile)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.getPresence", func(_ context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{
			"presence":  d.coop.ListPresence(),
			"claims":    d.coop.ListClaims(),
			"openFiles": d.coop.ListOpenFiles(),
		}, nil
	})

	d.srv.Handle("coop.claimFile", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		ok, holder := d.coop.ClaimFile(p.Path, p.Owner)
		return map[string]any{"ok": ok, "holder": holder}, nil
	})

	d.srv.Handle("coop.releaseFile", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path  string `json:"path"`
			Owner string `json:"owner"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Owner == "" {
			p.Owner = "human"
		}
		d.coop.ReleaseFile(p.Path, p.Owner)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("coop.requestReview", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Thread  string `json:"thread"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rev := d.coop.AddReview(p.Thread, p.Summary)
		d.srv.Notify("agent.reviewRequested", map[string]any{
			"threadId": p.Thread,
			"summary":  p.Summary,
			"id":       rev.ID,
		})
		return map[string]any{"id": rev.ID}, nil
	})

	// --- per-tool approval -------------------------------------------------
	// permission.request comes from an agent's MCP bridge and blocks until the
	// human answers via permission.respond (or an 8-minute safety timeout).
	d.srv.Handle("permission.request", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string          `json:"threadId"`
			ToolName string          `json:"toolName"`
			Input    json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		id, ch := d.broker.Open()
		d.srv.Notify("permission.requested", map[string]any{
			"threadId":  p.ThreadID,
			"requestId": id,
			"toolName":  p.ToolName,
			"input":     p.Input,
		})
		select {
		case dec := <-ch:
			res := map[string]any{"allow": dec.Allow}
			if len(dec.UpdatedInput) > 0 {
				res["updatedInput"] = dec.UpdatedInput
			}
			return res, nil
		case <-time.After(8 * time.Minute):
			d.broker.Close(id)
			return map[string]any{"allow": false}, nil
		}
	})

	d.srv.Handle("permission.respond", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			RequestID    string          `json:"requestId"`
			Allow        bool            `json:"allow"`
			UpdatedInput json.RawMessage `json:"updatedInput"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.broker.Resolve(p.RequestID,
			permission.Decision{Allow: p.Allow, UpdatedInput: p.UpdatedInput})
		return map[string]any{"ok": true}, nil
	})

	// --- VS Code extension reuse -------------------------------------------
	// vsix.install downloads an extension from Open VSX, unpacks it and
	// detects the language server it bundles. It blocks for the duration of
	// the download; the IPC server dispatches each request on its own
	// goroutine, so other traffic is unaffected.
	d.srv.Handle("vsix.install", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ExtensionID string `json:"extensionId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ExtensionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "extensionId is required")
		}
		// Throttle progress so a fast download cannot flood the socket: emit at
		// most every 250ms or whenever the fraction advances by >= 1%.
		var lastEmit time.Time
		var lastFrac float64
		ext, err := d.extensions.InstallProgress(ctx, p.ExtensionID, func(done, total int64) {
			var frac float64
			if total > 0 {
				frac = float64(done) / float64(total)
			}
			now := time.Now()
			if now.Sub(lastEmit) < 250*time.Millisecond && frac-lastFrac < 0.01 && frac < 1.0 {
				return
			}
			lastEmit = now
			lastFrac = frac
			d.srv.Notify("vsix.installProgress", map[string]any{
				"extensionId": p.ExtensionID,
				"fraction":    frac,
				"indeterminate": total == 0,
			})
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.log.Info("extension installed", "id", ext.ID, "version", ext.Version,
			"hasServer", ext.Server != nil)
		return ext, nil
	})

	// vsix.uninstall deletes an installed extension from the cache. The id is
	// validated and resolved under the cache dir inside Manager.Remove, so a
	// crafted id can never delete anything outside it.
	d.srv.Handle("vsix.uninstall", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ExtensionID string `json:"extensionId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ExtensionID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "extensionId is required")
		}
		if err := d.extensions.Remove(p.ExtensionID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("extension uninstalled", "id", p.ExtensionID)
		return map[string]any{"ok": true}, nil
	})

	d.srv.Handle("vsix.list", func(ctx context.Context, _ json.RawMessage) (any, error) {
		exts, err := d.extensions.List()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if exts == nil {
			exts = []*vsix.Extension{}
		}
		// Enrich each entry with the latest published version so the UI can
		// flag updates. This is best effort and concurrency-bounded: any
		// lookup error (offline, removed upstream) simply omits the field and
		// never fails the list. A short timeout keeps the dialog responsive.
		out := make([]map[string]any, len(exts))
		latest := make([]string, len(exts))
		lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for i, e := range exts {
			wg.Add(1)
			i, id := i, e.ID
			safe.Go("vsix.latestVersion", func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if v, err := d.extensions.LatestVersion(lctx, id); err == nil {
					latest[i] = v
				}
			})
		}
		wg.Wait()
		for i, e := range exts {
			m := map[string]any{
				"id":         e.ID,
				"name":       e.Name,
				"version":    e.Version,
				"dir":        e.Dir,
				"server":     e.Server,
				"serverHint": e.ServerHint,
			}
			if latest[i] != "" {
				m["latest"] = latest[i]
				m["updateAvailable"] = latest[i] != e.Version
			}
			out[i] = m
		}
		return map[string]any{"extensions": out}, nil
	})

	// vsix.search queries the Open VSX registry. It is network-dependent and
	// best effort — a failure returns an error the UI surfaces inline rather
	// than blocking the dialog. Hits already installed are tagged like the
	// curated catalog so the UI can disable their Install button.
	d.srv.Handle("vsix.search", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		entries, err := d.extensions.Search(sctx, p.Query)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		installed := map[string]bool{}
		if list, err := d.extensions.List(); err == nil {
			for _, e := range list {
				installed[e.ID] = true
			}
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"id":          e.ID,
				"displayName": e.DisplayName,
				"summary":     e.Summary,
				"category":    e.Category,
				"installed":   installed[e.ID],
			})
		}
		return map[string]any{"entries": out}, nil
	})

	// vsix.catalog returns the curated list of popular extensions, each
	// tagged with whether the user already has it installed so the UI can
	// hide its Install button.
	d.srv.Handle("vsix.catalog", func(_ context.Context, _ json.RawMessage) (any, error) {
		installed := map[string]bool{}
		if list, err := d.extensions.List(); err == nil {
			for _, e := range list {
				installed[e.ID] = true
			}
		}
		entries := vsix.Catalog()
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, map[string]any{
				"id":          e.ID,
				"displayName": e.DisplayName,
				"summary":     e.Summary,
				"category":    e.Category,
				"installed":   installed[e.ID],
			})
		}
		return map[string]any{"entries": out}, nil
	})

	// --- Claude Code skills ------------------------------------------------
	// skills.listCatalog returns every skill in the central Agent Kate catalog
	// (XDG_DATA_HOME/agentkate/skills). An empty catalog is fine — the UI
	// reveals the catalog directory so the user can drop skills into it.
	d.srv.Handle("skills.listCatalog", func(_ context.Context, _ json.RawMessage) (any, error) {
		if err := d.skills.EnsureDir(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		list, err := d.skills.List()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"skills": list, "catalogDir": d.skills.Dir()}, nil
	})

	// skills.listInstalled enumerates target/.claude/skills, flagging the
	// entries the catalog owns so the UI can show their install state.
	d.srv.Handle("skills.listInstalled", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		list, err := d.skills.ListInstalled(p.Target)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"installed": list}, nil
	})

	d.srv.Handle("skills.install", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		path, err := d.skills.Install(p.Name, p.Target)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("skill installed", "name", p.Name, "target", p.Target, "path", path)
		return map[string]any{"path": path}, nil
	})

	d.srv.Handle("skills.uninstall", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Name   string `json:"name"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.skills.Uninstall(p.Name, p.Target); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("skill uninstalled", "name", p.Name, "target", p.Target)
		return map[string]any{"ok": true}, nil
	})

	// skills.read returns the full markdown of a catalog skill for the detail
	// pane. Content is capped inside the catalog so a huge file cannot bloat
	// the reply.
	d.srv.Handle("skills.read", func(_ context.Context, raw json.RawMessage) (any, error) {
		if err := d.skills.EnsureDir(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		content, err := d.skills.ReadContent(p.Name)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"content": content}, nil
	})

	// skills.create scaffolds a new directory skill (SKILL.md + frontmatter) in
	// the catalog. Invalid or duplicate names are rejected by the catalog.
	d.srv.Handle("skills.create", func(_ context.Context, raw json.RawMessage) (any, error) {
		if err := d.skills.EnsureDir(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		var p struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		skill, err := d.skills.Create(p.Name, p.Description)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		d.log.Info("skill created", "name", skill.Name, "path", skill.Path)
		return skill, nil
	})

	// --- git status (read-only) -------------------------------------------
	// git.snapshot returns every registered thread's worktree status, drawn
	// from the cache. The UI polls this at ~1 Hz to drive the worktree
	// dashboard.
	d.srv.Handle("git.snapshot", func(_ context.Context, _ json.RawMessage) (any, error) {
		// The fs watcher keeps the cache honest, so polling just reads —
		// only stale entries pay the recompute cost.
		snaps := d.gitCache.Snapshots()
		if snaps == nil {
			snaps = []*gitstatus.Snapshot{}
		}
		// Surface each distinct workspace alongside the threads so the log
		// viewer can offer "view main" as a picker entry without needing the
		// user to have started an agent on the workspace.
		seen := make(map[string]bool)
		workspaces := []map[string]any{}
		for _, s := range snaps {
			if s.RepoRoot == "" || seen[s.RepoRoot] {
				continue
			}
			seen[s.RepoRoot] = true
			workspaces = append(workspaces, map[string]any{
				"repoRoot": s.RepoRoot,
				"branch":   gitstatus.WorkspaceHeadBranch(s.RepoRoot),
			})
		}
		return map[string]any{"threads": snaps, "workspaces": workspaces}, nil
	})

	// git.prDraft returns a suggested PR title and body for the thread's
	// branch, drawn from its commit history since the fork point. Pure
	// read; no network. The UI uses it to prefill the PR dialog.
	d.srv.Handle("git.prDraft", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		title, body, err := worktree.PRDraft(rec.Worktree)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"title": title, "body": body}, nil
	})

	// git.openPR pushes the thread's branch and opens a GitHub pull
	// request, accepting an edited title / body (and a draft flag) from
	// the UI. agent.openPR remains as the title-only shortcut.
	d.srv.Handle("git.openPR", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Title    string `json:"title"`
			Body     string `json:"body"`
			Draft    bool   `json:"draft"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		url, err := worktree.OpenPRWithOptions(rec.Worktree, worktree.PROptions{
			Title: p.Title,
			Body:  p.Body,
			Draft: p.Draft,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"url": url}, nil
	})

	// git.land merges the thread's branch into the workspace's current
	// branch. Always takes keepConflicts: the UI passes true so conflicts
	// surface as a banner instead of silently rolling back. agent.land
	// remains the always-rollback shortcut for callers that want that.
	d.srv.Handle("git.land", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID      string `json:"threadId"`
			KeepConflicts bool   `json:"keepConflicts"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		res, err := worktree.LandWithOptions(rec.Worktree, p.KeepConflicts)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// Whether clean or conflicting, both worktrees' state moved.
		d.gitCache.InvalidateAll()
		if len(res.Conflicts) == 0 {
			d.log.Info("git.land complete", "thread", p.ThreadID,
				"branch", res.Branch, "into", res.Into)
		} else {
			d.log.Info("git.land left conflicts", "thread", p.ThreadID,
				"branch", res.Branch, "into", res.Into,
				"conflicts", len(res.Conflicts))
		}
		return res, nil
	})

	// git.discardChanges throws away every uncommitted change in a thread's
	// worktree (git reset --hard HEAD + git clean -fd). DESTRUCTIVE: the UI
	// gates this behind an explicit confirmation. Guard: refuse while the
	// agent is live, so we never yank files out from under a running session.
	d.srv.Handle("git.discardChanges", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if d.sup.Running(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"cannot discard changes while the agent is running — stop it first")
		}
		if err := worktree.DiscardChanges(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.Invalidate(p.ThreadID)
		d.log.Info("git.discardChanges complete", "thread", p.ThreadID, "path", wt.Path)
		return map[string]any{"ok": true}, nil
	})

	// git.removeWorktree deletes an isolated agent worktree and its branch.
	// DESTRUCTIVE: the UI confirms first. Guards: only isolated worktrees can
	// be removed (never the shared workspace), and never while the agent is
	// live.
	d.srv.Handle("git.removeWorktree", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if !wt.Isolated {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"this thread runs directly in the workspace and has no worktree to remove")
		}
		if d.sup.Running(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"cannot remove the worktree while the agent is running — stop it first")
		}
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// The worktree is gone; forget its session + cache entry just like
		// agent.discard does, and tell every UI client to refresh.
		_ = d.sessions.Remove(p.ThreadID)
		_ = d.summaries.Remove(p.ThreadID)
		d.gitCache.Forget(p.ThreadID)
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		d.srv.Notify("git.invalidated", map[string]any{"threadIds": []string{p.ThreadID}})
		d.log.Info("git.removeWorktree complete", "thread", p.ThreadID, "path", wt.Path)
		return map[string]any{"ok": true}, nil
	})

	// git.abortMerge rolls back an in-progress merge in the thread's
	// workspace, restoring it to pre-merge state.
	d.srv.Handle("git.abortMerge", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.AbortMerge(rec.Worktree.RepoRoot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.InvalidateAll()
		return map[string]any{"ok": true}, nil
	})

	// git.finalizeMerge commits the in-progress merge in the thread's
	// workspace using git's default merge message. Fails if any conflict
	// markers are still present.
	d.srv.Handle("git.finalizeMerge", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.FinalizeMerge(rec.Worktree.RepoRoot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.InvalidateAll()
		return map[string]any{"ok": true}, nil
	})

	// git.openConflictTool spawns KDiff3 (via git mergetool) for every
	// unmerged path in the thread's workspace, detached so akcore doesn't
	// wait. The fs watcher catches each save and keeps the dashboard fresh.
	d.srv.Handle("git.openConflictTool", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.OpenConflictTool(rec.Worktree.RepoRoot); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// git.workspaceMergeStatus tells the UI whether the workspace is mid-
	// merge and which paths are still unresolved. Polled by the conflict
	// banner so it can dismiss itself once the human finishes in KDiff3.
	d.srv.Handle("git.workspaceMergeStatus", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		rec, ok := d.sessions.Get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		return worktree.WorkspaceMergeStatus(rec.Worktree.RepoRoot), nil
	})

	// git.commit stages a subset of paths (or everything when paths is
	// empty) and commits them to the thread's branch. agent.commit is kept
	// as the "commit everything with this message" shortcut.
	d.srv.Handle("git.commit", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string   `json:"threadId"`
			Message  string   `json:"message"`
			Paths    []string `json:"paths"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := worktree.CommitPaths(wt, p.Message, p.Paths); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.gitCache.Invalidate(p.ThreadID)
		return map[string]any{"ok": true, "branch": wt.Branch}, nil
	})

	// git.suggestCommitMessage asks Sonnet to draft a commit message for the
	// worktree's current diff. Used by the Commit dialog's "Suggest" button.
	// Long-running (one Claude turn); the IPC dispatcher already runs each
	// handler on its own goroutine so this does not block the bus.
	d.srv.Handle("git.suggestCommitMessage", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		msg, err := gitstatus.SuggestCommitMessage(ctx, wt, "", p.Model)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"message": msg}, nil
	})

	// git.diff returns a unified patch of the worktree vs HEAD, scoped to a
	// single path when one is given. Untracked files are folded in as full
	// new-file diffs, and the index is never touched.
	d.srv.Handle("git.diff", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		patch, err := gitstatus.UnifiedDiff(wt, p.Path)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{
			"patch":  patch,
			"branch": wt.Branch,
		}, nil
	})

	// git.blame returns one BlameLine per source line of an absolute file
	// path. The path is resolved against registered worktrees the same way
	// git.file does it, so an open editor file maps to its owning agent's
	// branch without the UI having to know the thread id.
	d.srv.Handle("git.blame", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path     string `json:"path"`
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Path == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "path is required")
		}
		// Resolve owning worktree + thread + relative path, mirroring git.file.
		var wt worktree.Worktree
		var threadID, relPath string
		if p.ThreadID != "" {
			if rec, ok := d.threads.get(p.ThreadID); ok {
				if rel, err := filepath.Rel(rec.Path, p.Path); err == nil &&
					!strings.HasPrefix(rel, "..") {
					wt = rec
					threadID = p.ThreadID
					relPath = filepath.ToSlash(rel)
				}
			}
		}
		if wt.Path == "" {
			s, rel, ok := d.gitCache.FindByPath(p.Path)
			if !ok {
				return map[string]any{"lines": []gitstatus.BlameLine{}}, nil
			}
			// FindByPath returns a snapshot; we just need the worktree behind
			// it. Look it up via the thread registry / sessions store.
			if rec, ok := d.threads.get(s.ThreadID); ok {
				wt = rec
			} else if rec, ok := d.sessions.Get(s.ThreadID); ok {
				wt = rec.Worktree
			}
			threadID = s.ThreadID
			relPath = rel
		}
		if wt.Path == "" {
			return map[string]any{"lines": []gitstatus.BlameLine{}}, nil
		}
		// Prefer the cache (keyed on the snapshot generation, busted on HEAD
		// move or save) so a repeated blame for an unchanged file never re-shells
		// `git blame`. Fall back to a direct compute only when the thread is not
		// cache-registered.
		var lines []gitstatus.BlameLine
		if cached, ok, err := d.gitCache.BlameFor(threadID, relPath); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		} else if ok {
			lines = cached
		} else {
			l, err := gitstatus.Blame(wt, relPath)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			lines = l
		}
		if lines == nil {
			lines = []gitstatus.BlameLine{}
		}
		return map[string]any{"lines": lines}, nil
	})

	// git.file returns line-level hunks for one absolute file path vs the
	// owning worktree's HEAD. The UI's gutter polls this per open buffer.
	d.srv.Handle("git.file", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Path     string `json:"path"`
			ThreadID string `json:"threadId"` // optional hint
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Path == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "path is required")
		}
		// Locate the owning worktree. The thread hint short-cuts the search;
		// otherwise FindByPath picks the most specific worktree containing
		// the file (so files in .agentkate/worktrees/<id>/ map to that thread,
		// not the parent workspace).
		var snap *gitstatus.Snapshot
		var relPath string
		if p.ThreadID != "" {
			if s, ok := d.gitCache.SnapshotFor(p.ThreadID); ok {
				if rec, ok := d.threads.get(p.ThreadID); ok {
					if rel, err := filepath.Rel(rec.Path, p.Path); err == nil &&
						!strings.HasPrefix(rel, "..") {
						snap = s
						relPath = filepath.ToSlash(rel)
					}
				}
			}
		}
		if snap == nil {
			s, rel, ok := d.gitCache.FindByPath(p.Path)
			if !ok {
				return map[string]any{"status": gitstatus.StatusClean,
					"hunks": []gitstatus.Hunk{}}, nil
			}
			snap, relPath = s, rel
		}
		fileStatus := gitstatus.StatusClean
		for _, f := range snap.Files {
			if f.Path == relPath {
				fileStatus = f.Status
				break
			}
		}
		hunks, generation, _, err := d.gitCache.HunksFor(snap.ThreadID, relPath)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if hunks == nil {
			hunks = []gitstatus.Hunk{}
		}
		return map[string]any{
			"threadId":   snap.ThreadID,
			"branch":     snap.Branch,
			"status":     fileStatus,
			"hunks":      hunks,
			"headSha":    snap.HeadSHA,
			"generation": generation,
		}, nil
	})

	// git.log returns one page of commits for the thread's worktree, with
	// lane/edge metadata so the UI can render a graph rail. Skip drives
	// pagination; Path narrows the view to a file's history (and disables the
	// graph, since the parent edges between filtered commits would be
	// synthetic).
	d.srv.Handle("git.log", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
			Skip     int    `json:"skip"`
			Limit    int    `json:"limit"`
			Path     string `json:"path"`
			Branch   string `json:"branch"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		var wt worktree.Worktree
		if p.ThreadID != "" {
			w, ok := d.threads.get(p.ThreadID)
			if !ok {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
			}
			wt = w
		} else if p.RepoRoot != "" {
			// Workspace-level log: synthesize a Worktree pointing at the
			// repo root so gitstatus.Log can resolve the requested branch.
			wt = worktree.Worktree{Path: p.RepoRoot, RepoRoot: p.RepoRoot}
		} else {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "git.log requires threadId or repoRoot")
		}
		opts := gitstatus.LogOptions{
			Skip:   p.Skip,
			Limit:  p.Limit,
			Path:   p.Path,
			Branch: p.Branch,
		}
		// The unfiltered HEAD graph for a registered thread goes through the
		// cache, which keeps one history walk per (thread, HEAD) so deep scroll
		// pages slice a precomputed array instead of re-walking git each time.
		// Path / branch-scoped views and the workspace (repoRoot) view fall
		// through to the bare walk — identical results either way.
		var entries []gitstatus.LogEntry
		if cached, ok, err := d.gitCache.LogPageFor(p.ThreadID, opts); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		} else if ok {
			entries = cached
		} else {
			e, err := gitstatus.Log(wt, opts)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			entries = e
		}
		if entries == nil {
			entries = []gitstatus.LogEntry{}
		}
		return map[string]any{"entries": entries}, nil
	})

	// git.commit.detail returns one commit's metadata + per-file change list
	// for the right-hand pane of the log viewer.
	d.srv.Handle("git.commit.detail", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
			SHA      string `json:"sha"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, err := resolveLogSource(d, p.ThreadID, p.RepoRoot)
		if err != nil {
			return nil, err
		}
		detail, err := gitstatus.CommitDetailFn(wt, p.SHA)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return detail, nil
	})

	// git.commit.diff returns the unified diff for one commit, optionally
	// scoped to a single file.
	d.srv.Handle("git.commit.diff", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			RepoRoot string `json:"repoRoot"` // workspace fallback when no thread
			SHA      string `json:"sha"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, err := resolveLogSource(d, p.ThreadID, p.RepoRoot)
		if err != nil {
			return nil, err
		}
		patch, err := gitstatus.CommitDiff(wt, p.SHA, p.Path)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"patch": patch}, nil
	})

	// --- worktree cleanup -----------------------------------------------------
	// SAFETY-CRITICAL. cleanup.analyze is a pure read that classifies every
	// worktree in a project as safe / review / blocked / orphaned. The verdict
	// is advisory to the UI; the server RE-DERIVES it in cleanup.archiveAndRemove
	// before anything destructive happens, so a stale client can never bypass
	// the gate.
	d.srv.Handle("cleanup.analyze", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Project string `json:"project"`
			Advise  bool   `json:"advise"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}

		cands := analyzeCleanupCandidates(d, p.Project)

		// Phase 2: optional Sonnet advisory. ADVISORY ONLY — AdviseCleanup
		// never changes State/Removable; on any LLM error it returns the
		// candidates unchanged.
		if p.Advise {
			cands = gitstatus.AdviseCleanup(ctx, "", "", cands)
		}
		if cands == nil {
			cands = []gitstatus.CleanupCandidate{}
		}
		return map[string]any{"candidates": cands}, nil
	})

	// cleanup.archiveAndRemove is THE destructive path. It NEVER trusts the
	// client: it re-resolves the worktree, re-runs AnalyzeCandidate, refuses on
	// any blocker, and refuses on warnings unless confirmDestroy is set. The
	// record is archived (reversibly, transcript left on disk) BEFORE git is
	// touched, so a failed Remove still preserves the record.
	d.srv.Handle("cleanup.archiveAndRemove", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID       string `json:"threadId"`
			ConfirmDestroy bool   `json:"confirmDestroy"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
		}

		// 1. Resolve the worktree. Prefer the live registry; fall back to the
		//    session record (an orphaned thread is not in the registry).
		wt, ok := d.threads.get(p.ThreadID)
		rec, recOK := d.sessions.Get(p.ThreadID)
		if !ok {
			if !recOK {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
			}
			wt = rec.Worktree
		}

		// 2. RE-RUN the analysis NOW, server-side. The snapshot may be stale or
		//    absent (orphaned); AnalyzeCandidate handles a nil snapshot.
		snap, _ := d.gitCache.SnapshotFor(p.ThreadID)
		running := d.sup.Running(p.ThreadID)
		title := ""
		if recOK {
			title = rec.Title
		}
		c := gitstatus.AnalyzeCandidate(wt, snap, running, title, time.Time{})

		// 3. Refuse on ANY blocker — never remove running / non-isolated /
		//    detached / broken worktrees.
		if !c.Removable {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"refusing to remove: "+cleanupBlockerReason(c.Blockers))
		}
		// 4. Refuse on warnings unless the client explicitly confirmed the loss.
		if len(c.Warnings) > 0 && !p.ConfirmDestroy {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"unmerged/uncommitted work present; confirmDestroy required")
		}

		// 5. Defensive stop — even though "running" is a blocker, guard against
		//    a process that started between analysis and now.
		_ = d.sup.Stop(p.ThreadID)

		// 6. Archive the record BEFORE touching git, so a failed Remove leaves
		//    the record (and transcript) intact and recoverable.
		if recOK {
			if err := d.sessions.Archive(p.ThreadID, "cleanup: "+string(c.State)); err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, "archive failed: "+err.Error())
			}
		}

		// 7. Remove the worktree (orphaned → Remove falls back to prune + branch -D).
		if err := worktree.Remove(wt); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}

		// 8. Drop the rest of the thread's state and notify, mirroring
		//    agent.discard's teardown.
		_ = d.summaries.Remove(p.ThreadID)
		d.gitCache.Forget(p.ThreadID)
		d.threads.remove(p.ThreadID)
		d.srv.Notify("agent.discarded", map[string]any{"threadId": p.ThreadID})
		d.srv.Notify("git.invalidated", map[string]any{"threadId": p.ThreadID})

		d.log.Info("cleanup.archiveAndRemove complete", "thread", p.ThreadID,
			"state", c.State, "confirmDestroy", p.ConfirmDestroy)
		return map[string]any{"ok": true, "archived": recOK}, nil
	})

	// cleanup.listArchived returns archived records newest-first.
	d.srv.Handle("cleanup.listArchived", func(_ context.Context, _ json.RawMessage) (any, error) {
		arch := d.sessions.ListArchived()
		if arch == nil {
			arch = []session.ArchiveRecord{}
		}
		return map[string]any{"archived": arch}, nil
	})

	// cleanup.restore moves an archived record back as a dormant, non-isolated
	// thread. Its worktree is gone, so it can only resume in the workspace.
	d.srv.Handle("cleanup.restore", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if err := d.sessions.Restore(p.ThreadID); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"ok": true}, nil
	})

	// --- code search ----------------------------------------------------------
	// search.code runs a filtered ripgrep across a project root. The UI calls
	// it from its global search panel.
	d.srv.Handle("search.code", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Query         string   `json:"query"`
			Root          string   `json:"root"`
			Regex         bool     `json:"regex"`
			CaseSensitive bool     `json:"caseSensitive"`
			WholeWord     bool     `json:"wholeWord"`
			Includes      []string `json:"includes"`
			Excludes      []string `json:"excludes"`
			MaxResults    int      `json:"maxResults"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.Root == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "root required")
		}
		res, err := search.Run(search.Options{
			Query:         p.Query,
			Root:          p.Root,
			Regex:         p.Regex,
			CaseSensitive: p.CaseSensitive,
			WholeWord:     p.WholeWord,
			Includes:      p.Includes,
			Excludes:      p.Excludes,
			MaxResults:    p.MaxResults,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return res, nil
	})
}

// resolveLogSource maps the (threadId, repoRoot) pair the log-viewer RPCs
// accept to a Worktree go-git can read. Workspace-branch sources synthesize a
// Worktree pointing at the repo root since the workspace itself is a working
// repo.
func resolveLogSource(d handlerDeps, threadID, repoRoot string) (worktree.Worktree, error) {
	if threadID != "" {
		w, ok := d.threads.get(threadID)
		if !ok {
			return worktree.Worktree{}, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+threadID)
		}
		return w, nil
	}
	if repoRoot != "" {
		return worktree.Worktree{Path: repoRoot, RepoRoot: repoRoot}, nil
	}
	return worktree.Worktree{}, ipc.Errorf(ipc.CodeInvalidParams, "requires threadId or repoRoot")
}

// startAgentThread creates the thread's worktree, writes its MCP config and
// launches the headless agent. Failures are reported as lifecycle events
// rather than as an error reply, since the agent.start reply has already been
// sent by the time this runs.
func startAgentThread(d handlerDeps, threadID, sessionID string, p agentStartParams) {
	wt, err := worktree.Create(p.WorkspacePath, threadID, p.Isolation)
	if err != nil {
		d.log.Error("worktree create failed", "thread", threadID, "err", err)
		emitLifecycle(d.srv, threadID, "error", "worktree: "+err.Error(), nil)
		return
	}
	wt.Number = d.sessions.NextNumber(p.WorkspacePath)
	d.threads.put(threadID, wt)
	d.gitCache.Register(wt)
	d.gitCache.Activate(threadID) // an agent is about to run here — watch it live

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, threadID, wt.Path, p.CoworkEnabled)
	if err != nil {
		emitLifecycle(d.srv, threadID, "error", "mcp config: "+err.Error(), &wt)
		return
	}

	// The UI sends a tier token ("opus"/"sonnet"/"haiku"/"fable"/""); resolve
	// it to a concrete --model id once here and persist that id, so a later
	// resume replays the exact same model.
	model := resolveModel(p.Model)

	if _, err := d.sup.Start(agent.StartOptions{
		ID:             threadID,
		WorkDir:        wt.Path,
		Prompt:         p.Prompt,
		MCPConfig:      mcpConfig,
		PermissionMode: p.PermissionMode,
		Effort:         p.Effort,
		Model:          model,
		Attachments:    p.Attachments,
		SessionID:      sessionID,
		CoworkEnabled:  p.CoworkEnabled,
	}); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, threadID, "error", err.Error(), &wt)
		return
	}

	// Persist the thread so it survives a stop, a crash or an Agent Kate
	// restart, and can later be resumed on this same Claude Code session.
	permMode := p.PermissionMode
	if permMode == "" {
		permMode = "acceptEdits"
	}
	if err := d.sessions.Put(session.Record{
		ThreadID:       threadID,
		SessionID:      sessionID,
		Project:        p.WorkspacePath,
		Worktree:       wt,
		PermissionMode: permMode,
		Effort:         p.Effort,
		Model:          model,
		Title:          summarizePrompt(p.Prompt),
		Created:        time.Now(),
		Status:         session.StatusRunning,
		CoworkEnabled:  p.CoworkEnabled,
	}); err != nil {
		d.log.Warn("could not persist thread record", "thread", threadID, "err", err)
	}

	mode := "directly in the workspace"
	if wt.Isolated {
		mode = "in an isolated worktree on " + wt.Branch
	}
	d.log.Info("agent thread started", "thread", threadID, "isolated", wt.Isolated, "dir", wt.Path)
	emitLifecycle(d.srv, threadID, "started", "running "+mode, &wt)
}

// resumeAgentThread re-launches a dormant thread. If a current compacted
// summary exists, a fresh Claude Code session is started seeded with that
// summary instead of replaying the full transcript — that is where the
// compaction savings actually land. Without a current summary the thread
// resumes on its original session via --resume as before.
func resumeAgentThread(d handlerDeps, rec session.Record) {
	if _, err := os.Stat(rec.Worktree.Path); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error",
			"worktree no longer exists: "+rec.Worktree.Path, nil)
		return
	}
	d.threads.put(rec.ThreadID, rec.Worktree)
	d.gitCache.Register(rec.Worktree)
	d.gitCache.Activate(rec.ThreadID) // resuming — this thread is active again, watch it

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, rec.ThreadID, rec.Worktree.Path, rec.CoworkEnabled)
	if err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", "mcp config: "+err.Error(), &rec.Worktree)
		return
	}

	// Pick a path: seed-from-summary if a current summary is on disk, else
	// classic --resume. A summary is "current" when it has been refreshed
	// after the last user/agent turn.
	sum, _ := d.summaries.Get(rec.ThreadID)
	current := sum != nil &&
		(rec.LastTurnAt.IsZero() || !rec.LastTurnAt.After(rec.SummaryUpdatedAt))

	opts := agent.StartOptions{
		ID:             rec.ThreadID,
		WorkDir:        rec.Worktree.Path,
		MCPConfig:      mcpConfig,
		PermissionMode: rec.PermissionMode,
		Effort:         rec.Effort,
		Model:          rec.Model,
		CoworkEnabled:  rec.CoworkEnabled,
	}
	var sessionIDForRecord string
	var detail string
	if current {
		// Fresh session seeded with the summary text. The instruction line
		// at the bottom asks the agent to acknowledge briefly so its first
		// turn is short — minimising the prefix that gets cached.
		newID := session.NewID()
		opts.SessionID = newID
		opts.Resume = false
		opts.Prompt = sum.Body +
			"\n\n---\n\nThe above is prior context from a session that has " +
			"been compacted. Acknowledge in one sentence that you have read " +
			"it, then wait for the user's next instruction."
		sessionIDForRecord = newID
		detail = "resumed from compacted summary (new session)"
	} else {
		opts.SessionID = rec.SessionID
		opts.Resume = true
		sessionIDForRecord = rec.SessionID
		detail = "resumed Claude Code session"
	}

	if _, err := d.sup.Start(opts); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, rec.ThreadID, "error", err.Error(), &rec.Worktree)
		return
	}

	_ = d.sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) {
		r.Status = session.StatusRunning
		if current {
			// The summary is now baked into the new session; clear our
			// staleness signals so the next compact cycle starts fresh.
			r.SessionID = sessionIDForRecord
			r.SummaryUpdatedAt = time.Time{}
			r.LastTurnAt = time.Time{}
		}
	})
	if current {
		// The previous summary belonged to the old session; drop it so a
		// missed exit-compact on the new session does not silently reuse
		// stale content.
		_ = d.summaries.Remove(rec.ThreadID)
	}
	d.log.Info("agent thread resumed",
		"thread", rec.ThreadID, "session", sessionIDForRecord, "seeded", current)
	emitLifecycle(d.srv, rec.ThreadID, "resumed", detail, &rec.Worktree)
}

// promoteAgentThread upgrades a non-isolated thread to an isolated worktree: it
// stops the agent, moves the working tree and Claude Code session into a fresh
// worktree, then resumes the thread there.
func promoteAgentThread(d handlerDeps, rec session.Record) {
	// Stop any live process and wait for it to exit before touching git.
	_ = d.sup.Stop(rec.ThreadID)
	for i := 0; i < 60 && d.sup.Running(rec.ThreadID); i++ {
		time.Sleep(200 * time.Millisecond)
	}

	iso, err := worktree.Promote(rec.Worktree)
	if err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", "promote: "+err.Error(), &rec.Worktree)
		return
	}
	// Relocate the Claude Code session so `--resume` finds it in the worktree.
	if err := session.PromoteTranscript(rec.SessionID, rec.ThreadID); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", "promote: "+err.Error(), &iso)
		return
	}

	rec.Worktree = iso
	_ = d.sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) { r.Worktree = iso })
	d.threads.put(rec.ThreadID, iso)
	d.gitCache.Register(iso)
	d.gitCache.Activate(rec.ThreadID) // re-point the watch onto the new isolated worktree
	d.log.Info("agent thread promoted", "thread", rec.ThreadID, "branch", iso.Branch)
	emitLifecycle(d.srv, rec.ThreadID, "promoted",
		"promoted to an isolated worktree on "+iso.Branch, &iso)

	// Bring the thread back up, now inside its isolated worktree.
	resumeAgentThread(d, rec)
}

// summarizePrompt makes a short, single-line title from an opening prompt.
func summarizePrompt(prompt string) string {
	s := strings.TrimSpace(prompt)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 70
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// normalizeTags cleans a raw tag slice for persistence: trims whitespace,
// drops empties, dedupes case-insensitively (keeping the first-seen casing),
// caps each tag at 32 characters and the whole set at 12 tags. Order of the
// surviving tags is preserved. Always returns a non-nil slice when there is at
// least one valid tag; an all-empty input yields nil so omitempty stays clean.
func normalizeTags(in []string) []string {
	const maxLen = 32
	const maxTags = 12
	var out []string
	seen := make(map[string]bool)
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len([]rune(t)) > maxLen {
			t = string([]rune(t)[:maxLen])
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// runHotCompactIfConfigured runs a Hot-Opus compaction on the live thread
// (if its strategy calls for one) before the caller terminates the process.
// Synchronous on purpose: the supervisor must wait for the compact to land
// before reap, so the assistant's final turn — the summary itself — is
// captured. No-op when the strategy is something else or the thread is
// already dead.
func runHotCompactIfConfigured(d handlerDeps, threadID string) {
	rec, ok := d.sessions.Get(threadID)
	if !ok {
		return
	}
	if compact.Strategy(rec.CompactStrategy).Resolve() != compact.ExitOpusHot {
		return
	}
	if !d.sup.Running(threadID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	text, err := d.sup.Compact(ctx, threadID, compact.CompactPrompt)
	if err != nil {
		d.log.Warn("hot-opus compact failed", "thread", threadID, "err", err)
		return
	}
	if strings.TrimSpace(text) == "" {
		d.log.Warn("hot-opus compact returned empty body", "thread", threadID)
		return
	}
	sum := compact.Summary{
		ThreadID:  threadID,
		SessionID: rec.SessionID,
		Strategy:  compact.ExitOpusHot,
		Stripped:  rec.CompactStrip,
		Created:   time.Now().UTC(),
		Body:      text,
	}
	if err := d.summaries.Put(sum); err != nil {
		d.log.Warn("could not store hot summary", "thread", threadID, "err", err)
		return
	}
	_ = d.sessions.UpdateQuiet(threadID, func(r *session.Record) {
		r.SummaryUpdatedAt = sum.Created
	})
	d.log.Info("hot-opus compact complete",
		"thread", threadID, "body_bytes", len(sum.Body))
}

// runHotCompactsAtShutdown fires Hot-Opus in parallel for every running
// thread configured for it, before the supervisor terminates them all.
// Each compact has its own 2-minute timeout, so a stuck thread cannot
// hold up shutdown of the others.
func runHotCompactsAtShutdown(d handlerDeps, progress shutdownProgressFn) {
	var targets []session.Record
	for _, rec := range d.sessions.List("") {
		if !d.sup.Running(rec.ThreadID) {
			continue
		}
		if compact.Strategy(rec.CompactStrategy).Resolve() != compact.ExitOpusHot {
			continue
		}
		targets = append(targets, rec)
	}
	total := len(targets)
	if total == 0 {
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	for _, rec := range targets {
		wg.Add(1)
		rec := rec
		safe.Go("shutdown.hotCompact", func() {
			defer wg.Done()
			runHotCompactIfConfigured(d, rec.ThreadID)
			// Report completion as each finishes so the shutdown dialog can show
			// "Compacting context for resume (i of N)".
			mu.Lock()
			done++
			progress("compacting", rec.Title, done, total)
			mu.Unlock()
		})
	}
	wg.Wait()
}

// shutdownProgressFn reports graceful-shutdown progress. phase is a stable code
// (preparing|compacting|stopping|draining|watchers|done) the UI maps to text;
// detail is an optional agent title; index/total drive the "(i of N)" counter.
type shutdownProgressFn func(phase, detail string, index, total int)

// exitCompactCap bounds how long shutdown waits for cold-exit compactions to
// finish. A cold `claude --resume` usually lands in a few seconds; the cap is
// a backstop so a hung one cannot wedge quit forever. Kept conservative so a
// dogfood quit never feels stuck — stragglers are cancelled and logged.
//
// IMPORTANT: the UI SIGKILLs the core if it doesn't exit within its own grace
// window after SIGTERM (CoreClient::~CoreClient, waitForFinished). That grace
// MUST stay larger than this cap, or the core is killed mid-drain at app quit
// and the summary goes missing — the very bug this fixes. Keep them in sync.
const exitCompactCap = 15 * time.Second

// exitCompactTracker owns the in-flight cold-exit compactions so the shutdown
// path can drain them before the process exits. ctx is shared by every spawned
// compaction; cancelling it (at the shutdown deadline) kills stragglers.
type exitCompactTracker struct {
	wg        sync.WaitGroup
	ctx       context.Context
	claudeBin string // injectable for tests; "" resolves to "claude" via PATH
}

// spawn launches a cold-exit compaction for rec as a tracked background
// goroutine and returns immediately. The goroutine is registered with the
// WaitGroup before it starts, so a later drain() reliably blocks on it.
func (e *exitCompactTracker) spawn(log *slog.Logger, sessions *session.Store,
	summaries *compact.Store, rec session.Record, strategy compact.Strategy) {
	e.wg.Add(1)
	safe.Go("shutdown.exitCompact", func() {
		defer e.wg.Done()
		runExitCompact(e.ctx, e.claudeBin, log, sessions, summaries, rec, strategy)
	})
}

// drain waits for all spawned cold-exit compactions to finish, bounded by cap.
// On timeout it cancels stragglers (killing any hung claude) and proceeds, so
// a wedged compaction can never block quit indefinitely.
func (e *exitCompactTracker) drain(cancel context.CancelFunc, cap time.Duration, log *slog.Logger) {
	if waitWithDeadline(&e.wg, cap) {
		return
	}
	log.Warn("exit compactions exceeded shutdown cap; cancelling stragglers", "cap", cap)
	cancel()
	// Give cancelled compactions a brief grace period to unwind, but never
	// block quit on one that ignores cancellation.
	if !waitWithDeadline(&e.wg, 2*time.Second) {
		log.Warn("exit compactions still running after cancel; exiting anyway")
	}
}

// waitWithDeadline blocks until wg drains or d elapses. Returns true if the
// WaitGroup drained in time, false on timeout.
func waitWithDeadline(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	safe.Go("shutdown.waitWithDeadline", func() {
		wg.Wait()
		close(done)
	})
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// runExitCompact performs a cold compaction after an agent exits. Errors are
// logged but do not block anything — the next resume will either find a usable
// summary or trigger the recovery dialog in the UI. ctx is the shutdown-scoped
// context: its cancellation (at the shutdown deadline) aborts a straggling
// claude rather than orphaning it. An empty claudeBin resolves to "claude".
func runExitCompact(ctx context.Context, claudeBin string, log *slog.Logger,
	sessions *session.Store, summaries *compact.Store,
	rec session.Record, strategy compact.Strategy) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	sum, err := compact.RunLLM(ctx, rec.ThreadID, strategy, compact.LLMOptions{
		ClaudeBin: claudeBin,
		WorkDir:   rec.Worktree.Path,
		SessionID: rec.SessionID,
		Model:     strategy.Model(),
		Timeout:   5 * time.Minute,
	})
	if err != nil {
		log.Warn("exit compaction failed",
			"thread", rec.ThreadID, "strategy", strategy, "err", err)
		return
	}
	if err := summaries.Put(sum); err != nil {
		log.Warn("could not store summary", "thread", rec.ThreadID, "err", err)
		return
	}
	_ = sessions.UpdateQuiet(rec.ThreadID, func(r *session.Record) {
		r.SummaryUpdatedAt = sum.Created
	})
	log.Info("exit compaction complete",
		"thread", rec.ThreadID, "strategy", strategy,
		"turns", sum.Turns, "body_bytes", len(sum.Body))
}

// resolveModel maps a UI-facing tier token ("opus", "sonnet", "haiku",
// "fable") to the concrete claude --model id we spawn or compact with. It is
// the single source of truth for the tier→id map, shared by the agent-spawn
// path (startAgentThread) and the compaction path (resolveCompactModel) so the
// two never disagree about what "opus" means. An empty or unrecognised token
// is returned trimmed and unchanged: "" leaves Claude Code on its configured
// default, and an already-full id (e.g. "claude-opus-4-8") passes through.
func resolveModel(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "opus":
		return "claude-opus-4-8"
	case "sonnet":
		return "claude-sonnet-4-6"
	case "haiku":
		return "claude-haiku-4-5-20251001"
	case "fable":
		return "claude-fable-5"
	default:
		return strings.TrimSpace(tier)
	}
}

// resolveCompactModel maps the UI-facing model token from the recovery dialog
// ("opus", "sonnet", "haiku", "fable", "local") to the claude --model id we
// spawn with and the strategy stamp for the resulting summary. The id half is
// delegated to resolveModel so spawn and compaction share one map. Empty or
// unrecognised tokens fall through to the programmatic compactor, the safe
// free fallback.
func resolveCompactModel(token string) (modelID string, strategy compact.Strategy, isLocal bool) {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "opus":
		return resolveModel("opus"), compact.ResumeOpusCold, false
	case "sonnet":
		return resolveModel("sonnet"), compact.ResumeSonnetCold, false
	case "haiku":
		return resolveModel("haiku"), compact.ResumeHaikuCold, false
	case "fable":
		// No dedicated Fable strategy bucket; stamp the closest (Sonnet-cold).
		return resolveModel("fable"), compact.ResumeSonnetCold, false
	case "local", "":
		return "", compact.ResumeLocal, true
	default:
		// Treat as a literal claude --model id; stamp the closest bucket.
		return token, compact.ResumeSonnetCold, false
	}
}

// emitLifecycle pushes a synthetic _lifecycle agent event to the UI.
func emitLifecycle(srv *ipc.Server, threadID, phase, detail string, wt *worktree.Worktree) {
	ev := map[string]any{"type": "_lifecycle", "phase": phase, "detail": detail}
	if wt != nil {
		ev["isolated"] = wt.Isolated
		ev["branch"] = wt.Branch
		ev["workdir"] = wt.Path
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	// Single lifecycle event, but sent in the same batch shape as the supervisor
	// relay so the UI has exactly one wire contract to parse.
	srv.Notify("agent.event", agentEventParams{
		ThreadID: threadID,
		Events:   []json.RawMessage{b},
	})
}

// writeMCPConfig writes a per-thread --mcp-config file that points `claude` at
// this binary's Cooperation MCP bridge subcommand, plus the opt-in Cowork desktop
// bridge when the thread enabled it (a second `akcore mcp ... --cowork` server).
func writeMCPConfig(exePath, socketPath, threadID, workspace string, coworkEnabled bool) (string, error) {
	servers := map[string]any{
		"cooperation": map[string]any{
			"type":    "stdio",
			"command": exePath,
			"args": []string{
				"mcp",
				"--socket", socketPath,
				"--thread", threadID,
				"--workspace", workspace,
			},
		},
	}
	if coworkEnabled {
		servers["cowork"] = map[string]any{
			"type":    "stdio",
			"command": exePath,
			"args": []string{
				"mcp",
				"--socket", socketPath,
				"--thread", threadID,
				"--workspace", workspace,
				"--cowork",
			},
		}
	}
	cfg := map[string]any{"mcpServers": servers}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "agentkate-mcp-"+threadID+"-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	return f.Name(), nil
}
