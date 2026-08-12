package bridge

import (
	"github.com/spf13/pflag"
)

// RegisterCommonFlags defines the flags every broker adapter shares: how to reach the Hippocampus
// service (address/auth/TLS) and how the DefaultTransformer shapes each message into a memory. A
// broker adapter's main adds its own broker-specific flags (server URL, topic/subject, credentials)
// on top. Only flag definitions live here - each adapter's main.go owns the viper binding and reads,
// so all viper access stays in main (per the repo convention), building the ClientConfig and
// TransformConfig from the keys these flags register.
//
// The flag names match the ClientConfig/TransformConfig fields so a main can read them back with the
// same string, e.g. viper.GetString("address").
func RegisterCommonFlags(fs *pflag.FlagSet) {
	// Connection to the Hippocampus service.
	fs.StringP("address", "a", "localhost:50051", "address of the hippocampus gRPC service")
	fs.String("token", "", "bearer token sent on every RPC when the service requires auth")
	fs.Bool("tls", false, "dial the service over TLS")
	fs.String("tls-ca-cert", "", "PEM CA bundle to verify the service certificate against, in place of the system pool (used with --tls)")
	fs.String("tls-cert", "", "client certificate for mutual TLS (used with --tls; requires --tls-key)")
	fs.String("tls-key", "", "client private key for mutual TLS (used with --tls; requires --tls-cert)")
	fs.Bool("tls-insecure-skip-verify", false, "skip verification of the service certificate (dev only; used with --tls)")
	fs.Int("call-timeout-seconds", 30, "per-message timeout bounding each StoreMemory RPC")

	// OIDC client-credentials auth, for a long-running bridge against an IdP-backed service: the
	// bridge mints its own access tokens and refreshes them, instead of carrying a --token that
	// eventually expires and then fails every write silently. Setting --oidc-client-id selects this
	// over --token. The names match the generators' so one realm configures both the same way.
	fs.String("oidc-issuer", "", "OIDC issuer to discover the token endpoint from (client-credentials auth)")
	fs.String("oidc-token-url", "", "OIDC token endpoint, used in place of discovery from --oidc-issuer")
	fs.String("oidc-client-id", "", "OIDC client id; setting it selects client-credentials auth over --token")
	fs.String("oidc-client-secret", "", "OIDC client secret (prefer the HIPPOCAMPUS_<BROKER>_OIDC_CLIENT_SECRET env var)")
	fs.String("oidc-scope", "", "optional space-separated scopes to request")
	fs.String("oidc-audience", "", "optional audience to request (Auth0 needs its API identifier; Keycloak ignores it)")

	// DefaultTransformer shaping.
	fs.Int32("significance", 1, "significance stamped on each stored memory (a per-message header can override it)")
	fs.String("significance-header", "", "message header whose integer value overrides --significance for that message")
	fs.String("group", "", "group label stamped on each memory (empty falls back to --group-from-subject)")
	fs.Bool("group-from-subject", true, "when --group is empty, use the message subject/topic as the memory group")
	fs.String("group-header", "", "message header whose value overrides the group for that message")
	fs.Bool("binary", false, "treat payloads as binary: base64-encode the body and mark it is_binary (never content-indexed)")
	fs.Int("max-body-bytes", 0, "truncate a payload to at most this many bytes before it becomes a memory body (0 = unlimited)")
	fs.StringSlice("metadata", nil, "metadata label as 'key=value' stamped on every memory (repeatable)")
	fs.StringSlice("metadata-header", nil, "message header to copy onto each memory's metadata (repeatable)")
	fs.String("metadata-header-prefix", "", "copy every message header carrying this prefix onto the memory's metadata, with the prefix stripped from the key")
	fs.String("subject-metadata-key", "", "record the message subject/topic as a metadata label under this key, as well as (or instead of) using it as the group - set this with an explicit --group when the service token is group-scoped")

	fs.String("log-level", "info", "logging level (trace, debug, info, warn, error)")

	// Observability. Metrics/tracing are off until a collector is wanted, matching the service's own
	// defaults; the probe listener is ON, because a bridge that cannot be probed is a bridge whose
	// stalling is invisible. Running several bridges on one host means giving each its own
	// --health-port.
	fs.Bool("metrics", false, "export OTEL metrics over OTLP/gRPC")
	fs.Bool("tracing", false, "export OTEL traces over OTLP/gRPC")
	fs.Float64("tracing-sampling-ratio", 0.1, "fraction of locally started traces to sample")
	fs.String("otlp-endpoint", "", "OTLP/gRPC collector endpoint (empty uses the OTEL_EXPORTER_OTLP_* environment variables, then localhost:4317)")
	fs.Bool("otlp-insecure", true, "connect to the collector without TLS")
	fs.Int("metrics-interval-seconds", 0, "OTEL metric export interval (0 selects the SDK default)")
	fs.String("metrics-group", "", "tenancy label stamped on this process's telemetry as a resource attribute; defaults to --group when that is set")
	fs.Int("health-port", 8090, "port serving /healthz and /readyz (0 disables)")
	fs.String("health-bind-address", "", "interface for the health listener (empty binds all)")
}
