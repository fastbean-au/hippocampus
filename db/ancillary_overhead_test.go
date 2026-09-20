package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fastbean-au/hippocampus/types"
)

// The drift guard on the flat per-row allowances the two fixed-width excluded tables are charged.
//
// TestRowOverheadMatchesRealStorage is the same idea for the memories the capacity target counts.
// This is its counterpart for the storage it deliberately does not: dialect.tombstoneRowBytes and
// dialect.outboxRowBytes are numbers nothing executes, and they are read in three places that each
// fail in a different direction when they are wrong -
//
//   - AncillaryStorage reports them to an operator and to HippocampusAncillaryStorageHigh, where
//     being low is being optimistic exactly where the figure exists to warn;
//   - rowsWithinBytes converts a byte cap into a row cap with them, where being low lets a table
//     grow past a cap expressed in the unit an operator sized their disk in; and
//   - UsedBytes subtracts them on SQLite, where being HIGH would make the store look smaller than
//     it is and forget less than it should.
//
// The first two want the allowance to cover everything a row really costs. The third is why it
// covers structure and not bloat: a dead tuple awaiting vacuum is real disk, but it is transient and
// dialect-specific, and charging for it would have the embedded store subtract space it is about to
// get back.
//
// It exists because one flat 192 served all three dialects while a live Postgres store held 100,002
// tombstones in 46 MB, and the doc comment had claimed to cover "the two index entries" since the day
// it was written. Two things were wrong with it. The table carries three btree indexes, not two - the
// surrogate primary key as well - which is most of the gap between 192 and the 253 a compacted
// Postgres row measures here. The rest is space the engine has not reclaimed, which is what
// AncillaryTable.DiskBytes reports and what this test deliberately does not measure: every reading
// below is taken after a compaction, because the allowance is a control input and a control input
// must not chase a figure that does not drop (TODO-2 item 126).

// ancillaryRows is the sample, and it matches overheadRows for one dialect's sake: InnoDB's
// allocation and statistics granularity make a smaller table's reading jump around badly - measured,
// two thousand rows put MySQL's tombstone at 254 bytes and five thousand at 400, against a stable
// 342 at ten thousand.
const ancillaryRows = overheadRows

// ancillaryBand is the tolerance, as a fraction of the real reading. A band rather than a value for
// the reason overheadBandLow/High is one: page fill and a server's allocation granularity move a
// few per cent between runs, and a test that failed on that would be turned off rather than read.
//
// Both ends are checked, and they guard opposite faults. Below the reading is a report that
// understates and a byte cap that admits more rows than it says - the fault that produced this test.
// Above it is an exclusion that subtracts more than the log occupies, which on the embedded dialect
// makes the store look smaller than it is and forget less than it should.
const (
	ancillaryBandLow  = 0.85
	ancillaryBandHigh = 1.25
)

// ancillaryRelation is one excluded table, as this measurement addresses it.
type ancillaryRelation struct {
	name string

	// measured reads the table's reported size out of an AncillaryStorage, so the comparison is
	// against the figure the console and the alert rule are actually shown rather than against the
	// constant behind it.
	measured func(AncillaryStorage) AncillaryTable
}

// ancillaryRelations are the two tables whose bytes ARE their row count times an allowance.
//
// The callback queue is deliberately absent. Its bytes are summed from payload_bytes written at
// insert, so the only allowance in them is callbackRowOverheadBytes - a small share of a row
// carrying a rendered delivery - and the payload dominates the comparison rather than the figure
// under test. Worse, a delivery batching thousands of ids is a multi-kilobyte value, which Postgres
// TOASTs and compresses beneath the length that was written: the reading would be smaller than the
// truth for a reason that is not drift, which is the failure mode overheadCases already documents
// at the other end of the same trap.
var ancillaryRelations = []ancillaryRelation{
	{name: tombstonesTable, measured: func(s AncillaryStorage) AncillaryTable { return s.ForgottenLog }},
	{name: searchOutboxTable, measured: func(s AncillaryStorage) AncillaryTable { return s.SearchOutbox }},
}

// fillAncillary writes ancillaryRows memories and then forgets the lot, which is what puts a row in
// each of the two tables.
//
// The memories arrive through ImportMemories rather than CreateMemory, and that is about the SHARED
// SERVERS rather than about this test. What is being measured is the shape of the rows the delete
// writes, and a batch upsert produces byte-for-byte the same memory rows as ten thousand individual
// creates - but in twenty round trips instead of ten thousand, against the same Postgres and MySQL
// instances every other integration test in the repo is using. At ten thousand round trips it
// starved the cmd/hippocampus Postgres test of its ten-second startup budget under a coverage run,
// which is a failure in a different package with nothing to say about what caused it.
//
// The deletion is the real path either way: ConsolidateMemories, through the chokepoint that writes
// the tombstone and queues the outbox row inside the delete's own transaction.
func fillAncillary(t *testing.T, database *DB) {
	t.Helper()

	database.SetTombstonePolicy(TombstonePolicy{Enabled: true})
	database.SetSearchOutbox(true)

	ctx := context.Background()

	for start := 0; start < ancillaryRows; start += ancillaryImportBatch {
		batch := make([]types.Memory, 0, ancillaryImportBatch)

		for range min(ancillaryImportBatch, ancillaryRows-start) {
			batch = append(batch, types.Memory{
				Id:           uuid.New().String(),
				TimeStamp:    time.Now().UnixNano(),
				Significance: 5,
				Body:         "a body of no particular interest",
				Group:        "ancillary",
			})
		}

		if _, err := database.ImportMemories(ctx, batch); err != nil {
			t.Fatalf("ImportMemories: %s", err)
		}
	}

	if _, err := database.ConsolidateMemories(ctx, forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}
}

// ancillaryImportBatch is how many memories go in one upsert. It is the page size Transfer already
// sends, so nothing here is asking the import path to do something a deployment does not.
const ancillaryImportBatch = 500

// checkAncillary compares what AncillaryStorage reports for one table against what the server says
// that table really occupies, and reports the per-row figures either way so a run that fails says
// what to set instead.
func checkAncillary(
	t *testing.T,
	relation ancillaryRelation,
	reported AncillaryTable,
	real int64,
	dialectMeasures bool,
) {
	t.Helper()

	if reported.Rows != ancillaryRows {
		t.Fatalf("%s holds %d rows, want %d", relation.name, reported.Rows, ancillaryRows)
	}

	// The relation reading and the structural estimate are two different questions, and a dialect
	// either answers the first or says so. A silent 0 on a dialect that declares a relationBytes
	// query would leave the footprint quietly falling back to the estimate - which is the one
	// failure mode of this pair that nothing else would notice.
	switch measured := reported.DiskBytes > 0; {

	case measured && !dialectMeasures:
		t.Errorf("%s reported %d disk bytes on a dialect that declares no relation measurement",
			relation.name, reported.DiskBytes)

	case !measured && dialectMeasures:
		t.Errorf("%s reported no disk bytes on a dialect that declares a relation measurement",
			relation.name)
	}

	ratio := float64(reported.Bytes) / float64(real)

	t.Logf(
		"%s: reported %d, real %d, ratio %.2f | per row: allowed %.0f, real %.0f",
		relation.name, reported.Bytes, real, ratio,
		float64(reported.Bytes)/float64(reported.Rows),
		float64(real)/float64(reported.Rows),
	)

	if ratio < ancillaryBandLow || ratio > ancillaryBandHigh {
		t.Errorf(
			"%s is reported at %.2f of its real storage, outside [%.2f, %.2f]: a row really costs "+
				"%.0f bytes against an allowed %.0f, so this table's flat allowance wants re-deriving",
			relation.name, ratio, ancillaryBandLow, ancillaryBandHigh,
			float64(real)/float64(reported.Rows), float64(reported.Bytes)/float64(reported.Rows),
		)
	}
}

// TestAncillaryAllowancesMatchRealStorage measures the two fixed-width excluded tables against what
// the server reports they occupy.
func TestAncillaryAllowancesMatchRealStorage(t *testing.T) {
	// Measured once per run, for the reason TestRowOverheadMatchesRealStorage gives: this test
	// drives its own opens, so the dialect sweep would repeat every dialect it already covers.
	if os.Getenv(dialectEnv) != "" {
		t.Skipf("measured once per run: %s repeats every dialect this test already covers", dialectEnv)
	}

	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()

		database := openForAncillary(t, func(opts ...Option) (*DB, error) {
			return New(t.TempDir(), opts...)
		})

		fillAncillary(t, database)

		if err := database.Preserve(context.Background()); err != nil {
			t.Fatalf("Preserve: %s", err)
		}

		measureAncillary(t, database, func(tables []string) int64 {
			return sqliteRelationBytes(t, database, tables)
		})
	})

	t.Run("postgres", func(t *testing.T) {
		t.Parallel()

		dsn := os.Getenv(postgresTestDSNEnv)
		if dsn == "" {
			t.Skipf("set %s to run the postgres ancillary measurement", postgresTestDSNEnv)
		}

		database := openForAncillary(t, func(opts ...Option) (*DB, error) {
			return NewPostgres(dsn, false, opts...)
		})

		fillAncillary(t, database)

		measureAncillary(t, database, func(tables []string) int64 {
			return postgresRelationBytes(t, database, tables)
		})
	})

	t.Run("mysql", func(t *testing.T) {
		t.Parallel()

		dsn := os.Getenv(mysqlTestDSNEnv)
		if dsn == "" {
			t.Skipf("set %s to run the mysql ancillary measurement", mysqlTestDSNEnv)
		}

		database := openForAncillary(t, func(opts ...Option) (*DB, error) {
			return NewMySQL(dsn, false, opts...)
		})

		fillAncillary(t, database)

		measureAncillary(t, database, func(tables []string) int64 {
			return mysqlRelationBytes(t, database, tables)
		})
	})
}

// measureAncillary reads one AncillaryStorage and checks each relation in it against the dialect's
// own sizing.
func measureAncillary(t *testing.T, database *DB, real func([]string) int64) {
	t.Helper()

	storage, err := database.AncillaryStorage(context.Background(), AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	for _, relation := range ancillaryRelations {
		checkAncillary(
			t,
			relation,
			relation.measured(storage),
			real([]string{relation.name}),
			database.dialect().relationBytes != "",
		)
	}
}

// openForAncillary opens a store and empties it.
//
// It keeps the store's content index, although nothing here measures it and indexing ten thousand
// bodies is the most expensive part of the fill. Turning it off is what a first draft did, and on
// the SHARED server instances it is actively unsafe: it leaves `memories` populated while
// `memories_fts` is empty, which is exactly the state initContentSearch backfills from - so any
// other process opening that database concurrently starts copying rows this test is in the middle of
// deleting, its inserts fail the index's foreign key, migration 14 fails, and a service somewhere
// else refuses to start. That was observed as a ten-second startup timeout in cmd/hippocampus with
// nothing in its own output to say why.
func openForAncillary(t *testing.T, open func(...Option) (*DB, error)) *DB {
	t.Helper()

	database, err := open()
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})

	// Purge empties the excluded tables as well, which is what makes each dialect measure its own
	// rows rather than a previous run's.
	if err := database.Purge(context.Background()); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	t.Cleanup(func() {
		if err := database.Purge(context.Background()); err != nil {
			t.Errorf("Purge on cleanup: %s", err)
		}
	})

	return database
}

// mysqlRelationBytes reports what the named tables and their indexes occupy, from
// information_schema.TABLES rather than from the tablespace files mysqlTablespaceBytes reads.
//
// Two things about this reading, and they are why the MySQL dialect declares no relationBytes of its
// own. The tablespace file is the wrong measure for a narrow table - InnoDB extends one four
// megabytes at a time, so ten thousand of these rows measured nine megabytes where the pages in use
// were two - and information_schema.TABLES is served from a cache that
// information_schema_stats_expiry refreshes at most once a day, so read as it stands it answers with
// whatever the table was the last time anything looked. It reported 16 KiB for these ten thousand
// rows in a suite where an earlier test had seen the table empty. ANALYZE TABLE is what refreshes
// that cache, and running it here is affordable in a way that running it once per sleep cycle - a
// write, on the consolidating path - is not.
func mysqlRelationBytes(t *testing.T, database *DB, tables []string) int64 {
	t.Helper()

	var total int64

	for _, table := range tables {
		if _, err := database.sql.Exec(`OPTIMIZE TABLE ` + table); err != nil {
			t.Fatalf("OPTIMIZE TABLE %s: %s", table, err)
		}

		if _, err := database.sql.Exec(`ANALYZE TABLE ` + table); err != nil {
			t.Fatalf("ANALYZE TABLE %s: %s", table, err)
		}

		var bytes int64

		if err := database.sql.QueryRow(
			`SELECT COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`,
			table,
		).Scan(&bytes); err != nil {
			t.Fatalf("information_schema.TABLES(%s): %s", table, err)
		}

		total += bytes
	}

	return total
}

// sqliteRelationBytes reports what the named tables and their indexes really occupy, from dbstat -
// which modernc.org/sqlite carries, and which is the only per-relation accounting SQLite has. The
// whole-file page count UsedBytes uses cannot answer this: every table in the store shares one file.
func sqliteRelationBytes(t *testing.T, database *DB, tables []string) int64 {
	t.Helper()

	var total int64

	for _, table := range tables {
		var bytes int64

		// An index is a relation of its own in dbstat, named for the table it is on - so the table
		// and everything built over it are matched together, which is what pg_total_relation_size
		// and the InnoDB tablespace reading both give for free.
		if err := database.sql.QueryRow(
			`SELECT COALESCE(SUM(pgsize), 0) FROM dbstat
			WHERE name = ? OR name LIKE ? OR name LIKE ?`,
			table, "idx_"+table+"%", "sqlite_autoindex_"+table+"%",
		).Scan(&bytes); err != nil {
			t.Fatalf("dbstat(%s): %s", table, err)
		}

		if bytes == 0 {
			t.Fatalf("dbstat reported nothing for %s", table)
		}

		total += bytes
	}

	return total
}
