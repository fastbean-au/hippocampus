package bridge

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/observability"
)

// shutdownTimeout bounds each teardown step. Both run after the consume loop has already returned,
// so all they do is flush and close.
const shutdownTimeout = 5 * time.Second

// RuntimeConfig is the observability wiring every broker bridge shares. It carries VALUES rather
// than reading viper, so each cmd's main keeps ownership of its own configuration per the repo
// convention while the wiring itself exists once.
type RuntimeConfig struct {
	// Broker names the adapter ("nats", "mqtt", ...). It becomes the component name in the probe
	// bodies and the service.name in the telemetry.
	Broker string

	// Version is stamped into the telemetry resource and reported by /healthz.
	Version string

	// Observability is passed through to observability.Init; ServiceName and ServiceVersion are
	// filled in from Broker and Version, so a caller need not repeat them.
	Observability observability.Config

	// HealthPort and HealthBindAddress configure the probe listener. A zero port disables it.
	HealthPort        int
	HealthBindAddress string
}

// StartRuntime installs observability and starts the health endpoints, returning a stop function the
// caller defers.
//
// The connection is used for the readiness check: a bridge is ready when the Hippocampus instance it
// writes to can actually serve, which is what the gRPC health service reports.
//
// The BROKER is deliberately not part of readiness, because the two failure modes it has are both
// already handled elsewhere. A broker unreachable at startup exits the process before the consume
// loop begins - the supervisor's problem, and visible as a restart - while a mid-run disconnect is
// the adapters' own to retry on their own schedule. What nothing else would notice is the
// Hippocampus end going away, since a bridge with no traffic and a bridge that cannot write look
// identical from outside. That is the gap this closes.
func StartRuntime(ctx context.Context, cfg RuntimeConfig, conn *grpc.ClientConn) (func(), error) {
	component := "hippocampus-" + cfg.Broker + "-bridge"

	obsCfg := cfg.Observability
	obsCfg.ServiceName = component
	obsCfg.ServiceVersion = cfg.Version

	shutdownObservability, err := observability.Init(ctx, obsCfg)
	if err != nil {
		return nil, fmt.Errorf("initialising observability: %w", err)
	}

	health := observability.NewHealthServer(observability.HealthConfig{
		Port:        cfg.HealthPort,
		BindAddress: cfg.HealthBindAddress,
		Version:     cfg.Version,
		Component:   component,
		Checks: map[string]observability.Check{
			"hippocampus": observability.GRPCHealthCheck(conn),
		},
	})

	if err := health.Start(); err != nil {
		// Observability is already up, so unwind it before returning - otherwise a bad health port
		// leaves an exporter goroutine behind in a process that is about to report a startup failure.
		stopObservability(shutdownObservability)

		return nil, fmt.Errorf("starting the health endpoints: %w", err)
	}

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := health.Shutdown(shutdownCtx); err != nil {
			log.Errorf("stopping the health endpoints: %s", err.Error())
		}

		stopObservability(shutdownObservability)
	}, nil
}

// stopObservability flushes the exporters, logging rather than returning a failure: it runs during
// teardown, where there is nothing left to do about it.
func stopObservability(shutdown func(context.Context) error) {
	flushCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := shutdown(flushCtx); err != nil {
		log.Errorf("flushing observability: %s", err.Error())
	}
}
