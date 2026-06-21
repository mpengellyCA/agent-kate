// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"container/heap"
	"errors"
	"strings"
	"time"

	"agentkate/internal/worktree"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// LogEntry is one row in the visual log. Fields with `,omitempty` are stripped
// from the JSON when zero so the wire payload stays compact for big histories.
type LogEntry struct {
	SHA         string    `json:"sha"`
	ShortSHA    string    `json:"shortSha"`
	Subject     string    `json:"subject"`
	Author      string    `json:"author"`
	AuthorEmail string    `json:"authorEmail,omitempty"`
	AuthorTime  time.Time `json:"authorTime"`
	Parents     []string  `json:"parents,omitempty"`
	Refs        []string  `json:"refs,omitempty"` // "main", "tag:v1.0", "origin/main"
	Lane        int       `json:"lane"`
	LanesIn     []int     `json:"lanesIn,omitempty"`
	LanesOut    []int     `json:"lanesOut,omitempty"`
}

// LogOptions narrows what Log returns. Skip/Limit drive pagination; Path
// scopes to a subdirectory or file (in which case the graph collapses to a
// single vertical lane, since the synthetic parent relationship between
// filtered commits is not the literal git parent); Branch picks a non-HEAD
// starting point.
type LogOptions struct {
	Skip   int
	Limit  int
	Path   string
	Branch string
}

const (
	defaultLogLimit = 500
	maxLogLimit     = 2000
)

// Log returns one page of commit history for the worktree.
//
// Pagination is naive forward-only: Skip drops the first N commits emitted by
// the iterator, Limit caps the page. That matches how the UI consumes pages
// (scroll-to-bottom asks for skip += pageSize).
//
// Lane allocation runs over commits actually present in the page; lanes whose
// "next expected commit" lies past the page boundary stay open at the bottom
// of the rail — exactly the visualization we want for "continues below."
//
// Log itself is uncached: it walks the full history for every call. The Cache's
// LogPageFor wraps it with a per-(thread, HEAD) cache for the common HEAD-graph
// case so deep scrolling no longer re-walks from the top each page. This bare
// entry point is still used for path / branch-filtered requests (which the
// cache deliberately skips) and by callers without a registered thread.
func Log(wt worktree.Worktree, opts LogOptions) ([]LogEntry, error) {
	walked, refsBySHA, err := walkLog(wt, opts)
	if err != nil || walked == nil {
		return nil, err
	}
	return pageLog(walked, refsBySHA, opts), nil
}

// walkLog performs the expensive half of a log query: open the repo, resolve
// the starting commit, and walk the FULL history from it (honouring an optional
// Path filter), collecting the commits in iterator order plus the ref map.
//
// Crucially it ignores Skip/Limit so the result is paginable by slicing — the
// cache keeps one walk per (thread, HEAD) and every page reuses it. A nil
// commit slice with a nil error means "nothing to show" (empty/unborn repo or
// unknown branch); callers turn that into an empty page.
func walkLog(wt worktree.Worktree, opts LogOptions) ([]*object.Commit, map[string][]string, error) {
	if wt.Path == "" {
		return nil, nil, nil
	}

	repo, err := openRepo(wt.Path)
	if err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	from, err := resolveLogStart(repo, opts.Branch)
	if err != nil {
		// Empty repo or unknown branch — return an empty page rather than
		// error, so the UI just shows "no commits yet".
		return nil, nil, nil
	}

	iterOpts := &git.LogOptions{From: from}
	if opts.Path != "" {
		target := opts.Path
		iterOpts.PathFilter = func(p string) bool {
			return p == target || strings.HasPrefix(p, target+"/")
		}
	}
	iter, err := repo.Log(iterOpts)
	if err != nil {
		return nil, nil, err
	}
	defer iter.Close()

	walked := make([]*object.Commit, 0, defaultLogLimit)
	for {
		c, err := iter.Next()
		if err != nil {
			break
		}
		walked = append(walked, c)
	}
	if len(walked) == 0 {
		return nil, nil, nil
	}
	return walked, collectRefs(repo), nil
}

// pageLog turns a full walk + ref map (from walkLog) into one rendered page,
// applying Skip/Limit by slicing and then running the same per-page topo-sort +
// lane assignment the uncached path always did. Slicing the cached walk instead
// of re-walking git is the whole point of the cache; the per-page layout is
// computed over exactly the page's commits, so the output is byte-for-byte
// identical to the old single-shot Log.
func pageLog(walked []*object.Commit, refsBySHA map[string][]string, opts LogOptions) []LogEntry {
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	if limit > maxLogLimit {
		limit = maxLogLimit
	}

	skip := opts.Skip
	if skip < 0 {
		skip = 0
	}
	if skip >= len(walked) {
		return nil
	}
	end := skip + limit
	if end > len(walked) {
		end = len(walked)
	}
	collected := walked[skip:end]
	if len(collected) == 0 {
		return nil
	}

	if opts.Path != "" {
		// Path-filtered view: render as a linear list. The git parent of one
		// filtered commit is rarely the previous filtered commit, so drawing
		// edges would lie.
		return buildLinearList(collected, refsBySHA)
	}

	ordered := topoSortCommits(collected)
	nodes := make([]CommitNode, len(ordered))
	for i, c := range ordered {
		parents := make([]string, c.NumParents())
		for j, p := range c.ParentHashes {
			parents[j] = p.String()
		}
		nodes[i] = CommitNode{SHA: c.Hash.String(), Parents: parents}
	}
	rows := assignLanes(nodes)

	out := make([]LogEntry, len(ordered))
	for i, c := range ordered {
		out[i] = newLogEntry(c, rows[i], refsBySHA)
	}
	return out
}

// resolveLogStart returns the SHA the log should walk from: an explicit branch
// or tag if Branch is set, otherwise HEAD.
func resolveLogStart(repo *git.Repository, branch string) (plumbing.Hash, error) {
	if branch == "" {
		head, err := repo.Head()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return head.Hash(), nil
	}
	if ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true); err == nil {
		return ref.Hash(), nil
	}
	if ref, err := repo.Reference(plumbing.NewTagReferenceName(branch), true); err == nil {
		return ref.Hash(), nil
	}
	// Last resort: a full ref name, e.g. "refs/remotes/origin/main".
	if ref, err := repo.Reference(plumbing.ReferenceName(branch), true); err == nil {
		return ref.Hash(), nil
	}
	return plumbing.ZeroHash, plumbing.ErrReferenceNotFound
}

// collectRefs enumerates every branch / tag / remote-tracking ref and groups
// them by the SHA they point at. Used to annotate LogEntry.Refs.
func collectRefs(repo *git.Repository) map[string][]string {
	out := make(map[string][]string)
	iter, err := repo.References()
	if err != nil {
		return out
	}
	defer iter.Close()
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() != plumbing.HashReference {
			return nil
		}
		name := ref.Name()
		var label string
		switch {
		case name.IsBranch():
			label = name.Short()
		case name.IsTag():
			label = "tag:" + name.Short()
		case name.IsRemote():
			label = name.Short() // "origin/main"
		default:
			return nil
		}
		sha := ref.Hash().String()
		out[sha] = append(out[sha], label)
		return nil
	})
	return out
}

// buildLinearList produces a LogEntry slice with no graph topology — every row
// sits on lane 0 with a single vertical edge to its neighbour. Used for the
// path-filtered file-history view.
func buildLinearList(commits []*object.Commit, refsBySHA map[string][]string) []LogEntry {
	out := make([]LogEntry, len(commits))
	for i, c := range commits {
		row := laneRow{Lane: 0}
		if i > 0 {
			row.LanesIn = []int{0}
		}
		if i < len(commits)-1 {
			row.LanesOut = []int{0}
		}
		out[i] = newLogEntry(c, row, refsBySHA)
	}
	return out
}

// newLogEntry flattens an object.Commit + lane layout + refs into the wire
// type. Subject is the first message line; the body lives in CommitDetail.
func newLogEntry(c *object.Commit, row laneRow, refsBySHA map[string][]string) LogEntry {
	sha := c.Hash.String()
	short := sha
	if len(short) > 8 {
		short = short[:8]
	}
	subject, _ := splitMessage(c.Message)
	parents := make([]string, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		parents[i] = p.String()
	}
	return LogEntry{
		SHA:         sha,
		ShortSHA:    short,
		Subject:     subject,
		Author:      c.Author.Name,
		AuthorEmail: c.Author.Email,
		AuthorTime:  c.Author.When.UTC(),
		Parents:     parents,
		Refs:        refsBySHA[sha],
		Lane:        row.Lane,
		LanesIn:     row.LanesIn,
		LanesOut:    row.LanesOut,
	}
}

func splitMessage(msg string) (subject, body string) {
	msg = strings.TrimSpace(msg)
	if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
		return strings.TrimSpace(msg[:idx]), strings.TrimSpace(msg[idx+1:])
	}
	return msg, ""
}

// topoSortCommits returns the input in true topological order: every commit
// appears after all of its (in-set) descendants. Ties broken by committer
// time descending, so siblings interleave roughly the way `git log` does.
//
// We can't trust go-git's iterator order: its DFS visits one branch of a
// merge to its full depth before the other, which means a shared ancestor
// can come *before* the second branch's tip and break lane assignment.
func topoSortCommits(commits []*object.Commit) []*object.Commit {
	if len(commits) <= 1 {
		return commits
	}
	bySHA := make(map[string]*object.Commit, len(commits))
	for _, c := range commits {
		bySHA[c.Hash.String()] = c
	}
	indeg := make(map[string]int, len(commits))
	for _, c := range commits {
		indeg[c.Hash.String()] = 0
	}
	for _, c := range commits {
		for _, ph := range c.ParentHashes {
			if _, ok := bySHA[ph.String()]; ok {
				indeg[ph.String()]++
			}
		}
	}

	h := &commitHeap{}
	heap.Init(h)
	for _, c := range commits {
		if indeg[c.Hash.String()] == 0 {
			heap.Push(h, c)
		}
	}
	out := make([]*object.Commit, 0, len(commits))
	for h.Len() > 0 {
		c := heap.Pop(h).(*object.Commit)
		out = append(out, c)
		for _, ph := range c.ParentHashes {
			psha := ph.String()
			if _, ok := bySHA[psha]; !ok {
				continue
			}
			indeg[psha]--
			if indeg[psha] == 0 {
				heap.Push(h, bySHA[psha])
			}
		}
	}
	// Stragglers (cycles in history — shouldn't exist in real git, but be
	// safe): append whatever's left in arbitrary order.
	if len(out) < len(commits) {
		seen := make(map[string]bool, len(out))
		for _, c := range out {
			seen[c.Hash.String()] = true
		}
		for _, c := range commits {
			if !seen[c.Hash.String()] {
				out = append(out, c)
			}
		}
	}
	return out
}

// commitHeap is a max-heap by committer time (newest pops first), with SHA as
// a deterministic tiebreaker so test runs are reproducible.
type commitHeap []*object.Commit

func (h commitHeap) Len() int { return len(h) }
func (h commitHeap) Less(i, j int) bool {
	ti, tj := h[i].Committer.When, h[j].Committer.When
	if ti.Equal(tj) {
		return h[i].Hash.String() < h[j].Hash.String()
	}
	return ti.After(tj)
}
func (h commitHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *commitHeap) Push(x any)   { *h = append(*h, x.(*object.Commit)) }
func (h *commitHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
