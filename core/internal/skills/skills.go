// Package skills manages a central catalog of Claude Code skills and links
// them into individual projects or worktrees on demand.
//
// A skill is a directory containing a SKILL.md file (the canonical Claude Code
// format) — a single standalone "<name>.md" file is also accepted. The
// catalog lives under XDG_DATA_HOME/agentkate/skills (default
// ~/.local/share/agentkate/skills); installing a skill into a target creates
// a symlink under each of skillDirs pointing back at the catalog, so edits to
// the central copy propagate without re-installing.
//
// One catalog, every engine: the same skill is linked into BOTH
// <target>/.claude/skills/ (Claude Code) and <target>/.agents/skills/ (the
// cross-tool convention kimi reads). Verified against kimi 0.30.0 — a skill
// dropped in .agents/skills/ shows up in an ACP session's command list as
// `skill:<name>` and the agent lists it when asked. That check mattered: the
// sibling .agents/agents/ subagent catalogue is v2-engine-only and invisible
// over ACP (plan 16 P3), so "kimi documents this directory" was not evidence
// enough on its own.
package skills

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skillDirs are the per-target directories a skill is linked into, relative to
// the target root. The first is the canonical one for listing.
var skillDirs = []string{
	filepath.Join(".claude", "skills"), // Claude Code
	filepath.Join(".agents", "skills"), // kimi (and other .agents-aware tools)
}

// Skill is one entry in the central catalog.
type Skill struct {
	Name        string `json:"name"`        // identifier; matches the directory or file stem
	Description string `json:"description"` // one-line summary from the YAML frontmatter
	Path        string `json:"path"`        // absolute source path in the catalog
	IsDir       bool   `json:"isDir"`       // true for SKILL.md directories, false for single-file skills
}

// Catalog is a directory of skills available to install into projects.
type Catalog struct {
	dir string
}

// New returns a Catalog backed by dir. The directory is created on demand
// (List on a missing dir returns an empty slice, not an error).
func New(dir string) *Catalog {
	return &Catalog{dir: dir}
}

// DefaultDir is the catalog location when the UI does not override it.
func DefaultDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "agentkate", "skills")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".local", "share", "agentkate", "skills")
}

// Dir is the catalog's filesystem location.
func (c *Catalog) Dir() string { return c.dir }

// EnsureDir creates the catalog directory if it does not yet exist.
func (c *Catalog) EnsureDir() error {
	return os.MkdirAll(c.dir, 0o755)
}

// List scans the catalog and returns every skill it can parse. Entries that
// look like skills but fail to parse are skipped silently — a malformed
// SKILL.md should not stop the dialog from listing valid neighbours.
func (c *Catalog) List() ([]Skill, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Skill{}, nil
		}
		return nil, err
	}
	out := make([]Skill, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(c.dir, name)
		if e.IsDir() {
			md := filepath.Join(full, "SKILL.md")
			if _, err := os.Stat(md); err != nil {
				continue // a directory without SKILL.md is not a skill
			}
			desc, _ := readDescription(md)
			out = append(out, Skill{Name: name, Description: desc, Path: full, IsDir: true})
			continue
		}
		if strings.HasSuffix(name, ".md") && !strings.EqualFold(name, "README.md") {
			desc, _ := readDescription(full)
			out = append(out, Skill{
				Name:        strings.TrimSuffix(name, ".md"),
				Description: desc,
				Path:        full,
				IsDir:       false,
			})
		}
	}
	return out, nil
}

// Get returns one skill by name, or an error if it is missing or unparseable.
func (c *Catalog) Get(name string) (Skill, error) {
	if err := validateName(name); err != nil {
		return Skill{}, err
	}
	dir := filepath.Join(c.dir, name)
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		md := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(md); err != nil {
			return Skill{}, fmt.Errorf("skill %q has no SKILL.md", name)
		}
		desc, _ := readDescription(md)
		return Skill{Name: name, Description: desc, Path: dir, IsDir: true}, nil
	}
	file := filepath.Join(c.dir, name+".md")
	if st, err := os.Stat(file); err == nil && !st.IsDir() {
		desc, _ := readDescription(file)
		return Skill{Name: name, Description: desc, Path: file, IsDir: false}, nil
	}
	return Skill{}, fmt.Errorf("skill %q not found in catalog", name)
}

// maxSkillContentBytes caps how much of a skill's markdown is read into memory
// for the detail view, so a pathological file cannot exhaust the daemon.
const maxSkillContentBytes = 256 * 1024

// ReadContent returns the full markdown of a skill by name — the SKILL.md for
// a directory skill, or the standalone file for a single-file skill. The
// content is capped at maxSkillContentBytes; longer files are truncated with a
// trailing notice rather than read in full.
func (c *Catalog) ReadContent(name string) (string, error) {
	skill, err := c.Get(name)
	if err != nil {
		return "", err
	}
	path := skill.Path
	if skill.IsDir {
		path = filepath.Join(skill.Path, "SKILL.md")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// Read one byte past the cap so we can tell "exactly maxSkillContentBytes"
	// (not truncated) from "larger than the cap" (truncated).
	buf := make([]byte, maxSkillContentBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if n > maxSkillContentBytes {
		return string(buf[:maxSkillContentBytes]) + "\n\n… (truncated)", nil
	}
	return string(buf[:n]), nil
}

// Create scaffolds a new directory skill in the catalog: a directory named
// <name> containing a SKILL.md with minimal YAML frontmatter. It refuses
// invalid names and names that already exist (as a directory or single file).
func (c *Catalog) Create(name, description string) (Skill, error) {
	if err := validateName(name); err != nil {
		return Skill{}, err
	}
	if err := c.EnsureDir(); err != nil {
		return Skill{}, err
	}
	dir := filepath.Join(c.dir, name)
	if _, err := os.Stat(dir); err == nil {
		return Skill{}, fmt.Errorf("skill %q already exists", name)
	}
	if _, err := os.Stat(filepath.Join(c.dir, name+".md")); err == nil {
		return Skill{}, fmt.Errorf("skill %q already exists", name)
	}
	// Collapse to a single line; the frontmatter reader is line-based and does
	// no quote-unescaping, so the value is written bare and kept readable.
	desc := sanitizeDescription(description)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Skill{}, err
	}
	md := filepath.Join(dir, "SKILL.md")
	body := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n# %s\n\nDescribe what this skill does and when Claude should use it.\n",
		name, desc, name)
	if err := os.WriteFile(md, []byte(body), 0o644); err != nil {
		// Best effort cleanup so a half-created skill does not linger.
		_ = os.RemoveAll(dir)
		return Skill{}, err
	}
	return Skill{Name: name, Description: desc, Path: dir, IsDir: true}, nil
}

// sanitizeDescription collapses whitespace and strips any leading quote so the
// generated value round-trips through the simple line-based frontmatter reader
// (which strips matching outer quotes but does not unescape). A colon or hash
// inside the value is fine — the reader keeps everything after the first
// "description:" verbatim.
func sanitizeDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	// A leading or trailing quote pair would be stripped on read; avoid the
	// surprise by trimming stray wrapping quotes up front.
	for len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// Install symlinks skillName from the catalog into every directory of
// skillDirs under target, so both engines see the same skill. An existing
// entry of the same name is replaced; missing directories are created. The
// returned path is the canonical (first) link.
func (c *Catalog) Install(skillName, target string) (string, error) {
	if err := validateTarget(target); err != nil {
		return "", err
	}
	skill, err := c.Get(skillName)
	if err != nil {
		return "", err
	}
	linkName := skill.Name
	if !skill.IsDir {
		linkName = skill.Name + ".md"
	}
	first := ""
	for _, rel := range skillDirs {
		dest := filepath.Join(target, rel)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return "", err
		}
		linkPath := filepath.Join(dest, linkName)
		// Replace any previous install so re-installing always reflects the
		// catalog's current shape (a single-file skill replacing a directory or
		// vice versa).
		if err := removeIfPresent(linkPath); err != nil {
			return "", err
		}
		if err := os.Symlink(skill.Path, linkPath); err != nil {
			return "", err
		}
		if first == "" {
			first = linkPath
		}
	}
	return first, nil
}

// Uninstall removes a previously installed skill from every skillDirs
// directory under target. Only entries that resolve back into the catalog are
// removed — a hand-edited skill of the same name in the target is left alone.
func (c *Catalog) Uninstall(skillName, target string) error {
	if err := validateName(skillName); err != nil {
		return err
	}
	if target == "" {
		return errors.New("target is required")
	}
	for _, rel := range skillDirs {
		dir := filepath.Join(target, rel)
		for _, n := range []string{skillName, skillName + ".md"} {
			p := filepath.Join(dir, n)
			st, err := os.Lstat(p)
			if err != nil {
				continue
			}
			if st.Mode()&os.ModeSymlink == 0 {
				// Not a symlink — refuse to delete a real, possibly user-owned dir.
				return fmt.Errorf("%s is not a managed skill (not a symlink)", p)
			}
			resolved, _ := os.Readlink(p)
			if !linkPointsInto(resolved, c.dir) {
				return fmt.Errorf("%s does not point into the catalog; refusing to remove", p)
			}
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// Installed describes one entry under target/.claude/skills.
type Installed struct {
	Name      string `json:"name"`      // identifier (no .md suffix for either form)
	Path      string `json:"path"`      // the link/file inside target/.claude/skills
	LinkedTo  string `json:"linkedTo"`  // resolved symlink target, empty when not a symlink
	InCatalog bool   `json:"inCatalog"` // true when the link resolves into this catalog
}

// ListInstalled returns every entry under the CANONICAL skills directory
// (skillDirs[0]), flagging which ones this catalog owns. It lists one
// directory on purpose: Install writes the same set of links to every
// skillDirs entry, so listing them all would report each skill once per
// engine. A target with no skills directory is not an error — it just has
// nothing installed yet.
func (c *Catalog) ListInstalled(target string) ([]Installed, error) {
	if target == "" {
		return nil, errors.New("target is required")
	}
	dir := filepath.Join(target, skillDirs[0])
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Installed{}, nil
		}
		return nil, err
	}
	out := make([]Installed, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(dir, name)
		st, err := os.Lstat(p)
		if err != nil {
			continue
		}
		entry := Installed{
			Name: strings.TrimSuffix(name, ".md"),
			Path: p,
		}
		if st.Mode()&os.ModeSymlink != 0 {
			if dest, err := os.Readlink(p); err == nil {
				entry.LinkedTo = dest
				entry.InCatalog = linkPointsInto(dest, c.dir)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// readDescription pulls the `description:` value out of a SKILL.md or
// standalone .md file's YAML frontmatter. Returns "" when no frontmatter is
// present or the field is missing.
func readDescription(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	if !sc.Scan() {
		return "", nil
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		return "", nil
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			return "", nil
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "description" {
			return cleanYAMLValue(val), nil
		}
	}
	return "", sc.Err()
}

func cleanYAMLValue(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "\r")
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return s
}

// validateTarget rejects install targets that are not an existing directory.
// target arrives over the IPC socket (i.e. potentially from an agent) and
// Install does MkdirAll under it, so without this an arbitrary caller-supplied
// string would seed .claude/skills trees anywhere on the filesystem. Requiring
// the project directory to already exist keeps every write inside a tree the
// user already has. Fails closed: an unstattable target is refused.
func validateTarget(target string) error {
	if strings.TrimSpace(target) == "" {
		return errors.New("target is required")
	}
	st, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("target %q is not usable: %w", target, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("target %q is not a directory", target)
	}
	return nil
}

// validateName rejects names that would let a caller escape the catalog dir.
func validateName(name string) error {
	if name == "" {
		return errors.New("skill name is required")
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return fmt.Errorf("invalid skill name %q", name)
	}
	return nil
}

// linkPointsInto returns true when linkTarget resolves to a path inside dir.
func linkPointsInto(linkTarget, dir string) bool {
	if linkTarget == "" || dir == "" {
		return false
	}
	abs, err := filepath.Abs(linkTarget)
	if err != nil {
		return false
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dirAbs, abs)
	if err != nil {
		return false
	}
	return !strings.HasPrefix(rel, "..")
}

func removeIfPresent(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	// A symlink (managed) goes via Remove; a real directory under our own
	// install path can also be removed since the user explicitly asked to
	// re-install the skill.
	if st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(path)
	}
	return os.Remove(path)
}
