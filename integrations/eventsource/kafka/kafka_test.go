package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// okStorer satisfies bridge.NewStore's client seam by embedding the generated interface and
// overriding only StoreMemory, so widening that seam never touches this file. The embedded value is
// nil: a call to any other RPC panics, which is the assertion this adapter wants.
type okStorer struct{ contract.HippocampusClient }

func (okStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	return &contract.StoreMemoryResponse{Id: "x"}, nil
}

// fakeReader serves a fixed set of messages then cancels the context so consume returns cleanly,
// and records every committed message.
type fakeReader struct {
	msgs      []kafkago.Message
	idx       int
	committed []kafkago.Message
	cancel    context.CancelFunc
	closed    bool
	commitErr error
}

func (f *fakeReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	if f.idx < len(f.msgs) {
		m := f.msgs[f.idx]
		f.idx++

		return m, nil
	}

	f.cancel()

	return kafkago.Message{}, context.Canceled
}

func (f *fakeReader) CommitMessages(ctx context.Context, msgs ...kafkago.Message) error {
	if f.commitErr != nil {
		return f.commitErr
	}

	f.committed = append(f.committed, msgs...)

	return nil
}

func (f *fakeReader) Close() error {
	f.closed = true

	return nil
}

func TestToMessage(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0)
	m := kafkago.Message{
		Topic: "orders",
		Value: []byte("payload"),
		Time:  ts,
		Headers: []kafkago.Header{
			{Key: "x-sig", Value: []byte("5")},
			{Key: "tenant", Value: []byte("acme")},
		},
	}

	got := toMessage(m)

	if got.Subject != "orders" {
		t.Errorf("subject = %q, want orders", got.Subject)
	}

	if string(got.Data) != "payload" {
		t.Errorf("data = %q", string(got.Data))
	}

	if !got.Timestamp.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, ts)
	}

	if got.Headers["x-sig"] != "5" || got.Headers["tenant"] != "acme" {
		t.Errorf("headers not mapped: %#v", got.Headers)
	}
}

func TestConsume_CommitsAfterSuccessfulStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fr := &fakeReader{
		cancel: cancel,
		msgs: []kafkago.Message{
			{Topic: "t", Value: []byte("a"), Offset: 1},
			{Topic: "t", Value: []byte("b"), Offset: 2},
		},
	}

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	b := New(Config{Topic: "t"}, store)

	if err := b.consume(ctx, fr); err != nil {
		t.Fatalf("consume = %v, want nil", err)
	}

	if len(fr.committed) != 2 {
		t.Fatalf("committed %d messages, want 2", len(fr.committed))
	}

	if fr.committed[0].Offset != 1 || fr.committed[1].Offset != 2 {
		t.Errorf("committed offsets = %d,%d; want 1,2", fr.committed[0].Offset, fr.committed[1].Offset)
	}
}

// TestConsume_DoesNotCommitOnStoreFailure: a message that keeps failing is retried in place, never
// fetched past and never committed, until the context ends.
func TestConsume_DoesNotCommitOnStoreFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fr := &fakeReader{
		cancel: cancel,
		msgs: []kafkago.Message{
			{Topic: "t", Value: []byte("a"), Offset: 1},
			{Topic: "t", Value: []byte("b"), Offset: 2},
		},
	}

	var attempts []string

	failing := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		attempts = append(attempts, string(msg.Data))

		if len(attempts) == 3 {
			cancel()
		}

		return nil, errors.New("boom")
	})

	store := bridge.NewStore(okStorer{}, failing, 0, "test")
	b := New(Config{Topic: "t", ErrorBackoff: time.Millisecond}, store)

	if err := b.consume(ctx, fr); err != nil {
		t.Fatalf("consume = %v, want nil", err)
	}

	if len(attempts) != 3 || attempts[0] != "a" || attempts[1] != "a" || attempts[2] != "a" {
		t.Errorf("store attempts = %q, want three on a and none on b", attempts)
	}

	if fr.idx != 1 {
		t.Errorf("fetched %d messages, want 1: the failing message was fetched past", fr.idx)
	}

	if len(fr.committed) != 0 {
		t.Errorf("committed %d messages, want 0 on store failure", len(fr.committed))
	}
}

// recordingStorer records the body of every memory it is asked to store, in order.
type recordingStorer struct {
	contract.HippocampusClient

	stored *[]string
}

func (r recordingStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	*r.stored = append(*r.stored, in.GetBody())

	return &contract.StoreMemoryResponse{Id: in.GetId()}, nil
}

// TestConsume_RetriesAFailedMessageBeforeMovingOn pins the at-least-once claim against kafka-go's
// actual semantics, which fakeReader models: a group reader's position advances on every fetch,
// committed or not, so fetching again after a failed store returns the NEXT message - and committing
// that one commits the partition past the failed one too. The failed message has to be retried in
// place, never fetched past (TODO-3 item 138).
func TestConsume_RetriesAFailedMessageBeforeMovingOn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fr := &fakeReader{
		cancel: cancel,
		msgs: []kafkago.Message{
			{Topic: "t", Value: []byte("a"), Offset: 1},
			{Topic: "t", Value: []byte("b"), Offset: 2},
		},
	}

	// "a" fails on its first attempt only, as a store does when the service blips.
	failures := map[string]int{"a": 1}
	inner := bridge.NewDefaultTransformer(bridge.TransformConfig{})

	flaky := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		if failures[string(msg.Data)] > 0 {
			failures[string(msg.Data)]--

			return nil, errors.New("service unavailable")
		}

		return inner.Transform(msg)
	})

	var stored []string

	store := bridge.NewStore(recordingStorer{stored: &stored}, flaky, 0, "test")
	b := New(Config{Topic: "t", ErrorBackoff: time.Millisecond}, store)

	if err := b.consume(ctx, fr); err != nil {
		t.Fatalf("consume = %v, want nil", err)
	}

	if len(stored) != 2 || stored[0] != "a" || stored[1] != "b" {
		t.Fatalf("stored %q, want [a b]: a message whose store failed was skipped", stored)
	}

	if len(fr.committed) != 2 || fr.committed[0].Offset != 1 || fr.committed[1].Offset != 2 {
		t.Errorf("committed %v, want offsets 1 then 2", fr.committed)
	}
}

func TestConsume_ReturnsErrorOnFetchFailure(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	b := New(Config{Topic: "t"}, store)

	if err := b.consume(context.Background(), &errReader{}); err == nil {
		t.Errorf("consume = nil, want an error on a non-context fetch failure")
	}
}

// errReader always fails its fetch with a non-context error.
type errReader struct{}

func (errReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	return kafkago.Message{}, errors.New("broker unreachable")
}

func (errReader) CommitMessages(ctx context.Context, msgs ...kafkago.Message) error {
	return nil
}

func (errReader) Close() error {
	return nil
}

func TestRun_UsesInjectedReaderAndCloses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fr := &fakeReader{
		cancel: cancel,
		msgs:   []kafkago.Message{{Topic: "t", Value: []byte("a"), Offset: 1}},
	}

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	b := New(Config{Topic: "t", GroupID: "g"}, store)
	b.newReader = func(Config) reader {
		return fr
	}

	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !fr.closed {
		t.Errorf("reader should be closed on shutdown")
	}

	if len(fr.committed) != 1 {
		t.Errorf("committed %d, want 1", len(fr.committed))
	}
}

func TestConsume_BackoffCancelStops(t *testing.T) {
	// A store failure with a long backoff: cancelling ctx during the backoff returns cleanly.
	ctx, cancel := context.WithCancel(context.Background())

	fr := &fakeReader{
		msgs:   []kafkago.Message{{Topic: "t", Value: []byte("a"), Offset: 1}},
		cancel: func() {},
	}

	failing := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		return nil, errors.New("boom")
	})

	store := bridge.NewStore(okStorer{}, failing, 0, "test")
	b := New(Config{Topic: "t", ErrorBackoff: time.Hour}, store)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if err := b.consume(ctx, fr); err != nil {
		t.Errorf("consume = %v, want nil after cancel during backoff", err)
	}

	if len(fr.committed) != 0 {
		t.Errorf("committed %d, want 0", len(fr.committed))
	}
}

func TestDefaultReader_BuildsReader(t *testing.T) {
	// kafka-go's NewReader does not dial until the first fetch, so this is safe offline.
	r := defaultReader(Config{Brokers: []string{"localhost:9092"}, Topic: "t", GroupID: "g", MinBytes: 1, MaxBytes: 10})
	if r == nil {
		t.Fatalf("defaultReader returned nil")
	}

	_ = r.Close()
}

func TestSleep_ReturnsOnTimer(t *testing.T) {
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleep = %v, want nil", err)
	}
}

func TestConsume_CommitErrorReturned(t *testing.T) {
	fr := &fakeReader{
		cancel:    func() {},
		msgs:      []kafkago.Message{{Topic: "t", Value: []byte("a"), Offset: 1}},
		commitErr: errors.New("commit failed"),
	}

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	b := New(Config{Topic: "t"}, store)

	if err := b.consume(context.Background(), fr); err == nil {
		t.Errorf("consume should surface a commit error when ctx is live")
	}
}

func TestConsume_CommitErrorSwallowedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fr := &fakeReader{
		cancel:    func() {},
		msgs:      []kafkago.Message{{Topic: "t", Value: []byte("a"), Offset: 1}},
		commitErr: errors.New("commit failed"),
	}

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	b := New(Config{Topic: "t"}, store)

	if err := b.consume(ctx, fr); err != nil {
		t.Errorf("consume = %v, want nil when ctx already cancelled", err)
	}
}

// StoreMemories delegates to StoreMemory so a fake answers a batch write exactly as it answers the
// single one, and every existing expectation holds through the batch path. A gRPC status error
// becomes that memory's own result, as the service would report it; anything else is a transport
// failure and fails the call.
func (o okStorer) StoreMemories(ctx context.Context, in *contract.StoreMemoriesRequest, opts ...grpc.CallOption) (*contract.StoreMemoriesResponse, error) {
	res := &contract.StoreMemoriesResponse{}

	for _, m := range in.GetMemories() {
		resp, err := o.StoreMemory(ctx, m, opts...)

		switch {

		case err != nil:
			st, ok := status.FromError(err)
			if !ok {
				return nil, err
			}

			res.Failed++

			res.Results = append(res.Results, &contract.StoreMemoryResult{Code: int32(st.Code()), Error: st.Message()})

		case resp.GetRejected():
			res.Rejected++

			res.Results = append(res.Results, &contract.StoreMemoryResult{Rejected: true})

		default:
			res.Stored++

			res.Results = append(res.Results, &contract.StoreMemoryResult{Id: resp.GetId()})

		}
	}

	return res, nil
}
