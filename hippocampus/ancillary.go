package hippocampus

import (
	"context"
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
// This reports it and decides nothing. Nothing here caps, trims or evicts on the figure; the two
// queues already trim themselves against their own row caps, and whether those caps should be byte
// caps is a separate question (TODO-2 item 112.3's other half). Turning an invisible growth into a
// visible one is worth doing on its own and first.
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

	s.lastAncillary.Store(&ancillarySnapshot{measuredAt: time.Now(), storage: storage})

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
		ForgottenLog:  ancillaryTableToProto(in.storage.ForgottenLog),
		SearchOutbox:  ancillaryTableToProto(in.storage.SearchOutbox),
		CallbackQueue: ancillaryTableToProto(in.storage.CallbackQueue),
	}
}

func ancillaryTableToProto(in db.AncillaryTable) *contract.AncillaryTable {
	return &contract.AncillaryTable{
		Enabled: in.Enabled,
		Rows:    in.Rows,
		Bytes:   in.Bytes,
	}
}
