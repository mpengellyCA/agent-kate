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
// ACP initialize handshake and hands the connected client to fn — along with
// the RAW initialize result, which used to be discarded (plan 26): it carries
// the authMethods whose _meta.terminal-auth names the exact login command the
// engine health card offers verbatim. No prompt is ever sent, so no model
// inference (or token spend) occurs. Teardown closes stdin and kills the whole
// process group — the same -pgid signalling the interrupt backstop uses — so
// nothing the CLI spawned can linger. Do NOT use this for real threads: there
// is no translator, no event log, no supervisor registration.
func (s *Supervisor) withProbe(fn func(ctx context.Context, client *acpClient, tmpDir string, initRaw json.RawMessage) error) error {
	return s.withProbeContext(context.Background(), fn)
}

// withProbeContext is withProbe with a caller-owned cancellation boundary. A
// health card's per-check deadline must be able to reap this child too; using
// Background here made a wedged auth probe outlive the health timeout.
func (s *Supervisor) withProbeContext(parent context.Context, fn func(ctx context.Context, client *acpClient, tmpDir string, initRaw json.RawMessage) error) error {
	if parent == nil {
		parent = context.Background()
	}
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

	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	var initRaw json.RawMessage
	if err := client.call(ctx, "initialize", initializeParams(), &initRaw); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	return fn(ctx, client, tmp, initRaw)
}

// Bin is the kimi binary this supervisor spawns ("kimi" unless overridden at
// construction) — exported so the engine-health checks run the SAME binary the
// threads will, never a different one that happens to be on PATH first.
func (s *Supervisor) Bin() string { return s.kimiBin }

// terminalAuthRemedy extracts the login command an ACP initialize result
// advertises (_meta.terminal-auth on an authMethod), rendered as the verbatim
// command line, e.g. "kimi login". Empty when the engine advertises none —
// the health card then offers NO remedy rather than inventing one.
func terminalAuthRemedy(initRaw json.RawMessage) string {
	var res struct {
		AuthMethods []struct {
			Meta struct {
				TerminalAuth struct {
					Command string   `json:"command"`
					Args    []string `json:"args"`
				} `json:"terminal-auth"`
			} `json:"_meta"`
		} `json:"authMethods"`
	}
	if json.Unmarshal(initRaw, &res) != nil {
		return ""
	}
	for _, m := range res.AuthMethods {
		if cmd := m.Meta.TerminalAuth.Command; cmd != "" {
			return strings.Join(append([]string{cmd}, m.Meta.TerminalAuth.Args...), " ")
		}
	}
	return ""
}

// EngineAuth is the engine-level auth verdict ProbeEngineAuth reaches — the
// answer to "would a session start right now?", asked with no thread. States
// are the harness health vocabulary ("ok" / "bad" / "unknown"), kept as plain
// strings so this package does not import the harness contract it serves.
type EngineAuth struct {
	State  string
	Detail string
	// Remedy is the engine's own advertised login command (see
	// terminalAuthRemedy) — never invented here.
	Remedy string
	// Models is the model-catalogue size the probe's session/new reported;
	// 0 when the session never opened.
	Models int
}

// ProbeEngineAuth runs initialize + session/new in a throwaway home-respecting
// probe. session/new succeeding IS the auth check — an unauthenticated kimi
// answers it with "Authentication required" (verified live on 0.31.1), while
// initialize succeeds either way and merely advertises the login command. A
// successful probe also seeds the DiscoverOptions cache, since session/new
// carries the same configOptions a discovery probe would spawn a second
// process to fetch.
func (s *Supervisor) ProbeEngineAuth(ctx context.Context) (EngineAuth, error) {
	out := EngineAuth{State: "unknown"}
	err := s.withProbeContext(ctx, func(ctx context.Context, client *acpClient, tmp string,
		initRaw json.RawMessage) error {
		out.Remedy = terminalAuthRemedy(initRaw)
		var res struct {
			ConfigOptions []ConfigOption `json:"configOptions"`
		}
		err := client.call(ctx, "session/new", map[string]any{
			"cwd":        tmp,
			"mcpServers": []MCPServer{}, // kimi rejects a null mcpServers (-32602)
		}, &res)
		if err == nil {
			out.State = "ok"
			out.Detail = "signed in"
			for _, o := range res.ConfigOptions {
				if o.ID == "model" {
					out.Models = len(o.Options)
				}
			}
			s.discoverMu.Lock()
			if !s.discovered && len(res.ConfigOptions) > 0 {
				s.discovered = true
				s.discoveredOpts = res.ConfigOptions
			}
			s.discoverMu.Unlock()
			return nil
		}
		if strings.Contains(err.Error(), "Authentication required") {
			out.State = "bad"
			out.Detail = "not signed in (the engine answered: Authentication required)"
			return nil
		}
		// The session failed for some OTHER reason — that is not an auth
		// verdict, and claiming "bad" here would tell the user to log in when
		// login would fix nothing.
		out.Detail = "session probe failed: " + err.Error()
		return nil
	})
	if err != nil {
		// The CLI never came up or never finished initialize: an unknown, with
		// whatever the probe could say. Best-effort by contract — the caller
		// renders the state, never an error.
		out.State = "unknown"
		if out.Detail == "" {
			out.Detail = err.Error()
		}
		return out, nil
	}
	return out, nil
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
	err := s.withProbe(func(ctx context.Context, client *acpClient, tmp string,
		_ json.RawMessage) error {
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
	err := s.withProbe(func(ctx context.Context, client *acpClient, _ string,
		_ json.RawMessage) error {
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

// SessionDir locates a kimi session's on-disk directory, or "" if there is
// none. kimi stores sessions under <home>/sessions/wd_<slug>_<hash>/session_<id>,
// where the wd_* segment encodes the working directory — so the session id
// alone needs a glob rather than a computable path. <home> is $KIMI_CODE_HOME
// when set (the per-thread isolation lever, plan 16 P6), else ~/.kimi-code.
func SessionDir(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home := os.Getenv("KIMI_CODE_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".kimi-code")
	}
	// Session directories are named "session_<id>"; a caller may pass either
	// spelling, so normalise to the on-disk one.
	name := sessionID
	if !strings.HasPrefix(name, "session_") {
		name = "session_" + name
	}
	matches, _ := filepath.Glob(filepath.Join(home, "sessions", "*", name))
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			return m
		}
	}
	return ""
}
