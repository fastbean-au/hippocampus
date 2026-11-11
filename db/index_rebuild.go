package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Rebuilding the indexes forgetting empties (TODO-3 item 183).
//
// footprint.go measures the bloat; this returns it. Two mechanisms produce it on PostgreSQL, both
// reproduced against a local server before anything here was written, and neither is something a
// storage parameter can hold back:
//
//   - Sparse pages. Eviction forgets by value, so its deletes land all across an index's key range
//     and leave a survivor or two on many leaves. VACUUM deletes only an EMPTY B-tree page, nothing
//     merges sparse ones, and every new row lands on the right-hand edge of the timestamp-ordered
//     indexes - so a thinned leaf is never refilled. The demo's consolidation index was at 5% leaf
//     density.
//   - The high-water mark. A page that IS emptied is deleted and recycled, but an index file never
//     shrinks, so an index stays at the largest it has ever been. On a store that churns faster than
//     autovacuum's one-minute naptime, that is set by the dead entries one naptime accumulates.
//     It is the whole story for the queues: the demo's search outbox held ten rows in 833 MB of
//     index, its heap having been truncated back to 24 kB.
//
// The memories table has both. A steady-state run of 50,000 rows under churn took the listing index
// from 6 MB to 127 MB, 79% of its pages deleted and reusable and the rest at 48% density - and the
// demo's 533 MB consolidation index carried 20,183 deleted pages beside its 5%-dense leaves. The
// same run with a scale factor of 0.005, no cost delay and a fillfactor of 70 reached 133 MB:
// autovacuum was already running on every naptime, which is a server setting, and fillfactor sets
// how full a page starts, not whether it is refilled.
//
// REINDEX INDEX CONCURRENTLY returns both, online. It takes no lock that stops reads or writes, and
// on the indexes this is about it takes seconds: 533 MB to 13 MB in under one, on the demo.
//
// Off by default (storage.indexRebuild.enabled). footprint.go's first decision was that the service
// reports this and never acts on it, because a store that reindexes itself surprises its operator.
// What changed is the evidence that no configuration prevents it, and that the cadence the
// alternative needs is a daily job nobody runs. An operator now chooses it, and it is still never on
// a timer: an index is rebuilt only when its own reading says it is mostly air.

// IndexRebuildPolicy is when an index is worth rebuilding: large enough that the space matters, and
// costing far more per entry than an entry of any index this store creates.
//
// Both bounds are in the units the measurement is already published in (hippocampus.index_bytes and
// hippocampus.index_entries), so the policy and HippocampusIndexBloated can be set against each other
// by reading the same two series.
type IndexRebuildPolicy struct {
	// MinBytes exempts small indexes. A nearly empty B-tree is a metapage and a few leaves whatever it
	// holds, so its bytes per entry is large without being bloat, and what a rebuild returns there
	// is not worth a pass.
	MinBytes int64

	// MaxBytesPerEntry is the cost per entry past which an index is rebuilt. The widest key here is
	// a 37-character id and seven numbers; packed, no index this store creates costs much over a
	// hundred bytes an entry.
	MaxBytesPerEntry int64

	// Skip names indexes this pass must leave alone, keyed "table.index". The caller fills it with
	// the indexes whose last rebuild failed, so that one an operator's role cannot rebuild is retried
	// on the caller's schedule rather than every cycle.
	Skip map[string]bool
}

// wants reports whether this policy rebuilds an index.
//
// An empty index counts as one entry rather than being skipped. That is the queue case: a drained
// outbox has no entries and hundreds of megabytes of index, which is the most bloated an index can
// be, and dividing by its entry count would exempt it exactly when it is worst.
//
// The content index is never rebuilt. It is a tsvector under GIN, whose entries grow with the text
// they index, so its cost per entry is a property of the stored bodies rather than a number with a
// right answer - the reason HippocampusIndexBloated excludes it too.
func (p IndexRebuildPolicy) wants(index IndexFootprint) bool {
	if index.Table == contentSearchTable || p.Skip[index.Table+"."+index.Index] {

		return false
	}

	if index.Bytes < p.MinBytes {

		return false
	}

	return index.Bytes/max(index.Entries, 1) > p.MaxBytesPerEntry
}

// IndexRebuild is what one rebuild did. Err is set where it failed, and then BytesAfter is 0.
type IndexRebuild struct {
	Table       string
	Index       string
	Entries     int64
	BytesBefore int64
	BytesAfter  int64
	Duration    time.Duration
	Err         error
}

// rebuildTables are the tables a pass looks over: the ones the capacity target counts, and the three
// it deliberately does not, where the store has them.
//
// The excluded three are here because they are the queues. footprint.go leaves them out of its
// reading on purpose - it is compared against UsedBytes - but they bloat by the second mechanism
// above, and the worst index the demo carried was the outbox's.
func (d *DB) rebuildTables() []string {
	tables := d.countedTables()

	for _, probe := range []ancillaryProbe{d.tombstoneProbe(), d.outboxProbe(), d.callbackProbe()} {
		if !probe.exists {
			continue
		}

		tables = append(tables, probe.table)
	}

	return tables
}

// RebuildBloatedIndexes rebuilds, online, every index the policy says is mostly air, and reports
// each rebuild it attempted.
//
// A dialect with no online rebuild returns nothing. SQLite has no bloat to return - a freed page
// goes to the freelist that Preserve already hands back - and MySQL keeps its B-trees dense, holding
// what forgetting freed as tablespace free space it reuses instead.
//
// The returned error is for the measurement only. A rebuild that fails is reported in its own entry
// and the pass moves on to the next index, since one index an operator's role cannot rebuild should
// not keep the others at their bloat.
//
// Each rebuild runs under the store's query timeout. One that is cut short leaves its transient copy
// behind as an invalid index, which the next pass drops before it measures - see
// dropAbandonedRebuilds.
func (d *DB) RebuildBloatedIndexes(ctx context.Context, policy IndexRebuildPolicy) ([]IndexRebuild, error) {
	log.Trace("func() db.RebuildBloatedIndexes")

	if d.dialect().rebuildIndex == "" || d.readOnly {

		return nil, nil
	}

	sizes := d.catalogue()
	defer sizes.close()

	var out []IndexRebuild

	for _, table := range d.rebuildTables() {
		if err := d.dropAbandonedRebuilds(ctx, table); err != nil {

			return out, err
		}

		measureCtx, cancel := d.opContext(ctx)
		indexes, err := d.indexFootprints(measureCtx, sizes, table)
		cancel()

		if err != nil {

			return out, err
		}

		for _, index := range indexes {
			if !policy.wants(index) {
				continue
			}

			out = append(out, d.rebuildIndex(ctx, sizes, index))
		}
	}

	return out, nil
}

// rebuildIndex rebuilds one index and measures what it left.
func (d *DB) rebuildIndex(ctx context.Context, sizes *catalogue, index IndexFootprint) IndexRebuild {
	out := IndexRebuild{
		Table:       index.Table,
		Index:       index.Index,
		Entries:     index.Entries,
		BytesBefore: index.Bytes,
	}

	started := time.Now()

	if _, err := d.exec(ctx, fmt.Sprintf(d.dialect().rebuildIndex, quoteIdentifier(index.Index))); err != nil {
		out.Duration = time.Since(started)
		out.Err = fmt.Errorf("rebuilding %s on %s: %w", index.Index, index.Table, err)

		return out
	}

	out.Duration = time.Since(started)

	measureCtx, cancel := d.opContext(ctx)
	defer cancel()

	after, err := sizes.relationBytes(measureCtx, index.Index)
	if err != nil {
		log.Warnf("rebuilt %s on %s but could not measure it afterwards: %s", index.Index, index.Table, err.Error())

		return out
	}

	out.BytesAfter = after

	return out
}

// dropAbandonedRebuilds drops the invalid copies an interrupted rebuild left over one table.
//
// REINDEX CONCURRENTLY builds a new index beside the old one and swaps them; cut short, it leaves
// the new one behind, invalid and suffixed _ccnew. The planner never uses it, but every write still
// maintains it, so an interruption left in place is a permanent cost on the write path - and the
// next rebuild of the same index would leave a second one beside it. PostgreSQL's own advice is to
// drop it and rebuild again, which is this followed by the pass.
func (d *DB) dropAbandonedRebuilds(ctx context.Context, table string) error {
	names, err := d.abandonedRebuilds(ctx, table)
	if err != nil {

		return err
	}

	for _, name := range names {
		if _, err := d.exec(ctx, fmt.Sprintf(d.dialect().dropIndexOnline, quoteIdentifier(name))); err != nil {

			return fmt.Errorf("dropping the abandoned index rebuild %s on %s: %w", name, table, err)
		}

		log.Infof("dropped %s, an index rebuild on %s that was interrupted before it finished", name, table)
	}

	return nil
}

// abandonedRebuilds lists the invalid copies an interrupted rebuild left over one table.
func (d *DB) abandonedRebuilds(ctx context.Context, table string) ([]string, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, d.dialect().abandonedRebuilds, table)
	if err != nil {

		return nil, fmt.Errorf("listing abandoned index rebuilds on %s: %w", table, err)
	}

	defer func() { _ = rows.Close() }()

	var names []string

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {

			return nil, fmt.Errorf("listing abandoned index rebuilds on %s: %w", table, err)
		}

		names = append(names, name)
	}

	if err := rows.Err(); err != nil {

		return nil, fmt.Errorf("listing abandoned index rebuilds on %s: %w", table, err)
	}

	return names, nil
}

// quoteIdentifier quotes a name for a statement that cannot take it as a bind parameter. The names
// come from the catalogue, not from a caller, but an operator's own index may be named anything.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
