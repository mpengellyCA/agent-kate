package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadTranscript returns the raw JSON lines from a session's Claude Code
// transcript, in order, so the UI can replay the conversation when reopening a
// dormant thread. Returns nil with no error if there is no transcript yet.
func ReadTranscript(sessionID string) ([]json.RawMessage, error) {
	path := findTranscript(sessionID)
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []json.RawMessage
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		out = append(out, append(json.RawMessage(nil), line...))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PreviewMessage is one role-labelled turn extracted from a transcript for the
// session browser's preview pane.
type PreviewMessage struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"` // plain text of the turn
}

// PreviewTranscript returns up to limit of the most recent user/assistant turns
// from a session's transcript, for a read-only preview before attaching. It
// streams the file line by line (never loading the whole transcript into
// memory) and reports whether earlier messages were dropped. limit <= 0
// defaults to 20.
func PreviewTranscript(sessionID string, limit int) ([]PreviewMessage, bool, error) {
	if limit <= 0 {
		limit = 20
	}
	path := findTranscript(sessionID)
	if path == "" {
		return nil, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var all []PreviewMessage
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &probe) != nil {
			continue
		}
		if probe.Type != "user" && probe.Type != "assistant" {
			continue
		}
		text := messageText(probe.Type, probe.Message)
		if strings.TrimSpace(text) == "" {
			continue
		}
		all = append(all, PreviewMessage{Role: probe.Type, Text: text})
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	truncated := false
	if len(all) > limit {
		all = all[len(all)-limit:]
		truncated = true
	}
	return all, truncated, nil
}

// previewMaxRunes caps each preview turn so a giant message cannot bloat the
// reply, while still leaving room for a few paragraphs (unlike the 100-rune
// title clamp used for the session list).
const previewMaxRunes = 2000

// messageText extracts displayable text from a transcript message for the
// preview pane. Content may be a bare string or an array of content blocks;
// text blocks are joined with blank lines and newlines are preserved. When a
// turn carries only non-text blocks (tool_use / tool_result) a short
// placeholder is returned instead of an empty string, so the turn still shows
// up in the preview rather than silently vanishing.
func messageText(_ string, raw json.RawMessage) string {
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
		return clampRunes(strings.TrimRight(s, " \t\r\n"), previewMaxRunes)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	sawTool := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, strings.TrimRight(b.Text, " \t\r\n"))
			}
		case "tool_use", "tool_result":
			sawTool = true
		}
	}
	if len(parts) == 0 {
		if sawTool {
			return "[tool activity]"
		}
		return ""
	}
	return clampRunes(strings.Join(parts, "\n\n"), previewMaxRunes)
}

// clampRunes truncates s to max runes, appending an ellipsis when it cuts.
func clampRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// DeleteTranscript removes a discovered session's transcript file from disk.
// It returns an error when no transcript exists for the id, so the caller can
// surface a clear "already gone" message.
func DeleteTranscript(sessionID string) error {
	path := findTranscript(sessionID)
	if path == "" {
		return fmt.Errorf("no Claude Code transcript found for session %s", sessionID)
	}
	return os.Remove(path)
}

// encodeProjectPath turns a filesystem path into the directory name Claude Code
// uses for it under ~/.claude/projects — every "/" and "." becomes "-".
func encodeProjectPath(p string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(p)
}

// findTranscript locates a session's transcript anywhere under the Claude Code
// projects directory, or returns "" when there is none.
func findTranscript(sessionID string) string {
	matches, _ := filepath.Glob(
		filepath.Join(claudeHome(), "projects", "*", sessionID+".jsonl"))
	if len(matches) > 0 {
		return matches[0]
	}
	return ""
}

// PromoteTranscript moves a Claude Code session transcript into the project
// folder for an agent's isolated worktree, so the session can be resumed from
// that worktree. Claude Code scopes `--resume` to the current directory's
// project, so the transcript has to live alongside it.
//
// The worktree is always <repoRoot>/.agentkate/worktrees/<threadID>, so its
// project-folder name is the source folder name plus that encoded suffix — the
// repoRoot encoding is taken from disk rather than recomputed.
func PromoteTranscript(sessionID, threadID string) error {
	src := findTranscript(sessionID)
	if src == "" {
		return fmt.Errorf("no Claude Code transcript found for session %s", sessionID)
	}
	suffix := encodeProjectPath("/.agentkate/worktrees/" + threadID)
	dstName := filepath.Base(filepath.Dir(src)) + suffix
	dst := filepath.Join(claudeHome(), "projects", dstName, sessionID+".jsonl")
	if src == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
