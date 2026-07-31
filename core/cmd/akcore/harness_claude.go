package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/modelcatalog"
	"agentkate/internal/session"
)

// claudeHarness adapts the Claude Code supervisor (`claude -p` over
// stream-json) to the harness interface. It is the default engine and the
// most capable one: forking, compaction, promotion, provider routing and the
// Cowork desktop server are all Claude-only today.
type claudeHarness struct {
	sup        *agent.Supervisor
	exePath    string
	socketPath string
}

func newClaudeHarness(sup *agent.Supervisor, exePath, socketPath string) *claudeHarness {
	return &claudeHarness{sup: sup, exePath: exePath, socketPath: socketPath}
}

func (h *claudeHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{
		// The canonical id. Persisted records historically use "" for the
		// default backend (session.BackendClaude); the registry resolves ""
		// here, and records written from now on carry the explicit id.
		ID:              "claude",
		DisplayName:     "Claude Code",
		Badge:           "", // the default engine stays unmarked in the roster
		Fork:            true,
		Compaction:      true,
		Promote:         true,
		ProviderRouting: true,
		Cowork:          true,
		// Verified against claude 2.1.220: set_model / set_permission_mode
		// exist mid-session, set_effort does not — effort is start-time only.
		EffortLive:        false,
		UsageReporting:    true,
		SessionBrowse:     true,
		TranscriptPreview: true, // claude keeps the on-disk session store
		MintsSessionID:    true,
		// Both verified against claude 2.1.220 in print mode:
		// --append-system-prompt reaches the model (a probe persona changed the
		// reply), and --agents registers custom subagents whose tools AND model
		// are honored (a haiku main agent's "sonnet" profile ran on
		// claude-sonnet-5 and saw only its allow-listed tools).
		SystemPrompt:    true,
		CustomSubagents: true,
		// Models are discovered live (`claude -p /model` for direct, the
		// provider's /v1/models for routed); the picker lists those plus free
		// text. Permission modes and effort remain static vocabularies below.
		ModelPicker: harness.ModelPickerDiscovered,
		PermissionModes: []string{
			"acceptEdits", "default", "plan", "auto", "bypassPermissions",
		},
		Efforts: []string{"low", "medium", "high", "xhigh", "max"},
	}
}

// claudeAgentEntry is one value of claude's --agents JSON object (the name is
// the object KEY, not a field). Verified against claude 2.1.220: "description"
// and "prompt" are required — an entry missing either is dropped SILENTLY,
// with no CLI error — "tools" must be a JSON array (a comma-separated string
// drops the whole entry), and "model" is honored for that profile's subagent
// turns. Unknown extra fields are tolerated.
type claudeAgentEntry struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools,omitempty"`
	Model       string   `json:"model,omitempty"`
}

// maxArgBytes is the kernel's cap on ONE argv element (Linux MAX_ARG_STRLEN =
// 32 pages = 128 KiB including the trailing NUL). Both persona flags are a
// single element each, and exceeding it fails the whole exec with an opaque
// E2BIG — a launch failure that would look nothing like "your prompt is too
// long". Each element is therefore measured on its own before the spawn, and
// an oversize one is dropped with a reason instead.
const maxArgBytes = 128*1024 - 1

// tooLongForArgv is the reason an oversize persona element reports.
func tooLongForArgv(what string, n int) string {
	return fmt.Sprintf("%s is %d bytes, over the %d-byte limit on a single "+
		"command-line argument (Linux MAX_ARG_STRLEN); it was not passed to "+
		"the CLI, which would have failed to start", what, n, maxArgBytes)
}

// personaSystemPrompt normalises a requested system prompt into what is
// actually passed on the command line, plus the reason when it is not. Empty
// or blank text is no request at all (an empty --append-system-prompt would
// still read as a custom prompt to the CLI); oversize text is dropped with a
// reason rather than left to fail the spawn with an opaque E2BIG.
func personaSystemPrompt(requested string) (pass, whyNot string) {
	if strings.TrimSpace(requested) == "" {
		return "", ""
	}
	if len(requested) > maxArgBytes {
		return "", tooLongForArgv("the system prompt", len(requested))
	}
	return requested, ""
}

// buildAgentsJSON renders the profiles claude can actually register into the
// --agents payload, and reports per-profile applied-truth for the rest. The
// binary validates nothing (a malformed payload or an incomplete entry is
// silently ignored), so the checks live here: a profile that would vanish
// inside the CLI is refused up front and named as unapplied instead.
func buildAgentsJSON(profiles []harness.AgentProfile) (string, []harness.AppliedAgent) {
	if len(profiles) == 0 {
		return "", nil
	}
	entries := make(map[string]claudeAgentEntry, len(profiles))
	applied := make([]harness.AppliedAgent, 0, len(profiles))
	for _, p := range profiles {
		name := strings.TrimSpace(p.Name)
		a := harness.AppliedAgent{Name: p.Name}
		switch {
		case name == "":
			a.Unapplied = []string{"the profile has no name"}
		case strings.TrimSpace(p.Description) == "":
			a.Unapplied = []string{"a description is required (Claude Code drops a profile without one)"}
		case strings.TrimSpace(p.Prompt) == "":
			a.Unapplied = []string{"a prompt is required (Claude Code drops a profile without one)"}
		default:
			if _, dup := entries[name]; dup {
				a.Unapplied = []string{"another profile is already named " + name}
				break
			}
			entries[name] = claudeAgentEntry{
				Description: p.Description,
				Prompt:      p.Prompt,
				Tools:       p.Tools,
				Model:       p.Model,
			}
			a.Applied = true
		}
		applied = append(applied, a)
	}
	if len(entries) == 0 {
		return "", applied
	}
	payload, err := json.Marshal(entries)
	// refuseAll turns an all-or-nothing payload failure into per-profile
	// truth: the flag carries every profile at once, so if it cannot be
	// passed, none of them were.
	refuseAll := func(reason string) (string, []harness.AppliedAgent) {
		for i := range applied {
			if applied[i].Applied {
				applied[i].Applied = false
				applied[i].Unapplied = []string{reason}
			}
		}
		return "", applied
	}
	if err != nil {
		// Unreachable for these field types, but a payload claude would ignore
		// must never be reported as applied.
		return refuseAll("the profile could not be encoded: " + err.Error())
	}
	if len(payload) > maxArgBytes {
		return refuseAll(tooLongForArgv("the combined subagent definitions JSON", len(payload)))
	}
	return string(payload), applied
}

func (h *claudeHarness) Launch(spec harness.StartSpec) (harness.Launched, error) {
	mcpConfig, err := writeMCPConfig(h.exePath, h.socketPath, spec.ThreadID,
		spec.WorkDir, spec.Cowork)
	if err != nil {
		return harness.Launched{}, fmt.Errorf("mcp config: %w", err)
	}
	model := resolveModel(spec.Model)
	agentsJSON, appliedAgents := buildAgentsJSON(spec.Agents)
	systemPrompt, systemPromptWhyNot := personaSystemPrompt(spec.SystemPrompt)
	if _, err := h.sup.Start(agent.StartOptions{
		ID:             spec.ThreadID,
		WorkDir:        spec.WorkDir,
		Prompt:         spec.Prompt,
		MCPConfig:      mcpConfig,
		PermissionMode: spec.PermissionMode,
		Effort:         spec.Effort,
		Model:          model,
		Attachments:    spec.Attachments,
		SessionID:      spec.SessionID,
		Resume:         spec.Resume,
		ForkSession:    spec.ForkSession,
		CoworkEnabled:  spec.Cowork,
		Provider:       spec.Provider,
		SystemPrompt:   systemPrompt,
		AgentsJSON:     agentsJSON,
	}); err != nil {
		os.Remove(mcpConfig)
		return harness.Launched{}, err
	}
	mode := spec.PermissionMode
	if mode == "" {
		mode = "acceptEdits" // the supervisor's own default — persist reality
	}
	sessionID := spec.SessionID
	if spec.ForkSession {
		// A fork mints a fresh session id; the run loop captures it from the
		// init event into the record.
		sessionID = ""
	}
	return harness.Launched{
		SessionID:      sessionID,
		Model:          model,
		Effort:         spec.Effort,
		PermissionMode: mode,
		// --append-system-prompt is unconditional once the CLI has started, so
		// whatever survived the checks above IS the applied truth here.
		SystemPromptApplied:   systemPrompt != "",
		SystemPromptUnapplied: systemPromptWhyNot,
		Agents:                appliedAgents,
	}, nil
}

func (h *claudeHarness) Send(threadID, text string, atts []agent.Attachment) error {
	return h.sup.Send(threadID, text, atts)
}

func (h *claudeHarness) Interrupt(threadID string) error { return h.sup.Interrupt(threadID) }
func (h *claudeHarness) Stop(threadID string) error      { return h.sup.Stop(threadID) }
func (h *claudeHarness) Running(threadID string) bool    { return h.sup.Running(threadID) }
func (h *claudeHarness) StopAll()                        { h.sup.StopAll() }

func (h *claudeHarness) ReadTranscript(threadID, sessionID string) ([]json.RawMessage, error) {
	if sessionID == "" {
		return nil, nil // no session yet — nothing to replay
	}
	return session.ReadTranscript(sessionID)
}

// DiscoverOptions: Claude's mode/effort vocabularies are static (see
// Capabilities); its models are discovered separately via DiscoverModels, so
// there is nothing to probe here.
func (h *claudeHarness) DiscoverOptions() ([]harness.DiscoveredOption, error) {
	return nil, nil
}

// DiscoverModels enumerates the live model vocabulary. For a routed provider it
// GETs that provider's /v1/models; for Claude direct it runs `claude -p /model`.
// Both are best-effort: any failure returns an empty list (never an error that
// would blank a cached picker), so the UI keeps its last good catalogue.
func (h *claudeHarness) DiscoverModels(p *agent.Provider) ([]harness.DiscoveredOptionValue, error) {
	if p.Routed() {
		key := p.AuthToken
		if key == "" && p.EnvVar != "" {
			key = os.Getenv(p.EnvVar)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		models, err := modelcatalog.Fetch(ctx, p.BaseURL, key)
		if err != nil {
			return nil, nil
		}
		out := make([]harness.DiscoveredOptionValue, 0, len(models))
		for _, m := range models {
			out = append(out, harness.DiscoveredOptionValue{Value: m.ID, Name: m.Name})
		}
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	models, err := h.sup.DiscoverModels(ctx)
	if err != nil {
		return nil, nil
	}
	out := make([]harness.DiscoveredOptionValue, 0, len(models))
	for _, m := range models {
		out = append(out, harness.DiscoveredOptionValue{Value: m.Value, Name: m.Name})
	}
	return out, nil
}

// BrowseSessions wraps the on-disk transcript discovery in the neutral browse
// shape, so session.browse treats every browse-capable harness the same.
func (h *claudeHarness) BrowseSessions() ([]harness.BrowsableSession, error) {
	found, err := session.Discover()
	if err != nil {
		return nil, err
	}
	out := make([]harness.BrowsableSession, 0, len(found))
	for _, f := range found {
		out = append(out, harness.BrowsableSession{
			SessionID: f.SessionID,
			Backend:   h.Capabilities().ID,
			Project:   f.Project,
			Title:     f.Title,
			// Second-precision UTC, matching the other adapters, so the merged
			// browse list sorts correctly as plain strings.
			Updated: f.Modified.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func (h *claudeHarness) SetOption(threadID, option, value string) (string, error) {
	switch option {
	case "model":
		applied := resolveModel(value)
		return applied, h.sup.SetModel(threadID, applied)
	case "permissionMode":
		return value, h.sup.SetPermissionMode(threadID, value)
	case "effort":
		return "", fmt.Errorf(
			"Claude Code does not support changing the thinking effort mid-session")
	default:
		return "", fmt.Errorf("unknown option %q", option)
	}
}
