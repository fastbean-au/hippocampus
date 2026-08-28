package promoter

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/observability"
)

const scopeName = "github.com/fastbean-au/hippocampus/integrations/ingestor"

// tel bundles the ingestor's instruments. It is built from the GLOBAL OTEL providers, which
// delegate to the real ones installed in main when observability is enabled and stay no-ops
// otherwise - so every recording site is safe to run unconditionally, including from tests that
// construct a Promoter directly. This mirrors hippocampus/telemetry.go exactly.
var tel = newTelemetry()

// The attribute keys. Every one of them is low-cardinality by construction: `outcome` and `kind` are
// small closed enums, and `rule` is bounded by the number of rules in the operator's own file.
//
// Notably absent is a per-record group. The tenancy dimension is a RESOURCE attribute set once per
// process (observability.GroupAttribute) rather than read off each event, because an edge may hold
// arbitrarily many group labels and one metric stream per label is exactly the unbounded series
// count this repo's instrumentation rules forbid.
const (
	attrOutcome = "outcome"
	attrRule    = "rule"
	attrKind    = "kind"

	// defaultRuleLabel is what `rule` carries when no rule matched and the default action applied.
	// A literal is used rather than an empty string so the series is legible in a query.
	defaultRuleLabel = "(default)"
)

// Outcome values for the event counter. They are three-valued in the same spirit as the service's
// RED metrics: an event deliberately dropped by a rule is a SUCCESS of the admission gate, and must
// not share a series with one the ingestor failed to handle - otherwise a working rules file that
// drops most of what it sees is indistinguishable from an ingestor that is broken.
const (
	outcomePromoted = "promoted"
	outcomeDropped  = "dropped"
	outcomeSkipped  = "skipped"
	outcomeFailed   = "failed"
)

type telemetry struct {
	events         metric.Int64Counter
	memories       metric.Int64Counter
	orphans        metric.Int64Gauge
	orphansHandled metric.Int64Counter

	ruleErrors   metric.Int64Counter
	passes       metric.Int64Counter
	passDuration metric.Float64Histogram
	sinceLastRun metric.Float64Gauge
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		events: newInt64Counter(meter, "hippocampus.ingestor.events",
			"Events judged, by outcome (promoted/dropped/skipped/failed) and the rule that decided."),
		memories: newInt64Counter(meter, "hippocampus.ingestor.memories",
			"Memories promoted to the target instance, by kind (event/orphan)."),
		orphans: newInt64Gauge(meter, "hippocampus.ingestor.orphans",
			"Memories on the source carrying no event, as seen by the last pass. Rising means writers are not associating memories with events; those memories are never judged."),
		orphansHandled: newInt64Counter(meter, "hippocampus.ingestor.orphans.handled",
			"Event-less memories acted on, by outcome (promoted/dropped)."),
		ruleErrors: newInt64Counter(meter, "hippocampus.ingestor.rule_errors",
			"Rule evaluations that errored, by rule. A rule erroring on every event never matches, and so silently changes what is promoted."),
		passes: newInt64Counter(meter, "hippocampus.ingestor.passes",
			"Passes run, by outcome (ok/failed)."),
		passDuration: newFloat64Histogram(meter, "hippocampus.ingestor.pass.duration", "s",
			"Duration of a full pass in seconds. Per-RPC latency is hippocampus.client.rpc.duration, recorded by the shared client interceptor.",
			observability.CycleBuckets()),
		sinceLastRun: newFloat64Gauge(meter, "hippocampus.ingestor.seconds_since_last_pass",
			"Seconds since the last pass completed. This is the staleness signal: a stalled ingestor looks exactly like a quiet one in every other metric."),
	}
}

func newInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create counter '%s': %s", name, err.Error())
	}

	return c
}

func newFloat64Histogram(meter metric.Meter, name string, unit string, description string, options ...metric.Float64HistogramOption) metric.Float64Histogram {
	options = append([]metric.Float64HistogramOption{metric.WithUnit(unit), metric.WithDescription(description)}, options...)

	h, err := meter.Float64Histogram(name, options...)
	if err != nil {
		log.Errorf("failed to create histogram '%s': %s", name, err.Error())
	}

	return h
}

func newFloat64Gauge(meter metric.Meter, name string, description string) metric.Float64Gauge {
	g, err := meter.Float64Gauge(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create gauge '%s': %s", name, err.Error())
	}

	return g
}

func newInt64Gauge(meter metric.Meter, name string, description string) metric.Int64Gauge {
	g, err := meter.Int64Gauge(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create gauge '%s': %s", name, err.Error())
	}

	return g
}
