package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ColdCompactOptions configures one cold compaction pass: a fresh `claude
// --print --resume` over an existing session, which pays a full prefix
// re-cache but works on a thread with no live process.
type ColdCompactOptions struct {
	WorkDir   string // cwd for the subprocess
	SessionID string // claude --resume <id>
	Model     string // claude --model <id>
	Prompt    string // the single turn to send
}

// CompactCold runs that pass and returns the model's reply.
//
// It lives on the supervisor because the supervisor owns the claude binary
// path — the same reason Start does. Before plan 16 P6 this was
// compact.RunLLM, i.e. a Claude-shaped subprocess spawned from the
// harness-neutral compaction package; the harness interface's Compact now
// routes here, and a harness that cannot do this says so instead.
func (s *Supervisor) CompactCold(ctx context.Context, opts ColdCompactOptions) (string, error) {
	if opts.SessionID == "" {
		return "", fmt.Errorf("compact: no session id to resume")
	}
	if opts.Model == "" {
		return "", fmt.Errorf("compact: cold compaction requires a model")
	}
	cmd := exec.CommandContext(ctx, s.claudeBin,
		"--print",
		"--output-format", "text",
		"--resume", opts.SessionID,
		"--model", opts.Model,
		opts.Prompt,
	)
	cmd.Dir = opts.WorkDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("compact: claude failed: %w (stderr: %s)",
			err, strings.TrimSpace(stderr.String()))
	}
	body := strings.TrimSpace(stdout.String())
	if body == "" {
		return "", fmt.Errorf("compact: claude returned empty body (stderr: %s)",
			strings.TrimSpace(stderr.String()))
	}
	return body, nil
}
