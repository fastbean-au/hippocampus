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
	"crypto/tls"
	"crypto/x509"
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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fastbean-au/hippocampus/contract"
)

// version is stamped into the MCP server's Implementation so an MCP host can display which build
// it is talking to. It is a var so a release build can override it with -ldflags "-X main.version".
var version = "dev"

func main() {
	if err := registerFlags(pflag.CommandLine, os.Args[1:]); err != nil {
		log.Panicf("failed to register command line flags: %s", err.Error())
	}

	// Turn SIGINT/SIGTERM into a cancelled context so both transports shut down cleanly when the
	// host stops the subprocess (or the operator Ctrl-Cs the HTTP mode).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx); err != nil {
		log.Fatalf("hippocampus-mcp exited with an error: %s", err.Error())
	}
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
	fs.String("http-address", ":8090", "listen address for the streamable-HTTP transport (used with --transport http)")
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
	creds, err := transportCredentials()
	if err != nil {

		return fmt.Errorf("failed to build transport credentials: %w", err)
	}

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	// A bearer token, when configured, is attached to every outgoing RPC as "authorization: Bearer
	// <token>" metadata - exactly what the service's auth interceptor reads - via a client
	// interceptor, so no individual tool handler has to remember to send it.
	if token := viper.GetString("token"); token != "" {
		dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(bearerTokenInterceptor(token)))
	}

	address := viper.GetString("address")

	conn, err := grpc.NewClient(address, dialOpts...)
	if err != nil {

		return fmt.Errorf("failed to create gRPC client for '%s': %w", address, err)
	}

	defer func() { _ = conn.Close() }()

	log.Infof("connecting to hippocampus at %s", address)

	b := &bridge{
		client:      contract.NewHippocampusClient(conn),
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

		return serveHTTP(ctx, server, viper.GetString("http-address"))

	default:

		return fmt.Errorf("unknown transport '%s' (expected 'stdio' or 'http')", transport)
	}
}

// serveHTTP serves the MCP server over the streamable-HTTP transport, shutting the listener down
// when ctx is cancelled. The same server instance is handed to every request - the bridge is
// stateless, so one server can back concurrent sessions.
func serveHTTP(ctx context.Context, server *mcp.Server, httpAddress string) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {

		return server
	}, nil)

	httpServer := &http.Server{
		Addr:              httpAddress,
		Handler:           handler,
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

// bearerTokenInterceptor returns a unary client interceptor that stamps the bearer token onto every
// RPC's outgoing metadata in the form the service's auth interceptor expects.
func bearerTokenInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// transportCredentials builds the gRPC transport credentials from the --tls* flags, mirroring the
// trust options the service's own Transfer client honours: plaintext when --tls is off; otherwise
// TLS against the system pool, an optional private-CA bundle, an optional client certificate for
// mutual TLS, and an insecureSkipVerify escape hatch.
func transportCredentials() (credentials.TransportCredentials, error) {
	if !viper.GetBool("tls") {

		return insecure.NewCredentials(), nil
	}

	certFile := viper.GetString("tls-cert")
	keyFile := viper.GetString("tls-key")

	if (certFile == "") != (keyFile == "") {

		return nil, fmt.Errorf("mutual TLS requires both --tls-cert and --tls-key, or neither")
	}

	conf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: viper.GetBool("tls-insecure-skip-verify"),
	}

	if caCertFile := viper.GetString("tls-ca-cert"); caCertFile != "" {
		pem, err := os.ReadFile(caCertFile)
		if err != nil {

			return nil, fmt.Errorf("reading CA cert file %q: %w", caCertFile, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {

			return nil, fmt.Errorf("CA cert file %q contained no valid certificates", caCertFile)
		}

		conf.RootCAs = pool
	}

	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {

			return nil, fmt.Errorf("loading client certificate: %w", err)
		}

		conf.Certificates = []tls.Certificate{cert}
	}

	return credentials.NewTLS(conf), nil
}
