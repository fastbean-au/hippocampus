// Command kafka bridges an Apache Kafka topic into a Hippocampus instance: every message read from
// the topic is stored as a memory, with offsets committed only after a successful store, for
// at-least-once delivery. Run several instances sharing a --group to split a topic's partitions.
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

	"github.com/fastbean-au/hippocampus/observability"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
	kafkabridge "github.com/fastbean-au/hippocampus/integrations/eventsource/kafka"
)

var version = "dev"

// brokerName identifies this adapter in the telemetry and the probe bodies.
const brokerName = "kafka"

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
		log.Errorf("kafka bridge exited with an error: %s", err.Error())

		return 1
	}

	return 0
}

func registerFlags(fs *pflag.FlagSet, args []string) error {
	bridge.RegisterCommonFlags(fs)

	fs.StringSlice("brokers", []string{"localhost:9092"}, "Kafka bootstrap broker addresses")
	fs.StringP("topic", "t", "", "topic to consume (required)")
	fs.StringP("consumer-group", "g", "", "consumer group id (required)")
	fs.Int("min-bytes", 0, "minimum bytes per fetch (0 = reader default)")
	fs.Int("max-bytes", 0, "maximum bytes per fetch (0 = reader default)")
	fs.Int("error-backoff-seconds", 1, "seconds to wait before re-reading an uncommitted message after a store failure")
	fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_KAFKA")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	return nil
}

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

func run(ctx context.Context) error {
	if viper.GetString("topic") == "" {
		return fmt.Errorf("--topic is required")
	}

	if viper.GetString("consumer-group") == "" {
		return fmt.Errorf("--consumer-group is required")
	}

	clientCfg := bridge.ClientConfig{
		Address:               viper.GetString("address"),
		Token:                 viper.GetString("token"),
		TLS:                   viper.GetBool("tls"),
		TLSCACertFile:         viper.GetString("tls-ca-cert"),
		TLSCertFile:           viper.GetString("tls-cert"),
		TLSKeyFile:            viper.GetString("tls-key"),
		TLSInsecureSkipVerify: viper.GetBool("tls-insecure-skip-verify"),
		Endpoint:              "hippocampus",
	}

	conn, client, err := bridge.Dial(clientCfg)
	if err != nil {
		return fmt.Errorf("dialling hippocampus: %w", err)
	}

	defer func() { _ = conn.Close() }()

	// Observability and the probe endpoints, started once the connection exists so readiness can
	// report whether the service this bridge writes to can actually serve.
	stopRuntime, err := bridge.StartRuntime(ctx, runtimeConfig(), conn)
	if err != nil {
		return err
	}

	defer stopRuntime()

	transformer := bridge.NewDefaultTransformer(transformConfig())

	store := bridge.NewStore(client, transformer, time.Duration(viper.GetInt("call-timeout-seconds"))*time.Second, brokerName)

	b := kafkabridge.New(kafkabridge.Config{
		Brokers:      viper.GetStringSlice("brokers"),
		Topic:        viper.GetString("topic"),
		GroupID:      viper.GetString("consumer-group"),
		MinBytes:     viper.GetInt("min-bytes"),
		MaxBytes:     viper.GetInt("max-bytes"),
		ErrorBackoff: time.Duration(viper.GetInt("error-backoff-seconds")) * time.Second,
	}, store)

	log.WithField("address", clientCfg.Address).Info("connecting to hippocampus")

	return b.Run(ctx)
}

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

// runtimeConfig assembles the observability wiring from viper. Shared shape across every bridge cmd;
// kept here (not in the bridge package) so all viper reads stay in main.
//
// The tenancy label falls back to --group when --metrics-group is unset, because a bridge configured
// with a fixed group already IS one tenant's - and a resource attribute is the only shape that is
// safe here, since the per-message group can be the message subject and so unbounded.
func runtimeConfig() bridge.RuntimeConfig {
	group := viper.GetString("metrics-group")
	if group == "" {
		group = viper.GetString("group")
	}

	return bridge.RuntimeConfig{
		Broker:  brokerName,
		Version: version,
		Observability: observability.Config{
			TracingEnabled:         viper.GetBool("tracing"),
			TracingSamplingRatio:   viper.GetFloat64("tracing-sampling-ratio"),
			MetricsEnabled:         viper.GetBool("metrics"),
			MetricsIntervalSeconds: viper.GetInt("metrics-interval-seconds"),
			OTLPEndpoint:           viper.GetString("otlp-endpoint"),
			OTLPInsecure:           viper.GetBool("otlp-insecure"),
			Group:                  group,
		},
		HealthPort:        viper.GetInt("health-port"),
		HealthBindAddress: viper.GetString("health-bind-address"),
	}
}
