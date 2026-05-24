// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"path/filepath"
	"strings"
	"time"

	"agentkate/internal/worktree"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// CommitDetail is everything the right-hand pane of the log viewer needs to
// render a selected commit: its LogEntry fields plus the full message body
// and the per-file change list.
type CommitDetail struct {
	LogEntry
	Body          string       `json:"body,omitempty"`
	CommitterName string       `json:"committerName,omitempty"`
	CommitTime    time.Time    `json:"commitTime"`
	Files         []CommitFile `json:"files,omitempty"`
}

// CommitFile is one entry in CommitDetail.Files. Added / Deleted are -1 for
// binary files (mirrors `git diff --numstat`'s "-" markers).
type CommitFile struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"` // set when go-git reports a rename
	Status  string `json:"status"`            // "added" | "modified" | "deleted"
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// CommitDetailFn returns metadata + the file list for one commit. Used by the
// `git.commit.detail` RPC.
func CommitDetailFn(wt worktree.Worktree, sha string) (*CommitDetail, error) {
	if wt.Path == "" || sha == "" {
		return nil, nil
	}
	repo, err := openRepo(wt.Path)
	if err != nil {
		return nil, err
	}
	c, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, err
	}
	subject, body := splitMessage(c.Message)
	parents := make([]string, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		parents[i] = p.String()
	}
	short := sha
	if len(short) > 8 {
		short = short[:8]
	}

	detail := &CommitDetail{
		LogEntry: LogEntry{
			SHA:         sha,
			ShortSHA:    short,
			Subject:     subject,
			Author:      c.Author.Name,
			AuthorEmail: c.Author.Email,
			AuthorTime:  c.Author.When.UTC(),
			Parents:     parents,
			Refs:        collectRefs(repo)[sha],
		},
		Body:          body,
		CommitterName: c.Committer.Name,
		CommitTime:    c.Committer.When.UTC(),
	}

	if files, err := commitFiles(c); err == nil {
		detail.Files = files
	}

	return detail, nil
}

// commitFiles walks the patch against the first parent (or the bare tree for a
// root commit) to produce one CommitFile per touched path. The status comes
// from which side of the diff carries the file — Stats() alone can't tell
// "modified, only added lines" from "newly added," because both have zero
// deletions.
func commitFiles(c *object.Commit) ([]CommitFile, error) {
	if c.NumParents() == 0 {
		return rootCommitFiles(c)
	}
	parent, err := c.Parents().Next()
	if err != nil {
		return nil, err
	}
	patch, err := parent.Patch(c)
	if err != nil {
		return nil, err
	}
	var files []CommitFile
	for _, fp := range patch.FilePatches() {
		from, to := fp.Files()
		cf := CommitFile{}
		switch {
		case from == nil && to != nil:
			cf.Path = to.Path()
			cf.Status = "added"
		case from != nil && to == nil:
			cf.Path = from.Path()
			cf.Status = "deleted"
		default: // both non-nil
			cf.Path = to.Path()
			cf.Status = "modified"
			if from.Path() != to.Path() {
				cf.OldPath = from.Path()
			}
		}
		if fp.IsBinary() {
			cf.Added, cf.Deleted = -1, -1
		} else {
			cf.Added, cf.Deleted = countChunkLines(fp.Chunks())
		}
		files = append(files, cf)
	}
	return files, nil
}

// rootCommitFiles is the no-parent case: every file in the tree shows up as
// added, with its line count.
func rootCommitFiles(c *object.Commit) ([]CommitFile, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	var files []CommitFile
	err = tree.Files().ForEach(func(f *object.File) error {
		added := 0
		bin, _ := f.IsBinary()
		if !bin {
			content, _ := f.Contents()
			added = strings.Count(content, "\n")
			if len(content) > 0 && !strings.HasSuffix(content, "\n") {
				added++ // unterminated last line counts as one
			}
		} else {
			added = -1
		}
		files = append(files, CommitFile{
			Path:   f.Name,
			Status: "added",
			Added:  added,
		})
		return nil
	})
	return files, err
}

func countChunkLines(chunks []diff.Chunk) (added, deleted int) {
	for _, ch := range chunks {
		s := ch.Content()
		n := strings.Count(s, "\n")
		if n == 0 && s != "" {
			n = 1 // unterminated trailing fragment
		}
		switch ch.Type() {
		case diff.Add:
			added += n
		case diff.Delete:
			deleted += n
		}
	}
	return
}

// CommitDiff returns the unified diff for one commit. relPath narrows the
// patch to a single file when non-empty.
//
// We shell out to `git show` rather than synthesise via go-git because go-git
// renders patches noticeably slower than git for repos with many changes per
// commit, and `git show` handles edge cases (binary, mode changes, renames)
// out of the box.
func CommitDiff(wt worktree.Worktree, sha, relPath string) (string, error) {
	if wt.Path == "" || sha == "" {
		return "", nil
	}
	// `-m --first-parent` makes merge commits show their diff against the
	// first parent (default `git show` emits header-only for merges, which
	// leaves the diff pane blank even though our file list has entries).
	args := []string{"show", "--no-color", "-M", "-m", "--first-parent", sha}
	if relPath != "" {
		args = append(args, "--", filepath.FromSlash(relPath))
	}
	out, err := runGit(wt.Path, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}
