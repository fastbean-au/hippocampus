// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter // import "github.com/fastbean-au/hippocampus/otel/hippocampusexporter"

import (
	"context"
	"errors"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"

	"github.com/fastbean-au/hippocampus/otel/hippocampusexporter/internal/metadata"
)

const defaultEndpoint = "localhost:50051"

// NewFactory creates a factory for the Hippocampus logs exporter.
func NewFactory() exporter.Factory {
	return exporter.NewFactory(
		metadata.Type,
		createDefaultConfig,
		exporter.WithLogs(createLogsExporter, metadata.LogsStability))
}

func createDefaultConfig() component.Config {
	clientConfig := configgrpc.NewDefaultClientConfig()
	clientConfig.Endpoint = defaultEndpoint

	return &Config{
		ClientConfig:  clientConfig,
		TimeoutConfig: exporterhelper.NewDefaultTimeoutConfig(),
		BackOffConfig: configretry.NewDefaultBackOffConfig(),
		QueueConfig:   configoptional.Some(exporterhelper.NewDefaultQueueConfig()),

		CreateEvents: true,
		GroupFrom:    "service.name",
		DefaultGroup: "unknown",

		EventKeyFrom:      []string{"service.name"},
		EventBucket:       eventBucketDay,
		EventNameTemplate: "{key} — {bucket}",
		EventSignificance: 12000,

		BodyFrom: bodyFromRecordBody,

		// Defaults mirror the docs/demonstrations.md logs mapping: monotonic by severity so the
		// decay cycle forgets DEBUG/INFO noise first and keeps ERROR/FATAL.
		Significance: SignificanceConfig{
			Trace:   1000,
			Debug:   2000,
			Info:    6000,
			Warn:    16000,
			Error:   28000,
			Fatal:   32000,
			Default: 6000,
			Jitter:  1500,
			Min:     1,
			Max:     32767,
		},
	}
}

func createLogsExporter(ctx context.Context, set exporter.Settings, config component.Config) (exporter.Logs, error) {
	cfg, ok := config.(*Config)
	if !ok {

		return nil, errors.New("invalid configuration type; can't cast to hippocampusexporter.Config")
	}

	exp := newExporter(cfg, set)

	return exporterhelper.NewLogs(
		ctx,
		set,
		config,
		exp.pushLogs,
		exporterhelper.WithStart(exp.start),
		exporterhelper.WithShutdown(exp.shutdown),
		exporterhelper.WithTimeout(cfg.TimeoutConfig),
		exporterhelper.WithQueue(cfg.QueueConfig),
		exporterhelper.WithRetry(cfg.BackOffConfig),
		exporterhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
	)
}
