// Command object-reaper deletes the object behind a memory Hippocampus has forgotten.
//
// It is the actuator for the retention-controller deployment: the decay cycle decides, and this
// carries the decision out in the bucket. Instructions reach it three ways - a memory_forgotten
// callback as the cycle runs, the forgotten log read back over a window at startup, and a reverse
// sweep of the bucket - because a dropped instruction is not a missed notification but a payload
// nothing will ever mention again.
//
// It does NOT delete by default. --delete arms it; without it every deletion is selected, counted
// and logged, which is how a deployment compares this controller's judgement against whatever flat
// expiry it is replacing before making it authoritative over data the store cannot see.
//
// See docs/objectstore.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/observability"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/client"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/httpserve"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/reap"
)

// version is stamped into logs and the probe body; a release build overrides it with
// -ldflags "-X main.version".
var version = "dev"

const component = "hippocampus-object-reaper"

// Shutdown bounds. Each runs after the serving loop has returned, so all they do is drain and flush.
const (
	serverShutdownTimeout     = 10 * time.Second
	healthShutdownTimeout     = 5 * time.Second
	observabilityFlushTimeout = 5 * time.Second
)

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain is the testable body of main: it registers flags, installs the signal handler and runs
// serve, returning an exit code rather than calling os.Exit itself.
func realMain(args []string) int {
	if err := registerFlags(pflag.CommandLine, args); err != nil {
		log.Errorf("failed to register command line flags: %s", err.Error())

		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Errorf("the object reaper exited with an error: %s", err.Error())

		return 1
	}

	log.Info("object reaper stopped")

	return 0
}

func registerFlags(fs *pflag.FlagSet, args []string) error {
	client.RegisterCommonFlags(fs, 8091)

	fs.Bool("delete", false, "actually delete objects; without it the reaper runs in shadow mode, reporting what it would delete")
	fs.String("causes", "", "comma-separated deletion causes to act on (default consolidation,eviction,cascade; see docs/objectstore.md)")

	// The push path.
	fs.Int("listen-port", 8089, "port serving the callback endpoint (0 disables the push path)")
	fs.String("bind-address", "", "interface for the callback listener (empty binds all)")
	fs.String("callback-path", "/callbacks", "path the service's callback sink posts to")
	fs.String("callback-token", "", "bearer token required on every delivery, matching callbacks.token on the service")
	fs.String("callback-secret", "", "shared secret the delivery signature is verified with, matching callbacks.signingSecret on the service")
	fs.Int("callback-max-age-seconds", 300, "how old a signed delivery may be before it is refused as a replay")
	fs.String("listen-tls-cert", "", "certificate for serving the callback endpoint over HTTPS (requires --listen-tls-key)")
	fs.String("listen-tls-key", "", "private key for serving the callback endpoint over HTTPS (requires --listen-tls-cert)")

	// The pull path.
	fs.Duration("catch-up", time.Hour, "at startup, reap everything the forgotten log says went within this window (0 disables)")

	// The sweep.
	fs.Duration("sweep-interval", 0, "how often to sweep the bucket for objects the store no longer remembers (0 disables)")
	fs.Duration("sweep-min-age", 24*time.Hour, "how long an object must have existed before the sweep will judge it; cannot be zero")
	fs.String("sweep-prefix", "", "restrict the sweep to one prefix of the bucket")
	fs.Bool("sweep-now", false, "run one sweep, report, and exit (no listener; the shadow-mode dry run)")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_OBJECT_REAPER")
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

// agent is what run assembles: the pieces the three paths share.
type agent struct {
	memories *client.Memories
	reaper   *reap.Reaper
	sweep    *reap.Sweep
}

// validate checks everything that can be judged without touching the bucket or the network, and
// returns the parsed cause set so run need not read it twice. It is a separate function so the
// refusals can be tested without a service to dial.
func validate() (reap.Causes, error) {
	if viper.GetString("bucket") == "" {
		return nil, fmt.Errorf("--bucket is required")
	}

	// A reaper with every path disabled would start, pass its probes and do nothing at all, which
	// is the one failure an operator has no way to see: there is no error, no backlog and no
	// metric that moves, only objects quietly outliving their memories.
	if !viper.GetBool("sweep-now") &&
		viper.GetInt("listen-port") == 0 &&
		viper.GetDuration("catch-up") <= 0 &&
		viper.GetDuration("sweep-interval") <= 0 {
		return nil, fmt.Errorf("every path is disabled: set --listen-port, --catch-up or --sweep-interval")
	}

	causes, err := reap.NewCauses(viper.GetString("causes"))
	if err != nil {
		return nil, fmt.Errorf("invalid --causes: %w", err)
	}

	return causes, nil
}

// run reads the configuration, builds the pieces and runs until ctx is cancelled. All viper access
// lives here, in main, per the repo convention.
func run(ctx context.Context) error {
	causes, err := validate()
	if err != nil {
		return err
	}

	bucket := viper.GetString("bucket")

	shutdownObservability, err := observability.Init(ctx, observability.Config{
		TracingEnabled:         viper.GetBool("tracing"),
		TracingSamplingRatio:   viper.GetFloat64("tracing-sampling-ratio"),
		MetricsEnabled:         viper.GetBool("metrics"),
		MetricsIntervalSeconds: viper.GetInt("metrics-interval-seconds"),
		OTLPEndpoint:           viper.GetString("otlp-endpoint"),
		OTLPInsecure:           viper.GetBool("otlp-insecure"),
		PrometheusEnabled:      viper.GetBool("prometheus"),
		ServiceName:            component,
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

	store, err := objects.NewS3Store(ctx, objects.Config{
		Endpoint:     viper.GetString("s3-endpoint"),
		Region:       viper.GetString("s3-region"),
		Bucket:       bucket,
		UsePathStyle: viper.GetBool("s3-path-style"),
	})
	if err != nil {
		return fmt.Errorf("opening the bucket: %w", err)
	}

	conn, hippo, err := client.Dial(client.Config{
		Address:               viper.GetString("address"),
		Token:                 viper.GetString("token"),
		TLS:                   viper.GetBool("tls"),
		TLSCACertFile:         viper.GetString("tls-ca-cert"),
		TLSCertFile:           viper.GetString("tls-cert"),
		TLSKeyFile:            viper.GetString("tls-key"),
		TLSInsecureSkipVerify: viper.GetBool("tls-insecure-skip-verify"),
		Endpoint:              "hippocampus",
	})
	if err != nil {
		return fmt.Errorf("dialling the hippocampus service: %w", err)
	}

	defer func() { _ = conn.Close() }()

	reaper, err := reap.New(reap.Config{
		Store:  store,
		Delete: viper.GetBool("delete"),
		Causes: causes,
	})
	if err != nil {
		return fmt.Errorf("building the reaper: %w", err)
	}

	memories := client.NewMemories(hippo, time.Duration(viper.GetInt("call-timeout-seconds"))*time.Second)

	sweep, err := reap.NewSweep(reap.SweepConfig{
		Store:    store,
		Memories: memories,
		Reaper:   reaper,
		MinAge:   viper.GetDuration("sweep-min-age"),
		Prefix:   viper.GetString("sweep-prefix"),
	})
	if err != nil {
		return fmt.Errorf("building the sweep: %w", err)
	}

	agent := &agent{
		memories: memories,
		reaper:   reaper,
		sweep:    sweep,
	}

	announce(reaper, bucket)

	// --sweep-now is the shadow-mode dry run: one pass, a report, and out. It needs no listener and
	// no probes, so it is answered before either is started.
	if viper.GetBool("sweep-now") {
		_, err := agent.sweep.Run(ctx)

		return err
	}

	health := observability.NewHealthServer(observability.HealthConfig{
		Port:           viper.GetInt("health-port"),
		BindAddress:    viper.GetString("health-bind-address"),
		Version:        version,
		Component:      component,
		MetricsHandler: observability.PrometheusHandler(),

		// Both ends, unlike the gateway's: this agent's job needs the store (to be told what went,
		// and to ask what is still held) as well as the bucket (to delete), so either being
		// unreachable means it cannot do it.
		Checks: map[string]observability.Check{
			"hippocampus": observability.GRPCHealthCheck(conn),
			"bucket":      func(ctx context.Context) error { return store.Ping(ctx) },
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

	// The catch-up runs before the listener opens, deliberately: what it recovers is exactly what
	// was missed while this process was not listening, and running it first means the window it
	// covers ends where the push path begins rather than overlapping it by however long startup
	// takes.
	if err := agent.catchUp(ctx); err != nil {
		// Not fatal. A catch-up that cannot run - a store with no forgotten log, an instance still
		// starting - must not stop the push path from coming up, since the push path is what stops
		// the backlog growing any further.
		log.WithError(err).Warn("the catch-up pass failed; the push path and the sweep still apply")
	}

	go agent.sweepPeriodically(ctx, viper.GetDuration("sweep-interval"))

	if viper.GetInt("listen-port") == 0 {
		log.Info("no callback listener configured; running the sweep alone")

		<-ctx.Done()

		return ctx.Err()
	}

	return agent.listen(ctx)
}

// announce says what the agent will actually do, at Info, because the difference between shadow and
// armed is the difference between a report and somebody's data.
func announce(reaper *reap.Reaper, bucket string) {
	fields := log.Fields{
		"bucket":  bucket,
		"address": viper.GetString("address"),
		"version": version,
		"causes":  viper.GetString("causes"),
	}

	if !reaper.Armed() {
		log.WithFields(fields).
			Info("object reaper starting in SHADOW mode: it will report what it would delete and delete nothing (--delete arms it)")

		return
	}

	log.WithFields(fields).
		Warn("object reaper starting ARMED: it will delete objects in the bucket as their memories are forgotten")
}

// catchUp runs the pull path once.
func (a *agent) catchUp(ctx context.Context) error {
	window := viper.GetDuration("catch-up")

	catchUp, err := reap.NewCatchUp(a.reaper, a.memories, window)
	if err != nil {
		return err
	}

	_, err = catchUp.Run(ctx)

	return err
}

// sweepPeriodically runs the reverse sweep on its interval until ctx is cancelled.
func (a *agent) sweepPeriodically(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-ticker.C:
			if _, err := a.sweep.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.WithError(err).Error("the reverse sweep failed; it will be retried on the next interval")
			}

		}
	}
}

// listen serves the callback endpoint until ctx is cancelled.
func (a *agent) listen(ctx context.Context) error {
	receiver, err := reap.NewReceiver(reap.ReceiverConfig{
		Reaper: a.reaper,
		Token:  viper.GetString("callback-token"),
		Secret: viper.GetString("callback-secret"),
		MaxAge: time.Duration(viper.GetInt("callback-max-age-seconds")) * time.Second,
	})
	if err != nil {
		return fmt.Errorf("building the callback receiver: %w", err)
	}

	if viper.GetString("callback-token") == "" && viper.GetString("callback-secret") == "" {
		log.Warn("the callback endpoint is unauthenticated: anything that can reach it can instruct " +
			"this agent to delete objects (set --callback-token, --callback-secret, or both)")
	}

	path := viper.GetString("callback-path")

	mux := http.NewServeMux()
	mux.Handle(path, receiver)

	log.WithFields(log.Fields{
		"port": viper.GetInt("listen-port"),
		"path": path,
	}).
		Info("listening for forget-callbacks")

	return httpserve.Serve(ctx, httpserve.Config{
		Handler:         mux,
		Name:            "callbacks",
		BindAddress:     viper.GetString("bind-address"),
		Port:            viper.GetInt("listen-port"),
		TLSCertFile:     viper.GetString("listen-tls-cert"),
		TLSKeyFile:      viper.GetString("listen-tls-key"),
		WriteTimeout:    30 * time.Second,
		ShutdownTimeout: serverShutdownTimeout,
	})
}
