package stats

import (
	"context"
	"time"

	humanise "github.com/dustin/go-humanize"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/db"
)

const scopeName = "github.com/fastbean-au/hippocampus/stats"

// Start registers the observable count gauges and launches the periodic stats-logging ticker. It
// returns a stop function that halts the ticker goroutine, so its 5-minute queries never outlive
// the database (the caller must invoke it before closing the DB). The observable
// gauges are driven by the metric reader, not this ticker, and are stopped separately when
// observability shuts down.
func Start(db db.Store) (stop func()) {
	log.Trace("func() stats.Start()")

	registerMetrics(db)

	ticker := time.NewTicker(5 * time.Minute)
	done := make(chan struct{})

	go func() {
		for {
			select {

			case <-done:
				ticker.Stop()

				return

			case <-ticker.C:
				e := db.CountEvents()
				em, m := db.CountMemories()
				log.Infof("Stats: %s events with %s memories, %s memories without events", humanise.Comma(int64(e)), humanise.Comma(int64(em)), humanise.Comma(int64(m)))
			}
		}
	}()

	return func() { close(done) }
}

// registerMetrics registers observable gauges for the current event and memory counts. The
// callback runs on each metric collection, so the counts are only queried when metrics are
// enabled and being exported; with metrics disabled the global no-op meter never invokes it.
func registerMetrics(db db.Store) {
	log.Trace("func() stats.registerMetrics()")

	meter := otel.Meter(scopeName)

	eventCount, err := meter.Int64ObservableGauge("hippocampus.events.count",
		metric.WithDescription("Number of events currently stored."))
	if err != nil {
		log.Errorf("failed to create events count gauge: %s", err.Error())

		return
	}

	memoryCount, err := meter.Int64ObservableGauge("hippocampus.memories.count",
		metric.WithDescription("Number of memories currently stored, by whether they are associated with an event."))
	if err != nil {
		log.Errorf("failed to create memories count gauge: %s", err.Error())

		return
	}

	callback := func(ctx context.Context, o metric.Observer) error {

		// The DB count helpers return negative values on error; skip observing rather than
		// reporting a bogus count.
		if e := db.CountEvents(); e >= 0 {
			o.ObserveInt64(eventCount, int64(e))
		}

		withEvents, withoutEvents := db.CountMemories()
		if withEvents >= 0 && withoutEvents >= 0 {
			o.ObserveInt64(memoryCount, int64(withEvents), metric.WithAttributes(attribute.Bool("has_event", true)))
			o.ObserveInt64(memoryCount, int64(withoutEvents), metric.WithAttributes(attribute.Bool("has_event", false)))
		}

		return nil
	}

	if _, err := meter.RegisterCallback(callback, eventCount, memoryCount); err != nil {
		log.Errorf("failed to register stats metrics callback: %s", err.Error())
	}
}
