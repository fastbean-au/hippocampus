package db

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
)

// What the capacity target is true about, and what the disk says.
//
// UsedBytes on the server drivers is a live-row estimate and that is deliberate: no file-size
// measure on either server shrinks after a delete, so eviction driven by one would chase a figure
// that cannot drop. usedBytesLiveRows sets out the death spiral that avoids, the decision stands,
// and nothing in this file is a control input.
//
// Its consequence is what this file is about. Nothing compared the estimate with what the store was
// really occupying, and on a high-churn deployment the two diverge by most of an order of magnitude:
// a live instance reporting 153 MB against a 160 MB capacityBytes was holding 892 MB of database,
// of which 687 MB was indexes on `memories` and 533 MB was the covering index alone, at an average
// leaf density of 5%. The heap was fine and autovacuum was keeping up. The bloat is not a fault in
// either - VACUUM marks a B-tree page reusable and never repacks it, and a store keyed on a UUID
// that deletes sixty-two million rows over its life never refills a page it emptied.
//
// A store that forgets is a store whose indexes bloat. That is the direct cost of the thing this
// service exists to do, it is unbounded, and before this it was invisible to everything the service
// published.
//
// Three decisions carry the shape.
//
// (1) It is REPORTED AND NEVER ACTED ON. REINDEX CONCURRENTLY is online but it is still a
// maintenance decision, and a memory store that reindexes itself on a timer is a store that
// surprises its operator. docs/operations.md carries the runbook and the cadence the measurement
// justifies; this publishes the figure that says when.
//
// (2) It is MEASURED, not estimated. Every number here is a catalogue reading - what the engine
// says the relation and each of its indexes occupy, and how many entries each index holds. The one
// estimate in the comparison is UsedBytes, which is already published as one. The authoritative
// bloat figure, pgstatindex's avg_leaf_density, is deliberately not taken: it reads every page of
// the index, which is unaffordable once a sleep cycle on the index this exists about, and it needs
// an extension a managed instance may not have. Bytes against entries says the same thing from the
// catalogue and for free.
//
// (3) It is PER INDEX, because only that says what to act on. pg_database_size says the store is
// large; it does not say that one of five indexes is 80% of it. The per-index reading is also what
// makes this visible on a SMALL store, which the same measurement found to be proportionally the
// worst affected - 1,011 memories in a 1.3 MB heap carrying a 3.5 MB listing index, 3,581 bytes an
// entry. Bloat tracks churn, not row count.

// maxIndexesPerTable bounds how many indexes one table reports.
//
// It is a cardinality bound rather than tidiness: an index name is a metric attribute, and while
// the indexes this store creates are a small fixed set, an operator may add their own and nothing
// here would otherwise stop the series count growing with them. The list arrives largest first, so
// truncation keeps the index an operator would act on and drops the ones they would not.
const maxIndexesPerTable = 16

// IndexFootprint is what one index really occupies and how many entries it holds.
//
// The pair is the reading, not two readings. A byte count alone says an index is large, which on a
// large store is expected; bytes divided by entries says what one entry is costing, and that is a
// number with a known right answer - the widest key in this store is a 37-character id and seven
// numeric columns, so an entry that costs a kilobyte is air whatever the store's size. It is the
// same argument AncillaryTable makes for reporting Rows beside Bytes, arriving at the opposite
// conclusion about which of the two is the measurement.
//
// Entries is the catalogue's own estimate (pg_class.reltuples), refreshed by ANALYZE rather than
// maintained per write, so it lags a heavy burst. That is acceptable here and would not be if this
// drove anything: the quantity being judged moves over days.
type IndexFootprint struct {
	Table   string
	Index   string
	Bytes   int64
	Entries int64
}

// BytesPerEntry is what one entry of this index is costing on disk, or 0 for an index the catalogue
// reports as empty - which is an absence rather than a ratio, and must not be rendered as one.
func (i IndexFootprint) BytesPerEntry() float64 {
	if i.Entries <= 0 {

		return 0
	}

	return float64(i.Bytes) / float64(i.Entries)
}

// TableFootprint is one counted table's real occupancy: everything the engine holds for it, and the
// share of that its indexes account for.
//
// Bytes is pg_total_relation_size - heap, out-of-line storage and indexes together - so it is the
// figure to compare against the store's estimate of that table. IndexBytes sums the indexes below
// it, and the difference is what the rows themselves are holding. Reporting the split is what
// separates the two findings this exists for: a heap growing past its estimate is a store that is
// not forgetting fast enough, while indexes growing past it is a store that is forgetting exactly
// as designed and paying for it in space nothing reclaims.
type TableFootprint struct {
	Table      string
	Bytes      int64
	IndexBytes int64
	Indexes    []IndexFootprint
}

// HeapBytes is what the rows and their out-of-line storage hold, which is everything the relation
// occupies that is not an index. Floored at zero: the two readings are taken by separate statements
// against a live store, so a table that gained an index between them could otherwise report a
// negative heap.
func (t TableFootprint) HeapBytes() int64 {
	return max(t.Bytes-t.IndexBytes, 0)
}

// StorageFootprint is what the tables inside the byte capacity target really occupy.
//
// Measured separates the two zeroes, on AncillaryTable.Enabled's reasoning: a dialect that cannot
// answer cheaply reports nothing, and a nothing that rendered as 0 bytes would read as a store
// occupying no disk. Only one of the three dialects can answer, for the reasons dialect.relationBytes
// gives - and on the embedded one there is nothing to answer, since page accounting already counts
// every index inside the target.
//
// The tables are exactly the ones usedBytesLiveRows counts, plus the content index where the store
// carries one, because the comparison is only meaningful against the same set: this figure is read
// against UsedBytes, and a footprint covering tables the estimate excludes would report a gap that
// was never the estimate's to close. The three tables the target deliberately excludes have their
// own reading - see AncillaryStorage, which reports their disk the same way.
type StorageFootprint struct {
	Measured bool
	Bytes    int64
	Tables   []TableFootprint
}

// IndexBytes is what every index over every counted table holds, which is the figure that moved:
// on the deployment this was measured against it was 687 MB against 32 MB of honest index.
func (f StorageFootprint) IndexBytes() int64 {
	var bytes int64

	for _, table := range f.Tables {
		bytes += table.IndexBytes
	}

	return bytes
}

// Indexes flattens every index across every counted table, largest first within each table, for a
// caller publishing one series per index or rendering a list.
func (f StorageFootprint) Indexes() []IndexFootprint {
	var out []IndexFootprint

	for _, table := range f.Tables {
		out = append(out, table.Indexes...)
	}

	return out
}

// countedTables names the tables the byte capacity target counts, in the order a report reads best.
//
// It is deliberately the same set usedBytesLiveRows sums and not "every table this store has": the
// footprint exists to be read against that estimate. The content index joins the list only where the
// store carries one, matching contentIndexed - which is also what memoryFootprint's own allowance
// for it is gated on, so the two sides of the comparison agree about whether it is there.
func (d *DB) countedTables() []string {
	tables := []string{"memories", "events", memoryLinksTable, eventLinksTable}

	if d.contentIndexed() {
		tables = append(tables, contentSearchTable)
	}

	return tables
}

// StorageFootprint measures what the counted tables really occupy, per table and per index.
//
// Catalogue lookups only - two statements per table, neither of which reads a row of data - so it
// costs a handful of round trips per sleep cycle and no scan. That bound is the whole reason the
// authoritative reading is not taken here; see dialect.indexFootprint.
//
// A dialect with no catalogue to ask returns an unmeasured footprint rather than an error: there is
// nothing wrong with a store on the embedded driver, and a caller that had to distinguish "failed"
// from "not applicable" by inspecting the error would be doing dialect reasoning outside this file.
func (d *DB) StorageFootprint(ctx context.Context) (StorageFootprint, error) {
	log.Trace("func() db.StorageFootprint")

	if d.dialect().relationBytes == "" {

		return StorageFootprint{}, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	out := StorageFootprint{Measured: true}

	for _, table := range d.countedTables() {
		measured, err := d.tableFootprint(ctx, table)
		if err != nil {

			return StorageFootprint{}, err
		}

		// A table this store does not have occupies nothing and is left out rather than reported as
		// an empty row: the list is what the deployment is holding, and a zero-byte entry in it
		// invites the reader to wonder which of the two it is.
		if measured.Bytes == 0 && len(measured.Indexes) == 0 {
			continue
		}

		out.Bytes += measured.Bytes
		out.Tables = append(out.Tables, measured)
	}

	return out, nil
}

// tableFootprint measures one table: what the engine holds for it, and what each of its indexes
// holds inside that.
func (d *DB) tableFootprint(ctx context.Context, table string) (TableFootprint, error) {
	total, err := d.relationBytes(ctx, table)
	if err != nil {

		return TableFootprint{}, err
	}

	indexes, err := d.indexFootprints(ctx, table)
	if err != nil {

		return TableFootprint{}, err
	}

	out := TableFootprint{Table: table, Bytes: total, Indexes: indexes}

	for _, index := range indexes {
		out.IndexBytes += index.Bytes
	}

	return out, nil
}

// indexFootprints lists one table's indexes, largest first, truncated at maxIndexesPerTable.
//
// The truncation is applied here rather than in the SQL so the dialect's query stays a description
// of what the catalogue holds and the bound stays one number in one place. The rows it drops are the
// smallest, which is what a reader would have skipped.
func (d *DB) indexFootprints(ctx context.Context, table string) ([]IndexFootprint, error) {
	query := d.dialect().indexFootprint

	if query == "" {

		return nil, nil
	}

	rows, err := d.query(ctx, d.rebind(query), table)
	if err != nil {

		return nil, fmt.Errorf("measuring the indexes on %s: %w", table, err)
	}

	defer func() { _ = rows.Close() }()

	var out []IndexFootprint

	for rows.Next() {
		index := IndexFootprint{Table: table}

		if err := rows.Scan(&index.Index, &index.Bytes, &index.Entries); err != nil {

			return nil, fmt.Errorf("measuring the indexes on %s: %w", table, err)
		}

		if len(out) >= maxIndexesPerTable {
			break
		}

		out = append(out, index)
	}

	if err := rows.Err(); err != nil {

		return nil, fmt.Errorf("measuring the indexes on %s: %w", table, err)
	}

	return out, nil
}
