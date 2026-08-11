package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeStorer records StoreMemory calls and returns a scripted response/error.
type fakeStorer struct {
	calls    []*contract.Memory
	rejected bool
	err      error
}

func (f *fakeStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}

	return &contract.StoreMemoryResponse{Id: "id-" + in.GetBody(), Rejected: f.rejected}, nil
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
