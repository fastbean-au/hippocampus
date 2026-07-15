package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
