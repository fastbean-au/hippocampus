package hippocampus

import (
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
)

// TestCallbackAtRiskMarginIsClampedToZero covers the one bound in the callback configuration whose
// sign inverts its meaning rather than merely disabling it. The margin RAISES the threshold the
// at-risk scan selects on, so a negative one would lower the bar below the threshold actually in
// force and omit exactly the memories the next cycle is about to delete - a warning that goes quiet
// when it matters most. --check-config refuses one at startup, and this is the second line of
// defence for a Server built by anything that skipped that.
func TestCallbackAtRiskMarginIsClampedToZero(t *testing.T) {
	viper.Set("callbacks.atRiskMargin", -0.5)
	t.Cleanup(func() { viper.Set("callbacks.atRiskMargin", nil) })

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	s := &Server{db: database, consolidationEnabled: true}

	// A nil notifier returns before the dispatcher starts, which is all this needs: the bounds are
	// resolved above that return, exactly as the outbox's caps are.
	s.startCallbackDispatch(nil)

	if s.callbackAtRiskMargin != 0 {
		t.Errorf("callbackAtRiskMargin = %v, want it clamped to 0", s.callbackAtRiskMargin)
	}
}

// TestObservedCallerInsertIsIdempotent covers the re-check under the write lock. Two requests from
// the same new client can both find it missing under the read lock, and returning the entry the
// winner inserted rather than a second one is what keeps one client's call count from splitting
// across two boxes on the diagram.
func TestObservedCallerInsertIsIdempotent(t *testing.T) {
	registry := &observedCallers{}
	now := time.Now()

	first := registry.insert("bridge", now)
	second := registry.insert("bridge", now)

	if first != second {
		t.Error("a second insert for the same client id created a second entry")
	}

	if len(registry.callers) != 1 {
		t.Errorf("the registry holds %d entries, want 1", len(registry.callers))
	}
}

// TestEvictOldestOnAnEmptyRegistry covers the guard. It is only reachable if the map empties between
// the fullness check and the eviction, which cannot happen under the write lock - but the guard is
// what stops a delete of the zero-value id, which would be a real entry the moment some client
// presented an empty client_id.
func TestEvictOldestOnAnEmptyRegistry(t *testing.T) {
	registry := &observedCallers{callers: map[string]*observedCaller{}}

	registry.evictOldest()

	if len(registry.callers) != 0 {
		t.Errorf("the registry gained %d entries from an eviction", len(registry.callers))
	}
}
