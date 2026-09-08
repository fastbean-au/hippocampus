package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// seedForPredicate writes n memories in the given group, attached to eventId ("" for none).
func seedForPredicate(t *testing.T, d *DB, group string, eventId string, n int) []string {
	t.Helper()

	ids := make([]string, 0, n)

	for i := range n {
		id := fmt.Sprintf("%s-%03d", group, i)

		if _, err := d.CreateMemory(context.Background(), types.Memory{
			Id:           id,
			Body:         "body",
			TimeStamp:    int64(1000 + i),
			Significance: 5,
			EventId:      eventId,
			Group:        group,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}

		ids = append(ids, id)
	}

	return ids
}

// TestMemoryIdsMatching checks the selection half of a predicate delete: the filter narrows, the
// limit bounds, and the ordering is by id so successive batches are deterministic.
func TestMemoryIdsMatching(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	seedForPredicate(t, d, "a", "", 5)
	seedForPredicate(t, d, "b", "", 2)

	all, err := d.MemoryIdsMatching(ctx, MemoryFilter{Group: "a"})
	if err != nil {
		t.Fatalf("MemoryIdsMatching: %s", err)
	}

	if len(all) != 5 {
		t.Errorf("got %d ids, want 5", len(all))
	}

	for _, id := range all {
		if id[0] != 'a' {
			t.Errorf("the group filter returned %q", id)
		}
	}

	bounded, err := d.MemoryIdsMatching(ctx, MemoryFilter{Group: "a", Limit: 2})
	if err != nil {
		t.Fatalf("MemoryIdsMatching (bounded): %s", err)
	}

	if len(bounded) != 2 {
		t.Fatalf("got %d ids under Limit 2", len(bounded))
	}

	// Ordered by id, so the bounded selection is the FIRST two of the whole set - which is what
	// makes deleting a batch enough to advance the next selection.
	if bounded[0] != all[0] || bounded[1] != all[1] {
		t.Errorf("bounded selection %v is not the head of %v", bounded, all[:2])
	}
}

// TestMemoryIdsMatching_SignificanceFilter exercises the branch that swaps in the
// significance-carrying view, which is the one place this query's FROM clause is not the plain
// table.
func TestMemoryIdsMatching_SignificanceFilter(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	for i, significance := range []int32{1, 5, 9} {
		if _, err := d.CreateMemory(ctx, types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			Body:         "body",
			TimeStamp:    100,
			Significance: significance,
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	ids, err := d.MemoryIdsMatching(ctx, MemoryFilter{SignificanceMin: 5})
	if err != nil {
		t.Fatalf("MemoryIdsMatching: %s", err)
	}

	if len(ids) != 2 {
		t.Errorf("got %d ids for significance >= 5, want 2", len(ids))
	}
}

// TestEventIdsMatching is MemoryIdsMatching's counterpart.
func TestEventIdsMatching(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	for i := range 4 {
		group := "a"
		if i >= 2 {
			group = "b"
		}

		if _, err := d.CreateEvent(ctx, types.Event{
			Id:           fmt.Sprintf("e%d", i),
			Name:         "event",
			TimeStart:    100,
			Significance: 5,
			Group:        group,
		}); err != nil {
			t.Fatalf("CreateEvent: %s", err)
		}
	}

	ids, err := d.EventIdsMatching(ctx, EventFilter{Group: "a"})
	if err != nil {
		t.Fatalf("EventIdsMatching: %s", err)
	}

	if len(ids) != 2 {
		t.Errorf("got %d ids, want 2", len(ids))
	}

	bounded, err := d.EventIdsMatching(ctx, EventFilter{Group: "a", Limit: 1})
	if err != nil {
		t.Fatalf("EventIdsMatching (bounded): %s", err)
	}

	if len(bounded) != 1 || bounded[0] != ids[0] {
		t.Errorf("bounded selection %v is not the head of %v", bounded, ids)
	}
}

// TestEventIdsForMemories: distinct, non-empty, and read while the memories are still there.
func TestEventIdsForMemories(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"e1", "e2"} {
		if _, err := d.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 5}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	seedForPredicate(t, d, "x", "e1", 3)
	seedForPredicate(t, d, "y", "e2", 1)
	seedForPredicate(t, d, "z", "", 2)

	ids, err := d.EventIdsForMemories(ctx, []string{"x-000", "x-001", "x-002", "y-000", "z-000"})
	if err != nil {
		t.Fatalf("EventIdsForMemories: %s", err)
	}

	if len(ids) != 2 {
		t.Fatalf("got %v, want the two distinct event ids", ids)
	}

	seen := map[string]bool{ids[0]: true, ids[1]: true}

	if !seen["e1"] || !seen["e2"] {
		t.Errorf("got %v, want e1 and e2", ids)
	}

	// The event-less memory contributes nothing: "no event" is not an event to consider deleting.
	if seen[""] {
		t.Error("the empty event id was returned")
	}

	if got, err := d.EventIdsForMemories(ctx, nil); err != nil || got != nil {
		t.Errorf("EventIdsForMemories(nil) = %v, %v; want nil, nil", got, err)
	}
}

// TestDeleteEvents is the batch event delete: the named events go, the others stay, and the link
// graph is pruned so a survivor is not left counting significance from an edge to a deleted event.
func TestDeleteEvents(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"e1", "e2", "e3"} {
		if _, err := d.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 5}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	if err := d.LinkEvents(ctx, "e1", []types.Link{{Id: "e3", Significance: 4}}); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	deleted, err := d.DeleteEvents(ctx, []string{"e1", "e2"})
	if err != nil {
		t.Fatalf("DeleteEvents: %s", err)
	}

	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	if n := d.CountEvents(ctx); n != 1 {
		t.Errorf("%d events remain, want 1", n)
	}

	links, significance, err := d.GetEventLinks(ctx, "e3", types.LinkDirectionBoth)
	if err != nil {
		t.Fatalf("GetEventLinks: %s", err)
	}

	if len(links) != 0 || significance != 0 {
		t.Errorf("e3 still carries %d link(s) worth %d after its far end was deleted", len(links), significance)
	}
}

// TestDeleteEvents_UnknownIdsAndEmpty: an id the store does not hold is not an error (nothing is
// there to delete), and an empty request does nothing at all.
func TestDeleteEvents_UnknownIdsAndEmpty(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	deleted, err := d.DeleteEvents(ctx, []string{"e1", "nope"})
	if err != nil {
		t.Fatalf("DeleteEvents: %s", err)
	}

	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the unknown id contributes nothing)", deleted)
	}

	if deleted, err := d.DeleteEvents(ctx, nil); err != nil || deleted != 0 {
		t.Errorf("DeleteEvents(nil) = %d, %v; want 0, nil", deleted, err)
	}
}

// TestDeleteEvents_QueuesCallbacks: with the queue on, a batch delete announces the events it
// removed - and announces nothing when nothing went, which is the guard around the queueing.
func TestDeleteEvents_QueuesCallbacks(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	d.SetCallbackPolicy(CallbackPolicy{Enabled: true, EventEvents: true, AllDeletions: true})

	for _, id := range []string{"e1", "e2"} {
		if _, err := d.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 5}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	if _, err := d.DeleteEvents(ctx, []string{"e1", "e2"}); err != nil {
		t.Fatalf("DeleteEvents: %s", err)
	}

	queued, err := d.CallbackQueueDepth(ctx)
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err)
	}

	if queued == 0 {
		t.Fatal("a batch event delete queued no callback")
	}

	// Nothing goes, so nothing is announced: an id the store does not hold must not produce a
	// delivery claiming an event was deleted.
	before := queued

	if _, err := d.DeleteEvents(ctx, []string{"never-existed"}); err != nil {
		t.Fatalf("DeleteEvents (unknown id): %s", err)
	}

	after, err := d.CallbackQueueDepth(ctx)
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err)
	}

	if after != before {
		t.Errorf("deleting nothing queued %d extra callback(s)", after-before)
	}
}
