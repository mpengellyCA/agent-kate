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
	"agentkate/internal/safe"
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
	cowork    bool // serve the opt-in Cowork desktop tool set instead of Cooperation
	// noPermissionTool hides the request_permission tool for harnesses whose
	// permission prompts don't flow over MCP (kimi asks via ACP instead).
	noPermissionTool bool
	log              *slog.Logger

	mu  sync.Mutex // guards out across concurrent handlers
	out *bufio.Writer
}

// runMCPBridge is the entry point for `akcore mcp ...`.
func runMCPBridge(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	socket := fs.String("socket", "", "core IPC socket path")
	thread := fs.String("thread", "", "agent thread id")
	workspace := fs.String("workspace", "", "workspace path")
	coworkMode := fs.Bool("cowork", false, "serve the opt-in KDE Cowork desktop tools instead of Cooperation")
	noPermTool := fs.Bool("no-permission-tool", false,
		"hide the request_permission tool (for harnesses whose permissions don't flow over MCP)")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := ipc.Dial(*socket)
	if err != nil {
		log.Error("mcp bridge cannot reach core", "socket", *socket, "err", err)
		os.Exit(1)
	}
	defer client.Close()

	b := &mcpBridge{
		client:           client,
		thread:           *thread,
		workspace:        *workspace,
		cowork:           *coworkMode,
		noPermissionTool: *noPermTool,
		log:              log,
		out:              bufio.NewWriter(os.Stdout),
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
		frame := f
		safe.Go("mcp.handle", func() { b.handle(&frame) })
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
			"serverInfo":      map[string]any{"name": b.serverName(), "version": version},
		})
	case "notifications/initialized":
		// notification — no response
	case "ping":
		b.reply(f, map[string]any{})
	case "tools/list":
		b.reply(f, map[string]any{"tools": b.advertisedTools()})
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
	// desktop_screenshot returns an MCP image content block, not text.
	if b.cowork && p.Name == "desktop_screenshot" {
		content, err := b.runScreenshot(p.Arguments)
		if err != nil {
			b.reply(f, toolResult(err.Error(), true))
			return
		}
		b.reply(f, map[string]any{"content": content, "isError": false})
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
	if b.cowork {
		return b.runCoworkTool(name, args)
	}
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
		return "Review requested — the human has been notified in Agent Kate.", nil

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
				Parent   string    `json:"parentThreadId"`
				Role     string    `json:"role"`
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
			if t.Role != "" {
				linkage := t.Role
				if t.Parent != "" {
					linkage += " of " + t.Parent
				}
				fmt.Fprintf(&sb, "    role:   %s\n", linkage)
			}
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

	case "launch_agent":
		var a struct {
			Backend        string `json:"backend"`
			Model          string `json:"model"`
			Prompt         string `json:"prompt"`
			Title          string `json:"title"`
			Isolation      string `json:"isolation"`
			PermissionMode string `json:"permission_mode"`
			Effort         string `json:"effort"`
			Wait           bool   `json:"wait"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.Prompt) == "" {
			return "", fmt.Errorf("launch_agent requires a non-empty 'prompt'")
		}
		var res struct {
			ThreadID  string            `json:"threadId"`
			SessionID string            `json:"sessionId"`
			Backend   string            `json:"backend"`
			Isolated  bool              `json:"isolated"`
			Branch    string            `json:"branch"`
			Applied   map[string]string `json:"applied"`
			Unapplied []struct {
				Option    string `json:"option"`
				Requested string `json:"requested"`
				Applied   string `json:"applied"`
			} `json:"unapplied"`
		}
		// Synchronous launch: worktree creation plus a CLI handshake can take
		// a while (kimi's ACP handshake in particular), so give it room.
		if err := b.client.CallTimeout("agent.launchWorker", map[string]any{
			"parentThreadId": b.thread,
			"backend":        a.Backend,
			"model":          a.Model,
			"prompt":         a.Prompt,
			"title":          a.Title,
			"isolation":      a.Isolation,
			"permissionMode": a.PermissionMode,
			"effort":         a.Effort,
		}, &res, 3*time.Minute); err != nil {
			return "", err
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Launched worker %s on the %s backend.\n", res.ThreadID, res.Backend)
		if res.Isolated {
			fmt.Fprintf(&sb, "It runs in its own isolated worktree on branch %s.\n", res.Branch)
		} else {
			sb.WriteString("It runs directly in the workspace (no isolated worktree).\n")
		}
		if m := res.Applied["model"]; m != "" {
			fmt.Fprintf(&sb, "Model: %s\n", m)
		}
		for _, u := range res.Unapplied {
			fmt.Fprintf(&sb, "NOT APPLIED: %s %q was not accepted (running with %q instead).\n",
				u.Option, u.Requested, u.Applied)
		}
		if a.Wait {
			waitText, err := b.waitAgent(res.ThreadID, 0)
			if err != nil {
				return "", fmt.Errorf("worker %s launched, but waiting on it failed: %w",
					res.ThreadID, err)
			}
			sb.WriteString(waitText)
		} else {
			sb.WriteString("It is working now; collect its result with wait_agent.")
		}
		return strings.TrimRight(sb.String(), "\n"), nil

	case "send_agent":
		var a struct {
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Wait     bool   `json:"wait"`
		}
		_ = json.Unmarshal(args, &a)
		a.ThreadID = strings.TrimSpace(a.ThreadID)
		if a.ThreadID == "" || strings.TrimSpace(a.Message) == "" {
			return "", fmt.Errorf("send_agent requires 'thread_id' and a non-empty 'message'")
		}
		// A target outside this agent's own subtree needs one human approval,
		// which can take minutes — the long timeout mirrors request_permission.
		if err := b.client.CallTimeout("agent.send", map[string]any{
			"threadId":     a.ThreadID,
			"text":         a.Message,
			"fromThreadId": b.thread,
		}, nil, 10*time.Minute); err != nil {
			return "", err
		}
		if a.Wait {
			return b.waitAgent(a.ThreadID, 0)
		}
		return fmt.Sprintf("Message delivered to %s; collect its reply with wait_agent.",
			a.ThreadID), nil

	case "wait_agent":
		var a struct {
			ThreadID   string `json:"thread_id"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		_ = json.Unmarshal(args, &a)
		a.ThreadID = strings.TrimSpace(a.ThreadID)
		if a.ThreadID == "" {
			return "", fmt.Errorf("wait_agent requires 'thread_id'")
		}
		return b.waitAgent(a.ThreadID, a.TimeoutSec)

	case "close_agent":
		var a struct {
			ThreadID string `json:"thread_id"`
		}
		_ = json.Unmarshal(args, &a)
		a.ThreadID = strings.TrimSpace(a.ThreadID)
		if a.ThreadID == "" {
			return "", fmt.Errorf("close_agent requires 'thread_id'")
		}
		if a.ThreadID == b.thread {
			return "", fmt.Errorf("an agent cannot close itself — ask the human to close this thread")
		}
		// Stopping a busy thread waits for its turn to wind down, and a
		// cross-subtree target may need a human approval first.
		if err := b.client.CallTimeout("agent.stopClose", map[string]any{
			"threadId":     a.ThreadID,
			"fromThreadId": b.thread,
		}, nil, 10*time.Minute); err != nil {
			return "", err
		}
		return fmt.Sprintf("Closed agent %s: stopped politely and archived (reversible — "+
			"the human can restore it from the Sessions browser). Its worktree is untouched.",
			a.ThreadID), nil

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
			return `{"behavior":"deny","message":"Agent Kate could not reach the approval UI"}`, nil
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
				"behavior": "deny", "message": "Denied by the user in Agent Kate",
			})
		}
		return string(decision), nil

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

// waitAgent blocks on the core's agent.wait until the target thread goes idle
// (or the timeout fires) and formats the outcome for the calling agent.
// timeoutSec <= 0 uses the core's default (5 minutes). The IPC timeout is the
// wait timeout plus slack, so the blocking RPC itself never races the wait.
func (b *mcpBridge) waitAgent(threadID string, timeoutSec int) (string, error) {
	effective := timeoutSec
	if effective <= 0 {
		effective = 300
	}
	var res struct {
		Status   string `json:"status"`
		LastText string `json:"lastText"`
	}
	if err := b.client.CallTimeout("agent.wait",
		map[string]any{"threadId": threadID, "timeoutSec": timeoutSec},
		&res, time.Duration(effective)*time.Second+30*time.Second); err != nil {
		return "", err
	}
	var sb strings.Builder
	switch res.Status {
	case "idle":
		fmt.Fprintf(&sb, "Agent %s is idle (turn complete; it accepts follow-ups via send_agent).\n", threadID)
	case "exited":
		fmt.Fprintf(&sb, "Agent %s has finished and its process exited (send_agent would resume it — ask the human if unsure).\n", threadID)
	case "timeout":
		fmt.Fprintf(&sb, "Timed out after %ds: agent %s is STILL WORKING (it may also be blocked on a human permission). Call wait_agent again to keep waiting.\n", effective, threadID)
	default:
		fmt.Fprintf(&sb, "Agent %s: %s.\n", threadID, res.Status)
	}
	if res.LastText != "" {
		fmt.Fprintf(&sb, "Its last reply:\n%s", res.LastText)
	} else {
		sb.WriteString("It has produced no reply text yet.")
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// toolDefs is the Cooperation MCP tool catalogue advertised via tools/list.
func toolDefs() []map[string]any {
	noArgs := map[string]any{"type": "object", "properties": map[string]any{}}
	return []map[string]any{
		{
			"name": "list_open_files",
			"description": "List every file currently open in the Agent Kate arena and " +
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
			"description": "Flag your work for the human to review in Agent Kate, with a " +
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
			"description": "List every Agent Kate agent thread on record — its id, title, " +
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
			"name": "launch_agent",
			"description": "Launch a WORKER: a real Agent Kate agent thread parented to you. " +
				"It gets its own worktree (per 'isolation'), appears in the human's roster " +
				"with a live transcript, and the human can inspect or take it over at any " +
				"time. 'backend' is an engine id from this arena's registry — \"claude\" " +
				"(Claude Code) or \"kimi\" (Kimi Code); omit it to use your own engine. " +
				"'model' must belong to that backend's vocabulary (see agent list / ask the " +
				"human if unsure); options the backend rejects are reported back as NOT " +
				"APPLIED — never silently emulated. Workers needing tool approval prompt " +
				"the HUMAN, which can stall an unattended worker: pass permission_mode " +
				"\"acceptEdits\" (or \"auto\" on claude) for autonomous work. With " +
				"wait=true this call blocks until the worker finishes its first turn and " +
				"returns its reply.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string",
						"description": "The worker's opening instruction (its whole briefing)."},
					"backend": map[string]any{"type": "string",
						"description": "Engine id: \"claude\" or \"kimi\". Empty = same engine as you."},
					"model": map[string]any{"type": "string",
						"description": "Model id in the target backend's vocabulary. Empty = its default."},
					"title": map[string]any{"type": "string",
						"description": "Short roster title. Empty = derived from the prompt."},
					"isolation": map[string]any{"type": "string",
						"description": "\"auto\" (default: isolated worktree when the repo has commits), \"isolated\", or \"workspace\"."},
					"permission_mode": map[string]any{"type": "string",
						"description": "The backend's permission mode. Empty = its default (gated tools then prompt the human)."},
					"effort": map[string]any{"type": "string",
						"description": "Reasoning effort / thinking level in the backend's vocabulary. Empty = default."},
					"wait": map[string]any{"type": "boolean",
						"description": "Block until the worker's first turn completes and include its reply."},
				},
				"required": []string{"prompt"},
			},
		},
		{
			"name": "send_agent",
			"description": "Send a follow-up message to a running agent thread (use " +
				"list_agents for ids). Targets outside your own worker subtree need a " +
				"one-time human approval. Set wait=true to block until the reply lands " +
				"and get its text back.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"thread_id": map[string]any{"type": "string",
						"description": "Target agent thread id."},
					"message": map[string]any{"type": "string",
						"description": "The message to deliver."},
					"wait": map[string]any{"type": "boolean",
						"description": "Block until the target finishes the turn and include its reply."},
				},
				"required": []string{"thread_id", "message"},
			},
		},
		{
			"name": "wait_agent",
			"description": "Block until an agent thread is idle (its current turn finished " +
				"or its process ended) and return its status plus its last reply text. " +
				"Times out after timeout_sec (default 300) with status \"timeout\" if the " +
				"agent is still working — call again to keep waiting. Note: a worker " +
				"blocked on a human permission counts as working.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"thread_id": map[string]any{"type": "string",
						"description": "Agent thread id to wait on."},
					"timeout_sec": map[string]any{"type": "integer",
						"description": "Give up after this many seconds (default 300, max 3600)."},
				},
				"required": []string{"thread_id"},
			},
		},
		{
			"name": "close_agent",
			"description": "Politely retire an agent thread: stop its process (letting a " +
				"running turn wind down) and ARCHIVE it out of the live roster. Reversible " +
				"— the record and worktree survive and the human can restore it; use " +
				"discard_agent only when the worktree should be destroyed. Refuses to " +
				"close yourself; targets outside your own worker subtree need a one-time " +
				"human approval.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"thread_id": map[string]any{"type": "string",
						"description": "Agent thread id to close."},
				},
				"required": []string{"thread_id"},
			},
		},
		{
			"name": "request_permission",
			"description": "Internal Agent Kate permission gate — Claude Code calls this " +
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
