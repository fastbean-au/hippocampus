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

// mysqlTestDSNEnv names the environment variable carrying the DSN of a disposable MySQL database
// for the integration tests below. When unset, those tests skip: they need a real server, unlike
// the SQLite tests' in-memory database. The DSN is go-sql-driver format, e.g.
// hippocampus:hippocampus@tcp(localhost:3306)/hippocampus_test
const mysqlTestDSNEnv = "HIPPOCAMPUS_TEST_MYSQL_DSN"

// newMySQLTestDB opens the database named by HIPPOCAMPUS_TEST_MYSQL_DSN, purging any rows left by
// earlier runs and closing (which releases the instance lock) when the test ends. Skips the test
// when the variable is unset.
func newMySQLTestDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv(mysqlTestDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run mysql integration tests", mysqlTestDSNEnv)
	}

	database, err := NewMySQL(dsn, true)
	if err != nil {
		t.Fatalf("NewMySQL: %s", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})

	if err := database.Purge(); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	return database
}

// TestMySQL_InstanceLockExcludesSecondInstance verifies that a second connection to the same
// database is refused while the first instance holds the GET_LOCK lock, and admitted once the
// first instance closes.
func TestMySQL_InstanceLockExcludesSecondInstance(t *testing.T) {
	database := newMySQLTestDB(t)

	if _, err := NewMySQL(os.Getenv(mysqlTestDSNEnv), true); err == nil {
		t.Fatal("second NewMySQL against the same database should fail while the lock is held")
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := NewMySQL(os.Getenv(mysqlTestDSNEnv), true)
	if err != nil {
		t.Fatalf("NewMySQL after Close should succeed: %s", err)
	}

	_ = reopened.Close()
}

// TestMySQL_ReplicaSkipsInstanceLock verifies the horizontal-scaling contract: an instance
// opened with consolidate false does not take the GET_LOCK lock, so it can run alongside the
// consolidating instance (which does hold the lock) against the same database, and two such replicas
// can coexist. It also confirms the replica has no keepalive connection to leak.
func TestMySQL_ReplicaSkipsInstanceLock(t *testing.T) {
	// The consolidating instance holds the lock.
	leader := newMySQLTestDB(t)

	// A replica against the same database must open despite the lock being held.
	replica, err := NewMySQL(os.Getenv(mysqlTestDSNEnv), false)
	if err != nil {
		t.Fatalf("replica NewMySQL(consolidate=false) should succeed alongside the lock holder: %s", err)
	}

	t.Cleanup(func() { _ = replica.Close() })

	if replica.lockConn != nil {
		t.Error("a replica must not hold the instance lock connection")
	}

	if replica.stopKeepalive != nil {
		t.Error("a replica must not start the instance-lock keepalive")
	}

	// A second replica must also be admitted.
	replica2, err := NewMySQL(os.Getenv(mysqlTestDSNEnv), false)
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

// waitMySQLInstanceLockFree blocks until the schema-scoped instance lock can be acquired and then
// released on a single dedicated session, which is only possible once the previous holder's session
// has fully ended. It makes the keepalive recovery test deterministic (see the Postgres
// counterpart). The lock name mirrors acquireMySQLInstanceLock: CONCAT('hippocampus:', DATABASE()).
func waitMySQLInstanceLockFree(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)

	for {
		conn, err := sqlDB.Conn(ctx)
		if err == nil {
			var got sql.NullInt64

			if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(CONCAT('hippocampus:', DATABASE()), 0)").Scan(&got); err == nil && got.Valid && got.Int64 == 1 {
				_, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(CONCAT('hippocampus:', DATABASE()))")
				_ = conn.Close()

				return
			}

			_ = conn.Close()
		}

		if time.Now().After(deadline) {
			t.Fatal("instance lock never freed after killing the lock session")
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// TestMySQL_InstanceLockKeepaliveRecovers is a regression test on MySQL: when the
// pinned lock session dies (here via KILL, the same effect as a wait_timeout reap or a failover),
// the keepalive's verifyInstanceLock must notice the dead connection and retake the GET_LOCK lock on
// a fresh one, keeping the instance exclusive rather than running lockless. It then confirms the
// lock is genuinely held again by proving a second instance is still excluded.
func TestMySQL_InstanceLockKeepaliveRecovers(t *testing.T) {
	database := newMySQLTestDB(t)

	// Drive verifyInstanceLock deterministically from the test: stop the background keepalive so it
	// cannot fire (and log.Fatal) concurrently.
	close(database.stopKeepalive)
	<-database.keepaliveStopped
	database.stopKeepalive = nil

	ctx := context.Background()

	var connID int64
	if err := database.lockConn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connID); err != nil {
		t.Fatalf("read lock session connection id: %s", err)
	}

	// Kill the lock session from a separate pool connection, releasing the named lock and breaking
	// the pinned connection. KILL does not accept a placeholder, so the id is formatted in (it comes
	// from CONNECTION_ID(), not user input).
	if _, err := database.sql.ExecContext(ctx, fmt.Sprintf("KILL %d", connID)); err != nil {
		t.Fatalf("kill lock session: %s", err)
	}

	// Wait until the lock is provably free (the killed session has gone), so verifyInstanceLock's
	// single reacquisition attempt is not racing the teardown.
	waitMySQLInstanceLockFree(t, database.sql)

	if err := database.verifyInstanceLock(); err != nil {
		t.Fatalf("verifyInstanceLock should recover a dropped lock session, got: %s", err)
	}

	// The lock is held again on the reacquired connection, so a second instance is still refused.
	if other, err := NewMySQL(os.Getenv(mysqlTestDSNEnv), true); err == nil {
		_ = other.Close()
		t.Fatal("after recovery the instance lock should be held again, but a second instance acquired it")
	}
}

// TestMySQL_BatchedDeleteRespectsRecallGuard exercises the MySQL arm of the batched delete
// (SELECT ... FOR UPDATE over the row-value guard, then DELETE by id, since MySQL has no
// DELETE ... RETURNING) across a chunk boundary: it must delete exactly the
// still-matching snapshots and protect any recalled since the scan.
func TestMySQL_BatchedDeleteRespectsRecallGuard(t *testing.T) {
	database := newMySQLTestDB(t)

	const total = deleteChunkSize + 20

	var snapshot []memoryRecallSnapshot

	for i := 0; i < total; i++ {
		id := fmt.Sprintf("m%05d", i)

		if _, err := database.CreateMemory(types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}

		snapshot = append(snapshot, memoryRecallSnapshot{id: id, timeRecalled: 0, recallCount: 0})
	}

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

// TestMySQL_ReadOnlyOpenBypassesInstanceLock verifies the backfill tool's open path: it must
// succeed while a service instance holds the instance lock, read what that instance wrote (here
// via the backfill's own page query, which also exercises NOT is_binary and a bound LIMIT on the
// MySQL dialect), and leave the lock untouched when it closes.
func TestMySQL_ReadOnlyOpenBypassesInstanceLock(t *testing.T) {
	database := newMySQLTestDB(t)

	if _, err := database.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "text"}); err != nil {
		t.Fatalf("CreateMemory(m1): %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, Body: "\x00\x01", IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory(m2): %s", err)
	}

	reader, err := NewMySQLReadOnly(os.Getenv(mysqlTestDSNEnv))
	if err != nil {
		t.Fatalf("NewMySQLReadOnly should succeed while the lock is held: %s", err)
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
	if _, err := NewMySQL(os.Getenv(mysqlTestDSNEnv), true); err == nil {
		t.Fatal("the instance lock should still be held after the read-only handle closes")
	}
}

// TestMySQL_UpdateNoOpValueReportsExists is the key regression test for the update nit on
// MySQL: a same-value UPDATE reports 0 changed rows there, so the single-statement UpdateEvent must
// fall back to an existence probe to avoid misreporting the existing event as missing.
func TestMySQL_UpdateNoOpValueReportsExists(t *testing.T) {
	database := newMySQLTestDB(t)

	if _, err := database.CreateEvent(types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	// Significance is already 5, so MySQL's UPDATE changes 0 rows; the fallback probe must still find
	// the event and report it exists.
	ok, err := database.UpdateEvent(types.Event{Id: "e1", Significance: 5})
	if err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	}

	if !ok {
		t.Error("a same-value update of an existing event must report it exists on MySQL (fallback probe)")
	}

	// An unknown id still reports missing.
	if ok, err := database.UpdateEvent(types.Event{Id: "ghost", Significance: 5}); err != nil || ok {
		t.Errorf("update of a missing event = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestMySQL_ReadOnlyOpenFailsFastWithoutTables is a regression test: the read-only
// open must run no schema DDL (which could trigger a long collation MODIFY rebuild contending with a
// live service). Against a database with no tables it must fail fast and must not have created them.
func TestMySQL_ReadOnlyOpenFailsFastWithoutTables(t *testing.T) {
	database := newMySQLTestDB(t)

	if _, err := database.sql.Exec(`DROP TABLE IF EXISTS memories, events`); err != nil {
		t.Fatalf("drop tables: %s", err)
	}

	t.Cleanup(func() {
		if err := database.initMySQLSchema(); err != nil {
			t.Fatalf("restore schema: %s", err)
		}
	})

	reader, err := NewMySQLReadOnly(os.Getenv(mysqlTestDSNEnv))
	if err == nil {
		_ = reader.Close()
		t.Fatal("NewMySQLReadOnly should fail fast when the tables do not exist")
	}

	var count int
	if err := database.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'memories'`,
	).Scan(&count); err != nil {
		t.Fatalf("probe tables: %s", err)
	}

	if count != 0 {
		t.Error("the read-only open created the memories table; it must run no DDL")
	}
}

// TestMySQL_UsedBytesAndEviction verifies the live-row byte accounting behind
// consolidation.capacityBytes on MySQL: the reading is exactly payload bytes plus the fixed
// per-row allowance eviction uses when estimating freed bytes, and — unlike a file-size measure —
// it drops the moment rows are deleted, by exactly the eviction's own estimate.
func TestMySQL_UsedBytesAndEviction(t *testing.T) {
	database := newMySQLTestDB(t)

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

// TestMySQL_MemoryAndEventRoundTrip exercises the CRUD surface end to end on MySQL: create,
// upsert (the ON DUPLICATE KEY UPDATE dialect branch), recall reinforcement (the transactional
// update-then-select branch, since MySQL has no UPDATE ... RETURNING), range queries, and the
// counts.
func TestMySQL_MemoryAndEventRoundTrip(t *testing.T) {
	database := newMySQLTestDB(t)

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

	if (*memories)[0].TimeRecalled == 0 {
		t.Error("RecallMemories should return the reinforced recall time, got 0")
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

// TestMySQL_IdsAreCaseSensitive is a regression test: ids differing only in case
// must be distinct keys on MySQL, matching SQLite and Postgres. Under the server default collation
// (utf8mb4_0900_ai_ci, case- and accent-insensitive) "a" and "A" were the same primary key, so the
// second create collided; the id/event_id/group_name columns are now COLLATE utf8mb4_bin.
func TestMySQL_IdsAreCaseSensitive(t *testing.T) {
	database := newMySQLTestDB(t)

	if _, err := database.CreateEvent(types.Event{Id: "e", Name: "lower", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent(e): %s", err)
	}

	if _, err := database.CreateEvent(types.Event{Id: "E", Name: "upper", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent(E) should not collide with 'e': %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "a", TimeStamp: 100, Significance: 1, Body: "lower"}); err != nil {
		t.Fatalf("CreateMemory(a): %s", err)
	}

	if _, err := database.CreateMemory(types.Memory{Id: "A", TimeStamp: 100, Significance: 1, Body: "upper"}); err != nil {
		t.Fatalf("CreateMemory(A) should not collide with 'a': %s", err)
	}

	// Both memories must exist as distinct rows carrying their own bodies.
	if with, without := database.CountMemories(); with+without != 2 {
		t.Fatalf("expected 2 distinct memories, got %d", with+without)
	}

	lower, err := database.GetMemoriesByIds([]string{"a"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds(a): %s", err)
	}

	if len(*lower) != 1 || (*lower)[0].Body != "lower" {
		t.Errorf("expected memory 'a' with body 'lower', got %+v", *lower)
	}

	upper, err := database.GetMemoriesByIds([]string{"A"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds(A): %s", err)
	}

	if len(*upper) != 1 || (*upper)[0].Body != "upper" {
		t.Errorf("expected memory 'A' with body 'upper', got %+v", *upper)
	}

	// Events differing only in case must likewise be distinct.
	if lowerEvent, err := database.GetEvent("e"); err != nil || lowerEvent.Name != "lower" {
		t.Errorf("expected event 'e' named 'lower', got %+v (%v)", lowerEvent, err)
	}

	if upperEvent, err := database.GetEvent("E"); err != nil || upperEvent.Name != "upper" {
		t.Errorf("expected event 'E' named 'upper', got %+v (%v)", upperEvent, err)
	}
}

// TestMySQL_ConsolidationAndSummarization drives the sleep-cycle scan surface on MySQL: the
// loose-memory and evented-memory consolidation passes (including the atomic
// re-check-before-delete primitives), the summarization candidate query (which uses the GREATEST
// dialect branch), and summary replacement.
func TestMySQL_ConsolidationAndSummarization(t *testing.T) {
	database := newMySQLTestDB(t)

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
