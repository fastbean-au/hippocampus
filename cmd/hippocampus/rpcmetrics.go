package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
)

// hippocampusServicePrefix scopes the RPC instrumentation to the Hippocampus service, mirroring the
// same check in the auth interceptor and the purge gate: the health service is a probe surface, not
// an RPC, and counting it would put a probe's steady tick in the denominator of every error-rate
// query.
const hippocampusServicePrefix = "/hippocampus.v1.Hippocampus/"

// Attribute values for the RPC metrics. Every one is drawn from a fixed set - two transports, the
// RPCs named in the service descriptor, the gRPC code names or HTTP status codes, three outcomes -
// so these instruments stay inside the repo's rule that no metric attribute may carry unbounded
// cardinality.
const (
	transportGRPC = "grpc"
	transportHTTP = "http"

	// outcome is deliberately three-valued rather than a success bool. A client sending malformed
	// requests is not the service failing, so an SLO or alert built on a bool would fire on traffic
	// the operator cannot fix; separating client from server fault makes "is it broken" answerable
	// with one expression while leaving the client-fault rate visible in its own right.
	outcomeOK          = "ok"
	outcomeClientError = "client_error"
	outcomeServerError = "server_error"

	// rpcUnknown labels a request that never reached routing - rejected by authentication, by the
	// body-size cap, or matching no route at all - so it can be counted without its path (which
	// carries ids) becoming the label.
	rpcUnknown = "unknown"
)

// rpcRequests and rpcDuration are the RED instruments: rate and errors come from the counter's
// outcome/code attributes, duration from the histogram. They are built from the global meter (like
// panicsRecovered and hippocampus.tel), so they are no-ops when observability is disabled and pick
// up the real provider installed in main() otherwise.
var rpcRequests, rpcDuration = newRPCInstruments()

func newRPCInstruments() (metric.Int64Counter, metric.Float64Histogram) {
	meter := otel.Meter(interceptorScopeName)

	requests, err := meter.Int64Counter(
		"hippocampus.rpc.requests",
		metric.WithDescription("Hippocampus RPCs served, by transport, RPC, response code, and outcome."),
	)
	if err != nil {
		log.Errorf("failed to create RPC request counter: %s", err.Error())
	}

	duration, err := meter.Float64Histogram(
		"hippocampus.rpc.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Server-side duration of a Hippocampus RPC in seconds, by transport, RPC, response code, and outcome."),
	)
	if err != nil {
		log.Errorf("failed to create RPC duration histogram: %s", err.Error())
	}

	return requests, duration
}

// recordRPC writes one request to both instruments under the same attribute set, so a rate and a
// latency quantile for the same slice of traffic are always filtered identically.
func recordRPC(ctx context.Context,
	transport string,
	rpc string,
	code string,
	outcome string,
	duration time.Duration,
) {
	attrs := metric.WithAttributes(
		attribute.String("transport", transport),
		attribute.String("rpc", rpc),
		attribute.String("code", code),
		attribute.String("outcome", outcome),
	)

	rpcRequests.Add(ctx, 1, attrs)
	rpcDuration.Record(ctx, duration.Seconds(), attrs)
}

// outcomeForCode classifies a gRPC status. It reuses isClientFaultCode, the same split
// InterceptorLogger uses to decide between an Info and a Warn line, so a request logged as a client
// mistake is also counted as one.
func outcomeForCode(code codes.Code) string {
	switch {

	case code == codes.OK:
		return outcomeOK

	case isClientFaultCode(code):
		return outcomeClientError

	default:
		return outcomeServerError

	}
}

// outcomeForStatus is the HTTP counterpart to outcomeForCode. The gateway translates a gRPC code
// into a status before this sees it, so classification is by status class rather than by inverting
// that mapping - which is lossy (400 alone stands in for three codes) and would report a code the
// service never returned.
func outcomeForStatus(status int) string {
	switch {

	case status >= http.StatusInternalServerError:
		return outcomeServerError

	case status >= http.StatusBadRequest:
		return outcomeClientError

	default:
		return outcomeOK

	}
}

// InterceptorMetrics records the RED metrics for a gRPC call. It is installed second in the chain -
// inside panic recovery, but outside authentication - so a request rejected for a bad token still
// appears in the rate and error counts, and with its RPC name, which gRPC knows before any
// interceptor runs.
//
// The recording is deliberately not deferred. Panic recovery is installed outside this interceptor,
// so a panicking handler unwinds straight through here with no error to classify, and a deferred
// record would count that call as a success. A recovered panic is counted by
// hippocampus.panics_recovered instead, which is the metric an operator would alert on anyway.
func InterceptorMetrics(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	if !strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
		return handler(ctx, req)
	}

	ts := time.Now()

	resp, err := handler(ctx, req)

	code := status.Code(err)

	recordRPC(ctx,
		transportGRPC,
		strings.TrimPrefix(info.FullMethod, hippocampusServicePrefix),
		code.String(),
		outcomeForCode(code),
		time.Since(ts),
	)

	return resp, err
}

// routeCapture carries the matched RPC name from inside the gateway mux back out to
// httpMetricsMiddleware. The two cannot be one middleware: only a post-routing gateway middleware
// knows which route matched, but only an outer http.Handler sees requests rejected before routing
// (authentication failures, oversized bodies, unmatched paths), and those are exactly the requests
// an error-rate metric must not miss. It is written and read on one request's goroutine, so it needs
// no synchronisation.
type routeCapture struct {
	rpc string
}

// routeCaptureKey is the private context key under which httpMetricsMiddleware passes the capture
// down to gatewayRouteMiddleware.
type routeCaptureKey struct{}

// gatewayRouteMiddleware names the RPC a gateway request resolved to. It must be registered first in
// runtime.WithMiddlewares so it runs outermost of the gateway middlewares - ahead of the authoriser,
// whose rejection would otherwise leave a 403 uncounted against the RPC it was aimed at.
//
// The name comes from the matched pattern rather than the request path: the path holds memory and
// event ids, and labelling a metric with it would put unbounded cardinality into the time series
// database. A route with no policy entry leaves the capture at rpcUnknown for the same reason.
func gatewayRouteMiddleware() runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			capture, ok := r.Context().Value(routeCaptureKey{}).(*routeCapture)
			if !ok {
				// No capture on the context means the gateway is serving a request that did not come
				// through httpMetricsMiddleware (only possible in a test); nothing to record against.
				next(w, r, pathParams)

				return
			}

			if pattern, found := runtime.HTTPPattern(r.Context()); found {
				if rpc, known := auth.RouteRPC(r.Method, pattern.String()); known {
					capture.rpc = rpc
				}
			}

			next(w, r, pathParams)
		}
	}
}

// isRPCPath reports whether a gateway request is a call on the RPC surface, and so belongs in the
// RPC metrics. Everything else the gateway serves - the health and readiness probes, the console and
// its front-channel config, the login endpoints, the static OpenAPI document - is a support surface
// whose traffic would distort a request rate and an error rate meant to describe the service's work.
func isRPCPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") && path != openAPIPath
}

// isRateLimitedPath reports whether a gateway request is subject to the arrival rate limiter. It is
// isRPCPath plus the OpenAPI document, and the two differ on purpose.
//
// The document is excluded from the METRICS because it is not the service's work, and counting it
// would distort the request and error rates the shipped alert rules read. But it is 148 KB of
// unauthenticated response, and until it was brought under the limiter it was the cheapest
// bandwidth amplifier the gateway offered - cheaper than gRPC reflection, which at least requires a
// gRPC client. Being uninteresting to measure and being safe to serve without a ceiling are
// different questions, and this predicate is where they stopped being answered together.
func isRateLimitedPath(path string) bool {
	return isRPCPath(path) || path == openAPIPath
}

// httpMetricsMiddleware is the gateway counterpart to InterceptorMetrics, and exists for the same
// reason httpLoggingMiddleware does: the gateway calls the service directly and never runs the gRPC
// interceptor chain, so without this half the RPC surface reports no rate, errors, or latency at all.
//
// It is installed inside panic recovery but outside authentication and the body-size cap, so a
// request rejected by either is still counted (as rpcUnknown, since neither rejection has routed the
// request far enough to know which RPC it was for). Like the gRPC interceptor it does not defer the
// recording, so a panic recovered above it is left to hippocampus.panics_recovered rather than
// counted here as a success.
func httpMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRPCPath(r.URL.Path) {
			next.ServeHTTP(w, r)

			return
		}

		capture := &routeCapture{rpc: rpcUnknown}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		ts := time.Now()

		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), routeCaptureKey{}, capture)))

		recordRPC(r.Context(),
			transportHTTP,
			capture.rpc,
			strconv.Itoa(recorder.status),
			outcomeForStatus(recorder.status),
			time.Since(ts),
		)
	})
}
