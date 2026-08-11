// Command ingestor promotes completed events from an edge ("source") Hippocampus instance into a
// central ("target") one, under a rules file that decides, per event, whether the data is worth
// keeping at all.
//
// It is a client of two instances and holds no state: the source store is the queue, and what it
// contains is exactly what has not been judged yet. See docs/ingestor.md.
package main

import (
	"context"
	"errors"
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

	"github.com/fastbean-au/hippocampus/integrations/ingestor/client"
	"github.com/fastbean-au/hippocampus/integrations/ingestor/promoter"
	"github.com/fastbean-au/hippocampus/integrations/ingestor/rules"
)

// version is stamped into logs; a release build overrides it with -ldflags "-X main.version".
var version = "dev"

// The two endpoint names the connection flags are registered under. They are also the dependency
// names /readyz reports, so a failing probe names the end that is unreachable.
const (
	sourceEndpoint = "source"
	targetEndpoint = "target"
)

// Shutdown bounds. Both are deliberately short: they run after the promoter's loop has already
// returned, so all they do is flush and close.
const (
	observabilityFlushTimeout = 5 * time.Second
	healthShutdownTimeout     = 5 * time.Second
)

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

	// A cancelled context is how the promoter's loop reports a SIGINT/SIGTERM, which is an ordinary
	// shutdown rather than a failure - reporting it as one would make every restart look like a
	// crash to a supervisor.
	if err := serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Errorf("ingestor exited with an error: %s", err.Error())

		return 1
	}

	log.Info("ingestor stopped")

	return 0
}

// registerFlags defines the two endpoints' connection flags plus the ingestor's own, parses args,
// and wires the HIPPOCAMPUS_INGESTOR_* environment overrides so tokens can be injected without
// appearing in argv.
func registerFlags(fs *pflag.FlagSet, args []string) error {
	client.RegisterFlags(fs, sourceEndpoint, "localhost:50051", "edge (source)")
	client.RegisterFlags(fs, targetEndpoint, "", "central (target)")

	fs.StringP("rules", "r", "", "path to the JSON rules file (required)")
	fs.Int("rules-refresh-seconds", 30, "how often the rules file is re-stat'ed for changes (0 selects the default)")
	fs.Int("interval-seconds", 30, "how often a pass runs")
	fs.Int("settle-seconds", 60, "how long an event must have been ended before it is judged")
	fs.Int32("page-size", 100, "page size for the event scan and the per-event memory reads")
	fs.Int("call-timeout-seconds", 30, "per-RPC timeout")
	fs.Int("max-batch-bytes", 3*1024*1024, "maximum estimated size of one ImportBatch call to the target")
	fs.Int("max-event-memories", promoter.DefaultMaxEventMemories, "an event holding more memories than this is left unjudged rather than judged on a truncated view of itself")
	fs.Uint64("rule-cost-limit", rules.DefaultCostLimit, "CEL cost budget for one rule evaluation")
	fs.Int("rule-timeout-seconds", 2, "wall-clock bound on one rule evaluation")
	fs.String("orphans", string(promoter.OrphanIgnore), "what to do with memories carrying no event: ignore, promote or drop")
	fs.Duration("orphan-age", time.Hour, "how old an event-less memory must be before --orphans acts on it")
	fs.Bool("dry-run", false, "judge and report without promoting, dropping or draining anything")
	fs.Bool("check-rules", false, "load and compile the rules file, print a summary, and exit (no connection is made)")
	fs.String("log-level", "info", "logging level (trace, debug, info, warn, error)")
	fs.Bool("version", false, "print the version and exit")

	// Observability. Metrics/tracing are off until an endpoint is wanted, matching the service's own
	// defaults; the probe listener is ON, because an ingestor that cannot be probed is an ingestor
	// whose stalling is invisible.
	fs.Bool("metrics", false, "export OTEL metrics over OTLP/gRPC")
	fs.Bool("tracing", false, "export OTEL traces over OTLP/gRPC")
	fs.Float64("tracing-sampling-ratio", 0.1, "fraction of locally started traces to sample")
	fs.String("otlp-endpoint", "", "OTLP/gRPC collector endpoint (empty uses the OTEL_EXPORTER_OTLP_* environment variables, then localhost:4317)")
	fs.Bool("otlp-insecure", true, "connect to the collector without TLS")
	fs.Int("metrics-interval-seconds", 0, "OTEL metric export interval (0 selects the SDK default)")
	fs.String("metrics-group", "", "tenancy label stamped on this process's telemetry as a resource attribute (see docs/ingestor.md)")
	fs.Int("health-port", 8090, "port serving /healthz and /readyz (0 disables)")
	fs.String("health-bind-address", "", "interface for the health listener (empty binds all)")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_INGESTOR")
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

// run reads config from viper, loads the rules, dials both instances and runs the promoter until ctx
// is cancelled. All viper access lives here, in main, per the repo convention.
func run(ctx context.Context) error {
	rulesPath := viper.GetString("rules")
	if rulesPath == "" {
		return fmt.Errorf("--rules is required")
	}

	orphans := promoter.OrphanPolicy(viper.GetString("orphans"))
	if !promoter.ValidOrphanPolicy(orphans) {
		return fmt.Errorf(
			"invalid --orphans %q (want %q, %q or %q)",
			orphans,
			promoter.OrphanIgnore,
			promoter.OrphanPromote,
			promoter.OrphanDrop,
		)
	}

	ruleOpts := rules.Options{
		CostLimit:   viper.GetUint64("rule-cost-limit"),
		EvalTimeout: time.Duration(viper.GetInt("rule-timeout-seconds")) * time.Second,
	}

	// --check-rules is a rules linter: it compiles the file and exits, so a bad expression is found
	// before it is deployed rather than at the next reload of a running ingestor. It touches neither
	// instance, so it needs no addresses and no tokens.
	if viper.GetBool("check-rules") {
		return checkRules(rulesPath, ruleOpts)
	}

	targetAddress := viper.GetString(client.Key(targetEndpoint, "address"))
	if targetAddress == "" {
		return fmt.Errorf("--target-address is required")
	}

	// Observability is installed BEFORE anything is instrumented, so the very first pass is
	// measured. The instruments themselves are built from the global providers and are no-ops until
	// this runs, which is what makes every recording site safe to leave unconditional.
	shutdownObservability, err := observability.Init(ctx, observability.Config{
		TracingEnabled:         viper.GetBool("tracing"),
		TracingSamplingRatio:   viper.GetFloat64("tracing-sampling-ratio"),
		MetricsEnabled:         viper.GetBool("metrics"),
		MetricsIntervalSeconds: viper.GetInt("metrics-interval-seconds"),
		OTLPEndpoint:           viper.GetString("otlp-endpoint"),
		OTLPInsecure:           viper.GetBool("otlp-insecure"),
		ServiceName:            "hippocampus-ingestor",
		ServiceVersion:         version,
		Group:                  viper.GetString("metrics-group"),
	})
	if err != nil {
		return fmt.Errorf("initialising observability: %w", err)
	}

	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), observabilityFlushTimeout)
		defer cancel()

		if err := shutdownObservability(flushCtx); err != nil {
			log.Errorf("flushing observability: %s", err.Error())
		}
	}()

	// The rules are loaded BEFORE either connection: a broken rules file must stop the process
	// here, with a message about the rules, rather than after two dials have obscured it.
	watcher, err := rules.NewWatcher(
		rulesPath,
		ruleOpts,
		time.Duration(viper.GetInt("rules-refresh-seconds"))*time.Second,
	)
	if err != nil {
		return fmt.Errorf("loading rules: %w", err)
	}

	defer watcher.Stop()

	sourceConn, source, err := client.Dial(endpointConfig(sourceEndpoint))
	if err != nil {
		return fmt.Errorf("dialling the source instance: %w", err)
	}

	defer func() { _ = sourceConn.Close() }()

	targetConn, target, err := client.Dial(endpointConfig(targetEndpoint))
	if err != nil {
		return fmt.Errorf("dialling the target instance: %w", err)
	}

	defer func() { _ = targetConn.Close() }()

	// Both connections exist by now, so readiness can report which END is unreachable rather than
	// only that something is. The checks call the gRPC health service, which needs no token and is
	// driven on the service side by its own database readiness - so "ready" here means the far end
	// can actually serve, not merely that a socket opened.
	health := observability.NewHealthServer(observability.HealthConfig{
		Port:        viper.GetInt("health-port"),
		BindAddress: viper.GetString("health-bind-address"),
		Version:     version,
		Component:   "hippocampus-ingestor",
		Checks: map[string]observability.Check{
			sourceEndpoint: observability.GRPCHealthCheck(sourceConn),
			targetEndpoint: observability.GRPCHealthCheck(targetConn),
		},
	})

	if err := health.Start(); err != nil {
		return fmt.Errorf("starting the health endpoints: %w", err)
	}

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), healthShutdownTimeout)
		defer cancel()

		if err := health.Shutdown(shutdownCtx); err != nil {
			log.Errorf("stopping the health endpoints: %s", err.Error())
		}
	}()

	cfg := promoter.Config{
		Interval:         time.Duration(viper.GetInt("interval-seconds")) * time.Second,
		Settle:           time.Duration(viper.GetInt("settle-seconds")) * time.Second,
		PageSize:         viper.GetInt32("page-size"),
		CallTimeout:      time.Duration(viper.GetInt("call-timeout-seconds")) * time.Second,
		MaxBatchBytes:    viper.GetInt("max-batch-bytes"),
		MaxEventMemories: viper.GetInt("max-event-memories"),
		Orphans:          orphans,
		OrphanAge:        viper.GetDuration("orphan-age"),
		DryRun:           viper.GetBool("dry-run"),
	}

	log.WithFields(log.Fields{
		"version": version,
		"source":  viper.GetString(client.Key(sourceEndpoint, "address")),
		"target":  targetAddress,
		"rules":   rulesPath,
		"dry_run": cfg.DryRun,
		"orphans": string(orphans),
	}).
		Info("ingestor starting")

	if cfg.DryRun {
		log.Warn("running with --dry-run: events are judged and reported, and nothing is promoted, dropped or drained")
	}

	return promoter.New(source, target, watcher, cfg).Run(ctx)
}

// checkRules compiles the rules file and reports what it holds, without connecting to anything.
func checkRules(path string, opts rules.Options) error {
	set, err := rules.Load(path, opts)
	if err != nil {
		return err
	}

	fmt.Printf(
		"%s: %d rule(s), default action '%s', reads memory bodies: %t\n",
		path,
		set.Rules(),
		set.DefaultAction(),
		set.NeedsMemories(),
	)

	return nil
}

// endpointConfig reads one endpoint's connection settings from viper. The endpoint name is carried
// through onto the connection so every RPC it makes is attributed to the right end in the metrics.
func endpointConfig(endpoint string) client.Config {
	return client.Config{
		Endpoint:              endpoint,
		Address:               viper.GetString(client.Key(endpoint, "address")),
		Token:                 viper.GetString(client.Key(endpoint, "token")),
		TLS:                   viper.GetBool(client.Key(endpoint, "tls")),
		TLSCACertFile:         viper.GetString(client.Key(endpoint, "tls-ca-cert")),
		TLSCertFile:           viper.GetString(client.Key(endpoint, "tls-cert")),
		TLSKeyFile:            viper.GetString(client.Key(endpoint, "tls-key")),
		TLSInsecureSkipVerify: viper.GetBool(client.Key(endpoint, "tls-insecure-skip-verify")),
	}
}
