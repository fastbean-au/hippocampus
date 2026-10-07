// hippocampus-mcp is a Model Context Protocol (MCP) server that gives an LLM client (Claude
// Desktop, Claude Code, or any other MCP host) a curated set of tools for storing and recalling
// memories in a running Hippocampus instance. It is a thin bridge: every tool call is turned into
// a gRPC request against the Hippocampus service named by --address, so the MCP server holds no
// state of its own and can be spawned, killed, and restarted freely by the host.
//
// The default transport is stdio (the host launches this binary as a subprocess and speaks MCP
// over its stdin/stdout), which is why all logging goes to stderr - stdout carries only the MCP
// protocol. The optional streamable-HTTP transport (--transport http) serves the same tools over
// HTTP for a remote/hosted host instead.
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fastbean-au/hippocampus/dial"
)

// version is stamped into the MCP server's Implementation so an MCP host can display which build
// it is talking to. It is a var so a release build can override it with -ldflags "-X main.version".
var version = "dev"

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain is the testable body of main: it registers flags, installs the signal handler, and runs
// serve, returning a process exit code rather than calling os.Exit itself so tests can drive every
// branch. It mirrors the eventsource bridges' entry point.
func realMain(args []string) int {
	if err := registerFlags(pflag.CommandLine, args); err != nil {
		log.Errorf("failed to register command line flags: %s", err.Error())

		return 1
	}

	// Turn SIGINT/SIGTERM into a cancelled context so both transports shut down cleanly when the
	// host stops the subprocess (or the operator Ctrl-Cs the HTTP mode).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx); err != nil {
		log.Errorf("hippocampus-mcp exited with an error: %s", err.Error())

		return 1
	}

	return 0
}

// serve handles the --version short-circuit, sets the log level, and hands off to run. It is split
// out of main (which only registers flags and installs the signal handler) so the whole
// version/level/serve path can be exercised by a test.
func serve(ctx context.Context) error {
	if viper.GetBool("version") {
		fmt.Fprintln(os.Stderr, version)

		return nil
	}

	// logrus defaults to stderr, which the stdio transport depends on: stdout must carry only the
	// MCP JSON-RPC stream. Set the level, keep the stream.
	level, err := log.ParseLevel(viper.GetString("log-level"))
	if err != nil {
		return fmt.Errorf("invalid log level '%s': %w", viper.GetString("log-level"), err)
	}

	log.SetLevel(level)

	return run(ctx)
}

// registerFlags defines the command line flags on fs, parses args into it, binds them onto viper,
// and wires the HIPPOCAMPUS_MCP_* environment overrides. It takes the flag set and args explicitly
// (rather than using the pflag globals directly) so a test can drive it with a fresh flag set. Env
// overrides let a secret like the bearer token be injected by the MCP host's config env block
// rather than written into an argv the host stores in plaintext.
func registerFlags(fs *pflag.FlagSet, args []string) error {
	fs.StringP("address", "a", "localhost:50051", "address of the hippocampus gRPC service")
	fs.String("transport", "stdio", "MCP transport: 'stdio' (the host spawns this as a subprocess) or 'http' (streamable HTTP)")
	fs.String("http-address", "127.0.0.1:8090", "listen address for the streamable-HTTP transport (used with --transport http); loopback unless --http-token is set")
	fs.String("http-token", "", "bearer token every streamable-HTTP request must present (overridable by HIPPOCAMPUS_MCP_HTTP_TOKEN)")
	fs.Bool("allow-unauthenticated-http", false, "serve the streamable-HTTP transport on a non-loopback address with no --http-token (anyone who can reach it acts with --token)")
	fs.String("token", "", "bearer token sent on every RPC when the service requires auth (overridable by HIPPOCAMPUS_MCP_TOKEN)")
	fs.Bool("tls", false, "dial the service over TLS")
	fs.String("tls-ca-cert", "", "PEM CA bundle to verify the service certificate against, in place of the system pool (used with --tls)")
	fs.String("tls-cert", "", "client certificate for mutual TLS (used with --tls; requires --tls-key)")
	fs.String("tls-key", "", "client private key for mutual TLS (used with --tls; requires --tls-cert)")
	fs.Bool("tls-insecure-skip-verify", false, "skip verification of the service certificate (dev only; used with --tls)")
	fs.Int("call-timeout-seconds", 30, "per-tool-call timeout bounding each gRPC request")
	fs.String("log-level", "info", "logging level (written to stderr, never stdout)")
	fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse command line flags: %w", err)
	}

	if err := viper.BindPFlags(fs); err != nil {
		return fmt.Errorf("failed to bind command line flags: %w", err)
	}

	viper.SetEnvPrefix("HIPPOCAMPUS_MCP")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	return nil
}

// run dials the Hippocampus service, builds the MCP server, and serves it over the configured
// transport until ctx is cancelled or the transport fails. It is split out of main so the
// dial/serve lifecycle can be exercised by a test.
func run(ctx context.Context) error {
	// The connection is the shared dial package's (TODO-3 item 172): the bearer token and this
	// bridge's own version ride on every RPC, so no tool handler has to remember to send either.
	cfg := dialConfig()
	address := cfg.Address

	conn, client, err := dial.Dial(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to '%s': %w", address, err)
	}

	defer func() { _ = conn.Close() }()

	log.Infof("connecting to hippocampus at %s", address)

	b := &bridge{
		client:      client,
		callTimeout: time.Duration(viper.GetInt("call-timeout-seconds")) * time.Second,
	}

	server := newServer(b, version)

	switch transport := viper.GetString("transport"); transport {

	case "stdio":
		log.Info("serving MCP over stdio")

		if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
			return fmt.Errorf("stdio transport failed: %w", err)
		}

		return nil

	case "http":
		return serveHTTP(ctx, server, httpConfig{
			address:              viper.GetString("http-address"),
			token:                viper.GetString("http-token"),
			allowUnauthenticated: viper.GetBool("allow-unauthenticated-http"),
		})

	default:
		return fmt.Errorf("unknown transport '%s' (expected 'stdio' or 'http')", transport)
	}
}

// httpConfig is the streamable-HTTP transport's listener and its inbound authentication.
type httpConfig struct {
	address              string
	token                string
	allowUnauthenticated bool
}

// serveHTTP serves the MCP server over the streamable-HTTP transport, shutting the listener down
// when ctx is cancelled. The same server instance is handed to every request - the bridge is
// stateless, so one server can back concurrent sessions.
//
// Whoever reaches this listener acts with the bridge's own service token, which for a useful bridge
// is a writer's - so an unauthenticated listener anyone on the network can reach is that token,
// handed out (TODO-3 item 160). With no --http-token, a non-loopback address is therefore refused
// unless --allow-unauthenticated-http says the exposure is deliberate, on the precedent of the
// object gateway's --allow-anonymous.
func serveHTTP(ctx context.Context, server *mcp.Server, cfg httpConfig) error {
	if cfg.token == "" && !isLoopbackAddress(cfg.address) {
		if !cfg.allowUnauthenticated {
			return fmt.Errorf(
				"refusing to serve MCP over HTTP on '%s' with no --http-token: anyone who can reach it acts with this bridge's service token; set --http-token, bind a loopback address, or pass --allow-unauthenticated-http",
				cfg.address,
			)
		}

		log.Warnf("serving MCP over HTTP on %s with no --http-token: anyone who can reach it acts with this bridge's service token", cfg.address)
	}

	httpAddress := cfg.address

	httpServer := &http.Server{
		Addr:              httpAddress,
		Handler:           httpHandler(server, cfg.token),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)

	go func() {
		log.Infof("serving MCP over streamable HTTP on %s", httpAddress)

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {

	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		return httpServer.Shutdown(shutdownCtx)

	case err := <-serveErr:
		return fmt.Errorf("http transport failed: %w", err)
	}
}

// httpHandler is the streamable-HTTP handler, behind the inbound token when one is configured.
func httpHandler(server *mcp.Server, token string) http.Handler {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)

	return requireBearer(token, handler)
}

// requireBearer refuses any request not carrying "Authorization: Bearer <token>", comparing in
// constant time. An empty token is no requirement, and returns next unchanged.
func requireBearer(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}

	want := []byte(token)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, presented, ok := strings.Cut(r.Header.Get("Authorization"), " ")

		if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(presented), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hippocampus-mcp"`)
			http.Error(w, "unauthorised", http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// isLoopbackAddress reports whether a listen address binds only the loopback interface. An empty
// host binds every interface, and a hostname other than localhost is not resolved - the answer has
// to be right without trusting DNS, so anything uncertain reads as exposed.
func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// dialConfig is the --address, --token and --tls* flags in the shared dial package's terms.
func dialConfig() dial.Config {
	return dial.Config{
		Address:               viper.GetString("address"),
		Token:                 viper.GetString("token"),
		TLS:                   viper.GetBool("tls"),
		TLSCACertFile:         viper.GetString("tls-ca-cert"),
		TLSCertFile:           viper.GetString("tls-cert"),
		TLSKeyFile:            viper.GetString("tls-key"),
		TLSInsecureSkipVerify: viper.GetBool("tls-insecure-skip-verify"),
		ClientVersion:         "hippocampus-mcp/" + version,
	}
}
