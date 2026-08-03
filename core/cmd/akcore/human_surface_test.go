package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/permission"
	"agentkate/internal/session"
)

func TestRemoteHumanPrincipalHasOnlyRemoteHumanActions(t *testing.T) {
	p := humanPrincipal{Surface: remoteSurface, Device: "Pixel"}
	for _, action := range []string{"archive", "discard", "settings", "cowork.respondGrant"} {
		if p.may(action) {
			t.Fatalf("remote principal unexpectedly may %q", action)
		}
	}
	for _, action := range []string{"send", "interrupt", "stop", "permission.respond"} {
		if !p.may(action) {
			t.Fatalf("remote principal should be allowed %q", action)
		}
	}
}

func TestDesktopHumanPrincipalCanUseTheCanonicalSendSurface(t *testing.T) {
	if !desktopPrincipal().may("send") {
		t.Fatal("desktop human principal unexpectedly may not send")
	}
}

type humanSendHarness struct {
	*fakeHarness
	sent [][]agent.Attachment
	text []string
}

func (h *humanSendHarness) Send(_ string, text string, atts []agent.Attachment) error {
	h.text = append(h.text, text)
	h.sent = append(h.sent, append([]agent.Attachment(nil), atts...))
	return nil
}

func (h *humanSendHarness) Running(string) bool { return true }

func TestRemoteQueuedSendUsesOneBusyEdgeAndTypedEcho(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "threads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(session.Record{
		ThreadID: "thread-1", Backend: "fake", SessionID: "session-1",
		AgentRef: harness.AgentRef{ThreadID: "thread-1", HarnessID: "fake"},
	}); err != nil {
		t.Fatal(err)
	}
	h := &humanSendHarness{fakeHarness: &fakeHarness{}}
	registry := harness.NewRegistry("fake")
	registry.Register(h)
	turns := agent.NewTurnTracker()
	queue := newHumanSendQueue()
	remoteCtl := newRemoteControl(t.Context(), nil)
	var d handlerDeps
	d = handlerDeps{sessions: store, harnesses: registry, turns: turns, humanQueue: queue, remote: remoteCtl}
	turns.SetOnChange(func(threadID string, busy bool) {
		if !busy {
			queue.drainOne(d, threadID)
		}
	})

	turns.TurnQueued("thread-1")
	result, err := d.humanSend(humanPrincipal{Surface: remoteSurface, Device: "Pixel"}, "thread-1", "next step", []agent.Attachment{{Kind: "text", Name: "note.txt", MediaType: "text/plain", Text: "safe"}})
	if err != nil || !result.Queued || result.Position != 1 {
		t.Fatalf("queue result = %#v, %v", result, err)
	}
	if len(h.text) != 0 {
		t.Fatalf("remote follow-up interleaved a live turn: %#v", h.text)
	}
	if echoes := remoteCtl.mergeHumanEchoes("thread-1", nil); len(echoes) != 1 || echoes[0].Text != "next step" {
		t.Fatalf("accepted remote send has no typed echo: %#v", echoes)
	}

	// This is the same TurnTracker edge used in run.go: a single queued prompt
	// is delivered, marks the new turn busy, and leaves later prompts for its
	// next terminal edge.
	turns.TurnFailed("thread-1")
	if len(h.text) != 1 || h.text[0] != "next step" || !turns.Busy("thread-1") {
		t.Fatalf("busy-edge delivery = text %#v busy=%v", h.text, turns.Busy("thread-1"))
	}
}

func TestRemotePermissionResponseCannotResolveDesktopOnlyRequest(t *testing.T) {
	b := permission.New()
	requestID, _ := b.OpenLocal()
	d := handlerDeps{broker: b}
	if d.humanRespondPermission(humanPrincipal{Surface: remoteSurface, Device: "Pixel"}, requestID, true, nil) {
		t.Fatal("remote device resolved a desktop-only request")
	}
	if _, ok := b.Get(requestID); !ok {
		t.Fatal("desktop-only request was consumed")
	}
}

func TestRemotePermissionResponseOnlyAllowsQuestionAnswers(t *testing.T) {
	b := permission.New()
	d := handlerDeps{broker: b}
	p := humanPrincipal{Surface: remoteSurface, Device: "Pixel"}

	normal, _ := b.Open("thread-a", "Bash", "Approve a shell command", time.Minute)
	if d.humanRespondPermission(p, normal.ID, true, json.RawMessage(`{"command":"echo secret"}`)) {
		t.Fatal("remote device supplied updated input for a normal tool")
	}
	if _, ok := b.Get(normal.ID); !ok {
		t.Fatal("rejected normal-tool response consumed request")
	}

	questions := json.RawMessage(`[{"question":"Deploy?","options":["yes","no"]}]`)
	question, answerCh := b.OpenWithDetail("thread-a", "AskUserQuestion", "Answer the agent's question", permission.Detail{Questions: questions}, time.Minute)
	wrong := json.RawMessage(`{"questions":[{"question":"Other?"}],"answers":["yes"]}`)
	if d.humanRespondPermission(p, question.ID, true, wrong) {
		t.Fatal("remote device changed the question payload")
	}
	answer := json.RawMessage(`{"questions":[{"question":"Deploy?","options":["yes","no"]}],"answers":["yes"]}`)
	if !d.humanRespondPermission(p, question.ID, true, answer) {
		t.Fatal("matching AskUserQuestion answer was rejected")
	}
	select {
	case got := <-answerCh:
		if !got.Allow || string(got.UpdatedInput) != string(answer) || got.ResolvedBy != "remote:Pixel" {
			t.Fatalf("unexpected delivered remote decision: %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("matching response was not delivered")
	}
}

func TestDesktopPermissionResponseKeepsLocalUpdatedInput(t *testing.T) {
	b := permission.New()
	d := handlerDeps{broker: b}
	request, answerCh := b.Open("thread-a", "Bash", "Approve a shell command", time.Minute)
	updated := json.RawMessage(`{"command":"echo local edit"}`)
	if !d.humanRespondPermission(desktopPrincipal(), request.ID, true, updated) {
		t.Fatal("desktop response was rejected")
	}
	got := <-answerCh
	if got.ResolvedBy != "desktop" || string(got.UpdatedInput) != string(updated) {
		t.Fatalf("desktop decision changed: %#v", got)
	}
}
