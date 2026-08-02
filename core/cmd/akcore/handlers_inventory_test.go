package main

// The authorisation INVENTORY (audit F34/F36 pass 3, 2026-08-01).
//
// Pass 2 pinned who may call each UI-only handler with a hand-written list of
// method names. A hand-written list only ever describes the handlers somebody
// remembered to put in it, so it said nothing at all about search.code,
// session.browse, session.listThreads, cleanup.restore,
// agent.setCompactStrategy or app.shutdown — six privileged, ungated handlers
// sitting in the same files as the ones being audited, right through a round
// whose whole subject was "which handlers are gated". A list that cannot notice
// what is missing from it is not an inventory.
//
// This one enumerates the REGISTRY (ipc.Server.Methods) and requires every
// registered method to be one of two things:
//
//   - named in agentReachable below, with a written reason — a deliberate,
//     reviewed decision that an agent's own bridge may call it; or
//   - refused to an agent bridge, with the UI-only refusal.
//
// A handler added without a decision is in neither, and the build breaks. That
// is the entire point: the failure mode this test exists for is not "somebody
// wrote a bad gate", it is "somebody wrote a new handler and nobody thought
// about the gate at all".

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentkate/internal/ipc"
	"agentkate/internal/session"
)

// agentReachable is the reviewed set: methods an agent's own (authenticated,
// thread-bound) bridge connection is MEANT to reach, with the reason. Adding an
// entry here is the decision — it should be as hard to write as it is to
// justify, so every one of them says what protects it INSTEAD of requireUIWindow.
//
// Pass 3 left nineteen entries prefixed "DEFERRED": reachable, not agent-facing
// by design, and not gated because the caller survey was outside that round's
// file set. Pass 4 finished the survey (every one of the nineteen is called
// only from ui/src) and gated all of them, so the prefix is gone and this map
// contains only decisions. It is worth saying why: DEFERRED was a to-do list
// wearing the costume of a decision, and a test that accepts one cannot tell an
// approval from a postponement. There is no DEFERRED tier any more — a method
// is either here with a reason, or it is refused.
var agentReachable = map[string]string{
	// --- identity ----------------------------------------------------------
	"handshake": "the UI-role claim itself; MarkUI refuses a bridge connection",
	"bridge.identify": "the bridge's own authenticated door — it redeems the " +
		"per-launch secret akcore handed that thread (F13)",

	// --- orchestration: bound to the calling thread, not to the UI ----------
	"agent.send":         "requireCallerThread + authorizeAgentTarget (F13/F35)",
	"agent.wait":         "requireCallerThread + authorizeAgentTarget (F35)",
	"agent.stopClose":    "requireCallerThread + authorizeAgentTarget (F13)",
	"agent.discard":      "requireCallerThread + authorizeAgentTarget (F13)",
	"agent.launchWorker": "requireUIOrOwnBridge + the launch authority gate (F1)",
	"permission.request": "RequireBridge binds the asking thread; the UI never calls it (F36)",
	"agent.list": "the deliberately reduced roster projection the orchestration " +
		"tools need — ids, titles, status, worktree, parent linkage — built " +
		"field by field and pinned by TestAgentListProjectionIsNarrow. Contrast " +
		"session.listThreads, which is the whole record and is gated",

	// --- the cooperation board: shared by design ---------------------------
	"coop.setOpenFiles":  "the cooperation board is a shared agent workspace",
	"coop.listOpenFiles": "the cooperation board is a shared agent workspace",
	"coop.postNote":      "the cooperation board is a shared agent workspace",
	"coop.readNotes":     "the cooperation board is a shared agent workspace",
	"coop.setPresence":   "the cooperation board is a shared agent workspace",
	"coop.getPresence":   "the cooperation board is a shared agent workspace",
	"coop.claimFile":     "the cooperation board is a shared agent workspace",
	"coop.releaseFile":   "the cooperation board is a shared agent workspace",
	"coop.requestReview": "the cooperation board is a shared agent workspace",
	"coop.listReviews":   "the cooperation board is a shared agent workspace",
	"coop.getState":      "the cooperation board is a shared agent workspace",

	// --- Cowork: gated by desktop CONSENT, which is a stronger gate ---------
	// These are the agent's desktop tools. They are not UI-only — the whole
	// point is that an agent drives them — and each one runs the per-thread
	// enablement check plus a human grant (cowork.go). Not this test's subject.
	"cowork.status":              "per-thread Cowork enablement + human grant",
	"cowork.threadState":         "requireUIOrOwnBridge; the bridge asks about its OWN thread",
	"cowork.toolsListed":         "the bridge reporting what it advertised, for its own thread",
	"cowork.requestEnable":       "the agent ASKING the human for desktop access (plan 18)",
	"cowork.listWindows":         "per-thread Cowork enablement + human grant",
	"cowork.listElements":        "per-thread Cowork enablement + human grant",
	"cowork.readText":            "per-thread Cowork enablement + human grant",
	"cowork.screenshot":          "per-thread Cowork enablement + human grant",
	"cowork.activateElement":     "per-thread Cowork enablement + human grant",
	"cowork.setElementText":      "per-thread Cowork enablement + human grant",
	"cowork.injectInput":         "per-thread Cowork enablement + human grant",
	"cowork.playInput":           "per-thread Cowork enablement + human grant",
	"cowork.movePointer":         "per-thread Cowork enablement + human grant",
	"cowork.movePointerRelative": "per-thread Cowork enablement + human grant",
	"cowork.pointerClick":        "per-thread Cowork enablement + human grant",
	"cowork.pointerClickElement": "per-thread Cowork enablement + human grant",
	"cowork.pointerDrag":         "per-thread Cowork enablement + human grant",
	"cowork.scroll":              "per-thread Cowork enablement + human grant",
	"cowork.setPointerProfile":   "per-thread Cowork enablement + human grant",
	"cowork.launchBrowser":       "per-thread Cowork enablement + human grant",

	// --- engine metadata: no thread state, same answer for everyone --------
	"agent.capabilities": "static per-harness capability sets; no thread state",
	"agent.discoverOptions": "the CLI's own option vocabulary; no thread state " +
		"beyond which engine is being asked",
	"agent.discoverModels": "the engine/provider model catalogue; no thread state",
	"mode.list":            "the ensemble recipe catalogue; the mutations (save/delete/apply) are gated",
	"mode.get":             "one ensemble recipe; the mutations are gated",
	"vsix.catalog":         "the static extension catalogue",
	"vsix.list":            "installed extension ids",
	"vsix.search":          "the extension marketplace query; no local state",
	"skills.listCatalog":   "the skill catalogue listing",
	"skills.listInstalled": "installed skill names",
}

// TestHandlerInventoryIsClassified fails when a registered method is neither
// reviewed as agent-reachable nor refused to an agent bridge.
//
// It drives the REAL server: every unclassified method is called from an
// authenticated bridge connection and must come back with the UI-only refusal.
// Calling for real, rather than reading source, is what makes the test believe
// the gate rather than the comment above it — a `requireUIWindow` that is not
// the handler's first statement, or one whose error is swallowed, fails here.
func TestHandlerInventoryIsClassified(t *testing.T) {
	sock, secrets, _, srv := pass2Core(t, []session.Record{{ThreadID: "t-a"}})

	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	asBridge(t, secrets, bridge, "t-a")

	registered := map[string]bool{}
	for _, method := range srv.Methods() {
		registered[method] = true
		if strings.HasPrefix(method, "test.") {
			continue // helpers registered by the test harness itself
		}
		if why, ok := agentReachable[method]; ok {
			if strings.TrimSpace(why) == "" {
				t.Errorf("agentReachable[%q] has no reason: the reason IS the review", method)
			}
			continue
		}
		// Empty params on purpose: a gate that runs first refuses before the
		// handler ever looks at them, and anything that gets past the gate
		// stops at its own validation instead of doing something real.
		err := bridge.CallTimeout(method, map[string]any{}, nil, 15*time.Second)
		if err == nil || !strings.Contains(err.Error(), uiOnlyRefusal) {
			t.Errorf("%s: reachable from an agent bridge (err = %v).\n"+
				"    Either gate it with requireUIWindow, or add it to "+
				"agentReachable with the reason an agent may call it.", method, err)
		}
	}

	// A stale entry is the other way this rots: a method is renamed or removed,
	// its exemption stays behind, and the next handler that happens to take
	// that name inherits an approval nobody gave it.
	//
	// The Cowork family used to be exempt from THIS check, and invisible to the
	// one above: registerCoworkHandlers returned early without a live KDE
	// session bus, so on a test machine (and in CI) those methods were not
	// registered at all and a new ungated cowork handler could not break the
	// build. Its registration is now unconditional — the stand-ins carry the
	// same caller gate and then report the service unavailable (coworkRegistrar,
	// cowork.go) — so the family is enumerable here like every other.
	for method := range agentReachable {
		if !registered[method] {
			t.Errorf("agentReachable names %q, which is not registered — "+
				"stale exemption", method)
		}
	}
}

// TestInventoryCoversTheWholeRegistry guards the guard: the inventory above is
// only worth anything if Methods() really sees every handler. A registry that
// answered with a subset — or with nothing — would let TestHandlerInventoryIsClassified
// pass while checking nothing at all, which is the exact failure mode this
// round was called to fix.
func TestInventoryCoversTheWholeRegistry(t *testing.T) {
	_, _, _, srv := pass2Core(t, nil)
	methods := map[string]bool{}
	for _, m := range srv.Methods() {
		methods[m] = true
	}
	// A sample spanning every registration site, so a whole family dropping out
	// of the enumeration is visible: registerHandlers itself, and each of the
	// register*() helpers it calls, plus run.go's shutdown handler.
	// registerCoworkHandlers is skipped without a live KDE session bus, so the
	// desktop-control methods are deliberately absent from this sample;
	// cowork.setEnabled stands in for the family, being registered either way.
	for _, want := range []string{
		"handshake",                 // registerHandlers, inline
		"agent.transcript",          // registerHandlers, the F34 family
		"agent.wait",                // registerOrchestrationHandlers
		"bridge.identify",           // registerMCPActivity
		"mode.apply",                // registerModeHandlers
		"cowork.setEnabled",         // registerCoworkEnableHandlers
		"agent.subagentTranscripts", // registerSubagentHandlers
		"app.shutdown",              // run.go's registerShutdownHandler
		"search.code",               // the last registration in registerHandlers
	} {
		if !methods[want] {
			t.Errorf("Methods() does not report %q; the inventory is not "+
				"seeing the whole registry", want)
		}
	}
	if len(methods) < 90 {
		t.Errorf("Methods() reported only %d handlers; the core registers ninety-odd "+
			"even without the desktop family, so the enumeration is broken", len(methods))
	}
}

// TestAgentListProjectionIsNarrow (audit F34 pass 4) guards the one roster read
// an agent's own bridge may make.
//
// The gate on session.listThreads is only worth something while agent.list
// stays a PROJECTION. Its comment used to justify that gate by naming the
// project path, the worktree path and branch, the title and the parent linkage
// — every one of which agent.list already hands to any bridge, so the stated
// reason protected nothing. What the gate really withholds is the rest of the
// record: the thread's system prompt, its environment overlay, its provider
// routing (including the env var its API token is read from), its restriction
// set, and its session id.
//
// So this test asserts the projection twice over: the exact key set, and — the
// assertion that actually catches the regression — that none of the withheld
// VALUES appears anywhere in the reply. A future `row["record"] = r`, or a
// switch to marshalling the record, passes a key-set check by accident and
// fails this.
func TestAgentListProjectionIsNarrow(t *testing.T) {
	// One record with every sensitive field populated with a distinctive
	// marker, so a leak is unmistakable in the raw bytes.
	sock, secrets, _, _ := pass2Core(t, []session.Record{{
		ThreadID: "t-a", Project: "/p", Title: "roster title",
		SessionID:       "SECRET-session-id",
		SystemPrompt:    "SECRET-persona",
		Env:             map[string]string{"SECRET_ENV_KEY": "SECRET-env-value"},
		ProviderID:      "SECRET-provider",
		ProviderBaseURL: "https://SECRET-endpoint.example",
		ProviderEnvVar:  "SECRET_TOKEN_VAR",
		PermissionMode:  "SECRET-mode",
		DisallowedTools: []string{"SECRET-banned-tool"},
		AddDirs:         []string{"/SECRET-extra-root"},
		Tags:            []string{"SECRET-tag"},
		MaxBudgetUSD:    12345.67,
		CompactStrategy: "SECRET-strategy",
	}})
	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	asBridge(t, secrets, bridge, "t-a")

	var raw json.RawMessage
	if err := bridge.CallTimeout("agent.list", map[string]any{}, &raw,
		10*time.Second); err != nil {
		t.Fatalf("agent.list from a bridge: %v", err)
	}

	// Nothing the gate exists to withhold may appear, under any key or none.
	for _, secret := range []string{
		"SECRET-session-id", "SECRET-persona", "SECRET_ENV_KEY", "SECRET-env-value",
		"SECRET-provider", "SECRET-endpoint", "SECRET_TOKEN_VAR", "SECRET-mode",
		"SECRET-banned-tool", "SECRET-extra-root", "SECRET-tag", "12345.67",
		"SECRET-strategy",
	} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("agent.list leaked %q to an agent bridge.\nreply: %s", secret, raw)
		}
	}

	var res struct {
		Threads []map[string]any `json:"threads"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Threads) != 1 {
		t.Fatalf("agent.list returned %d threads, want 1", len(res.Threads))
	}
	// The key set is the contract. Adding to it is a decision — make it here,
	// on purpose, with the reason in the handler's comment.
	want := map[string]bool{
		"threadId": true, "project": true, "title": true, "status": true,
		"branch": true, "path": true, "isolated": true, "number": true,
		"created": true, "lastTurn": true, "model": true,
		"parentThreadId": true, "role": true,
	}
	for key := range res.Threads[0] {
		if !want[key] {
			t.Errorf("agent.list projects %q, which is not in the reviewed set — "+
				"if an agent needs it, say why in the handler's comment and add it "+
				"here; if not, drop it", key)
		}
	}
	for key := range want {
		if _, ok := res.Threads[0][key]; !ok {
			t.Errorf("agent.list no longer projects %q; the orchestration tools "+
				"in mcp.go decode it", key)
		}
	}
	// ...and it still does its job: the roster the tools render is intact.
	if res.Threads[0]["title"] != "roster title" {
		t.Errorf("title = %v, want the record's own", res.Threads[0]["title"])
	}
}
