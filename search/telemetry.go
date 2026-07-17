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
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		indexed: newInt64Counter(meter, "hippocampus.search.indexed", "Number of memory documents written to the search index."),
		deleted: newInt64Counter(meter, "hippocampus.search.deleted", "Number of delete operations applied to the search index."),
		dropped: newInt64Counter(meter, "hippocampus.search.dropped", "Number of index operations dropped (queue full, or all apply attempts failed)."),
		queries: newInt64Counter(meter, "hippocampus.search.queries", "Number of content-search queries served."),
	}
}

func newInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create counter '%s': %s", name, err.Error())
	}

	return c
}
