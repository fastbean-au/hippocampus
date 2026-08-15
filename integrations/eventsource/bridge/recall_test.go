package bridge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

func oneMemory(msg Message) ([]*contract.Memory, error) {
	return []*contract.Memory{{Body: "a", Significance: 1}}, nil
}

// TestRecallCountsHitsAndMisses is the reason Recall returns a count rather than just an error. The
// ids an engagement stream offers are mostly for memories the store has already forgotten, and that
// ratio - not the error rate - is what says whether the decay model is doing anything.
func TestRecallCountsHitsAndMisses(t *testing.T) {
	fake := &fakeStorer{
		recallResp: &contract.GetMemoriesResponse{
			Memories: []*contract.Memory{{Id: "a"}, {Id: "b"}},
		},
	}

	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	var hits int

	collected := collectMetrics(t, func() {
		var err error

		hits, err = s.Recall(context.Background(), []string{"a", "b", "c", "d"})
		if err != nil {
			t.Fatalf("Recall: %s", err)
		}
	})

	if hits != 2 {
		t.Errorf("hits = %d, want 2", hits)
	}

	if len(fake.recalled) != 1 || len(fake.recalled[0]) != 4 {
		t.Fatalf("expected one recall of 4 ids, got %v", fake.recalled)
	}

	if got, found := counterValue(t, collected, "hippocampus.bridge.recalls", map[string]string{
		"broker":  "bluesky",
		"outcome": OutcomeReinforced,
	}); !found || got != 2 {
		t.Errorf("reinforced = %d (found %v), want 2", got, found)
	}

	if got, _ := counterValue(t, collected, "hippocampus.bridge.recalls", map[string]string{
		"outcome": OutcomeMissing,
	}); got != 2 {
		t.Errorf("missing = %d, want 2", got)
	}
}

// TestRecallEmptyIdsIssuesNoRPC: the engagement path calls this on every flush, including empty
// ones, so an empty batch must not cost a round trip.
func TestRecallEmptyIdsIssuesNoRPC(t *testing.T) {
	fake := &fakeStorer{}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	hits, err := s.Recall(context.Background(), nil)
	if err != nil {
		t.Fatalf("Recall: %s", err)
	}

	if hits != 0 || len(fake.recalled) != 0 {
		t.Errorf("hits = %d, recalls = %v; want 0 and none", hits, fake.recalled)
	}
}

// TestRecallAbsorbsNotFound covers the group-scoped-token misconfiguration. The service scope-checks
// the ids BEFORE recalling them and reports an id it does not hold as NotFound for the whole batch,
// so a bound token turns every flush into an error. Absorbing it means reinforcement silently stops
// working rather than the bridge stopping consuming - which is the right failure for something whose
// job is to keep reading a firehose.
func TestRecallAbsorbsNotFound(t *testing.T) {
	fake := &fakeStorer{recallErr: status.Error(codes.NotFound, "no such memory: a")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	var hits int

	collected := collectMetrics(t, func() {
		var err error

		hits, err = s.Recall(context.Background(), []string{"a"})
		if err != nil {
			t.Fatalf("Recall should absorb NotFound, got %s", err)
		}
	})

	if hits != 0 {
		t.Errorf("hits = %d, want 0", hits)
	}

	if got, _ := counterValue(t, collected, "hippocampus.bridge.recalls", map[string]string{
		"outcome": OutcomeMissing,
	}); got != 1 {
		t.Errorf("missing = %d, want 1", got)
	}
}

func TestRecallPropagatesOtherErrors(t *testing.T) {
	fake := &fakeStorer{recallErr: status.Error(codes.Unavailable, "down")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	collected := collectMetrics(t, func() {
		if _, err := s.Recall(context.Background(), []string{"a"}); err == nil {
			t.Fatal("expected Unavailable to propagate")
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.bridge.recalls", map[string]string{
		"outcome": OutcomeFailed,
	}); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
}

func TestRecallRecordsBatchSize(t *testing.T) {
	s := NewStore(&fakeStorer{}, TransformerFunc(oneMemory), 0, "bluesky")

	collected := collectMetrics(t, func() {
		if _, err := s.Recall(context.Background(), []string{"a", "b", "c"}); err != nil {
			t.Fatalf("Recall: %s", err)
		}
	})

	found := false

	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "hippocampus.bridge.recall.batch_size" {
				found = true
			}
		}
	}

	if !found {
		t.Error("hippocampus.bridge.recall.batch_size was not recorded")
	}
}

// TestEnsureEventTreatsAlreadyExistsAsSuccess is the rule the whole stateless design rests on: the
// service's event create is a plain INSERT, so "it is already there" and "this frame is a replay
// after a reconnect" arrive as the same error and need the same answer.
func TestEnsureEventTreatsAlreadyExistsAsSuccess(t *testing.T) {
	fake := &fakeStorer{eventErr: status.Error(codes.AlreadyExists, "a record with that id already exists")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	collected := collectMetrics(t, func() {
		if err := s.EnsureEvent(context.Background(), &contract.Event{Id: "at://x", Name: "x"}); err != nil {
			t.Fatalf("EnsureEvent should absorb AlreadyExists, got %s", err)
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.bridge.events", map[string]string{
		"broker":  "bluesky",
		"outcome": OutcomeExists,
	}); got != 1 {
		t.Errorf("exists = %d, want 1", got)
	}
}

func TestEnsureEventOutcomes(t *testing.T) {
	cases := []struct {
		name    string
		fake    *fakeStorer
		want    string
		wantErr bool
	}{
		{name: "created", fake: &fakeStorer{}, want: OutcomeCreated},
		{
			name: "rejected below the minimum significance",
			fake: &fakeStorer{eventResp: &contract.StoreEventResponse{Rejected: true}},
			want: OutcomeRejected,
		},
		{
			name:    "failed",
			fake:    &fakeStorer{eventErr: errors.New("boom")},
			want:    OutcomeFailed,
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(c.fake, TransformerFunc(oneMemory), 0, "bluesky")

			collected := collectMetrics(t, func() {
				err := s.EnsureEvent(context.Background(), &contract.Event{Id: "at://x", Name: "x"})

				if c.wantErr && err == nil {
					t.Error("expected an error")
				}

				if !c.wantErr && err != nil {
					t.Errorf("EnsureEvent: %s", err)
				}
			})

			if got, found := counterValue(t, collected, "hippocampus.bridge.events", map[string]string{
				"outcome": c.want,
			}); !found || got != 1 {
				t.Errorf("outcome %q counted %d times (found %v), want 1", c.want, got, found)
			}
		})
	}
}

// TestHandleEventAttachesMemoriesAsNested is the round-trip saving: an event and the memory that
// opens it are one RPC, not two.
func TestHandleEventAttachesMemoriesAsNested(t *testing.T) {
	fake := &fakeStorer{}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err != nil {
		t.Fatalf("HandleEvent: %s", err)
	}

	if len(fake.events) != 1 {
		t.Fatalf("StoreEvent called %d times, want 1", len(fake.events))
	}

	if len(fake.events[0].GetMemories()) != 1 {
		t.Errorf("nested memories = %d, want 1", len(fake.events[0].GetMemories()))
	}

	// The point of the nesting is that the memory never needs its own call.
	if len(fake.calls) != 0 {
		t.Errorf("StoreMemory called %d times, want 0", len(fake.calls))
	}
}

// TestHandleEventShortfallIsRejected: nested memories are best-effort service-side, so a memory that
// did not land surfaces only as a lower memory_count. There is nothing to redeliver, so it is a
// success - but it must not read as `created`.
func TestHandleEventShortfallIsRejected(t *testing.T) {
	fake := &fakeStorer{eventResp: &contract.StoreEventResponse{Id: "at://x", MemoryCount: 0}}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	collected := collectMetrics(t, func() {
		if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err != nil {
			t.Fatalf("HandleEvent: %s", err)
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.bridge.events", map[string]string{
		"outcome": OutcomeRejected,
	}); got != 1 {
		t.Errorf("rejected = %d, want 1", got)
	}
}

func TestHandleEventTransformFailuresAndFilters(t *testing.T) {
	cases := []struct {
		name        string
		transformer Transformer
		want        string
		wantErr     bool
		wantCalls   int
	}{
		{
			name:        "a transform error never reaches the service",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) { return nil, errors.New("bad") }),
			want:        OutcomeFailed,
			wantErr:     true,
		},
		{
			name:        "a transformer yielding nothing opens no event",
			transformer: TransformerFunc(func(Message) ([]*contract.Memory, error) { return nil, nil }),
			want:        OutcomeFiltered,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeStorer{}
			s := NewStore(fake, c.transformer, 0, "bluesky")

			collected := collectMetrics(t, func() {
				err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"})

				if c.wantErr && err == nil {
					t.Error("expected an error")
				}

				if !c.wantErr && err != nil {
					t.Errorf("HandleEvent: %s", err)
				}
			})

			if len(fake.events) != c.wantCalls {
				t.Errorf("StoreEvent called %d times, want %d", len(fake.events), c.wantCalls)
			}

			if got, _ := counterValue(t, collected, "hippocampus.bridge.events", map[string]string{
				"outcome": c.want,
			}); got != 1 {
				t.Errorf("outcome %q counted %d times, want 1", c.want, got)
			}
		})
	}
}

// TestHandleEventPropagatesStoreFailure: unlike AlreadyExists, a transport failure means the frame
// was not durably handled, so the adapter must be told to replay it.
func TestHandleEventPropagatesStoreFailure(t *testing.T) {
	fake := &fakeStorer{eventErr: status.Error(codes.Unavailable, "down")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	collected := collectMetrics(t, func() {
		if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err == nil {
			t.Fatal("expected Unavailable to propagate")
		}
	})

	if got, _ := counterValue(t, collected, "hippocampus.bridge.events", map[string]string{
		"outcome": OutcomeFailed,
	}); got != 1 {
		t.Errorf("failed = %d, want 1", got)
	}
}

// TestHandleEventStoresMemoriesWhenTheEventAlreadyExists is a regression test for silent data loss
// found by running the Bluesky bridge against the live firehose.
//
// An event is routinely opened by something other than the record that owns it: a reply arriving
// before the post it replies to opens that post's event. When the post itself then turns up, its
// StoreEvent gets AlreadyExists - and absorbing that without storing the nested memories dropped the
// post's own memory on the floor, with nothing logged and nothing to redeliver.
func TestHandleEventStoresMemoriesWhenTheEventAlreadyExists(t *testing.T) {
	fake := &fakeStorer{eventErr: status.Error(codes.AlreadyExists, "exists")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err != nil {
		t.Fatalf("HandleEvent should absorb AlreadyExists, got %s", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("StoreMemory called %d times, want 1 - the memory must not be lost with the event", len(fake.calls))
	}

	if fake.calls[0].GetBody() != "a" {
		t.Errorf("stored body = %q, want the transformed memory", fake.calls[0].GetBody())
	}
}

// TestHandleEventFallbackKeepsMemoriesInTheirEvent is the other half of the bug above, and the one
// that survived it: the memories were stored, but they were stored LOOSE.
//
// A nested memory has no event_id of its own - the service stamps the event's id on it as it creates
// them - so the fallback, which stores them individually, wrote them with no event at all. The
// visible symptom is exactly the case that fallback exists for: the post that opens a thread arrives
// after a reply has already opened its event, and the console then shows an event whose own opening
// post is not among its memories.
func TestHandleEventFallbackKeepsMemoriesInTheirEvent(t *testing.T) {
	fake := &fakeStorer{eventErr: status.Error(codes.AlreadyExists, "exists")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err != nil {
		t.Fatalf("HandleEvent should absorb AlreadyExists, got %s", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("StoreMemory called %d times, want 1", len(fake.calls))
	}

	if got := fake.calls[0].GetEventId(); got != "at://x" {
		t.Errorf("stored memory's event_id = %q, want the event it was nested under", got)
	}
}

// TestHandleEventFallbackLeavesAnExplicitEventAlone: the stamp is a default, not an override. A
// transformer that put a memory in a DIFFERENT event than the one being opened meant it.
func TestHandleEventFallbackLeavesAnExplicitEventAlone(t *testing.T) {
	fake := &fakeStorer{eventErr: status.Error(codes.AlreadyExists, "exists")}

	elsewhere := func(msg Message) ([]*contract.Memory, error) {
		return []*contract.Memory{{Body: "a", Significance: 1, EventId: "at://other"}}, nil
	}

	s := NewStore(fake, TransformerFunc(elsewhere), 0, "bluesky")

	if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err != nil {
		t.Fatalf("HandleEvent should absorb AlreadyExists, got %s", err)
	}

	if got := fake.calls[0].GetEventId(); got != "at://other" {
		t.Errorf("stored memory's event_id = %q, want the one the transformer set", got)
	}
}

// TestHandleEventAbsorbsADuplicateMemoryToo: on the fallback path a replayed frame re-writes a
// memory the store already holds, which is what at-least-once delivery is supposed to look like.
func TestHandleEventAbsorbsADuplicateMemoryToo(t *testing.T) {
	fake := &fakeStorer{
		eventErr: status.Error(codes.AlreadyExists, "exists"),
		err:      status.Error(codes.AlreadyExists, "a record with that id already exists"),
	}

	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err != nil {
		t.Fatalf("a duplicate memory on the fallback path should be absorbed, got %s", err)
	}
}

// TestHandleEventFallbackPropagatesARealFailure: only AlreadyExists is absorbed; a transport failure
// still has to make the frame replay.
func TestHandleEventFallbackPropagatesARealFailure(t *testing.T) {
	fake := &fakeStorer{
		eventErr: status.Error(codes.AlreadyExists, "exists"),
		err:      status.Error(codes.Unavailable, "down"),
	}

	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.HandleEvent(context.Background(), Message{Subject: "s"}, &contract.Event{Id: "at://x", Name: "x"}); err == nil {
		t.Fatal("expected a transport failure on the fallback path to propagate")
	}
}

func TestForget(t *testing.T) {
	cases := []struct {
		name      string
		fake      *fakeStorer
		ids       []string
		wantErr   bool
		wantCalls int
	}{
		{name: "deletes the named ids", fake: &fakeStorer{}, ids: []string{"a", "b"}, wantCalls: 1},
		{name: "an empty batch issues no RPC", fake: &fakeStorer{}, ids: nil},
		{
			name:      "an id the store does not hold is absorbed",
			fake:      &fakeStorer{deleteErr: status.Error(codes.NotFound, "no such memory")},
			ids:       []string{"a"},
			wantCalls: 1,
		},
		{
			name:      "a transport failure propagates",
			fake:      &fakeStorer{deleteErr: status.Error(codes.Unavailable, "down")},
			ids:       []string{"a"},
			wantErr:   true,
			wantCalls: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewStore(c.fake, TransformerFunc(oneMemory), 0, "bluesky")

			err := s.Forget(context.Background(), c.ids)

			if c.wantErr && err == nil {
				t.Error("expected an error")
			}

			if !c.wantErr && err != nil {
				t.Errorf("Forget: %s", err)
			}

			if len(c.fake.deleted) != c.wantCalls {
				t.Errorf("DeleteMemories called %d times, want %d", len(c.fake.deleted), c.wantCalls)
			}
		})
	}
}

// TestCallTimeoutApplies pins that the new methods honour the Store's per-call bound rather than
// running unbounded on the caller's context, which is what store() already does for Handle.
func TestCallTimeoutApplies(t *testing.T) {
	deadlines := 0

	fake := &deadlineStorer{onCall: func(ctx context.Context) {
		if _, ok := ctx.Deadline(); ok {
			deadlines++
		}
	}}

	s := NewStore(fake, TransformerFunc(oneMemory), time.Second, "bluesky")
	ctx := context.Background()

	if _, err := s.Recall(ctx, []string{"a"}); err != nil {
		t.Fatalf("Recall: %s", err)
	}

	if err := s.Forget(ctx, []string{"a"}); err != nil {
		t.Fatalf("Forget: %s", err)
	}

	if err := s.EnsureEvent(ctx, &contract.Event{Id: "x", Name: "x"}); err != nil {
		t.Fatalf("EnsureEvent: %s", err)
	}

	if deadlines != 3 {
		t.Errorf("%d of 3 calls carried a deadline", deadlines)
	}
}

// deadlineStorer reports the context each RPC was given, so the timeout wiring can be asserted
// without waiting for anything to expire.
type deadlineStorer struct {
	contract.HippocampusClient

	onCall func(ctx context.Context)
}

func (d *deadlineStorer) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	d.onCall(ctx)

	return &contract.GetMemoriesResponse{}, nil
}

func (d *deadlineStorer) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	d.onCall(ctx)

	return &contract.GeneralResponse{}, nil
}

func (d *deadlineStorer) StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	d.onCall(ctx)

	return &contract.StoreEventResponse{Id: in.GetId()}, nil
}

func TestStoreMemoriesTreatsAnExistingMemoryAsSuccess(t *testing.T) {
	// The polled-source contract: re-reading a ranked feed hands back the same posts every time, and
	// only the new ones should be written. Crucially the existing memory is left ALONE, so
	// reinforcement it has accumulated since is not rolled back.
	fake := &fakeStorer{err: status.Error(codes.AlreadyExists, "a record with that id already exists")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	err := s.StoreMemories(context.Background(), []*contract.Memory{
		{Id: "at://a", Body: "one"},
		{Id: "at://b", Body: "two"},
	})
	if err != nil {
		t.Fatalf("StoreMemories: %s", err)
	}

	if len(fake.calls) != 2 {
		t.Errorf("StoreMemory called %d times, want both attempted", len(fake.calls))
	}
}

// TestStoreMemoriesSkipsAMemoryWhoseEventIsMissing is a regression test for a PERMANENT STALL, not
// for one dropped memory.
//
// A polled source hands back the same page every read. When one memory in that page named an event
// the store did not hold, the service's FailedPrecondition aborted the batch - so every memory after
// it went unwritten, the next poll returned the same page, and the write stopped at the same memory
// again. The store simply stopped growing, while the bridge logged a retryable-looking error each
// tick. The live symptom was a feed poll storing 46 of 99 posts, forever.
func TestStoreMemoriesSkipsAMemoryWhoseEventIsMissing(t *testing.T) {
	fake := &fakeStorer{
		errFor: func(in *contract.Memory) error {
			if in.GetEventId() == "" {
				return nil
			}

			return status.Error(codes.FailedPrecondition, "event 'at://root' does not exist")
		},
	}

	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	err := s.StoreMemories(context.Background(), []*contract.Memory{
		{Id: "at://a", Body: "one"},
		{Id: "at://b", Body: "reply", EventId: "at://root"},
		{Id: "at://c", Body: "three"},
	})
	if err != nil {
		t.Fatalf("StoreMemories should skip an orphaned memory, got %s", err)
	}

	if len(fake.calls) != 3 {
		t.Fatalf("StoreMemory called %d times, want all three attempted", len(fake.calls))
	}

	if fake.calls[2].GetId() != "at://c" {
		t.Errorf("last memory attempted was %q, want the batch to have continued to at://c", fake.calls[2].GetId())
	}
}

// TestStoreMemoriesPropagatesFailedPreconditionWithoutAnEvent: the skip is narrowed to memories that
// actually name an event, so the same code from any other cause still fails the batch and is still
// reported rather than being silently swallowed.
func TestStoreMemoriesPropagatesFailedPreconditionWithoutAnEvent(t *testing.T) {
	fake := &fakeStorer{err: status.Error(codes.FailedPrecondition, "something else entirely")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.StoreMemories(context.Background(), []*contract.Memory{{Id: "a", Body: "x"}}); err == nil {
		t.Error("expected a FailedPrecondition unrelated to an event to propagate")
	}
}

func TestStoreMemoriesPropagatesRealFailures(t *testing.T) {
	fake := &fakeStorer{err: status.Error(codes.Unavailable, "down")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.StoreMemories(context.Background(), []*contract.Memory{{Id: "a"}}); err == nil {
		t.Error("expected a transport failure to propagate")
	}
}

func TestStoreMemoriesSkipsNils(t *testing.T) {
	fake := &fakeStorer{}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if err := s.StoreMemories(context.Background(), []*contract.Memory{nil, {Id: "a", Body: "x"}}); err != nil {
		t.Fatalf("StoreMemories: %s", err)
	}

	if len(fake.calls) != 1 {
		t.Errorf("StoreMemory called %d times, want 1", len(fake.calls))
	}
}

// TestImportMemoriesChunks pins that a large seed cannot build one oversized message.
func TestImportMemoriesChunks(t *testing.T) {
	fake := &fakeStorer{}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	mems := make([]*contract.Memory, 0, importChunkSize*2+5)
	for i := range importChunkSize*2 + 5 {
		mems = append(mems, &contract.Memory{Id: fmt.Sprintf("at://%d", i), Body: "x"})
	}

	var imported int

	collected := collectMetrics(t, func() {
		var err error

		imported, err = s.ImportMemories(context.Background(), mems)
		if err != nil {
			t.Fatalf("ImportMemories: %s", err)
		}
	})

	if imported != len(mems) {
		t.Errorf("imported %d, want %d", imported, len(mems))
	}

	if len(fake.imported) != 3 {
		t.Errorf("ImportBatch called %d times, want 3 chunks", len(fake.imported))
	}

	if got, _ := counterValue(t, collected, "hippocampus.bridge.memories", map[string]string{
		"outcome": OutcomeStored,
	}); got != int64(len(mems)) {
		t.Errorf("counted %d imported memories, want %d", got, len(mems))
	}
}

// TestImportMemoriesCarriesRecallHistory is why ImportBatch is on the seam at all: StoreMemory
// deliberately zeroes recall state, so it is the only way to say "this was returned to N times".
func TestImportMemoriesCarriesRecallHistory(t *testing.T) {
	fake := &fakeStorer{}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if _, err := s.ImportMemories(context.Background(), []*contract.Memory{
		{Id: "at://a", Body: "seeded", RecallCount: 7},
	}); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	if len(fake.imported) != 1 || fake.imported[0][0].GetRecallCount() != 7 {
		t.Errorf("imported %v, want the recall count preserved", fake.imported)
	}
}

func TestImportMemoriesPropagatesFailures(t *testing.T) {
	fake := &fakeStorer{importErr: status.Error(codes.Unavailable, "down")}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if _, err := s.ImportMemories(context.Background(), []*contract.Memory{{Id: "a"}}); err == nil {
		t.Error("expected the failure to propagate")
	}
}

func TestImportMemoriesEmptyIsANoOp(t *testing.T) {
	fake := &fakeStorer{}
	s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

	if _, err := s.ImportMemories(context.Background(), nil); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	if len(fake.imported) != 0 {
		t.Error("an empty import should issue no RPC")
	}
}

// TestTransformerAccessorReturnsTheSameInstance is what makes a second source provably consistent
// with the broker one rather than merely configured alike.
func TestTransformerAccessorReturnsTheSameInstance(t *testing.T) {
	tr := TransformerFunc(oneMemory)
	s := NewStore(&fakeStorer{}, tr, 0, "bluesky")

	got := s.Transformer()
	if got == nil {
		t.Fatal("Transformer() returned nil")
	}

	mems, err := got.Transform(Message{Subject: "s"})
	if err != nil || len(mems) != 1 {
		t.Errorf("the returned transformer did not behave like the one supplied: %v, %v", mems, err)
	}
}

func TestLink(t *testing.T) {
	links := []*contract.Link{{Id: "at://b", Significance: 50}}

	t.Run("relates the named memories", func(t *testing.T) {
		fake := &fakeStorer{}
		s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

		if err := s.Link(context.Background(), "at://a", links); err != nil {
			t.Fatalf("Link: %s", err)
		}

		if len(fake.linked) != 1 || fake.linked[0].id != "at://a" {
			t.Fatalf("linked %v, want at://a", fake.linked)
		}

		if len(fake.linked[0].links) != 1 || fake.linked[0].links[0].GetId() != "at://b" {
			t.Errorf("links = %v", fake.linked[0].links)
		}
	})

	t.Run("an empty id or no links issues no RPC", func(t *testing.T) {
		fake := &fakeStorer{}
		s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

		if err := s.Link(context.Background(), "", links); err != nil {
			t.Fatalf("Link: %s", err)
		}

		if err := s.Link(context.Background(), "at://a", nil); err != nil {
			t.Fatalf("Link: %s", err)
		}

		if len(fake.linked) != 0 {
			t.Errorf("issued %d link calls, want none", len(fake.linked))
		}
	})

	// A target the store has already forgotten is the expected case in a store whose job is
	// forgetting - it must leave the memory stored-but-unrelated, not fail.
	t.Run("a forgotten target is absorbed", func(t *testing.T) {
		fake := &fakeStorer{linkErr: status.Error(codes.NotFound, "no such memory")}
		s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

		if err := s.Link(context.Background(), "at://a", links); err != nil {
			t.Errorf("Link should absorb NotFound, got %s", err)
		}
	})

	t.Run("a transport failure propagates", func(t *testing.T) {
		fake := &fakeStorer{linkErr: status.Error(codes.Unavailable, "down")}
		s := NewStore(fake, TransformerFunc(oneMemory), 0, "bluesky")

		if err := s.Link(context.Background(), "at://a", links); err == nil {
			t.Error("expected a transport failure to propagate")
		}
	})
}
