package main

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/ratelimit"
)

// The rate-limit adapters live here rather than in the ratelimit package, for the same reason
// rpcmetrics.go does: they need the two things that are transport-specific and already settled in
// this package - the service prefix and /v1 path scoping that keep probes, the console, and the
// login endpoints out of the limit, and the identification of a principal from a gRPC peer or an
// HTTP request. The package itself stays transport-agnostic, so both transports share one set of
// buckets and cannot drift into enforcing different policies.

// rateLimitRejected counts requests refused by a limit, by transport and by which level of the
// hierarchy refused. The principal is deliberately not an attribute: it is unbounded, and the whole
// point of a rejection metric is that it stays cheap while under attack.
var rateLimitRejected = newRateLimitCounter()

func newRateLimitCounter() metric.Int64Counter {
	c, err := otel.Meter(interceptorScopeName).Int64Counter(
		"hippocampus.ratelimit.rejected",
		metric.WithDescription("Requests refused by a rate limit, by transport and by the level of the hierarchy that refused."),
	)
	if err != nil {
		log.Errorf("failed to create rate limit counter: %s", err.Error())
	}

	return c
}

// registerRateLimitGauge publishes how many principals currently hold a bucket. It is registered at
// wiring time rather than at package init because a callback registered against the global no-op
// meter would never be re-registered against the real provider main installs.
//
// It is worth publishing because the per-client table is bounded: a value sitting at
// rateLimit.maxClients means the table is churning - either the deployment has more callers than
// the cap allows for, or something is minting identities - and a churning table is one whose
// evicted callers get a fresh full bucket.
func registerRateLimitGauge(limiter *ratelimit.Limiter) {
	meter := otel.Meter(interceptorScopeName)

	clients, err := meter.Int64ObservableGauge(
		"hippocampus.ratelimit.clients",
		metric.WithDescription("Principals currently holding a per-client rate-limit bucket."),
	)
	if err != nil {
		log.Errorf("failed to create rate limit clients gauge: %s", err.Error())

		return
	}

	callback := func(ctx context.Context, o metric.Observer) error {
		o.ObserveInt64(clients, int64(limiter.Clients()))

		return nil
	}

	if _, err := meter.RegisterCallback(callback, clients); err != nil {
		log.Errorf("failed to register the rate limit clients callback: %s", err.Error())
	}
}

// recordRateLimitRejection counts one refusal.
func recordRateLimitRejection(ctx context.Context, transport string, scope ratelimit.Scope) {
	rateLimitRejected.Add(ctx, 1, metric.WithAttributes(
		attribute.String("transport", transport),
		attribute.String("scope", string(scope)),
	))
}

// retryAfterSeconds renders a wait as the whole number of seconds a Retry-After carries, rounded up
// and never below 1 - a "come back in 0 seconds" would invite exactly the immediate retry the limit
// is there to prevent.
func retryAfterSeconds(decision ratelimit.Decision) string {
	return strconv.Itoa(max(1, int(math.Ceil(decision.RetryAfter.Seconds()))))
}

// InterceptorRateLimitArrival enforces the global ceiling on gRPC. It is installed ahead of
// authentication so a flood is bounded before the service pays for verifying the tokens in it -
// which, with an identity provider, is an RS256 signature per request. That position is also why it
// can only enforce the global level: nothing has identified the caller yet, and a limit keyed on an
// unverified token would be avoided by minting a new client id per request.
func InterceptorRateLimitArrival(limiter *ratelimit.Limiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if !strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
			return handler(ctx, req)
		}

		if decision := limiter.Arrival(); !decision.Allowed {
			return nil, rejectRPC(ctx, info.FullMethod, "", decision)
		}

		return handler(ctx, req)
	}
}

// InterceptorRateLimitPrincipal enforces the per-client and per-tier levels on gRPC. It is
// installed after authentication and authorisation, because both of the things it keys on - the
// verified client id and the resolved tier - are stashed on the context by those two interceptors.
// With authentication disabled there are no claims to read, so it falls back to the peer address:
// the default deployment has no principal, and a limit that did nothing there would be a limit that
// does nothing by default.
func InterceptorRateLimitPrincipal(limiter *ratelimit.Limiter) grpc.UnaryServerInterceptor {
	return func(ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if !strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
			return handler(ctx, req)
		}

		principal := grpcPrincipalKey(ctx)

		if decision := limiter.Principal(principal, tierName(ctx)); !decision.Allowed {
			return nil, rejectRPC(ctx, info.FullMethod, principal, decision)
		}

		return handler(ctx, req)
	}
}

// rejectRPC counts the refusal, attaches the wait as a trailer, and returns the status. The wait
// goes in a trailer rather than a header because a handler that returns an error without sending a
// message produces a trailers-only response, and a trailer is the part of it a client is certain to
// see.
func rejectRPC(ctx context.Context, method string, principal string, decision ratelimit.Decision) error {
	recordRateLimitRejection(ctx, transportGRPC, decision.Scope)
	logRateLimitRejection(transportGRPC, method, principal, decision)

	_ = grpc.SetTrailer(ctx, metadata.Pairs("retry-after", retryAfterSeconds(decision)))

	return status.Errorf(codes.ResourceExhausted, "rate limit exceeded (%s)", decision.Scope)
}

// logRateLimitRejection is the one place a refusal is written to the log, and it is at Debug on
// purpose. Both enforcement points sit ahead of InterceptorLogger and httpLoggingMiddleware in
// their chains (the whole point of the arrival one is to reject before anything else runs), so a
// throttled request is otherwise invisible in the log at any level - and a level that is on by
// default would mean a flood writing a line per rejected request, which is a second denial of
// service on the log pipeline. The counter is the signal to alert on; this is what an operator
// turns up to find out which caller is being throttled. The principal is empty for the global
// ceiling, which by construction does not know who is calling.
func logRateLimitRejection(transport string, route string, principal string, decision ratelimit.Decision) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}

	fields := log.Fields{
		"transport":   transport,
		"route":       route,
		"scope":       string(decision.Scope),
		"retry_after": retryAfterSeconds(decision),
	}

	if principal != "" {
		fields["principal"] = principal
	}

	log.WithFields(fields).Debug("rate limit refused a request")
}

// tierName returns the caller's resolved authorisation tier as the name the configuration uses, or
// "" when authorisation did not run (auth disabled), in which case no per-tier bucket applies.
func tierName(ctx context.Context) string {
	tier, ok := auth.TierFromContext(ctx)
	if !ok {
		return ""
	}

	return tier.String()
}

// grpcPrincipalKey identifies the caller a per-client bucket belongs to. The verified client id is
// preferred, then the token's subject, then the peer's IP address.
//
// The prefixes matter: without them a client id that happens to read as an address would share a
// bucket with that address, and the fallback is precisely the case where an attacker chooses part
// of the key.
func grpcPrincipalKey(ctx context.Context) string {
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		if claims.ClientID != "" {
			return "client:" + claims.ClientID
		}

		if claims.Subject != "" {
			return "sub:" + claims.Subject
		}
	}

	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return ""
	}

	return "ip:" + hostOnly(p.Addr.String())
}

// hostOnly strips the port from an address, so a caller's every connection shares one bucket rather
// than getting a fresh one per ephemeral source port - which would make a per-client limit
// trivially avoidable.
func hostOnly(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}

	return host
}

// rateLimitArrivalMiddleware is the gateway counterpart to InterceptorRateLimitArrival, and exists
// for the reason every gateway middleware here does: the gateway calls the service directly and
// never runs the gRPC interceptor chain, so without it half the RPC surface would be unlimited.
//
// It is scoped to the RPC surface by the same isRPCPath the metrics use, so the health and
// readiness probes, the console, the login endpoints, and the OpenAPI document are never throttled.
// Throttling a probe would turn a busy instance into a restarted one.
func rateLimitArrivalMiddleware(limiter *ratelimit.Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRPCPath(r.URL.Path) {
			next.ServeHTTP(w, r)

			return
		}

		if decision := limiter.Arrival(); !decision.Allowed {
			rejectHTTP(w, r, "", decision)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// rateLimitGatewayMiddleware is the gateway counterpart to InterceptorRateLimitPrincipal. Unlike
// the arrival middleware it is a grpc-gateway middleware rather than an outer http.Handler, and it
// has to be: the caller's tier is stashed on the request context by the authoriser, which is itself
// a post-routing gateway middleware, so an outer wrapper would run before the tier existed and
// would silently enforce no per-tier limit on this transport at all.
//
// Registering it after the authoriser is therefore load-bearing, and costs nothing: a request the
// authoriser rejects never ran, so not charging it to a bucket is right.
//
// It needs no path scoping of its own - the gateway only invokes middlewares on a matched /v1
// route, so the probes, the console, the login endpoints, and the OpenAPI document never reach it.
// trustForwardedFor decides whether the fallback identification believes X-Forwarded-For; see
// httpPrincipalKey.
func rateLimitGatewayMiddleware(limiter *ratelimit.Limiter, trustForwardedFor bool) runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			principal := httpPrincipalKey(r, trustForwardedFor)

			decision := limiter.Principal(principal, tierName(r.Context()))
			if !decision.Allowed {
				rejectHTTP(w, r, principal, decision)

				return
			}

			next(w, r, pathParams)
		}
	}
}

// rejectHTTP counts the refusal and writes a 429 in the same small JSON shape the auth middleware's
// 401 and the authoriser's 403 use, with the RFC 9110 Retry-After header.
func rejectHTTP(w http.ResponseWriter, r *http.Request, principal string, decision ratelimit.Decision) {
	recordRateLimitRejection(r.Context(), transportHTTP, decision.Scope)
	logRateLimitRejection(transportHTTP, r.Method+" "+r.URL.Path, principal, decision)

	w.Header().Set("Retry-After", retryAfterSeconds(decision))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded", "scope": string(decision.Scope)})
}

// httpPrincipalKey is the gateway's counterpart to grpcPrincipalKey, with the same preference order
// and the same prefixes.
//
// X-Forwarded-For is consulted only when trustForwardedFor says to, and that default is off on
// purpose: the header is caller-supplied, so believing it on a directly-reachable listener hands
// every caller the ability to pick its own bucket - and therefore an unlimited number of them. It
// is safe only behind a proxy that overwrites rather than appends, which is why the leftmost entry
// is the one taken (the client the proxy saw) rather than a walk back through a chain the service
// cannot verify.
func httpPrincipalKey(r *http.Request, trustForwardedFor bool) string {
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		if claims.ClientID != "" {
			return "client:" + claims.ClientID
		}

		if claims.Subject != "" {
			return "sub:" + claims.Subject
		}
	}

	if trustForwardedFor {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			if client := strings.TrimSpace(strings.Split(forwarded, ",")[0]); client != "" {
				return "ip:" + client
			}
		}
	}

	return "ip:" + hostOnly(r.RemoteAddr)
}
