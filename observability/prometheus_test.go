package observability

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// scrape drives the published handler and returns the exposition text.
func scrape(t *testing.T, handler http.Handler) string {
	t.Helper()

	if handler == nil {
		t.Fatal("no scrape handler was published")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, DefaultMetricsPath, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("scrape returned %d", recorder.Code)
	}

	return recorder.Body.String()
}

// restoreProviders puts the global providers back, so a test installing real ones cannot leak into
// the rest of the package's tests, which rely on the no-op defaults.
func restoreProviders(t *testing.T) {
	t.Helper()

	tracer := otel.GetTracerProvider()
	meter := otel.GetMeterProvider()

	t.Cleanup(func() {
		otel.SetTracerProvider(tracer)
		otel.SetMeterProvider(meter)
		scrapeHandler.Store(nil)
	})
}

// TestPrometheusHandler_OffByDefault pins that nothing is published unless the scrape reader was
// asked for: a caller mounts PrometheusHandler unconditionally, so a nil return is what keeps the
// route off a listener that has no metrics behind it.
func TestPrometheusHandler_OffByDefault(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{}); err != nil {
		t.Fatalf("Init (disabled): %s", err)
	}

	if PrometheusHandler() != nil {
		t.Error("expected no scrape handler when observability is disabled")
	}

	// Metrics on, but pushed rather than scraped: still nothing to serve.
	if _, err := Init(context.Background(), Config{MetricsEnabled: true, OTLPEndpoint: "127.0.0.1:1", OTLPInsecure: true}); err != nil {
		t.Fatalf("Init (OTLP only): %s", err)
	}

	if PrometheusHandler() != nil {
		t.Error("expected no scrape handler when only the OTLP exporter is enabled")
	}
}

// TestPrometheusReaderStandsAlone is the point of the item: a deployment that scrapes must not have
// to enable the push exporter (and so name a collector) to get metrics at all.
func TestPrometheusReaderStandsAlone(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true, ServiceVersion: "v0.0.0-test"}); err != nil {
		t.Fatalf("Init (scrape only): %s", err)
	}

	counter, err := otel.Meter("test").Int64Counter("hippocampus.test.standalone")
	if err != nil {
		t.Fatalf("Int64Counter: %s", err)
	}

	counter.Add(context.Background(), 3)

	body := scrape(t, PrometheusHandler())
	if !strings.Contains(body, "hippocampus_test_standalone_total") {
		t.Errorf("expected the recorded instrument in the scrape body, got:\n%s", body)
	}
}

// TestScrapeNamesMatchTheAlertRules is the guard the endpoint exists for.
//
// deploy/observability/prometheus-alerts.yaml is written against the series names the
// OTLP-to-Prometheus translation produces, because until now a collector was the only way these
// metrics reached a Prometheus. Serving them directly only helps if it produces the same names, and
// the exporter's default translation strategy is not fixed - it follows prometheus/common's global
// name-validation scheme, which under "utf8" leaves the dots in place. newScrapeReader therefore
// pins the strategy, and this checks both halves: that each name below really is one the shipped
// rules query, and that a scrape actually exposes it.
func TestScrapeNamesMatchTheAlertRules(t *testing.T) {
	restoreProviders(t)

	rules, err := os.ReadFile(filepath.Join("..", "deploy", "observability", "prometheus-alerts.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped alert rules: %s", err)
	}

	cases := []struct {
		instrument string
		unit       string
		histogram  bool
		series     string
	}{
		{instrument: "hippocampus.rpc.requests", series: "hippocampus_rpc_requests_total"},
		{instrument: "hippocampus.rpc.duration", unit: "s", histogram: true, series: "hippocampus_rpc_duration_seconds_bucket"},
		{instrument: "hippocampus.capacity_pressure", series: "hippocampus_capacity_pressure"},
		{instrument: "hippocampus.bridge.messages", series: "hippocampus_bridge_messages_total"},
	}

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true}); err != nil {
		t.Fatalf("Init (scrape only): %s", err)
	}

	meter := otel.Meter("test")

	for _, tc := range cases {
		if !strings.Contains(string(rules), tc.series) {
			t.Errorf("%s is not queried by any shipped alert rule, so this case no longer guards anything", tc.series)
		}

		if tc.histogram {
			h, err := meter.Float64Histogram(tc.instrument, metric.WithUnit(tc.unit))
			if err != nil {
				t.Fatalf("Float64Histogram %s: %s", tc.instrument, err)
			}

			h.Record(context.Background(), 0.5)

			continue
		}

		if tc.unit == "" && strings.HasSuffix(tc.series, "_total") {
			c, err := meter.Int64Counter(tc.instrument)
			if err != nil {
				t.Fatalf("Int64Counter %s: %s", tc.instrument, err)
			}

			c.Add(context.Background(), 1)

			continue
		}

		g, err := meter.Float64Gauge(tc.instrument)
		if err != nil {
			t.Fatalf("Float64Gauge %s: %s", tc.instrument, err)
		}

		g.Record(context.Background(), 0.25)
	}

	body := scrape(t, PrometheusHandler())

	for _, tc := range cases {
		if !strings.Contains(body, tc.series) {
			t.Errorf("expected %q in the scrape body (from instrument %s), got:\n%s", tc.series, tc.instrument, body)
		}
	}
}

// TestMetricsServer_Disabled covers both ways the standalone listener is a no-op, since a caller
// starts it unconditionally: no scrape reader installed, and a zero port.
func TestMetricsServer_Disabled(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{}); err != nil {
		t.Fatalf("Init (disabled): %s", err)
	}

	server := NewMetricsServer(MetricsConfig{Port: 9464})

	if server.Handler() != nil {
		t.Error("expected no handler when no scrape reader was installed")
	}

	if err := server.Start(); err != nil {
		t.Errorf("Start with no reader: %s", err)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown after a no-op Start: %s", err)
	}

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true}); err != nil {
		t.Fatalf("Init (scrape only): %s", err)
	}

	if err := NewMetricsServer(MetricsConfig{Port: 0}).Start(); err != nil {
		t.Errorf("Start on port 0: %s", err)
	}
}

// TestMetricsServer_ServesTheConfiguredPath confirms the endpoint answers where it was configured
// and nowhere else - the path is a setting on the service precisely so it can sit behind something
// that expects a particular one.
func TestMetricsServer_ServesTheConfiguredPath(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true}); err != nil {
		t.Fatalf("Init: %s", err)
	}

	handler := NewMetricsServer(MetricsConfig{Path: "/internal/metrics"}).Handler()
	if handler == nil {
		t.Fatal("expected a handler")
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/internal/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("expected 200 on the configured path, got %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, DefaultMetricsPath, nil))

	if recorder.Code != http.StatusNotFound {
		t.Errorf("expected 404 on the default path once one was configured, got %d", recorder.Code)
	}
}

// TestHealthServer_MountsMetrics covers the client daemons' route onto the probe listener: the
// handler appears at DefaultMetricsPath, and the probes are unaffected either way.
func TestHealthServer_MountsMetrics(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true}); err != nil {
		t.Fatalf("Init: %s", err)
	}

	withMetrics := NewHealthServer(HealthConfig{Component: "test", MetricsHandler: PrometheusHandler()}).Handler()

	recorder := httptest.NewRecorder()
	withMetrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, DefaultMetricsPath, nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("expected the scrape endpoint on the probe listener, got %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	withMetrics.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("expected /healthz to keep answering, got %d", recorder.Code)
	}

	without := NewHealthServer(HealthConfig{Component: "test"}).Handler()

	recorder = httptest.NewRecorder()
	without.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, DefaultMetricsPath, nil))

	if recorder.Code != http.StatusNotFound {
		t.Errorf("expected no metrics route without a handler, got %d", recorder.Code)
	}
}

// TestMetricsServer_ServesOnItsOwnPort binds the real listener, since the whole point of the
// standalone server is that the endpoint is reachable somewhere the gateway is not. It uses the
// freePort helper in health_test.go rather than port 0, which MetricsServer reads as "disabled".
func TestMetricsServer_ServesOnItsOwnPort(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true}); err != nil {
		t.Fatalf("Init: %s", err)
	}

	counter, err := otel.Meter("test").Int64Counter("hippocampus.test.listener")
	if err != nil {
		t.Fatalf("Int64Counter: %s", err)
	}

	counter.Add(context.Background(), 1)

	port := freePort(t)
	server := NewMetricsServer(MetricsConfig{Port: port, BindAddress: "127.0.0.1", Component: "test"})

	if err := server.Start(); err != nil {
		t.Fatalf("Start: %s", err)
	}

	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %s", err)
		}
	})

	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, DefaultMetricsPath))
	if err != nil {
		t.Fatalf("GET the scrape endpoint: %s", err)
	}

	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the scrape body: %s", err)
	}

	if !strings.Contains(string(body), "hippocampus_test_listener_total") {
		t.Errorf("expected the recorded instrument in the served body, got:\n%s", body)
	}
}

// TestMetricsServer_BindFailureIsReturned pins that a port already in use fails startup rather than
// being logged and skipped. Silently serving nothing is how a deployment ends up believing it is
// being scraped when it is not - the same reasoning HealthServer.Start applies to the probes.
func TestMetricsServer_BindFailureIsReturned(t *testing.T) {
	restoreProviders(t)

	if _, err := Init(context.Background(), Config{PrometheusEnabled: true}); err != nil {
		t.Fatalf("Init: %s", err)
	}

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %s", err)
	}

	defer func() { _ = holder.Close() }()

	port := holder.Addr().(*net.TCPAddr).Port

	if err := NewMetricsServer(MetricsConfig{Port: port, BindAddress: "127.0.0.1"}).Start(); err == nil {
		t.Error("expected a bind failure on a port already in use")
	}
}
