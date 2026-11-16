package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The tests in this file drive ReencryptBodies' failure and skip paths with a mocked database/sql
// handle: a query or UPDATE that fails, a row that will not scan, a row that changed between being
// read and being rewritten. None of them can be provoked reliably against a real store.

// Patterns for the statements ReencryptBodies issues, as sqlmock regular expressions.
const (
	reencryptMemoriesQuery  = `SELECT id, body, is_compressed, body_key_id, is_binary FROM memories`
	reencryptMemoryUpdate   = `UPDATE memories SET body = \?, is_compressed = \?, body_key_id = \?`
	reencryptCallbacksQuery = `SELECT seq, payload, is_compressed, payload_key_id FROM callback_queue`
	reencryptCallbackUpdate = `UPDATE callback_queue SET payload = \?`
)

var errReencryptMock = errors.New("mock failure")

// newReencryptMockDB is a mocked SQLite DB sealing under a fresh key, with the callback queue on or
// off.
func newReencryptMockDB(t *testing.T, callbackTable bool) (*DB, sqlmock.Sqlmock) {
	t.Helper()

	d, mock := newMockDB(t, driverSQLite)
	ring, _ := testKeyring(t)

	d.SetKeyring(ring)
	d.callbackTable = callbackTable

	return d, mock
}

// memoryCandidateRows is one plaintext memory row as reencryptCandidates reads it.
func memoryCandidateRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "body", "is_compressed", "body_key_id", "is_binary"}).
		AddRow("m1", []byte("a plain body"), false, "", false)
}

// noMoreCandidates is the empty batch that ends ReencryptBodies' memory pass.
func noMoreCandidates() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "body", "is_compressed", "body_key_id", "is_binary"})
}

// queuedCallbackRows is one plaintext callback payload as reencryptCallbacks reads it.
func queuedCallbackRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"seq", "payload", "is_compressed", "payload_key_id"}).
		AddRow(int64(7), []byte(`{}`), false, "")
}

func TestReencryptBodies_CandidateQueryError(t *testing.T) {
	d, mock := newReencryptMockDB(t, false)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnError(errReencryptMock)

	if _, err := d.ReencryptBodies(context.Background()); !errors.Is(err, errReencryptMock) {
		t.Errorf("ReencryptBodies error = %v, want the query failure", err)
	}

	expectationsMet(t, mock)
}

func TestReencryptBodies_CandidateScanError(t *testing.T) {
	d, mock := newReencryptMockDB(t, false)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(
		sqlmock.NewRows([]string{"id", "body", "is_compressed", "body_key_id", "is_binary"}).
			AddRow("m1", []byte("body"), "not a bool", "", false),
	)

	if _, err := d.ReencryptBodies(context.Background()); err == nil {
		t.Error("ReencryptBodies succeeded over a row that does not scan")
	}

	expectationsMet(t, mock)
}

func TestReencryptBodies_UnopenableMemoryFails(t *testing.T) {
	d, mock := newReencryptMockDB(t, false)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(
		sqlmock.NewRows([]string{"id", "body", "is_compressed", "body_key_id", "is_binary"}).
			AddRow("m1", []byte("sealed elsewhere"), false, "deadbeef", false),
	)

	if _, err := d.ReencryptBodies(context.Background()); err == nil {
		t.Error("ReencryptBodies succeeded over a body under a key the ring does not hold")
	}

	expectationsMet(t, mock)
}

func TestReencryptBodies_MemoryUpdateError(t *testing.T) {
	d, mock := newReencryptMockDB(t, false)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(memoryCandidateRows())
	mock.ExpectExec(reencryptMemoryUpdate).WillReturnError(errReencryptMock)

	if _, err := d.ReencryptBodies(context.Background()); !errors.Is(err, errReencryptMock) {
		t.Errorf("ReencryptBodies error = %v, want the update failure", err)
	}

	expectationsMet(t, mock)
}

func TestReencryptBodies_MemoryRowsAffectedError(t *testing.T) {
	d, mock := newReencryptMockDB(t, false)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(memoryCandidateRows())
	mock.ExpectExec(reencryptMemoryUpdate).WillReturnResult(sqlmock.NewErrorResult(errReencryptMock))

	if _, err := d.ReencryptBodies(context.Background()); !errors.Is(err, errReencryptMock) {
		t.Errorf("ReencryptBodies error = %v, want the RowsAffected failure", err)
	}

	expectationsMet(t, mock)
}

// TestReencryptBodies_CountsChangedRowsAsSkipped covers both passes finding their row changed under
// them: a memory rewritten meanwhile and a callback delivered meanwhile are each left alone and
// counted as skipped, not rewritten.
func TestReencryptBodies_CountsChangedRowsAsSkipped(t *testing.T) {
	d, mock := newReencryptMockDB(t, true)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(memoryCandidateRows())
	mock.ExpectExec(reencryptMemoryUpdate).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(noMoreCandidates())
	mock.ExpectQuery(reencryptCallbacksQuery).WillReturnRows(queuedCallbackRows())
	mock.ExpectExec(reencryptCallbackUpdate).WillReturnResult(sqlmock.NewResult(0, 0))

	stats, err := d.ReencryptBodies(context.Background())
	if err != nil {
		t.Fatalf("ReencryptBodies: %s", err)
	}

	if stats.Memories != 0 || stats.Callbacks != 0 || stats.Skipped != 2 {
		t.Errorf("stats = %+v, want nothing rewritten and 2 skipped", stats)
	}

	expectationsMet(t, mock)
}

func TestReencryptBodies_NoCallbackTableSkipsTheQueue(t *testing.T) {
	d, mock := newReencryptMockDB(t, false)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(noMoreCandidates())

	stats, err := d.ReencryptBodies(context.Background())
	if err != nil {
		t.Fatalf("ReencryptBodies: %s", err)
	}

	if stats != (ReencryptStats{}) {
		t.Errorf("stats = %+v, want nothing done", stats)
	}

	expectationsMet(t, mock)
}

func TestReencryptBodies_CallbackQueryError(t *testing.T) {
	d, mock := newReencryptMockDB(t, true)

	mock.ExpectQuery(reencryptMemoriesQuery).WillReturnRows(noMoreCandidates())
	mock.ExpectQuery(reencryptCallbacksQuery).WillReturnError(errReencryptMock)

	if _, err := d.ReencryptBodies(context.Background()); !errors.Is(err, errReencryptMock) {
		t.Errorf("ReencryptBodies error = %v, want the callback query failure", err)
	}

	expectationsMet(t, mock)
}

func TestReencryptCallbacks_ScanError(t *testing.T) {
	d, mock := newReencryptMockDB(t, true)

	mock.ExpectQuery(reencryptCallbacksQuery).WillReturnRows(
		sqlmock.NewRows([]string{"seq", "payload", "is_compressed", "payload_key_id"}).
			AddRow("not a number", []byte(`{}`), false, ""),
	)

	if _, _, err := d.reencryptCallbacks(context.Background(), d.keyring.Active()); err == nil {
		t.Error("reencryptCallbacks succeeded over a row that does not scan")
	}

	expectationsMet(t, mock)
}

func TestReencryptCallbacks_UnopenablePayloadFails(t *testing.T) {
	d, mock := newReencryptMockDB(t, true)

	mock.ExpectQuery(reencryptCallbacksQuery).WillReturnRows(
		sqlmock.NewRows([]string{"seq", "payload", "is_compressed", "payload_key_id"}).
			AddRow(int64(7), []byte("sealed elsewhere"), false, "deadbeef"),
	)

	if _, _, err := d.reencryptCallbacks(context.Background(), d.keyring.Active()); err == nil {
		t.Error("reencryptCallbacks succeeded over a payload under a key the ring does not hold")
	}

	expectationsMet(t, mock)
}

func TestReencryptCallbacks_UpdateError(t *testing.T) {
	d, mock := newReencryptMockDB(t, true)

	mock.ExpectQuery(reencryptCallbacksQuery).WillReturnRows(queuedCallbackRows())
	mock.ExpectExec(reencryptCallbackUpdate).WillReturnError(errReencryptMock)

	if _, _, err := d.reencryptCallbacks(context.Background(), d.keyring.Active()); !errors.Is(err, errReencryptMock) {
		t.Errorf("reencryptCallbacks error = %v, want the update failure", err)
	}

	expectationsMet(t, mock)
}

func TestReencryptCallbacks_RowsAffectedError(t *testing.T) {
	d, mock := newReencryptMockDB(t, true)

	mock.ExpectQuery(reencryptCallbacksQuery).WillReturnRows(queuedCallbackRows())
	mock.ExpectExec(reencryptCallbackUpdate).WillReturnResult(sqlmock.NewErrorResult(errReencryptMock))

	if _, _, err := d.reencryptCallbacks(context.Background(), d.keyring.Active()); !errors.Is(err, errReencryptMock) {
		t.Errorf("reencryptCallbacks error = %v, want the RowsAffected failure", err)
	}

	expectationsMet(t, mock)
}
