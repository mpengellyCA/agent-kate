// Package harness defines the contract every agent backend ("harness")
// fulfils, plus the capability set that drives routing and UI affordances.
//
// A harness is one way of running an agent: a CLI binary spoken to over some
// protocol (Claude Code over stream-json, Kimi Code over ACP, …). The
// orchestration layer never asks "is this kimi?" — it asks the thread's
// harness to act, and consults its Capabilities for anything optional. Adding
// a backend means implementing this interface and registering it (see
// docs/HARNESSES.md); no string compares should appear outside the adapter.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agentkate/internal/agent"
)

// ModelPicker kinds — how the UI should offer model choice for a harness.
const (
	// ModelPickerTiers: a fixed set of tier tokens (opus/sonnet/…) that the
	// core resolves to concrete model ids at launch.
	ModelPickerTiers = "tiers"
	// ModelPickerDiscovered: the CLI enumerates its models per session (the
	// init event's configOptions); the picker lists those plus free text.
	ModelPickerDiscovered = "discovered"
)

// Capabilities declares what one harness supports. The handlers gate optional
// RPCs on these (one shared "X is not supported by <DisplayName> agents"
// message), and the UI fetches them via agent.capabilities to drive its
// affordances — so a wrong flag here shows up as a wrongly-enabled button,
// never as a scattered conditional.
type Capabilities struct {
	ID          string `json:"id"`          // registry key, e.g. "claude", "kimi"
	DisplayName string `json:"displayName"` // human name, e.g. "Claude Code"
	// Badge prefixes the roster subtitle ("Kimi · working"); empty for the
	// default engine so the common case stays unmarked.
	Badge string `json:"badge"`

	Fork bool `json:"fork"` // agent.fork (--fork-session semantics)
	// Compaction: the thread's context can be compacted at all — it gates the
	// strategy/status RPCs and the HOT (in-session) mechanism, which every
	// compacting harness has.
	Compaction bool `json:"compaction"`
	// ColdCompact: the harness can also compact a thread that is NOT running,
	// by reading the session back from disk (session.ReadTranscript) or
	// re-running the CLI over the stored session, and hand back summary TEXT
	// the core stores and later replays into a fresh session.
	//
	// False for harnesses whose only mechanism rewrites the LIVE session's own
	// context and keeps it (kimi's `/compact`): there is no local transcript in
	// the claude format to read and no summary to store, so the cold path must
	// be refused rather than fed an empty or foreign-shaped read — seeding a
	// new session from that would discard the thread's whole history.
	ColdCompact     bool `json:"coldCompact"`
	Promote         bool `json:"promote"`         // agent.promote (move session into an isolated worktree)
	ProviderRouting bool `json:"providerRouting"` // third-party Anthropic-compatible endpoints
	Cowork          bool `json:"cowork"`          // the KDE Cowork desktop MCP server
	// LiveToolReveal: the CLI honours the MCP notifications/tools/list_changed
	// notification, so a server that starts advertising new tools mid-session
	// is picked up without a relaunch. True for claude 2.1.220 (probed: the
	// revealed tool was listed AND callable in the next turn); false for kimi
	// 0.30, which lists a server's tools once at session/new and never
	// re-lists — switching Cowork on there re-attaches the session instead
	// (session/resume keeps the conversation and takes a new mcpServers list,
	// also probed).
	LiveToolReveal bool `json:"liveToolReveal"`
	EffortLive     bool `json:"effortLive"`     // thinking effort adjustable mid-session
	UsageReporting bool `json:"usageReporting"` // tokens/cost in result events
	SessionBrowse  bool `json:"sessionBrowse"`  // on-disk session discovery (session.browse)
	// TranscriptPreview: the harness keeps a previewable, forgettable on-disk
	// transcript store (session.preview / session.forget). False for harnesses
	// whose transcript lives only in the core's translated-event log (kimi), so
	// the session browser shows metadata instead of a preview and hides Forget.
	TranscriptPreview bool `json:"transcriptPreview"`
	// MintsSessionID: the core pre-mints the session id and passes it to the
	// CLI (claude --session-id). False = the CLI assigns its own during the
	// handshake, so agent.start replies with an empty sessionId and the id is
	// captured from the init event.
	MintsSessionID bool `json:"mintsSessionId"`

	// SystemPrompt: the CLI takes caller-supplied persona text ALONGSIDE its
	// own system prompt (claude --append-system-prompt). False means the
	// channel does not exist — Launch reports a requested StartSpec.SystemPrompt
	// as unapplied and the caller folds the persona into the opening message
	// instead, which works on every harness.
	SystemPrompt bool `json:"systemPrompt"`
	// CustomSubagents: the CLI takes caller-defined subagent profiles for the
	// session (claude --agents), so the thread's agent can delegate to them.
	// False means the CLI's subagent vocabulary is fixed; Launch reports every
	// requested StartSpec.AgentProfile as unapplied.
	CustomSubagents bool `json:"customSubagents"`
	// The launch-option sweep (plan 16 P6). Each gates one StartSpec list; a
	// false flag means the UI does not offer the option and Launch reports a
	// request for it as unapplied rather than dropping it.
	FallbackModels  bool `json:"fallbackModels"`  // ordered model fallbacks
	DisallowedTools bool `json:"disallowedTools"` // per-session tool deny-list
	AddDirs         bool `json:"addDirs"`         // extra reachable directories
	// The control-channel sweep. StrictMCPConfig: the thread can be isolated
	// from the human's globally-configured MCP servers. CostBudget: the thread
	// takes a hard spend ceiling the CLI enforces itself.
	StrictMCPConfig bool `json:"strictMcpConfig"`
	CostBudget      bool `json:"costBudget"`
	// SubagentTranscripts: the CLI writes a per-subagent conversation file the
	// UI can tail (agent.subagentTranscripts). False = a thread's delegations
	// are only visible in its own transcript.
	SubagentTranscripts bool `json:"subagentTranscripts"`
	// SkillReload: a RUNNING session can be told to re-read its skill
	// directories (claude's reload_skills control request), so a skill the
	// human installs mid-session reaches an agent that is already working.
	//
	// False means the session reads its skills once, at start, and nothing can
	// change that without a relaunch — which is a fact the HUMAN has to be
	// told (audit F50): they install a skill, the fleet-wide reload reports
	// success, and the agent they were about to hand the work to silently does
	// not have it. reloadSkillsEverywhere therefore drops a notice in the
	// panels of the running threads it had to SKIP, instead of skipping them
	// silently. Keep in lockstep with ui/src/state/HarnessTraits.cpp.
	SkillReload bool `json:"skillReload"`

	ModelPicker string `json:"modelPicker"` // ModelPickerTiers | ModelPickerDiscovered
	// PermissionModes / Efforts: the harness's static vocabularies (values
	// only; the UI owns the human labels). Empty = the vocabulary is
	// discovered per session from the CLI's configOptions instead.
	PermissionModes []string `json:"permissionModes"`
	Efforts         []string `json:"efforts"`
}

// DiscoveredOption mirrors one CLI config-option enumeration (the shape the
// init event already carries), for harnesses whose vocabulary is discovered.
type DiscoveredOption struct {
	ID      string                  `json:"id"`
	Name    string                  `json:"name"`
	Options []DiscoveredOptionValue `json:"options"`
	// Current is the value a FRESH session of this harness comes up with — the
	// CLI's own default for the option, not a guess. Empty when the harness
	// does not report one.
	//
	// Load-bearing beyond the picker: the launch authority gate reads the
	// `mode` option's Current to know what a worker launched with NO permission
	// mode will actually run at, for engines whose vocabulary is discovered
	// rather than declared (authority.go, launchBaselineRank). Dropping it here
	// would make that gate fall back to a guess.
	Current string `json:"current,omitempty"`
}

// DiscoveredOptionValue is one selectable value of a DiscoveredOption.
type DiscoveredOptionValue struct {
	Value string `json:"value"`
	Name  string `json:"name"`
	// Efforts, on a model value, are the reasoning-effort tiers that model
	// supports (claude's list_models reports them). Empty means the harness
	// said nothing, which the UI reads as "every tier" — never as "none".
	Efforts []string `json:"efforts,omitempty"`
}

// BrowsableSession is one discoverable past session in the neutral browse
// shape session.browse serves — whichever harness owns it.
type BrowsableSession struct {
	SessionID string `json:"sessionId"`
	Backend   string `json:"backend"` // owning harness id
	Project   string `json:"project"` // cwd the session ran in
	Title     string `json:"title"`
	Updated   string `json:"updated"`  // RFC3339, UTC
	Attached  bool   `json:"attached"` // filled by the handler
}

// StartSpec is the harness-neutral launch request. Adapters translate it into
// their CLI's own options; fields a harness doesn't support are validated
// away by the capability gates before Launch is ever called, so an adapter
// may simply ignore them.
type StartSpec struct {
	ThreadID    string
	WorkDir     string // the worktree (also the MCP bridge's workspace)
	Prompt      string // opening message; empty on resume
	Attachments []agent.Attachment

	Model          string // harness vocabulary (tier token or CLI model id)
	Effort         string // harness vocabulary (claude --effort / kimi thinking)
	PermissionMode string // harness vocabulary (claude mode / kimi mode)

	SessionID   string // pre-minted (MintsSessionID) or the session to resume
	Resume      bool   // re-attach SessionID instead of starting fresh
	ForkSession bool   // with Resume: branch a NEW session off the resumed context

	// Cowork is the thread's desktop opt-in as it stands at launch. Both
	// shipped adapters ignore it: they wire the Cowork bridge in
	// unconditionally and it reads the session record live, which is what lets
	// the opt-in change mid-session. It stays in the neutral spec for an
	// adapter that must decide its tool wiring up front and has no equivalent
	// of a bridge that can go quiet.
	Cowork   bool
	Provider *agent.Provider // third-party API routing; nil = direct

	// BridgeSecret and CoworkBridgeSecret authenticate this launch's two MCP
	// bridges back to the core (audit F13) — one secret each, because a
	// redeemed secret is spent while its holder is connected, so two bridges
	// sharing one would make the second look like a replay of the first.
	//
	// The adapter passes each to ITS OWN `akcore mcp` server, in the server's
	// ENVIRONMENT — never in argv, which any local process can read — and never
	// persists either: they are minted per launch and live only in the core's
	// memory, the bridge process's environment, and (for claude) the 0600
	// mcp-config file the supervisor deletes when the process exits.
	//
	// An adapter that spawns no akcore bridge ignores them. An empty value means
	// none was minted; that bridge then fails its identify and exits, which is
	// the fail-closed direction (no tools, rather than unauthenticated ones).
	BridgeSecret       string
	CoworkBridgeSecret string

	// Env overlays the agent process's environment, on top of the core's own
	// (and after any provider routing). Neutral by design — every adapter
	// applies it the same way, because "run this thread's CLI with this
	// variable set" needs no CLI knowledge. It is how per-thread CLI state
	// isolation is expressed (e.g. KIMI_CODE_HOME pointing a kimi thread at
	// its own home directory).
	//
	// It is deliberately NOT reachable from an agent: launch_agent does not
	// accept it. A worker's environment decides where its credentials come
	// from and which endpoint they are sent to, so handing that lever to a
	// model would route around the permission prompt that guards every other
	// way of doing the same thing.
	Env map[string]string

	// SystemPrompt is persona text to run the thread's agent with, on top of
	// the CLI's own system prompt. Gated by Capabilities.SystemPrompt: a
	// harness without the channel reports it unapplied rather than emulating
	// it (an emulated persona would silently outrank the CLI's own prompt).
	SystemPrompt string
	// Agents are custom subagent definitions the thread's agent may delegate
	// to. Gated by Capabilities.CustomSubagents; reported per profile in
	// Launched.Agents, since a harness may express some fields and not others.
	Agents []AgentProfile

	// The launch-option sweep (plan 16 P6), each capability-gated and each
	// reported in Launched.UnappliedOptions when the harness has no equivalent:
	//
	//   FallbackModels  models to fall back to when the first is overloaded or
	//                   unavailable, in order (claude --fallback-model).
	//   DisallowedTools tool names the thread may not use at all
	//                   (claude --disallowedTools). Deny beats allow.
	//   AddDirs         extra directories the thread's tools may reach, beyond
	//                   its worktree (claude --add-dir).
	FallbackModels  []string
	DisallowedTools []string
	AddDirs         []string

	// The control-channel sweep:
	//
	//   StrictMCPConfig  run with ONLY the MCP servers the core passes, ignoring
	//                    the human's global configuration (claude
	//                    --strict-mcp-config). Gated by Capabilities.StrictMCPConfig.
	//   MaxBudgetUSD     a hard spend ceiling for the session, enforced by the
	//                    CLI (claude --max-budget-usd). 0 = uncapped. Gated by
	//                    Capabilities.CostBudget.
	//   Title            the thread's human title, passed so the session is
	//                    identifiable in the CLI's own session listings (claude
	//                    --name). Cosmetic, so it is NOT capability-gated: an
	//                    adapter with no equivalent simply ignores it.
	StrictMCPConfig bool
	MaxBudgetUSD    float64
	Title           string
}

// AgentProfile is one custom subagent definition, harness-neutrally. Adapters
// translate it into their CLI's own vocabulary and report, per profile, what
// they could express (Launched.Agents).
type AgentProfile struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"` // body / system prompt
	// Tools is an allow-list of tool names; empty = every tool the thread has.
	// Names pass through UNVALIDATED, matching the CLIs' own permissiveness:
	// neither harness rejects an unknown tool name, it simply grants nothing
	// for it. Validating here would mean maintaining a tool catalogue per
	// harness that goes stale with every CLI release.
	Tools []string `json:"tools,omitempty"`
	Model string   `json:"model,omitempty"` // harness-specific id or ""
}

// AppliedAgent is the applied-truth for one requested AgentProfile: whether
// the profile reached the CLI at all, and — when it did — which of its fields
// the CLI could not express. Reasons are human-readable; they reach the
// launching agent verbatim.
type AppliedAgent struct {
	Name      string   // the profile's name, as requested
	Applied   bool     // the profile itself reached the harness
	Unapplied []string // what could not be expressed; empty = fully applied
}

// UnappliedAgents reports every requested profile as not applied under one
// shared reason — what an adapter whose CustomSubagents capability is false
// returns from Launch, so the request surfaces as a downgrade instead of
// vanishing.
func UnappliedAgents(profiles []AgentProfile, reason string) []AppliedAgent {
	if len(profiles) == 0 {
		return nil
	}
	out := make([]AppliedAgent, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, AppliedAgent{Name: p.Name, Unapplied: []string{reason}})
	}
	return out
}

// Launched reports what a Launch actually applied, for the thread's record —
// so resume replays reality, not the request. SessionID may be empty when the
// id is assigned later (a fork's init event mints it).
type Launched struct {
	SessionID      string
	Model          string
	Effort         string
	PermissionMode string

	// SystemPromptApplied reports whether a requested StartSpec.SystemPrompt
	// actually reached the CLI. False with no request simply means none was
	// asked for — callers compare it against what they sent.
	SystemPromptApplied bool
	// SystemPromptUnapplied is why it did not, when the adapter knows
	// something more specific than "this harness has no such channel" (an
	// oversize prompt, say). Empty leaves the caller on the shared
	// capability wording.
	SystemPromptUnapplied string
	// Agents carries one entry per requested StartSpec.AgentProfile, in the
	// same order, so a caller can name exactly which profile lost what.
	Agents []AppliedAgent
	// UnappliedOptions names launch options the harness could not express, one
	// entry each with a reason. It is for the list-valued options (the P6
	// sweep), where there is no single "applied value" to diff against — the
	// adapter has to say so explicitly or the request would simply vanish.
	UnappliedOptions []UnappliedOption
}

// UnappliedOption is one requested launch option a harness could not apply.
type UnappliedOption struct {
	Option string // the neutral option name, e.g. "addDirs"
	Reason string // human-readable; reaches the launching agent verbatim
}

// CompactSpec is one context-compaction pass, harness-neutrally. Two shapes,
// distinguished by Hot:
//
//   - Hot: send Prompt into the LIVE thread and take the assistant's reply as
//     the summary. No re-cache cost, but it needs a running process.
//   - Cold: a fresh, separate pass over SessionID from WorkDir on Model. Works
//     on a dormant thread and pays a full prefix re-cache.
//
// Only ever called on harnesses whose Capabilities().Compaction is true, and
// the cold shape additionally requires Capabilities().ColdCompact — a harness
// whose compaction only rewrites the live session has no dormant pass to run.
// The rest return the shared not-supported error, so no caller needs to know
// which CLI can do this.
type CompactSpec struct {
	ThreadID  string
	SessionID string // the session a cold pass resumes; empty for hot
	WorkDir   string // cwd for a cold pass
	Model     string // cold-pass model; empty leaves the CLI's default
	Prompt    string // the compaction instruction
	Hot       bool
	Timeout   time.Duration // 0 = the caller's context deadline only
}

// Harness is one agent backend. Implementations wrap a supervisor owning the
// child processes; all methods must be safe for concurrent use.
type Harness interface {
	Capabilities() Capabilities

	// Launch starts (or resumes, or forks — per spec) one agent thread.
	Launch(spec StartSpec) (Launched, error)

	Send(threadID, text string, atts []agent.Attachment) error
	Interrupt(threadID string) error
	Stop(threadID string) error
	Running(threadID string) bool
	StopAll()

	// ReadTranscript returns the thread's replayable event history. sessionID
	// is the record's session id — harnesses whose transcript lives with the
	// CLI (claude) need it; harnesses that keep their own event log ignore it.
	ReadTranscript(threadID, sessionID string) ([]json.RawMessage, error)

	// SetOption changes one session option — "model", "effort" or
	// "permissionMode" — on a RUNNING thread, mid-session. It returns the
	// value as applied (e.g. a tier resolved to a concrete model id) so the
	// caller persists reality. An unsupported option returns an error naming
	// the harness.
	SetOption(threadID, option, value string) (applied string, err error)

	// DiscoverOptions probes the harness's live configuration vocabulary
	// (model / effort / mode enumerations, with display names) without
	// starting a thread. Static-vocabulary harnesses (ModelPicker "tiers")
	// return (nil, nil). Implementations may cache.
	DiscoverOptions() ([]DiscoveredOption, error)

	// BrowseSessions returns this harness's discoverable past sessions. Only
	// called for harnesses whose Capabilities().SessionBrowse is true.
	BrowseSessions() ([]BrowsableSession, error)

	// Compact runs one compaction pass and returns the summary body. Gated by
	// Capabilities().Compaction (and Capabilities().ColdCompact for a cold
	// spec): a harness without it returns Unsupported("Compaction", …) rather
	// than emulating a summary, because a summary the model never wrote is
	// worse than none.
	//
	// A harness whose mechanism compacts the context in place and returns no
	// text has no summary body to give. It reports that as its OWN sentinel
	// error (see kimiHarness / ErrCompactedInPlace) — every caller must
	// errors.Is it and treat it as success-without-summary: store nothing,
	// reseed nothing, keep the same session.
	Compact(ctx context.Context, spec CompactSpec) (string, error)
}

// Unsupported is the shared "this harness cannot do that" error, so a
// capability gate reads identically wherever it is enforced (the RPC layer
// wraps the same wording in an IPC error).
func Unsupported(feature string, caps Capabilities) error {
	return fmt.Errorf("%s is not supported by %s agents", feature, caps.DisplayName)
}

// Registry holds the registered harnesses. It is built once at startup and
// read-only afterwards, so it needs no locking.
type Registry struct {
	defaultID string
	order     []string
	m         map[string]Harness
}

// NewRegistry creates a registry whose Get resolves the empty id to
// defaultID — persisted records use "" for the default backend.
func NewRegistry(defaultID string) *Registry {
	return &Registry{defaultID: defaultID, m: make(map[string]Harness)}
}

// Register adds a harness under its Capabilities().ID. Registration order is
// preserved (it is the order pickers list engines in).
func (r *Registry) Register(h Harness) {
	id := h.Capabilities().ID
	if _, dup := r.m[id]; !dup {
		r.order = append(r.order, id)
	}
	r.m[id] = h
}

// Get returns the harness for id; the empty id resolves to the default.
func (r *Registry) Get(id string) (Harness, bool) {
	if id == "" {
		id = r.defaultID
	}
	h, ok := r.m[id]
	return h, ok
}

// All returns the harnesses in registration order.
func (r *Registry) All() []Harness {
	out := make([]Harness, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.m[id])
	}
	return out
}

// SubagentTranscript points at one subagent's on-disk conversation for a
// thread — the file the UI live-tails in the subagent transcript viewer.
// Layouts differ per CLI (claude keeps
// <project>/<session>/subagents/agent-<id>.jsonl, kimi keeps
// <session-dir>/agents/<id>/wire.jsonl), which is exactly why discovery
// belongs to the adapter and the neutral shape is just "id, label, path".
type SubagentTranscript struct {
	ID    string `json:"id"`    // the CLI's own subagent id
	Label string `json:"label"` // profile/type name when the file records one
	Path  string `json:"path"`  // absolute path to the JSONL file
}
