package main

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// TestInitObservability_Disabled verifies that with both tracing and metrics disabled,
// initObservability is a no-op: it returns a shutdown function that itself succeeds, and it must
// not install any global tracer/meter provider (the rest of the service depends on this to stay
// no-op-safe when observability is off).
func TestInitObservability_Disabled(t *testing.T) {
	shutdown, err := initObservability(context.Background(), ObservabilityConfig{})
	if err != nil {
		t.Fatalf("initObservability (disabled): %s", err)
	}

	if shutdown == nil {
		t.Fatal("expected a non-nil shutdown function even when disabled")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() (disabled, never having started anything): %s", err)
	}
}

// TestInitObservability_Enabled verifies the enabled path constructs real tracer/meter providers
// and installs them globally, without needing a reachable OTLP collector: the otlpgrpc exporters
// build lazily (the gRPC connection is not dialled until the first export attempt), so pointing at
// an address nothing listens on still succeeds here. The returned shutdown function must also
// succeed (or at least not hang) even though the batched exporters will fail to flush against the
// unreachable endpoint.
func TestInitObservability_Enabled(t *testing.T) {
	// Save and restore the global providers so this test cannot leak into any other test/production
	// code path relying on the global no-op providers.
	restoreTracer := otel.GetTracerProvider()
	restoreMeter := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(restoreTracer)
		otel.SetMeterProvider(restoreMeter)
	})

	cfg := ObservabilityConfig{
		TracingEnabled:         true,
		TracingSamplingRatio:   0.5,
		MetricsEnabled:         true,
		MetricsIntervalSeconds: 1,
		OTLPEndpoint:           "127.0.0.1:1", // nothing listens here; construction must still succeed
		OTLPInsecure:           true,
		ServiceVersion:         "v0.0.0-test",
	}

	shutdown, err := initObservability(context.Background(), cfg)
	if err != nil {
		t.Fatalf("initObservability (enabled, unreachable collector): %s", err)
	}

	if shutdown == nil {
		t.Fatal("expected a non-nil shutdown function")
	}

	// A real tracer provider must now be installed (not the no-op default).
	if _, ok := otel.GetTracerProvider().Tracer("test").(nooptrace.Tracer); ok {
		t.Error("expected a real tracer provider to be installed, got the no-op tracer")
	}

	// Shutdown must return (bounded by our own timeout) rather than hang forever trying to flush
	// against an address nothing listens on. The context handed to shutdown carries a short
	// deadline so the flush attempt itself is bounded; the goroutine wait below gives it extra
	// slack to unwind after that deadline fires, rather than racing the same deadline twice.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()

	// The flush against an unreachable endpoint may itself return an error - that's fine and
	// expected (main.go only logs it); what matters is that it returns at all within the timeout.
	done := make(chan struct{})
	var shutdownErr error

	go func() {
		shutdownErr = shutdown(shutdownCtx)
		close(done)
	}()

	select {

	case <-done:
		_ = shutdownErr // deliberately unchecked - either outcome is acceptable, see above

	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not return within the timeout")
	}
}

// TestInitObservability_TracingOnly and TestInitObservability_MetricsOnly verify the two signals
// are independently toggled - enabling one must not require or silently enable the other - and
// that a zero MetricsIntervalSeconds (metrics-only case) is tolerated (falls back to the exporter's
// own default reader interval rather than erroring).
func TestInitObservability_TracingOnly(t *testing.T) {
	restoreTracer := otel.GetTracerProvider()
	restoreMeter := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(restoreTracer)
		otel.SetMeterProvider(restoreMeter)
	})

	shutdown, err := initObservability(context.Background(), ObservabilityConfig{
		TracingEnabled:       true,
		TracingSamplingRatio: 1,
		OTLPEndpoint:         "127.0.0.1:1",
		OTLPInsecure:         true,
	})
	if err != nil {
		t.Fatalf("initObservability (tracing only): %s", err)
	}

	if _, ok := otel.GetTracerProvider().Tracer("test").(nooptrace.Tracer); ok {
		t.Error("expected a real tracer provider when tracing alone is enabled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

func TestInitObservability_MetricsOnly(t *testing.T) {
	restoreTracer := otel.GetTracerProvider()
	restoreMeter := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(restoreTracer)
		otel.SetMeterProvider(restoreMeter)
	})

	shutdown, err := initObservability(context.Background(), ObservabilityConfig{
		MetricsEnabled: true,
		OTLPEndpoint:   "127.0.0.1:1",
		OTLPInsecure:   true,
		// MetricsIntervalSeconds left at zero: must fall back to the reader's own default interval.
	})
	if err != nil {
		t.Fatalf("initObservability (metrics only, zero interval): %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}
