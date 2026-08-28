package main

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fastbean-au/hippocampus/observability"
)

// TestRPCDurationUsesLatencyBuckets ties the RPC duration histogram to the explicit boundaries in
// observability, because the shipped HippocampusRequestLatencyHigh alert depends on that coupling
// and fails SILENTLY without it.
//
// The history is the argument for the test. hippocampus.rpc.duration records SECONDS, and the OTel
// SDK's default bucket boundaries are millisecond-shaped, so before those boundaries existed every
// observation landed in the single 0-5s bucket and histogram_quantile interpolated p95 to ~4.75s
// on a service whose real mean was 0.44ms. The alert fires above 1s, so it fired permanently, on
// every deployment with traffic, and nothing about the service getting faster could clear it.
//
// Deleting the LatencyBuckets() option would restore exactly that, and nothing else in the repo
// would notice: the instrument still exists, the alert still parses, the drift guard still matches
// the metric name to an instrument. Only the number changes, and only in production.
func TestRPCDurationUsesLatencyBuckets(t *testing.T) {
	assertHistogramBounds(t, observability.LatencyBucketBoundaries, func() {
		_, duration := newRPCInstruments()
		duration.Record(context.Background(), 0.001)
	})
}

// assertHistogramBounds installs a manual-reader provider, runs record (which must build its
// instrument from the global meter and observe at least once), and checks the collected histogram's
// boundaries.
func assertHistogramBounds(t *testing.T, want []float64, record func()) {
	t.Helper()

	restore := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(restore) })

	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	record()

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %s", err)
	}

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			data, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || len(data.DataPoints) == 0 {
				continue
			}

			bounds := data.DataPoints[0].Bounds

			if len(bounds) != len(want) {
				t.Fatalf("%s has %d bucket boundaries, want %d - the explicit boundaries look to have "+
					"been dropped, which silently breaks the latency alert", m.Name, len(bounds), len(want))
			}

			for i := range want {
				if bounds[i] != want[i] {
					t.Fatalf("%s boundary %d is %v, want %v", m.Name, i, bounds[i], want[i])
				}
			}

			return
		}
	}

	t.Fatal("no histogram was collected")
}
