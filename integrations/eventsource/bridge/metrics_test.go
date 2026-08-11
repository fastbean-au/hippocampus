package bridge

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fastbean-au/hippocampus/contract"
)

// collectMetrics installs a real SDK meter provider with a manual reader, rebuilds the package's
// instruments against it, runs fn, and returns everything recorded.
//
// The instruments are package-level and bound to whatever provider was global when they were built,
// so they have to be rebuilt here - which is the same order main uses in production (StartRuntime
// before the consume loop). Both are restored afterwards so the other tests stay on the no-op
// providers they expect.
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

// counterValue sums a counter's points matching every given attribute, and reports whether the
// instrument existed - so a renamed metric fails loudly rather than reading as a zero.
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
				matched := true

				for key, want := range attrs {
					value, found := point.Attributes.Value(attribute.Key(key))
					if !found || value.AsString() != want {
						matched = false

						break
					}
				}

				if matched {
					total += point.Value
				}
			}

			return total, true
		}
	}

	return 0, false
}

// TestMessageOutcomesAreDistinct is the reason the outcome is four-valued rather than a success
// bool. A memory the SERVICE declined for insignificance is the decay model working; a message a
// Transformer filtered was dropped on purpose; only `failed` is the bridge not doing its job. An SLO
// that could not separate them would fire on a correctly-configured bridge.
func TestMessageOutcomesAreDistinct(t *testing.T) {
	cases := []struct {
		name        string
		transformer Transformer
		storer      *fakeStorer
		wantOutcome string
		wantErr     bool
	}{
		{
			name: "stored",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) {
				return []*contract.Memory{{Body: "a", Significance: 1}}, nil
			}),
			storer:      &fakeStorer{},
			wantOutcome: OutcomeStored,
		},
		{
			name: "rejected below the minimum significance",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) {
				return []*contract.Memory{{Body: "a", Significance: 1}}, nil
			}),
			storer:      &fakeStorer{rejected: true},
			wantOutcome: OutcomeRejected,
		},
		{
			name: "filtered by the transformer",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) {
				return nil, nil
			}),
			storer:      &fakeStorer{},
			wantOutcome: OutcomeFiltered,
		},
		{
			name: "failed to store",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) {
				return []*contract.Memory{{Body: "a", Significance: 1}}, nil
			}),
			storer:      &fakeStorer{err: errors.New("unavailable")},
			wantOutcome: OutcomeFailed,
			wantErr:     true,
		},
		{
			name: "failed to transform",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) {
				return nil, errors.New("bad payload")
			}),
			storer:      &fakeStorer{},
			wantOutcome: OutcomeFailed,
			wantErr:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(c.storer, c.transformer, 0, "nats")

			collected := collectMetrics(t, func() {
				err := s.Handle(context.Background(), Message{Subject: "events.a"})

				if c.wantErr && err == nil {
					t.Error("expected Handle to report a failure")
				}

				if !c.wantErr && err != nil {
					t.Errorf("Handle: %s", err)
				}
			})

			got, found := counterValue(t, collected, "hippocampus.bridge.messages", map[string]string{
				"broker":  "nats",
				"outcome": c.wantOutcome,
			})

			if !found {
				t.Fatal("hippocampus.bridge.messages was not recorded")
			}

			if got != 1 {
				t.Errorf("expected outcome %q to be counted once, got %d", c.wantOutcome, got)
			}
		})
	}
}

// TestMessageOutcomeIsTheWorstOfItsMemories: one message can yield several memories, and the
// adapter is about to redeliver the whole message if any of them failed - so the message's outcome
// must not read as a success.
func TestMessageOutcomeIsTheWorstOfItsMemories(t *testing.T) {
	fake := &fakeStorer{rejected: true}

	tr := TransformerFunc(func(Message) ([]*contract.Memory, error) {
		return []*contract.Memory{
			{Body: "a", Significance: 1},
			{Body: "b", Significance: 2},
		}, nil
	})

	s := NewStore(fake, tr, 0, "kafka")

	collected := collectMetrics(t, func() {
		if err := s.Handle(context.Background(), Message{Subject: "s"}); err != nil {
			t.Fatalf("Handle: %s", err)
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.bridge.messages", map[string]string{
		"outcome": OutcomeRejected,
	}); got != 1 {
		t.Errorf("expected the message to report the rejected outcome, got %d", got)
	}

	// Both memories are still counted individually, so "how many memories did this bridge write"
	// stays answerable independently of the message count.
	if got, _ := counterValue(t, collected, "hippocampus.bridge.memories", map[string]string{
		"broker":  "kafka",
		"outcome": OutcomeRejected,
	}); got != 2 {
		t.Errorf("expected 2 rejected memories, got %d", got)
	}
}

// TestBrokerAttributeIsAlwaysSet pins the fallback: an empty broker would otherwise produce a blank
// label, which is far harder to notice in a query than a literal "unknown".
func TestBrokerAttributeIsAlwaysSet(t *testing.T) {
	s := NewStore(&fakeStorer{}, TransformerFunc(func(Message) ([]*contract.Memory, error) {
		return []*contract.Memory{{Body: "a", Significance: 1}}, nil
	}), 0, "")

	collected := collectMetrics(t, func() {
		if err := s.Handle(context.Background(), Message{Subject: "s"}); err != nil {
			t.Fatalf("Handle: %s", err)
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.bridge.messages", map[string]string{
		"broker": "unknown",
	}); got != 1 {
		t.Errorf("expected the unknown-broker fallback, got %d", got)
	}
}
