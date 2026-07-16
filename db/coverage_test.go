package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestPing verifies the readiness probe reports a live database as reachable and a closed one as
// unreachable, so the health surfaces flip to not-ready once the store is gone.
func TestPing(t *testing.T) {
	db := newTestDB(t)

	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a live database: %s", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("expected Ping to fail after Close")
	}
}

// TestCheckReadOnlyTables verifies the read-only guard passes on an initialised store (events and
// memories present) and errors once a required table is missing - the signal the backfill tool
// uses to refuse a database the service has never initialised.
func TestCheckReadOnlyTables(t *testing.T) {
	db := newTestDB(t)

	if err := db.checkReadOnlyTables(); err != nil {
		t.Fatalf("expected an initialised store to pass, got %s", err)
	}

	if _, err := db.sql.Exec(`DROP TABLE memories`); err != nil {
		t.Fatalf("DROP TABLE memories: %s", err)
	}

	if err := db.checkReadOnlyTables(); err == nil {
		t.Fatal("expected an error once the memories table is missing")
	}
}

// TestReplaceMemoriesWithSummary_RollsBackOnInsertConflict verifies the whole operation is atomic:
// when the summary insert fails (its id collides with a memory on another event, so the delete
// never removed it), the delete of the target event's memories is rolled back and nothing changes.
func TestReplaceMemoriesWithSummary_RollsBackOnInsertConflict(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "target", TimeStart: 100, Significance: 1})
	mustCreateEvent(t, db, types.Event{Id: "e2", Name: "other", TimeStart: 100, Significance: 1})
	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "b"})
	mustCreateMemory(t, db, types.Memory{Id: "keep", TimeStamp: 100, Significance: 1, EventId: "e2", Body: "c"})

	// The summary reuses an id already held by a memory on a different event, so the delete for e1
	// leaves it in place and the insert violates the primary key.
	summary := types.Memory{Id: "keep", TimeStamp: 200, Significance: 5, EventId: "e1", Body: "gist", IsSummary: true}

	if _, err := db.ReplaceMemoriesWithSummary(context.Background(), "e1", summary); err == nil {
		t.Fatal("expected a primary-key conflict on the summary insert")
	}

	// The transaction must have rolled back: e1's original memories survive intact.
	survivors, err := db.GetMemoriesByEventId(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId(e1): %s", err)
	}

	if len(*survivors) != 2 {
		t.Fatalf("expected e1's 2 memories to survive the rollback, got %d", len(*survivors))
	}
}

// TestPurge_RollsBackWhenEventsDeleteFails verifies Purge is atomic: when the events delete fails
// (its table is dropped out from under it), the already-issued memories delete is rolled back and
// the store is left intact rather than half-purged. Dropping the table is a stand-in for any mid-
// transaction failure on the second statement.
func TestPurge_RollsBackWhenEventsDeleteFails(t *testing.T) {
	db := newTestDB(t)

	mustCreateMemory(t, db, types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "a"})
	mustCreateMemory(t, db, types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, Body: "b"})

	// Remove the events table so Purge's second statement (DELETE FROM events) fails after the
	// first (DELETE FROM memories) has run inside the transaction.
	if _, err := db.sql.Exec(`DROP TABLE events`); err != nil {
		t.Fatalf("DROP TABLE events: %s", err)
	}

	if err := db.Purge(context.Background()); err == nil {
		t.Fatal("expected Purge to fail once the events delete errors")
	}

	// The memories delete must have rolled back: both rows survive.
	if with, without := db.CountMemories(context.Background()); with+without != 2 {
		t.Fatalf("expected the memories delete to roll back leaving 2 rows, got %d", with+without)
	}
}

// TestMutations_ErrorOnClosedDB verifies the transaction-opening mutations surface an error (rather
// than panicking) when the database is closed - the beginTx-failure arm each of them guards.
func TestMutations_ErrorOnClosedDB(t *testing.T) {
	db := newTestDB(t)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if err := db.Purge(context.Background()); err == nil {
		t.Error("expected Purge to error on a closed database")
	}

	if _, err := db.DeleteMemories(context.Background(), []string{"m1"}); err == nil {
		t.Error("expected DeleteMemories to error on a closed database")
	}

	if _, err := db.ReplaceMemoriesWithSummary(context.Background(), "e1", types.Memory{Id: "s1", TimeStamp: 1, Significance: 1, Body: "x"}); err == nil {
		t.Error("expected ReplaceMemoriesWithSummary to error on a closed database")
	}
}

// TestUpdateMemory_MissingReportsNotExisting verifies updatedRowExisted's negative arm: updating a
// memory that is not present matches no row and reports non-existence rather than inserting one.
func TestUpdateMemory_MissingReportsNotExisting(t *testing.T) {
	db := newTestDB(t)

	existed, err := db.UpdateMemory(context.Background(), types.Memory{Id: "ghost", Significance: 9})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if existed {
		t.Fatal("expected UpdateMemory of a missing id to report not-existing")
	}

	if getMemory(t, db, "ghost") != nil {
		t.Fatal("UpdateMemory must not insert a missing memory")
	}
}
