// Command hippocampus runs the Hippocampus service: the gRPC server, the optional JSON/HTTP gateway
// and web console, and the sleep cycle that forgets what has become insignificant.
//
// It also carries the operator's offline tools as flags that run and exit instead of serving:
// --version, --check-config, --schema-version, --mint-token, --backup and --backfill-search.
//
// All configuration is read here, from the JSON file named by --config_file (./config.json by
// default) with HIPPOCAMPUS_* environment overrides; see docs/configuration.md.
package main
