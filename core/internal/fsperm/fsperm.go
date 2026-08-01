// Package fsperm centralises the on-disk permission discipline for every store
// Agent Kate owns under its data directory.
//
// The data class is the same one both harness CLIs already protect: their own
// homes (~/.claude/projects, ~/.kimi-code) are 0700, and Agent Kate's cowork
// stores are 0700/0600 (see internal/cowork/audit.go, grants.go). What Agent
// Kate keeps beside them is no less sensitive — thread records carry the
// persisted persona, the per-thread environment overlay and a title cut from
// the opening prompt; compaction summaries carry condensed conversation;
// attachment sidecars carry the names and paths of every file the human
// attached; the kimi event logs carry whole transcripts. Written 0644 inside a
// 0755 directory, all of it is readable by every local user.
//
// Two halves, and BOTH are load-bearing:
//
//   - New writes: MkdirAll/WriteFile here create 0700 dirs and 0600 files.
//   - Migration: Harden* tightens what an earlier build already created
//     world-readable. Mode constants on new writes alone would leave every
//     existing installation exposed forever — the files that matter most are
//     the ones that already exist.
//
// Fail-closed: a path whose permissions cannot be read or changed is an error,
// never a silent pass. A store that cannot confirm it is private must say so
// rather than assume it is.
package fsperm

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

const (
	// DirMode is owner-only for directories: nobody else may even list them.
	DirMode fs.FileMode = 0o700
	// FileMode is owner-only for files.
	FileMode fs.FileMode = 0o600
)

// looseBits are the group/other permission bits no Agent Kate-owned path may
// carry. Anything matching here is tightened by the Harden* helpers.
const looseBits fs.FileMode = 0o077

// MkdirAll creates dir (and any missing parents) with DirMode, and tightens dir
// itself if it already existed with looser permissions — MkdirAll applies its
// mode only to directories it CREATES, so without the second step every
// pre-existing 0755 store directory would stay 0755 forever.
func MkdirAll(dir string) error {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return err
	}
	_, err := HardenDir(dir)
	return err
}

// WriteFile writes b to path with FileMode.
//
// The explicit tighten afterwards is not redundant: os.WriteFile applies its
// mode only when it CREATES the file. A leftover world-readable file — a `.tmp`
// staging file abandoned by an older build's crash, which the store then
// renames over the real thing — would otherwise keep its old 0644 mode and
// carry it onto the published store file.
func WriteFile(path string, b []byte) error {
	if err := os.WriteFile(path, b, FileMode); err != nil {
		return err
	}
	_, err := tightenIfLoose(path, FileMode)
	return err
}

// HardenFile tightens a single existing file whose mode is looser than
// FileMode. A file that does not exist is not an error (nothing to migrate).
// Symlinks are refused, never followed — see tightenIfLoose.
func HardenFile(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return tightenIfLoose(path, FileMode)
}

// HardenDir tightens a single existing directory whose mode is looser than
// DirMode. A directory that does not exist is not an error.
//
// Unlike HardenFile this DOES follow a symlink, and deliberately: relocating an
// XDG data directory by symlinking it is a legitimate, common layout, and
// refusing it would fail every store open on such a machine. The asymmetry is
// safe because planting a symlink where a store directory belongs requires
// write access to its parent, which for these paths is the owner's own home —
// whereas a symlink standing in for a store FILE has no legitimate use and is
// the classic chmod-redirect primitive, so that case stays refused.
//
// A path that exists but is not a directory is an error: something else is
// sitting where a store directory belongs, and chmod'ing it blind is exactly
// what fail-closed forbids.
func HardenDir(dir string) (bool, error) {
	fi, err := os.Stat(dir) // follows, per the doc comment
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !fi.IsDir() {
		return false, fmt.Errorf("fsperm: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&looseBits == 0 {
		return false, nil
	}
	if err := os.Chmod(dir, DirMode); err != nil {
		return false, fmt.Errorf("fsperm: cannot tighten %s to %s: %w", dir, DirMode, err)
	}
	return true, nil
}

// HardenTree tightens dir and everything below it: directories to DirMode,
// regular files to FileMode. A missing dir is not an error.
//
// Symlinks are never followed and never chmod'ed: the mode that matters lives
// on the target, which is outside this store's ownership, and chmod through a
// link is exactly the primitive a planted symlink wants. They are logged and
// skipped rather than silently ignored, so an unexpected one is visible.
//
// Returns the number of paths tightened. Anything that cannot be stat'ed or
// chmod'ed aborts the walk with an error — see the package comment.
func HardenTree(dir string) (int, error) {
	fi, err := os.Stat(dir) // follows a symlinked store root; see HardenDir
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !fi.IsDir() {
		return 0, fmt.Errorf("fsperm: %s is not a directory", dir)
	}
	// WalkDir lstats its root, so a symlinked store root would otherwise be
	// reported as a single non-directory entry and the whole tree skipped.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0, err
	}
	tightened := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			n, err := tightenIfLoose(path, DirMode)
			if err != nil {
				return err
			}
			if n {
				tightened++
			}
		case d.Type().IsRegular():
			n, err := tightenIfLoose(path, FileMode)
			if err != nil {
				return err
			}
			if n {
				tightened++
			}
		default:
			slog.Warn("fsperm: skipping non-regular path in an Agent Kate store",
				"path", path, "type", d.Type().String())
		}
		return nil
	})
	return tightened, err
}

// LogMigration reports a completed migration once per store, and only when it
// actually changed something — a quiet startup on an already-private store is
// the normal case and should stay quiet.
func LogMigration(store string, tightened int) {
	if tightened > 0 {
		slog.Info("tightened permissions on an Agent Kate store created world-readable by an earlier build",
			"store", store, "paths", tightened, "dirs", DirMode.String(), "files", FileMode.String())
	}
}

// tightenIfLoose chmods path to want when its current mode carries any
// group/other bit. Returns true if it changed anything.
//
// FAIL CLOSED: an unreadable mode (Lstat error) or a failed chmod is returned
// as an error rather than treated as "probably fine". A symlink is refused
// outright — os.Chmod would follow it and change permissions on a file this
// store does not own.
func tightenIfLoose(path string, want fs.FileMode) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return false, fmt.Errorf("fsperm: %s is a symlink; refusing to chmod through it", path)
	}
	if fi.Mode().Perm()&looseBits == 0 {
		return false, nil
	}
	if err := os.Chmod(path, want); err != nil {
		return false, fmt.Errorf("fsperm: cannot tighten %s to %s: %w", path, want, err)
	}
	return true, nil
}
