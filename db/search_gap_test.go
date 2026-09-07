package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// --- the server dialects' DDL and index statements, over a mock rather than a server. The shapes
// these pin are the ones a live server would reject loudly and a test with no server would never
// reach at all: the cascade that IS the delete story, and the tsvector/FULLTEXT halves of the write.
// The behavioural cover is in search_test.go, which runs under HIPPOCAMPUS_TEST_DIALECT. ---

// TestCreateContentIndex_ServerDialectDDL pins the two server dialects' index creation, and in
// particular the foreign key: it is what makes every deletion path retire its index entry without a
// single call site knowing the index exists, so a CREATE TABLE that lost it would leave the index
// growing forever and answering with memories the store no longer holds.
func TestCreateContentIndex_ServerDialectDDL(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories_fts[\s\S]*REFERENCES memories\(id\) ON DELETE CASCADE`).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`CREATE INDEX IF NOT EXISTS memories_fts_body ON memories_fts USING GIN \(body_search\)`).
			WillReturnResult(sqlmock.NewResult(0, 0))

		if err := d.createContentIndex(); err != nil {
			t.Fatalf("createContentIndex: %v", err)
		}

		expectationsMet(t, mock)
	})

	t.Run("mysql", func(t *testing.T) {
		d, mock := newMockDB(t, driverMySQL)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories_fts[\s\S]*REFERENCES memories\(id\) ON DELETE CASCADE`).
			WillReturnResult(sqlmock.NewResult(0, 0))
		// No CREATE FULLTEXT INDEX IF NOT EXISTS on this dialect, so it probes first - and probes
		// on every startup, which is what lets an index somebody dropped come back.
		mock.ExpectQuery(`FROM information_schema.statistics`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectExec(`ALTER TABLE memories_fts ADD FULLTEXT INDEX memories_fts_body \(body\)`).
			WillReturnResult(sqlmock.NewResult(0, 0))

		if err := d.createContentIndex(); err != nil {
			t.Fatalf("createContentIndex: %v", err)
		}

		expectationsMet(t, mock)
	})

	t.Run("mysql leaves an existing index alone", func(t *testing.T) {
		d, mock := newMockDB(t, driverMySQL)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories_fts`).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`FROM information_schema.statistics`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

		if err := d.createContentIndex(); err != nil {
			t.Fatalf("createContentIndex: %v", err)
		}

		expectationsMet(t, mock)
	})
}

// TestWriteContentIndexEntry_DialectStatements pins that each dialect writes through its own
// index's shape - and that the two server dialects UPSERT, which is what lets one call site serve a
// create, an update and an import upsert alike.
func TestWriteContentIndexEntry_DialectStatements(t *testing.T) {
	tests := []struct {
		name      string
		driver    driver
		statement string
	}{
		{
			name:      "sqlite resolves the rowid in the insert",
			driver:    driverSQLite,
			statement: `INSERT INTO memories_fts \(rowid, body\) SELECT rowid, \? FROM memories WHERE id = \?`,
		},
		{
			name:      "postgres stores a tsvector",
			driver:    driverPostgres,
			statement: `to_tsvector\('simple', \$2\)\)\s*ON CONFLICT \(memory_id\) DO UPDATE SET body_search = excluded.body_search`,
		},
		{
			name:      "mysql stores the text",
			driver:    driverMySQL,
			statement: `ON DUPLICATE KEY UPDATE body = new.body`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, test.driver)

			mock.ExpectExec(test.statement).WillReturnResult(sqlmock.NewResult(0, 1))

			if err := d.writeContentIndexEntry(context.Background(), "m1", "a body"); err != nil {
				t.Fatalf("writeContentIndexEntry: %v", err)
			}

			expectationsMet(t, mock)
		})
	}
}

// TestDeleteContentIndexEntry_DialectStatements is the reindex path's half: the embedded dialect
// addresses a row by the memories rowid, the server dialects by the memory id.
func TestDeleteContentIndexEntry_DialectStatements(t *testing.T) {
	tests := []struct {
		name      string
		driver    driver
		statement string
	}{
		{
			name:      "sqlite deletes by rowid",
			driver:    driverSQLite,
			statement: `DELETE FROM memories_fts WHERE rowid = \(SELECT rowid FROM memories WHERE id = \?\)`,
		},
		{
			name:      "postgres deletes by memory id",
			driver:    driverPostgres,
			statement: `DELETE FROM memories_fts WHERE memory_id = \$1`,
		},
		{
			name:      "mysql deletes by memory id",
			driver:    driverMySQL,
			statement: `DELETE FROM memories_fts WHERE memory_id = \?`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, test.driver)

			mock.ExpectExec(test.statement).WillReturnResult(sqlmock.NewResult(0, 1))

			if err := d.deleteContentIndexEntry(context.Background(), "m1"); err != nil {
				t.Fatalf("deleteContentIndexEntry: %v", err)
			}

			expectationsMet(t, mock)
		})
	}
}

// TestContentSearchTerms_ScoreDirection is the rule the whole search surface rests on: whichever
// engine answered, a higher score is a better match. Two of the three say so natively; the third's
// bm25 runs backwards and has its sign flipped here rather than anywhere above.
func TestContentSearchTerms_ScoreDirection(t *testing.T) {
	tests := []struct {
		driver driver
		score  string
		args   int
	}{
		{driver: driverSQLite, score: `-memories_fts.rank`, args: 0},
		{driver: driverPostgres, score: `ts_rank(f.body_search, to_tsquery('simple', ?))`, args: 1},
		{driver: driverMySQL, score: `MATCH(f.body) AGAINST (? IN BOOLEAN MODE)`, args: 1},
	}

	for _, test := range tests {
		d := &DB{driver: test.driver}

		t.Run(d.dialect().name, func(t *testing.T) {
			terms := d.contentSearchTerms("x")

			if terms.score != test.score {
				t.Errorf("score = %q, want %q", terms.score, test.score)
			}

			if len(terms.scoreArgs) != test.args {
				t.Errorf("score consumes %d arguments, want %d", len(terms.scoreArgs), test.args)
			}

			if terms.predicate == "" || len(terms.predicateArgs) != 1 {
				t.Errorf("predicate = %q with %d arguments, want one of each",
					terms.predicate, len(terms.predicateArgs))
			}
		})
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
		mock.ExpectQuery(`SELECT count\(\*\) FROM memories WHERE NOT is_binary`).
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
	mock.ExpectQuery(`SELECT count\(\*\) FROM memories WHERE NOT is_binary`).
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

		mock.ExpectQuery(`JOIN memories_fts`).WillReturnError(errors.New("boom"))

		if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "hello"}); err == nil {
			t.Fatal("expected the query failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`JOIN memories_fts`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "score"}).AddRow(nil, -1.5))

		if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "hello"}); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`JOIN memories_fts`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "score"}).
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
