package stats

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fastbean-au/hippocampus/db"
)

// countingStore is a db.Store stub that counts CountEvents/CountMemories calls, so the cache's
// de-duplication can be asserted without a real database. Embedding db.Store means only the two
// count methods need implementing.
type countingStore struct {
	db.Store

	events   int
	memories int
}

func (c *countingStore) CountEvents(ctx context.Context) int {
	c.events++

	return 7
}

func (c *countingStore) CountMemories(ctx context.Context) (int, int) {
	c.memories++

	return 5, 3
}

// fixedStore is a db.Store stub that returns caller-supplied counts, so the metric callback's
// negative-on-error skip contract can be exercised. Embedding db.Store means only the two count
// methods need implementing.
type fixedStore struct {
	db.Store

	events          int
	memoriesWith    int
	memoriesWithout int
}

func (f *fixedStore) CountEvents(ctx context.Context) int {
	return f.events
}

func (f *fixedStore) CountMemories(ctx context.Context) (int, int) {
	return f.memoriesWith, f.memoriesWithout
}

// collectStats installs a fresh SDK meter provider backed by a manual reader, runs registerMetrics
// against the supplied store, and collects one metric reading. It restores the previous global
// provider before returning so tests do not leak state into one another.
func collectStats(t *testing.T, store db.Store) metricdata.ResourceMetrics {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	registerMetrics(newCountCache(store, time.Minute))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %s", err.Error())
	}

	return rm
}

// gaugeValues flattens every int64 gauge data point in a reading into name→sum, summing points
// that share a name (the memory gauge emits one point per has_event attribute). It is enough to
// assert which gauges reported at all and their combined totals.
func gaugeValues(rm metricdata.ResourceMetrics) map[string]int64 {
	out := make(map[string]int64)

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}

			for _, dp := range g.DataPoints {
				out[m.Name] += dp.Value
			}
		}
	}

	return out
}

// TestStart_DisabledIntervalRegistersMetrics verifies a non-positive interval returns a usable
// no-op stop function (the quiet mode) while still registering the count gauges.
func TestStart_DisabledIntervalRegistersMetrics(t *testing.T) {
	store := &countingStore{}

	stop := Start(store, 0)
	if stop == nil {
		t.Fatal("Start returned a nil stop function")
	}

	// The returned stop must be safe to call even though no ticker was launched.
	stop()
}

// TestStart_TickerStops verifies a positive interval launches the logging ticker and that the
// returned stop function halts it without panicking.
func TestStart_TickerStops(t *testing.T) {
	store := &countingStore{}

	stop := Start(store, 1)
	if stop == nil {
		t.Fatal("Start returned a nil stop function")
	}

	stop()
}

// TestRegisterMetrics_ReportsCounts verifies the observable gauges report the store's counts when
// metrics are collected, and that the memory gauge splits into its has_event points (summing to
// with+without).
func TestRegisterMetrics_ReportsCounts(t *testing.T) {
	store := &fixedStore{events: 11, memoriesWith: 5, memoriesWithout: 3}

	values := gaugeValues(collectStats(t, store))

	if got := values["hippocampus.events.count"]; got != 11 {
		t.Fatalf("expected events gauge 11, got %d", got)
	}

	if got := values["hippocampus.memories.count"]; got != 8 {
		t.Fatalf("expected memories gauge total 8 (5+3), got %d", got)
	}
}

// TestRegisterMetrics_SkipsNegativeCounts verifies the callback honours the negative-on-error
// contract: a store reporting negative counts observes nothing rather than a bogus value.
func TestRegisterMetrics_SkipsNegativeCounts(t *testing.T) {
	store := &fixedStore{events: -1, memoriesWith: -1, memoriesWithout: -1}

	values := gaugeValues(collectStats(t, store))

	if _, ok := values["hippocampus.events.count"]; ok {
		t.Fatalf("expected no events gauge point for a negative count, got %d", values["hippocampus.events.count"])
	}

	if _, ok := values["hippocampus.memories.count"]; ok {
		t.Fatalf("expected no memories gauge point for negative counts, got %d", values["hippocampus.memories.count"])
	}
}

// TestCountCache_SharesReadWithinMaxAge verifies two reads inside the max-age window hit the store
// exactly once, and that a read past the window refreshes.
func TestCountCache_SharesReadWithinMaxAge(t *testing.T) {
	store := &countingStore{}
	cache := newCountCache(store, time.Minute)

	first := cache.get()
	if first.events != 7 || first.memoriesWith != 5 || first.memoriesWithout != 3 {
		t.Fatalf("unexpected counts: %+v", first)
	}

	// A second read within the window must reuse the cached value.
	_ = cache.get()

	if store.events != 1 || store.memories != 1 {
		t.Fatalf("expected the store queried once within max-age, got events=%d memories=%d", store.events, store.memories)
	}

	// Expire the cache and read again: the store is queried a second time.
	cache.maxAge = 0

	_ = cache.get()

	if store.events != 2 || store.memories != 2 {
		t.Fatalf("expected a refresh past max-age, got events=%d memories=%d", store.events, store.memories)
	}
}
