package main

import (
	"encoding/json"
	"fmt"
	"reflect"

	"agentkate/internal/agent"
	"agentkate/internal/permission"
)

// humanSurface names the authority that initiated an operation. It is not an
// IPC role: a remote device is deliberately never treated as a UI window.
type humanSurface string

const (
	desktopSurface humanSurface = "desktop"
	remoteSurface  humanSurface = "remote"
)

// humanPrincipal is constructed by the trusted adapter, never decoded from an
// action request. Remote identity is an immutable HTTPS-session attribute.
type humanPrincipal struct {
	Surface humanSurface
	Device  string
}

func desktopPrincipal() humanPrincipal { return humanPrincipal{Surface: desktopSurface} }

func (p humanPrincipal) resolvedBy() string {
	if p.Surface != remoteSurface {
		return "desktop"
	}
	if p.Device == "" {
		return "remote"
	}
	return "remote:" + p.Device
}

func (p humanPrincipal) may(action string) bool {
	if p.Surface == desktopSurface {
		return true
	}
	switch action {
	case "interrupt", "stop", "permission.respond":
		return p.Surface == remoteSurface
	default:
		return false
	}
}

// humanSend is the canonical desktop-human send operation. The HTTPS remote
// principal is deliberately denied until a core-owned queue and one canonical
// desktop/remote transcript echo exist. Agent bridges do not enter here: their
// caller binding and cross-subtree grant are established at the IPC entrypoint
// before they use the same accepted-send delivery primitive below.
func (d handlerDeps) humanSend(principal humanPrincipal, threadID, text string, atts []agent.Attachment) error {
	if !principal.may("send") {
		return fmt.Errorf("human surface may not send")
	}
	return d.deliverAcceptedSend(threadID, text, atts)
}

// deliverAcceptedSend is shared only after the entrypoint has established
// authority. It is intentionally not exported: calling it from a remote
// adapter would bypass the remote principal's denied send capability.
func (d handlerDeps) deliverAcceptedSend(threadID, text string, atts []agent.Attachment) error {
	d.turns.TurnQueued(threadID)
	if err := d.agentSend(threadID, text, atts); err != nil {
		d.turns.TurnFailed(threadID)
		return err
	}
	// The sidecar stores only attachment display metadata for desktop replay;
	// raw attachment bodies never join a remote projection.
	recordAttachments(d, threadID, text, atts)
	return nil
}

// humanInterrupt is the one implementation of an Esc-like human interrupt.
// UI-gated IPC and authenticated remote requests both enter here; agent bridges
// do not, so their caller binding remains exactly where it is today.
func (d handlerDeps) humanInterrupt(principal humanPrincipal, threadID string) error {
	if !principal.may("interrupt") {
		return fmt.Errorf("human surface may not interrupt")
	}
	if d.broker != nil {
		d.broker.CancelThread(threadID, permission.Interrupted)
	}
	return d.agentInterrupt(threadID)
}

// humanStop shares the desktop's graceful-stop sequencing without giving a
// remote adapter a direct harness bypass. It never archives or discards.
func (d handlerDeps) humanStop(principal humanPrincipal, threadID string) error {
	if !principal.may("stop") {
		return fmt.Errorf("human surface may not stop")
	}
	d.rateWakes.Cancel(threadID, "you stopped it")
	runHotCompactIfConfigured(d, threadID)
	return d.agentStop(threadID)
}

// humanRespondPermission is the one permission-response operation. The remote
// branch is intentionally narrower than desktop: it can answer a tool prompt,
// approve a plan, or return an AskUserQuestion payload, but cannot smuggle an
// arbitrary updatedInput into a normal tool call.
func (d handlerDeps) humanRespondPermission(principal humanPrincipal, requestID string, allow bool, updatedInput json.RawMessage) bool {
	if !principal.may("permission.respond") || d.broker == nil {
		return false
	}
	req, ok := d.broker.Get(requestID)
	if principal.Surface == remoteSurface {
		req, ok = d.broker.GetRemote(requestID)
	}
	if !ok {
		return false
	}
	if principal.Surface == remoteSurface && !remoteAnswerAllowed(req, updatedInput) {
		return false
	}
	_, delivered := d.broker.Resolve(requestID, permission.Decision{
		Allow: allow, UpdatedInput: updatedInput, ResolvedBy: principal.resolvedBy(),
	})
	return delivered
}

func remoteAnswerAllowed(req permission.Request, updatedInput json.RawMessage) bool {
	if len(updatedInput) == 0 {
		return true
	}
	if !json.Valid(updatedInput) {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(updatedInput, &object) != nil {
		return false
	}
	switch req.ToolName {
	case "AskUserQuestion":
		questions, ok := object["questions"]
		if !ok || !jsonEqual(questions, req.Questions) {
			return false
		}
		_, ok = object["answers"]
		return ok
	case "ExitPlanMode":
		// Plan approval carries no synthetic tool arguments. The stored plan is
		// render-only and never gets reflected into a different tool input.
		return false
	default:
		return false
	}
}

func jsonEqual(a, b json.RawMessage) bool {
	var av any
	var bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && reflect.DeepEqual(av, bv)
}
