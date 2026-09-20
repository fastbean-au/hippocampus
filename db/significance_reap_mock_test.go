package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The reap's failure paths, driven through sqlmock because a real store cannot be made to fail one
// statement of a multi-statement pass on demand.
//
// They are worth having individually rather than as one "it returns the error" test, because the
// pass is not atomic: the delete commits before the two marks run, so a failure after it has to
// report the rows it already removed. A reap that lost that count would leave the gauge describing
// a registry that is no longer there until the next cycle.

const (
	reapMarksQuery      = `SELECT id, unused_since FROM significance_levels`
	reapReferencedQuery = `SELECT DISTINCT significance_level_id FROM`
	reapDeleteQuery     = `DELETE FROM significance_levels`
	reapMarkQuery       = `UPDATE significance_levels SET unused_since`
	reapGrace           = 7 * 24 * time.Hour
)

// reapMarkRows is the registry as the reap reads it: one level per (id, mark) pair.
func reapMarkRows(marks ...sql.NullInt64) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "unused_since"})

	for i, mark := range marks {
		rows = rows.AddRow(int64(i+1), mark)
	}

	return rows
}

// staleMark is a mark old enough for the reap's cutoff to have passed it.
func staleMark() sql.NullInt64 {
	return sql.NullInt64{Int64: time.Now().Add(-30 * 24 * time.Hour).UnixNano(), Valid: true}
}

func TestReapSignificanceLevels_RegistryReadError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(reapMarksQuery).WillReturnError(errors.New("boom"))

	if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReapSignificanceLevels_RegistryScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(reapMarksQuery).
		WillReturnRows(sqlmock.NewRows([]string{"id", "unused_since"}).AddRow("not-an-id", nil))

	if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReapSignificanceLevels_RegistryRowsError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(reapMarksQuery).
		WillReturnRows(reapMarkRows(sql.NullInt64{}).RowError(0, errors.New("boom")))

	if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// An empty registry is not an error and costs nothing further: there is nothing to compare against
// the item tables, so the reference scan never runs.
func TestReapSignificanceLevels_EmptyRegistrySkipsTheScan(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(reapMarksQuery).WillReturnRows(reapMarkRows())

	registry, err := d.ReapSignificanceLevels(context.Background(), reapGrace)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}

	if registry.Levels != 0 || registry.Reaped != 0 {
		t.Errorf("registry = %+v, want zero", registry)
	}

	expectationsMet(t, mock)
}

func TestReapSignificanceLevels_ReferenceScanErrors(t *testing.T) {
	cases := []struct {
		name  string
		rows  *sqlmock.Rows
		fails bool
	}{
		{name: "query", fails: true},
		{name: "scan", rows: sqlmock.NewRows([]string{"significance_level_id"}).AddRow("not-an-id")},
		{
			name: "rows",
			rows: sqlmock.NewRows([]string{"significance_level_id"}).
				AddRow(int64(1)).
				RowError(0, errors.New("boom")),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			mock.ExpectQuery(reapMarksQuery).WillReturnRows(reapMarkRows(sql.NullInt64{}))

			if c.fails {
				mock.ExpectQuery(reapReferencedQuery).WillReturnError(errors.New("boom"))
			} else {
				mock.ExpectQuery(reapReferencedQuery).WillReturnRows(c.rows)
			}

			if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
				t.Fatal("expected an error")
			}

			expectationsMet(t, mock)
		})
	}
}

// expectReapThroughReference scripts a pass up to the point where one marked, unreferenced level is
// ready to be deleted.
func expectReapThroughReference(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(reapMarksQuery).WillReturnRows(reapMarkRows(staleMark()))

	for range 2 {
		mock.ExpectQuery(reapReferencedQuery).
			WillReturnRows(sqlmock.NewRows([]string{"significance_level_id"}))
	}
}

func TestReapSignificanceLevels_DeleteErrors(t *testing.T) {
	t.Run("exec", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		expectReapThroughReference(mock)
		mock.ExpectExec(reapDeleteQuery).WillReturnError(errors.New("boom"))

		registry, err := d.ReapSignificanceLevels(context.Background(), reapGrace)
		if err == nil {
			t.Fatal("expected an error")
		}

		if registry.Reaped != 0 {
			t.Errorf("Reaped = %d, want 0 - nothing was removed", registry.Reaped)
		}

		expectationsMet(t, mock)
	})

	// RowsAffected failing is not the same as the DELETE failing: the rows are gone, and the pass
	// has no way to say how many. Reporting is what it loses, not correctness.
	t.Run("rows affected", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		expectReapThroughReference(mock)
		mock.ExpectExec(reapDeleteQuery).WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

		if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

// A mark that cannot be written fails the pass, but the rows the delete already removed are still
// reported - the gauge is published from this, and a registry that has shrunk must not keep
// reporting its old size until the next cycle.
func TestReapSignificanceLevels_MarkErrorStillReportsTheDeletions(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	// Two levels: one stale and unreferenced (deleted), one unmarked and unreferenced (to mark).
	mock.ExpectQuery(reapMarksQuery).WillReturnRows(reapMarkRows(staleMark(), sql.NullInt64{}))

	for range 2 {
		mock.ExpectQuery(reapReferencedQuery).
			WillReturnRows(sqlmock.NewRows([]string{"significance_level_id"}))
	}

	mock.ExpectExec(reapDeleteQuery).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(reapMarkQuery).WillReturnError(errors.New("boom"))

	registry, err := d.ReapSignificanceLevels(context.Background(), reapGrace)
	if err == nil {
		t.Fatal("expected an error")
	}

	if registry.Reaped != 1 || registry.Levels != 1 {
		t.Errorf("registry = %+v, want 1 reaped and 1 left", registry)
	}

	expectationsMet(t, mock)
}

// The unmark of a level carried again runs before the mark of one that has just fallen idle, and a
// failure there fails the pass in the same way.
func TestReapSignificanceLevels_UnmarkError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(reapMarksQuery).WillReturnRows(reapMarkRows(staleMark()))

	// Referenced by a memory although marked, which is the state the unmark exists to correct.
	mock.ExpectQuery(reapReferencedQuery).
		WillReturnRows(sqlmock.NewRows([]string{"significance_level_id"}).AddRow(int64(1)))
	mock.ExpectQuery(reapReferencedQuery).
		WillReturnRows(sqlmock.NewRows([]string{"significance_level_id"}))

	mock.ExpectExec(reapMarkQuery).WillReturnError(errors.New("boom"))

	if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// The registry lock is taken before anything is read, so a lock this instance cannot get stops the
// pass rather than letting two instances renumber and reap at once.
func TestReapSignificanceLevels_RegistryLockError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`pg_advisory_lock`).WillReturnError(errors.New("boom"))

	if _, err := d.ReapSignificanceLevels(context.Background(), reapGrace); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestReapSignificanceLevels_CountErrorWhenDisabled(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM significance_levels`).WillReturnError(errors.New("boom"))

	if _, err := d.ReapSignificanceLevels(context.Background(), 0); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- the writer's half: claiming a marked level back ---

func TestClaimSignificanceLevel_Errors(t *testing.T) {
	t.Run("exec", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(reapMarkQuery).WillReturnError(errors.New("boom"))

		if _, err := d.claimSignificanceLevel(context.Background(), 1); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	// A claim that cannot say whether it took effect must not be read as having taken effect: the
	// id would then be handed to a write, which is the one outcome the mark exists to prevent.
	t.Run("rows affected", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(reapMarkQuery).WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

		claimed, err := d.claimSignificanceLevel(context.Background(), 1)
		if err == nil {
			t.Fatal("expected an error")
		}

		if claimed {
			t.Error("a claim that could not be confirmed reported success")
		}

		expectationsMet(t, mock)
	})
}

// findLevel surfaces a failed claim rather than swallowing it, so a write is never resolved against
// a level whose survival is unknown.
func TestFindLevel_ClaimError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT id, unused_since FROM significance_levels WHERE level_rank`).
		WillReturnRows(reapMarkRows(staleMark()))
	mock.ExpectExec(reapMarkQuery).WillReturnError(errors.New("boom"))

	if _, _, err := d.findLevel(context.Background(), 5); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// findOrCreateLevelTx clears the mark inside the caller's transaction, under the registry lock, so
// it needs no confirmation - but the statement can still fail, and it must fail the write rather
// than return a level whose mark is still running.
func TestFindOrCreateLevelTx_ClearMarkError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectQuery(`SELECT id, unused_since FROM significance_levels WHERE level_rank`).
		WillReturnRows(reapMarkRows(staleMark()))
	mock.ExpectExec(reapMarkQuery).WillReturnError(errors.New("boom"))

	if _, err := d.findOrCreateLevelTx(context.Background(), tx, 5); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}
