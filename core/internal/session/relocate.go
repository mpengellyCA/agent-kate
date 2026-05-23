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
