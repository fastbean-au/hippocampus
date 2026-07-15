package auth

import (
	"context"
	"strings"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// hippocampusServicePrefix scopes the interceptor to the Hippocampus service, mirroring
// InterceptorBlockWhenPurgeInProgress in hippocampus/server.go. Anything outside this prefix -
// principally the gRPC health service - is never touched, so it stays reachable without a token
// for orchestrator liveness/readiness probes.
const hippocampusServicePrefix = "/proto.Hippocampus/"

// UnaryServerInterceptor returns a gRPC interceptor that requires a valid bearer token, carried
// in the "authorization" metadata key as "Bearer <token>", on every Hippocampus RPC. It is a free
// function rather than a method on a server type: it only closes over an immutable Verifier, so
// whether it runs at all is decided once at startup by whoever builds the interceptor chain.
func UnaryServerInterceptor(v Verifier) grpc.UnaryServerInterceptor {
	return func(ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		if !strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
			return handler(ctx, req)
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok || len(md.Get("authorization")) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing authorization metadata")
		}

		token, err := ExtractBearerToken(md.Get("authorization")[0])
		if err != nil {
			log.Trace("rejecting request - malformed authorization metadata")

			return nil, status.Error(codes.Unauthenticated, err.Error())
		}

		if _, err := v.Verify(token); err != nil {
			log.Trace("rejecting request - invalid token")

			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}

		return handler(ctx, req)
	}
}
