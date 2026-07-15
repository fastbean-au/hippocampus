package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fastbean-au/hippocampus/db"
)

// readinessProbe answers "can this instance actually serve traffic?" by pinging the database,
// which /healthz and the startup gRPC health status deliberately do not: on the server drivers a
// database can become unreachable (partition, restart, credential rotation) while the process is
// perfectly alive, and without this the orchestrator/load balancer keeps routing to an instance
// whose every RPC fails. The result is cached for cacheTTL so a burst of probes (Kubernetes
// liveness+readiness, an LB health check, the gRPC updater) collapses to at most one ping per
// window, keeping the probe from becoming its own load on the database.
type readinessProbe struct {
	db       db.Store
	timeout  time.Duration
	cacheTTL time.Duration

	mu        sync.Mutex
	lastErr   error
	checkedAt time.Time
}

// newReadinessProbe builds a probe over the store. A non-positive cacheTTL/timeout falls back to
// sane defaults so the caller cannot accidentally disable caching or leave a ping unbounded.
func newReadinessProbe(store db.Store, timeout time.Duration, cacheTTL time.Duration) *readinessProbe {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}

	if cacheTTL <= 0 {
		cacheTTL = 3 * time.Second
	}

	return &readinessProbe{db: store, timeout: timeout, cacheTTL: cacheTTL}
}

// check returns the store's readiness, reusing a cached result within cacheTTL. The ping itself
// runs outside the lock so a slow database never blocks a concurrent probe; the worst case is a
// few overlapping pings when the cache is cold, which is harmless.
func (p *readinessProbe) check(ctx context.Context) error {
	p.mu.Lock()
	if time.Since(p.checkedAt) < p.cacheTTL {
		err := p.lastErr
		p.mu.Unlock()

		return err
	}
	p.mu.Unlock()

	pingCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	err := p.db.Ping(pingCtx)

	p.mu.Lock()
	p.lastErr = err
	p.checkedAt = time.Now()
	p.mu.Unlock()

	return err
}

// handler serves /readyz: 200 when the database is reachable, 503 otherwise. It is separate from
// /healthz (pure process liveness), so a slow database makes the instance not-ready without the
// orchestrator kill-looping the process.
func (p *readinessProbe) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := p.check(r.Context()); err != nil {
			log.Debugf("readiness check failed: %s", err.Error())

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not ready", "error": "database unreachable"})

			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}

// updateHealth drives the gRPC health service off the same probe: it flips the "hippocampus"
// serving status between SERVING and NOT_SERVING every cacheTTL so gRPC clients and load balancers
// see a dead database too, not only HTTP probers. It returns when stop is closed; the caller waits
// on the returned channel during shutdown.
func (p *readinessProbe) updateHealth(hs *health.Server, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(p.cacheTTL)
		defer ticker.Stop()

		for {
			select {

			case <-stop:
				return

			case <-ticker.C:
				status := healthgrpc.HealthCheckResponse_SERVING
				if err := p.check(context.Background()); err != nil {
					status = healthgrpc.HealthCheckResponse_NOT_SERVING
				}

				hs.SetServingStatus("hippocampus", status)
			}
		}
	}()

	return done
}
