package remote

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleEvents is GET /api/v1/events — the SSE stream.
//
// The shape of this function is the answer to three separate hazards, and each
// step is placed where it is on purpose:
//
//  1. Resumability. The cursor is read, the replay slice and the registration
//     happen under ONE hub lock, and replay is written before the live loop
//     starts. There is no instant between "what I have" and "what I will get".
//  2. Backpressure. Replay is written from a slice, not pushed through the
//     bounded channel, so a long replay cannot trip the very drop policy it
//     exists to recover from.
//  3. Revocation. The connection is re-authenticated on every keepalive tick as
//     well as being terminated by a push, so a revoke lands even if the push
//     were somehow missed.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r.Context())

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = scopeRoster
	}
	threadID := r.URL.Query().Get("threadId")
	switch scope {
	case scopeRoster:
		threadID = "" // a roster subscription is never thread-scoped
	case scopeThread:
		if threadID == "" {
			writeError(w, http.StatusBadRequest, "bad-scope",
				"scope=thread requires a threadId.")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "bad-scope", "Unknown subscription scope.")
		return
	}

	fromID, hasCursor := eventCursor(r)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// Defeats proxy buffering (nginx, and `tailscale serve` in some configs),
	// which would otherwise hold every frame until the response ended — i.e.
	// forever, for a stream.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	out := newSSEWriter(w)
	// Tell the browser how long to wait before reconnecting. Without it a
	// reconnect storm against a core that is shutting down is the default.
	if !out.raw("retry: " + strconv.Itoa(sseRetryMillis) + "\n\n") {
		return
	}

	c := newSSEClient(scope, threadID, sess.DeviceID, sess.ID)
	replay, head, gap := s.hub.subscribe(c, fromID, hasCursor)
	defer s.hub.unsubscribe(c)

	// hello, gap and revoked carry NO id:. They are per-connection control
	// frames, not entries in the global sequence, and giving them an id would
	// make a browser's Last-Event-ID point at something no other client saw.
	if !out.write(0, evHello, mustJSON(map[string]any{
		"serverTime":  s.nowRFC3339(),
		"apiVersion":  APIVersion,
		"resumed":     hasCursor && gap == "",
		"lastEventId": head,
	})) {
		return
	}
	if gap != "" {
		if !out.write(0, evGap, mustJSON(map[string]any{"reason": gap})) {
			return
		}
	}
	for _, ev := range replay {
		if !out.write(ev.id, ev.name, ev.data) {
			return
		}
	}

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	ctx := r.Context()

	for {
		// Checked at the top of every iteration rather than inside a branch, so
		// both paths that can set it — a full buffer and the wake poke — reach
		// the same handling. Everything still buffered is discarded: it is all
		// older than the hole, and a client that has been told to re-sync will
		// refetch it anyway.
		if c.dropped.CompareAndSwap(true, false) {
			c.drain()
			if !out.write(0, evGap, mustJSON(map[string]any{"reason": gapSlowClient})) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case reason := <-c.rev:
			// Emit, then close. A revoked device must be told why, or a phone
			// that simply lost signal and one that was cut off look identical.
			out.write(0, evRevoked, mustJSON(map[string]any{"reason": reason}))
			return
		case ev := <-c.ch:
			if !out.write(ev.id, ev.name, ev.data) {
				return
			}
		case <-c.wake:
			// Loop; the drop check at the top does the work.
		case <-ticker.C:
			// Re-authenticate on the same tick as the keepalive. A stream is the
			// one request that outlives its own authorisation check, so it has to
			// keep asking.
			if _, ok := s.sessionFrom(r); !ok {
				out.write(0, evRevoked, mustJSON(map[string]any{"reason": "revoked"}))
				return
			}
			if !out.comment("keepalive") {
				return
			}
		}
	}
}

// eventCursor resolves where to resume from.
//
// The Last-Event-ID HEADER wins over the query parameter, and that order is
// load-bearing: EventSource reconnects to the ORIGINAL url, so a query cursor
// would be replayed on every reconnect forever, while the header carries the
// browser's actual high-water mark. The query parameter exists for the one case
// the header cannot serve — the first subscription after a transcript fetch,
// which hands back lastEventId so the join is gapless.
func eventCursor(r *http.Request) (uint64, bool) {
	if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			return n, true
		}
		// A malformed header is a client that thinks it has a position. Treating
		// it as "no cursor" would silently start it from live and lose whatever
		// it missed; treating it as an unknown cursor produces a gap and a
		// refetch, which is right.
		return ^uint64(0), true
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("lastEventId")); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			return n, true
		}
		return ^uint64(0), true
	}
	return 0, false
}
