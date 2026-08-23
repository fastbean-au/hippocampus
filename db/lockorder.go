package db

import "sort"

// Lock ordering.
//
// Several statements in this package mutate a SET of rows in one transaction, addressed by an id
// list: recall's UPDATE, spreading activation's UPDATE, the link-significance recompute, and the
// delete chokepoint's DELETE. A row-level lock is taken on each row as the statement reaches it, and
// two transactions that hold overlapping sets while taking them in DIFFERENT orders will deadlock -
// which Postgres resolves by killing one of them.
//
// The orders genuinely differed. Recall took its ids from the request, spreading activation from the
// link graph, and eviction from a scan sorted by computed value; nothing made them agree. Running
// the retention benchmark's writer against a Postgres store produced four deadlocks in four and a
// half minutes: one reached the client as codes.Internal, and one failed an eviction, leaving the
// store over its capacity target for that cycle.
//
// The fix is to give every such statement one global order, which any total order over the ids
// supplies. Sorting is not for tidiness here and is not optional - it is what makes the lock
// acquisition sequence identical between transactions that would otherwise interleave.
//
// SQLite cannot exhibit this (one writer, serialised), so the guard test is Postgres-gated, but the
// sorting is unconditional: it costs a sort of a chunk-sized slice and it is the invariant that has
// to hold everywhere for it to hold anywhere.

// lockOrderedIDs returns the ids sorted ascending, so every transaction locking a set of rows takes
// them in the same sequence. It copies rather than sorting in place: callers pass slices they did
// not necessarily build, and a caller-visible reordering is a side effect nobody asked for.
func lockOrderedIDs(ids []string) []string {
	out := make([]string, len(ids))
	copy(out, ids)

	sort.Strings(out)

	return out
}

// lockOrderedSnapshots is lockOrderedIDs for the delete chokepoint, which carries a recall-state
// snapshot alongside each id and must keep the two together.
func lockOrderedSnapshots(items []memoryRecallSnapshot) []memoryRecallSnapshot {
	out := make([]memoryRecallSnapshot, len(items))
	copy(out, items)

	sort.Slice(out, func(i int, j int) bool {

		return out[i].id < out[j].id
	})

	return out
}
