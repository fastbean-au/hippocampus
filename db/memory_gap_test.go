package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// This file targets the memory.go weak spots called out for this coverage push -
// updatedRowExisted, ReplaceMemoriesWithSummary, ConsolidateMemories, and ConsolidateEventMemories
// - via a mocked handle, since their remaining branches (RowsAffected() failing, a mid-transaction
// exec failing, rows.Err() after Next() itself errors) can't be triggered on demand against a real
// SQLite connection.

// --- updatedRowExisted ---

func TestUpdatedRowExisted_RowsAffectedError(t *testing.T) {
	d, _ := newMockDB(t, driverSQLite)

	if _, err := d.updatedRowExisted(context.Background(), sqlmock.NewErrorResult(errors.New("boom")), "memories", "m1"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUpdatedRowExisted_MySQLZeroRowsProbesExistence(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`SELECT EXISTS`).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	existed, err := d.updatedRowExisted(context.Background(), sqlmock.NewResult(0, 0), "memories", "m1")
	if err != nil {
		t.Fatalf("updatedRowExisted: %v", err)
	}

	if !existed {
		t.Fatal("expected existed=true from the fallback existence probe")
	}

	expectationsMet(t, mock)
}

func TestUpdatedRowExisted_MySQLZeroRowsExistenceProbeError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`SELECT EXISTS`).WillReturnError(errors.New("boom"))

	if _, err := d.updatedRowExisted(context.Background(), sqlmock.NewResult(0, 0), "memories", "m1"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- ReplaceMemoriesWithSummary ---

func TestReplaceMemoriesWithSummary_BeginTxError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	// summary.Significance is 0 (unranked), so ensureSignificanceLevel never touches d.sql and the
	// only expectation needed is the failing Begin.
	mock.ExpectBegin().WillReturnError(errors.New("boom"))

	if _, err := d.ReplaceMemoriesWithSummary(context.Background(), "e1", types.Memory{Id: "s1", Body: "x"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReplaceMemoriesWithSummary_DeleteExecError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memories WHERE event_id`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if _, err := d.ReplaceMemoriesWithSummary(context.Background(), "e1", types.Memory{Id: "s1", Body: "x"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReplaceMemoriesWithSummary_DeleteRowsAffectedError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memories WHERE event_id`).WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))
	mock.ExpectRollback()

	if _, err := d.ReplaceMemoriesWithSummary(context.Background(), "e1", types.Memory{Id: "s1", Body: "x"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReplaceMemoriesWithSummary_CommitError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memories WHERE event_id`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`INSERT INTO memories`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit().WillReturnError(errors.New("boom"))

	if _, err := d.ReplaceMemoriesWithSummary(context.Background(), "e1", types.Memory{Id: "s1", Body: "x"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- ConsolidateMemories ---

func emptyRanksQuery(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT id, level_rank FROM significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "level_rank"}))
}

func TestConsolidateMemories_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories WHERE event_id`).WillReturnError(errors.New("boom"))

	if _, err := d.ConsolidateMemories(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateMemories_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories WHERE event_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "timestamp", "significance_level_id", "time_recalled", "recall_count"}).
			AddRow("m1", "not-an-int", nil, int64(0), int32(0)))

	if _, err := d.ConsolidateMemories(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateMemories_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories WHERE event_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "timestamp", "significance_level_id", "time_recalled", "recall_count"}).
			AddRow("m1", int64(1), nil, int64(0), int32(0)).
			RowError(0, errors.New("boom")))

	if _, err := d.ConsolidateMemories(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateMemories_DeleteErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories WHERE event_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "timestamp", "significance_level_id", "time_recalled", "recall_count"}).
			AddRow("m1", int64(1), nil, int64(0), int32(0)))
	mock.ExpectBegin().WillReturnError(errors.New("boom"))

	if _, err := d.ConsolidateMemories(context.Background(), &stubServer{consolidateMemories: true}); err == nil {
		t.Fatal("expected the delete failure to propagate")
	}

	expectationsMet(t, mock)
}

// --- ConsolidateEventMemories ---

// consolidateEventMemoriesColumns mirrors the ten columns ConsolidateEventMemories' query
// projects: m.id, m.timestamp, m.significance_level_id, m.time_recalled, m.recall_count,
// m.event_id, e.significance_level_id, relationship_significance, memories_consolidated, e.id.
var consolidateEventMemoriesColumns = []string{
	"id", "timestamp", "significance_level_id", "time_recalled", "recall_count",
	"event_id", "e_significance_level_id", "relationship_significance", "memories_consolidated", "e_id",
}

func TestConsolidateEventMemories_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).WillReturnError(errors.New("boom"))

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateEventMemories_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(consolidateEventMemoriesColumns).
			AddRow("m1", "not-an-int", nil, int64(0), int32(0), "e1", nil, int64(0), false, "e1"))

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateEventMemories_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(consolidateEventMemoriesColumns).
			AddRow("m1", int64(1), nil, int64(0), int32(0), "e1", nil, int64(0), false, "e1").
			RowError(0, errors.New("boom")))

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// TestConsolidateEventMemories_DeleteMemoriesErrorIsBestEffort drives the bulk memory-delete
// failure (deleteMemoriesIfUnrecalled itself erroring): best-effort, so the function must still
// run the per-event cleanup and return the error rather than stopping short.
func TestConsolidateEventMemories_DeleteMemoriesErrorIsBestEffort(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	// One memory on event e1, selected for deletion (ShouldConsolidateMemory true below).
	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(consolidateEventMemoriesColumns).
			AddRow("m1", int64(1), nil, int64(0), int32(0), "e1", nil, int64(0), false, "e1"))

	// deleteMemoriesIfUnrecalled's beginTx fails.
	mock.ExpectBegin().WillReturnError(errors.New("boom"))

	// The per-event cleanup still runs despite the delete failure: e1 has no undeleted memory, so
	// DeleteEventIfEmpty is attempted and (here) succeeds with nothing to delete.
	mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events SET memories_consolidated`).WillReturnResult(sqlmock.NewResult(0, 1))

	_, events, _, err := d.ConsolidateEventMemories(context.Background(), &stubServer{consolidateMemories: true})
	if err == nil {
		t.Fatal("expected the bulk delete failure to surface")
	}

	if events != 1 {
		t.Fatalf("events seen = %d, want 1", events)
	}

	expectationsMet(t, mock)
}

// TestConsolidateEventMemories_PerEventCleanupErrorsAreBestEffort drives both the
// DeleteEventIfEmpty and setEventConsolidated failure branches in the per-event cleanup loop: both
// are logged and folded into the first-seen retErr rather than stopping the loop.
func TestConsolidateEventMemories_PerEventCleanupErrorsAreBestEffort(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(consolidateEventMemoriesColumns).
			AddRow("m1", int64(1), nil, int64(0), int32(0), "e1", nil, int64(0), false, "e1"))

	// The bulk delete succeeds this time...
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memories WHERE id`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// ...but both per-event cleanup steps fail.
	mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnError(errors.New("delete failed"))
	mock.ExpectExec(`UPDATE events SET memories_consolidated`).WillReturnError(errors.New("consolidate failed"))

	memories, events, eventsDeleted, err := d.ConsolidateEventMemories(context.Background(), &stubServer{consolidateMemories: true})
	if err == nil {
		t.Fatal("expected a per-event cleanup error to surface")
	}

	if memories != 1 || events != 1 || eventsDeleted != 0 {
		t.Fatalf("got (memories=%d, events=%d, eventsDeleted=%d), want (1, 1, 0)", memories, events, eventsDeleted)
	}

	expectationsMet(t, mock)
}
