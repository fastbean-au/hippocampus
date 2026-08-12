package bridge

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/fastbean-au/hippocampus/integrations/eventsource"

// tel bundles the bridge instruments, shared by all four broker adapters. Built from the GLOBAL OTEL
// providers, which delegate to the real ones installed in each cmd's main when observability is
// enabled and stay no-ops otherwise - so every recording site is safe to run unconditionally,
// including from the adapter tests. This mirrors hippocampus/telemetry.go.
var tel = newTelemetry()

// Attribute keys. Both are small closed enums.
//
// There is deliberately NO per-message group attribute. A bridge derives a memory's group from the
// message subject by default (--group-from-subject), so on a wildcard subscription that value is
// unbounded - one metric stream per subject, which is exactly the high-cardinality attribute this
// repo's instrumentation rules forbid. The tenancy dimension is instead a resource attribute set
// once per process (--metrics-group), bounded by how many bridges are deployed.
const (
	attrOutcome = "outcome"
	attrBroker  = "broker"
)

// Message outcomes. They are four-valued rather than a success bool because the three non-failures
// are operationally different: a memory the SERVICE declined for insignificance is the decay model
// working, a message a Transformer chose to yield nothing for was filtered on purpose, and neither
// should share a series with a message that could not be delivered. An SLO on "the bridge is
// broken" has to be able to exclude the first two.
// "orphaned" is a MEMORY outcome only, never a message one: it is one memory refused because the
// event it names is not in the store, which the batch write skips rather than aborting on (see
// storeEach). It is separate from "rejected" because the two say opposite things about the bridge -
// rejected is the decay model declining an insignificant memory, orphaned is a memory that could
// have been stored had its event been opened first, so a non-zero rate is worth looking at.
const (
	OutcomeStored   = "stored"
	OutcomeRejected = "rejected"
	OutcomeFiltered = "filtered"
	OutcomeFailed   = "failed"
	OutcomeOrphaned = "orphaned"
)

// Recall and event outcomes, kept in their own block because they are a DIFFERENT closed enum from
// the message outcomes above and confusing the two would make a dashboard silently wrong.
//
// "missing" is the one worth explaining: an id an engagement stream asked to reinforce that the
// store no longer holds is not a failure and not a filter - it is the decay model having already
// done its job. The ratio of missing to reinforced is the single most informative number a
// reinforcing bridge produces, which is why it gets its own value rather than being folded into
// "filtered".
const (
	OutcomeReinforced = "reinforced"
	OutcomeMissing    = "missing"
	OutcomeCreated    = "created"
	OutcomeExists     = "exists"
)

type telemetry struct {
	messages      metric.Int64Counter
	memories      metric.Int64Counter
	storeDuration metric.Float64Histogram
	bodyBytes     metric.Int64Histogram
	recalls       metric.Int64Counter
	events        metric.Int64Counter
	recallBatch   metric.Int64Histogram
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		messages: newInt64Counter(meter, "hippocampus.bridge.messages",
			"Broker messages handled, by broker and outcome (stored/rejected/filtered/failed)."),
		memories: newInt64Counter(meter, "hippocampus.bridge.memories",
			"Memories written to the service, by broker and outcome (stored/rejected/orphaned). "+
				"One message may yield several."),
		storeDuration: newFloat64Histogram(meter, "hippocampus.bridge.message.duration", "s",
			"Time to transform and store one broker message, in seconds. Per-RPC latency is hippocampus.client.rpc.duration."),
		bodyBytes: newInt64Histogram(meter, "hippocampus.bridge.body_bytes", "",
			"Size in bytes of each memory body a bridge writes, mirroring the service's own memory.body_bytes."),
		recalls: newInt64Counter(meter, "hippocampus.bridge.recalls",
			"Memory ids submitted for reinforcement, by broker and outcome (reinforced/missing/failed). "+
				"'missing' is the engagement stream landing on something the store has already forgotten."),
		events: newInt64Counter(meter, "hippocampus.bridge.events",
			"Events opened by a bridge, by broker and outcome (created/exists/rejected/failed)."),
		recallBatch: newInt64Histogram(meter, "hippocampus.bridge.recall.batch_size", "",
			"Ids per RecallMemories call, so a batching window can be tuned against the RPC rate it saves."),
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
