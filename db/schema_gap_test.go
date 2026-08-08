package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// --- event.go: the row decoder and the two delete transactions, both of which prune the event half
// of the link graph inside the same transaction that removes the event. ---

// TestScanEvent_MetadataDecodeErrorPropagates drives an event row whose metadata column is not JSON.
func TestScanEvent_MetadataDecodeErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT`).WillReturnRows(
		sqlmock.NewRows([]string{
			"id", "name", "time_start", "time_end", "significance",
			"memories_consolidated", "group_name", "metadata", "link_significance",
		}).AddRow("e1", "trip", 100, 200, 5, false, "", []byte("{not json"), 0))

	if _, err := d.GetEvents(context.Background(), EventFilter{}); err == nil {
		t.Fatal("expected the metadata decode failure to propagate")
	}
}

// TestDeleteEvent_Failures walks every step of the delete transaction. The prune matters most: an
// event's links must go with it, or the events at the far end keep counting significance from a
// link to something that no longer exists.
func TestDeleteEvent_Failures(t *testing.T) {
	tests := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
	}{
		{
			name: "begin",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin().WillReturnError(errors.New("boom"))
			},
		},
		{
			name: "delete",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "rows affected",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM events WHERE id`).
					WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))
				mock.ExpectRollback()
			},
		},
		{
			name: "prune",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM event_links`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM event_links`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
				mock.ExpectCommit().WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)
			test.expect(mock)

			if _, err := d.DeleteEvent(context.Background(), "e1"); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			expectationsMet(t, mock)
		})
	}
}

// TestDeleteEventIfEmpty_Failures covers the guarded delete's prune and commit.
func TestDeleteEventIfEmpty_Failures(t *testing.T) {
	tests := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
	}{
		{
			name: "prune",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM event_links`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM event_links`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
				mock.ExpectCommit().WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)
			test.expect(mock)

			if _, err := d.DeleteEventIfEmpty(context.Background(), "e1"); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			expectationsMet(t, mock)
		})
	}
}

// TestDeleteEventIfEmpty_SparedEventSkipsThePrune pins the guard's other half: the emptiness check
// may have spared the event, and a spared event keeps its links. The mock fails if a prune runs.
func TestDeleteEventIfEmpty_SparedEventSkipsThePrune(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	deleted, err := d.DeleteEventIfEmpty(context.Background(), "e1")
	if err != nil {
		t.Fatalf("DeleteEventIfEmpty: %v", err)
	}

	if deleted {
		t.Error("expected the event to be reported as spared")
	}

	expectationsMet(t, mock)
}

// --- db.go: the open paths and Purge. ---

// TestPurge_LinkPurgeErrorRollsBack pins that nothing survives a purge only if the whole purge
// commits: a failed link purge must take the rest with it.
func TestPurge_LinkPurgeErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	// Links go first: they reference both tables, and nothing survives to have an aggregate
	// recalculated.
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.Purge(context.Background()); err == nil {
		t.Fatal("expected the link purge failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestNew_UnopenableDirectoryFails covers the open failure path, including the lock release that
// has to happen when the database behind an acquired lock cannot be opened.
func TestNew_UnopenableDirectoryFails(t *testing.T) {
	// A path whose parent is a regular file cannot hold a database file.
	dir := t.TempDir()
	notADirectory := filepath.Join(dir, "file")

	if err := os.WriteFile(notADirectory, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := New(filepath.Join(notADirectory, "store")); err == nil {
		t.Fatal("expected opening a database under a regular file to fail")
	}
}

// TestNewSQLiteReadOnly_MissingFileFails covers the read-only open's failure path. It is the
// backfill tool's entry point, so failing loudly is what stops it running against nothing.
func TestNewSQLiteReadOnly_MissingFileFails(t *testing.T) {
	if _, err := NewSQLiteReadOnly(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected a read-only open of a non-existent database to fail")
	}
}

// --- postgres.go / mysql.go: the schema initialisers' remaining failure branches. Both drivers
// share initLinkTables and dropLegacyRelationshipColumns, and a failure in either must stop
// startup rather than leave a half-migrated schema. ---

func TestInitPostgresSchema_Failures(t *testing.T) {
	tests := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
	}{
		{
			name: "link tables",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectExec(`CREATE TABLE IF NOT EXISTS \w+_links`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name: "legacy column drop",
			expect: func(mock sqlmock.Sqlmock) {
				expectLinkTables(mock, driverPostgres)
				mock.ExpectQuery(`information_schema.columns`).WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverPostgres)

			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

			test.expect(mock)

			if err := d.initPostgresSchema(); err == nil {
				t.Fatal("expected the failure to stop schema initialisation")
			}

			expectationsMet(t, mock)
		})
	}
}

func TestInitMySQLSchema_Failures(t *testing.T) {
	// columnPresent scripts one addColumnIfMissing probe reporting the column already there.
	columnPresent := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	tests := []struct {
		name string
		// columns is how many addColumnIfMissing probes succeed before the failing one.
		columns int
		expect  func(sqlmock.Sqlmock)
	}{
		{
			name:    "memories.link_significance",
			columns: 2,
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`column_name FROM information_schema`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name:    "events.link_significance",
			columns: 3,
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`column_name FROM information_schema`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name:    "memories.metadata",
			columns: 4,
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`column_name FROM information_schema`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name:    "events.metadata",
			columns: 5,
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`column_name FROM information_schema`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name:    "link tables",
			columns: 8,
			expect: func(mock sqlmock.Sqlmock) {
				for range 5 {
					mock.ExpectQuery(`collation_name FROM information_schema`).
						WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
				}

				mock.ExpectQuery(`column_name FROM information_schema`).
					WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
				mock.ExpectQuery(`column_name FROM information_schema`).
					WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))

				mock.ExpectExec(`CREATE TABLE IF NOT EXISTS \w+_links`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name:    "legacy column drop",
			columns: 8,
			expect: func(mock sqlmock.Sqlmock) {
				for range 5 {
					mock.ExpectQuery(`collation_name FROM information_schema`).
						WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
				}

				mock.ExpectQuery(`column_name FROM information_schema`).
					WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
				mock.ExpectQuery(`column_name FROM information_schema`).
					WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))

				expectLinkTables(mock, driverMySQL)
				mock.ExpectQuery(`information_schema.columns`).WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverMySQL)

			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

			for range test.columns {
				columnPresent(mock)
			}

			test.expect(mock)

			if err := d.initMySQLSchema(); err == nil {
				t.Fatal("expected the failure to stop schema initialisation")
			}

			expectationsMet(t, mock)
		})
	}
}

// --- lock.go: the diagnostics the lock file carries. The refusal stands on the kernel's lock, not
// on what the file says about it, so every one of these is best-effort by design. ---

// TestWriteLockHolder_WriteFailureIsSwallowed pins that a lock file which cannot be written still
// excludes correctly - the property that actually matters. A file opened O_APPEND accepts the
// truncate and rejects the positional write, which is exactly the shape of the failure.
func TestWriteLockHolder_WriteFailureIsSwallowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hippocampus.lock")

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = file.Close() }()

	// No panic, no propagation: writeLockHolder returns nothing to propagate with.
	writeLockHolder(file)
}

// TestReadLockHolder_BlankFileSaysNothing covers the branch where the file exists and holds only
// whitespace - a lock taken by a version that wrote no holder details, or a write that failed.
func TestReadLockHolder_BlankFileSaysNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hippocampus.lock")

	if err := os.WriteFile(path, []byte("   \n  \n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = file.Close() }()

	if got := readLockHolder(file); got != "" {
		t.Errorf("expected a blank lock file to add nothing to the refusal, got %q", got)
	}
}

// TestReadLockHolder_RoundTrip covers the ordinary case, pinning that what writeLockHolder records
// is what the next process to try actually reads back.
func TestReadLockHolder_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hippocampus.lock")

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = file.Close() }()

	writeLockHolder(file)

	holder := readLockHolder(file)
	if holder == "" {
		t.Fatal("expected the recorded holder details to read back")
	}

	for _, want := range []string{"held by", "pid", "host", "since"} {
		if !contains(holder, want) {
			t.Errorf("expected the holder clause to mention %q, got %q", want, holder)
		}
	}
}

// TestImportMemories_RoundTripsMetadata covers the import write path carrying metadata through,
// which is the half of ImportMemories the metadata-encode failure branch guards.
func TestImportMemories_RoundTripsMetadata(t *testing.T) {
	d := newTestDB(t)

	memories := []types.Memory{{
		Id:           "m1",
		TimeStamp:    100,
		Significance: 5,
		Body:         "body",
		Metadata:     map[string]string{"source": "slack"},
	}}

	if _, err := d.ImportMemories(context.Background(), memories); err != nil {
		t.Fatalf("ImportMemories: %v", err)
	}

	read, err := d.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %v", err)
	}

	if len(*read) != 1 || (*read)[0].Metadata["source"] != "slack" {
		t.Errorf("expected the imported metadata to round trip, got %+v", *read)
	}
}

// TestNew_SchemaFailureReleasesTheLock covers the other half of New's failure handling: the lock
// was acquired, so a database that then refuses to initialise must give it back rather than leave
// the directory locked against the next attempt.
//
// A directory standing where the database file belongs is what produces it: sql.Open is lazy and
// succeeds, and initSchema's first pragma is what discovers the file cannot be opened.
func TestNew_SchemaFailureReleasesTheLock(t *testing.T) {
	directory := t.TempDir()

	if err := os.Mkdir(filepath.Join(directory, DataFile), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if _, err := New(directory); err == nil {
		t.Fatal("expected schema initialisation to fail against a directory")
	}

	// The lock must be free again: a second attempt has to reach the same failure, not be refused
	// as though another instance held the store.
	_, err := New(directory)
	if err == nil {
		t.Fatal("expected the second attempt to fail the same way")
	}

	if contains(err.Error(), "already holds the storage lock") {
		t.Errorf("expected the lock to have been released, got a lock refusal: %s", err)
	}
}

// TestNewPostgres_MalformedDSNFails pins that a typo in the configured DSN fails at construction
// rather than being accepted and surfacing on the first request. Both constructors are checked
// because they open the handle separately.
//
// The refusal comes from the connection setup, not from sql.Open: database/sql opens lazily, so
// sql.Open's own error branch in each constructor is reachable only if the driver were never
// registered, which the package's own import guarantees it is.
func TestNewPostgres_MalformedDSNFails(t *testing.T) {
	const malformed = "postgres://user:pass@host:not-a-port/db"

	if _, err := NewPostgres(malformed, false); err == nil {
		t.Error("expected a malformed DSN to be rejected")
	}

	if _, err := NewPostgresReadOnly(malformed); err == nil {
		t.Error("expected a malformed DSN to be rejected on the read-only open too")
	}
}
