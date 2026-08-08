package main

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/fastbean-au/hippocampus/ratelimit"
)

// TestRegisterRateLimitGauge_ObservesTheClientTable pins what the gauge is for: the per-client
// table is bounded, so a value sitting at rateLimit.maxClients means the table is churning - and a
// churning table is one whose evicted callers get a fresh full bucket.
//
// It is registered at wiring time rather than at package init because a callback registered against
// the global no-op meter would never be re-registered against the real provider, which is exactly
// what installing a real provider here exercises.
func TestRegisterRateLimitGauge_ObservesTheClientTable(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)

	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	limiter, err := ratelimit.New(ratelimit.Config{
		PerClient:  ratelimit.Rule{RequestsPerSecond: 100, Burst: 100},
		MaxClients: 16,
	})
	if err != nil {
		t.Fatalf("ratelimit.New: %s", err)
	}

	// Two distinct principals, so the observed figure is something other than zero by default.
	limiter.Principal("client-a", "")
	limiter.Principal("client-b", "")

	registerRateLimitGauge(limiter)

	var collected metricdata.ResourceMetrics

	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("Collect: %s", err)
	}

	observed, found := gaugeValue(collected, "hippocampus.ratelimit.clients")
	if !found {
		t.Fatal("expected the clients gauge to be collected")
	}

	if want := int64(limiter.Clients()); observed != want {
		t.Errorf("expected the gauge to report %d tracked principals, got %d", want, observed)
	}
}

// gaugeValue finds a named int64 gauge in a collection and returns its single data point.
func gaugeValue(collected metricdata.ResourceMetrics, name string) (int64, bool) {
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}

			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || len(gauge.DataPoints) == 0 {
				return 0, false
			}

			return gauge.DataPoints[0].Value, true
		}
	}

	return 0, false
}
