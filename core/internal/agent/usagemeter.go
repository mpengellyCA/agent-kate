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
type usageMeter struct {
	log      *slog.Logger
	threadID string

	mu       sync.Mutex
	model    string
	byMsgID  map[string]usageBlock // latest usage we've seen per assistant message id
	order    []string              // insertion order, so per-turn logs stay chronological
	logged   map[string]bool       // ids we have already emitted a turn_usage line for
}

func newUsageMeter(log *slog.Logger, threadID string) *usageMeter {
	return &usageMeter{
		log:      log,
		threadID: threadID,
		byMsgID:  make(map[string]usageBlock),
		logged:   make(map[string]bool),
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
	// Stream events without an id (very old Claude Code, defensive) get a
	// synthetic key so they still appear in the totals, just not deduped.
	key := id
	if key == "" {
		key = fmt.Sprintf("anon-%d", len(m.byMsgID))
	}

	m.mu.Lock()
	if model != "" {
		m.model = model
	}
	_, known := m.byMsgID[key]
	if !known {
		m.order = append(m.order, key)
	}
	// Keep the latest usage for this id — partial stream events get
	// superseded by the final one.
	m.byMsgID[key] = u
	shouldLog := !m.logged[key]
	m.logged[key] = true
	turn := len(m.order)
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

// Summary logs the per-thread roll-up by walking the deduped per-message map,
// so partial stream events that emit the same usage block multiple times do
// not inflate the totals. Called at thread exit; the figures should agree
// with the sum of claude's `session_usage` events for the same thread.
func (m *usageMeter) Summary() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.byMsgID) == 0 {
		return
	}
	var totalIn, totalOut, cacheRead, cacheCreate int64
	for _, u := range m.byMsgID {
		totalIn += u.InputTokens
		totalOut += u.OutputTokens
		cacheRead += u.CacheReadInputTokens
		cacheCreate += u.CacheCreationInputTokens
	}
	prompt := totalIn + cacheRead + cacheCreate
	m.log.Info("usage_total",
		"thread", m.threadID,
		"model", m.model,
		"turns", len(m.byMsgID),
		"in", totalIn,
		"out", totalOut,
		"cache_read", cacheRead,
		"cache_create", cacheCreate,
		"prompt_total", prompt,
		"cache_hit_pct", percent(cacheRead, prompt),
	)
}

func percent(num, denom int64) int {
	if denom <= 0 {
		return 0
	}
	return int((num * 100) / denom)
}
