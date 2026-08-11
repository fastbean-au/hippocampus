package promoter

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeStore is an in-memory stand-in for a Hippocampus instance, implementing exactly the Client
// slice the promoter uses. It is a real little store rather than a call recorder, because what these
// tests are checking is the END STATE of the two sides - what reached the target and what is left on
// the source - which a recorder cannot show.
type fakeStore struct {
	mu sync.Mutex

	events   map[string]*contract.Event
	memories map[string]*contract.Memory

	// eventLinks[id] is the event's outbound links.
	eventLinks map[string][]*contract.Link

	// failures, when set for an RPC name, is returned by the next call to it. Set via failNext.
	failures map[string]error

	// summarise replaces an event's memories with one summary when called. Nil means the instance
	// has no summariser configured, which is what a service without ollama.enabled reports.
	summariser func(eventId string, memories []*contract.Memory) *contract.Memory

	calls map[string]int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		events:     map[string]*contract.Event{},
		memories:   map[string]*contract.Memory{},
		eventLinks: map[string][]*contract.Link{},
		failures:   map[string]error{},
		calls:      map[string]int{},
	}
}

// failNext arms a one-shot failure for the named RPC.
func (f *fakeStore) failNext(rpc string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures[rpc] = err
}

// check records the call and returns an armed failure, if any.
func (f *fakeStore) check(rpc string) error {
	f.calls[rpc]++

	err, ok := f.failures[rpc]
	if !ok {
		return nil
	}

	delete(f.failures, rpc)

	return err
}

func (f *fakeStore) callCount(rpc string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[rpc]
}

func (f *fakeStore) putEvent(event *contract.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.events[event.GetId()] = event
}

func (f *fakeStore) putMemory(memory *contract.Memory) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.memories[memory.GetId()] = memory
}

// memoryIds returns the ids held, sorted, for assertions.
func (f *fakeStore) memoryIds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.memories))
	for id := range f.memories {
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}

func (f *fakeStore) eventIds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.events))
	for id := range f.events {
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}

func (f *fakeStore) GetEvents(
	_ context.Context,
	in *contract.GetEventsRequest,
	_ ...grpc.CallOption,
) (*contract.GetEventsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("GetEvents"); err != nil {
		return nil, err
	}

	var matched []*contract.Event

	for _, event := range f.events {
		if in.GetTimeEndMin() > 0 && event.GetTimeEnd() < in.GetTimeEndMin() {
			continue
		}

		if in.GetTimeEndMax() > 0 && event.GetTimeEnd() > in.GetTimeEndMax() {
			continue
		}

		matched = append(matched, event)
	}

	sort.Slice(matched, func(i int, j int) bool { return matched[i].GetId() < matched[j].GetId() })

	total := len(matched)

	if limit := int(in.GetLimit()); limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}

	return &contract.GetEventsResponse{Events: matched, TotalCount: int32(total)}, nil
}

func (f *fakeStore) GetMemories(
	_ context.Context,
	in *contract.GetMemoriesRequest,
	_ ...grpc.CallOption,
) (*contract.GetMemoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("GetMemories"); err != nil {
		return nil, err
	}

	var matched []*contract.Memory

	for _, memory := range f.memories {
		if in.GetEventId() != "" && memory.GetEventId() != in.GetEventId() {
			continue
		}

		switch in.GetHasEvent() {

		case contract.Bool_FALSE:
			if memory.GetEventId() != "" {
				continue
			}

		case contract.Bool_TRUE:
			if memory.GetEventId() == "" {
				continue
			}

		}

		if in.GetTimestampMax() > 0 && memory.GetTimeStamp() > in.GetTimestampMax() {
			continue
		}

		matched = append(matched, memory)
	}

	sort.Slice(matched, func(i int, j int) bool { return matched[i].GetId() < matched[j].GetId() })

	total := len(matched)

	if offset := int(in.GetOffset()); offset > 0 {
		if offset >= len(matched) {
			matched = nil
		} else {
			matched = matched[offset:]
		}
	}

	if limit := int(in.GetLimit()); limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}

	return &contract.GetMemoriesResponse{Memories: matched, TotalCount: int32(total)}, nil
}

func (f *fakeStore) GetEventLinks(
	_ context.Context,
	in *contract.GetEventLinksRequest,
	_ ...grpc.CallOption,
) (*contract.GetLinksResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("GetEventLinks"); err != nil {
		return nil, err
	}

	var edges []*contract.LinkEdge

	for _, link := range f.eventLinks[in.GetId()] {
		edges = append(edges, &contract.LinkEdge{
			Id:           link.GetId(),
			Significance: link.GetSignificance(),
			Direction:    contract.LinkDirection_LINK_DIRECTION_OUTBOUND,
		})
	}

	return &contract.GetLinksResponse{Links: edges}, nil
}

func (f *fakeStore) SummariseMemories(
	_ context.Context,
	in *contract.SummariseMemoriesRequest,
	_ ...grpc.CallOption,
) (*contract.SummariseMemoriesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("SummariseMemories"); err != nil {
		return nil, err
	}

	// What a service without ollama.enabled reports.
	if f.summariser == nil {
		return nil, status.Error(codes.FailedPrecondition, "no summariser is configured (ollama.enabled)")
	}

	var replaced []*contract.Memory

	for _, memory := range f.memories {
		if memory.GetEventId() != in.GetEventId() {
			continue
		}

		replaced = append(replaced, memory)
	}

	summary := f.summariser(in.GetEventId(), replaced)

	for _, memory := range replaced {
		delete(f.memories, memory.GetId())
	}

	f.memories[summary.GetId()] = summary

	return &contract.SummariseMemoriesResponse{Id: summary.GetId(), MemoriesReplaced: int32(len(replaced))}, nil
}

func (f *fakeStore) DeleteEvent(
	_ context.Context,
	in *contract.DeleteEventRequest,
	_ ...grpc.CallOption,
) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("DeleteEvent"); err != nil {
		return nil, err
	}

	if _, ok := f.events[in.GetId()]; !ok {
		return nil, status.Errorf(codes.NotFound, "event '%s' not found", in.GetId())
	}

	delete(f.events, in.GetId())
	delete(f.eventLinks, in.GetId())

	for id, memory := range f.memories {
		if memory.GetEventId() != in.GetId() {
			continue
		}

		if in.GetMemories() {
			delete(f.memories, id)

			continue
		}

		memory.EventId = ""
	}

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeStore) DeleteMemories(
	_ context.Context,
	in *contract.DeleteMemoriesRequest,
	_ ...grpc.CallOption,
) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("DeleteMemories"); err != nil {
		return nil, err
	}

	for _, id := range in.GetIds() {
		delete(f.memories, id)
	}

	return &contract.GeneralResponse{Ok: true}, nil
}

func (f *fakeStore) ImportBatch(
	_ context.Context,
	in *contract.ImportBatchRequest,
	_ ...grpc.CallOption,
) (*contract.ImportBatchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.check("ImportBatch"); err != nil {
		return nil, err
	}

	for _, event := range in.GetEvents() {
		if event.GetId() == "" {
			return nil, fmt.Errorf("imported event without an id")
		}

		// A full-state upsert by id, which is what makes promotion idempotent.
		f.events[event.GetId()] = event

		if len(event.GetLinks()) > 0 {
			f.eventLinks[event.GetId()] = event.GetLinks()
		}
	}

	for _, memory := range in.GetMemories() {
		if memory.GetId() == "" {
			return nil, fmt.Errorf("imported memory without an id")
		}

		f.memories[memory.GetId()] = memory
	}

	return &contract.ImportBatchResponse{
		EventsImported:   int32(len(in.GetEvents())),
		MemoriesImported: int32(len(in.GetMemories())),
	}, nil
}
