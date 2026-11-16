package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/observability"
)

// observabilityConfigFromViper builds the observability configuration from the config file, in the
// same mould as hmacConfigFromViper: the viper reads stay in this package, and what they produce is a
// plain struct a test can assert on without starting a service.
//
// observability.serviceName is the one key here that a single-instance deployment never needs and a
// horizontally-scaled one cannot do without. The OTLP-to-Prometheus translation promotes only
// service.name/service.version/job/instance onto each series, so every instance sharing this name
// publishes one series per instrument between them, and the value a dashboard reads is whichever
// instance exported last. Empty is left empty rather than defaulted here, so the fallback to
// "hippocampus" stays in observability.Config.serviceName and an existing config file keeps the
// resource attributes it has always had.
func observabilityConfigFromViper(version versionInfo) observability.Config {
	log.Trace("func() observabilityConfigFromViper")

	return observability.Config{
		TracingEnabled:         viper.GetBool("observability.tracing.enabled"),
		TracingSamplingRatio:   viper.GetFloat64("observability.tracing.samplingRatio"),
		MetricsEnabled:         viper.GetBool("observability.metrics.enabled"),
		MetricsIntervalSeconds: viper.GetInt("observability.metrics.exportIntervalSeconds"),
		OTLPEndpoint:           viper.GetString("observability.otlp.endpoint"),
		OTLPInsecure:           viper.GetBool("observability.otlp.insecure"),
		PrometheusEnabled:      viper.GetBool("observability.prometheus.enabled"),
		ServiceName:            viper.GetString("observability.serviceName"),
		ServiceVersion:         version.Version,
	}
}

// observabilityFlushTimeout bounds the exporter flush on a bootstrap failure. The configured
// shutdown.timeoutSeconds is not used here: this path runs before the configuration has been
// validated as a whole, and a startup that is already failing should not also be able to hang.
const observabilityFlushTimeout = 5 * time.Second

// flushObservability stops the exporters during a bootstrap failure, logging rather than returning
// a problem: the caller is already returning one, and there is nothing left to do about a failed
// flush at that point.
func flushObservability(shutdown func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), observabilityFlushTimeout)
	defer cancel()

	if err := shutdown(ctx); err != nil {
		log.Errorf("failed to flush observability: %s", err.Error())
	}
}

// validateMetricsEndpoint checks the Prometheus scrape endpoint's settings.
//
// Every failure here has the same shape, and it is the one a metrics endpoint is worst at
// reporting: the process starts, the configuration looks enabled, and nothing is ever scraped. A
// scraper's own view of that is an instance that is simply down, which sends an operator to look at
// the service rather than at the four lines of configuration that describe where it is listening.
// So refuse each of them at startup, where the message can name the key.
func validateMetricsEndpoint() error {
	if !viper.GetBool("observability.prometheus.enabled") {
		return nil
	}

	if port := viper.GetInt("observability.prometheus.port"); port <= 0 || port > 65535 {
		return fmt.Errorf("observability.prometheus.port must be between 1 and 65535 when observability.prometheus.enabled is set, got %d", port)
	}

	if path := viper.GetString("observability.prometheus.path"); !strings.HasPrefix(path, "/") {
		return fmt.Errorf("observability.prometheus.path must begin with '/', got %q", path)
	}

	// A port collision is refused rather than left to the listener, because only one of the two
	// binds fails and which one depends on the order they are started in. The gRPC port is the one
	// that would win here, so the message an operator would otherwise get is about the metrics
	// listener when the mistake is as likely to be in the other key.
	for key, port := range map[string]int{"port": viper.GetInt("port"), "gateway.port": viper.GetInt("gateway.port")} {
		if port > 0 && port == viper.GetInt("observability.prometheus.port") {
			return fmt.Errorf("observability.prometheus.port is %d, which %s is already using: the scrape endpoint needs a listener of its own", port, key)
		}
	}

	return nil
}

// setObservabilityDefaults applies the observability.* defaults.
func setObservabilityDefaults() {
	// The Prometheus scrape endpoint. Off by default (a listener nobody asked for is a listener
	// nobody firewalled), but its port and path are defaulted so that enabling it is one boolean:
	// 9464 is the port the OpenTelemetry Prometheus exporter conventionally uses, which is what a
	// scrape config is most likely already written against.
	viper.SetDefault("observability.prometheus.port", 9464)
	viper.SetDefault("observability.prometheus.path", "/metrics")
}

// metricsServerConfigFromViper reads the scrape endpoint's listener keys.
func metricsServerConfigFromViper() observability.MetricsConfig {
	log.Trace("func() metricsServerConfigFromViper")

	return observability.MetricsConfig{
		Port:        viper.GetInt("observability.prometheus.port"),
		BindAddress: viper.GetString("observability.prometheus.bindAddress"),
		Path:        viper.GetString("observability.prometheus.path"),
		Component:   "hippocampus",
	}
}

// initObservability installs the OTEL providers and starts the scrape endpoint, on a listener of its
// own rather than the gateway - see observability.MetricsServer for why that separation is the
// point rather than an implementation detail. The endpoint starts here, immediately after the
// providers, so a port clash fails startup before the database is opened; it is a no-op when the
// scrape reader was not installed.
//
// On success the caller shuts down both the returned server and the providers.
func initObservability(obsCfg observability.Config, metricsCfg observability.MetricsConfig) (*observability.MetricsServer, func(context.Context) error, error) {
	log.Trace("func() initObservability")

	shutdownObservability, err := observability.Init(context.Background(), obsCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialise observability: %w", err)
	}

	metricsServer := observability.NewMetricsServer(metricsCfg)

	if err := metricsServer.Start(); err != nil {
		// Observability is already installed, so unwind it before returning - otherwise a bad
		// metrics port leaves exporter goroutines behind in a process reporting a startup failure.
		flushObservability(shutdownObservability)

		return nil, nil, fmt.Errorf("failed to start the metrics endpoint: %w", err)
	}

	return metricsServer, shutdownObservability, nil
}
