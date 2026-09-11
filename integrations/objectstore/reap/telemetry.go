package reap

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/observability"
)

const scopeName = "github.com/fastbean-au/hippocampus/integrations/objectstore"

// tel bundles the reaper's instruments, built from the global OTEL providers exactly as the tap's
// and the gateway's are.
var tel = newTelemetry()

// Attribute keys. All three are small closed enums - a key, a bucket and a group are never
// attributes here, being caller-controlled and unbounded.
const (
	attrOutcome = "outcome"
	attrPath    = "path"
	attrKind    = "kind"
)

// The three paths a deletion can arrive by. They are separated because they fail independently and
// for different reasons: the push path stops when a receiver is unreachable, the pull path stops
// when the forgotten log is off, and the sweep stops when the store cannot be asked what it holds.
// A deployment where two of the three are at zero is one delivery outage away from leaking.
const (
	PathCallback = "callback"
	PathCatchUp  = "catchup"
	PathSweep    = "sweep"
)

// Deletion outcomes.
//
// "shadow" is what the default configuration produces: the object was selected for deletion and
// deliberately not deleted. It is a separate value rather than an absence so that a shadow-mode
// deployment produces a number an operator can compare against what the far end's own expiry would
// have dropped, which is the whole point of running in shadow first.
//
// "foreign" and "unmappable" are the two ways an id is not this agent's business: one names another
// bucket, the other cannot be an object reference at all. Neither is an error, and neither is ever
// a deletion.
const (
	OutcomeDeleted    = "deleted"
	OutcomeShadow     = "shadow"
	OutcomeForeign    = "foreign"
	OutcomeUnmappable = "unmappable"
	OutcomeFailed     = "failed"
)

// Delivery outcomes for the callback receiver.
const (
	OutcomeAccepted     = "accepted"
	OutcomeIgnored      = "ignored"
	OutcomeRejected     = "rejected"
	OutcomeUnauthorised = "unauthorised"
)

type telemetry struct {
	deletions  metric.Int64Counter
	deliveries metric.Int64Counter
	examined   metric.Int64Counter
	sweep      metric.Float64Histogram
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		deletions: newInt64Counter(meter, "hippocampus.objectstore.deletions",
			"Objects the reaper acted on, by path (callback/catchup/sweep) and outcome "+
				"(deleted/shadow/foreign/unmappable/failed). 'shadow' is the default configuration "+
				"reporting what it would have deleted."),
		deliveries: newInt64Counter(meter, "hippocampus.objectstore.deliveries",
			"Callback deliveries received, by kind and outcome (accepted/ignored/rejected/unauthorised). "+
				"A rejected delivery is retried by the service's queue; an ignored one is a kind or a "+
				"cause this agent does not act on."),
		examined: newInt64Counter(meter, "hippocampus.objectstore.sweep.examined",
			"Objects enumerated by the reverse sweep. Compare with the deletions it produces: a sweep "+
				"examining millions and deleting none is the push path keeping up, which is the healthy case."),
		sweep: newFloat64Histogram(meter, "hippocampus.objectstore.sweep.duration", "s",
			"Time for one reverse sweep of the bucket, in seconds.",
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
