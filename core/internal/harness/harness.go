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

	Fork            bool `json:"fork"`            // agent.fork (--fork-session semantics)
	Compaction      bool `json:"compaction"`      // summaries: setCompactStrategy/compactNow/summaryStatus + exit compaction
	Promote         bool `json:"promote"`         // agent.promote (move session into an isolated worktree)
	ProviderRouting bool `json:"providerRouting"` // third-party Anthropic-compatible endpoints
	Cowork          bool `json:"cowork"`          // the KDE Cowork desktop MCP server
	EffortLive      bool `json:"effortLive"`      // thinking effort adjustable mid-session
	UsageReporting  bool `json:"usageReporting"`  // tokens/cost in result events
	SessionBrowse   bool `json:"sessionBrowse"`   // on-disk session discovery (session.browse)
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
}

// DiscoveredOptionValue is one selectable value of a DiscoveredOption.
type DiscoveredOptionValue struct {
	Value string `json:"value"`
	Name  string `json:"name"`
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

	Cowork   bool            // opt into the Cowork desktop MCP server
	Provider *agent.Provider // third-party API routing; nil = direct

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
}

// CompactSpec is one context-compaction pass, harness-neutrally. Two shapes,
// distinguished by Hot:
//
//   - Hot: send Prompt into the LIVE thread and take the assistant's reply as
//     the summary. No re-cache cost, but it needs a running process.
//   - Cold: a fresh, separate pass over SessionID from WorkDir on Model. Works
//     on a dormant thread and pays a full prefix re-cache.
//
// Only ever called on harnesses whose Capabilities().Compaction is true; the
// rest return the shared not-supported error, so no caller needs to know which
// CLI can do this.
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
	// Capabilities().Compaction: a harness without it returns
	// Unsupported("Compaction", …) rather than emulating a summary, because a
	// summary the model never wrote is worse than none.
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
