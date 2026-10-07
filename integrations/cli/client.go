package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/dial"
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

	// ClientVersion is reported to the service on every request in contract.ClientVersionHeader, on
	// either transport, so the service's deployment view can say which build of the CLI is calling.
	ClientVersion string
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

// newGRPCClient dials the service over gRPC through the shared dial package (TODO-3 item 172), which
// attaches the bearer token and the version header. grpc.NewClient is lazy, so the dial itself never
// blocks here; a bad address surfaces on the first RPC.
func newGRPCClient(cfg Config) (contract.HippocampusClient, func() error, error) {
	conn, client, err := dial.Dial(dialConfig(cfg))
	if err != nil {
		return nil, nil, err
	}

	return client, conn.Close, nil
}

// dialConfig is the CLI's connection settings in the shared package's terms.
func dialConfig(cfg Config) dial.Config {
	return dial.Config{
		Address:               cfg.Address,
		Token:                 cfg.Token,
		TLS:                   cfg.TLS.Enabled,
		TLSCACertFile:         cfg.TLS.CACert,
		TLSCertFile:           cfg.TLS.Cert,
		TLSKeyFile:            cfg.TLS.Key,
		TLSInsecureSkipVerify: cfg.TLS.InsecureSkipVerify,
		ClientVersion:         cfg.ClientVersion,
	}
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
		baseURL:       strings.TrimRight(base, "/"),
		token:         cfg.Token,
		clientVersion: cfg.ClientVersion,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}

	return client, func() error { return nil }, nil
}

// httpTransport builds the HTTP round-tripper, applying the same TLS trust options as the gRPC
// path. A plaintext client uses the default transport.
func httpTransport(cfg TLSConfig) (http.RoundTripper, error) {
	if !cfg.Enabled {
		return http.DefaultTransport, nil
	}

	conf, err := dial.TLSClientConfig(dialConfig(Config{TLS: cfg}))
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = conf

	return transport, nil
}
