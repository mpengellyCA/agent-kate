package main

// The authorisation INVENTORY (audit F34/F36, passes 3-6, 2026-08-01).
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
// Pass 3 replaced it with an enumeration of the REGISTRY (ipc.Server.Methods),
// requiring every registered method to be either refused to an agent bridge or
// named in agentReachable with a written reason. That caught the missing
// handlers. It did not catch a WRONG reason: the entry was a free-text string
// and the only assertion on it was that it was non-empty, so
//
//	"harness.catalog": "the engine/provider model catalogue; no thread state"
//
// passed — a sentence that is true about the RESULT and silent about the
// parameter, which was an attacker-chosen URL plus the name of an environment
// variable the core would read its own credentials out of and post there. The
// exemption was the vulnerability, and the test rubber-stamped it. A map entry
// also short-circuited the whole check, so an entry left behind after a handler
// WAS gated kept passing too — the inventory could not tell an approval from a
// leftover.
//
// Pass 5 makes the entry carry its own weight, four ways, none of them prose:
//
//  1. a CLOSED VOCABULARY of bases (reachBasis). "Because it seems harmless" is
//     not spellable; the reviewer must pick a category the test knows how to
//     check, and an unknown one fails.
//  2. STRUCTURE PER BASIS. A caller-bound claim must reference the actual gate
//     function as a Go VALUE — the compiler proves the gate exists and a rename
//     breaks the build — and must name the test that drives it, also as a
//     value. Family bases (coop./cowork.) must match the family.
//  3. A BEHAVIOURAL PROBE for the "static catalogue" basis: the reply to empty
//     params must be identical to the reply to a params object stuffed with
//     every key that could name a network endpoint, a filesystem path, an
//     environment variable or another thread. A handler that HONOURS one of
//     those is not a static catalogue whatever its comment says. This is the
//     check that a caller-controlled catalogue lookup fails.
//  4. Every entry must be PROVEN REACHABLE. An exemption for a method that is
//     actually gated is a lie about the boundary and now fails.
//
// Pass 6 (the convergence round) answers the obvious question about pass 5:
// which of those four checks actually ran for the bases that matter? Only the
// third, and only for basisStaticCatalogue. basisCallerBound required a
// Binding, a Pin and a non-empty Why — three CITATIONS, none of them exercised
// — and the family bases required a method-name prefix and nothing else. A
// wrong classification satisfied all of it. So:
//
//  5. THE CITED TEST IS RUN, as a subtest (runPin). A name is a claim; a test
//     that no longer passes, or no longer drives this handler, now fails here.
//  6. A SECOND CONNECTION. Every basis whose claim is about the caller is
//     driven from a bridge bound to ANOTHER thread naming this one, and each
//     basis says what that must produce: a refusal by identity (caller-bound,
//     desktop-consent), service without one (the shared board), or the same
//     answer as everyone else (static catalogue). This is the check that found
//     cowork.status filed under "per-thread consent" with no gate at all.
//
// A handler added without a decision is in neither set, and the build breaks.
// That is still the entire point: the failure mode this test exists for is not
// "somebody wrote a bad gate", it is "somebody wrote a new handler and nobody
// thought about the gate at all" — and, since pass 5, "somebody wrote a reason
// that sounded fine", and since pass 6, "somebody wrote a reason nothing ever
// checked".

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/coop"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/modes"
	"agentkate/internal/permission"
	"agentkate/internal/session"
	"agentkate/internal/skills"
	"agentkate/internal/vsix"
)

// --- the vocabulary ---------------------------------------------------------

// reachBasis is the closed set of reasons a handler may be reachable from an
// agent's own bridge. Each one is checked by checkBasis below; a basis with no
// check is not a basis, it is an opinion.
type reachBasis string

const (
	// basisIdentityDoor: the handshake methods themselves. They cannot be
	// UI-only, because they are how a connection acquires an identity at all.
	basisIdentityDoor reachBasis = "identity-door"
	// basisCallerBound: reachable, but bound to the CALLING thread — the
	// connection's authenticated identity decides what it may name, and
	// anything outside the caller's own subtree costs a human approval.
	basisCallerBound reachBasis = "caller-bound"
	// basisNarrowRoster: agent.list, and only agent.list — a deliberately
	// reduced projection of the session record, pinned field by field.
	basisNarrowRoster reachBasis = "narrow-roster-projection"
	// basisSharedBoard: the cooperation board, which is a shared agent
	// workspace by design; agents writing to it IS the feature.
	basisSharedBoard reachBasis = "shared-cooperation-board"
	// basisDesktopConsent: the cowork.* family, gated by per-thread enablement
	// plus a human grant — a stronger gate than requireUIWindow, and not this
	// test's subject.
	basisDesktopConsent reachBasis = "desktop-consent"
	// basisStaticCatalogue: the answer is the same for every caller and no
	// caller-supplied input reaches outside the process. Verified, not
	// asserted: see hostileProbe.
	basisStaticCatalogue reachBasis = "static-catalogue"
)

// reachDecision is one reviewed exemption. Every field except Why is a
// reference the compiler checks, which is the point: prose rots silently,
// symbols do not.
type reachDecision struct {
	Basis reachBasis
	// Why is still required — a category is not an explanation — but it is no
	// longer the whole of the review, and it is no longer the only thing that
	// has to be true.
	Binding []any // the gate function(s), as values (basisCallerBound)
	// Pin names the test that drives this handler's gate. Pass 5 required it to
	// be non-nil; pass 6 RUNS it, as a subtest of the inventory, so a citation
	// to a test that no longer passes fails here rather than being taken on
	// trust. (A deleted or renamed one was already a compile error.)
	Pin func(*testing.T)
	// Cross is the params this handler is driven with FROM A BRIDGE BOUND TO
	// ANOTHER THREAD, and it is where the caller-bound and desktop-consent
	// claims stop being assertions. They must name probeSelf — the thread the
	// probing connection is NOT — so the handler is asked to act for a thread
	// its caller has no identity for. See checkBasis for what each basis then
	// requires of the answer.
	Cross map[string]any
	// CrossIgnored says this handler answers a foreign bridge with a plain ack
	// and silently DROPS the work, rather than refusing. It is the one shape
	// the cross probe cannot judge, so it costs a mandatory Pin that proves the
	// drop, and a Why of its own.
	CrossIgnored bool
	Why          string
}

// probeSelf is the thread the inventory's own bridge is bound to, and therefore
// the thread the FOREIGN bridge (bound to probeOther) has no right to act for.
// Every Cross params object names it.
const (
	probeSelf  = "t-a"
	probeOther = "t-b"
)

// crossThreadRefusal is ipc.Server.RequireBridge's own words for "the payload
// named a thread this connection is not". Asserting the SENTENCE, not merely
// that an error came back, is what stops the probe passing on somebody's
// parameter validation — or, in the Cowork family, on a "desktop integration
// unavailable" from a stand-in handler that never ran a gate at all.
const crossThreadRefusal = "thread mismatch: connection is bound to a different thread"

// bindingNames resolves the Binding references to their real symbol names. A
// caller-bound decision must cite a gate that EXISTS; referencing it as a value
// means a rename or a deletion is a compile error in this file, and resolving
// the name here means the citation cannot be a plausible-looking string.
var knownBindings = []string{
	"requireCallerThread",  // orchestrate.go — binds fromThreadId to the connection
	"requireUIOrOwnBridge", // cowork_enable.go — the UI, or the bridge for THIS thread
	"authorizeAgentTarget", // orchestrate.go — subtree rule + the human approval
	"RequireBridge",        // ipc — the connection's authenticated thread identity
	"MarkUI",               // ipc — claiming the UI role, refused to a bridge
}

func bindingName(f any) string {
	v := reflect.ValueOf(f)
	if v.Kind() != reflect.Func {
		return ""
	}
	fn := runtime.FuncForPC(v.Pointer())
	if fn == nil {
		return ""
	}
	name := fn.Name()
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, "-fm")
}

// hostileProbe is the params object every basisStaticCatalogue handler is
// called with. Every key here NAMES A RESOURCE OUTSIDE THE PROCESS: a network
// endpoint, an environment variable to read a credential from, a filesystem
// path, another thread. A handler whose reply changes when these are supplied
// is reaching somewhere on the caller's instruction, and "static catalogue" is
// the wrong basis for it — which is exactly what the former catalogue lookup did
// under "no thread state".
//
// Deliberately absent: keys that select WITHIN the handler's own catalogue
// (mode.get's `name`). Selecting a row of a local list is not reach, and
// including it here would flag the honest handlers along with the dangerous
// ones — a check that fires on everything gets suppressed like any other.
var hostileProbe = map[string]any{
	"providerId": "probe",
	"baseUrl":    "http://127.0.0.1:9/",
	"url":        "http://127.0.0.1:9/",
	"envVar":     "HOME",
	"env":        map[string]string{"HOME": "/"},
	"backend":    "probe-no-such-backend",
	"target":     "/",
	"path":       "/",
	"cwd":        "/",
	"command":    "/bin/true",
	"threadId":   "t-a",
	"sessionId":  "probe",
	"query":      "probe",
}

// agentReachable is the reviewed set: methods an agent's own (authenticated,
// thread-bound) bridge connection is MEANT to reach. Adding an entry is the
// decision — it should be as hard to write as it is to justify, so every one of
// them says what protects it INSTEAD of requireUIWindow, in a form the test can
// check.
//
// Pass 3 left nineteen entries prefixed "DEFERRED": reachable, not agent-facing
// by design, and not gated because the caller survey was outside that round's
// file set. Pass 4 finished the survey and gated all of them, so the prefix is
// gone. It is worth saying why: DEFERRED was a to-do list wearing the costume
// of a decision, and a test that accepts one cannot tell an approval from a
// postponement. There is no DEFERRED tier — a method is either here with a
// checkable basis, or it is refused.
var agentReachable = map[string]reachDecision{
	// --- identity ----------------------------------------------------------
	"handshake": {
		Basis: basisIdentityDoor, Binding: []any{(*ipc.Server).MarkUI},
		Pin: TestBridgeCannotBecomeUI,
		Why: "the UI-role claim itself; MarkUI refuses it to a bridge connection, " +
			"so reaching it is how a bridge learns it is not the window",
	},
	"bridge.identify": {
		Basis: basisIdentityDoor, Binding: []any{(*ipc.Server).MarkUI},
		Pin: TestBridgeIdentityNeedsItsSecret,
		Why: "the bridge's own authenticated door — it redeems the per-launch " +
			"secret akcore handed that thread (F13); unauthenticated callers get nothing",
	},

	// --- orchestration: bound to the calling thread, not to the UI ----------
	"agent.send": {
		Basis:   basisCallerBound,
		Binding: []any{requireCallerThread, handlerDeps.authorizeAgentTarget},
		Pin:     TestPerThreadHandlersBindTheCaller,
		Cross:   map[string]any{"threadId": probeSelf, "fromThreadId": probeSelf, "text": "x"},
		Why: "the connection may only speak for its own thread, and a target " +
			"outside its subtree costs one human approval (F13/F35)",
	},
	"agent.wait": {
		Basis:   basisCallerBound,
		Binding: []any{requireCallerThread, handlerDeps.authorizeAgentTarget},
		Pin:     TestWaitAgentBindsTheCaller,
		Cross:   map[string]any{"threadId": probeSelf, "fromThreadId": probeSelf, "timeoutSec": 1},
		Why: "same binding as agent.send; the in-band read of another thread's " +
			"reply is what F35 gated, and it is gated the same way",
	},
	"agent.stopClose": {
		Basis:   basisCallerBound,
		Binding: []any{requireCallerThread, handlerDeps.authorizeAgentTarget},
		Pin:     TestPerThreadHandlersBindTheCaller,
		Cross:   map[string]any{"threadId": probeSelf, "fromThreadId": probeSelf},
		Why:     "stopping another thread is bound and approved exactly like sending to it (F13)",
	},
	"agent.discard": {
		Basis:   basisCallerBound,
		Binding: []any{requireCallerThread, handlerDeps.authorizeAgentTarget},
		Pin:     TestDiscardGoesThroughGate,
		Cross:   map[string]any{"threadId": probeSelf, "fromThreadId": probeSelf},
		Why:     "destructive, and therefore bound and approved like the rest of the family (F13)",
	},
	"agent.launchWorker": {
		Basis:   basisCallerBound,
		Binding: []any{requireUIOrOwnBridge},
		Pin:     TestLaunchWorkerBindsTheCallerToItsParent,
		Cross:   map[string]any{"parentThreadId": probeSelf, "prompt": "x"},
		Why: "a bridge may launch only FROM its own thread, and the launch " +
			"authority gate then measures the request against that thread's own (F1)",
	},
	"permission.request": {
		Basis:   basisCallerBound,
		Binding: []any{(*ipc.Server).RequireBridge},
		Pin:     TestPermissionRequestBindsTheAskingThread,
		Cross:   map[string]any{"threadId": probeSelf, "toolName": "Bash"},
		Why: "the asking thread is the connection's own, not the payload's; the " +
			"UI never calls this at all (F36)",
	},
	"agent.list": {
		Basis: basisNarrowRoster, Pin: TestAgentListProjectionIsNarrow,
		Why: "the deliberately reduced roster projection the orchestration tools " +
			"need — ids, titles, status, worktree, parent linkage — built field by " +
			"field. Contrast session.listThreads, which is the whole record and is gated",
	},

	// --- the cooperation board: shared by design ---------------------------
	"coop.setOpenFiles":  sharedBoard("who is editing what"),
	"coop.listOpenFiles": sharedBoard("who is editing what"),
	"coop.postNote":      sharedBoard("the shared note wall"),
	"coop.readNotes":     sharedBoard("the shared note wall"),
	"coop.setPresence":   sharedBoard("who is around"),
	"coop.getPresence":   sharedBoard("who is around"),
	"coop.claimFile":     sharedBoard("the advisory file lock"),
	"coop.releaseFile":   sharedBoard("the advisory file lock"),
	"coop.requestReview": sharedBoard("the review queue"),
	"coop.listReviews":   sharedBoard("the review queue"),
	"coop.getState":      sharedBoard("the whole board in one read"),

	// --- Cowork: gated by desktop CONSENT, which is a stronger gate ---------
	// These are the agent's desktop tools. They are not UI-only — the whole
	// point is that an agent drives them — and each one runs the per-thread
	// enablement check plus a human grant (cowork.go). Not this test's subject.
	"cowork.listWindows":         desktopConsent(),
	"cowork.listElements":        desktopConsent(),
	"cowork.readText":            desktopConsent(),
	"cowork.screenshot":          desktopConsent(),
	"cowork.activateElement":     desktopConsent(),
	"cowork.setElementText":      desktopConsent(),
	"cowork.injectInput":         desktopConsent(),
	"cowork.playInput":           desktopConsent(),
	"cowork.movePointer":         desktopConsent(),
	"cowork.movePointerRelative": desktopConsent(),
	"cowork.pointerClick":        desktopConsent(),
	"cowork.pointerClickElement": desktopConsent(),
	"cowork.pointerDrag":         desktopConsent(),
	"cowork.scroll":              desktopConsent(),
	"cowork.setPointerProfile":   desktopConsent(),
	"cowork.launchBrowser":       desktopConsent(),
	"cowork.threadState": {
		Basis: basisDesktopConsent, Binding: []any{requireUIOrOwnBridge},
		Cross: map[string]any{"threadId": probeSelf},
		Why: "the bridge asking about its OWN thread's desktop state; it is how " +
			"the bridge decides whether to advertise the desktop tools at all",
	},
	"cowork.toolsListed": {
		Basis: basisDesktopConsent, Binding: []any{(*ipc.Server).RequireBridge},
		CrossIgnored: true, Pin: TestToolsListedFromAForeignBridgeSignalsNothing,
		Why: "the bridge reporting what it advertised, for its own thread. It is " +
			"a NOTIFICATION — the bridge never waits for the reply — so a foreign " +
			"caller gets the same bare ack and the reveal signal is dropped, " +
			"rather than an error nobody would read",
	},
	"cowork.requestEnable": {
		Basis:   basisDesktopConsent,
		Binding: []any{(*ipc.Server).RequireBridge, handlerDeps.authorizeAgentTarget},
		Cross:   map[string]any{"fromThreadId": probeSelf, "threadId": probeSelf},
		Why: "the agent ASKING the human for desktop access (plan 18); it grants " +
			"nothing by itself — askCoworkEnable fails closed with no human to ask",
	},

	// --- static catalogues: the same answer for every caller ---------------
	// Verified by hostileProbe, not asserted. Everything that used to sit here
	// and DID take caller-supplied reach — the former option/catalogue lookup,
	// vsix.list, vsix.search, skills.listInstalled — is
	// gated in handlers.go now (audit F36 pass 5) and is deliberately absent.
	"mode.list": {
		Basis: basisStaticCatalogue,
		Why: "the ensemble recipe catalogue; the mutations (save/delete/apply) " +
			"are UI-only, so this reads a list an agent cannot influence",
	},
	"mode.get": {
		Basis: basisStaticCatalogue,
		Why: "one ensemble recipe, selected by name from that same local store; " +
			"the name reaches nothing but the store's own map",
	},
	"vsix.catalog": {
		Basis: basisStaticCatalogue,
		Why: "the compiled-in extension catalogue plus a local installed flag; " +
			"the network-touching siblings (list, search) are gated",
	},
	"skills.listCatalog": {
		Basis: basisStaticCatalogue,
		Why: "the names in the core's own skill catalogue directory; the target " +
			"directory is the core's, never the caller's (listInstalled is gated)",
	},
	// cowork.status sat under desktop-consent until pass 6's cross-thread probe
	// drove it and found NO GATE: it is registered through coworkRegistrar.probe
	// precisely so it can answer when there is no service, no consent and no
	// thread — the UI's capability read, whose failure mode is a panel stuck on
	// "checking…". So it never ran the per-thread enablement the family's shared
	// reason described, and seventeen entries' worth of inherited prose was
	// wrong about this one. That is the whole point of a checked basis: the
	// label was plausible, the sentence was reasonable, and the handler did
	// something else.
	"cowork.status": {
		Basis: basisStaticCatalogue,
		Why: "the desktop capability probe: whether the service came up, whether " +
			"the kill switch is down, whether the grant store was tampered with. " +
			"Three process-wide booleans, no parameters read, no thread named — " +
			"and deliberately answerable with no consent, because 'desktop access " +
			"is off' is the answer the UI most needs",
	},
}

// sharedBoard and desktopConsent keep the two large families readable without
// letting them become unreasoned: the basis is still checked against the method
// name's family, and each entry still says which part of the board or which
// consent path it is.
func sharedBoard(what string) reachDecision {
	return reachDecision{Basis: basisSharedBoard,
		// The board is open — a bridge for any thread is SERVED, which is what
		// the cross probe proves rather than the entry asserting it. But since
		// F63 the writer's NAME is not the payload's to choose: a bridge's
		// writes are attributed as its bound thread whatever owner/author it
		// supplied ("human" is reserved for the UI role). Overriding is not
		// refusing, so the openness claim below still holds; the attribution
		// binding has its own test, TestCoopWritesAreAttributedToTheCaller.
		// A coop handler that started REFUSING foreign bridges would fail this
		// and belong under basisCallerBound instead.
		Cross: map[string]any{"owner": probeSelf, "author": probeSelf,
			"thread": probeSelf, "text": "probe", "summary": "probe"},
		Why: "the cooperation board is a shared agent workspace by design — " + what}
}

func desktopConsent() reachDecision {
	return reachDecision{Basis: basisDesktopConsent,
		// Every desktop tool takes the thread it is acting for in `threadId`,
		// and requireCoworkBridge/requirePointerControl bind that id to the
		// connection before anything else happens.
		Cross: map[string]any{"threadId": probeSelf},
		Why: "per-thread Cowork enablement plus a human grant, checked inside the " +
			"handler (cowork.go); a stronger gate than the UI-only one"}
}

// --- the checks -------------------------------------------------------------

// basisProbes are the two ways checkBasis drives a handler FOR REAL: as the
// bridge bound to probeSelf, and as a bridge bound to probeOther — a
// connection with a genuine, authenticated identity that is simply not the
// thread the payload names. The second one is the attacker in every finding
// this inventory exists for: not an outsider, but the thread next door.
type basisProbes struct {
	asSelf    func(map[string]any) (string, string)
	asForeign func(map[string]any) (string, string)
}

// runPin runs the test an entry cites, once per distinct test, as a subtest of
// the inventory (pass 6).
//
// Naming a test used to be the whole of the requirement, and a name is only a
// claim: the citation could point at a test that no longer runs this handler's
// gate, or no longer passes at all, and the inventory would still report a
// clean boundary. Running it means the inventory FAILS when its own evidence
// fails — which is the difference between an inventory and a bibliography.
func runPin(t *testing.T, method string, d reachDecision, already map[string]bool) {
	if d.Pin == nil {
		return
	}
	name := bindingName(d.Pin)
	if !strings.HasPrefix(name, "Test") {
		t.Errorf("%s: Pin resolves to %q, which is not a test function", method, name)
		return
	}
	if name == "TestHandlerInventoryIsClassified" {
		t.Errorf("%s: Pin points at the inventory itself, which proves nothing "+
			"and would recurse", method)
		return
	}
	if already[name] {
		return // cited by several methods; one run is the evidence
	}
	already[name] = true
	t.Run("pin/"+name, func(t *testing.T) { d.Pin(t) })
}

// checkBasis enforces the structure each basis promises. It is where "the
// reason carries weight" actually lives: every branch below can fail, and the
// failure names what the reviewer must change.
//
// Pass 6 closes the hole an adversarial read found in pass 5: for the
// caller-bound and family bases, checkBasis CHECKED NOTHING. It required a
// non-empty Why, a citation, and — for the families — a method-name prefix,
// all of which a WRONG classification satisfies. A UI-only handler mislabelled
// caller-bound, or a thread-scoped one filed under the cooperation board,
// passed silently; only basisStaticCatalogue actually drove its handler. So
// every basis now runs the thing it claims: the cited test, and a call from a
// bridge bound to another thread.
func checkBasis(t *testing.T, method string, d reachDecision, p basisProbes) {
	t.Helper()
	if n := len([]rune(strings.TrimSpace(d.Why))); n < 40 {
		t.Errorf("%s: Why is %d characters. The basis is the category; Why is the "+
			"review, and a review that fits in a label is not one", method, n)
	}
	for _, b := range d.Binding {
		name := bindingName(b)
		found := false
		for _, known := range knownBindings {
			if name == known {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: Binding names %q, which is not one of the gates this "+
				"codebase recognises (%s). Either it is not a caller gate, or it "+
				"is a new one that belongs in knownBindings with the others",
				method, name, strings.Join(knownBindings, ", "))
		}
	}

	switch d.Basis {
	case basisIdentityDoor:
		if method != "handshake" && method != "bridge.identify" {
			t.Errorf("%s claims the identity-door basis. There are exactly two "+
				"doors, handshake and bridge.identify; a third is a new trust "+
				"boundary and needs its own review, not this label", method)
		}
		if d.Pin == nil {
			t.Errorf("%s: an identity door with no test pinning it", method)
		}
	case basisCallerBound:
		if len(d.Binding) == 0 {
			t.Errorf("%s claims to be caller-bound and names no gate. The "+
				"citation is the claim: reference the function that binds the "+
				"caller, so a rename cannot leave this entry behind", method)
		}
		if d.Pin == nil {
			t.Errorf("%s claims to be caller-bound and names no test. A binding "+
				"nothing exercises is a comment; name the test that drives it "+
				"from a bridge that is not the thread it claims", method)
		}
		// And the claim itself, driven: an authenticated bridge for another
		// thread names probeSelf in the payload and must be refused BY IDENTITY.
		// This is the assertion the basis was missing — everything above is
		// satisfied by a wrong classification.
		requireCrossThreadRefusal(t, method, d, p)
	case basisNarrowRoster:
		if method != "agent.list" {
			t.Errorf("%s claims the narrow-roster basis, which describes exactly "+
				"one handler; a second roster read needs its own projection test", method)
		}
		if d.Pin == nil {
			t.Errorf("%s: a projection with no test pinning its key set", method)
		}
	case basisSharedBoard:
		if !strings.HasPrefix(method, "coop.") {
			t.Errorf("%s claims the shared-board basis but is not a coop.* "+
				"method; the board's openness is not transferable to other families", method)
		}
		// The board's claim is the OPPOSITE of the caller-bound one, so it is
		// proven the opposite way: a bridge for another thread is served, and
		// the entry is approving exactly that. A coop handler that starts
		// binding its caller fails here and belongs under basisCallerBound —
		// which is the misclassification this basis could previously hide,
		// since a prefix check cannot tell an open handler from a gated one.
		_, errStr := p.asForeign(d.Cross)
		if strings.Contains(errStr, crossThreadRefusal) ||
			strings.Contains(errStr, uiOnlyRefusal) {
			t.Errorf("%s is filed under the shared board, which says any agent may "+
				"use it — but it refuses a bridge bound to another thread (%s). "+
				"It has a caller gate: classify it as caller-bound, with the gate "+
				"and the test that drives it", method, errStr)
		}
	case basisDesktopConsent:
		if !strings.HasPrefix(method, "cowork.") {
			t.Errorf("%s claims the desktop-consent basis but is not a cowork.* "+
				"method; the consent machinery only covers that family", method)
		}
		// "Per-thread enablement plus a human grant" begins with binding the
		// thread to the connection — no binding, no per-thread anything. The
		// probe drives that first step for real, so the family's shared reason
		// stops being a sentence seventeen entries inherit unread.
		//
		// Deliberately asserted on RequireBridge's own sentence: without a live
		// service the registrar's stand-ins answer "desktop integration
		// unavailable" to everyone, which is a refusal that proves nothing, and
		// this check must fail rather than accept it (see inventoryCore).
		if d.CrossIgnored {
			if d.Pin == nil {
				t.Errorf("%s says it IGNORES a foreign bridge rather than refusing "+
					"it. That is the one answer the probe cannot judge, so it costs "+
					"a test that proves the work is dropped — name it in Pin", method)
			}
			break
		}
		requireCrossThreadRefusal(t, method, d, p)
	case basisStaticCatalogue:
		// The one basis a reviewer cannot simply assert. Two calls: empty
		// params, and params naming every kind of resource outside the process.
		// The same answer both times, or it is not a static catalogue.
		bareRes, bareErr := p.asSelf(map[string]any{})
		hostRes, hostErr := p.asSelf(hostileProbe)
		// "The same answer for EVERY CALLER" was half the basis's sentence and
		// none of its check: until pass 6 exactly one connection was ever asked.
		// A handler that answered per-thread would have satisfied the other half
		// perfectly, since both of its calls came from the same thread.
		if otherRes, otherErr := p.asForeign(hostileProbe); otherRes != bareRes ||
			otherErr != bareErr {
			t.Errorf("%s answers a DIFFERENT caller differently.\n  as %s: %s / %s"+
				"\n  as %s: %s / %s\nThe basis says the answer is the same for "+
				"everyone; this handler has caller-dependent state, so it needs a "+
				"basis that says what depends on the caller.",
				method, probeSelf, bareRes, bareErr, probeOther, otherRes, otherErr)
		}
		if bareRes != hostRes || bareErr != hostErr {
			t.Errorf("%s claims to answer the same for everyone, but a caller "+
				"that names a URL, an environment variable, a path or a thread "+
				"gets a DIFFERENT answer — so caller-supplied input reaches "+
				"something.\n  bare:    %s / %s\n  hostile: %s / %s\n"+
				"This is the shape a caller-controlled catalogue lookup had: a truthful sentence "+
				"about the result, and a parameter nobody classified. Gate it, or "+
				"give it a basis that describes what the parameter reaches.",
				method, bareRes, bareErr, hostRes, hostErr)
		}
	default:
		t.Errorf("%s: unknown basis %q. The vocabulary is closed on purpose — a "+
			"new kind of justification needs a check written for it here before "+
			"it can be used", method, d.Basis)
	}
}

// requireCrossThreadRefusal is the behavioural half of every basis whose claim
// is "the CONNECTION decides what this call may name".
//
// It drives the handler from a bridge that is authenticated — it holds
// probeOther's launch secret and identified with it — and has it ask for
// probeSelf. Both the payload and the caller are real; only the pairing is
// wrong, which is precisely audit F13's attacker: a worker naming its
// controller, a sibling naming the thread next door. The refusal must be
// RequireBridge's, because any other error (a parameter validation, a "not
// found", a service that happens to be unavailable in this process) would mean
// the gate never ran and the pass was luck.
func requireCrossThreadRefusal(t *testing.T, method string, d reachDecision, p basisProbes) {
	t.Helper()
	if len(d.Cross) == 0 {
		t.Errorf("%s: no Cross params. The basis claims the connection's identity "+
			"decides what the payload may name — give the probe a params object "+
			"naming %q so it can be driven from a bridge that is not that thread",
			method, probeSelf)
		return
	}
	named := false
	for _, v := range d.Cross {
		if s, ok := v.(string); ok && s == probeSelf {
			named = true
		}
	}
	if !named {
		t.Errorf("%s: Cross names no thread, so the probe asks nothing the "+
			"connection is not entitled to. Name %q in the field the handler "+
			"binds on", method, probeSelf)
		return
	}
	res, errStr := p.asForeign(d.Cross)
	if !strings.Contains(errStr, crossThreadRefusal) {
		t.Errorf("%s: a bridge bound to %s named %s in its payload and was NOT "+
			"refused by identity.\n  result: %s\n  error:  %s\n"+
			"The entry claims the connection decides what the call may name; "+
			"this is that claim, driven, and it did not hold. Either the gate is "+
			"missing (the F13 defect, verbatim) or the basis is wrong for this "+
			"handler.", method, probeOther, probeSelf, res, errStr)
	}
}

// --- the test ---------------------------------------------------------------

// TestHandlerInventoryIsClassified fails when a registered method is neither
// reviewed as agent-reachable nor refused to an agent bridge — and, since pass
// 5, when a review is present but does not hold up.
//
// It drives the REAL server: every unclassified method is called from an
// authenticated bridge connection and must come back with the UI-only refusal,
// and every CLASSIFIED one is called too and must not. Calling for real, rather
// than reading source, is what makes the test believe the gate rather than the
// comment above it — a `requireUIWindow` that is not the handler's first
// statement, or one whose error is swallowed, fails here.
//
// Pass 6 adds the SECOND connection. One bridge can only ever show that a
// handler answers its own thread; the finding this inventory exists for is a
// handler that answers somebody ELSE's, so every basis whose claim is about the
// caller is now driven from a bridge bound to probeOther naming probeSelf.
func TestHandlerInventoryIsClassified(t *testing.T) {
	sock, secrets, srv := inventoryCore(t, []session.Record{
		{ThreadID: probeSelf}, {ThreadID: probeOther},
	})

	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	asBridge(t, secrets, bridge, probeSelf)

	// The thread next door: a real, authenticated bridge — the strongest caller
	// identity a prompt-injected agent can obtain — that is simply not probeSelf.
	foreign, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial (foreign): %v", err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	asBridge(t, secrets, foreign, probeOther)

	// call returns the reply and the error as strings, so two calls can be
	// compared without caring which of the two they came back in.
	callAs := func(client *ipc.Client, method string, params map[string]any) (string, string) {
		var raw json.RawMessage
		err := client.CallTimeout(method, params, &raw, 20*time.Second)
		if err != nil {
			return "", err.Error()
		}
		return string(raw), ""
	}
	call := func(method string, params map[string]any) (string, string) {
		return callAs(bridge, method, params)
	}

	pinsRun := map[string]bool{}
	registered := map[string]bool{}
	for _, method := range srv.Methods() {
		registered[method] = true
		if strings.HasPrefix(method, "test.") {
			continue // helpers registered by the test harness itself
		}
		if d, ok := agentReachable[method]; ok {
			runPin(t, method, d, pinsRun)
			checkBasis(t, method, d, basisProbes{
				asSelf: func(p map[string]any) (string, string) {
					return call(method, p)
				},
				asForeign: func(p map[string]any) (string, string) {
					return callAs(foreign, method, p)
				},
			})
			// ...and it really is reachable. A decision that describes a gated
			// handler is a lie about the boundary, and before pass 5 the map
			// lookup above simply skipped the call, so gating a handler and
			// leaving its exemption behind was invisible.
			if _, errStr := call(method, map[string]any{}); strings.Contains(errStr, uiOnlyRefusal) {
				t.Errorf("%s is listed as agent-reachable but the handler refuses "+
					"an agent bridge (%s). Delete the entry — the gate is the "+
					"decision now", method, errStr)
			}
			continue
		}
		// Empty params on purpose: a gate that runs first refuses before the
		// handler ever looks at them, and anything that gets past the gate
		// stops at its own validation instead of doing something real.
		_, errStr := call(method, map[string]any{})
		if !strings.Contains(errStr, uiOnlyRefusal) {
			t.Errorf("%s: reachable from an agent bridge (err = %q).\n"+
				"    Either gate it with requireUIWindow, or add it to "+
				"agentReachable with a basis the test can check.", method, errStr)
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

// TestToolsListedFromAForeignBridgeSignalsNothing is the price of the one
// CrossIgnored entry in the inventory.
//
// cowork.toolsListed is how a bridge says "I have just re-advertised my tools",
// and it is what setCoworkEnabled waits on before telling the human that
// desktop access is usable NOW. The bridge sends it as a notification and never
// reads the reply, so the handler answers everyone the same bare ack and simply
// declines to act on a mismatched thread — which means the cross-thread probe
// in checkBasis cannot see the difference between the gate working and the gate
// being deleted. This test can: it registers a waiter for probeSelf, has the
// bridge for probeOther claim probeSelf's re-list, and requires the waiter to
// stay unwoken.
//
// What that buys is not cosmetic. A foreign bridge that could fire this ack
// would let one thread satisfy ANOTHER thread's reveal wait, and the enable
// would report `revealed: true` — "the tools are live in the next turn" — for a
// CLI that had not re-listed anything.
func TestToolsListedFromAForeignBridgeSignalsNothing(t *testing.T) {
	sock, secrets, _ := inventoryCore(t, []session.Record{
		{ThreadID: probeSelf}, {ThreadID: probeOther},
	})
	foreign, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	asBridge(t, secrets, foreign, probeOther)

	ack := coworkReveal.add(probeSelf)
	t.Cleanup(func() { coworkReveal.drop(probeSelf, ack) })

	if err := foreign.CallTimeout("cowork.toolsListed",
		map[string]any{"threadId": probeSelf}, nil, 10*time.Second); err != nil {
		// The ack is a notification: answering it is not the property under
		// test, and an error here would only mean the shape changed.
		t.Logf("cowork.toolsListed answered the foreign bridge with %v", err)
	}
	select {
	case <-ack:
		t.Fatalf("a bridge bound to %s woke %s's reveal waiter — one thread can "+
			"satisfy another's 'my tools are live now' proof, so an enable reports "+
			"revealed:true for a CLI that never re-listed", probeOther, probeSelf)
	default:
	}

	// ...and the same call from the thread's OWN bridge does wake it, so the
	// assertion above is about the binding and not about a signal path that
	// stopped working.
	own, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial (own): %v", err)
	}
	t.Cleanup(func() { _ = own.Close() })
	asBridge(t, secrets, own, probeSelf)
	if err := own.CallTimeout("cowork.toolsListed",
		map[string]any{"threadId": probeSelf}, nil, 10*time.Second); err != nil {
		t.Fatalf("cowork.toolsListed from its own bridge: %v", err)
	}
	select {
	case <-ack:
	case <-time.After(5 * time.Second):
		t.Fatal("the thread's own bridge did not wake its reveal waiter, so the " +
			"refusal above proves nothing")
	}
}

// TestInventoryCoversTheWholeRegistry guards the guard: the inventory above is
// only worth anything if Methods() really sees every handler. A registry that
// answered with a subset — or with nothing — would let TestHandlerInventoryIsClassified
// pass while checking nothing at all, which is the exact failure mode this
// round was called to fix.
func TestInventoryCoversTheWholeRegistry(t *testing.T) {
	_, _, srv := inventoryCore(t, nil)
	methods := map[string]bool{}
	for _, m := range srv.Methods() {
		methods[m] = true
	}
	// A sample spanning every registration site, so a whole family dropping out
	// of the enumeration is visible: registerHandlers itself, and each of the
	// register*() helpers it calls, plus run.go's shutdown handler.
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
		// registerCoworkHandlers, which used to be skipped entirely without a
		// live KDE session bus — i.e. in every test process — and so hid the
		// desktop family from the inventory. coworkRegistrar registers the
		// names either way now, and this sample is what keeps that true: a
		// return to conditional registration fails here, not silently.
		"cowork.listWindows", // reg.agent — an agent-reachable desktop tool
		"cowork.setPolicy",   // reg.ui — a UI-only desktop RPC
		"cowork.status",      // reg.probe — must answer even with no service
	} {
		if !methods[want] {
			t.Errorf("Methods() does not report %q; the inventory is not "+
				"seeing the whole registry", want)
		}
	}
	if len(methods) < 110 {
		t.Errorf("Methods() reported only %d handlers; the core registers a "+
			"hundred and ten-odd including the desktop family, so the "+
			"enumeration is broken", len(methods))
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
	sock, secrets, _ := inventoryCore(t, []session.Record{{
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

// inventoryCore is pass2Core plus the catalogue stores (extensions, skills,
// ensembles). It exists because the static-catalogue probe CALLS those handlers
// for real, and a nil dependency turns the call into a recovered panic — which
// the probe would happily compare against another recovered panic and call
// "the same answer for everyone". A basis that is verified by running the
// handler has to actually run it.
//
// The same reasoning is why it now builds a real cowork.Service (pass 6). With
// d.cowork nil, coworkRegistrar registers STAND-INS for the desktop family that
// answer "desktop integration unavailable" to every caller without running a
// gate at all — so a cross-thread probe against them would come back refused,
// pass, and prove precisely nothing about per-thread consent. The service here
// has no KDE client behind it (selfService), so the handlers reach their real
// gates and stop there: the binding runs, the desktop does not.
func inventoryCore(t *testing.T, records []session.Record) (sock string,
	secrets *bridgeSecrets, srv *ipc.Server) {
	t.Helper()
	sessions := testSessions(t)
	for _, r := range records {
		if r.Project == "" {
			r.Project = "/p"
		}
		if r.Created.IsZero() {
			r.Created = time.Now()
		}
		if err := sessions.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.ThreadID, err)
		}
	}
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sock = filepath.Join(t.TempDir(), "inventory.sock")
	srv = ipc.NewServer(sock, log)
	sup := agent.NewSupervisor("", log, func(string, []json.RawMessage) {})
	harnesses := harness.NewRegistry("claude")
	harnesses.Register(newClaudeHarness(sup, "", ""))
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	ensembles, err := modes.NewStore(filepath.Join(dir, "modes.json"))
	if err != nil {
		t.Fatalf("modes.NewStore: %v", err)
	}
	secrets = newBridgeSecrets()
	registerHandlers(handlerDeps{
		srv: srv, harnesses: harnesses, broker: permission.New(),
		turns: agent.NewTurnTracker(), orchGrants: newOrchGrants(),
		coop: coop.NewState(), threads: newThreadRegistry(),
		gitCache: gitCache, sessions: sessions, log: log,
		bridgeSecrets: secrets,
		extensions:    vsix.NewManager(filepath.Join(dir, "vsix")),
		skills:        skills.New(filepath.Join(dir, "skills")),
		modes:         ensembles,
		cowork:        selfService(t),
	})
	// app.shutdown is registered by runCore, not registerHandlers, so without
	// this it is invisible to every test in this file AND to the registry
	// inventory — which is precisely how the most powerful RPC the core serves
	// (stop every agent, exit the process) stayed ungated. No-op closures: the
	// gate refuses before either runs, and a test that got past the gate must
	// not take the process down with it.
	registerShutdownHandler(srv, func(progress shutdownProgressFn) {
		progress("done", "", 0, 0)
	}, func() {})
	serveIPC(t, srv, sock)
	return sock, secrets, srv
}
