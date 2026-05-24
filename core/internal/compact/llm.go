package compact

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CompactPrompt is the user message sent to the compactor model. It asks for
// a dense, decision-aware summary the next session can resume from. Crafted
// to favour facts, decisions and pending work over restating tool output.
const CompactPrompt = `This conversation is being compacted before it is resumed in a new session. Produce a concise, information-dense summary that the next session can start from.

Include:
1. The original task and any evolved goal.
2. Key decisions made and the reasoning behind them (constraints, gotchas, anything that would surprise a fresh reader).
3. Files touched and their current state.
4. Open TODOs or unfinished work.
5. The most recent user request, verbatim.

Output only the summary, in markdown, with no preamble or sign-off. Aim for under 5,000 tokens.`

// LLMOptions configures one cold LLM compaction.
type LLMOptions struct {
	ClaudeBin string        // claude binary; empty resolves to "claude" via PATH
	WorkDir   string        // working directory for the subprocess
	SessionID string        // claude --resume <id>
	Model     string        // claude --model <id>; required for cold compactions
	Timeout   time.Duration // 0 = no timeout
}

// RunLLM spawns a claude subprocess that resumes the given session, sends a
// single compaction turn with CompactPrompt, and returns its response as the
// Summary body. The strategy is recorded on the result so the caller knows
// which path produced it (Hot Opus uses a separate inline path).
//
// This is the cold path: the spawned claude pays a full prefix re-cache on
// its chosen model. That cost is part of the trade — see compact.Strategy.
func RunLLM(ctx context.Context, threadID string, strategy Strategy, opts LLMOptions) (Summary, error) {
	if opts.SessionID == "" {
		return Summary{}, fmt.Errorf("compact: no session id to resume")
	}
	if opts.Model == "" {
		return Summary{}, fmt.Errorf("compact: cold LLM compaction requires a model")
	}
	bin := opts.ClaudeBin
	if bin == "" {
		bin = "claude"
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin,
		"--print",
		"--output-format", "text",
		"--resume", opts.SessionID,
		"--model", opts.Model,
		CompactPrompt,
	)
	cmd.Dir = opts.WorkDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Summary{}, fmt.Errorf("compact: claude failed: %w (stderr: %s)",
			err, strings.TrimSpace(stderr.String()))
	}

	body := strings.TrimSpace(stdout.String())
	if body == "" {
		return Summary{}, fmt.Errorf("compact: claude returned empty body (stderr: %s)",
			strings.TrimSpace(stderr.String()))
	}

	return Summary{
		ThreadID:  threadID,
		SessionID: opts.SessionID,
		Strategy:  strategy,
		Created:   time.Now().UTC(),
		Body:      body,
	}, nil
}
