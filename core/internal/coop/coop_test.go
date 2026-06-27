package coop

import "testing"

func TestClaims(t *testing.T) {
	s := NewState()

	if ok, _ := s.ClaimFile("a.go", "t-1"); !ok {
		t.Fatal("first claim should succeed")
	}
	// Same owner re-claiming is fine.
	if ok, _ := s.ClaimFile("a.go", "t-1"); !ok {
		t.Fatal("re-claim by the same owner should succeed")
	}
	// A different owner is blocked and told the holder.
	ok, holder := s.ClaimFile("a.go", "t-2")
	if ok || holder != "t-1" {
		t.Fatalf("conflicting claim: ok=%v holder=%q, want false/t-1", ok, holder)
	}
	// After release, the file is free.
	s.ReleaseFile("a.go", "t-1")
	if ok, _ := s.ClaimFile("a.go", "t-2"); !ok {
		t.Fatal("claim after release should succeed")
	}
	if claims := s.ListClaims(); len(claims) != 1 || claims[0].Owner != "t-2" {
		t.Fatalf("ListClaims = %+v, want one claim by t-2", claims)
	}
}

func TestPresenceAndReviews(t *testing.T) {
	s := NewState()

	s.SetPresence("human", "src/main.cpp")
	s.SetPresence("t-1", "src/agent.go")
	if p := s.ListPresence(); len(p) != 2 {
		t.Fatalf("ListPresence len = %d, want 2", len(p))
	}

	r := s.AddReview("t-1", "Refactored the parser")
	if r.ID != 1 || r.Thread != "t-1" {
		t.Fatalf("AddReview = %+v", r)
	}
	if reviews := s.ListReviews(); len(reviews) != 1 {
		t.Fatalf("ListReviews len = %d, want 1", len(reviews))
	}
}

func TestClearOwner(t *testing.T) {
	s := NewState()
	s.SetPresence("t-1", "x.go")
	s.ClaimFile("x.go", "t-1")
	s.SetOpenFiles("t-1", []string{"x.go"})

	s.ClearOwner("t-1")

	if len(s.ListPresence()) != 0 || len(s.ListClaims()) != 0 || len(s.ListOpenFiles()) != 0 {
		t.Fatal("ClearOwner should drop the owner's presence, claims and open files")
	}
}

func TestBoardEviction(t *testing.T) {
	s := NewState()

	// Post well past the cap; the board must retain only the most recent
	// maxBoardEntries entries, evicting oldest-first.
	const n = maxBoardEntries + 250
	for i := 0; i < n; i++ {
		s.PostNote("t-1", "note")
	}
	notes := s.ReadNotes()
	if len(notes) != maxBoardEntries {
		t.Fatalf("ReadNotes len = %d, want %d", len(notes), maxBoardEntries)
	}
	// IDs keep climbing across eviction: the oldest survivor is the
	// (n-cap+1)'th note posted, and the newest is the n'th.
	if got, want := notes[0].ID, n-maxBoardEntries+1; got != want {
		t.Fatalf("oldest retained note ID = %d, want %d", got, want)
	}
	if got := notes[len(notes)-1].ID; got != n {
		t.Fatalf("newest note ID = %d, want %d", got, n)
	}

	// Reviews share the same cap and eviction behaviour.
	for i := 0; i < n; i++ {
		s.AddReview("t-1", "summary")
	}
	reviews := s.ListReviews()
	if len(reviews) != maxBoardEntries {
		t.Fatalf("ListReviews len = %d, want %d", len(reviews), maxBoardEntries)
	}
	if got, want := reviews[0].ID, n-maxBoardEntries+1; got != want {
		t.Fatalf("oldest retained review ID = %d, want %d", got, want)
	}
}
