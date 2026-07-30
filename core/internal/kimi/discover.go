package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentkate/internal/safe"
)

// initializeParams is the ACP initialize request body, shared verbatim by the
// real thread handshake and the one-shot probes below so the CLI always sees
// the same client.
func initializeParams() map[string]any {
	return map[string]any{
		"protocolVersion": 1,
		// fs/terminal capabilities are deliberately NOT advertised: kimi then
		// does its own file I/O and shell execution locally instead of
		// reverse-RPCing them back to us.
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "agentkate", "version": "1"},
	}
}

// probeDirPrefix names the throwaway working directories one-shot probes run
// in — and lets ListSessions recognise (and hide) the sessions those probes
// leave behind in kimi's own store.
const probeDirPrefix = "akcore-kimi-probe-"

// withProbe spawns a throwaway `kimi acp` child in a temp directory, runs the
// ACP initialize handshake and hands the connected client to fn. No prompt is
// ever sent, so no model inference (or token spend) occurs. Teardown closes
// stdin and kills the whole process group — the same -pgid signalling the
// interrupt backstop uses — so nothing the CLI spawned can linger. Do NOT use
// this for real threads: there is no translator, no event log, no supervisor
// registration.
func (s *Supervisor) withProbe(fn func(ctx context.Context, client *acpClient, tmpDir string) error) error {
	tmp, err := os.MkdirTemp("", probeDirPrefix)
	if err != nil {
		return fmt.Errorf("probe dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command(s.kimiBin, "acp")
	cmd.Dir = tmp
	// Own process group, like the real threads, so teardown can signal kimi
	// plus anything it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s acp: %w", s.kimiBin, err)
	}
	safe.Go("kimi.probeStderr", func() { _, _ = io.Copy(io.Discard, stderr) })

	client := newACPClient(stdin, s.log)
	client.onNotification = func(string, json.RawMessage) {} // probe: nothing to translate
	client.onRequest = func(f acpFrame) {
		// A probe never grants permissions or reverse file access.
		client.respondError(f.ID, codeMethodNotFound, "probe")
	}
	safe.Go("kimi.probeRead", func() { client.readLoop(stdout) })

	defer func() {
		// End stdin, then make sure the whole group is gone before the temp
		// dir is reclaimed. A probe has no turn to finish gracefully.
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.call(ctx, "initialize", initializeParams(), nil); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	return fn(ctx, client, tmp)
}

// DiscoverOptions returns kimi's live config-option vocabulary (the model /
// thinking / mode enumerations session/new reports), probed via a one-shot
// handshake and cached for the process lifetime; a failed probe is not cached,
// so the next call retries. session/new does persist a throwaway session in
// kimi's own store — acceptable, and ListSessions filters those out.
func (s *Supervisor) DiscoverOptions() ([]ConfigOption, error) {
	// The mutex is held across the probe deliberately: a concurrent caller
	// waits for the in-flight probe's cache instead of spawning a duplicate.
	s.discoverMu.Lock()
	defer s.discoverMu.Unlock()
	if s.discovered {
		return s.discoveredOpts, nil
	}
	var opts []ConfigOption
	err := s.withProbe(func(ctx context.Context, client *acpClient, tmp string) error {
		var res struct {
			ConfigOptions []ConfigOption `json:"configOptions"`
		}
		if err := client.call(ctx, "session/new", map[string]any{
			"cwd":        tmp,
			"mcpServers": []MCPServer{}, // kimi rejects a null mcpServers (-32602)
		}, &res); err != nil {
			return fmt.Errorf("acp session/new: %w", err)
		}
		opts = res.ConfigOptions
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.discovered = true
	s.discoveredOpts = opts
	return opts, nil
}

// SessionInfo is one past kimi session from session/list.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"` // RFC3339
}

// ListSessions lists kimi's stored past sessions via a one-shot handshake and
// session/list, optionally filtered to sessions started in cwd (empty = all).
// Never cached — the store changes as sessions run. The throwaway sessions
// DiscoverOptions probes leave behind are filtered out.
func (s *Supervisor) ListSessions(cwd string) ([]SessionInfo, error) {
	var out []SessionInfo
	err := s.withProbe(func(ctx context.Context, client *acpClient, _ string) error {
		params := map[string]any{}
		if cwd != "" {
			params["cwd"] = cwd
		}
		var res struct {
			Sessions []SessionInfo `json:"sessions"`
		}
		if err := client.call(ctx, "session/list", params, &res); err != nil {
			return fmt.Errorf("acp session/list: %w", err)
		}
		probeDirs := filepath.Join(os.TempDir(), probeDirPrefix)
		for _, sess := range res.Sessions {
			if strings.HasPrefix(sess.Cwd, probeDirs) {
				continue // our own probes' throwaway sessions
			}
			out = append(out, sess)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
