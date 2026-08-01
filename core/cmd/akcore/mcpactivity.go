package main

// The MCP activity feed (plan 16 P2, Feature 4a): every request an agent's MCP
// bridge completes is broadcast to the UI as an `mcp.activity` notification, so
// cooperation and orchestration traffic is watchable live instead of being
// invisible plumbing.
//
// It is emitted CORE-SIDE, from the one place every bridge call already passes
// through (the IPC server's dispatch), never by the bridge itself — the bridge
// stays a thin stdio↔IPC client, and an agent cannot fabricate, suppress or
// mistime its own feed entry. Nothing here knows which harness the thread runs:
// the map is keyed by RPC method, which is identical for every backend.
//
// The digest is deliberately small and capped. It names WHAT the agent did, not
// the payload it did it with: no environment values, no tokens, no prompt or
// message bodies, no text destined for a password field.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agentkate/internal/ipc"
)

// mcpSummaryCap bounds every string that reaches the feed. Summaries are a
// glance, not a log; the transcript already holds the full call.
const mcpSummaryCap = 120

// mcpToolNames maps the RPC methods a bridge can call onto the MCP tool the
// agent actually invoked (`akcore mcp`'s two catalogues: Cooperation in
// mcp.go, Cowork in mcp_cowork.go). A method missing here is reported under its
// raw name rather than dropped — a new RPC must never make traffic disappear.
// TestMCPToolMapCoversBridgeCallSites keeps it complete.
var mcpToolNames = map[string]string{
	// Cooperation
	"coop.listOpenFiles":   "list_open_files",
	"coop.postNote":        "post_note",
	"coop.readNotes":       "read_notes",
	"coop.getPresence":     "get_presence",
	"coop.claimFile":       "claim_file",
	"coop.releaseFile":     "release_file",
	"coop.requestReview":   "request_review",
	"agent.list":           "list_agents",
	"agent.discard":        "discard_agent",
	"agent.launchWorker":   "launch_agent",
	"agent.send":           "send_agent",
	"agent.wait":           "wait_agent",
	"agent.stopClose":      "close_agent",
	"permission.request":   "request_permission",
	"cowork.requestEnable": "enable_cowork",
	// Cowork desktop
	"cowork.listWindows":         "desktop_list_windows",
	"cowork.screenshot":          "desktop_screenshot",
	"cowork.listElements":        "desktop_list_elements",
	"cowork.readText":            "desktop_read_text",
	"cowork.activateElement":     "desktop_activate_element",
	"cowork.setElementText":      "desktop_set_text",
	"cowork.launchBrowser":       "desktop_open_browser",
	"cowork.injectInput":         "desktop_inject_input",
	"cowork.playInput":           "desktop_play_input",
	"cowork.movePointer":         "desktop_move_pointer",
	"cowork.movePointerRelative": "desktop_move_pointer_relative",
	"cowork.pointerClick":        "desktop_click",
	"cowork.pointerClickElement": "desktop_click_element",
	"cowork.scroll":              "desktop_scroll",
	"cowork.pointerDrag":         "desktop_drag",
	"cowork.setPointerProfile":   "desktop_set_pointer_profile",
}

// mcpQuietMethods are the connection's own identity handshake and capability
// reads — plumbing, not tool use. bridge.identify binds the connection (so it
// is itself the first request seen as a bridge); handshake can only reach here
// if a client identified as both, which it never does; cowork.threadState is
// how the Cowork bridge decides whether it has any tools to advertise, asked
// once per tools/list and never on the agent's behalf.
var mcpQuietMethods = map[string]bool{
	"bridge.identify":    true,
	"handshake":          true,
	"cowork.threadState": true,
}

// mcpToolFor names the tool behind an RPC method, falling back to the method.
func mcpToolFor(method string) string {
	if tool, ok := mcpToolNames[method]; ok {
		return tool
	}
	return method
}

// capText trims a value to one line and caps it, so no summary can carry a
// document, a prompt body or a multi-line secret into the feed.
func capText(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " "))
	if len(s) > mcpSummaryCap {
		// Cut on a rune boundary so the summary stays valid UTF-8.
		cut := mcpSummaryCap
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		return strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}

// utf8Start reports whether b can begin a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// firstLine keeps only the opening line of a free-text argument (a note, a
// message): the feed shows the gist, the transcript holds the whole thing.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// mcpArgsSummary is the per-tool digest of a bridge request's params. The
// params are the RPC's, not the MCP tool's, so the field names here are the
// core-side ones (mcp.go builds them from the agent's snake_case arguments).
func mcpArgsSummary(tool string, raw json.RawMessage) string {
	var p struct {
		Path           string            `json:"path"`
		Text           string            `json:"text"`
		Summary        string            `json:"summary"`
		ThreadID       string            `json:"threadId"`
		Backend        string            `json:"backend"`
		Model          string            `json:"model"`
		Title          string            `json:"title"`
		ToolName       string            `json:"toolName"`
		Reason         string            `json:"reason"`
		ElementID      string            `json:"elementId"`
		Action         string            `json:"action"`
		Button         string            `json:"button"`
		Name           string            `json:"name"`
		TargetWindowID string            `json:"targetWindowId"`
		Events         []json.RawMessage `json:"events"`
		X              int               `json:"x"`
		Y              int               `json:"y"`
		DX             int               `json:"dx"`
		DY             int               `json:"dy"`
		FromX          int               `json:"fromX"`
		FromY          int               `json:"fromY"`
		ToX            int               `json:"toX"`
		ToY            int               `json:"toY"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}

	switch tool {
	case "post_note":
		return capText(firstLine(p.Text))
	case "claim_file", "release_file":
		return capText(p.Path)
	case "request_review":
		return capText(firstLine(p.Summary))
	case "launch_agent":
		engine := strings.Trim(capText(p.Backend)+"/"+capText(p.Model), "/")
		if engine == "" {
			engine = "(caller's engine)"
		}
		if title := capText(p.Title); title != "" {
			return capText(engine + ": " + title)
		}
		return engine
	case "send_agent":
		// The target plus the message's first line — never the whole body.
		return capText(p.ThreadID + ": " + firstLine(p.Text))
	case "wait_agent", "close_agent", "discard_agent":
		return capText(p.ThreadID)
	case "enable_cowork":
		// The stated reason is the point of the entry — it is what the human
		// was shown when they approved (or refused) desktop access.
		return capText(firstLine(p.Reason))
	case "request_permission":
		// Name the tool being gated, never its input: a Bash command line or an
		// API argument is exactly the kind of thing that carries secrets.
		return capText(p.ToolName)
	case "desktop_activate_element":
		return capText(strings.TrimSpace(p.ElementID + " " + p.Action))
	case "desktop_set_text":
		// The text itself may be a password — the element is all the feed gets.
		return capText(p.ElementID)
	case "desktop_click_element":
		return capText(strings.TrimSpace(p.ElementID + " " + p.Button))
	case "desktop_list_elements", "desktop_read_text":
		return capText(p.TargetWindowID)
	case "desktop_open_browser":
		return capText(p.Name)
	case "desktop_click", "desktop_move_pointer":
		return fmt.Sprintf("%d,%d", p.X, p.Y)
	case "desktop_move_pointer_relative", "desktop_scroll":
		return fmt.Sprintf("%+d,%+d", p.DX, p.DY)
	case "desktop_drag":
		return fmt.Sprintf("%d,%d → %d,%d", p.FromX, p.FromY, p.ToX, p.ToY)
	case "desktop_inject_input", "desktop_play_input":
		return fmt.Sprintf("%d event(s)", len(p.Events))
	default:
		// Parameterless tools (read_notes, get_presence, list_agents…) and any
		// unmapped method: the name is the whole story, and dumping unknown
		// params is exactly how a secret would leak into the feed.
		return ""
	}
}

// mcpActivityParams builds one `mcp.activity` notification payload.
//
// `error` is a handler's own error string, and it is INSIDE the same redaction
// boundary as argsSummary: no handler a bridge can reach may echo a
// secret-bearing argument (a prompt or message body, a gated tool's input,
// text destined for a field) into the error it returns.
// TestBridgeErrorsCarryNoSecrets guards that for the reachable handlers.
func mcpActivityParams(threadID, method string, params json.RawMessage,
	dur time.Duration, errText string) map[string]any {
	tool := mcpToolFor(method)
	out := map[string]any{
		"threadId":    threadID,
		"tool":        tool,
		"argsSummary": mcpArgsSummary(tool, params),
		"durationMs":  dur.Milliseconds(),
		"ok":          errText == "",
	}
	if errText != "" {
		out["error"] = capText(errText)
	}
	return out
}

// registerMCPActivity wires the bridge identity handshake and the activity
// feed. They belong together: the feed can only attribute a call to a thread
// because the bridge bound its connection first.
func registerMCPActivity(d handlerDeps) {
	// bridge.identify tags the connection as an agent bridge for its thread,
	// once, at bridge startup. It is the ONE door to that identity (audit F13):
	// every other handler asserts an identity that already exists, so this is
	// the only place the secret has to be checked.
	//
	// The secret is the one akcore minted for THIS thread's launch and passed to
	// its bridges in the environment (bridgeauth.go). It is verified before the
	// binding, and a failure is refused without saying which half was wrong — a
	// caller that can name a thread learns nothing about its secret, and one
	// holding a stale secret learns nothing about the thread.
	d.srv.Handle("bridge.identify", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			ThreadID string `json:"threadId"`
			Secret   string `json:"secret"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if p.ThreadID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "threadId is required")
		}
		// Fail closed: no ledger, no secret, a stale secret, an unknown thread,
		// a caller with no connection identity — and a secret whose bridge is
		// already connected (the replay gate) — are all the same refusal. Note
		// this is checked BEFORE the role checks in IdentifyBridge, so a caller
		// cannot use the reason text to probe which threads have live bridges.
		//
		// redeem CLAIMS the secret for this connection, so the check and the
		// binding cannot drift: a second connection replaying a live bridge's
		// secret is turned away here, and a bridge whose engine respawned it
		// (its old connection dropped) walks straight back into its own slot.
		caller := connIdentityOf(ipc.ConnFromContext(ctx))
		if !d.bridgeSecrets.redeem(p.ThreadID, p.Secret, caller) {
			// An EMPTY secret is called out separately: it is what a legitimate
			// bridge presents when the engine dropped the environment we launched
			// it with, and that diagnosis is otherwise a long hunt through a CLI
			// that simply has no cooperation tools.
			why := "the secret did not match"
			switch {
			case strings.TrimSpace(p.Secret) == "":
				why = "no secret was presented — if this is a real bridge, its " +
					"engine did not forward the environment it was launched with"
			case d.bridgeSecrets.heldElsewhere(p.ThreadID, p.Secret, caller):
				// The one refusal that can hit a legitimate bridge. Named in the
				// log (never in the reply) so a lockout is diagnosable rather
				// than indistinguishable from a dropped environment.
				why = "that secret is already redeemed by a live connection — " +
					"either this bridge's predecessor has not disconnected yet, " +
					"or another process redeemed it first"
			}
			d.log.Warn("refused a bridge identity", "thread", p.ThreadID, "why", why)
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"bridge identity refused: this connection did not present the "+
					"secret Agent Kate issued to that thread's bridge")
		}
		if ok, reason := d.srv.IdentifyBridge(ctx, p.ThreadID); !ok {
			// The binding failed AFTER the claim (a UI connection, or one
			// already bound to another thread). Give the slot back: a refused
			// caller must not be able to park the secret its real bridge needs,
			// which would turn a rejected forgery into a denial of service.
			d.bridgeSecrets.release(p.ThreadID, p.Secret, caller)
			return nil, ipc.Errorf(ipc.CodeInvalidParams, reason)
		}
		// Observability only — deliberately NOT a rejection. A bridge can
		// identify before its thread's record is persisted (the spawn and the
		// record write race at startup), so refusing an unknown id would break
		// legitimate launches. A warning is enough to notice a bridge binding
		// an id the core has never heard of.
		if d.sessions != nil {
			if _, known := d.sessions.Get(p.ThreadID); !known {
				d.log.Warn("mcp bridge identified as an unknown thread",
					"thread", p.ThreadID)
			}
		}
		return map[string]any{"ok": true}, nil
	})

	d.srv.OnBridgeActivity(func(threadID, method string, params json.RawMessage,
		dur time.Duration, errText string) {
		if mcpQuietMethods[method] {
			return
		}
		d.srv.NotifyUI("mcp.activity",
			mcpActivityParams(threadID, method, params, dur, errText))
	})
}
