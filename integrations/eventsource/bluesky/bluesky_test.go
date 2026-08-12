package bluesky

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// fakeStream serves a fixed set of frames then either returns err or blocks until the context is
// cancelled, so consume can be driven end to end with no network.
type fakeStream struct {
	mu     sync.Mutex
	frames [][]byte
	idx    int
	err    error
	closed bool
}

func (f *fakeStream) Next(ctx context.Context) ([]byte, error) {
	f.mu.Lock()

	if f.idx < len(f.frames) {
		frame := f.frames[f.idx]
		f.idx++

		f.mu.Unlock()

		return frame, nil
	}

	err := f.err

	f.mu.Unlock()

	if err != nil {
		return nil, err
	}

	<-ctx.Done()

	return nil, ctx.Err()
}

func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true

	return nil
}

// fakeClient records every RPC the Store issues. It embeds the generated interface so an unexpected
// call panics rather than silently returning a zero value.
type fakeClient struct {
	contract.HippocampusClient

	mu sync.Mutex

	memories []*contract.Memory
	events   []*contract.Event
	recalled []string
	deleted  []string

	storeErr  error
	eventErr  error
	eventErrs []error // consumed one per StoreEvent call, before eventErr
	imported  [][]*contract.Memory
	importErr error
}

func (f *fakeClient) ImportBatch(ctx context.Context, in *contract.ImportBatchRequest, opts ...grpc.CallOption) (*contract.ImportBatchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.importErr != nil {
		return nil, f.importErr
	}

	f.imported = append(f.imported, in.GetMemories())

	return &contract.ImportBatchResponse{MemoriesImported: int32(len(in.GetMemories()))}, nil
}

func (f *fakeClient) importedBatches() [][]*contract.Memory {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([][]*contract.Memory, len(f.imported))
	copy(out, f.imported)

	return out
}

func (f *fakeClient) storedMemories() []*contract.Memory {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]*contract.Memory, len(f.memories))
	copy(out, f.memories)

	return out
}

func (f *fakeClient) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.storeErr != nil {
		return nil, f.storeErr
	}

	f.memories = append(f.memories, in)

	return &contract.StoreMemoryResponse{Id: in.GetId()}, nil
}

func (f *fakeClient) StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.eventErrs) > 0 {
		err := f.eventErrs[0]
		f.eventErrs = f.eventErrs[1:]

		if err != nil {
			return nil, err
		}
	} else if f.eventErr != nil {
		return nil, f.eventErr
	}

	f.events = append(f.events, in)

	return &contract.StoreEventResponse{Id: in.GetId(), MemoryCount: int32(len(in.GetMemories()))}, nil
}

func (f *fakeClient) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recalled = append(f.recalled, in.GetIds()...)

	return &contract.GetMemoriesResponse{}, nil
}

func (f *fakeClient) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.deleted = append(f.deleted, in.GetIds()...)

	return &contract.GeneralResponse{}, nil
}

func (f *fakeClient) snapshot() ([]*contract.Memory, []*contract.Event, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.memories, f.events, f.recalled, f.deleted
}

// testBridge wires a Bridge over a fake client with synchronous recalls, so assertions need no
// timing.
func testBridge(t *testing.T, cfg Config, client *fakeClient) *Bridge {
	t.Helper()

	tr := NewTransformer(bridge.TransformConfig{Significance: 10, Group: "bluesky"}, Options{Events: cfg.Events})
	store := bridge.NewStore(client, tr, 0, "bluesky")

	return New(cfg, store)
}

func postJSON(cursor int64, rkey string, text string, extra string) []byte {
	return []byte(fmt.Sprintf(`{"did":"did:plc:abc","cursor":%d,"kind":"commit","commit":{
	  "operation":"create","collection":"app.bsky.feed.post","rkey":%q,
	  "record":{"text":%q,"createdAt":"2026-08-12T00:00:00Z"%s}}}`, cursor, rkey, text, extra))
}

func likeJSON(cursor int64, subject string) []byte {
	return []byte(fmt.Sprintf(`{"did":"did:plc:liker","cursor":%d,"kind":"commit","commit":{
	  "operation":"create","collection":"app.bsky.feed.like","rkey":"lk",
	  "record":{"createdAt":"2026-08-12T00:00:00Z","subject":{"uri":%q}}}}`, cursor, subject))
}

func TestConsumeAdvancesCursorOnSuccess(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone}, client)

	s := &fakeStream{frames: [][]byte{
		postJSON(10, "a", "first", ""),
		postJSON(20, "b", "second", ""),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	last, err := b.consume(ctx, s, 0)
	if err != nil {
		t.Fatalf("consume: %s", err)
	}

	if last != 20 {
		t.Errorf("cursor = %d, want 20", last)
	}

	memories, _, _, _ := client.snapshot()

	if len(memories) != 2 {
		t.Fatalf("stored %d memories, want 2", len(memories))
	}

	if memories[0].GetId() != "at://did:plc:abc/app.bsky.feed.post/a" {
		t.Errorf("id = %q", memories[0].GetId())
	}
}

// TestConsumeDoesNotAdvanceOnStoreFailure is the cursor-gating contract: a frame that did not store
// must be replayed after the reconnect, so the cursor must still point at the last frame that DID.
func TestConsumeDoesNotAdvanceOnStoreFailure(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, MaxRetries: 0}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The first frame stores and moves the cursor to 10. The stream then reports a clean end so the
	// call returns without waiting out the context.
	first, err := b.consume(ctx, &fakeStream{
		frames: [][]byte{postJSON(10, "a", "stored", "")},
		err:    errors.New("connection reset"),
	}, 0)
	if err == nil {
		t.Fatal("expected the stream error to end the connection")
	}

	if first != 10 {
		t.Fatalf("cursor after the first frame = %d, want 10", first)
	}

	// Now the service starts failing, and frame 20 cannot be stored.
	client.mu.Lock()
	client.storeErr = errors.New("unavailable")
	client.mu.Unlock()

	last, err := b.consume(ctx, &fakeStream{frames: [][]byte{postJSON(20, "b", "fails", "")}}, first)
	if err == nil {
		t.Fatal("expected the store failure to end the connection")
	}

	if last != 10 {
		t.Errorf("cursor = %d, want it held at 10 so frame 20 replays", last)
	}
}

// TestConsumeSkipsUndecodableFrames: a malformed frame can never become valid, and blocking the
// firehose on it is how a consumer gets dropped for being slow.
func TestConsumeSkipsUndecodableFrames(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone}, client)

	s := &fakeStream{frames: [][]byte{
		[]byte(`{"this is not": `),
		postJSON(20, "b", "after the bad frame", ""),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	last, err := b.consume(ctx, s, 0)
	if err != nil {
		t.Fatalf("consume: %s", err)
	}

	if last != 20 {
		t.Errorf("cursor = %d, want the loop to have continued to 20", last)
	}

	memories, _, _, _ := client.snapshot()

	if len(memories) != 1 {
		t.Errorf("stored %d memories, want 1", len(memories))
	}
}

// TestConsumeSkipsNonCommitFrames: identity and account frames arrive unconditionally whatever
// wantedCollections says, and v2 adds more kinds. They are protocol traffic, not messages.
func TestConsumeSkipsNonCommitFrames(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone}, client)

	s := &fakeStream{frames: [][]byte{
		[]byte(`{"did":"did:plc:a","cursor":5,"kind":"identity","identity":{"handle":"a.bsky.social"}}`),
		[]byte(`{"did":"did:plc:a","cursor":6,"kind":"account","account":{"active":true}}`),
		[]byte(`{"did":"did:plc:a","cursor":7,"kind":"aFutureKind"}`),
		postJSON(8, "b", "a real post", ""),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	last, err := b.consume(ctx, s, 0)
	if err != nil {
		t.Fatalf("consume: %s", err)
	}

	if last != 8 {
		t.Errorf("cursor = %d, want 8", last)
	}

	memories, _, _, _ := client.snapshot()

	if len(memories) != 1 {
		t.Errorf("stored %d memories, want 1", len(memories))
	}
}

// TestConsumeReinforcesFromEngagement is the centrepiece: a like names its target by the same at://
// URI the memory is keyed on, so reinforcing it needs no lookup and no state.
func TestConsumeReinforcesFromEngagement(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, Recall: true}, client)

	target := "at://did:plc:abc/app.bsky.feed.post/a"

	s := &fakeStream{frames: [][]byte{
		postJSON(10, "a", "a post", ""),
		likeJSON(20, target),
		likeJSON(30, "at://did:plc:never/app.bsky.feed.post/seen"),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := b.consume(ctx, s, 0); err != nil {
		t.Fatalf("consume: %s", err)
	}

	_, _, recalled, _ := client.snapshot()

	if len(recalled) != 2 {
		t.Fatalf("recalled %v, want both likes", recalled)
	}

	if recalled[0] != target {
		t.Errorf("recalled[0] = %q, want %q", recalled[0], target)
	}

	// A like for a post the bridge never ingested is still submitted: the service no-ops on an id it
	// does not hold, which is exactly what makes the design stateless.
	if recalled[1] != "at://did:plc:never/app.bsky.feed.post/seen" {
		t.Errorf("recalled[1] = %q", recalled[1])
	}
}

// TestConsumeDoesNotUnreinforceOnUnlike: reinforcement is a fact about the past, and there is no
// operation to undo it.
func TestConsumeDoesNotUnreinforceOnUnlike(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, Recall: true}, client)

	raw := []byte(`{"did":"did:plc:l","cursor":9,"kind":"commit","commit":{"operation":"delete",
	  "collection":"app.bsky.feed.like","rkey":"lk"}}`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := b.consume(ctx, &fakeStream{frames: [][]byte{raw}}, 0); err != nil {
		t.Fatalf("consume: %s", err)
	}

	if _, _, recalled, deleted := client.snapshot(); len(recalled) != 0 || len(deleted) != 0 {
		t.Errorf("an unlike produced recalls %v and deletes %v; want neither", recalled, deleted)
	}
}

func TestConsumeHonoursPostDeletes(t *testing.T) {
	cases := []struct {
		name    string
		honour  bool
		deleted int
	}{
		{name: "honoured", honour: true, deleted: 1},
		{name: "ignored", honour: false, deleted: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := &fakeClient{}
			b := testBridge(t, Config{Events: EventsNone, HonourDeletes: c.honour}, client)

			raw := []byte(`{"did":"did:plc:abc","cursor":9,"kind":"commit","commit":{"operation":"delete",
			  "collection":"app.bsky.feed.post","rkey":"a"}}`)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			if _, err := b.consume(ctx, &fakeStream{frames: [][]byte{raw}}, 0); err != nil {
				t.Fatalf("consume: %s", err)
			}

			_, _, _, deleted := client.snapshot()

			if len(deleted) != c.deleted {
				t.Fatalf("deleted %v, want %d", deleted, c.deleted)
			}

			if c.deleted > 0 && deleted[0] != "at://did:plc:abc/app.bsky.feed.post/a" {
				t.Errorf("deleted[0] = %q", deleted[0])
			}
		})
	}
}

func TestConsumeRetriesThenGivesUp(t *testing.T) {
	client := &fakeClient{storeErr: errors.New("unavailable")}

	b := testBridge(t, Config{
		Events:       EventsNone,
		MaxRetries:   2,
		ErrorBackoff: time.Millisecond,
	}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := b.consume(ctx, &fakeStream{frames: [][]byte{postJSON(10, "a", "x", "")}}, 0); err == nil {
		t.Fatal("expected consume to give up and return the error")
	}
}

// TestServeReconnectsFromLastCursor is the most valuable test in the package: it is the only thing
// that proves a dropped connection resumes where it left off rather than at the live tip.
func TestServeReconnectsFromLastCursor(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, ReconnectBackoff: time.Millisecond}, client)

	var (
		mu      sync.Mutex
		cursors []int64
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.dial = func(ctx context.Context, cfg Config, cursor int64) (stream, error) {
		mu.Lock()
		cursors = append(cursors, cursor)
		attempt := len(cursors)
		mu.Unlock()

		if attempt == 1 {
			// Two frames, then the connection drops.
			return &fakeStream{
				frames: [][]byte{postJSON(10, "a", "one", ""), postJSON(20, "b", "two", "")},
				err:    errors.New("connection reset"),
			}, nil
		}

		cancel()

		return &fakeStream{err: errors.New("done")}, nil
	}

	if err := b.serve(ctx); err != nil {
		t.Fatalf("serve: %s", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(cursors) < 2 {
		t.Fatalf("dialled %d times, want at least 2", len(cursors))
	}

	if cursors[0] != 0 {
		t.Errorf("first dial cursor = %d, want 0 (the live tip)", cursors[0])
	}

	if cursors[1] != 20 {
		t.Errorf("reconnect cursor = %d, want 20 (the last frame fully handled)", cursors[1])
	}
}

func TestServeRetriesAFailedDial(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, ReconnectBackoff: time.Millisecond}, client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	attempts := 0

	b.dial = func(ctx context.Context, cfg Config, cursor int64) (stream, error) {
		attempts++

		if attempts < 3 {
			return nil, errors.New("connection refused")
		}

		cancel()

		return nil, errors.New("connection refused")
	}

	if err := b.serve(ctx); err != nil {
		t.Fatalf("serve: %s", err)
	}

	if attempts < 3 {
		t.Errorf("dialled %d times, want the loop to have retried", attempts)
	}
}

func TestServeStopsOnCancel(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, ReconnectBackoff: time.Hour}, client)

	ctx, cancel := context.WithCancel(context.Background())

	b.dial = func(ctx context.Context, cfg Config, cursor int64) (stream, error) {
		cancel()

		return nil, errors.New("refused")
	}

	done := make(chan error, 1)

	go func() { done <- b.serve(ctx) }()

	select {

	case err := <-done:
		if err != nil {
			t.Errorf("serve: %s", err)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return when its context was cancelled")
	}
}

// TestRunFlushesBufferedRecallsOnShutdown covers the goroutine half of the recall buffer: ids
// buffered but not yet flushed must still be submitted when the bridge stops.
func TestRunFlushesBufferedRecallsOnShutdown(t *testing.T) {
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:            EventsNone,
		Recall:            true,
		RecallBatchSize:   1000, // far above what this test buffers, so only shutdown can flush it
		RecallBatchWindow: time.Hour,
		ReconnectBackoff:  time.Millisecond,
	}, client)

	ctx, cancel := context.WithCancel(context.Background())

	b.dial = func(dialCtx context.Context, cfg Config, cursor int64) (stream, error) {
		return &fakeStream{frames: [][]byte{
			likeJSON(10, "at://did:plc:abc/app.bsky.feed.post/a"),
		}}, nil
	}

	done := make(chan error, 1)

	go func() { done <- b.Run(ctx) }()

	// Give the frame time to be consumed and buffered before shutting down.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %s", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if _, _, recalled, _ := client.snapshot(); len(recalled) != 1 {
		t.Errorf("recalled %v on shutdown, want the buffered id to have been flushed", recalled)
	}
}

func TestStorePostThreadMode(t *testing.T) {
	root := "at://did:plc:root/app.bsky.feed.post/rr"

	t.Run("a top-level post opens its thread in one RPC", func(t *testing.T) {
		client := &fakeClient{}
		b := testBridge(t, Config{Events: EventsThread, Group: "bluesky"}, client)

		s := &fakeStream{frames: [][]byte{postJSON(10, "a", "a thread starts", "")}}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if _, err := b.consume(ctx, s, 0); err != nil {
			t.Fatalf("consume: %s", err)
		}

		memories, events, _, _ := client.snapshot()

		if len(events) != 1 {
			t.Fatalf("created %d events, want 1", len(events))
		}

		if events[0].GetId() != "at://did:plc:abc/app.bsky.feed.post/a" {
			t.Errorf("event id = %q", events[0].GetId())
		}

		if events[0].GetName() != "a thread starts" {
			t.Errorf("event name = %q", events[0].GetName())
		}

		if events[0].GetGroup() != "bluesky" {
			t.Errorf("event group = %q, want bluesky", events[0].GetGroup())
		}

		if len(events[0].GetMemories()) != 1 {
			t.Errorf("nested memories = %d, want 1", len(events[0].GetMemories()))
		}

		// The nesting is the point: the memory costs no call of its own.
		if len(memories) != 0 {
			t.Errorf("StoreMemory called %d times, want 0", len(memories))
		}
	})

	t.Run("a reply ensures its root exists first", func(t *testing.T) {
		client := &fakeClient{}
		b := testBridge(t, Config{Events: EventsThread}, client)

		extra := fmt.Sprintf(`,"reply":{"root":{"uri":%q},"parent":{"uri":%q}}`, root, root)
		s := &fakeStream{frames: [][]byte{postJSON(10, "a", "a reply", extra)}}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if _, err := b.consume(ctx, s, 0); err != nil {
			t.Fatalf("consume: %s", err)
		}

		memories, events, _, _ := client.snapshot()

		if len(events) != 1 || events[0].GetId() != root {
			t.Fatalf("events = %v, want the root created", events)
		}

		if events[0].GetName() != "thread rr" {
			t.Errorf("lazy root name = %q, want %q", events[0].GetName(), "thread rr")
		}

		if len(memories) != 1 || memories[0].GetEventId() != root {
			t.Fatalf("memory event id = %v, want %q", memories, root)
		}
	})

	t.Run("a cached root is not re-ensured", func(t *testing.T) {
		client := &fakeClient{}
		b := testBridge(t, Config{Events: EventsThread, RootCacheSize: 16}, client)

		extra := fmt.Sprintf(`,"reply":{"root":{"uri":%q},"parent":{"uri":%q}}`, root, root)

		s := &fakeStream{frames: [][]byte{
			postJSON(10, "a", "first reply", extra),
			postJSON(20, "b", "second reply", extra),
		}}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if _, err := b.consume(ctx, s, 0); err != nil {
			t.Fatalf("consume: %s", err)
		}

		_, events, _, _ := client.snapshot()

		if len(events) != 1 {
			t.Errorf("StoreEvent called %d times, want 1 (the cache should absorb the second)", len(events))
		}
	})

	t.Run("a root consolidated under a cache hit is recreated and retried", func(t *testing.T) {
		client := &fakeClient{}
		b := testBridge(t, Config{Events: EventsThread, RootCacheSize: 16}, client)

		// Pretend the root was seen earlier, then have the store report it gone.
		b.roots.Add(root)
		client.storeErr = status.Error(codes.FailedPrecondition, "event does not exist")

		extra := fmt.Sprintf(`,"reply":{"root":{"uri":%q}}`, root)
		msg := toMessage(mustDecode(t, postJSON(10, "a", "a reply", extra)))

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		// The first Handle fails, the root is recreated, and the retry is attempted. The fake keeps
		// failing, so the error surfaces - what matters is that the root was re-ensured in between.
		_ = b.storePost(ctx, msg, root)

		_, events, _, _ := client.snapshot()

		if len(events) != 1 || events[0].GetId() != root {
			t.Errorf("events = %v, want the root recreated after FailedPrecondition", events)
		}

		if b.roots.Contains(root) != true {
			t.Error("the recreated root should be back in the cache")
		}
	})
}

// TestDispatchIgnoresUnknownCollections: a wildcard subscription, or a lexicon added later, must not
// fail a frame the bridge simply has no mapping for.
func TestDispatchIgnoresUnknownCollections(t *testing.T) {
	client := &fakeClient{}
	b := testBridge(t, Config{Events: EventsNone, Recall: true}, client)

	raw := []byte(`{"did":"did:plc:a","cursor":9,"kind":"commit","commit":{"operation":"create",
	  "collection":"app.bsky.graph.follow","rkey":"f","record":{"subject":"did:plc:b"}}}`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	last, err := b.consume(ctx, &fakeStream{frames: [][]byte{raw}}, 0)
	if err != nil {
		t.Fatalf("consume: %s", err)
	}

	if last != 9 {
		t.Errorf("cursor = %d, want the frame acknowledged", last)
	}

	memories, events, recalled, deleted := client.snapshot()

	if len(memories)+len(events)+len(recalled)+len(deleted) != 0 {
		t.Error("a follow should have produced no writes at all")
	}
}

// TestDeleteFlushesBufferedRecallsFirst pins the ordering claimed in dispatchPost: without it, a
// like buffered moments earlier could reinforce a memory this very call is about to delete.
func TestDeleteFlushesBufferedRecallsFirst(t *testing.T) {
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:            EventsNone,
		Recall:            true,
		HonourDeletes:     true,
		RecallBatchSize:   1000,
		RecallBatchWindow: time.Hour,
	}, client)

	target := "at://did:plc:abc/app.bsky.feed.post/a"

	del := []byte(`{"did":"did:plc:abc","cursor":20,"kind":"commit","commit":{"operation":"delete",
	  "collection":"app.bsky.feed.post","rkey":"a"}}`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// A like lands first and is buffered, then the post is deleted.
	if _, err := b.consume(ctx, &fakeStream{frames: [][]byte{likeJSON(10, target), del}}, 0); err != nil {
		t.Fatalf("consume: %s", err)
	}

	_, _, recalled, deleted := client.snapshot()

	if len(recalled) != 1 {
		t.Errorf("recalled %v, want the buffer flushed before the delete", recalled)
	}

	if len(deleted) != 1 {
		t.Fatalf("deleted %v, want 1", deleted)
	}
}

// TestStorePostPropagatesAnEnsureFailure: if the thread's event cannot be opened, the memory must
// not be written without it and the frame must replay.
func TestStorePostPropagatesAnEnsureFailure(t *testing.T) {
	client := &fakeClient{eventErr: errors.New("unavailable")}
	b := testBridge(t, Config{Events: EventsThread}, client)

	root := "at://did:plc:root/app.bsky.feed.post/rr"
	extra := `,"reply":{"root":{"uri":"` + root + `"}}`
	msg := toMessage(mustDecode(t, postJSON(10, "a", "a reply", extra)))

	if err := b.storePost(context.Background(), msg, root); err == nil {
		t.Fatal("expected the ensure failure to propagate")
	}

	if memories, _, _, _ := client.snapshot(); len(memories) != 0 {
		t.Error("the memory should not have been written without its event")
	}

	if b.roots.Contains(root) {
		t.Error("a root that could not be created must not be cached as existing")
	}
}

func mustDecode(t *testing.T, raw []byte) *event {
	t.Helper()

	return decode(t, string(raw))
}

func TestSubscribeURL(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		cursor int64
		want   []string
		absent []string
	}{
		{
			name:   "collections are repeated, not joined",
			cfg:    Config{URL: DefaultURL, Collections: []string{CollectionPost, CollectionLike}},
			want:   []string{"wantedCollections=app.bsky.feed.post", "wantedCollections=app.bsky.feed.like"},
			absent: []string{"cursor="},
		},
		{
			name: "dids are repeated too",
			cfg:  Config{URL: DefaultURL, DIDs: []string{"did:plc:a", "did:plc:b"}},
			want: []string{"wantedDids=did%3Aplc%3Aa", "wantedDids=did%3Aplc%3Ab"},
		},
		{
			name:   "a cursor is sent only when set",
			cfg:    Config{URL: DefaultURL},
			cursor: 4210,
			want:   []string{"cursor=4210"},
		},
		{
			name:   "an empty URL falls back to the default endpoint",
			cfg:    Config{},
			want:   []string{"jetstream"},
			absent: []string{"cursor="},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := subscribeURL(c.cfg, c.cursor)
			if err != nil {
				t.Fatalf("subscribeURL: %s", err)
			}

			for _, v := range c.want {
				if !strings.Contains(got, v) {
					t.Errorf("URL %q does not contain %q", got, v)
				}
			}

			for _, v := range c.absent {
				if strings.Contains(got, v) {
					t.Errorf("URL %q unexpectedly contains %q", got, v)
				}
			}
		})
	}
}

func TestSubscribeURLRejectsAMalformedEndpoint(t *testing.T) {
	if _, err := subscribeURL(Config{URL: "://not a url"}, 0); err == nil {
		t.Error("expected a malformed URL to be reported")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	b := New(Config{}, nil)

	if b.cfg.URL != DefaultURL {
		t.Errorf("URL = %q, want the default", b.cfg.URL)
	}

	if b.cfg.Events != EventsNone {
		t.Errorf("Events = %q, want %q", b.cfg.Events, EventsNone)
	}

	if b.cfg.ErrorBackoff <= 0 || b.cfg.ReconnectBackoff <= 0 || b.cfg.ReconnectMaxBackoff <= 0 {
		t.Errorf("backoffs were left unset: %+v", b.cfg)
	}

	if b.recall != nil {
		t.Error("recall should be off unless Config.Recall is set")
	}

	if New(Config{Recall: true}, nil).recall == nil {
		t.Error("recall should be on when Config.Recall is set")
	}
}

func TestHelpers(t *testing.T) {
	t.Run("truncate bounds bytes and keeps runes whole", func(t *testing.T) {
		// The service counts bytes, so the budget is bytes - but slicing bytes would split the
		// multi-byte rune and produce a string proto3 cannot carry. "hé" is exactly 3 bytes.
		if got := truncate("héllo", 3); got != "hé" {
			t.Errorf("truncate = %q, want %q", got, "hé")
		}

		// Backing up past a partial rune is what stops the byte budget from cutting one in half.
		if got := truncate("héllo", 2); got != "h" {
			t.Errorf("truncate = %q, want %q", got, "h")
		}

		if got := truncate("short", 50); got != "short" {
			t.Errorf("truncate = %q, want it unchanged", got)
		}

		// The regression the first live firehose run found: 256 emoji is 1024 bytes, and a
		// rune-counting truncation let every one of them through to be refused by the service.
		emoji := strings.Repeat("🎉", 256)

		if got := truncate(emoji, maxEventNameLength); len(got) > maxEventNameLength {
			t.Errorf("truncate produced %d bytes, want at most %d", len(got), maxEventNameLength)
		}

		if got := truncate(emoji, maxEventNameLength); !utf8.ValidString(got) {
			t.Error("truncate produced invalid UTF-8, which a proto3 string cannot carry")
		}
	})

	t.Run("rkeyOf", func(t *testing.T) {
		if got := rkeyOf("at://did:plc:a/app.bsky.feed.post/rr"); got != "rr" {
			t.Errorf("rkeyOf = %q, want rr", got)
		}

		if got := rkeyOf("nopath"); got != "nopath" {
			t.Errorf("rkeyOf = %q, want the whole string", got)
		}

		if got := rkeyOf("trailing/"); got != "trailing/" {
			t.Errorf("rkeyOf = %q", got)
		}
	})

	t.Run("advance never moves the cursor backwards", func(t *testing.T) {
		if got := advance(20, 10); got != 20 {
			t.Errorf("advance(20,10) = %d, want 20", got)
		}

		// A frame carrying no cursor at all must not reset the resume point and replay the window.
		if got := advance(20, 0); got != 20 {
			t.Errorf("advance(20,0) = %d, want 20", got)
		}

		if got := advance(10, 20); got != 20 {
			t.Errorf("advance(10,20) = %d, want 20", got)
		}
	})

	t.Run("growBackoff is capped", func(t *testing.T) {
		if got := growBackoff(time.Second, time.Minute); got != 2*time.Second {
			t.Errorf("growBackoff = %s, want 2s", got)
		}

		if got := growBackoff(time.Minute, time.Minute); got != time.Minute {
			t.Errorf("growBackoff = %s, want the cap", got)
		}
	})

	t.Run("eventStart clamps the future", func(t *testing.T) {
		// The service has no clock-skew guard on events, so a future-dated event would never decay.
		future := time.Now().Add(72 * time.Hour)

		if got := eventStart(future); got > time.Now().UnixNano() {
			t.Errorf("eventStart did not clamp a future time: %d", got)
		}

		if got := eventStart(time.Time{}); got == 0 {
			t.Error("eventStart(zero) should be now, not 0")
		}

		past := time.Now().Add(-time.Hour)

		if got := eventStart(past); got != past.UnixNano() {
			t.Errorf("eventStart altered a past time: %d", got)
		}
	})

	t.Run("sleep returns on cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := sleep(ctx, time.Hour); err == nil {
			t.Error("expected sleep to report the cancellation")
		}

		if err := sleep(context.Background(), time.Millisecond); err != nil {
			t.Errorf("sleep: %s", err)
		}
	})
}

// TestThreadEventFallsBackToTheRkey: an event's name is required non-empty, and a post whose text is
// only whitespace would otherwise produce one the service refuses.
func TestThreadEventFallsBackToTheRkey(t *testing.T) {
	b := New(Config{Events: EventsThread}, nil)

	ev := b.threadEvent(bridge.Message{
		Data: []byte("   "),
		Headers: map[string]string{
			hdrURI:  "at://did:plc:a/app.bsky.feed.post/xyz",
			hdrRkey: "xyz",
		},
	})

	if ev.GetName() != "post xyz" {
		t.Errorf("name = %q, want the rkey fallback", ev.GetName())
	}
}
