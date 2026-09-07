package observability

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	log "github.com/sirupsen/logrus"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// DefaultMetricsPath is where a scrape endpoint is served. It is a constant rather than a setting
// for the components that mount it on their probe listener (the bridges and the ingestor), because
// a path is not a thing an operator has any reason to move on a private port; the service, whose
// endpoint is separately bindable and may sit behind something else, does make it configurable.
const DefaultMetricsPath = "/metrics"

// scrapeHandler holds the handler serving the Prometheus registry, published by Init when the
// scrape reader is installed and nil otherwise. Atomic for the reason attributes.go gives about the
// group: Init runs on the main goroutine while a listener reads this from another, which is a data
// race whether or not it is ever observed in practice.
var scrapeHandler atomic.Pointer[http.Handler]

// PrometheusHandler returns the handler that serves this process's metrics in the Prometheus text
// format, or nil when no scrape reader was installed. A caller mounts it on whichever listener it
// already has: the probe port for the client daemons, a dedicated one for the service.
func PrometheusHandler() http.Handler {
	if handler := scrapeHandler.Load(); handler != nil {
		return *handler
	}

	return nil
}

// newScrapeReader builds the Prometheus metric reader and the handler that serves it.
//
// Two choices here decide whether the shipped alert rules work against this endpoint rather than
// merely decorate it.
//
// The translation strategy is pinned. The exporter's default depends on prometheus/common's global
// name-validation scheme, which under "utf8" leaves dots in place - so hippocampus.rpc.requests
// would be exported as `hippocampus.rpc.requests_total` on one build of one dependency and
// `hippocampus_rpc_requests_total` on another, and every expression in
// deploy/observability/prometheus-alerts.yaml is written against the second. Those files were
// written for the OTLP-to-Prometheus translation a collector performs, and
// UnderscoreEscapingWithSuffixes IS that translation, so pinning it is what makes the two paths
// produce one set of series names. TestScrapeNamesMatchTheAlertRules holds it.
//
// The registry is this exporter's own rather than prometheus.DefaultRegisterer. The default
// registry carries the client library's Go-runtime and process collectors, whose series nothing in
// this repository declares, documents or alerts on - runtime.go already publishes the runtime
// figures this project does claim, through OTEL, so scraping a second unrelated set of them would
// make the endpoint's contents depend on which registry happened to be reached. It also keeps a
// second Init in one process (which is to say, a test) from failing on a duplicate registration.
func newScrapeReader() (sdkmetric.Reader, http.Handler, error) {
	registry := promclient.NewRegistry()

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		return nil, nil, err
	}

	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})

	return exporter, handler, nil
}

// MetricsConfig configures the standalone scrape listener the service uses. The client daemons do
// not need one: they already serve a probe port and mount the same handler on it.
type MetricsConfig struct {
	// Port is the TCP port to serve on. Zero disables the listener, matching the gateway.port and
	// health-port convention.
	Port int

	// BindAddress restricts the interface; empty binds all of them. This is the setting that makes
	// the endpoint safe to enable: a scrape endpoint enumerates every instrument this process
	// publishes, which is more than a memory store has any reason to offer the public internet, so
	// a deployment reachable from outside should bind it to the interface its scraper is on.
	BindAddress string

	// Path is where the handler is mounted; empty selects DefaultMetricsPath.
	Path string

	// Component names the binary in the log lines.
	Component string
}

// MetricsServer serves the Prometheus scrape endpoint on a listener of its own.
//
// It is deliberately NOT the gateway. The gateway is the public surface - the JSON API, the
// console, the OpenAPI document - and is frequently the thing an ingress terminates and exposes; a
// scrape endpoint on it would be an information disclosure (store size, capacity pressure, RPC
// rates, client ids' worth of shape) available to anyone who can reach the API, and gating it
// behind a token instead would put it out of reach of every scraper that does not carry one, which
// is most of them. A separate, separately bindable port is what lets the endpoint be open to the
// scraper and closed to everybody else.
//
// Keeping it off the gateway also settles the other half of the requirement for free: the endpoint
// never passes through httpMetricsMiddleware, so a scrape cannot appear in hippocampus.rpc.*'s own
// request rate - the same exclusion the probes and the console assets already have.
type MetricsServer struct {
	cfg    MetricsConfig
	server *http.Server
}

// NewMetricsServer builds the scrape listener. It does not listen until Start is called.
func NewMetricsServer(cfg MetricsConfig) *MetricsServer {
	if cfg.Path == "" {
		cfg.Path = DefaultMetricsPath
	}

	if cfg.Component == "" {
		cfg.Component = "hippocampus"
	}

	return &MetricsServer{cfg: cfg}
}

// Handler is the scrape mux, exported so a test can drive the endpoint without binding a port. It
// is nil when no scrape reader was installed.
func (m *MetricsServer) Handler() http.Handler {
	handler := PrometheusHandler()
	if handler == nil {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle(m.cfg.Path, handler)

	return mux
}

// Start binds the listener and serves in the background. A zero port, or an Init that installed no
// scrape reader, disables it and returns nil so a caller need not branch. A bind failure is
// returned rather than logged, for the same reason HealthServer.Start returns its own: a metrics
// port already in use is a configuration error, and quietly serving nothing is how a deployment
// ends up believing it is being scraped when it is not.
func (m *MetricsServer) Start() error {
	handler := m.Handler()
	if handler == nil || m.cfg.Port == 0 {
		return nil
	}

	address := fmt.Sprintf("%s:%d", m.cfg.BindAddress, m.cfg.Port)

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("binding the metrics listener on %s: %w", address, err)
	}

	m.server = &http.Server{
		Handler:           handler,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
		ReadHeaderTimeout: defaultHeaderTimeout,
	}

	go func() {
		if err := m.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("%s: metrics listener stopped: %s", m.cfg.Component, err.Error())
		}
	}()

	log.Infof("%s: Prometheus scrape endpoint on %s%s", m.cfg.Component, address, m.cfg.Path)

	return nil
}

// Shutdown stops the listener. It is safe to call when Start was a no-op.
func (m *MetricsServer) Shutdown(ctx context.Context) error {
	if m.server == nil {
		return nil
	}

	return m.server.Shutdown(ctx)
}
