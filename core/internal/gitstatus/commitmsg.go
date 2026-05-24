// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"agentkate/internal/worktree"
)

// commitMessagePrompt asks Sonnet for a conventional git commit message in
// the AgentKate house style: a short imperative subject line under 70 chars,
// optionally followed by a blank line and a short body that explains *why*,
// never *what* (the diff already shows what). No emojis, no preamble.
const commitMessagePrompt = `You are writing a git commit message for the diff below.

Output ONLY the commit message — no preamble, no quoting, no markdown code fences.

Format:
- Subject line: imperative, under 70 characters, no trailing period.
- If the change is non-trivial, leave one blank line and add a short body (1–3 lines) explaining *why* the change was made. Don't restate what the diff already shows. Skip the body entirely when the subject is self-evident.
- No emojis. No Co-authored-by lines. No "AI" / "Claude" mentions.

Diff:
`

// maxDiffBytesForSuggestion caps how much patch text we hand to the LLM.
// Above this we truncate from the tail with a note so the prompt stays cheap
// and Sonnet still has a representative sample of the change.
const maxDiffBytesForSuggestion = 64 * 1024

// SuggestCommitMessage runs Claude Sonnet on the worktree's current diff and
// returns the suggested commit message. The returned string is already
// trimmed; on an empty diff it returns the empty string with no error.
//
// claudeBin is the binary to invoke ("" → "claude" via PATH); model is the
// Sonnet variant to use (e.g. "claude-sonnet-4-6").
func SuggestCommitMessage(ctx context.Context, wt worktree.Worktree, claudeBin, model string) (string, error) {
	if wt.Path == "" {
		return "", fmt.Errorf("suggest: worktree has no path")
	}
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	bin := claudeBin
	if bin == "" {
		bin = "claude"
	}

	patch, err := UnifiedDiff(wt, "")
	if err != nil {
		return "", err
	}
	patch = strings.TrimSpace(patch)
	if patch == "" {
		return "", nil
	}
	if len(patch) > maxDiffBytesForSuggestion {
		patch = patch[:maxDiffBytesForSuggestion] +
			"\n\n[diff truncated — only the first 64 KiB shown]\n"
	}

	cmd := exec.CommandContext(ctx, bin,
		"--print",
		"--output-format", "text",
		"--model", model,
		commitMessagePrompt+patch,
	)
	cmd.Dir = wt.Path
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude failed: %w (stderr: %s)", err,
			strings.TrimSpace(stderr.String()))
	}

	msg := strings.TrimSpace(stdout.String())
	// Defensive: Claude sometimes wraps a one-liner in a markdown code fence
	// despite the instruction. Strip a leading/trailing ``` block if it's
	// the only fencing.
	msg = stripCodeFence(msg)
	return msg, nil
}

// stripCodeFence removes a single wrapping ```...``` fence from msg if the
// entire response is one fenced block. Leaves multi-fence content alone.
func stripCodeFence(msg string) string {
	if !strings.HasPrefix(msg, "```") || !strings.HasSuffix(msg, "```") {
		return msg
	}
	inner := strings.TrimPrefix(msg, "```")
	if i := strings.IndexByte(inner, '\n'); i >= 0 {
		inner = inner[i+1:]
	}
	inner = strings.TrimSuffix(inner, "```")
	return strings.TrimSpace(inner)
}
