package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// mustCreateEvent creates an event and fails the test on error.
func mustCreateEvent(t *testing.T, db *DB, e types.Event) {
	t.Helper()

	if _, err := db.CreateEvent(context.Background(), e); err != nil {
		t.Fatalf("CreateEvent(%s): %s", e.Id, err)
	}
}

// mustCreateMemory creates a memory and fails the test on error.
func mustCreateMemory(t *testing.T, db *DB, m types.Memory) {
	t.Helper()

	if _, err := db.CreateMemory(context.Background(), m); err != nil {
		t.Fatalf("CreateMemory(%s): %s", m.Id, err)
	}
}

// TestEventExists verifies the existence probe reports present and absent events.
func TestEventExists(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})

	if exists, err := db.EventExists(context.Background(), "e1"); err != nil || !exists {
		t.Errorf("expected e1 to exist, got exists=%v err=%v", exists, err)
	}

	if exists, err := db.EventExists(context.Background(), "missing"); err != nil || exists {
		t.Errorf("expected 'missing' not to exist, got exists=%v err=%v", exists, err)
	}
}

// TestDeleteEvent verifies a present event is deleted (reporting true) and an unknown id reports
// false without error.
func TestDeleteEvent(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})

	if deleted, err := db.DeleteEvent(context.Background(), "e1"); err != nil || !deleted {
		t.Errorf("expected e1 deleted, got deleted=%v err=%v", deleted, err)
	}

	if exists, _ := db.EventExists(context.Background(), "e1"); exists {
		t.Error("e1 should be gone after DeleteEvent")
	}

	if deleted, err := db.DeleteEvent(context.Background(), "missing"); err != nil || deleted {
		t.Errorf("expected no deletion for unknown id, got deleted=%v err=%v", deleted, err)
	}
}

// TestCalculateSignificancePercentile verifies the nearest-rank percentile over stored event
// significances.
func TestCalculateSignificancePercentile(t *testing.T) {
	db := newTestDB(t)

	for _, sig := range []int32{10, 20, 30, 40, 50} {
		mustCreateEvent(t, db, types.Event{Id: "e" + string(rune('0'+sig/10)), Name: "n", TimeStart: 100, Significance: sig})
	}

	// Nearest-rank 100th percentile is the maximum.
	if got, err := db.CalculateSignificancePercentile(context.Background(), 100); err != nil || got != 50 {
		t.Errorf("expected 100th percentile 50, got %v err=%v", got, err)
	}

	// A low percentile picks the smallest value.
	if got, err := db.CalculateSignificancePercentile(context.Background(), 1); err != nil || got != 10 {
		t.Errorf("expected 1st percentile 10, got %v err=%v", got, err)
	}
}

// TestDeleteEventMemories verifies every memory attached to an event is deleted and the count
// returned, while memories on other events are untouched.
func TestDeleteEventMemories(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})
	mustCreateEvent(t, db, types.Event{Id: "e2", Name: "two", TimeStart: 100, Significance: 1})
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "b"})
	mustCreateMemory(t, db, types.Memory{Id: "m3", TimeStamp: 100, Significance: 1, EventId: "e2", Body: "c"})

	n, err := db.DeleteEventMemories(context.Background(), "e1")
	if err != nil {
		t.Fatalf("DeleteEventMemories: %s", err)
	}

	if n != 2 {
		t.Errorf("expected 2 memories deleted, got %d", n)
	}

	survivors, err := db.GetMemoriesByEventId(context.Background(), "e2")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*survivors) != 1 {
		t.Errorf("expected e2's memory to survive, got %d", len(*survivors))
	}
}

// TestUnsetMemoriesEventId verifies the event_id is cleared on every memory of an event, orphaning
// them rather than deleting them.
func TestUnsetMemoriesEventId(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "b"})

	n, err := db.UnsetMemoriesEventId(context.Background(), "e1")
	if err != nil {
		t.Fatalf("UnsetMemoriesEventId: %s", err)
	}

	if n != 2 {
		t.Errorf("expected 2 memories unset, got %d", n)
	}

	if remaining, _ := db.GetMemoriesByEventId(context.Background(), "e1"); len(*remaining) != 0 {
		t.Errorf("expected e1 to have no memories after unset, got %d", len(*remaining))
	}

	orphans, err := db.GetMemoriesByEventIds(context.Background(), []string{""})
	if err != nil {
		t.Fatalf("GetMemoriesByEventIds: %s", err)
	}

	if len(*orphans) != 2 {
		t.Errorf("expected 2 orphaned (event-less) memories, got %d", len(*orphans))
	}
}

// TestGetMemoriesByEventIds verifies memories are fetched across multiple event ids and an empty
// input short-circuits to an empty slice.
func TestGetMemoriesByEventIds(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})
	mustCreateEvent(t, db, types.Event{Id: "e2", Name: "two", TimeStart: 100, Significance: 1})
	mustCreateEvent(t, db, types.Event{Id: "e3", Name: "three", TimeStart: 100, Significance: 1})
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e2", Body: "b"})
	mustCreateMemory(t, db, types.Memory{Id: "m3", TimeStamp: 100, Significance: 1, EventId: "e3", Body: "c"})

	got, err := db.GetMemoriesByEventIds(context.Background(), []string{"e1", "e2"})
	if err != nil {
		t.Fatalf("GetMemoriesByEventIds: %s", err)
	}

	if len(*got) != 2 {
		t.Errorf("expected 2 memories for e1+e2, got %d", len(*got))
	}

	empty, err := db.GetMemoriesByEventIds(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetMemoriesByEventIds(nil): %s", err)
	}

	if len(*empty) != 0 {
		t.Errorf("expected no memories for an empty id set, got %d", len(*empty))
	}
}

// TestMergeEventMemories verifies memories are re-pointed from one event to another.
func TestMergeEventMemories(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "src", Name: "source", TimeStart: 100, Significance: 1})
	mustCreateEvent(t, db, types.Event{Id: "dst", Name: "dest", TimeStart: 100, Significance: 1})
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "src", Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "src", Body: "b"})

	if err := db.MergeEventMemories(context.Background(), "dst", "src"); err != nil {
		t.Fatalf("MergeEventMemories: %s", err)
	}

	if remaining, _ := db.GetMemoriesByEventId(context.Background(), "src"); len(*remaining) != 0 {
		t.Errorf("expected src to be emptied, got %d memories", len(*remaining))
	}

	moved, err := db.GetMemoriesByEventId(context.Background(), "dst")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId(dst): %s", err)
	}

	if len(*moved) != 2 {
		t.Errorf("expected 2 memories moved to dst, got %d", len(*moved))
	}
}

// TestConsolidateMemories verifies the event-less consolidation pass deletes only memories the
// decision function selects, and leaves evented memories for the other passes.
func TestConsolidateMemories(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})
	mustCreateMemory(t, db, types.Memory{Id: "free", TimeStamp: 100, Significance: 1, Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "evented", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "b"})

	deleted, err := db.ConsolidateMemories(context.Background(), &stubServer{consolidateMemories: true})
	if err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	if deleted != 1 {
		t.Errorf("expected 1 event-less memory deleted, got %d", deleted)
	}

	if got, _ := db.GetMemoriesByIds(context.Background(), []string{"evented"}); len(*got) != 1 {
		t.Error("evented memory must survive the event-less pass")
	}

	if got, _ := db.GetMemoriesByIds(context.Background(), []string{"free"}); len(*got) != 0 {
		t.Error("event-less memory should have been deleted")
	}
}

// TestConsolidateMemories_KeepsAll verifies nothing is deleted when the decision function declines.
func TestConsolidateMemories_KeepsAll(t *testing.T) {
	db := newTestDB(t)

	mustCreateMemory(t, db, types.Memory{Id: "free", TimeStamp: 100, Significance: 1, Body: "a"})

	deleted, err := db.ConsolidateMemories(context.Background(), &stubServer{consolidateMemories: false})
	if err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	if deleted != 0 {
		t.Errorf("expected no deletions, got %d", deleted)
	}
}
