package bluesky

import (
	"container/list"
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// recallBuffer coalesces ids from the engagement stream into bulk RecallMemories calls.
//
// Likes arrive at several hundred a second on the public firehose. Unbatched that is several hundred
// RPCs a second, of which the great majority reinforce nothing at all (they name posts older than
// anything the store still holds), and against a single embedded instance those calls would dominate
// the latency this demo exists to show. The service's own recall is already chunked internally, so
// batching costs nothing on its side.
//
// The trade, stated here and in the package doc: a frame counts as handled once its id is BUFFERED,
// so a crash inside the window loses at most one window of reinforcement. That is deliberate - a
// lost like is a memory that decays slightly sooner, not a memory that is wrong - and a size of 0
// turns every id into its own synchronous call for anyone who disagrees.
type recallBuffer struct {
	store  *bridge.Store
	size   int
	window time.Duration

	mu   sync.Mutex
	ids  []string
	seen map[string]struct{}
}

func newRecallBuffer(store *bridge.Store, size int, window time.Duration) *recallBuffer {
	return &recallBuffer{
		store:  store,
		size:   size,
		window: window,
		seen:   make(map[string]struct{}),
	}
}

// Add buffers one id, flushing inline once the batch is full. With a size of 0 it recalls
// immediately, which restores at-least-once reinforcement at the cost of an RPC per engagement.
//
// Ids are deduplicated within a window. The service dedupes too, but doing it here shrinks the
// request and - more usefully - makes the reinforced count a number of DISTINCT memories, which is
// what a viewer of the metric actually wants when a viral post draws forty likes a second.
func (r *recallBuffer) Add(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}

	if r.size <= 0 {
		_, err := r.store.Recall(ctx, []string{id})

		return err
	}

	r.mu.Lock()

	if _, ok := r.seen[id]; ok {
		r.mu.Unlock()

		return nil
	}

	r.seen[id] = struct{}{}
	r.ids = append(r.ids, id)

	if len(r.ids) < r.size {
		r.mu.Unlock()

		return nil
	}

	batch := r.take()

	r.mu.Unlock()

	return r.recall(ctx, batch)
}

// Flush recalls whatever is buffered. Safe to call on an empty buffer, which is the common case for
// the ticker.
func (r *recallBuffer) Flush(ctx context.Context) error {
	r.mu.Lock()
	batch := r.take()
	r.mu.Unlock()

	return r.recall(ctx, batch)
}

// Run flushes on the window until ctx is cancelled, then flushes once more so a clean shutdown does
// not drop a partial batch. It is the goroutine half of the buffer; Add covers the full-batch case
// inline, so a busy stream rarely waits for the ticker at all.
func (r *recallBuffer) Run(ctx context.Context) {
	if r.size <= 0 || r.window <= 0 {
		return
	}

	ticker := time.NewTicker(r.window)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			// The parent context is already cancelled, so the final flush needs its own bounded one
			// or it would be refused before it was sent.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlushTimeout)
			defer cancel()

			if err := r.Flush(flushCtx); err != nil {
				log.WithError(err).Debug("final recall flush failed during shutdown")
			}

			return

		case <-ticker.C:
			if err := r.Flush(ctx); err != nil {
				log.WithError(err).Warn("flushing buffered recalls failed; that reinforcement is lost")
			}
		}
	}
}

// take empties the buffer and returns what was in it. The caller must hold the mutex.
func (r *recallBuffer) take() []string {
	if len(r.ids) == 0 {
		return nil
	}

	batch := r.ids

	r.ids = nil
	r.seen = make(map[string]struct{})

	return batch
}

func (r *recallBuffer) recall(ctx context.Context, batch []string) error {
	if len(batch) == 0 {
		return nil
	}

	hits, err := r.store.Recall(ctx, batch)
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{
		"ids":   len(batch),
		"hits":  hits,
		"ratio": float64(hits) / float64(len(batch)),
	}).
		Debug("reinforced from the engagement stream")

	return nil
}

// shutdownFlushTimeout bounds the last flush after the context is cancelled.
const shutdownFlushTimeout = 5 * time.Second

// rootCache remembers thread roots the bridge has already opened an event for.
//
// It is a PURE OPTIMISATION and nothing reads it for correctness: EnsureEvent is idempotent
// (AlreadyExists is a success), so dropping the whole cache costs one redundant RPC per thread and
// changes no outcome. That is what makes it safe to bound, safe to lose on a restart, and safe to
// evict from under a live thread.
type rootCache struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[string]*list.Element
}

func newRootCache(size int) *rootCache {
	if size <= 0 {
		size = 1
	}

	return &rootCache{
		size:  size,
		order: list.New(),
		items: make(map[string]*list.Element, size),
	}
}

// Contains reports whether the root is known to exist, promoting it on a hit so the popular threads
// are the ones that stay cached.
func (c *rootCache) Contains(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[id]
	if !ok {
		return false
	}

	c.order.MoveToFront(el)

	return true
}

// Add records a root as known, evicting the least recently used entry at capacity.
func (c *rootCache) Add(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[id]; ok {
		c.order.MoveToFront(el)

		return
	}

	c.items[id] = c.order.PushFront(id)

	for c.order.Len() > c.size {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}

		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(string))
	}
}

// Remove forgets a root, for when the store turns out no longer to hold it - the store's own sleep
// cycle can consolidate an event between our caching it and our next write to it.
func (c *rootCache) Remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[id]
	if !ok {
		return
	}

	c.order.Remove(el)
	delete(c.items, id)
}
