package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// --- the non-SQLite early returns. Content search is an FTS5 feature, so every entry point has to
// stand down on the server drivers rather than issue SQL no other dialect understands. A bare DB
// with no handle at all is the assertion: any statement would panic. ---

func TestContentSearch_NonSQLiteEntryPointsAreNoOps(t *testing.T) {
	for _, drv := range []driver{driverPostgres, driverMySQL} {
		d := &DB{driver: drv}

		if err := d.initContentSearch(); err != nil {
			t.Errorf("initContentSearch on %v = %v; want nil", drv, err)
		}

		if err := d.RebuildContentSearch(context.Background()); err != nil {
			t.Errorf("RebuildContentSearch on %v = %v; want nil", drv, err)
		}

		if d.ContentSearchAvailable() {
			t.Errorf("expected ContentSearchAvailable to be false on %v", drv)
		}

		// The write hooks share the same guard, and are called on every write on every driver.
		if err := d.indexMemoryContent(context.Background(), "m1", "body", false); err != nil {
			t.Errorf("indexMemoryContent on %v = %v; want nil", drv, err)
		}

		if err := d.reindexMemoryContent(context.Background(), "m1", "body", false); err != nil {
			t.Errorf("reindexMemoryContent on %v = %v; want nil", drv, err)
		}

		if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "x"}); !errors.Is(err, ErrContentSearchUnavailable) {
			t.Errorf("SearchMemoryHits on %v = %v; want ErrContentSearchUnavailable", drv, err)
		}
	}
}

// TestContentSearchAvailable_FalseWhenReadOnly pins the other half of the guard: a read-only open
// (the backfill tool running beside a live service) must never write to the index the service owns.
func TestContentSearchAvailable_FalseWhenReadOnly(t *testing.T) {
	d := &DB{driver: driverSQLite, readOnly: true}

	if d.ContentSearchAvailable() {
		t.Error("expected content search to be unavailable on a read-only open")
	}
}

// --- startup failures, which a real handle cannot be made to produce on demand. ---

// TestInitContentSearch_DDLErrorPropagates covers the virtual table failing to create.
func TestInitContentSearch_DDLErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts`).WillReturnError(errors.New("boom"))

	if err := d.initContentSearch(); err == nil {
		t.Fatal("expected the DDL failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestBackfillContentSearch_CountErrorsPropagate covers both counts the upgrade probe runs: the
// index's, and - only when the index is empty - the table's.
func TestBackfillContentSearch_CountErrorsPropagate(t *testing.T) {
	t.Run("index count", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT count\(\*\) FROM memories_fts`).WillReturnError(errors.New("boom"))

		if err := d.backfillContentSearch(); err == nil {
			t.Fatal("expected the index count's failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("table count", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT count\(\*\) FROM memories_fts`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectQuery(`SELECT count\(\*\) FROM memories WHERE is_binary = 0`).
			WillReturnError(errors.New("boom"))

		if err := d.backfillContentSearch(); err == nil {
			t.Fatal("expected the table count's failure to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestBackfillContentSearch_PopulatedIndexIsLeftAlone pins the deliberately narrow guard: a store
// whose index already has rows pays one COUNT and moves on, and never tries to repair a partially
// populated index. The mock fails if a second statement is issued.
func TestBackfillContentSearch_PopulatedIndexIsLeftAlone(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT count\(\*\) FROM memories_fts`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))

	if err := d.backfillContentSearch(); err != nil {
		t.Fatalf("backfillContentSearch: %v", err)
	}

	expectationsMet(t, mock)
}

// TestBackfillContentSearch_RebuildErrorPropagates covers the populate step failing on a store that
// does need one.
func TestBackfillContentSearch_RebuildErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT count\(\*\) FROM memories_fts`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT count\(\*\) FROM memories WHERE is_binary = 0`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectExec(`DELETE FROM memories_fts`).WillReturnError(errors.New("boom"))

	if err := d.backfillContentSearch(); err == nil {
		t.Fatal("expected the rebuild's failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestRebuildContentSearch_ClearErrorPropagates covers the truncation that opens a rebuild.
func TestRebuildContentSearch_ClearErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`DELETE FROM memories_fts`).WillReturnError(errors.New("boom"))

	if err := d.RebuildContentSearch(context.Background()); err == nil {
		t.Fatal("expected the clear's failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestRebuildContentSearch_PageErrorPropagates covers the paged read failing partway.
func TestRebuildContentSearch_PageErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`DELETE FROM memories_fts`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("boom"))

	if err := d.RebuildContentSearch(context.Background()); err == nil {
		t.Fatal("expected the page read's failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestIndexMemoryContent_ErrorPropagates covers the insert failing. It is logged and returned: the
// caller (a write helper) decides whether a failed index entry should fail the write.
func TestIndexMemoryContent_ErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`INSERT INTO memories_fts`).WillReturnError(errors.New("boom"))

	if err := d.indexMemoryContent(context.Background(), "m1", "body", false); err == nil {
		t.Fatal("expected the insert failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestIndexMemoryContent_BinaryIsSkipped covers the other half of the guard: a binary body is
// client-encoded and opaque, so indexing it would match on the encoding rather than the content.
func TestIndexMemoryContent_BinaryIsSkipped(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	if err := d.indexMemoryContent(context.Background(), "m1", "body", true); err != nil {
		t.Fatalf("indexMemoryContent: %v", err)
	}

	expectationsMet(t, mock)
}

// TestReindexMemoryContent_DeleteErrorPropagates covers the delete half of the reindex, which runs
// even for a binary memory so a replaced body cannot keep matching on its old text.
func TestReindexMemoryContent_DeleteErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`DELETE FROM memories_fts`).WillReturnError(errors.New("boom"))

	if err := d.reindexMemoryContent(context.Background(), "m1", "body", false); err == nil {
		t.Fatal("expected the delete failure to propagate")
	}

	expectationsMet(t, mock)
}

// --- SearchMemoryHits' read failures. ---

func TestSearchMemoryHits_Failures(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`FROM memories_fts`).WillReturnError(errors.New("boom"))

		if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "hello"}); err == nil {
			t.Fatal("expected the query failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`FROM memories_fts`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "rank"}).AddRow(nil, -1.5))

		if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "hello"}); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`FROM memories_fts`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "rank"}).
				AddRow("m1", -1.5).RowError(1, errors.New("boom")).AddRow("m2", -1.0))

		if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "hello"}); err == nil {
			t.Fatal("expected the row error to propagate")
		}

		expectationsMet(t, mock)
	})
}

// --- metadataConditions' dialect branches. They are pure string building, so they need no handle -
// and they are exactly the kind of thing that drifts silently, since only a live server of that
// dialect would otherwise notice. ---

func TestMetadataConditions_DialectSpecificPredicates(t *testing.T) {
	metadata := map[string]string{"source": "slack"}

	tests := []struct {
		name     string
		driver   driver
		clause   string
		firstArg any
	}{
		{
			name:     "sqlite addresses the member by json path",
			driver:   driverSQLite,
			clause:   `json_extract(m.metadata, ?) = ?`,
			firstArg: `$."source"`,
		},
		{
			name:     "postgres takes the key itself",
			driver:   driverPostgres,
			clause:   `m.metadata ->> ? = ?`,
			firstArg: "source",
		},
		{
			// The COLLATE is the whole point: without it MySQL matches case-insensitively and the
			// same store answers the same filter differently depending on its driver.
			name:     "mysql forces a binary collation",
			driver:   driverMySQL,
			clause:   `JSON_UNQUOTE(JSON_EXTRACT(m.metadata, ?)) COLLATE utf8mb4_bin = ?`,
			firstArg: `$."source"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := &DB{driver: test.driver}

			clauses, args := d.metadataConditions("m.", metadata)

			if len(clauses) != 1 || clauses[0] != test.clause {
				t.Errorf("clause = %q; want %q", clauses, test.clause)
			}

			if len(args) != 2 || args[0] != test.firstArg || args[1] != "slack" {
				t.Errorf("args = %v; want [%v slack]", args, test.firstArg)
			}
		})
	}
}

// TestMetadataConditions_EmptyIsNoPredicate covers the early return, which is what makes an
// unfiltered read pay nothing for the feature.
func TestMetadataConditions_EmptyIsNoPredicate(t *testing.T) {
	d := &DB{driver: driverSQLite}

	clauses, args := d.metadataConditions("m.", nil)
	if clauses != nil || args != nil {
		t.Errorf("metadataConditions(nil) = %v, %v; want nil, nil", clauses, args)
	}
}

// TestMetadataConditions_KeysAreOrdered pins the sort: the clauses and their arguments are built in
// one pass over the same ordering, so an unordered map iteration would pair a key with another
// key's value. It also makes the generated SQL stable, which is what lets a driver cache it.
func TestMetadataConditions_KeysAreOrdered(t *testing.T) {
	d := &DB{driver: driverSQLite}

	metadata := map[string]string{"zebra": "z", "apple": "a", "mango": "m"}

	for range 20 {
		_, args := d.metadataConditions("m.", metadata)

		if len(args) != 6 {
			t.Fatalf("expected 6 args, got %d", len(args))
		}

		if args[0] != `$."apple"` || args[1] != "a" {
			t.Fatalf("expected apple first with its own value, got %v / %v", args[0], args[1])
		}

		if args[2] != `$."mango"` || args[3] != "m" {
			t.Fatalf("expected mango second with its own value, got %v / %v", args[2], args[3])
		}

		if args[4] != `$."zebra"` || args[5] != "z" {
			t.Fatalf("expected zebra last with its own value, got %v / %v", args[4], args[5])
		}
	}
}
