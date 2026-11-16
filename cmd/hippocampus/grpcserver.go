package main

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/hippocampus"
	"github.com/fastbean-au/hippocampus/observability"
	"github.com/fastbean-au/hippocampus/ratelimit"

	"github.com/spf13/viper"
)

// unauthenticatedExposure returns a warning when auth.method is none and a listener is bound beyond
// loopback, naming the listeners, or "" (TODO-3 item 168). That pairing is the whole store open to
// whoever can route to the host, and is a supported shape - inside a container the listener must
// bind every interface, and the published port decides the exposure - so it is said, not refused.
func unauthenticatedExposure(authMethod string, grpcBind string, gatewayBind string, gatewayPort int) string {
	if authMethod != "none" {
		return ""
	}

	var exposed []string

	if !loopbackBind(grpcBind) {
		exposed = append(exposed, "gRPC")
	}

	if gatewayPort > 0 && !loopbackBind(gatewayBind) {
		exposed = append(exposed, "HTTP gateway")
	}

	if len(exposed) == 0 {
		return ""
	}

	return "auth.method is 'none' and the " + strings.Join(exposed, " and ") + " listener binds beyond loopback: " +
		"anything that can reach it can read, write and delete the whole store. Set auth.method, or bind " +
		"127.0.0.1 (bindAddress / gateway.bindAddress) - or, in a container, publish the port on 127.0.0.1 only"
}

// loopbackBind reports whether a bind address reaches only this host. Empty binds every interface.
func loopbackBind(address string) bool {
	if address == "localhost" {
		return true
	}

	ip := net.ParseIP(strings.Trim(address, "[]"))

	return ip != nil && ip.IsLoopback()
}

// reflectionSetting decides whether to register the gRPC reflection service, and returns the reason
// alongside it so the startup line can say which of the two it took.
//
// The default is derived rather than fixed: reflection is a convenience for whoever is pointing a
// tool at the instance, and a reconnaissance surface for anybody else, so it follows the one setting
// that already distinguishes those two cases. An unauthenticated instance is a local or demo one,
// where the whole cost of not having it is a step at the moment somebody is deciding whether this is
// worth their afternoon; an instance configured for anybody but its owner turns it off. viper.IsSet
// is what makes the derived default overridable in both directions - a plain GetBool could only
// express "off", since an unset key and an explicit false read the same.
func reflectionSetting(authMethod string) (bool, string) {
	if viper.IsSet("reflection.enabled") {
		return viper.GetBool("reflection.enabled"), "set by reflection.enabled"
	}

	if authMethod == "none" {
		return true, "the default with auth.method 'none'"
	}

	return false, "the default with auth.method '" + authMethod + "'"
}

// grpcServerConfig is what newGRPCServer needs to build the gRPC server.
type grpcServerConfig struct {
	BindAddress string
	Port        int

	// GatewayBindAddress and GatewayPort are read alongside for the unauthenticated-exposure
	// warning, which names both listeners.
	GatewayBindAddress string
	GatewayPort        int

	MaxRecvMsgBytes      int
	MaxConcurrentStreams int

	KeepaliveMinTime             time.Duration
	KeepalivePermitWithoutStream bool

	// Telemetry installs the otelgrpc stats handler; it is set when tracing or metrics are on.
	Telemetry bool

	Reflection       bool
	ReflectionReason string
}

// grpcServerDeps is what the gRPC server serves and guards it with.
type grpcServerDeps struct {
	Hippocampus *hippocampus.Server

	// Served is the value registered as the service: Hippocampus wrapped for the audit log.
	Served contract.HippocampusServer

	Auth    authSetup
	Limiter *ratelimit.Limiter
	TLS     *tls.Config
}

// grpcServerConfigFromViper reads the gRPC listener's keys.
func grpcServerConfigFromViper(authMethod string, obsCfg observability.Config) grpcServerConfig {
	log.Trace("func() grpcServerConfigFromViper")

	reflectionEnabled, reflectionReason := reflectionSetting(authMethod)

	return grpcServerConfig{
		BindAddress:                  viper.GetString("bindAddress"),
		Port:                         viper.GetInt("port"),
		GatewayBindAddress:           viper.GetString("gateway.bindAddress"),
		GatewayPort:                  viper.GetInt("gateway.port"),
		MaxRecvMsgBytes:              viper.GetInt("maxRecvMsgBytes"),
		MaxConcurrentStreams:         viper.GetInt("maxConcurrentStreams"),
		KeepaliveMinTime:             time.Duration(viper.GetInt("keepalive.minTimeSeconds")) * time.Second,
		KeepalivePermitWithoutStream: viper.GetBool("keepalive.permitWithoutStream"),
		Telemetry:                    obsCfg.TracingEnabled || obsCfg.MetricsEnabled,
		Reflection:                   reflectionEnabled,
		ReflectionReason:             reflectionReason,
	}
}

// grpcInterceptors is the unary interceptor chain, in order.
//
// Panic recovery runs first (outermost) so it catches a panic from any handler or later interceptor
// and returns codes.Internal rather than letting it crash the process. RPC metrics run next, so a
// request rejected by authentication or a rate limit still appears in the rate and error counts.
// Auth follows, so an unauthenticated request is still rejected before the purge check or the
// handler does any work on it.
func grpcInterceptors(deps grpcServerDeps) []grpc.UnaryServerInterceptor {
	interceptors := []grpc.UnaryServerInterceptor{InterceptorRecoverPanic, InterceptorMetrics}

	// The instance-wide ceiling goes ahead of authentication: a flood of unauthenticated requests
	// is exactly what it exists to bound, and behind the verifier it would only ever bound requests
	// the service had already paid to verify.
	if deps.Limiter.ArrivalActive() {
		interceptors = append(interceptors, InterceptorRateLimitArrival(deps.Limiter))
	}

	if deps.Auth.enabled() {
		// Authentication first (rejects an invalid/absent token), then authorisation (rejects a
		// valid token whose role does not grant the RPC), both ahead of the purge gate and logger.
		//
		// The deployment view's caller registry sits BETWEEN the two, deliberately: a client whose
		// token is valid but whose role is refused every call is exactly the one an operator goes
		// looking for on that diagram, and behind the authoriser it would never be recorded. It is
		// appended only when auth is on, since a caller with no verified client_id is not recorded
		// at all.
		interceptors = append(interceptors,
			auth.UnaryServerInterceptor(deps.Auth.Verifier),
			deps.Hippocampus.InterceptorObserveCaller,
			deps.Auth.Authoriser.UnaryServerInterceptor(),
		)
	}

	// The per-client and per-tier levels go after both, since both of the things they key on - the
	// verified client id and the resolved tier - are stashed on the context by those interceptors.
	if deps.Limiter.PrincipalActive() {
		interceptors = append(interceptors, InterceptorRateLimitPrincipal(deps.Limiter))
	}

	return append(interceptors,
		deps.Hippocampus.InterceptorBlockWhenPurgeInProgress,
		InterceptorLogger,
	)
}

// streamInterceptors is the unary chain applied to the service's streaming RPCs (Watch).
//
// Adapted rather than written twice, so a streaming RPC can never be guarded by a different chain
// than a unary one: before Watch every RPC was unary and this chain was unary-only, and a stream
// added without it would have reached no authentication, no authorisation and no rate limit at all.
// None of the interceptors reads the request message, which is what makes the adaptation exact.
func streamInterceptors(deps grpcServerDeps) []grpc.StreamServerInterceptor {
	unary := grpcInterceptors(deps)
	out := make([]grpc.StreamServerInterceptor, 0, len(unary))

	for _, interceptor := range unary {
		out = append(out, streamFromUnary(interceptor))
	}

	return out
}

// streamFromUnary runs a unary interceptor around a stream, handing the stream on with whatever
// context the interceptor derived - the verified claims, the resolved tier.
//
// Scoped to the Hippocampus service: reflection is a stream too, and has always been reachable
// without a token where it is enabled at all (see reflectionSetting). Bringing it under the chain
// would change that as a side effect of adding an RPC.
func streamFromUnary(interceptor grpc.UnaryServerInterceptor) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
			return handler(srv, stream)
		}

		unaryInfo := &grpc.UnaryServerInfo{Server: srv, FullMethod: info.FullMethod}

		_, err := interceptor(stream.Context(), nil, unaryInfo, func(ctx context.Context, _ any) (any, error) {
			return nil, handler(srv, &contextStream{ServerStream: stream, ctx: ctx})
		})

		return err
	}
}

// contextStream is a stream carrying the context an interceptor derived.
type contextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (c *contextStream) Context() context.Context {
	return c.ctx
}

// newGRPCServer builds the gRPC server with its interceptor chain, transport limits, health
// service and, when enabled, reflection. It does not listen.
func newGRPCServer(cfg grpcServerConfig, deps grpcServerDeps) (*grpc.Server, *health.Server) {
	log.Trace("func() newGRPCServer")

	serverOpts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(grpcInterceptors(deps)...),
		grpc.ChainStreamInterceptor(streamInterceptors(deps)...),
	}

	// Raise the gRPC max-receive-message size above the 4 MiB default so operators can accept larger
	// ImportBatch/single-memory payloads (Transfer keeps its own batches under transfer.maxBatchBytes
	// by default). 0 keeps grpc-go's default.
	if cfg.MaxRecvMsgBytes > 0 {
		serverOpts = append(serverOpts, grpc.MaxRecvMsgSize(cfg.MaxRecvMsgBytes))
	}

	// Cap the concurrent HTTP/2 streams a single connection may open, bounding the work one caller
	// can pin at once - the cheapest first defence if the gRPC port is ever exposed beyond trusted
	// callers. 0 (the default) keeps grpc-go's own default.
	if cfg.MaxConcurrentStreams > 0 {
		serverOpts = append(serverOpts, grpc.MaxConcurrentStreams(uint32(cfg.MaxConcurrentStreams)))
	}

	// Enforce a keepalive policy so an abusive client cannot ping the server into a CPU/bandwidth
	// spiral. minTimeSeconds is the minimum interval the server tolerates between client pings
	// before it terminates the connection; permitWithoutStream allows keepalive pings on an idle
	// connection. Both default to grpc-go's own policy when unset (minTimeSeconds 0 leaves it).
	if cfg.KeepaliveMinTime > 0 {
		serverOpts = append(serverOpts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             cfg.KeepaliveMinTime,
			PermitWithoutStream: cfg.KeepalivePermitWithoutStream,
		}))
	}

	// The otelgrpc stats handler creates a server span for every RPC (extracting any incoming
	// trace context) and records the standard low-cardinality RPC metrics (method, service,
	// status code). Only installed when observability is enabled.
	if cfg.Telemetry {
		serverOpts = append(serverOpts, grpc.StatsHandler(otelgrpc.NewServerHandler()))
	}

	if deps.TLS != nil {
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(deps.TLS)))
	}

	s := grpc.NewServer(serverOpts...)

	hs := health.NewServer()
	healthgrpc.RegisterHealthServer(s, hs)

	contract.RegisterHippocampusServer(s, deps.Served)

	// Server reflection lets grpcurl, Postman, Insomnia and every gRPC GUI discover the schema from
	// a running instance instead of being handed contract/hippocampus.proto. It is the gRPC
	// counterpart of the gateway's /v1/openapi.json.
	//
	// The default follows auth.method, and the reason is NOT that reflection publishes the schema -
	// that argument was examined and dropped, because the proto and the generated swagger are both
	// published with the source, so there is no confidentiality here to defend (openAPIHandler
	// carries the same conclusion for the HTTP side). What is left is surface: reflection is a
	// streaming RPC outside the Hippocampus service, which the stream chain (streamFromUnary) passes
	// straight through, so it reaches neither the auth interceptor nor either rate limiter. It is the only thing on this port that is both
	// unauthenticated and unthrottled, which is enough to keep the default derived rather than a
	// flat true. Either way the choice is logged: "reflection is not working" is otherwise
	// indistinguishable from "the tool is wrong".
	if warning := unauthenticatedExposure(
		deps.Auth.Method,
		cfg.BindAddress,
		cfg.GatewayBindAddress,
		cfg.GatewayPort,
	); warning != "" {
		log.Warn(warning)
	}

	if cfg.Reflection {
		reflection.Register(s)

		log.Infof("gRPC server reflection enabled (%s): grpcurl and gRPC GUIs can discover the schema from this instance. Set reflection.enabled to false to turn it off", cfg.ReflectionReason)
	} else {
		log.Infof("gRPC server reflection disabled (%s): a gRPC tool must be given contract/hippocampus.proto, or a descriptor set built from it, rather than discovering the schema. Set reflection.enabled to true to turn it on", cfg.ReflectionReason)
	}

	return s, hs
}
