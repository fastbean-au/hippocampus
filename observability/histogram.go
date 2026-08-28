package observability

import (
	"go.opentelemetry.io/otel/metric"
)

// The explicit bucket boundaries for this repo's duration histograms, in SECONDS.
//
// They exist because the OTel SDK's default boundaries are
//
//	0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000
//
// which are chosen for MILLISECONDS. Every duration histogram here records seconds, so against
// those defaults the first finite bucket is five seconds - coarser than every observation a healthy
// deployment ever makes. All of them land in one bucket, and histogram_quantile then interpolates
// linearly across it, so p95 comes back as 0.95 x 5 = 4.75 seconds whatever the real figure is.
//
// That is not merely imprecise, it is actively wrong in a way that had already shipped: the
// HippocampusHighLatency alert in both rule files fires on p95 > 1s, so it fired permanently on any
// instance serving traffic - measured against a service whose real mean RPC duration was 0.44
// MILLISECONDS - and no amount of the service getting faster could ever have cleared it. Found by
// the soak harness in demo/soak.sh, which needed a trustworthy sleep-cycle quantile of its own.
//
// Both sets are ordinary Prometheus-shaped ladders. What matters about them is only that they
// bracket the range the instrument actually observes, so that a quantile is interpolated across a
// narrow bucket rather than across the whole plausible range.
var (
	// LatencyBucketBoundaries suit a request served in-process: an RPC, a store call, one message
	// handled by a bridge. The bottom of the ladder is deliberately below a millisecond, since that
	// is where this service's RPCs actually sit and a quantile is useless if everything is in the
	// first bucket - which is the whole defect above, one order of magnitude down.
	LatencyBucketBoundaries = []float64{
		0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	}

	// CycleBucketBoundaries suit a periodic pass rather than a request: a sleep cycle, an ingestor
	// pass. They run for milliseconds on a small store and can legitimately run for minutes on a
	// large one, so the ladder reaches ten minutes - a cycle that long is a real condition an
	// operator needs to see the shape of, not an outlier to be lumped into +Inf.
	CycleBucketBoundaries = []float64{
		0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600,
	}
)

// LatencyBuckets returns the histogram option carrying LatencyBucketBoundaries, so a call site
// reads as what it is rather than as a slice reference.
func LatencyBuckets() metric.Float64HistogramOption {
	return metric.WithExplicitBucketBoundaries(LatencyBucketBoundaries...)
}

// CycleBuckets returns the histogram option carrying CycleBucketBoundaries.
func CycleBuckets() metric.Float64HistogramOption {
	return metric.WithExplicitBucketBoundaries(CycleBucketBoundaries...)
}
