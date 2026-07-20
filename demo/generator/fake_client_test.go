package main

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeHippoClient is an in-memory stand-in for contract.HippocampusClient, used to exercise the
// generator's worker logic without a real gRPC server. Embedding the interface (left nil) means
// any method the tests don't call panics loudly instead of silently compiling away a real
// implementation, which keeps this fake honest about what the generator actually uses.
type fakeHippoClient struct {
	contract.HippocampusClient

	mu sync.Mutex

	events   map[string]*contract.Event
	memories map[string]*contract.Memory

	nextEventID  int
	nextMemoryID int

	// errOn forces the named method to fail on its next N calls (decremented per call).
	errOn map[string]int

	calls map[string]int

	// recalled records ids passed to RecallMemories, in call order.
	recalled [][]string
}

func newFakeHippoClient() *fakeHippoClient {
	return &fakeHippoClient{
		events:   make(map[string]*contract.Event),
		memories: make(map[string]*contract.Memory),
		errOn:    make(map[string]int),
		calls:    make(map[string]int),
	}
}

func (f *fakeHippoClient) fail(method string) bool {
	f.calls[method]++

	if n := f.errOn[method]; n > 0 {
		f.errOn[method] = n - 1

		return true
	}

	return false
}

func (f *fakeHippoClient) StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("StoreEvent") {
		return nil, fmt.Errorf("fake StoreEvent error")
	}

	f.nextEventID++
	id := fmt.Sprintf("evt-%d", f.nextEventID)

	f.events[id] = &contract.Event{
		Id:           id,
		TimeStart:    in.GetTimeStart(),
		TimeEnd:      in.GetTimeEnd(),
		Significance: in.GetSignificance(),
		Name:         in.GetName(),
		Description:  in.GetDescription(),
		Group:        in.GetGroup(),
	}

	return &contract.StoreEventResponse{Id: id}, nil
}

func (f *fakeHippoClient) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("StoreMemory") {
		return nil, fmt.Errorf("fake StoreMemory error")
	}

	f.nextMemoryID++
	id := fmt.Sprintf("mem-%d", f.nextMemoryID)

	f.memories[id] = &contract.Memory{
		Id:           id,
		TimeStamp:    in.GetTimeStamp(),
		Significance: in.GetSignificance(),
		EventId:      in.GetEventId(),
		Body:         in.GetBody(),
		IsBinary:     in.GetIsBinary(),
		Group:        in.GetGroup(),
	}

	return &contract.StoreMemoryResponse{Id: id}, nil
}

func (f *fakeHippoClient) EndEvent(ctx context.Context, in *contract.EndEventRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("EndEvent") {
		return nil, fmt.Errorf("fake EndEvent error")
	}

	if ev, ok := f.events[in.GetId()]; ok {
		ev.TimeEnd = in.GetTimeEnd()
	}

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeHippoClient) UpdateEventSignificance(ctx context.Context, in *contract.UpdateEventSignificanceRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("UpdateEventSignificance") {
		return nil, fmt.Errorf("fake UpdateEventSignificance error")
	}

	if ev, ok := f.events[in.GetId()]; ok {
		ev.Significance = in.GetSignificance()
	}

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeHippoClient) MergeEvents(ctx context.Context, in *contract.MergeEventsRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("MergeEvents") {
		return nil, fmt.Errorf("fake MergeEvents error")
	}

	delete(f.events, in.GetMergeFrom())

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeHippoClient) DeleteEvent(ctx context.Context, in *contract.DeleteEventRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("DeleteEvent") {
		return nil, fmt.Errorf("fake DeleteEvent error")
	}

	delete(f.events, in.GetId())

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeHippoClient) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("DeleteMemories") {
		return nil, fmt.Errorf("fake DeleteMemories error")
	}

	for _, id := range in.GetIds() {
		delete(f.memories, id)
	}

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeHippoClient) GetEventById(ctx context.Context, in *contract.GetEventByIdRequest, opts ...grpc.CallOption) (*contract.GetEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("GetEventById") {
		return nil, fmt.Errorf("fake GetEventById error")
	}

	ev, ok := f.events[in.GetId()]
	if !ok {
		return nil, fmt.Errorf("not found")
	}

	out := &contract.Event{
		Id:           ev.GetId(),
		TimeStart:    ev.GetTimeStart(),
		TimeEnd:      ev.GetTimeEnd(),
		Significance: ev.GetSignificance(),
		Name:         ev.GetName(),
		Description:  ev.GetDescription(),
		Group:        ev.GetGroup(),
	}

	if in.GetMemories() {
		for _, m := range f.memories {
			if m.GetEventId() == ev.GetId() {
				out.Memories = append(out.Memories, m)
			}
		}
	}

	return &contract.GetEventResponse{Event: out}, nil
}

func (f *fakeHippoClient) GetEvents(ctx context.Context, in *contract.GetEventsRequest, opts ...grpc.CallOption) (*contract.GetEventsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("GetEvents") {
		return nil, fmt.Errorf("fake GetEvents error")
	}

	var out []*contract.Event
	for _, ev := range f.events {
		out = append(out, ev)
	}

	limit := int(in.GetLimit())
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}

	return &contract.GetEventsResponse{Events: out, TotalCount: int32(len(f.events))}, nil
}

func (f *fakeHippoClient) GetMemories(ctx context.Context, in *contract.GetMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("GetMemories") {
		return nil, fmt.Errorf("fake GetMemories error")
	}

	var out []*contract.Memory
	for _, m := range f.memories {
		out = append(out, m)
	}

	limit := int(in.GetLimit())
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}

	return &contract.GetMemoriesResponse{Memories: out, TotalCount: int32(len(f.memories))}, nil
}

func (f *fakeHippoClient) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recalled = append(f.recalled, in.GetIds())

	if f.fail("RecallMemories") {
		return nil, fmt.Errorf("fake RecallMemories error")
	}

	var out []*contract.Memory
	for _, id := range in.GetIds() {
		if m, ok := f.memories[id]; ok {
			out = append(out, m)
		}
	}

	return &contract.GetMemoriesResponse{Memories: out}, nil
}

func (f *fakeHippoClient) Sleep(ctx context.Context, in *contract.EmptyRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail("Sleep") {
		return nil, fmt.Errorf("fake Sleep error")
	}

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeHippoClient) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[method]
}
