package contract

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// ClientVersionHeader is the request header (gRPC metadata key, and the HTTP header of the same
// name on the gateway) a client uses to report its own build to the service it calls. The service
// shows it on the client's node in the deployment topology view - the only way a caller the service
// holds no address for, and so cannot probe, can have a version on that diagram at all.
//
// It lives here, beside the generated client, because this is the one package every client already
// imports: a home anywhere heavier would pull the service's dependency tree into the CLI and the MCP
// bridge for the sake of one string.
//
// A dedicated header rather than the user agent, because the user agent is somebody else's: grpc-go
// appends its own product token to whatever a client sets, a browser sends "Mozilla/5.0", and a
// third-party client sends only its library's version. Parsing a client's version out of that
// would put a library's or a browser's version on the diagram as if it were the client's, where a
// dedicated header is either the client's own statement or absent.
//
// The value is free-form, conventionally "<product>/<version>" ("hippo/0.52.0"). It is a claim
// rather than a fact, and the service bounds and sanitises it before showing it as one.
const ClientVersionHeader = "hippocampus-client-version"

// UnaryClientVersionInterceptor attaches ClientVersionHeader to every outgoing RPC. An empty
// version attaches nothing, so a caller can install it unconditionally.
func UnaryClientVersionInterceptor(version string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		if version != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, ClientVersionHeader, version)
		}

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
