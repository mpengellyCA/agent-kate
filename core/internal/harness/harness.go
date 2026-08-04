// Package harness defines the versioned, neutral linkage contract every agent
// backend ("harness") fulfils.  It is the only package that owns data which
// crosses from a native harness into the core/UI boundary.
//
// A harness is one way of running an agent: a CLI binary spoken to over some
// protocol (Claude Code over stream-json, Kimi Code over ACP, …). The
// orchestration layer never asks "is this kimi?" — it asks the thread's
// harness to act and reads its descriptor/catalogue. Adding a backend means
// implementing this interface and registering it (see docs/HARNESSES.md); no
// string compares should appear outside the adapter.
package harness

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"agentkate/internal/agent"
)

// ContractVersion changes only when the JSON DTO semantics change.  It is
// deliberately in each response, rather than negotiated through an SDK: the
// core and UI are released together but fixtures can still reject a drift.
const ContractVersion = 2

// OperationKind is a named end-to-end action.  A descriptor contains an entry
// only when the adapter has implemented and tested that action; absence is the
// unsupported state.  This replaces the old boolean capability matrix.
type OperationKind string

const (
	OperationFork                OperationKind = "fork"
	OperationCompaction          OperationKind = "compaction"
	OperationColdCompaction      OperationKind = "coldCompaction"
	OperationPromote             OperationKind = "promote"
	OperationProviderRouting     OperationKind = "providerRouting"
	OperationProviderRegistry    OperationKind = "providerRegistry"
	OperationCowork              OperationKind = "cowork"
	OperationLiveToolReveal      OperationKind = "liveToolReveal"
	OperationUsageReporting      OperationKind = "usageReporting"
	OperationSessionBrowse       OperationKind = "sessionBrowse"
	OperationTranscriptPreview   OperationKind = "transcriptPreview"
	OperationMintSessionID       OperationKind = "mintSessionId"
	OperationSystemPrompt        OperationKind = "systemPrompt"
	OperationCustomSubagents     OperationKind = "customSubagents"
	OperationFallbackModels      OperationKind = "fallbackModels"
	OperationDisallowedTools     OperationKind = "disallowedTools"
	OperationAddDirectories      OperationKind = "addDirectories"
	OperationStrictMCPConfig     OperationKind = "strictMcpConfig"
	OperationCostBudget          OperationKind = "costBudget"
	OperationSubagentTranscripts OperationKind = "subagentTranscripts"
	OperationSkillReload         OperationKind = "skillReload"
	// OperationCommands means the harness supplies a native command catalogue
	// for the composer's command completion.  It is intentionally separate from
	// the ability to *send* a slash-prefixed prompt: a client must not advertise
	// a made-up command list merely because raw text happens to work.
	OperationCommands OperationKind = "commands"
)

// InteropSupport is the availability of a user-visible harness feature.  The
// matrix deliberately says nothing about a native protocol feature that the
// adapter has not wired end-to-end: Unsupported is the safe default.
type InteropSupport string

const (
	InteropUnsupported InteropSupport = "unsupported"
	InteropNative      InteropSupport = "native"
	// InteropManaged is implemented by Agent Kate rather than by the harness,
	// but is still available consistently to a user of that harness.  It is
	// useful for bounded continuation, whose control loop is host-owned.
	InteropManaged InteropSupport = "managed"
)

// CompactionInterop distinguishes a live in-place native compact operation
// from a cold (dormant-session) compaction pass.  They have materially
// different preconditions and must never be collapsed into one boolean.
type CompactionInterop struct {
	InPlace InteropSupport `json:"inPlace"`
	Cold    InteropSupport `json:"cold"`
}

// InteroperabilityMatrix is the feature-parity DTO.  It is intentionally
// capability-specific rather than a harness-name switch: UI and orchestration
// code can present an equivalent workflow where it exists and an honest
// unsupported state where it does not.
//
// Plans, Tasks and Subagents mean their complete native lifecycle is surfaced,
// not merely that transcript text happens to mention them.  SubagentTranscripts
// separately captures the useful narrower case supported by existing adapters.
type InteroperabilityMatrix struct {
	Commands            InteropSupport    `json:"commands"`
	Compaction          CompactionInterop `json:"compaction"`
	Continuation        InteropSupport    `json:"continuation"`
	Plans               InteropSupport    `json:"plans"`
	Tasks               InteropSupport    `json:"tasks"`
	Subagents           InteropSupport    `json:"subagents"`
	SubagentTranscripts InteropSupport    `json:"subagentTranscripts"`
	Questions           InteropSupport    `json:"questions"`
	DynamicTools        InteropSupport    `json:"dynamicTools"`
}

// InteropFromOperations provides the conservative baseline for adapters that
// already declare the older operation DTO.  New dimensions remain unsupported
// until a harness explicitly advertises and tests them.
func InteropFromOperations(operations []OperationDescriptor) InteroperabilityMatrix {
	matrix := InteroperabilityMatrix{
		Commands: InteropUnsupported, Continuation: InteropUnsupported,
		Plans: InteropUnsupported, Tasks: InteropUnsupported,
		Subagents: InteropUnsupported, SubagentTranscripts: InteropUnsupported,
		Questions: InteropUnsupported, DynamicTools: InteropUnsupported,
		Compaction: CompactionInterop{InPlace: InteropUnsupported, Cold: InteropUnsupported},
	}
	for _, operation := range operations {
		switch operation.Kind {
		case OperationCommands:
			matrix.Commands = InteropNative
		case OperationCompaction:
			matrix.Compaction.InPlace = InteropNative
		case OperationColdCompaction:
			matrix.Compaction.Cold = InteropNative
		case OperationSubagentTranscripts:
			matrix.SubagentTranscripts = InteropNative
		}
	}
	return matrix
}

func normaliseInteropSupport(value InteropSupport) InteropSupport {
	if value == "" {
		return InteropUnsupported
	}
	return value
}

// Available reports whether a matrix value represents an implemented feature.
// Callers that need to distinguish native from Agent-Kate-managed functionality
// can compare the value directly.
func (s InteropSupport) Available() bool {
	return s == InteropNative || s == InteropManaged
}

// ApplicationTiming says when a requested setting takes effect. It is part of
// the setting descriptor and of every application result, so a client never
// has to infer timing from a harness id.
type ApplicationTiming string

const (
	TimingLaunch   ApplicationTiming = "launch"
	TimingNextTurn ApplicationTiming = "nextTurn"
	TimingLive     ApplicationTiming = "live"
)

// OperationDescriptor is intentionally small. Native configuration and bridge
// bindings do not belong here; they stay inside adapter runtime objects.
type OperationDescriptor struct {
	Kind OperationKind `json:"kind"`
}

// HarnessDescriptor is the stable public identity and operation surface of a
// harness. Health detail remains available through engine.health; Health is a
// snapshot state suitable for rendering a descriptor before a fresh probe.
type HarnessDescriptor struct {
	ContractVersion  int                   `json:"contractVersion"`
	ID               string                `json:"id"`
	DisplayName      string                `json:"displayName"`
	Badge            string                `json:"badge,omitempty"`
	InstalledVersion string                `json:"installedVersion,omitempty"`
	ProtocolVersion  string                `json:"protocolVersion,omitempty"`
	Health           string                `json:"health"`
	Operations       []OperationDescriptor `json:"operations"`
	// Interop is a complete parity matrix.  Descriptors created by pre-matrix
	// adapters are serialised with the conservative operation-derived baseline;
	// adapters add new dimensions only after end-to-end coverage exists.
	Interop InteroperabilityMatrix `json:"interop"`
}

// Interoperability returns a complete, safe matrix.  The legacy operation
// bridge makes the DTO immediately useful without inventing any of the new
// protocol features.
func (d HarnessDescriptor) Interoperability() InteroperabilityMatrix {
	baseline := InteropFromOperations(d.Operations)
	configured := d.Interop
	merge := func(base, override InteropSupport) InteropSupport {
		if override != "" {
			return normaliseInteropSupport(override)
		}
		return base
	}
	baseline.Commands = merge(baseline.Commands, configured.Commands)
	baseline.Compaction.InPlace = merge(baseline.Compaction.InPlace, configured.Compaction.InPlace)
	baseline.Compaction.Cold = merge(baseline.Compaction.Cold, configured.Compaction.Cold)
	baseline.Continuation = merge(baseline.Continuation, configured.Continuation)
	baseline.Plans = merge(baseline.Plans, configured.Plans)
	baseline.Tasks = merge(baseline.Tasks, configured.Tasks)
	baseline.Subagents = merge(baseline.Subagents, configured.Subagents)
	baseline.SubagentTranscripts = merge(baseline.SubagentTranscripts, configured.SubagentTranscripts)
	baseline.Questions = merge(baseline.Questions, configured.Questions)
	baseline.DynamicTools = merge(baseline.DynamicTools, configured.DynamicTools)
	return baseline
}

// MarshalJSON keeps the wire DTO complete while retaining source compatibility
// for adapters that have not yet needed any new matrix dimension.
func (d HarnessDescriptor) MarshalJSON() ([]byte, error) {
	type descriptorAlias HarnessDescriptor
	copy := descriptorAlias(d)
	copy.Interop = d.Interoperability()
	return json.Marshal(copy)
}

// Supports is the one operation gate used by core and UI mappers.
func (d HarnessDescriptor) Supports(kind OperationKind) bool {
	for _, operation := range d.Operations {
		if operation.Kind == kind {
			return true
		}
	}
	return false
}

// Operations is a concise constructor used only by adapters and fixtures.
func Operations(kinds ...OperationKind) []OperationDescriptor {
	out := make([]OperationDescriptor, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, OperationDescriptor{Kind: kind})
	}
	return out
}

// SettingKey is a stable, typed key; native names such as ACP's "thinking"
// never leave the adapter.
type SettingKey string

const (
	SettingModel           SettingKey = "model"
	SettingReasoningEffort SettingKey = "reasoningEffort"
	SettingPermissionMode  SettingKey = "permissionMode"
	SettingSandboxMode     SettingKey = "sandboxMode"
)

type SettingChoice struct {
	Value       string `json:"value"`
	DisplayName string `json:"displayName"`
}

type SettingDependency struct {
	Key    SettingKey `json:"key"`
	Equals string     `json:"equals"`
}

// SettingDescriptor is a picker contract. EffectiveValue is optional because
// a catalogue can describe a fresh session before an agent exists.
type SettingDescriptor struct {
	Key            SettingKey          `json:"key"`
	DisplayName    string              `json:"displayName"`
	Choices        []SettingChoice     `json:"choices"`
	Dependencies   []SettingDependency `json:"dependencies,omitempty"`
	DefaultValue   string              `json:"defaultValue,omitempty"`
	EffectiveValue string              `json:"effectiveValue,omitempty"`
	Timing         ApplicationTiming   `json:"timing"`
}

type ModelDescriptor struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	// Role is an optional, deliberately small product-facing grouping.  It
	// never substitutes for ID or DisplayName: adapters set it only when the
	// live native catalogue itself identifies a known role family.
	Role                      ModelRole         `json:"role,omitempty"`
	SupportedReasoningEfforts []string          `json:"supportedReasoningEfforts,omitempty"`
	Metadata                  map[string]string `json:"metadata,omitempty"`
}

// CatalogueScope contains only identities. Profiles, credentials and native
// client configuration are resolved inside the core/adapters, never supplied
// by a UI catalogue request.
type CatalogueScope struct {
	HarnessID  string `json:"harnessId,omitempty"`
	ProviderID string `json:"providerId,omitempty"`
}

type CatalogueSnapshot struct {
	ContractVersion int                 `json:"contractVersion"`
	HarnessID       string              `json:"harnessId"`
	ProviderID      string              `json:"providerId,omitempty"`
	Revision        string              `json:"revision"`
	Models          []ModelDescriptor   `json:"models"`
	Settings        []SettingDescriptor `json:"settings"`
}

// CatalogueRevision derives a stable content revision without exposing any
// native protocol state. Adapters call it after their mapping is complete.
func CatalogueRevision(snapshot CatalogueSnapshot) string {
	copy := snapshot
	copy.Revision = ""
	b, err := json.Marshal(copy)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("v%d-%x", ContractVersion, sum[:8])
}

// ValidateDescriptor and ValidateCatalogue are deliberately usable by adapter
// tests and conformance fixtures. They reject malformed neutral contracts
// before an invalid native value has a chance to reach the UI.
func ValidateDescriptor(descriptor HarnessDescriptor) error {
	if descriptor.ContractVersion != ContractVersion {
		return fmt.Errorf("unsupported harness contract version %d", descriptor.ContractVersion)
	}
	if descriptor.ID == "" || descriptor.DisplayName == "" {
		return fmt.Errorf("harness descriptor needs id and display name")
	}
	seen := map[OperationKind]bool{}
	for _, operation := range descriptor.Operations {
		if operation.Kind == "" {
			return fmt.Errorf("harness descriptor has an empty operation kind")
		}
		if seen[operation.Kind] {
			return fmt.Errorf("harness descriptor repeats operation %q", operation.Kind)
		}
		seen[operation.Kind] = true
	}
	derived := InteropFromOperations(descriptor.Operations)
	// The older operation gates are still used by existing core paths.  Do not
	// permit the parity DTO to contradict one of those gates, or different UI
	// surfaces could make opposite claims about the same native behavior.
	for name, declared := range map[string]struct {
		operation InteropSupport
		matrix    InteropSupport
	}{
		"commands":            {derived.Commands, descriptor.Interop.Commands},
		"compaction.inPlace":  {derived.Compaction.InPlace, descriptor.Interop.Compaction.InPlace},
		"compaction.cold":     {derived.Compaction.Cold, descriptor.Interop.Compaction.Cold},
		"subagentTranscripts": {derived.SubagentTranscripts, descriptor.Interop.SubagentTranscripts},
	} {
		if declared.operation == InteropNative && declared.matrix != "" && declared.matrix != InteropNative {
			return fmt.Errorf("harness descriptor interop %s contradicts its operation gate", name)
		}
	}
	matrix := descriptor.Interoperability()
	for name, support := range map[string]InteropSupport{
		"commands": matrix.Commands, "compaction.inPlace": matrix.Compaction.InPlace,
		"compaction.cold": matrix.Compaction.Cold, "continuation": matrix.Continuation,
		"plans": matrix.Plans, "tasks": matrix.Tasks, "subagents": matrix.Subagents,
		"subagentTranscripts": matrix.SubagentTranscripts, "questions": matrix.Questions,
		"dynamicTools": matrix.DynamicTools,
	} {
		if support != InteropUnsupported && support != InteropNative && support != InteropManaged {
			return fmt.Errorf("harness descriptor has invalid interop support %q for %s", support, name)
		}
	}
	return nil
}

func ValidateCatalogue(snapshot CatalogueSnapshot) error {
	if snapshot.ContractVersion != ContractVersion || snapshot.HarnessID == "" || snapshot.Revision == "" {
		return fmt.Errorf("catalogue needs contract version, harness id and revision")
	}
	models := map[string]bool{}
	for _, model := range snapshot.Models {
		if model.ID == "" || model.DisplayName == "" || models[model.ID] {
			return fmt.Errorf("catalogue has an invalid or duplicate model")
		}
		if model.Role != "" && !model.Role.Valid() {
			return fmt.Errorf("catalogue has invalid model role %q", model.Role)
		}
		models[model.ID] = true
	}
	settings := map[SettingKey]bool{}
	for _, setting := range snapshot.Settings {
		if setting.Key == "" || setting.DisplayName == "" || setting.Timing == "" || settings[setting.Key] {
			return fmt.Errorf("catalogue has an invalid or duplicate setting")
		}
		settings[setting.Key] = true
	}
	return nil
}

// AgentSettings separates requested settings from each adapter's effective
// state. It intentionally contains no native configuration blob.
type AgentSettings struct {
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	PermissionMode  string `json:"permissionMode,omitempty"`
	SandboxMode     string `json:"sandboxMode,omitempty"`
}

type AgentRef struct {
	ThreadID        string `json:"threadId"`
	HarnessID       string `json:"harnessId"`
	ProviderID      string `json:"providerId,omitempty"`
	NativeSessionID string `json:"nativeSessionId,omitempty"`
}

type AgentSnapshot struct {
	Ref       AgentRef      `json:"ref"`
	Requested AgentSettings `json:"requested"`
	Effective AgentSettings `json:"effective"`
}

type RejectedSetting struct {
	Key    SettingKey `json:"key"`
	Value  string     `json:"value"`
	Reason string     `json:"reason"`
}

type AppliedSettings struct {
	Requested AgentSettings     `json:"requested"`
	Effective AgentSettings     `json:"effective"`
	Timing    ApplicationTiming `json:"timing"`
	Rejected  []RejectedSetting `json:"rejected,omitempty"`
}

// HealthState is a traffic light, deliberately coarse — the detail lives in
// Checks. Unknown is a real state: a probe that timed out has not said "bad".
const (
	HealthOK      = "ok"
	HealthWarn    = "warn"    // usable, something is off
	HealthBad     = "bad"     // will not start
	HealthUnknown = "unknown" // the probe could not answer
)

// healthRank orders the states for the worst-of roll-up. A warn outranks an
// unknown: "something IS off" beats "one probe did not answer", and an engine
// whose only blemish is a hung doctor must not paint itself amber.
var healthRank = map[string]int{
	HealthOK:      0,
	HealthUnknown: 1,
	HealthWarn:    2,
	HealthBad:     3,
}

// WorstState rolls a check list up into the engine's overall state — THE
// roll-up, shared by every adapter so no engine can rank the lights its own
// way. No checks at all is Unknown (an empty card must not claim health), and
// an unrecognised state string is treated as Unknown rather than trusted.
func WorstState(checks []Check) string {
	worst := HealthUnknown
	for i, c := range checks {
		state := c.State
		if _, known := healthRank[state]; !known {
			state = HealthUnknown
		}
		if i == 0 || healthRank[state] > healthRank[worst] {
			worst = state
		}
	}
	return worst
}

// Check is one named health assertion, with a remedy the UI can act on.
type Check struct {
	Name   string `json:"name"` // "binary", "config", "auth", "models", …
	State  string `json:"state"`
	Detail string `json:"detail"`
	// Remedy is a command the user can run, verbatim, e.g. "kimi login".
	// Taken from the engine where it advertises one (kimi's authMethods
	// _meta.terminal-auth) — never invented by us.
	Remedy string `json:"remedy,omitempty"`
}

// Health is one engine's preflight verdict — the answer to "will an agent
// started on this engine actually come up?", asked before any thread exists.
type Health struct {
	EngineID string  `json:"engineId"`
	State    string  `json:"state"` // worst of Checks (WorstState)
	Version  string  `json:"version"`
	Checks   []Check `json:"checks"`
	// Models is the discovered catalogue size, so the card can say
	// "4 models" without a second round trip.
	Models int `json:"models"`
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

// StartSpec is an adapter-private runtime binding. It is deliberately not a
// JSON DTO: it carries bridge secrets, environment overlays, and native
// provider data that must never cross the Harness Linkage boundary. The core
// passes it alongside AgentLaunch only after resolving those private values.
type StartSpec struct {
	ThreadID    string
	WorkDir     string // the worktree (also the MCP bridge's workspace)
	Prompt      string // opening message; empty on resume
	Attachments []agent.Attachment

	Model          string // harness vocabulary (tier token or CLI model id)
	Effort         string // harness vocabulary (claude --effort / kimi thinking)
	PermissionMode string // harness vocabulary (claude mode / kimi mode)
	SandboxMode    string // harness vocabulary; currently Codex only

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
	// the CLI's own system prompt. Gated by the SystemPrompt operation: a
	// harness without the channel reports it unapplied rather than emulating
	// it (an emulated persona would silently outrank the CLI's own prompt).
	SystemPrompt string
	// Agents are custom subagent definitions the thread's agent may delegate
	// to. Gated by CustomSubagents; reported per profile in
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
	//                    --strict-mcp-config). Gated by StrictMCPConfig.
	//   MaxBudgetUSD     a hard spend ceiling for the session, enforced by the
	//                    CLI (claude --max-budget-usd). 0 = uncapped. Gated by
	//                    CostBudget.
	//   Title            the thread's human title, passed so the session is
	//                    identifiable in the CLI's own session listings (claude
	//                    --name). Cosmetic, so it is NOT capability-gated: an
	//                    adapter with no equivalent simply ignores it.
	StrictMCPConfig bool
	MaxBudgetUSD    float64
	Title           string
}

// AgentLaunch is the serializable, neutral launch DTO. The core turns this
// into its private StartSpec only after resolving profiles and creating native
// runtime bindings (credentials, environment overlays and bridge secrets).
type AgentLaunch struct {
	Ref         AgentRef           `json:"ref"`
	WorkDir     string             `json:"workDir"`
	Prompt      string             `json:"prompt"`
	Attachments []agent.Attachment `json:"attachments,omitempty"`
	Settings    AgentSettings      `json:"settings"`
	Resume      bool               `json:"resume,omitempty"`
	ForkSession bool               `json:"forkSession,omitempty"`
	Cowork      bool               `json:"cowork,omitempty"`
	Title       string             `json:"title,omitempty"`
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
// Only ever called on harnesses whose descriptor supports Compaction, and
// the cold shape additionally requires ColdCompaction — a harness
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
	// Descriptor is stable, neutral metadata for this harness.
	Descriptor() HarnessDescriptor
	// Catalogue returns one revisioned model/settings snapshot. Implementations
	// accept identities only; credentials and native configuration stay private.
	Catalogue(ctx context.Context, scope CatalogueScope) (CatalogueSnapshot, error)

	// Launch starts (or resumes, or forks — per spec) one agent thread.
	Launch(launch AgentLaunch, runtime StartSpec) (Launched, error)

	Send(threadID, text string, atts []agent.Attachment) error
	Interrupt(threadID string) error
	Stop(threadID string) error
	Running(threadID string) bool
	StopAll()

	// ReadTranscript returns the thread's replayable event history. sessionID
	// is the record's session id — harnesses whose transcript lives with the
	// CLI (claude) need it; harnesses that keep their own event log ignore it.
	ReadTranscript(threadID, sessionID string) ([]json.RawMessage, error)

	// UpdateSettings applies a typed requested state and returns the state the
	// native harness actually accepted, including its timing and explicit
	// rejections. This replaces the stringly per-option mutation API.
	UpdateSettings(ctx context.Context, ref AgentRef, requested AgentSettings) (AppliedSettings, error)

	// Health answers the engine-level (thread-less) preflight question: is
	// this engine's CLI present, configured, authenticated, and does it have
	// a model catalogue? Deliberately NOT capability-gated — every harness
	// can answer, even if the answer is all-Unknown; a flag would let an
	// adapter opt out of being diagnosable, which is the opposite of the
	// point. Implementations are BEST-EFFORT with per-check timeouts: a hung
	// doctor yields an Unknown check, never an error that blanks the card.
	// The error return is for genuinely unexpected failure only.
	Health(ctx context.Context) (Health, error)

	// BrowseSessions returns this harness's discoverable past sessions. Only
	// called for harnesses whose descriptor advertises OperationSessionBrowse.
	BrowseSessions() ([]BrowsableSession, error)

	// Compact runs one compaction pass and returns the summary body. Gated by
	// OperationCompaction (and OperationColdCompaction for a cold
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
func Unsupported(feature string, descriptor HarnessDescriptor) error {
	return fmt.Errorf("%s is not supported by %s agents", feature, descriptor.DisplayName)
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

// Register adds a harness under its Descriptor().ID. Registration order is
// preserved (it is the order pickers list engines in).
func (r *Registry) Register(h Harness) {
	id := h.Descriptor().ID
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
