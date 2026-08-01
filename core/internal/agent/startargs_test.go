package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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
	args := buildStartArgs(nil, StartOptions{
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
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws", Model: "haiku"})
	for _, flag := range []string{"--append-system-prompt", "--agents"} {
		if _, ok := flagValue(args, flag); ok {
			t.Errorf("%s present with nothing requested: %q", flag, args)
		}
	}
	// Blank counts as absent, the same rule the adapter and the applied-truth
	// report use — an empty --append-system-prompt still reads as a custom
	// prompt to the CLI.
	blank := buildStartArgs(nil, StartOptions{WorkDir: "/ws", SystemPrompt: "  \n\t "})
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
	// Both bridges are allowed on every thread: the Cowork server is wired in
	// unconditionally so it can be switched on mid-session, and advertises no
	// tools until the thread opts in.
	if v, _ := flagValue(args, "--allowedTools"); v != "mcp__cooperation,mcp__cowork" {
		t.Errorf("--allowedTools = %q", v)
	}
}

// TestBuildStartArgsSessionAndCowork covers the rest of the extracted builder
// so the refactor cannot quietly change how a resume, a fork or a Cowork
// thread is spawned.
func TestBuildStartArgsSessionAndCowork(t *testing.T) {
	fork := buildStartArgs(nil, StartOptions{
		SessionID: "sess-1", Resume: true, ForkSession: true,
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

	fresh := buildStartArgs(nil, StartOptions{SessionID: "sess-2"})
	if v, _ := flagValue(fresh, "--session-id"); v != "sess-2" {
		t.Errorf("--session-id = %q", v)
	}
	if _, ok := flagValue(fresh, "--resume"); ok {
		t.Error("a fresh thread must not --resume")
	}
}

// TestBuildStartArgsSweepFlags pins the plan 16 P6 launch-option sweep, all
// three verified present on claude 2.1.220. --fallback-model takes ONE
// comma-separated value; --disallowedTools and --add-dir are variadic
// (`<tools...>`, `<directories...>`), so each value is passed as its own flag
// occurrence — a variadic flag greedily eats whatever argv follows it, which a
// live probe confirmed by watching `--add-dir /tmp "prompt"` swallow the
// prompt.
func TestBuildStartArgsSweepFlags(t *testing.T) {
	args := buildStartArgs(nil, StartOptions{
		WorkDir:         "/ws",
		FallbackModels:  []string{"sonnet", "haiku"},
		DisallowedTools: []string{"Bash(git push:*)", "WebFetch", "  "},
		AddDirs:         []string{"/ref/docs", "/ref/schemas", ""},
	})

	if v, ok := flagValue(args, "--fallback-model"); !ok || v != "sonnet,haiku" {
		t.Errorf("--fallback-model = %q (present=%v), want the comma-joined list", v, ok)
	}
	// Each list value is its own occurrence, and blanks are dropped rather than
	// passed as an empty argument the CLI would have to interpret.
	var tools, dirs []string
	for i, a := range args {
		if i+1 >= len(args) {
			continue
		}
		switch a {
		case "--disallowedTools":
			tools = append(tools, args[i+1])
		case "--add-dir":
			dirs = append(dirs, args[i+1])
		}
	}
	if len(tools) != 2 || tools[0] != "Bash(git push:*)" || tools[1] != "WebFetch" {
		t.Errorf("--disallowedTools occurrences = %q", tools)
	}
	if len(dirs) != 2 || dirs[0] != "/ref/docs" || dirs[1] != "/ref/schemas" {
		t.Errorf("--add-dir occurrences = %q", dirs)
	}
}

// TestBuildStartArgsSweepOmitted: nothing requested, nothing in the argv.
func TestBuildStartArgsSweepOmitted(t *testing.T) {
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws"})
	for _, flag := range []string{"--fallback-model", "--disallowedTools", "--add-dir"} {
		if _, ok := flagValue(args, flag); ok {
			t.Errorf("%s present without a request", flag)
		}
	}
}

// TestBuildStartArgsStreamChannel: the two stream-channel flags are always in
// the argv for an UNPROBED CLI (nil vocabulary) — no StartOptions field turns
// them off, because the UI's provisional-row rendering and its live subagent
// readout both depend on the events they unlock. Only the binary's own
// vocabulary can remove them (see the tests below).
func TestBuildStartArgsStreamChannel(t *testing.T) {
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws"})
	for _, flag := range []string{"--include-partial-messages", "--forward-subagent-text"} {
		if _, ok := flagValue(args, flag); !ok {
			t.Errorf("%s missing from the argv", flag)
		}
	}
}

// TestBuildStartArgsStreamChannelGated: a CLI whose probed vocabulary lacks the
// stream flags launches WITHOUT them, instead of dying on an unknown option.
// The rest of the fixed prefix is untouched — degrading the stream must not
// degrade the protocol.
func TestBuildStartArgsStreamChannelGated(t *testing.T) {
	old := cliFlags{"--print": true, "--output-format": true, "--input-format": true,
		"--verbose": true, "--permission-mode": true, "--allowedTools": true}
	args := buildStartArgs(old, StartOptions{WorkDir: "/ws"})
	for _, flag := range []string{"--include-partial-messages", "--forward-subagent-text"} {
		if _, ok := flagValue(args, flag); ok {
			t.Errorf("%s passed to a CLI that does not advertise it: %v", flag, args)
		}
	}
	if v, ok := flagValue(args, "--output-format"); !ok || v != "stream-json" {
		t.Errorf("--output-format = %q, %v; the protocol flags must survive gating", v, ok)
	}
	if _, ok := flagValue(args, "--verbose"); !ok {
		t.Error("--verbose dropped")
	}
}

// TestBuildStartArgsStreamChannelSupported: a probe that DOES list them keeps
// them. This is the case that must not regress — every current claude is here.
func TestBuildStartArgsStreamChannelSupported(t *testing.T) {
	modern := cliFlags{
		"--include-partial-messages": true,
		"--forward-subagent-text":    true,
	}
	args := buildStartArgs(modern, StartOptions{WorkDir: "/ws"})
	for _, flag := range []string{"--include-partial-messages", "--forward-subagent-text"} {
		if _, ok := flagValue(args, flag); !ok {
			t.Errorf("%s dropped for a CLI that advertises it: %v", flag, args)
		}
	}
}

// TestParseCLIFlags covers the help parse: the shapes flags actually appear in
// (bare, with a value placeholder, with '='), and the empty answer that means
// "learned nothing" — which supports() has to read as permissive.
func TestParseCLIFlags(t *testing.T) {
	help := `Usage: claude [options] [prompt]

Options:
  -p, --print                     Print response and exit
      --output-format <format>    Output format
      --effort=<level>            Reasoning effort
      --include-partial-messages  Include partial message chunks
  -h, --help                      display help for command
`
	flags := parseCLIFlags(help)
	for _, want := range []string{"--print", "--output-format", "--effort",
		"--include-partial-messages", "--help"} {
		if !flags[want] {
			t.Errorf("parseCLIFlags missed %s: %v", want, flags)
		}
	}
	if flags[flagForwardSubagentText] {
		t.Errorf("parseCLIFlags invented %s", flagForwardSubagentText)
	}
	if !flags.supports(flagIncludePartialMessages) {
		t.Error("supports() denied a flag the help lists")
	}
	if flags.supports(flagForwardSubagentText) {
		t.Error("supports() allowed a flag the help does not list")
	}

	// A probe that told us nothing must not be read as "nothing is supported":
	// a failed probe leaves every flag on.
	if empty := parseCLIFlags("claude: command not found"); empty != nil {
		t.Errorf("parseCLIFlags(garbage) = %v, want nil", empty)
	}
	if !cliFlags(nil).supports(flagForwardSubagentText) {
		t.Error("an unprobed vocabulary must treat every flag as supported")
	}
	empty := cliFlags{}
	if !empty.supports(flagIncludePartialMessages) {
		t.Error("an empty vocabulary must treat every flag as supported")
	}
}

// TestSupportedFlagsCaches: the probe runs ONCE per binary identity. The stub
// counts its own invocations, so a second Start cannot pay for a second
// subprocess.
func TestSupportedFlagsCaches(t *testing.T) {
	bin, countFile := stubClaudeHelp(t, "  --include-partial-messages  chunks\n")
	s := NewSupervisor(bin, testLogger(), nil)

	first := s.supportedFlags()
	if !first.supports(flagIncludePartialMessages) {
		t.Fatalf("probe missed the advertised flag: %v", first)
	}
	if first.supports(flagForwardSubagentText) {
		t.Errorf("probe invented %s", flagForwardSubagentText)
	}
	s.supportedFlags()
	s.supportedFlags()
	if n := probeCount(t, countFile); n != 1 {
		t.Errorf("help probed %d times, want 1 (cached)", n)
	}
}

// TestSupportedFlagsProbeFailurePermissive: a binary that cannot be run teaches
// us nothing, and "nothing" must not silently strip the stream flags.
func TestSupportedFlagsProbeFailurePermissive(t *testing.T) {
	s := NewSupervisor(filepath.Join(t.TempDir(), "no-such-claude"), testLogger(), nil)
	flags := s.supportedFlags()
	if flags != nil {
		t.Errorf("failed probe = %v, want nil", flags)
	}
	args := buildStartArgs(flags, StartOptions{WorkDir: "/ws"})
	if _, ok := flagValue(args, flagIncludePartialMessages); !ok {
		t.Error("a failed probe stripped the stream flags")
	}
}

// stubClaudeHelp writes a fake `claude` that appends a line to a counter file
// every time it is run and prints helpText for `--help`. Returns its path and
// the counter file.
func stubClaudeHelp(t *testing.T, helpText string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	count := filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho run >> " + count + "\ncat <<'EOF'\nUsage: claude [options]\n" +
		helpText + "EOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, count
}

func probeCount(t *testing.T, countFile string) int {
	t.Helper()
	b, err := os.ReadFile(countFile)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "run")
}

// TestClassifyStreamEventNeverDedups: two byte-identical text deltas in one
// batch are both real text. Flagging a stream_event as a dedup candidate would
// drop the second and silently corrupt the streamed row.
func TestClassifyStreamEventNeverDedups(t *testing.T) {
	raw := []byte(`{"type":"stream_event","event":{"type":"content_block_delta",` +
		`"index":0,"delta":{"type":"text_delta","text":" the"}}}`)
	boundary, dedup, subagent, typ := classifyEvent(raw)
	if typ != "stream_event" {
		t.Fatalf("type = %q", typ)
	}
	if subagent {
		t.Error("an event without parent_tool_use_id is not subagent output")
	}
	if dedup {
		t.Error("stream_event must never be a dedup candidate")
	}
	if boundary {
		t.Error("stream_event must not force a coalescer flush")
	}
}

// TestClassifySubagentEvents: --forward-subagent-text tags a child session's
// events with parent_tool_use_id. Those are display-only for the parent thread
// — its usage, its tool bookkeeping and its turn boundaries all belong to the
// events without the tag, and a subagent `result` must not close the parent's
// turn.
func TestClassifySubagentEvents(t *testing.T) {
	child := []byte(`{"type":"result","subtype":"success","parent_tool_use_id":"toolu_01",` +
		`"session_id":"sess-child"}`)
	_, _, subagent, typ := classifyEvent(child)
	if typ != "result" {
		t.Fatalf("type = %q", typ)
	}
	if !subagent {
		t.Error("parent_tool_use_id must mark the event as subagent output")
	}

	parent := []byte(`{"type":"result","subtype":"success","session_id":"sess-parent"}`)
	if _, _, subagent, _ := classifyEvent(parent); subagent {
		t.Error("the parent's own result was mistaken for subagent output")
	}
}

// TestBuildStartArgsControlSweepFlags covers the control-channel launch sweep:
// MCP isolation, the CLI-enforced spend ceiling, and the session label that
// makes an Agent Kate thread identifiable in `claude agents`.
func TestBuildStartArgsControlSweepFlags(t *testing.T) {
	args := buildStartArgs(nil, StartOptions{
		WorkDir:         "/ws",
		StrictMCPConfig: true,
		MaxBudgetUSD:    2.5,
		Title:           "Land the control-channel drift work",
	})
	if _, ok := flagValue(args, "--strict-mcp-config"); !ok {
		t.Error("--strict-mcp-config missing")
	}
	// Formatted without a trailing exponent or spurious decimals, since the
	// value goes on the command line verbatim.
	if v, ok := flagValue(args, "--max-budget-usd"); !ok || v != "2.5" {
		t.Errorf("--max-budget-usd = %q (present=%v)", v, ok)
	}
	if v, ok := flagValue(args, "--name"); !ok || v != "Land the control-channel drift work" {
		t.Errorf("--name = %q (present=%v)", v, ok)
	}
}

// TestBuildStartArgsControlSweepOmitted: an unset budget is not a budget of
// zero, an untitled thread passes no --name, and MCP isolation is opt-in.
func TestBuildStartArgsControlSweepOmitted(t *testing.T) {
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws", Title: "   "})
	for _, flag := range []string{"--strict-mcp-config", "--max-budget-usd", "--name"} {
		if _, ok := flagValue(args, flag); ok {
			t.Errorf("%s present without a request", flag)
		}
	}
}

// TestBuildStartArgsNameTruncated keeps an over-long title from becoming an
// unreadable label — titles are summarised prompts and can run long.
func TestBuildStartArgsNameTruncated(t *testing.T) {
	long := strings.Repeat("é", 200) // multi-byte, to exercise the rune boundary
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws", Title: long})
	v, ok := flagValue(args, "--name")
	if !ok {
		t.Fatal("--name missing")
	}
	if len(v) > maxNameBytes {
		t.Errorf("--name is %d bytes, over the %d cap", len(v), maxNameBytes)
	}
	if !utf8.ValidString(v) {
		t.Errorf("--name was cut mid-rune: %q", v)
	}
}

// TestBuildStartArgsNameNeverEmpty: a title whose first maxNameBytes are all
// whitespace truncates to nothing, and `--name ""` labels the session with an
// empty string in `claude agents`. No name is the right answer there.
func TestBuildStartArgsNameNeverEmpty(t *testing.T) {
	blank := strings.Repeat(" ", maxNameBytes+10)
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws", Title: blank})
	if v, ok := flagValue(args, "--name"); ok {
		t.Errorf("--name = %q; an all-whitespace title must pass no flag", v)
	}
	// Leading padding must not cost the title itself.
	args = buildStartArgs(nil, StartOptions{WorkDir: "/ws", Title: blank + "real title"})
	if v, _ := flagValue(args, "--name"); v != "real title" {
		t.Errorf("--name = %q, want \"real title\"", v)
	}
}

// TestBuildStartArgsPersonaFile pins the F23 fix: when Start has staged the
// persona in a private file, the TEXT must not appear anywhere in the argv —
// /proc/<pid>/cmdline is world-readable, and a persona carries the human's
// standing instructions and context.
func TestBuildStartArgsPersonaFile(t *testing.T) {
	const persona = "You are the arena's scout. The staging server password hint is in ~/notes."
	args := buildStartArgs(nil, StartOptions{
		WorkDir:          "/ws",
		SystemPrompt:     persona,
		systemPromptFile: "/tmp/agentkate-persona-t-1-abc.txt",
	})

	if v, ok := flagValue(args, flagAppendSystemPromptFile); !ok || v != "/tmp/agentkate-persona-t-1-abc.txt" {
		t.Errorf("%s = %q (present=%v)", flagAppendSystemPromptFile, v, ok)
	}
	if _, ok := flagValue(args, "--append-system-prompt"); ok {
		t.Error("the inline flag is still present; the persona is back in argv")
	}
	for _, a := range args {
		if strings.Contains(a, persona) {
			t.Fatalf("persona text found in argv: %q", a)
		}
	}
}

// TestBuildStartArgsPersonaFallsBackToArgv: a CLI without the file flag must
// still get the persona. Dropping it would silently hand the human a different
// agent than the one they configured; the inline flag is the honest fallback.
func TestBuildStartArgsPersonaFallsBackToArgv(t *testing.T) {
	args := buildStartArgs(nil, StartOptions{WorkDir: "/ws", SystemPrompt: "You are the scout."})
	if v, ok := flagValue(args, "--append-system-prompt"); !ok || v != "You are the scout." {
		t.Errorf("--append-system-prompt = %q (present=%v)", v, ok)
	}
	if _, ok := flagValue(args, flagAppendSystemPromptFile); ok {
		t.Error("the file flag was passed with no file staged")
	}
}

// TestParseCLIFlagsOptionalSuffix: claude 2.1.220 advertises
// --append-system-prompt-file ONLY as "--append-system-prompt[-file]" in its
// help. Without expanding that notation the file form looks unsupported on a
// binary that has it, and the persona stays in argv forever.
func TestParseCLIFlagsOptionalSuffix(t *testing.T) {
	help := `Options:
      --append-system-prompt <prompt>  Append a system prompt
                                       Explicitly provide context via:
                                       --system-prompt[-file],
                                       --append-system-prompt[-file], --add-dir
`
	flags := parseCLIFlags(help)
	for _, want := range []string{
		"--append-system-prompt", flagAppendSystemPromptFile,
		"--system-prompt", "--system-prompt-file", "--add-dir",
	} {
		if !flags[want] {
			t.Errorf("parseCLIFlags missed %s: %v", want, flags)
		}
	}
	// The expansion must not invent flags out of ordinary bracketed prose.
	if flags["--add-dir-file"] {
		t.Error("parseCLIFlags invented --add-dir-file")
	}
}

// TestWritePersonaFilePrivate: the staged file is owner-only and holds exactly
// the persona. 0600 from creation, never 0644-then-chmod.
func TestWritePersonaFilePrivate(t *testing.T) {
	const persona = "You are the scout."
	path, err := writePersonaFile("t-1", persona)
	if err != nil {
		t.Fatalf("writePersonaFile: %v", err)
	}
	defer os.Remove(path)

	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("persona file mode = %o, want 600", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != persona {
		t.Errorf("persona file body = %q, want the persona verbatim", b)
	}
}
