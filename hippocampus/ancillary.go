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

// What the capacity target cannot see, made visible.
//
// The forgotten log, the search outbox and the callback queue are excluded from UsedBytes for
// reasons db/ancillary.go sets out, and the exclusion stays. What did not exist was any way to see
// how large the excluded part had grown - and on the embedded deployment those three tables share
// the store's own file, so a receiver that is down while a large cycle runs grows the disk while
// capacity pressure stays exactly where it was. The one number an operator is told to watch reads
// green while the resource it exists to protect is going.
//
// This reports it and decides nothing. Nothing here caps, trims or evicts on the figure - each of
// the three tables trims itself, against its own bounds, in the storage layer. What this adds is
// that the bound travels WITH the figure (limit_bytes), because a byte count with nothing to read it
// against is most of the problem this file exists about: an operator who can see 400 MB and not the
// cap it is approaching cannot tell a queue that is filling from one that is about to start
// discarding deliveries.
//
// Two decisions carry the shape.
//
// It is measured ONCE PER SLEEP CYCLE and cached, not measured per request. A count of the callback
// queue is a scan of up to callbacks.maxRows rows on the server dialects, and GetConsolidationStatus
// is an RPC a console polls - which is item 25.9's lesson, and the same reason ExplainConsolidation
// caches its snapshot. The queues move when a cycle runs and when a drain worker catches up, not
// second by second.
//
// It is reported PER TABLE rather than as one total, because the three have different failure
// modes and only the split says which is happening. The two fixed-width tables have row caps that
// are effectively byte caps; the callback queue's rows carry a rendered payload and, under
// callbacks.includeBodies, memory bodies - so it is the one whose bytes its row cap barely bounds.

// describeByteCap renders a byte cap for a log line, naming the unbounded case rather than printing
// a 0 that reads as "zero bytes allowed" - which is the opposite of what it means.
func describeByteCap(bytes int64) string {
	if bytes <= 0 {

		return "no byte cap"
	}

	return fmt.Sprintf("%d bytes", bytes)
}

// ancillarySnapshot is one measurement of the excluded tables, with when it was taken.
//
// Held as an atomic.Pointer for the reason lastCycle is: written once per cycle by the sleep
// goroutine, read on every status poll from each caller's own goroutine, and immutable after
// publication - so a reader never blocks the cycle and never sees a half-filled measurement.
//
// In memory only, and so absent until a cycle has run in this process. A replica never measures it
// at all, which is correct rather than a gap: the tables belong to the instance that consolidates,
// and a replica reporting them would be describing work it does not do.
type ancillarySnapshot struct {
	measuredAt time.Time
	storage    db.AncillaryStorage
	limits     ancillaryLimits
}

// ancillaryLimits is the byte cap in force on each of the three tables when a measurement was taken,
// captured beside it rather than read at render time so a figure and its bound are always the pair
// that were true together.
//
// Zero means unbounded, which is what a deployment has until an operator sets one; see
// AncillaryTable.limit_bytes in the contract for why there is no default.
type ancillaryLimits struct {
	forgottenLog  int64
	searchOutbox  int64
	callbackQueue int64
}

// ancillaryLimits reads the three caps off the same fields the prune paths are given, so what is
// reported and what is enforced cannot be two different numbers. The forgotten log's is mirrored
// from configuration rather than from the storage layer's policy for the reason the enabled flag
// beside it is (see consolidationConfig.tombstoneMaxBytes).
func (s *Server) ancillaryLimits() ancillaryLimits {
	return ancillaryLimits{
		forgottenLog:  s.consolidation.tombstoneMaxBytes,
		searchOutbox:  s.outboxBounds.MaxBytes,
		callbackQueue: s.callbackBounds.MaxBytes,
	}
}

// recordAncillaryStorage measures the three excluded tables, publishes the gauge and caches the
// result for GetConsolidationStatus.
//
// Best-effort in the strongest sense: it runs after everything the cycle exists to do, and a
// failure leaves the previous measurement standing rather than replacing it with a zero. A stale
// figure with an older measured_at is a reading an operator can interpret; a fresh zero is one that
// says the queues are empty when nobody knows whether they are.
func (s *Server) recordAncillaryStorage(ctx context.Context) {
	log.Debug("recordAncillaryStorage()")

	ctx, span := tel.tracer.Start(ctx, "record_ancillary_storage")
	defer span.End()

	storage, err := s.db.AncillaryStorage(ctx)
	if err != nil {
		log.Warnf("failed to measure the storage outside the capacity target: %s", err.Error())
		span.RecordError(err)

		return
	}

	s.lastAncillary.Store(&ancillarySnapshot{
		measuredAt: time.Now(),
		storage:    storage,
		limits:     s.ancillaryLimits(),
	})

	// A disabled table publishes no series at all, on the reasoning the external capacity axis
	// follows: a flat zero reads as a queue that is keeping up rather than as a feature nobody
	// turned on, and the sum over the three is then a sum of what the deployment actually spends.
	recordAncillaryTable(ctx, "forgotten_log", storage.ForgottenLog)
	recordAncillaryTable(ctx, "search_outbox", storage.SearchOutbox)
	recordAncillaryTable(ctx, "callback_queue", storage.CallbackQueue)

	span.SetAttributes(attribute.Int64("ancillary_bytes", storage.TotalBytes()))
}

func recordAncillaryTable(ctx context.Context, component string, table db.AncillaryTable) {
	if !table.Enabled {
		return
	}

	tel.ancillaryBytes.Record(ctx, table.Bytes, metric.WithAttributes(attribute.String("component", component)))
}

// ancillaryToProto projects the cached measurement onto the wire, separate from the handler for the
// reason cycleReportToProto is: a test can assert the mapping without standing up a server.
func ancillaryToProto(in *ancillarySnapshot) *contract.AncillaryStorage {
	return &contract.AncillaryStorage{
		MeasuredAt:    in.measuredAt.UnixNano(),
		TotalBytes:    in.storage.TotalBytes(),
		ForgottenLog:  ancillaryTableToProto(in.storage.ForgottenLog, in.limits.forgottenLog),
		SearchOutbox:  ancillaryTableToProto(in.storage.SearchOutbox, in.limits.searchOutbox),
		CallbackQueue: ancillaryTableToProto(in.storage.CallbackQueue, in.limits.callbackQueue),
	}
}

func ancillaryTableToProto(in db.AncillaryTable, limitBytes int64) *contract.AncillaryTable {
	return &contract.AncillaryTable{
		Enabled:    in.Enabled,
		Rows:       in.Rows,
		Bytes:      in.Bytes,
		LimitBytes: limitBytes,
	}
}
