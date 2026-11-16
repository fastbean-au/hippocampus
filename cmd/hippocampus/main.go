package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/hippocampus"
	"github.com/fastbean-au/hippocampus/stats"
)

func main() {
	execute(os.Args[1:])
}

// execute is main() with its process-owned inputs made injectable: it takes the argument slice
// rather than reading os.Args, and returns on the CLI-mode success paths (--version/--mint-token/
// --backfill-search) rather than calling os.Exit, so the whole startup dispatch can be driven
// in-process by a test. The fatal-error paths keep their log.Panicf/log.Fatalf calls; a test traps
// those through logrus's ExitFunc (see withFatalPanic) or a recovered panic.
func execute(args []string) {
	flags := pflag.NewFlagSet("hippocampus", pflag.ContinueOnError)
	flags.StringP("config_file", "c", "./config.json", "path to configuration file")
	flags.Int("port", 50051, "gRPC server listen port (overrides the config file's \"port\")")
	flags.Int("gateway-port", 0, "HTTP/JSON gateway listen port; 0 (the default) disables the gateway. 8080 is the conventional port (overrides the config file's \"gateway.port\")")
	flags.Bool("version", false, "print the build version and exit")
	flags.Bool("mint-token", false, "mint a signed auth token from the configured signing secret and exit")
	flags.String("client-id", "", "client_id claim to embed in a minted token (used with --mint-token)")
	flags.StringSlice("role", nil, "authorisation role(s) to embed in a minted token: reader, writer, and/or admin (repeatable or comma-separated; used with --mint-token)")
	flags.StringSlice("group", nil, "group label(s) to scope a minted token to, restricting it to records carrying them (repeatable or comma-separated; omit for an unscoped token that sees the whole store; used with --mint-token)")
	flags.Duration("ttl", 24*time.Hour, "token lifetime (used with --mint-token)")
	flags.String("signing-secret", "", "override auth.signingSecret from the config file (used with --mint-token)")
	flags.String("kid", "", "signing-key id to stamp on a minted token; defaults to auth.activeKid or the first auth.signingKeys entry (used with --mint-token)")
	flags.Bool("schema-version", false, "print the configured store's schema version and exit; exits non-zero if the store is newer than this build")
	flags.Bool("check-config", false, "validate the resolved configuration and exit; exits non-zero if the service would refuse to start on it")
	flags.String("output", "text", "output format for --schema-version and --check-config: text or json")
	flags.String("backup", "", "write a consistent copy of the SQLite store to this path and exit; safe beside a running instance (for postgres/mysql use pg_dump or mysqldump)")
	flags.Bool("backfill-search", false, "rebuild the opensearch content-search index from the primary store and exit")
	flags.Bool("reindex", false, "delete and recreate the index before backfilling, removing stale entries (used with --backfill-search)")
	flags.Int("backfill-batch-size", 500, "memories read from the primary store per batch (used with --backfill-search)")

	// --help is not an error: pflag prints usage and returns ErrHelp, which should exit cleanly.
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return
		}

		log.Panicf("failed to parse command line flags: %s", err.Error())
	}

	if err := viper.BindPFlags(flags); err != nil {
		log.Panicf("failed to bind command line flags: %s", err.Error())
	}

	// The gateway port lives under the nested config key "gateway.port", which a flat flag name
	// cannot reach through BindPFlags, so bind it explicitly. The gRPC "port" flag maps to its
	// config key directly. For both, an explicit flag beats the config file, which beats the flag
	// default (50051 / 8080).
	if err := viper.BindPFlag("gateway.port", flags.Lookup("gateway-port")); err != nil {
		log.Panicf("failed to bind gateway port flag: %s", err.Error())
	}

	version := readVersionInfo()

	// --version is a CLI mode that needs nothing else: print the build identification to stdout and
	// exit before the config file is even read, mirroring --mint-token/--backfill-search but earlier.
	if viper.GetBool("version") {
		fmt.Println(version.String())

		return
	}

	configFile := viper.GetString("config_file")

	// A config file missing from the *default* path is not an error: every key the service cannot
	// run without now carries a viper.SetDefault (see setStartupDefaults), so `go run
	// ./cmd/hippocampus` on a fresh clone starts rather than panicking on a file the newcomer has
	// not written yet. A --config_file the operator named explicitly is still fatal when it is
	// absent - they pointed at a configuration, and quietly starting on defaults instead of it
	// would run a service that is not the one they configured. Every other read error (a
	// directory, no permission) stays fatal on both paths.
	configMissing := false

	c, err := os.ReadFile(configFile)

	switch {

	case err == nil:
		viper.SetConfigType("json")

		// A discarded parse error here would start the service with an all-zero config: auth off,
		// storage.directory empty (an in-memory SQLite database — every write lost on restart), and
		// consolidation.unitsOfAgeInDays 0. Fail fast, matching the os.ReadFile handling
		// above.
		if err := viper.ReadConfig(bytes.NewBuffer(c)); err != nil {
			log.Panicf("failed to parse config file '%s': %s", configFile, err.Error())
		}

	case errors.Is(err, os.ErrNotExist) && !flags.Changed("config_file"):
		configMissing = true

	default:
		log.Panicf("failed to read config file '%s': %s", configFile, err.Error())
	}

	// Let environment variables override the config file, so secrets (auth.signingSecret,
	// opensearch.password, transfer.token, the postgres/mysql DSN passwords) can be injected as
	// container/Kubernetes secrets rather than baked into a committed config.json. Called after
	// ReadConfig; viper's precedence is flag > env > file > default regardless.
	configureEnvOverrides()

	initLogging(viper.GetString("logging.level"), viper.GetBool("logging.json"))

	// --mint-token is a CLI mode, not part of normal startup: it only needs the signing secret,
	// so it runs before anything else, including the two log lines below - logging in this
	// service writes to stdout (see initLogging), and the token must be the only thing on stdout
	// for `token=$(hippocampus --mint-token ...)` to work.
	if viper.GetBool("mint-token") {
		// Minting is HMAC-only: under the idp method the identity provider issues tokens, and a
		// locally minted HS256 token would be rejected by the RS256-pinned verifier anyway.
		if viper.GetString("auth.method") == "idp" {
			log.Fatal("--mint-token is not available with auth.method 'idp' - the identity provider issues tokens")
		}

		secret, kid := resolveMintKey(hmacConfigFromViper())

		roles := viper.GetStringSlice("role")

		// A token with no role authorises no Hippocampus RPC under the default-closed policy, so
		// minting one is almost certainly a mistake - fail loudly rather than hand back a dead token.
		if len(roles) == 0 {
			log.Fatal("--mint-token requires at least one --role (reader, writer, and/or admin); a role-less token is denied every RPC")
		}

		// Unlike --role, no --group is a legitimate and common request: it mints an unscoped token,
		// which is what every token was before group scoping existed. It is also the most
		// privileged shape a token has, so the fact is reported on the stderr line below rather
		// than left to be inferred from a missing field.
		groups := viper.GetStringSlice("group")

		token, err := auth.MintToken(auth.MintRequest{
			Secret:   secret,
			Kid:      kid,
			ClientID: viper.GetString("client-id"),
			Roles:    roles,
			Groups:   groups,
			TTL:      viper.GetDuration("ttl"),
		})
		if err != nil {
			log.Fatalf("failed to mint token: %s", err.Error())
		}

		fmt.Println(token)

		// The jti goes to stderr - not stdout, which must carry only the token so
		// `token=$(hippocampus --mint-token ...)` works - so an operator can record it for later
		// per-token revocation.
		if id, err := auth.TokenID(token); err == nil {
			scope := "unscoped (whole store)"
			if len(groups) > 0 {
				scope = strings.Join(groups, ",")
			}

			fmt.Fprintf(os.Stderr, "jti=%s client_id=%s roles=%s groups=%s\n", id, viper.GetString("client-id"), strings.Join(roles, ","), scope)
		}

		return
	}

	// Before the defaults, and before every CLI mode below, because all of them - and the running
	// service - read the llm.* names only. See resolveLLMAliases for why the ordering is not
	// negotiable. The warning it earns is deliberately not emitted here: --check-config and
	// --schema-version render to stdout, which logging also writes to, so it waits with the
	// configMissing line below for every rendering mode to have returned.
	legacyLLMKeys := resolveLLMAliases()

	setStartupDefaults()

	// --check-config is a CLI mode like --mint-token: it validates the resolved configuration and
	// exits without opening the store or starting the server (see checkconfig.go).
	//
	// It runs BEFORE the configMissing warning below rather than after it, for the reason that mode
	// documents about its own output: logging writes to stdout, and --output json makes stdout a
	// data channel, so a Warn line emitted first would sit above the document and make it
	// unparseable. Nothing is lost by moving ahead of it - the report states whether it is checking
	// a file or the built-in defaults, which is what that warning would have said.
	if viper.GetBool("check-config") {
		output := viper.GetString("output")
		if output != "text" && output != "json" {
			log.Fatalf("--output %q is not a known format (expected 'text' or 'json')", output)
		}

		checkConfig(checkConfigConfig{
			ConfigFile:    configFile,
			ConfigMissing: configMissing,
			Deprecated:    legacyLLMKeys,
			Driver:        viper.GetString("storage.driver"),
			JSON:          output == "json",
		})

		return
	}

	// --schema-version is a CLI mode like --mint-token: it reads the store's recorded schema version
	// and exits without starting the server (see schema.go). It runs before validateConfig, because
	// the store's version is a question worth answering about a deployment whose configuration is
	// also wrong - and it reads nothing validateConfig checks.
	if viper.GetBool("schema-version") {
		output := viper.GetString("output")
		if output != "text" && output != "json" {
			log.Fatalf("--output %q is not a known format (expected 'text' or 'json')", output)
		}

		reportSchemaVersion(schemaVersionConfig{
			StorageDriver:    viper.GetString("storage.driver"),
			StorageDirectory: viper.GetString("storage.directory"),
			PostgresDSN:      viper.GetString("storage.postgres.dsn"),
			MySQLDSN:         viper.GetString("storage.mysql.dsn"),
			JSON:             output == "json",
		})

		return
	}

	// Said once, at Warn, and only here. Its position is load-bearing in both directions. It is
	// after the defaults are set, so it can report the values actually in force, and after every CLI
	// mode that RENDERS to stdout has returned - --mint-token, --check-config and --schema-version -
	// because logging in this service writes to stdout, and a Warn line above a document the caller
	// asked for as JSON makes it unparseable. Each of those modes points logging at stderr, but only
	// once it is running, which is too late for a line emitted before the dispatch.
	//
	// It stays ABOVE --backfill-search, which renders nothing and does want it: that mode runs for a
	// long time against a real store, and which store is exactly what an operator needs to know.
	//
	// Running on defaults is supported, but it is never what a deployment wants, so it must not be
	// silent.
	if configMissing {
		log.Warnf("no configuration file at '%s' - starting on built-in defaults: %s storage in '%s', gRPC on port %d, no authentication",
			configFile,
			viper.GetString("storage.driver"),
			viper.GetString("storage.directory"),
			viper.GetInt("port"),
		)
	}

	// Said once, here, for the reason the configMissing warning above gives about its own position.
	// A deprecated key that still works is exactly the kind of thing nobody discovers until the
	// release that stops honouring it, so it is Warn rather than Info and it names every key rather
	// than counting them.
	if len(legacyLLMKeys) > 0 {
		log.Warnf("the ollama.* configuration keys are deprecated and were renamed to llm.* in v0.42.0 - still reading %s; they will be honoured for now, but rename them",
			strings.Join(legacyLLMKeys, ", "),
		)
	}

	// --backup is a CLI mode like --backfill-search: it copies the store and exits without starting
	// the server (see backup.go). Below the configMissing warning for the same reason that mode is -
	// which store it read is exactly what an operator needs to know.
	if destination := viper.GetString("backup"); destination != "" {
		if err := runBackup(backupConfig{
			StorageDriver:    viper.GetString("storage.driver"),
			StorageDirectory: viper.GetString("storage.directory"),
			Destination:      destination,
		}); err != nil {
			log.Fatalf("backup failed: %s", err.Error())
		}

		return
	}

	// --backfill-search is a CLI mode like --mint-token: it rebuilds the content-search index
	// from the primary store and exits without starting the server (see backfill.go).
	if viper.GetBool("backfill-search") {
		// Without OpenSearch the index being rebuilt is the store's own, which is a different job
		// with different constraints (it writes to the service's own database), so it has its own
		// entry point rather than a flag inside this one.
		if !viper.GetBool("opensearch.enabled") {
			// The setting is passed through rather than left to default, because this mode opens
			// the store read-WRITE and so runs initSchema: without it the tool would recreate,
			// populate and leave behind the very index the service was configured to drop, and the
			// next service start would drop it again.
			contentIndex, _ := contentIndexSetting()

			rebuildContentSearch(backfillConfig{
				StorageDriver:    viper.GetString("storage.driver"),
				StorageDirectory: viper.GetString("storage.directory"),
				PostgresDSN:      viper.GetString("storage.postgres.dsn"),
				MySQLDSN:         viper.GetString("storage.mysql.dsn"),
				NoContentIndex:   !contentIndex,
			})

			return
		}

		backfillSearch(backfillConfig{
			StorageDriver:    viper.GetString("storage.driver"),
			StorageDirectory: viper.GetString("storage.directory"),
			PostgresDSN:      viper.GetString("storage.postgres.dsn"),
			MySQLDSN:         viper.GetString("storage.mysql.dsn"),
			Search:           searchConfigFromViper(),
			Reindex:          viper.GetBool("reindex"),
			BatchSize:        viper.GetInt("backfill-batch-size"),
			Embed:            backfillEmbedderFromViper(),
		})

		return
	}

	// Validate the consolidation and sleep config before touching the database or server. A
	// consolidation.unitsOfAgeInDays explicitly set to 0 makes the age term +Inf, which
	// collapses every decay method to a value of 0 and deletes every memory and event past the
	// minimum age on the first sleep cycle; the other checks guard against equally destructive or
	// runaway configurations. Fail fast rather than start and forget
	// everything.
	if err := validateConfig(); err != nil {
		log.Fatalf("invalid configuration: %s", err.Error())
	}

	// Turn SIGINT/SIGTERM into a cancelled context, so run's serve/shutdown lifecycle is driven by a
	// context a test can cancel too, rather than a signal only a real process receives.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, version); err != nil {
		log.Fatalf("hippocampus exited with an error: %s", err.Error())
	}
}

// setStartupDefaults applies every built-in default shared by normal startup and the
// --backfill-search CLI mode. It is a function rather than a run of statements inside execute so a
// test can assert what the service does with no configuration file at all - which, since these
// defaults are what makes that a supported way to start, is the property worth pinning.
func setStartupDefaults() {
	setStoreDefaults()
	setConsolidationDefaults()
	setObservabilityDefaults()
	setGatewayDefaults()
	setSearchDefaults()
	setArchiveDefaults()
	setCallbackDefaults()
	setLLMDefaults()
	setTopologyDefaults()

	viper.SetDefault("shutdown.timeoutSeconds", 10)

	// A listing's TotalCount is a second unbounded pass over the same predicate as its page, and
	// since the listing index made the page a walk of `limit` rows it is the larger half of a list
	// request by an order of magnitude. It cannot be answered with an index (one that speeds the
	// count slows the page far more - TODO 74.3), so it is cached for a few seconds instead. The
	// consequence is a total that can lag the page beside it, which is the same eventual consistency
	// the content-search index already has; 0 disables the cache and counts every time.
	viper.SetDefault("listing.countCacheSeconds", 5)
}

// run builds the server (observability, database, the optional search/S3/auth/TLS surface, the gRPC
// server, and the optional HTTP gateway), serves until ctx is cancelled - SIGINT/SIGTERM in main,
// or an explicit cancel in a test - or a listener fails, then shuts every component down in order.
// It returns an error for a bootstrap failure or a fatal serve error; a clean shutdown returns nil.
// Split out of main so the whole serve/shutdown lifecycle can be exercised in-process by a test,
// which main() (blocking on real signals) cannot be.
func run(ctx context.Context, version versionInfo) error {
	log.Info("initialising hippocampus")
	log.Infof("version: %s", version.String())
	log.Infof("logging level: %s", log.GetLevel())

	log.Debug("initialising observability")

	obsCfg := observabilityConfigFromViper(version)

	metricsServer, shutdownObservability, err := initObservability(obsCfg, metricsServerConfigFromViper())
	if err != nil {
		return err
	}

	log.Debug("observability initialised")

	// storage.driver selects the backend; sqlite (the default) preserves the embedded,
	// zero-dependency behaviour of every prior release.
	log.Debug("initialising database")

	storeCfg := storeConfigFromViper()

	database, err := initStore(storeCfg)
	if err != nil {
		return err
	}

	log.Debug("database initialised")

	searchIndex, err := initSearchIndex(searchIndexConfigFromViper(storeCfg.ContentIndex, storeCfg.ContentIndexReason), database)
	if err != nil {
		return err
	}

	summariser, err := initSummariser(summariserConfigFromViper())
	if err != nil {
		return err
	}

	embedder, err := initEmbedder(embedderSetupFromViper())
	if err != nil {
		return err
	}

	objects, err := initObjectStore(context.Background(), objectStoreConfigFromViper())
	if err != nil {
		return err
	}

	notifier, err := initNotifier(callbackSinkConfigFromViper())
	if err != nil {
		return err
	}

	// auth.method selects the verification scheme. The boolean auth.enabled predates it and remains
	// as a deprecated alias for "hmac", consulted only when auth.method is unset, so existing
	// configs keep working unchanged.
	authMethod := resolveAuthMethod()

	tlsConf, stopTLS, err := initServerTLS(serverTLSConfigFromViper())
	if err != nil {
		return err
	}

	defer stopTLS()

	authCfg, err := authConfigFromViper(authMethod)
	if err != nil {
		return err
	}

	authSet, err := initAuth(authCfg)
	if err != nil {
		return err
	}

	// Rate limiting: a hierarchy of token buckets (an instance-wide ceiling, a bucket per
	// authorisation tier, and a bucket per principal) that a request must pass every configured
	// level of. Off unless rateLimit.enabled, and a no-op even then until a level is given a rate.
	limiter, err := rateLimiterFromViper()
	if err != nil {
		authSet.stop()

		return fmt.Errorf("failed to initialise rate limiting: %w", err)
	}

	if limiter.Active() {
		registerRateLimitGauge(limiter)

		if !authSet.enabled() {
			log.Info("rate limiting is enabled with authentication off: per-client limits are keyed on the caller's address, and no per-tier limit applies (a tier comes from a token's roles)")
		}
	}

	log.Debug("initialising gRPC server")

	hipo := hippocampus.New(hippocampus.Dependencies{
		DB:         database,
		Search:     searchIndex,
		Objects:    objects,
		Summariser: summariser,
		Embedder:   embedder,
		Notifier:   notifier,
		Version:    version.Version,
	}, serverConfig(authMethod))

	// Both transports serve the audited server: every administrative mutation is one Info line with
	// audit=true, the caller and what it did (TODO-3 item 169). The gateway calls the server
	// directly and never runs the gRPC interceptor chain, so wrapping the served value is the one
	// place that reaches both.
	served := hippocampus.Audited(hipo)

	grpcCfg := grpcServerConfigFromViper(authMethod, obsCfg)

	s, hs := newGRPCServer(grpcCfg, grpcServerDeps{
		Hippocampus: hipo,
		Served:      served,
		Auth:        authSet,
		Limiter:     limiter,
		TLS:         tlsConf,
	})

	// A listener that fails to bind or serve sends its error here; run selects on it alongside
	// ctx.Done so such a failure ends the process (via main's log.Fatal on the returned error)
	// instead of the old in-goroutine log.Fatal, which a test could not observe.
	serveErr := make(chan error, 2)

	// Start listening. bindAddress (empty by default) restricts the interface the listener binds to
	// - e.g. "127.0.0.1" for loopback only behind a sidecar/mesh that terminates TLS; empty binds
	// all interfaces, the previous behaviour.
	go func() {
		lis, err := net.Listen("tcp", grpcCfg.BindAddress+":"+strconv.Itoa(grpcCfg.Port))
		if err != nil {
			serveErr <- fmt.Errorf("gRPC server failed to listen: %w", err)

			return
		}

		if err := s.Serve(lis); err != nil {
			serveErr <- fmt.Errorf("gRPC server failed to serve: %w", err)
		}
	}()

	hs.SetServingStatus("hippocampus", healthgrpc.HealthCheckResponse_SERVING)

	// Readiness is database-aware: unlike /healthz (pure process liveness) and the SERVING status
	// set above, it pings the store so an instance whose database has become unreachable is reported
	// not-ready instead of looking healthy while every RPC fails. The same probe drives the HTTP
	// /readyz endpoint (registered on the gateway below) and, via updateHealth, the gRPC serving
	// status. Timeout/cache windows fall back to internal defaults when unset.
	readiness := newReadinessProbe(
		database,
		time.Duration(viper.GetInt("readiness.pingTimeoutSeconds"))*time.Second,
		time.Duration(viper.GetInt("readiness.cacheSeconds"))*time.Second,
	)

	stopReadiness := make(chan struct{})
	readinessDone := readiness.updateHealth(hs, stopReadiness)

	// The HTTP/JSON gateway. A non-positive gateway.port disables it.
	var gwServer *http.Server

	if gwCfg := gatewayConfigFromViper(authMethod); gwCfg.Port > 0 {
		log.Debug("initialising HTTP gateway")

		handler, err := newGatewayHandler(gwCfg, gatewayDeps{
			Hippocampus: hipo,
			Served:      served,
			Auth:        authSet,
			Limiter:     limiter,
			Readiness:   readiness,
			Version:     version,
		})
		if err != nil {
			// The gRPC listener is already serving by this point, so returning straight out would
			// leave it bound and the database open for the life of the process. main turns this into
			// an exit, which hides that - a test calling run does not.
			close(stopReadiness)
			<-readinessDone
			s.Stop()
			_ = database.Close()

			return err
		}

		gwServer = newGatewayServer(gwCfg.BindAddress, gwCfg.Port, handler)

		go func() {
			var err error

			if tlsConf != nil {
				gwServer.TLSConfig = tlsConf

				err = gwServer.ListenAndServeTLS("", "")
			} else {
				err = gwServer.ListenAndServe()
			}

			if err != nil && err != http.ErrServerClosed {
				serveErr <- fmt.Errorf("HTTP gateway failed to serve: %w", err)
			}
		}()

		log.Debug("HTTP gateway initialised")
	} else {
		// Disabling the gateway removes far more than the JSON API - the web console, the OpenAPI
		// document, and the HTTP health probes a container or Kubernetes deployment needs all live
		// on it - and binding nothing looked identical to binding something that then failed. Say
		// what is off, and how to turn it on.
		log.Info("HTTP gateway disabled (gateway.port is 0): no JSON/HTTP API, no /ui console, no /v1/openapi.json, and no /healthz or /readyz probes. Set gateway.port (8080 is conventional) or pass --gateway-port to enable it")
	}

	// stats.intervalSeconds drives the periodic stats log line and bounds how often the shared count
	// cache re-queries the store; 0 disables the log line (the gauges still read a cached count).
	viper.SetDefault("stats.intervalSeconds", 300)
	statsStop := stats.Start(database, viper.GetInt("stats.intervalSeconds"))

	log.Info("hippocampus started")

	// Serve until a shutdown signal (ctx cancelled) or a fatal listener error. A serve error is
	// returned after the same orderly shutdown a signal triggers, so the process still drains
	// cleanly before main turns it into a non-zero exit.
	var runErr error

	select {

	case <-ctx.Done():
		log.Info("shutdown signal received - shutting down.")

	case err := <-serveErr:
		log.Errorf("server error - shutting down: %s", err.Error())

		runErr = err

	}

	// Mark not-serving before closing any listener, so load balancers and orchestrators drain this
	// instance ahead of the shutdown rather than racing an in-flight request into a closing server.
	// /readyz fails from here on, and the readiness updater is stopped first so it cannot flip the
	// gRPC status back to SERVING.
	readiness.drain()
	close(stopReadiness)
	<-readinessDone
	hs.SetServingStatus("hippocampus", healthgrpc.HealthCheckResponse_NOT_SERVING)

	// Each shutdown phase (gateway drain, gRPC graceful stop, observability flush) is bounded by the
	// same timeout so a stuck phase cannot hang the process; a slower store can raise it.
	shutdownTimeout := time.Duration(viper.GetInt("shutdown.timeoutSeconds")) * time.Second

	if gwServer != nil {
		gwCtx, gwCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer gwCancel()

		if err := gwServer.Shutdown(gwCtx); err != nil {
			log.Errorf("failed to shut down HTTP gateway cleanly: %s", err.Error())
		}
	}

	hs.Shutdown()

	// Stop gracefully so in-flight RPCs (e.g. a long Export/Transfer) finish, but bound it with a
	// timeout: a stuck call must not hang shutdown, so fall back to a hard Stop past the deadline.
	stopped := make(chan struct{})

	go func() {
		s.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		log.Warn("graceful shutdown timed out - forcing stop")
		s.Stop()
	}

	// The gRPC server has drained RPC-initiated sleep cycles; now stop the background sleep loop and
	// the stats ticker, waiting for any in-flight consolidation to finish, before the database is
	// closed underneath them.
	hipo.Stop()
	statsStop()

	authSet.stop()

	// The gRPC server is stopped, so no new index operations can be enqueued; drain whatever is
	// still queued before flushing observability, so the final export captures the search
	// counters.
	if err := searchIndex.Close(); err != nil {
		log.Errorf("failed to close search index cleanly: %s", err.Error())
	}

	// Flush observability before closing the DB: the final metric collection invokes the stats
	// gauge callbacks, which query the database.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Stop answering scrapes first. A collection triggered after the meter provider has been shut
	// down reports nothing, and a scraper reading that would record a store with no memories rather
	// than a store that is no longer there.
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Errorf("failed to shut down the metrics endpoint cleanly: %s", err.Error())
	}

	if err := shutdownObservability(shutdownCtx); err != nil {
		log.Errorf("failed to shut down observability cleanly: %s", err.Error())
	}

	_ = database.Close()

	return runErr
}

// resolveMintKey decides which signing secret and kid --mint-token should use. An explicit
// --signing-secret override wins (for minting on a host that carries only the secret, not the full
// config), staying kid-less unless --kid is also given. Otherwise the key comes from the config:
// --kid, else auth.activeKid, else the first configured signing key, else the legacy secret with no
// kid. A --kid naming no configured key yields an empty secret, so MintToken fails fast rather than
// silently signing with the wrong key.
func resolveMintKey(cfg auth.HMACConfig) (string, string) {
	if override := viper.GetString("signing-secret"); override != "" {
		return override, viper.GetString("kid")
	}

	kid := viper.GetString("kid")

	if kid == "" {
		kid = cfg.ActiveKid
	}

	if kid == "" && len(cfg.Keys) > 0 {
		kid = cfg.Keys[0].Kid
	}

	if kid == "" {
		return cfg.LegacySecret, ""
	}

	for _, v := range cfg.Keys {
		if v.Kid != kid {
			continue
		}

		return v.Secret, kid
	}

	return "", kid
}

// configureEnvOverrides wires viper to read environment variables so any config key can be
// overridden by HIPPOCAMPUS_<KEY> with dots replaced by underscores (e.g.
// HIPPOCAMPUS_AUTH_SIGNINGSECRET, HIPPOCAMPUS_STORAGE_POSTGRES_DSN, HIPPOCAMPUS_OPENSEARCH_PASSWORD,
// HIPPOCAMPUS_TRANSFER_TOKEN). This keeps secrets out of the committed/baked config file. The
// prefix scopes it so unrelated environment variables cannot collide with a config key.
func configureEnvOverrides() {
	viper.SetEnvPrefix("HIPPOCAMPUS")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()
}

// validateConfig rejects settings that would make the service behave destructively. It reads
// straight from viper (all viper access lives in cmd/hippocampus) and returns every problem it
// finds joined into one error, or nil when the configuration is sound. sleep.periodSeconds is not
// checked here: a non-positive value is a supported "no timed sleep" mode handled by autoSleep.
func validateConfig() error {
	return errors.Join(configProblems()...)
}

// configProblems runs every configuration check and returns all of them that failed, in the order
// they are declared.
//
// It reports all of them rather than the first for the sake of the --check-config mode, which is a
// pre-flight tool: an operator running it wants the list of what to fix, not one item of it per run,
// and a deploy pipeline gating on it gets one failed build instead of five. Startup gains the same
// thing for free - a configuration with three mistakes in it now names three rather than sending an
// operator around the restart loop once per mistake.
//
// Checks are therefore independent by construction: none may assume an earlier one passed, since
// none of them stop the rest running. Where two checks genuinely overlap (a method-3 configuration
// with a non-positive aggressiveness trips both the generic bound and the 1/e one) reporting both is
// correct - they are two true statements about the same value.
func configProblems() []error {
	var problems []error

	problems = append(problems, consolidationProblems()...)
	problems = append(problems, storeProblems()...)
	problems = append(problems, archiveProblems()...)
	problems = append(problems, searchProblems()...)
	problems = append(problems, gatewayProblems()...)

	if err := validateCallbackConfig(); err != nil {
		problems = append(problems, err)
	}

	if err := validateTopologyConfig(); err != nil {
		problems = append(problems, err)
	}

	if err := validateMetricsEndpoint(); err != nil {
		problems = append(problems, err)
	}

	problems = append(problems, llmProblems()...)

	return problems
}
