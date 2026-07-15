package db

import (
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// evictionTestDB builds a store with one event holding three memories of ascending significance
// and one loose memory, for the eviction tests. The decisionServer values each memory by its own
// significance, so eviction order is deterministic.
func evictionTestDB(t *testing.T) (*DB, *decisionServer) {
	t.Helper()

	db := newTestDB(t)

	if _, err := db.CreateEvent(types.Event{Id: "e1", Name: "an event", TimeStart: 100, Significance: 50}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "one"},
		{Id: "m2", TimeStamp: 100, Significance: 2, EventId: "e1", Body: "two"},
		{Id: "m3", TimeStamp: 100, Significance: 3, EventId: "e1", Body: "three"},
		{Id: "m4", TimeStamp: 100, Significance: 4, Body: "loose"},
	}

	for _, memory := range memories {
		if _, err := db.CreateMemory(memory); err != nil {
			t.Fatalf("CreateMemory %s: %s", memory.Id, err)
		}
	}

	server := &decisionServer{
		value: func(candidate MemoryConsolidationCandidate) float64 {
			return float64(candidate.MemorySignificance)
		},
	}

	return db, server
}

// TestEvictMemories_LowestValueFirst verifies that eviction deletes memories in ascending value
// order and stops once the requested bytes are freed, flagging the partially stripped event as
// consolidated.
func TestEvictMemories_LowestValueFirst(t *testing.T) {
	db, server := evictionTestDB(t)

	// One row's footprint satisfies a 1-byte request, so only the least valuable memory goes.
	deletedMemories, deletedEvents, freed, err := db.EvictMemories(server, 1)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if deletedMemories != 1 || deletedEvents != 0 {
		t.Fatalf("expected 1 memory and 0 events deleted, got %d and %d", deletedMemories, deletedEvents)
	}

	if freed <= 0 {
		t.Errorf("expected a positive freed-bytes estimate, got %d", freed)
	}

	if m := getMemory(t, db, "m1"); m != nil {
		t.Error("m1 (lowest value) should have been evicted")
	}

	for _, id := range []string{"m2", "m3", "m4"} {
		if m := getMemory(t, db, id); m == nil {
			t.Errorf("%s should have survived", id)
		}
	}

	event, err := db.GetEvent("e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if !event.MemoriesConsolidated {
		t.Error("partially stripped event should be flagged as consolidated")
	}
}

// TestEvictMemories_DeletesEmptiedEvent verifies that an event loses its record when eviction
// removes its last memory, and that a large enough request empties the store.
func TestEvictMemories_DeletesEmptiedEvent(t *testing.T) {
	db, server := evictionTestDB(t)

	deletedMemories, deletedEvents, _, err := db.EvictMemories(server, 1<<30)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if deletedMemories != 4 || deletedEvents != 1 {
		t.Fatalf("expected 4 memories and 1 event deleted, got %d and %d", deletedMemories, deletedEvents)
	}

	if db.CountEvents() != 0 {
		t.Errorf("expected 0 events after full eviction, got %d", db.CountEvents())
	}

	with, without := db.CountMemories()
	if with != 0 || without != 0 {
		t.Errorf("expected 0 memories after full eviction, got %d with events, %d without", with, without)
	}
}

// TestDeleteEventIfEmpty verifies the atomic check-and-delete DeleteEventIfEmpty relies on:
// deletion only happens when the event truly has no memories at the moment of the call. This is
// what protects against the race where EvictMemories, ConsolidateEventMemories, or
// ConsolidateEvents scan an event as empty, a concurrent write then attaches a fresh memory to it,
// and only afterwards does the pass get around to deleting the "empty" event — without this
// re-check at delete time, that memory would be left pointing at a deleted event.
func TestDeleteEventIfEmpty(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "empty", Name: "no memories", TimeStart: 100, Significance: 1},
		{Id: "occupied", Name: "has a memory", TimeStart: 100, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	if _, err := db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "occupied", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	deleted, err := db.DeleteEventIfEmpty("empty")
	if err != nil {
		t.Fatalf("DeleteEventIfEmpty(empty): %s", err)
	}

	if !deleted {
		t.Error("expected the memory-less event to be deleted")
	}

	if _, err := db.GetEvent("empty"); err == nil {
		t.Error("expected 'empty' to be gone")
	}

	deleted, err = db.DeleteEventIfEmpty("occupied")
	if err != nil {
		t.Fatalf("DeleteEventIfEmpty(occupied): %s", err)
	}

	if deleted {
		t.Error("expected the occupied event to survive")
	}

	if _, err := db.GetEvent("occupied"); err != nil {
		t.Errorf("expected 'occupied' to survive: %s", err)
	}
}

// TestEvictMemories_NoOpWhenNothingToFree verifies that a non-positive request deletes nothing.
func TestEvictMemories_NoOpWhenNothingToFree(t *testing.T) {
	db, server := evictionTestDB(t)

	deletedMemories, deletedEvents, freed, err := db.EvictMemories(server, 0)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if deletedMemories != 0 || deletedEvents != 0 || freed != 0 {
		t.Errorf("expected no deletions for a zero request, got %d memories, %d events, %d bytes", deletedMemories, deletedEvents, freed)
	}
}

// TestUsedBytes verifies that the used-bytes measure is positive for a populated store and does
// not count pages freed by deletion.
func TestUsedBytes(t *testing.T) {
	db := newTestDB(t)

	before, err := db.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	if before <= 0 {
		t.Fatalf("expected positive used bytes for an initialised store, got %d", before)
	}

	// A memory with a body spanning multiple pages must grow the measure.
	body := make([]byte, 64*1024)
	if _, err := db.CreateMemory(types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	grown, err := db.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	if grown <= before {
		t.Errorf("expected used bytes to grow after storing a 64 KiB body, got %d -> %d", before, grown)
	}

	// Deleting the memory returns its pages to the freelist, which the measure must exclude.
	if err := db.DeleteMemory("big"); err != nil {
		t.Fatalf("DeleteMemory: %s", err)
	}

	shrunk, err := db.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	if shrunk >= grown {
		t.Errorf("expected used bytes to shrink after deletion, got %d -> %d", grown, shrunk)
	}
}
