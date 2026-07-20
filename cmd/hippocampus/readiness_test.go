package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fastbean-au/hippocampus/db"
)

// pingStore is a db.Store stub that counts Ping calls and returns a configurable error, so the
// readiness tests can assert both the cache behaviour and the ready/not-ready mapping without a
// real database. Embedding db.Store means only Ping needs implementing.
type pingStore struct {
	db.Store
	pings atomic.Int32
	err   error
}

func (p *pingStore) Ping(ctx context.Context) error {
	p.pings.Add(1)

	return p.err
}

// TestReadinessProbe_CachesWithinTTL verifies that repeated checks inside the cache window hit the
// store only once, so probe storms do not become load on the database.
func TestReadinessProbe_CachesWithinTTL(t *testing.T) {
	store := &pingStore{}
	probe := newReadinessProbe(store, time.Second, time.Minute)

	for i := range 5 {
		if err := probe.check(context.Background()); err != nil {
			t.Fatalf("check %d returned an error: %s", i, err)
		}
	}

	if got := store.pings.Load(); got != 1 {
		t.Errorf("expected the store to be pinged once within the cache window, got %d", got)
	}
}

// waitForServingStatus polls the gRPC health service until "hippocampus" reaches want, failing the
// test if it does not within a generous deadline.
func waitForServingStatus(t *testing.T, hs *health.Server, want healthgrpc.HealthCheckResponse_ServingStatus) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := hs.Check(context.Background(), &healthgrpc.HealthCheckRequest{Service: "hippocampus"})
		if err == nil && resp.GetStatus() == want {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for serving status %s", want)
}

// TestUpdateHealth_ReflectsStoreState verifies the gRPC health updater flips "hippocampus" to
// SERVING while the database pings cleanly and NOT_SERVING once it fails, and that closing stop
// ends the goroutine (its done channel closes).
func TestUpdateHealth_ReflectsStoreState(t *testing.T) {
	// A reachable store must drive the status to SERVING.
	healthy := health.NewServer()
	stopHealthy := make(chan struct{})
	doneHealthy := newReadinessProbe(&pingStore{}, time.Second, 10*time.Millisecond).updateHealth(healthy, stopHealthy)

	waitForServingStatus(t, healthy, healthgrpc.HealthCheckResponse_SERVING)

	close(stopHealthy)

	select {

	case <-doneHealthy:

	case <-time.After(2 * time.Second):
		t.Fatal("updateHealth goroutine did not exit after stop was closed")
	}

	// An unreachable store must drive the status to NOT_SERVING.
	sick := health.NewServer()
	stopSick := make(chan struct{})
	t.Cleanup(func() { close(stopSick) })

	probe := newReadinessProbe(&pingStore{err: fmt.Errorf("connection refused")}, time.Second, 10*time.Millisecond)
	probe.updateHealth(sick, stopSick)

	waitForServingStatus(t, sick, healthgrpc.HealthCheckResponse_NOT_SERVING)
}

// TestReadinessProbe_Handler verifies the HTTP mapping: a reachable store yields 200, an
// unreachable one 503.
func TestReadinessProbe_Handler(t *testing.T) {
	ready := newReadinessProbe(&pingStore{}, time.Second, 0)

	rec := httptest.NewRecorder()
	ready.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 when the database is reachable, got %d", rec.Code)
	}

	notReady := newReadinessProbe(&pingStore{err: fmt.Errorf("connection refused")}, time.Second, 0)

	rec = httptest.NewRecorder()
	notReady.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when the database is unreachable, got %d", rec.Code)
	}
}

// TestNewReadinessProbe_DefaultsForNonPositiveInputs verifies that a non-positive timeout or
// cacheTTL falls back to the documented defaults (2s / 3s) rather than leaving the probe with an
// unbounded ping or a disabled cache, covering both the zero and negative cases for each field.
func TestNewReadinessProbe_DefaultsForNonPositiveInputs(t *testing.T) {
	cases := []struct {
		name         string
		timeout      time.Duration
		cacheTTL     time.Duration
		wantTimeout  time.Duration
		wantCacheTTL time.Duration
	}{
		{name: "zero values", timeout: 0, cacheTTL: 0, wantTimeout: 2 * time.Second, wantCacheTTL: 3 * time.Second},
		{name: "negative values", timeout: -1, cacheTTL: -1, wantTimeout: 2 * time.Second, wantCacheTTL: 3 * time.Second},
		{name: "positive values pass through", timeout: 5 * time.Second, cacheTTL: 7 * time.Second, wantTimeout: 5 * time.Second, wantCacheTTL: 7 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newReadinessProbe(&pingStore{}, tc.timeout, tc.cacheTTL)

			if p.timeout != tc.wantTimeout {
				t.Errorf("timeout = %s, want %s", p.timeout, tc.wantTimeout)
			}

			if p.cacheTTL != tc.wantCacheTTL {
				t.Errorf("cacheTTL = %s, want %s", p.cacheTTL, tc.wantCacheTTL)
			}
		})
	}
}
