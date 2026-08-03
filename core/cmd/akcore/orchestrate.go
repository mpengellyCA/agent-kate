package main

// Orchestration RPCs (plan 16 P1): the core side of the Cooperation bridge's
// launch_agent / send_agent / wait_agent / close_agent tools. Workers are real
// Agent Kate threads — full worktree handling, roster visibility, archive
// semantics and the shared permission flow — and every bit of orchestration
// state (parent linkage, turn tracking, cross-subtree grants) lives here, not
// in the bridge.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/safe"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// agentWait bounds. The bridge passes the caller's timeoutSec through; the
// clamp keeps a mistyped value from parking an IPC handler for hours.
const (
	waitDefaultTimeout = 5 * time.Minute
	waitMaxTimeout     = time.Hour
)

// orchGrantTTL is how long one human approval of a cross-subtree (caller →
// target → action) pairing keeps covering later calls (audit F24).
//
// It used to be the WHOLE CORE RUN. Agent Kate is a desktop app people leave
// open for days, so "approved once" meant an approval given on Monday silently
// authorising a send on Thursday — to a thread outside the caller's own worker
// subtree, i.e. exactly the case the prompt exists for. A grant that never
// expires is indistinguishable from no gate at all on a long-lived process, and
// the human has no way to see, let alone withdraw, what they are still holding
// open.
//
// Fifteen minutes is chosen against the work, not the clock: a controller
// coordinating with a sibling crew does it in bursts, so a real collaboration
// re-asks at most a few times an hour, while an injected instruction that lands
// hours after the human walked away gets stopped. Every use SLIDES the window
// (see has), so an active conversation is never interrupted mid-flow.
const orchGrantTTL = 15 * time.Minute

// orchGrants remembers which cross-subtree (caller → target → action) triples
// the human has already approved, so an active collaboration asks once rather
// than on every send — and stops being covered once it goes quiet.
type orchGrants struct {
	mu      sync.Mutex
	granted map[string]time.Time // key -> when the grant stops covering calls
	// now is the clock, injectable so the expiry is testable without sleeping.
	now func() time.Time
}

func newOrchGrants() *orchGrants {
	return &orchGrants{granted: make(map[string]time.Time), now: time.Now}
}

func (g *orchGrants) key(from, target, action string) string {
	return from + "\x00" + target + "\x00" + action
}

// has reports whether a live grant covers this triple, and SLIDES its expiry
// when one does: the window bounds idle time, not the length of a conversation
// the human already approved. An expired entry is deleted on the way past, so
// the map cannot accumulate dead grants over a long run.
func (g *orchGrants) has(from, target, action string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := g.key(from, target, action)
	exp, ok := g.granted[k]
	if !ok {
		return false
	}
	now := g.now()
	// FAIL CLOSED on the boundary: !now.Before(exp) treats "exactly expired" as
	// expired, so a grant is never extended by a tie.
	if !now.Before(exp) {
		delete(g.granted, k)
		return false
	}
	g.granted[k] = now.Add(orchGrantTTL)
	return true
}

func (g *orchGrants) grant(from, target, action string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.granted[g.key(from, target, action)] = g.now().Add(orchGrantTTL)
}

// forgetThread drops every grant that names the thread — as granter or as
// target. A discarded thread's approvals must not linger to silently cover a
// future thread that happens to reuse the id.
func (g *orchGrants) forgetThread(threadID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.granted {
		parts := strings.SplitN(k, "\x00", 3)
		if len(parts) == 3 && (parts[0] == threadID || parts[1] == threadID) {
			delete(g.granted, k)
		}
	}
}

// inSubtree reports whether target lies in caller's own subtree: the caller
// itself, or a thread whose ParentThreadID chain reaches the caller. The
// depth cap guards against a cyclic chain in a hand-edited threads.json.
func (d handlerDeps) inSubtree(callerID, targetID string) bool {
	id := targetID
	for depth := 0; depth < 32; depth++ {
		if id == callerID {
			return true
		}
		rec, ok := d.sessions.Get(id)
		if !ok || rec.ParentThreadID == "" {
			return false
		}
		id = rec.ParentThreadID
	}
	return false
}

// requireCallerThread binds a self-declared `fromThreadId` to the connection
// that sent it. Every per-thread handler that takes one MUST run this before
// authorizeAgentTarget, because authorizeAgentTarget measures the relationship
// between two ids and proves nothing about WHO is speaking:
//
//   - fromID == "" means "the UI" to it, and it returns nil — so a bridge that
//     simply OMITS fromThreadId skipped the whole gate, human approval included;
//   - a bridge that names a DIFFERENT thread as fromID is measured as that
//     thread — so naming its own controller bought it the controller's subtree,
//     and with it every worker in the arena that controller launched.
//
// Neither needed a secret, a grant or a click: the parameter was the authority.
// Here the caller is established first, from the connection identity that
// bridge.identify authenticated (audit F13), and only then is the relationship
// measured.
//
// The UI passes with any fromID (including none): it is the human, the one
// authority these gates exist to consult. A connection that is neither the UI
// nor a bridge bound to fromID is refused — fail closed, including for a caller
// with no connection identity at all.
func requireCallerThread(d handlerDeps, ctx context.Context, fromThreadID string) error {
	if d.srv.RequireUI(ctx) {
		return nil
	}
	if fromThreadID == "" {
		return ipc.Errorf(ipc.CodeInvalidParams,
			"fromThreadId is required: only the Agent Kate window may act on a "+
				"thread without naming the thread it is acting for")
	}
	if ok, reason := d.srv.RequireBridge(ctx, fromThreadID); !ok {
		return ipc.Errorf(ipc.CodeInvalidParams,
			"this connection may not act for thread "+fromThreadID+": "+reason)
	}
	return nil
}

// --- what the human is actually asked, for a COMPOSITE ----------------------

// orchActionGloss says, in plain words, what each cooperation verb does TO the
// target thread. The verb names are the grant ledger's keys and the MCP tool
// names; they are not a sentence a human should have to decode at a decision
// point, and "wait_agent" in particular hides the only thing about it that
// matters — that it reads the other thread's reply into the caller's context.
var orchActionGloss = map[string]string{
	"send_agent":    "send it a message",
	"wait_agent":    "wait for it, and READ its reply",
	"close_agent":   "stop it and archive it",
	"discard_agent": "delete it, worktree and all",
	// enable_cowork reaches this table through cowork.requestEnable, which
	// gates a cross-subtree ask like any other verb. Its name says nothing
	// about the desktop, and what it widens is the one authority that leaves
	// the workspace entirely.
	"enable_cowork": "let it see and control your desktop",
}

func actionGloss(action string) string {
	if g, ok := orchActionGloss[action]; ok {
		return g
	}
	// A verb with no gloss is named rather than described. Better a bare name
	// than an invented description of what it does.
	return "perform " + clipTo(action, 40)
}

// compositeApprovalTool is the name the permission bar prints in "Allow the
// agent to use <name>?" for a multi-action ask, and — load-bearing — the name
// the UI's summariser dispatches on.
//
// SECURITY (audit F35 pass 4): it is deliberately NOT
// "mcp__cooperation__send_agent". Under that name ui/src/AgentChatHelpers.cpp
// digests the payload with its `send_agent` branch, which prints the target and
// the message and NOTHING ELSE — so the extra action the same click grants
// (`alsoGrant`, consumed in authorizeAgentTarget below) never reached the human
// at all. The composite was one decision, correctly; it was one decision
// described as half of itself, which is the same defect class as F27 and F32: a
// recorded authority wider than the one displayed. Under a name the digest does
// not claim, permSummary falls through to its generic key scan (file_path,
// path, pattern, description) and prints our `description` verbatim. The keys
// this payload uses must therefore avoid file_path/path/pattern, and
// `description` must stay present — TestCompositeApprovalPromptNamesBothActions
// pins both against a port of the UI's own summariser. Same trick, and same
// reason, as launchApprovalTool in authority.go.
func compositeApprovalTool(action string, alsoGrant []string) string {
	names := append([]string{clipTo(action, 40)}, alsoGrant...)
	for i := range names {
		names[i] = clipTo(names[i], 40)
	}
	return strings.Join(names, " + ") + " (one approval, several actions)"
}

// humanDuration renders a window for the sentence a human reads at a decision
// point. It exists because the obvious spelling LIES.
//
// standingGrantClause used to say strconv.Itoa(int(orchGrantTTL.Minutes())),
// which TRUNCATES: shorten the window to 45 seconds — the direction a security
// review would push it — and the approval dialog reads "stay allowed for 0 min",
// i.e. "this expires immediately", for a grant that in fact stands for the next
// three quarters of a minute and slides on every use. This is the one place in
// the product where a wrong number directly misinforms a security decision, and
// the failure mode is silent: the constant changes, the sentence keeps
// compiling, and the human reads a number nobody chose.
//
// So it is honest for EVERY value the constant can take, not just round
// minutes, and it never rounds a non-zero window down to zero of anything.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		// Sub-second is not a plausible grant window, but "0 sec" would be the
		// same lie in a smaller unit, so it says what it is.
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	case d < time.Minute:
		return strconv.FormatInt(int64(d.Seconds()), 10) + " sec"
	case d < time.Hour:
		out := strconv.FormatInt(int64(d.Minutes()), 10) + " min"
		if sec := int64(d.Seconds()) % 60; sec != 0 {
			out += " " + strconv.FormatInt(sec, 10) + " sec"
		}
		return out
	default:
		out := strconv.FormatInt(int64(d.Hours()), 10) + " h"
		if min := int64(d.Minutes()) % 60; min != 0 {
			out += " " + strconv.FormatInt(min, 10) + " min"
		}
		return out
	}
}

// standingGrantClause says the thing the recorded authority does and the
// displayed authority used to leave out (audit F35 pass 5).
//
// Approving does not authorise ONE exchange. authorizeAgentTarget writes a
// grant per (caller, target, action) with a TTL, `has` SLIDES that TTL on every
// use, and the composite writes one for each named action INDEPENDENTLY — so a
// single click on "send and wait" also buys unlimited later `wait_agent` calls
// on that thread, standalone, for as long as the caller keeps using them at
// least once per window. That is the F35 read channel, re-opened by consent the
// human did not know they were giving: they were shown a one-off exchange and
// the ledger recorded a standing, self-renewing pair of permissions.
//
// The clause is derived from orchGrantTTL rather than spelling "15" out, so a
// change to the window cannot leave the dialog quoting the old number — the
// exact drift that turns an honest prompt into a false one. `actions` is the
// number of grants this one click writes, because "both" is only true of two:
// a single-action ask (enable_cowork) and a hypothetical third verb would each
// have been described by a sentence that was wrong about its own subject.
func standingGrantClause(actions int) string {
	subject := "it stays"
	switch {
	case actions == 2:
		subject = "both stay"
	case actions > 2:
		subject = "all " + strconv.Itoa(actions) + " stay"
	}
	return "Then " + subject + " allowed for " + humanDuration(orchGrantTTL) +
		", renewed by use."
}

// compositeApprovalSummary is the sentence the human decides on when one call
// asks for several actions. Built to the bar's 240-character budget rather than
// trimmed by it (escalationSummaryLimit, authority.go), facts first:
//
//  1. that approving covers MORE THAN ONE action, counted;
//  2. what each of them does, in plain words, in the order they happen;
//  3. which thread they happen to, and who is asking;
//  4. that the approval STANDS — see standingGrantClause;
//  5. the message body — the agent's own words, and therefore last and fitted.
//
// 4 is unconditional and 5 is not: the message is attacker-controlled text and
// the standing-grant clause is the scope of the authority, so when the budget
// runs out it is the agent's prose that goes. The wording of 1-3 is kept dense
// for exactly that reason — every character spent on phrasing is a character
// of the agent's own words the human does not get to read.
func compositeApprovalSummary(fromID, targetID string, actions []string, message string) string {
	const (
		idCap          = 40
		messageReserve = 32
	)
	glossed := make([]string, 0, len(actions))
	for i, a := range actions {
		glossed = append(glossed, fmt.Sprintf("(%d) %s", i+1, actionGloss(a)))
	}
	// The two clauses that state the AUTHORITY are budgeted first, and
	// everything else is fitted to what they leave. Written the other way round
	// — ids capped at idCap, scope clause appended last — a forged 40-character
	// fromThreadId, or a fifth cooperation verb, pushed the standing-grant
	// sentence onto the wrong side of the final clipTo and the human was back to
	// reading a one-off exchange. The facts must not be losable by an input the
	// asker chooses.
	const idFrame = len("Thread , outside 's own workers.")
	standing := standingGrantClause(len(actions))
	lead := clipTo(fmt.Sprintf("Approve = %d actions, not 1: %s.",
		len(actions), strings.Join(glossed, "; ")),
		escalationSummaryLimit-len(standing)-idFrame-2*8-2)
	idRoom := (escalationSummaryLimit - len(lead) - len(standing) - idFrame - 2) / 2
	if idRoom > idCap {
		idRoom = idCap
	}
	if idRoom < 8 {
		idRoom = 8
	}
	parts := []string{
		lead,
		"Thread " + clipTo(targetID, idRoom) + ", outside " +
			clipTo(fromID, idRoom) + "'s own workers.",
		standing,
	}
	used := len(strings.Join(parts, " "))
	if msg := strings.TrimSpace(message); msg != "" {
		room := escalationSummaryLimit - used - len(" Message: ")
		if room >= messageReserve {
			parts = append(parts, "Message: "+clipTo(msg, room))
		}
	}
	// Backstop, so the bar's own elision never gets to choose what the human
	// does not read: with more actions than today's two the lead alone could
	// outgrow the budget, and the cut has to land on the agent's text, which is
	// last, rather than wherever 240 characters happens to fall.
	return clipTo(strings.Join(parts, " "), escalationSummaryLimit)
}

// uiDigestedVerbs are the cooperation verbs ui/src/AgentChatHelpers.cpp's
// mcpSummary has a branch for. For those, "mcp__cooperation__<verb>" IS the
// human-readable prompt: the bar prints the target (and, for a send, the
// message) from the payload's own fields.
//
// A verb that is NOT in here renders through permSummary's last resort, which
// dumps the whole input object as compact JSON into a 240-character bar — so
// the human deciding whether to widen an authority reads
// `{"grantRenewedByUse":true,"grantTtlMinutes":15,"grantedActions":["enab…`
// and never reaches the facts. enable_cowork, the ask that hands an agent the
// screen, keyboard and pointer, was exactly that case: no branch, no sentence,
// and the structured grant-scope fields added in pass 5 spending the budget
// that the facts needed.
//
// Keeping the list here rather than "everything has a description" is
// deliberate: a description on a digested verb is dead weight the digest never
// shows, and TestUndigestedVerbsCarryASentence pins the two halves against each
// other. Update it when the C++ grows or loses a branch.
var uiDigestedVerbs = map[string]bool{
	"send_agent":    true,
	"wait_agent":    true,
	"close_agent":   true,
	"discard_agent": true,
}

// singleActionApprovalSummary is compositeApprovalSummary's counterpart for the
// one-action asks the UI has no digest for.
//
// It reaches the human the same way the composite does — permSummary's generic
// key scan, which reads file_path, path, pattern and THEN description — so the
// same constraint applies: a detail key called file_path, path or pattern would
// be printed in place of this sentence. TestUndigestedVerbsCarryASentence pins
// that none of the three is in the payload.
//
// Same budget discipline as the composite, same order:
// what it does, to whom, on whose behalf, that the approval STANDS — and the
// agent's own words (its stated reason) last, because they are the part an
// attacker chooses and therefore the part that may be cut.
func singleActionApprovalSummary(fromID, targetID, action, reason string) string {
	const (
		idCap         = 40
		reasonReserve = 24
	)
	standing := standingGrantClause(1)
	const idFrame = len("Approve =  on thread , outside 's own workers.")
	lead := clipTo("Approve = "+actionGloss(action), escalationSummaryLimit-
		len(standing)-idFrame-2*8-2)
	idRoom := (escalationSummaryLimit - len(lead) - len(standing) - idFrame - 2) / 2
	if idRoom > idCap {
		idRoom = idCap
	}
	if idRoom < 8 {
		idRoom = 8
	}
	parts := []string{
		lead + " on thread " + clipTo(targetID, idRoom) + ", outside " +
			clipTo(fromID, idRoom) + "'s own workers.",
		standing,
	}
	used := len(strings.Join(parts, " "))
	if why := strings.TrimSpace(reason); why != "" {
		room := escalationSummaryLimit - used - len(" Its reason: ")
		if room >= reasonReserve {
			parts = append(parts, "Its reason: "+clipTo(why, room))
		}
	}
	return clipTo(strings.Join(parts, " "), escalationSummaryLimit)
}

// authorizeAgentTarget gates one agent acting on another thread. UI-driven
// calls (empty fromID) are never gated; a target inside the caller's own
// subtree is always allowed; anything else needs one human approval per
// (caller, target, action), through the same permission flow every gated tool
// uses — so the ask shows up in the caller's panel like any other approval.
//
// fromID must already have been bound to the calling connection by
// requireCallerThread. This function TRUSTS it: on its own it is arithmetic on
// two thread ids, not an authentication.
//
// alsoGrant names the FURTHER actions this one call is going to perform on the
// same target, so a composite operation costs ONE human decision (audit F35
// pass 3). send_agent(wait:true) is the case that forced it: it asks for
// `send_agent`, delivers, and then waits — which used to ask a second time, for
// `wait_agent`, because a grant keys on (from, target, ACTION). Two prompts for
// one operation trains the human to click through, and the second prompt came
// AFTER delivery, so a human who approved the send and then declined the wait
// had the message delivered and the reply thrown away — the worst of both
// answers. Every named action must be covered before the operation starts;
// otherwise the human is asked once, up front, for the whole of it, and all of
// them are granted together. Note this NEVER weakens a standalone verb: a bare
// wait_agent names no extras and is gated exactly as F35 left it.
func (d handlerDeps) authorizeAgentTarget(fromID, targetID, action string,
	detail map[string]any, alsoGrant ...string) error {
	if fromID == "" {
		return nil
	}
	if d.inSubtree(fromID, targetID) {
		return nil
	}
	// has() slides a live grant's window, so ask for every action the call will
	// take before deciding — a partially covered composite must still reach the
	// human for the part that is not covered.
	covered := d.orchGrants.has(fromID, targetID, action)
	for _, extra := range alsoGrant {
		if !d.orchGrants.has(fromID, targetID, extra) {
			covered = false
		}
	}
	if covered {
		return nil
	}
	// The scope of what is being RECORDED, as structured fields, on every ask —
	// composite or not (audit F35 pass 5). The composite states it in words too
	// (compositeApprovalSummary), because a composite is rendered by the generic
	// summariser and there is room for a sentence. A single-action ask keeps the
	// digested `mcp__cooperation__<verb>` name, whose UI branch prints the target
	// and the message and nothing else, so for those these fields are the only
	// place the standing grant exists at all — a KNOWN gap on the display side,
	// not a closed one: a bare send_agent still LOOKS one-off.
	//
	// grantTtlMinutes is NOT int(Minutes()): the truncation that made
	// standingGrantClause say "0 min" for a 45-second window said "0" here too,
	// and a surface reading the field would have rendered the same falsehood.
	input := map[string]any{
		"targetThreadId":    targetID,
		"grantTtlMinutes":   orchGrantTTL.Minutes(),
		"grantRenewedByUse": true,
		"grantedActions":    append([]string{action}, alsoGrant...),
	}
	for k, v := range detail {
		input[k] = v
	}
	// The human is told what the WHOLE operation does, not just its first verb.
	//
	// A single-action ask keeps the tool name the UI digests, because for those
	// the digest already IS the whole truth: the verb is the name on the bar and
	// the target (and, for a send, the message) is the summary. A composite has
	// nowhere in that digest for its second action to appear, so it moves to the
	// generic renderer with a sentence we build (audit F35 pass 4).
	//
	// ...and a verb the UI does not digest AT ALL has the same problem in a
	// worse form: no branch means the bar prints this payload as raw JSON, so
	// the grant-scope fields above are spending a budget the facts needed and
	// the human reads brace-and-quote soup instead of "let it see and control
	// your desktop". Those get a written sentence too (uiDigestedVerbs).
	promptTool := "mcp__cooperation__" + action
	switch {
	case len(alsoGrant) > 0:
		input["alsoPerforms"] = alsoGrant
		msg, _ := detail["message"].(string)
		input["description"] = compositeApprovalSummary(fromID, targetID,
			append([]string{action}, alsoGrant...), msg)
		promptTool = compositeApprovalTool(action, alsoGrant)
	case !uiDigestedVerbs[action]:
		why, _ := detail["reason"].(string)
		input["description"] = singleActionApprovalSummary(fromID, targetID, action, why)
	}
	rawInput, _ := json.Marshal(input)
	dec, ok := askHumanPermission(d.srv, d.broker, fromID, promptTool, rawInput)
	if !ok || !dec.Allow {
		why := "(the target is outside your own worker subtree)"
		if !d.srv.HasUI() {
			// Fail-closed, but say which closed door this is: no Agent Kate
			// window is connected, so there was nobody to ask (audit F35
			// pass 3). Refused in about a millisecond rather than after the
			// 8-minute permission window.
			why = "(no Agent Kate window is connected, so nobody could be asked)"
		}
		return ipc.Errorf(ipc.CodeInvalidParams,
			action+" on thread "+targetID+" was not approved by the human "+why)
	}
	d.orchGrants.grant(fromID, targetID, action)
	for _, extra := range alsoGrant {
		d.orchGrants.grant(fromID, targetID, extra)
	}
	return nil
}

// unappliedOptions compares what a launch requested against what the harness
// reports it applied, for the applied-truth half of launch_agent's contract:
// anything requested but not applied is named, never silently dropped.
func unappliedOptions(requested map[string]string, launched harness.Launched) []map[string]string {
	applied := map[string]string{
		"model":          launched.Model,
		"effort":         launched.Effort,
		"permissionMode": launched.PermissionMode,
	}
	// Deterministic order for tests and readable tool output.
	var out []map[string]string
	for _, opt := range []string{"model", "effort", "permissionMode"} {
		want := strings.TrimSpace(requested[opt])
		if want == "" || want == applied[opt] {
			continue
		}
		out = append(out, map[string]string{
			"option": opt, "requested": want, "applied": applied[opt],
		})
	}
	return out
}

// unappliedPersona is the same applied-truth pass for the persona channels
// (plan 16 P3). These entries carry a "reason" instead of a requested/applied
// pair: there is no downgraded value to report, only what was lost and why.
func unappliedPersona(systemPrompt string, profiles []harness.AgentProfile,
	launched harness.Launched, descriptor harness.HarnessDescriptor) []map[string]string {
	var out []map[string]string
	if strings.TrimSpace(systemPrompt) != "" && !launched.SystemPromptApplied {
		// The adapter's own reason wins when it has one (an oversize prompt is
		// not a missing capability); otherwise the channel is simply absent.
		reason := launched.SystemPromptUnapplied
		if reason == "" {
			reason = unsupportedDetail("a custom system prompt", descriptor) +
				"; put the persona in the worker's opening prompt instead"
		}
		out = append(out, map[string]string{
			"option": "system_prompt",
			"reason": reason,
		})
	}
	for _, a := range launched.Agents {
		if a.Applied && len(a.Unapplied) == 0 {
			continue
		}
		reason := strings.Join(a.Unapplied, "; ")
		if reason == "" {
			// A verdict with no explanation. Guessing "unsupported" would be
			// wrong for a harness that HAS the capability and lost the profile
			// for some other reason, so say only what is known.
			reason = "not applied; the harness gave no reason"
		}
		out = append(out, map[string]string{
			"option": "agents[" + profileLabel(a.Name) + "]",
			"reason": reason,
		})
	}
	// Backstop: an adapter that reports NOTHING for a requested profile would
	// otherwise drop it silently, which is exactly what applied-truth forbids.
	// Launched.Agents carries one entry per request, so anything past its end
	// is unaccounted for.
	for i := len(launched.Agents); i < len(profiles); i++ {
		out = append(out, map[string]string{
			"option": "agents[" + profileLabel(profiles[i].Name) + "]",
			"reason": descriptor.DisplayName + " did not report whether this subagent " +
				"profile was applied; assume it was not",
		})
	}
	return out
}

// unappliedSweepReport renders the harness's own UnappliedOptions (the plan 16
// P6 list-valued launch options) into the same reason-carrying shape. The
// adapter owns the wording, because only it knows why its CLI cannot express
// the option.
func unappliedSweepReport(launched harness.Launched) []map[string]string {
	var out []map[string]string
	for _, u := range launched.UnappliedOptions {
		out = append(out, map[string]string{"option": u.Option, "reason": u.Reason})
	}
	return out
}

// profileLabel names a profile in applied-truth output, standing in for the
// nameless (which no harness can register anyway).
func profileLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(unnamed)"
	}
	return name
}

func registerOrchestrationHandlers(d handlerDeps) {
	// The fan-out reservation ledger is not optional: without it every worker
	// launch fails closed (authority.go). Filled in here as well as in run.go so
	// that no assembly of handlerDeps can accidentally register a launch path
	// with no ledger behind it — the handlers close over this copy.
	if d.workerSlots == nil {
		d.workerSlots = newWorkerSlots()
	}
	// agent.wait blocks until the thread is idle (no turn in flight, or the
	// process ended) or the timeout fires, and returns the thread's last
	// assistant text. Backed by the turn tracker's broadcast wait — the IPC
	// handler simply blocks; no polling anywhere.
	//
	// SECURITY (audit F35): wait_agent is a READ, and it was the one
	// orchestration handler that never asked who was reading. It hands back the
	// target thread's last assistant message, so an agent that named any id in
	// any workspace — the human's own private threads included — pulled that
	// text into its own context with nobody in the chain. Its siblings
	// agent.send and agent.stopClose have bound the caller since F13; this one
	// now uses exactly the same pair, so a target inside the caller's own
	// worker subtree stays free and anything else costs one human approval
	// (remembered per (caller, target, wait_agent) by the grant TTL).
	d.srv.Handle("agent.wait", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID     string `json:"threadId"`
			FromThreadID string `json:"fromThreadId"`
			TimeoutSec   int    `json:"timeoutSec"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
		}
		// WHO is waiting, before WHOM they may wait on — in that order, because
		// authorizeAgentTarget is arithmetic on two ids and proves nothing
		// about the speaker.
		if err := requireCallerThread(d, ctx, p.FromThreadID); err != nil {
			return nil, err
		}
		// Existence before approval, deliberately: a thread that does not exist
		// is a typo, not a decision, and putting the ask second keeps the human
		// from being prompted about ids that name nothing.
		//
		// The trade this makes is REAL and is accepted with its eyes open
		// (audit F35 pass 3): the ordering is an oracle, so an unauthorised
		// caller can tell a nonexistent id (answered instantly, silently) from
		// a live one (a visible prompt in the caller's own panel). What it buys
		// the attacker is thread-id enumeration; what it costs them is one
		// human-visible approval dialog PER POSITIVE HIT, in their own panel,
		// naming them. Thread ids are not secrets — agent.list hands the roster
		// to any bridge by design — so the leak is small, while the alternative
		// (ask first, then answer "unknown thread") would train the human to
		// dismiss orchestration prompts that turn out to mean nothing, which
		// devalues the one prompt that does. If thread ids ever become
		// capability-bearing, this ordering must be revisited.
		if _, ok := d.sessions.Get(p.ThreadID); !ok && !d.agentRunning(p.ThreadID) {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
		}
		if err := d.authorizeAgentTarget(p.FromThreadID, p.ThreadID,
			"wait_agent", nil); err != nil {
			return nil, err
		}
		timeout := waitDefaultTimeout
		if p.TimeoutSec > 0 {
			timeout = time.Duration(p.TimeoutSec) * time.Second
			if timeout > waitMaxTimeout {
				timeout = waitMaxTimeout
			}
		}
		lastText, timedOut := d.turns.Wait(ctx, p.ThreadID, timeout)
		// A cancelled context means the caller (a bridge whose agent died, a
		// disconnecting client) is gone — report that rather than a timeout.
		if err := ctx.Err(); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, "wait cancelled: "+err.Error())
		}
		status := "idle"
		switch {
		case timedOut:
			status = "timeout"
		case !d.agentRunning(p.ThreadID):
			// Idle because the process is gone (finished and stopped, crashed,
			// or launch failed) — the thread is dormant/ended, not waiting.
			status = "exited"
		}
		return map[string]any{"status": status, "lastText": lastText}, nil
	})

	// agent.launchWorker starts a real Agent Kate thread on behalf of another
	// thread (the Cooperation bridge's launch_agent), synchronously — the
	// caller gets applied-truth back, not a promise. The worker is parented to
	// the launcher and appears in the roster like any human-started agent.
	d.srv.Handle("agent.launchWorker", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ParentThreadID string                 `json:"parentThreadId"`
			Backend        string                 `json:"backend"`
			Model          string                 `json:"model"`
			Prompt         string                 `json:"prompt"`
			Title          string                 `json:"title"`
			Isolation      string                 `json:"isolation"`
			PermissionMode string                 `json:"permissionMode"`
			Effort         string                 `json:"effort"`
			SystemPrompt   string                 `json:"systemPrompt"`
			Agents         []harness.AgentProfile `json:"agents"`
			Cowork         bool                   `json:"cowork"`
			// The per-thread restriction fields. Not advertised by the
			// launch_agent tool — an agent has no way to ask for them — but
			// measured anyway, because this handler is reachable from the
			// socket and "the bridge doesn't send it" is not a gate. nil means
			// "inherit the parent's" (authority.go, inheritRestrictions).
			DisallowedTools []string `json:"disallowedTools"`
			AddDirs         []string `json:"addDirs"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ParentThreadID == "" || strings.TrimSpace(p.Prompt) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"parentThreadId and prompt are required")
		}
		// Bind the caller to the thread it claims to be launching FROM (audit
		// F1/§4). The whole gate below measures the requested authority against
		// the PARENT's — which measures nothing at all if any connection can
		// name any parent. A bridge may launch only from its own thread; the UI
		// passes because a human at the New Agent dialog is the authority the
		// gate exists to consult.
		if err := requireUIOrOwnBridge(d, ctx, p.ParentThreadID); err != nil {
			return nil, err
		}
		parent, ok := d.sessions.Get(p.ParentThreadID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"unknown launching thread "+p.ParentThreadID)
		}
		// Empty backend = the caller's own harness (not the registry default:
		// a kimi controller's unqualified worker is another kimi).
		backend := p.Backend
		if backend == "" {
			backend = parent.Backend
		}
		h, ok := d.harnesses.Get(backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown backend "+backend)
		}
		descriptor := h.Descriptor()
		switch p.Isolation {
		case "", worktree.ModeAuto, worktree.ModeIsolated, worktree.ModeWorkspace:
		default:
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"isolation must be auto, isolated or workspace")
		}

		// The authority gate (audit F1): a reserved worker slot, plus one human
		// approval when this launch would give the worker more authority than
		// the launcher holds — a more permissive mode, the human's main
		// checkout to work in, or fewer restrictions than bind the launcher.
		// Asked BEFORE anything is created, so a refusal leaves no worktree, no
		// record and no process behind. See authority.go for the ordering, the
		// caps and the per-field reasoning.
		req := workerLaunchRequest{
			ParentThreadID:  p.ParentThreadID,
			Backend:         backend,
			PermissionMode:  p.PermissionMode,
			Isolation:       p.Isolation,
			Title:           p.Title,
			Prompt:          p.Prompt,
			DisallowedTools: p.DisallowedTools,
			AddDirs:         p.AddDirs,
		}
		// The engine's own default is only consulted for a launch that names NO
		// mode — the only case whose rank it decides — so a specified mode never
		// pays for the harness's option probe.
		engineDefault := ""
		if strings.TrimSpace(p.PermissionMode) == "" {
			engineDefault = engineDefaultPermissionMode(h, descriptor)
		}
		release, err := d.authorizeWorkerLaunch(parent, descriptor, engineDefault, req)
		if err != nil {
			return nil, err
		}
		// Hold the slot until the launch is over, whichever way it goes: from
		// then on the worker's own persisted record is what the cap counts.
		defer release()
		// What the worker is actually restricted BY: the parent's fields unless
		// the human approved something looser above.
		disallowedTools, addDirs, _ := inheritRestrictions(parent, req)

		// Desktop access for a worker follows the same rule as enable_cowork:
		// an agent may ask, the human decides. Asked BEFORE the launch, so a
		// refusal costs nothing and the worker simply comes up without it —
		// reported NOT APPLIED rather than silently dropped.
		cowork := false
		coworkWhyNot := ""
		switch {
		case !p.Cowork:
		case !descriptor.Supports(harness.OperationCowork):
			coworkWhyNot = unsupportedDetail("desktop cowork", descriptor)
		case askCoworkEnable(d, p.ParentThreadID, "", orDefault(p.Title, "a new worker"),
			capText(firstLine(p.Prompt))):
			cowork = true
		default:
			coworkWhyNot = "the human did not approve desktop access for this worker"
		}

		threadID := agent.NewThreadID()
		sessionID := ""
		if descriptor.Supports(harness.OperationMintSessionID) {
			sessionID = session.NewID()
		}
		// The opening prompt is a turn; queue it before the launch so a
		// wait_agent racing the start never sees a false idle. A failed launch
		// emits the "error" lifecycle, which clears it.
		d.turns.TurnQueued(threadID)
		launched, wt, err := launchThread(d, h, threadID, sessionID, agentStartParams{
			// Workers root in the PARENT'S PROJECT (never its worktree), so an
			// isolated worker gets a sibling worktree of its controller's.
			WorkspacePath:  parent.Project,
			Prompt:         p.Prompt,
			PermissionMode: p.PermissionMode,
			Effort:         p.Effort,
			Model:          p.Model,
			Backend:        descriptor.ID,
			Isolation:      p.Isolation,
			SystemPrompt:   p.SystemPrompt,
			Agents:         p.Agents,
			CoworkEnabled:  cowork,
			// Restrictions travel DOWN the tree: a worker cannot shed the deny
			// list or the extra roots its launcher runs under, and cannot shed
			// the launcher's MCP isolation or spend ceiling either. Before this,
			// none of these were set at all and every worker escaped them.
			DisallowedTools: disallowedTools,
			AddDirs:         addDirs,
			StrictMCPConfig: parent.StrictMCPConfig,
			MaxBudgetUSD:    parent.MaxBudgetUSD,
		}, launchMeta{ParentThreadID: p.ParentThreadID, Title: p.Title})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// An approved desktop worker asks for the OS-level permission right
		// away, so its first desktop action does not stall on a dialog.
		if cowork && d.cowork != nil && d.cowork.Available() {
			safe.Go("cowork.preflight", func() {
				_, _ = coworkPreflight(context.Background(), d, threadID, true)
			})
		}
		// First worker promotes the launcher to controller; a worker that
		// launches sub-workers keeps its own worker role (the parent chain
		// carries the structure).
		if parent.Role == "" {
			_ = d.sessions.UpdateQuiet(p.ParentThreadID, func(r *session.Record) {
				if r.Role == "" {
					r.Role = session.RoleController
				}
			})
		}
		// Applied-truth: the downgraded options first, then the persona
		// channels — one flat list the bridge renders verbatim.
		unapplied := unappliedOptions(map[string]string{
			"model":          p.Model,
			"effort":         p.Effort,
			"permissionMode": p.PermissionMode,
		}, launched)
		unapplied = append(unapplied,
			unappliedPersona(p.SystemPrompt, p.Agents, launched, descriptor)...)
		unapplied = append(unapplied, unappliedSweepReport(launched)...)
		if coworkWhyNot != "" {
			unapplied = append(unapplied, map[string]string{
				"option": "cowork", "requested": "true", "reason": coworkWhyNot,
			})
		}
		var appliedAgents []string
		for _, a := range launched.Agents {
			if a.Applied {
				appliedAgents = append(appliedAgents, a.Name)
			}
		}
		return map[string]any{
			"threadId":  threadID,
			"sessionId": launched.SessionID,
			"backend":   descriptor.ID,
			"isolated":  wt.Isolated,
			"branch":    wt.Branch,
			"applied": map[string]string{
				"model":          launched.Model,
				"effort":         launched.Effort,
				"permissionMode": launched.PermissionMode,
			},
			"systemPromptApplied": launched.SystemPromptApplied,
			"appliedAgents":       appliedAgents,
			"unapplied":           unapplied,
		}, nil
	})
}
