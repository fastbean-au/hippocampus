package db

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fastbean-au/hippocampus/types"
)

// mysqlAdminDSNEnv names a MySQL DSN with the PROCESS privilege. The sizing query below reads
// information_schema.innodb_tablespaces, which is the only place a FULLTEXT index's auxiliary
// tables are visible at all, and which the scoped test user may not read.
const mysqlAdminDSNEnv = "HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN"

// overheadRows is the sample the measurements below take. Large enough that a server's allocation
// granularity - InnoDB extends a tablespace four megabytes at a time - is a small share of the
// reading rather than most of it, and small enough that four cases per dialect stay inside a test
// run somebody will wait for.
const overheadRows = 10000

// overheadVocabulary is a realistic log-message word list. The words repeat, which matters: a body
// of random tokens gives every lexeme in it a distinct entry in the content index, and measuring
// against that would size the allowance for text nobody stores.
var overheadVocabulary = []string{
	"request", "completed", "handler", "upstream", "timeout", "retry", "connection",
	"pool", "exhausted", "cache", "miss", "hit", "user", "session", "expired", "token",
	"refresh", "queue", "depth", "worker", "shutdown", "graceful", "listener", "bound",
	"config", "reload", "applied", "latency", "budget", "exceeded", "backoff",
}

// overheadBody builds a body of a little over n bytes shaped like structured log JSON: a timestamp,
// real trace ids and a message.
func overheadBody(rng *rand.Rand, n int) string {
	message := make([]byte, 0, n)

	for len(message) < n {
		message = append(message, overheadVocabulary[rng.Intn(len(overheadVocabulary))]...)
		message = append(message, ' ')
	}

	return fmt.Sprintf(
		`{"ts":"%s","level":"info","trace_id":"%016x%016x","msg":"%s"}`,
		time.Now().Format(time.RFC3339Nano), rng.Uint64(), rng.Uint64(), string(message[:n]),
	)
}

// overheadWriters is how many goroutines share the fill. Every one of these writes is a round trip
// to a server, so a serial fill measures the network rather than the storage: it made this the
// longest test in the repo by an order of magnitude and put the whole db package past the default
// ten-minute timeout on CI. Writing concurrently is how the service itself fills a store, and what
// the rows occupy when it is done does not depend on the order they arrived in.
const overheadWriters = 8

// fillForOverhead writes overheadRows memories through CreateMemory - so compression, the content
// index and the significance registry all run exactly as they do in the service - and returns the
// total plain body size, which is what the stored payload is compared against to report a ratio.
func fillForOverhead(t *testing.T, database *DB, bodyBytes int) int64 {
	t.Helper()

	var (
		writers  sync.WaitGroup
		payloads = make([]int64, overheadWriters)
		failures = make([]error, overheadWriters)
	)

	for w := 0; w < overheadWriters; w++ {
		writers.Add(1)

		// Each writer takes every overheadWriters-th row and seeds its own generator, so the total
		// written stays the same on every run whatever order the writers interleave in.
		go func(w int) {
			defer writers.Done()

			rng := rand.New(rand.NewSource(int64(w) + 1))
			ctx := context.Background()

			for i := w; i < overheadRows; i += overheadWriters {
				body := overheadBody(rng, bodyBytes)
				payloads[w] += int64(len(body))

				if _, err := database.CreateMemory(ctx, types.Memory{
					Id:           uuid.New().String(),
					TimeStamp:    time.Now().UnixNano(),
					Significance: int32(1 + rng.Intn(10)),
					Body:         body,
					Group:        "overhead",
				}); err != nil {
					failures[w] = fmt.Errorf("CreateMemory %d: %w", i, err)

					return
				}
			}
		}(w)
	}

	writers.Wait()

	var payload int64

	for i, v := range payloads {
		if failures[i] != nil {
			t.Fatalf("filling the store: %s", failures[i])
		}

		payload += v
	}

	return payload
}

// overheadBand is the tolerance the estimate is held to, as a fraction of the real reading. It is a
// band rather than a value on purpose: page fill, a server's allocation granularity and the
// statistics these figures are derived from all move a few per cent between runs, and a test that
// failed on that would be turned off rather than read.
//
// What it is really guarding is the class of drift that produced it - a 256-byte constant that
// stayed put from the initial commit while the columns it allowed for grew, a second index was
// added, the covering index was widened twice and the content index arrived, leaving the capacity
// target regulating a figure three to five times smaller than the disk.
const (
	overheadBandLow  = 0.80
	overheadBandHigh = 1.20
)

// storeEstimate reads the two payload figures out of the store and returns what the store estimates
// they occupy, alongside the stored payload itself.
//
// Read back rather than accumulated during the fill, deliberately: with compression on, what a
// caller wrote and what the row holds are different sizes, and it is the second that the capacity
// target counts. These are the same two terms usedBytesLiveRows sums.
func storeEstimate(t *testing.T, database *DB) (int64, int64) {
	t.Helper()

	var payload, indexed int64

	query := `SELECT COALESCE(SUM(length(body) + ` + database.metadataBytesExpr("") + `), 0),
		COALESCE(SUM(COALESCE(indexed_bytes, length(body))), 0) FROM memories`

	if err := database.sql.QueryRow(query).Scan(&payload, &indexed); err != nil {
		t.Fatalf("reading the stored payload: %s", err)
	}

	return database.memoriesFootprint(overheadRows, payload, indexed), payload
}

// checkOverhead compares what the store estimates against what the server says it is really using,
// and reports the per-row figures either way so a run that fails says what to set instead.
func checkOverhead(t *testing.T, database *DB, plain int64, real int64) {
	t.Helper()

	estimate, payload := storeEstimate(t, database)
	ratio := float64(estimate) / float64(real)

	t.Logf(
		"plain %d, stored %d (%.2f), estimate %d, real %d, ratio %.2f | per row: stored %.0f, estimated overhead %.0f, real overhead %.0f",
		plain, payload, float64(payload)/float64(plain), estimate, real, ratio,
		float64(payload)/overheadRows,
		float64(estimate-payload)/overheadRows,
		float64(real-payload)/overheadRows,
	)

	if ratio < overheadBandLow || ratio > overheadBandHigh {
		t.Errorf(
			"the estimate is %.2f of real storage, outside [%.2f, %.2f]: real overhead is %.0f bytes "+
				"per row against an estimated %.0f, so this dialect's memoryRowOverheadBytes and "+
				"contentIndex* figures want re-deriving",
			ratio, overheadBandLow, overheadBandHigh,
			float64(real-payload)/overheadRows, float64(estimate-payload)/overheadRows,
		)
	}
}

// countedTables are the relations UsedBytes claims to account for. The ancillary three - the
// forgotten log, the search outbox and the callback queue - are deliberately outside it and so are
// outside this comparison too; the registry, the migration ledger and the significance levels are
// fixed overheads that do not scale with the store.
func countedTables(database *DB) []string {
	tables := []string{"memories", "events", "memory_links", "event_links"}

	if database.contentIndexed() {
		tables = append(tables, contentSearchTable)
	}

	return tables
}

// overheadCase is one measurement: a body size, whether the store keeps its own content index, and
// whether bodies are compressed.
//
// The compressed cases are the ones that matter most and were the last to be added. Measured, a
// content index is the same size to within a byte whether the bodies beside it were compressed or
// not - it is fed the plain body, from inside the storage boundary and before compressBody sees it -
// so an estimate that scaled the index off the stored length under-counted the shipped default
// configuration by a quarter while every uncompressed case passed. There is no compressed case at 64
// bytes because storage.compression.minBytes (512) leaves such a body uncompressed anyway, and none
// with the index off because compression and the index only interact through the index.
type overheadCase struct {
	bodyBytes    int
	contentIndex bool
	compress     bool
}

// overheadCases stops at a kilobyte for the uncompressed cases on purpose. Postgres TOASTs a value
// past about two kilobytes and compresses it beneath octet_length, so an uncompressed four-kilobyte
// body occupies far less disk than the payload sum says - which is the deliberate stored-logical
// convention documented in docs/operations.md, not an error in these allowances, and not something a
// per-row figure could express. Compressed, the stored value is small enough that TOAST never
// engages, so the large case is measured there.
var overheadCases = []overheadCase{
	{bodyBytes: 64, contentIndex: true},
	{bodyBytes: 64, contentIndex: false},
	{bodyBytes: 1024, contentIndex: true},
	{bodyBytes: 1024, contentIndex: false},
	{bodyBytes: 1024, contentIndex: true, compress: true},
	{bodyBytes: 4096, contentIndex: true, compress: true},
}

// name renders a case as a subtest name.
func (c overheadCase) name() string {
	return fmt.Sprintf("body=%d/contentIndex=%t/compressed=%t", c.bodyBytes, c.contentIndex, c.compress)
}

// TestRowOverheadMatchesRealStorage is the drift guard on the per-row allowances: it writes a known
// number of memories of a known size and fails if the store's own estimate of what they occupy
// falls outside a band of what the server reports it is really holding.
//
// It exists because those allowances are numbers that nothing executes. The one they replaced had
// been 256 bytes since the initial commit, its doc comment had always claimed to cover "the
// remaining columns and the index entries", and it had never once been measured against either.
//
// Both index modes are exercised, because the content index is the largest single term and a
// deployment answering its searches from OpenSearch does not carry it.
func TestRowOverheadMatchesRealStorage(t *testing.T) {
	// The dialect sweep would run every one of these measurements a second and a third time and
	// learn nothing: this test drives its own opens rather than newTestDB, so dialectEnv selects
	// nothing here and each pass measures all three dialects again. Measure once, in the pass that
	// collects coverage.
	if os.Getenv(dialectEnv) != "" {
		t.Skipf("measured once per run: %s repeats every dialect this test already covers", dialectEnv)
	}

	// The three dialects are three separate stores on three separate engines, so they are measured
	// concurrently - the cases within one dialect share a database and stay sequential. Serially
	// this test took longer than every other test in the package put together.

	// The embedded dialect measures its own pages, so UsedBytes there is exact - it matched the
	// database file byte for byte in every run of the measurement this test came from. That makes it
	// the reading to hold the estimate against: these allowances still drive eviction's view of what
	// a deletion frees and the preview's byte totals, on every dialect.
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()

		for _, test := range overheadCases {
			t.Run(test.name(), func(t *testing.T) {
				directory := t.TempDir()

				database := openForOverhead(t, func(opts ...Option) (*DB, error) {
					return New(directory, opts...)
				}, test)

				plain := fillForOverhead(t, database, test.bodyBytes)

				if err := database.Preserve(context.Background()); err != nil {
					t.Fatalf("Preserve: %s", err)
				}

				real, err := database.UsedBytes(context.Background())
				if err != nil {
					t.Fatalf("UsedBytes: %s", err)
				}

				checkOverhead(t, database, plain, real)
			})
		}
	})

	t.Run("postgres", func(t *testing.T) {
		t.Parallel()

		dsn := os.Getenv(postgresTestDSNEnv)
		if dsn == "" {
			t.Skipf("set %s to run the postgres storage measurement", postgresTestDSNEnv)
		}

		for _, test := range overheadCases {
			t.Run(test.name(), func(t *testing.T) {
				database := openForOverhead(t, func(opts ...Option) (*DB, error) {
					// A replica open: the measurement consolidates nothing, and taking the
					// single-consolidator lock would starve whichever other package is trying to
					// start a service against the same database.
					return NewPostgres(dsn, false, opts...)
				}, test)

				plain := fillForOverhead(t, database, test.bodyBytes)

				checkOverhead(t, database, plain, postgresRelationBytes(t, database))
			})
		}
	})

	t.Run("mysql", func(t *testing.T) {
		t.Parallel()

		dsn := os.Getenv(mysqlTestDSNEnv)
		if dsn == "" {
			t.Skipf("set %s to run the mysql storage measurement", mysqlTestDSNEnv)
		}

		if os.Getenv(mysqlAdminDSNEnv) == "" {
			t.Skipf("set %s as well: the tablespace sizes need the PROCESS privilege", mysqlAdminDSNEnv)
		}

		for _, test := range overheadCases {
			t.Run(test.name(), func(t *testing.T) {
				database := openForOverhead(t, func(opts ...Option) (*DB, error) {
					return NewMySQL(dsn, false, opts...)
				}, test)

				plain := fillForOverhead(t, database, test.bodyBytes)

				checkOverhead(t, database, plain, mysqlTablespaceBytes(t, database))
			})
		}
	})
}

// openForOverhead opens a store in the case's index mode, sets its compression policy and empties
// it, so each case measures its own rows rather than the previous one's.
func openForOverhead(t *testing.T, open func(...Option) (*DB, error), test overheadCase) *DB {
	t.Helper()

	var opts []Option

	if !test.contentIndex {
		opts = append(opts, WithoutContentIndex())
	}

	database, err := open(opts...)
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})

	// The db package leaves compression off; the service turns it on. Set it explicitly either way,
	// so a case says which configuration it measures rather than inheriting a default.
	database.SetCompression(test.compress, 512)

	if err := database.Purge(context.Background()); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	// And empty it again on the way out. The purge above is what makes a case measure its own rows,
	// but it happens after the open - so a case leaving rows behind for a case that indexes them
	// makes the next open backfill a content index it is about to discard, which on MySQL cost
	// nearly as much as the fill itself.
	t.Cleanup(func() {
		if err := database.Purge(context.Background()); err != nil {
			t.Errorf("Purge on cleanup: %s", err)
		}
	})

	return database
}

// postgresRelationBytes reports what the counted relations really occupy, indexes and TOAST
// included. VACUUM FULL first, or the dead tuples an earlier case left behind are counted as this
// one's overhead - which inflated the first reading anybody took here by 44%.
func postgresRelationBytes(t *testing.T, database *DB) int64 {
	t.Helper()

	if _, err := database.sql.Exec(`VACUUM (FULL, ANALYZE)`); err != nil {
		t.Fatalf("VACUUM: %s", err)
	}

	var total int64

	for _, table := range countedTables(database) {
		var bytes int64

		if err := database.sql.QueryRow(`SELECT pg_total_relation_size($1)`, table).Scan(&bytes); err != nil {
			t.Fatalf("pg_total_relation_size(%s): %s", table, err)
		}

		total += bytes
	}

	return total
}

// mysqlTablespaceBytes reports the same for MySQL, from the tablespace files rather than from
// information_schema.tables: a FULLTEXT index's auxiliary tables are tablespaces of their own and
// appear in no other accounting, and they are a large part of what that index costs.
func mysqlTablespaceBytes(t *testing.T, database *DB) int64 {
	t.Helper()

	for _, table := range countedTables(database) {
		if _, err := database.sql.Exec(`OPTIMIZE TABLE ` + table); err != nil {
			t.Fatalf("OPTIMIZE TABLE %s: %s", table, err)
		}
	}

	admin, err := sql.Open("mysql", os.Getenv(mysqlAdminDSNEnv))
	if err != nil {
		t.Fatalf("opening the admin connection: %s", err)
	}

	defer func() { _ = admin.Close() }()

	rows, err := admin.Query(`
		SELECT SUBSTRING_INDEX(name, '/', -1), file_size
		FROM information_schema.innodb_tablespaces
		WHERE name LIKE CONCAT(DATABASE(), '/%')`)
	if err != nil {
		t.Fatalf("reading the tablespace sizes: %s", err)
	}

	defer func() { _ = rows.Close() }()

	counted := map[string]bool{}

	for _, table := range countedTables(database) {
		counted[table] = true
	}

	var total int64

	for rows.Next() {
		var (
			name  string
			bytes int64
		)

		if err := rows.Scan(&name, &bytes); err != nil {
			t.Fatalf("scanning a tablespace size: %s", err)
		}

		// A FULLTEXT index's auxiliary tablespaces are named for the index rather than the table,
		// so they are matched by prefix and attributed to the content index they belong to.
		if counted[name] || (database.contentIndexed() && strings.HasPrefix(strings.ToLower(name), "fts_")) {
			total += bytes
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading the tablespace sizes: %s", err)
	}

	return total
}
