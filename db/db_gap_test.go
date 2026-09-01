package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// This file targets db.go's remaining coverage gaps not reached by the existing SQLite-backed
// tests: os-level failure injection for New/WALBytes, and mocked-handle failure injection for
// initSchema/UsedBytes/Preserve/Close/Purge branches a real SQLite connection can't be made to
// fail on demand.

// --- New: the directory-exists-but-MkdirAll-fails branch (distinct from
// TestNew_DirectoryCreationFails, which never reaches MkdirAll at all: stat on a path under a
// blocking file returns ENOTDIR, not IsNotExist, so the directory branch is skipped entirely and
// the failure actually surfaces later, opening the sqlite file). Here the parent exists but is
// read-only, so os.Stat correctly reports the child missing and MkdirAll then fails on
// permission. ---

func TestNew_MkdirAllPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits are not enforced the same way on windows")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission")
	}

	parent := t.TempDir()

	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatalf("chmod parent read-only: %s", err)
	}

	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })

	if _, err := New(filepath.Join(parent, "sub")); err == nil {
		t.Error("expected New to fail when the storage directory cannot be created under a read-only parent")
	}
}

// --- WALBytes: driven directly against a bare *DB literal, so neither branch needs a real
// database connection at all. ---

func TestWALBytes_PathDoesNotExist(t *testing.T) {
	d := &DB{walFilePath: filepath.Join(t.TempDir(), "hippocampus.db-wal")}

	n, err := d.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes: %s", err)
	}

	if n != 0 {
		t.Errorf("WALBytes for a nonexistent file = %d, want 0", n)
	}
}

func TestWALBytes_StatError(t *testing.T) {
	// A NUL byte is rejected by the OS path syscalls with an error distinct from "not exist"
	// (invalid argument), driving the generic-error branch without needing a permission trick.
	d := &DB{walFilePath: "bad\x00path"}

	if _, err := d.WALBytes(); err == nil {
		t.Error("expected a stat error for an invalid path")
	}
}

// --- addColumnIfMissing: rows.Err() branch, mirroring columnExists' equivalent test. ---

func TestAddColumnIfMissing_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("x").RowError(0, errors.New("boom")))

	if err := d.addColumnIfMissing("memories", "x", "INTEGER"); err == nil {
		t.Fatal("expected an error from rows.Err()")
	}

	expectationsMet(t, mock)
}

// --- UsedBytes: the non-SQLite delegation, and the SQLite pragma-read error branches. ---

func TestUsedBytes_DelegatesToLiveRowsOnServerDrivers(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`SELECT`).WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(int64(1234)))

	used, err := d.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %v", err)
	}

	if used != 1234 {
		t.Fatalf("used = %d, want 1234", used)
	}

	expectationsMet(t, mock)
}

func TestUsedBytes_FreelistCountError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`PRAGMA page_count`).WillReturnRows(sqlmock.NewRows([]string{"page_count"}).AddRow(int64(10)))
	mock.ExpectQuery(`PRAGMA freelist_count`).WillReturnError(errors.New("boom"))

	if _, err := d.UsedBytes(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestUsedBytes_PageSizeError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`PRAGMA page_count`).WillReturnRows(sqlmock.NewRows([]string{"page_count"}).AddRow(int64(10)))
	mock.ExpectQuery(`PRAGMA freelist_count`).WillReturnRows(sqlmock.NewRows([]string{"freelist_count"}).AddRow(int64(2)))
	mock.ExpectQuery(`PRAGMA page_size`).WillReturnError(errors.New("boom"))

	if _, err := d.UsedBytes(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- Preserve: the WAL checkpoint failure (the incremental-vacuum failure is already covered by
// an existing SQLite test). ---

func TestPreserve_WALCheckpointError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`PRAGMA incremental_vacuum`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`PRAGMA wal_checkpoint`).WillReturnError(errors.New("boom"))

	if err := d.Preserve(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- Close: a failing Preserve is logged but does not stop Close from completing, and a failing
// lockConn.Close() is likewise logged rather than propagated. ---

func TestClose_PreserveFailureIsLoggedNotPropagated(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`PRAGMA incremental_vacuum`).WillReturnError(errors.New("boom"))
	mock.ExpectClose()

	if err := d.Close(); err != nil {
		t.Fatalf("Close must not propagate a Preserve failure, got: %v", err)
	}

	expectationsMet(t, mock)
}

func TestClose_LockConnCloseErrorIsLogged(t *testing.T) {
	d := newTestDB(t)

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %s", err)
	}

	// Close it once up front so the Close call under test hits an already-closed connection,
	// which errors rather than panicking.
	if err := conn.Close(); err != nil {
		t.Fatalf("pre-close: %s", err)
	}

	d.readOnly = true // Preserve becomes a no-op so Close doesn't need a free connection.
	d.lockConn = conn

	if err := d.Close(); err != nil {
		t.Fatalf("Close must not propagate a lockConn close failure, got: %s", err)
	}
}

// --- Purge: the commit and final-Preserve failure branches, which a real SQLite connection
// can't easily be made to hit on demand. ---

func TestPurge_CommitError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM event_links`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM search_outbox`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit().WillReturnError(errors.New("boom"))

	if err := d.Purge(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestPurge_FinalPreserveError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM event_links`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM search_outbox`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectExec(`PRAGMA incremental_vacuum`).WillReturnError(errors.New("boom"))

	if err := d.Purge(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- initSchema: every remaining SQLite failure branch, driven against a mocked handle since a
// real in-memory database can't be made to fail these specific pragma/DDL steps on demand. Each
// test queues every step preceding the one under test as succeeding, mirroring the
// expectPostgresSchemaInitFresh/expectMySQLSchemaInitFresh helpers in mock_test.go. ---

func sqliteColumnPresent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`pragma_table_info`).WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("present"))
}

func sqliteColumnAbsent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`pragma_table_info`).WillReturnRows(sqlmock.NewRows([]string{"name"}))
}

func TestInitSchema_ConfigureStorageQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`PRAGMA auto_vacuum = INCREMENTAL`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`PRAGMA auto_vacuum`).WillReturnError(errors.New("boom"))

	if err := d.initSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitSchema_ConfigureStorageVacuumError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`PRAGMA auto_vacuum = INCREMENTAL`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`PRAGMA auto_vacuum`).WillReturnRows(sqlmock.NewRows([]string{"auto_vacuum"}).AddRow(0))
	mock.ExpectExec(`VACUUM`).WillReturnError(errors.New("boom"))

	if err := d.initSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestInitSchema_LedgerCreateError covers the migrations table itself failing to be created, which
// is the one piece of schema that cannot be a migration and so has no recovery beyond failing.
func TestInitSchema_LedgerCreateError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`PRAGMA auto_vacuum = INCREMENTAL`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`PRAGMA auto_vacuum`).WillReturnRows(sqlmock.NewRows([]string{"auto_vacuum"}).AddRow(2))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS ` + schemaMigrationsTable).WillReturnError(errors.New("boom"))

	if err := d.initSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestInitSchema_LedgerReadError covers the ledger existing but being unreadable. It must fail
// rather than proceed as though nothing had been applied: proceeding would run the `once`
// migrations again, and one of those is a table rebuild.
func TestInitSchema_LedgerReadError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`PRAGMA auto_vacuum = INCREMENTAL`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`PRAGMA auto_vacuum`).WillReturnRows(sqlmock.NewRows([]string{"auto_vacuum"}).AddRow(2))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS ` + schemaMigrationsTable).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT version FROM ` + schemaMigrationsTable).WillReturnError(errors.New("boom"))

	if err := d.initSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestInitSchema_MigrationFailuresStopTheRun drives each migration failing in turn and requires the
// run to stop there. It is table-driven off the real list rather than one test per step, so a
// migration added to the list is covered without anyone remembering to add a test - which is the
// same reason the column cases below read coreColumnMigrations.
func TestInitSchema_MigrationFailuresStopTheRun(t *testing.T) {
	failures := map[string]func(sqlmock.Sqlmock){
		"core_tables": func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnError(errors.New("boom"))
		},
		"core_columns": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`pragma_table_info`).WillReturnError(errors.New("boom"))
		},
		"link_tables": func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS \w+_links`).WillReturnError(errors.New("boom"))
		},
		"drop_legacy_relationships": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`pragma_table_info`).WillReturnError(errors.New("boom"))
		},
		"significance_levels": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`pragma_table_info`).WillReturnError(errors.New("boom"))
		},
		"covering_index": func(mock sqlmock.Sqlmock) {
			expectSupersededIndexDrop(mock, driverSQLite)
			mock.ExpectExec(`CREATE INDEX IF NOT EXISTS`).WillReturnError(errors.New("boom"))
		},
		"listing_index": func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(`CREATE INDEX IF NOT EXISTS`).WillReturnError(errors.New("boom"))
		},
		"search_outbox": func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS search_outbox`).WillReturnError(errors.New("boom"))
		},
		"forgotten_log": func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memory_tombstones`).WillReturnError(errors.New("boom"))
		},
		"content_search": func(mock sqlmock.Sqlmock) {
			mock.ExpectExec(`CREATE VIRTUAL TABLE IF NOT EXISTS`).WillReturnError(errors.New("boom"))
		},
	}

	for _, migration := range (&DB{driver: driverSQLite}).migrations() {
		if migration.when != nil && !migration.when(dialects[driverSQLite]) {
			continue
		}

		fail, ok := failures[migration.name]
		if !ok {
			t.Errorf("migration %q has no failure case here - add one, or the step's error path is "+
				"the one thing about it nothing exercises", migration.name)

			continue
		}

		t.Run(migration.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			expectSchemaThrough(t, mock, driverSQLite, migration.name)
			fail(mock)

			if err := d.initSchema(); err == nil {
				t.Fatalf("a failure in %s must stop schema initialisation", migration.name)
			}

			expectationsMet(t, mock)
		})
	}
}

// TestInitSchema_EveryCoreColumnFailureStopsTheRun drives the column migration failing on each of
// its columns in turn. It replaced ten near-identical tests that each hard-coded how many columns
// preceded theirs, and it covers an eleventh column the day it is added.
func TestInitSchema_EveryCoreColumnFailureStopsTheRun(t *testing.T) {
	for i, column := range (&DB{driver: driverSQLite}).coreColumnMigrations() {
		t.Run(column.table+"."+column.column, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			expectSchemaThrough(t, mock, driverSQLite, "core_columns")

			for range i {
				sqliteColumnPresent(mock)
			}

			sqliteColumnAbsent(mock)
			mock.ExpectExec(`ALTER TABLE ` + column.table + ` ADD COLUMN ` + column.column).
				WillReturnError(errors.New("boom"))

			if err := d.initSchema(); err == nil {
				t.Fatalf("a failure adding %s.%s must stop schema initialisation", column.table, column.column)
			}

			expectationsMet(t, mock)
		})
	}
}

// --- verifyInstanceLock: the Postgres/MySQL reacquisition branches (the SQLite/"neither" default
// is already covered by TestVerifyInstanceLock_ReacquiresOnDeadConnection). A dead pinned
// connection is simulated the same way that test does - closing it out from under the lock - and
// the mocked handle supplies the reacquisition query. ---

func deadMockConn(t *testing.T, d *DB) *sql.Conn {
	t.Helper()

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("pre-close: %v", err)
	}

	return conn
}

func TestVerifyInstanceLock_PostgresReacquires(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)
	d.lockConn = deadMockConn(t, d)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	if err := d.verifyInstanceLock(); err != nil {
		t.Fatalf("verifyInstanceLock: %v", err)
	}

	if d.lockConn != nil {
		_ = d.lockConn.Close()
	}

	expectationsMet(t, mock)
}

func TestVerifyInstanceLock_PostgresReacquireFails(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)
	d.lockConn = deadMockConn(t, d)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))

	if err := d.verifyInstanceLock(); err == nil {
		t.Fatal("expected an error when the lock cannot be reacquired")
	}

	expectationsMet(t, mock)
}

func TestVerifyInstanceLock_MySQLReacquires(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)
	d.lockConn = deadMockConn(t, d)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(1)))

	if err := d.verifyInstanceLock(); err != nil {
		t.Fatalf("verifyInstanceLock: %v", err)
	}

	if d.lockConn != nil {
		_ = d.lockConn.Close()
	}

	expectationsMet(t, mock)
}

func TestVerifyInstanceLock_MySQLReacquireFails(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)
	d.lockConn = deadMockConn(t, d)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(0)))

	if err := d.verifyInstanceLock(); err == nil {
		t.Fatal("expected an error when the lock cannot be reacquired")
	}

	expectationsMet(t, mock)
}
