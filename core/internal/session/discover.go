package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Discovered is one Claude Code session found on disk — a conversation that can
// be attached to Agent Kate and resumed, whether or not Agent Kate started it.
type Discovered struct {
	SessionID string    `json:"sessionId"`
	Project   string    `json:"project"`  // the directory the session ran in
	Title     string    `json:"title"`    // first prompt or summary
	Modified  time.Time `json:"modified"` // transcript file mtime
	Attached  bool      `json:"attached"` // already tracked as an Agent Kate thread
}

// claudeHome is the Claude Code config directory (~/.claude unless overridden
// by CLAUDE_CONFIG_DIR).
func claudeHome() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// Discover lists every Claude Code session transcript on disk, newest first.
func Discover() ([]Discovered, error) {
	home := claudeHome()
	if home == "" {
		return nil, nil
	}
	return discoverIn(filepath.Join(home, "projects"))
}

// discoverIn scans a Claude Code projects directory for session transcripts.
func discoverIn(root string) ([]Discovered, error) {
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Discovered
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		sub := filepath.Join(root, dir.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if len(id) != 36 { // not a session UUID — skip stray files
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			project, title := readTranscriptMeta(filepath.Join(sub, name))
			if project == "" {
				project = decodeProjectDir(dir.Name())
			}
			if title == "" {
				title = "(untitled session)"
			}
			out = append(out, Discovered{
				SessionID: id,
				Project:   project,
				Title:     title,
				Modified:  info.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// readTranscriptMeta pulls the working directory and a title out of a session
// transcript. Only the first lines are parsed — large transcripts are not read
// in full.
func readTranscriptMeta(path string) (project, title string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for parsed := 0; parsed < 400 && sc.Scan(); parsed++ {
		var probe struct {
			Type    string          `json:"type"`
			Cwd     string          `json:"cwd"`
			Summary string          `json:"summary"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &probe) != nil {
			continue
		}
		if project == "" && probe.Cwd != "" {
			project = probe.Cwd
		}
		if title == "" && probe.Type == "summary" {
			title = probe.Summary
		}
		if title == "" && probe.Type == "user" {
			title = firstUserText(probe.Message)
		}
		if project != "" && title != "" {
			break
		}
	}
	return project, tidyTitle(title)
}

// firstUserText extracts plain text from a transcript user message, whose
// content may be a bare string or an array of content blocks.
func firstUserText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return ""
}

// decodeProjectDir best-effort decodes a Claude Code project directory name
// back into a path. The encoding (slashes to dashes) is lossy, so this is only
// a fallback for when the transcript itself carries no cwd.
func decodeProjectDir(name string) string {
	return strings.ReplaceAll(name, "-", "/")
}

// tidyTitle collapses whitespace and truncates a title for display.
func tidyTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 100
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// SubagentDir returns the directory Claude Code writes a session's per-subagent
// transcripts into (<project>/<session>/subagents), or "" when the session has
// none. Verified against claude 2.1.220: each delegation lands in that
// directory as agent-<id>.jsonl, in the same shape as the main transcript.
func SubagentDir(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	matches, _ := filepath.Glob(
		filepath.Join(claudeHome(), "projects", "*", sessionID, "subagents"))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			return m
		}
	}
	return ""
}
