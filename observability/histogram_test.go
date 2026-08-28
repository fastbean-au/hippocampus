package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// quantileFromBuckets is the linear interpolation Prometheus' histogram_quantile performs: find the
// bucket the rank falls in and interpolate across it. Reproduced here rather than asserted against
// because the whole point of the boundaries is what this function does with them, and the failure
// this guards was invisible until somebody computed a quantile.
func quantileFromBuckets(t *testing.T, histogram metricdata.HistogramDataPoint[float64], quantile float64) float64 {
	t.Helper()

	if histogram.Count == 0 {
		t.Fatal("no observations recorded")
	}

	rank := quantile * float64(histogram.Count)

	var cumulative float64

	lower := 0.0

	for i, count := range histogram.BucketCounts {
		cumulative += float64(count)

		if cumulative < rank {
			if i < len(histogram.Bounds) {
				lower = histogram.Bounds[i]
			}

			continue
		}

		// The +Inf bucket has no upper bound to interpolate towards; Prometheus returns the last
		// finite boundary there, and so do we.
		if i >= len(histogram.Bounds) {
			return lower
		}

		upper := histogram.Bounds[i]
		inBucket := float64(count)

		if inBucket == 0 {
			return upper
		}

		return lower + (upper-lower)*((rank-(cumulative-inBucket))/inBucket)
	}

	return lower
}

// collectHistogram records the given observations into a Float64Histogram built with the given
// options and returns the resulting data point.
func collectHistogram(t *testing.T, options []metric.Float64HistogramOption, observations []float64) metricdata.HistogramDataPoint[float64] {
	t.Helper()

	restore := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(restore) })

	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	histogram, err := otel.Meter("test").Float64Histogram("test.duration", options...)
	if err != nil {
		t.Fatalf("Float64Histogram: %s", err)
	}

	for _, observation := range observations {
		histogram.Record(context.Background(), observation)
	}

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

			return data.DataPoints[0]
		}
	}

	t.Fatal("no histogram was collected")

	return metricdata.HistogramDataPoint[float64]{}
}

// TestDefaultBucketsCannotResolveSubSecondLatency is the regression this whole file exists for, and
// it asserts the BROKEN behaviour deliberately - a defect that has already shipped once is worth
// being able to point at.
//
// The SDK's default boundaries are millisecond-shaped (0, 5, 10, 25, ... 10000). Recording SECONDS
// against them puts every realistic observation in the single 0-5s bucket, and the interpolated p95
// comes back near 4.75 seconds however fast the service actually is. That is what made the shipped
// HippocampusHighLatency alert (p95 > 1s) fire permanently against a service whose real mean RPC
// duration was 0.44 milliseconds.
func TestDefaultBucketsCannotResolveSubSecondLatency(t *testing.T) {
	observations := make([]float64, 0, 200)
	for i := 0; i < 200; i++ {
		observations = append(observations, 0.001)
	}

	point := collectHistogram(t, nil, observations)
	p95 := quantileFromBuckets(t, point, 0.95)

	if p95 < 4.0 {
		t.Fatalf("the SDK defaults appear to have changed: p95 of 1ms observations came out at %.3fs, "+
			"where the millisecond-shaped defaults give ~4.75s. If the defaults are now seconds-shaped, "+
			"the explicit boundaries in this package may no longer be needed.", p95)
	}
}

// TestLatencyBucketsResolveSubMillisecondLatency is the fix: the same observations, through the
// boundaries this package declares, must produce a quantile that is actually about the data.
func TestLatencyBucketsResolveSubMillisecondLatency(t *testing.T) {
	observations := make([]float64, 0, 200)
	for i := 0; i < 190; i++ {
		observations = append(observations, 0.0008)
	}

	for i := 0; i < 10; i++ {
		observations = append(observations, 0.02)
	}

	point := collectHistogram(t, []metric.Float64HistogramOption{LatencyBuckets()}, observations)
	p95 := quantileFromBuckets(t, point, 0.95)

	// 95% of the observations are under a millisecond, so p95 must land in the low milliseconds -
	// and above all must be nowhere near the 1s threshold the shipped latency alert uses.
	if p95 > 0.05 {
		t.Errorf("p95 came out at %.4fs for a distribution that is 95%% sub-millisecond", p95)
	}

	if p95 <= 0 {
		t.Errorf("p95 came out at %.4fs, which is not a plausible latency", p95)
	}
}

// TestCycleBucketsResolveASleepCycle covers the other ladder: a sleep cycle or an ingestor pass,
// which runs for tens of milliseconds on a small store and can run for minutes on a large one.
func TestCycleBucketsResolveASleepCycle(t *testing.T) {
	observations := make([]float64, 0, 100)
	for i := 0; i < 95; i++ {
		observations = append(observations, 0.03)
	}

	for i := 0; i < 5; i++ {
		observations = append(observations, 45.0)
	}

	point := collectHistogram(t, []metric.Float64HistogramOption{CycleBuckets()}, observations)

	if p50 := quantileFromBuckets(t, point, 0.50); p50 > 0.1 {
		t.Errorf("p50 came out at %.3fs for cycles that mostly took 30ms", p50)
	}

	// The long tail has to remain visible: a ladder that stopped at ten seconds would put the slow
	// cycles in +Inf and report the same figure whether they took a minute or an hour.
	if p99 := quantileFromBuckets(t, point, 0.99); p99 < 10 {
		t.Errorf("p99 came out at %.3fs; the 45s cycles should be resolvable, not lumped into the tail", p99)
	}
}

// TestBucketBoundariesAreOrdered guards the one way a boundary list can be silently wrong: the OTel
// SDK requires strictly increasing boundaries and quietly produces nonsense otherwise.
func TestBucketBoundariesAreOrdered(t *testing.T) {
	for name, boundaries := range map[string][]float64{
		"LatencyBucketBoundaries": LatencyBucketBoundaries,
		"CycleBucketBoundaries":   CycleBucketBoundaries,
	} {
		if len(boundaries) == 0 {
			t.Errorf("%s is empty", name)

			continue
		}

		if boundaries[0] <= 0 {
			t.Errorf("%s starts at %v; a duration boundary must be positive", name, boundaries[0])
		}

		for i := 1; i < len(boundaries); i++ {
			if boundaries[i] <= boundaries[i-1] {
				t.Errorf("%s is not strictly increasing at index %d (%v then %v)",
					name, i, boundaries[i-1], boundaries[i])
			}
		}
	}
}
