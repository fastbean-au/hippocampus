package client

import (
	"github.com/spf13/pflag"
)

// RegisterFlags defines the connection flags for one endpoint, prefixed with its name - so the
// ingestor registers "source-*" and "target-*" from one definition rather than two hand-kept
// copies. The unprefixed address flag every other integration calls --address is spelled
// --source-address / --target-address here, because there is no single service to be "the" one.
//
// Only flag definitions live here; cmd/ingestor/main.go owns the viper binding and reads, per the
// repo convention. Key returns the viper key a main reads a field back with.
func RegisterFlags(fs *pflag.FlagSet, endpoint string, defaultAddress string, description string) {
	fs.String(Key(endpoint, "address"), defaultAddress, "address of the "+description+" hippocampus gRPC service")
	fs.String(Key(endpoint, "token"), "", "bearer token sent on every RPC to the "+description+" service")
	fs.Bool(Key(endpoint, "tls"), false, "dial the "+description+" service over TLS")
	fs.String(Key(endpoint, "tls-ca-cert"), "", "PEM CA bundle to verify the "+description+" service certificate against, in place of the system pool")
	fs.String(Key(endpoint, "tls-cert"), "", "client certificate for mutual TLS to the "+description+" service (requires the matching key)")
	fs.String(Key(endpoint, "tls-key"), "", "client private key for mutual TLS to the "+description+" service (requires the matching certificate)")
	fs.Bool(Key(endpoint, "tls-insecure-skip-verify"), false, "skip verification of the "+description+" service certificate (dev only)")
}

// Key builds the flag/viper key for one endpoint's field, so registration and reads cannot drift.
func Key(endpoint string, field string) string {
	return endpoint + "-" + field
}
