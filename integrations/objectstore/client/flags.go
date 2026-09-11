package client

import (
	"github.com/spf13/pflag"
)

// RegisterCommonFlags defines the flags both binaries share: how to reach the Hippocampus service,
// how to reach the bucket, and the logging and observability wiring. Each command adds its own on
// top.
//
// Only flag definitions live here - each command's main.go owns the viper binding and the reads,
// per the repo convention - and the flag names match the Config fields so a main reads them back
// with the same string.
//
// defaultHealthPort differs by binary rather than by preference: the gateway and the reaper are
// routinely run on one host, and two processes defaulting to the same probe port means one of them
// fails to start for a reason that has nothing to do with its job.
func RegisterCommonFlags(fs *pflag.FlagSet, defaultHealthPort int) {
	// Connection to the Hippocampus service.
	fs.StringP("address", "a", "localhost:50051", "address of the hippocampus gRPC service")
	fs.String("token", "", "bearer token sent on every RPC when the service requires auth")
	fs.Bool("tls", false, "dial the service over TLS")
	fs.String("tls-ca-cert", "", "PEM CA bundle to verify the service certificate against, in place of the system pool (used with --tls)")
	fs.String("tls-cert", "", "client certificate for mutual TLS (used with --tls; requires --tls-key)")
	fs.String("tls-key", "", "client private key for mutual TLS (used with --tls; requires --tls-cert)")
	fs.Bool("tls-insecure-skip-verify", false, "skip verification of the service certificate (dev only; used with --tls)")
	fs.Int("call-timeout-seconds", 30, "per-RPC timeout")

	// The bucket. Credentials come from the standard AWS chain, never from a flag.
	fs.String("bucket", "", "object storage bucket this agent manages (required)")
	fs.String("s3-endpoint", "", "S3 endpoint URL, for an S3-compatible store such as MinIO (empty uses AWS)")
	fs.String("s3-region", "", "AWS region (empty uses the standard AWS configuration chain)")
	fs.Bool("s3-path-style", false, "address the bucket path-style rather than virtual-host style (MinIO needs this)")

	fs.String("log-level", "info", "logging level (trace, debug, info, warn, error)")
	fs.Bool("version", false, "print the version and exit")

	// Observability. Metrics and tracing are off until a collector is wanted, matching the
	// service's own defaults; the probe listener is ON, because an agent that cannot be probed is
	// an agent whose stalling is invisible.
	fs.Bool("metrics", false, "export OTEL metrics over OTLP/gRPC")
	fs.Bool("tracing", false, "export OTEL traces over OTLP/gRPC")
	fs.Float64("tracing-sampling-ratio", 0.1, "fraction of locally started traces to sample")
	fs.String("otlp-endpoint", "", "OTLP/gRPC collector endpoint (empty uses the OTEL_EXPORTER_OTLP_* environment variables, then localhost:4317)")
	fs.Bool("otlp-insecure", true, "connect to the collector without TLS")
	fs.Int("metrics-interval-seconds", 0, "OTEL metric export interval (0 selects the SDK default)")
	fs.String("metrics-group", "", "tenancy label stamped on this process's telemetry as a resource attribute")
	fs.Int("health-port", defaultHealthPort, "port serving /healthz and /readyz (0 disables)")
	fs.String("health-bind-address", "", "interface for the health listener (empty binds all)")
	fs.Bool("prometheus", false, "serve the metrics for Prometheus to scrape at /metrics on the health port, instead of (or as well as) pushing them with --metrics")
}
