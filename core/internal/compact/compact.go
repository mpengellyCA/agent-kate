// Package compact condenses an agent thread's Claude Code transcript so a
// resume can be seeded from a small summary instead of replaying the full
// prefix. The dominant cost on long agent threads is the prompt cache:
// re-caching a 180K-token prefix on every resume runs at $0.40–$0.60 per
// resume. Replacing that prefix with a 5K-token summary kills the cost
// entirely.
//
// Compaction comes in two flavours:
//
//   - Programmatic — pure-Go, free, deterministic, behaviourally lossless
//     (strips tool_result bodies but keeps every decision and tool call).
//   - LLM-driven — a separate claude invocation produces a richer natural
//     summary. Hot-Opus does it inline before reap while the KV cache is
//     warm; cold variants spawn a fresh claude --resume with --model X.
//
// A per-thread strategy on session.Record picks when and how compaction runs.
package compact

// Strategy names a per-thread compaction policy. The five values match the
// dropdown the user sees in the AgentPanel.
type Strategy string

const (
	// ExitOpusHot summarises the live session via a final inline turn before
	// the supervisor reaps it. Cheapest in dollars (cache_read instead of
	// input) but spends Opus quota; the recommended default.
	ExitOpusHot Strategy = "exit_opus_hot"

	// ExitSonnetCold spawns claude --resume --model claude-sonnet-* after
	// the agent exits. Pays cold-cache cost on Sonnet (small absolute amount)
	// but does not touch Opus quota — ideal for Max plans where Sonnet has
	// its own bucket.
	ExitSonnetCold Strategy = "exit_sonnet_cold"

	// ResumeHaikuCold defers compaction until the next resume and runs it
	// with Haiku. Cheapest cold compactor by absolute dollars.
	ResumeHaikuCold Strategy = "resume_haiku_cold"

	// ResumeSonnetCold defers compaction until the next resume and runs it
	// with Sonnet. Higher summary quality than Haiku.
	ResumeSonnetCold Strategy = "resume_sonnet_cold"

	// ResumeOpusCold runs a cold Opus compact on resume. Not in the default
	// dropdown but offered by the recovery dialog when an exit-compact was
	// missed and the user wants maximum quality regardless of cost.
	ResumeOpusCold Strategy = "resume_opus_cold"

	// ResumeLocal compacts programmatically with no LLM — free, behaviourally
	// lossless, deterministic. The fallback when no LLM quota is available.
	ResumeLocal Strategy = "resume_local"
)

// Default is the strategy a new thread inherits when none is set.
const Default = ExitOpusHot

// Valid reports whether s is a known strategy. Empty s is treated as Default
// elsewhere, so Valid("") returns false on purpose — callers should resolve
// emptiness via Resolve first.
func (s Strategy) Valid() bool {
	switch s {
	case ExitOpusHot, ExitSonnetCold,
		ResumeOpusCold, ResumeSonnetCold, ResumeHaikuCold, ResumeLocal:
		return true
	}
	return false
}

// Resolve returns s when set and valid, Default otherwise. Use this whenever
// reading a persisted CompactStrategy that may be an empty string from a
// thread predating the compaction feature.
func (s Strategy) Resolve() Strategy {
	if s.Valid() {
		return s
	}
	return Default
}

// RunsOnExit reports whether the strategy fires at supervisor reap time.
func (s Strategy) RunsOnExit() bool {
	switch s.Resolve() {
	case ExitOpusHot, ExitSonnetCold:
		return true
	}
	return false
}

// RunsOnResume reports whether the strategy fires when a thread is resumed.
func (s Strategy) RunsOnResume() bool {
	switch s.Resolve() {
	case ResumeOpusCold, ResumeHaikuCold, ResumeSonnetCold, ResumeLocal:
		return true
	}
	return false
}

// Model returns the claude --model id for LLM-based strategies, or "" for
// the programmatic strategy. Hot-Opus reuses the agent's running model and
// returns "" — callers must dispatch on the strategy, not on Model alone.
func (s Strategy) Model() string {
	switch s.Resolve() {
	case ExitOpusHot:
		// Hot path reuses the live process; the model arg is implicit.
		return ""
	case ResumeOpusCold:
		return "claude-opus-4-7"
	case ExitSonnetCold, ResumeSonnetCold:
		return "claude-sonnet-4-6"
	case ResumeHaikuCold:
		return "claude-haiku-4-5-20251001"
	case ResumeLocal:
		return ""
	}
	return ""
}
