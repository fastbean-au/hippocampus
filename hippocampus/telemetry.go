package hippocampus

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/fastbean-au/hippocampus/hippocampus"

// tel bundles the tracer and instruments used throughout the service. It is built from the global
// OTEL providers, which delegate to the real providers installed in main when observability is
// enabled and remain no-ops otherwise, so instrumented code paths are always safe to run
// (including from tests that construct a Server directly).
var tel = newTelemetry()

type telemetry struct {
	tracer trace.Tracer

	memoriesStored       metric.Int64Counter
	memoriesRejected     metric.Int64Counter
	memoriesRecalled     metric.Int64Counter
	memoriesDeleted      metric.Int64Counter
	memoriesConsolidated metric.Int64Counter
	memoriesEvicted      metric.Int64Counter
	memoriesSearched     metric.Int64Counter
	memoryBodyBytes      metric.Int64Histogram
	bytesEvicted         metric.Int64Counter

	eventsStored       metric.Int64Counter
	eventsRejected     metric.Int64Counter
	eventsDeleted      metric.Int64Counter
	eventsMerged       metric.Int64Counter
	eventsConsolidated metric.Int64Counter
	eventsEvicted      metric.Int64Counter

	sleeps           metric.Int64Counter
	sleepDuration    metric.Float64Histogram
	capacityPressure metric.Float64Gauge
	usedBytes        metric.Int64Gauge
	capacityBytes    metric.Int64Gauge
	memoriesRetained metric.Int64Gauge
	retainedBytes    metric.Int64Gauge
	purges           metric.Int64Counter

	tombstones        metric.Int64Gauge
	tombstonesDeleted metric.Int64Counter

	summarisationCandidates metric.Int64Gauge
	memoriesSummarised      metric.Int64Counter
	summariesCreated        metric.Int64Counter
	summarisations          metric.Int64Counter

	exports         metric.Int64Counter
	imports         metric.Int64Counter
	transfers       metric.Int64Counter
	recordsExported metric.Int64Counter
	recordsImported metric.Int64Counter
	recordsCleared  metric.Int64Counter
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		tracer: otel.Tracer(scopeName),

		memoriesStored:       newInt64Counter(meter, "hippocampus.memories.stored", "Number of memories accepted and stored."),
		memoriesRejected:     newInt64Counter(meter, "hippocampus.memories.rejected", "Number of memories rejected at storage time."),
		memoriesRecalled:     newInt64Counter(meter, "hippocampus.memories.recalled", "Number of memories recalled (and thereby reinforced)."),
		memoriesDeleted:      newInt64Counter(meter, "hippocampus.memories.deleted", "Number of memories explicitly deleted via RPC."),
		memoriesConsolidated: newInt64Counter(meter, "hippocampus.memories.consolidated", "Number of memories forgotten by the sleep cycle."),
		memoriesEvicted:      newInt64Counter(meter, "hippocampus.memories.evicted", "Number of memories evicted to meet the capacity target."),
		memoriesSearched:     newInt64Counter(meter, "hippocampus.memories.searched", "Number of memories returned by content search, by whether the search reinforced them."),
		memoryBodyBytes:      newInt64Histogram(meter, "hippocampus.memory.body_bytes", "", "Size in bytes of each memory body accepted and stored."),
		bytesEvicted:         newInt64Counter(meter, "hippocampus.bytes.evicted", "Estimated bytes reclaimed by capacity eviction."),

		eventsStored:       newInt64Counter(meter, "hippocampus.events.stored", "Number of events accepted and stored."),
		eventsRejected:     newInt64Counter(meter, "hippocampus.events.rejected", "Number of events rejected at storage time."),
		eventsDeleted:      newInt64Counter(meter, "hippocampus.events.deleted", "Number of events explicitly deleted via RPC."),
		eventsMerged:       newInt64Counter(meter, "hippocampus.events.merged", "Number of event merges performed."),
		eventsConsolidated: newInt64Counter(meter, "hippocampus.events.consolidated", "Number of events forgotten by the sleep cycle."),
		eventsEvicted:      newInt64Counter(meter, "hippocampus.events.evicted", "Number of events deleted because eviction removed their last memory."),

		sleeps:           newInt64Counter(meter, "hippocampus.sleeps", "Number of sleep cycles run."),
		sleepDuration:    newFloat64Histogram(meter, "hippocampus.sleep.duration", "s", "Duration of a full sleep cycle in seconds."),
		capacityPressure: newFloat64Gauge(meter, "hippocampus.capacity_pressure", "Deletion-threshold multiplier derived from store utilisation, recalculated each sleep cycle."),
		usedBytes:        newInt64Gauge(meter, "hippocampus.used_bytes", "Bytes the store occupies excluding free pages, measured each sleep cycle when a capacity target is set."),
		capacityBytes:    newInt64Gauge(meter, "hippocampus.capacity_bytes", "The configured byte capacity target, exported alongside used_bytes so a dashboard need not hard-code the limit."),
		memoriesRetained: newInt64Gauge(meter, "hippocampus.memories.retained", "Memories inside the minimum retention window, and so exempt from both consolidation and eviction."),
		retainedBytes:    newInt64Gauge(meter, "hippocampus.retained_bytes", "Stored bytes held by the minimum retention window. Approaching capacity_bytes means the capacity target has become unreachable, since retention overrides it."),
		purges:           newInt64Counter(meter, "hippocampus.purges", "Number of purges performed."),

		// The forgotten log's own size and turnover. What was forgotten and by which rule is
		// already reported by memories.consolidated and memories.evicted; these two answer the
		// question the log itself raises - is it growing without bound, and is anything trimming it.
		tombstones:        newInt64Gauge(meter, "hippocampus.tombstones", "Records held by the forgotten log, measured each sleep cycle while it is enabled."),
		tombstonesDeleted: newInt64Counter(meter, "hippocampus.tombstones.deleted", "Forgotten-log records removed, by whether the removal was a manual request or the configured caps."),

		summarisationCandidates: newInt64Gauge(meter, "hippocampus.summarisation_candidates", "Number of events identified as summarisation candidates by the most recent sleep cycle."),
		memoriesSummarised:      newInt64Counter(meter, "hippocampus.memories.summarised", "Number of memories replaced by a summary memory (ReplaceMemoriesWithSummary or SummariseMemories)."),
		summariesCreated:        newInt64Counter(meter, "hippocampus.summaries.created", "Number of summary memories created (ReplaceMemoriesWithSummary or SummariseMemories)."),
		summarisations:          newInt64Counter(meter, "hippocampus.summarisations", "Number of embedded-LLM summarisation calls, by success."),

		exports:         newInt64Counter(meter, "hippocampus.exports", "Number of Export runs, by success."),
		imports:         newInt64Counter(meter, "hippocampus.imports", "Number of Import runs, by success."),
		transfers:       newInt64Counter(meter, "hippocampus.transfers", "Number of Transfer runs, by success."),
		recordsExported: newInt64Counter(meter, "hippocampus.records.exported", "Number of events and memories written to archives or transferred, by kind."),
		recordsImported: newInt64Counter(meter, "hippocampus.records.imported", "Number of events and memories ingested by Import/ImportBatch, by kind."),
		recordsCleared:  newInt64Counter(meter, "hippocampus.records.cleared", "Number of events and memories deleted by manifest-scoped clears, by kind."),
	}
}

func newInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create counter '%s': %s", name, err.Error())
	}

	return c
}

func newInt64Histogram(meter metric.Meter, name string, unit string, description string) metric.Int64Histogram {
	h, err := meter.Int64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create histogram '%s': %s", name, err.Error())
	}

	return h
}

func newFloat64Histogram(meter metric.Meter, name string, unit string, description string) metric.Float64Histogram {
	h, err := meter.Float64Histogram(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create histogram '%s': %s", name, err.Error())
	}

	return h
}

func newInt64Gauge(meter metric.Meter, name string, description string) metric.Int64Gauge {
	g, err := meter.Int64Gauge(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create gauge '%s': %s", name, err.Error())
	}

	return g
}

func newFloat64Gauge(meter metric.Meter, name string, description string) metric.Float64Gauge {
	g, err := meter.Float64Gauge(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create gauge '%s': %s", name, err.Error())
	}

	return g
}
