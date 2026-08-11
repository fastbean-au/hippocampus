package observability

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// collectClientMetrics installs a real SDK meter provider, rebuilds the client instruments against
// it, runs fn, and returns what was recorded. Both globals are restored afterwards.
func collectClientMetrics(t *testing.T, fn func()) metricdata.ResourceMetrics {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	previousProvider := otel.GetMeterProvider()
	previousTel := clientTel

	otel.SetMeterProvider(provider)
	clientTel = newClientTelemetry()

	t.Cleanup(func() {
		otel.SetMeterProvider(previousProvider)
		clientTel = previousTel

		_ = provider.Shutdown(context.Background())
	})

	fn()

	var collected metricdata.ResourceMetrics

	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collecting metrics: %s", err)
	}

	return collected
}

// pointAttributes returns the attribute set of the single data point of the named counter.
func pointAttributes(t *testing.T, collected metricdata.ResourceMetrics, name string) attribute.Set {
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

			if len(sum.DataPoints) != 1 {
				t.Fatalf("expected exactly one data point on %s, got %d", name, len(sum.DataPoints))
			}

			return sum.DataPoints[0].Attributes
		}
	}

	t.Fatalf("%s was not recorded", name)

	return attribute.Set{}
}

func attributeValue(set attribute.Set, key string) string {
	value, ok := set.Value(attribute.Key(key))
	if !ok {
		return ""
	}

	return value.AsString()
}

// TestClientMetricsInterceptorRecords covers the ordinary path and the attribute set, which is the
// part that fails silently: nothing breaks, the dashboards are just empty.
func TestClientMetricsInterceptorRecords(t *testing.T) {
	interceptor := UnaryClientMetricsInterceptor("source")

	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return nil
	}

	collected := collectClientMetrics(t, func() {
		err := interceptor(
			context.Background(),
			"/hippocampus.v1.Hippocampus/GetMemories",
			nil, nil, nil, invoker,
		)
		if err != nil {
			t.Fatalf("interceptor: %s", err)
		}
	})

	attrs := pointAttributes(t, collected, "hippocampus.client.rpc.requests")

	if got := attributeValue(attrs, "endpoint"); got != "source" {
		t.Errorf("expected the endpoint attribute, got %q", got)
	}

	// The short name, never the full path - the service's own metrics use the same form, so a query
	// joins across the two without translating.
	if got := attributeValue(attrs, "rpc"); got != "GetMemories" {
		t.Errorf("expected the short rpc name, got %q", got)
	}

	if got := attributeValue(attrs, "code"); got != "OK" {
		t.Errorf("expected code OK, got %q", got)
	}

	if got := attributeValue(attrs, "outcome"); got != "ok" {
		t.Errorf("expected outcome ok, got %q", got)
	}

	if !metricNamePresent(collected, "hippocampus.client.rpc.duration") {
		t.Error("expected the duration histogram to be recorded alongside the counter")
	}
}

// TestClientMetricsInterceptorClassifiesFailures pins the three-valued outcome. The distinction is
// the whole point: an alert on the far end being unhealthy must not fire because this client sent
// something invalid, and the error is still returned to the caller either way.
func TestClientMetricsInterceptorClassifiesFailures(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantCode    string
		wantOutcome string
	}{
		{"unavailable is the far end", status.Error(codes.Unavailable, "down"), "Unavailable", "server_error"},
		{"internal is the far end", status.Error(codes.Internal, "boom"), "Internal", "server_error"},
		{"invalid argument is this client", status.Error(codes.InvalidArgument, "bad"), "InvalidArgument", "client_error"},
		{"not found is this client", status.Error(codes.NotFound, "gone"), "NotFound", "client_error"},
		{"rate limited is this client", status.Error(codes.ResourceExhausted, "slow down"), "ResourceExhausted", "client_error"},
		{"a plain error is unknown and a server fault", errors.New("boom"), "Unknown", "server_error"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			interceptor := UnaryClientMetricsInterceptor("target")

			invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
				return c.err
			}

			collected := collectClientMetrics(t, func() {
				if err := interceptor(
					context.Background(),
					"/hippocampus.v1.Hippocampus/ImportBatch",
					nil, nil, nil, invoker,
				); err == nil {
					t.Error("expected the interceptor to return the invoker's error unchanged")
				}
			})

			attrs := pointAttributes(t, collected, "hippocampus.client.rpc.requests")

			if got := attributeValue(attrs, "code"); got != c.wantCode {
				t.Errorf("expected code %q, got %q", c.wantCode, got)
			}

			if got := attributeValue(attrs, "outcome"); got != c.wantOutcome {
				t.Errorf("expected outcome %q, got %q", c.wantOutcome, got)
			}
		})
	}
}

// TestRPCName covers the method-to-name reduction, including the shapes that are not a full path.
func TestRPCName(t *testing.T) {
	cases := map[string]string{
		"/hippocampus.v1.Hippocampus/GetEvents": "GetEvents",
		"/grpc.health.v1.Health/Check":          "Check",
		"NoSlashes":                             "NoSlashes",
		"":                                      "",
	}

	for method, want := range cases {
		if got := rpcName(method); got != want {
			t.Errorf("rpcName(%q) = %q, want %q", method, got, want)
		}
	}
}

// TestWithGroup covers the tenancy attribute: absent by default, present once configured, and
// cleared again by an empty value - so a deployment that does not partition by group emits exactly
// the series it did before.
func TestWithGroup(t *testing.T) {
	previous := group.Load()

	t.Cleanup(func() { group.Store(previous) })

	setGroup("")

	interceptor := UnaryClientMetricsInterceptor("source")

	invoker := func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		return nil
	}

	call := func() metricdata.ResourceMetrics {
		return collectClientMetrics(t, func() {
			_ = interceptor(context.Background(), "/svc/Method", nil, nil, nil, invoker)
		})
	}

	attrs := pointAttributes(t, call(), "hippocampus.client.rpc.requests")

	if _, ok := attrs.Value(attribute.Key(GroupAttribute)); ok {
		t.Error("expected no tenancy attribute when none is configured")
	}

	setGroup("edge-alpha")

	attrs = pointAttributes(t, call(), "hippocampus.client.rpc.requests")

	if got := attributeValue(attrs, GroupAttribute); got != "edge-alpha" {
		t.Errorf("expected the configured group, got %q", got)
	}

	setGroup("")

	attrs = pointAttributes(t, call(), "hippocampus.client.rpc.requests")

	if _, ok := attrs.Value(attribute.Key(GroupAttribute)); ok {
		t.Error("expected an empty group to clear the attribute rather than emit a blank one")
	}
}

// TestInitSetsTheGroup pins that the attribute is installed even with every exporter disabled - the
// tenancy label belongs on the metrics whether or not this process is exporting them anywhere.
func TestInitSetsTheGroup(t *testing.T) {
	previous := group.Load()

	t.Cleanup(func() { group.Store(previous) })

	setGroup("")

	shutdown, err := Init(context.Background(), Config{Group: "edge-beta"})
	if err != nil {
		t.Fatalf("Init: %s", err)
	}

	defer func() { _ = shutdown(context.Background()) }()

	kv := group.Load()
	if kv == nil {
		t.Fatal("expected Init to record the group with observability disabled")
	}

	if kv.Value.AsString() != "edge-beta" {
		t.Errorf("expected edge-beta, got %q", kv.Value.AsString())
	}
}

// TestServiceNameDefault covers the fallback that keeps the service's own resource attributes
// exactly as they were before this package was shared.
func TestServiceNameDefault(t *testing.T) {
	if got := (Config{}).serviceName(); got != "hippocampus" {
		t.Errorf("expected the service's own name by default, got %q", got)
	}

	if got := (Config{ServiceName: "hippocampus-ingestor"}).serviceName(); got != "hippocampus-ingestor" {
		t.Errorf("expected the configured name, got %q", got)
	}
}

func metricNamePresent(collected metricdata.ResourceMetrics, name string) bool {
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return true
			}
		}
	}

	return false
}
