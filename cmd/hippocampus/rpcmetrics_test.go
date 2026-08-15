package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// installRPCInstruments points a fresh SDK meter provider (backed by a manual reader) at the RPC
// instruments and returns the reader. It rebuilds the package-level instruments rather than relying
// on the global provider's delegation, because that delegation is one-time: instruments built at
// package init would bind to whichever test installed a provider first, and every later test would
// collect nothing. Both the instruments and a known-inert global provider are restored afterwards.
func installRPCInstruments(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()

	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	requests, duration := rpcRequests, rpcDuration
	rpcRequests, rpcDuration = newRPCInstruments()

	t.Cleanup(func() {
		rpcRequests, rpcDuration = requests, duration

		otel.SetMeterProvider(noop.NewMeterProvider())
	})

	return reader
}

// collectRPCPoints collects one reading and returns the attribute sets recorded against the named
// instrument, each with the value at that point, so a test can assert both what was labelled and how
// many calls landed on it.
func collectRPCPoints(t *testing.T, reader *sdkmetric.ManualReader, name string) map[attribute.Distinct]int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics

	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %s", err.Error())
	}

	out := map[attribute.Distinct]int64{}

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}

			switch data := m.Data.(type) {

			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					out[dp.Attributes.Equivalent()] = dp.Value
				}

			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					out[dp.Attributes.Equivalent()] = int64(dp.Count)
				}

			default:
				t.Fatalf("unexpected data type %T for metric %q", m.Data, name)

			}
		}
	}

	return out
}

// rpcAttributes rebuilds the attribute set recordRPC writes, so a test can look a data point up by
// the labels it expects rather than by scanning.
func rpcAttributes(transport string, rpc string, code string, outcome string) attribute.Distinct {
	set := attribute.NewSet(
		attribute.String("transport", transport),
		attribute.String("rpc", rpc),
		attribute.String("code", code),
		attribute.String("outcome", outcome),
	)

	return set.Equivalent()
}

// TestInterceptorMetrics verifies that a gRPC call is counted and timed under the bare RPC name, the
// status code, and the outcome its code classifies to - and that the handler's own response is
// passed through untouched.
func TestInterceptorMetrics(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		err         error
		wantRPC     string
		wantCode    string
		wantOutcome string
	}{
		{
			name:        "success",
			method:      "/hippocampus.v1.Hippocampus/StoreMemory",
			err:         nil,
			wantRPC:     "StoreMemory",
			wantCode:    "OK",
			wantOutcome: outcomeOK,
		},
		{
			name:        "client fault",
			method:      "/hippocampus.v1.Hippocampus/GetEventById",
			err:         status.Error(codes.NotFound, "no such event"),
			wantRPC:     "GetEventById",
			wantCode:    "NotFound",
			wantOutcome: outcomeClientError,
		},
		{
			name:        "server fault",
			method:      "/hippocampus.v1.Hippocampus/Sleep",
			err:         status.Error(codes.Internal, "boom"),
			wantRPC:     "Sleep",
			wantCode:    "Internal",
			wantOutcome: outcomeServerError,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reader := installRPCInstruments(t)

			handler := func(ctx context.Context, req interface{}) (interface{}, error) {
				return "response", c.err
			}

			resp, err := InterceptorMetrics(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: c.method}, handler)
			if resp != "response" {
				t.Errorf("expected the handler's response to pass through, got %v", resp)
			}

			if status.Code(err) != status.Code(c.err) {
				t.Errorf("expected the handler's error to pass through, got %v", err)
			}

			want := rpcAttributes(transportGRPC, c.wantRPC, c.wantCode, c.wantOutcome)

			requests := collectRPCPoints(t, reader, "hippocampus.rpc.requests")
			if requests[want] != 1 {
				t.Errorf("expected one request counted for %s/%s, got %v", c.wantRPC, c.wantOutcome, requests)
			}

			durations := collectRPCPoints(t, reader, "hippocampus.rpc.duration")
			if durations[want] != 1 {
				t.Errorf("expected one duration recorded for %s/%s, got %v", c.wantRPC, c.wantOutcome, durations)
			}
		})
	}
}

// TestInterceptorMetrics_SkipsNonHippocampusRPCs verifies the service-prefix scoping: the health
// service is a probe surface, and counting its steady tick would sit in the denominator of every
// error-rate query.
func TestInterceptorMetrics_SkipsNonHippocampusRPCs(t *testing.T) {
	reader := installRPCInstruments(t)

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	if _, err := InterceptorMetrics(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if points := collectRPCPoints(t, reader, "hippocampus.rpc.requests"); len(points) != 0 {
		t.Errorf("expected the health service to be uncounted, got %v", points)
	}
}

// TestNewRPCInstruments_InstrumentCreationError covers both error branches: when the installed
// MeterProvider fails to create an instrument, construction must log and still return usable (if
// inert) instruments, since they are built at package init and must never stop the process starting.
func TestNewRPCInstruments_InstrumentCreationError(t *testing.T) {
	otel.SetMeterProvider(erroringMeterProvider{})
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	hook := logtest.NewGlobal()
	log.SetLevel(log.InfoLevel)

	requests, duration := newRPCInstruments()

	// Must not panic when used, even though construction failed.
	requests.Add(context.Background(), 1)
	duration.Record(context.Background(), 1)

	if len(hook.AllEntries()) != 2 {
		t.Fatalf("expected an error log entry per failed instrument, got %d", len(hook.AllEntries()))
	}

	for _, entry := range hook.AllEntries() {
		if entry.Level != log.ErrorLevel {
			t.Errorf("expected an Error-level log entry, got %s", entry.Level)
		}
	}
}

// TestOutcomeForStatus pins the HTTP status classification, including the boundaries between the
// three outcomes.
func TestOutcomeForStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusOK:                  outcomeOK,
		http.StatusNoContent:           outcomeOK,
		http.StatusMovedPermanently:    outcomeOK,
		http.StatusBadRequest:          outcomeClientError,
		http.StatusUnauthorized:        outcomeClientError,
		http.StatusNotFound:            outcomeClientError,
		http.StatusInternalServerError: outcomeServerError,
		http.StatusServiceUnavailable:  outcomeServerError,
	}

	for code, want := range cases {
		if got := outcomeForStatus(code); got != want {
			t.Errorf("outcomeForStatus(%d) = %s, want %s", code, got, want)
		}
	}
}

// TestIsRPCPath pins which gateway paths belong in the RPC metrics: the /v1 call surface and nothing
// else, so probe, console, login, and OpenAPI traffic cannot distort the rate or error figures.
func TestIsRPCPath(t *testing.T) {
	cases := map[string]bool{
		"/v1/memories":      true,
		"/v1/memories/abc":  true,
		"/v1/sleep/preview": true,
		"/v1/openapi.json":  false,
		"/healthz":          false,
		"/readyz":           false,
		"/ui":               false,
		"/ui/app.js":        false,
		"/ui/styles.css":    false,
		"/ui/config":        false,
		"/auth/login":       false,
		"/":                 false,
	}

	for path, want := range cases {
		if got := isRPCPath(path); got != want {
			t.Errorf("isRPCPath(%q) = %t, want %t", path, got, want)
		}
	}
}

// gatewayForMetrics builds a grpc-gateway mux carrying the route-capture middleware and a handler
// registered on a real Hippocampus route template, so the pattern the middleware reads is compiled
// by the gateway exactly as it is in main() rather than simulated.
func gatewayForMetrics(t *testing.T, verb string, template string, respond func(http.ResponseWriter)) http.Handler {
	t.Helper()

	mux := runtime.NewServeMux(runtime.WithMiddlewares(gatewayRouteMiddleware()))

	err := mux.HandlePath(verb, template, func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		respond(w)
	})
	if err != nil {
		t.Fatalf("failed to register %s %s: %s", verb, template, err.Error())
	}

	return mux
}

// TestHTTPMetricsMiddleware_NamesTheMatchedRPC verifies the two-part route capture end to end: the
// gateway middleware resolves the matched pattern to the same RPC name the gRPC interceptor reports,
// and the outer middleware records it with the response status - with the id in the path nowhere in
// the attributes.
func TestHTTPMetricsMiddleware_NamesTheMatchedRPC(t *testing.T) {
	reader := installRPCInstruments(t)

	gateway := gatewayForMetrics(t, http.MethodPatch, "/v1/memories/{memory_id}", func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	httpMetricsMiddleware(gateway).ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/memories/a-memory-id", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the request to be served, got %d", rec.Code)
	}

	want := rpcAttributes(transportHTTP, "UpdateMemory", "200", outcomeOK)

	requests := collectRPCPoints(t, reader, "hippocampus.rpc.requests")
	if requests[want] != 1 {
		t.Errorf("expected one UpdateMemory request counted, got %v", requests)
	}

	durations := collectRPCPoints(t, reader, "hippocampus.rpc.duration")
	if durations[want] != 1 {
		t.Errorf("expected one UpdateMemory duration recorded, got %v", durations)
	}
}

// TestHTTPMetricsMiddleware_CountsPreRoutingRejections verifies the reason the capture exists at all:
// a request rejected before routing (as an authentication failure or an oversized body is, both of
// which wrap inside this middleware) is still counted, as rpcUnknown, so it cannot disappear from
// the error rate.
func TestHTTPMetricsMiddleware_CountsPreRoutingRejections(t *testing.T) {
	reader := installRPCInstruments(t)

	rejecting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	rec := httptest.NewRecorder()
	httpMetricsMiddleware(rejecting).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/memories", nil))

	requests := collectRPCPoints(t, reader, "hippocampus.rpc.requests")

	want := rpcAttributes(transportHTTP, rpcUnknown, "401", outcomeClientError)
	if requests[want] != 1 {
		t.Errorf("expected the rejected request counted as unknown, got %v", requests)
	}
}

// TestHTTPMetricsMiddleware_SkipsNonRPCPaths verifies that a support surface is passed through
// unrecorded, and still served.
func TestHTTPMetricsMiddleware_SkipsNonRPCPaths(t *testing.T) {
	reader := installRPCInstruments(t)

	served := false

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true

		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	httpMetricsMiddleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !served {
		t.Error("expected the request to still be served")
	}

	if points := collectRPCPoints(t, reader, "hippocampus.rpc.requests"); len(points) != 0 {
		t.Errorf("expected /healthz to be uncounted, got %v", points)
	}
}

// TestHTTPMetricsMiddleware_NamesEveryPublishedRoute is the drift guard for the gateway's half of
// the rpc attribute. Every route in the published OpenAPI description is driven through the real
// generated gateway - the same registration main() performs - and must come back labelled with the
// RPC name its operationId declares. Without this, an annotation whose path stopped matching the
// policy table would not break anything visibly: the routes would still serve, and every gateway
// call would quietly be attributed to "unknown", which is only noticed when a dashboard is needed.
func TestHTTPMetricsMiddleware_NamesEveryPublishedRoute(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}

	if err := json.Unmarshal(contract.SwaggerJSON, &spec); err != nil {
		t.Fatalf("failed to read the embedded OpenAPI description: %s", err.Error())
	}

	if len(spec.Paths) == 0 {
		t.Fatal("the embedded OpenAPI description declares no paths")
	}

	gwMux := runtime.NewServeMux(runtime.WithMiddlewares(gatewayRouteMiddleware()))

	// The handlers themselves are irrelevant here - every one answers Unimplemented - because the
	// route capture happens in the middleware, before the handler is reached.
	err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, contract.UnimplementedHippocampusServer{})
	if err != nil {
		t.Fatalf("failed to register the gateway: %s", err.Error())
	}

	handler := httpMetricsMiddleware(gwMux)

	for path, operations := range spec.Paths {
		for verb, operation := range operations {
			want := strings.TrimPrefix(operation.OperationID, "Hippocampus_")

			t.Run(want, func(t *testing.T) {
				reader := installRPCInstruments(t)

				// Substitute a literal for each capture segment, so the request exercises the same
				// matching a real call with an id in the path would.
				concrete := captureSegment.ReplaceAllString(path, "an-id")

				request := httptest.NewRequest(strings.ToUpper(verb), concrete, strings.NewReader("{}"))
				request.Header.Set("Content-Type", "application/json")

				handler.ServeHTTP(httptest.NewRecorder(), request)

				for attrs := range collectRPCPoints(t, reader, "hippocampus.rpc.requests") {
					if attrs == rpcAttributes(transportHTTP, want, "501", outcomeServerError) {
						return
					}
				}

				t.Errorf("%s %s was not recorded as rpc=%q", verb, path, want)
			})
		}
	}
}

// captureSegment matches an OpenAPI path capture ("{eventId}"), so a published path template can be
// turned into a concrete request path.
var captureSegment = regexp.MustCompile(`\{[^}]*\}`)

// TestGatewayRouteMiddleware_WithoutCapture verifies the middleware is inert when the request did not
// come through httpMetricsMiddleware, rather than assuming the context value is present.
func TestGatewayRouteMiddleware_WithoutCapture(t *testing.T) {
	served := false

	gateway := gatewayForMetrics(t, http.MethodPost, "/v1/memories", func(w http.ResponseWriter) {
		served = true

		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	gateway.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/memories", nil))

	if !served {
		t.Errorf("expected the request to be served without a capture on the context, got %d", rec.Code)
	}
}

// TestServicePrefixMatchesDescriptor holds hippocampusServicePrefix to the generated service
// descriptor, mirroring the same guard in auth and hippocampus. A stale copy here fails quietly
// rather than open: the RED metrics would stop counting every gRPC call, leaving the error-rate
// alerts firing on nothing.
func TestServicePrefixMatchesDescriptor(t *testing.T) {
	if want := "/" + contract.Hippocampus_ServiceDesc.ServiceName + "/"; hippocampusServicePrefix != want {
		t.Fatalf("hippocampusServicePrefix = %q, want %q", hippocampusServicePrefix, want)
	}
}
