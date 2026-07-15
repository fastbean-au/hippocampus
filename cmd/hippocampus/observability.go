package main

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// ObservabilityConfig carries the observability settings read from viper in main(). Tracing and
// metrics are independently optional; when both are disabled no providers are installed and the
// instrumentation throughout the service falls back to the global no-op providers.
type ObservabilityConfig struct {
	TracingEnabled         bool
	TracingSamplingRatio   float64
	MetricsEnabled         bool
	MetricsIntervalSeconds int
	OTLPEndpoint           string
	OTLPInsecure           bool
}

// initObservability installs the global OTEL tracer and meter providers according to the
// configuration and returns a shutdown function that flushes and stops them. Spans are exported
// over OTLP/gRPC and sampled with a parent-based trace-ID ratio sampler, so the sampling ratio
// applies to locally started traces while honouring sampling decisions made by callers. An empty
// endpoint leaves the exporter's own default in place (the OTEL_EXPORTER_OTLP_* environment
// variables, falling back to localhost:4317).
func initObservability(ctx context.Context, cfg ObservabilityConfig) (func(context.Context) error, error) {
	log.Trace("func() initObservability")

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

	res := resource.NewSchemaless(semconv.ServiceName("hippocampus"))

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

		log.Info("metrics enabled")
	}

	return shutdown, nil
}
