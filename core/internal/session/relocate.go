package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"agentkate/internal/fsperm"
	"agentkate/internal/harness"
)

// ReadTranscript returns the raw JSON lines from a session's Claude Code
// transcript, in order, so the UI can replay the conversation when reopening a
// dormant thread. Returns nil with no error if there is no transcript yet.
//
// The read is BOUNDED (audit F10), the same way the kimi reader is: only the
// most recent events that fit the replay caps are kept, and core memory never
// exceeds those caps however long the on-disk transcript has grown. The old
// version read the WHOLE file in — a months-old thread cost hundreds of MB in
// the core before the handler-side cap ever ran, which is the freeze the cap
// exists to prevent. When older events are dropped the returned slice opens
// with a truncation notice, so a shortened history is visible rather than
// silent.
//
// Claude Code writes this file; a torn last line is possible if it was killed
// mid-append, so lines that are not valid JSON are skipped rather than relayed
// to the UI as a parse error in the panel.
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
	// Budget leaves room for the notice event the handler-side cap would
	// otherwise have to add on top (harness.CapTranscript is then a no-op).
	maxEvents := harness.MaxReplayEvents - 1
	maxBytes := harness.MaxReplayBytes - 4096

	var (
		ring    []json.RawMessage // most recent events, oldest first
		bytesIn int
		omitted int
		torn    int
	)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			torn++
			continue
		}
		ring = append(ring, append(json.RawMessage(nil), line...))
		bytesIn += len(line)
		// Keep at least one event, so a single oversized event still says
		// something rather than emptying the replay.
		for len(ring) > maxEvents || (bytesIn > maxBytes && len(ring) > 1) {
			bytesIn -= len(ring[0])
			ring = ring[1:]
			omitted++
		}
	}
	if err := sc.Err(); err != nil {
		// A line longer than the scanner cap ends the read. Everything up to it
		// is still a usable history, so report what we have rather than nothing
		// — but say so, because the tail is missing.
		if !errors.Is(err, bufio.ErrTooLong) {
			return nil, err
		}
		torn++
	}
	if torn > 0 {
		slog.Warn("claude transcript: skipped unreadable lines",
			"session", sessionID, "lines", torn)
	}
	if omitted == 0 {
		return ring, nil
	}
	if notice := harness.TruncationNotice(omitted); notice != nil {
		return append([]json.RawMessage{notice}, ring...), nil
	}
	return ring, nil
}

// PreviewMessage is one role-labelled turn extracted from a transcript for the
// session browser's preview pane.
type PreviewMessage struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"` // plain text of the turn
}

// PreviewTranscript returns up to limit of the most recent user/assistant turns
// from a session's transcript, for a read-only preview before attaching. It
// streams the file line by line and keeps only a limit-sized ring of turns, so
// peak memory is bounded by what it RETURNS rather than by the file — the
// comment used to promise this while the accumulator grew for every turn in the
// transcript, each up to previewMaxRunes. Reports whether earlier messages were
// dropped. limit <= 0 defaults to 20.
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
	var (
		ring      []PreviewMessage // most recent turns, oldest first
		truncated bool
	)
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
		ring = append(ring, PreviewMessage{Role: probe.Type, Text: text})
		if len(ring) > limit {
			ring = ring[1:]
			truncated = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return ring, truncated, nil
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
	// 0700, matching the ~/.claude/projects tree we are creating this project
	// directory inside: Claude Code keeps its own home owner-only, and a
	// transcript directory Agent Kate creates there must not be the one
	// world-readable node in it.
	if err := fsperm.MkdirAll(filepath.Dir(dst)); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
