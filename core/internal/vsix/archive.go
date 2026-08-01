package vsix

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Extraction limits. A .vsix is downloaded from a public registry, so its
// contents are attacker-influenced: an entry declaring a small compressed size
// can decompress to an unbounded stream (a "zip bomb"), and a malformed
// archive can declare thousands of entries. These caps are generous for real
// extensions (the largest language-server bundles are tens of MB) and turn
// resource exhaustion of the daemon into a plain error.
// Vars rather than consts only so the tests can shrink them; nothing in the
// daemon writes to them.
var (
	maxEntryBytes   int64 = 256 << 20 // 256 MiB from any single entry
	maxArchiveBytes int64 = 1 << 30   // 1 GiB decompressed in total
	maxEntries            = 20000     // entries in one archive
)

// unzip extracts a .vsix (a zip archive) into dest. It rejects any entry whose
// path would escape dest — lexically ("zip-slip": ../ in the entry name) AND
// after resolving symlinks, so an entry cannot be written through a link that
// points outside. Sizes are capped (see above). The executable bit is
// preserved so bundled server binaries stay runnable; no other mode bit is
// honoured, in particular an entry claiming to be a symlink is written as an
// ordinary file rather than becoming one.
func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open vsix: %w", err)
	}
	defer r.Close()

	root := filepath.Clean(dest)
	// The resolved root is what containment is measured against. dest is
	// created by the caller; if it cannot be resolved we refuse to extract
	// rather than fall back to a lexical-only check (fail closed).
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("vsix destination %q is not usable: %w", dest, err)
	}

	if len(r.File) > maxEntries {
		return fmt.Errorf("vsix has %d entries, refusing more than %d",
			len(r.File), maxEntries)
	}
	var total int64
	for _, f := range r.File {
		target := filepath.Join(root, f.Name)
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("vsix entry escapes archive root: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := checkContained(realRoot, target); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// Re-check AFTER the parent directories exist: only now can the path be
		// resolved through symlinks, which is what catches an entry aimed at a
		// link planted by an earlier entry of the same archive.
		if err := checkContained(realRoot, filepath.Dir(target)); err != nil {
			return err
		}
		n, err := extractFile(f, target, maxArchiveBytes-total)
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}

// checkContained verifies that dir, resolved through symlinks, is realRoot or
// lies inside it. Any resolution failure is an error, never a pass.
func checkContained(realRoot, dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("cannot resolve vsix extraction path %q: %w", dir, err)
	}
	rel, err := filepath.Rel(realRoot, resolved)
	if err != nil {
		return fmt.Errorf("cannot compare vsix extraction path %q with %q: %w",
			resolved, realRoot, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("vsix entry resolves outside the archive root: %s", dir)
	}
	return nil
}

// extractFile writes one zip entry to target and returns the number of bytes
// written. budget is how much of the whole-archive allowance is left; the
// per-entry cap applies on top of it. O_NOFOLLOW means an existing symlink at
// target is an error rather than a write through it (ELOOP), and O_EXCL is not
// used because a well-formed archive may legitimately list a name twice.
func extractFile(f *zip.File, target string, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, fmt.Errorf("vsix exceeds the %d byte decompressed limit", maxArchiveBytes)
	}
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	// Honour only the executable bit; everything else (setuid, sticky, symlink,
	// device) is dropped so an archive cannot ask for a privileged file mode.
	mode := os.FileMode(0o644)
	if f.Mode().Perm()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(target,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return 0, fmt.Errorf("vsix entry %q would be written through a symlink", f.Name)
		}
		return 0, err
	}
	defer out.Close()

	limit := budget
	if limit > maxEntryBytes {
		limit = maxEntryBytes
	}
	// Read one byte past the limit so hitting it exactly is distinguishable
	// from overrunning it.
	n, err := io.Copy(out, io.LimitReader(rc, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, fmt.Errorf("vsix entry %q exceeds the decompressed size limit", f.Name)
	}
	return n, nil
}
