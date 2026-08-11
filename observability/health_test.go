package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// probe drives one endpoint against the handler, returning the status and decoded body.
func probe(t *testing.T, h *HealthServer, path string) (int, map[string]any) {
	t.Helper()

	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var body map[string]any

	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the %s body: %s", path, err)
	}

	return rec.Code, body
}

// TestLivenessIgnoresDependencies is the split that matters: a dependency being down must not fail
// /healthz, or an orchestrator will kill-loop a perfectly healthy process every time the far end
// restarts.
func TestLivenessIgnoresDependencies(t *testing.T) {
	h := NewHealthServer(HealthConfig{
		Component: "hippocampus-ingestor",
		Version:   "v1.2.3",
		Checks: map[string]Check{
			"source": func(context.Context) error { return errors.New("connection refused") },
		},
	})

	code, body := probe(t, h, "/healthz")

	if code != http.StatusOK {
		t.Errorf("expected liveness to stay 200 with a dead dependency, got %d", code)
	}

	if body["status"] != "ok" || body["version"] != "v1.2.3" || body["component"] != "hippocampus-ingestor" {
		t.Errorf("unexpected liveness body: %+v", body)
	}

	// The same server reports not-ready, which is where a dead dependency belongs.
	if code, _ := probe(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("expected readiness to be 503, got %d", code)
	}
}

// TestReadinessNamesTheFailingDependency covers the per-dependency breakdown - a probe that only
// said "not ready" would leave an operator guessing which end of an ingestor is unreachable.
func TestReadinessNamesTheFailingDependency(t *testing.T) {
	h := NewHealthServer(HealthConfig{
		Checks: map[string]Check{
			"source": func(context.Context) error { return nil },
			"target": func(context.Context) error { return errors.New("connection refused") },
		},
	})

	code, body := probe(t, h, "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", code)
	}

	deps, ok := body["dependencies"].(map[string]any)
	if !ok {
		t.Fatalf("expected a dependencies map, got %+v", body["dependencies"])
	}

	if deps["source"] != "ok" || deps["target"] != "unreachable" {
		t.Errorf("expected source ok and target unreachable, got %+v", deps)
	}
}

// TestReadinessWithNoChecks pins that a component with no dependencies reports ready rather than
// erroring or hanging - /readyz then simply means the same as /healthz, which is honest.
func TestReadinessWithNoChecks(t *testing.T) {
	h := NewHealthServer(HealthConfig{})

	code, body := probe(t, h, "/readyz")

	if code != http.StatusOK || body["status"] != "ready" {
		t.Errorf("expected a dependency-free component to be ready, got %d %+v", code, body)
	}
}

// TestReadinessCachesResults is what stops a probe becoming its own load on the thing it probes:
// Kubernetes liveness plus readiness plus a load balancer can all land inside one window.
func TestReadinessCachesResults(t *testing.T) {
	var calls atomic.Int32

	h := NewHealthServer(HealthConfig{
		CacheTTL: time.Hour,
		Checks: map[string]Check{
			"source": func(context.Context) error {
				calls.Add(1)

				return nil
			},
		},
	})

	for range 5 {
		if code, _ := probe(t, h, "/readyz"); code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("expected the check to run once within the TTL, ran %d times", got)
	}
}

// TestReadinessRecoversAfterTheTTL pins the other half: a cached failure must not stick once the
// dependency comes back.
func TestReadinessRecoversAfterTheTTL(t *testing.T) {
	var healthy atomic.Bool

	h := NewHealthServer(HealthConfig{
		CacheTTL: time.Millisecond,
		Checks: map[string]Check{
			"source": func(context.Context) error {
				if healthy.Load() {
					return nil
				}

				return errors.New("connection refused")
			},
		},
	})

	if code, _ := probe(t, h, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while down, got %d", code)
	}

	healthy.Store(true)

	time.Sleep(5 * time.Millisecond)

	if code, _ := probe(t, h, "/readyz"); code != http.StatusOK {
		t.Errorf("expected the probe to recover once the dependency did")
	}
}

// TestCheckTimeoutIsApplied covers the bound on one check: a dependency that never answers must not
// hold the probe open until the HTTP write timeout.
func TestCheckTimeoutIsApplied(t *testing.T) {
	h := NewHealthServer(HealthConfig{
		CheckTimeout: 20 * time.Millisecond,
		Checks: map[string]Check{
			"slow": func(ctx context.Context) error {
				<-ctx.Done()

				return ctx.Err()
			},
		},
	})

	start := time.Now()

	code, _ := probe(t, h, "/readyz")

	if code != http.StatusServiceUnavailable {
		t.Errorf("expected a timed-out check to report not ready, got %d", code)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("expected the check timeout to bound the probe, took %s", elapsed)
	}
}

// TestStartAndShutdown covers the real listener, including that a zero port is a no-op rather than
// an error and that Shutdown is safe in that case.
func TestStartAndShutdown(t *testing.T) {
	disabled := NewHealthServer(HealthConfig{Port: 0})

	if err := disabled.Start(); err != nil {
		t.Fatalf("a zero port must disable the server, not fail: %s", err)
	}

	if err := disabled.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on a disabled server: %s", err)
	}

	port := freePort(t)

	h := NewHealthServer(HealthConfig{Port: port, BindAddress: "127.0.0.1", Component: "test"})

	if err := h.Start(); err != nil {
		t.Fatalf("Start: %s", err)
	}

	defer func() { _ = h.Shutdown(context.Background()) }()

	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("probing the listener: %s", err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from the live listener, got %d", res.StatusCode)
	}
}

// TestStartReportsABindFailure pins that a port already in use fails startup rather than leaving a
// deployment believing it has probes it does not.
func TestStartReportsABindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("holding a port: %s", err)
	}

	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port

	h := NewHealthServer(HealthConfig{Port: port, BindAddress: "127.0.0.1"})

	if err := h.Start(); err == nil {
		t.Error("expected a bind failure on a port already in use")
	}
}

// TestGRPCHealthCheck drives the check against a real gRPC health service, covering the three
// answers that matter: serving, not serving, and a service that is not there at all.
func TestGRPCHealthCheck(t *testing.T) {
	server := grpc.NewServer()
	hs := health.NewServer()
	healthgrpc.RegisterHealthServer(server, hs)
	hs.SetServingStatus("hippocampus", healthgrpc.HealthCheckResponse_SERVING)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %s", err)
	}

	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling: %s", err)
	}

	defer func() { _ = conn.Close() }()

	check := GRPCHealthCheck(conn)

	if err := check(context.Background()); err != nil {
		t.Errorf("expected a SERVING instance to pass: %s", err)
	}

	// The service's own readiness probe flips this when its database goes away, which is exactly
	// what a bridge should refuse to call itself ready for.
	hs.SetServingStatus("hippocampus", healthgrpc.HealthCheckResponse_NOT_SERVING)

	if err := check(context.Background()); err == nil {
		t.Error("expected NOT_SERVING to fail the check")
	}

	// An unknown service name reports NotFound rather than SERVING, so a check aimed at something
	// that is not a Hippocampus instance fails rather than passing silently.
	if _, err := healthgrpc.NewHealthClient(conn).Check(
		context.Background(),
		&healthgrpc.HealthCheckRequest{Service: "not-hippocampus"},
	); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for an unknown service, got %v", err)
	}
}

// TestGRPCHealthCheckTransportFailure covers an unreachable far end.
func TestGRPCHealthCheckTransportFailure(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling: %s", err)
	}

	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := GRPCHealthCheck(conn)(ctx); err == nil {
		t.Error("expected an unreachable instance to fail the check")
	}
}

// freePort returns a port that was free a moment ago, for a test that needs a real listener.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %s", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port

	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the port: %s", err)
	}

	return port
}
