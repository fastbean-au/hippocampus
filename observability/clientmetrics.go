package observability

import (
	"context"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const clientScopeName = "github.com/fastbean-au/hippocampus/observability"

// clientTel holds the client-side RED instruments, shared by every component that dials a
// Hippocampus instance. Built from the global providers, so it is a no-op until observability is
// initialised.
var clientTel = newClientTelemetry()

type clientTelemetry struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
}

func newClientTelemetry() *clientTelemetry {
	meter := otel.Meter(clientScopeName)

	requests, err := meter.Int64Counter(
		"hippocampus.client.rpc.requests",
		metric.WithDescription("RPCs made to a Hippocampus instance, by endpoint, rpc, code and outcome."),
	)
	if err != nil {
		log.Errorf("failed to create the client request counter: %s", err.Error())
	}

	duration, err := meter.Float64Histogram(
		"hippocampus.client.rpc.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration in seconds of each RPC made to a Hippocampus instance."),
	)
	if err != nil {
		log.Errorf("failed to create the client duration histogram: %s", err.Error())
	}

	return &clientTelemetry{requests: requests, duration: duration}
}

// UnaryClientMetricsInterceptor records the rate, errors and duration of every RPC a client makes,
// tagged with which endpoint it went to.
//
// It is the client-side counterpart to the service's own RED metrics, and it is deliberately here
// rather than in each integration: the ingestor dials two instances and the broker bridges dial one,
// but "how many calls, how many failed, how long did they take" is the same question in both, and
// two copies of it would drift.
//
// Three things follow the service's rpcmetrics.go rather than being invented here. The `rpc`
// attribute is the method's short name, never a path carrying ids. `outcome` is three-valued
// (ok/client_error/server_error) rather than a success bool, so an alert can fire on the far end
// failing without also firing when this client sends something invalid. And the recording is NOT
// deferred around the invoker: a panic in a call must not be counted as a success.
func UnaryClientMetricsInterceptor(endpoint string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		start := time.Now()

		err := invoker(ctx, method, req, reply, cc, opts...)

		code := status.Code(err)

		attrs := WithGroup(
			attribute.String("endpoint", endpoint),
			attribute.String("rpc", rpcName(method)),
			attribute.String("code", code.String()),
			attribute.String("outcome", clientOutcome(code)),
		)

		clientTel.requests.Add(ctx, 1, attrs)
		clientTel.duration.Record(ctx, time.Since(start).Seconds(), attrs)

		return err
	}
}

// rpcName reduces a full gRPC method ("/hippocampus.v1.Hippocampus/GetMemories") to its short name.
// The full path is bounded and would be safe as an attribute, but the short name is what the
// service's own metrics use, so a query joins across the two without a translation step.
func rpcName(method string) string {
	if index := strings.LastIndex(method, "/"); index >= 0 {
		return method[index+1:]
	}

	return method
}

// clientOutcome classifies a response the way the service classifies its own, so "the error rate"
// means one thing across both sides of the connection. The list of client-fault codes is the
// service's isClientFaultCode, read from the caller's side: an InvalidArgument is this client
// sending something wrong, and must not appear in an alert about the far end being unhealthy.
func clientOutcome(code codes.Code) string {
	switch code {

	case codes.OK:
		return "ok"

	case codes.InvalidArgument,
		codes.NotFound,
		codes.AlreadyExists,
		codes.PermissionDenied,
		codes.Unauthenticated,
		codes.FailedPrecondition,
		codes.OutOfRange,
		codes.Canceled,
		codes.ResourceExhausted:

		return "client_error"

	default:
		return "server_error"

	}
}
