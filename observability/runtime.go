package observability

import (
	"context"
	"runtime"
	runtimemetrics "runtime/metrics"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// scopeName names the instrumentation scope these runtime gauges are published under. It is
// distinct from the domain scopes (hippocampus/telemetry.go, stats/stats.go) so that a dashboard
// can tell process health apart from what the process is doing.
const scopeName = "github.com/fastbean-au/hippocampus/observability"

// The runtime/metrics samples read on each collection. Both are cumulative-free reads of the
// runtime's own accounting, costing a stop-the-world-free lookup rather than a ReadMemStats.
//
// heapObjectsMetric is live heap, which is what grows when something is retained that should not
// be. totalMetric is every byte the process has mapped from the operating system, which is the
// closest a Go program can portably get to its own RSS - there is no OS-independent way to read
// the real figure, and the alternatives are a cgo call on macOS or /proc on Linux, neither of
// which belongs in a package five binaries share.
const (
	heapObjectsMetric = "/memory/classes/heap/objects:bytes"
	totalMetric       = "/memory/classes/total:bytes"
)

// registerRuntimeMetrics publishes the process-health gauges every binary in this repo shares:
// goroutine count and two memory figures. They carry NO attributes at all, which keeps them
// trivially low-cardinality, and they are observable gauges so the runtime is only sampled when
// metrics are enabled and being collected.
//
// Goroutines is the one that earns its place. A goroutine leak is the failure a long-running
// deployment actually suffers and the only one that a clean log will never show: the service goes
// on answering correctly while its memory climbs, and nothing in the domain metrics moves. It is
// also what item 20's soak runs are required to capture, and there was previously no way to read
// it from outside the process at all.
func registerRuntimeMetrics() {
	log.Trace("func() observability.registerRuntimeMetrics()")

	meter := otel.Meter(scopeName)

	goroutines, err := meter.Int64ObservableGauge("hippocampus.runtime.goroutines",
		metric.WithDescription("Goroutines currently running. Sustained growth is a leak; it is the failure a clean log will not show."))
	if err != nil {
		log.Errorf("failed to create goroutines gauge: %s", err.Error())

		return
	}

	heapBytes, err := meter.Int64ObservableGauge("hippocampus.runtime.heap_bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes occupied by live heap objects."))
	if err != nil {
		log.Errorf("failed to create heap bytes gauge: %s", err.Error())

		return
	}

	totalBytes, err := meter.Int64ObservableGauge("hippocampus.runtime.memory_bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Bytes mapped from the operating system by the Go runtime; the portable stand-in for resident set size."))
	if err != nil {
		log.Errorf("failed to create memory bytes gauge: %s", err.Error())

		return
	}

	// One sample slice per callback invocation rather than a package-level one, because a meter
	// provider may collect concurrently and runtimemetrics.Read writes into the slice it is given.
	callback := func(ctx context.Context, o metric.Observer) error {
		o.ObserveInt64(goroutines, int64(runtime.NumGoroutine()))

		samples := []runtimemetrics.Sample{
			{Name: heapObjectsMetric},
			{Name: totalMetric},
		}
		runtimemetrics.Read(samples)

		// A metric the running Go version does not publish comes back KindBad, which must not be
		// read as a zero - a gauge reporting no memory at all is worse than a gauge reporting
		// nothing, since a dashboard cannot tell the two apart.
		if samples[0].Value.Kind() == runtimemetrics.KindUint64 {
			o.ObserveInt64(heapBytes, int64(samples[0].Value.Uint64()))
		}

		if samples[1].Value.Kind() == runtimemetrics.KindUint64 {
			o.ObserveInt64(totalBytes, int64(samples[1].Value.Uint64()))
		}

		return nil
	}

	if _, err := meter.RegisterCallback(callback, goroutines, heapBytes, totalBytes); err != nil {
		log.Errorf("failed to register runtime metrics callback: %s", err.Error())
	}
}
