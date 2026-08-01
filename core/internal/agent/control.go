package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"agentkate/internal/safe"
)

// This file holds the read-only / mid-session half of Claude Code's
// stream-json control channel. Probed against claude 2.1.220: the CLI accepts
// control_request lines on the ALREADY-OPEN stdin of a running print-mode
// session and answers each with a control_response, and get_context_usage,
// get_usage, get_session_cost, list_models, set_max_thinking_tokens and
// reload_skills all return success. The write/response-matching machinery is
// sendControlResult in agent.go; everything here is a caller of it.

// effortThinkingTokens maps Agent Kate's effort vocabulary — the same tokens
// buildStartArgs passes to `claude --effort` — onto the thinking-token budgets
// set_max_thinking_tokens takes. The values are Claude Code's own think-keyword
// budgets ("think" 4k, "think hard" 10k, "think harder" ~32k, "ultrathink"
// ~64k), which is what --effort resolves to internally, so a mid-session change
// lands the thread on the same budget a relaunch at that effort would have.
var effortThinkingTokens = map[string]int{
	"low":    4000,
	"medium": 10000,
	"high":   31999,
	"xhigh":  63999,
	"max":    127999,
}

// EffortThinkingTokens reports the thinking-token budget for one effort tier,
// and whether the tier is one we know how to express.
func EffortThinkingTokens(effort string) (int, bool) {
	n, ok := effortThinkingTokens[strings.ToLower(strings.TrimSpace(effort))]
	return n, ok
}

// SetEffort changes a running thread's reasoning budget mid-session via
// set_max_thinking_tokens. `claude` has no set_effort control request, but the
// effort tiers are thinking-token budgets underneath, so this is the same lever
// under its real name — no relaunch, and the change applies from the next turn.
func (s *Supervisor) SetEffort(threadID, effort string) error {
	tokens, ok := EffortThinkingTokens(effort)
	if !ok {
		return fmt.Errorf("unknown thinking effort %q", effort)
	}
	return s.sendControl(threadID, "set_max_thinking_tokens",
		map[string]any{"max_thinking_tokens": tokens})
}

// ReloadSkills makes a running thread re-read the skills directories, so a
// skill installed into its worktree after launch becomes callable without a
// relaunch.
func (s *Supervisor) ReloadSkills(threadID string) error {
	return s.sendControl(threadID, "reload_skills", nil)
}

// ContextUsage asks a running thread what its context actually holds. This is
// the authoritative number: the UI's fallback estimate is derived from the last
// result event's prompt-side token sums, which counts what was SENT, not what
// the CLI is carrying (system prompt, tool schemas, memory files and the
// autocompact buffer are all in the window and in none of the usage fields).
func (s *Supervisor) ContextUsage(threadID string) (ContextUsage, error) {
	payload, err := s.sendControlResult(threadID, "get_context_usage", nil)
	if err != nil {
		return ContextUsage{}, err
	}
	cu, ok := parseContextUsage(payload)
	if !ok {
		return ContextUsage{}, fmt.Errorf("get_context_usage: unrecognised response shape")
	}
	return cu, nil
}

// ContextUsage is one context-fill reading, normalised out of whatever shape
// the CLI answered with.
type ContextUsage struct {
	UsedTokens int64
	MaxTokens  int64
	// Breakdown is the per-category split (system prompt, tools, messages, …),
	// in descending token order. Empty when the CLI reported no categories.
	Breakdown []ContextCategory
}

// ContextCategory is one line of a context-usage breakdown.
type ContextCategory struct {
	Label  string `json:"label"`
	Tokens int64  `json:"tokens"`
}

// JSON renders a reading into the `_context` synthetic event's payload.
func (c ContextUsage) JSON() map[string]any {
	cats := make([]map[string]any, 0, len(c.Breakdown))
	for _, b := range c.Breakdown {
		cats = append(cats, map[string]any{"label": b.Label, "tokens": b.Tokens})
	}
	return map[string]any{
		"type":       "_context",
		"usedTokens": c.UsedTokens,
		"maxTokens":  c.MaxTokens,
		"breakdown":  cats,
	}
}

// contextUsageWire is the get_context_usage answer in the shape claude 2.1.220
// actually sends, captured live off the control channel:
//
//	{"subtype":"success","request_id":"…","response":{
//	   "categories":[{"name":"System prompt","tokens":3231}, …],
//	   "totalTokens":18074,"maxTokens":1000000}}
//
// sendControlResult hands us the OUTER response object, so the figures sit one
// level further down under "response" — which is why the earlier alias-probing
// parser never matched anything and the meter silently never fired.
type contextUsageWire struct {
	TotalTokens int64 `json:"totalTokens"`
	MaxTokens   int64 `json:"maxTokens"`
	Categories  []struct {
		Name   string `json:"name"`
		Tokens int64  `json:"tokens"`
	} `json:"categories"`
}

// usedKeys / maxKeys / nestKeys / breakdownKeys are the field spellings seen
// across CLI versions, used only by the loose fallback below. The control
// channel is undocumented and its payload has changed shape before, so a
// payload that does not match the captured shape is still normalised by probing
// a small set of aliases — an unrecognised payload reports not-ok and the UI
// keeps its estimate, which is strictly better than a meter that reads zero.
var (
	usedKeys      = []string{"used_tokens", "usedTokens", "total_tokens", "totalTokens", "used", "input_tokens"}
	maxKeys       = []string{"max_tokens", "maxTokens", "context_window", "contextWindow", "window", "limit", "total"}
	nestKeys      = []string{"response", "context_usage", "contextUsage", "usage", "context", "result"}
	breakdownKeys = []string{"breakdown", "categories", "components", "by_category", "byCategory", "sections"}
)

// parseContextUsage normalises a get_context_usage response: the captured
// 2.1.220 shape first, then the loose alias probe for anything else.
func parseContextUsage(payload json.RawMessage) (ContextUsage, bool) {
	if cu, ok := parseContextUsageWire(payload); ok {
		return cu, true
	}
	return parseContextUsageLoose(payload)
}

// parseContextUsageWire reads the captured nested shape exactly.
func parseContextUsageWire(payload json.RawMessage) (ContextUsage, bool) {
	var env struct {
		Response contextUsageWire `json:"response"`
	}
	if json.Unmarshal(payload, &env) != nil || env.Response.TotalTokens <= 0 {
		return ContextUsage{}, false
	}
	cu := ContextUsage{
		UsedTokens: env.Response.TotalTokens,
		MaxTokens:  env.Response.MaxTokens,
	}
	for _, c := range env.Response.Categories {
		if c.Name == "" {
			continue
		}
		cu.Breakdown = append(cu.Breakdown, ContextCategory{Label: prettyLabel(c.Name), Tokens: c.Tokens})
	}
	sort.SliceStable(cu.Breakdown, func(i, j int) bool {
		return cu.Breakdown[i].Tokens > cu.Breakdown[j].Tokens
	})
	return cu, true
}

// parseContextUsageLoose is the version-tolerant fallback.
func parseContextUsageLoose(payload json.RawMessage) (ContextUsage, bool) {
	var root map[string]any
	if json.Unmarshal(payload, &root) != nil {
		return ContextUsage{}, false
	}
	// Descend at most one level into a wrapper object before giving up: the
	// response either carries the figures directly or nests them under one of
	// the names above.
	obj := root
	if _, ok := firstNumber(obj, usedKeys); !ok {
		for _, k := range nestKeys {
			if nested, ok := obj[k].(map[string]any); ok {
				if _, found := firstNumber(nested, usedKeys); found {
					obj = nested
					break
				}
			}
		}
	}
	used, ok := firstNumber(obj, usedKeys)
	if !ok {
		return ContextUsage{}, false
	}
	// The window is optional — a payload that reports only what has been used
	// still drives a useful readout, and the UI falls back to the model's own
	// limit. Used tokens are not optional: a reading of zero says nothing.
	window, _ := firstNumber(obj, maxKeys)
	cu := ContextUsage{UsedTokens: used, MaxTokens: window, Breakdown: parseBreakdown(obj)}
	if cu.UsedTokens <= 0 {
		return ContextUsage{}, false
	}
	return cu, true
}

// firstNumber returns the first of keys present on obj as a whole number.
func firstNumber(obj map[string]any, keys []string) (int64, bool) {
	for _, k := range keys {
		if f, ok := obj[k].(float64); ok {
			return int64(f), true
		}
	}
	return 0, false
}

// parseBreakdown pulls a per-category split out of whichever container the
// payload used: a map of label -> tokens, a map of label -> object, or an
// array of objects. Categories are returned largest-first.
func parseBreakdown(obj map[string]any) []ContextCategory {
	var raw any
	for _, k := range breakdownKeys {
		if v, ok := obj[k]; ok {
			raw = v
			break
		}
	}
	var out []ContextCategory
	switch v := raw.(type) {
	case map[string]any:
		for label, val := range v {
			if n, ok := categoryTokens(val); ok {
				out = append(out, ContextCategory{Label: prettyLabel(label), Tokens: n})
			}
		}
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			label := ""
			for _, k := range []string{"label", "name", "category", "id"} {
				if s, ok := m[k].(string); ok && s != "" {
					label = s
					break
				}
			}
			n, ok := categoryTokens(m)
			if label == "" || !ok {
				continue
			}
			out = append(out, ContextCategory{Label: prettyLabel(label), Tokens: n})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Tokens > out[j].Tokens })
	return out
}

// categoryTokens reads one breakdown entry's token count, whether the entry is
// a bare number or an object carrying one.
func categoryTokens(val any) (int64, bool) {
	switch v := val.(type) {
	case float64:
		return int64(v), true
	case map[string]any:
		return firstNumber(v, []string{"tokens", "token_count", "tokenCount", "used_tokens", "usedTokens", "value", "count"})
	}
	return 0, false
}

// prettyLabel turns a wire label ("system_prompt", "systemTools") into
// something a tooltip can show. Purely cosmetic; the value is never matched on.
func prettyLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	// 2.1.220 already sends prose labels ("System prompt", "MCP tools"); leave
	// those alone — splitting on their inner capitals would give "M c p tools".
	if strings.Contains(s, " ") {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r == '_' || r == '-':
			b.WriteByte(' ')
		case r >= 'A' && r <= 'Z' && i > 0:
			b.WriteByte(' ')
			b.WriteRune(r + 32)
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	return strings.ToUpper(out[:1]) + out[1:]
}

// reportContextUsage probes a thread's real context fill and emits it as the
// synthetic `_context` event. Called on each completed turn, off the stdout
// pump goroutine: sendControlResult blocks until the pump reads the matching
// control_response, so running it inline would deadlock the thread.
//
// Best-effort throughout — a failed probe emits nothing, and the panel keeps
// the estimate it already had.
func (s *Supervisor) reportContextUsage(t *Thread) {
	t.mu.Lock()
	if !t.alive || t.stopping || t.ctxProbePending {
		t.mu.Unlock()
		return
	}
	t.ctxProbePending = true
	t.mu.Unlock()

	safe.Go("agent.contextUsage", func() {
		defer func() {
			t.mu.Lock()
			t.ctxProbePending = false
			t.mu.Unlock()
		}()
		cu, err := s.ContextUsage(t.ID)
		if err != nil {
			s.log.Debug("context usage probe failed", "thread", t.ID, "err", err)
			return
		}
		s.emitObj(t, cu.JSON())
	})
}

// --- list_models ------------------------------------------------------------

// listModels asks one running thread for the CLI's live model vocabulary.
func (s *Supervisor) listModels(threadID string) ([]ClaudeModel, error) {
	payload, err := s.sendControlResult(threadID, "list_models", nil)
	if err != nil {
		return nil, err
	}
	models := parseModelList(payload)
	if len(models) == 0 {
		return nil, fmt.Errorf("list_models: no models in response")
	}
	return models, nil
}

// anyRunningThread returns the id of some live, non-stopping thread, or "".
// Reusing a thread the human already has open makes model discovery free: no
// second CLI process, no second auth handshake.
func (s *Supervisor) anyRunningThread() string {
	s.mu.Lock()
	threads := make([]*Thread, 0, len(s.threads))
	for _, t := range s.threads {
		threads = append(threads, t)
	}
	s.mu.Unlock()
	for _, t := range threads {
		t.mu.Lock()
		usable := t.alive && !t.stopping
		t.mu.Unlock()
		if usable {
			return t.ID
		}
	}
	return ""
}

// modelListWire is the list_models answer in the shape claude 2.1.220 actually
// sends, captured live off the control channel:
//
//	{"subtype":"success","request_id":"…","response":{"models":[
//	  {"value":"opus[1m]","displayName":"Opus (1M context)",
//	   "supportsEffort":true,"supportedEffortLevels":["low", …]}, …]}}
//
// Same peel as get_context_usage: the models array lives under
// response.response, not at the root of the payload we are handed.
type modelListWire struct {
	Models []struct {
		Value                 string   `json:"value"`
		DisplayName           string   `json:"displayName"`
		SupportedEffortLevels []string `json:"supportedEffortLevels"`
	} `json:"models"`
}

// parseModelList normalises a list_models response: the captured 2.1.220 shape
// first, then a version-tolerant probe. Alongside each model the CLI reports
// which reasoning-effort tiers it supports, which the effort picker uses to
// grey out the ones a model cannot run.
func parseModelList(payload json.RawMessage) []ClaudeModel {
	if models := parseModelListWire(payload); len(models) > 0 {
		return models
	}
	return parseModelListLoose(payload)
}

// parseModelListWire reads the captured nested shape exactly. supportsEffort is
// deliberately not consulted: an empty Efforts slice already means "the CLI
// made no claim", which callers read as every tier allowed, so a false there
// has no distinct representation and offering tiers a model ignores is
// harmless where withholding them would not be.
func parseModelListWire(payload json.RawMessage) []ClaudeModel {
	var env struct {
		Response modelListWire `json:"response"`
	}
	if json.Unmarshal(payload, &env) != nil {
		return nil
	}
	out := make([]ClaudeModel, 0, len(env.Response.Models))
	seen := map[string]bool{}
	for _, m := range env.Response.Models {
		if m.Value == "" || seen[m.Value] {
			continue
		}
		seen[m.Value] = true
		name := m.DisplayName
		if name == "" || name == m.Value {
			name = prettyModelAlias(m.Value)
		}
		var efforts []string
		for _, e := range m.SupportedEffortLevels {
			if e = strings.TrimSpace(e); e != "" {
				efforts = append(efforts, e)
			}
		}
		out = append(out, ClaudeModel{Value: m.Value, Name: name, Efforts: efforts})
	}
	return out
}

// parseModelListLoose is the version-tolerant fallback.
func parseModelListLoose(payload json.RawMessage) []ClaudeModel {
	var root map[string]any
	if json.Unmarshal(payload, &root) != nil {
		return nil
	}
	// Peel one control-envelope level before probing, so a nested payload whose
	// entries use older spellings is still readable.
	if inner, ok := root["response"].(map[string]any); ok {
		root = inner
	}
	var items []any
	for _, k := range []string{"models", "data", "result", "list"} {
		if arr, ok := root[k].([]any); ok {
			items = arr
			break
		}
		// One level of nesting, same reasoning as parseContextUsage.
		if nested, ok := root[k].(map[string]any); ok {
			for _, k2 := range []string{"models", "data", "list"} {
				if arr, ok := nested[k2].([]any); ok {
					items = arr
					break
				}
			}
		}
		if items != nil {
			break
		}
	}
	out := make([]ClaudeModel, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			// A bare string list is still a model vocabulary.
			if s, ok := item.(string); ok && s != "" && !seen[s] {
				seen[s] = true
				out = append(out, ClaudeModel{Value: s, Name: prettyModelAlias(s)})
			}
			continue
		}
		value := ""
		for _, k := range []string{"value", "id", "model", "alias", "name"} {
			if s, ok := m[k].(string); ok && s != "" {
				value = s
				break
			}
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		name := ""
		for _, k := range []string{"display_name", "displayName", "name", "label"} {
			if s, ok := m[k].(string); ok && s != "" && s != value {
				name = s
				break
			}
		}
		if name == "" {
			name = prettyModelAlias(value)
		}
		out = append(out, ClaudeModel{Value: value, Name: name, Efforts: parseEfforts(m)})
	}
	return out
}

// parseEfforts reads a model entry's supported reasoning-effort tiers. An empty
// result means the CLI said nothing about efforts for this model, which the UI
// must read as "all tiers allowed" — never as "none".
func parseEfforts(m map[string]any) []string {
	var raw any
	for _, k := range []string{"supportedEffortLevels", "supported_effort_levels", "supported_efforts", "supportedEfforts", "efforts", "effort_levels", "effortLevels", "reasoning_efforts"} {
		if v, ok := m[k]; ok {
			raw = v
			break
		}
	}
	var out []string
	switch v := raw.(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case map[string]any:
		// A support map: {"low": true, "max": false}.
		for k, val := range v {
			if b, ok := val.(bool); ok && b {
				out = append(out, k)
			}
		}
		sort.Strings(out)
	}
	return out
}

// probeTimeout bounds the throwaway discovery session end to end.
const probeTimeout = 20 * time.Second

// listModelsProbe spawns a short-lived stream-json session purely to answer one
// list_models control request, then closes stdin so the CLI exits. No turn is
// started, so nothing is billed.
//
// This replaced regex-parsing the `claude -p /model` prose: the control channel
// answers with structured data including per-model effort support, which the
// prose never carried. parseClaudeModelList stays as the fallback for a CLI too
// old to know the subtype.
func (s *Supervisor) listModelsProbe(ctx context.Context) ([]ClaudeModel, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, s.claudeBin,
		"--print", "--output-format", "stream-json",
		"--input-format", "stream-json", "--verbose")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	reqID := "ak-list_models-" + NewThreadID()
	frame, err := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": reqID,
		"request":    map[string]any{"subtype": "list_models"},
	})
	if err != nil {
		return nil, err
	}
	if _, err := stdin.Write(append(frame, '\n')); err != nil {
		return nil, err
	}

	type answer struct {
		models []ClaudeModel
		err    error
	}
	done := make(chan answer, 1)
	safe.Go("agent.listModelsProbe", func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var ev struct {
				Type     string `json:"type"`
				Response struct {
					RequestID string `json:"request_id"`
					Subtype   string `json:"subtype"`
					Error     string `json:"error"`
				} `json:"response"`
			}
			line := sc.Bytes()
			if json.Unmarshal(line, &ev) != nil || ev.Type != "control_response" {
				continue
			}
			if ev.Response.RequestID != reqID {
				continue
			}
			if ev.Response.Subtype == "error" {
				done <- answer{err: fmt.Errorf("list_models: %s", ev.Response.Error)}
				return
			}
			var wrap struct {
				Response json.RawMessage `json:"response"`
			}
			_ = json.Unmarshal(append([]byte(nil), line...), &wrap)
			done <- answer{models: parseModelList(wrap.Response)}
			return
		}
		done <- answer{err: fmt.Errorf("list_models: session ended without a response")}
	})

	select {
	case a := <-done:
		if a.err != nil {
			return nil, a.err
		}
		if len(a.models) == 0 {
			return nil, fmt.Errorf("list_models: no models in response")
		}
		return a.models, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
