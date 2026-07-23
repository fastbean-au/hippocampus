// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

//go:generate mdatagen metadata.yaml

// Package hippocampusexporter implements an OpenTelemetry Collector logs exporter that stores log
// records as Hippocampus memories over the service's gRPC contract. Each log record becomes a
// memory whose significance is derived from the record's severity, so the Hippocampus consolidation
// (decay) cycle forgets routine low-severity noise first and keeps errors. Optionally, records are
// bucketed into events keyed by configurable resource/log attributes.
package hippocampusexporter // import "github.com/fastbean-au/hippocampus/otel/hippocampusexporter"
