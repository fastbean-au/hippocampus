package stats

import (
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
)

// countingStore is a db.Store stub that counts CountEvents/CountMemories calls, so the cache's
// de-duplication can be asserted without a real database. Embedding db.Store means only the two
// count methods need implementing.
type countingStore struct {
	db.Store

	events   int
	memories int
}

func (c *countingStore) CountEvents() int {
	c.events++

	return 7
}

func (c *countingStore) CountMemories() (int, int) {
	c.memories++

	return 5, 3
}

// TestCountCache_SharesReadWithinMaxAge verifies two reads inside the max-age window hit the store
// exactly once, and that a read past the window refreshes.
func TestCountCache_SharesReadWithinMaxAge(t *testing.T) {
	store := &countingStore{}
	cache := newCountCache(store, time.Minute)

	first := cache.get()
	if first.events != 7 || first.memoriesWith != 5 || first.memoriesWithout != 3 {
		t.Fatalf("unexpected counts: %+v", first)
	}

	// A second read within the window must reuse the cached value.
	_ = cache.get()

	if store.events != 1 || store.memories != 1 {
		t.Fatalf("expected the store queried once within max-age, got events=%d memories=%d", store.events, store.memories)
	}

	// Expire the cache and read again: the store is queried a second time.
	cache.maxAge = 0

	_ = cache.get()

	if store.events != 2 || store.memories != 2 {
		t.Fatalf("expected a refresh past max-age, got events=%d memories=%d", store.events, store.memories)
	}
}
