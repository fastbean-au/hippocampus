package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// These benchmarks pin the cost of the sleep-cycle scans, whose acceptable performance rests on
// invariants nothing else checks — most importantly that the consolidation scans read only the
// covering index and never touch memory bodies. They are run on demand (not gated in CI, where
// shared-runner noise swamps real regressions):
//
//	go test ./db -bench . -run XXX
//
// Compare against a baseline with benchstat when touching sleep.go, the db scans, or the schema.
// A regression that starts reading bodies shows up as a jump in both ns/op and B/op.

// benchBodyBytes is the body size for seeded memories — large enough that accidentally reading
// bodies inside a scan is unmissable, small enough that the 100k-row store stays modest.
const benchBodyBytes = 1024

// benchSizes are the store sizes each benchmark runs at. Two points are enough to see whether a
// change moved the constant or the slope.
var benchSizes = []int{10_000, 100_000}

// seedBenchStore populates the store through a single transaction with prepared statements
// (CreateMemory's one-implicit-transaction-per-row is far too slow for 100k rows), working on
// both dialects via rebind. Layout: half the memories attach to events (10 per event), half are
// loose, and one empty event per 100 memories feeds the bare-event scan.
func seedBenchStore(b *testing.B, d *DB, memories int) {
	b.Helper()

	body := []byte(strings.Repeat("x", benchBodyBytes))
	now := time.Now().UnixNano()

	tx, err := d.sql.Begin()
	if err != nil {
		b.Fatalf("Begin: %s", err)
	}

	levelID := seedBenchLevels(b, d, tx)

	insertMemory, err := tx.Prepare(d.rebind(
		`INSERT INTO memories (` + memoryStoredColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	))
	if err != nil {
		b.Fatalf("Prepare (memories): %s", err)
	}

	insertEvent, err := tx.Prepare(d.rebind(
		`INSERT INTO events (id, time_start, time_end, significance_level_id, name, description,
			memories_consolidated, relationship_significance, relationships)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	))
	if err != nil {
		b.Fatalf("Prepare (events): %s", err)
	}

	for i := 0; i < memories; i++ {
		eventId := ""

		// The first half of the memories attach to events, 10 per event; the event row is
		// created alongside its first memory.
		if i < memories/2 {
			eventId = fmt.Sprintf("bench-event-%07d", i/10)

			if i%10 == 0 {
				if _, err := insertEvent.Exec(
					eventId, now, now, levelID[1+i%100], "bench event", "", false, 0, "[]",
				); err != nil {
					b.Fatalf("insert event: %s", err)
				}
			}
		}

		if _, err := insertMemory.Exec(
			fmt.Sprintf("bench-memory-%08d", i), now, levelID[1+i%100], eventId, body, false, 0, 0, false, "",
		); err != nil {
			b.Fatalf("insert memory: %s", err)
		}
	}

	// Empty events, one per 100 memories, for the bare-event pass.
	for i := 0; i < memories/100; i++ {
		if _, err := insertEvent.Exec(
			fmt.Sprintf("bench-empty-event-%07d", i), now, now, levelID[1+i%100], "bench empty event", "", false, 0, "[]",
		); err != nil {
			b.Fatalf("insert empty event: %s", err)
		}
	}

	if err := tx.Commit(); err != nil {
		b.Fatalf("Commit: %s", err)
	}
}

// seedBenchLevels seeds the significance registry with ranks 1..100 inside the given transaction and
// returns a rank -> level id map, so the bulk seed can reference levels directly (matching the
// 1+i%100 significance spread the benchmarks use) instead of resolving one level per row.
func seedBenchLevels(b *testing.B, d *DB, tx *sql.Tx) map[int]int64 {
	b.Helper()

	// Idempotent so a re-seed (the delete benchmark re-seeds rows each iteration over an already
	// level-seeded store) does not collide on the rank UNIQUE constraint.
	for r := 1; r <= 100; r++ {
		if _, err := tx.Exec(d.rebind(
			`INSERT INTO significance_levels (level_rank) SELECT ? WHERE NOT EXISTS (SELECT 1 FROM significance_levels WHERE level_rank = ?)`,
		), r, r); err != nil {
			b.Fatalf("insert level: %s", err)
		}
	}

	rows, err := tx.Query(`SELECT id, level_rank FROM significance_levels`)
	if err != nil {
		b.Fatalf("read levels: %s", err)
	}

	levelID := make(map[int]int64, 100)

	for rows.Next() {
		var id int64
		var rank int

		if err := rows.Scan(&id, &rank); err != nil {
			_ = rows.Close()
			b.Fatalf("scan level: %s", err)
		}

		levelID[rank] = id
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		b.Fatalf("read levels: %s", err)
	}

	_ = rows.Close()

	return levelID
}

// newBenchSQLite returns a seeded in-memory store. In-memory keeps the numbers deterministic
// (no filesystem noise); the scan and allocation costs being pinned are identical either way.
func newBenchSQLite(b *testing.B, memories int) *DB {
	b.Helper()

	d, err := New("")
	if err != nil {
		b.Fatalf("New: %s", err)
	}

	b.Cleanup(func() { _ = d.Close() })

	seedBenchStore(b, d, memories)

	return d
}

// scanOnlyServer never consolidates, so each iteration scans the full store without mutating it.
func scanOnlyServer() *decisionServer {
	return &decisionServer{value: func(candidate MemoryConsolidationCandidate) float64 {
		return float64(candidate.MemorySignificance)
	}}
}

// BenchmarkConsolidateMemories measures the loose-memory pass (memories with no event).
func BenchmarkConsolidateMemories(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := newBenchSQLite(b, size)
			server := scanOnlyServer()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, _ = d.ConsolidateMemories(context.Background(), server)
			}
		})
	}
}

// BenchmarkConsolidateEventMemories measures the evented pass (the join against events).
func BenchmarkConsolidateEventMemories(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := newBenchSQLite(b, size)
			server := scanOnlyServer()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, _, _, _ = d.ConsolidateEventMemories(context.Background(), server)
			}
		})
	}
}

// BenchmarkConsolidateEvents measures the bare-event pass (events with no memories).
func BenchmarkConsolidateEvents(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := newBenchSQLite(b, size)
			server := scanOnlyServer()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, _ = d.ConsolidateEvents(context.Background(), server)
			}
		})
	}
}

// BenchmarkEvictMemories measures the eviction path: the full candidate scan, the value sort,
// and one conditional delete (a 1-byte request evicts a single memory per iteration, so the
// store shrinks negligibly across the run while the dominant scan-and-sort cost stays honest).
func BenchmarkEvictMemories(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := newBenchSQLite(b, size)
			server := scanOnlyServer()

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, _, _, _ = d.EvictMemories(context.Background(), server, 1)
			}
		})
	}
}

// seedDeletableMemories inserts n loose, unrecalled memories in one transaction and returns their
// snapshots, for the delete-throughput benchmark's per-iteration re-seed (run under a stopped
// timer, so its cost is excluded).
func seedDeletableMemories(b *testing.B, d *DB, n int) []memoryRecallSnapshot {
	b.Helper()

	body := []byte(strings.Repeat("x", benchBodyBytes))
	now := time.Now().UnixNano()

	tx, err := d.sql.Begin()
	if err != nil {
		b.Fatalf("Begin: %s", err)
	}

	levelID := seedBenchLevels(b, d, tx)

	insert, err := tx.Prepare(d.rebind(`INSERT INTO memories (` + memoryStoredColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`))
	if err != nil {
		b.Fatalf("Prepare: %s", err)
	}

	snapshots := make([]memoryRecallSnapshot, n)

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("del-%08d", i)

		if _, err := insert.Exec(id, now, levelID[1], "", body, false, 0, 0, false, ""); err != nil {
			b.Fatalf("insert: %s", err)
		}

		snapshots[i] = memoryRecallSnapshot{id: id, timeRecalled: 0, recallCount: 0}
	}

	if err := tx.Commit(); err != nil {
		b.Fatalf("Commit: %s", err)
	}

	return snapshots
}

// BenchmarkDeleteMemoriesIfUnrecalled pins the throughput of the batched, guarded delete so a
// regression from the chunked row-value batching back toward one guarded statement per row
// shows up as a jump in ns/op. Each iteration re-seeds the rows it deletes under a
// stopped timer, so only the delete itself is measured.
func BenchmarkDeleteMemoriesIfUnrecalled(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := newBenchSQLite(b, 0)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				snapshots := seedDeletableMemories(b, d, size)
				b.StartTimer()

				if _, err := d.deleteMemoriesIfUnrecalled(context.Background(), snapshots); err != nil {
					b.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
				}
			}
		})
	}
}

// BenchmarkUsedBytesSQLite measures the PRAGMA-based reading — constant-time by construction;
// the benchmark exists so a change that accidentally makes it scale with the store is caught.
func BenchmarkUsedBytesSQLite(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := newBenchSQLite(b, size)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := d.UsedBytes(context.Background()); err != nil {
					b.Fatalf("UsedBytes: %s", err)
				}
			}
		})
	}
}

// BenchmarkUsedBytesPostgres measures the live-row estimate, which is a heap scan per reading by
// design — this pins its cost curve. Skips unless HIPPOCAMPUS_TEST_POSTGRES_DSN names a
// disposable database, same as the integration tests.
func BenchmarkUsedBytesPostgres(b *testing.B) {
	dsn := os.Getenv(postgresTestDSNEnv)
	if dsn == "" {
		b.Skipf("set %s to run postgres benchmarks", postgresTestDSNEnv)
	}

	benchmarkUsedBytesServer(b, func() (*DB, error) { return NewPostgres(dsn, true) })
}

// BenchmarkUsedBytesMySQL is the MySQL arm of the same live-row-estimate benchmark. Skips unless
// HIPPOCAMPUS_TEST_MYSQL_DSN names a disposable database, same as the integration tests.
func BenchmarkUsedBytesMySQL(b *testing.B) {
	dsn := os.Getenv(mysqlTestDSNEnv)
	if dsn == "" {
		b.Skipf("set %s to run mysql benchmarks", mysqlTestDSNEnv)
	}

	benchmarkUsedBytesServer(b, func() (*DB, error) { return NewMySQL(dsn, true) })
}

func benchmarkUsedBytesServer(b *testing.B, open func() (*DB, error)) {
	b.Helper()

	for _, size := range benchSizes {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d, err := open()
			if err != nil {
				b.Fatalf("open: %s", err)
			}

			b.Cleanup(func() {
				_ = d.Purge(context.Background())
				_ = d.Close()
			})

			if err := d.Purge(context.Background()); err != nil {
				b.Fatalf("Purge: %s", err)
			}

			seedBenchStore(b, d, size)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := d.UsedBytes(context.Background()); err != nil {
					b.Fatalf("UsedBytes: %s", err)
				}
			}
		})
	}
}
