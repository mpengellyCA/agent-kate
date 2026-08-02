package cowork

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SECURITY (audit F32): a policy toggle is a persisted standing grant — no per-action
// prompt, for any cowork-enabled agent, overriding even the R2 default. So a
// capability with no tool behind it must not be toggleable, and a stale entry left by
// an older build must never become live when the tool eventually ships.

func TestUnimplementedCapabilitiesAreNotToggleable(t *testing.T) {
	// screencast and vd_sandbox have no tool in coworkToolDefs ("land in v3").
	for _, c := range []Capability{CapScreencast, CapVDSandbox, CapRemoteDesktop} {
		if Toggleable(c) {
			t.Fatalf("%q has no tool behind it and must not be toggleable", c)
		}
		for _, t2 := range AllToggleable() {
			if t2 == c {
				t.Fatalf("%q must not appear in AllToggleable()", c)
			}
		}
	}
	// The implemented ones must still be there — this test has to fail if the fix is
	// over-applied as well as under-applied.
	for _, c := range []Capability{CapWindowList, CapScreenshot, CapA11yRead,
		CapLaunchBrowser, CapA11yAction, CapInputInject, CapPointerControl} {
		if !Toggleable(c) {
			t.Fatalf("%q is implemented and must stay toggleable", c)
		}
	}
}

func TestPolicyRefusesToArmUnimplementedCapability(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "cowork-policy.json"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if err := p.Set(CapScreencast, true); err == nil {
		t.Fatal("Set(screencast, true) must be refused")
	}
	if p.Allows(CapScreencast) {
		t.Fatal("screencast must not be pre-authorized after a refused Set")
	}
	// A real capability still works, so the guard is not a blanket refusal.
	if err := p.Set(CapScreenshot, true); err != nil {
		t.Fatalf("Set(screenshot, true): %v", err)
	}
	if !p.Allows(CapScreenshot) {
		t.Fatal("screenshot must be pre-authorized after Set")
	}
}

// seedPolicyFile writes a policy file containing both stale and live entries — the
// situation of a user who flipped "Watch the screen" on an older build.
func seedPolicyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cowork-policy.json")
	b, err := json.MarshalIndent(policyFile{SchemaVersion: policySchemaVersion,
		Enabled: []Capability{CapScreencast, CapVDSandbox, CapScreenshot}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readPolicyFile(t *testing.T, path string) policyFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pf policyFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatalf("policy file is not valid JSON: %v (%s)", err, raw)
	}
	return pf
}

// The load path is the one that matters for that user: the entry is on disk already,
// and it must never be honoured.
func TestStalePolicyEntryIsIgnored(t *testing.T) {
	path := seedPolicyFile(t)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.Allows(CapScreencast) || p.Allows(CapVDSandbox) {
		t.Fatal("a persisted entry for an unimplemented capability must NOT be honoured")
	}
	if !p.Allows(CapScreenshot) {
		t.Fatal("the implemented capability in the same file must survive")
	}
	if list := p.List(); len(list) != 1 || !list[CapScreenshot] {
		t.Fatalf("List() must report only the surviving toggle, got %v", list)
	}
}

// SECURITY (audit F35): loading the policy is a READ. It used to MkdirAll + WriteFile +
// Rename the data dir whenever it saw a stale entry — a load-time disk mutation that
// breaks a read-only data dir and lets a second akcore instance rewrite the file under
// the first one at startup. The in-memory posture is correct either way, so nothing is
// lost by leaving the bytes alone.
func TestLoadPolicyDoesNotTouchTheDisk(t *testing.T) {
	path := seedPolicyFile(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	entriesBefore, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LoadPolicy(path); err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("LoadPolicy rewrote the policy file.\nbefore: %s\nafter:  %s", before, after)
	}
	entriesAfter, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("LoadPolicy created files in the data dir: %d -> %d", len(entriesBefore), len(entriesAfter))
	}
}

// …and the prune still happens, just at the next legitimate write, so the stale entry
// cannot revive if the capability is ever added back to AllToggleable().
func TestStalePolicyEntryIsPrunedByTheNextWrite(t *testing.T) {
	path := seedPolicyFile(t)
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if err := p.Set(CapWindowList, true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	after := readPolicyFile(t, path)
	for _, c := range after.Enabled {
		if c == CapScreencast || c == CapVDSandbox {
			t.Fatalf("stale entry %q survived the next write: %v", c, after.Enabled)
		}
	}
	if len(after.Enabled) != 2 {
		t.Fatalf("expected screenshot + window_list on disk, got %v", after.Enabled)
	}
}

// Clear() writes only when something live changed, which would have skipped the cleanup
// for a file whose ONLY entries are stale. p.stale is what closes that hole.
func TestClearPrunesStaleEntriesWithNothingLiveEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cowork-policy.json")
	b, err := json.MarshalIndent(policyFile{SchemaVersion: policySchemaVersion,
		Enabled: []Capability{CapScreencast}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	p.Clear()
	if got := readPolicyFile(t, path).Enabled; len(got) != 0 {
		t.Fatalf("Clear must prune the stale-only file, got %v", got)
	}
}

// SECURITY (audit F35): the panic button was in-memory only, so quitting akcore
// un-pressed it and the panel came back reading "Stop ALL desktop access" as though the
// user had never hit it. It latches on disk now, and only an explicit re-arm lifts it.
func TestKillSwitchLatchSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cowork-policy.json")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.Killed() {
		t.Fatal("a fresh policy must not start killed")
	}
	if err := p.SetKilled(true); err != nil {
		t.Fatalf("SetKilled: %v", err)
	}

	reloaded, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Killed() {
		t.Fatal("the kill-switch latch must survive a restart")
	}
	if err := reloaded.SetKilled(false); err != nil {
		t.Fatalf("SetKilled(false): %v", err)
	}
	again, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("reload after re-arm: %v", err)
	}
	if again.Killed() {
		t.Fatal("an explicit re-arm must clear the latch on disk too")
	}
}

// Defence in depth: even if an entry somehow reaches the in-memory map, the read
// path denies it. This is the check that keeps Authorize's policy fast-path honest.
func TestAllowsDeniesNonToggleableEvenIfPresentInMemory(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "cowork-policy.json"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	p.mu.Lock()
	p.enabled[CapScreencast] = true
	p.mu.Unlock()
	if p.Allows(CapScreencast) {
		t.Fatal("Allows must deny a capability that is not toggleable")
	}
}
