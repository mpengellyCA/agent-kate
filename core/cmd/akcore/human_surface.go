package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/permission"
	"agentkate/internal/safe"
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
		return p.Surface == remoteSurface && action == "send"
	}
}

type humanSendResult struct {
	Queued   bool
	Position int
	Resuming bool
}

// humanSend is the canonical human-surface send operation. A remote device may
// queue a follow-up but cannot interleave a live turn. Desktop UI sends use the
// same echo path; agent bridges keep their caller binding at the IPC entrypoint
// and use the narrower delivery primitive below.
func (d handlerDeps) humanSend(principal humanPrincipal, threadID, text string, atts []agent.Attachment) (humanSendResult, error) {
	if !principal.may("send") {
		return humanSendResult{}, fmt.Errorf("human surface may not send")
	}
	if principal.Surface == remoteSurface && (d.harnesses == nil || !d.agentRunning(threadID)) {
		if err := d.canResumeHumanSend(threadID); err != nil {
			return humanSendResult{}, err
		}
		position, err := d.humanQueue.enqueue(threadID, text, atts)
		if err != nil {
			return humanSendResult{}, err
		}
		// The user turn is accepted before the asynchronous resume starts. The
		// typed echo makes that decision visible on both surfaces immediately;
		// emitLifecycle("resumed") drains exactly one queued turn once it is safe.
		d.publishAcceptedHumanSend(threadID, text, atts)
		if d.humanQueue.beginResume(threadID) {
			safe.Go("remote.resumeForHumanSend", func() {
				defer d.humanQueue.finishResume(threadID)
				d.resumeHumanSendThread(threadID)
			})
		}
		return humanSendResult{Queued: true, Position: position, Resuming: true}, nil
	}
	if principal.Surface == remoteSurface && d.turns != nil && d.turns.Busy(threadID) {
		position, err := d.humanQueue.enqueue(threadID, text, atts)
		if err != nil {
			return humanSendResult{}, err
		}
		// This is an accepted human turn. Echo it at acceptance, rather than at
		// later delivery, so every surface sees the same conversation order.
		d.publishAcceptedHumanSend(threadID, text, atts)
		return humanSendResult{Queued: true, Position: position}, nil
	}
	if err := d.deliverAcceptedHumanSend(threadID, text, atts, true); err != nil {
		return humanSendResult{}, err
	}
	return humanSendResult{}, nil
}

// canResumeHumanSend accepts only the persisted thread configuration. It does
// not take model, provider, worktree, permission-mode, or Cowork options from
// the remote request, preserving the UI-only authority gate on those choices.
func (d handlerDeps) canResumeHumanSend(threadID string) error {
	if d.sessions == nil || d.harnesses == nil || d.humanQueue == nil {
		return fmt.Errorf("remote send is unavailable while this core is starting")
	}
	rec, ok := d.sessions.Get(threadID)
	if !ok {
		return fmt.Errorf("unknown thread")
	}
	if _, ok := d.harnesses.Get(rec.Backend); !ok {
		return fmt.Errorf("the agent's harness is no longer registered")
	}
	if rec.SessionID == "" {
		return fmt.Errorf("the agent has no session to resume")
	}
	if _, err := resolveProviderBinding(rec.ProviderID); err != nil {
		return err
	}
	return nil
}

func (d handlerDeps) resumeHumanSendThread(threadID string) {
	if d.agentRunning(threadID) {
		return // a desktop Resume won the small race; its lifecycle will drain us.
	}
	rec, ok := d.sessions.Get(threadID)
	if !ok {
		return
	}
	h, ok := d.harnesses.Get(rec.Backend)
	if !ok {
		return
	}
	provider, err := resolveProviderBinding(rec.ProviderID)
	if err != nil {
		emitLifecycle(d, threadID, "error", err.Error(), &rec.Worktree)
		return
	}
	if d.rateWakes != nil {
		d.rateWakes.Cancel(threadID, "a paired device resumed it")
	}
	resumeThread(d, h, rec, provider)
}

// drainHumanSendAfterResume handles an unseeded resume. A summary-seeded
// resume is itself a real busy turn, so its ordinary busy->idle edge handles
// the queue later instead.
func drainHumanSendAfterResume(d handlerDeps, threadID string) {
	if d.humanQueue == nil || d.turns == nil || d.turns.Busy(threadID) {
		return
	}
	d.humanQueue.drainOne(d, threadID)
}

// deliverAcceptedSend is shared only after the entrypoint has established
// authority. Agent bridges call it after their binding/cross-subtree gates;
// calling it from a remote adapter would bypass the remote principal.
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

// deliverAcceptedHumanSend layers the canonical desktop + remote transcript
// echo on top of an already-authorised delivery. A queued turn was echoed when
// it entered the queue, so draining it passes echo=false.
func (d handlerDeps) deliverAcceptedHumanSend(threadID, text string, atts []agent.Attachment, echo bool) error {
	if err := d.deliverAcceptedSend(threadID, text, atts); err != nil {
		return err
	}
	if echo {
		d.publishAcceptedHumanSend(threadID, text, atts)
	}
	return nil
}

// publishAcceptedHumanSend is the only live user-turn fan-out. The desktop
// retains its UI-only raw event, while the HTTPS surface receives one typed,
// redacted DTO. Attachment bytes and paths are absent from both remote paths.
func (d handlerDeps) publishAcceptedHumanSend(threadID, text string, atts []agent.Attachment) {
	at := time.Now().UTC()
	remoteText := text
	if remoteText == "" && len(atts) > 0 {
		remoteText = fmt.Sprintf("Attached %d file(s)", len(atts))
	}
	metas := make([]map[string]string, 0, len(atts))
	for _, att := range atts {
		metas = append(metas, map[string]string{"name": att.Name, "kind": att.Kind, "mediaType": att.MediaType})
	}
	content := []map[string]string{{"type": "text", "text": remoteText}}
	raw, err := json.Marshal(map[string]any{
		"type": "user", "timestamp": at,
		"agentkateAcceptedHumanSend": true,
		"message":                    map[string]any{"role": "user", "content": content},
		"attachments":                metas,
	})
	if err == nil && d.srv != nil {
		d.srv.NotifyUI("agent.event", agentEventParams{ThreadID: threadID, Events: []json.RawMessage{raw}})
	}
	if d.remote != nil {
		d.remote.publishAcceptedHumanSend(threadID, remoteText, at, atts)
	}
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
