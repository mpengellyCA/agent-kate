package agent

// Antigravity backend support. Google's `agy` CLI is one-shot in its
// documented `--print` mode — no stream-json protocol, no per-process MCP,
// no session-id pinning. So each turn spawns a fresh process; we capture its
// stdout and synthesise stream-json events (system / assistant / result) so
// the UI's existing renderer can show the conversation uniformly.
//
// Limitations versus the Claude backend:
//   - no live tool cards (agy's tool calls are not surfaced in --print mode)
//   - no per-tool approvals; we pass --dangerously-skip-permissions
//   - no image attachments (text attachments are inlined into the prompt)
//   - resumption uses `agy --continue` (most-recent conversation in the
//     working directory), not a session id; safe because each agent lives in
//     its own worktree

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// agyVisiblePath returns a non-hidden path that points at t.WorkDir, suitable
// for handing to `agy` as a workspace. AgentKate's worktrees live under
// `.agentkate/worktrees/<id>` and agy refuses to load workspaces whose URI
// contains any hidden (dot-prefixed) component ("is hidden: ignore uri" in
// its log). We work around that by symlinking the real worktree from a clean
// path under XDG_RUNTIME_DIR or /tmp and returning the symlink path. Safe to
// call repeatedly — the link is recreated if missing or stale.
func agyVisiblePath(workDir string) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "agentkate-agy-"+strconv.Itoa(os.Getuid()))
	} else {
		base = filepath.Join(base, "agentkate-agy")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("agy link base: %w", err)
	}
	// Stable per-worktree link name; an absolute-path hash would avoid
	// collisions across worktrees with the same basename in different repos,
	// but a single AgentKate session can't put two of those on the same path
	// anyway. Use the basename plus a digest of the parent dir for safety.
	link := filepath.Join(base, filepath.Base(workDir)+"-"+strconv.FormatUint(stableHash(workDir), 16))
	// If a link or file is already there, only replace it if it doesn't
	// already resolve to workDir.
	if existing, err := os.Readlink(link); err == nil {
		if existing == workDir {
			return link, nil
		}
		_ = os.Remove(link)
	} else if _, err := os.Lstat(link); err == nil {
		_ = os.Remove(link)
	}
	if err := os.Symlink(workDir, link); err != nil {
		return "", fmt.Errorf("agy symlink: %w", err)
	}
	return link, nil
}

// stableHash is a tiny FNV-1a so we can derive a deterministic suffix without
// dragging in hash/fnv at every call site.
func stableHash(s string) uint64 {
	const offset = 1469598103934665603
	const prime = 1099511628211
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// startAntigravity registers a thread for the agy backend and fires the
// initial prompt. There is no long-lived process — each turn is a spawn.
func (s *Supervisor) startAntigravity(opts StartOptions) (*Thread, error) {
	id := opts.ID
	if id == "" {
		id = NewThreadID()
	}
	t := &Thread{
		ID:        id,
		WorkDir:   opts.WorkDir,
		backend:   BackendAntigravity,
		sessionID: opts.SessionID,
		meter:     newToolMeter(s.log, id),
		usage:     newUsageMeter(s.log, id),
		alive:     true,
	}
	// A resumed thread continues the most-recent agy conversation in its
	// worktree, so the first follow-up should pass --continue.
	if opts.Resume {
		t.agyHadTurn = true
	}

	s.mu.Lock()
	s.threads[t.ID] = t
	s.mu.Unlock()

	s.log.Info("agent thread registered", "thread", t.ID, "backend", BackendAntigravity,
		"dir", opts.WorkDir)

	// Synthetic init event so the UI sees a consistent stream-json header.
	s.emitAntigravityInit(t)

	if opts.Prompt != "" || len(opts.Attachments) > 0 {
		if err := s.sendAntigravity(t, opts.Prompt, opts.Attachments); err != nil {
			s.log.Warn("antigravity initial send failed", "thread", t.ID, "err", err)
		}
	}
	return t, nil
}

// sendAntigravity spawns one `agy --print` per turn, streams its stdout into
// a buffer, and emits an assistant text event followed by a result event when
// the process exits. The thread's per-turn cmd is held on the Thread so Stop
// can kill it; concurrent sends on the same thread are not supported (the UI
// disables the send button while a turn is in flight).
func (s *Supervisor) sendAntigravity(t *Thread, text string, attachments []Attachment) error {
	t.mu.Lock()
	if !t.alive {
		t.mu.Unlock()
		return fmt.Errorf("thread %q is not running", t.ID)
	}
	if t.agyCmd != nil {
		t.mu.Unlock()
		return fmt.Errorf("thread %q has a turn in flight", t.ID)
	}

	// agy refuses workspaces whose URI contains a hidden (dot-prefixed)
	// component, and AgentKate worktrees live under `.agentkate/`. Symlink
	// the worktree to a clean path under XDG_RUNTIME_DIR and hand THAT to
	// agy; both --add-dir (workspace) and cmd.Dir (cwd) point at the link.
	workspacePath, err := agyVisiblePath(t.WorkDir)
	if err != nil {
		t.mu.Unlock()
		return fmt.Errorf("agy workspace prep: %w", err)
	}

	prompt := buildAntigravityPrompt(text, attachments)
	// --add-dir tells agy to treat the worktree as the active workspace; cwd
	// alone isn't enough (without it agy falls back to ~/.gemini/.../scratch).
	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--add-dir", workspacePath,
	}
	if t.agyHadTurn {
		args = append(args, "--continue")
	}
	cmd := exec.Command(s.antigravityBin, args...)
	cmd.Dir = workspacePath
	cmd.Stdin = strings.NewReader(prompt)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.mu.Unlock()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.mu.Unlock()
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		t.mu.Unlock()
		return fmt.Errorf("start %s: %w", s.antigravityBin, err)
	}
	t.agyCmd = cmd
	t.mu.Unlock()

	// Echo the user message back as a stream-json `user` event so the panel's
	// existing renderer logs the turn boundary (the panel itself already shows
	// the user bubble client-side, but transcripts and observers want the
	// event in the stream too).
	s.emitAntigravityUser(t, text, attachments)

	go s.runAntigravityTurn(t, cmd, stdoutPipe, stderrPipe)
	return nil
}

// runAntigravityTurn drains the per-turn process and emits synthetic events.
func (s *Supervisor) runAntigravityTurn(t *Thread, cmd *exec.Cmd, stdout, stderr io.Reader) {
	start := time.Now()

	var wg sync.WaitGroup
	var outBuf bytes.Buffer
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&outBuf, stdout)
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				line := strings.TrimRight(string(buf[:n]), "\n")
				if line != "" {
					s.log.Debug("agy stderr", "thread", t.ID, "line", line)
					s.emitSynthetic(t.ID, "_stderr", line)
				}
			}
			if err != nil {
				return
			}
		}
	}()
	wg.Wait()
	waitErr := cmd.Wait()

	t.mu.Lock()
	t.agyCmd = nil
	t.agyHadTurn = true
	t.mu.Unlock()

	reply := strings.TrimRight(outBuf.String(), "\n")
	if reply != "" {
		s.emitAntigravityAssistant(t, reply)
	}

	subtype := "success"
	detail := ""
	if waitErr != nil {
		subtype = "error"
		detail = waitErr.Error()
	}
	s.emitAntigravityResult(t, subtype, detail, time.Since(start))
}

// stopAntigravity kills any in-flight turn and marks the thread no-longer-alive.
func (s *Supervisor) stopAntigravity(t *Thread) error {
	t.mu.Lock()
	t.alive = false
	cmd := t.agyCmd
	t.agyCmd = nil
	t.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	s.mu.Lock()
	delete(s.threads, t.ID)
	s.mu.Unlock()
	s.emitLifecycle(t.ID, "exited", "stopped")
	return nil
}

// buildAntigravityPrompt flattens text + text attachments into one prompt.
// Image attachments are dropped with a note since `agy --print` has no
// documented multi-modal input.
func buildAntigravityPrompt(text string, attachments []Attachment) string {
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
	}
	dropped := 0
	for _, a := range attachments {
		switch a.Kind {
		case "text":
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			fmt.Fprintf(&b, "Attached file `%s`:\n```\n%s\n```", a.Name, a.Text)
		case "image":
			dropped++
		}
	}
	if dropped > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "(%d image attachment(s) dropped — Antigravity --print does not accept images)", dropped)
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

func (s *Supervisor) emitAntigravityInit(t *Thread) {
	s.emitObj(t.ID, map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": t.sessionID,
		"model":      "antigravity-cli",
	})
}

func (s *Supervisor) emitAntigravityUser(t *Thread, text string, attachments []Attachment) {
	s.emitObj(t.ID, map[string]any{
		"type":       "user",
		"session_id": t.sessionID,
		"message": map[string]any{
			"role":    "user",
			"content": buildUserContent(text, attachments),
		},
	})
}

func (s *Supervisor) emitAntigravityAssistant(t *Thread, reply string) {
	s.emitObj(t.ID, map[string]any{
		"type":       "assistant",
		"session_id": t.sessionID,
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": reply},
			},
		},
	})
}

func (s *Supervisor) emitAntigravityResult(t *Thread, subtype, detail string, dur time.Duration) {
	obj := map[string]any{
		"type":        "result",
		"subtype":     subtype,
		"session_id":  t.sessionID,
		"duration_ms": dur.Milliseconds(),
	}
	if detail != "" {
		obj["is_error"] = true
		obj["result"] = detail
	}
	if b, err := json.Marshal(obj); err == nil {
		s.emit(t.ID, json.RawMessage(b))
	}
}
