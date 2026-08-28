// Package observability installs the global OTEL tracer and meter providers, and serves the
// liveness/readiness endpoints an orchestrator probes.
//
// It lives in the root module so that all five binaries share one implementation: the service
// (cmd/hippocampus), the ingestor, and the four event-sourcing broker bridges. The integration
// modules already depend on the root module for the contract, and the root module already carries
// the OTEL dependencies, so sharing this costs neither side anything - whereas a second copy of
// exporter wiring in each integration is precisely the kind of thing that drifts.
package observability

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// GroupAttribute is the tenancy dimension every component stamps on its metrics, and it is
// deliberately a RESOURCE attribute set once per process rather than an instrument attribute read
// off each record.
//
// That is the only shape that is safe here. A bridge derives a memory's group from the message
// subject by default, so on a wildcard subscription the per-record value is unbounded - it would be
// a new metric stream per subject, which is exactly the high-cardinality attribute this repo's
// instrumentation rules forbid. Set once per process it is bounded by how many processes are
// deployed, which is what makes "show me this tenant's ingest" answerable without an unbounded
// series count. In the fleet-of-edges model each edge IS the tenant, so the two coincide.
const GroupAttribute = "hippocampus.group"

// Config carries the observability settings, read from viper in each binary's main(). Tracing and
// metrics are independently optional; when both are disabled no providers are installed and the
// instrumentation throughout falls back to the global no-op providers.
type Config struct {
	TracingEnabled         bool
	TracingSamplingRatio   float64
	MetricsEnabled         bool
	MetricsIntervalSeconds int
	OTLPEndpoint           string
	OTLPInsecure           bool

	// ServiceName names the component in the telemetry (semconv service.name): "hippocampus" for
	// the service itself, "hippocampus-ingestor", "hippocampus-nats-bridge", and so on. Empty
	// falls back to "hippocampus", preserving the service's own resource attributes exactly.
	ServiceName string

	ServiceVersion string

	// Group is the tenancy label stamped on every metric this process emits. Empty omits the
	// attribute entirely rather than emitting a blank one, so a deployment that does not partition
	// by group produces the same series it always did. See GroupAttribute.
	Group string
}

// serviceName resolves the name the component reports itself under.
func (c Config) serviceName() string {
	if c.ServiceName == "" {
		return "hippocampus"
	}

	return c.ServiceName
}

// Init installs the global OTEL tracer and meter providers according to the configuration and
// returns a shutdown function that flushes and stops them. Spans are exported over OTLP/gRPC and
// sampled with a parent-based trace-ID ratio sampler, so the sampling ratio applies to locally
// started traces while honouring sampling decisions made by callers. An empty endpoint leaves the
// exporter's own default in place (the OTEL_EXPORTER_OTLP_* environment variables, falling back to
// localhost:4317).
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	log.Trace("func() observability.Init")

	// Recorded before the early return: the tenancy attribute belongs on every metric this process
	// emits, and whether an exporter is installed has nothing to do with it.
	setGroup(cfg.Group)

	shutdowns := []func(context.Context) error{}
	shutdown := func(ctx context.Context) error {
		var err error
		for _, s := range shutdowns {
			if e := s(ctx); e != nil {
				err = e
			}
		}

		return err
	}

	if !cfg.TracingEnabled && !cfg.MetricsEnabled {
		log.Debug("observability disabled")

		return shutdown, nil
	}

	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.serviceName())}

	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}

	if cfg.Group != "" {
		attrs = append(attrs, attribute.String(GroupAttribute, cfg.Group))
	}

	res := resource.NewSchemaless(attrs...)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.TracingEnabled {
		opts := []otlptracegrpc.Option{}
		if cfg.OTLPEndpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint))
		}
		if cfg.OTLPInsecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}

		exporter, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			log.Errorf("failed to create OTLP trace exporter: %s", err.Error())

			return shutdown, fmt.Errorf("failed to create OTLP trace exporter")
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TracingSamplingRatio))),
			sdktrace.WithBatcher(exporter),
		)
		shutdowns = append(shutdowns, tp.Shutdown)

		otel.SetTracerProvider(tp)

		log.Infof("tracing enabled with sampling ratio %0.3f", cfg.TracingSamplingRatio)
	}

	if cfg.MetricsEnabled {
		opts := []otlpmetricgrpc.Option{}
		if cfg.OTLPEndpoint != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint))
		}
		if cfg.OTLPInsecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}

		exporter, err := otlpmetricgrpc.New(ctx, opts...)
		if err != nil {
			log.Errorf("failed to create OTLP metric exporter: %s", err.Error())

			return shutdown, fmt.Errorf("failed to create OTLP metric exporter")
		}

		readerOpts := []sdkmetric.PeriodicReaderOption{}
		if cfg.MetricsIntervalSeconds > 0 {
			readerOpts = append(readerOpts, sdkmetric.WithInterval(time.Duration(cfg.MetricsIntervalSeconds)*time.Second))
		}

		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, readerOpts...)),
		)
		shutdowns = append(shutdowns, mp.Shutdown)

		otel.SetMeterProvider(mp)

		// Registered only under an installed provider: against the global no-op meter the
		// callback would never be invoked anyway, and registering it there would leave a
		// registration attached to a provider that is about to be replaced.
		registerRuntimeMetrics()

		log.Info("metrics enabled")
	}

	return shutdown, nil
}
