package main

import (
	"math/rand"
	"sync"
)

// registry tracks event and memory ids the generator has stored or seen in query results, so
// that the query, recall, and mutation workers can target real rows. It is bounded: once full,
// the oldest ids fall off the front, mirroring the service's own forgetting. Ids can go stale
// when the service consolidates the rows behind them; callers prune on failed lookups.
type registry struct {
	mu          sync.Mutex
	maxEvents   int
	maxMemories int
	eventIDs    []string
	memoryIDs   []string
}

func newRegistry(maxEvents int, maxMemories int) *registry {
	return &registry{
		maxEvents:   maxEvents,
		maxMemories: maxMemories,
	}
}

func (r *registry) addEventID(id string) {
	if id == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.eventIDs = append(r.eventIDs, id)
	r.eventIDs = trim(r.eventIDs, r.maxEvents)
}

func (r *registry) addMemoryIDs(ids []string) {
	if len(ids) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.memoryIDs = append(r.memoryIDs, ids...)
	r.memoryIDs = trim(r.memoryIDs, r.maxMemories)
}

func (r *registry) randomEventID(rng *rand.Rand) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.eventIDs) == 0 {
		return "", false
	}

	return r.eventIDs[rng.Intn(len(r.eventIDs))], true
}

// twoRandomEventIDs returns two distinct event ids for a merge. It makes a single attempt; if
// the two picks collide the caller simply skips this round.
func (r *registry) twoRandomEventIDs(rng *rand.Rand) (string, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.eventIDs) < 2 {
		return "", "", false
	}

	a := r.eventIDs[rng.Intn(len(r.eventIDs))]
	b := r.eventIDs[rng.Intn(len(r.eventIDs))]

	if a == b {
		return "", "", false
	}

	return a, b, true
}

// randomMemoryIDs returns up to count distinct memory ids; fewer when the picks collide or the
// registry holds less than count ids.
func (r *registry) randomMemoryIDs(rng *rand.Rand, count int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.memoryIDs) == 0 {
		return nil
	}

	seen := make(map[string]bool, count)
	ids := make([]string, 0, count)

	for i := 0; i < count; i++ {
		id := r.memoryIDs[rng.Intn(len(r.memoryIDs))]
		if seen[id] {
			continue
		}

		seen[id] = true
		ids = append(ids, id)
	}

	return ids
}

func (r *registry) removeEventID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept := r.eventIDs[:0]
	for _, v := range r.eventIDs {
		if v == id {
			continue
		}

		kept = append(kept, v)
	}

	r.eventIDs = kept
}

func (r *registry) removeMemoryIDs(ids []string) {
	if len(ids) == 0 {
		return
	}

	gone := make(map[string]bool, len(ids))
	for _, id := range ids {
		gone[id] = true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	kept := r.memoryIDs[:0]
	for _, id := range r.memoryIDs {
		if gone[id] {
			continue
		}

		kept = append(kept, id)
	}

	r.memoryIDs = kept
}

// trim drops the oldest ids once the slice exceeds max, copying to a fresh slice so the
// discarded prefix does not pin the old backing array.
func trim(ids []string, max int) []string {
	if len(ids) <= max {
		return ids
	}

	kept := make([]string, max)
	copy(kept, ids[len(ids)-max:])

	return kept
}
