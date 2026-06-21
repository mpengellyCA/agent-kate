// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// ThreadTagProposal is Sonnet's suggested tag set for one thread. It is
// advisory: the UI previews proposals and the user confirms before anything is
// written, so SuggestTagOrganization never mutates the store.
type ThreadTagProposal struct {
	ThreadID string   `json:"threadId"`
	Tags     []string `json:"tags"`
}

// organizePrompt instructs Sonnet to cluster the supplied threads into a small
// shared vocabulary of tags. The strict-JSON-only contract mirrors
// commitmsg.go so the same fence-stripping defends the parse.
const organizePrompt = `You organize a list of coding-agent threads into tags for a sidebar.

You are given a JSON array of threads, each with: threadId, title, branch, status, lastTurn, existingTags.

Rules:
- Propose 3 to 7 short, lowercase tags TOTAL across all threads (reuse the same tags across threads; do not invent one tag per thread).
- Prefer reusing any existingTags you see; only add new ones when they capture a real grouping (e.g. area of code, feature, kind of work).
- Assign each thread 1 to 3 tags.
- Tags are short single words or hyphenated (e.g. "frontend", "bugfix", "auth"). No spaces, no punctuation other than hyphens.

Output ONLY a JSON array, no prose and no markdown code fences, in exactly this shape:
[{"threadId":"t-xxx","tags":["frontend","bugfix"]}]

Threads:
`

// organizeThread is the compact projection handed to Sonnet — just enough
// signal to cluster, nothing heavy like transcripts.
type organizeThread struct {
	ThreadID     string   `json:"threadId"`
	Title        string   `json:"title"`
	Branch       string   `json:"branch"`
	Status       string   `json:"status"`
	LastTurn     string   `json:"lastTurn,omitempty"`
	ExistingTags []string `json:"existingTags,omitempty"`
}

// SuggestTagOrganization runs a one-shot Sonnet call over the given records and
// returns its proposed tag assignments. It is read-only: callers apply the
// proposals (or not) themselves. claudeBin is the binary to invoke ("" →
// "claude" via PATH); model is the Sonnet variant ("" → "claude-sonnet-4-6").
// Proposals for thread ids not present in records are dropped, and each
// thread's tag list is clamped via clampProposalTags (≤3 tags/thread, ≤32
// chars/tag) to match the prompt.
func SuggestTagOrganization(ctx context.Context, threads []Record, claudeBin, model string) ([]ThreadTagProposal, error) {
	if len(threads) == 0 {
		return nil, nil
	}
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	bin := claudeBin
	if bin == "" {
		bin = "claude"
	}

	known := make(map[string]bool, len(threads))
	compact := make([]organizeThread, 0, len(threads))
	for _, r := range threads {
		known[r.ThreadID] = true
		ot := organizeThread{
			ThreadID:     r.ThreadID,
			Title:        r.Title,
			Branch:       r.Worktree.Branch,
			Status:       r.Status,
			ExistingTags: r.Tags,
		}
		if !r.LastTurnAt.IsZero() {
			ot.LastTurn = r.LastTurnAt.Format("2006-01-02")
		}
		compact = append(compact, ot)
	}
	payload, err := json.Marshal(compact)
	if err != nil {
		return nil, fmt.Errorf("organize: marshal threads: %w", err)
	}

	cmd := exec.CommandContext(ctx, bin,
		"--print",
		"--output-format", "text",
		"--model", model,
		organizePrompt+string(payload),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("claude failed: %w (stderr: %s)", err,
			strings.TrimSpace(stderr.String()))
	}

	out := stripOrganizeFence(strings.TrimSpace(stdout.String()))
	var raw []ThreadTagProposal
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("organize: parse model output: %w", err)
	}

	proposals := make([]ThreadTagProposal, 0, len(raw))
	for _, p := range raw {
		if !known[p.ThreadID] {
			continue // ignore hallucinated thread ids
		}
		tags := clampProposalTags(p.Tags)
		if len(tags) == 0 {
			continue
		}
		proposals = append(proposals, ThreadTagProposal{ThreadID: p.ThreadID, Tags: tags})
	}
	return proposals, nil
}

// stripOrganizeFence removes a single wrapping ```...``` fence from s if the
// whole response is one fenced block. Mirrors gitstatus.stripCodeFence —
// Sonnet occasionally fences JSON despite the instruction not to.
func stripOrganizeFence(s string) string {
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") {
		return s
	}
	inner := strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(inner, '\n'); i >= 0 {
		inner = inner[i+1:]
	}
	inner = strings.TrimSuffix(inner, "```")
	return strings.TrimSpace(inner)
}

// clampProposalTags trims, lowercases, dedupes and caps a proposed tag list at
// 3 per thread (matching the prompt) with a 32-char ceiling per tag.
func clampProposalTags(in []string) []string {
	const maxPerThread = 3
	const maxLen = 32
	var out []string
	seen := make(map[string]bool)
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if len([]rune(t)) > maxLen {
			t = string([]rune(t)[:maxLen])
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= maxPerThread {
			break
		}
	}
	return out
}
