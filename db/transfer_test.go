package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestGetPagesRoundTrip verifies the keyset pagination walks every row exactly once, in id
// order, including binary memories (an archive carries the whole store).
func TestGetPagesRoundTrip(t *testing.T) {
	db := newTestDB(t)

	for i := 1; i <= 5; i++ {
		memory := types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			TimeStamp:    int64(i) * 100,
			Significance: int32(i),
			Body:         "x",
			IsBinary:     i == 3,
		}

		if _, err := db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", memory.Id, err)
		}

		event := types.Event{
			Id:           fmt.Sprintf("e%d", i),
			Name:         "event",
			TimeStart:    int64(i) * 100,
			Significance: int32(i),
		}

		if _, err := db.CreateEvent(context.Background(), event); err != nil {
			t.Fatalf("CreateEvent(%s): %s", event.Id, err)
		}
	}

	var memoryIds []string
	afterId := ""

	for {
		page, err := db.GetMemoriesPage(context.Background(), afterId, 2)
		if err != nil {
			t.Fatalf("GetMemoriesPage: %s", err)
		}

		if len(page) == 0 {
			break
		}

		for _, memory := range page {
			memoryIds = append(memoryIds, memory.Id)
		}

		afterId = page[len(page)-1].Id
	}

	if fmt.Sprint(memoryIds) != "[m1 m2 m3 m4 m5]" {
		t.Errorf("expected all five memories in id order (binary included), got %v", memoryIds)
	}

	var eventIds []string
	afterId = ""

	for {
		page, err := db.GetEventsPage(context.Background(), afterId, 2)
		if err != nil {
			t.Fatalf("GetEventsPage: %s", err)
		}

		if len(page) == 0 {
			break
		}

		for _, event := range page {
			eventIds = append(eventIds, event.Id)
		}

		afterId = page[len(page)-1].Id
	}

	if fmt.Sprint(eventIds) != "[e1 e2 e3 e4 e5]" {
		t.Errorf("expected all five events in id order, got %v", eventIds)
	}
}

// TestImportPreservesFullState verifies the import upserts carry every column as-is (a data
// migration, not a fresh write), and that re-importing the same rows is idempotent.
func TestImportPreservesFullState(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{
			Id:                   "e1",
			TimeStart:            100,
			TimeEnd:              200,
			Significance:         7,
			Name:                 "imported",
			Description:          "with history",
			MemoriesConsolidated: true,
			Relationships:        []types.Relationship{{EventId: "e2", Significance: 3}},
			Group:                "billing",
		},
	}

	memories := []types.Memory{
		{
			Id:           "m1",
			TimeStamp:    150,
			Significance: 5,
			EventId:      "e1",
			Body:         "remembered",
			TimeRecalled: 180,
			RecallCount:  4,
			IsSummary:    true,
			Group:        "billing",
		},
	}

	for range 2 { // twice: the second pass must be a no-op overwrite, not a duplicate or error
		if _, err := db.ImportEvents(context.Background(), events); err != nil {
			t.Fatalf("ImportEvents: %s", err)
		}

		if _, err := db.ImportMemories(context.Background(), memories); err != nil {
			t.Fatalf("ImportMemories: %s", err)
		}
	}

	event, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if !event.MemoriesConsolidated || event.Group != "billing" || event.RelationshipSignificance != 3 ||
		event.TimeEnd != 200 || len(event.Relationships) != 1 {
		t.Errorf("event state not preserved: %+v", event)
	}

	memory := getMemory(t, db, "m1")
	if memory == nil {
		t.Fatal("imported memory not found")
	}

	if memory.TimeRecalled != 180 || memory.RecallCount != 4 || !memory.IsSummary ||
		memory.Group != "billing" || memory.TimeStamp != 150 || memory.Body != "remembered" {
		t.Errorf("memory state not preserved: %+v", memory)
	}

	// An import over an existing row overwrites even fields the incremental upserts would keep.
	memories[0].RecallCount = 0
	memories[0].TimeRecalled = 0

	if _, err := db.ImportMemories(context.Background(), memories); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	if memory := getMemory(t, db, "m1"); memory == nil || memory.RecallCount != 0 || memory.TimeRecalled != 0 {
		t.Errorf("full-state import should overwrite recall state, got %+v", memory)
	}
}

// TestClearMemoriesRespectsRecallSnapshots verifies Clear's protection: a memory recalled after
// being captured survives the clear, and the search-index delete observer sees only the rows
// actually deleted.
func TestClearMemoriesRespectsRecallSnapshots(t *testing.T) {
	db := newTestDB(t)

	for _, id := range []string{"m1", "m2"} {
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	var observed []string
	db.SetMemoryDeleteObserver(func(ids []string) { observed = append(observed, ids...) })

	// Capture both unrecalled, then a recall lands on m2 before the clear.
	snapshots := []MemoryRecallSnapshot{
		{Id: "m1", TimeRecalled: 0, RecallCount: 0},
		{Id: "m2", TimeRecalled: 0, RecallCount: 0},
	}

	if _, err := db.RecallMemories(context.Background(), []string{"m2"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	cleared, err := db.ClearMemories(context.Background(), snapshots)
	if err != nil {
		t.Fatalf("ClearMemories: %s", err)
	}

	if cleared != 1 {
		t.Errorf("expected 1 memory cleared (m2 protected by its recall), got %d", cleared)
	}

	if getMemory(t, db, "m1") != nil {
		t.Error("m1 should have been cleared")
	}

	if getMemory(t, db, "m2") == nil {
		t.Error("m2 was recalled after capture and must survive the clear")
	}

	if fmt.Sprint(observed) != "[m1]" {
		t.Errorf("delete observer should see exactly [m1], got %v", observed)
	}
}
