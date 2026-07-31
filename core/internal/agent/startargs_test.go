package agent

import (
	"strings"
	"testing"
)

// flagValue returns the argument following flag, and whether the flag is
// present at all.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// TestBuildStartArgsPersonaFlags pins the persona channel's argv (plan 16 P3):
// the persona rides on --append-system-prompt (which ADDS to Claude Code's own
// system prompt; --system-prompt would replace it), and the subagent payload
// on --agents, verbatim as the adapter rendered it.
func TestBuildStartArgsPersonaFlags(t *testing.T) {
	payload := `{"reviewer":{"description":"Reviews code","prompt":"You review.","tools":["Read"],"model":"sonnet"}}`
	args := buildStartArgs(StartOptions{
		WorkDir:      "/ws",
		SystemPrompt: "You are the arena's scout.",
		AgentsJSON:   payload,
	})

	if v, ok := flagValue(args, "--append-system-prompt"); !ok || v != "You are the arena's scout." {
		t.Errorf("--append-system-prompt = %q (present=%v)", v, ok)
	}
	if v, ok := flagValue(args, "--agents"); !ok || v != payload {
		t.Errorf("--agents = %q (present=%v)", v, ok)
	}
	if _, ok := flagValue(args, "--system-prompt"); ok {
		t.Error("--system-prompt must never be used: it REPLACES Claude Code's own prompt")
	}
	// The payload is one argv element, not shell-quoted or split.
	joined := strings.Join(args, "\x00")
	if strings.Count(joined, payload) != 1 {
		t.Errorf("--agents payload not passed as a single argument: %q", args)
	}
}

// TestBuildStartArgsPersonaOmitted keeps the flags out of the argv when no
// persona was requested — an empty --append-system-prompt would still count as
// a custom prompt to the CLI.
func TestBuildStartArgsPersonaOmitted(t *testing.T) {
	args := buildStartArgs(StartOptions{WorkDir: "/ws", Model: "haiku"})
	for _, flag := range []string{"--append-system-prompt", "--agents"} {
		if _, ok := flagValue(args, flag); ok {
			t.Errorf("%s present with nothing requested: %q", flag, args)
		}
	}
	// Blank counts as absent, the same rule the adapter and the applied-truth
	// report use — an empty --append-system-prompt still reads as a custom
	// prompt to the CLI.
	blank := buildStartArgs(StartOptions{WorkDir: "/ws", SystemPrompt: "  \n\t "})
	if _, ok := flagValue(blank, "--append-system-prompt"); ok {
		t.Errorf("blank system prompt produced a flag: %q", blank)
	}
	// The unrelated flags still land, so the split-out builder stayed faithful.
	if v, _ := flagValue(args, "--model"); v != "haiku" {
		t.Errorf("--model = %q", v)
	}
	if v, _ := flagValue(args, "--permission-mode"); v != "acceptEdits" {
		t.Errorf("default --permission-mode = %q", v)
	}
	if v, _ := flagValue(args, "--allowedTools"); v != "mcp__cooperation" {
		t.Errorf("--allowedTools = %q", v)
	}
}

// TestBuildStartArgsSessionAndCowork covers the rest of the extracted builder
// so the refactor cannot quietly change how a resume, a fork or a Cowork
// thread is spawned.
func TestBuildStartArgsSessionAndCowork(t *testing.T) {
	fork := buildStartArgs(StartOptions{
		SessionID: "sess-1", Resume: true, ForkSession: true, CoworkEnabled: true,
		MCPConfig: "/tmp/mcp.json",
	})
	if v, _ := flagValue(fork, "--resume"); v != "sess-1" {
		t.Errorf("--resume = %q", v)
	}
	if _, ok := flagValue(fork, "--fork-session"); !ok {
		t.Error("--fork-session missing on a fork")
	}
	if v, _ := flagValue(fork, "--allowedTools"); v != "mcp__cooperation,mcp__cowork" {
		t.Errorf("cowork --allowedTools = %q", v)
	}
	if v, _ := flagValue(fork, "--permission-prompt-tool"); v != "mcp__cooperation__request_permission" {
		t.Errorf("--permission-prompt-tool = %q", v)
	}

	fresh := buildStartArgs(StartOptions{SessionID: "sess-2"})
	if v, _ := flagValue(fresh, "--session-id"); v != "sess-2" {
		t.Errorf("--session-id = %q", v)
	}
	if _, ok := flagValue(fresh, "--resume"); ok {
		t.Error("a fresh thread must not --resume")
	}
}
