package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
)

// usageMeter records the token-usage figures Claude Code reports for each
// assistant turn on a single agent thread, so we can see what Agent Kate is
// actually being billed for (separate from what fills the context — that is
// the toolMeter's job). Cache-hit ratio gets its own field because a low
// ratio is itself a kind of waste: the same context being recomputed each
// turn instead of read from cache.
//
// Claude Code's `--verbose` stream emits the same assistant message multiple
// times as streaming progresses, each carrying its own usage block. We dedupe
// by `message.id` so the per-thread roll-up reflects one logical turn per id
// regardless of how many partial events we observed.
//
// Over a long-lived thread the per-message bookkeeping would grow without
// bound, so we fold each finalized turn into running cumulative totals and
// retain detail only for a bounded recent window of message ids. The window is
// sized to comfortably exceed the burst of partial stream events for a single
// logical turn (which arrive contiguously), so supersession — replacing a
// partial usage block with the final one for the same id — always lands while
// the id is still in-window. Once an id ages out it never reappears, so its
// contribution is already baked exactly into the running totals.
type usageMeter struct {
	log      *slog.Logger
	threadID string

	mu    sync.Mutex
	model string

	// Running cumulative totals across every unique turn observed, kept exact
	// regardless of window eviction. turns counts distinct message ids.
	turns                                     int
	totalIn, totalOut, cacheRead, cacheCreate int64

	// recent retains the latest usage seen per id for the bounded window only,
	// so partial stream events can supersede earlier ones. recentOrder is the
	// eviction queue (oldest first). seq is a monotonically increasing turn
	// counter used for the synthetic anon key and the chronological turn number
	// in per-turn logs, so neither depends on the (now bounded) map size.
	recent      map[string]usageBlock
	recentOrder []string
	seq         int
}

// usageWindow bounds how many distinct message ids retain per-turn detail. It
// only needs to exceed the number of partial stream events for one in-flight
// turn (a handful in practice); 256 leaves a wide safety margin while capping
// retained memory regardless of session length.
const usageWindow = 256

func newUsageMeter(log *slog.Logger, threadID string) *usageMeter {
	return &usageMeter{
		log:      log,
		threadID: threadID,
		recent:   make(map[string]usageBlock),
	}
}

// usageBlock is the subset of Claude Code's usage object we care about; the
// field names match the Anthropic Messages API verbatim.
type usageBlock struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Observe inspects one stream-json event from `claude` and records any usage
// data it carries. Assistant events report per-turn usage; the final result
// event reports the session total plus a billed cost.
func (m *usageMeter) Observe(event json.RawMessage) {
	var head struct {
		Type    string `json:"type"`
		Message struct {
			ID    string     `json:"id"`
			Model string     `json:"model"`
			Usage usageBlock `json:"usage"`
		} `json:"message"`
		Usage        usageBlock `json:"usage"`
		DurationMS   int64      `json:"duration_ms"`
		NumTurns     int        `json:"num_turns"`
		TotalCostUSD float64    `json:"total_cost_usd"`
	}
	if json.Unmarshal(event, &head) != nil {
		return
	}
	switch head.Type {
	case "assistant":
		m.recordTurn(head.Message.ID, head.Message.Model, head.Message.Usage)
	case "result":
		m.recordSession(head.Usage, head.NumTurns, head.TotalCostUSD, head.DurationMS)
	}
}

func (m *usageMeter) recordTurn(id, model string, u usageBlock) {
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadInputTokens == 0 && u.CacheCreationInputTokens == 0 {
		// Some assistant messages (e.g. the system init wrapper) carry no
		// usage; skip them so the log isn't littered with empty turns.
		return
	}
	m.mu.Lock()
	if model != "" {
		m.model = model
	}
	// Stream events without an id (very old Claude Code, defensive) get a
	// synthetic key so they still appear in the totals, just not deduped. The
	// key uses the monotonic seq rather than a map size so it stays unique even
	// after window eviction.
	key := id
	if key == "" {
		key = fmt.Sprintf("anon-%d", m.seq)
	}

	prev, known := m.recent[key]
	if known {
		// Supersede the partial usage we last saw for this id: adjust the
		// running totals by the delta so the final block per id is what counts.
		m.totalIn += u.InputTokens - prev.InputTokens
		m.totalOut += u.OutputTokens - prev.OutputTokens
		m.cacheRead += u.CacheReadInputTokens - prev.CacheReadInputTokens
		m.cacheCreate += u.CacheCreationInputTokens - prev.CacheCreationInputTokens
		m.recent[key] = u
	} else {
		// First sighting of this id: a new distinct turn.
		m.seq++
		m.turns++
		m.totalIn += u.InputTokens
		m.totalOut += u.OutputTokens
		m.cacheRead += u.CacheReadInputTokens
		m.cacheCreate += u.CacheCreationInputTokens
		m.recent[key] = u
		// Evict the oldest retained detail once the window is full. Its
		// contribution is already final in the running totals, and a superseded
		// id never reappears, so dropping the detail loses nothing. The shift
		// reuses the backing array in place so its capacity stays bounded by the
		// window rather than growing with the session.
		if len(m.recentOrder) >= usageWindow {
			delete(m.recent, m.recentOrder[0])
			copy(m.recentOrder, m.recentOrder[1:])
			m.recentOrder[len(m.recentOrder)-1] = key
		} else {
			m.recentOrder = append(m.recentOrder, key)
		}
	}
	// A first sighting that lands in the window is one we have not logged yet.
	shouldLog := !known
	turn := m.turns
	m.mu.Unlock()

	if !shouldLog {
		return
	}
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	m.log.Info("turn_usage",
		"thread", m.threadID,
		"turn", turn,
		"msg", id,
		"model", model,
		"in", u.InputTokens,
		"out", u.OutputTokens,
		"cache_read", u.CacheReadInputTokens,
		"cache_create", u.CacheCreationInputTokens,
		"prompt_total", prompt,
		"cache_hit_pct", percent(u.CacheReadInputTokens, prompt),
	)
}

func (m *usageMeter) recordSession(u usageBlock, turns int, cost float64, durMS int64) {
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	m.log.Info("session_usage",
		"thread", m.threadID,
		"num_turns", turns,
		"in", u.InputTokens,
		"out", u.OutputTokens,
		"cache_read", u.CacheReadInputTokens,
		"cache_create", u.CacheCreationInputTokens,
		"cache_hit_pct", percent(u.CacheReadInputTokens, prompt),
		"cost_usd", cost,
		"duration_ms", durMS,
	)
}

// Summary logs the per-thread roll-up from the running cumulative totals, which
// are kept deduped (one logical turn per message id, latest usage block wins)
// as turns are observed — so partial stream events that emit the same usage
// block multiple times do not inflate the figures, and the totals stay exact
// even after old per-message detail is evicted from the bounded window. Called
// at thread exit; the figures should agree with the sum of claude's
// `session_usage` events for the same thread.
func (m *usageMeter) Summary() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turns == 0 {
		return
	}
	prompt := m.totalIn + m.cacheRead + m.cacheCreate
	m.log.Info("usage_total",
		"thread", m.threadID,
		"model", m.model,
		"turns", m.turns,
		"in", m.totalIn,
		"out", m.totalOut,
		"cache_read", m.cacheRead,
		"cache_create", m.cacheCreate,
		"prompt_total", prompt,
		"cache_hit_pct", percent(m.cacheRead, prompt),
	)
}

func percent(num, denom int64) int {
	if denom <= 0 {
		return 0
	}
	return int((num * 100) / denom)
}
