package search

import (
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// TestNewInt64Counter_InvalidNameIsSurvivable pins that a counter the SDK refuses is logged and
// then used anyway. The OTEL API returns a usable no-op instrument alongside the error, so the
// alternative - propagating it - would take down a service over a metric it cannot record.
//
// The names this package actually passes are all valid; this drives the guard through an
// instrument name the SDK rejects (they must start with a letter).
func TestNewInt64Counter_InvalidNameIsSurvivable(t *testing.T) {
	meter := sdkmetric.NewMeterProvider().Meter("test")

	counter := newInt64Counter(meter, "9-not-a-valid-instrument-name", "description")
	if counter == nil {
		t.Fatal("expected a usable counter even when the SDK refused the name")
	}

	// Recording against it must not panic - which is the whole reason the error is only logged.
	counter.Add(t.Context(), 1)
}
