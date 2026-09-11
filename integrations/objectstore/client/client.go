// Package client dials the Hippocampus gRPC service and wraps the three RPCs this integration
// uses.
//
// Those three are the whole surface, and the list is a statement rather than an accident:
// RecallMemories to reinforce what is being read, ExplainConsolidation to ask which ids the store
// still holds, and GetForgottenMemories to catch up on what it forgot while nobody was listening.
// Nothing here writes a memory, deletes one, or purges anything - the only destructive thing this
// integration does is in the bucket, and the store is strictly the authority it reads.
//
// A consequence worth knowing when a token is minted for it: the tap needs WRITER tier, because
// RecallMemories reinforces and a reader-tier token gets a plain non-reinforcing read unless the
// deployment sets auth.readerRecallReinforces. The reaper needs only READER. Both want an UNSCOPED
// token, for the reason Recall's own doc gives.
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

// Config describes how to reach the Hippocampus gRPC service. It mirrors the trust options the
// broker bridges, the MCP bridge and the ingestor honour, so every client in this repository is
// configured the same way.
type Config struct {
	// Address is the host:port of the Hippocampus gRPC service.
	Address string

	// Token, when set, is sent as "authorization: Bearer <token>" metadata on every RPC.
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

	// Endpoint names this connection in the client RPC metrics. Empty omits the interceptor.
	Endpoint string
}

// Dial opens a gRPC client connection and returns it alongside a ready-to-use Hippocampus client.
// The caller owns the connection and must Close it.
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
// matching integrations/mcp, integrations/eventsource, integrations/ingestor and the OTEL exporter.
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
