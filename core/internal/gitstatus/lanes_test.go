// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

import (
	"reflect"
	"testing"
)

// Each case below feeds a hand-built topology to assignLanes and checks the
// exact lane layout. The graph drawings in the comments use rows from newest
// (top) to oldest (bottom), matching the order assignLanes consumes.

func TestAssignLanes_Linear(t *testing.T) {
	// A → B → C → D
	nodes := []CommitNode{
		{SHA: "A", Parents: []string{"B"}},
		{SHA: "B", Parents: []string{"C"}},
		{SHA: "C", Parents: []string{"D"}},
		{SHA: "D"},
	}
	want := []laneRow{
		{Lane: 0, LanesIn: nil, LanesOut: []int{0}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: []int{0}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: []int{0}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: nil},
	}
	if got := assignLanes(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("Linear mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAssignLanes_SimpleMerge(t *testing.T) {
	// A is a merge of C1 and C2, both branched from D.
	//   A
	//  /|
	// C1 C2
	//  \|
	//   D
	nodes := []CommitNode{
		{SHA: "A", Parents: []string{"C1", "C2"}},
		{SHA: "C1", Parents: []string{"D"}},
		{SHA: "C2", Parents: []string{"D"}},
		{SHA: "D"},
	}
	want := []laneRow{
		{Lane: 0, LanesIn: nil, LanesOut: []int{0, 1}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: []int{0}},
		{Lane: 1, LanesIn: []int{1}, LanesOut: []int{0}}, // C2 merges back into lane 0
		{Lane: 0, LanesIn: []int{0}, LanesOut: nil},
	}
	if got := assignLanes(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("SimpleMerge mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAssignLanes_Octopus(t *testing.T) {
	// M is an octopus merge of P1, P2, P3 — three unrelated roots.
	nodes := []CommitNode{
		{SHA: "M", Parents: []string{"P1", "P2", "P3"}},
		{SHA: "P1"},
		{SHA: "P2"},
		{SHA: "P3"},
	}
	want := []laneRow{
		{Lane: 0, LanesIn: nil, LanesOut: []int{0, 1, 2}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: nil},
		{Lane: 1, LanesIn: []int{1}, LanesOut: nil},
		{Lane: 2, LanesIn: []int{2}, LanesOut: nil},
	}
	if got := assignLanes(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("Octopus mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAssignLanes_CrissCross(t *testing.T) {
	// X is a merge of A and B; A and B both merge C and D; C and D both have
	// E as their only parent. The criss-cross shape is the classic case where
	// re-using existing lanes (rather than opening fresh slots) matters.
	//      X
	//     /|
	//    A B
	//    |X|
	//    C D
	//     \|
	//      E
	nodes := []CommitNode{
		{SHA: "X", Parents: []string{"A", "B"}},
		{SHA: "A", Parents: []string{"C", "D"}},
		{SHA: "B", Parents: []string{"C", "D"}},
		{SHA: "C", Parents: []string{"E"}},
		{SHA: "D", Parents: []string{"E"}},
		{SHA: "E"},
	}
	want := []laneRow{
		{Lane: 0, LanesIn: nil, LanesOut: []int{0, 1}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: []int{0, 2}},
		{Lane: 1, LanesIn: []int{1}, LanesOut: []int{0, 2}}, // reuses lanes
		{Lane: 0, LanesIn: []int{0}, LanesOut: []int{0}},
		{Lane: 2, LanesIn: []int{2}, LanesOut: []int{0}}, // D merges back to lane 0
		{Lane: 0, LanesIn: []int{0}, LanesOut: nil},
	}
	if got := assignLanes(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("CrissCross mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestAssignLanes_LaneReuse(t *testing.T) {
	// Two unrelated chains in sequence — the second tip should reuse lane 0
	// once the first chain closes, not open lane 1.
	nodes := []CommitNode{
		{SHA: "T1", Parents: []string{"R1"}},
		{SHA: "R1"},
		{SHA: "T2", Parents: []string{"R2"}},
		{SHA: "R2"},
	}
	want := []laneRow{
		{Lane: 0, LanesIn: nil, LanesOut: []int{0}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: nil},
		{Lane: 0, LanesIn: nil, LanesOut: []int{0}},
		{Lane: 0, LanesIn: []int{0}, LanesOut: nil},
	}
	if got := assignLanes(nodes); !reflect.DeepEqual(got, want) {
		t.Fatalf("LaneReuse mismatch\n got: %+v\nwant: %+v", got, want)
	}
}
