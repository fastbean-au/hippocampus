package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// memoryReadRows builds an empty result set shaped like memoryColumns - the joined read view every
// non-RETURNING reader selects.
func memoryReadRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "timestamp", "significance", "event_id", "body", "is_binary",
		"time_recalled", "recall_count", "is_summary", "group_name",
		"is_compressed", "metadata", "link_significance",
	})
}

// memoryStoredRows builds an empty result set shaped like memoryReturningColumns - the physical
// row, which carries significance_level_id instead of a resolved rank.
func memoryStoredRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "timestamp", "significance_level_id", "event_id", "body", "is_binary",
		"time_recalled", "recall_count", "is_summary", "group_name",
		"is_compressed", "metadata", "link_significance",
	})
}

// okMemoryRow is a well-formed row, for the cases that need a second row to fail on.
func okMemoryRow(rows *sqlmock.Rows, id string) *sqlmock.Rows {
	return rows.AddRow(id, 100, 5, "", []byte("body"), false, 0, 0, false, "", false, nil, 0)
}

// --- scanMemory / scanMemoryStored: the two row decoders, and the two ways a stored row can be
// undecodable. Both are reachable only from a store whose rows are already damaged, which is
// exactly why they must not be silently swallowed. ---

// TestScanMemory_MetadataDecodeErrorPropagates drives a row whose metadata column is not JSON.
func TestScanMemory_MetadataDecodeErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	rows := memoryReadRows().
		AddRow("m1", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, []byte("{not json"), 0)

	mock.ExpectQuery(`SELECT`).WillReturnRows(rows)

	if _, err := d.GetMemoriesByIds(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected the metadata decode failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestScanMemory_DecompressErrorPropagates drives a row flagged compressed whose body is not a
// gzip stream. Decompression follows the row's own flag rather than the current configuration, so
// this is what a corrupted flag looks like.
func TestScanMemory_DecompressErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	rows := memoryReadRows().
		AddRow("m1", 100, 5, "", []byte("not a gzip stream"), false, 0, 0, false, "", true, nil, 0)

	mock.ExpectQuery(`SELECT`).WillReturnRows(rows)

	if _, err := d.GetMemoriesByIds(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected the decompression failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestScanMemoryStored_DecodeErrorsPropagate drives the same two failures through the RETURNING
// decoder, which is a separate function and so a separate chance to swallow them.
func TestScanMemoryStored_DecodeErrorsPropagate(t *testing.T) {
	t.Run("metadata", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		rows := memoryStoredRows().
			AddRow("m1", 100, nil, "", []byte("body"), false, 0, 0, false, "", false, []byte("{not json"), 0)

		mock.ExpectQuery(`UPDATE memories SET time_recalled`).WillReturnRows(rows)

		if _, err := d.RecallMemories(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the metadata decode failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("body", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		rows := memoryStoredRows().
			AddRow("m1", 100, nil, "", []byte("not a gzip stream"), false, 0, 0, false, "", true, nil, 0)

		mock.ExpectQuery(`UPDATE memories SET time_recalled`).WillReturnRows(rows)

		if _, err := d.RecallMemories(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the decompression failure to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestRecallMemoriesReturning_Failures walks the RETURNING arm's remaining failure points: the row
// error, and the rank resolution that has to follow because RETURNING cannot join.
func TestRecallMemoriesReturning_Failures(t *testing.T) {
	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		rows := memoryStoredRows().
			AddRow("m1", 100, nil, "", []byte("body"), false, 0, 0, false, "", false, nil, 0).
			RowError(1, errors.New("boom")).
			AddRow("m2", 100, nil, "", []byte("body"), false, 0, 0, false, "", false, nil, 0)

		mock.ExpectQuery(`UPDATE memories SET time_recalled`).WillReturnRows(rows)

		if _, err := d.RecallMemories(context.Background(), []string{"m1", "m2"}); err == nil {
			t.Fatal("expected the row error to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("rank resolution", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		rows := memoryStoredRows().
			AddRow("m1", 100, nil, "", []byte("body"), false, 0, 0, false, "", false, nil, 0)

		mock.ExpectQuery(`UPDATE memories SET time_recalled`).WillReturnRows(rows)
		mock.ExpectQuery(`SELECT id, level_rank FROM significance_levels`).WillReturnError(errors.New("boom"))

		if _, err := d.RecallMemories(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the rank resolution failure to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestRecallMemories_EmptyIdsIsANoOp covers the early return, which must issue no statement.
func TestRecallMemories_EmptyIdsIsANoOp(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	memories, err := d.RecallMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("RecallMemories(nil): %v", err)
	}

	if memories == nil || len(*memories) != 0 {
		t.Errorf("expected an empty result, got %+v", memories)
	}

	expectationsMet(t, mock)
}

// TestRecallMemoriesMySQL_Failures walks the MySQL arm, which is a transaction rather than one
// RETURNING statement and so has four more places to fail.
func TestRecallMemoriesMySQL_Failures(t *testing.T) {
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
			name: "update",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "select",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "scan",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT`).WillReturnRows(memoryReadRows().
					AddRow("m1", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, []byte("{bad"), 0))
				mock.ExpectRollback()
			},
		},
		{
			name: "row error",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT`).WillReturnRows(okMemoryRow(memoryReadRows(), "m1").
					RowError(1, errors.New("boom")).
					AddRow("m2", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, nil, 0))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT`).WillReturnRows(okMemoryRow(memoryReadRows(), "m1"))
				mock.ExpectCommit().WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverMySQL)
			test.expect(mock)

			if _, err := d.RecallMemories(context.Background(), []string{"m1", "m2"}); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			expectationsMet(t, mock)
		})
	}
}

// --- the delete paths. Every one of them prunes links inside its own transaction, so the failure
// that matters most is the one that must roll the deletion back rather than leave the aggregate the
// consolidation scans read describing memories that are gone. ---

// TestDeleteMemoriesByIds_EmptyIsANoOp covers the early return.
func TestDeleteMemoriesByIds_EmptyIsANoOp(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	cnt, err := d.deleteMemoriesByIds(context.Background(), nil)
	if err != nil || cnt != 0 {
		t.Errorf("deleteMemoriesByIds(nil) = %d, %v; want 0, nil", cnt, err)
	}

	expectationsMet(t, mock)
}

// TestDeleteMemoriesByIds_Failures walks the delete, the link prune and the commit.
func TestDeleteMemoriesByIds_Failures(t *testing.T) {
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
				mock.ExpectExec(`DELETE FROM memories WHERE id IN`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "prune",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM memories WHERE id IN`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM memories WHERE id IN`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
				mock.ExpectCommit().WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)
			test.expect(mock)

			if _, err := d.deleteMemoriesByIds(context.Background(), []string{"m1"}); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			expectationsMet(t, mock)
		})
	}
}

// TestDeleteMemoriesIfUnrecalled_Failures walks the race-safe delete's own three failure points.
func TestDeleteMemoriesIfUnrecalled_Failures(t *testing.T) {
	snapshots := []memoryRecallSnapshot{{id: "m1"}}

	tests := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
	}{
		{
			name: "chunk delete",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM memories WHERE id = \?`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "prune",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM memories WHERE id = \?`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectExec(`DELETE FROM memories WHERE id = \?`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
				mock.ExpectCommit().WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)
			test.expect(mock)

			if _, err := d.deleteMemoriesIfUnrecalled(context.Background(), snapshots); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			expectationsMet(t, mock)
		})
	}
}

// TestDeleteChunkIfUnrecalled_ReturningArmFailures covers the Postgres arm, which deletes and reads
// back the ids in one statement.
func TestDeleteChunkIfUnrecalled_ReturningArmFailures(t *testing.T) {
	snapshots := []memoryRecallSnapshot{{id: "m1"}}

	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectBegin()
		mock.ExpectQuery(`DELETE FROM memories`).WillReturnError(errors.New("boom"))

		tx, _, err := d.beginTx(context.Background())
		if err != nil {
			t.Fatalf("beginTx: %v", err)
		}

		if _, err := d.deleteChunkIfUnrecalled(tx, snapshots); err == nil {
			t.Fatal("expected the delete failure to propagate")
		}

		_ = tx.Rollback()
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectBegin()
		mock.ExpectQuery(`DELETE FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nil))

		tx, _, err := d.beginTx(context.Background())
		if err != nil {
			t.Fatalf("beginTx: %v", err)
		}

		if _, err := d.deleteChunkIfUnrecalled(tx, snapshots); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		_ = tx.Rollback()
	})
}

// TestDeleteChunkMySQL_Failures covers the MySQL arm, which has to lock the surviving rows with
// SELECT ... FOR UPDATE and then delete them by id, MySQL having no DELETE ... RETURNING.
func TestDeleteChunkMySQL_Failures(t *testing.T) {
	snapshots := []memoryRecallSnapshot{{id: "m1"}}

	tests := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
	}{
		{
			name: "select for update",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`SELECT id FROM memories`).WillReturnError(errors.New("boom"))
			},
		},
		{
			name: "scan",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`SELECT id FROM memories`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nil))
			},
		},
		{
			name: "delete",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`SELECT id FROM memories`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
				mock.ExpectExec(`DELETE FROM memories WHERE id IN`).WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverMySQL)

			mock.ExpectBegin()
			test.expect(mock)

			tx, _, err := d.beginTx(context.Background())
			if err != nil {
				t.Fatalf("beginTx: %v", err)
			}

			if _, err := d.deleteChunkIfUnrecalled(tx, snapshots); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			_ = tx.Rollback()
		})
	}
}

// TestDeleteChunkMySQL_NothingMatchedSkipsTheDelete covers the branch where every row was recalled
// between the scan and the delete, so the guard matched nothing and there is nothing to remove.
func TestDeleteChunkMySQL_NothingMatchedSkipsTheDelete(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM memories`).WillReturnRows(sqlmock.NewRows([]string{"id"}))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	ids, err := d.deleteChunkIfUnrecalled(tx, []memoryRecallSnapshot{{id: "m1"}})
	if err != nil {
		t.Fatalf("deleteChunkIfUnrecalled: %v", err)
	}

	if len(ids) != 0 {
		t.Errorf("expected nothing deleted, got %+v", ids)
	}

	_ = tx.Rollback()

	expectationsMet(t, mock)
}

// TestDeleteEventMemories_Failures walks every step of the read-then-delete transaction, which
// reads the ids first precisely so the link graph can be pruned by memory id.
func TestDeleteEventMemories_Failures(t *testing.T) {
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
			name: "id read",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "id scan",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nil))
				mock.ExpectRollback()
			},
		},
		{
			name: "id row error",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).
						AddRow("m1").RowError(1, errors.New("boom")).AddRow("m2"))
				mock.ExpectRollback()
			},
		},
		{
			name: "delete",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
				mock.ExpectExec(`DELETE FROM memories WHERE event_id`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "prune",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
				mock.ExpectExec(`DELETE FROM memories WHERE event_id`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "rows affected",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
				mock.ExpectExec(`DELETE FROM memories WHERE event_id`).
					WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
				mock.ExpectRollback()
			},
		},
		{
			name: "commit",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
				mock.ExpectExec(`DELETE FROM memories WHERE event_id`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
				mock.ExpectCommit().WillReturnError(errors.New("boom"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)
			test.expect(mock)

			if _, err := d.DeleteEventMemories(context.Background(), "e1"); err == nil {
				t.Fatal("expected the failure to propagate")
			}

			expectationsMet(t, mock)
		})
	}
}

// TestUnsetMemoriesEventId_Failures covers the update and its row count.
func TestUnsetMemoriesEventId_Failures(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(`UPDATE memories SET event_id`).WillReturnError(errors.New("boom"))

		if _, err := d.UnsetMemoriesEventId(context.Background(), "e1"); err == nil {
			t.Fatal("expected the update failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("rows affected", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(`UPDATE memories SET event_id`).
			WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

		if _, err := d.UnsetMemoriesEventId(context.Background(), "e1"); err == nil {
			t.Fatal("expected the row-count failure to propagate")
		}

		expectationsMet(t, mock)
	})
}

// --- the paged and filtered readers. Each is query, scan, row error; they are separate functions
// and so each is its own chance to drop an error on the floor. ---

func TestMemoryReaders_RowFailuresPropagate(t *testing.T) {
	readers := []struct {
		name  string
		match string
		call  func(*DB) error
	}{
		{
			name:  "GetMemoriesByIds",
			match: `SELECT`,
			call: func(d *DB) error {
				_, err := d.GetMemoriesByIds(context.Background(), []string{"m1", "m2"})

				return err
			},
		},
		{
			name:  "GetIndexableMemoriesPage",
			match: `SELECT`,
			call: func(d *DB) error {
				_, err := d.GetIndexableMemoriesPage(context.Background(), "", 10)

				return err
			},
		},
		{
			name:  "GetMemoriesByEventIds",
			match: `SELECT`,
			call: func(d *DB) error {
				_, err := d.GetMemoriesByEventIds(context.Background(), []string{"e1"})

				return err
			},
		},
		{
			name:  "GetMemoriesByEventId",
			match: `SELECT`,
			call: func(d *DB) error {
				_, err := d.GetMemoriesByEventId(context.Background(), "e1")

				return err
			},
		},
		{
			name:  "CountMemoriesByEventIds",
			match: `SELECT`,
			call: func(d *DB) error {
				_, err := d.CountMemoriesByEventIds(context.Background(), []string{"e1"}, nil)

				return err
			},
		},
	}

	for _, reader := range readers {
		t.Run(reader.name+" scan", func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			mock.ExpectQuery(reader.match).WillReturnRows(memoryReadRows().
				AddRow("m1", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, []byte("{bad"), 0))

			if err := reader.call(d); err == nil {
				t.Fatal("expected the scan failure to propagate")
			}
		})

		t.Run(reader.name+" row error", func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			mock.ExpectQuery(reader.match).WillReturnRows(okMemoryRow(memoryReadRows(), "m1").
				RowError(1, errors.New("boom")).
				AddRow("m2", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, nil, 0))

			if err := reader.call(d); err == nil {
				t.Fatal("expected the row error to propagate")
			}
		})
	}
}

// TestGetMemories_RowFailuresPropagate covers the filtered listing, which runs its own query
// builder and so is not one of the readers above.
func TestGetMemories_RowFailuresPropagate(t *testing.T) {
	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT`).WillReturnRows(memoryReadRows().
			AddRow("m1", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, []byte("{bad"), 0))

		if _, err := d.GetMemories(context.Background(), MemoryFilter{}); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT`).WillReturnRows(okMemoryRow(memoryReadRows(), "m1").
			RowError(1, errors.New("boom")).
			AddRow("m2", 100, 5, "", []byte("body"), false, 0, 0, false, "", false, nil, 0))

		if _, err := d.GetMemories(context.Background(), MemoryFilter{}); err == nil {
			t.Fatal("expected the row error to propagate")
		}
	})
}

// TestGetMemories_IdsFilter covers the id restriction the linked_to listing resolves to, which has
// to compose with the ordinary filters rather than replace them.
func TestGetMemories_IdsFilter(t *testing.T) {
	d := newTestDB(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		mustCreateMemory(t, d, types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "body"})
	}

	memories, err := d.GetMemories(context.Background(), MemoryFilter{Ids: []string{"m1", "m3"}})
	if err != nil {
		t.Fatalf("GetMemories: %v", err)
	}

	if len(*memories) != 2 {
		t.Fatalf("expected the two named memories, got %d", len(*memories))
	}

	for _, m := range *memories {
		if m.Id != "m1" && m.Id != "m3" {
			t.Errorf("unexpected memory in an id-filtered listing: %s", m.Id)
		}
	}

	// The count has to see the same restriction, or pagination over a linked_to listing would
	// report a total for the whole store.
	total, err := d.CountMemoriesFiltered(context.Background(), MemoryFilter{Ids: []string{"m1", "m3"}})
	if err != nil {
		t.Fatalf("CountMemoriesFiltered: %v", err)
	}

	if total != 2 {
		t.Errorf("expected a filtered count of 2, got %d", total)
	}
}

// TestFindSummarisationCandidates_RowFailuresPropagate covers the candidate scan's decoder.
func TestFindSummarisationCandidates_RowFailuresPropagate(t *testing.T) {
	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT`).
			WillReturnRows(sqlmock.NewRows([]string{"event_id", "name", "cnt"}).AddRow(nil, "trip", 3))

		if _, err := d.FindSummarisationCandidates(context.Background(), 2, 0, 10); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT`).
			WillReturnRows(sqlmock.NewRows([]string{"event_id", "name", "cnt"}).
				AddRow("e1", "trip", 3).RowError(1, errors.New("boom")).AddRow("e2", "walk", 4))

		if _, err := d.FindSummarisationCandidates(context.Background(), 2, 0, 10); err == nil {
			t.Fatal("expected the row error to propagate")
		}
	})
}

// --- ReplaceMemoriesWithSummary, which deletes an event's memories and inserts one summary in
// their place, all in one transaction. ---

func TestReplaceMemoriesWithSummary_Failures(t *testing.T) {
	summary := types.Memory{Id: "s1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "gist"}

	tests := []struct {
		name   string
		expect func(sqlmock.Sqlmock)
	}{
		{
			name: "id read",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
		{
			name: "prune",
			expect: func(mock sqlmock.Sqlmock) {
				mock.ExpectBegin()
				mock.ExpectQuery(`SELECT id FROM memories WHERE event_id`).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
				mock.ExpectQuery(`SELECT 1 FROM memory_links`).WillReturnError(errors.New("boom"))
				mock.ExpectRollback()
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			// The summary's level is resolved before the transaction opens.
			mock.ExpectQuery(`significance_levels`).
				WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))

			test.expect(mock)

			if _, err := d.ReplaceMemoriesWithSummary(context.Background(), "e1", summary); err == nil {
				t.Fatal("expected the failure to propagate")
			}
		})
	}
}

// --- eviction, which is the one scan that both reads and deletes, and whose per-event bookkeeping
// carries the first error rather than the last. ---

func TestEvictMemories_ScanFailuresPropagate(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)
		emptyRanksQuery(mock)

		mock.ExpectQuery(`FROM memories`).WillReturnError(errors.New("boom"))

		if _, _, _, err := d.EvictMemories(context.Background(), &stubServer{}, 1000); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestEvictMemories_NothingToFreeIsANoOp covers the early return: eviction is driven by a byte
// shortfall, so a non-positive one means the store is already under its target.
func TestEvictMemories_NothingToFreeIsANoOp(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	evicted, events, freed, err := d.EvictMemories(context.Background(), &stubServer{}, 0)
	if err != nil || evicted != 0 || events != 0 || freed != 0 {
		t.Errorf("EvictMemories(0) = %d, %d, %d, %v; want 0, 0, 0, nil", evicted, events, freed, err)
	}

	expectationsMet(t, mock)
}
