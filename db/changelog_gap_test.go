package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// This file targets changelog.go's remaining coverage gaps: real-store scenarios for the read side's
// edges (no table, the head and seq lookups, an undecodable row, the byte cap's keep-the-newest
// rule) and the closed-database failures, plus mocked-handle scenarios for the branches a real
// connection can't be made to fail on demand (a failed index, insert or later prune statement).

var errChangeLogMock = errors.New("mock failure")

// TestTheChangeLogReadsNothingWithoutItsTable: a store whose migration has not created the table
// reads as an empty log rather than failing every watcher on a missing relation.
func TestTheChangeLogReadsNothingWithoutItsTable(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})
	d.changeLogTable = false

	ctx := context.Background()

	if changes, err := d.ReadChanges(ctx, 0, 0, 10); err != nil || changes != nil {
		t.Errorf("ReadChanges without the table = (%v, %v), want (nil, nil)", changes, err)
	}

	if head, err := d.ChangeLogHead(ctx); err != nil || head != 0 {
		t.Errorf("ChangeLogHead without the table = (%d, %v), want (0, nil)", head, err)
	}

	if found, err := d.HasChange(ctx, 1); err != nil || found {
		t.Errorf("HasChange without the table = (%v, %v), want (false, nil)", found, err)
	}
}

// TestChangeLogHeadAndHasChangeFollowTheLog: the head is 0 on an empty log and the newest seq after
// it, and HasChange finds a seq the log holds and reports one it does not.
func TestChangeLogHeadAndHasChangeFollowTheLog(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})

	ctx := context.Background()

	if head, err := d.ChangeLogHead(ctx); err != nil || head != 0 {
		t.Fatalf("ChangeLogHead on an empty log = (%d, %v), want (0, nil)", head, err)
	}

	for range 2 {
		if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
			t.Fatalf("AppendChange: %s", err)
		}
	}

	changes := readAllChanges(t, d)

	head, err := d.ChangeLogHead(ctx)
	if err != nil || head != changes[len(changes)-1].Seq {
		t.Errorf("ChangeLogHead = (%d, %v), want (%d, nil)", head, err, changes[len(changes)-1].Seq)
	}

	if found, err := d.HasChange(ctx, changes[0].Seq); err != nil || !found {
		t.Errorf("HasChange(%d) = (%v, %v), want (true, nil)", changes[0].Seq, found, err)
	}

	if found, err := d.HasChange(ctx, head+100); err != nil || found {
		t.Errorf("HasChange(%d) = (%v, %v), want (false, nil)", head+100, found, err)
	}
}

// TestReadChangesPassesAnUndecodableRowOnEmpty: a row whose payload is not the expected JSON is still
// a change, handed on with no items, and the rows after it are still read.
func TestReadChangesPassesAnUndecodableRowOnEmpty(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})

	ctx := context.Background()

	if _, err := d.exec(
		ctx,
		`INSERT INTO `+changeLogTable+` (kind, cause, cycle_id, recorded_at, payload, payload_bytes) VALUES (?, ?, ?, ?, ?, ?)`,
		int(CallbackKindSleepCompleted),
		0,
		int64(0),
		int64(1),
		[]byte("{not json"),
		int64(9),
	); err != nil {
		t.Fatalf("insert an undecodable row: %s", err)
	}

	if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted, Report: []byte("r")}); err != nil {
		t.Fatalf("AppendChange: %s", err)
	}

	changes := readAllChanges(t, d)

	if len(changes) != 2 {
		t.Fatalf("read %d changes, want the undecodable row and the one after it", len(changes))
	}

	if changes[0].Items != nil || changes[0].Report != nil {
		t.Errorf("the undecodable row carried %+v, want it passed on empty", changes[0])
	}

	if string(changes[1].Report) != "r" {
		t.Errorf("the row after the undecodable one carried report %q, want %q", changes[1].Report, "r")
	}
}

// TestPruneChangeLogByteCapEdges: a newest row larger than the whole cap is kept rather than leaving
// a feed that carries nothing, and an empty log prunes nothing.
func TestPruneChangeLogByteCapEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("a newest row over the cap is kept", func(t *testing.T) {
		d, _ := changeLogTestDB(t, ChangeLogPolicy{MaxBytes: 1})

		for range 3 {
			if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
				t.Fatalf("AppendChange: %s", err)
			}
		}

		before := readAllChanges(t, d)

		pruned, err := d.PruneChangeLog(ctx)
		if err != nil {
			t.Fatalf("PruneChangeLog: %s", err)
		}

		after := readAllChanges(t, d)

		if pruned != 2 || len(after) != 1 || after[0].Seq != before[2].Seq {
			t.Errorf("the byte cap pruned %d and kept %+v, want only the newest row", pruned, after)
		}
	})

	t.Run("an empty log", func(t *testing.T) {
		d, _ := changeLogTestDB(t, ChangeLogPolicy{MaxBytes: 1})

		if pruned, err := d.PruneChangeLog(ctx); err != nil || pruned != 0 {
			t.Errorf("PruneChangeLog on an empty log = (%d, %v), want (0, nil)", pruned, err)
		}
	})
}

// TestChangeLogMethods_ErrorOnClosedDB: each read and each cap's first statement surfaces the failure
// of a closed database. Each cap gets a store of its own, since the first to fail returns.
func TestChangeLogMethods_ErrorOnClosedDB(t *testing.T) {
	ctx := context.Background()

	closed := func(t *testing.T, policy ChangeLogPolicy) *DB {
		t.Helper()

		d, _ := changeLogTestDB(t, policy)

		if err := d.Close(); err != nil {
			t.Fatalf("Close: %s", err)
		}

		return d
	}

	d := closed(t, ChangeLogPolicy{})

	checks := []struct {
		name string
		call func(d *DB) error
	}{
		{"AppendChange", func(d *DB) error { return d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}) }},
		{"ReadChanges", func(d *DB) error { _, err := d.ReadChanges(ctx, 0, 0, 10); return err }},
		{"ChangeLogHead", func(d *DB) error { _, err := d.ChangeLogHead(ctx); return err }},
		{"HasChange", func(d *DB) error { _, err := d.HasChange(ctx, 1); return err }},
	}

	for _, check := range checks {
		if err := check.call(d); err == nil {
			t.Errorf("%s: expected an error against a closed database", check.name)
		}
	}

	policies := map[string]ChangeLogPolicy{
		"age":   {MaxAge: 1},
		"rows":  {MaxRows: 1},
		"bytes": {MaxBytes: 1},
	}

	for k, v := range policies {
		if _, err := closed(t, v).PruneChangeLog(ctx); err == nil {
			t.Errorf("PruneChangeLog (%s cap): expected an error against a closed database", k)
		}
	}
}

// TestInitChangeLog_IndexFails: a failed age index fails the migration and leaves the table unmarked,
// so nothing reads or records into a log the migration did not finish.
func TestInitChangeLog_IndexFails(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS " + changeLogTable).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("CREATE INDEX IF NOT EXISTS idx_" + changeLogTable).WillReturnError(errChangeLogMock)

	if err := d.initChangeLog(); !errors.Is(err, errChangeLogMock) {
		t.Errorf("initChangeLog = %v, want the index failure", err)
	}

	if d.changeLogTable {
		t.Error("expected a failed migration to leave the change log table unmarked")
	}

	expectationsMet(t, mock)
}

// TestLogChanges_InsertFails: a change log row that cannot be written fails the change it records,
// rather than committing a change every watcher would miss.
func TestLogChanges_InsertFails(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO " + changeLogTable).WillReturnError(errChangeLogMock)
	mock.ExpectRollback()

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("Begin: %s", err)
	}

	deliveries := []CallbackDelivery{{Kind: CallbackKindMemoryForgotten}}

	if err := d.logChanges(tx, deliveries); !errors.Is(err, errChangeLogMock) {
		t.Errorf("logChanges = %v, want the insert failure", err)
	}

	_ = tx.Rollback()

	expectationsMet(t, mock)
}

// TestReadChanges_ScanFails: a row that does not scan fails the read rather than being skipped, which
// would hand the reader a seq gap it would take for the caps.
func TestReadChanges_ScanFails(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	d.changeLogTable = true

	mock.ExpectQuery("SELECT seq, kind, cause, cycle_id, recorded_at, payload FROM " + changeLogTable).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "kind", "cause", "cycle_id", "recorded_at", "payload"}).
			AddRow("not a seq", 0, 0, "", 0, nil))

	if _, err := d.ReadChanges(context.Background(), 0, 0, 10); err == nil {
		t.Error("expected ReadChanges to fail on a row that does not scan")
	}

	expectationsMet(t, mock)
}

// TestPruneChangeLog_ByteCapLaterStatementsFail: the byte cap's fallback read of the newest row and
// its delete each surface their failure.
func TestPruneChangeLog_ByteCapLaterStatementsFail(t *testing.T) {
	ctx := context.Background()

	t.Run("the newest row read", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)
		d.changeLogTable = true
		d.SetChangeLogPolicy(ChangeLogPolicy{Enabled: true, MaxBytes: 1})

		mock.ExpectQuery("SELECT MIN\\(seq\\)").
			WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(sql.NullInt64{}))
		mock.ExpectQuery("SELECT MAX\\(seq\\) FROM " + changeLogTable).WillReturnError(errChangeLogMock)

		if _, err := d.PruneChangeLog(ctx); !errors.Is(err, errChangeLogMock) {
			t.Errorf("PruneChangeLog = %v, want the newest row read's failure", err)
		}

		expectationsMet(t, mock)
	})

	t.Run("the delete", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)
		d.changeLogTable = true
		d.SetChangeLogPolicy(ChangeLogPolicy{Enabled: true, MaxBytes: 1})

		mock.ExpectQuery("SELECT MIN\\(seq\\)").WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(int64(5)))
		mock.ExpectExec("DELETE FROM " + changeLogTable + " WHERE seq < \\?").WillReturnError(errChangeLogMock)

		if _, err := d.PruneChangeLog(ctx); !errors.Is(err, errChangeLogMock) {
			t.Errorf("PruneChangeLog = %v, want the delete's failure", err)
		}

		expectationsMet(t, mock)
	})
}

// TestInitChangeLog_TableFails: a failed CREATE TABLE fails the migration before it reaches the index,
// and leaves the table unmarked.
func TestInitChangeLog_TableFails(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS " + changeLogTable).WillReturnError(errChangeLogMock)

	if err := d.initChangeLog(); !errors.Is(err, errChangeLogMock) {
		t.Errorf("initChangeLog = %v, want the table failure", err)
	}

	if d.changeLogTable {
		t.Error("expected a failed migration to leave the change log table unmarked")
	}

	expectationsMet(t, mock)
}
