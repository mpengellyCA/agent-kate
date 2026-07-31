package agent

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// TurnTracker mirrors every thread's turn state at the orchestration layer,
// backend-agnostically: the handlers mark a turn queued when they write a user
// message (agent.start's opening prompt, agent.send), and the event relay
// feeds every translated event back in via Observe — a `result` ends a turn,
// a terminal lifecycle phase ends the thread. Both supervisors already keep
// their own in-flight counts (claude turnsInFlight, kimi activePrompts), but
// those are private per backend; this tracker is the one place agent.wait can
// block for ANY harness.
//
// Wait blocks on a broadcast channel that is replaced on every state change —
// the Go idiom for a condition variable that also needs a timeout (sync.Cond
// cannot wait with a deadline). No polling.
//
// It also captures the thread's last assistant text so wait_agent can hand a
// controller its worker's answer without replaying the transcript: claude's
// `result` events carry the final text verbatim; kimi's do not, so the last
// non-empty assistant text event stands in (the kimi translator flushes the
// trailing text of a turn as one event).
type TurnTracker struct {
	mu      sync.Mutex
	changed chan struct{} // closed + replaced on every state change (broadcast)
	threads map[string]*turnState
}

type turnState struct {
	inFlight int
	ended    bool   // terminal lifecycle seen; no result is coming
	lastText string // last assistant text (or claude result text)
}

// NewTurnTracker creates an empty tracker.
func NewTurnTracker() *TurnTracker {
	return &TurnTracker{
		changed: make(chan struct{}),
		threads: make(map[string]*turnState),
	}
}

// state returns (creating if needed) the tracked state. Caller holds tt.mu.
func (tt *TurnTracker) state(threadID string) *turnState {
	st := tt.threads[threadID]
	if st == nil {
		st = &turnState{}
		tt.threads[threadID] = st
	}
	return st
}

// broadcastLocked wakes every waiter. Caller holds tt.mu.
func (tt *TurnTracker) broadcastLocked() {
	close(tt.changed)
	tt.changed = make(chan struct{})
}

// TurnQueued records that a user message was (or is about to be) written to
// the thread, so the thread counts as busy until the matching result lands.
// Call it BEFORE the asynchronous launch/send so a wait that races the start
// never sees a false idle.
func (tt *TurnTracker) TurnQueued(threadID string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	st := tt.state(threadID)
	st.inFlight++
	st.ended = false
	tt.broadcastLocked()
}

// TurnFailed undoes a TurnQueued whose send never reached the agent (write
// error, dead thread) — no result is coming for it.
func (tt *TurnTracker) TurnFailed(threadID string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	st := tt.state(threadID)
	if st.inFlight > 0 {
		st.inFlight--
	}
	tt.broadcastLocked()
}

// Forget drops a thread's state (discard/cleanup paths).
func (tt *TurnTracker) Forget(threadID string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	delete(tt.threads, threadID)
	tt.broadcastLocked()
}

// Observe feeds one translated stream-json event through the tracker. Called
// by the relay for every event of every thread; events it does not care about
// are cheap early returns.
func (tt *TurnTracker) Observe(threadID string, raw json.RawMessage) {
	var head struct {
		Type    string          `json:"type"`
		Phase   string          `json:"phase"`
		Result  json.RawMessage `json:"result"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return
	}
	switch head.Type {
	case "assistant":
		var sb strings.Builder
		for _, blk := range head.Message.Content {
			if blk.Type == "text" {
				sb.WriteString(blk.Text)
			}
		}
		if text := strings.TrimSpace(sb.String()); text != "" {
			tt.mu.Lock()
			tt.state(threadID).lastText = text
			tt.mu.Unlock()
		}
	case "result":
		// Claude result events carry the turn's final text; prefer it over the
		// assistant-event accumulation (it is the exact final message). Kimi's
		// results carry no text — keep whatever the assistant events left.
		var resText string
		if len(head.Result) > 0 {
			_ = json.Unmarshal(head.Result, &resText)
		}
		tt.mu.Lock()
		st := tt.state(threadID)
		if strings.TrimSpace(resText) != "" {
			st.lastText = resText
		}
		if st.inFlight > 0 {
			st.inFlight--
		}
		tt.broadcastLocked()
		tt.mu.Unlock()
	case "_lifecycle":
		tt.ObserveLifecycle(threadID, head.Phase)
	}
}

// ObserveLifecycle applies one lifecycle phase. Terminal phases (exited,
// interrupted, error) clear the in-flight count and wake every waiter — a
// thread that died mid-turn will never deliver its result. It is called from
// Observe for supervisor-emitted phases and directly by the orchestration
// layer's emitLifecycle for the phases that never cross the relay (started /
// resumed / launch errors).
func (tt *TurnTracker) ObserveLifecycle(threadID, phase string) {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	st := tt.state(threadID)
	switch phase {
	case "exited", "interrupted", "error":
		st.inFlight = 0
		st.ended = true
		tt.broadcastLocked()
	case "started", "resumed":
		st.ended = false
	}
}

// LastText returns the thread's last known assistant text.
func (tt *TurnTracker) LastText(threadID string) string {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if st := tt.threads[threadID]; st != nil {
		return st.lastText
	}
	return ""
}

// Wait blocks until the thread is idle — no turn in flight, or the thread
// ended — or the timeout fires. It returns the thread's last assistant text
// and whether the deadline was hit (timedOut true means the thread was still
// mid-turn when Wait gave up).
func (tt *TurnTracker) Wait(threadID string, timeout time.Duration) (lastText string, timedOut bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		tt.mu.Lock()
		st := tt.state(threadID)
		if st.inFlight == 0 || st.ended {
			text := st.lastText
			tt.mu.Unlock()
			return text, false
		}
		ch := tt.changed
		tt.mu.Unlock()
		select {
		case <-ch:
		case <-deadline.C:
			return tt.LastText(threadID), true
		}
	}
}
