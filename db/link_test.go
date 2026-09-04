package db

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// The link graph's tests. The prune is the part most likely to be wrong - it runs inside every path
// that deletes a memory or an event, and a path that forgot it would leave the denormalised
// aggregate counting significance for something that no longer exists, quietly making the survivor
// harder to forget forever. So every delete path gets its own case here, by name.

// seedLinkedMemories creates n memories (m1..mn) with no event, for the link cases.
func seedLinkedMemories(t *testing.T, db *DB, n int) {
	t.Helper()

	for i := 1; i <= n; i++ {
		mustCreateMemory(t, db, types.Memory{
			Id:           memoryId(i),
			TimeStamp:    100,
			Significance: 1,
			Body:         "body",
		})
	}
}

func memoryId(i int) string {
	return "m" + string(rune('0'+i))
}

// linkSignificanceOfMemory reads a memory's stored aggregate directly, which is what the
// consolidation scans read and therefore what these tests assert on.
func linkSignificanceOfMemory(t *testing.T, db *DB, id string) int64 {
	t.Helper()

	var total int64

	err := db.sql.QueryRow(db.rebind(`SELECT link_significance FROM memories WHERE id = ?`), id).Scan(&total)
	if err != nil {
		t.Fatalf("read link_significance of %s: %s", id, err)
	}

	return total
}

func countLinkRows(t *testing.T, db *DB, table string) int {
	t.Helper()

	var n int

	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %s", table, err)
	}

	return n
}

// TestLinkMemoriesMaintainsAggregateOnBothEnds is the core invariant: value is symmetric even though
// storage is directed, so declaring a link raises the near end AND the far end.
func TestLinkMemoriesMaintainsAggregateOnBothEnds(t *testing.T) {
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 3)

	if err := db.LinkMemories(context.Background(), "m1", []types.Link{
		{Id: "m2", Significance: 3},
		{Id: "m3", Significance: 4},
	}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	// m1 declared both, so it holds their sum; each far end holds only its own.
	for id, want := range map[string]int64{"m1": 7, "m2": 3, "m3": 4} {
		if got := linkSignificanceOfMemory(t, db, id); got != want {
			t.Errorf("%s link_significance = %d, want %d", id, got, want)
		}
	}
}

// TestLinkMemoriesReLinkUpdatesRatherThanDuplicating pins the composite primary key doing its job:
// re-linking a pair re-weights that edge. Without it the aggregate would double on every re-link.
func TestLinkMemoriesReLinkUpdatesRatherThanDuplicating(t *testing.T) {
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	for _, significance := range []int32{3, 10} {
		if err := db.LinkMemories(context.Background(), "m1", []types.Link{{Id: "m2", Significance: significance}}); err != nil {
			t.Fatalf("LinkMemories(%d): %s", significance, err)
		}
	}

	if n := countLinkRows(t, db, memoryLinksTable); n != 1 {
		t.Errorf("expected one edge after a re-link, got %d", n)
	}

	if got := linkSignificanceOfMemory(t, db, "m1"); got != 10 {
		t.Errorf("re-link should re-weight, got link_significance %d, want 10", got)
	}
}

// TestUnlinkMemoriesIsSymmetric verifies unlinking removes the edge whichever end declared it -
// value is symmetric, so a caller asking to unlink A from B does not mean "only if I declared it".
func TestUnlinkMemoriesIsSymmetric(t *testing.T) {
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	if err := db.LinkMemories(context.Background(), "m1", []types.Link{{Id: "m2", Significance: 5}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	// Unlink from the end that did NOT declare the edge.
	if err := db.UnlinkMemories(context.Background(), "m2", []string{"m1"}); err != nil {
		t.Fatalf("UnlinkMemories: %s", err)
	}

	if n := countLinkRows(t, db, memoryLinksTable); n != 0 {
		t.Errorf("expected the edge removed from either end, %d rows remain", n)
	}

	for _, id := range []string{"m1", "m2"} {
		if got := linkSignificanceOfMemory(t, db, id); got != 0 {
			t.Errorf("%s link_significance = %d after unlink, want 0", id, got)
		}
	}
}

// TestPruneOnEveryDeletePath is the guard the whole feature rests on: whichever way a memory
// leaves the store, its links go with it and the surviving end's aggregate drops. A path that
// forgot to prune would leave the survivor counting significance from something gone.
func TestPruneOnEveryDeletePath(t *testing.T) {
	ctx := context.Background()

	// Each case links m1 -> m2, deletes m1 by its own route, and expects m2 left with nothing.
	cases := []struct {
		name   string
		delete func(t *testing.T, db *DB)
	}{
		{
			name: "DeleteMemories",
			delete: func(t *testing.T, db *DB) {
				if _, err := db.DeleteMemories(ctx, []string{"m1"}); err != nil {
					t.Fatalf("DeleteMemories: %s", err)
				}
			},
		},
		{
			name: "DeleteMemory",
			delete: func(t *testing.T, db *DB) {
				if err := db.DeleteMemory(ctx, "m1"); err != nil {
					t.Fatalf("DeleteMemory: %s", err)
				}
			},
		},
		{
			// The consolidation/eviction/clear chokepoint.
			name: "deleteMemoriesIfUnrecalled",
			delete: func(t *testing.T, db *DB) {
				if _, err := db.deleteMemoriesIfUnrecalled(ctx, []memoryRecallSnapshot{{id: "m1"}}, forgetReason{}); err != nil {
					t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
				}
			},
		},
		{
			name: "ConsolidateMemories",
			delete: func(t *testing.T, db *DB) {
				server := &decisionServer{memory: func(c MemoryConsolidationCandidate) bool {
					// Only m1: m2 must survive to have its aggregate checked.
					return c.MemoryLinkSignificance == 5 && c.Timestamp == 100
				}}

				if _, err := db.ConsolidateMemories(ctx, server); err != nil {
					t.Fatalf("ConsolidateMemories: %s", err)
				}
			},
		},
		{
			name: "EvictMemories",
			delete: func(t *testing.T, db *DB) {
				// Rank m1 least valuable so it is the one evicted.
				server := &decisionServer{value: func(c MemoryConsolidationCandidate) float64 {
					if c.Timestamp == 100 {
						return 0
					}

					return 100
				}}

				if _, _, _, err := db.EvictMemories(ctx, server, 1); err != nil {
					t.Fatalf("EvictMemories: %s", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t)
			t.Cleanup(func() { _ = db.Close() })

			// m1 at timestamp 100, m2 at 200, so the consolidation/eviction deciders can tell them
			// apart without reading bodies.
			mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "one"})
			mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 200, Significance: 1, Body: "two"})

			if err := db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 5}}); err != nil {
				t.Fatalf("LinkMemories: %s", err)
			}

			if got := linkSignificanceOfMemory(t, db, "m2"); got != 5 {
				t.Fatalf("precondition: m2 link_significance = %d, want 5", got)
			}

			c.delete(t, db)

			if n := countLinkRows(t, db, memoryLinksTable); n != 0 {
				t.Errorf("%s left %d link row(s) behind - the graph can now dangle", c.name, n)
			}

			if got := linkSignificanceOfMemory(t, db, "m2"); got != 0 {
				t.Errorf("%s left m2 counting %d significance from a deleted memory", c.name, got)
			}
		})
	}
}

// TestPruneOnEventMemoryDeletePaths covers the two paths that delete by event_id and so never learn
// the memory ids themselves - they have to read them first, which is exactly the step easiest to
// omit.
func TestPruneOnEventMemoryDeletePaths(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		delete func(t *testing.T, db *DB)
	}{
		{
			name: "DeleteEventMemories",
			delete: func(t *testing.T, db *DB) {
				if _, err := db.DeleteEventMemories(ctx, "e1"); err != nil {
					t.Fatalf("DeleteEventMemories: %s", err)
				}
			},
		},
		{
			name: "ReplaceMemoriesWithSummary",
			delete: func(t *testing.T, db *DB) {
				summary := types.Memory{Id: "sum", TimeStamp: 300, Significance: 5, EventId: "e1", Body: "summary", IsSummary: true}

				if _, err := db.ReplaceMemoriesWithSummary(ctx, "e1", summary); err != nil {
					t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t)
			t.Cleanup(func() { _ = db.Close() })

			mustCreateEvent(t, db, types.Event{Id: "e1", Name: "an event", TimeStart: 100, Significance: 1})

			// m1 belongs to the event and will go; survivor does not and must not.
			mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "one"})
			mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 200, Significance: 1, Body: "two"})

			if err := db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 5}}); err != nil {
				t.Fatalf("LinkMemories: %s", err)
			}

			c.delete(t, db)

			if n := countLinkRows(t, db, memoryLinksTable); n != 0 {
				t.Errorf("%s left %d link row(s) behind", c.name, n)
			}

			if got := linkSignificanceOfMemory(t, db, "m2"); got != 0 {
				t.Errorf("%s left m2 counting %d significance from a deleted memory", c.name, got)
			}
		})
	}
}

// TestPruneEventLinksOnEventDelete covers the event graph's two delete paths.
func TestPruneEventLinksOnEventDelete(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name   string
		delete func(t *testing.T, db *DB)
	}{
		{
			name: "DeleteEvent",
			delete: func(t *testing.T, db *DB) {
				if _, err := db.DeleteEvent(ctx, "e1"); err != nil {
					t.Fatalf("DeleteEvent: %s", err)
				}
			},
		},
		{
			name: "DeleteEventIfEmpty",
			delete: func(t *testing.T, db *DB) {
				deleted, err := db.DeleteEventIfEmpty(ctx, "e1", CauseConsolidation)
				if err != nil {
					t.Fatalf("DeleteEventIfEmpty: %s", err)
				}

				if !deleted {
					t.Fatal("precondition: the empty event should have been deleted")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newTestDB(t)
			t.Cleanup(func() { _ = db.Close() })

			for _, id := range []string{"e1", "e2"} {
				mustCreateEvent(t, db, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 1})
			}

			if err := db.LinkEvents(ctx, "e1", []types.Link{{Id: "e2", Significance: 6}}); err != nil {
				t.Fatalf("LinkEvents: %s", err)
			}

			c.delete(t, db)

			if n := countLinkRows(t, db, eventLinksTable); n != 0 {
				t.Errorf("%s left %d event link row(s) behind", c.name, n)
			}

			survivor, err := db.GetEvent(ctx, "e2")
			if err != nil {
				t.Fatalf("GetEvent(e2): %s", err)
			}

			if survivor.LinkSignificance != 0 {
				t.Errorf("%s left e2 counting %d significance from a deleted event", c.name, survivor.LinkSignificance)
			}
		})
	}
}

// TestDeleteEventIfEmptySparedEventKeepsItsLinks is the other half: the emptiness guard can spare
// the event, and a spared event must keep its graph.
func TestDeleteEventIfEmptySparedEventKeepsItsLinks(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	for _, id := range []string{"e1", "e2"} {
		mustCreateEvent(t, db, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 1})
	}

	// A memory on e1 makes it non-empty, so the delete is refused.
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "x"})

	if err := db.LinkEvents(ctx, "e1", []types.Link{{Id: "e2", Significance: 6}}); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	if deleted, err := db.DeleteEventIfEmpty(ctx, "e1", CauseConsolidation); err != nil {
		t.Fatalf("DeleteEventIfEmpty: %s", err)
	} else if deleted {
		t.Fatal("a non-empty event should not have been deleted")
	}

	if n := countLinkRows(t, db, eventLinksTable); n != 1 {
		t.Errorf("a spared event must keep its links, %d rows remain", n)
	}
}

// TestPurgeEmptiesBothGraphs verifies a purge takes the link tables with it, so nothing survives to
// reference rows that are gone.
func TestPurgeEmptiesBothGraphs(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	for _, id := range []string{"e1", "e2"} {
		mustCreateEvent(t, db, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 1})
	}

	if err := db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 5}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	if err := db.LinkEvents(ctx, "e1", []types.Link{{Id: "e2", Significance: 6}}); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	if err := db.Purge(ctx); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	for _, table := range []string{memoryLinksTable, eventLinksTable} {
		if n := countLinkRows(t, db, table); n != 0 {
			t.Errorf("Purge left %d rows in %s", n, table)
		}
	}
}

// TestLinkSignificanceDoesNotOverflowInt32 is the guard that moved here from types when the sum
// became the store's to compute: link significances summing past math.MaxInt32 must accumulate in
// 64 bits rather than wrapping negative, which would invert the decay maths for that memory.
func TestLinkSignificanceDoesNotOverflowInt32(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 4)

	// Three links at the int32 ceiling. The per-link cap in types is lower, but the store must not
	// be the thing that breaks if it is ever raised.
	if err := db.LinkMemories(ctx, "m1", []types.Link{
		{Id: "m2", Significance: math.MaxInt32},
		{Id: "m3", Significance: math.MaxInt32},
		{Id: "m4", Significance: math.MaxInt32},
	}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	want := int64(math.MaxInt32) * 3

	if got := linkSignificanceOfMemory(t, db, "m1"); got != want {
		t.Errorf("link_significance = %d, want %d (int32 overflow wrap?)", got, want)
	}
}

// TestGetMemoryLinksDirections verifies each direction returns its own half, and that a narrowed
// read still reports the item's full link significance - the figure the decay maths damps does not
// change because the caller asked to see less of the graph.
func TestGetMemoryLinksDirections(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 3)

	// m1 -> m2 (outbound from m1), m3 -> m1 (inbound to m1).
	if err := db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 3}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	if err := db.LinkMemories(ctx, "m3", []types.Link{{Id: "m1", Significance: 4}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	cases := []struct {
		direction types.LinkDirection
		wantIds   []string
	}{
		{types.LinkDirectionBoth, []string{"m2", "m3"}},
		{types.LinkDirectionOutbound, []string{"m2"}},
		{types.LinkDirectionInbound, []string{"m3"}},
	}

	for _, c := range cases {
		edges, total, err := db.GetMemoryLinks(ctx, "m1", c.direction)
		if err != nil {
			t.Fatalf("GetMemoryLinks(%v): %s", c.direction, err)
		}

		if len(edges) != len(c.wantIds) {
			t.Errorf("direction %v returned %d edges, want %d", c.direction, len(edges), len(c.wantIds))

			continue
		}

		seen := make(map[string]bool, len(edges))
		for _, e := range edges {
			seen[e.Id] = true
		}

		for _, want := range c.wantIds {
			if !seen[want] {
				t.Errorf("direction %v did not return %s (got %+v)", c.direction, want, edges)
			}
		}

		// Both halves count toward the total whichever direction was asked for.
		if total != 7 {
			t.Errorf("direction %v reported total %d, want the full 7", c.direction, total)
		}
	}
}

// TestLinksForMemoriesIsOutboundOnly pins the decision that keeps an archive round trip faithful.
// Returning both directions would put every edge in the archive twice, once under each end, and an
// import would then write two directed rows where one existed - doubling the aggregate.
func TestLinksForMemoriesIsOutboundOnly(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	if err := db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 3}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	links, err := db.LinksForMemories(ctx, []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("LinksForMemories: %s", err)
	}

	if len(links["m1"]) != 1 || links["m1"][0].Id != "m2" {
		t.Errorf("m1 should carry the edge it declared, got %+v", links["m1"])
	}

	if len(links["m2"]) != 0 {
		t.Errorf("m2 declared no edge and must carry none, got %+v", links["m2"])
	}
}

// TestLinkedMemoryIds verifies one-hop traversal in both directions, excluding the seeds.
func TestLinkedMemoryIds(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 4)

	// m1 -> m2, m3 -> m1. m4 is unconnected and must not appear.
	if err := db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 1}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	if err := db.LinkMemories(ctx, "m3", []types.Link{{Id: "m1", Significance: 1}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	ids, err := db.LinkedMemoryIds(ctx, []string{"m1"})
	if err != nil {
		t.Fatalf("LinkedMemoryIds: %s", err)
	}

	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		seen[id] = true
	}

	if len(ids) != 2 || !seen["m2"] || !seen["m3"] {
		t.Errorf("expected the neighbours in both directions, got %v", ids)
	}

	if seen["m1"] {
		t.Error("the seed must not be returned as its own neighbour")
	}
}

// TestReinforceLinkedMemories pins spreading activation's two rules: it advances the decay clock,
// and it never touches recall_count. Inflating the count would quietly change the value
// calculation's recall term, search ranking, and the recalled metric all at once, and would make an
// association indistinguishable from an actual retrieval.
func TestReinforceLinkedMemories(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	// Created well in the past, never recalled.
	old := time.Now().Add(-100 * 24 * time.Hour).UnixNano()
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: old, Significance: 1, Body: "x"})

	if err := db.ReinforceLinkedMemories(ctx, []string{"m1"}, 0.5); err != nil {
		t.Fatalf("ReinforceLinkedMemories: %s", err)
	}

	memories, err := db.GetMemoriesByIds(ctx, []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	m := (*memories)[0]

	if m.RecallCount != 0 {
		t.Errorf("spreading activation must not touch recall_count, got %d", m.RecallCount)
	}

	// Half way from creation to now.
	midpoint := old + (time.Now().UnixNano()-old)/2

	if m.TimeRecalled <= old || m.TimeRecalled > time.Now().UnixNano() {
		t.Fatalf("time_recalled %d is not between creation and now", m.TimeRecalled)
	}

	// Generous tolerance: "now" moves between the update and the read.
	if diff := math.Abs(float64(m.TimeRecalled - midpoint)); diff > float64(time.Minute) {
		t.Errorf("time_recalled should sit near the midpoint, off by %v", time.Duration(diff))
	}
}

// TestReinforceLinkedMemoriesNeverMovesAClockBackwards verifies a memory already recalled more
// recently than the computed point is left alone - a directly recalled memory that is also
// someone's neighbour must keep its full reinforcement.
func TestReinforceLinkedMemoriesNeverMovesAClockBackwards(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "x"})

	// Recall it, so its clock is at now.
	if _, err := db.RecallMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	before, err := db.GetMemoriesByIds(ctx, []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	was := (*before)[0].TimeRecalled

	if err := db.ReinforceLinkedMemories(ctx, []string{"m1"}, 0.5); err != nil {
		t.Fatalf("ReinforceLinkedMemories: %s", err)
	}

	after, err := db.GetMemoriesByIds(ctx, []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*after)[0].TimeRecalled < was {
		t.Errorf("time_recalled moved backwards: %d -> %d", was, (*after)[0].TimeRecalled)
	}
}

// TestReinforceLinkedMemoriesNoOps verifies the disabled and empty cases cost nothing.
func TestReinforceLinkedMemoriesNoOps(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	if err := db.ReinforceLinkedMemories(ctx, nil, 0.5); err != nil {
		t.Errorf("no ids should be a no-op, got %s", err)
	}

	if err := db.ReinforceLinkedMemories(ctx, []string{"m1"}, 0); err != nil {
		t.Errorf("a zero fraction should be a no-op, got %s", err)
	}
}

// TestMissingIds verifies the existence check the RPC layer uses to keep links from dangling.
func TestMissingIds(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	missing, err := db.MissingMemoryIds(ctx, []string{"m1", "nope", "m2", "also-nope"})
	if err != nil {
		t.Fatalf("MissingMemoryIds: %s", err)
	}

	if len(missing) != 2 || missing[0] != "nope" || missing[1] != "also-nope" {
		t.Errorf("expected the two unknown ids in request order, got %v", missing)
	}

	if empty, err := db.MissingMemoryIds(ctx, nil); err != nil || empty != nil {
		t.Errorf("MissingMemoryIds(nil) = %v, %v; want nil, nil", empty, err)
	}
}

// TestImportMemoryLinksAppliesForwardReferences is the import's whole reason for being a second
// pass: an archive is a set of rows in no particular order, so a link's target routinely appears
// after the memory declaring it. Applied at the end, none are lost; a target that is genuinely
// absent is dropped and counted rather than failing the import.
func TestImportMemoryLinksAppliesForwardReferences(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	written, dropped, err := db.ImportMemoryLinks(ctx, map[string][]types.Link{
		"m1": {
			{Id: "m2", Significance: 4},
			{Id: "never-imported", Significance: 9},
			{Id: "m1", Significance: 1}, // a self-link: dropped, not rejected
		},
		"absent-owner": {{Id: "m2", Significance: 1}},
	})
	if err != nil {
		t.Fatalf("ImportMemoryLinks: %s", err)
	}

	if written != 1 {
		t.Errorf("written = %d, want 1", written)
	}

	// The absent target, the self-link, and the whole set belonging to the absent owner.
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}

	if got := linkSignificanceOfMemory(t, db, "m1"); got != 4 {
		t.Errorf("m1 link_significance = %d, want 4", got)
	}

	if empty, _, err := db.ImportMemoryLinks(ctx, nil); err != nil || empty != 0 {
		t.Errorf("ImportMemoryLinks(nil) = %d, %v; want 0, nil", empty, err)
	}
}

// TestPruneIsANoOpOnAnEmptyGraph pins the guard that keeps the feature free for a store that does
// not use links: with no edges at all the prune answers in one existence probe rather than a read
// and a delete per chunk. Behaviourally it must be invisible.
func TestPruneIsANoOpOnAnEmptyGraph(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	seedLinkedMemories(t, db, 2)

	if _, err := db.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories on a link-free store: %s", err)
	}

	if got := linkSignificanceOfMemory(t, db, "m2"); got != 0 {
		t.Errorf("m2 link_significance = %d, want 0", got)
	}
}

// --- error branches, driven against a mocked handle: these are rollback and driver-failure paths a
// real in-memory SQLite connection cannot be made to hit on demand. ---

func TestPruneLinks_ProbeError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnError(errors.New("boom"))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err == nil {
		t.Fatal("expected the existence probe's error to propagate")
	}

	_ = tx.Rollback()
}

func TestPruneLinks_ReadError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).WillReturnError(errors.New("boom"))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err == nil {
		t.Fatal("expected the read's error to propagate")
	}

	_ = tx.Rollback()
}

// TestPruneLinks_DeleteError drives the branch where edges were found and the delete then fails,
// which is the only path that reaches the delete at all.
func TestPruneLinks_DeleteError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id"}).AddRow("m1", "m2"))
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnError(errors.New("boom"))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err == nil {
		t.Fatal("expected the delete's error to propagate")
	}

	_ = tx.Rollback()
}

// TestPruneLinks_RecalculateError drives the final step: the edges went, and recomputing the
// surviving end's aggregate fails.
func TestPruneLinks_RecalculateError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id"}).AddRow("m1", "m2"))
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnError(errors.New("boom"))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err == nil {
		t.Fatal("expected the recalculation's error to propagate")
	}

	_ = tx.Rollback()
}

// TestPruneLinks_ScanError drives the row-scan failure inside the read.
func TestPruneLinks_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id"}).
			AddRow("m1", "m2").RowError(0, errors.New("boom")))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err == nil {
		t.Fatal("expected the row error to propagate")
	}

	_ = tx.Rollback()
}

// TestCreateLinks_UpsertErrorRollsBack verifies a failing edge write rolls the whole set back
// rather than leaving the graph half-written with a stale aggregate.
func TestCreateLinks_UpsertErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO memory_links`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.LinkMemories(context.Background(), "m1", []types.Link{{Id: "m2", Significance: 1}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestCreateLinks_RecalculateErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.LinkMemories(context.Background(), "m1", []types.Link{{Id: "m2", Significance: 1}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestDeleteLinks_ErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.UnlinkMemories(context.Background(), "m1", []string{"m2"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestCreateAndDeleteLinks_EmptyAreNoOps verifies neither mutation opens a transaction when there
// is nothing to do.
func TestCreateAndDeleteLinks_EmptyAreNoOps(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	if err := d.LinkMemories(context.Background(), "m1", nil); err != nil {
		t.Errorf("an empty link set should be a no-op, got %s", err)
	}

	if err := d.UnlinkMemories(context.Background(), "m1", nil); err != nil {
		t.Errorf("an empty target set should be a no-op, got %s", err)
	}

	expectationsMet(t, mock)
}

// TestLinkUpsertMySQLUsesRowAlias pins the dialect split: MySQL has no ON CONFLICT, and its
// ON DUPLICATE KEY UPDATE needs the 8.0.20+ row alias.
func TestLinkUpsertMySQLUsesRowAlias(t *testing.T) {
	mysql := &DB{driver: driverMySQL}
	other := &DB{driver: driverSQLite}

	if got := mysql.linkUpsert(memoryLinksTable); !strings.Contains(got, "ON DUPLICATE KEY UPDATE") ||
		!strings.Contains(got, "AS new") {
		t.Errorf("MySQL upsert should use the row alias form, got %q", got)
	}

	if got := other.linkUpsert(memoryLinksTable); !strings.Contains(got, "ON CONFLICT") {
		t.Errorf("non-MySQL upsert should use ON CONFLICT, got %q", got)
	}
}

// TestFractionRatio pins the integer ratio spreading activation applies its fraction as. The
// rounding and the clamp both matter: a truncating conversion would make 0.999 and 0.001 differ by
// one part in a thousand at one end and by nothing at the other, and an unclamped fraction above 1
// (which the caller already guards, belt and braces) would advance a clock past now.
func TestFractionRatio(t *testing.T) {
	tests := []struct {
		name     string
		fraction float64
		wantNum  int64
	}{
		{name: "whole", fraction: 1, wantNum: fractionScale},
		{name: "half", fraction: 0.5, wantNum: fractionScale / 2},
		{name: "rounds up", fraction: 0.0006, wantNum: 1},
		{name: "rounds down", fraction: 0.0004, wantNum: 0},
		{name: "clamped above one", fraction: 1.5, wantNum: fractionScale},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			num, den := fractionRatio(tt.fraction)

			if num != tt.wantNum {
				t.Errorf("fractionRatio(%v) numerator = %d, want %d", tt.fraction, num, tt.wantNum)
			}

			if den != fractionScale {
				t.Errorf("fractionRatio(%v) denominator = %d, want %d", tt.fraction, den, fractionScale)
			}
		})
	}
}

func TestPurgeLinks_Error(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnError(errors.New("boom"))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.purgeLinks(tx); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
}
