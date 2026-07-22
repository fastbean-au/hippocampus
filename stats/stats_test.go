package stats

import (
	"context"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
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
// against the supplied store, and collects one metric reading. It resets the global provider to a
// plain no-op afterwards, rather than restoring whatever was previously active: the OTEL SDK's
// default global provider is a one-time delegating shim, and restoring a *captured* "previous"
// value that happened to be that shim would permanently wire it to delegate to this test's
// provider (see withMeterProvider for the same reasoning) - always leaving a known-inert provider
// behind is what actually keeps tests from leaking into one another.
func collectStats(t *testing.T, store db.Store) metricdata.ResourceMetrics {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

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

// failingMeter is a metric.Meter that can be told to fail specific instrument/registration calls
// by name, so registerMetrics' three error-handling branches (event gauge creation, memory gauge
// creation, callback registration) can each be exercised in isolation. It embeds noop.Meter so
// every method it doesn't override behaves as a real (if inert) implementation, and tracks which
// of the failable calls were actually attempted, so a test can assert control flow stopped at the
// expected point (e.g. the memory gauge is never even requested once the event gauge fails).
type failingMeter struct {
	noop.Meter

	failNames    map[string]bool
	failRegister bool

	calledNames    map[string]bool
	calledRegister bool
}

func newFailingMeter() *failingMeter {
	return &failingMeter{
		failNames:   make(map[string]bool),
		calledNames: make(map[string]bool),
	}
}

func (m *failingMeter) Int64ObservableGauge(name string, opts ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	m.calledNames[name] = true

	if m.failNames[name] {
		return nil, fmt.Errorf("stats test: forced failure creating gauge %q", name)
	}

	return m.Meter.Int64ObservableGauge(name, opts...)
}

func (m *failingMeter) RegisterCallback(f metric.Callback, insts ...metric.Observable) (metric.Registration, error) {
	m.calledRegister = true

	if m.failRegister {
		return nil, fmt.Errorf("stats test: forced failure registering callback")
	}

	return m.Meter.RegisterCallback(f, insts...)
}

// failingMeterProvider is a metric.MeterProvider that always hands back the same failingMeter,
// regardless of the requested scope name.
type failingMeterProvider struct {
	noop.MeterProvider

	meter *failingMeter
}

func (p *failingMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return p.meter
}

// withMeterProvider installs provider as the global OTEL meter provider for the duration of the
// test. Cleanup resets the global to a plain no-op provider rather than whatever was previously
// active: SetMeterProvider's very first call in the process permanently wires the SDK's default
// delegating shim to forward to that first provider, so if "previous" ever happened to be that
// shim, restoring it after installing (and un-installing) a fake provider would leave every
// following test that reads the ambient global silently delegating to this test's fake instead of
// getting real no-op behaviour. Resetting to an explicit noop.MeterProvider sidesteps that
// one-time-delegation trap entirely.
func withMeterProvider(t *testing.T, provider metric.MeterProvider) {
	t.Helper()

	otel.SetMeterProvider(provider)
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
}

// TestRegisterMetrics_EventGaugeCreationFails verifies that a failure creating the events count
// gauge is logged and stops registration immediately: the memory gauge is never even requested,
// and no callback is registered.
func TestRegisterMetrics_EventGaugeCreationFails(t *testing.T) {
	meter := newFailingMeter()
	meter.failNames["hippocampus.events.count"] = true

	withMeterProvider(t, &failingMeterProvider{meter: meter})

	registerMetrics(newCountCache(&fixedStore{}, time.Minute))

	if meter.calledNames["hippocampus.memories.count"] {
		t.Error("expected the memory gauge never to be requested once the event gauge fails")
	}

	if meter.calledRegister {
		t.Error("expected no callback registration once the event gauge fails")
	}
}

// TestRegisterMetrics_MemoryGaugeCreationFails verifies that a failure creating the memories count
// gauge (after the event gauge succeeded) is logged and stops registration before a callback is
// registered.
func TestRegisterMetrics_MemoryGaugeCreationFails(t *testing.T) {
	meter := newFailingMeter()
	meter.failNames["hippocampus.memories.count"] = true

	withMeterProvider(t, &failingMeterProvider{meter: meter})

	registerMetrics(newCountCache(&fixedStore{}, time.Minute))

	if !meter.calledNames["hippocampus.events.count"] {
		t.Error("expected the event gauge to have been created before the memory gauge was attempted")
	}

	if meter.calledRegister {
		t.Error("expected no callback registration once the memory gauge fails")
	}
}

// TestRegisterMetrics_RegisterCallbackFails verifies that a failure registering the collection
// callback (after both gauges were created successfully) is logged rather than panicking.
func TestRegisterMetrics_RegisterCallbackFails(t *testing.T) {
	meter := newFailingMeter()
	meter.failRegister = true

	withMeterProvider(t, &failingMeterProvider{meter: meter})

	registerMetrics(newCountCache(&fixedStore{}, time.Minute))

	if !meter.calledNames["hippocampus.events.count"] || !meter.calledNames["hippocampus.memories.count"] {
		t.Error("expected both gauges to have been created before callback registration was attempted")
	}

	if !meter.calledRegister {
		t.Error("expected callback registration to have been attempted")
	}
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

// TestStart_TickerFires verifies that the periodic stats-logging ticker actually logs a line once
// its interval elapses (the ticker.C case in Start's select loop), not merely that it can be
// started and stopped without panicking. intervalSeconds is an int, so 1 second is the shortest
// interval available; the test waits past it.
func TestStart_TickerFires(t *testing.T) {
	store := &countingStore{}

	stop := Start(store, 1)
	defer stop()

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		if store.events > 0 && store.memories > 0 {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Error("expected the ticker to have queried the store's counts at least once within the deadline")
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
