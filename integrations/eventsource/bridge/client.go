package bridge

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/observability"
)

// ClientConfig describes how to reach the Hippocampus gRPC service. It mirrors the trust options the
// service's own Transfer client and the MCP bridge honour, so an operator configures the broker
// bridges the same way they configure everything else that dials the service.
type ClientConfig struct {
	// Address is the host:port of the Hippocampus gRPC service.
	Address string

	// Token, when set, is sent as "authorization: Bearer <token>" metadata on every RPC, matching
	// the service's auth interceptor. It is the right shape for a hand-run bridge; for a long-running
	// one against an IdP prefer OIDC below, since a static token eventually expires and the bridge
	// then fails every write for as long as it is left running.
	Token string

	// OIDC, when its ClientID is set, mints and refreshes access tokens via the client-credentials
	// grant instead of using Token. This is what a bridge deployed beside an IdP-backed service
	// wants, and it takes precedence over Token.
	OIDC OIDCConfig

	// TLS enables a TLS dial. The remaining TLS* fields are consulted only when TLS is true.
	TLS bool

	// TLSCACertFile is a PEM CA bundle to verify the service certificate against, in place of the
	// system pool.
	TLSCACertFile string

	// TLSCertFile and TLSKeyFile together enable mutual TLS (both or neither).
	TLSCertFile string
	TLSKeyFile  string

	// TLSInsecureSkipVerify skips verification of the service certificate (dev only).
	TLSInsecureSkipVerify bool

	// Endpoint names this connection in the client RPC metrics. Empty omits the metrics interceptor
	// entirely, which is what a caller that does not want the instrumentation gets.
	Endpoint string
}

// Dial opens a gRPC client connection to the service described by cfg and returns it alongside a
// ready-to-use Hippocampus client. The caller owns the connection and must Close it. A bearer token,
// when configured, is attached to every outgoing RPC via a client interceptor so no call site has to
// remember to send it - whether that token is the fixed one from --token or one minted and
// refreshed via the OIDC client-credentials grant.
//
// Dial performs no network I/O of its own (grpc.NewClient does not block, and OIDC discovery is
// deferred to the first RPC), so it fails only on genuine misconfiguration.
func Dial(cfg ClientConfig) (*grpc.ClientConn, contract.HippocampusClient, error) {
	creds, err := transportCredentials(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("building transport credentials: %w", err)
	}

	source, err := tokenSource(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("configuring bearer-token auth: %w", err)
	}

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	// Chained rather than two WithUnaryInterceptor options, the second of which would silently
	// replace the first. The metrics interceptor is outermost so it measures the whole call
	// including the token being attached - and, with OIDC, including a refresh when one falls due,
	// which is exactly the latency an operator would want to see attributed to the RPC.
	var interceptors []grpc.UnaryClientInterceptor

	if cfg.Endpoint != "" {
		interceptors = append(interceptors, observability.UnaryClientMetricsInterceptor(cfg.Endpoint))
	}

	if source != nil {
		interceptors = append(interceptors, bearerInterceptor(source))
	}

	if len(interceptors) > 0 {
		dialOpts = append(dialOpts, grpc.WithChainUnaryInterceptor(interceptors...))
	}

	conn, err := grpc.NewClient(cfg.Address, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("creating gRPC client for %q: %w", cfg.Address, err)
	}

	return conn, contract.NewHippocampusClient(conn), nil
}

// transportCredentials builds the gRPC transport credentials from the TLS* fields, mirroring the
// MCP bridge's transportCredentials: plaintext when TLS is off; otherwise TLS against the system
// pool, an optional private-CA bundle, an optional client certificate for mutual TLS, and an
// insecureSkipVerify escape hatch.
func transportCredentials(cfg ClientConfig) (credentials.TransportCredentials, error) {
	if !cfg.TLS {
		return insecure.NewCredentials(), nil
	}

	if (cfg.TLSCertFile == "") != (cfg.TLSKeyFile == "") {
		return nil, fmt.Errorf("mutual TLS requires both a certificate and a key, or neither")
	}

	conf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.TLSInsecureSkipVerify,
	}

	if cfg.TLSCACertFile != "" {
		pem, err := os.ReadFile(cfg.TLSCACertFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA cert file %q: %w", cfg.TLSCACertFile, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA cert file %q contained no valid certificates", cfg.TLSCACertFile)
		}

		conf.RootCAs = pool
	}

	if cfg.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}

		conf.Certificates = []tls.Certificate{cert}
	}

	return credentials.NewTLS(conf), nil
}
