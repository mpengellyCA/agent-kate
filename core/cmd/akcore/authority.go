package main

// Authority gating for agent-initiated launches (audit F1, 2026-08-01).
//
// THE RULE: an agent may ask for authority; only the human may grant it.
//
// Agent Kate runs every agent at the user's own uid, so the defences here are
// not a sandbox — they exist so that a PROMPT-INJECTED agent (poisoned repo
// content steering an otherwise-trusted model) cannot escalate its OWN
// authority without a human click. Before this gate, an agent running in
// `acceptEdits` — where Bash stops and asks the human — could call
// `launch_agent(permission_mode:"bypassPermissions", isolation:"workspace")`
// and get a worker with ungated Bash in the user's main checkout, with no
// prompt anywhere in the chain. The escalation was in-band, which is exactly
// the adversary the project defends against.
//
// Everything below fails CLOSED: when a mode, a parent's own mode, or a live
// worker count cannot be established, the code assumes the arrangement that
// needs the human, never the one that skips them.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// --- desktop access at thread-creation time ---------------------------------

// authorizeCoworkAtStart is the one gate every thread-creating handler runs its
// CoworkEnabled flag through (audit F5): agent.start, mode.apply, and anything
// added later. Desktop control is the capability that reaches outside the
// workspace, so it may only ever come from the human.
//
// A UI caller passes. That is not an exemption — it is the recognition that the
// flag can only be set by ticking the box in the New Agent / ensemble dialog,
// which IS the human click askCoworkEnable exists to obtain. Prompting a second
// time for the same click would train the human to dismiss the desktop-access
// dialog by reflex, which is precisely the prompt that must keep its meaning
// when an AGENT raises it through enable_cowork or launch_agent.
//
// Every other caller must ask, and askCoworkEnable fails closed: it denies on
// timeout and denies when no UI is connected to ask. Today the handlers that
// call this also carry requireUI, so that branch should be unreachable; it is
// written anyway so that a future caller — or a requireUI someone removes —
// cannot silently become a way to self-grant the desktop.
func authorizeCoworkAtStart(d handlerDeps, ctx context.Context, requested bool,
	title, reason string) error {
	if !requested {
		return nil
	}
	if d.srv.RequireUI(ctx) {
		return nil
	}
	if !askCoworkEnable(d, "", "", orDefault(title, "a new agent"), capText(reason)) {
		return ipc.Errorf(codeCoworkDenied,
			"NOT APPLIED: the human did not approve desktop access for this agent; "+
				"it was not started")
	}
	return nil
}

// --- permission-mode ordering ----------------------------------------------

// permissivenessRanks orders permission modes from most supervised (0) to least
// (5). ONE table for every engine: claude's vocabulary
// (plan/default/acceptEdits/auto/dontAsk/bypassPermissions, harness_claude.go)
// and kimi's (plan/default/auto/yolo, kimi/translate.go's ConfigOption doc)
// overlap on three names and agree on what they mean, so a cross-engine launch
// compares like for like without an engine if-ladder.
//
// The ordering is by HOW MUCH THE AGENT CAN DO WITHOUT ASKING:
//
//	plan               read-only; it may not even edit
//	default            every gated tool (Bash, outside-cwd writes) asks the human
//	acceptEdits        file edits are auto-accepted; Bash still asks
//	auto               the engine auto-approves what it considers safe
//	dontAsk            no prompts, but the engine's own deny rules still apply
//	bypassPermissions  every gate off (claude)
//	yolo               every gate off (kimi) — the same rung as bypassPermissions
//
// Adding an engine means adding its modes HERE, not writing a second table.
var permissivenessRanks = map[string]int{
	"plan":              0,
	"default":           1,
	"acceptEdits":       2,
	"auto":              3,
	"dontAsk":           4,
	"bypassPermissions": 5,
	"yolo":              5,
}

// rankUnknownMode ranks a mode this table has never heard of. It sits ABOVE
// every known mode on purpose: a future engine's new vocabulary must need the
// human's approval until somebody ranks it in permissivenessRanks. Assuming an
// unknown mode is safe would silently re-open F1 the day a backend is added.
const rankUnknownMode = 99

// rankStaticEngineDefault is the rank an UNSPECIFIED mode lands on for a
// harness with a STATIC vocabulary. Exact, not a guess: the claude supervisor
// rewrites an empty mode to acceptEdits (core/internal/agent/agent.go) and
// persists that as the applied truth.
const rankStaticEngineDefault = 2 // == acceptEdits

// requestedPermissiveness ranks a mode a CALLER asked for. Unknown ranks
// maximal — fail closed, because over-ranking a request costs one human click
// while under-ranking it grants authority nobody approved.
//
// An EMPTY mode is not a request at all; it means "whatever this engine starts
// with", and is ranked by launchBaselineRank instead.
func requestedPermissiveness(mode string) int {
	m := strings.TrimSpace(mode)
	if m == "" {
		return rankStaticEngineDefault
	}
	if r, ok := permissivenessRanks[m]; ok {
		return r
	}
	return rankUnknownMode
}

// launchBaselineRank is what a worker launched with NO permission_mode will
// actually run at.
//
//   - Static-vocabulary harness (claude): exactly rankStaticEngineDefault. The
//     supervisor's rewrite is visible in the code, so a plan-mode launcher
//     asking for an unspecified worker IS an escalation and is caught.
//   - Discovered-vocabulary harness (kimi): engineDefault is the CLI's OWN
//     answer — the `mode` option's currentValue from a fresh session handshake,
//     which harness.DiscoveredOption now carries and the supervisor caches for
//     the process lifetime (engineDefaultPermissionMode). An earlier revision
//     called that value unreadable and substituted the LAUNCHER's mode, which
//     under-ranked a real escalation: a kimi thread pinned to "plan" launching
//     an unspecified same-engine worker read as no escalation even though the
//     worker comes up in kimi's "default". The value was already being read and
//     persisted; it is used here now, so the approximation is gone.
//
// An unreadable default (probe failure, or a harness that discovers nothing)
// falls back to rankStaticEngineDefault — the most permissive baseline any
// registered engine applies — because an unknown baseline must cost a prompt,
// never a silent grant.
func launchBaselineRank(_ harness.HarnessDescriptor, engineDefault string) int {
	if m := strings.TrimSpace(engineDefault); m != "" {
		if r, ok := permissivenessRanks[m]; ok {
			return r
		}
		return rankUnknownMode // a vocabulary nobody has ranked yet: ask
	}
	return rankStaticEngineDefault
}

// engineDefaultPermissionMode reports the permission mode a FRESH session of
// this harness comes up in, for harnesses whose vocabulary is discovered rather
// than declared. Static-vocabulary harnesses return "" — launchBaselineRank
// does not need it and a probe would be wasted work.
//
// The read is the harness's own option discovery, which for kimi is a one-shot
// `kimi acp` handshake cached by the supervisor for the process lifetime (and
// warmed by the UI's engine picker long before any worker is launched). A
// failed probe returns "" and the caller fails closed.
func engineDefaultPermissionMode(h harness.Harness, _ harness.HarnessDescriptor) string {
	if h == nil {
		return ""
	}
	catalogue, err := h.Catalogue(context.Background(), harness.CatalogueScope{})
	if err != nil {
		return ""
	}
	for _, setting := range catalogue.Settings {
		if setting.Key == harness.SettingPermissionMode {
			return setting.DefaultValue
		}
	}
	return ""
}

// heldPermissiveness ranks the mode a thread ALREADY holds — the authority it
// is allowed to pass on. Fail-closed in the opposite direction from
// requestedPermissiveness: an empty or unrecognised mode is ranked at the most
// supervised rung, because a thread cannot delegate authority we cannot read.
// The cost of being wrong here is an extra approval prompt, never a silent
// grant.
func heldPermissiveness(mode string) int {
	m := strings.TrimSpace(mode)
	if m == "" {
		return 0
	}
	if r, ok := permissivenessRanks[m]; ok {
		return r
	}
	return 0
}

// modeLabel names a mode in the approval prompt, so an empty one does not
// render as a blank the human has to guess at.
func modeLabel(mode string) string {
	if m := strings.TrimSpace(mode); m != "" {
		return m
	}
	return "(the engine's default)"
}

// --- concurrent worker caps -------------------------------------------------

// Caps on LIVE (currently running) workers. A prompt-injected controller that
// is refused a permissive worker must not be able to get the same effect by
// launching a hundred supervised ones — fan-out is itself authority (API spend,
// parallel edits, and one human drowning in approval prompts).
//
// Both caps are counted, and a worker launching sub-workers is bound by the
// TREE cap, so a chain cannot multiply past it. The numbers are deliberately
// generous for real orchestration (an ensemble runs 2-5 workers) and tight
// enough that a runaway loop stops within seconds.
const (
	maxLiveWorkersPerParent = 8
	maxLiveWorkersPerTree   = 24
)

// orchestrationDepthCap bounds every parent-chain walk, so a cyclic chain in a
// hand-edited threads.json cannot spin a handler. Same value and reason as
// inSubtree's own cap.
const orchestrationDepthCap = 32

// treeRoot walks a thread's parent chain to the top-most ancestor — the
// controller the whole orchestration tree hangs off. A thread with no parent is
// its own root, and a chain that exceeds the depth cap reports the deepest id
// reached (bounded, never a spin).
func (d handlerDeps) treeRoot(threadID string) string {
	id := threadID
	for depth := 0; depth < orchestrationDepthCap; depth++ {
		rec, ok := d.sessions.Get(id)
		if !ok || rec.ParentThreadID == "" {
			return id
		}
		id = rec.ParentThreadID
	}
	return id
}

// liveWorkerCounts reports how many workers are running directly under
// parentID, and how many are running anywhere in parentID's orchestration tree.
// "Live" means a process is actually up: a worker that has finished and gone
// dormant frees its slot, which is what makes a cap on CONCURRENCY rather than
// on total launches the right shape.
//
// This counts only PERSISTED records, which is exactly why it cannot be the
// whole cap: a worker's record is written by launchThread, long after the gate
// ran, so N concurrent launches would all read the same pre-launch count and
// all pass. workerSlots below covers that window.
func (d handlerDeps) liveWorkerCounts(parentID string) (perParent, perTree int) {
	root := d.treeRoot(parentID)
	for _, rec := range d.sessions.List("") {
		if rec.ParentThreadID == "" { // a human-started thread is not a worker
			continue
		}
		if !d.agentRunning(rec.ThreadID) {
			continue
		}
		if rec.ParentThreadID == parentID {
			perParent++
		}
		if d.treeRoot(rec.ThreadID) == root {
			perTree++
		}
	}
	return perParent, perTree
}

// workerSlots is the reservation ledger that makes the caps a RESERVATION
// rather than a check-then-act.
//
// The window it covers: authorizeWorkerLaunch counts live workers, then the
// caller creates a worktree, spawns a CLI and waits out its handshake before
// launchThread persists the record that liveWorkerCounts can see. That window
// is seconds long and a controller can drive launch_agent concurrently, so
// counting records alone let any number of launches through one cap check.
//
// A slot is taken under the ledger's mutex TOGETHER with the live count — one
// atomic decision — and released when the launch finishes, by which point the
// persisted record carries the count. Release is idempotent so a deferred
// release after an early return cannot double-free a slot.
type workerSlots struct {
	mu     sync.Mutex
	parent map[string]int // in-flight launches, by launching thread
	tree   map[string]int // in-flight launches, by orchestration-tree root
}

func newWorkerSlots() *workerSlots {
	return &workerSlots{parent: map[string]int{}, tree: map[string]int{}}
}

// reserve takes one slot for parentID (in tree root) if both caps still allow
// it, counting reservations already held PLUS whatever count() reports live.
// count runs under the ledger's lock, which is what makes the whole check-and-
// take atomic. It reports the counts that refused, so the caller can name them.
func (s *workerSlots) reserve(parentID, root string, count func() (int, int)) (release func(), perParent, perTree int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	liveParent, liveTree := count()
	perParent = liveParent + s.parent[parentID]
	perTree = liveTree + s.tree[root]
	if perParent >= maxLiveWorkersPerParent || perTree >= maxLiveWorkersPerTree {
		return func() {}, perParent, perTree, false
	}
	s.parent[parentID]++
	s.tree[root]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.parent[parentID] <= 1 {
				delete(s.parent, parentID)
			} else {
				s.parent[parentID]--
			}
			if s.tree[root] <= 1 {
				delete(s.tree, root)
			} else {
				s.tree[root]--
			}
		})
	}, perParent, perTree, true
}

// --- what the human is actually asked ---------------------------------------

// escalationSummaryLimit mirrors the UI's permission bar, which renders
// permSummary(toolName, input) and elides it at 240 characters
// (ui/src/AgentPanel.cpp: `summary.left(240) + "…"`). The summary below is
// BUILT to that budget instead of being trimmed by the renderer: the facts the
// human decides on come first, and the agent-supplied text — a title, an
// opening line — is fitted into what is left, so a wordy worker prompt can
// never push a fact off the end of the dialog. Counted in bytes, which is
// never fewer than the characters the UI counts.
const escalationSummaryLimit = 240

// launchApprovalTool is the name the permission bar prints in "Allow the agent
// to use <name>?", and — load-bearing — the name the UI's summariser dispatches
// on.
//
// It is deliberately NOT "mcp__cooperation__launch_agent". That name IS
// digested by ui/src/AgentChatHelpers.cpp (mcpSummary's launch_agent branch),
// which prints the backend/model/title ARGUMENTS of the tool call — none of
// which this payload carries — so the escalation prompt rendered as the two
// words "same engine". The human was approving a bypassPermissions worker in
// their own checkout while reading a sentence about the ENGINE. Under a name
// the digest does not claim, permSummary falls through to its generic key scan
// (file_path, path, pattern, description) and prints our `description`
// verbatim, which is where the facts are. The keys used here must therefore
// avoid `file_path`/`path`/`pattern`, and `description` must stay present:
// TestEscalationPromptRendersTheFacts pins both against a port of the UI's own
// summariser.
//
// The parenthetical says what the prompt IS rather than asserting an escalation
// the specific launch may not be — the description below states the claim only
// when it is true.
const launchApprovalTool = "launch_agent (authority check)"

// clipTo flattens s to one line and trims it to at most max BYTES, on a rune
// boundary, marking the cut. Bytes, not characters: the UI counts characters,
// and a byte budget is never the looser of the two.
//
// capText is deliberately not reused here — it caps at the activity feed's own
// 120-byte summary limit, which would silently truncate a 240-byte dialog.
func clipTo(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(
		firstLine(s), "\r", " "), "\n", " "))
	if len(s) <= max {
		return s
	}
	if max < len("…") {
		return ""
	}
	cut := max - len("…")
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

// escalationSummary is the sentence the human decides on. Every fact the audit
// requires is in it, in decreasing order of how badly the human needs it:
//
//  1. that approving CREATES an agent holding authority the asker does not;
//  2. the mode it wants against the mode it holds;
//  3. the isolation, spelled out — "workspace" is jargon, "YOUR real files" is
//     the thing that actually happens to the user's checkout;
//  4. every other relaxation (a dropped tool ban, an added directory);
//  5. WHO is asking — thread id, then its title;
//  6. the worker's first instruction, the agent's own words.
//
// 5 and 6 come last precisely because they are attacker-controlled text, and
// every segment past the first three is fitted to the budget that is left: a
// prompt-injected agent must not be able to bury the facts under its own prose,
// nor push them past the bar's elision point with a 4 KB title.
// effIsolation is the isolation the worktree layer would ACTUALLY apply, not
// the string the caller asked for — see worktree.EffectiveIsolation.
func escalationSummary(parent session.Record, req workerLaunchRequest, relax []string,
	modeUp bool, effIsolation string) string {
	// Reserves, so the segments that come last are not starved by the ones
	// before them: enough for a thread id and a short opening line.
	const (
		askerReserve = 45
		firstReserve = 32
		modeLabelCap = 24 // a caller-supplied mode is caller-supplied TEXT
		relaxLineCap = 40
	)
	// The lead states the claim only when it is TRUE. A worker gets authority
	// the asker does not have when it out-ranks the asker's mode, sheds a
	// restriction that binds it, or reaches the real checkout from a launcher
	// confined to a worktree. Landing in the workspace is asked about even when
	// the launcher is already there (a worker in the user's files is the human's
	// call every time), and in exactly that case the blanket claim would be a
	// lie — the kind that teaches a human to stop reading these dialogs.
	beyond := modeUp || len(relax) > 0 ||
		(effIsolation == worktree.ModeWorkspace && parent.Worktree.Isolated)
	lead := "Approve = a NEW agent with the authority below."
	if beyond {
		lead = "Approve = a NEW agent with MORE authority than the asker."
	}
	parts := []string{
		lead,
		"Mode " + clipTo(modeLabel(req.PermissionMode), modeLabelCap) +
			" vs its own " + clipTo(modeLabel(parent.PermissionMode), modeLabelCap) + ".",
	}
	if effIsolation == worktree.ModeWorkspace {
		parts = append(parts, "Isolation workspace = YOUR real files.")
	} else {
		parts = append(parts, "Isolation: its own worktree.")
	}
	used := len(strings.Join(parts, " "))

	for _, r := range relax {
		room := escalationSummaryLimit - used - askerReserve - firstReserve
		if room > relaxLineCap {
			room = relaxLineCap
		}
		if room < 12 {
			break
		}
		line := clipTo(r, room)
		parts = append(parts, line)
		used += len(line) + 1
	}

	asker := "From " + clipTo(req.ParentThreadID, 24)
	if room := escalationSummaryLimit - used - firstReserve - len(asker) - 4; room > 8 {
		if t := clipTo(parent.Title, room); t != "" {
			asker += ` "` + t + `"`
		}
	}
	asker += "."
	parts = append(parts, asker)
	used += len(asker) + 1

	if room := escalationSummaryLimit - used - len(`First: "".`); room > 8 {
		if p := clipTo(req.Prompt, room); p != "" {
			parts = append(parts, `First: "`+p+`".`)
		}
	}
	// Belt and braces: whatever the inputs were, the whole line fits the bar.
	return clipTo(strings.Join(parts, " "), escalationSummaryLimit)
}

// --- the gate ---------------------------------------------------------------

// workerLaunchRequest is what an agent asked for, as agent.launchWorker
// received it — before anything is created.
type workerLaunchRequest struct {
	ParentThreadID string
	Backend        string
	PermissionMode string
	Isolation      string
	Title          string
	Prompt         string
	// The per-thread restriction fields. nil means "not asked for", which
	// inheritRestrictions turns into the PARENT'S OWN restrictions — never into
	// "unrestricted". A non-nil value that drops one of the parent's denies, or
	// reaches a directory the parent cannot, is an escalation like any other.
	DisallowedTools []string
	AddDirs         []string
}

// inheritRestrictions decides the restriction fields the worker actually
// launches with (audit F1, third bug): the parent's, unless the caller asked
// for something else.
//
// Before this, agentStartParams was built without DisallowedTools/AddDirs at
// all, so EVERY worker escaped its parent's deny-list by default — a thread
// launched with `--disallowedTools Bash` could launch a worker with Bash, in
// the same worktree, and never involve the human. The inheritance is the fix;
// the relaxation report is what makes an explicit widening visible instead.
// The escalation check and the launch MUST see the same strings, which is why
// every list is normalised HERE, once, at the boundary — and why the launch
// gets the NORMALISED values back rather than the caller's raw slice.
//
// Before this, missingFrom compared entries with TrimSpace on both sides while
// the caller's untrimmed slice went to the CLI: " Bash" matched the parent's
// "Bash" and passed the escalation check, then reached `--disallowedTools
// " Bash"` as a different string, banning nothing. One leading space and the
// worker had a tool its launcher was denied, with no prompt anywhere.
func inheritRestrictions(parent session.Record, req workerLaunchRequest) (disallowed, addDirs []string, relax []string) {
	// The parent's own lists are normalised too: they come off disk, where
	// anything running as the user can have edited them, and both sides of the
	// comparison must be normalised for the comparison to mean anything.
	parentTools := normalizeEntries(parent.DisallowedTools)
	parentDirs := normalizeEntries(parent.AddDirs)

	disallowed = normalizeEntries(req.DisallowedTools)
	if disallowed == nil {
		disallowed = parentTools
	} else if dropped := missingFrom(parentTools, disallowed); len(dropped) > 0 {
		relax = append(relax, "Un-bans tools: "+clipTo(strings.Join(dropped, ", "), 40)+".")
	}
	addDirs = normalizeEntries(req.AddDirs)
	if addDirs == nil {
		addDirs = parentDirs
	} else if extra := missingFrom(addDirs, parentDirs); len(extra) > 0 {
		relax = append(relax, "Adds dirs you lack: "+clipTo(strings.Join(extra, ", "), 40)+".")
	}
	return disallowed, addDirs, relax
}

// normalizeEntries trims every entry and drops the ones that are empty
// afterwards, preserving order.
//
// nil in, nil out: for these fields nil means "not asked for" (inherit the
// parent's) while an EMPTY non-nil slice means "explicitly none" (a relaxation
// that must be reported), and normalising must not turn one into the other. A
// non-nil slice of nothing but blanks stays non-nil for the same reason.
func normalizeEntries(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// missingFrom returns the entries of want that have no match in have. Both
// slices must already be normalised (see normalizeEntries) — comparing raw and
// normalised entries is the bug this pair exists to close.
func missingFrom(want, have []string) []string {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

// authorizeWorkerLaunch is the human gate on an agent launching an agent.
//
// It RESERVES a worker slot (refusing outright when a concurrency cap is
// exceeded — a cap is a limit, not a question; asking the human to raise it on
// the agent's behalf would make it no cap at all), and asks the human — through
// the SAME broker and the same permission.requested flow every gated tool uses,
// so the ask appears in the launching agent's own panel — whenever the launch
// would hand the worker authority the launcher does not itself hold:
//
//   - a permission mode MORE PERMISSIVE than the parent thread's own, on the
//     shared ordering above; and/or
//   - an EFFECTIVE isolation of "workspace" — the worker running in the user's
//     MAIN CHECKOUT rather than a throwaway worktree. That is authority, not a
//     preference, and it is asked for regardless of the parent's own isolation:
//     a worker already running in the workspace still may not seed more of them
//     without the human seeing it. Effective, not requested: worktree.Create
//     degrades "auto" to the real checkout in a project with no commits, so the
//     gate asks the worktree layer what this project would actually do rather
//     than matching the caller's string (worktree.EffectiveIsolation); and/or
//   - a restriction RELAXATION: a deny-list shorter than the parent's, or a
//     working root the parent does not have.
//
// It returns a release func for the reserved slot. The caller must call it when
// the launch has finished, one way or the other — from then on the worker's own
// persisted record is what the cap counts.
//
// A refusal (or a timeout, or no UI to ask) means the worker is NOT launched.
//
// WHAT ELSE CROSSES THIS BOUNDARY, and why it is or is not gated here:
//
//   - SystemPrompt / Agents (custom subagent profiles): persona text. A
//     subagent runs under the WORKER's permission mode, and its `tools` list can
//     only narrow the session's tool set, never add a tool the session lacks —
//     so neither channel confers authority the mode gate has not already
//     covered. Both are reported as applied-truth. NOT gated, by decision.
//   - Backend: picking an engine cannot exceed what the human installed and
//     configured; the credentials come from the core's own env/KWallet either
//     way. Its risk is a DIFFERENT MODE VOCABULARY, which the shared rank table
//     above handles (unknown mode -> asks). NOT gated, by decision.
//   - Model / Effort: spend, not authority. Bounded by the worker caps and by
//     the harness's own budget ceiling. NOT gated, by decision.
//   - StrictMCPConfig / MaxBudgetUSD: restrictions, not authority, and not
//     caller-supplied here — the worker inherits the parent's, so a thread
//     isolated from the human's global MCP servers cannot launch one that is
//     not, and a spend ceiling is not shed by spawning.
//   - Cowork (desktop control): gated separately and unconditionally by
//     askCoworkEnable in the caller, immediately AFTER this gate — so a launch
//     that is going to be refused on authority never spends a desktop-access
//     prompt on the human first.
//   - WorkspacePath: NOT caller-supplied here — launchWorker roots every worker
//     in the PARENT'S OWN PROJECT. An agent cannot point a worker at another
//     repo. (agent.start, where the path IS caller-supplied, is UI-only; see
//     audit F5.)
//   - Env: deliberately absent from the launch_agent surface (see
//     harness.StartSpec.Env) — it is applied after provider credential
//     scrubbing and could redirect a provider token, so no agent-facing path
//     may set it.
func (d handlerDeps) authorizeWorkerLaunch(parent session.Record,
	descriptor harness.HarnessDescriptor, engineDefault string, req workerLaunchRequest) (func(), error) {
	noop := func() {}
	// Fail closed: without the ledger there is no way to bound fan-out, and an
	// unbounded launch path is exactly what the cap exists to prevent. This is
	// a wiring error (run.go builds one), never a runtime condition.
	if d.workerSlots == nil {
		return noop, ipc.Errorf(ipc.CodeInternalError,
			"NOT APPLIED: worker launches are unavailable (no reservation ledger); "+
				"the worker was not launched")
	}
	// Caps first, and as a RESERVATION: a refusal that costs nothing should
	// never be preceded by a prompt the human then has to answer for a launch
	// that cannot happen, and the slot must be held across the approval so that
	// N concurrent launches cannot all pass one count.
	release, perParent, perTree, ok := d.workerSlots.reserve(req.ParentThreadID,
		d.treeRoot(req.ParentThreadID),
		func() (int, int) { return d.liveWorkerCounts(req.ParentThreadID) })
	if !ok {
		if perParent >= maxLiveWorkersPerParent {
			return noop, ipc.Errorf(ipc.CodeInvalidParams,
				"NOT APPLIED: you already have "+strconv.Itoa(perParent)+
					" workers running or starting, the limit is "+
					strconv.Itoa(maxLiveWorkersPerParent)+
					" — wait for one to finish (wait_agent) or retire one (close_agent) "+
					"before launching another; the worker was not launched")
		}
		return noop, ipc.Errorf(ipc.CodeInvalidParams,
			"NOT APPLIED: this orchestration tree already has "+strconv.Itoa(perTree)+
				" workers running or starting, the limit is "+
				strconv.Itoa(maxLiveWorkersPerTree)+
				" — the crew must finish work before it grows; the worker was not launched")
	}

	var why []string
	wantRank := requestedPermissiveness(req.PermissionMode)
	if strings.TrimSpace(req.PermissionMode) == "" {
		wantRank = launchBaselineRank(descriptor, engineDefault)
	}
	haveRank := heldPermissiveness(parent.PermissionMode)
	if wantRank > haveRank {
		why = append(why, "the worker would run with MORE authority than you have")
	}
	// The EFFECTIVE isolation, not the requested word. worktree.Create degrades
	// "auto" (and the unspecified default) to the human's real checkout whenever
	// the project has no commit to branch from — a fresh repo, or a workspace
	// that is not a git repository — so gating on the literal string "workspace"
	// let a worker land in the user's own files with the gate silent. The
	// question is asked of the same layer that will answer it for real, against
	// the same project root launchThread roots the worker in (parent.Project).
	effIsolation := worktree.EffectiveIsolation(parent.Project, req.Isolation)
	if effIsolation == worktree.ModeWorkspace {
		why = append(why, "the worker would run in the human's main checkout, "+
			"not an isolated worktree")
	}
	_, _, relax := inheritRestrictions(parent, req)
	if len(relax) > 0 {
		why = append(why, "the worker would shed restrictions that bind you")
	}
	if len(why) == 0 {
		return release, nil
	}
	// Fail closed: with no broker or no server there is no human to ask, and an
	// unaskable question is a refusal, never an approval.
	if d.broker == nil || d.srv == nil {
		release()
		return noop, ipc.Errorf(ipc.CodeInternalError,
			"NOT APPLIED: this launch needs the human's approval and there is no "+
				"way to ask them; the worker was not launched")
	}

	// The established approval vocabulary: who is asking, what changes, and the
	// caller's own words for why — never a paraphrase. `description` is the one
	// the UI actually renders (see launchApprovalTool); the structured keys
	// beside it are for any surface that wants the fields rather than the
	// sentence.
	input, err := json.Marshal(map[string]any{
		"description": escalationSummary(parent, req, relax,
			wantRank > haveRank, effIsolation),
		"launchingThreadId":    req.ParentThreadID,
		"launchingThreadTitle": parent.Title,
		"launchingThreadMode":  modeLabel(parent.PermissionMode),
		"requestedMode":        modeLabel(req.PermissionMode),
		"requestedIsolation":   orDefault(req.Isolation, worktree.ModeAuto),
		// What that request MEANS for this project: "auto" in a repo with no
		// commits is the human's own checkout, and the dialog says so.
		"effectiveIsolation": effIsolation,
		"requestedBackend":   orDefault(req.Backend, parent.Backend),
		"workerTitle":        orDefault(req.Title, "a new worker"),
		"reason":             capText(firstLine(req.Prompt)),
		"why":                strings.Join(why, "; "),
	})
	if err != nil {
		// Fail closed: an ask we cannot render is an ask the human never saw.
		release()
		return noop, ipc.Errorf(ipc.CodeInternalError,
			"NOT APPLIED: this launch needs the human's approval and the request "+
				"could not be rendered for them; the worker was not launched")
	}
	dec, ok := askHumanPermission(d.srv, d.broker, req.ParentThreadID,
		launchApprovalTool, input)
	if !ok || !dec.Allow {
		release()
		return noop, ipc.Errorf(ipc.CodeInvalidParams,
			"NOT APPLIED: the human did not approve this launch ("+
				strings.Join(why, "; ")+"; requested permission_mode "+
				modeLabel(req.PermissionMode)+" against your own "+
				modeLabel(parent.PermissionMode)+", isolation "+
				orDefault(req.Isolation, worktree.ModeAuto)+
				" = "+effIsolation+
				") — the worker was not launched. Launch it with your own "+
				"permission mode and an isolated worktree, or ask the human directly.")
	}
	// Deliberately NOT cached in orchGrants: every escalating launch is its own
	// act of authority. One approval must never cover the next one.
	return release, nil
}
