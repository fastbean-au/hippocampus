package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeStorer records StoreMemory calls and returns a scripted response/error.
//
// It satisfies the client seam by EMBEDDING the generated interface and overriding only the methods
// a test exercises, so widening that seam never touches this file again. The embedded value is nil,
// so a call to any other method panics - which is the assertion we want: a Store method reaching an
// RPC the test did not script should fail loudly rather than return a zero value.
type fakeStorer struct {
	contract.HippocampusClient

	calls    []*contract.Memory
	rejected bool
	err      error

	// errFor scripts a PER-MEMORY failure, for the batch paths where what matters is which memories
	// were attempted after one of them failed.
	errFor func(*contract.Memory) error

	events     []*contract.Event
	eventResp  *contract.StoreEventResponse
	eventErr   error
	recalled   [][]string
	recallResp *contract.GetMemoriesResponse
	recallErr  error
	deleted    [][]string
	deleteErr  error
	imported   [][]*contract.Memory
	importErr  error
	linked     []storerLink
	linkErr    error
}

type storerLink struct {
	id    string
	links []*contract.Link
}

func (f *fakeStorer) LinkMemories(ctx context.Context, in *contract.LinkMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	if f.linkErr != nil {
		return nil, f.linkErr
	}

	f.linked = append(f.linked, storerLink{id: in.GetId(), links: in.GetLinks()})

	return &contract.GeneralResponse{}, nil
}

func (f *fakeStorer) ImportBatch(ctx context.Context, in *contract.ImportBatchRequest, opts ...grpc.CallOption) (*contract.ImportBatchResponse, error) {
	f.imported = append(f.imported, in.GetMemories())
	if f.importErr != nil {
		return nil, f.importErr
	}

	return &contract.ImportBatchResponse{MemoriesImported: int32(len(in.GetMemories()))}, nil
}

func (f *fakeStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.calls = append(f.calls, in)

	if f.errFor != nil {
		if err := f.errFor(in); err != nil {
			return nil, err
		}
	}

	if f.err != nil {
		return nil, f.err
	}

	return &contract.StoreMemoryResponse{Id: "id-" + in.GetBody(), Rejected: f.rejected}, nil
}

func (f *fakeStorer) StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	f.events = append(f.events, in)
	if f.eventErr != nil {
		return nil, f.eventErr
	}

	if f.eventResp != nil {
		return f.eventResp, nil
	}

	return &contract.StoreEventResponse{Id: in.GetId(), MemoryCount: int32(len(in.GetMemories()))}, nil
}

func (f *fakeStorer) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.recalled = append(f.recalled, in.GetIds())
	if f.recallErr != nil {
		return nil, f.recallErr
	}

	if f.recallResp != nil {
		return f.recallResp, nil
	}

	return &contract.GetMemoriesResponse{}, nil
}

func (f *fakeStorer) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.deleted = append(f.deleted, in.GetIds())
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}

	return &contract.GeneralResponse{}, nil
}

func TestStore_HandleStoresEachMemory(t *testing.T) {
	fake := &fakeStorer{}
	tr := TransformerFunc(func(msg Message) ([]*contract.Memory, error) {

		return []*contract.Memory{
			{Body: "a", Significance: 1},
			{Body: "b", Significance: 2},
		}, nil
	})

	s := NewStore(fake, tr, 0, "test")

	if err := s.Handle(context.Background(), Message{Subject: "s"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fake.calls) != 2 {
		t.Fatalf("StoreMemory called %d times, want 2", len(fake.calls))
	}

	if fake.calls[0].GetBody() != "a" || fake.calls[1].GetBody() != "b" {
		t.Errorf("bodies = %q,%q; want a,b", fake.calls[0].GetBody(), fake.calls[1].GetBody())
	}
}

func TestStore_HandleTransformErrorPropagates(t *testing.T) {
	fake := &fakeStorer{}
	want := errors.New("boom")
	tr := TransformerFunc(func(msg Message) ([]*contract.Memory, error) {
		return nil, want
	})

	s := NewStore(fake, tr, 0, "test")

	err := s.Handle(context.Background(), Message{Subject: "s"})
	if !errors.Is(err, want) {
		t.Fatalf("Handle error = %v, want wrapping %v", err, want)
	}

	if len(fake.calls) != 0 {
		t.Errorf("StoreMemory should not be called on transform error, got %d calls", len(fake.calls))
	}
}

func TestStore_HandleStoreErrorPropagates(t *testing.T) {
	want := errors.New("transport down")
	fake := &fakeStorer{err: want}
	tr := NewDefaultTransformer(TransformConfig{})

	s := NewStore(fake, tr, 0, "test")

	err := s.Handle(context.Background(), Message{Subject: "s", Data: []byte("x")})
	if !errors.Is(err, want) {
		t.Fatalf("Handle error = %v, want wrapping %v", err, want)
	}
}

// TestStore_HandleAlreadyExistsIsNotError is a regression test for a bridge that wedged permanently
// on the live Bluesky demo, and it is the whole point of at-least-once delivery.
//
// A memory the store already holds is the expected outcome of a redelivery: every adapter here acks
// after the write, so a redelivery after a reconnect re-presents a message that was already stored.
// Returning that as an error told the adapter the frame had NOT been handled - so it retried it,
// dropped the connection, resumed from the same cursor, and was handed the same already-stored
// record again. The demo sat in that loop for hours, storing nothing new and reinforcing nothing,
// which reads from the console exactly like an instance that has stopped consolidating.
//
// Note the shape: this can only ever be a permanent failure, because a duplicate id never becomes a
// non-duplicate. Every other path in this package already treats it as a success (storeEach,
// StoreMemories, EnsureEvent); Handle was the one that did not.
func TestStore_HandleAlreadyExistsIsNotError(t *testing.T) {
	fake := &fakeStorer{err: status.Error(codes.AlreadyExists, "a record with that id already exists")}
	tr := NewDefaultTransformer(TransformConfig{})

	s := NewStore(fake, tr, 0, "test")

	collected := collectMetrics(t, func() {
		if err := s.Handle(context.Background(), Message{Subject: "s", Data: []byte("x")}); err != nil {
			t.Fatalf("Handle should absorb AlreadyExists, got %v", err)
		}
	})

	if len(fake.calls) != 1 {
		t.Errorf("StoreMemory calls = %d, want 1", len(fake.calls))
	}

	// Reported, not silent: a bridge whose whole stream is duplicates is doing nothing, and that has
	// to be visible as something other than success.
	if got, _ := counterValue(t, collected, "hippocampus.bridge.messages", map[string]string{
		"outcome": OutcomeExists,
	}); got != 1 {
		t.Errorf("messages{outcome=exists} = %d, want 1", got)
	}
}

func TestStore_HandleRejectedIsNotError(t *testing.T) {
	fake := &fakeStorer{rejected: true}
	tr := NewDefaultTransformer(TransformConfig{})

	s := NewStore(fake, tr, 0, "test")

	if err := s.Handle(context.Background(), Message{Subject: "s", Data: []byte("x")}); err != nil {
		t.Fatalf("Handle should not error on a rejected (insignificant) memory: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Errorf("StoreMemory calls = %d, want 1", len(fake.calls))
	}
}

func TestStore_HandleNoMemoriesIsNoOp(t *testing.T) {
	fake := &fakeStorer{}
	tr := TransformerFunc(func(msg Message) ([]*contract.Memory, error) {
		return nil, nil
	})

	s := NewStore(fake, tr, 0, "test")

	if err := s.Handle(context.Background(), Message{Subject: "s"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fake.calls) != 0 {
		t.Errorf("StoreMemory calls = %d, want 0", len(fake.calls))
	}
}

func TestStore_HandleWithCallTimeout(t *testing.T) {
	fake := &fakeStorer{}
	tr := NewDefaultTransformer(TransformConfig{})

	s := NewStore(fake, tr, time.Second, "test")

	if err := s.Handle(context.Background(), Message{Subject: "s", Data: []byte("x")}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Errorf("StoreMemory calls = %d, want 1", len(fake.calls))
	}
}

func TestStore_HandleSkipsNilMemory(t *testing.T) {
	fake := &fakeStorer{}
	tr := TransformerFunc(func(msg Message) ([]*contract.Memory, error) {
		return []*contract.Memory{nil, {Body: "a"}}, nil
	})

	s := NewStore(fake, tr, 0, "test")

	if err := s.Handle(context.Background(), Message{Subject: "s"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("StoreMemory calls = %d, want 1 (nil skipped)", len(fake.calls))
	}
}
