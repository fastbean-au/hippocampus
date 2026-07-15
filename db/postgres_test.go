package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// postgresTestDSNEnv names the environment variable carrying the DSN of a disposable Postgres
// database for the integration tests below. When unset, those tests skip: they need a real
// server, unlike the SQLite tests' in-memory database.
const postgresTestDSNEnv = "HIPPOCAMPUS_TEST_POSTGRES_DSN"

// TestRebind verifies the ?-to-$N placeholder conversion, including that the SQLite driver
// passes queries through untouched. Pure string manipulation, so it needs no server.
func TestRebind(t *testing.T) {
	pg := &DB{driver: driverPostgres}
	lite := &DB{driver: driverSQLite}

	tests := []struct {
		name  string
		in    string
		wantP string
	}{
		{name: "no placeholders", in: `SELECT 1`, wantP: `SELECT 1`},
		{name: "single", in: `DELETE FROM memories WHERE id = ?`, wantP: `DELETE FROM memories WHERE id = $1`},
		{
			name:  "multiple",
			in:    `UPDATE memories SET time_recalled = ?, recall_count = ? WHERE id = ?`,
			wantP: `UPDATE memories SET time_recalled = $1, recall_count = $2 WHERE id = $3`,
		},
		{name: "in list", in: `WHERE id IN (?,?,?)`, wantP: `WHERE id IN ($1,$2,$3)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pg.rebind(tt.in); got != tt.wantP {
				t.Errorf("postgres rebind(%q) = %q, want %q", tt.in, got, tt.wantP)
			}

			if got := lite.rebind(tt.in); got != tt.in {
				t.Errorf("sqlite rebind(%q) = %q, want unchanged", tt.in, got)
			}
		})
	}
}

// newPostgresTestDB opens the database named by HIPPOCAMPUS_TEST_POSTGRES_DSN, purging any rows
// left by earlier runs and closing (which releases the instance advisory lock) when the test
// ends. Skips the test when the variable is unset.
func newPostgresTestDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv(postgresTestDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run postgres integration tests", postgresTestDSNEnv)
	}

	database, err := NewPostgres(dsn, true)
	if err != nil {
		t.Fatalf("NewPostgres: %s", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})

	if err := database.Purge(); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	return database
}

// TestPostgres_AdvisoryLockExcludesSecondInstance verifies that a second connection to the same
// database is refused while the first instance holds the advisory lock, and admitted once the
// first instance closes.
func TestPostgres_AdvisoryLockExcludesSecondInstance(t *testing.T) {
	database := newPostgresTestDB(t)

	if _, err := NewPostgres(os.Getenv(postgresTestDSNEnv), true); err == nil {
		t.Fatal("second NewPostgres against the same database should fail while the lock is held")
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := NewPostgres(os.Getenv(postgresTestDSNEnv), true)
	if err != nil {
		t.Fatalf("NewPostgres after Close should succeed: %s", err)
	}

	_ = reopened.Close()
}

// TestPostgres_ReplicaSkipsInstanceLock verifies the horizontal-scaling contract: an
// instance opened with consolidate false does not take the advisory lock, so it can run alongside
// the consolidating instance (which does hold the lock) against the same database, and two such
// replicas can coexist. It also confirms the replica has no keepalive connection to leak.
func TestPostgres_ReplicaSkipsInstanceLock(t *testing.T) {
	// The consolidating instance holds the lock.
	leader := newPostgresTestDB(t)

	// A replica against the same database must open despite the lock being held.
	replica, err := NewPostgres(os.Getenv(postgresTestDSNEnv), false)
	if err != nil {
		t.Fatalf("replica NewPostgres(consolidate=false) should succeed alongside the lock holder: %s", err)
	}

	t.Cleanup(func() { _ = replica.Close() })

	if replica.lockConn != nil {
		t.Error("a replica must not hold the instance lock connection")
	}

	if replica.stopKeepalive != nil {
		t.Error("a replica must not start the instance-lock keepalive")
	}

	// A second replica must also be admitted.
	replica2, err := NewPostgres(os.Getenv(postgresTestDSNEnv), false)
	if err != nil {
		t.Fatalf("a second replica should also open: %s", err)
	}

	_ = replica2.Close()

	// The replica can still read/write the shared database.
	if _, err := replica.CreateEvent(types.Event{Id: "replica-evt", Name: "an event", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("replica CreateEvent: %s", err)
	}

	_ = leader
}

// waitPostgresAdvisoryLockFree blocks until the instance advisory lock can be acquired and then
// released on a single dedicated session, which is only possible once the previous holder's
// session has fully ended. It makes the keepalive recovery test deterministic: after killing the
// lock session, the reacquisition inside verifyInstanceLock succeeds on the first try only if the
// lock is genuinely free by then.
func waitPostgresAdvisoryLockFree(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)

	for {
		conn, err := sqlDB.Conn(ctx)
		if err == nil {
			var got bool

			if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&got); err == nil && got {
				_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey)
				_ = conn.Close()

				return
			}

			_ = conn.Close()
		}

		if time.Now().After(deadline) {
			t.Fatal("advisory lock never freed after terminating the lock session")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// TestPostgres_InstanceLockKeepaliveRecovers is a regression test: when the pinned
// lock session dies (here simulated with pg_terminate_backend, the same effect as a failover or an
// idle-timeout reap), the keepalive's verifyInstanceLock must notice the dead connection and retake
// the advisory lock on a fresh one - so the instance keeps its exclusivity instead of silently
// running lockless. It then confirms the lock is genuinely held again by proving a second instance
// is still excluded.
func TestPostgres_InstanceLockKeepaliveRecovers(t *testing.T) {
	database := newPostgresTestDB(t)

	// Drive verifyInstanceLock deterministically from the test: stop the background keepalive so it
	// cannot fire (and log.Fatal) concurrently.
	close(database.stopKeepalive)
	<-database.keepaliveStopped
	database.stopKeepalive = nil

	ctx := context.Background()

	var pid int
	if err := database.lockConn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("read lock session pid: %s", err)
	}

	// Kill the lock session from a separate pool connection, releasing the advisory lock and
	// breaking the pinned connection.
	if _, err := database.sql.ExecContext(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate lock session: %s", err)
	}

	// Wait until the lock is provably free (the killed backend has exited), so the single
	// reacquisition attempt inside verifyInstanceLock is not racing the backend's teardown.
	waitPostgresAdvisoryLockFree(t, database.sql)

	if err := database.verifyInstanceLock(); err != nil {
		t.Fatalf("verifyInstanceLock should recover a dropped lock session, got: %s", err)
	}

	// The lock is held again on the reacquired connection, so a second instance is still refused.
	if other, err := NewPostgres(os.Getenv(postgresTestDSNEnv), true); err == nil {
		_ = other.Close()
		t.Fatal("after recovery the advisory lock should be held again, but a second instance acquired it")
	}
}

// TestPostgres_BatchedDeleteRespectsRecallGuard exercises the Postgres arm of the batched delete
// (DELETE ... WHERE (id, time_recalled, recall_count) IN (...) RETURNING id) across a
// chunk boundary: it must delete exactly the still-matching snapshots and leave any recalled since
// the scan in place, returning the ids actually deleted.
func TestPostgres_BatchedDeleteRespectsRecallGuard(t *testing.T) {
	database := newPostgresTestDB(t)

	const total = deleteChunkSize + 20

	var snapshot []memoryRecallSnapshot

	for i := 0; i < total; i++ {
		id := fmt.Sprintf("m%05d", i)

		if _, err := database.CreateMemory(types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}

		snapshot = append(snapshot, memoryRecallSnapshot{id: id, timeRecalled: 0, recallCount: 0})
	}

	// Reinforce two memories, one in each chunk, after the snapshot - the guard must protect them.
	protected := []string{"m00002", fmt.Sprintf("m%05d", deleteChunkSize+3)}
	if _, err := database.RecallMemories(protected); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	deleted, err := database.deleteMemoriesIfUnrecalled(snapshot)
	if err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
	}

	if len(deleted) != total-len(protected) {
		t.Errorf("expected %d deletions, got %d", total-len(protected), len(deleted))
	}

	with, without := database.CountMemories()
	if with+without != len(protected) {
		t.Errorf("expected only the %d recalled memories to remain, got %d", len(protected), with+without)
	}
}

// TestPostgres_ReadOnlyOpenBypassesAdvisoryLock verifies the backfill tool's open path: it must
// succeed while a service instance holds the advisory lock, read what that instance wrote (here
// via the backfill's own page query, which also exercises NOT is_binary and a bound LIMIT on the
// Postgres dialect), and leave the lock untouched when it closes.
func TestPostgres_ReadOnlyOpenBypassesAdvisoryLock(t *testing.T) {
	database := newPostgresTestDB(t)

	if _, err := database.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "text"}); err != nil {
		t.Fatalf("CreateMemory(m1): %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, Body: "\x00\x01", IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory(m2): %s", err)
	}

	reader, err := NewPostgresReadOnly(os.Getenv(postgresTestDSNEnv))
	if err != nil {
		t.Fatalf("NewPostgresReadOnly should succeed while the lock is held: %s", err)
	}

	page, err := reader.GetIndexableMemoriesPage("", 10)
	if err != nil {
		t.Fatalf("GetIndexableMemoriesPage: %s", err)
	}

	if len(page) != 1 || page[0].Id != "m1" || page[0].Body != "text" {
		t.Errorf("expected only the non-binary m1 with its body, got %+v", page)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Close (reader): %s", err)
	}

	// The reader never held the lock, so closing it must not have released the instance's.
	if _, err := NewPostgres(os.Getenv(postgresTestDSNEnv), true); err == nil {
		t.Fatal("the advisory lock should still be held after the read-only handle closes")
	}
}

// TestPostgres_EvictionDoesNotFlagEventWhenNothingDeleted is the regression test for the
// eviction nit: EvictMemories used to flag an event consolidated from the *selection*, so an event
// whose only memory was recall-skipped by the delete guard got flagged though nothing was evicted.
// This is deterministically reproduced by a MemoryValue callback that reinforces the memory as a
// side effect - after EvictMemories has snapshotted its recall state - so the guarded delete skips
// it. It needs concurrent connections (the recall runs while the eviction scan cursor is open), so
// it is a Postgres test, not SQLite (whose single connection would deadlock).
func TestPostgres_EvictionDoesNotFlagEventWhenNothingDeleted(t *testing.T) {
	database := newPostgresTestDB(t)

	if _, err := database.CreateEvent(types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	recalled := false
	server := &decisionServer{
		value: func(MemoryConsolidationCandidate) float64 {
			if !recalled {
				// Reinforce m1 so its live recall state no longer matches the snapshot the eviction
				// scan just took; the guarded delete will skip it.
				_, _ = database.RecallMemories([]string{"m1"})
				recalled = true
			}

			return 0 // lowest value, so m1 is selected for eviction
		},
	}

	memories, events, _, err := database.EvictMemories(server, 1<<30)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if memories != 0 || events != 0 {
		t.Fatalf("expected nothing evicted (m1 recall-skipped), got %d memories / %d events", memories, events)
	}

	event, err := database.GetEvent("e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if event.MemoriesConsolidated {
		t.Error("event was flagged consolidated though none of its memories were actually evicted")
	}
}

// TestPostgres_UpdateNoOpValueReportsExists guards the single-statement UpdateEvent's existence
// semantics on Postgres, where RowsAffected counts matched rows so a same-value update still reports
// the row exists.
func TestPostgres_UpdateNoOpValueReportsExists(t *testing.T) {
	database := newPostgresTestDB(t)

	if _, err := database.CreateEvent(types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	ok, err := database.UpdateEvent(types.Event{Id: "e1", Significance: 5})
	if err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	}

	if !ok {
		t.Error("a same-value update of an existing event must report it exists")
	}
}

// TestPostgres_ReadOnlyOpenFailsFastWithoutTables is a regression test: the
// read-only open must run no schema DDL (which would take an ACCESS EXCLUSIVE lock contending with a
// live service). Against a database with no tables it must fail fast and, crucially, must not have
// created them.
func TestPostgres_ReadOnlyOpenFailsFastWithoutTables(t *testing.T) {
	database := newPostgresTestDB(t)

	// Simulate a database the service has never initialised.
	if _, err := database.sql.Exec(`DROP TABLE IF EXISTS memories, events`); err != nil {
		t.Fatalf("drop tables: %s", err)
	}

	// Restore the shared schema for later tests regardless of the assertions below.
	t.Cleanup(func() {
		if err := database.initPostgresSchema(); err != nil {
			t.Fatalf("restore schema: %s", err)
		}
	})

	reader, err := NewPostgresReadOnly(os.Getenv(postgresTestDSNEnv))
	if err == nil {
		_ = reader.Close()
		t.Fatal("NewPostgresReadOnly should fail fast when the tables do not exist")
	}

	// The failed open must not have created the tables - a read-only tool runs no DDL.
	var exists bool
	if err := database.sql.QueryRow(
		`SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'memories')`,
	).Scan(&exists); err != nil {
		t.Fatalf("probe tables: %s", err)
	}

	if exists {
		t.Error("the read-only open created the memories table; it must run no DDL")
	}
}

// TestPostgres_UsedBytesAndEviction verifies the live-row byte accounting behind
// consolidation.capacityBytes on Postgres: the reading is exactly payload bytes plus the fixed
// per-row allowance eviction uses when estimating freed bytes, and — unlike a file-size measure —
// it drops the moment rows are deleted, by exactly the eviction's own estimate.
func TestPostgres_UsedBytesAndEviction(t *testing.T) {
	database := newPostgresTestDB(t)

	used, err := database.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes (empty): %s", err)
	}

	if used != 0 {
		t.Fatalf("expected 0 used bytes in an empty store, got %d", used)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: strings.Repeat("a", 1000)}); err != nil {
		t.Fatalf("CreateMemory(m1): %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m2", TimeStamp: 100, Significance: 5, Body: strings.Repeat("b", 3000)}); err != nil {
		t.Fatalf("CreateMemory(m2): %s", err)
	}

	used, err = database.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	want := int64(1000 + 3000 + 2*evictionRowOverheadBytes)

	if used != want {
		t.Fatalf("UsedBytes = %d, want %d (body bytes plus the per-row allowance)", used, want)
	}

	// Rank m1 (significance 1) below m2 so a 1-byte request evicts exactly m1.
	server := &decisionServer{value: func(candidate MemoryConsolidationCandidate) float64 {
		return float64(candidate.MemorySignificance)
	}}

	memories, events, freed, err := database.EvictMemories(server, 1)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if memories != 1 || events != 0 {
		t.Fatalf("expected exactly 1 memory evicted, got %d memories and %d events", memories, events)
	}

	remaining, err := database.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes (after eviction): %s", err)
	}

	// The two accountings must converge: the eviction's estimated freed bytes are exactly the
	// drop in the reading, so eviction can never chase a figure that does not move.
	if remaining != used-freed {
		t.Errorf("UsedBytes after eviction = %d, want %d (%d - %d freed)", remaining, used-freed, used, freed)
	}

	// Events contribute too - their payload plus the same per-row allowance.
	if _, err := database.CreateEvent(types.Event{Id: "e1", Name: "sized event", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	withEvent, err := database.UsedBytes()
	if err != nil {
		t.Fatalf("UsedBytes (with event): %s", err)
	}

	if withEvent <= remaining {
		t.Errorf("UsedBytes should grow when an event is added: %d -> %d", remaining, withEvent)
	}
}

// TestPostgres_MemoryAndEventRoundTrip exercises the CRUD surface end to end on Postgres: create,
// upsert, recall reinforcement (UPDATE ... RETURNING), range queries, and the counts.
func TestPostgres_MemoryAndEventRoundTrip(t *testing.T) {
	database := newPostgresTestDB(t)

	if _, err := database.CreateEvent(types.Event{Id: "e1", Name: "event one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 3, EventId: "e1", Body: "hello"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m2", TimeStamp: 200, Significance: 7, Body: "loose"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// The conditional UPDATE exercises the partial-overwrite semantics: only non-zero fields may
	// overwrite, so the name must change while significance survives.
	if ok, err := database.UpdateEvent(types.Event{Id: "e1", Name: "renamed event"}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	} else if !ok {
		t.Fatal("UpdateEvent reported the existing event as missing")
	}

	event, err := database.GetEvent("e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if event.Name != "renamed event" || event.Significance != 5 {
		t.Errorf("GetEvent after upsert = (%q, %d), want ('renamed event', 5)", event.Name, event.Significance)
	}

	memories, err := database.RecallMemories([]string{"m1"})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].RecallCount != 1 || (*memories)[0].Body != "hello" {
		t.Errorf("RecallMemories should return the reinforced memory, got %+v", *memories)
	}

	if ok, err := database.UpdateMemory(types.Memory{Id: "m2", Significance: 9}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	ranged, err := database.GetMemories(MemoryFilter{TimeStampMin: 150, SignificanceMin: 8})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*ranged) != 1 || (*ranged)[0].Id != "m2" || (*ranged)[0].Body != "loose" {
		t.Errorf("range query should return only the upserted m2, got %+v", *ranged)
	}

	with, without := database.CountMemories()
	if with != 1 || without != 1 {
		t.Errorf("CountMemories = (%d, %d), want (1, 1)", with, without)
	}

	if count := database.CountEvents(); count != 1 {
		t.Errorf("CountEvents = %d, want 1", count)
	}
}

// TestPostgres_ConsolidationAndSummarization drives the sleep-cycle scan surface on Postgres:
// the loose-memory and evented-memory consolidation passes (including the atomic
// re-check-before-delete primitives), the summarization candidate query (which uses the
// GREATEST dialect branch), and summary replacement.
func TestPostgres_ConsolidationAndSummarization(t *testing.T) {
	database := newPostgresTestDB(t)

	if _, err := database.CreateEvent(types.Event{Id: "e1", Name: "quiet event", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := database.CreateMemory(types.Memory{Id: id, TimeStamp: 100, Significance: 3, EventId: "e1", Body: "evented"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	if _, err := database.CreateMemory(types.Memory{Id: "loose", TimeStamp: 100, Significance: 3, Body: "loose"}); err != nil {
		t.Fatalf("CreateMemory(loose): %s", err)
	}

	candidates, err := database.FindSummarizationCandidates(3, time.Now().UnixNano(), 10)
	if err != nil {
		t.Fatalf("FindSummarizationCandidates: %s", err)
	}

	if len(candidates) != 1 || candidates[0].EventId != "e1" || candidates[0].MemoryCount != 3 {
		t.Errorf("expected e1 as the sole candidate with 3 memories, got %+v", candidates)
	}

	replaced, err := database.ReplaceMemoriesWithSummary("e1", types.Memory{Id: "sum", TimeStamp: 300, Significance: 5, EventId: "e1", Body: "summary", IsSummary: true})
	if err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	if replaced != 3 {
		t.Errorf("ReplaceMemoriesWithSummary replaced %d memories, want 3", replaced)
	}

	// The summary memory is flagged is_summary, so the event must no longer be a candidate.
	candidates, err = database.FindSummarizationCandidates(1, time.Now().UnixNano(), 10)
	if err != nil {
		t.Fatalf("FindSummarizationCandidates after replacement: %s", err)
	}

	if len(candidates) != 0 {
		t.Errorf("summarized event should not reappear as a candidate, got %+v", candidates)
	}

	// Consolidate everything: the loose pass deletes 'loose', the evented pass deletes 'sum' and
	// then the event with it via DeleteEventIfEmpty.
	server := &stubServer{consolidateMemories: true, consolidateEvents: true}

	if deleted, err := database.ConsolidateMemories(server); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	} else if deleted != 1 {
		t.Errorf("ConsolidateMemories deleted %d, want 1", deleted)
	}

	memoriesDeleted, eventsSeen, eventsDeleted, err := database.ConsolidateEventMemories(server)
	if err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err)
	}

	if memoriesDeleted != 1 || eventsSeen != 1 || eventsDeleted != 1 {
		t.Errorf("ConsolidateEventMemories = (%d, %d, %d), want (1, 1, 1)", memoriesDeleted, eventsSeen, eventsDeleted)
	}

	with, without := database.CountMemories()
	if with != 0 || without != 0 {
		t.Errorf("CountMemories after consolidation = (%d, %d), want (0, 0)", with, without)
	}

	if count := database.CountEvents(); count != 0 {
		t.Errorf("CountEvents after consolidation = %d, want 0", count)
	}
}
