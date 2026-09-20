package hippocampus

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// What the capacity target is true about, and what the disk says.
//
// db/footprint.go carries the finding and why the estimate stays an estimate. This is the half that
// publishes it: one measurement per sleep cycle, a gauge per index, and the reading cached for
// GetConsolidationStatus to serve.
//
// It reports and it decides nothing. There is no seam for it to act through and that is deliberate -
// Preserve() is already a no-op on both server dialects, correctly, because autovacuum and InnoDB's
// purge own reclamation there. What neither of them does is REPACK an index, and the remedy that
// would (REINDEX CONCURRENTLY) is a maintenance decision even though it runs online. A memory store
// that reindexes itself on a timer is a store that surprises its operator at the moment it is
// already under pressure.
//
// Three things carry the shape, beyond the storage layer's.
//
// It is published per index, and an index name is a low-cardinality attribute here for a reason
// worth stating rather than assuming: the indexes this store creates are a fixed set named in
// db/significance.go, and the storage layer caps what it will report from any one table so an
// operator's own indexes cannot make the series count grow without bound.
//
// The estimate travels WITH the measurement. The gap is the finding, not either figure alone, and
// used_bytes is published only when a byte capacity target is set - so a deployment with no target
// would otherwise have the measurement and nothing to read it against. This carries the estimate
// into the same snapshot, taken from the same cycle.
//
// A failure leaves the previous measurement standing, exactly as recordAncillaryStorage does and
// for the same reason: a stale reading with an older measured_at can be interpreted, and a fresh
// zero says the store occupies no disk.

// footprintSnapshot is one measurement of what the counted tables really occupy, with when it was
// taken and the store's own estimate of the same tables at that moment.
//
// Held as an atomic.Pointer for the reason lastCycle is: written once per cycle by the sleep
// goroutine, read on every status poll from each caller's own goroutine, immutable after
// publication.
//
// estimated is the cycle's UsedBytes reading, or 0 where it could not be taken. It is the figure
// the comparison is against, and carrying it here rather than leaving a client to pair two gauges
// is what makes the ratio a property of one measurement rather than of two readings that may be
// minutes apart.
type footprintSnapshot struct {
	measuredAt time.Time
	estimated  int64
	footprint  db.StorageFootprint
}

// bloatFactor is what the disk is holding for every byte the store's own accounting believes it
// holds, or 0 where there is no estimate to compare against - which is an absence rather than a
// ratio of 1, and is why this returns the pair's meaning rather than the division alone.
func (f *footprintSnapshot) bloatFactor() float64 {
	if f.estimated <= 0 || !f.footprint.Measured {

		return 0
	}

	return float64(f.footprint.Bytes) / float64(f.estimated)
}

// bloatReportFactor is the ratio at which the cycle says so in the log.
//
// For a deployment with no metrics stack and no console, on recordRetention's precedent. It is set
// where the reading has stopped being the ordinary cost of an index and become the finding: a
// healthy store carries indexes worth a few hundred bytes a row against payloads of a few hundred
// more, so a little over 1 is normal and 2 is unremarkable. The deployment this item came from was
// at 5.8.
const bloatReportFactor = 3.0

// recordStorageFootprint measures what the counted tables really occupy, publishes the gauges and
// caches the result for GetConsolidationStatus.
//
// estimated is the used-bytes reading this cycle already took, passed in rather than taken again:
// it costs a full scan on the server drivers, and a second one for a figure that is only ever
// reported would be item 25.9's lesson unlearned. 0 where the cycle could not take it.
func (s *Server) recordStorageFootprint(ctx context.Context, estimated int64) {
	log.Debug("recordStorageFootprint()")

	ctx, span := tel.tracer.Start(ctx, "record_storage_footprint")
	defer span.End()

	footprint, err := s.db.StorageFootprint(ctx)
	if err != nil {
		log.Warnf("failed to measure what the store really occupies on disk: %s", err.Error())
		span.RecordError(err)

		return
	}

	// A driver that cannot answer publishes nothing at all rather than a measured:false snapshot -
	// the gauges would be a flat zero, which on a dashboard reads as a store occupying no disk, and
	// the response field would say the same thing to a console. Absence is the honest rendering of
	// "this driver cannot see it"; the field's documentation is where a client learns why.
	if !footprint.Measured {

		return
	}

	snapshot := &footprintSnapshot{
		measuredAt: time.Now(),
		estimated:  estimated,
		footprint:  footprint,
	}

	reportBloat(s.lastFootprint.Load(), snapshot)

	s.lastFootprint.Store(snapshot)

	tel.diskBytes.Record(ctx, footprint.Bytes)

	for _, index := range footprint.Indexes() {
		attributes := metric.WithAttributes(
			attribute.String("table", index.Table),
			attribute.String("index", index.Index),
		)

		tel.indexBytes.Record(ctx, index.Bytes, attributes)
		tel.indexEntries.Record(ctx, index.Entries, attributes)
	}

	span.SetAttributes(
		attribute.Int64("disk_bytes", footprint.Bytes),
		attribute.Int64("index_bytes", footprint.IndexBytes()),
		attribute.Int64("estimated_bytes", estimated),
	)
}

// reportBloat says in the log that the disk is holding several times what the store's accounting
// believes it holds, and names the index responsible.
//
// Only when the answer CROSSES the threshold, on reportBindingChanges' reasoning: a cycle runs every
// sleep.periodSeconds and a line per cycle is a line nobody reads. It says it again when the ratio
// falls back, because a reindex having worked is the other half of the same operator's question and
// there is otherwise nothing anywhere that confirms it.
func reportBloat(previous *footprintSnapshot, current *footprintSnapshot) {
	factor := current.bloatFactor()

	if factor == 0 {

		return
	}

	// A first measurement has nothing to have crossed, so it is reported if it is already over -
	// which after a restart is the ordinary case, the condition having taken days to build.
	was := 0.0
	if previous != nil {
		was = previous.bloatFactor()
	}

	if (factor >= bloatReportFactor) == (was >= bloatReportFactor) {

		return
	}

	if factor < bloatReportFactor {
		log.Infof(
			"the store now occupies %d bytes on disk against an estimated %d (%.1fx) - whatever was "+
				"holding the unreclaimed space has been returned",
			current.footprint.Bytes, current.estimated, factor,
		)

		return
	}

	log.Warnf(
		"the store occupies %d bytes on disk against the %d its own accounting counts (%.1fx), of "+
			"which %d is indexes%s - a B-tree page emptied by forgetting is marked reusable and "+
			"never repacked, so this grows with what the store has forgotten. See "+
			"docs/operations.md, \"Index bloat on the server drivers\"",
		current.footprint.Bytes,
		current.estimated,
		factor,
		current.footprint.IndexBytes(),
		describeWorstIndex(current.footprint),
	)
}

// describeWorstIndex names the index costing the most per entry, which is the one to act on and not
// always the largest: the largest index on a large store may be the honest cost of holding it, while
// an entry costing a kilobyte over a 37-byte key is air at any size.
//
// Returns an empty string where nothing can be said - no indexes, or a catalogue that has not
// analysed them - so the line above reads as a complete sentence without it.
func describeWorstIndex(footprint db.StorageFootprint) string {
	var worst db.IndexFootprint

	for _, index := range footprint.Indexes() {
		if index.BytesPerEntry() <= worst.BytesPerEntry() {
			continue
		}

		worst = index
	}

	if worst.BytesPerEntry() == 0 {

		return ""
	}

	return fmt.Sprintf(
		", the largest per entry being %s at %.0f bytes for each of its %d entries",
		worst.Index, worst.BytesPerEntry(), worst.Entries,
	)
}

// footprintToProto projects the cached measurement onto the wire, separate from the handler for the
// reason cycleReportToProto is: a test can assert the mapping without standing up a server.
func footprintToProto(in *footprintSnapshot) *contract.StorageFootprint {
	out := &contract.StorageFootprint{
		Measured:       in.footprint.Measured,
		MeasuredAt:     in.measuredAt.UnixNano(),
		Bytes:          in.footprint.Bytes,
		IndexBytes:     in.footprint.IndexBytes(),
		EstimatedBytes: in.estimated,
	}

	for _, table := range in.footprint.Tables {
		out.Tables = append(out.Tables, tableFootprintToProto(table))
	}

	return out
}

func tableFootprintToProto(in db.TableFootprint) *contract.TableFootprint {
	out := &contract.TableFootprint{
		Table:      in.Table,
		Bytes:      in.Bytes,
		IndexBytes: in.IndexBytes,
	}

	for _, index := range in.Indexes {
		out.Indexes = append(out.Indexes, &contract.IndexFootprint{
			Table:   index.Table,
			Index:   index.Index,
			Bytes:   index.Bytes,
			Entries: index.Entries,
		})
	}

	return out
}
