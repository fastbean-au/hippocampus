package hippocampus

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// maxCountCacheEntries bounds the cache. The key is derived from a CALLER-SUPPLIED filter - a
// metadata pair, an id list, a time range are all whatever the request said - so the key space
// belongs to the caller and an unbounded map would be memory a caller controls. It is the same
// reasoning as the topology view's observed-caller registry, and the cap is deliberately small: the
// entries worth having are the handful of filter shapes a console or a paging client repeats, not a
// long tail of one-off queries that will never be asked again.
const maxCountCacheEntries = 256

// countCache memoises a listing's TotalCount for a short window.
//
// TotalCount is a second unbounded pass over the same predicate as the page, and once the listing
// index made the page a walk of `limit` rows, it became effectively the whole cost of a request -
// 32 ms against the page's 0.26 ms at 100,000 memories (TODO 74.3). Indexing the predicate is not
// available as an answer: measured, an index that makes a scoped count 57x faster makes the scoped
// page 181x slower by pulling the planner off the listing index, and the pair comes out worse.
//
// So the total is cached rather than made cheaper, and the consequence is that it can be up to one
// TTL stale while the page beside it is current. That is a deliberate trade in a store whose content
// search is already eventually consistent, and it is bounded in a useful direction: a paging client
// sees a STABLE total across pages, where an exact one recomputed per page can move underneath it
// mid-traversal and make the last page appear to shift.
//
// What is NOT traded is which rows a caller may count. The key is derived from every field of the
// filter, including the caller's group scope, so two callers with different scopes can never share
// an entry - see filterCacheKey.
type countCache struct {
	mu  sync.Mutex
	ttl time.Duration

	entries map[string]countCacheEntry
}

type countCacheEntry struct {
	count   int
	expires time.Time
}

// newCountCache returns a cache with the given TTL. A non-positive TTL disables it entirely, so the
// configuration reaches straight through to "count every time" without a second flag.
func newCountCache(ttl time.Duration) *countCache {
	return &countCache{
		ttl:     ttl,
		entries: make(map[string]countCacheEntry),
	}
}

// enabled reports whether the cache does anything.
func (c *countCache) enabled() bool {
	return c != nil && c.ttl > 0
}

// get returns a cached count when one is present and unexpired.
func (c *countCache) get(key string) (int, bool) {
	if !c.enabled() {
		return 0, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return 0, false
	}

	// An expired entry is dropped rather than left to be evicted later: the same key is about to be
	// recomputed and stored, and leaving it would keep a stale row occupying the cap.
	if time.Now().After(entry.expires) {
		delete(c.entries, key)

		return 0, false
	}

	return entry.count, true
}

// reset drops every entry.
//
// Called only from the two paths that delete in bulk - Purge and clearManifest - and deliberately not
// from the ordinary writes. A general invalidation would clear the cache on every StoreMemory, which
// defeats it exactly where it earns its keep: the deployments large enough for the count to cost
// anything (a broker bridge, the collector exporter, the demo generator) are the ones writing
// continuously. Bounded staleness is the trade the cache exists to make.
//
// These two are different in kind rather than in degree. They move the total by orders of magnitude
// in one call, and a caller who has just been told a purge succeeded and is then shown a five-figure
// total is not reading a slightly stale figure, it is reading a wrong one. The short-page shortcut
// hides that at offset 0, where an emptied store answers its own total - which is precisely why this
// is worth doing rather than leaving to the shortcut: the shape it does NOT cover is a client that
// was mid-traversal, on a page, when the store went away underneath it.
func (c *countCache) reset() {
	if !c.enabled() {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]countCacheEntry)
}

// put stores a count, evicting the entry closest to expiry when the cache is full.
func (c *countCache) put(key string, count int) {
	if !c.enabled() {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= maxCountCacheEntries {
		if _, replacing := c.entries[key]; !replacing {
			c.evictOldest()
		}
	}

	c.entries[key] = countCacheEntry{
		count:   count,
		expires: time.Now().Add(c.ttl),
	}
}

// evictOldest drops the entry nearest expiry. Called with the lock held.
//
// Nearest-expiry rather than least-recently-used, because every entry has the same lifetime from
// the moment it was written: the oldest expiry IS the least recently written, and it is about to
// become useless anyway. That makes eviction a single pass over a map bounded at 256 rather than a
// second data structure to keep ordered.
func (c *countCache) evictOldest() {
	var (
		oldestKey string
		oldest    time.Time
	)

	for key, entry := range c.entries {
		if oldestKey == "" || entry.expires.Before(oldest) {
			oldestKey, oldest = key, entry.expires
		}
	}

	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// filterCacheKey renders a listing filter as a cache key, by reflection over every field it has.
//
// Reflection rather than a hand-written list of fields, and this is the whole reason the cache is
// safe: a filter field added later is included automatically. Hand-listing them would mean a new
// predicate silently absent from the key, which does not fail a build or a test - it returns another
// filter's count, and the first place it would show is a caller seeing a total for rows it cannot
// read. TestFilterCacheKeyCoversEveryField holds the reflection to that promise.
//
// Limit and Offset are the two deliberate exclusions: the count ignores both by definition, so
// including them would give every page of one traversal its own entry and defeat the cache exactly
// where it is most useful.
//
// fmt's %v is deterministic for the types a filter carries, maps included - it sorts map keys - so
// two equal filters always render identically.
func filterCacheKey(prefix string, filter any) string {
	value := reflect.ValueOf(filter)
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}

	if value.Kind() != reflect.Struct {
		return prefix
	}

	var b strings.Builder

	b.WriteString(prefix)

	structType := value.Type()

	for i := range value.NumField() {
		field := structType.Field(i)

		if !field.IsExported() || field.Name == "Limit" || field.Name == "Offset" {
			continue
		}

		fmt.Fprintf(&b, "|%s=%v", field.Name, value.Field(i).Interface())
	}

	return b.String()
}
