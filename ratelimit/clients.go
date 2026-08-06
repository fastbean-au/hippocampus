package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// clientEntry is one tracked principal's bucket, plus when it was last consulted so the table can
// expire it.
type clientEntry struct {
	key    string
	bucket *bucket
	seen   time.Time
}

// clientTable holds one bucket per principal, bounded in both size and age. The bound is the point:
// a map keyed by whatever identifies a caller is itself a memory-exhaustion surface, which would be
// a poor thing to introduce in the course of adding denial-of-service protection.
//
// Expiry is by least-recent use, which is the right order here rather than merely a convenient one.
// A caller being throttled is by definition the most recently seen, so it is never the entry
// evicted to make room; the entries at the cold end are the idle ones, whose buckets have refilled
// to full anyway and so carry no state worth keeping.
//
// There is no sweeper goroutine: every access expires from the cold end first, so the table is
// bounded without a lifecycle to start, stop, or leak.
type clientTable struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	max     int
	idle    time.Duration
	entries map[string]*list.Element
	order   *list.List
}

func newClientTable(rate float64, burst float64, max int, idle time.Duration) *clientTable {
	return &clientTable{
		rate:    rate,
		burst:   burst,
		max:     max,
		idle:    idle,
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

// bucketFor returns the bucket for key, creating it (starting full) if the principal is new or has
// been away longer than the idle window.
func (t *clientTable) bucketFor(key string, now time.Time) *bucket {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expire(now)

	if element, found := t.entries[key]; found {
		entry := element.Value.(*clientEntry)
		entry.seen = now

		t.order.MoveToFront(element)

		return entry.bucket
	}

	entry := &clientEntry{
		key:    key,
		bucket: newBucket(t.rate, t.burst, now),
		seen:   now,
	}

	t.entries[key] = t.order.PushFront(entry)

	// Only after the insert, so a table at its cap makes room for the caller in front of it rather
	// than refusing to track it (an untracked caller would be unlimited, which is the one outcome
	// this must not have).
	for t.order.Len() > t.max {
		t.evictOldest()
	}

	return entry.bucket
}

// expire drops entries idle for longer than the window, from the cold end. It stops at the first
// entry still inside the window, since the list is in use order.
func (t *clientTable) expire(now time.Time) {
	if t.idle <= 0 {
		return
	}

	for {
		oldest := t.order.Back()
		if oldest == nil {
			return
		}

		if now.Sub(oldest.Value.(*clientEntry).seen) < t.idle {
			return
		}

		t.evictOldest()
	}
}

func (t *clientTable) evictOldest() {
	oldest := t.order.Back()
	if oldest == nil {
		return
	}

	delete(t.entries, oldest.Value.(*clientEntry).key)
	t.order.Remove(oldest)
}

// size reports how many principals are currently tracked, which is what the clients gauge
// publishes: an operator watching it climb to the cap knows the table is churning and the cap needs
// raising (or that something is minting identities).
func (t *clientTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.order.Len()
}
