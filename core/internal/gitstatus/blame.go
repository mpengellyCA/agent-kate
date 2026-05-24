// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"agentkate/internal/worktree"
)

// BlameLine is the per-line attribution the UI's annotation gutter shows.
// SHA is the abbreviated hash for compactness; AuthorTime is when the author
// originally wrote the change (committer time is intentionally ignored for
// blame purposes — it drifts on rebase and confuses readers).
type BlameLine struct {
	SHA        string    `json:"sha"`
	Author     string    `json:"author"`
	Summary    string    `json:"summary"`
	AuthorTime time.Time `json:"authorTime"`
}

// Blame attributes each line of relPath in the worktree to the commit that
// last touched it. Shells out to `git blame --porcelain`, which is the
// canonical machine-readable format and far simpler to parse than the
// human-oriented output.
func Blame(wt worktree.Worktree, relPath string) ([]BlameLine, error) {
	if wt.Path == "" || relPath == "" {
		return nil, nil
	}
	cmd := exec.Command("git", "blame", "--porcelain", "HEAD", "--",
		filepath.FromSlash(relPath))
	cmd.Dir = wt.Path
	out, err := cmd.Output()
	if err != nil {
		// File is untracked / not in HEAD — no blame possible, return empty.
		return nil, nil
	}
	return parsePorcelainBlame(string(out)), nil
}

// commitMeta caches the metadata fields porcelain only emits once per commit
// (subsequent runs of lines from that commit use the abbreviated header).
type commitMeta struct {
	author   string
	summary  string
	authored time.Time
}

func parsePorcelainBlame(out string) []BlameLine {
	commits := make(map[string]*commitMeta)
	var lines []BlameLine
	var curSHA string
	var curMeta *commitMeta
	var curFinalLine int // 1-based line number from blame's perspective

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// Content line: a single TAB, then the source. Records the blame.
		if line[0] == '\t' {
			if curMeta != nil && curFinalLine > 0 {
				lines = ensureSize(lines, curFinalLine)
				abbrev := curSHA
				if len(abbrev) > 8 {
					abbrev = abbrev[:8]
				}
				lines[curFinalLine-1] = BlameLine{
					SHA:        abbrev,
					Author:     curMeta.author,
					Summary:    curMeta.summary,
					AuthorTime: curMeta.authored,
				}
			}
			continue
		}
		// Header line. Two shapes:
		//   <sha> <origLine> <finalLine> <numLines>   ← first run of a commit
		//   <sha> <origLine> <finalLine>              ← subsequent lines
		// A 40-hex sha followed by a space is the canonical signature; we
		// don't need to inspect more of the line than that to distinguish a
		// header from a metadata line like "summary X" or "boundary".
		if len(line) >= 41 && line[40] == ' ' && isHexDigits(line[:40]) {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 3 {
				continue
			}
			curSHA = fields[0]
			if n, err := strconv.Atoi(fields[2]); err == nil {
				curFinalLine = n
			}
			if m, ok := commits[curSHA]; ok {
				curMeta = m
			} else {
				curMeta = &commitMeta{}
				commits[curSHA] = curMeta
			}
			continue
		}
		// Metadata for the current commit (only after the long-form header).
		switch {
		case strings.HasPrefix(line, "author "):
			curMeta.author = strings.TrimPrefix(line, "author ")
		case strings.HasPrefix(line, "author-time "):
			if t, err := strconv.ParseInt(
				strings.TrimPrefix(line, "author-time "), 10, 64); err == nil {
				curMeta.authored = time.Unix(t, 0).UTC()
			}
		case strings.HasPrefix(line, "summary "):
			curMeta.summary = strings.TrimPrefix(line, "summary ")
		}
	}
	return lines
}

func ensureSize(s []BlameLine, n int) []BlameLine {
	if len(s) >= n {
		return s
	}
	out := make([]BlameLine, n)
	copy(out, s)
	return out
}

func isHexDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
