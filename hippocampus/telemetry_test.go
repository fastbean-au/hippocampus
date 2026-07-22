package hippocampus

import (
	"errors"
	"testing"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// erroringMeter embeds the no-op meter (satisfying the interface's every method) but overrides the
// five instrument constructors newTelemetry's helpers use, always failing them - so each helper's
// error-logging branch (never reachable through the real no-op/SDK meters in ordinary operation)
// can be exercised directly, without a live OTEL SDK meter provider misconfigured to produce a
// genuine instrument-registration conflict.
type erroringMeter struct {
	noop.Meter
}

func (erroringMeter) Int64Counter(name string, options ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return nil, errors.New("counter boom")
}

func (erroringMeter) Int64Histogram(name string, options ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	return nil, errors.New("int histogram boom")
}

func (erroringMeter) Float64Histogram(name string, options ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return nil, errors.New("float histogram boom")
}

func (erroringMeter) Int64Gauge(name string, options ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	return nil, errors.New("int gauge boom")
}

func (erroringMeter) Float64Gauge(name string, options ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return nil, errors.New("float gauge boom")
}

// TestNewInstrumentHelpers_LogAndReturnZeroValueOnError verifies each of newTelemetry's five
// instrument-construction helpers logs and returns the zero value (rather than panicking or
// propagating) when the underlying meter fails to create the instrument - a case the real no-op
// and SDK meters essentially never hit in practice, but which must still degrade safely.
func TestNewInstrumentHelpers_LogAndReturnZeroValueOnError(t *testing.T) {
	meter := erroringMeter{}

	if got := newInt64Counter(meter, "test.counter", "a test counter"); got != nil {
		t.Errorf("expected a nil counter on error, got %v", got)
	}

	if got := newInt64Histogram(meter, "test.histogram.int", "ms", "a test int histogram"); got != nil {
		t.Errorf("expected a nil histogram on error, got %v", got)
	}

	if got := newFloat64Histogram(meter, "test.histogram.float", "s", "a test float histogram"); got != nil {
		t.Errorf("expected a nil histogram on error, got %v", got)
	}

	if got := newInt64Gauge(meter, "test.gauge.int", "a test int gauge"); got != nil {
		t.Errorf("expected a nil gauge on error, got %v", got)
	}

	if got := newFloat64Gauge(meter, "test.gauge.float", "a test float gauge"); got != nil {
		t.Errorf("expected a nil gauge on error, got %v", got)
	}
}
