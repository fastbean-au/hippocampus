package main

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const interceptorScopeName = "github.com/fastbean-au/hippocampus/cmd/hippocampus"

// panicsRecovered counts panics caught by the recovery interceptor and the gateway recovery
// middleware, labelled by transport. It is built from the global meter (like hippocampus.tel), so
// it is a no-op when observability is disabled and picks up the real provider installed in main()
// otherwise.
var panicsRecovered = newPanicCounter()

func newPanicCounter() metric.Int64Counter {
	c, err := otel.Meter(interceptorScopeName).Int64Counter(
		"hippocampus.panics_recovered",
		metric.WithDescription("Panics recovered by the gRPC interceptor or HTTP gateway middleware, by transport."),
	)
	if err != nil {
		log.Errorf("failed to create panics counter: %s", err.Error())
	}

	return c
}

// InterceptorRecoverPanic recovers a panic from anywhere below it in the interceptor chain (a
// handler or a later interceptor), logs it with a stack trace, records a metric, and returns
// codes.Internal so the process survives instead of crashing. grpc-go does not recover handler
// panics itself, so without this a single poison request would take down the whole instance (and,
// on the consolidating instance, the consolidator role until it is restarted). It is installed
// first (outermost) in the chain so it wraps every other interceptor as well as the handler,
// matching the HTTP gateway, whose net/http server already recovers panics per connection (the
// middleware counterpart, recoverMiddleware, only improves the response it returns).
func InterceptorRecoverPanic(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("recovered panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			panicsRecovered.Add(ctx, 1, metric.WithAttributes(attribute.String("transport", "grpc")))

			err = status.Error(codes.Internal, "internal error")
		}
	}()

	return handler(ctx, req)
}

// recoverMiddleware is the HTTP counterpart to InterceptorRecoverPanic, wrapping the gateway mux
// outermost. net/http already recovers a handler panic per connection, but it aborts the
// connection without a response and logs to its own error writer; this returns a clean JSON 500
// (matching the gateway's other error responses), routes the log through logrus, and records the
// same metric. A panic after the response has begun cannot be turned into a 500 (the header is
// already sent), but is still logged.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Errorf("recovered panic serving %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				panicsRecovered.Add(r.Context(), 1, metric.WithAttributes(attribute.String("transport", "http")))

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func InterceptorLogger(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	log.Tracef("gRPC -> %s", info.FullMethod)

	ts := time.Now()

	resp, err := handler(ctx, req)

	log.Tracef("gRPC <- %s (%d us)", info.FullMethod, time.Since(ts).Microseconds())

	return resp, err
}
