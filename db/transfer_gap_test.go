package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// This file targets transfer.go's remaining coverage gaps - GetMemoriesPage/GetEventsPage's
// scan/rows.Err() branches, and ImportMemories/ImportEvents' MySQL upsert variant, registry-lock
// error, per-row level-resolution error, insert error, and commit error branches - all driven
// against a mocked handle since a real SQLite connection can't be made to fail mid-transaction on
// demand, and the driver-specific query text is otherwise never exercised outside a live server.

// memoryPageColumns mirrors memoryColumns (id, timestamp, significance, event_id, body, is_binary,
// time_recalled, recall_count, is_summary, group_name).
var memoryPageColumns = []string{
	"id", "timestamp", "significance", "event_id", "body",
	"is_binary", "time_recalled", "recall_count", "is_summary", "group_name",
}

func TestGetMemoriesPage_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(memoryPageColumns).
		AddRow("m1", "not-an-int", int32(1), "", []byte("x"), false, int64(0), int32(0), false, ""))

	if _, err := d.GetMemoriesPage(context.Background(), "", 10); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestGetMemoriesPage_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(memoryPageColumns).
		AddRow("m1", int64(1), int32(1), "", []byte("x"), false, int64(0), int32(0), false, "").
		RowError(0, errors.New("boom")))

	if _, err := d.GetMemoriesPage(context.Background(), "", 10); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

func TestGetEventsPage_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(eventRowsColumns).
		AddRow("e1", "not-an-int", int64(0), int32(1), "n", "d", false, int64(0), "[]", ""))

	if _, err := d.GetEventsPage(context.Background(), "", 10); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestGetEventsPage_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(eventRowsColumns).
		AddRow("e1", int64(1), int64(0), int32(1), "n", "d", false, int64(0), "[]", "").
		RowError(0, errors.New("boom")))

	if _, err := d.GetEventsPage(context.Background(), "", 10); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// --- ImportMemories ---

func TestImportMemories_MySQLUpsertVariant(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`GET_LOCK`).WillReturnResult(sqlmock.NewResult(0, 0)) // acquireRegistryLock takes the registry lock on the server drivers.
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO memories`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`RELEASE_LOCK`).WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := d.ImportMemories(context.Background(), []types.Memory{{Id: "m1", TimeStamp: 1, Body: "x"}})
	if err != nil {
		t.Fatalf("ImportMemories: %v", err)
	}

	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}

	expectationsMet(t, mock)
}

func TestImportMemories_RegistryLockError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`GET_LOCK`).WillReturnError(errors.New("boom"))

	if _, err := d.ImportMemories(context.Background(), []types.Memory{{Id: "m1", TimeStamp: 1, Body: "x"}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestImportMemories_LevelResolutionErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if _, err := d.ImportMemories(context.Background(), []types.Memory{{Id: "m1", TimeStamp: 1, Significance: 5, Body: "x"}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestImportMemories_InsertExecErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO memories`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if _, err := d.ImportMemories(context.Background(), []types.Memory{{Id: "m1", TimeStamp: 1, Body: "x"}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestImportMemories_CommitError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO memories`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("boom"))

	if _, err := d.ImportMemories(context.Background(), []types.Memory{{Id: "m1", TimeStamp: 1, Body: "x"}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- ImportEvents ---

func TestImportEvents_MySQLUpsertVariant(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`GET_LOCK`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO events`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`RELEASE_LOCK`).WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := d.ImportEvents(context.Background(), []types.Event{{Id: "e1", Name: "n", TimeStart: 1}})
	if err != nil {
		t.Fatalf("ImportEvents: %v", err)
	}

	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}

	expectationsMet(t, mock)
}

func TestImportEvents_RegistryLockError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`GET_LOCK`).WillReturnError(errors.New("boom"))

	if _, err := d.ImportEvents(context.Background(), []types.Event{{Id: "e1", Name: "n", TimeStart: 1}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestImportEvents_LevelResolutionErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if _, err := d.ImportEvents(context.Background(), []types.Event{{Id: "e1", Name: "n", TimeStart: 1, Significance: 5}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestImportEvents_InsertExecErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO events`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if _, err := d.ImportEvents(context.Background(), []types.Event{{Id: "e1", Name: "n", TimeStart: 1}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestImportEvents_CommitError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO events`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("boom"))

	if _, err := d.ImportEvents(context.Background(), []types.Event{{Id: "e1", Name: "n", TimeStart: 1}}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}
