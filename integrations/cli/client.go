package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
)

// TLSConfig carries the client TLS trust options, mirroring the block the service's own Transfer
// client and the MCP bridge honour: an optional private-CA bundle, an optional client certificate
// for mutual TLS, and an insecureSkipVerify escape hatch.
type TLSConfig struct {
	Enabled            bool
	CACert             string
	Cert               string
	Key                string
	InsecureSkipVerify bool
}

// Config is the resolved connection configuration for a single CLI invocation. The same struct
// drives both transports; Transport selects which client newClient builds.
type Config struct {
	Transport string // "grpc" or "http"
	Address   string // host:port for gRPC, or a base URL/host for HTTP
	Token     string // bearer token attached to every request when set
	Timeout   time.Duration
	TLS       TLSConfig
}

// newClient builds a transport-agnostic contract.HippocampusClient from cfg, plus a closer to
// release any underlying connection. Both the gRPC and HTTP implementations satisfy the same
// generated client interface, so every command handler is written once against it.
func newClient(cfg Config) (contract.HippocampusClient, func() error, error) {
	switch cfg.Transport {

	case "grpc":
		return newGRPCClient(cfg)

	case "http":
		return newHTTPClient(cfg)

	default:
		return nil, nil, fmt.Errorf("unknown transport %q (expected 'grpc' or 'http')", cfg.Transport)
	}
}

// newGRPCClient dials the service over gRPC, attaching the bearer token as an outgoing-metadata
// interceptor when one is configured. grpc.NewClient is lazy, so the dial itself never blocks here;
// a bad address surfaces on the first RPC.
func newGRPCClient(cfg Config) (contract.HippocampusClient, func() error, error) {
	creds, err := transportCredentials(cfg.TLS)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build transport credentials: %w", err)
	}

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	if cfg.Token != "" {
		dialOpts = append(dialOpts, grpc.WithUnaryInterceptor(bearerTokenInterceptor(cfg.Token)))
	}

	conn, err := grpc.NewClient(cfg.Address, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create gRPC client for %q: %w", cfg.Address, err)
	}

	return contract.NewHippocampusClient(conn), conn.Close, nil
}

// newHTTPClient builds an HTTP client that speaks to the service's /v1 grpc-gateway. The address is
// normalised to a base URL: a bare host[:port] gains an http:// (or https:// under --tls) scheme so
// the common `--transport http --address localhost:8080` form works without a scheme.
func newHTTPClient(cfg Config) (contract.HippocampusClient, func() error, error) {
	transport, err := httpTransport(cfg.TLS)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build HTTP transport: %w", err)
	}

	base := cfg.Address
	if !strings.Contains(base, "://") {
		scheme := "http"
		if cfg.TLS.Enabled {
			scheme = "https"
		}

		base = scheme + "://" + base
	}

	client := &httpClient{
		baseURL: strings.TrimRight(base, "/"),
		token:   cfg.Token,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}

	return client, func() error { return nil }, nil
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

// transportCredentials builds the gRPC transport credentials from the TLS config: plaintext when
// disabled; otherwise TLS against the system pool, an optional private-CA bundle, an optional client
// certificate for mutual TLS, and an insecureSkipVerify escape hatch.
func transportCredentials(cfg TLSConfig) (credentials.TransportCredentials, error) {
	if !cfg.Enabled {
		return insecure.NewCredentials(), nil
	}

	conf, err := tlsClientConfig(cfg)
	if err != nil {
		return nil, err
	}

	return credentials.NewTLS(conf), nil
}

// httpTransport builds the HTTP round-tripper, applying the same TLS trust options as the gRPC
// path. A plaintext client uses the default transport.
func httpTransport(cfg TLSConfig) (http.RoundTripper, error) {
	if !cfg.Enabled {
		return http.DefaultTransport, nil
	}

	conf, err := tlsClientConfig(cfg)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = conf

	return transport, nil
}

// tlsClientConfig turns the TLS trust options into a *tls.Config shared by both transports.
func tlsClientConfig(cfg TLSConfig) (*tls.Config, error) {
	if (cfg.Cert == "") != (cfg.Key == "") {
		return nil, fmt.Errorf("mutual TLS requires both --tls-cert and --tls-key, or neither")
	}

	conf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // opt-in dev-only escape hatch
	}

	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert file %q: %w", cfg.CACert, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA cert file %q contained no valid certificates", cfg.CACert)
		}

		conf.RootCAs = pool
	}

	if cfg.Cert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}

		conf.Certificates = []tls.Certificate{cert}
	}

	return conf, nil
}
