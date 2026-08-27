package db

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestDeletesAreQueuedTransactionally is the property the outbox exists for: a memory that is
// deleted leaves a durable record that its index document must go, committed with the delete
// itself rather than handed to a queue that may drop it.
func TestDeletesAreQueuedTransactionally(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	ids := []string{"outbox-a", "outbox-b", "outbox-c"}

	for _, id := range ids {
		if _, err := database.CreateMemory(ctx, types.Memory{
			Id: id, Body: "queued", Significance: 1000, TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	// Creating memories must not queue anything: only deletes go in the outbox.
	if n, err := database.SearchOutboxDepth(ctx); err != nil || n != 0 {
		t.Fatalf("depth after writes: got %d (err %v), want 0", n, err)
	}

	if _, err := database.DeleteMemories(ctx, ids[:2]); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	n, err := database.SearchOutboxDepth(ctx)
	if err != nil {
		t.Fatalf("SearchOutboxDepth: %s", err)
	}

	if n != 2 {
		t.Fatalf("depth after deleting two memories: got %d, want 2", n)
	}

	claimed, err := database.ClaimSearchDeletes(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimSearchDeletes: %s", err)
	}

	got := map[string]bool{}

	for _, v := range claimed {
		got[v.ID] = true
	}

	for _, id := range ids[:2] {
		if !got[id] {
			t.Errorf("deleted memory %q was not queued for index deletion", id)
		}
	}

	if got[ids[2]] {
		t.Error("a memory that was never deleted was queued")
	}
}

// TestClaimDoesNotRemove pins the at-least-once contract: a claimed row survives until the index
// has actually accepted the deletion, so a crash mid-pass replays the work instead of losing it.
func TestClaimDoesNotRemove(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id: "claim-me", Body: "x", Significance: 1000, TimeStamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.DeleteMemories(ctx, []string{"claim-me"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	first, err := database.ClaimSearchDeletes(ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim: %d rows (err %v), want 1", len(first), err)
	}

	// A second claim without confirmation sees the same work: nothing was consumed.
	second, err := database.ClaimSearchDeletes(ctx, 10)
	if err != nil || len(second) != 1 {
		t.Fatalf("second claim: %d rows (err %v), want 1 - claiming must not consume", len(second), err)
	}

	if err := database.ConfirmSearchDeletes(ctx, []int64{first[0].Seq}); err != nil {
		t.Fatalf("ConfirmSearchDeletes: %s", err)
	}

	third, err := database.ClaimSearchDeletes(ctx, 10)
	if err != nil || len(third) != 0 {
		t.Fatalf("after confirmation: %d rows (err %v), want 0", len(third), err)
	}
}

// TestPruneBoundsTheOutbox covers the case the drain cannot: an index unreachable for long enough
// that the queue would otherwise grow without bound. The abandoned deletions become the
// reconciliation sweep's problem, which is what the sweep is for.
func TestPruneBoundsTheOutbox(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		id := string(rune('a' + i))

		if _, err := database.CreateMemory(ctx, types.Memory{
			Id: id, Body: "x", Significance: 1000, TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}

		if _, err := database.DeleteMemories(ctx, []string{id}); err != nil {
			t.Fatalf("DeleteMemories: %s", err)
		}
	}

	if n, _ := database.SearchOutboxDepth(ctx); n != 6 {
		t.Fatalf("depth: got %d, want 6", n)
	}

	if _, err := database.PruneSearchOutbox(ctx, 0, 4); err != nil {
		t.Fatalf("PruneSearchOutbox: %s", err)
	}

	n, err := database.SearchOutboxDepth(ctx)
	if err != nil {
		t.Fatalf("SearchOutboxDepth: %s", err)
	}

	if n != 4 {
		t.Errorf("after trimming to 4 rows: got %d", n)
	}

	// The rows kept are the NEWEST, since an older queued delete has had longer for the sweep to
	// have noticed it.
	kept, err := database.ClaimSearchDeletes(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimSearchDeletes: %s", err)
	}

	for _, v := range kept {
		if v.ID == "a" || v.ID == "b" {
			t.Errorf("trimming kept the oldest row %q", v.ID)
		}
	}
}

// TestAgePruneAbandonsStaleQueuedDeletes covers the other cap.
func TestAgePruneAbandonsStaleQueuedDeletes(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id: "old", Body: "x", Significance: 1000, TimeStamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.DeleteMemories(ctx, []string{"old"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	// Nothing is old enough yet.
	if pruned, err := database.PruneSearchOutbox(ctx, time.Hour, 0); err != nil || pruned != 0 {
		t.Fatalf("premature prune: %d rows (err %v)", pruned, err)
	}

	// Everything is, at a zero-width window.
	if _, err := database.PruneSearchOutbox(ctx, time.Nanosecond, 0); err != nil {
		t.Fatalf("PruneSearchOutbox: %s", err)
	}

	if n, _ := database.SearchOutboxDepth(ctx); n != 0 {
		t.Errorf("after the age cap: got %d, want 0", n)
	}
}

// newOutboxTestDB is newTestDB with the outbox turned on, which is what main does when the active
// search backend is one whose deletes can be lost.
func newOutboxTestDB(t *testing.T) *DB {
	t.Helper()

	database := newTestDB(t)
	database.SetSearchOutbox(true)

	return database
}

// TestNothingIsQueuedWithoutTheOutbox is the other half of the gate, and it is not merely an
// optimisation being pinned: without it every SQLite deployment - where index deletes are already
// transactional, through an AFTER DELETE trigger - would write a row per forgotten memory into a
// table with nothing running to drain it, and a sleep cycle deleting ten thousand memories would
// leave ten thousand rows behind forever.
func TestNothingIsQueuedWithoutTheOutbox(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id: "ungated", Body: "queued", Significance: 1000, TimeStamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.DeleteMemories(ctx, []string{"ungated"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	// Read the table directly: SearchOutboxDepth is itself gated, so asking it would prove nothing.
	var count int64

	if err := database.queryRow(ctx, `SELECT COUNT(*) FROM `+searchOutboxTable).Scan(&count); err != nil {
		t.Fatalf("counting the outbox: %s", err)
	}

	if count != 0 {
		t.Fatalf("a store with the outbox disabled queued %d deletions; it must queue none", count)
	}
}
