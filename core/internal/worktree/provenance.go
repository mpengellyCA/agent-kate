// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package worktree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// managedDir is the ONLY directory this package ever creates worktrees in,
// relative to a repo root. Create / CreateFrom / Promote all join exactly
// these two components — provenance checking below relies on that.
var managedDir = []string{".agentkate", "worktrees"}

// managedBranchPrefix is the only branch namespace this package ever creates.
const managedBranchPrefix = "agentkate/"

// VerifyProvenance is the fail-closed gate that decides whether a Worktree
// RECORD may be acted on destructively. Records come out of threads.json — a
// file that lives in the user's data dir and is writable by anything running
// as the user, including a prompt-injected agent. Nothing downstream may
// assume the path in a record is one we created.
//
// A record passes only when BOTH hold:
//
//	(a) wt.Path, resolved through symlinks, lies strictly inside the MANAGED
//	    ROOT — and the managed root is pinned by managedRoot() to exactly
//	    <resolved RepoRoot>/.agentkate/worktrees, with neither `.agentkate` nor
//	    `worktrees` allowed to be a symlink; and
//	(b) git itself lists wt.Path as a registered worktree of RepoRoot.
//
// Why (a) pins the ROOT and not just the target: .agentkate/ lives INSIDE the
// repository the agent has write access to. Resolving the containment root
// through symlinks and then only testing "is the target under it" is not a
// containment test at all — an agent replaces .agentkate/worktrees with a
// symlink to /home/user/photos, the root resolves to /home/user/photos, and
// every path under it "passes". So the root is derived from the RESOLVED repo
// root by string join, each component is Lstat'ed and refused if it is a
// symlink (or exists but is not a directory), and a final resolution of the
// join must be a no-op (string equality, never a prefix test).
//
// (b) is skipped in exactly one case: the directory no longer exists. There is
// then nothing on disk to destroy, and demanding registration would strand
// orphaned records whose bookkeeping git has already pruned — the removal path
// for those does nothing but `worktree prune` + `branch -D`, both repo-scoped.
// Containment (a) is still required even then, so a record pointing at
// /home/user/photos is refused whether or not that directory exists.
//
// FAIL CLOSED: every failure to EVALUATE a check — empty repo root, an
// unreadable or missing repo root, a path component that is not a directory, a
// git invocation that errors — is a refusal, never a pass. The returned error
// is safe to show a human: it names the path and the reason.
func VerifyProvenance(wt Worktree) error {
	if strings.TrimSpace(wt.Path) == "" {
		return errors.New("the record has no worktree path")
	}
	if strings.TrimSpace(wt.RepoRoot) == "" {
		return errors.New("the record has no repository root to validate the path against")
	}
	// A ".." element makes the gate and the kernel disagree. resolveExisting
	// starts with filepath.Abs, which Cleans ".." LEXICALLY, while the Lstat and
	// RemoveAll below walk the path the kernel's way — and the two differ exactly
	// when a ".." backs out of a symlinked component ("<root>/link/../x" is
	// "<root>/x" lexically but "/elsewhere/x" to the kernel). No record this
	// package writes contains one: Create / CreateFrom / Promote join clean
	// components. Refusing the whole class is cheaper and far easier to reason
	// about than making the two agree.
	for _, p := range []string{wt.Path, wt.RepoRoot} {
		if hasDotDot(p) {
			return fmt.Errorf(
				"%q contains a %q element — refusing a record Agent Kate would never "+
					"have written", p, "..")
		}
	}

	root, err := managedRoot(wt.RepoRoot)
	if err != nil {
		return err
	}

	target, err := resolveExisting(wt.Path)
	if err != nil {
		return fmt.Errorf("cannot resolve the worktree path %q: %w", wt.Path, err)
	}
	if !insideDir(root, target) {
		return fmt.Errorf(
			"%q is not inside this project's %s directory (resolved to %q) — "+
				"refusing to touch a path Agent Kate did not create",
			wt.Path, filepath.Join(managedDir...), target)
	}

	// (b) — only when there is something on disk to destroy.
	if _, statErr := os.Lstat(wt.Path); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil // orphaned: contained, and nothing left to delete
		}
		return fmt.Errorf("cannot stat the worktree path %q: %w", wt.Path, statErr)
	}
	registered, err := registeredWorktrees(wt.RepoRoot)
	if err != nil {
		// Cannot evaluate registration -> refuse. (Fail closed.)
		return fmt.Errorf("cannot list the repository's worktrees: %w", err)
	}
	if !registered[target] {
		return fmt.Errorf(
			"%q is not a git-registered worktree of %s — refusing to delete it",
			wt.Path, wt.RepoRoot)
	}
	return nil
}

// hasDotDot reports whether any element of path is "..". Both separators are
// checked so a Windows-style record cannot smuggle one past a POSIX build.
func hasDotDot(path string) bool {
	for _, e := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == filepath.Separator
	}) {
		if e == ".." {
			return true
		}
	}
	return false
}

// managedRoot returns the one directory a record's path may live under:
// <repoRoot resolved through symlinks>/.agentkate/worktrees. It is the anchor
// of the whole gate, so it is built rather than resolved.
//
// The construction, and why each step is load-bearing:
//
//  1. The REPO ROOT is resolved through symlinks. That is legitimate and
//     common (a project reached via a symlinked home, /home -> /usr/home on
//     some systems) and it is safe: the repo root is the value the human chose
//     when creating the project, not a field an agent can repoint without the
//     rest of the record disagreeing with itself.
//
//  2. `.agentkate` and `worktrees` are then JOINED, never resolved. They live
//     inside the repository the agent can write to, so resolving them would let
//     the agent choose the containment root — the exact bug this replaces
//     (audit F4, "MOVED not closed"): symlink .agentkate/worktrees at
//     /home/user/photos and every path under /home/user/photos passes
//     containment.
//
//  3. Each joined component is Lstat'ed. A SYMLINK is refused outright — this
//     package only ever creates those two components as real directories
//     (Create/CreateFrom/Promote MkdirAll them), so a symlink there is either
//     tampering or a layout we did not make and must not delete through.
//     Something that exists but is not a directory is refused for the same
//     reason. A component that does NOT exist is fine: it cannot be a symlink,
//     and an orphaned record whose project directory is long gone must still be
//     cleanable (see VerifyProvenance's orphan case).
//
//  4. Belt and braces: resolving the assembled path must be a no-op. If it is
//     not, some component resolved to somewhere else despite step 3 (a race, a
//     mount, a filesystem we do not understand) — refuse rather than guess.
//     This is string EQUALITY, not a prefix test.
//
// FAIL CLOSED: every error path returns an error, never a permissive root.
func managedRoot(repoRoot string) (string, error) {
	repo, err := resolveExisting(repoRoot)
	if err != nil {
		return "", fmt.Errorf("cannot resolve the repository root %q: %w", repoRoot, err)
	}
	root := repo
	for _, comp := range managedDir {
		root = filepath.Join(root, comp)
		fi, err := os.Lstat(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // missing: cannot be a symlink, and nothing below it exists
			}
			return "", fmt.Errorf("cannot inspect %q: %w", root, err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf(
				"%q is a symlink — Agent Kate only ever creates it as a real "+
					"directory, so refusing to treat anything under it as a worktree "+
					"it created", root)
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%q exists but is not a directory — refusing", root)
		}
	}
	resolved, err := resolveExisting(root)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", root, err)
	}
	if resolved != root {
		return "", fmt.Errorf(
			"%q resolves to %q — refusing to treat a relocated directory as this "+
				"project's worktree directory", root, resolved)
	}
	return root, nil
}

// VerifyRunPath is the lighter gate for operations that act INSIDE a recorded
// worktree rather than deleting it (DiscardChanges' `reset --hard` +
// `clean -fd` destroy uncommitted work just as effectively as rm -rf, so the
// path they run in has to be validated too).
//
// Isolated records go through the full VerifyProvenance gate. A
// direct-workspace record has no worktree of its own: every constructor in the
// codebase sets Path == RepoRoot for those, so that equality (resolved through
// symlinks) is what we require. It bounds the operation to the project root
// the user picked and the UI displays — it cannot be silently repointed at an
// unrelated tree by editing one field of the record.
func VerifyRunPath(wt Worktree) error {
	if wt.Isolated {
		return VerifyProvenance(wt)
	}
	if strings.TrimSpace(wt.Path) == "" || strings.TrimSpace(wt.RepoRoot) == "" {
		return errors.New("the record has no workspace path")
	}
	pathAbs, err := filepath.EvalSymlinks(wt.Path)
	if err != nil {
		return fmt.Errorf("cannot resolve the workspace path %q: %w", wt.Path, err)
	}
	repoAbs, err := filepath.EvalSymlinks(wt.RepoRoot)
	if err != nil {
		return fmt.Errorf("cannot resolve the repository root %q: %w", wt.RepoRoot, err)
	}
	if pathAbs != repoAbs {
		return fmt.Errorf(
			"this agent runs directly in the workspace, but its path %q is not its "+
				"project root %q — refusing to act on a record that disagrees with itself",
			wt.Path, wt.RepoRoot)
	}
	return nil
}

// isManagedBranch reports whether name is in the branch namespace this package
// creates. Used to keep a tampered record from turning a worktree removal into
// `git branch -D main`.
func isManagedBranch(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && strings.HasPrefix(name, managedBranchPrefix) &&
		len(name) > len(managedBranchPrefix)
}

// registeredWorktrees returns the set of worktree paths git itself knows about
// for repoRoot, each resolved through symlinks so it can be compared with a
// resolved candidate path. An error means "could not determine" — callers must
// treat that as a refusal, not an empty set.
func registeredWorktrees(repoRoot string) (map[string]bool, error) {
	out, err := git(repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok || strings.TrimSpace(p) == "" {
			continue
		}
		resolved, err := resolveExisting(p)
		if err != nil {
			continue // a listing we cannot resolve simply does not authorise anything
		}
		set[resolved] = true
	}
	return set, nil
}

// resolveExisting resolves path through symlinks, tolerating a missing leaf:
// it evaluates the deepest ancestor that DOES exist and re-appends the missing
// components. That is sound because a component which does not exist cannot be
// a symlink, so nothing is left unresolved. Any error other than "does not
// exist" (permission denied, not-a-directory) is returned, so callers fail
// closed instead of falling back to a lexical comparison.
func resolveExisting(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty path")
	}
	cur, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Walked to the filesystem root without finding anything that exists.
			return "", fmt.Errorf("no existing ancestor of %q", path)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// insideDir reports whether target is STRICTLY inside root. Both must already
// be resolved absolute paths. root itself is not "inside" root — removing the
// whole worktrees directory is never a per-thread operation.
func insideDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
