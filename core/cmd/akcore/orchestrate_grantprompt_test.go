package main

// What the approval dialog SAYS about the authority it is recording
// (convergence round, 2026-08-01). Two defects, both found by reading the
// previous round's code rather than by anything failing:
//
//  1. the window was rendered with int(Minutes()), which truncates — so a
//     shorter TTL than today's would have the dialog telling the human "0 min"
//     about a grant that stands and self-renews;
//  2. enable_cowork — the ask that hands an agent the screen, the keyboard and
//     the pointer — reached the bar under a name the UI has no digest for, so
//     it rendered as a raw JSON dump of the payload, inside a 240-character
//     budget the grant-scope fields were already eating.
//
// Both are honest-labelling defects: the mechanism was right and the sentence
// describing it was not.

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentkate/internal/ipc"
	"agentkate/internal/session"
)

// TestGrantWindowIsStatedHonestly drives humanDuration across the values
// orchGrantTTL could plausibly take. The 45-second row is the one that matters:
// under the old strconv.Itoa(int(d.Minutes())) it renders "0 min" — the dialog
// telling the human the authority expires immediately, in the sentence they are
// deciding on.
func TestGrantWindowIsStatedHonestly(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45 sec"},
		{30 * time.Second, "30 sec"},
		{time.Minute, "1 min"},
		{90 * time.Second, "1 min 30 sec"},
		{15 * time.Minute, "15 min"},
		{2 * time.Hour, "2 h"},
		{90 * time.Minute, "1 h 30 min"},
		{500 * time.Millisecond, "500 ms"},
	} {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// No value the constant can hold may render as a zero of anything: "0 min"
	// and "0 sec" are the two ways this defect reappears.
	for _, d := range []time.Duration{
		time.Millisecond, time.Second, 45 * time.Second, 59 * time.Second,
		time.Minute, orchGrantTTL, time.Hour, 24 * time.Hour,
	} {
		got := humanDuration(d)
		if strings.HasPrefix(got, "0 ") {
			t.Errorf("humanDuration(%s) = %q — a live window rendered as zero", d, got)
		}
	}

	// ...and the sentence the human reads carries that rendering, not its own
	// arithmetic.
	clause := standingGrantClause(2)
	if !strings.Contains(clause, humanDuration(orchGrantTTL)) {
		t.Errorf("the standing-grant clause does not quote the real window (%s): %q",
			humanDuration(orchGrantTTL), clause)
	}
	// The subject agrees with the number of grants the click writes. "both" for
	// one action was the same class of defect in miniature: a sentence wrong
	// about its own subject.
	if got := standingGrantClause(1); !strings.Contains(got, "it stays allowed") {
		t.Errorf("a single-action clause reads %q; it must not say \"both\"", got)
	}
	if got := standingGrantClause(2); !strings.Contains(got, "both stay allowed") {
		t.Errorf("a two-action clause reads %q", got)
	}
	if got := standingGrantClause(3); !strings.Contains(got, "all 3 stay allowed") {
		t.Errorf("a three-action clause reads %q", got)
	}
	if !strings.Contains(clause, "renewed by use") {
		t.Errorf("the clause states a window but not that use renews it: %q", clause)
	}
}

// TestUndigestedVerbsCarryASentence is the enable_cowork half, asserted on the
// RENDERED bar through the port of the UI's own summariser — because the
// payload was already "correct" and the dialog was still unreadable.
//
// enable_cowork has no branch in ui/src/AgentChatHelpers.cpp's mcpSummary, so
// permSummary falls through to its generic key scan and then to a compact JSON
// dump of the whole input. Under that last resort the human deciding whether to
// hand an agent the desktop read a wall of field names, with the facts pushed
// past the bar's 240-character elision by the grant-scope fields themselves.
func TestUndigestedVerbsCarryASentence(t *testing.T) {
	sock, secrets, broker, srv := pass2Core(t, []session.Record{
		{ThreadID: "t-a"},
		{ThreadID: "t-x", Status: session.StatusDormant}, // outside t-a's subtree
		{ThreadID: "t-y", Status: session.StatusDormant}, // ditto, for the contrast
	})
	var allow atomic.Bool // deny: the ASK is what this measures
	read := permCapturingResponder(t, srv, sock, broker, &allow)

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	asBridge(t, secrets, client, "t-a")

	const reason = "to check the rendering in Firefox"
	if err := client.CallTimeout("cowork.requestEnable", map[string]any{
		"fromThreadId": "t-a", "threadId": "t-x", "reason": reason,
	}, nil, 30*time.Second); err == nil {
		t.Fatal("a denied cross-subtree enable_cowork must return an error")
	}

	asks := read()
	if len(asks) != 1 {
		t.Fatalf("enable_cowork asked %d times, want exactly 1: %+v", len(asks), asks)
	}
	ask := asks[0]
	rendered := permBarText(ask.tool, ask.input)

	// The facts, in the words the human sees.
	for _, want := range []string{
		"control your desktop",      // WHAT approving does — not the verb's name
		"t-x",                       // to which thread
		"t-a",                       // on whose behalf
		humanDuration(orchGrantTTL), // that it STANDS, for how long
		"renewed by use",            // ...and renews itself
		"outside",                   // that this is a cross-subtree reach
		"rendering in Firefox",      // the agent's stated reason, last
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the desktop-access bar never says %q.\nrendered: %s", want, rendered)
		}
	}
	// The regression itself: the raw-JSON last resort. Its signature is the
	// payload's own field names appearing verbatim in the bar.
	for _, jsonLeak := range []string{"grantRenewedByUse", "targetThreadId", `{"`} {
		if strings.Contains(rendered, jsonLeak) {
			t.Errorf("the bar is rendering the payload as JSON (%q present) — the "+
				"human reads field names instead of what they are approving.\n"+
				"rendered: %s", jsonLeak, rendered)
		}
	}
	// The sentence only reaches the human because permSummary's key scan gets as
	// far as `description`. It checks file_path, path and pattern first, so one
	// of those in the payload would be printed INSTEAD — silently, with the
	// dialog then describing a file rather than an authority.
	for _, hijack := range []string{"file_path", "path", "pattern"} {
		if _, ok := ask.input[hijack]; ok {
			t.Errorf("the payload carries %q, which the UI's key scan prints before "+
				"the description — the sentence would never be shown", hijack)
		}
	}
	// ...and it fits, so none of the above is elided off the end.
	summary := strings.TrimPrefix(rendered, "Allow the agent to use "+ask.tool+"? ")
	if n := len([]rune(summary)); n > escalationSummaryLimit {
		t.Errorf("summary is %d characters, so the bar elides a fact:\n%s", n, summary)
	}

	// The contrast, which keeps this from becoming "every payload carries a
	// paragraph": a verb the UI DOES digest gets no description, because the
	// digest is the sentence and a second one would never be shown.
	before := len(read())
	_ = client.CallTimeout("agent.send", map[string]any{
		"threadId": "t-y", "fromThreadId": "t-a", "text": "fyi",
	}, nil, 30*time.Second)
	asks = read()
	if len(asks) != before+1 {
		t.Fatalf("the plain send asked %d times, want 1", len(asks)-before)
	}
	if _, ok := asks[len(asks)-1].input["description"]; ok {
		t.Error("a digested verb carries a description the bar will never show; " +
			"either the UI grew a branch and uiDigestedVerbs is stale, or this is dead weight")
	}
}

// TestSingleActionApprovalSummaryFitsTheBar: the reason is the AGENT's text, so
// its length is the agent's choice, and so are the thread ids on a forged
// payload. None of them may push the standing-grant clause — the scope of what
// is being granted — past the bar's elision point.
func TestSingleActionApprovalSummaryFitsTheBar(t *testing.T) {
	got := singleActionApprovalSummary(
		strings.Repeat("from", 100), strings.Repeat("target", 100),
		"enable_cowork", strings.Repeat("filler ", 500))
	if n := len([]rune(got)); n > escalationSummaryLimit {
		t.Errorf("summary is %d characters, over the bar's budget:\n%s", n, got)
	}
	if !strings.Contains(got, standingGrantClause(1)) {
		t.Errorf("the standing-grant clause was squeezed out by the agent's own "+
			"text — the scope of the authority is the one part that may not be "+
			"lost:\n%s", got)
	}
	// An unglossed verb is NAMED, never described by invention.
	if got := singleActionApprovalSummary("t-a", "t-x", "future_verb", ""); !strings.Contains(
		got, "future_verb") {
		t.Errorf("a verb with no gloss must still appear by name:\n%s", got)
	}
}
