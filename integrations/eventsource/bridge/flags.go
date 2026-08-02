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

	// DefaultTransformer shaping.
	fs.Int32("significance", 1, "significance stamped on each stored memory (a per-message header can override it)")
	fs.String("significance-header", "", "message header whose integer value overrides --significance for that message")
	fs.String("group", "", "group label stamped on each memory (empty falls back to --group-from-subject)")
	fs.Bool("group-from-subject", true, "when --group is empty, use the message subject/topic as the memory group")
	fs.String("group-header", "", "message header whose value overrides the group for that message")
	fs.Bool("binary", false, "treat payloads as binary: base64-encode the body and mark it is_binary (never content-indexed)")
	fs.Int("max-body-bytes", 0, "truncate a payload to at most this many bytes before it becomes a memory body (0 = unlimited)")

	fs.String("log-level", "info", "logging level (trace, debug, info, warn, error)")
}
