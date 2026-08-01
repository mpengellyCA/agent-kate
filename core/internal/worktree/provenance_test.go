// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the provenance gate (audit finding F4). The threat is a
// session record — threads.json, owner-writable, so reachable by a
// prompt-injected agent — whose Worktree.Path has been repointed at a
// directory Agent Kate never created. Every destructive entry point must
// refuse such a record rather than delete what it names.

// victimDir creates a directory of "the user's other project" with a file in
// it, outside any repo, and returns its path.
func victimDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "irreplaceable.txt"),
		[]byte("a decade of work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertStillThere(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "irreplaceable.txt")); err != nil {
		t.Fatalf("the unrelated directory was destroyed: %v", err)
	}
}

// A record pointing outside <repoRoot>/.agentkate/worktrees/ is refused, and
// nothing at that path is touched.
func TestRemoveRefusesPathOutsideWorktreesDir(t *testing.T) {
	repo := initRepo(t)
	victim := victimDir(t)

	wt := Worktree{
		ThreadID: "t-tampered",
		RepoRoot: repo,
		Path:     victim,
		Branch:   "agentkate/t-tampered",
		Isolated: true,
	}
	err := Remove(wt)
	if err == nil {
		t.Fatal("Remove accepted a record pointing outside the worktrees directory")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("error should say it refused, got: %v", err)
	}
	assertStillThere(t, victim)
}

// The same record must never be classified removable by the analysis the UI
// pre-checks rows from — see the AnalyzeCandidate test in internal/gitstatus.
// Here we pin the underlying gate directly.
func TestVerifyProvenanceRefusesForeignPath(t *testing.T) {
	repo := initRepo(t)
	if err := VerifyProvenance(Worktree{
		RepoRoot: repo, Path: victimDir(t), Isolated: true,
	}); err == nil {
		t.Fatal("VerifyProvenance accepted a foreign path")
	}
}

// A path that is lexically inside the worktrees directory but symlinks out of
// it is refused: containment is measured after resolving symlinks.
func TestRemoveRefusesSymlinkedEscape(t *testing.T) {
	repo := initRepo(t)
	victim := victimDir(t)

	wtDir := filepath.Join(repo, ".agentkate", "worktrees")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wtDir, "t-link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	wt := Worktree{
		ThreadID: "t-link",
		RepoRoot: repo,
		Path:     link,
		Branch:   "agentkate/t-link",
		Isolated: true,
	}
	if err := Remove(wt); err == nil {
		t.Fatal("Remove followed a symlink out of the worktrees directory")
	}
	assertStillThere(t, victim)
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the symlink itself should be left alone: %v", err)
	}
}

// A directory sitting inside .agentkate/worktrees/ that git does not list as a
// worktree is not one of ours either: git's own remove fails and the manual
// os.RemoveAll fallback must refuse rather than delete it.
func TestRemoveRefusesUnregisteredDirInsideWorktreesDir(t *testing.T) {
	repo := initRepo(t)
	dir := filepath.Join(repo, ".agentkate", "worktrees", "t-planted")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "irreplaceable.txt"),
		[]byte("planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wt := Worktree{
		ThreadID: "t-planted",
		RepoRoot: repo,
		Path:     dir,
		Branch:   "agentkate/t-planted",
		Isolated: true,
	}
	if err := Remove(wt); err == nil {
		t.Fatal("Remove deleted a directory git does not know as a worktree")
	}
	assertStillThere(t, dir)
}

// The gate must not break the real thing: a worktree this package created is
// still removed cleanly, directory and branch.
func TestRemoveLegitimateWorktreeStillWorks(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-real", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Remove(wt); err != nil {
		t.Fatalf("Remove refused a legitimate worktree: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory survived removal: %v", err)
	}
	out, _ := git(repo, "branch", "--list", wt.Branch)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("branch %s survived removal: %q", wt.Branch, out)
	}
}

// An orphaned record — contained path, directory already deleted out of band —
// still cleans up its git bookkeeping. Requiring git registration here would
// strand such rows in the UI forever, so the gate deliberately skips that half
// of the check when there is nothing on disk to destroy.
func TestRemoveOrphanedRecordStillPrunes(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-orphan", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatal(err)
	}
	if err := Remove(wt); err != nil {
		t.Fatalf("Remove refused an orphaned record: %v", err)
	}
	out, _ := git(repo, "worktree", "list", "--porcelain")
	if strings.Contains(out, wt.Path) {
		t.Fatalf("worktree bookkeeping was not pruned:\n%s", out)
	}
}

// A record whose Branch was rewritten to a branch this package never creates
// must not turn worktree removal into the loss of the user's own branch.
func TestRemoveNeverDeletesAnUnmanagedBranch(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-branch", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A branch of the user's own that is NOT checked out anywhere — git would
	// happily delete it, so only our own prefix check stands in the way.
	mustGit(t, repo, "branch", "release-1.0")
	tampered := wt
	tampered.Branch = "release-1.0"

	if err := Remove(tampered); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	out, _ := git(repo, "branch", "--list", "release-1.0")
	if strings.TrimSpace(out) == "" {
		t.Fatal("removal deleted the user's own branch release-1.0")
	}
}

// DiscardChanges destroys uncommitted work as thoroughly as a delete, so it is
// gated too: a record pointing at an unrelated git repo is refused.
func TestDiscardChangesRefusesForeignPath(t *testing.T) {
	repo := initRepo(t)
	other := initRepo(t) // the user's other checkout, with uncommitted work
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("unsaved\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wt := Worktree{
		ThreadID: "t-discard",
		RepoRoot: repo,
		Path:     other,
		Branch:   "agentkate/t-discard",
		Isolated: true,
	}
	if err := DiscardChanges(wt); err == nil {
		t.Fatal("DiscardChanges accepted a record pointing at another repo")
	}
	got, err := os.ReadFile(filepath.Join(other, "a.txt"))
	if err != nil || string(got) != "unsaved\n" {
		t.Fatalf("uncommitted work in the other repo was reset: %q %v", got, err)
	}
}

// A direct-workspace record whose Path no longer matches its RepoRoot is
// self-inconsistent — the only shape the codebase creates is Path == RepoRoot.
func TestDiscardChangesRefusesWorkspaceRecordThatDisagreesWithItself(t *testing.T) {
	repo := initRepo(t)
	other := initRepo(t)
	if err := DiscardChanges(Worktree{
		ThreadID: "ws", RepoRoot: repo, Path: other, Isolated: false,
	}); err == nil {
		t.Fatal("DiscardChanges accepted a workspace record pointing elsewhere")
	}
	// The honest shape still works.
	if err := DiscardChanges(Worktree{
		ThreadID: "ws", RepoRoot: repo, Path: repo, Isolated: false,
	}); err != nil {
		t.Fatalf("DiscardChanges refused a legitimate workspace record: %v", err)
	}
}

// resolveExisting must tolerate a missing leaf (that is how orphaned records
// are still checked for containment) but never silently pass a path it cannot
// evaluate.
func TestResolveExistingTolerantOfMissingLeaf(t *testing.T) {
	root := t.TempDir()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveExisting(filepath.Join(root, "gone", "deeper"))
	if err != nil {
		t.Fatalf("resolveExisting: %v", err)
	}
	if want := filepath.Join(real, "gone", "deeper"); got != want {
		t.Fatalf("resolveExisting = %q, want %q", got, want)
	}
	if _, err := resolveExisting(""); err == nil {
		t.Fatal("an empty path must be an error, not a pass")
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// A record whose whole project directory the user has since deleted must stay
// cleanable: nothing exists to destroy, so the gate falls back to lexical
// containment rather than stranding the row forever.
func TestRemoveTolerantOfADeletedProject(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-gone", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if err := Remove(wt); err != nil {
		t.Fatalf("Remove refused a record whose project is gone: %v", err)
	}
	// …but a record that points elsewhere is still refused, project gone or not.
	victim := victimDir(t)
	if err := Remove(Worktree{
		ThreadID: "t-gone2", RepoRoot: repo, Path: victim,
		Branch: "agentkate/t-gone2", Isolated: true,
	}); err == nil {
		t.Fatal("a foreign path was accepted once the project directory vanished")
	}
	assertStillThere(t, victim)
}

// --- the containment ROOT itself (audit F4, second pass) --------------------
//
// The first fix resolved <RepoRoot>/.agentkate/worktrees through symlinks and
// then asked "is the target inside it". That is not containment: .agentkate/
// lives inside the repository the agent has write access to, so the agent
// picks what the root resolves to. These tests pin the root.

// .agentkate replaced by a symlink to the victim's parent: the "worktrees"
// directory then resolves under the victim, and every path there would have
// passed the old prefix test.
func TestRemoveRefusesSymlinkedAgentkateDir(t *testing.T) {
	repo := initRepo(t)
	victim := victimDir(t)
	// The attacker's staging area: <victim>/worktrees/t-x holds the file we
	// must not lose, reached as <repo>/.agentkate/worktrees/t-x.
	planted := filepath.Join(victim, "worktrees", "t-x")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "irreplaceable.txt"),
		[]byte("a decade of work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(repo, ".agentkate")); err != nil {
		t.Fatal(err)
	}

	wt := Worktree{
		ThreadID: "t-x", RepoRoot: repo,
		Path:   filepath.Join(repo, ".agentkate", "worktrees", "t-x"),
		Branch: "agentkate/t-x", Isolated: true,
	}
	err := Remove(wt)
	if err == nil {
		t.Fatal("Remove accepted a record whose .agentkate is a symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should name the symlink, got: %v", err)
	}
	assertStillThere(t, planted)
	if err := VerifyRunPath(wt); err == nil {
		t.Fatal("VerifyRunPath accepted a record whose .agentkate is a symlink")
	}
}

// .agentkate is a real directory but `worktrees` inside it is a symlink to the
// victim — the narrowest form of the same escape.
func TestRemoveRefusesSymlinkedWorktreesDir(t *testing.T) {
	repo := initRepo(t)
	victim := victimDir(t)
	planted := filepath.Join(victim, "t-y")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "irreplaceable.txt"),
		[]byte("a decade of work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agentkate"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(repo, ".agentkate", "worktrees")); err != nil {
		t.Fatal(err)
	}

	wt := Worktree{
		ThreadID: "t-y", RepoRoot: repo,
		Path:   filepath.Join(repo, ".agentkate", "worktrees", "t-y"),
		Branch: "agentkate/t-y", Isolated: true,
	}
	err := Remove(wt)
	if err == nil {
		t.Fatal("Remove accepted a record whose worktrees dir is a symlink")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error should name the symlink, got: %v", err)
	}
	assertStillThere(t, planted)
}

// A FILE (or anything that is not a directory) where the managed root belongs
// is refused too — chmod/rm through it is exactly what fail-closed forbids.
func TestVerifyProvenanceRefusesNonDirectoryManagedRoot(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agentkate"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agentkate", "worktrees"),
		[]byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := VerifyProvenance(Worktree{
		ThreadID: "t-z", RepoRoot: repo,
		Path:   filepath.Join(repo, ".agentkate", "worktrees", "t-z"),
		Branch: "agentkate/t-z", Isolated: true,
	})
	if err == nil {
		t.Fatal("a non-directory managed root was accepted")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error should say what is wrong, got: %v", err)
	}
}

// A record that names the managed root itself, or its parent, is not a
// worktree — removing the whole worktrees directory is never a per-thread op.
func TestVerifyProvenanceRefusesTheManagedRootItself(t *testing.T) {
	repo := initRepo(t)
	if _, err := Create(repo, "t-keep", ModeAuto); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, p := range []string{
		filepath.Join(repo, ".agentkate", "worktrees"),
		filepath.Join(repo, ".agentkate"),
		repo,
	} {
		if err := VerifyProvenance(Worktree{
			ThreadID: "t-root", RepoRoot: repo, Path: p,
			Branch: "agentkate/t-root", Isolated: true,
		}); err == nil {
			t.Fatalf("VerifyProvenance accepted %q as a worktree record", p)
		}
	}
}

// The legitimate case the pinning must not break: a project reached through a
// symlinked path. Users do relocate project trees; only the two components
// INSIDE the repo are forbidden from being links.
func TestRemoveWorksThroughASymlinkedRepoRoot(t *testing.T) {
	real := initRepo(t)
	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "project")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	// Created through the symlinked path, exactly as a user who picked the link
	// in the project chooser would get.
	wt, err := Create(link, "t-linkroot", ModeAuto)
	if err != nil {
		t.Fatalf("Create through a symlinked repo root: %v", err)
	}
	if !wt.Isolated {
		t.Fatal("expected an isolated worktree")
	}
	if err := VerifyProvenance(wt); err != nil {
		t.Fatalf("VerifyProvenance refused a legitimate symlinked-root worktree: %v", err)
	}
	if err := Remove(wt); err != nil {
		t.Fatalf("Remove refused a legitimate symlinked-root worktree: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory survived removal: %v", err)
	}
}

// A crafted git registration cannot satisfy the gate on its own: even when git
// itself lists the victim as a registered worktree of the repo, containment
// still refuses it. Registration is the SECOND half of the gate, never a
// substitute for the first.
func TestVerifyProvenanceRegistrationCannotDefeatContainment(t *testing.T) {
	repo := initRepo(t)
	victim := victimDir(t)
	// git will happily register a worktree anywhere the user points it.
	mustGit(t, repo, "worktree", "add", "-q", "-b", "agentkate/t-reg",
		filepath.Join(victim, "wt"))

	wt := Worktree{
		ThreadID: "t-reg", RepoRoot: repo,
		Path:   filepath.Join(victim, "wt"),
		Branch: "agentkate/t-reg", Isolated: true,
	}
	if err := VerifyProvenance(wt); err == nil {
		t.Fatal("a git-registered worktree outside the managed root was accepted")
	}
	if err := Remove(wt); err == nil {
		t.Fatal("Remove deleted a git-registered worktree outside the managed root")
	}
	assertStillThere(t, victim)
}

// A ".." element is refused outright: it is the one construction where the
// gate's lexical resolution and the kernel's walk can disagree, and no record
// this package writes ever contains one.
func TestVerifyProvenanceRefusesDotDotElements(t *testing.T) {
	repo := initRepo(t)
	wt, err := Create(repo, "t-dots", ModeAuto)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Lexically this Cleans right back inside the managed root, so nothing but
	// an explicit ban stops it.
	sneaky := wt
	// NOT filepath.Join: it Cleans the ".." away. A record on disk is raw text.
	sneaky.Path = filepath.Join(repo, ".agentkate", "worktrees", "x") +
		string(filepath.Separator) + ".." + string(filepath.Separator) + "t-dots"
	if err := VerifyProvenance(sneaky); err == nil {
		t.Fatal("a path with a \"..\" element was accepted")
	}
	sneaky = wt
	sneaky.RepoRoot = filepath.Join(repo, "sub") + string(filepath.Separator) + ".."
	if err := VerifyProvenance(sneaky); err == nil {
		t.Fatal("a repo root with a \"..\" element was accepted")
	}
	// The clean record is still fine.
	if err := VerifyProvenance(wt); err != nil {
		t.Fatalf("the legitimate record stopped passing: %v", err)
	}
}
