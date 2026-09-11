// Command object-gateway fronts an object-storage bucket and reinforces the pointer-memory behind
// every object it serves.
//
// It is the recall tap for the retention-controller deployment: the payload stays in the bucket,
// Hippocampus holds one pointer-memory per object and decides what survives, and this is what tells
// it which objects are still being read. Without a tap the controller has significance, age, links
// and capacity pressure to work with, but not the one input no expiry policy anywhere has.
//
// See docs/objectstore.md.
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

	"github.com/fastbean-au/hippocampus/integrations/objectstore/client"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/gateway"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/httpserve"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/tap"
)

// version is stamped into logs and the probe body; a release build overrides it with
// -ldflags "-X main.version".
var version = "dev"

const component = "hippocampus-object-gateway"

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

	// A cancelled context is how a SIGINT/SIGTERM arrives, which is an ordinary shutdown rather
	// than a failure - reporting it as one would make every restart look like a crash.
	if err := serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Errorf("the object gateway exited with an error: %s", err.Error())

		return 1
	}

	log.Info("object gateway stopped")

	return 0
}

func registerFlags(fs *pflag.FlagSet, args []string) error {
	client.RegisterCommonFlags(fs, 8090)

	fs.Int("listen-port", 8088, "port serving the object routes")
	fs.String("bind-address", "", "interface for the object listener (empty binds all)")
	fs.String("listen-tls-cert", "", "certificate for serving the object routes over HTTPS (requires --listen-tls-key)")
	fs.String("listen-tls-key", "", "private key for serving the object routes over HTTPS (requires --listen-tls-cert)")
	fs.String("mode", string(gateway.ModeRedirect), "how a read is answered: redirect (presigned URL, no payload passes through this process) or proxy")
	fs.Int("url-ttl-seconds", 300, "how long a presigned URL stays valid, in redirect mode")
	fs.String("path-prefix", "/o/", "path objects are served under")
	fs.String("auth-token", "", "bearer token required on every inbound request (see --allow-anonymous)")
	fs.Bool("allow-anonymous", false, "serve without an inbound token: anybody who can reach this gateway can read any object in the bucket")
	fs.Int("recall-batch-size", 100, "distinct object ids buffered before one RecallMemories call (0 recalls each read immediately)")
	fs.Int("recall-batch-window-ms", 2000, "how long a partial batch waits before being flushed")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_OBJECT_GATEWAY")
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

// run reads the configuration, builds the pieces and serves until ctx is cancelled. All viper
// access lives here, in main, per the repo convention.
func run(ctx context.Context) error {
	bucket := viper.GetString("bucket")
	if bucket == "" {
		return fmt.Errorf("--bucket is required")
	}

	mode := gateway.Mode(viper.GetString("mode"))
	if !gateway.ValidMode(mode) {
		return fmt.Errorf("invalid --mode %q (want %q or %q)", mode, gateway.ModeRedirect, gateway.ModeProxy)
	}

	authToken := viper.GetString("auth-token")

	// Refused rather than warned about, because of what the gateway hands out: a presigned URL
	// grants a read of an object to whoever holds it, so an unauthenticated gateway is a public
	// read endpoint for the whole bucket. That may be exactly what a deployment wants - a bucket of
	// public assets whose retention is being managed - which is why the escape hatch exists; it
	// just should not be reachable by leaving a flag unset.
	if authToken == "" && !viper.GetBool("allow-anonymous") {
		return fmt.Errorf("--auth-token is required, or pass --allow-anonymous to serve the bucket to anybody who can reach this gateway")
	}

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

	memories := client.NewMemories(hippo, time.Duration(viper.GetInt("call-timeout-seconds"))*time.Second)

	reads := tap.New(tap.Config{
		Recaller:  memories,
		BatchSize: viper.GetInt("recall-batch-size"),
		Window:    time.Duration(viper.GetInt("recall-batch-window-ms")) * time.Millisecond,
	})

	front, err := gateway.New(gateway.Config{
		Store:      store,
		Tap:        reads,
		Mode:       mode,
		URLTTL:     time.Duration(viper.GetInt("url-ttl-seconds")) * time.Second,
		AuthToken:  authToken,
		PathPrefix: viper.GetString("path-prefix"),
	})
	if err != nil {
		return fmt.Errorf("building the gateway: %w", err)
	}

	// Readiness covers the BUCKET and deliberately not the Hippocampus instance, which is the
	// opposite of what the broker bridges do and for the same underlying reason: readiness should
	// name the dependency the component's actual job needs. A bridge's job is writing to the
	// service, so a service it cannot reach makes it unready. This gateway's job is serving
	// objects, and reinforcement is secondary - taking it out of a load balancer because a memory
	// store is down would turn a lost decay-clock update into an outage for every reader. The tap's
	// own failure shows up as a hit rate of zero and a Warn line instead.
	health := observability.NewHealthServer(observability.HealthConfig{
		Port:           viper.GetInt("health-port"),
		BindAddress:    viper.GetString("health-bind-address"),
		Version:        version,
		Component:      component,
		MetricsHandler: observability.PrometheusHandler(),
		Checks: map[string]observability.Check{
			"bucket": func(ctx context.Context) error { return store.Ping(ctx) },
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

	go reads.Run(ctx)

	log.WithFields(log.Fields{
		"bucket":       bucket,
		"mode":         mode,
		"address":      viper.GetString("address"),
		"anonymous":    authToken == "",
		"batch_size":   viper.GetInt("recall-batch-size"),
		"path_prefix":  viper.GetString("path-prefix"),
		"listen_port":  viper.GetInt("listen-port"),
		"version":      version,
		"url_ttl_secs": viper.GetInt("url-ttl-seconds"),
	}).
		Info("object gateway starting")

	return httpserve.Serve(ctx, httpserve.Config{
		Handler:     front.Handler(),
		Name:        "objects",
		BindAddress: viper.GetString("bind-address"),
		Port:        viper.GetInt("listen-port"),
		TLSCertFile: viper.GetString("listen-tls-cert"),
		TLSKeyFile:  viper.GetString("listen-tls-key"),

		// No write timeout: proxy mode streams objects, and one large enough to take longer than
		// any figure chosen here is exactly the kind this gateway exists to serve. The read-header
		// timeout is what bounds a slow client holding a connection open for nothing.
		WriteTimeout:    0,
		ShutdownTimeout: serverShutdownTimeout,
	})
}
