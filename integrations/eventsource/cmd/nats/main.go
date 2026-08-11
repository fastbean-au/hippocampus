// Command nats bridges a NATS subject into a Hippocampus instance: every message published on the
// subject is stored as a memory. It is a thin wiring of the reusable bridge core - flags in, a
// DefaultTransformer, and the nats adapter's consume loop - so an operator runs one binary per
// subject (or several sharing a --queue group for load balancing).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
	natsbridge "github.com/fastbean-au/hippocampus/integrations/eventsource/nats"
)

// version is stamped into logs; a release build overrides it with -ldflags "-X main.version".
var version = "dev"

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain is the testable body of main: it registers flags, installs the signal handler, and runs
// serve, returning a process exit code rather than calling os.Exit itself so tests can drive every
// branch.
func realMain(args []string) int {
	if err := registerFlags(pflag.CommandLine, args); err != nil {
		log.Errorf("failed to register command line flags: %s", err.Error())

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx); err != nil {
		log.Errorf("nats bridge exited with an error: %s", err.Error())

		return 1
	}

	return 0
}

// registerFlags defines the common bridge flags plus the NATS-specific ones, parses args, and wires
// the HIPPOCAMPUS_NATS_* environment overrides so secrets can be injected without appearing in argv.
func registerFlags(fs *pflag.FlagSet, args []string) error {
	bridge.RegisterCommonFlags(fs)

	fs.String("nats-url", "nats://localhost:4222", "NATS server URL")
	fs.StringP("subject", "s", "", "NATS subject to subscribe to (required; supports wildcards)")
	fs.String("queue", "", "optional queue group for load-balanced consumers")
	fs.String("connection-name", "hippocampus-nats-bridge", "connection name reported to the NATS server")
	fs.String("nats-creds", "", "NATS credentials (.creds) file for decentralised auth")
	fs.String("nats-token", "", "NATS server auth token")
	fs.String("nats-username", "", "NATS username")
	fs.String("nats-password", "", "NATS password")
	fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_NATS")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	return nil
}

// serve handles --version and the log level, then hands off to run.
func serve(ctx context.Context) error {
	if viper.GetBool("version") {
		fmt.Println(version)

		return nil
	}

	level, err := log.ParseLevel(viper.GetString("log-level"))
	if err != nil {
		return fmt.Errorf("invalid log level %q: %w", viper.GetString("log-level"), err)
	}

	log.SetLevel(level)

	return run(ctx)
}

// run reads config from viper, dials the service, and runs the NATS adapter until ctx is cancelled.
// All viper access lives here, in main, per the repo convention.
func run(ctx context.Context) error {
	if viper.GetString("subject") == "" {
		return fmt.Errorf("--subject is required")
	}

	clientCfg := bridge.ClientConfig{
		Address:               viper.GetString("address"),
		Token:                 viper.GetString("token"),
		TLS:                   viper.GetBool("tls"),
		TLSCACertFile:         viper.GetString("tls-ca-cert"),
		TLSCertFile:           viper.GetString("tls-cert"),
		TLSKeyFile:            viper.GetString("tls-key"),
		TLSInsecureSkipVerify: viper.GetBool("tls-insecure-skip-verify"),
	}

	conn, client, err := bridge.Dial(clientCfg)
	if err != nil {
		return fmt.Errorf("dialling hippocampus: %w", err)
	}

	defer func() { _ = conn.Close() }()

	transformer := bridge.NewDefaultTransformer(transformConfig())

	store := bridge.NewStore(client, transformer, time.Duration(viper.GetInt("call-timeout-seconds"))*time.Second)

	b := natsbridge.New(natsbridge.Config{
		URL:             viper.GetString("nats-url"),
		Subject:         viper.GetString("subject"),
		Queue:           viper.GetString("queue"),
		Name:            viper.GetString("connection-name"),
		CredentialsFile: viper.GetString("nats-creds"),
		Token:           viper.GetString("nats-token"),
		Username:        viper.GetString("nats-username"),
		Password:        viper.GetString("nats-password"),
	}, store)

	log.WithField("address", clientCfg.Address).Info("connecting to hippocampus")

	return b.Run(ctx)
}

// transformConfig assembles the DefaultTransformer config from viper. Shared shape across every
// bridge cmd; kept here (not in the bridge package) so all viper reads stay in main.
func transformConfig() bridge.TransformConfig {
	return bridge.TransformConfig{
		Significance:       viper.GetInt32("significance"),
		SignificanceHeader: viper.GetString("significance-header"),
		Group:              viper.GetString("group"),
		GroupFromSubject:   viper.GetBool("group-from-subject"),
		GroupHeader:        viper.GetString("group-header"),
		Binary:             viper.GetBool("binary"),
		MaxBodyBytes:       viper.GetInt("max-body-bytes"),

		Metadata:             metadataLabels(),
		MetadataHeaders:      viper.GetStringSlice("metadata-header"),
		MetadataHeaderPrefix: viper.GetString("metadata-header-prefix"),
		SubjectMetadataKey:   viper.GetString("subject-metadata-key"),
	}
}

// metadataLabels turns the repeated --metadata 'key=value' flags into the fixed labels stamped on
// every memory, splitting on the FIRST '=' so a value may contain one. A malformed entry is skipped
// with a warning rather than failing startup: the bridge's job is to keep consuming, and one bad
// flag should not stop it.
func metadataLabels() map[string]string {
	raw := viper.GetStringSlice("metadata")
	if len(raw) == 0 {
		return nil
	}

	out := make(map[string]string, len(raw))

	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			log.Warnf("ignoring malformed --metadata %q (want 'key=value')", entry)

			continue
		}

		out[key] = value
	}

	return out
}
