package promoter

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// collectMetrics installs a real SDK meter provider with a manual reader, rebuilds the package's
// instruments against it, runs fn, and returns everything recorded.
//
// The instruments are package-level and bound to whatever provider was global when they were built,
// so they have to be rebuilt here - which is exactly what main does in production order
// (observability.Init before the first pass). Restoring both afterwards keeps the other tests on
// the no-op providers they expect.
func collectMetrics(t *testing.T, fn func()) metricdata.ResourceMetrics {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	previousProvider := otel.GetMeterProvider()
	previousTel := tel

	otel.SetMeterProvider(provider)
	tel = newTelemetry()

	t.Cleanup(func() {
		otel.SetMeterProvider(previousProvider)
		tel = previousTel

		_ = provider.Shutdown(context.Background())
	})

	fn()

	var collected metricdata.ResourceMetrics

	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collecting metrics: %s", err)
	}

	return collected
}

// counterValue sums a counter's data points matching every one of the given attributes, and reports
// whether the instrument was found at all - so a renamed metric fails loudly rather than reading as
// a zero.
func counterValue(t *testing.T, collected metricdata.ResourceMetrics, name string, attrs map[string]string) (int64, bool) {
	t.Helper()

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is not an int64 sum, got %T", name, m.Data)
			}

			total := int64(0)

			for _, point := range sum.DataPoints {
				if !hasAttributes(point, attrs) {
					continue
				}

				total += point.Value
			}

			return total, true
		}
	}

	return 0, false
}

func hasAttributes(point metricdata.DataPoint[int64], attrs map[string]string) bool {
	for key, want := range attrs {
		value, ok := point.Attributes.Value(attributeKey(key))
		if !ok || value.AsString() != want {
			return false
		}
	}

	return true
}

// metricNames lists everything recorded, for the "was it emitted at all" assertions.
func metricNames(collected metricdata.ResourceMetrics) map[string]bool {
	out := map[string]bool{}

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			out[m.Name] = true
		}
	}

	return out
}

// TestPassEmitsMetrics drives a whole pass against the real SDK and asserts the instrument names and
// the attribute values. Names and attributes are the part of instrumentation that fails silently:
// nothing breaks, the dashboards are simply empty.
func TestPassEmitsMetrics(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", map[string]string{"severity": "error"}))
	source.putEvent(endedEvent("e2", "discard", nil))
	source.putMemory(memory("m1", "e1", "first", 3))
	source.putMemory(memory("m2", "e1", "second", 7))
	source.putMemory(memory("m3", "e2", "noise", 1))
	source.putMemory(orphan("o1", 0))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{"name":"errors","expr":"event.metadata[?'severity'].orValue('') == 'error'","action":"promote"}]
	}`, Config{Orphans: OrphanIgnore})

	collected := collectMetrics(t, func() {
		if _, err := p.Pass(context.Background()); err != nil {
			t.Fatalf("Pass: %s", err)
		}
	})

	names := metricNames(collected)

	for _, name := range []string{
		"hippocampus.ingestor.events",
		"hippocampus.ingestor.memories",
		"hippocampus.ingestor.passes",
		"hippocampus.ingestor.pass.duration",
		"hippocampus.ingestor.orphans",
	} {
		if !names[name] {
			t.Errorf("expected %s to be recorded; got %v", name, names)
		}
	}

	// The promoted event is attributed to the rule that decided it, which is what makes "which rule
	// is admitting everything" answerable.
	if got, found := counterValue(t, collected, "hippocampus.ingestor.events", map[string]string{
		"outcome": outcomePromoted,
		"rule":    "errors",
	}); !found || got != 1 {
		t.Errorf("expected 1 promotion attributed to the 'errors' rule, got %d (found=%t)", got, found)
	}

	// A rule DROPPING an event is the gate working, so it must not share a series with a failure.
	if got, _ := counterValue(t, collected, "hippocampus.ingestor.events", map[string]string{
		"outcome": outcomeDropped,
		"rule":    defaultRuleLabel,
	}); got != 1 {
		t.Errorf("expected 1 drop attributed to the default action, got %d", got)
	}

	if got, _ := counterValue(t, collected, "hippocampus.ingestor.events", map[string]string{
		"outcome": outcomeFailed,
	}); got != 0 {
		t.Errorf("expected no failures, got %d", got)
	}

	if got, _ := counterValue(t, collected, "hippocampus.ingestor.memories", map[string]string{
		"kind": "event",
	}); got != 2 {
		t.Errorf("expected 2 memories promoted, got %d", got)
	}

	if got, _ := counterValue(t, collected, "hippocampus.ingestor.passes", map[string]string{
		"outcome": "ok",
	}); got != 1 {
		t.Errorf("expected 1 successful pass, got %d", got)
	}
}

// TestFailuresAreAttributed covers the two series an alert would actually fire on: an event the
// ingestor could not handle, and a rule that errors on every event it sees.
func TestFailuresAreAttributed(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "no metadata", nil))
	source.putMemory(memory("m1", "e1", "body", 5))

	target.failNext("ImportBatch", errors.New("target unavailable"))

	p := newPromoter(t, source, target, `{
		"defaultAction": "promote",
		"rules": [{"name":"unguarded","expr":"event.metadata['team'] == 'x'","action":"drop"}]
	}`, Config{})

	collected := collectMetrics(t, func() {
		if _, err := p.Pass(context.Background()); err != nil {
			t.Fatalf("Pass: %s", err)
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.ingestor.events", map[string]string{
		"outcome": outcomeFailed,
	}); got != 1 {
		t.Errorf("expected the failed promotion to be counted, got %d", got)
	}

	// Per rule, not just a total: "rule X errors on everything" is the diagnosis a bare count
	// cannot give.
	if got, found := counterValue(t, collected, "hippocampus.ingestor.rule_errors", map[string]string{
		"rule": "unguarded",
	}); !found || got != 1 {
		t.Errorf("expected the evaluation error attributed to 'unguarded', got %d (found=%t)", got, found)
	}
}

// TestSkippedEventsAreCounted pins that an event too large to judge lands in its own series rather
// than in the failure one - it is a deliberate refusal, and an operator needs to tell the two apart.
func TestSkippedEventsAreCounted(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "huge", nil))
	source.putMemory(memory("m1", "e1", "a", 1))
	source.putMemory(memory("m2", "e1", "b", 1))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{MaxEventMemories: 1})

	collected := collectMetrics(t, func() {
		if _, err := p.Pass(context.Background()); err != nil {
			t.Fatalf("Pass: %s", err)
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.ingestor.events", map[string]string{
		"outcome": outcomeSkipped,
	}); got != 1 {
		t.Errorf("expected 1 skipped event, got %d", got)
	}

	if got, _ := counterValue(t, collected, "hippocampus.ingestor.events", map[string]string{
		"outcome": outcomeFailed,
	}); got != 0 {
		t.Errorf("a skip must not be counted as a failure, got %d", got)
	}
}

// attributeKey adapts a plain string to the attribute package's key type, keeping the assertions
// above readable.
func attributeKey(key string) attribute.Key {
	return attribute.Key(key)
}
