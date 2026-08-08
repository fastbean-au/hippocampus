package ratelimit

import (
	"testing"
	"time"
)

// TestBucket_ZeroRateNeverPromisesAToken pins the guard the comment calls out: a zero rate would
// otherwise divide into an infinite wait, and a caller would be handed a Retry-After it could never
// usefully honour. Reachable when a bucket is configured with no refill at all.
func TestBucket_ZeroRateNeverPromisesAToken(t *testing.T) {
	now := time.Now()
	b := newBucket(0, 1, now)

	// The bucket starts full, so the first take is served from the burst.
	if ok, _ := b.take(now); !ok {
		t.Fatal("expected the initial burst token to be served")
	}

	ok, wait := b.take(now)
	if ok {
		t.Fatal("expected a zero-rate bucket to refuse once its burst is spent")
	}

	if wait != 0 {
		t.Errorf("expected no retry hint from a bucket that never refills, got %s", wait)
	}
}

// TestClientTable_ExpiryDisabled covers the idle window being off, in which case entries are only
// ever dropped by the size cap. The table is left holding an entry older than any window.
func TestClientTable_ExpiryDisabled(t *testing.T) {
	table := newClientTable(1, 1, 10, 0)

	now := time.Now()
	table.bucketFor("client-a", now)

	table.expire(now.Add(24 * time.Hour))

	if got := table.size(); got != 1 {
		t.Errorf("expected the entry to survive with expiry disabled, got size %d", got)
	}
}

// TestClientTable_ExpiryOnAnEmptyTable covers the walk running out of entries, which is what
// happens when every tracked principal has gone idle at once.
func TestClientTable_ExpiryOnAnEmptyTable(t *testing.T) {
	table := newClientTable(1, 1, 10, time.Minute)

	now := time.Now()
	table.bucketFor("client-a", now)
	table.bucketFor("client-b", now)

	// Both are past the window, so the walk empties the list and must stop rather than run on.
	table.expire(now.Add(time.Hour))

	if got := table.size(); got != 0 {
		t.Errorf("expected every idle entry to be dropped, got size %d", got)
	}

	// And again on the now-empty table.
	table.expire(now.Add(time.Hour))

	if got := table.size(); got != 0 {
		t.Errorf("expected expiry on an empty table to be a no-op, got size %d", got)
	}
}

// TestClientTable_EvictOldestOnAnEmptyTable covers the same guard on the size-cap path.
func TestClientTable_EvictOldestOnAnEmptyTable(t *testing.T) {
	table := newClientTable(1, 1, 10, time.Minute)

	table.evictOldest()

	if got := table.size(); got != 0 {
		t.Errorf("expected an empty table to stay empty, got size %d", got)
	}
}
