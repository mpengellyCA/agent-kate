package modes

import (
	"fmt"
	"strings"
)

// DefaultMasterPrompt is the opening message a controller receives when its
// ensemble does not define its own. It is the behaviour heart of Feature 3:
// the orchestration tools exist for every agent, but nothing tells an agent to
// USE them, or with which arguments.
//
// Written harness-neutrally on purpose — a Kimi controller and a Claude
// controller get byte-identical text. The tool names are the bridge's real
// ones; the concrete backend/model strings for each worker role arrive through
// {{worker_roster}}, so the controller never has to guess a model id.
//
// Placeholders: {{ensemble_name}}, {{workspace}}, {{worker_roster}}.
const DefaultMasterPrompt = `You are the CONTROLLER of the "{{ensemble_name}}" ensemble in Agent Kate, a multi-agent arena. The human is watching this conversation, and every agent you launch appears in their roster as a real agent with its own transcript and its own git worktree.

Workspace: {{workspace}}

## Your crew

You launch workers yourself, on demand — none exist yet. Each row gives the exact arguments to launch that role with:

{{worker_roster}}

## How to orchestrate

Use these tools (they are ordinary tools available to you right now):

- mcp__cooperation__launch_agent — start a worker. Pass backend and model exactly as the roster gives them, plus prompt (the worker's ENTIRE briefing: it cannot see this conversation), and optionally title, isolation, permission_mode, effort. Returns the worker's thread_id.
- mcp__cooperation__send_agent — send a follow-up message to a worker you launched (thread_id, message).
- mcp__cooperation__wait_agent — block until a worker is idle and read its last reply (thread_id, timeout_sec; default 300s, returns status "timeout" if it is still working — call again to keep waiting).
- mcp__cooperation__list_agents — see every agent, its role, and who launched it.
- mcp__cooperation__close_agent — retire a worker when its job is done (archives it; the human can restore it).
- mcp__cooperation__post_note, read_notes — the shared notes board the human reads.
- mcp__cooperation__claim_file, release_file — advisory file claims.

### Waiting vs. fire-and-forget

- Launch with wait: true (or follow up with wait_agent) when you need the worker's answer before your next decision — a scout's findings, a reviewer's verdict.
- Launch WITHOUT waiting when you want several workers running at once. Launch them all first, then wait_agent on each in turn. Waiting on a worker you have not launched yet serialises the crew for no reason.
- A worker blocked on a human permission prompt counts as working, so a timeout does not mean it is stuck. Re-wait, or check the roster.

### Keeping the human informed

- post_note a short update when you launch a worker, when one reports back, and when the overall job is done. The notes board is what the human reads to follow the orchestration without opening every transcript.
- Never claim a worker finished something you have not verified from its reply.

### Working on files

- claim_file before you edit a file, and release_file when you are done, so a worker does not overwrite you (and you do not overwrite it).
- Prefer giving a worker a file to own over editing the same file from two agents.

### Permissions — you cannot give away authority you do not have

- Workers run under the permission_mode you launch them with. A worker in the default mode stops and asks the HUMAN for permission on gated tools — good for anything destructive, but it stalls until the human answers.
- Launching a worker MORE PERMISSIVE than your own mode, or with isolation="workspace" (the human's main checkout, not a throwaway worktree), stops the launch and asks the human first. Expect that pause, and only ask for it when the task genuinely needs it — say why in the worker's prompt, because the human sees its first line.
- Leave permission_mode unset unless you have a reason. A worker that inherits the normal default launches immediately and still asks the human about anything destructive.
- There is a cap on how many workers can run at once, per controller and per crew. Retire finished workers with close_agent rather than launching around the limit.

## Now

Read the task below (or ask the human for one if there is none), decide which roles you actually need — launching nobody is a valid answer for a small task — brief them precisely, and report back here.`

// builtIns is the shipped ensemble catalogue. Every model string is real
// vocabulary, probed against the CLIs (see plan 16 P4): claude 2.1.220's
// aliases from "claude -p /model" (sonnet, opus, haiku, fable, …) and kimi
// 0.30.0's ids from its ACP config-option enumeration (kimi-code/k3,
// kimi-code/kimi-for-coding, kimi-code/kimi-for-coding-highspeed, …).
//
// None of these is privileged: the user can edit or delete any of them, and a
// deletion survives an upgrade (see Store.Delete).
func builtIns() []Mode {
	return []Mode{
		{
			Name:        "Fable Controls Opus",
			Description: "A fast Fable controller directing Opus for the heavy lifting, with Sonnet scouting.",
			Controller:  Participant{Backend: "claude", Model: "fable"},
			Workers: []Participant{
				{Role: "coder", Backend: "claude", Model: "opus", Isolation: "auto",
					Notes: "Implements changes. Give it one well-scoped task at a time."},
				{Role: "scout", Backend: "claude", Model: "sonnet", Isolation: "workspace",
					Notes: "Reads and reports — searches the codebase, answers questions. Runs in the workspace, so do not ask it to edit."},
			},
		},
		{
			Name:        "Fable Controls Kimi K3",
			Description: "A Claude controller driving a Kimi K3 worker — the cross-engine ensemble.",
			Controller:  Participant{Backend: "claude", Model: "fable"},
			Workers: []Participant{
				{Role: "coder", Backend: "kimi", Model: "kimi-code/k3", Isolation: "auto",
					Notes: "Implements changes on Kimi K3. It cannot see your conversation — brief it fully."},
			},
		},
		{
			Name:        "Kimi K3 Controls K2.7 Code",
			Description: "An all-Kimi ensemble: K3 orchestrating K2.7 Coding workers.",
			Controller:  Participant{Backend: "kimi", Model: "kimi-code/k3"},
			Workers: []Participant{
				{Role: "coder", Backend: "kimi", Model: "kimi-code/kimi-for-coding", Isolation: "auto",
					Notes: "Implements changes."},
				{Role: "fast-coder", Backend: "kimi", Model: "kimi-code/kimi-for-coding-highspeed",
					Isolation: "auto",
					Notes:     "The high-speed variant — mechanical edits, wide sweeps, anything latency-bound."},
			},
		},
		{
			Name:        "Opus Controls Kimi Coders",
			Description: "Opus planning and reviewing, two Kimi coders doing the work in parallel.",
			Controller:  Participant{Backend: "claude", Model: "opus"},
			Workers: []Participant{
				{Role: "coder-a", Backend: "kimi", Model: "kimi-code/kimi-for-coding", Isolation: "auto",
					Notes: "First implementation worker. Give it files nobody else is editing."},
				{Role: "coder-b", Backend: "kimi", Model: "kimi-code/kimi-for-coding", Isolation: "auto",
					Notes: "Second implementation worker, for work that can run in parallel with coder-a."},
			},
		},
	}
}

// BuiltIns returns a copy of the shipped catalogue, so a caller mutating an
// entry cannot rewrite the defaults for everyone else.
func BuiltIns() []Mode {
	return builtIns()
}

// Render turns an ensemble into the controller's opening message.
// permissiveModes maps backend id -> the permission mode that runs unattended,
// as the CALLER resolved it from the harness registry (a backend the caller
// could not resolve is simply absent, and its roles get no permission hint —
// this package never guesses a harness's vocabulary).
func Render(m Mode, workspace string, permissiveModes map[string]string) string {
	tmpl := strings.TrimSpace(m.MasterPrompt)
	if tmpl == "" {
		tmpl = DefaultMasterPrompt
	}
	r := strings.NewReplacer(
		"{{ensemble_name}}", m.Name,
		"{{workspace}}", workspace,
		"{{worker_roster}}", WorkerRoster(m, permissiveModes),
	)
	return r.Replace(tmpl)
}

// WorkerRoster renders the worker table the controller launches from: one line
// per role with the exact launch_agent arguments. Kept plain text (not JSON or
// a markdown table) because it is read by a model, and a line the controller
// can copy is worth more than a shape it has to parse.
func WorkerRoster(m Mode, permissiveModes map[string]string) string {
	if len(m.Workers) == 0 {
		return "(no worker roles defined — launch_agent still works; choose backend and model yourself, " +
			"or ask the human which engines this arena has.)"
	}
	var b strings.Builder
	for _, w := range m.Workers {
		backend := w.Backend
		if backend == "" {
			backend = m.Controller.Backend
		}
		fmt.Fprintf(&b, "- %s — launch_agent with backend=%q", w.Role, backend)
		if w.Model != "" {
			fmt.Fprintf(&b, ", model=%q", w.Model)
		}
		if w.Isolation != "" {
			fmt.Fprintf(&b, ", isolation=%q", w.Isolation)
		}
		if w.PermissionMode != "" {
			fmt.Fprintf(&b, ", permission_mode=%q", w.PermissionMode)
		}
		if w.Effort != "" {
			fmt.Fprintf(&b, ", effort=%q", w.Effort)
		}
		b.WriteString("\n")
		if w.Notes != "" {
			fmt.Fprintf(&b, "  %s\n", w.Notes)
		}
		// The engine's unattended mode is NAMED, never recommended: launching a
		// worker above your own permission mode stops for the human's approval
		// (core/cmd/akcore/authority.go), so a roster that sold it as the way to
		// get autonomous work would be briefing controllers to trip a gate.
		if pm := permissiveModes[backend]; w.PermissionMode == "" && pm != "" {
			fmt.Fprintf(&b, "  This engine's never-ask mode is %q; asking for it "+
				"needs the human's approval, so only request it when the task "+
				"truly cannot pause.\n", pm)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
