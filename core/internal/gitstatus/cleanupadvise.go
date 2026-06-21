// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The Agent Kate developers

package gitstatus

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// cleanupAdvicePrompt asks Sonnet for a one-word recommendation per candidate.
// ADVISORY ONLY — the deterministic gate (State/Removable) is unaffected. The
// model never sees or controls whether a worktree CAN be removed; it only
// suggests whether a removable one is worth keeping.
const cleanupAdvicePrompt = `You are advising on which agent git worktrees are safe to clean up.

For EACH numbered candidate below, output exactly one line in the form:

  <number>: <RECOMMENDATION> - <short reason>

Where RECOMMENDATION is one of: REMOVE, KEEP, REVIEW.
- REMOVE: clearly disposable (merged, no unique work).
- KEEP: has valuable unmerged/uncommitted work that should be landed first.
- REVIEW: ambiguous; a human should look.

Output ONLY those lines, one per candidate, in order. No preamble, no markdown.

Candidates:
`

// AdviseCleanup runs one batched Sonnet call over the candidates and fills each
// one's Recommendation and Reason fields. It is ADVISORY ONLY: it never touches
// State, Removable, Blockers, or Warnings. On ANY error (LLM unavailable, parse
// failure, context cancelled) it returns the candidates unchanged, so the
// deterministic verdict always stands alone.
//
// claudeBin is the binary to invoke ("" → "claude" via PATH); model defaults to
// the Sonnet variant SuggestCommitMessage uses.
func AdviseCleanup(ctx context.Context, claudeBin, model string, cands []CleanupCandidate) []CleanupCandidate {
	if len(cands) == 0 {
		return cands
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bin := claudeBin
	if bin == "" {
		bin = "claude"
	}
	if model == "" {
		model = "claude-sonnet-4-6"
	}

	var prompt strings.Builder
	prompt.WriteString(cleanupAdvicePrompt)
	for i, c := range cands {
		fmt.Fprintf(&prompt, "%d) agent #%d branch=%q state=%s merged=%v ahead=%d dirty=%d unpushed=%d stash=%d title=%q\n",
			i+1, c.Number, c.Branch, c.State, c.Merged, c.Ahead, c.DirtyCount,
			c.UnpushedCommits, c.StashCount, c.Title)
	}

	cmd := exec.CommandContext(ctx, bin,
		"--print",
		"--output-format", "text",
		"--model", model,
		prompt.String(),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return cands // advisory: never let an LLM failure change the result
	}

	parsed := parseAdvice(stdout.String())
	out := make([]CleanupCandidate, len(cands))
	copy(out, cands)
	for i := range out {
		if a, ok := parsed[i+1]; ok {
			out[i].Recommendation = a.rec
			out[i].Reason = a.reason
		}
	}
	return out
}

type advice struct {
	rec    string
	reason string
}

// parseAdvice maps each "<n>: REC - reason" line to its candidate index. Lines
// it cannot parse are skipped; nothing here can fail in a way that changes the
// verdict.
func parseAdvice(text string) map[int]advice {
	out := map[int]advice{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(line[:colon]))
		if err != nil {
			continue
		}
		rest := strings.TrimSpace(line[colon+1:])
		rec, reason := rest, ""
		if dash := strings.Index(rest, " - "); dash >= 0 {
			rec = strings.TrimSpace(rest[:dash])
			reason = strings.TrimSpace(rest[dash+3:])
		}
		out[n] = advice{rec: rec, reason: reason}
	}
	return out
}
