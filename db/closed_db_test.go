package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestStoreMethods_ErrorOnClosedDB sweeps the Store surface against a closed database, asserting
// each method surfaces an error (or the documented sentinel) rather than panicking. A closed
// *sql.DB fails every query/exec immediately, so this exercises the "first database call fails"
// error-return branch that guards nearly every method in db.go/event.go/memory.go/significance.go/
// transfer.go — the single most common uncovered shape in this package (a query, then
// `if err != nil { log.Errorf(...); return ... }`). It does not reach deeper branches inside
// multi-statement methods (a later scan, a second query); those are covered by more targeted tests
// elsewhere.
func TestStoreMethods_ErrorOnClosedDB(t *testing.T) {
	db := newTestDB(t)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	ctx := context.Background()
	server := &stubServer{consolidateMemories: true, consolidateEvents: true}

	memory := types.Memory{Id: "m1", Body: "x", Significance: 1, TimeStamp: 1}
	event := types.Event{Id: "e1", Name: "n", TimeStart: 1, Significance: 1}

	checks := []struct {
		name string
		call func() error
	}{
		{"CreateMemory", func() error { _, err := db.CreateMemory(ctx, memory); return err }},
		{"UpdateMemory", func() error { _, err := db.UpdateMemory(ctx, memory); return err }},
		{"UpdateMemory (no-op probe)", func() error { _, err := db.UpdateMemory(ctx, types.Memory{Id: "m1"}); return err }},
		{"DeleteMemory", func() error { return db.DeleteMemory(ctx, "m1") }},
		{"DeleteMemories", func() error { _, err := db.DeleteMemories(ctx, []string{"m1"}); return err }},
		{"RecallMemories", func() error { _, err := db.RecallMemories(ctx, []string{"m1"}); return err }},
		{"ReplaceMemoriesWithSummary", func() error {
			_, err := db.ReplaceMemoriesWithSummary(ctx, "e1", types.Memory{Id: "s1", Significance: 1, TimeStamp: 1, Body: "x"})

			return err
		}},
		{"GetMemories", func() error { _, err := db.GetMemories(ctx, MemoryFilter{}); return err }},
		{"GetMemoriesByEventId", func() error { _, err := db.GetMemoriesByEventId(ctx, "e1"); return err }},
		{"GetMemoriesByEventIds", func() error { _, err := db.GetMemoriesByEventIds(ctx, []string{"e1"}); return err }},
		{"GetMemoriesByIds", func() error { _, err := db.GetMemoriesByIds(ctx, []string{"m1"}); return err }},
		{"GetIndexableMemoriesPage", func() error { _, err := db.GetIndexableMemoriesPage(ctx, "", 10); return err }},
		{"CountMemoriesFiltered", func() error { _, err := db.CountMemoriesFiltered(ctx, MemoryFilter{}); return err }},
		{"CreateEvent", func() error { _, err := db.CreateEvent(ctx, event); return err }},
		{"UpdateEvent", func() error { _, err := db.UpdateEvent(ctx, event); return err }},
		{"UpdateEvent (no-op probe)", func() error { _, err := db.UpdateEvent(ctx, types.Event{Id: "e1"}); return err }},
		{"DeleteEvent", func() error { _, err := db.DeleteEvent(ctx, "e1"); return err }},
		{"DeleteEventIfEmpty", func() error { _, err := db.DeleteEventIfEmpty(ctx, "e1"); return err }},
		{"EventExists", func() error { _, err := db.EventExists(ctx, "e1"); return err }},
		{"GetEvent", func() error { _, err := db.GetEvent(ctx, "e1"); return err }},
		{"GetEvents", func() error { _, err := db.GetEvents(ctx, EventFilter{}); return err }},
		{"CountEventsFiltered", func() error { _, err := db.CountEventsFiltered(ctx, EventFilter{}); return err }},
		{"MergeEventMemories", func() error { return db.MergeEventMemories(ctx, "e1", "e2") }},
		{"DeleteEventMemories", func() error { _, err := db.DeleteEventMemories(ctx, "e1"); return err }},
		{"UnsetMemoriesEventId", func() error { _, err := db.UnsetMemoriesEventId(ctx, "e1"); return err }},
		{"CalculateSignificancePercentile", func() error { _, err := db.CalculateSignificancePercentile(ctx, 50); return err }},
		{"ResolveSignificanceLevel", func() error {
			_, _, err := db.ResolveSignificanceLevel(ctx, SignificanceSpec{Value: 1})

			return err
		}},
		{"ResolveSignificanceLevel (placement)", func() error {
			_, _, err := db.ResolveSignificanceLevel(ctx, SignificanceSpec{Placement: PlacementAbove, Anchor: 1, AnchorKind: AnchorMemory})

			return err
		}},
		{"CompactSignificanceLevels", func() error { return db.CompactSignificanceLevels(ctx) }},
		{"ConsolidateMemories", func() error { _, err := db.ConsolidateMemories(ctx, server); return err }},
		{"ConsolidateEventMemories", func() error { _, _, _, err := db.ConsolidateEventMemories(ctx, server); return err }},
		{"ConsolidateEvents", func() error { _, err := db.ConsolidateEvents(ctx, server); return err }},
		{"EvictMemories", func() error { _, _, _, err := db.EvictMemories(ctx, server, 1024); return err }},
		{"FindSummarizationCandidates", func() error { _, err := db.FindSummarizationCandidates(ctx, 1, 1, 0); return err }},
		{"GetMemoriesPage", func() error { _, err := db.GetMemoriesPage(ctx, "", 10); return err }},
		{"GetEventsPage", func() error { _, err := db.GetEventsPage(ctx, "", 10); return err }},
		{"ImportMemories", func() error { _, err := db.ImportMemories(ctx, []types.Memory{memory}); return err }},
		{"ImportEvents", func() error { _, err := db.ImportEvents(ctx, []types.Event{event}); return err }},
		{"ClearMemories", func() error {
			_, err := db.ClearMemories(ctx, []MemoryRecallSnapshot{{Id: "m1"}})

			return err
		}},
		{"Preserve", func() error { return db.Preserve(ctx) }},
		{"Purge", func() error { return db.Purge(ctx) }},
	}

	for _, c := range checks {
		if err := c.call(); err == nil {
			t.Errorf("%s: expected an error against a closed database", c.name)
		}
	}

	// The two sentinel-return methods (no error in their signature) must report their documented
	// failure value rather than panicking.
	if with, without := db.CountMemories(ctx); with != -1 || without != -1 {
		t.Errorf("CountMemories on a closed database = (%d, %d), want (-1, -1)", with, without)
	}

	if n := db.CountEvents(ctx); n != -1 {
		t.Errorf("CountEvents on a closed database = %d, want -1", n)
	}
}

// TestUsedBytes_ErrorOnClosedDB covers UsedBytes' SQLite PRAGMA error path separately: it takes ctx
// but is otherwise the same "closed database" shape as the sweep above.
func TestUsedBytes_ErrorOnClosedDB(t *testing.T) {
	db := newTestDB(t)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := db.UsedBytes(context.Background()); err == nil {
		t.Error("expected UsedBytes to error against a closed database")
	}
}
