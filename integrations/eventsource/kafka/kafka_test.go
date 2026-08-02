package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

type okStorer struct{}

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

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
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

func TestConsume_DoesNotCommitOnStoreFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	fr := &fakeReader{
		cancel: cancel,
		msgs:   []kafkago.Message{{Topic: "t", Value: []byte("a"), Offset: 1}},
	}

	failing := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		return nil, errors.New("boom")
	})

	store := bridge.NewStore(okStorer{}, failing, 0)
	b := New(Config{Topic: "t", ErrorBackoff: time.Millisecond}, store)

	if err := b.consume(ctx, fr); err != nil {
		t.Fatalf("consume = %v, want nil", err)
	}

	if len(fr.committed) != 0 {
		t.Errorf("committed %d messages, want 0 on store failure", len(fr.committed))
	}
}

func TestConsume_ReturnsErrorOnFetchFailure(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
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

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
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

	store := bridge.NewStore(okStorer{}, failing, 0)
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

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
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

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(Config{Topic: "t"}, store)

	if err := b.consume(ctx, fr); err != nil {
		t.Errorf("consume = %v, want nil when ctx already cancelled", err)
	}
}
