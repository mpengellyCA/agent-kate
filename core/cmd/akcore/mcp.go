package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"agentkate/internal/coop"
	"agentkate/internal/ipc"
)

const (
	mcpServerName          = "agentkate-cooperation"
	defaultProtocolVersion = "2025-06-18"
)

// mcpBridge is the Cooperation MCP stdio server. Each agent thread spawns its
// own bridge (via `akcore mcp`); the bridge speaks MCP to `claude` on stdio and
// JSON-RPC to the core over the IPC socket, so cooperation state stays central.
type mcpBridge struct {
	client    *ipc.Client
	thread    string
	workspace string
	log       *slog.Logger

	mu  sync.Mutex // guards out across concurrent handlers
	out *bufio.Writer
}

// runMCPBridge is the entry point for `akcore mcp ...`.
func runMCPBridge(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	socket := fs.String("socket", "", "core IPC socket path")
	thread := fs.String("thread", "", "agent thread id")
	workspace := fs.String("workspace", "", "workspace path")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := ipc.Dial(*socket)
	if err != nil {
		log.Error("mcp bridge cannot reach core", "socket", *socket, "err", err)
		os.Exit(1)
	}
	defer client.Close()

	b := &mcpBridge{
		client:    client,
		thread:    *thread,
		workspace: *workspace,
		log:       log,
		out:       bufio.NewWriter(os.Stdout),
	}
	b.serve()
}

func (b *mcpBridge) serve() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f ipc.Frame
		if err := json.Unmarshal(line, &f); err != nil {
			b.log.Warn("bad mcp frame", "err", err)
			continue
		}
		// Handle concurrently: a pending permission request must not block
		// other MCP traffic (pings, further tool calls).
		go b.handle(&f)
	}
}

func (b *mcpBridge) handle(f *ipc.Frame) {
	switch f.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(f.Params, &p)
		protocol := p.ProtocolVersion
		if protocol == "" {
			protocol = defaultProtocolVersion
		}
		b.reply(f, map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": version},
		})
	case "notifications/initialized":
		// notification — no response
	case "ping":
		b.reply(f, map[string]any{})
	case "tools/list":
		b.reply(f, map[string]any{"tools": toolDefs()})
	case "tools/call":
		b.handleToolCall(f)
	default:
		if f.ID != nil {
			b.replyError(f, ipc.CodeMethodNotFound, "method not found: "+f.Method)
		}
	}
}

func (b *mcpBridge) handleToolCall(f *ipc.Frame) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(f.Params, &p); err != nil {
		b.replyError(f, ipc.CodeInvalidParams, err.Error())
		return
	}
	text, err := b.runTool(p.Name, p.Arguments)
	if err != nil {
		b.reply(f, toolResult(err.Error(), true))
		return
	}
	b.reply(f, toolResult(text, false))
}

func (b *mcpBridge) runTool(name string, args json.RawMessage) (string, error) {
	switch name {
	case "whoami":
		return fmt.Sprintf("thread: %s\nworkspace: %s", b.thread, b.workspace), nil

	case "list_open_files":
		var res struct {
			Files []coop.OpenFile `json:"files"`
		}
		if err := b.client.Call("coop.listOpenFiles", map[string]any{}, &res); err != nil {
			return "", err
		}
		if len(res.Files) == 0 {
			return "No files are currently open in the arena.", nil
		}
		var sb strings.Builder
		for _, fl := range res.Files {
			fmt.Fprintf(&sb, "%s  (open by: %s)\n", fl.Path, fl.Owner)
		}
		return strings.TrimRight(sb.String(), "\n"), nil

	case "post_note":
		var a struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.Text) == "" {
			return "", fmt.Errorf("post_note requires a non-empty 'text'")
		}
		var note coop.Note
		if err := b.client.Call("coop.postNote",
			map[string]any{"author": b.thread, "text": a.Text}, &note); err != nil {
			return "", err
		}
		return fmt.Sprintf("Note #%d posted to the cooperation board.", note.ID), nil

	case "read_notes":
		var res struct {
			Notes []coop.Note `json:"notes"`
		}
		if err := b.client.Call("coop.readNotes", map[string]any{}, &res); err != nil {
			return "", err
		}
		if len(res.Notes) == 0 {
			return "The cooperation board is empty.", nil
		}
		var sb strings.Builder
		for _, n := range res.Notes {
			fmt.Fprintf(&sb, "#%d [%s] %s\n", n.ID, n.Author, n.Text)
		}
		return strings.TrimRight(sb.String(), "\n"), nil

	case "get_presence":
		var res struct {
			Presence  []coop.Presence `json:"presence"`
			Claims    []coop.Claim    `json:"claims"`
			OpenFiles []coop.OpenFile `json:"openFiles"`
		}
		if err := b.client.Call("coop.getPresence", map[string]any{}, &res); err != nil {
			return "", err
		}
		var sb strings.Builder
		sb.WriteString("Focus:\n")
		if len(res.Presence) == 0 {
			sb.WriteString("  (nobody is reporting focus)\n")
		}
		for _, p := range res.Presence {
			ff := p.FocusedFile
			if ff == "" {
				ff = "(none)"
			}
			fmt.Fprintf(&sb, "  %s → %s\n", p.Owner, ff)
		}
		sb.WriteString("Claimed files:\n")
		if len(res.Claims) == 0 {
			sb.WriteString("  (none)\n")
		}
		for _, c := range res.Claims {
			fmt.Fprintf(&sb, "  %s — held by %s\n", c.Path, c.Owner)
		}
		sb.WriteString("Open files:\n")
		if len(res.OpenFiles) == 0 {
			sb.WriteString("  (none)\n")
		}
		for _, f := range res.OpenFiles {
			fmt.Fprintf(&sb, "  %s (open by %s)\n", f.Path, f.Owner)
		}
		return strings.TrimRight(sb.String(), "\n"), nil

	case "claim_file":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.Path) == "" {
			return "", fmt.Errorf("claim_file requires a 'path'")
		}
		var res struct {
			OK     bool   `json:"ok"`
			Holder string `json:"holder"`
		}
		if err := b.client.Call("coop.claimFile",
			map[string]any{"path": a.Path, "owner": b.thread}, &res); err != nil {
			return "", err
		}
		if res.OK {
			return fmt.Sprintf("You now hold the claim on %s.", a.Path), nil
		}
		return fmt.Sprintf("%s is already claimed by %s — coordinate before editing it.",
			a.Path, res.Holder), nil

	case "release_file":
		var a struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.Path) == "" {
			return "", fmt.Errorf("release_file requires a 'path'")
		}
		if err := b.client.Call("coop.releaseFile",
			map[string]any{"path": a.Path, "owner": b.thread}, nil); err != nil {
			return "", err
		}
		return fmt.Sprintf("Released your claim on %s.", a.Path), nil

	case "request_review":
		var a struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.Summary) == "" {
			return "", fmt.Errorf("request_review requires a 'summary'")
		}
		if err := b.client.Call("coop.requestReview",
			map[string]any{"thread": b.thread, "summary": a.Summary}, nil); err != nil {
			return "", err
		}
		return "Review requested — the human has been notified in AgentKate.", nil

	case "list_agents":
		var a struct {
			AllWorkspaces bool `json:"all_workspaces"`
		}
		_ = json.Unmarshal(args, &a)
		project := b.workspace
		if a.AllWorkspaces {
			project = ""
		}
		var res struct {
			Threads []struct {
				ThreadID string    `json:"threadId"`
				Project  string    `json:"project"`
				Title    string    `json:"title"`
				Status   string    `json:"status"`
				Branch   string    `json:"branch"`
				Path     string    `json:"path"`
				Isolated bool      `json:"isolated"`
				Number   int       `json:"number"`
				Created  time.Time `json:"created"`
				LastTurn time.Time `json:"lastTurn"`
				Model    string    `json:"model"`
			} `json:"threads"`
		}
		if err := b.client.Call("agent.list",
			map[string]any{"project": project}, &res); err != nil {
			return "", err
		}
		if len(res.Threads) == 0 {
			return "No agent threads on record.", nil
		}
		now := time.Now()
		var sb strings.Builder
		for _, t := range res.Threads {
			self := ""
			if t.ThreadID == b.thread {
				self = " (you)"
			}
			title := t.Title
			if title == "" {
				title = "(no title)"
			}
			age := now.Sub(t.Created).Round(time.Minute)
			idleNote := ""
			if !t.LastTurn.IsZero() {
				idleNote = fmt.Sprintf(", idle %s", now.Sub(t.LastTurn).Round(time.Minute))
			}
			wtKind := "workspace"
			if t.Isolated {
				wtKind = "isolated"
			}
			fmt.Fprintf(&sb, "#%d %s%s — %s [%s, %s, age %s%s]\n",
				t.Number, t.ThreadID, self, title, t.Status, wtKind, age, idleNote)
			if t.Branch != "" {
				fmt.Fprintf(&sb, "    branch: %s\n", t.Branch)
			}
			if t.Path != "" {
				fmt.Fprintf(&sb, "    path:   %s\n", t.Path)
			}
		}
		return strings.TrimRight(sb.String(), "\n"), nil

	case "discard_agent":
		var a struct {
			ThreadID string `json:"thread_id"`
			Force    bool   `json:"force"`
		}
		_ = json.Unmarshal(args, &a)
		a.ThreadID = strings.TrimSpace(a.ThreadID)
		if a.ThreadID == "" {
			return "", fmt.Errorf("discard_agent requires 'thread_id'")
		}
		if a.ThreadID == b.thread {
			return "", fmt.Errorf("an agent cannot discard itself — ask the human to remove this thread")
		}
		var list struct {
			Threads []struct {
				ThreadID string `json:"threadId"`
				Status   string `json:"status"`
				Branch   string `json:"branch"`
				Path     string `json:"path"`
				Isolated bool   `json:"isolated"`
			} `json:"threads"`
		}
		if err := b.client.Call("agent.list", map[string]any{"project": ""}, &list); err != nil {
			return "", err
		}
		var found *struct {
			ThreadID string `json:"threadId"`
			Status   string `json:"status"`
			Branch   string `json:"branch"`
			Path     string `json:"path"`
			Isolated bool   `json:"isolated"`
		}
		for i := range list.Threads {
			if list.Threads[i].ThreadID == a.ThreadID {
				found = &list.Threads[i]
				break
			}
		}
		if found == nil {
			return "", fmt.Errorf("unknown thread %q", a.ThreadID)
		}
		if found.Status == "running" && !a.Force {
			return "", fmt.Errorf("thread %s is still running; stop it first or pass force=true",
				a.ThreadID)
		}
		if err := b.client.Call("agent.discard",
			map[string]any{"threadId": a.ThreadID}, nil); err != nil {
			return "", err
		}
		detail := "removed from registry"
		if found.Isolated {
			detail = fmt.Sprintf("removed worktree %s and deleted branch %s",
				found.Path, found.Branch)
		}
		return fmt.Sprintf("Discarded agent %s (%s).", a.ThreadID, detail), nil

	case "request_permission":
		// Claude Code calls this for any gated tool. Forward the request to
		// the core (which prompts the human in the agent panel) and answer
		// with the {behavior} JSON the permission protocol expects.
		var a struct {
			ToolName  string          `json:"tool_name"`
			ToolNameB string          `json:"toolName"`
			Input     json.RawMessage `json:"input"`
			InputB    json.RawMessage `json:"tool_input"`
		}
		_ = json.Unmarshal(args, &a)
		toolName := a.ToolName
		if toolName == "" {
			toolName = a.ToolNameB
		}
		input := a.Input
		if len(input) == 0 {
			input = a.InputB
		}
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}

		var res struct {
			Allow        bool            `json:"allow"`
			UpdatedInput json.RawMessage `json:"updatedInput"`
		}
		if err := b.client.CallTimeout("permission.request",
			map[string]any{"threadId": b.thread, "toolName": toolName, "input": input},
			&res, 10*time.Minute); err != nil {
			return `{"behavior":"deny","message":"AgentKate could not reach the approval UI"}`, nil
		}

		var decision []byte
		if res.Allow {
			// AskUserQuestion answers come back as updatedInput; ordinary
			// tools keep their original input.
			finalInput := res.UpdatedInput
			if len(finalInput) == 0 {
				finalInput = input
			}
			decision, _ = json.Marshal(map[string]any{
				"behavior": "allow", "updatedInput": finalInput,
			})
		} else {
			decision, _ = json.Marshal(map[string]any{
				"behavior": "deny", "message": "Denied by the user in AgentKate",
			})
		}
		return string(decision), nil

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

// toolDefs is the Cooperation MCP tool catalogue advertised via tools/list.
func toolDefs() []map[string]any {
	noArgs := map[string]any{"type": "object", "properties": map[string]any{}}
	return []map[string]any{
		{
			"name": "list_open_files",
			"description": "List every file currently open in the AgentKate arena and " +
				"who has it open (the human, or another agent thread). Check this " +
				"before editing so you do not clobber a collaborator's work.",
			"inputSchema": noArgs,
		},
		{
			"name":        "whoami",
			"description": "Return this agent's own thread id and workspace path.",
			"inputSchema": noArgs,
		},
		{
			"name": "post_note",
			"description": "Post a short note to the shared cooperation board so the " +
				"human and other agents can see what you are doing or hand off context.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "The note to post."},
				},
				"required": []string{"text"},
			},
		},
		{
			"name":        "read_notes",
			"description": "Read every note on the shared cooperation board, oldest first.",
			"inputSchema": noArgs,
		},
		{
			"name": "get_presence",
			"description": "Show what every collaborator is focused on, which files are " +
				"claimed, and which files are open. Check this before editing.",
			"inputSchema": noArgs,
		},
		{
			"name": "claim_file",
			"description": "Place an advisory claim on a file so other agents know you " +
				"are working on it. Call release_file when you are done.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path to claim."},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "release_file",
			"description": "Release your advisory claim on a file.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "File path to release."},
				},
				"required": []string{"path"},
			},
		},
		{
			"name": "request_review",
			"description": "Flag your work for the human to review in AgentKate, with a " +
				"short summary of what you changed.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{"type": "string", "description": "Short summary of the changes."},
				},
				"required": []string{"summary"},
			},
		},
		{
			"name": "list_agents",
			"description": "List every AgentKate agent thread on record — its id, title, " +
				"status (running/dormant), worktree branch and path, and how long it has " +
				"been idle. Defaults to the current workspace; pass all_workspaces=true " +
				"to include every project. Use this to find stale agents to clean up.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"all_workspaces": map[string]any{
						"type":        "boolean",
						"description": "Include threads from every workspace, not just this one.",
					},
				},
			},
		},
		{
			"name": "discard_agent",
			"description": "Permanently delete an agent thread: stop its process, remove " +
				"its git worktree, and delete its branch. DESTRUCTIVE — any uncommitted " +
				"work in that worktree is lost. Refuses to discard the calling agent or " +
				"a running thread (pass force=true to override the running check). Use " +
				"list_agents first to pick the right thread_id.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"thread_id": map[string]any{
						"type":        "string",
						"description": "Agent thread id to discard.",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Discard even if the thread is currently running.",
					},
				},
				"required": []string{"thread_id"},
			},
		},
		{
			"name": "request_permission",
			"description": "Internal AgentKate permission gate — Claude Code calls this " +
				"automatically to have the human approve a gated tool use. Do not " +
				"call it directly.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool_name": map[string]any{"type": "string"},
					"input":     map[string]any{"type": "object"},
				},
			},
		},
	}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
}

func (b *mcpBridge) reply(f *ipc.Frame, result any) {
	if f.ID == nil {
		return
	}
	rb, err := json.Marshal(result)
	if err != nil {
		b.replyError(f, ipc.CodeInternalError, err.Error())
		return
	}
	b.write(ipc.Frame{JSONRPC: "2.0", ID: f.ID, Result: rb})
}

func (b *mcpBridge) replyError(f *ipc.Frame, code int, msg string) {
	if f.ID == nil {
		return
	}
	b.write(ipc.Frame{JSONRPC: "2.0", ID: f.ID, Error: ipc.Errorf(code, msg)})
}

func (b *mcpBridge) write(f ipc.Frame) {
	line, err := json.Marshal(f)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.out.Write(append(line, '\n'))
	_ = b.out.Flush()
}
