package main

import (
	"testing"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/observability"
)

// observabilityConfigFromViper is the whole of the service's observability configuration surface,
// and every field on it is a key whose misreading is silent: a wrong endpoint exports nowhere, a
// wrong sampling ratio samples the wrong share, and a wrong service name publishes under somebody
// else's identity. None of them stops the service serving, so a test is the only thing that reads
// them back.
//
// observability.serviceName is the field these tests exist for. It was added after a deployment
// running five independent stores against one collector was found publishing one series per
// instrument between them - the OTLP-to-Prometheus translation promotes only service.name onto each
// series, so a dashboard read whichever instance had exported last.

// TestObservabilityConfigFromViper_ReadsEveryKey pins each field to its key, so a rename or a
// misspelling fails here rather than presenting as a setting that does nothing.
func TestObservabilityConfigFromViper_ReadsEveryKey(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("observability.tracing.enabled", true)
	viper.Set("observability.tracing.samplingRatio", 0.25)
	viper.Set("observability.metrics.enabled", true)
	viper.Set("observability.metrics.exportIntervalSeconds", 30)
	viper.Set("observability.otlp.endpoint", "collector:4317")
	viper.Set("observability.otlp.insecure", true)
	viper.Set("observability.prometheus.enabled", true)
	viper.Set("observability.serviceName", "hippocampus-agent")

	cfg := observabilityConfigFromViper(versionInfo{Version: "v1.2.3"})

	want := observability.Config{
		TracingEnabled:         true,
		TracingSamplingRatio:   0.25,
		MetricsEnabled:         true,
		MetricsIntervalSeconds: 30,
		OTLPEndpoint:           "collector:4317",
		OTLPInsecure:           true,
		PrometheusEnabled:      true,
		ServiceName:            "hippocampus-agent",
		ServiceVersion:         "v1.2.3",
	}

	if cfg != want {
		t.Errorf("observabilityConfigFromViper() = %+v, want %+v", cfg, want)
	}
}

// TestObservabilityConfigFromViper_ServiceNameDefaultsEmpty is the compatibility half. The fallback
// to "hippocampus" belongs to observability.Config.serviceName, not to a viper default here: an
// existing configuration file names no service and must keep reporting exactly the resource
// attributes it always has.
func TestObservabilityConfigFromViper_ServiceNameDefaultsEmpty(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	setStartupDefaults()

	if name := observabilityConfigFromViper(versionInfo{}).ServiceName; name != "" {
		t.Errorf("ServiceName = %q for an unset key, want empty so observability.Config applies its own fallback", name)
	}
}
