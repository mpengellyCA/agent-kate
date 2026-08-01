package main

// Per-bridge identity secrets (audit F13, bridge half).
//
// THE RULE: a connection becomes thread T's agent bridge by PROVING it is the
// bridge akcore spawned for T — never by asking to be, and never twice over.
//
// Before this, `bridge.identify` (and every handler that bound a connection on
// first use) was trust-on-first-use: any connection that had not yet identified
// could name any thread id and become that thread's bridge, inheriting the
// thread's authority — its permission mode, its worktree, its right to launch
// workers and to hold desktop consent. A prompt-injected agent needed one Bash
// command to read the socket path out of its own bridge's argv, one connection,
// and one JSON line to act as the most privileged thread in the arena.
//
// Now akcore mints a fresh secret PER BRIDGE PROCESS per launch and hands it to
// that bridge in its environment (never argv, which is world-readable — see
// docs/security-model.md §1). `bridge.identify` demands it back.
//
// And a redeemed secret is SPENT while the bridge that redeemed it is alive: a
// second connection presenting the same secret is refused (the replay gate
// below). That is the difference between a secret and a password — the secret
// still travels through channels a same-uid process can read (a bridge's
// environment; the claude `--mcp-config` file, 0600 in a 0700 directory), so
// what stops a thief is not that they cannot READ it but that they cannot USE
// it while its real holder is connected. A holder that disconnects frees its
// slot at once, so a bridge whose engine respawns it is never locked out.
//
// WHAT THIS IS NOT: a boundary against a determined same-uid process. Agents
// run at the user's own uid, so /proc/<pid>/environ of another thread's live
// bridge — and the mcp-config file of a running claude thread — are readable in
// principle, and a thief who wins the race to a NOT-yet-redeemed secret (an MCP
// server the engine has not spawned yet) takes the slot and locks the real
// bridge out, loudly. It removes the free-for-all and it makes the surviving
// attack a race that leaves evidence, and that is the whole of the claim. See
// the F13 section of docs/security-model.md.

import (
	"crypto/rand"
	"crypto/subtle"
	"sync"

	"agentkate/internal/ipc"
)

// bridgeSecretEnvVar carries a bridge's secret from akcore to the `akcore mcp`
// child. Environment, not argv: /proc/<pid>/cmdline is world-readable while
// /proc/<pid>/environ is owner-only, and the socket path already in argv is
// what made the old forgery a one-liner.
const bridgeSecretEnvVar = "AGENTKATE_BRIDGE_SECRET"

// bridgesPerLaunch is how many `akcore mcp` processes one launch spawns:
// Cooperation and Cowork, on both shipped engines. Each gets its OWN secret
// (mintLaunch), which is what makes the one-live-holder rule above workable —
// with a single shared secret the second bridge would look exactly like a
// replay of the first.
const bridgesPerLaunch = 2

// bridgeSecretsKept is how many of a thread's most recent LAUNCHES keep valid
// secrets.
//
// Two, not one: a relaunch (resume, promote, a Cowork re-attach) mints fresh
// secrets while the previous run's bridges may still be winding down, and a CLI
// that respawns a crashed MCP server re-reads the config written at ITS launch.
// Invalidating those would silently kill a live thread's cooperation tools. Two
// bounds how long a superseded secret lives without making a relaunch fragile.
const bridgeSecretsKept = 2

// connIdentity is the connection a secret is redeemed by: an opaque comparable
// key plus a liveness probe. It is an indirection over ipc.ConnToken so the
// ledger's rules can be tested without a socket, and so the ledger holds no
// opinion about what a connection is.
type connIdentity struct {
	key  any
	live func() bool
}

// connIdentityOf lifts a calling connection into a ledger identity. A nil ref
// (an internally-synthesized call with no connection behind it) yields the zero
// identity, which redeem refuses.
func connIdentityOf(ref *ipc.ConnRef) connIdentity {
	if ref == nil {
		return connIdentity{}
	}
	tok := ref.Token()
	return connIdentity{key: tok, live: tok.Live}
}

func (c connIdentity) valid() bool { return c.key != nil && c.live != nil }

// bridgeSecretEntry is one issued secret and the connection currently holding
// it (the zero identity until one redeems it).
type bridgeSecretEntry struct {
	value  string
	holder connIdentity
}

// held reports whether some LIVE connection other than want holds this entry.
// A dead holder holds nothing: that is the reconnect path.
func (e *bridgeSecretEntry) held(by connIdentity) bool {
	if !e.holder.valid() || !e.holder.live() {
		return false
	}
	return e.holder.key != by.key
}

// bridgeSecrets is the per-thread ledger of secrets akcore has handed out.
// In memory only, for the lifetime of the core process.
type bridgeSecrets struct {
	mu       sync.Mutex
	byThread map[string][]*bridgeSecretEntry // thread id -> live secrets, oldest first
}

func newBridgeSecrets() *bridgeSecrets {
	return &bridgeSecrets{byThread: map[string][]*bridgeSecretEntry{}}
}

// launchSecrets is one launch's set: one secret per bridge process, so each is
// redeemable exactly once at a time.
type launchSecrets struct {
	Coop   string
	Cowork string
}

// mintLaunch issues this launch's secrets — one per bridge the launch will
// spawn — and retires anything older than bridgeSecretsKept launches.
func (b *bridgeSecrets) mintLaunch(threadID string) launchSecrets {
	return launchSecrets{Coop: b.mint(threadID), Cowork: b.mint(threadID)}
}

// mint issues one fresh secret for threadID's next bridge and returns it. The
// previous secrets stay valid up to bridgeSecretsKept launches' worth.
//
// A nil ledger (a handlerDeps assembled without one) returns "": the launch
// still happens, but no bridge can identify, which is the fail-closed
// direction — a bridge with no tools rather than an unauthenticated one.
func (b *bridgeSecrets) mint(threadID string) string {
	if b == nil || threadID == "" {
		return ""
	}
	// rand.Text is crypto/rand's own random string: 26 base32 characters, ~130
	// bits, and it panics rather than returning a weak value if the system
	// entropy source fails.
	secret := rand.Text()
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := append(b.byThread[threadID], &bridgeSecretEntry{value: secret})
	if max := bridgeSecretsKept * bridgesPerLaunch; len(kept) > max {
		kept = kept[len(kept)-max:]
	}
	b.byThread[threadID] = kept
	return secret
}

// redeem is the authentication step: it reports whether presented is one of
// threadID's live secrets AND claims it for by, which is what makes a replay
// from a second connection fail.
//
// FAILS CLOSED everywhere: a nil ledger, an unknown thread, an empty presented
// secret, an empty stored secret, a caller with no connection identity, and a
// secret whose slot is held by another live connection are all refusals. The
// comparison is constant-time and never exits early, so a wrong guess leaks
// nothing about how wrong it was.
//
// Idempotent for the SAME connection: a bridge that re-sends its identify (as
// IdentifyBridge itself allows) keeps its slot rather than colliding with
// itself.
func (b *bridgeSecrets) redeem(threadID, presented string, by connIdentity) bool {
	if b == nil || threadID == "" || presented == "" || !by.valid() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var match *bridgeSecretEntry
	for _, e := range b.byThread[threadID] {
		if e.value == "" {
			continue
		}
		// No early exit: every candidate is compared, so the answer does not
		// depend on WHICH secret matched.
		if subtle.ConstantTimeCompare([]byte(e.value), []byte(presented)) == 1 {
			match = e
		}
	}
	if match == nil || match.held(by) {
		return false
	}
	match.holder = by
	return true
}

// heldElsewhere reports whether presented IS one of threadID's secrets but is
// already claimed by another live connection — the replay case, as opposed to a
// wrong or stale secret.
//
// For the LOG ONLY. The caller-facing refusal is deliberately identical for
// every failure, so this distinction never reaches the network and is no
// oracle; it exists so that the one failure mode that looks like a bug (a real
// bridge that lost the race to a thief, or to its own predecessor) is
// diagnosable from the core's log instead of indistinguishable from a
// misconfigured environment.
func (b *bridgeSecrets) heldElsewhere(threadID, presented string, by connIdentity) bool {
	if b == nil || threadID == "" || presented == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.byThread[threadID] {
		if e.value == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(e.value), []byte(presented)) == 1 && e.held(by) {
			return true
		}
	}
	return false
}

// release gives a redeemed slot back. It is the unwind for a redemption whose
// binding then failed (a connection already bound to another thread), so a
// failed attempt cannot park a slot the real bridge needs. Only the holder may
// release.
func (b *bridgeSecrets) release(threadID, presented string, by connIdentity) {
	if b == nil || threadID == "" || presented == "" || !by.valid() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.byThread[threadID] {
		if e.value == presented && e.holder.valid() && e.holder.key == by.key {
			e.holder = connIdentity{}
		}
	}
}

// forget drops every secret for a thread that is gone for good, so a thread id
// that is later reused cannot be identified against a dead thread's secret.
func (b *bridgeSecrets) forget(threadID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.byThread, threadID)
}
