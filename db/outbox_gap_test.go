package db

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// The outbox's own gate and its storage-failure arms. outbox_test.go covers the mechanism - a
// delete queues a row, a claim does not consume it, the caps abandon a backlog - and these cover the
// two states around it: a store that is not using the queue at all, and one whose database has
// stopped answering. Both matter operationally. The gate is what keeps every SQLite deployment from
// writing a row per forgotten memory into a table nothing drains, and the failure arms are the
// difference between a drain that retries and one that takes the process down.

// TestOutboxMethodsAreGated walks every entry point with the outbox off. Each must answer as though
// the queue were empty rather than query a table the read-only opens cannot be sure exists.
func TestOutboxMethodsAreGated(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	entries, err := database.ClaimSearchDeletes(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimSearchDeletes: %s", err)
	}

	if len(entries) != 0 {
		t.Errorf("a gated store must claim nothing, got %d entries", len(entries))
	}

	if err := database.ConfirmSearchDeletes(ctx, []int64{1, 2}); err != nil {
		t.Errorf("ConfirmSearchDeletes: %s", err)
	}

	pruned, err := database.PruneSearchOutbox(ctx, QueueBounds{MaxAge: time.Hour, MaxRows: 10})
	if err != nil {
		t.Fatalf("PruneSearchOutbox: %s", err)
	}

	if pruned != 0 {
		t.Errorf("a gated store must prune nothing, got %d", pruned)
	}

	depth, err := database.SearchOutboxDepth(ctx)
	if err != nil {
		t.Fatalf("SearchOutboxDepth: %s", err)
	}

	if depth != 0 {
		t.Errorf("a gated store must report no depth, got %d", depth)
	}

	if bytes := database.searchOutboxBytes(ctx); bytes != 0 {
		t.Errorf("a gated store's queue must occupy nothing, got %d bytes", bytes)
	}
}

// TestConfirmSearchDeletesIgnoresAnEmptyBatch covers the short-circuit: the drain calls this with
// whatever it claimed, and an empty claim must not send a DELETE with no ids.
func TestConfirmSearchDeletesIgnoresAnEmptyBatch(t *testing.T) {
	database := newOutboxTestDB(t)

	if err := database.ConfirmSearchDeletes(context.Background(), nil); err != nil {
		t.Errorf("confirming nothing must be a no-op, got %s", err)
	}
}

// TestClaimSearchDeletesDefaultsItsLimit covers the limit fallback. A caller passing zero is asking
// for the default batch, not for nothing - the opposite reading would make a mis-wired drain look
// like an idle one, which is exactly the failure the depth metric exists to make visible.
func TestClaimSearchDeletesDefaultsItsLimit(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if _, err := database.CreateMemory(ctx, types.Memory{
			Id: id, Body: "x", Significance: 1, TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	if _, err := database.DeleteMemories(ctx, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	entries, err := database.ClaimSearchDeletes(ctx, 0)
	if err != nil {
		t.Fatalf("ClaimSearchDeletes: %s", err)
	}

	if len(entries) != 3 {
		t.Errorf("expected the default batch to claim all 3 queued deletions, got %d", len(entries))
	}
}

// TestOutboxMethodsSurfaceStorageFailures drives every arm against a closed database. The gate is
// enabled first, so each method reaches its query rather than returning on the short-circuit above -
// which is the whole reason this is a separate test from the gated one.
func TestOutboxMethodsSurfaceStorageFailures(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := database.ClaimSearchDeletes(ctx, 10); err == nil {
		t.Error("expected ClaimSearchDeletes to surface the query failure")
	}

	if err := database.ConfirmSearchDeletes(ctx, []int64{1}); err == nil {
		t.Error("expected ConfirmSearchDeletes to surface the query failure")
	}

	if _, err := database.SearchOutboxDepth(ctx); err == nil {
		t.Error("expected SearchOutboxDepth to surface the query failure")
	}

	// The age cap and the row cap are two statements, and the first one's failure returns before the
	// second runs - so both are asked for separately.
	if _, err := database.PruneSearchOutbox(ctx, QueueBounds{MaxAge: time.Hour}); err == nil {
		t.Error("expected the age prune to surface the query failure")
	}

	if _, err := database.PruneSearchOutbox(ctx, QueueBounds{MaxRows: 10}); err == nil {
		t.Error("expected the row-count prune to surface the query failure")
	}

	// searchOutboxBytes is subtracted from a whole-file measurement inside the capacity check, so it
	// reports zero rather than an error: charging the queue's bytes to the store because a count
	// failed would raise capacity pressure and evict live memories.
	if bytes := database.searchOutboxBytes(ctx); bytes != 0 {
		t.Errorf("a failed measurement must charge nothing, got %d bytes", bytes)
	}
}

// TestSearchOutboxBytesGrowsWithTheQueue is the accounting's positive case. The figure exists to be
// subtracted on SQLite, where the whole-file measurement would otherwise let the record of what was
// evicted raise capacity pressure and evict live memories to make room for itself.
func TestSearchOutboxBytesGrowsWithTheQueue(t *testing.T) {
	database := newOutboxTestDB(t)
	ctx := context.Background()

	if empty := database.searchOutboxBytes(ctx); empty != 0 {
		t.Fatalf("an empty queue must occupy nothing, got %d bytes", empty)
	}

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id: "counted", Body: "x", Significance: 1, TimeStamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.DeleteMemories(ctx, []string{"counted"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if bytes := database.searchOutboxBytes(ctx); bytes != outboxRowBytes {
		t.Errorf("expected one row's allowance (%d), got %d", outboxRowBytes, bytes)
	}
}
