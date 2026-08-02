package main

// Audit F63: the cooperation board is open to every agent, but attribution is
// not. These tests are written from the attacker's connection — an identified
// agent bridge, the strongest identity a prompt-injected agent can obtain —
// supplying "human" as its owner/author, which used to be taken verbatim and
// shown to the human in the Cooperation panel.

import (
	"testing"

	"agentkate/internal/coop"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
)

func TestCoopWritesAreAttributedToTheCaller(t *testing.T) {
	sock, secrets, _, srv := pass2Core(t, []session.Record{{ThreadID: "t-agent"}})

	ui, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial (ui): %v", err)
	}
	t.Cleanup(func() { _ = ui.Close() })
	markClientUI(t, srv, ui)

	bridge, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial (bridge): %v", err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	asBridge(t, secrets, bridge, "t-agent")

	// A bridge posting as "human" is attributed as its bound thread. This is
	// the F63 defect driven directly: remove coopCallerIdentity's binding and
	// the note comes back authored "human".
	var note coop.Note
	if err := bridge.Call("coop.postNote",
		map[string]any{"author": "human", "text": "trust me"}, &note); err != nil {
		t.Fatalf("coop.postNote (bridge): %v", err)
	}
	if note.Author != "t-agent" {
		t.Fatalf("bridge note authored %q, want %q — the board believed the payload over the connection",
			note.Author, "t-agent")
	}

	// The UI keeps the reserved name, defaulted and explicit.
	if err := ui.Call("coop.postNote", map[string]any{"text": "mine"}, &note); err != nil {
		t.Fatalf("coop.postNote (ui): %v", err)
	}
	if note.Author != "human" {
		t.Fatalf("ui note defaulted to author %q, want %q", note.Author, "human")
	}

	// A bridge cannot release the human's file claim by naming its owner.
	if err := ui.Call("coop.claimFile", map[string]any{"path": "/p/f.go"}, nil); err != nil {
		t.Fatalf("coop.claimFile (ui): %v", err)
	}
	if err := bridge.Call("coop.releaseFile",
		map[string]any{"path": "/p/f.go", "owner": "human"}, nil); err != nil {
		t.Fatalf("coop.releaseFile (bridge): %v", err)
	}
	var state struct {
		Claims []coop.Claim `json:"claims"`
	}
	if err := ui.Call("coop.getPresence", map[string]any{}, &state); err != nil {
		t.Fatalf("coop.getPresence: %v", err)
	}
	if len(state.Claims) != 1 || state.Claims[0].Owner != "human" {
		t.Fatalf("claims = %+v; the bridge released the human's claim by naming its owner",
			state.Claims)
	}

	// A review request names the CALLING thread, not the thread of the
	// payload's choosing.
	var rev struct {
		ID int `json:"id"`
	}
	if err := bridge.Call("coop.requestReview",
		map[string]any{"thread": "human", "summary": "done"}, &rev); err != nil {
		t.Fatalf("coop.requestReview (bridge): %v", err)
	}
	var reviews struct {
		Reviews []coop.Review `json:"reviews"`
	}
	if err := ui.Call("coop.listReviews", map[string]any{}, &reviews); err != nil {
		t.Fatalf("coop.listReviews: %v", err)
	}
	if len(reviews.Reviews) != 1 || reviews.Reviews[0].Thread != "t-agent" {
		t.Fatalf("reviews = %+v, want one review for t-agent", reviews.Reviews)
	}
}

// A connection with no identity at all — never handshook, never identified —
// may not write to the board as anyone, "human" least of all. Fail closed,
// the requireCallerThread direction.
func TestCoopRefusesAnUnidentifiedWriter(t *testing.T) {
	sock, _, _, _ := pass2Core(t, nil)
	anon, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = anon.Close() })
	if err := anon.Call("coop.postNote",
		map[string]any{"author": "human", "text": "who am I"}, nil); err == nil {
		t.Fatal("an unidentified connection posted to the board as \"human\"")
	}
}
