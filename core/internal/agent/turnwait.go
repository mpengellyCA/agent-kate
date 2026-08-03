package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	// onChange receives only busy-edge transitions. It is invoked after tt.mu
	// is released: a consumer may publish or re-enter the tracker without
	// stalling an in-flight wait.
	onChange func(threadID string, busy bool)
}

type turnState struct {
	inFlight int
	waiters  int    // Waits currently parked on this thread
	ended    bool   // terminal lifecycle seen; no result is coming
	lastText string // last assistant text (or claude result text), capped
}

// maxLastTextBytes bounds the retained lastText. Its only consumers are
// agent.wait / wait_agent, which hand a controller its worker's reply as tool
// result text — the head of a runaway final message is enough for that, and an
// uncapped copy of every thread's final message is a leak (F61).
const maxLastTextBytes = 64 << 10

// capLastText truncates on a rune boundary and marks the cut.
func capLastText(s string) string {
	if len(s) <= maxLastTextBytes {
		return s
	}
	cut := maxLastTextBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[truncated]"
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

// SetOnChange installs the busy-edge observer used by human-facing surfaces.
// The callback is always invoked without tt.mu held.
func (tt *TurnTracker) SetOnChange(f func(threadID string, busy bool)) {
	tt.mu.Lock()
	tt.onChange = f
	tt.mu.Unlock()
}

// busyLocked is the single definition of a busy turn. Keeping it shared by
// Busy, Snapshot, Wait and edge delivery prevents two surfaces disagreeing.
func busyLocked(st *turnState) bool {
	return st != nil && st.inFlight > 0 && !st.ended
}

// changedLocked wakes waiters and returns an edge callback for the caller to
// invoke after unlocking. Caller holds tt.mu.
func (tt *TurnTracker) changedLocked(threadID string, before bool, st *turnState) func() {
	tt.broadcastLocked()
	after := busyLocked(st)
	callback := tt.onChange
	if callback == nil || before == after {
		return nil
	}
	return func() { callback(threadID, after) }
}

// TurnQueued records that a user message was (or is about to be) written to
// the thread, so the thread counts as busy until the matching result lands.
// Call it BEFORE the asynchronous launch/send so a wait that races the start
// never sees a false idle.
func (tt *TurnTracker) TurnQueued(threadID string) {
	tt.mu.Lock()
	st := tt.state(threadID)
	before := busyLocked(st)
	st.inFlight++
	st.ended = false
	callback := tt.changedLocked(threadID, before, st)
	tt.mu.Unlock()
	if callback != nil {
		callback()
	}
}

// TurnFailed undoes a TurnQueued whose send never reached the agent (write
// error, dead thread) — no result is coming for it.
func (tt *TurnTracker) TurnFailed(threadID string) {
	tt.mu.Lock()
	st := tt.threads[threadID]
	if st == nil {
		tt.mu.Unlock()
		return
	}
	before := busyLocked(st)
	if st.inFlight > 0 {
		st.inFlight--
	}
	callback := tt.changedLocked(threadID, before, st)
	tt.mu.Unlock()
	if callback != nil {
		callback()
	}
}

// Forget drops a thread's state (discard/cleanup paths).
func (tt *TurnTracker) Forget(threadID string) {
	tt.mu.Lock()
	before := busyLocked(tt.threads[threadID])
	delete(tt.threads, threadID)
	callback := tt.changedLocked(threadID, before, nil)
	tt.mu.Unlock()
	if callback != nil {
		callback()
	}
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
			// Not state(): a stray event for a swept thread (a tail frame
			// racing the reap) must not recreate an entry nothing will prune.
			if st := tt.threads[threadID]; st != nil {
				st.lastText = capLastText(text)
			}
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
		st := tt.threads[threadID]
		if st == nil {
			tt.mu.Unlock()
			return
		}
		before := busyLocked(st)
		if strings.TrimSpace(resText) != "" {
			st.lastText = capLastText(resText)
		}
		if st.inFlight > 0 {
			st.inFlight--
		}
		callback := tt.changedLocked(threadID, before, st)
		tt.mu.Unlock()
		if callback != nil {
			callback()
		}
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
	switch phase {
	case "exited", "interrupted", "error":
		st := tt.threads[threadID]
		if st == nil {
			tt.mu.Unlock()
			return // never tracked; nothing to end, nothing to sweep
		}
		before := busyLocked(st)
		st.inFlight = 0
		st.ended = true
		callback := tt.changedLocked(threadID, before, st)
		// Reap sweep (F61): the entry's only remaining consumer is a parked
		// waiter reading lastText on wake. With none registered, drop it now
		// instead of holding the final message until discard/stopClose; with
		// waiters parked, the last one out completes the sweep
		// (unregisterWaiterLocked).
		if st.waiters == 0 {
			delete(tt.threads, threadID)
		}
		tt.mu.Unlock()
		if callback != nil {
			callback()
		}
	case "started", "resumed":
		tt.state(threadID).ended = false
		tt.mu.Unlock()
	default:
		tt.mu.Unlock()
	}
}

// Busy reports whether a turn is currently in flight for threadID. It does
// not allocate state for an unknown id: roster reads must not leak entries
// which can never receive a lifecycle event to reap them.
func (tt *TurnTracker) Busy(threadID string) bool {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return busyLocked(tt.threads[threadID])
}

// Snapshot returns busy state for every tracked thread in one lock acquisition.
// An absent id is idle.
func (tt *TurnTracker) Snapshot() map[string]bool {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	out := make(map[string]bool, len(tt.threads))
	for id, st := range tt.threads {
		out[id] = busyLocked(st)
	}
	return out
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
// ended — or the timeout fires, or ctx is cancelled (a disconnected bridge
// must release its waiter, not park it until the deadline). It returns the
// thread's last assistant text and whether the wait gave up before idle
// (deadline or cancellation) — check ctx.Err() to tell the two apart.
//
// A never-seen id is idle with nothing to report. Wait deliberately does NOT
// create an entry for it (F61): any approved bridge can wait on any id, and a
// tracker entry with no thread behind it would never see the terminal
// lifecycle that sweeps it.
func (tt *TurnTracker) Wait(ctx context.Context, threadID string, timeout time.Duration) (lastText string, timedOut bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	registered := false
	for {
		tt.mu.Lock()
		st := tt.threads[threadID]
		if st == nil {
			tt.mu.Unlock()
			return "", false
		}
		if st.inFlight == 0 || st.ended {
			text := st.lastText
			if registered {
				tt.unregisterWaiterLocked(threadID, st)
			}
			tt.mu.Unlock()
			return text, false
		}
		if !registered {
			st.waiters++
			registered = true
		}
		ch := tt.changed
		tt.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return tt.detach(threadID, registered), true
		case <-deadline.C:
			return tt.detach(threadID, registered), true
		}
	}
}

// detach reads the thread's last text and, if this Wait had registered as a
// waiter, unregisters it — the give-up paths must not leave a phantom waiter
// holding an ended entry alive forever.
func (tt *TurnTracker) detach(threadID string, registered bool) string {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	st := tt.threads[threadID]
	if st == nil {
		return ""
	}
	text := st.lastText
	if registered {
		tt.unregisterWaiterLocked(threadID, st)
	}
	return text
}

// unregisterWaiterLocked drops one waiter; the last waiter leaving an ended
// thread completes the reap sweep ObserveLifecycle deferred to it. Caller
// holds tt.mu.
func (tt *TurnTracker) unregisterWaiterLocked(threadID string, st *turnState) {
	if st.waiters > 0 {
		st.waiters--
	}
	if st.ended && st.waiters == 0 {
		delete(tt.threads, threadID)
	}
}
