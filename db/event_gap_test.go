package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// This file targets event.go's remaining coverage gaps: a mix of cheap real-SQLite scenarios
// (invalid input, filters not otherwise exercised) and mocked-handle scenarios for branches a real
// connection can't be made to fail on demand (RowsAffected/rows.Err()/mid-loop delete failures).

// --- CreateEvent / UpdateEvent: validation and filter branches not otherwise exercised ---

func TestCreateEvent_ValidateError(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{
		Id: "e1", Name: "bad", TimeStart: 100, Significance: -1,
	}); err == nil {
		t.Fatal("expected CreateEvent to reject a negative significance")
	}
}

func TestUpdateEvent_TimeEndBranch(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})

	if existed, err := db.UpdateEvent(context.Background(), types.Event{Id: "e1", TimeEnd: 200}); err != nil || !existed {
		t.Fatalf("UpdateEvent(TimeEnd) = (%v, %v), want (true, nil)", existed, err)
	}

	got, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.TimeEnd != 200 {
		t.Errorf("TimeEnd = %d, want 200", got.TimeEnd)
	}
}

func TestUpdateEvent_ExecErrorOnClosedDB(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := db.UpdateEvent(context.Background(), types.Event{Id: "e1", Name: "new-name"}); err == nil {
		t.Error("expected UpdateEvent to fail once the underlying exec runs against a closed database")
	}
}

// TestEventFilterConditions_TimeStartMax verifies the TimeStartMax arm of eventFilterConditions,
// otherwise never exercised by any existing filter test.
func TestEventFilterConditions_TimeStartMax(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "in-range", Name: "a", TimeStart: 100, Significance: 1})
	mustCreateEvent(t, db, types.Event{Id: "too-late", Name: "b", TimeStart: 500, Significance: 1})

	got, err := db.GetEvents(context.Background(), EventFilter{TimeStartMax: 200})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Id != "in-range" {
		t.Errorf("GetEvents(TimeStartMax=200) = %+v, want only 'in-range'", *got)
	}
}

// --- DeleteEvent / DeleteEventIfEmpty: RowsAffected() failing, driven against a mocked handle
// since a real SQLite/driver Result never fails RowsAffected(). ---

func TestDeleteEvent_RowsAffectedError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

	if _, err := d.DeleteEvent(context.Background(), "e1"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestDeleteEventIfEmpty_RowsAffectedError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

	if _, err := d.DeleteEventIfEmpty(context.Background(), "e1"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- GetEvent / GetEvents: scanEvent's own Scan error (wrong column shape), the rows.Err()
// branch reached when Next() itself fails before yielding a row, and its propagation through both
// callers. ---

// eventRowsColumns mirrors eventColumns (id, time_start, time_end, significance, name,
// description, memories_consolidated, relationship_significance, relationships, group_name).
var eventRowsColumns = []string{
	"id", "time_start", "time_end", "significance", "name",
	"description", "memories_consolidated", "relationship_significance", "relationships", "group_name",
}

func TestGetEvent_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	// "not-an-int" in the time_start slot cannot convert to int64, so rows.Scan fails inside
	// scanEvent, propagating through GetEvent.
	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(eventRowsColumns).
		AddRow("e1", "not-an-int", int64(0), int32(1), "n", "d", false, int64(0), "[]", ""))

	if _, err := d.GetEvent(context.Background(), "e1"); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestGetEvent_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(eventRowsColumns).
		AddRow("e1", int64(1), int64(0), int32(1), "n", "d", false, int64(0), "[]", "").
		RowError(0, errors.New("boom")))

	if _, err := d.GetEvent(context.Background(), "e1"); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// TestGetEvent_MalformedRelationshipsJSON drives scanEvent's json.Unmarshal error branch via a
// real database: a row whose relationships column holds invalid JSON (only reachable by writing
// it directly - CreateEvent/ImportEvents always marshal a valid []Relationship first).
func TestGetEvent_MalformedRelationshipsJSON(t *testing.T) {
	db := newTestDB(t)

	mustCreateEvent(t, db, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 1})

	if _, err := db.sql.Exec(`UPDATE events SET relationships = 'not-json' WHERE id = ?`, "e1"); err != nil {
		t.Fatalf("corrupt relationships column: %s", err)
	}

	if _, err := db.GetEvent(context.Background(), "e1"); err == nil {
		t.Fatal("expected GetEvent to surface the JSON unmarshal error")
	}
}

func TestGetEvents_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(eventRowsColumns).
		AddRow("e1", "not-an-int", int64(0), int32(1), "n", "d", false, int64(0), "[]", ""))

	if _, err := d.GetEvents(context.Background(), EventFilter{}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestGetEvents_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM `).WillReturnRows(sqlmock.NewRows(eventRowsColumns).
		AddRow("e1", int64(1), int64(0), int32(1), "n", "d", false, int64(0), "[]", "").
		RowError(0, errors.New("boom")))

	if _, err := d.GetEvents(context.Background(), EventFilter{}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// --- ConsolidateEvents: the query/scan/rows.Err() failure branches, and a DeleteEventIfEmpty
// failure mid-loop (best-effort - the loop must continue to the next id and still surface the
// first error). ---

func TestConsolidateEvents_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).WillReturnError(errors.New("boom"))

	if _, err := d.ConsolidateEvents(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateEvents_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "time_start", "time_end", "significance", "relationship_significance"}).
			AddRow("e1", "not-an-int", int64(0), int32(0), int64(0)))

	if _, err := d.ConsolidateEvents(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateEvents_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "time_start", "time_end", "significance", "relationship_significance"}).
			AddRow("e1", int64(1), int64(0), int32(0), int64(0)).
			RowError(0, errors.New("boom")))

	if _, err := d.ConsolidateEvents(context.Background(), &stubServer{}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

func TestConsolidateEvents_DeleteErrorIsBestEffort(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "time_start", "time_end", "significance", "relationship_significance"}).
			AddRow("e1", int64(1), int64(0), int32(0), int64(0)))
	mock.ExpectExec(`DELETE FROM events WHERE id`).WillReturnError(errors.New("boom"))

	_, err := d.ConsolidateEvents(context.Background(), &stubServer{consolidateEvents: true})
	if err == nil {
		t.Fatal("expected the per-event delete failure to surface")
	}

	expectationsMet(t, mock)
}

// --- CalculateSignificancePercentile: scan and rows.Err() failure branches. ---

func TestCalculateSignificancePercentile_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM events e`).
		WillReturnRows(sqlmock.NewRows([]string{"significance"}).AddRow("not-an-int"))

	if _, err := d.CalculateSignificancePercentile(context.Background(), 50); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestCalculateSignificancePercentile_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`FROM events e`).
		WillReturnRows(sqlmock.NewRows([]string{"significance"}).AddRow(int32(5)).RowError(0, errors.New("boom")))

	if _, err := d.CalculateSignificancePercentile(context.Background(), 50); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}
