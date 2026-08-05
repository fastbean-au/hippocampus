package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/fastbean-au/hippocampus/archive"
	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/hippocampus"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/stats"
	"github.com/fastbean-au/hippocampus/summarise"
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
	flags.Duration("ttl", 24*time.Hour, "token lifetime (used with --mint-token)")
	flags.String("signing-secret", "", "override auth.signingSecret from the config file (used with --mint-token)")
	flags.String("kid", "", "signing-key id to stamp on a minted token; defaults to auth.activeKid or the first auth.signingKeys entry (used with --mint-token)")
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

	c, err := os.ReadFile(viper.GetString("config_file"))
	if err != nil {
		log.Panicf("failed to read config file '%s': %s", viper.GetString("config_file"), err.Error())
	}

	viper.SetConfigType("json")

	// A discarded parse error here would start the service with an all-zero config: auth off,
	// storage.directory empty (an in-memory SQLite database — every write lost on restart), and
	// consolidation.unitsOfAgeInDays 0. Fail fast, matching the os.ReadFile handling
	// above.
	if err := viper.ReadConfig(bytes.NewBuffer(c)); err != nil {
		log.Panicf("failed to parse config file '%s': %s", viper.GetString("config_file"), err.Error())
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

		token, err := auth.MintToken(auth.MintRequest{
			Secret:   secret,
			Kid:      kid,
			ClientID: viper.GetString("client-id"),
			Roles:    roles,
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
			fmt.Fprintf(os.Stderr, "jti=%s client_id=%s roles=%s\n", id, viper.GetString("client-id"), strings.Join(roles, ","))
		}

		return
	}

	// Defaults shared by normal startup and the --backfill-search CLI mode.
	viper.SetDefault("storage.driver", "sqlite")
	viper.SetDefault("storage.pool.maxOpenConns", 25)
	viper.SetDefault("storage.queryTimeoutSeconds", 60)
	viper.SetDefault("storage.compression.enabled", true)
	viper.SetDefault("storage.compression.minBytes", 512)
	viper.SetDefault("shutdown.timeoutSeconds", 10)
	viper.SetDefault("opensearch.index", "hippocampus-memories")
	viper.SetDefault("opensearch.queueSize", 1024)
	viper.SetDefault("opensearch.reconcileIntervalSeconds", 3600)
	viper.SetDefault("opensearch.reconcileBatchSize", 500)
	viper.SetDefault("ollama.address", "http://localhost:11434")
	viper.SetDefault("ollama.model", "llama3.2")
	viper.SetDefault("ollama.timeoutSeconds", 120)

	// --backfill-search is a CLI mode like --mint-token: it rebuilds the content-search index
	// from the primary store and exits without starting the server (see backfill.go).
	if viper.GetBool("backfill-search") {
		// Without OpenSearch the index being rebuilt is the store's own, which is a different job
		// with different constraints (it writes to the service's own database), so it has its own
		// entry point rather than a flag inside this one.
		if !viper.GetBool("opensearch.enabled") {
			rebuildContentSearch(viper.GetString("storage.driver"), viper.GetString("storage.directory"))

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
		})

		return
	}

	// Validate the consolidation and sleep config before touching the database or server. A
	// missing consolidation.unitsOfAgeInDays (viper returns 0) makes the age term +Inf, which
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

	// initialise observability
	log.Debug("initialising observability")
	obsCfg := ObservabilityConfig{
		TracingEnabled:         viper.GetBool("observability.tracing.enabled"),
		TracingSamplingRatio:   viper.GetFloat64("observability.tracing.samplingRatio"),
		MetricsEnabled:         viper.GetBool("observability.metrics.enabled"),
		MetricsIntervalSeconds: viper.GetInt("observability.metrics.exportIntervalSeconds"),
		OTLPEndpoint:           viper.GetString("observability.otlp.endpoint"),
		OTLPInsecure:           viper.GetBool("observability.otlp.insecure"),
		ServiceVersion:         version.Version,
	}

	shutdownObservability, err := initObservability(context.Background(), obsCfg)
	if err != nil {
		return fmt.Errorf("failed to initialise observability: %w", err)
	}
	log.Debug("observability initialised")

	// initialise DB. storage.driver selects the backend; sqlite (the default) preserves the
	// embedded, zero-dependency behaviour of every prior release.
	log.Debug("initialising database")

	// consolidation.enabled (default true) selects whether this instance runs the sleep cycle. In a
	// horizontally scaled deployment against a shared postgres/mysql database, exactly one instance
	// runs with it true - it takes the single-consolidator instance lock and runs consolidation -
	// while the rest run it false as read/write replicas that skip the lock and never sleep.
	// Passed to the server driver so a replica does not contend for the lock, and to the
	// gRPC server so it neither starts the sleep loop nor accepts the manual Sleep RPC.
	viper.SetDefault("consolidation.enabled", true)
	consolidate := viper.GetBool("consolidation.enabled")

	if !consolidate && viper.GetString("storage.driver") == "sqlite" {
		// SQLite is a single embedded file that cannot be shared between processes, so a
		// non-consolidating SQLite instance is not a horizontal-scaling replica - it is just an
		// instance that never forgets on its own. Warn so a misconfiguration expecting shared-store
		// scaling is visible; horizontal scaling requires the postgres or mysql driver.
		log.Warn("consolidation.enabled is false with storage.driver 'sqlite': SQLite cannot be shared between instances, so this instance simply never runs consolidation; horizontal scaling requires the postgres or mysql driver")
	}

	var database *db.DB

	switch storageDriver := viper.GetString("storage.driver"); storageDriver {

	case "sqlite":
		database, err = db.New(viper.GetString("storage.directory"))

	case "postgres":
		// WAL-triggered sleep is SQLite-specific (it exists to force a checkpoint when the
		// on-disk WAL file outgrows its trigger); neither server driver has a client-visible WAL
		// file to measure. Failing fast beats accepting the config and silently never triggering.
		// consolidation.capacityBytes works with every driver.
		if viper.GetInt64("consolidation.walTriggerBytes") > 0 {
			return fmt.Errorf("consolidation.walTriggerBytes is not supported with storage.driver 'postgres'")
		}

		database, err = db.NewPostgres(viper.GetString("storage.postgres.dsn"), consolidate)

	case "mysql":
		if viper.GetInt64("consolidation.walTriggerBytes") > 0 {
			return fmt.Errorf("consolidation.walTriggerBytes is not supported with storage.driver 'mysql'")
		}

		database, err = db.NewMySQL(viper.GetString("storage.mysql.dsn"), consolidate)

	default:
		return fmt.Errorf("unknown storage.driver '%s' (expected 'sqlite', 'postgres', or 'mysql')", storageDriver)
	}

	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Cap the connection pool on the server drivers so a burst of concurrent RPCs cannot exhaust the
	// shared database's connection slots (database/sql defaults to unlimited open connections).
	// SQLite caps itself at one connection, so this is skipped there. Sum maxOpenConns across every
	// instance in a replicated deployment must stay under the server's max_connections.
	if driver := viper.GetString("storage.driver"); driver == "postgres" || driver == "mysql" {
		database.SetPoolLimits(
			viper.GetInt("storage.pool.maxOpenConns"),
			viper.GetInt("storage.pool.maxIdleConns"),
		)
	}

	// Bound how long any single statement or transaction may run, so a hung or unreachable database
	// fails an operation after a bounded time rather than blocking the RPC goroutine and its pooled
	// connection indefinitely. Defaults to 60s (comfortably above a full consolidation scan at the
	// benchmarked sizes); 0 disables it. When set it must exceed the longest legitimate operation
	// (notably a full consolidation scan), or a cycle could be aborted mid-scan.
	database.SetQueryTimeout(time.Duration(viper.GetInt("storage.queryTimeoutSeconds")) * time.Second)

	// Store memory bodies compressed, trading CPU on the read/write paths for storage - which is the
	// resource the whole service manages to, so it buys retention. On by default: measured against
	// the surrounding database work it costs a few percent, well under what it saves. It governs
	// writes only: each row records whether it was compressed, so enabling or disabling it on an
	// existing store leaves every row already written perfectly readable.
	database.SetCompression(
		viper.GetBool("storage.compression.enabled"),
		viper.GetInt("storage.compression.minBytes"),
	)

	log.Debug("database initialised")

	// initialise the secondary content-search index. OpenSearch when it is configured; otherwise
	// the store's own index, which needs no configuration and no cluster, so SearchMemories works
	// out of the box. Only a driver with neither leaves the no-op in place, and that is logged
	// rather than left to be discovered through an empty search result. Construction of the
	// OpenSearch client only fails on unusable configuration (e.g. a malformed address) - an
	// unreachable cluster must not prevent startup, since that index is best-effort by design.
	searchIndex := search.NewNoop()

	switch {

	case viper.GetBool("opensearch.enabled"):
		log.Debug("initialising opensearch")

		idx, err := search.NewOpenSearch(searchConfigFromViper())
		if err != nil {
			return fmt.Errorf("failed to initialise opensearch: %w", err)
		}

		searchIndex = idx

		log.Debug("opensearch initialised")

	default:
		idx, err := search.NewSQL(database)
		if err != nil {
			log.Warnf(
				"content search is unavailable, so SearchMemories will be rejected: %s (enable opensearch.enabled to add it)",
				err.Error(),
			)

			break
		}

		searchIndex = idx

		log.Debug("content search initialised on the primary store")
	}

	// Consolidation and eviction delete memories inside the db layer, where the RPC-level
	// write-through hooks never see them; the observer closes that gap.
	database.SetMemoryDeleteObserver(searchIndex.DeleteMemories)

	// initialise the optional embedded-LLM summariser (Ollama). Disabled by default: the no-op
	// summariser makes SummariseMemories fail with FAILED_PRECONDITION and the sleep cycle's
	// auto-summarisation a no-op, so the service behaves exactly as it did before. Construction
	// only fails on unusable configuration (a missing address/model) - an unreachable Ollama server
	// must not prevent startup, since summarisation is optional and best-effort.
	summariser := summarise.NewNoop()

	if viper.GetBool("ollama.enabled") {
		log.Debug("initialising ollama summariser")

		s, err := summarise.NewOllama(summarise.Config{
			Address:         viper.GetString("ollama.address"),
			Model:           viper.GetString("ollama.model"),
			Timeout:         time.Duration(viper.GetInt("ollama.timeoutSeconds")) * time.Second,
			MaxBodies:       viper.GetInt("ollama.maxMemories"),
			PromptCharLimit: viper.GetInt("ollama.promptCharLimit"),
			SystemPrompt:    viper.GetString("ollama.systemPrompt"),
			Temperature:     viper.GetFloat64("ollama.temperature"),
		})
		if err != nil {
			return fmt.Errorf("failed to initialise ollama summariser: %w", err)
		}

		summariser = s

		log.Debug("ollama summariser initialised")
	}

	// initialise the optional S3 object store backing the Export/Import RPCs. Nil when no bucket
	// is configured, which makes those RPCs fail with FAILED_PRECONDITION rather than at startup:
	// most deployments never touch the archive surface. Credentials come from the standard AWS
	// chain; s3.endpoint and s3.usePathStyle exist for S3-compatible stores such as MinIO.
	var objects archive.ObjectStore

	if viper.GetString("s3.bucket") != "" {
		log.Debug("initialising s3 object store")

		store, err := archive.NewS3Store(context.Background(), archive.S3Config{
			Endpoint:     viper.GetString("s3.endpoint"),
			Region:       viper.GetString("s3.region"),
			Bucket:       viper.GetString("s3.bucket"),
			UsePathStyle: viper.GetBool("s3.usePathStyle"),
		})
		if err != nil {
			return fmt.Errorf("failed to initialise the s3 object store: %w", err)
		}

		objects = store

		log.Debug("s3 object store initialised")
	}

	// initialise auth and TLS. auth.method selects the verification scheme: "none" (the
	// default, preserving the no-auth behaviour of every prior release), "hmac" (shared-secret
	// HS256, tokens minted by --mint-token), or "idp" (RS256 against an identity provider's
	// JWKS endpoint - named directly by auth.jwksUrl or resolved via OIDC discovery from
	// auth.issuer). The boolean auth.enabled predates auth.method and remains as a deprecated
	// alias for "hmac", consulted only when auth.method is unset, so existing configs keep
	// working unchanged.
	authMethod := viper.GetString("auth.method")

	if authMethod == "" {
		authMethod = "none"

		if viper.GetBool("auth.enabled") {
			log.Warn("auth.enabled is deprecated - set auth.method to 'hmac' instead")

			authMethod = "hmac"
		}
	}

	tlsEnabled := viper.GetBool("tls.enabled")

	// Built once and shared by both listeners (below) so the gRPC service and the HTTP gateway
	// present the same certificate and enforce the same TLS floor. Loaded here so a bad
	// certificate/key pair fails fast, before either listener starts.
	var tlsConf *tls.Config

	if tlsEnabled {
		cfg, err := loadServerTLS(viper.GetString("tls.certFile"), viper.GetString("tls.keyFile"))
		if err != nil {
			return fmt.Errorf("failed to load TLS credentials: %w", err)
		}

		tlsConf = cfg
	}

	var verifier auth.Verifier

	// oauth2Login, when built, is the server-side OIDC relying-party login (confidential-client
	// Authorization Code flow). It is only available under auth.method "idp" and only when
	// auth.oauth2.enabled is set; it is registered as /auth/* handlers on the gateway below and its
	// session cookie is fed to the HTTP auth middleware.
	var oauth2Login *auth.OAuth2Login

	switch authMethod {

	case "none":

	case "hmac":
		v, err := auth.NewHMACVerifier(hmacConfigFromViper())
		if err != nil {
			return fmt.Errorf("failed to initialise auth: %w", err)
		}

		verifier = v

	case "idp":
		viper.SetDefault("auth.jwksRefreshIntervalSeconds", 300)
		viper.SetDefault("auth.roleClaim", "roles")

		refreshInterval := time.Duration(viper.GetInt("auth.jwksRefreshIntervalSeconds")) * time.Second

		v, err := auth.NewJWKSVerifier(auth.JWKSConfig{
			JWKSURL:         viper.GetString("auth.jwksUrl"),
			Issuer:          viper.GetString("auth.issuer"),
			Audience:        viper.GetString("auth.audience"),
			RefreshInterval: refreshInterval,
			RoleClaim:       viper.GetString("auth.roleClaim"),
		})
		if err != nil {
			return fmt.Errorf("failed to initialise auth: %w", err)
		}

		verifier = v

		if viper.GetBool("auth.oauth2.enabled") {
			login, err := buildOAuth2Login(refreshInterval)
			if err != nil {
				return fmt.Errorf("failed to initialise oauth2 login: %w", err)
			}

			oauth2Login = login
		}

	default:
		return fmt.Errorf("unknown auth.method '%s' (expected 'none', 'hmac', or 'idp')", authMethod)
	}

	authEnabled := verifier != nil

	// A revocation list, when configured, wraps whichever verifier was built (hmac or idp) so a
	// leaked token or decommissioned client can be cut off without rotating the signing secret for
	// everyone. It reloads from disk on the file's mtime, so revocations take effect without a
	// restart; a named-but-broken file fails startup rather than silently revoking nothing.
	var revocations *auth.RevocationList

	if authEnabled {
		if path := viper.GetString("auth.revocationFile"); path != "" {
			viper.SetDefault("auth.revocationRefreshSeconds", 30)

			refresh := time.Duration(viper.GetInt("auth.revocationRefreshSeconds")) * time.Second

			list, err := auth.NewRevocationList(path, refresh)
			if err != nil {
				return fmt.Errorf("failed to load revocation list: %w", err)
			}

			revocations = list
			verifier = auth.NewRevokingVerifier(verifier, list)

			log.Infof("token revocation enabled from '%s'", path)
		}
	}

	if authEnabled && !tlsEnabled {
		log.Warn("authentication is enabled but tls.enabled is false - bearer tokens will be sent in " +
			"plaintext unless TLS is terminated upstream (e.g. by a proxy or service mesh)")
	}

	// Authorisation rides on top of authentication: it maps each RPC to a required role tier
	// (reader/writer/admin) and rejects an authenticated caller whose token does not grant it. Built
	// only when auth is enabled - with auth off there is no principal to authorise, so the service
	// behaves as it always has. auth.roleMapping translates an identity provider's own group names
	// onto the tiers (empty for hmac, where --mint-token stamps the tier names directly).
	var authoriser *auth.Authoriser

	if authEnabled {
		var roleMapping map[string]string

		if err := viper.UnmarshalKey("auth.roleMapping", &roleMapping); err != nil {
			return fmt.Errorf("failed to read auth.roleMapping: %w", err)
		}

		a, err := auth.NewAuthoriser(roleMapping)
		if err != nil {
			return fmt.Errorf("failed to initialise authorisation: %w", err)
		}

		authoriser = a
	}

	// initialise the gRPC server
	log.Debug("initialising gRPC server")

	hipo := hippocampus.New(database, searchIndex, objects, summariser)

	// Panic recovery runs first (outermost) so it catches a panic from any handler or later
	// interceptor and returns codes.Internal rather than letting it crash the process. Auth runs
	// next, so an unauthenticated request is still rejected before the purge check or the handler
	// does any work on it.
	interceptors := []grpc.UnaryServerInterceptor{InterceptorRecoverPanic}

	if authEnabled {
		// Authentication first (rejects an invalid/absent token), then authorisation (rejects a
		// valid token whose role does not grant the RPC), both ahead of the purge gate and logger.
		interceptors = append(interceptors,
			auth.UnaryServerInterceptor(verifier),
			authoriser.UnaryServerInterceptor(),
		)
	}

	interceptors = append(interceptors,
		hipo.InterceptorBlockWhenPurgeInProgress,
		InterceptorLogger,
	)

	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(interceptors...),
	}

	// Raise the gRPC max-receive-message size above the 4 MiB default so operators can accept larger
	// ImportBatch/single-memory payloads (Transfer keeps its own batches under transfer.maxBatchBytes
	// by default). 0 keeps grpc-go's default.
	if maxRecvMsgBytes := viper.GetInt("maxRecvMsgBytes"); maxRecvMsgBytes > 0 {
		serverOpts = append(serverOpts, grpc.MaxRecvMsgSize(maxRecvMsgBytes))
	}

	// Cap the concurrent HTTP/2 streams a single connection may open, bounding the work one caller
	// can pin at once - the cheapest first defence if the gRPC port is ever exposed beyond trusted
	// callers. 0 (the default) keeps grpc-go's own default.
	if maxConcurrentStreams := viper.GetInt("maxConcurrentStreams"); maxConcurrentStreams > 0 {
		serverOpts = append(serverOpts, grpc.MaxConcurrentStreams(uint32(maxConcurrentStreams)))
	}

	// Enforce a keepalive policy so an abusive client cannot ping the server into a CPU/bandwidth
	// spiral. minTimeSeconds is the minimum interval the server tolerates between client pings
	// before it terminates the connection; permitWithoutStream allows keepalive pings on an idle
	// connection. Both default to grpc-go's own policy when unset (minTimeSeconds 0 leaves it).
	if minTime := viper.GetInt("keepalive.minTimeSeconds"); minTime > 0 {
		serverOpts = append(serverOpts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             time.Duration(minTime) * time.Second,
			PermitWithoutStream: viper.GetBool("keepalive.permitWithoutStream"),
		}))
	}

	// The otelgrpc stats handler creates a server span for every RPC (extracting any incoming
	// trace context) and records the standard low-cardinality RPC metrics (method, service,
	// status code). Only installed when observability is enabled.
	if obsCfg.TracingEnabled || obsCfg.MetricsEnabled {
		serverOpts = append(serverOpts, grpc.StatsHandler(otelgrpc.NewServerHandler()))
	}

	if tlsConf != nil {
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsConf)))
	}

	s := grpc.NewServer(serverOpts...)

	hs := health.NewServer()
	healthgrpc.RegisterHealthServer(s, hs)

	contract.RegisterHippocampusServer(s, hipo)

	// A listener that fails to bind or serve sends its error here; run selects on it alongside
	// ctx.Done so such a failure ends the process (via main's log.Fatal on the returned error)
	// instead of the old in-goroutine log.Fatal, which a test could not observe.
	serveErr := make(chan error, 2)

	// Start listening. bindAddress (empty by default) restricts the interface the listener binds to
	// - e.g. "127.0.0.1" for loopback only behind a sidecar/mesh that terminates TLS; empty binds
	// all interfaces, the previous behaviour.
	go func() {
		lis, err := net.Listen("tcp", viper.GetString("bindAddress")+":"+strconv.Itoa(viper.GetInt("port")))
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

	// initialise the HTTP/JSON gateway. It calls straight into hipo rather than dialing back to
	// the gRPC listener, so there is no extra network hop or serialisation round trip. A
	// non-positive gateway.port disables it.
	var gwServer *http.Server

	if gatewayPort := viper.GetInt("gateway.port"); gatewayPort > 0 {
		log.Debug("initialising HTTP gateway")

		// Authorisation on the gateway runs as a post-routing middleware (so the matched route -
		// hence the RPC's required tier - is known) rather than an outer HTTP wrapper. It reads the
		// claims stashed by auth.HTTPMiddleware below, so it enforces the same policy as the gRPC
		// interceptor from the same table. Added only when auth is enabled, mirroring the gRPC side.
		var muxOpts []runtime.ServeMuxOption

		if authEnabled {
			muxOpts = append(muxOpts, runtime.WithMiddlewares(authoriser.GatewayMiddleware()))
		}

		gwMux := runtime.NewServeMux(muxOpts...)
		if err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, hipo); err != nil {
			return fmt.Errorf("failed to register HTTP gateway: %w", err)
		}

		httpMux := http.NewServeMux()
		httpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": version})
		})
		httpMux.HandleFunc("/readyz", readiness.handler())
		httpMux.HandleFunc("/v1/openapi.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(contract.SwaggerJSON)
		})
		httpMux.Handle("/ui", webUIHandler())

		// The front-channel OIDC config the console reads to start a login. The browser fetches it
		// before it has a token, so it must stay unauthenticated and reachable during a purge, like
		// the page itself. The issuer defaults to auth.issuer (the same one the verifier discovers
		// its JWKS from); scopes default to a plain openid/profile request when unset.
		uiIssuer := viper.GetString("auth.ui.issuer")

		if uiIssuer == "" {
			uiIssuer = viper.GetString("auth.issuer")
		}

		uiScopes := viper.GetString("auth.ui.scopes")

		if uiScopes == "" {
			uiScopes = "openid profile"
		}

		// loginMode tells the console how sign-in works: server-hosted (/auth/login) when the oauth2
		// login is built, otherwise the in-browser PKCE flow under idp, and empty otherwise.
		loginMode := ""

		if authMethod == "idp" {
			loginMode = "browser"

			if oauth2Login != nil {
				loginMode = "server"
			}
		}

		httpMux.Handle("/ui/config", uiConfigHandler(UIConfig{
			AuthMethod: authMethod,
			Issuer:     uiIssuer,
			ClientID:   viper.GetString("auth.ui.clientId"),
			Scopes:     uiScopes,
			Audience:   viper.GetString("auth.ui.audience"),
			LoginMode:  loginMode,
		}))

		// When the server-side login is enabled, register its handlers and mark them open (they run
		// before, or in place of, holding a token). /auth/refresh authenticates itself via the refresh
		// cookie, so it too must be reachable without an access token.
		sessionCookie := ""
		authOpenPaths := []string{"/healthz", "/readyz", "/ui", "/ui/config"}
		purgeOpenPaths := []string{"/healthz", "/readyz", "/v1/openapi.json", "/ui", "/ui/config"}

		if oauth2Login != nil {
			httpMux.Handle("/auth/login", http.HandlerFunc(oauth2Login.Login))
			httpMux.Handle("/auth/callback", http.HandlerFunc(oauth2Login.Callback))
			httpMux.Handle("/auth/refresh", http.HandlerFunc(oauth2Login.Refresh))
			httpMux.Handle("/auth/logout", http.HandlerFunc(oauth2Login.Logout))

			authPaths := []string{"/auth/login", "/auth/callback", "/auth/refresh", "/auth/logout"}
			authOpenPaths = append(authOpenPaths, authPaths...)
			purgeOpenPaths = append(purgeOpenPaths, authPaths...)

			sessionCookie = auth.SessionCookieName
		}

		httpMux.Handle("/", gwMux)

		// The gateway calls hipo directly, bypassing the gRPC interceptor chain, so the purge gate
		// must be re-applied here or /v1/... requests would run during a purge.
		// It is applied unconditionally (independent of auth); /healthz, /readyz, the static
		// OpenAPI doc, the console, its front-channel config, and the login endpoints stay reachable
		// while a purge runs.
		handler := hipo.HTTPMiddlewareBlockWhenPurgeInProgress(httpMux, purgeOpenPaths)

		// Per-request logging: the gateway never runs the gRPC interceptor chain, so without this its
		// traffic is invisible in logs. Positioned inside auth (so unauthenticated requests are still
		// rejected first) but around the purge gate and handler, capturing status and duration.
		handler = httpLoggingMiddleware(handler)

		// /healthz and /readyz stay open for liveness/readiness probes; everything else, including
		// /v1/openapi.json, requires a token when auth is enabled. Auth wraps the purge gate so an
		// unauthenticated request is rejected before any other check, mirroring the gRPC chain
		// order.
		if authEnabled {
			handler = auth.HTTPMiddleware(verifier, handler, authOpenPaths, sessionCookie)
		}

		// Cap the request body the gateway will read when configured (0, the default, leaves it
		// unbounded). Outermost so an oversized body is rejected before auth or any handler buffers
		// it. Off by default because a legitimate ImportBatch/Transfer body can be large; operators
		// exposing the gateway to untrusted callers should set a ceiling.
		if maxRequestBytes := viper.GetInt64("gateway.maxRequestBytes"); maxRequestBytes > 0 {
			handler = maxRequestBytesMiddleware(handler, maxRequestBytes)
		}

		// Panic recovery wraps everything (outermost) so a panic in any handler or middleware
		// becomes a clean 500 rather than a dropped connection.
		handler = recoverMiddleware(handler)

		gwServer = newGatewayServer(viper.GetString("gateway.bindAddress"), gatewayPort, handler)

		go func() {
			var err error

			if tlsEnabled {
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
	// Stop the readiness updater first so it cannot flip the status back to SERVING.
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

	if revocations != nil {
		revocations.Stop()
	}

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

	if err := shutdownObservability(shutdownCtx); err != nil {
		log.Errorf("failed to shut down observability cleanly: %s", err.Error())
	}

	_ = database.Close()

	return runErr
}

// hmacConfigFromViper reads the HMAC signing configuration into an auth.HMACConfig. Centralising the
// viper access here (per the project convention that all viper reads live in main) means the
// --mint-token CLI and the running verifier share one interpretation of signingSecret, signingKeys,
// and activeKid.
// searchConfigFromViper builds the OpenSearch client config from the opensearch.* viper keys,
// including the opensearch.tls.* transport-security block. Both the server bootstrap and the
// --backfill-search CLI mode use it, so the two paths stay in step.
func searchConfigFromViper() search.Config {

	return search.Config{
		Addresses: viper.GetStringSlice("opensearch.addresses"),
		Username:  viper.GetString("opensearch.username"),
		Password:  viper.GetString("opensearch.password"),
		Index:     viper.GetString("opensearch.index"),
		QueueSize: viper.GetInt("opensearch.queueSize"),

		// Worker tuning; each 0 (the default) falls back to the search package's own default.
		ApplyTimeout:          time.Duration(viper.GetInt("opensearch.applyTimeoutSeconds")) * time.Second,
		ApplyMaxAttempts:      viper.GetInt("opensearch.applyMaxAttempts"),
		ApplyRetryBaseBackoff: time.Duration(viper.GetInt("opensearch.applyRetryBaseBackoffMillis")) * time.Millisecond,
		CloseDrainTimeout:     time.Duration(viper.GetInt("opensearch.closeDrainTimeoutSeconds")) * time.Second,

		TLS: search.TLSConfig{
			CACertFile:         viper.GetString("opensearch.tls.caCertFile"),
			CertFile:           viper.GetString("opensearch.tls.certFile"),
			KeyFile:            viper.GetString("opensearch.tls.keyFile"),
			InsecureSkipVerify: viper.GetBool("opensearch.tls.insecureSkipVerify"),
		},
	}
}

func hmacConfigFromViper() auth.HMACConfig {
	var keys []auth.SigningKey

	if err := viper.UnmarshalKey("auth.signingKeys", &keys); err != nil {
		log.Fatalf("failed to read auth.signingKeys: %s", err.Error())
	}

	return auth.HMACConfig{
		LegacySecret: viper.GetString("auth.signingSecret"),
		Keys:         keys,
		ActiveKid:    viper.GetString("auth.activeKid"),
	}
}

// buildOAuth2Login assembles the server-side OIDC login from the auth.oauth2 config block. All viper
// reads stay here, per the repo convention. The issuer and audience default to the top-level
// auth.issuer/auth.audience the token verifier already uses, so a single-provider deployment only
// has to set the client credentials and redirect URL. A dedicated JWKS verifier is built to check
// the id_token (whose audience is the client id, not the API audience), reusing the same key
// machinery as the access-token verifier. The Secure cookie attribute follows the redirect URL's
// scheme unless auth.oauth2.cookieSecure is set explicitly, so a Secure cookie is not silently
// dropped on a local http rig nor omitted on an https deployment.
func buildOAuth2Login(refreshInterval time.Duration) (*auth.OAuth2Login, error) {
	issuer := viper.GetString("auth.oauth2.issuer")

	if issuer == "" {
		issuer = viper.GetString("auth.issuer")
	}

	audience := viper.GetString("auth.oauth2.audience")

	if audience == "" {
		audience = viper.GetString("auth.audience")
	}

	clientID := viper.GetString("auth.oauth2.clientId")

	scopes := strings.Fields(viper.GetString("auth.oauth2.scopes"))

	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "offline_access"}
	}

	redirectURL := viper.GetString("auth.oauth2.redirectUrl")

	cookieSecure := strings.HasPrefix(strings.ToLower(redirectURL), "https")

	if viper.IsSet("auth.oauth2.cookieSecure") {
		cookieSecure = viper.GetBool("auth.oauth2.cookieSecure")
	}

	// The id_token carries the client id as its audience, so it needs its own verifier distinct from
	// the access-token one (which enforces the API audience). Both discover from the same issuer and
	// share the JWKS fetch/cache logic.
	idTokenVerifier, err := auth.NewJWKSVerifier(auth.JWKSConfig{
		JWKSURL:         viper.GetString("auth.jwksUrl"),
		Issuer:          issuer,
		Audience:        clientID,
		RefreshInterval: refreshInterval,
		RoleClaim:       viper.GetString("auth.roleClaim"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build id_token verifier: %w", err)
	}

	return auth.NewOAuth2Login(auth.OAuth2Config{
		Issuer:                issuer,
		ClientID:              clientID,
		ClientSecret:          viper.GetString("auth.oauth2.clientSecret"),
		RedirectURL:           redirectURL,
		Scopes:                scopes,
		Audience:              audience,
		IDTokenVerifier:       idTokenVerifier,
		CookieSecure:          cookieSecure,
		CookieDomain:          viper.GetString("auth.oauth2.cookieDomain"),
		PostLogoutRedirectURL: viper.GetString("auth.oauth2.postLogoutRedirectUrl"),
		SuccessRedirectURL:    viper.GetString("auth.oauth2.successRedirectUrl"),
		SessionTTL:            time.Duration(viper.GetInt("auth.oauth2.sessionTTLSeconds")) * time.Second,
		RefreshTTL:            time.Duration(viper.GetInt("auth.oauth2.refreshTTLSeconds")) * time.Second,
	})
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

// Gateway HTTP server hardening timeouts. ReadHeaderTimeout bounds slow-header (slowloris) clients
// and IdleTimeout bounds idle keep-alive connections; both are safe to set unconditionally. There
// is deliberately no WriteTimeout - Export/Import/Transfer responses can legitimately run long, and
// a write deadline would abort them mid-stream.
const (
	gatewayReadHeaderTimeout = 10 * time.Second
	gatewayIdleTimeout       = 120 * time.Second
)

// newGatewayServer builds the HTTP gateway server with the hardening timeouts above. It is a
// separate function so the timeout policy can be unit-tested without standing up the whole service.
// bindAddress (empty by default) restricts the interface the listener binds to; empty binds all
// interfaces, mirroring the gRPC listener's bindAddress.
func newGatewayServer(bindAddress string, port int, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              bindAddress + ":" + strconv.Itoa(port),
		Handler:           handler,
		ReadHeaderTimeout: gatewayReadHeaderTimeout,
		IdleTimeout:       gatewayIdleTimeout,
	}
}

// loadServerTLS builds the TLS configuration shared by the gRPC listener and the HTTP gateway from
// the configured certificate/key pair, pinning a TLS 1.2 minimum. Go's current default server
// minimum is already TLS 1.2, but pinning it makes the floor explicit and immune to a future
// default change, keeping weak legacy protocol versions off both listeners.
func loadServerTLS(certFile string, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// maxRequestBytesMiddleware caps the request body the gateway will read, so an oversized (or
// deliberately huge) body is rejected by the transport before a handler buffers it into memory. A
// body that exceeds maxBytes fails the handler's read with a 413. GET requests (health, list) carry
// no body and are unaffected.
func maxRequestBytesMiddleware(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}

		next.ServeHTTP(w, r)
	})
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

// validateConfig rejects consolidation settings that would make the service behave destructively.
// It reads straight from viper (all viper access lives in main.go) and returns a single error
// describing the first problem found. sleep.periodSeconds is not checked here: a
// non-positive value is a supported "no timed sleep" mode handled by autoSleep.
func validateConfig() error {
	if unitsOfAgeInDays := viper.GetFloat64("consolidation.unitsOfAgeInDays"); unitsOfAgeInDays <= 0 {
		return fmt.Errorf("consolidation.unitsOfAgeInDays must be greater than 0, got %v", unitsOfAgeInDays)
	}

	if method := viper.GetInt("consolidation.method"); method < 1 || method > 6 {
		return fmt.Errorf("consolidation.method must be between 1 and 6, got %d", method)
	}

	if aggressiveness := viper.GetFloat64("consolidation.aggressiveness"); aggressiveness <= 0 {
		return fmt.Errorf("consolidation.aggressiveness must be greater than 0, got %v", aggressiveness)
	}

	// Method 3's decay factor is 1 + ln(aggressiveness), which goes non-positive for any
	// aggressiveness at or below 1/e (~0.368). calculateValue treats a non-positive factor as
	// "never delete" (it returns MaxFloat64 to keep NaN out of eviction's sort), so such a config
	// silently disables value-based consolidation entirely while the generic aggressiveness > 0
	// check above passes. Reject it here so the mistake surfaces at startup rather than as a store
	// that quietly never forgets anything.
	if method := viper.GetInt("consolidation.method"); method == 3 {
		if aggressiveness := viper.GetFloat64("consolidation.aggressiveness"); aggressiveness <= math.Exp(-1) {
			return fmt.Errorf("consolidation.aggressiveness must be greater than 1/e (~0.368) for consolidation.method 3, got %v", aggressiveness)
		}
	}

	// A negative retention window is meaningless (0 disables the floor). Catch it at startup rather
	// than let a mis-signed value silently disable the guarantee that overrides the capacity target.
	if retention := viper.GetInt("consolidation.minimumRetentionInDays"); retention < 0 {
		return fmt.Errorf("consolidation.minimumRetentionInDays must not be negative, got %d", retention)
	}

	// sleep.periodSeconds is deliberately not validated: a non-positive value disables automatic
	// timed sleep cycles (a supported mode - e.g. an import-only instance, or one driven purely by
	// the manual Sleep RPC or the WAL trigger). autoSleep treats it as "no timed sleep".

	// An empty storage.directory selects db.New's in-memory mode, which is intended for tests
	// only - every write is lost on restart. Refuse it for the sqlite driver so it can never be
	// reached by a real deployment. The postgres/mysql drivers use their own DSN
	// keys, not storage.directory.
	if viper.GetString("storage.driver") == "sqlite" && viper.GetString("storage.directory") == "" {
		return fmt.Errorf("storage.directory must be set for storage.driver 'sqlite' (an empty directory selects the test-only in-memory database)")
	}

	return nil
}
