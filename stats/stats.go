package stats

import (
	"context"
	"sync"
	"time"

	humanise "github.com/dustin/go-humanize"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/db"
)

const scopeName = "github.com/fastbean-au/hippocampus/stats"

// defaultCacheMaxAge bounds how stale the shared count reading may be when the stats log line is
// disabled (intervalSeconds <= 0); when the log line is enabled its interval drives the max-age
// instead.
const defaultCacheMaxAge = 5 * time.Minute

// counts is one reading of the store's row counts. Negative values carry through the
// CountEvents/CountMemories negative-on-error contract, so consumers skip reporting them.
type counts struct {
	events          int
	memoriesWith    int
	memoriesWithout int
}

// countCache shares a single reading of the (expensive) full-table counts between the periodic log
// ticker and the metric gauge callback, refreshing from the store at most once per maxAge. Without
// it the same two COUNT queries ran on the 5-minute ticker *and* on every metric export (default
// 60 s), so a metrics-enabled instance did up to six full scans per five minutes.
type countCache struct {
	store  db.Store
	maxAge time.Duration

	mu      sync.Mutex
	cached  counts
	fetched time.Time
	valid   bool
}

func newCountCache(store db.Store, maxAge time.Duration) *countCache {
	return &countCache{store: store, maxAge: maxAge}
}

// get returns the cached counts, querying the store only when the cache is empty or older than
// maxAge. The lock is held across the refresh so concurrent callers within the window collapse onto
// a single read rather than each launching their own.
func (c *countCache) get() counts {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.valid && time.Since(c.fetched) < c.maxAge {
		return c.cached
	}

	// The count cache is driven by an internal ticker and the metric-export callbacks, neither of
	// which carries a request context; a background context still gets the db layer's own
	// storage.queryTimeout bound applied inside each method.
	ctx := context.Background()

	events := c.store.CountEvents(ctx)
	with, without := c.store.CountMemories(ctx)

	c.cached = counts{events: events, memoriesWith: with, memoriesWithout: without}
	c.fetched = time.Now()
	c.valid = true

	return c.cached
}

// Start registers the observable count gauges and, when intervalSeconds > 0, launches the periodic
// stats-logging ticker. Both read through a shared countCache so the underlying COUNT queries run
// at most once per interval regardless of how often metrics are exported. It returns a stop
// function that halts the ticker goroutine, so its queries never outlive the database (the caller
// must invoke it before closing the DB). The observable gauges are driven by the metric reader, not
// this ticker, and are stopped separately when observability shuts down.
func Start(store db.Store, intervalSeconds int) (stop func()) {
	log.Trace("func() stats.Start()")

	maxAge := time.Duration(intervalSeconds) * time.Second
	if maxAge <= 0 {
		maxAge = defaultCacheMaxAge
	}

	cache := newCountCache(store, maxAge)

	registerMetrics(cache)

	// A non-positive interval disables the periodic log line (a supported quiet mode); the gauges
	// still work off the cache above.
	if intervalSeconds <= 0 {
		return func() {}
	}

	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	done := make(chan struct{})

	go func() {
		for {
			select {

			case <-done:
				ticker.Stop()

				return

			case <-ticker.C:
				c := cache.get()
				log.Infof("Stats: %s events with %s memories, %s memories without events", humanise.Comma(int64(c.events)), humanise.Comma(int64(c.memoriesWith)), humanise.Comma(int64(c.memoriesWithout)))
			}
		}
	}()

	return func() { close(done) }
}

// registerMetrics registers observable gauges for the current event and memory counts, served from
// the shared cache. The callback runs on each metric collection, so the counts are only queried
// when metrics are enabled and being exported; with metrics disabled the global no-op meter never
// invokes it.
func registerMetrics(cache *countCache) {
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
		c := cache.get()

		// The DB count helpers return negative values on error; skip observing rather than
		// reporting a bogus count.
		if c.events >= 0 {
			o.ObserveInt64(eventCount, int64(c.events))
		}

		if c.memoriesWith >= 0 && c.memoriesWithout >= 0 {
			o.ObserveInt64(memoryCount, int64(c.memoriesWith), metric.WithAttributes(attribute.Bool("has_event", true)))
			o.ObserveInt64(memoryCount, int64(c.memoriesWithout), metric.WithAttributes(attribute.Bool("has_event", false)))
		}

		return nil
	}

	if _, err := meter.RegisterCallback(callback, eventCount, memoryCount); err != nil {
		log.Errorf("failed to register stats metrics callback: %s", err.Error())
	}
}
