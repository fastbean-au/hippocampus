package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestNew_CreatesMissingDirectory verifies New creates the storage directory when it does not yet
// exist, rather than requiring the caller to pre-create it (the os.IsNotExist -> MkdirAll branch).
func TestNew_CreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "storage")

	d, err := New(dir)
	if err != nil {
		t.Fatalf("New with a missing directory: %s", err)
	}
	defer func() { _ = d.Close() }()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory after directory creation: %s", err)
	}
}

// TestNew_DirectoryCreationFails verifies New surfaces the MkdirAll error rather than panicking
// when the storage path cannot be created - here because a path component is a regular file, so
// no directory can ever be created at that location.
func TestNew_DirectoryCreationFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %s", err)
	}

	// blocker is a file, so a directory can never be created underneath it.
	if _, err := New(filepath.Join(blocker, "sub")); err == nil {
		t.Error("expected New to fail when the storage directory cannot be created")
	}
}

// TestNewSQLiteReadOnly_EmptyDirectory verifies the read-only open refuses an empty directory
// (there is no in-memory read-only mode - it always names a real file on disk).
func TestNewSQLiteReadOnly_EmptyDirectory(t *testing.T) {
	if _, err := NewSQLiteReadOnly(""); err == nil {
		t.Error("expected NewSQLiteReadOnly to reject an empty directory")
	}
}

// TestVerifyInstanceLock_ReacquiresOnDeadConnection drives verifyInstanceLock's failure arm: a
// lock connection that has died fails its ping, is dropped, and - for a driver with no server-lock
// reacquisition branch (the switch's default case, exercised here via driverSQLite standing in for
// "neither postgres nor mysql") - the method returns nil having simply cleared the dead connection.
// The Postgres/MySQL reacquisition branches themselves need a live server and are covered by the
// integration tests.
func TestVerifyInstanceLock_ReacquiresOnDeadConnection(t *testing.T) {
	d := newTestDB(t)

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %s", err)
	}

	// Kill the pinned connection out from under the lock so its next ping fails.
	if err := conn.Close(); err != nil {
		t.Fatalf("Close pinned conn: %s", err)
	}

	d.lockConn = conn
	d.driver = driverSQLite

	if err := d.verifyInstanceLock(); err != nil {
		t.Errorf("verifyInstanceLock with a dead connection (non-server driver) = %s, want nil", err)
	}

	if d.lockConn != nil {
		t.Error("expected the dead lock connection to be cleared")
	}
}

// TestVerifyInstanceLock_HealthyConnection verifies the happy path: a live pinned connection pings
// successfully and is left untouched.
func TestVerifyInstanceLock_HealthyConnection(t *testing.T) {
	d := newTestDB(t)

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %s", err)
	}
	defer func() { _ = conn.Close() }()

	d.lockConn = conn

	if err := d.verifyInstanceLock(); err != nil {
		t.Errorf("verifyInstanceLock with a healthy connection = %s, want nil", err)
	}

	if d.lockConn != conn {
		t.Error("expected the healthy lock connection to be left in place")
	}

	// Detach before newTestDB's cleanup closes the *sql.DB, so Close doesn't try to close conn
	// twice via the keepalive-less path.
	d.lockConn = nil
}

// TestClose_StopsKeepaliveAndReleasesLockConn drives Close's instance-lock teardown: it starts the
// real keepalive goroutine against a pinned connection (standing in for the Postgres/MySQL lock
// connection, which SQLite doesn't have in production) and verifies Close signals it to stop, waits
// for it to exit, and releases the connection - all without leaking the goroutine or double-closing.
func TestClose_StopsKeepaliveAndReleasesLockConn(t *testing.T) {
	d := newTestDB(t)

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %s", err)
	}

	// SQLite's pool is capped at one connection (see db.go); checking conn out of the pool as the
	// stand-in lock connection leaves none free for Close's Preserve step to use. Mark the DB
	// read-only so Preserve is a no-op (mirroring how it behaves for a real read-only open) rather
	// than deadlocking waiting for a connection this test is intentionally holding.
	d.readOnly = true
	d.lockConn = conn
	d.startLockKeepalive()

	if d.stopKeepalive == nil || d.keepaliveStopped == nil {
		t.Fatal("expected startLockKeepalive to set up its channels")
	}

	done := make(chan error, 1)

	go func() { done <- d.Close() }()

	select {

	case err := <-done:
		if err != nil {
			t.Errorf("Close: %s", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return - the keepalive goroutine likely did not stop")
	}

	if d.lockConn != nil {
		t.Error("expected lockConn to be cleared by Close")
	}
}

// TestStartLockKeepalive_NoOpWithoutLockConn verifies the keepalive is inert when there is no lock
// connection to guard (SQLite in production, and any read-only open).
func TestStartLockKeepalive_NoOpWithoutLockConn(t *testing.T) {
	d := &DB{}

	d.startLockKeepalive()

	if d.stopKeepalive != nil || d.keepaliveStopped != nil {
		t.Error("expected startLockKeepalive to be a no-op with no lock connection")
	}
}

// TestEvictMemories_RetainedMemoryExcludedButKeepsEventAlive verifies the retention floor: a
// memory MemoryRetained flags is excluded from the eviction candidate pool even though the store is
// over its byte target, yet still counts toward its event's memory total - so the event is not
// deleted out from under the retained memory even after every other memory on it is evicted.
func TestEvictMemories_RetainedMemoryExcludedButKeepsEventAlive(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "an event", TimeStart: 100, Significance: 50}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	memories := []types.Memory{
		{Id: "retained", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "keep-me"},
		{Id: "evictable", TimeStamp: 100, Significance: 2, EventId: "e1", Body: "take-me"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory %s: %s", m.Id, err)
		}
	}

	server := &decisionServer{
		value: func(candidate MemoryConsolidationCandidate) float64 {
			return float64(candidate.MemorySignificance)
		},
		retained: func(candidate MemoryConsolidationCandidate) bool {
			return candidate.MemorySignificance == 1
		},
	}

	// A large request would normally empty the event, but the retained memory must survive and keep
	// e1 alive.
	deletedMemories, deletedEvents, _, err := db.EvictMemories(context.Background(), server, 1<<30)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if deletedMemories != 1 || deletedEvents != 0 {
		t.Fatalf("expected 1 memory deleted and the event to survive, got %d memories, %d events", deletedMemories, deletedEvents)
	}

	if getMemory(t, db, "retained") == nil {
		t.Error("the retained memory must survive eviction")
	}

	if getMemory(t, db, "evictable") != nil {
		t.Error("the non-retained memory should have been evicted")
	}

	event, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: expected e1 to survive (kept alive by the retained memory): %s", err)
	}

	if !event.MemoriesConsolidated {
		t.Error("e1 should be flagged consolidated after losing its evictable memory")
	}
}

// TestImportMemories_EmptyInput and TestImportEvents_EmptyInput verify the empty-slice
// short-circuit reports zero writes without opening a transaction.
func TestImportMemories_EmptyInput(t *testing.T) {
	db := newTestDB(t)

	n, err := db.ImportMemories(context.Background(), nil)
	if err != nil || n != 0 {
		t.Errorf("ImportMemories(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestImportEvents_EmptyInput(t *testing.T) {
	db := newTestDB(t)

	n, err := db.ImportEvents(context.Background(), nil)
	if err != nil || n != 0 {
		t.Errorf("ImportEvents(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestImportMemories_UnrankedSignificance verifies importLevelID's non-positive-significance arm:
// a memory imported with significance 0 lands unranked (nil level id) rather than creating a
// spurious level.
func TestImportMemories_UnrankedSignificance(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.ImportMemories(context.Background(), []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 0, Body: "unranked"},
	}); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	m := getMemory(t, db, "m1")
	if m == nil {
		t.Fatal("expected m1 to be imported")
	}

	if m.Significance != 0 || m.SignificanceLevelID != nil {
		t.Errorf("expected an unranked import, got significance=%d levelID=%v", m.Significance, m.SignificanceLevelID)
	}
}

// TestSignificanceLevelsDDL_PerDialect verifies the registry table DDL differs correctly across
// the three dialects: the auto-increment/identity syntax genuinely diverges (Postgres's
// GENERATED BY DEFAULT AS IDENTITY, MySQL's AUTO_INCREMENT, SQLite's INTEGER PRIMARY KEY
// AUTOINCREMENT), and a wrong choice would break schema creation on that backend without any
// SQLite-only test ever catching it.
func TestSignificanceLevelsDDL_PerDialect(t *testing.T) {
	cases := []struct {
		driver driver
		want   string
	}{
		{driverSQLite, "AUTOINCREMENT"},
		{driverPostgres, "GENERATED BY DEFAULT AS IDENTITY"},
		{driverMySQL, "AUTO_INCREMENT"},
	}

	for _, c := range cases {
		d := &DB{driver: c.driver}

		ddl := d.significanceLevelsDDL()

		if !contains(ddl, c.want) {
			t.Errorf("driver %d DDL missing %q: %s", c.driver, c.want, ddl)
		}

		if !contains(ddl, "level_rank INTEGER NOT NULL UNIQUE") {
			t.Errorf("driver %d DDL missing the shared level_rank column: %s", c.driver, ddl)
		}
	}
}

func contains(haystack string, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}

		return false
	})()
}

// TestPurge_RollsBackWhenMemoriesDeleteFails verifies Purge's first statement failing (memories
// table gone) rolls back cleanly - the events and significance_levels deletes never ran, so there
// is nothing to undo, but Purge must still surface the error rather than silently continuing.
func TestPurge_RollsBackWhenMemoriesDeleteFails(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := db.sql.Exec(`DROP TABLE memories`); err != nil {
		t.Fatalf("DROP TABLE memories: %s", err)
	}

	if err := db.Purge(context.Background()); err == nil {
		t.Fatal("expected Purge to fail once the memories delete errors")
	}

	if exists, err := db.EventExists(context.Background(), "e1"); err != nil || !exists {
		t.Errorf("expected e1 to survive the rolled-back purge, got exists=%v err=%v", exists, err)
	}
}

// TestPurge_RollsBackWhenSignificanceLevelsDeleteFails verifies Purge's third statement failing
// (the registry table gone) rolls back the memories and events deletes that already ran in the
// same transaction, leaving the store untouched.
func TestPurge_RollsBackWhenSignificanceLevelsDeleteFails(t *testing.T) {
	db := newTestDB(t)

	createMemoryWithSignificance(t, db, "m1", 5)

	if _, err := db.sql.Exec(`DROP TABLE significance_levels`); err != nil {
		t.Fatalf("DROP TABLE significance_levels: %s", err)
	}

	if err := db.Purge(context.Background()); err == nil {
		t.Fatal("expected Purge to fail once the significance_levels delete errors")
	}

	// significance_levels is gone, so the normal joined read path can't be used; check the raw
	// table directly.
	var count int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, "m1").Scan(&count); err != nil {
		t.Fatalf("count memories: %s", err)
	}

	if count != 1 {
		t.Error("expected m1 to survive the rolled-back purge")
	}
}

// TestAddColumnIfMissing_AlterTableFails verifies the ALTER TABLE error is surfaced rather than
// swallowed: pragma_table_info on a nonexistent table returns zero rows (not an error), so the
// "column missing" branch is taken, and the ALTER TABLE against a table that doesn't exist fails.
func TestAddColumnIfMissing_AlterTableFails(t *testing.T) {
	db := newTestDB(t)

	if err := db.addColumnIfMissing("no_such_table", "extra", "INTEGER NOT NULL DEFAULT 0"); err == nil {
		t.Error("expected addColumnIfMissing to fail adding a column to a nonexistent table")
	}
}

// TestAddColumnIfMissing_MySQLProbeSelection verifies the MySQL arm selects the
// information_schema probe (rather than SQLite's pragma_table_info) - driving it against the
// test's actual SQLite backend, where that probe fails, so the branch selection itself is
// exercised even though the full MySQL path needs a real server (covered by mysql_test.go).
func TestAddColumnIfMissing_MySQLProbeSelection(t *testing.T) {
	db := newTestDB(t)
	db.driver = driverMySQL

	if err := db.addColumnIfMissing("memories", "extra", "INTEGER NOT NULL DEFAULT 0"); err == nil {
		t.Error("expected the MySQL information_schema probe to fail against a SQLite backend")
	}
}
