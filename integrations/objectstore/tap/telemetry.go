package tap

import (
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const scopeName = "github.com/fastbean-au/hippocampus/integrations/objectstore"

// tel bundles the tap's instruments. Built from the GLOBAL OTEL providers, which delegate to the
// real ones installed in each command's main when observability is enabled and stay no-ops
// otherwise - so every recording site is safe to run unconditionally, including from the tests.
var tel = newTelemetry()

const attrOutcome = "outcome"

// Recall outcomes. "missing" is the one worth explaining, and it is not a failure: an id the tap
// asked to reinforce that the store no longer holds is the decay model having already done its job.
// The ratio of missing to reinforced is the single most informative number this process produces -
// it is the hit rate, and a sustained zero is how a derivation mismatch between the producer and
// this agent shows up. See Tap.report.
const (
	OutcomeReinforced = "reinforced"
	OutcomeMissing    = "missing"
	OutcomeFailed     = "failed"
)

type telemetry struct {
	recalls   metric.Int64Counter
	batchSize metric.Int64Histogram
}

func newTelemetry() *telemetry {
	meter := otel.Meter(scopeName)

	return &telemetry{
		recalls: newInt64Counter(meter, "hippocampus.objectstore.recalls",
			"Memory ids submitted for reinforcement by the object-storage tap, by outcome "+
				"(reinforced/missing/failed). A hit rate of zero means the ids being recalled are not "+
				"the ids the pointer-memories were stored under."),
		batchSize: newInt64Histogram(meter, "hippocampus.objectstore.recall.batch_size", "",
			"Ids per RecallMemories call, so the batching window can be tuned against the RPC rate it saves."),
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
