package db

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// seedForFootprint writes enough memories that every index over `memories` holds entries, so a
// reading of zero anywhere is a fault rather than an empty store.
func seedForFootprint(t *testing.T, d *DB, count int) {
	t.Helper()

	ctx := context.Background()

	for i := range count {
		if _, err := d.CreateMemory(ctx, types.Memory{
			Id:           fmt.Sprintf("footprint-%08d-aaaa-bbbb-cccc-dddddddddddd", i),
			Body:         "a body long enough for the content index to hold something for it",
			Significance: 5,
			TimeStamp:    int64(1700000000000000000) + int64(i),
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}
}

// TestStorageFootprintAnswersOnlyWhereTheDialectCan is the shape of the whole feature: a driver
// that can read its catalogue cheaply reports what the disk holds, and one that cannot reports
// nothing rather than zero.
//
// The two-valued Measured flag is the thing under test, not the byte counts. A footprint of zero
// bytes and a footprint that could not be taken are opposite readings - the first says the store
// occupies no disk - and every caller above this branches on which it got.
func TestStorageFootprintAnswersOnlyWhereTheDialectCan(t *testing.T) {
	d := newTestDB(t)

	seedForFootprint(t, d, 50)

	footprint, err := d.StorageFootprint(context.Background())
	if err != nil {
		t.Fatalf("StorageFootprint: %s", err)
	}

	answers := d.dialect().relationBytes != ""

	if footprint.Measured != answers {
		t.Fatalf("Measured = %v on %s, want %v", footprint.Measured, d.dialect().name, answers)
	}

	if !answers {
		if footprint.Bytes != 0 || len(footprint.Tables) != 0 {
			t.Errorf("a dialect that cannot measure reported %d bytes over %d tables", footprint.Bytes, len(footprint.Tables))
		}

		return
	}

	if footprint.Bytes <= 0 {
		t.Errorf("a store holding 50 memories reported %d bytes on disk", footprint.Bytes)
	}

	if footprint.IndexBytes() <= 0 {
		t.Error("no index bytes reported for a store whose memories table carries three indexes")
	}

	if footprint.IndexBytes() > footprint.Bytes {
		t.Errorf("index bytes %d exceed the %d the tables occupy in total", footprint.IndexBytes(), footprint.Bytes)
	}
}

// TestStorageFootprintCoversTheTablesTheTargetCounts pins the set. The footprint is read AGAINST
// UsedBytes, so a footprint covering tables the estimate excludes would report a gap that was never
// the estimate's to close - and one missing a table the estimate counts would hide the gap it
// exists to show.
func TestStorageFootprintCoversTheTablesTheTargetCounts(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().relationBytes == "" {
		t.Skipf("%s reports no footprint", d.dialect().name)
	}

	seedForFootprint(t, d, 20)

	footprint, err := d.StorageFootprint(context.Background())
	if err != nil {
		t.Fatalf("StorageFootprint: %s", err)
	}

	reported := map[string]TableFootprint{}

	for _, table := range footprint.Tables {
		reported[table.Table] = table
	}

	for _, want := range d.countedTables() {
		if _, ok := reported[want]; !ok {
			t.Errorf("%s is counted by the capacity target but absent from the footprint", want)
		}
	}

	// The three excluded tables have their own reading (AncillaryStorage) and must not be counted
	// twice: a total that included them would not be comparable with used_bytes at all.
	for _, excluded := range []string{tombstonesTable, searchOutboxTable, callbackQueueTable} {
		if _, ok := reported[excluded]; ok {
			t.Errorf("%s is outside the capacity target but was reported inside the footprint", excluded)
		}
	}

	memories, ok := reported["memories"]
	if !ok {
		t.Fatal("no reading for the memories table")
	}

	if memories.HeapBytes() <= 0 {
		t.Errorf("memories heap = %d, want the rows to occupy something", memories.HeapBytes())
	}

	// A dialect that can only total its indexes says so by listing none, and still carries the
	// total: the heap/index split is half of what the reading is for.
	if d.dialect().indexFootprint == "" {
		if len(memories.Indexes) != 0 {
			t.Errorf("a dialect that lists no indexes reported %d of them", len(memories.Indexes))
		}

		if memories.IndexBytes <= 0 {
			t.Errorf("memories index bytes = %d, want the secondary indexes to occupy something", memories.IndexBytes)
		}

		return
	}

	// Three indexes: the primary key, the covering index and the listing index. Naming them is the
	// point - an operator acts on a NAME, and a per-table total would not tell them which.
	for _, want := range []string{"memories_pkey", coveringIndexName, listingIndexName} {
		found := false

		for _, index := range memories.Indexes {
			if index.Index != want {
				continue
			}

			found = true

			if index.Bytes <= 0 {
				t.Errorf("%s reported %d bytes", want, index.Bytes)
			}

			if index.Table != "memories" {
				t.Errorf("%s reports table %q", want, index.Table)
			}

			break
		}

		if !found {
			t.Errorf("index %s is missing from the memories footprint", want)
		}
	}
}

// TestStorageFootprintSurvivesAStoreWithoutTheContentIndex covers the one table in the counted set
// that a deployment may not have. It must be left out rather than reported as an empty row, since a
// zero-byte entry in the list invites the reader to wonder which of the two it is.
func TestStorageFootprintSurvivesAStoreWithoutTheContentIndex(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().relationBytes == "" {
		t.Skipf("%s reports no footprint", d.dialect().name)
	}

	if _, err := d.sql.Exec(`DROP TABLE IF EXISTS ` + contentSearchTable); err != nil {
		t.Fatalf("dropping the content index: %s", err)
	}

	footprint, err := d.StorageFootprint(context.Background())
	if err != nil {
		t.Fatalf("StorageFootprint: %s", err)
	}

	for _, table := range footprint.Tables {
		if table.Table == contentSearchTable {
			t.Errorf("%s was reported after being dropped", contentSearchTable)
		}
	}

	if !footprint.Measured {
		t.Error("a missing table made the whole reading unmeasured")
	}
}

// TestIndexFootprintBytesPerEntry is the division the alert and the log line both make, and the
// case it must not answer with is an index the catalogue reports as empty: that is an absence, and
// rendering it as a ratio would put an infinity on a dashboard.
func TestIndexFootprintBytesPerEntry(t *testing.T) {
	cases := []struct {
		name  string
		index IndexFootprint
		want  float64
	}{
		{"healthy", IndexFootprint{Bytes: 5200, Entries: 100}, 52},
		{"bloated", IndexFootprint{Bytes: 270000, Entries: 100}, 2700},
		{"never analysed", IndexFootprint{Bytes: 8192, Entries: 0}, 0},
		{"empty index", IndexFootprint{Bytes: 0, Entries: 0}, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.index.BytesPerEntry(); got != c.want {
				t.Errorf("BytesPerEntry() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestTableFootprintHeapIsNeverNegative pins the floor. The relation size and the index sizes are
// two statements against a live store, so a table that gained an index between them would otherwise
// report a negative heap - a figure that reads as a fault in the store rather than in the reading.
func TestTableFootprintHeapIsNeverNegative(t *testing.T) {
	table := TableFootprint{Bytes: 8192, IndexBytes: 16384}

	if got := table.HeapBytes(); got != 0 {
		t.Errorf("HeapBytes() = %d with indexes larger than the relation, want 0", got)
	}
}

// TestStorageFootprintTotalsAreDerivedNotStored covers the two accessors on their own, on every
// dialect. They are what the gauge and the log line are summed from, and a store the dialect cannot
// measure would otherwise leave them untested everywhere but one driver.
func TestStorageFootprintTotalsAreDerivedNotStored(t *testing.T) {
	footprint := StorageFootprint{
		Measured: true,
		Bytes:    900,
		Tables: []TableFootprint{
			{Table: "memories", Bytes: 700, IndexBytes: 400, Indexes: []IndexFootprint{
				{Table: "memories", Index: "memories_pkey", Bytes: 250},
				{Table: "memories", Index: "idx_memories_listing_v1", Bytes: 150},
			}},
			{Table: "events", Bytes: 200, IndexBytes: 100, Indexes: []IndexFootprint{
				{Table: "events", Index: "events_pkey", Bytes: 100},
			}},
		},
	}

	if got := footprint.IndexBytes(); got != 500 {
		t.Errorf("IndexBytes() = %d, want the two tables' 500", got)
	}

	// Flattened across tables, in each table's own order, so a caller publishing one series per
	// index does not have to walk the nesting itself.
	indexes := footprint.Indexes()

	if len(indexes) != 3 {
		t.Fatalf("Indexes() returned %d, want 3", len(indexes))
	}

	for _, index := range indexes {
		if index.Table == "" {
			t.Errorf("%s arrived without the table it belongs to, which is what a REINDEX needs", index.Index)
		}
	}

	if empty := (StorageFootprint{}); empty.IndexBytes() != 0 || empty.Indexes() != nil {
		t.Error("an unmeasured footprint reported totals")
	}
}

// TestCountedTablesFollowTheCapacityTarget pins the set on every dialect, since it is the thing that
// makes the reading comparable with UsedBytes at all - and the content index's membership has to
// track contentIndexed rather than being listed unconditionally.
func TestCountedTablesFollowTheCapacityTarget(t *testing.T) {
	d := newTestDB(t)

	tables := d.countedTables()

	for _, want := range []string{"memories", "events", memoryLinksTable, eventLinksTable} {
		if !slices.Contains(tables, want) {
			t.Errorf("%s is counted by the capacity target but absent from the footprint's tables", want)
		}
	}

	if got := slices.Contains(tables, contentSearchTable); got != d.contentIndexed() {
		t.Errorf("the content index is listed = %v while contentIndexed() = %v - the footprint and the estimate would disagree about whether it is there", got, d.contentIndexed())
	}

	for _, excluded := range []string{tombstonesTable, searchOutboxTable, callbackQueueTable, instancesTable} {
		if slices.Contains(tables, excluded) {
			t.Errorf("%s is outside the capacity target but is counted inside the footprint", excluded)
		}
	}
}

// footprintFollowWindow is how long TestStorageFootprintFollowsTheStore waits for a reading to move.
// InnoDB recalculates a table's statistics in the background once a tenth of its rows have changed,
// checking every ten seconds, so a current reading arrives within about that; a cached one never
// does inside any window a test could afford.
const footprintFollowWindow = 45 * time.Second

// TestStorageFootprintFollowsTheStore is the reading being CURRENT, which is a different property
// from it being taken at all. A figure served from a cache answers with whatever the table was the
// last time anything looked - MySQL's information_schema did exactly that by default, reporting a
// ten-thousand-row table at 16 KiB - and a stale size presented as what the disk holds is worse than
// no size, because it is believed.
//
// So: take a reading, grow the store well past it, and require the reading to follow.
func TestStorageFootprintFollowsTheStore(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().relationBytes == "" {
		t.Skipf("%s reports no footprint", d.dialect().name)
	}

	ctx := context.Background()

	seedForFootprint(t, d, 10)

	before, err := d.StorageFootprint(ctx)
	if err != nil {
		t.Fatalf("StorageFootprint: %s", err)
	}

	for i := range 4000 {
		if _, err := d.CreateMemory(ctx, types.Memory{
			Id:           fmt.Sprintf("follows-%08d-aaaa-bbbb-cccc-dddddddddddd", i),
			Body:         "a body long enough that four thousand of them occupy more than a page or two",
			Significance: 5,
			TimeStamp:    int64(1700000000000000000) + int64(i),
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	deadline := time.Now().Add(footprintFollowWindow)

	for {
		after, err := d.StorageFootprint(ctx)
		if err != nil {
			t.Fatalf("StorageFootprint: %s", err)
		}

		if after.Bytes > before.Bytes {
			t.Logf("footprint followed the store: %d -> %d bytes", before.Bytes, after.Bytes)

			return
		}

		if time.Now().After(deadline) {
			t.Fatalf(
				"the footprint still reads %d bytes %s after four thousand memories were written - "+
					"it is being served from a cache, not measured",
				after.Bytes, footprintFollowWindow,
			)
		}

		time.Sleep(time.Second)
	}
}
