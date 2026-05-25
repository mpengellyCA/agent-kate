package vsix

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// maxMainScanBytes caps how much of the activation bundle we read when
// searching for a server module path. Real extensions ship bundles in the low
// MBs; this bound keeps a tampered/huge file from costing us memory.
const maxMainScanBytes = 8 << 20 // 8 MiB

// serverPathPattern matches the kind of string literal an extension activation
// bundle uses when telling vscode-languageclient where the server module
// lives. The pattern is intentionally tight to keep false positives down:
// it requires the path to end in a *.js file whose basename contains "server"
// (case-insensitive). RE2 has no backtracking, so this is safe on adversarial
// input.
var serverPathPattern = regexp.MustCompile(
	`["'` + "`" + `]([A-Za-z0-9_./-]*[Ss][Ee][Rr][Vv][Ee][Rr][A-Za-z0-9_.-]*\.js)["'` + "`" + `]`)

// findServerByMainScan inspects the extension's activation script (referenced
// by package.json's "main" field) for a string literal pointing at a bundled
// server module. It returns the absolute path to the discovered server, or ""
// if nothing trustworthy is found.
//
// All discovered paths are clamped to extensionRoot — anything that would
// escape the extension directory (absolute paths, symlinks pointing out, "..")
// is rejected silently so a malicious VSIX cannot trick us into launching an
// arbitrary host binary.
func findServerByMainScan(extensionRootDir, mainRel string) string {
	if mainRel == "" {
		return ""
	}
	mainAbs, ok := safeJoin(extensionRootDir, mainRel)
	if !ok {
		return ""
	}
	data, err := readCapped(mainAbs, maxMainScanBytes)
	if err != nil {
		return ""
	}

	mainDir := filepath.Dir(mainAbs)
	matches := serverPathPattern.FindAllSubmatch(data, -1)

	// Prefer the candidate closest to extensionRoot, so that bundled vendor
	// references (e.g. a nested webpack chunk path) lose to a top-level
	// "dist/server.js".
	best, bestDepth := "", 1<<30
	seen := map[string]bool{}
	for _, m := range matches {
		rel := string(m[1])
		if seen[rel] {
			continue
		}
		seen[rel] = true

		// Try the literal resolved against the bundle's own directory (the
		// usual pattern — context.asAbsolutePath joins from the extension
		// root, but most string literals are already relative to the bundle).
		for _, base := range []string{mainDir, extensionRootDir} {
			abs, ok := safeJoin(base, rel)
			if !ok {
				continue
			}
			if !insideRoot(extensionRootDir, abs) {
				continue
			}
			info, err := os.Stat(abs)
			if err != nil || info.IsDir() {
				continue
			}
			depth := strings.Count(abs, string(os.PathSeparator))
			if depth < bestDepth {
				best, bestDepth = abs, depth
			}
			break
		}
	}
	return best
}

// safeJoin joins base with rel, rejecting results that escape base. Both
// absolute rel paths and ones that climb out via ".." are refused.
func safeJoin(base, rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return filepath.Join(base, cleaned), true
}

// insideRoot returns true when path resolves to a file inside root after
// following symlinks. Anything pointing outside root is rejected so a
// symlinked entry inside the extension cannot escape its sandbox.
func insideRoot(root, path string) bool {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// readCapped reads up to max bytes of path, returning an error for anything
// that cannot be opened. Files larger than max are truncated to max — the
// regex scan still gets useful input from the prefix.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}
