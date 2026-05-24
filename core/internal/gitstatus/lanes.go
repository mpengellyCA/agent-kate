// SPDX-License-Identifier: LGPL-2.0-or-later
// SPDX-FileCopyrightText: 2026 The AgentKate developers

package gitstatus

// CommitNode is the topology-only view of a commit that assignLanes needs.
// Keeping it tiny — and free of any go-git import — lets lanes_test.go
// exercise the allocator on synthetic graphs.
type CommitNode struct {
	SHA     string
	Parents []string
}

// laneRow is the rail layout for one commit row:
//
//	Lane     — column the commit's node sits in
//	LanesIn  — columns of edges arriving from rows above (children drawn earlier)
//	LanesOut — columns of edges leaving toward rows below (parents to draw later)
//
// LanesIn / LanesOut deliberately include Lane itself when it carries an edge,
// so the delegate can draw a "straight through" edge without special-casing.
type laneRow struct {
	Lane     int
	LanesIn  []int
	LanesOut []int
}

// assignLanes lays a topologically ordered (newest-first) commit list out into
// horizontal lanes for a graph rail. It's the standard algorithm used by gitg,
// lazygit, and tig: the first parent inherits its child's lane; additional
// parents claim the leftmost free lane (or extend the array); a lane closes
// the moment no live row still expects it.
//
// The caller must pass commits in --topo-order (parents strictly after
// children). go-git's Log iterator in TopoOrder satisfies this.
func assignLanes(nodes []CommitNode) []laneRow {
	rows := make([]laneRow, len(nodes))
	var lanes []string // index -> SHA the lane is currently routing toward

	for i, n := range nodes {
		// 1. Collect lanes that route into this commit (any lane expecting it).
		var in []int
		for li, sha := range lanes {
			if sha == n.SHA {
				in = append(in, li)
			}
		}
		var lane int
		if len(in) == 0 {
			// Fresh tip — claim the leftmost free lane, else append.
			lane = firstEmptyOrAppend(&lanes, "")
		} else {
			lane = in[0]
			for _, li := range in {
				lanes[li] = ""
			}
		}

		// 2. Place parents. Dedupe so a commit with the same parent listed
		//    twice (rare but legal in malformed history) doesn't double-draw,
		//    and reuse any lane that already routes toward the parent
		//    (criss-cross merges).
		var out []int
		placed := make(map[string]int)
		for pi, p := range n.Parents {
			if _, dup := placed[p]; dup {
				continue
			}
			existing := -1
			for li, sha := range lanes {
				if sha == p {
					existing = li
					break
				}
			}
			if existing >= 0 {
				placed[p] = existing
				out = append(out, existing)
				continue
			}
			var target int
			if pi == 0 && lane < len(lanes) && lanes[lane] == "" {
				target = lane
				lanes[target] = p
			} else {
				target = firstEmptyOrAppend(&lanes, p)
			}
			placed[p] = target
			out = append(out, target)
		}

		// Trim trailing empties so the rail width stays bounded by the
		// in-flight branch count.
		for len(lanes) > 0 && lanes[len(lanes)-1] == "" {
			lanes = lanes[:len(lanes)-1]
		}

		rows[i] = laneRow{Lane: lane, LanesIn: in, LanesOut: out}
	}
	return rows
}

func firstEmptyOrAppend(lanes *[]string, sha string) int {
	for i, s := range *lanes {
		if s == "" {
			(*lanes)[i] = sha
			return i
		}
	}
	*lanes = append(*lanes, sha)
	return len(*lanes) - 1
}
