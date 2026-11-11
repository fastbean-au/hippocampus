package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// churnMemories writes `turnovers` rounds of `perRound` memories and, after each round, forgets all
// but one in ten of everything the store holds - scattered across the key range rather than taken
// from one end, which is what value-ranked eviction does to a store.
//
// That pattern is the mechanism TODO-3 item 183 found. Every leaf a delete thins keeps a survivor or
// two, so VACUUM never finds it empty and never deletes it, and new rows only ever land on the
// right-hand edge of a timestamp-ordered index, so nothing refills it. The index grows with what the
// store has forgotten rather than with what it holds.
func churnMemories(t *testing.T, d *DB, turnovers int, perRound int) {
	t.Helper()

	ctx := context.Background()
	timestamp := int64(1700000000000000000)

	var held []string

	for round := range turnovers {
		for i := range perRound {
			timestamp++

			id, err := d.CreateMemory(ctx, types.Memory{
				Id:           fmt.Sprintf("%08x-%04x-churn-cccc-dddddddddddd", i*7919%perRound, round),
				Body:         "a body long enough for the content index to hold something for it",
				Significance: 5,
				TimeStamp:    timestamp,
			})
			if err != nil {
				t.Fatalf("CreateMemory: %s", err)
			}

			held = append(held, id)
		}

		var forgotten []string
		var kept []string

		for i, id := range held {
			if i%10 == 0 {
				kept = append(kept, id)

				continue
			}

			forgotten = append(forgotten, id)
		}

		if _, err := d.DeleteMemories(ctx, forgotten); err != nil {
			t.Fatalf("DeleteMemories: %s", err)
		}

		held = kept
	}
}

// vacuumForTest runs what autovacuum would have run by now, so the catalogue's entry counts describe
// the rows the store is left holding. A test cannot wait a minute for the daemon.
func vacuumForTest(t *testing.T, d *DB, tables ...string) {
	t.Helper()

	for _, table := range tables {
		if _, err := d.sql.Exec(`VACUUM ` + table); err != nil {
			t.Fatalf("VACUUM %s: %s", table, err)
		}
	}
}

// footprintOf finds one index in a measurement, failing the test where it is absent.
func footprintOf(t *testing.T, footprint StorageFootprint, index string) IndexFootprint {
	t.Helper()

	for _, measured := range footprint.Indexes() {
		if measured.Index != index {
			continue
		}

		return measured
	}

	t.Fatalf("index %s is not in the footprint", index)

	return IndexFootprint{}
}

// TestRebuildBloatedIndexesIsANoOpWithoutAnOnlineRebuild holds the dialects that have no online
// rebuild to doing nothing: SQLite has no bloat to return (a freed page goes to the freelist that
// incremental_vacuum already returns), and MySQL keeps its B-trees dense, holding what forgetting
// freed as tablespace free space instead.
func TestRebuildBloatedIndexesIsANoOpWithoutAnOnlineRebuild(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().rebuildIndex != "" {
		t.Skipf("%s rebuilds online; covered by TestRebuildBloatedIndexesReturnsTheSpace", d.dialect().name)
	}

	seedForFootprint(t, d, 20)

	rebuilt, err := d.RebuildBloatedIndexes(context.Background(), IndexRebuildPolicy{})
	if err != nil {
		t.Fatalf("RebuildBloatedIndexes: %s", err)
	}

	if len(rebuilt) != 0 {
		t.Errorf("a dialect with no online rebuild rebuilt %d indexes", len(rebuilt))
	}
}

// TestRebuildBloatedIndexesReturnsTheSpace is the dialect test TODO-3 item 183 asked for: several
// full turnovers of a store, forgotten the way eviction forgets, leave the indexes far past their
// live entries - and a rebuild pass brings each one back within a bound of them, while the store
// goes on answering from it.
func TestRebuildBloatedIndexesReturnsTheSpace(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().rebuildIndex == "" {
		t.Skipf("%s has no online rebuild", d.dialect().name)
	}

	ctx := context.Background()

	churnMemories(t, d, 5, 2000)
	vacuumForTest(t, d, "memories")

	before, err := d.StorageFootprint(ctx)
	if err != nil {
		t.Fatalf("StorageFootprint: %s", err)
	}

	listing := footprintOf(t, before, listingIndexName)

	// The precondition, asserted so the test cannot pass by never having bloated anything: packed,
	// an entry of this index is a timestamp, a 37-byte id and the tuple overhead - under a hundred
	// bytes.
	if listing.BytesPerEntry() < 300 {
		t.Fatalf("churn left %s at %.0f bytes an entry; the test needs it bloated to mean anything",
			listingIndexName,
			listing.BytesPerEntry(),
		)
	}

	// The floor is below the test's indexes and above an empty one, which is a single page whatever
	// it holds and so divides out to thousands of bytes an entry without a byte of it being bloat.
	const bound = 200

	policy := IndexRebuildPolicy{MinBytes: 64 << 10, MaxBytesPerEntry: bound}

	rebuilt, err := d.RebuildBloatedIndexes(ctx, policy)
	if err != nil {
		t.Fatalf("RebuildBloatedIndexes: %s", err)
	}

	var sawListing bool

	for _, rebuild := range rebuilt {
		t.Logf("rebuilt %s.%s: %d to %d bytes over %d entries, in %s",
			rebuild.Table,
			rebuild.Index,
			rebuild.BytesBefore,
			rebuild.BytesAfter,
			rebuild.Entries,
			rebuild.Duration,
		)

		if rebuild.Err != nil {
			t.Errorf("rebuilding %s failed: %s", rebuild.Index, rebuild.Err)
		}

		if rebuild.BytesAfter >= rebuild.BytesBefore {
			t.Errorf("rebuilding %s took it from %d to %d bytes", rebuild.Index, rebuild.BytesBefore, rebuild.BytesAfter)
		}

		if rebuild.Index == listingIndexName {
			sawListing = true
		}
	}

	if !sawListing {
		t.Fatalf("the pass did not rebuild %s at %.0f bytes an entry", listingIndexName, listing.BytesPerEntry())
	}

	after, err := d.StorageFootprint(ctx)
	if err != nil {
		t.Fatalf("StorageFootprint: %s", err)
	}

	for _, index := range after.Indexes() {
		if index.Table == contentSearchTable || index.Bytes < policy.MinBytes {
			continue
		}

		if index.BytesPerEntry() > bound {
			t.Errorf("%s.%s is still at %.0f bytes an entry after the pass", index.Table, index.Index, index.BytesPerEntry())
		}
	}

	memories, err := d.GetMemories(ctx, MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories after the rebuild: %s", err)
	}

	if len(*memories) == 0 {
		t.Error("the store answered with no memories after its indexes were rebuilt")
	}

	again, err := d.RebuildBloatedIndexes(ctx, policy)
	if err != nil {
		t.Fatalf("RebuildBloatedIndexes, second pass: %s", err)
	}

	for _, rebuild := range again {
		t.Errorf("a second pass rebuilt %s again, at %d bytes", rebuild.Index, rebuild.BytesBefore)
	}
}

// TestRebuildBloatedIndexesReturnsAQueueIndexToItsRows covers the other shape item 183 found, on
// the search outbox. A queue's keys only increase, so a drained page IS emptied, deleted and
// recycled - but an index file never shrinks, so the index stays at the largest the queue has ever
// been. With nothing queued, every byte of it is air.
func TestRebuildBloatedIndexesReturnsAQueueIndexToItsRows(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().rebuildIndex == "" {
		t.Skipf("%s has no online rebuild", d.dialect().name)
	}

	ctx := context.Background()

	d.SetSearchOutbox(true)
	t.Cleanup(func() { d.SetSearchOutbox(false) })

	churnMemories(t, d, 1, 5000)

	for {
		claimed, err := d.ClaimSearchDeletes(ctx, 1000)
		if err != nil {
			t.Fatalf("ClaimSearchDeletes: %s", err)
		}

		if len(claimed) == 0 {
			break
		}

		seqs := make([]int64, 0, len(claimed))
		for _, entry := range claimed {
			seqs = append(seqs, entry.Seq)
		}

		if err := d.ConfirmSearchDeletes(ctx, seqs); err != nil {
			t.Fatalf("ConfirmSearchDeletes: %s", err)
		}
	}

	vacuumForTest(t, d, searchOutboxTable)

	rebuilt, err := d.RebuildBloatedIndexes(ctx, IndexRebuildPolicy{MinBytes: 64 << 10, MaxBytesPerEntry: 512})
	if err != nil {
		t.Fatalf("RebuildBloatedIndexes: %s", err)
	}

	var sawOutbox bool

	for _, rebuild := range rebuilt {
		if rebuild.Table != searchOutboxTable {
			continue
		}

		sawOutbox = true

		if rebuild.BytesAfter >= rebuild.BytesBefore {
			t.Errorf("rebuilding %s took it from %d to %d bytes", rebuild.Index, rebuild.BytesBefore, rebuild.BytesAfter)
		}
	}

	if !sawOutbox {
		t.Error("the pass left the drained outbox's indexes at their high-water mark")
	}
}

// TestRebuildBloatedIndexesDropsAnAbandonedRebuild covers the failure a rebuild leaves behind. An
// interrupted REINDEX CONCURRENTLY - a timeout, a restart, a cancelled cycle - leaves its transient
// copy behind as an INVALID index named <index>_ccnew, which the planner never uses but every write
// still maintains. The next pass has to drop it, or each interruption adds a permanent write cost.
func TestRebuildBloatedIndexesDropsAnAbandonedRebuild(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().rebuildIndex == "" {
		t.Skipf("%s has no online rebuild", d.dialect().name)
	}

	ctx := context.Background()

	seedForFootprint(t, d, 20)

	// A unique index over a column every row shares fails its build, and CONCURRENTLY leaves the
	// failed index behind as invalid - the same state an interrupted rebuild leaves.
	abandoned := listingIndexName + "_ccnew"

	if _, err := d.sql.Exec(`CREATE UNIQUE INDEX CONCURRENTLY ` + abandoned + ` ON memories (recall_count)`); err == nil {
		t.Fatal("the unique build over duplicate values succeeded; the fixture needs it to fail")
	}

	if _, err := d.RebuildBloatedIndexes(ctx, IndexRebuildPolicy{MaxBytesPerEntry: 1 << 30}); err != nil {
		t.Fatalf("RebuildBloatedIndexes: %s", err)
	}

	var remaining int

	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM pg_class WHERE relname = $1`, abandoned).Scan(&remaining); err != nil {
		t.Fatalf("looking for %s: %s", abandoned, err)
	}

	if remaining != 0 {
		t.Errorf("the pass left the abandoned %s in place", abandoned)
	}
}

// TestIndexRebuildPolicyWants is the decision on its own, which every dialect shares.
func TestIndexRebuildPolicyWants(t *testing.T) {
	t.Parallel()

	policy := IndexRebuildPolicy{MinBytes: 16 << 20, MaxBytesPerEntry: 512}

	cases := []struct {
		name  string
		index IndexFootprint
		want  bool
	}{
		{"bloated", IndexFootprint{Table: "memories", Index: "a", Bytes: 400 << 20, Entries: 140000}, true},
		{"healthy", IndexFootprint{Table: "memories", Index: "a", Bytes: 20 << 20, Entries: 140000}, false},
		{"under the floor", IndexFootprint{Table: "memories", Index: "a", Bytes: 8 << 20, Entries: 10}, false},
		{"an emptied queue", IndexFootprint{Table: searchOutboxTable, Index: "a", Bytes: 400 << 20, Entries: 0}, true},
		{"the content index", IndexFootprint{Table: contentSearchTable, Index: "a", Bytes: 400 << 20, Entries: 10}, false},
	}

	for _, c := range cases {
		if got := policy.wants(c.index); got != c.want {
			t.Errorf("%s: wants = %v, want %v", c.name, got, c.want)
		}
	}
}
