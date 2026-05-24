// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"agentkate/internal/worktree"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// Hunk is one contiguous run of edits between HEAD and the working tree.
// Line numbers are 1-based; *Lines is the run length (0 for pure inserts).
// Kind is what the UI renders as: a green / blue / red marker.
type Hunk struct {
	BaseStart int    `json:"baseStart"` // first line in HEAD
	BaseLines int    `json:"baseLines"`
	NewStart  int    `json:"newStart"` // first line in the working tree
	NewLines  int    `json:"newLines"`
	Kind      string `json:"kind"` // "add" | "modify" | "delete"
}

// computeFileHunks diffs a single file in the worktree against its HEAD blob
// and returns the hunks. A file that's untracked or only-on-disk reports a
// single "add" hunk spanning all of its lines.
func computeFileHunks(wt worktree.Worktree, relPath string) ([]Hunk, error) {
	abs := filepath.Join(wt.Path, filepath.FromSlash(relPath))
	newBytes, newErr := os.ReadFile(abs)
	if newErr != nil && !errors.Is(newErr, os.ErrNotExist) {
		return nil, newErr
	}

	repo, err := openRepo(wt.Path)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return nil, nil
		}
		return nil, err
	}

	headBytes, err := readHeadBlob(repo, relPath)
	if err != nil {
		return nil, err
	}

	// File only exists on one side — collapse to one big hunk.
	if headBytes == nil && newBytes != nil {
		lines := countLines(newBytes)
		if lines == 0 {
			return nil, nil
		}
		return []Hunk{{NewStart: 1, NewLines: lines, Kind: "add"}}, nil
	}
	if headBytes != nil && newBytes == nil {
		lines := countLines(headBytes)
		if lines == 0 {
			return nil, nil
		}
		return []Hunk{{BaseStart: 1, BaseLines: lines, Kind: "delete"}}, nil
	}
	if headBytes == nil && newBytes == nil {
		return nil, nil
	}
	if bytes.Equal(headBytes, newBytes) {
		return nil, nil
	}

	return lineDiffHunks(string(headBytes), string(newBytes)), nil
}

// readHeadBlob returns the file's HEAD contents, or nil if the path is not in
// HEAD (e.g. an untracked or newly added file).
func readHeadBlob(repo *git.Repository, relPath string) ([]byte, error) {
	head, err := repo.Head()
	if err != nil {
		// No commits yet — every file looks "added".
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, nil
		}
		return nil, err
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	file, err := tree.File(relPath)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, nil
		}
		return nil, err
	}
	r, err := file.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// lineDiffHunks line-diffs old vs new and folds the edit list into hunks. The
// diffmatchpatch library compares strings; DiffLinesToChars maps each unique
// line to a code point so the underlying diff is line-granular.
func lineDiffHunks(oldText, newText string) []Hunk {
	dmp := diffmatchpatch.New()
	encOld, encNew, lines := dmp.DiffLinesToChars(oldText, newText)
	edits := dmp.DiffMain(encOld, encNew, false)
	edits = dmp.DiffCharsToLines(edits, lines)

	var hunks []Hunk
	oldLine, newLine := 1, 1
	flush := func(h *Hunk) {
		if h == nil {
			return
		}
		switch {
		case h.NewLines > 0 && h.BaseLines == 0:
			h.Kind = "add"
		case h.NewLines == 0 && h.BaseLines > 0:
			h.Kind = "delete"
		default:
			h.Kind = "modify"
		}
		hunks = append(hunks, *h)
	}

	var cur *Hunk
	for _, e := range edits {
		n := countLines([]byte(e.Text))
		// diffmatchpatch sometimes emits a trailing chunk without a newline;
		// treat at least one line of payload as present.
		if n == 0 && len(e.Text) > 0 {
			n = 1
		}
		switch e.Type {
		case diffmatchpatch.DiffEqual:
			flush(cur)
			cur = nil
			oldLine += n
			newLine += n
		case diffmatchpatch.DiffDelete:
			if cur == nil {
				cur = &Hunk{BaseStart: oldLine, NewStart: newLine}
			}
			cur.BaseLines += n
			oldLine += n
		case diffmatchpatch.DiffInsert:
			if cur == nil {
				cur = &Hunk{BaseStart: oldLine, NewStart: newLine}
			}
			cur.NewLines += n
			newLine += n
		}
	}
	flush(cur)
	return hunks
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if !strings.HasSuffix(string(b), "\n") {
		n++ // last line is unterminated
	}
	return n
}
