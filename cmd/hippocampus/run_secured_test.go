package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// The secured chain as run() assembles it - authentication, authorisation, the purge gate, the RED
// metrics and the probes, on both transports - is the security boundary, and every piece of it has
// unit tests. What none of them can show is that run() still wires the pieces together, in the
// right order, on both transports: the tests that start run() under auth only ever probed /healthz
// or asserted on a log line, so dropping or reordering the auth interceptor would have left every
// test green (TODO-3 item 152). These drive real requests through the assembled server instead.

const securedTestSecret = "a-test-signing-secret-of-adequate-length"

// startRun runs the server until the test ends, waiting for the gateway to answer first.
func startRun(t *testing.T, healthURL string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	t.Cleanup(func() {
		cancel()

		select {

		case err := <-done:
			if err != nil {
				t.Errorf("run returned an error on clean shutdown: %v", err)
			}

		case <-time.After(20 * time.Second):
			t.Error("run did not return after context cancellation")

		}
	})

	waitForOK(t, http.DefaultClient, healthURL)
}

// captureRPCMetrics points the package's RED instruments at a provider the test can read, for the
// duration of the test. They are package-level instruments bound to the global meter, and the
// global delegate binds only once per process, so installing a provider is not enough on its own.
func captureRPCMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter(interceptorScopeName)

	requests, err := meter.Int64Counter("hippocampus.rpc.requests")
	if err != nil {
		t.Fatalf("creating the request counter: %s", err)
	}

	duration, err := meter.Float64Histogram("hippocampus.rpc.duration")
	if err != nil {
		t.Fatalf("creating the duration histogram: %s", err)
	}

	previousRequests, previousDuration := rpcRequests, rpcDuration
	rpcRequests, rpcDuration = requests, duration

	t.Cleanup(func() {
		rpcRequests, rpcDuration = previousRequests, previousDuration
		_ = provider.Shutdown(context.Background())
	})

	return reader
}

// clientErrors sums hippocampus.rpc.requests{outcome="client_error"} per transport.
func clientErrors(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()

	var collected metricdata.ResourceMetrics

	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collecting metrics: %s", err)
	}

	counts := map[string]int64{}

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "hippocampus.rpc.requests" {
				continue
			}

			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}

			for _, point := range sum.DataPoints {
				outcome, _ := point.Attributes.Value(attribute.Key("outcome"))
				transport, _ := point.Attributes.Value(attribute.Key("transport"))

				if outcome.AsString() == "client_error" {
					counts[transport.AsString()] += point.Value
				}
			}
		}
	}

	return counts
}

func mint(t *testing.T, roles []string, groups []string) string {
	t.Helper()

	token, err := auth.MintToken(auth.MintRequest{
		Secret:   securedTestSecret,
		ClientID: "secured-chain-test",
		Roles:    roles,
		Groups:   groups,
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	return token
}

func dialService(t *testing.T, port int) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling the service: %s", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

func withToken(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func gatewayStatus(t *testing.T, method string, url string, token string, body string) int {
	t.Helper()

	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("building %s %s: %s", method, url, err)
	}

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %s", method, url, err)
	}

	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	return response.StatusCode
}

func TestRun_SecuredChain(t *testing.T) {
	grpcPort, gwBase := baseRunConfig(t)

	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", securedTestSecret)

	reader := captureRPCMetrics(t)

	startRun(t, gwBase+"/healthz")

	client := contract.NewHippocampusClient(dialService(t, grpcPort))
	readerToken := mint(t, []string{"reader"}, nil)
	writerToken := mint(t, []string{"writer"}, nil)

	t.Run("gRPC refuses a call carrying no token", func(t *testing.T) {
		_, err := client.GetMemories(context.Background(), &contract.GetMemoriesRequest{})
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("GetMemories with no token = %v, want Unauthenticated", err)
		}
	})

	t.Run("gRPC refuses a reader Purge", func(t *testing.T) {
		_, err := client.Purge(withToken(readerToken), &contract.EmptyRequest{})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("Purge with a reader token = %v, want PermissionDenied", err)
		}
	})

	t.Run("gRPC serves a writer's write", func(t *testing.T) {
		if _, err := client.StoreMemory(withToken(writerToken), &contract.Memory{Body: "secured", Significance: 5}); err != nil {
			t.Errorf("StoreMemory with a writer token: %s", err)
		}
	})

	t.Run("the gRPC health service needs no token", func(t *testing.T) {
		res, err := healthpb.NewHealthClient(dialService(t, grpcPort)).Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil || res.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Errorf("health check = %v, %v; want SERVING", res.GetStatus(), err)
		}
	})

	t.Run("the gateway refuses a call carrying no token", func(t *testing.T) {
		if code := gatewayStatus(t, http.MethodGet, gwBase+"/v1/memories", "", ""); code != http.StatusUnauthorized {
			t.Errorf("GET /v1/memories with no token = %d, want 401", code)
		}
	})

	t.Run("the gateway refuses a reader purge", func(t *testing.T) {
		if code := gatewayStatus(t, http.MethodPost, gwBase+"/v1/purge", readerToken, "{}"); code != http.StatusForbidden {
			t.Errorf("POST /v1/purge with a reader token = %d, want 403", code)
		}
	})

	t.Run("the gateway serves a writer's write", func(t *testing.T) {
		body := `{"body":"secured over http","significance":5}`

		if code := gatewayStatus(t, http.MethodPost, gwBase+"/v1/memories", writerToken, body); code != http.StatusOK {
			t.Errorf("POST /v1/memories with a writer token = %d, want 200", code)
		}
	})

	t.Run("the gateway's probes need no token", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/readyz"} {
			if code := gatewayStatus(t, http.MethodGet, gwBase+path, "", ""); code != http.StatusOK {
				t.Errorf("GET %s with no token = %d, want 200", path, code)
			}
		}
	})

	// Metrics sit outside authentication on purpose, so a rejected request still appears in the error
	// rate - as a client error, not a server one. Each transport refused two requests above.
	t.Run("rejected requests are counted as client errors", func(t *testing.T) {
		counts := clientErrors(t, reader)

		for _, transport := range []string{"grpc", "http"} {
			if counts[transport] < 2 {
				t.Errorf("%s client_error count = %d, want at least the 2 refused above", transport, counts[transport])
			}
		}
	})
}

// TestRun_RequireGroupScopeRefusesAnUnscopedToken: with auth.requireGroupScope set, run() wraps the
// verifier so a token naming no group - the MOST privileged shape there is - is refused outright,
// on both transports, while a scoped token is served.
func TestRun_RequireGroupScopeRefusesAnUnscopedToken(t *testing.T) {
	grpcPort, gwBase := baseRunConfig(t)

	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", securedTestSecret)
	viper.Set("auth.requireGroupScope", true)

	startRun(t, gwBase+"/healthz")

	client := contract.NewHippocampusClient(dialService(t, grpcPort))
	unscoped := mint(t, []string{"reader"}, nil)
	scoped := mint(t, []string{"reader"}, []string{"a"})

	if _, err := client.GetMemories(withToken(unscoped), &contract.GetMemoriesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("GetMemories with an unscoped token = %v, want Unauthenticated", err)
	}

	if _, err := client.GetMemories(withToken(scoped), &contract.GetMemoriesRequest{}); err != nil {
		t.Errorf("GetMemories with a scoped token: %s", err)
	}

	if code := gatewayStatus(t, http.MethodGet, gwBase+"/v1/memories", unscoped, ""); code != http.StatusUnauthorized {
		t.Errorf("GET /v1/memories with an unscoped token = %d, want 401", code)
	}

	if code := gatewayStatus(t, http.MethodGet, gwBase+"/v1/memories", scoped, ""); code != http.StatusOK {
		t.Errorf("GET /v1/memories with a scoped token = %d, want 200", code)
	}
}
