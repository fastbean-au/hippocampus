package gateway

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/observability"
)

const scopeName = "github.com/fastbean-au/hippocampus/integrations/objectstore"

// tel bundles the gateway's instruments, built from the global OTEL providers exactly as the tap's
// are.
var tel = newTelemetry()

// Attribute keys. Both are small closed enums; a key or a bucket is never an attribute, being
// caller-controlled and unbounded.
const (
	attrOutcome = "outcome"
	attrMode    = "mode"
)

// Request outcomes. They are multi-valued rather than a success bool because the non-successes are
// operationally different: "unmappable" is an object this integration could never manage (a key too
// long to be an id), "not_found" is the bucket saying no, and "failed" is the store or this process
// breaking. Only the last belongs in an error rate.
const (
	OutcomeServed      = "served"
	OutcomeRedirected  = "redirected"
	OutcomeNotFound    = "not_found"
	OutcomeUnmappable  = "unmappable"
	OutcomeUnauthorise = "unauthorised"
	OutcomeFailed      = "failed"
)

type telemetry struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		requests: newInt64Counter(meter, "hippocampus.objectstore.requests",
			"Object reads handled by the gateway, by mode (redirect/proxy) and outcome "+
				"(served/redirected/not_found/unmappable/unauthorised/failed)."),
		duration: newFloat64Histogram(meter, "hippocampus.objectstore.request.duration", "s",
			"Time to serve one object read, in seconds. In redirect mode this is the presign; in proxy "+
				"mode it includes streaming the object.",
			observability.LatencyBuckets()),
	}
}

func newInt64Counter(meter metric.Meter, name string, description string) metric.Int64Counter {
	c, err := meter.Int64Counter(name, metric.WithDescription(description))
	if err != nil {
		log.Errorf("failed to create counter '%s': %s", name, err.Error())
	}

	return c
}

func newFloat64Histogram(
	meter metric.Meter,
	name string,
	unit string,
	description string,
	options ...metric.Float64HistogramOption,
) metric.Float64Histogram {
	options = append([]metric.Float64HistogramOption{
		metric.WithUnit(unit),
		metric.WithDescription(description),
	}, options...)

	h, err := meter.Float64Histogram(name, options...)
	if err != nil {
		log.Errorf("failed to create histogram '%s': %s", name, err.Error())
	}

	return h
}
