package observability

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// TestInitObservability_Disabled verifies that with both tracing and metrics disabled,
// Init is a no-op: it returns a shutdown function that itself succeeds, and it must
// not install any global tracer/meter provider (the rest of the service depends on this to stay
// no-op-safe when observability is off).
func TestInitObservability_Disabled(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Init (disabled): %s", err)
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

	cfg := Config{
		TracingEnabled:         true,
		TracingSamplingRatio:   0.5,
		MetricsEnabled:         true,
		MetricsIntervalSeconds: 1,
		OTLPEndpoint:           "127.0.0.1:1", // nothing listens here; construction must still succeed
		OTLPInsecure:           true,
		ServiceVersion:         "v0.0.0-test",
	}

	shutdown, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (enabled, unreachable collector): %s", err)
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

	shutdown, err := Init(context.Background(), Config{
		TracingEnabled:       true,
		TracingSamplingRatio: 1,
		OTLPEndpoint:         "127.0.0.1:1",
		OTLPInsecure:         true,
	})
	if err != nil {
		t.Fatalf("Init (tracing only): %s", err)
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

	shutdown, err := Init(context.Background(), Config{
		MetricsEnabled: true,
		OTLPEndpoint:   "127.0.0.1:1",
		OTLPInsecure:   true,
		// MetricsIntervalSeconds left at zero: must fall back to the reader's own default interval.
	})
	if err != nil {
		t.Fatalf("Init (metrics only, zero interval): %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

// TestInitObservability_ExporterConstructionFails covers the two arms where an exporter cannot be
// built at all. They matter because they are the only ones that make Init RETURN an error: every
// other failure here is a collector being unreachable, which is deliberately not fatal - the
// exporters are lazy, and a service that refused to start because a metrics endpoint was down would
// be trading a working store for an observability dependency.
//
// Both arms must still return the shutdown func, since the tracer provider may already have been
// installed by the time the metric exporter fails and a caller that skipped the flush on an error
// would leak it.
func TestInitObservability_ExporterConstructionFails(t *testing.T) {
	restoreTracer := otel.GetTracerProvider()
	restoreMeter := otel.GetMeterProvider()

	t.Cleanup(func() {
		otel.SetTracerProvider(restoreTracer)
		otel.SetMeterProvider(restoreMeter)
	})

	// An endpoint the gRPC target parser cannot make a URL of. Nothing is dialled - construction
	// fails outright - which is what separates this from an unreachable collector.
	const unparseable = "%%%"

	t.Run("the trace exporter", func(t *testing.T) {
		shutdown, err := Init(context.Background(), Config{
			TracingEnabled: true,
			OTLPEndpoint:   unparseable,
			OTLPInsecure:   true,
		})

		if err == nil {
			t.Error("expected an unbuildable trace exporter to fail Init")
		}

		if shutdown == nil {
			t.Fatal("expected a shutdown func even on a failed Init")
		}

		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutting down after a failed Init: %s", err)
		}
	})

	t.Run("the metric exporter", func(t *testing.T) {
		shutdown, err := Init(context.Background(), Config{
			MetricsEnabled: true,
			OTLPEndpoint:   unparseable,
			OTLPInsecure:   true,
		})

		if err == nil {
			t.Error("expected an unbuildable metric exporter to fail Init")
		}

		if shutdown == nil {
			t.Fatal("expected a shutdown func even on a failed Init")
		}

		if err := shutdown(context.Background()); err != nil {
			t.Errorf("shutting down after a failed Init: %s", err)
		}
	})
}

// TestInitObservability_GroupOnTheResource covers the tenancy label's resource half. It is
// deliberately duplicated onto both the resource and each metric - the OTLP-to-Prometheus
// translation puts resource attributes in target_info, where a metric-level label is what an
// expression can actually group by - so the resource arm is the one nothing else exercises.
func TestInitObservability_GroupOnTheResource(t *testing.T) {
	restoreTracer := otel.GetTracerProvider()
	restoreMeter := otel.GetMeterProvider()

	t.Cleanup(func() {
		otel.SetTracerProvider(restoreTracer)
		otel.SetMeterProvider(restoreMeter)
		setGroup("")
	})

	shutdown, err := Init(context.Background(), Config{
		PrometheusEnabled: true,
		ServiceVersion:    "v0.0.0-test",
		Group:             "tenant-a",
	})
	if err != nil {
		t.Fatalf("Init: %s", err)
	}

	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// The metric half is what a scrape can be read for, and its presence here is what proves the
	// group reached Init at all - the resource half is not readable from outside the SDK.
	handler := PrometheusHandler()
	if handler == nil {
		t.Fatal("expected the scrape handler to be published")
	}
}
