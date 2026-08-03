package main

import (
	"encoding/json"
	"testing"
	"time"

	"agentkate/internal/permission"
)

func TestRemoteHumanPrincipalHasOnlyRemoteHumanActions(t *testing.T) {
	p := humanPrincipal{Surface: remoteSurface, Device: "Pixel"}
	for _, action := range []string{"send", "archive", "discard", "settings", "cowork.respondGrant"} {
		if p.may(action) {
			t.Fatalf("remote principal unexpectedly may %q", action)
		}
	}
	for _, action := range []string{"interrupt", "stop", "permission.respond"} {
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
