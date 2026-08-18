package db

import (
	"context"
	"fmt"
	"testing"
)

// These benchmarks measure the LISTING path - GetMemories with a filter, which is what the console's
// first screen, `hippo memory list`, and every paging client issue - as distinct from the sleep-cycle
// scans the benchmarks in bench_test.go pin.
//
//	go test ./db -bench 'BenchmarkList|BenchmarkScoped' -run XXX -benchmem
//
// What they exist to keep visible: the timestamp listing must WALK idx_memories_listing_v1, not sort.
// Before that index existed a page of 50 cost a full scan and a temp B-tree sort of everything the
// filter matched - 84 ms at 100,000 rows, growing with the store - and it is now 0.16 ms and flat.
// The "schema" variant is the store as shipped; the others add an index on top, and exist to record
// two traps measured in TODO 74.3: a plain (timestamp) index is 8.5x SLOWER than none (the planner
// takes it and turns a sequential scan into a random-access walk), and (group_name, timestamp) makes
// the scoped COUNT 57x faster while making the scoped PAGE 181x slower, for a net loss - which is
// why the count is not answered with an index.
//
// The seeded store spreads memories across benchGroupCount groups so the scoped variants measure a
// realistic selectivity rather than a filter that matches everything or nothing.

// benchGroupCount is how many groups the seeded store is spread across: a handful of tenants
// sharing one store, each holding a tenth of it.
const benchGroupCount = 10

// benchListingIndexes are the index shapes worth comparing. The empty definition is the schema as it
// stands, and has to stay first so a reader sees the baseline before the variants.
var benchListingIndexes = []struct {
	name       string
	definition string
}{
	{name: "schema"},
	{name: "timestamp", definition: `CREATE INDEX ix_bench ON memories (timestamp)`},
	{name: "timestamp-desc-id-asc", definition: `CREATE INDEX ix_bench ON memories (timestamp DESC, id ASC)`},
	{name: "group-timestamp", definition: `CREATE INDEX ix_bench ON memories (group_name, timestamp)`},
}

// seedBenchGroups spreads the seeded memories across benchGroupCount groups, as a bulk UPDATE rather
// than a change to seedBenchStore - so the sleep-cycle benchmarks keep measuring what they measured.
func seedBenchGroups(b *testing.B, d *DB) {
	b.Helper()

	// rowid % N is deterministic and evenly spread, so a single-group filter has a selectivity of
	// exactly 1/N rather than whatever an ordering accident produced.
	if _, err := d.sql.Exec(
		fmt.Sprintf(`UPDATE memories SET group_name = 'group-' || (rowid %% %d)`, benchGroupCount),
	); err != nil {
		b.Fatalf("seeding groups: %s", err)
	}
}

// newBenchListingStore seeds a store, spreads it across groups, and applies one of the candidate
// indexes.
func newBenchListingStore(b *testing.B, memories int, index string) *DB {
	b.Helper()

	d := newBenchSQLite(b, memories)

	seedBenchGroups(b, d)

	if index != "" {
		if _, err := d.sql.Exec(index); err != nil {
			b.Fatalf("creating index: %s", err)
		}
	}

	return d
}

// benchListPage runs one page request to exhaustion, failing if it does not come back full - a short
// page would mean the filter, not the ordering, decided the cost.
func benchListPage(b *testing.B, d *DB, filter MemoryFilter) {
	b.Helper()

	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		memories, err := d.GetMemories(ctx, filter)
		if err != nil {
			b.Fatalf("GetMemories: %s", err)
		}

		if len(*memories) != filter.Limit {
			b.Fatalf("expected a full page of %d, got %d", filter.Limit, len(*memories))
		}
	}
}

// BenchmarkListPage is an unscoped page of the most recently stored memories: the shape of an
// ordinary list request, and the control the scoped numbers are read against.
func BenchmarkListPage(b *testing.B) {
	for _, size := range benchSizes {
		for _, index := range benchListingIndexes {
			b.Run(fmt.Sprintf("%d/%s", size, index.name), func(b *testing.B) {
				d := newBenchListingStore(b, size, index.definition)

				benchListPage(b, d, MemoryFilter{OrderBy: "timestamp", Limit: 50})
			})
		}
	}
}

// BenchmarkScopedListPage is the same page as one group-scoped caller sees it. The comparison that
// matters is against BenchmarkListPage at the same size: the group predicate REDUCES the work,
// because what dominates is sorting everything the filter matched.
func BenchmarkScopedListPage(b *testing.B) {
	for _, size := range benchSizes {
		for _, index := range benchListingIndexes {
			b.Run(fmt.Sprintf("%d/%s", size, index.name), func(b *testing.B) {
				d := newBenchListingStore(b, size, index.definition)

				benchListPage(b, d, MemoryFilter{
					Groups:  []string{"group-3"},
					OrderBy: "timestamp",
					Limit:   50,
				})
			})
		}
	}
}

// BenchmarkScopedListCount measures the OTHER query every listing issues: GetMemories fills
// TotalCount from CountMemoriesFiltered, over the same predicate and with no limit to bound it. It
// is a second full pass per request, and unlike the page it has no ordering to blame - which is why
// an index shows up here so much more plainly.
func BenchmarkScopedListCount(b *testing.B) {
	for _, size := range benchSizes {
		for _, index := range benchListingIndexes {
			b.Run(fmt.Sprintf("%d/%s", size, index.name), func(b *testing.B) {
				d := newBenchListingStore(b, size, index.definition)

				filter := MemoryFilter{Groups: []string{"group-3"}}
				ctx := context.Background()

				b.ResetTimer()
				b.ReportAllocs()

				for i := 0; i < b.N; i++ {
					count, err := d.CountMemoriesFiltered(ctx, filter)
					if err != nil {
						b.Fatalf("CountMemoriesFiltered: %s", err)
					}

					if count == 0 {
						b.Fatal("expected the seeded group to be non-empty")
					}
				}
			})
		}
	}
}
