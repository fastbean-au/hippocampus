package bluesky

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// recordingRecaller captures each RecallMemories batch so the buffering can be asserted without
// timing assumptions.
type recordingRecaller struct {
	contract.HippocampusClient

	mu      sync.Mutex
	batches [][]string
	err     error
}

func (r *recordingRecaller) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return nil, r.err
	}

	r.batches = append(r.batches, in.GetIds())

	return &contract.GetMemoriesResponse{}, nil
}

func (r *recordingRecaller) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([][]string, len(r.batches))
	copy(out, r.batches)

	return out
}

func newTestBuffer(t *testing.T, size int, window time.Duration) (*recallBuffer, *recordingRecaller) {
	t.Helper()

	client := &recordingRecaller{}
	store := bridge.NewStore(client, bridge.TransformerFunc(func(bridge.Message) ([]*contract.Memory, error) {
		return nil, nil
	}), 0, "bluesky")

	return newRecallBuffer(store, size, window), client
}

func TestRecallBufferFlushesWhenFull(t *testing.T) {
	buf, client := newTestBuffer(t, 3, time.Hour)
	ctx := context.Background()

	for _, id := range []string{"a", "b"} {
		if err := buf.Add(ctx, id); err != nil {
			t.Fatalf("Add: %s", err)
		}
	}

	if got := client.snapshot(); len(got) != 0 {
		t.Fatalf("flushed %v before the batch was full", got)
	}

	if err := buf.Add(ctx, "c"); err != nil {
		t.Fatalf("Add: %s", err)
	}

	got := client.snapshot()

	if len(got) != 1 || len(got[0]) != 3 {
		t.Fatalf("batches = %v, want one batch of 3", got)
	}
}

// TestRecallBufferDedupesWithinAWindow: a viral post draws dozens of likes a second, and deduping
// here is what makes the reinforced count a number of DISTINCT memories rather than of engagements.
func TestRecallBufferDedupesWithinAWindow(t *testing.T) {
	buf, client := newTestBuffer(t, 100, time.Hour)
	ctx := context.Background()

	for range 10 {
		if err := buf.Add(ctx, "viral"); err != nil {
			t.Fatalf("Add: %s", err)
		}
	}

	if err := buf.Flush(ctx); err != nil {
		t.Fatalf("Flush: %s", err)
	}

	got := client.snapshot()

	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("batches = %v, want one batch of one distinct id", got)
	}
}

// TestRecallBufferDedupeResetsAfterAFlush: the dedupe is per window, not for all time - a post liked
// again a minute later must be reinforced again, since that is a fresh signal.
func TestRecallBufferDedupeResetsAfterAFlush(t *testing.T) {
	buf, client := newTestBuffer(t, 100, time.Hour)
	ctx := context.Background()

	for range 2 {
		if err := buf.Add(ctx, "post"); err != nil {
			t.Fatalf("Add: %s", err)
		}

		if err := buf.Flush(ctx); err != nil {
			t.Fatalf("Flush: %s", err)
		}
	}

	if got := client.snapshot(); len(got) != 2 {
		t.Errorf("batches = %v, want the id reinforced in each window", got)
	}
}

func TestRecallBufferSynchronousWhenSizeIsZero(t *testing.T) {
	buf, client := newTestBuffer(t, 0, time.Hour)

	if err := buf.Add(context.Background(), "a"); err != nil {
		t.Fatalf("Add: %s", err)
	}

	got := client.snapshot()

	if len(got) != 1 || got[0][0] != "a" {
		t.Errorf("batches = %v, want an immediate single-id recall", got)
	}
}

func TestRecallBufferIgnoresEmptyIds(t *testing.T) {
	buf, client := newTestBuffer(t, 2, time.Hour)

	if err := buf.Add(context.Background(), ""); err != nil {
		t.Fatalf("Add: %s", err)
	}

	if got := client.snapshot(); len(got) != 0 {
		t.Errorf("batches = %v, want none", got)
	}
}

func TestRecallBufferFlushOnAnEmptyBufferIssuesNoRPC(t *testing.T) {
	buf, client := newTestBuffer(t, 10, time.Hour)

	if err := buf.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %s", err)
	}

	if got := client.snapshot(); len(got) != 0 {
		t.Errorf("batches = %v, want none", got)
	}
}

func TestRecallBufferPropagatesAFlushError(t *testing.T) {
	buf, client := newTestBuffer(t, 10, time.Hour)

	client.err = errors.New("unavailable")

	if err := buf.Add(context.Background(), "a"); err != nil {
		t.Fatalf("Add should buffer without error: %s", err)
	}

	if err := buf.Flush(context.Background()); err == nil {
		t.Error("expected the flush failure to propagate")
	}
}

func TestRecallBufferRunFlushesOnTheWindow(t *testing.T) {
	buf, client := newTestBuffer(t, 1000, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go buf.Run(ctx)

	if err := buf.Add(ctx, "a"); err != nil {
		t.Fatalf("Add: %s", err)
	}

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if len(client.snapshot()) > 0 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Error("the ticker never flushed the buffered id")
}

// TestRecallBufferRunIsANoOpWhenBatchingIsOff: with a size of 0 every Add is already synchronous, so
// there is nothing for the goroutine to do and it must not spin on a zero-duration ticker.
func TestRecallBufferRunIsANoOpWhenBatchingIsOff(t *testing.T) {
	buf, _ := newTestBuffer(t, 0, 0)

	done := make(chan struct{})

	go func() {
		buf.Run(context.Background())
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(time.Second):
		t.Error("Run should return immediately when batching is disabled")
	}
}

func TestRootCache(t *testing.T) {
	t.Run("add and contains", func(t *testing.T) {
		c := newRootCache(4)

		if c.Contains("a") {
			t.Error("an empty cache reported a hit")
		}

		c.Add("a")

		if !c.Contains("a") {
			t.Error("expected a hit after Add")
		}

		// Adding twice must not double-count towards the capacity.
		c.Add("a")

		if c.order.Len() != 1 {
			t.Errorf("len = %d, want 1", c.order.Len())
		}
	})

	t.Run("evicts the least recently used at capacity", func(t *testing.T) {
		c := newRootCache(2)

		c.Add("a")
		c.Add("b")

		// Touching "a" makes "b" the eviction candidate.
		if !c.Contains("a") {
			t.Fatal("expected a hit for a")
		}

		c.Add("c")

		if c.Contains("b") {
			t.Error("b should have been evicted")
		}

		if !c.Contains("a") || !c.Contains("c") {
			t.Error("a and c should both still be cached")
		}
	})

	t.Run("remove", func(t *testing.T) {
		c := newRootCache(4)

		c.Add("a")
		c.Remove("a")

		if c.Contains("a") {
			t.Error("expected the entry to be gone")
		}

		// Removing something absent is a no-op, not a panic.
		c.Remove("nothing")
	})

	t.Run("a zero size still holds one entry", func(t *testing.T) {
		c := newRootCache(0)

		c.Add("a")

		if !c.Contains("a") {
			t.Error("a zero-sized cache should degrade to one entry, not to a panic")
		}
	})
}

// TestRootCacheEvictionIsCorrectnessNeutral documents the property that lets the cache be bounded at
// all: it is consulted only to SKIP an idempotent call, so losing an entry costs one redundant RPC
// and changes no outcome.
func TestRootCacheEvictionIsCorrectnessNeutral(t *testing.T) {
	root := "at://did:plc:root/app.bsky.feed.post/rr"

	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsThread, RootCacheSize: 1}, client)

	extra := `,"reply":{"root":{"uri":"` + root + `"}}`
	msg := toMessage(mustDecode(t, postJSON(10, "a", "a reply", extra)))

	ctx := context.Background()

	if err := b.storePost(ctx, msg, root); err != nil {
		t.Fatalf("storePost: %s", err)
	}

	// Evict the root by filling the one-entry cache with something else.
	b.roots.Add("at://did:plc:other/app.bsky.feed.post/zz")

	if b.roots.Contains(root) {
		t.Fatal("the root should have been evicted")
	}

	// The same write still succeeds; it just costs the ensure again.
	if err := b.storePost(ctx, msg, root); err != nil {
		t.Fatalf("storePost after eviction: %s", err)
	}

	memories, events, _, _ := client.snapshot()

	if len(memories) != 2 {
		t.Errorf("stored %d memories, want 2", len(memories))
	}

	if len(events) != 2 {
		t.Errorf("ensured the event %d times, want 2 - the second being the redundant call an "+
			"eviction costs", len(events))
	}
}
