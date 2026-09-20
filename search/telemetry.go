package search

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/fastbean-au/hippocampus/search"

// tel bundles the instruments used by the search index. It is built from the global OTEL meter
// provider, which delegates to the real provider installed in main when observability is enabled
// and remains a no-op otherwise, so instrumented code paths are always safe to run.
var tel = newTelemetry()

type telemetry struct {
	indexed metric.Int64Counter
	deleted metric.Int64Counter
	dropped metric.Int64Counter
	queries metric.Int64Counter

	queueDepth    metric.Int64Gauge
	queueCapacity metric.Int64Gauge
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		indexed: newInt64Counter(meter, "hippocampus.search.indexed", "Number of memory documents written to the search index."),
		deleted: newInt64Counter(meter, "hippocampus.search.deleted", "Number of delete operations applied to the search index."),
		dropped: newInt64Counter(meter, "hippocampus.search.dropped", "Number of index operations dropped, by operation and reason: a full apply queue, or every apply attempt against the cluster having failed."),
		queries: newInt64Counter(meter, "hippocampus.search.queries", "Number of content-search queries served."),

		// Depth and capacity are the pair opensearch.queueSize is tuned on. Depth alone cannot say
		// whether the next operation will be dropped, and a dashboard that supplies the limit from
		// its own copy of the configuration is wrong from the moment the limit changes - the same
		// reasoning that exports hippocampus.capacity_bytes beside used_bytes.
		queueDepth:    newInt64Gauge(meter, "hippocampus.search.queue_depth", "Index operations queued for the apply worker but not yet applied. Pinned at capacity means the service is writing faster than one worker can propagate, and operations are being dropped."),
		queueCapacity: newInt64Gauge(meter, "hippocampus.search.queue_capacity", "The apply queue's size (opensearch.queueSize), exported so utilisation need not hard-code it."),
	}
}

func newInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create counter '%s': %s", name, err.Error())
	}

	return c
}

func newInt64Gauge(meter metric.Meter, name string, description string) metric.Int64Gauge {
	g, err := meter.Int64Gauge(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create gauge '%s': %s", name, err.Error())
	}

	return g
}
