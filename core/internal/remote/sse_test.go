package remote

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// publishN pushes n turnState events and returns the last id assigned.
func publishN(h *hub, n int) uint64 {
	var last uint64
	for i := 0; i < n; i++ {
		last = h.publish(evTurnState, "t-1", []byte(fmt.Sprintf(`{"i":%d}`, i)))
	}
	return last
}

// TestPublishNeverBlocksOnSlowClient is the single most important test in this
// package.
//
// A phone on a dying mobile link must not be able to wedge the core's
// notification fan-out — the same hazard the IPC server's enqueue exists to
// solve. The proof is structural rather than statistical: NOTHING ever drains
// these clients, so if publish could block it would block forever and this test
// would hang rather than merely run slowly. The deadline turns that hang into a
// failure.
func TestPublishNeverBlocksOnSlowClient(t *testing.T) {
	h := newHub(nil)
	slow := newSSEClient(scopeRoster, "", "d-slow", "s-slow")
	h.subscribe(slow, 0, false)
	alsoSlow := newSSEClient(scopeRoster, "", "d-slow2", "s-slow2")
	h.subscribe(alsoSlow, 0, false)

	// Comfortably past the buffer, so overflow is certain.
	const total = clientBuffer * 4

	done := make(chan uint64, 1)
	go func() { done <- publishN(h, total) }()

	select {
	case last := <-done:
		if last != uint64(total) {
			t.Fatalf("last id = %d, want %d", last, total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a client that never drains — the core's fan-out " +
			"can be wedged by one bad connection")
	}

	if !slow.dropped.Load() || !alsoSlow.dropped.Load() {
		t.Fatal("an overflowing client was not marked for re-sync")
	}
	if got := len(slow.ch); got != clientBuffer {
		t.Fatalf("buffered %d events, want the full buffer of %d", got, clientBuffer)
	}
	// The buffer must hold the OLDEST arrivals intact. Dropping the newest keeps
	// the prefix contiguous, so the writer only ever has to announce one hole
	// instead of reasoning about a stream somebody edited underneath it.
	for i := 1; i <= clientBuffer; i++ {
		ev := <-slow.ch
		if ev.id != uint64(i) {
			t.Fatalf("buffered event %d has id %d; the drop policy corrupted the prefix", i, ev.id)
		}
	}
}

// TestSlowClientIsIsolatedFromHealthyOne pins the other half of the property:
// one wedged phone must not cost a working one a single event.
func TestSlowClientIsIsolatedFromHealthyOne(t *testing.T) {
	h := newHub(nil)
	slow := newSSEClient(scopeRoster, "", "d-slow", "s-slow")
	h.subscribe(slow, 0, false)
	healthy := newSSEClient(scopeRoster, "", "d-ok", "s-ok")
	h.subscribe(healthy, 0, false)

	// Publish and drain in lockstep so the healthy client's outcome is a fact
	// rather than a race against a consumer goroutine. Twice the buffer depth
	// guarantees the slow client overflows.
	const total = clientBuffer * 2
	for i := 1; i <= total; i++ {
		h.publish(evTurnState, "t-1", []byte(`{}`))
		select {
		case ev := <-healthy.ch:
			if ev.id != uint64(i) {
				t.Fatalf("healthy client event %d has id %d", i, ev.id)
			}
		default:
			t.Fatalf("healthy client never received event %d", i)
		}
	}
	if !slow.dropped.Load() {
		t.Fatal("the slow client never overflowed, so this test proves nothing")
	}
	if healthy.dropped.Load() {
		t.Fatal("a healthy client was marked for re-sync because another one stalled")
	}
}

func TestSubscribeCursorHandling(t *testing.T) {
	cases := []struct {
		name       string
		published  int
		fromID     uint64
		hasCursor  bool
		wantGap    string
		wantReplay int
	}{
		{name: "fresh subscriber gets no replay", published: 5, hasCursor: false},
		{
			name: "resume from the middle", published: 5, fromID: 2, hasCursor: true,
			wantReplay: 3,
		},
		{
			name: "resume from the head", published: 5, fromID: 5, hasCursor: true,
			wantReplay: 0,
		},
		{
			name: "resume from before the beginning", published: 5, fromID: 0, hasCursor: true,
			wantReplay: 5,
		},
		{
			name: "cursor ahead of the server", published: 5, fromID: 99, hasCursor: true,
			wantGap: gapUnknownCursor,
		},
		{
			name: "cursor fell out of the ring", published: ringSize + 10, fromID: 1,
			hasCursor: true, wantGap: gapRingOverflow,
		},
		{
			name: "cursor exactly at the ring edge", published: ringSize + 10,
			fromID: 10, hasCursor: true, wantReplay: ringSize,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(nil)
			publishN(h, tc.published)
			c := newSSEClient(scopeRoster, "", "d", "s")
			replay, head, gap := h.subscribe(c, tc.fromID, tc.hasCursor)
			if gap != tc.wantGap {
				t.Fatalf("gap = %q, want %q", gap, tc.wantGap)
			}
			if len(replay) != tc.wantReplay {
				t.Fatalf("replayed %d events, want %d", len(replay), tc.wantReplay)
			}
			if head != uint64(tc.published) {
				t.Fatalf("head = %d, want %d", head, tc.published)
			}
			for i := 1; i < len(replay); i++ {
				if replay[i].id <= replay[i-1].id {
					t.Fatalf("replay is not monotonic at %d: %d then %d",
						i, replay[i-1].id, replay[i].id)
				}
			}
		})
	}
}

func TestScopeFilteringIsServerSide(t *testing.T) {
	agentEvent := ringEvent{id: 1, name: evAgentEvent, threadID: "t-1"}
	otherAgentEvent := ringEvent{id: 2, name: evAgentEvent, threadID: "t-2"}
	roster := ringEvent{id: 3, name: evRoster}
	turn := ringEvent{id: 4, name: evTurnState, threadID: "t-2"}
	perm := ringEvent{id: 5, name: evPermissionRequested, threadID: "t-2"}

	cases := []struct {
		name   string
		client *sseClient
		ev     ringEvent
		want   bool
	}{
		{"roster scope refuses per-event traffic", newSSEClient(scopeRoster, "", "d", "s"), agentEvent, false},
		{"roster scope takes the roster body", newSSEClient(scopeRoster, "", "d", "s"), roster, true},
		{"roster scope takes turn state", newSSEClient(scopeRoster, "", "d", "s"), turn, true},
		{"thread scope takes its own events", newSSEClient(scopeThread, "t-1", "d", "s"), agentEvent, true},
		{"thread scope refuses another thread's events", newSSEClient(scopeThread, "t-1", "d", "s"), otherAgentEvent, false},
		{"thread scope refuses the roster body", newSSEClient(scopeThread, "t-1", "d", "s"), roster, false},
		// Small cross-thread summaries still flow to a thread subscriber: a phone
		// reading one chat must still learn that a different agent is parked.
		{"thread scope still sees other threads' prompts", newSSEClient(scopeThread, "t-1", "d", "s"), perm, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.client.wants(tc.ev); got != tc.want {
				t.Fatalf("wants(%s) = %v, want %v", tc.ev.name, got, tc.want)
			}
		})
	}
}

// TestTranscriptDroppedWithoutAViewer guards the ring budget: transcript
// traffic for a thread nobody is watching must not evict the permission prompts a
// reconnecting phone came back for.
func TestTranscriptDroppedWithoutAViewer(t *testing.T) {
	env := newTestEnv(t)
	events := []TranscriptEvent{{Kind: "assistant", Text: "safe"}}

	env.srv.PublishTranscript("t-nobody", events)
	if got := env.srv.hub.headID(); got != 0 {
		t.Fatalf("an unwatched thread's events consumed %d ids", got)
	}

	c := newSSEClient(scopeThread, "t-watched", "d", "s")
	env.srv.hub.subscribe(c, 0, false)
	env.srv.PublishTranscript("t-watched", events)
	if got := env.srv.hub.headID(); got != 1 {
		t.Fatalf("a watched thread's events were dropped (head=%d)", got)
	}
}

// TestThreadInterestOutlivesADisconnect covers the common failure the resumable
// design exists for: a phone in a tunnel. Its events must keep reaching the ring
// for a short while so the reconnect replays rather than gapping.
func TestThreadInterestOutlivesADisconnect(t *testing.T) {
	now := time.Now()
	h := newHub(func() time.Time { return now })
	c := newSSEClient(scopeThread, "t-1", "d", "s")
	h.subscribe(c, 0, false)
	h.unsubscribe(c)

	if !h.interested("t-1") {
		t.Fatal("interest expired the instant the viewer dropped")
	}
	now = now.Add(threadInterestWindow + time.Second)
	if h.interested("t-1") {
		t.Fatal("interest outlived its window")
	}
	if len(h.interest) != 0 {
		t.Fatalf("stale interest entries were not pruned: %v", h.interest)
	}
}

// --- end-to-end over HTTP ---------------------------------------------------

func TestStreamHelloAndKeepaliveShape(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()

	f := c.next()
	if f.event != evHello {
		t.Fatalf("first frame = %q, want hello", f.event)
	}
	if f.id != "" {
		t.Errorf("hello carried id %q; per-connection control frames must not "+
			"enter the global sequence", f.id)
	}
	body := decodeData(t, f)
	if body["apiVersion"] != float64(APIVersion) {
		t.Errorf("hello apiVersion = %v", body["apiVersion"])
	}
	if body["resumed"] != false {
		t.Errorf("hello resumed = %v, want false for a fresh subscriber", body["resumed"])
	}
	if body["serverTime"] == "" {
		t.Error("hello carried no serverTime; a client cannot derive its clock offset")
	}
}

func TestStreamReplaysFromLastEventID(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 5; i++ {
		env.srv.hub.publish(evTurnState, "t-1", []byte(`{"n":`+strconv.Itoa(i)+`}`))
	}

	c := env.openSSE("?scope=roster", 2)
	c.skipRetry()

	hello := c.next()
	if hello.event != evHello {
		t.Fatalf("first frame = %q", hello.event)
	}
	if decodeData(t, hello)["resumed"] != true {
		t.Error("a served cursor did not report resumed:true")
	}
	for want := 3; want <= 5; want++ {
		f := c.next()
		if f.event != evTurnState {
			t.Fatalf("frame = %q, want turnState", f.event)
		}
		if f.id != strconv.Itoa(want) {
			t.Fatalf("replayed id = %q, want %d", f.id, want)
		}
	}
}

func TestStreamEmitsGapWhenCursorFellOutOfTheRing(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < ringSize+50; i++ {
		env.srv.hub.publish(evTurnState, "t-1", []byte(`{}`))
	}

	c := env.openSSE("?scope=roster", 1)
	c.skipRetry()

	hello := c.next()
	if decodeData(t, hello)["resumed"] != false {
		t.Error("a cursor that could not be served reported resumed:true")
	}
	gap := c.next()
	if gap.event != evGap {
		t.Fatalf("second frame = %q, want gap", gap.event)
	}
	if got := decodeData(t, gap)["reason"]; got != gapRingOverflow {
		t.Fatalf("gap reason = %v, want %q", got, gapRingOverflow)
	}
	if gap.id != "" {
		t.Error("gap carried an id")
	}
}

func TestStreamEmitsGapForAnUnknownCursor(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", 99999)
	c.skipRetry()
	c.next() // hello
	gap := c.next()
	if gap.event != evGap {
		t.Fatalf("second frame = %q, want gap", gap.event)
	}
	if got := decodeData(t, gap)["reason"]; got != gapUnknownCursor {
		t.Fatalf("gap reason = %v, want %q", got, gapUnknownCursor)
	}
}

func TestStreamDeliversLiveEventsInOrder(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next() // hello

	for i := 0; i < 10; i++ {
		env.srv.PublishTurnState(TurnState{ThreadID: "t-1", Busy: i%2 == 0, Attention: true})
	}
	for want := 1; want <= 10; want++ {
		f := c.next()
		if f.event != evTurnState {
			t.Fatalf("frame %d = %q, want turnState", want, f.event)
		}
		if f.id != strconv.Itoa(want) {
			t.Fatalf("frame %d has id %q", want, f.id)
		}
	}
}

// TestRevokeTerminatesALiveStream is B2's headline requirement. Rejecting new
// requests is not revocation: a stream opened a moment earlier would keep
// feeding a revoked device indefinitely.
func TestRevokeTerminatesALiveStream(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next() // hello

	env.srv.RevokeDevice(env.device.ID, "test revoke")

	f := c.next()
	if f.event != evRevoked {
		t.Fatalf("frame after revoke = %q, want revoked", f.event)
	}
	if got := decodeData(t, f)["reason"]; got != "revoked" {
		t.Errorf("revoked reason = %v", got)
	}
	c.expectClosed()
}

func TestKillSwitchTerminatesEveryStream(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next() // hello

	env.srv.SetKillSwitch(true)

	f := c.next()
	if f.event != evRevoked {
		t.Fatalf("frame after kill-switch = %q, want revoked", f.event)
	}
	if got := decodeData(t, f)["reason"]; got != "kill-switch" {
		t.Errorf("reason = %v, want kill-switch", got)
	}
	c.expectClosed()
}

func TestStreamRejectsBadScope(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct {
		name, query string
	}{
		{"unknown scope", "?scope=everything"},
		{"thread scope with no thread", "?scope=thread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.auth("GET", "/api/v1/events"+tc.query, "")
			defer resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestEventCursorPrefersTheHeader(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		query     string
		wantID    uint64
		wantHas   bool
		wantAhead bool // expressed as "an unservable cursor"
	}{
		{name: "nothing", wantHas: false},
		{name: "header only", header: "7", wantID: 7, wantHas: true},
		{name: "query only", query: "9", wantID: 9, wantHas: true},
		// EventSource reconnects to the ORIGINAL url, so a query cursor would be
		// replayed forever if it won.
		{name: "header wins over query", header: "7", query: "9", wantID: 7, wantHas: true},
		{name: "malformed header is an unknown cursor", header: "abc", wantHas: true, wantAhead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCursorRequest(tc.header, tc.query)
			id, has := eventCursor(r)
			if has != tc.wantHas {
				t.Fatalf("hasCursor = %v, want %v", has, tc.wantHas)
			}
			if tc.wantAhead {
				if id != ^uint64(0) {
					t.Fatalf("malformed cursor = %d, want the unservable sentinel", id)
				}
				return
			}
			if has && id != tc.wantID {
				t.Fatalf("cursor = %d, want %d", id, tc.wantID)
			}
		})
	}
}
