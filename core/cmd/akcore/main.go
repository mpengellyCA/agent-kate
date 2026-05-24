// Command akcore is the AgentKate orchestration core. It supervises agent and
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
	"agentkate/internal/gitstatus"
	"agentkate/internal/ipc"
	"agentkate/internal/permission"
	"agentkate/internal/session"
	"agentkate/internal/skills"
	"agentkate/internal/vsix"
	"agentkate/internal/worktree"
)

// version is the akcore protocol/build version reported in the handshake.
const version = "0.1.0"

// defaultSocketPath mirrors the path the UI computes, so a manually launched
// core and UI still meet. The UI normally passes --socket explicitly.
func defaultSocketPath() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "agentkate.sock")
	}
	return filepath.Join(os.TempDir(), "agentkate.sock")
}

func main() {
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

	// The agent supervisor relays every thread event to the UI as a
	// notification, so the agent panel can render the conversation live.
	sup := agent.NewSupervisor("", log, func(threadID string, event json.RawMessage) {
		srv.Notify("agent.event", agentEventParams{ThreadID: threadID, Event: event})
		var probe struct {
			Type  string `json:"type"`
			Phase string `json:"phase"`
		}
		if json.Unmarshal(event, &probe) != nil {
			return
		}
		// Bump LastTurnAt on each turn completion. This is the staleness
		// signal for the compaction layer — if the summary is older than the
		// last turn, it needs to be refreshed before resume.
		if probe.Type == "result" {
			_ = sessions.Update(threadID, func(r *session.Record) {
				r.LastTurnAt = time.Now()
			})
			return
		}
		// When a thread exits, drop its cooperation locks and presence, and
		// mark it dormant so it can be resumed later. If the thread is
		// configured for a cold-exit compaction strategy, fire it in the
		// background — Hot-Opus is a separate pre-reap flow.
		if probe.Type == "_lifecycle" && probe.Phase == "exited" {
			coopState.ClearOwner(threadID)
			_ = sessions.Update(threadID, func(r *session.Record) {
				r.Status = session.StatusDormant
			})
			if rec, ok := sessions.Get(threadID); ok {
				strat := compact.Strategy(rec.CompactStrategy).Resolve()
				if strat.RunsOnExit() && strat != compact.ExitOpusHot {
					go runExitCompact(log, sessions, summaries, rec, strat)
				}
			}
		}
	})

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
		socketPath: *socket,
		exePath:    exePath,
		log:        log,
	}
	registerHandlers(deps)

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
	// Run Hot-Opus compacts in parallel for any thread configured for them
	// while the live process is still cache-warm; cold strategies fire from
	// the exit lifecycle handler after the process terminates.
	runHotCompactsAtShutdown(deps)
	sup.StopAll()
	if serveErr != nil {
		log.Error("ipc server stopped", "err", serveErr)
		os.Exit(1)
	}
	log.Info("akcore stopped")
}

// --- IPC parameter / result types ------------------------------------------

type agentEventParams struct {
	ThreadID string          `json:"threadId"`
	Event    json.RawMessage `json:"event"`
}

type agentStartParams struct {
	WorkspacePath  string             `json:"workspacePath"`
	Prompt         string             `json:"prompt"`
	PermissionMode string             `json:"permissionMode"`
	Effort         string             `json:"effort"`    // claude --effort level; "" = default
	Model          string             `json:"model"`     // claude --model id; "" = Claude Code default
	Isolation      string             `json:"isolation"` // worktree.Mode*; "" = auto
	Attachments    []agent.Attachment `json:"attachments"`
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
	socketPath string
	exePath    string
	log        *slog.Logger
}

// registerHandlers wires the JSON-RPC methods the core serves.
func registerHandlers(d handlerDeps) {
	d.srv.Handle("handshake", func(_ context.Context, _ json.RawMessage) (any, error) {
		d.log.Info("handshake received")
		return map[string]any{
			"name":    "akcore",
			"version": version,
			"pid":     os.Getpid(),
		}, nil
	})

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
		go startAgentThread(d, threadID, sessionID, p)
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
		go resumeAgentThread(d, rec)
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
		go promoteAgentThread(d, rec)
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
	// user can attach any past conversation — even ones AgentKate did not start.
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
	// AgentKate thread, which the UI then resumes like any other.
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
		_ = d.sessions.Update(p.ThreadID, func(r *session.Record) {
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
		ext, err := d.extensions.Install(ctx, p.ExtensionID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		d.log.Info("extension installed", "id", ext.ID, "version", ext.Version,
			"hasServer", ext.Server != nil)
		return ext, nil
	})

	d.srv.Handle("vsix.list", func(_ context.Context, _ json.RawMessage) (any, error) {
		exts, err := d.extensions.List()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		if exts == nil {
			exts = []*vsix.Extension{}
		}
		return map[string]any{"extensions": exts}, nil
	})

	// --- Claude Code skills ------------------------------------------------
	// skills.listCatalog returns every skill in the central AgentKate catalog
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
		// Resolve owning worktree + relative path, mirroring git.file.
		var wt worktree.Worktree
		var relPath string
		if p.ThreadID != "" {
			if rec, ok := d.threads.get(p.ThreadID); ok {
				if rel, err := filepath.Rel(rec.Path, p.Path); err == nil &&
					!strings.HasPrefix(rel, "..") {
					wt = rec
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
			relPath = rel
		}
		if wt.Path == "" {
			return map[string]any{"lines": []gitstatus.BlameLine{}}, nil
		}
		lines, err := gitstatus.Blame(wt, relPath)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
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
		entries, err := gitstatus.Log(wt, gitstatus.LogOptions{
			Skip:   p.Skip,
			Limit:  p.Limit,
			Path:   p.Path,
			Branch: p.Branch,
		})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
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
			SHA      string `json:"sha"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
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
			SHA      string `json:"sha"`
			Path     string `json:"path"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		wt, ok := d.threads.get(p.ThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		patch, err := gitstatus.CommitDiff(wt, p.SHA, p.Path)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"patch": patch}, nil
	})
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

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, threadID, wt.Path)
	if err != nil {
		emitLifecycle(d.srv, threadID, "error", "mcp config: "+err.Error(), &wt)
		return
	}

	if _, err := d.sup.Start(agent.StartOptions{
		ID:             threadID,
		WorkDir:        wt.Path,
		Prompt:         p.Prompt,
		MCPConfig:      mcpConfig,
		PermissionMode: p.PermissionMode,
		Effort:         p.Effort,
		Model:          p.Model,
		Attachments:    p.Attachments,
		SessionID:      sessionID,
	}); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, threadID, "error", err.Error(), &wt)
		return
	}

	// Persist the thread so it survives a stop, a crash or an AgentKate
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
		Model:          p.Model,
		Title:          summarizePrompt(p.Prompt),
		Created:        time.Now(),
		Status:         session.StatusRunning,
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

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, rec.ThreadID, rec.Worktree.Path)
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

	_ = d.sessions.Update(rec.ThreadID, func(r *session.Record) {
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
	_ = d.sessions.Update(rec.ThreadID, func(r *session.Record) { r.Worktree = iso })
	d.threads.put(rec.ThreadID, iso)
	d.gitCache.Register(iso)
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
	_ = d.sessions.Update(threadID, func(r *session.Record) {
		r.SummaryUpdatedAt = sum.Created
	})
	d.log.Info("hot-opus compact complete",
		"thread", threadID, "body_bytes", len(sum.Body))
}

// runHotCompactsAtShutdown fires Hot-Opus in parallel for every running
// thread configured for it, before the supervisor terminates them all.
// Each compact has its own 2-minute timeout, so a stuck thread cannot
// hold up shutdown of the others.
func runHotCompactsAtShutdown(d handlerDeps) {
	var wg sync.WaitGroup
	for _, rec := range d.sessions.List("") {
		if !d.sup.Running(rec.ThreadID) {
			continue
		}
		if compact.Strategy(rec.CompactStrategy).Resolve() != compact.ExitOpusHot {
			continue
		}
		wg.Add(1)
		go func(threadID string) {
			defer wg.Done()
			runHotCompactIfConfigured(d, threadID)
		}(rec.ThreadID)
	}
	wg.Wait()
}

// runExitCompact performs a cold compaction in the background after an agent
// exits. Errors are logged but do not block anything — the next resume will
// either find a usable summary or trigger the recovery dialog in the UI.
func runExitCompact(log *slog.Logger, sessions *session.Store, summaries *compact.Store,
	rec session.Record, strategy compact.Strategy) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	sum, err := compact.RunLLM(ctx, rec.ThreadID, strategy, compact.LLMOptions{
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
	_ = sessions.Update(rec.ThreadID, func(r *session.Record) {
		r.SummaryUpdatedAt = sum.Created
	})
	log.Info("exit compaction complete",
		"thread", rec.ThreadID, "strategy", strategy,
		"turns", sum.Turns, "body_bytes", len(sum.Body))
}

// resolveCompactModel maps the UI-facing model token from the recovery dialog
// ("opus", "sonnet", "haiku", "local") to the claude --model id we spawn with
// and the strategy stamp for the resulting summary. Empty or unrecognised
// tokens fall through to the programmatic compactor, the safe free fallback.
func resolveCompactModel(token string) (modelID string, strategy compact.Strategy, isLocal bool) {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "opus":
		return "claude-opus-4-7", compact.ResumeOpusCold, false
	case "sonnet":
		return "claude-sonnet-4-6", compact.ResumeSonnetCold, false
	case "haiku":
		return "claude-haiku-4-5-20251001", compact.ResumeHaikuCold, false
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
	srv.Notify("agent.event", agentEventParams{ThreadID: threadID, Event: b})
}

// writeMCPConfig writes a per-thread --mcp-config file that points `claude` at
// this binary's Cooperation MCP bridge subcommand.
func writeMCPConfig(exePath, socketPath, threadID, workspace string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
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
		},
	}
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
