package main

import (
	"fmt"

	"agentkate/internal/session"
)

// continuationPrompt is intentionally constant.  Continuation is a control
// chosen by the human, not a heuristic that tries to decide whether prose such
// as "I'll continue" meant another turn should be started.
const continuationPrompt = "Continue working on the current task. If it is complete, verify the result and report the final outcome."

const (
	defaultContinuationMaxTurns = 8
	maxContinuationMaxTurns     = 50
)

// continueAfterResult starts one bounded, opt-in host-owned follow-up.  It is
// called only for a translated result event, never for an idle edge: a failed
// pipe write, interrupt, process exit, or permission pause therefore cannot
// accidentally cause new work to begin.
func (d handlerDeps) continueAfterResult(threadID string) {
	if d.sessions == nil || d.turns == nil {
		return
	}
	shouldSend := false
	if err := d.sessions.Update(threadID, func(r *session.Record) {
		p := &r.Continuation
		if !p.Enabled || p.MaxTurns < 1 || p.TurnsUsed >= p.MaxTurns {
			return
		}
		// Reserve before delivery.  A synchronous result from an exceptionally
		// fast harness may re-enter this code immediately; persistence makes the
		// limit authoritative across both that race and restarts.
		p.TurnsUsed++
		shouldSend = true
	}); err != nil || !shouldSend {
		return
	}

	if _, err := d.humanQueue.enqueue(threadID, continuationPrompt, nil); err != nil {
		// The reserved slot was never queued. Return it, but do not retry: a
		// later human action/result is required to make progress after a failed
		// enqueue, which prevents a dead harness from becoming a retry loop.
		_ = d.sessions.UpdateQuiet(threadID, func(r *session.Record) {
			if r.Continuation.TurnsUsed > 0 {
				r.Continuation.TurnsUsed--
			}
		})
		if d.log != nil {
			d.log.Warn("bounded continuation follow-up was not delivered", "thread", threadID, "err", err)
		}
	}
}

func normaliseContinuation(p session.ContinuationPolicy) (session.ContinuationPolicy, error) {
	if !p.Enabled {
		return session.ContinuationPolicy{}, nil
	}
	if p.MaxTurns == 0 {
		p.MaxTurns = defaultContinuationMaxTurns
	}
	if p.MaxTurns < 1 || p.MaxTurns > maxContinuationMaxTurns {
		return session.ContinuationPolicy{}, fmt.Errorf("maxTurns must be between 1 and %d", maxContinuationMaxTurns)
	}
	// A new opt-in run always starts with a fresh budget.  TurnsUsed is only
	// internally maintained after it has been accepted.
	p.TurnsUsed = 0
	return p, nil
}
