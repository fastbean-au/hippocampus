package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectRuntimeMetrics registers the runtime gauges against a manual-reader provider installed
// globally (registerRuntimeMetrics resolves its meter from the global provider, as every other
// instrumentation site in this repo does), collects once, and hands back the scope's metrics.
func collectRuntimeMetrics(t *testing.T) []metricdata.Metrics {
	t.Helper()

	restore := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(restore) })

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(provider)

	registerRuntimeMetrics()

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %s", err)
	}

	for _, scope := range collected.ScopeMetrics {
		if scope.Scope.Name != scopeName {
			continue
		}

		return scope.Metrics
	}

	t.Fatalf("no metrics collected for scope %q", scopeName)

	return nil
}

// TestRuntimeMetricsArePublished pins the three gauges and, more to the point, that each reports a
// PLAUSIBLE value. A gauge that registers and then observes zero is the failure mode worth
// guarding: a process reporting no goroutines and no memory reads as broken instrumentation only
// if somebody notices, and a soak run watching for a leak would see a flat line and conclude
// there was none.
func TestRuntimeMetricsArePublished(t *testing.T) {
	values := map[string]int64{}

	for _, m := range collectRuntimeMetrics(t) {
		gauge, ok := m.Data.(metricdata.Gauge[int64])
		if !ok {
			t.Errorf("%s: expected an int64 gauge, got %T", m.Name, m.Data)

			continue
		}

		if len(gauge.DataPoints) != 1 {
			t.Errorf("%s: expected exactly one data point, got %d", m.Name, len(gauge.DataPoints))

			continue
		}

		// These gauges are deliberately attribute-free; an attribute arriving here is a
		// cardinality decision that should not pass unremarked.
		if attrs := gauge.DataPoints[0].Attributes.Len(); attrs != 0 {
			t.Errorf("%s: expected no attributes, got %d", m.Name, attrs)
		}

		values[m.Name] = gauge.DataPoints[0].Value
	}

	for _, name := range []string{
		"hippocampus.runtime.goroutines",
		"hippocampus.runtime.heap_bytes",
		"hippocampus.runtime.memory_bytes",
	} {
		value, ok := values[name]
		if !ok {
			t.Errorf("%s was not published", name)

			continue
		}

		if value <= 0 {
			t.Errorf("%s reported %d; a running process has a positive value for all three", name, value)
		}
	}

	// Live heap objects are a subset of everything mapped from the OS, so this ordering holds by
	// construction. It is asserted because it is the cheap way to catch the two samples being
	// read back in the wrong order.
	if values["hippocampus.runtime.heap_bytes"] > values["hippocampus.runtime.memory_bytes"] {
		t.Errorf("heap_bytes (%d) exceeds memory_bytes (%d); the runtime/metrics samples look transposed",
			values["hippocampus.runtime.heap_bytes"], values["hippocampus.runtime.memory_bytes"])
	}
}

// TestRuntimeGoroutineGaugeTracksGrowth verifies the goroutine gauge actually moves, which is the
// entire reason it exists. Registering an observable gauge that returns a constant would satisfy
// the test above and be useless for finding a leak.
func TestRuntimeGoroutineGaugeTracksGrowth(t *testing.T) {
	goroutineCount := func(t *testing.T) int64 {
		t.Helper()

		for _, m := range collectRuntimeMetrics(t) {
			if m.Name != "hippocampus.runtime.goroutines" {
				continue
			}

			return m.Data.(metricdata.Gauge[int64]).DataPoints[0].Value
		}

		t.Fatal("the goroutines gauge was not published")

		return 0
	}

	before := goroutineCount(t)

	const extra = 25

	release := make(chan struct{})
	running := make(chan struct{}, extra)

	for i := 0; i < extra; i++ {
		go func() {
			running <- struct{}{}
			<-release
		}()
	}

	for i := 0; i < extra; i++ {
		<-running
	}

	after := goroutineCount(t)
	close(release)

	if after-before < extra {
		t.Errorf("started %d goroutines but the gauge moved from %d to %d", extra, before, after)
	}
}
