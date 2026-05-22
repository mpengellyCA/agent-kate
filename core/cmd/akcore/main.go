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
	"agentkate/internal/coop"
	"agentkate/internal/ipc"
	"agentkate/internal/permission"
	"agentkate/internal/session"
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

	sessions, err := session.NewStore(session.DefaultPath())
	if err != nil {
		log.Error("cannot open the thread store", "err", err)
		os.Exit(1)
	}

	// The agent supervisor relays every thread event to the UI as a
	// notification, so the agent panel can render the conversation live.
	sup := agent.NewSupervisor("", log, func(threadID string, event json.RawMessage) {
		srv.Notify("agent.event", agentEventParams{ThreadID: threadID, Event: event})
		// When a thread exits, drop its cooperation locks and presence, and
		// mark it dormant so it can be resumed later.
		var probe struct {
			Type  string `json:"type"`
			Phase string `json:"phase"`
		}
		if json.Unmarshal(event, &probe) == nil &&
			probe.Type == "_lifecycle" && probe.Phase == "exited" {
			coopState.ClearOwner(threadID)
			_ = sessions.Update(threadID, func(r *session.Record) {
				r.Status = session.StatusDormant
			})
		}
	})

	registerHandlers(handlerDeps{
		srv:        srv,
		sup:        sup,
		coop:       coopState,
		threads:    threads,
		broker:     broker,
		extensions: extensions,
		sessions:   sessions,
		socketPath: *socket,
		exePath:    exePath,
		log:        log,
	})

	// Don't outlive the UI: when the last client disconnects, shut down.
	srv.OnAllClientsGone(func() {
		log.Info("ui disconnected; akcore shutting down")
		stop()
	})

	serveErr := srv.Serve(ctx)
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
		return map[string]any{"ok": true}, nil
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
	d.threads.put(threadID, wt)

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

// resumeAgentThread re-launches a dormant thread on its existing Claude Code
// session, reusing the worktree it left behind.
func resumeAgentThread(d handlerDeps, rec session.Record) {
	if _, err := os.Stat(rec.Worktree.Path); err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error",
			"worktree no longer exists: "+rec.Worktree.Path, nil)
		return
	}
	d.threads.put(rec.ThreadID, rec.Worktree)

	mcpConfig, err := writeMCPConfig(d.exePath, d.socketPath, rec.ThreadID, rec.Worktree.Path)
	if err != nil {
		emitLifecycle(d.srv, rec.ThreadID, "error", "mcp config: "+err.Error(), &rec.Worktree)
		return
	}

	if _, err := d.sup.Start(agent.StartOptions{
		ID:             rec.ThreadID,
		WorkDir:        rec.Worktree.Path,
		MCPConfig:      mcpConfig,
		PermissionMode: rec.PermissionMode,
		SessionID:      rec.SessionID,
		Resume:         true,
	}); err != nil {
		os.Remove(mcpConfig)
		emitLifecycle(d.srv, rec.ThreadID, "error", err.Error(), &rec.Worktree)
		return
	}

	_ = d.sessions.Update(rec.ThreadID, func(r *session.Record) {
		r.Status = session.StatusRunning
	})
	d.log.Info("agent thread resumed", "thread", rec.ThreadID, "session", rec.SessionID)
	emitLifecycle(d.srv, rec.ThreadID, "resumed", "resumed Claude Code session", &rec.Worktree)
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
