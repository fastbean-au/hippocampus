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
// that the bounds travel WITH the figure, because a byte count with nothing to read it against is
// most of the problem this file exists about: an operator who can see 400 MB and not the cap it is
// approaching cannot tell a queue that is filling from one that is about to start discarding
// deliveries.
//
// Which of those bounds is ACTING travels with it too, and that is a separate problem the same
// reading was silent about. The caps are independent bounds with no precedence between them, so
// which one binds depends on how fast the store is forgetting - a rate nobody can see when choosing
// them. A forgotten log configured for a hundred thousand rows and thirty days, on a store forgetting
// nineteen thousand memories a day, holds five days: both caps enforced, neither violated, and the
// window the operator expressed simply unreachable. See reportBindingChanges.
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
}

// ancillaryBounds are the caps to measure against. The two the service holds are passed down; the
// forgotten log's are not, because the storage layer holds the policy PruneTombstones actually
// applies and a second reading of the same configuration is how a report comes to describe a bound
// nothing enforces.
func (s *Server) ancillaryBounds() db.AncillaryBounds {
	return db.AncillaryBounds{
		SearchOutbox:  s.outboxBounds,
		CallbackQueue: s.callbackBounds,
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

	storage, err := s.db.AncillaryStorage(ctx, s.ancillaryBounds())
	if err != nil {
		log.Warnf("failed to measure the storage outside the capacity target: %s", err.Error())
		span.RecordError(err)

		return
	}

	measuredAt := time.Now()

	reportBindingChanges(s.lastAncillary.Load(), storage, measuredAt)

	s.lastAncillary.Store(&ancillarySnapshot{
		measuredAt: measuredAt,
		storage:    storage,
	})

	// A disabled table publishes no series at all, on the reasoning the external capacity axis
	// follows: a flat zero reads as a queue that is keeping up rather than as a feature nobody
	// turned on, and the sum over the three is then a sum of what the deployment actually spends.
	for _, component := range ancillaryComponents(storage) {
		recordAncillaryTable(ctx, component.name, component.table)
	}

	span.SetAttributes(attribute.Int64("ancillary_bytes", storage.TotalBytes()))
}

// recordAncillaryTable publishes one table's FOOTPRINT rather than its structural size: the gauge
// answers "how much disk does this deployment need beyond its capacity target", and on a server
// dialect the space the engine has not reclaimed is most of the difference between the two.
// HippocampusAncillaryStorageHigh reads it, and an alert that exists to warn must not be reading an
// estimate when a measurement of the same thing is available.
func recordAncillaryTable(ctx context.Context, component string, table db.AncillaryTable) {
	if !table.Enabled {
		return
	}

	tel.ancillaryBytes.Record(
		ctx,
		table.Footprint(),
		metric.WithAttributes(attribute.String("component", component)),
	)
}

// ancillaryComponent pairs one excluded table with the name it is reported under, so the gauge, the
// binding report and the projection below walk one list rather than three.
type ancillaryComponent struct {
	name  string
	table db.AncillaryTable
}

func ancillaryComponents(storage db.AncillaryStorage) []ancillaryComponent {
	return []ancillaryComponent{
		{"forgotten_log", storage.ForgottenLog},
		{"search_outbox", storage.SearchOutbox},
		{"callback_queue", storage.CallbackQueue},
	}
}

// reportBindingChanges says in the log which cap is deciding what each table drops, for a deployment
// with no metrics stack and no console to read it off - and says it only when the answer CHANGES,
// because a cycle runs every sleep.periodSeconds and a line per table per cycle is a line nobody
// reads.
//
// The case it exists for is the one that is silent in every other reading: a table sitting at its
// row cap while an age cap is ALSO configured is holding less history than the operator asked for,
// and nothing is wrong - both caps are enforced, neither is violated, and the window is simply
// unreachable at the rate this store is forgetting. The deployment that produced TODO-2 item 126
// asked for thirty days, was given five, and had nothing anywhere that said so.
//
// That case and the unbounded one are Warn; a table bound by exactly the cap its operator expressed
// is Info, because it is the arrangement working.
func reportBindingChanges(previous *ancillarySnapshot, storage db.AncillaryStorage, measuredAt time.Time) {
	for _, component := range ancillaryComponents(storage) {
		if !component.table.Enabled {
			continue
		}

		if previous != nil && bindingOf(previous.storage, component.name) == component.table.Binding {
			continue
		}

		line := describeBinding(component.name, component.table, measuredAt)

		if unreachableWindow(component.table) || component.table.Binding == db.BindingNone {
			log.Warn(line)

			continue
		}

		log.Info(line)
	}
}

// bindingNames spell the three caps as a line of prose wants them. The wire values are the plural
// nouns the settings are named for ("rows", "bytes"), which read wrongly in front of "cap".
var bindingNames = map[db.AncillaryBinding]string{
	db.BindingRows:  "row",
	db.BindingBytes: "byte",
	db.BindingAge:   "age",
}

// unreachableWindow reports the mismatch: a row or byte cap is what holds this table, and an age cap
// was configured as well, so the window the operator asked for is one this store will never reach.
func unreachableWindow(table db.AncillaryTable) bool {
	return table.LimitAge > 0 && table.Binding != db.BindingAge && table.Binding != db.BindingNone
}

// bindingOf reads one component's binding out of a measurement by name, so the comparison above does
// not need a second switch that can disagree with ancillaryComponents.
func bindingOf(storage db.AncillaryStorage, component string) db.AncillaryBinding {
	for _, candidate := range ancillaryComponents(storage) {
		if candidate.name != component {
			continue
		}

		return candidate.table.Binding
	}

	return db.BindingNone
}

// describeBinding is the line itself: which cap holds this table, what it holds, and - where a row
// or byte cap is what is holding it and an age cap was also asked for - how far short of that window
// it falls.
func describeBinding(component string, table db.AncillaryTable, measuredAt time.Time) string {
	if table.Binding == db.BindingNone {

		return fmt.Sprintf(
			"nothing bounds the %s: %d rows, and nothing will remove them",
			component, table.Rows,
		)
	}

	span := "nothing in it yet"

	if table.Oldest > 0 {
		span = fmt.Sprintf(
			"%.1f days of history",
			measuredAt.Sub(time.Unix(0, table.Oldest)).Hours()/24,
		)
	}

	held := fmt.Sprintf(
		"the %s is held by its %s cap: %d rows, %s",
		component, bindingNames[table.Binding], table.Rows, span,
	)

	if !unreachableWindow(table) {

		return held
	}

	return fmt.Sprintf(
		"%s - short of the %.0f days its age cap asks for, which this store forgets too fast to reach",
		held, table.LimitAge.Hours()/24,
	)
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
		Enabled:         in.Enabled,
		Rows:            in.Rows,
		Bytes:           in.Bytes,
		DiskBytes:       in.DiskBytes,
		OldestAt:        in.Oldest,
		LimitBytes:      in.LimitBytes,
		LimitRows:       in.LimitRows,
		LimitAgeSeconds: int64(in.LimitAge.Seconds()),
		BindingLimit:    string(in.Binding),
	}
}
