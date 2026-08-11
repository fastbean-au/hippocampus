// Package client dials a Hippocampus gRPC service. The ingestor needs two of them - the edge it
// drains and the central instance it promotes into - so unlike the broker bridges, which have one
// connection and one set of flags, everything here is parameterised by an endpoint name.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/observability"
)

// Config describes how to reach one Hippocampus gRPC service. It mirrors the trust options the
// service's own Transfer client, the MCP bridge and the broker bridges honour, so an operator
// configures every endpoint the same way.
type Config struct {
	// Address is the host:port of the Hippocampus gRPC service.
	Address string

	// Token, when set, is sent as "authorization: Bearer <token>" metadata on every RPC, matching
	// the service's auth interceptor. The two endpoints hold different tokens: the edge's needs
	// writer plus the read side, and the target's is what stamps the group on everything promoted.
	Token string

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

	// Endpoint names this connection in the client RPC metrics ("source"/"target"), so a query can
	// tell which of the ingestor's two ends is slow or failing. Empty omits the interceptor
	// entirely, which is what a caller that does not want the instrumentation gets.
	Endpoint string
}

// Dial opens a gRPC client connection to the service described by cfg and returns it alongside a
// ready-to-use Hippocampus client. The caller owns the connection and must Close it. A bearer token,
// when configured, is attached to every outgoing RPC via a client interceptor so no call site has to
// remember to send it.
func Dial(cfg Config) (*grpc.ClientConn, contract.HippocampusClient, error) {
	creds, err := transportCredentials(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("building transport credentials: %w", err)
	}

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	// Chained rather than two WithUnaryInterceptor options, the second of which would silently
	// replace the first. The metrics interceptor is outermost so it measures the whole call
	// including the token being attached.
	var interceptors []grpc.UnaryClientInterceptor

	if cfg.Endpoint != "" {
		interceptors = append(interceptors, observability.UnaryClientMetricsInterceptor(cfg.Endpoint))
	}

	if cfg.Token != "" {
		interceptors = append(interceptors, bearerTokenInterceptor(cfg.Token))
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

// bearerTokenInterceptor stamps "authorization: Bearer <token>" onto every RPC's outgoing metadata,
// matching integrations/mcp, integrations/eventsource and the OTEL exporter.
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

// transportCredentials builds the gRPC transport credentials from the TLS* fields: plaintext when
// TLS is off; otherwise TLS against the system pool, an optional private-CA bundle, an optional
// client certificate for mutual TLS, and an insecureSkipVerify escape hatch.
func transportCredentials(cfg Config) (credentials.TransportCredentials, error) {
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
