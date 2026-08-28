package hippocampus

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fastbean-au/hippocampus/observability"
)

// TestSleepDurationUsesCycleBuckets is the sleep cycle's half of the coupling that
// cmd/hippocampus/rpcbuckets_test.go documents: hippocampus.sleep.duration records seconds, and
// against the SDK's millisecond-shaped defaults every quantile of it was an artefact of the
// bucket boundaries rather than a measurement.
//
// It matters here for a different reason than it does for the RPC histogram. No alert reads this
// one, so nothing would page; what reads it is the soak harness's sleep-degradation check
// (demo/soak/report.py), which exists to answer the question item 20 was re-scoped around - whether
// a cycle that has grown from one scan to roughly six degrades over hours. A quantile pinned to a
// bucket edge answers "no" every time, which is the worst possible failure for that check.
func TestSleepDurationUsesCycleBuckets(t *testing.T) {
	restore := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(restore) })

	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	// Built fresh against the provider installed above; the package-level `tel` was constructed at
	// init against the no-op provider and would collect nothing.
	local := newTelemetry()
	local.sleepDuration.Record(context.Background(), 0.03)

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %s", err)
	}

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "hippocampus.sleep.duration" {
				continue
			}

			data, ok := m.Data.(metricdata.Histogram[float64])
			if !ok || len(data.DataPoints) == 0 {
				t.Fatalf("hippocampus.sleep.duration collected as %T", m.Data)
			}

			bounds := data.DataPoints[0].Bounds

			if len(bounds) != len(observability.CycleBucketBoundaries) {
				t.Fatalf("hippocampus.sleep.duration has %d bucket boundaries, want %d - the explicit "+
					"boundaries look to have been dropped, and every quantile of this metric becomes an "+
					"artefact of the SDK's millisecond-shaped defaults",
					len(bounds), len(observability.CycleBucketBoundaries))
			}

			for i := range observability.CycleBucketBoundaries {
				if bounds[i] != observability.CycleBucketBoundaries[i] {
					t.Fatalf("boundary %d is %v, want %v", i, bounds[i], observability.CycleBucketBoundaries[i])
				}
			}

			return
		}
	}

	t.Fatal("hippocampus.sleep.duration was not collected")
}
