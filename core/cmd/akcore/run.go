package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers; only served when AKCORE_PPROF is set
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/compact"
	"agentkate/internal/coop"
	"agentkate/internal/cowork"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/kde"
	"agentkate/internal/kimi"
	"agentkate/internal/modes"
	"agentkate/internal/permission"
	"agentkate/internal/safe"
	"agentkate/internal/session"
	"agentkate/internal/skills"
	"agentkate/internal/vsix"
	"agentkate/internal/worktree"
)

// defaultSocketPath mirrors the path the UI computes, so a manually launched
// core and UI still meet. The UI normally passes --socket explicitly.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agentkate.sock")
	}
	return filepath.Join(os.TempDir(), "agentkate.sock")
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

func runCore() {
	socket := flag.String("socket", defaultSocketPath(), "Unix domain socket path for the UI bus")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	log.Info("akcore starting", "version", version, "pid", os.Getpid())

	// Optional debug profiling endpoint, off unless AKCORE_PPROF names a listen
	// address (use a loopback address, e.g. 127.0.0.1:6060). It serves
	// net/http/pprof for live heap/goroutine/alloc profiles, e.g.
	//   go tool pprof http://127.0.0.1:6060/debug/pprof/heap
	// Disabled by default, so it adds no attack surface in normal operation.
	if addr := os.Getenv("AKCORE_PPROF"); addr != "" {
		log.Info("pprof endpoint enabled", "addr", addr)
		safe.Go("akcore.pprof", func() {
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Warn("pprof endpoint stopped", "err", err)
			}
		})
	}

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
	attachSide := session.NewAttachmentStore(session.DefaultAttachmentDir())

	summaries, err := compact.NewStore(compact.DefaultDir())
	if err != nil {
		log.Error("cannot open the summary store", "err", err)
		os.Exit(1)
	}

	// Ensembles are user data, but an unreadable file must not stop the core:
	// the built-in catalogue alone is a working arena, so a corrupt modes.json
	// degrades to "your saved ensembles are missing", not "Agent Kate is down".
	ensembles, err := modes.NewStore(modes.DefaultPath())
	if err != nil {
		log.Warn("cannot open the ensemble store; built-in ensembles only", "err", err)
		ensembles, _ = modes.NewStore(filepath.Join(os.TempDir(), "agentkate-modes-fallback.json"))
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

	// LastTurnAt is only a coarse staleness signal for the compaction layer, but a
	// naive sessions.Update on every "result" rewrites the whole threads.json
	// synchronously on this hot relay goroutine — many times a second under load.
	// Throttle the persistence to at most once per thread per interval; the summary
	// is always taken after the last turn anyway, so a slightly lagged LastTurnAt
	// never causes a stale summary to be reused.
	const lastTurnPersistInterval = 3 * time.Second
	var lastTurnMu sync.Mutex
	lastTurnPersisted := map[string]time.Time{}
	// The Claude Code session id can change mid-session (e.g. an in-session
	// compaction starts a new one). Persist it whenever it changes so a later
	// --resume targets the latest session rather than the stale start-time id.
	lastSessionID := map[string]string{}

	// The harness registry is built after the supervisors below (their emit
	// callback is this relay), but the relay's compaction gate needs it — so
	// declare it here and assign once construction completes. Events only flow
	// after handlers are registered, well past that point.
	var harnesses *harness.Registry

	// Turn tracker: the backend-agnostic idle/busy mirror agent.wait blocks on.
	// The handlers mark turns queued; the relay below feeds every event back in
	// (results end turns, terminal lifecycles end threads).
	turns := agent.NewTurnTracker()

	// The agent supervisors relay every thread event to the UI as a
	// notification, so the agent panel can render the conversation live. Events
	// arrive pre-coalesced as an ordered batch; we forward the whole batch in a
	// single notification and run the per-event side effects in order. Both
	// backends share this one relay — the kimi supervisor already translates
	// its ACP traffic into the same Claude-shaped events.
	relayEvents := func(threadID string, events []json.RawMessage) {
		if len(events) == 0 {
			return
		}
		srv.Notify("agent.event", agentEventParams{ThreadID: threadID, Events: events})
		for _, event := range events {
			turns.Observe(threadID, event)
			var probe struct {
				Type      string `json:"type"`
				Phase     string `json:"phase"`
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal(event, &probe) != nil {
				continue
			}
			// Persist a changed session id (rare — only on an in-session compaction)
			// so resume follows it. UpdateQuiet: bookkeeping, not user activity.
			if probe.SessionID != "" {
				lastTurnMu.Lock()
				changed := lastSessionID[threadID] != probe.SessionID
				if changed {
					lastSessionID[threadID] = probe.SessionID
				}
				lastTurnMu.Unlock()
				if changed {
					_ = sessions.UpdateQuiet(threadID, func(r *session.Record) {
						r.SessionID = probe.SessionID
					})
				}
			}
			// Bump LastTurnAt on each turn completion. This is the staleness
			// signal for the compaction layer — if the summary is older than the
			// last turn, it needs to be refreshed before resume.
			if probe.Type == "result" {
				now := time.Now()
				lastTurnMu.Lock()
				due := now.Sub(lastTurnPersisted[threadID]) >= lastTurnPersistInterval
				if due {
					lastTurnPersisted[threadID] = now
				}
				lastTurnMu.Unlock()
				if due {
					_ = sessions.Update(threadID, func(r *session.Record) {
						r.LastTurnAt = now
					})
				}
				continue
			}
			// When a thread exits, drop its cooperation locks and presence, and
			// mark it dormant so it can be resumed later. If the thread is
			// configured for a cold-exit compaction strategy, fire it in the
			// background — Hot-Opus is a separate pre-reap flow.
			if probe.Type == "_lifecycle" && (probe.Phase == "exited" || probe.Phase == "interrupted") {
				coopState.ClearOwner(threadID)
				srv.NotifyPrimaryUI("coop.changed", map[string]any{})
				// Drop this thread's throttle bookkeeping so the maps don't grow
				// for the life of the process across many agents.
				lastTurnMu.Lock()
				delete(lastTurnPersisted, threadID)
				delete(lastSessionID, threadID)
				lastTurnMu.Unlock()
				_ = sessions.UpdateQuiet(threadID, func(r *session.Record) {
					r.Status = session.StatusDormant
				})
				// Cold-exit compaction runs only on a normal exit; a user
				// interrupt is meant to be immediate, so we don't spend a turn
				// summarising it — and only for harnesses that support
				// compaction at all.
				if probe.Phase == "exited" {
					if rec, ok := sessions.Get(threadID); ok && harnesses != nil {
						if h, ok := harnesses.Get(rec.Backend); ok && h.Capabilities().Compaction {
							strat := compact.Strategy(rec.CompactStrategy).Resolve()
							if strat.RunsOnExit() && strat != compact.ExitOpusHot {
								coldCompacts.spawn(log, sessions, summaries, rec, strat)
							}
						}
					}
				}
			}
		}
	}
	sup := agent.NewSupervisor("", log, relayEvents)
	// The kimi supervisor drives Kimi Code threads (`kimi acp`, ACP). Its
	// permission asks go through the same broker + permission.requested
	// notification the Claude MCP bridge uses, so the UI approval flow is
	// identical across backends.
	ksup := kimi.NewSupervisor("", log, relayEvents,
		func(threadID, toolName string, input json.RawMessage) bool {
			dec, ok := askHumanPermission(srv, broker, threadID, toolName, input)
			return ok && dec.Allow
		}, "")

	// The harness registry: each supervisor wrapped in its adapter, in the
	// order pickers list engines. "claude" is the default — persisted records
	// use an empty Backend for it. Adding a backend means registering one
	// more adapter here (see docs/HARNESSES.md).
	harnesses = harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, exePath, *socket))
	harnesses.Register(newKimiHarness(ksup, exePath, *socket))

	// --- KDE Plasma Cowork (opt-in desktop see/control; off by default) --------
	// The shared D-Bus client and consent authority are constructed eagerly so the
	// capability probe (cowork.status) is accurate, but no thread can use them until
	// it is opted in (session.Record.CoworkEnabled) and the user grants consent.
	kdeClient, kerr := kde.New(log)
	if kerr != nil {
		log.Warn("cowork: KDE session bus unavailable; desktop features disabled", "err", kerr)
		kdeClient = nil
	}
	coworkSvc, coworkWarn, cerr := cowork.New(cowork.DefaultGrantsPath(), cowork.DefaultAuditPath(), cowork.DefaultPolicyPath(), kdeClient, coworkNotifier{srv}, log)
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
		// Teach the anti-escalation guards Agent Kate's own identity so a free pointer can
		// never click our consent/kill-switch UI (plan 09 §7). The AK window is owned by
		// the UI (Qt) process, which spawns akcore as a child (CoreClient QProcess) — so
		// our PARENT pid is the window owner; our own pid is added belt-and-suspenders.
		// We seed both plausible resourceClass spellings (the Wayland app_id may be the
		// reverse-DNS desktop name or the bare component name) so the geometric class
		// match holds whichever KWin reports.
		coworkSvc.SetSelfIdentity(
			[]string{"org.kde.agentkate", "agentkate"},
			[]int{os.Getppid(), os.Getpid()},
		)
	}

	deps := handlerDeps{
		srv:        srv,
		sup:        sup,
		harnesses:  harnesses,
		turns:      turns,
		orchGrants: newOrchGrants(),
		coop:       coopState,
		threads:    threads,
		broker:     broker,
		extensions: extensions,
		sessions:   sessions,
		attachSide: attachSide,
		summaries:  summaries,
		modes:      ensembles,
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
				if deps.agentRunning(rec.ThreadID) {
					running++
				}
			}
			progress("preparing", "", 0, running)
			// Hot-Opus compaction runs while the threads are still live and
			// cache-warm; it emits its own per-agent "compacting" progress.
			runHotCompactsAtShutdown(deps, progress)
			progress("stopping", "", 0, running)
			for _, h := range harnesses.All() {
				h.StopAll()
			}
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
