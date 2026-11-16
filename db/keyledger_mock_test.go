package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The tests in this file drive the key ledger's failure paths with a mocked database/sql handle: a
// statement that fails, a row that will not scan. None can be provoked reliably against a real
// store.

// Patterns for the statements the ledger issues, as sqlmock regular expressions.
const (
	ledgerProbe        = `SELECT 1 FROM ` + keyLedgerTable + ` WHERE 1 = 0`
	ledgerSelect       = `SELECT key_id, provider, kms_key, wrapped_key, created_at, drained_at FROM ` + keyLedgerTable
	ledgerBodyKeys     = `SELECT DISTINCT body_key_id FROM memories`
	ledgerCallbackKeys = `SELECT DISTINCT payload_key_id FROM ` + callbackQueueTable
	ledgerUpsert       = `INSERT INTO ` + keyLedgerTable
	ledgerBuildUpsert  = `INSERT INTO ` + keyLedgerBuildTable
	ledgerDrain        = `UPDATE ` + keyLedgerTable + ` SET drained_at`
)

var errLedgerMock = errors.New("mock failure")

// ledgerColumns is the ledger as KeyLedger reads it.
func ledgerColumns() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"key_id", "provider", "kms_key", "wrapped_key", "created_at", "drained_at"})
}

// expectLedgerBuild queues a migration-19 run as far as its seeding scans.
func expectLedgerBuild(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(ledgerProbe).WillReturnError(errors.New("no such table"))
	mock.ExpectExec(`DROP TABLE IF EXISTS ` + keyLedgerBuildTable).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS ` + keyLedgerBuildTable).WillReturnResult(sqlmock.NewResult(0, 0))
}

func TestInitKeyLedger_Failures(t *testing.T) {
	cases := map[string]func(mock sqlmock.Sqlmock){
		"the create": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(ledgerProbe).WillReturnError(errors.New("no such table"))
			mock.ExpectExec(`DROP TABLE IF EXISTS ` + keyLedgerBuildTable).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(`CREATE TABLE IF NOT EXISTS ` + keyLedgerBuildTable).WillReturnError(errLedgerMock)
		},
		"the body scan": func(mock sqlmock.Sqlmock) {
			expectLedgerBuild(mock)
			mock.ExpectQuery(ledgerBodyKeys).WillReturnError(errLedgerMock)
		},
		"a key id that will not scan": func(mock sqlmock.Sqlmock) {
			expectLedgerBuild(mock)
			mock.ExpectQuery(ledgerBodyKeys).WillReturnRows(sqlmock.NewRows([]string{"body_key_id"}).AddRow(nil))
		},
		"the seeding insert": func(mock sqlmock.Sqlmock) {
			expectLedgerBuild(mock)
			mock.ExpectQuery(ledgerBodyKeys).WillReturnRows(sqlmock.NewRows([]string{"body_key_id"}).AddRow("0a1b2c3d"))
			mock.ExpectQuery(ledgerCallbackKeys).WillReturnRows(sqlmock.NewRows([]string{"payload_key_id"}))
			mock.ExpectExec(ledgerBuildUpsert).WillReturnError(errLedgerMock)
		},
		"the rename": func(mock sqlmock.Sqlmock) {
			expectLedgerBuild(mock)
			mock.ExpectQuery(ledgerBodyKeys).WillReturnRows(sqlmock.NewRows([]string{"body_key_id"}))
			mock.ExpectQuery(ledgerCallbackKeys).WillReturnRows(sqlmock.NewRows([]string{"payload_key_id"}))
			mock.ExpectExec(`ALTER TABLE ` + keyLedgerBuildTable + ` RENAME TO`).WillReturnError(errLedgerMock)
		},
	}

	for name, expect := range cases {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			expect(mock)

			if err := d.initKeyLedger(); err == nil {
				t.Error("initKeyLedger succeeded")
			}
		})
	}
}

func TestKeyLedger_Failures(t *testing.T) {
	ctx := context.Background()

	t.Run("the read", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(ledgerSelect).WillReturnError(errLedgerMock)

		if _, err := d.KeyLedger(ctx); !errors.Is(err, errLedgerMock) {
			t.Errorf("KeyLedger: %v", err)
		}
	})

	t.Run("a row that will not scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(ledgerSelect).WillReturnRows(ledgerColumns().AddRow("k", "", nil, nil, "not a number", 0))

		if _, err := d.KeyLedger(ctx); err == nil {
			t.Error("KeyLedger scanned a bad row")
		}
	})

	t.Run("RecordKey", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(ledgerUpsert).WillReturnError(errLedgerMock)

		if err := d.RecordKey(ctx, "k"); !errors.Is(err, errLedgerMock) {
			t.Errorf("RecordKey: %v", err)
		}
	})

	t.Run("RecordWrappedKey", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(ledgerUpsert).WillReturnError(errLedgerMock)

		if err := d.RecordWrappedKey(ctx, KeyLedgerEntry{KeyId: "k"}); !errors.Is(err, errLedgerMock) {
			t.Errorf("RecordWrappedKey: %v", err)
		}
	})

	t.Run("KeyLedgerAvailable", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(ledgerProbe).WillReturnRows(sqlmock.NewRows([]string{"1"}))
		mock.ExpectQuery(ledgerProbe).WillReturnError(errors.New("no such table"))

		if !d.KeyLedgerAvailable() {
			t.Error("a ledger that answers is reported missing")
		}

		if d.KeyLedgerAvailable() {
			t.Error("a ledger that does not answer is reported available")
		}
	})
}

func TestDrainKeys_Failures(t *testing.T) {
	ctx := context.Background()

	emptyScans := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(ledgerBodyKeys).WillReturnRows(sqlmock.NewRows([]string{"body_key_id"}))
		mock.ExpectQuery(ledgerCallbackKeys).WillReturnRows(sqlmock.NewRows([]string{"payload_key_id"}))
	}

	cases := map[string]func(mock sqlmock.Sqlmock){
		"the scan": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(ledgerBodyKeys).WillReturnError(errLedgerMock)
		},
		"the ledger read": func(mock sqlmock.Sqlmock) {
			emptyScans(mock)
			mock.ExpectQuery(ledgerSelect).WillReturnError(errLedgerMock)
		},
		"the update": func(mock sqlmock.Sqlmock) {
			emptyScans(mock)
			mock.ExpectQuery(ledgerSelect).WillReturnRows(ledgerColumns().AddRow("old", "", nil, nil, int64(1), int64(0)))
			mock.ExpectExec(ledgerDrain).WillReturnError(errLedgerMock)
		},
	}

	for name, expect := range cases {
		t.Run(name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			expect(mock)

			if _, err := d.DrainKeys(ctx, "current"); !errors.Is(err, errLedgerMock) {
				t.Errorf("DrainKeys: %v", err)
			}
		})
	}
}
