package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeClient is a hippoClient stand-in that records the last request it saw and returns a canned
// response (or a canned error), so the tool handlers can be exercised without a live service.
type fakeClient struct {
	storeMemoryReq *contract.Memory
	storeMemoryRes *contract.StoreMemoryResponse

	recallReq *contract.RecallMemoriesRequest
	recallRes *contract.GetMemoriesResponse

	searchReq *contract.SearchMemoriesRequest
	searchRes *contract.GetMemoriesResponse

	getMemoriesReq *contract.GetMemoriesRequest
	getMemoriesRes *contract.GetMemoriesResponse

	storeEventReq *contract.Event
	storeEventRes *contract.StoreEventResponse

	getEventsReq *contract.GetEventsRequest
	getEventsRes *contract.GetEventsResponse

	candidatesRes *contract.GetSummarizationCandidatesResponse

	err error
}

func (f *fakeClient) StoreMemory(_ context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.storeMemoryReq = in

	return f.storeMemoryRes, f.err
}

func (f *fakeClient) RecallMemories(_ context.Context, in *contract.RecallMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.recallReq = in

	return f.recallRes, f.err
}

func (f *fakeClient) SearchMemories(_ context.Context, in *contract.SearchMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.searchReq = in

	return f.searchRes, f.err
}

func (f *fakeClient) GetMemories(_ context.Context, in *contract.GetMemoriesRequest, _ ...grpc.CallOption) (*contract.GetMemoriesResponse, error) {
	f.getMemoriesReq = in

	return f.getMemoriesRes, f.err
}

func (f *fakeClient) StoreEvent(_ context.Context, in *contract.Event, _ ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	f.storeEventReq = in

	return f.storeEventRes, f.err
}

func (f *fakeClient) GetEvents(_ context.Context, in *contract.GetEventsRequest, _ ...grpc.CallOption) (*contract.GetEventsResponse, error) {
	f.getEventsReq = in

	return f.getEventsRes, f.err
}

func (f *fakeClient) GetSummarizationCandidates(_ context.Context, _ *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GetSummarizationCandidatesResponse, error) {

	return f.candidatesRes, f.err
}

func newBridge(client hippoClient) *bridge {

	return &bridge{client: client, callTimeout: time.Second}
}

// TestHandlers_PropagateRPCError checks that every handler surfaces a gRPC error rather than
// swallowing it, exercising the error-return branch of each. store_memory has its own dedicated
// case above.
func TestHandlers_PropagateRPCError(t *testing.T) {
	f := &fakeClient{err: fmt.Errorf("rpc down")}
	b := newBridge(f)
	ctx := context.Background()

	if _, _, err := b.recallMemories(ctx, nil, recallMemoriesInput{Ids: []string{"m1"}}); err == nil {
		t.Error("recallMemories should propagate the RPC error")
	}

	if _, _, err := b.searchMemories(ctx, nil, searchMemoriesInput{Query: "x"}); err == nil {
		t.Error("searchMemories should propagate the RPC error")
	}

	if _, _, err := b.listMemories(ctx, nil, listMemoriesInput{}); err == nil {
		t.Error("listMemories should propagate the RPC error")
	}

	if _, _, err := b.createEvent(ctx, nil, createEventInput{Name: "e"}); err == nil {
		t.Error("createEvent should propagate the RPC error")
	}

	if _, _, err := b.listEvents(ctx, nil, listEventsInput{}); err == nil {
		t.Error("listEvents should propagate the RPC error")
	}

	if _, _, err := b.getSummarizationCandidates(ctx, nil, struct{}{}); err == nil {
		t.Error("getSummarizationCandidates should propagate the RPC error")
	}
}

// TestCallContext_ZeroTimeoutIsUnbounded covers the non-positive-timeout branch of callContext,
// which returns a cancellable-but-deadline-free context.
func TestCallContext_ZeroTimeoutIsUnbounded(t *testing.T) {
	b := &bridge{client: &fakeClient{}, callTimeout: 0}

	callCtx, cancel := b.callContext(context.Background())
	defer cancel()

	if _, ok := callCtx.Deadline(); ok {
		t.Fatal("expected no deadline when callTimeout is non-positive")
	}
}

func TestStoreMemory_MapsRequestAndResponse(t *testing.T) {
	f := &fakeClient{storeMemoryRes: &contract.StoreMemoryResponse{Id: "m1"}}
	b := newBridge(f)

	_, out, err := b.storeMemory(context.Background(), nil, storeMemoryInput{
		Body:         "remember this",
		Significance: 7,
		Group:        "notes",
		EventId:      "e1",
	})
	if err != nil {
		t.Fatalf("storeMemory returned error: %v", err)
	}

	if out.Id != "m1" || out.Rejected {
		t.Fatalf("unexpected output: %+v", out)
	}

	if f.storeMemoryReq.GetBody() != "remember this" ||
		f.storeMemoryReq.GetSignificance() != 7 ||
		f.storeMemoryReq.GetGroup() != "notes" ||
		f.storeMemoryReq.GetEventId() != "e1" {
		t.Fatalf("request not mapped through: %+v", f.storeMemoryReq)
	}
}

func TestStoreMemory_RejectsEmptyBody(t *testing.T) {
	f := &fakeClient{}
	b := newBridge(f)

	if _, _, err := b.storeMemory(context.Background(), nil, storeMemoryInput{}); err == nil {
		t.Fatal("expected an error for empty body")
	}

	if f.storeMemoryReq != nil {
		t.Fatal("StoreMemory should not have been called for empty body")
	}
}

func TestStoreMemory_PropagatesError(t *testing.T) {
	b := newBridge(&fakeClient{err: fmt.Errorf("boom")})

	if _, _, err := b.storeMemory(context.Background(), nil, storeMemoryInput{Body: "x"}); err == nil {
		t.Fatal("expected the gRPC error to propagate")
	}
}

func TestRecallMemories_MapsResponse(t *testing.T) {
	f := &fakeClient{recallRes: &contract.GetMemoriesResponse{
		Memories: []*contract.Memory{
			{Id: "m1", Body: "b1", Significance: 3, RecallCount: 2, IsBinary: contract.Bool_TRUE},
		},
	}}
	b := newBridge(f)

	_, out, err := b.recallMemories(context.Background(), nil, recallMemoriesInput{Ids: []string{"m1"}})
	if err != nil {
		t.Fatalf("recallMemories returned error: %v", err)
	}

	if len(out.Memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(out.Memories))
	}

	m := out.Memories[0]
	if m.Id != "m1" || m.Body != "b1" || m.RecallCount != 2 || !m.IsBinary {
		t.Fatalf("memory not mapped correctly: %+v", m)
	}

	if f.recallReq.GetIds()[0] != "m1" {
		t.Fatalf("recall ids not mapped: %+v", f.recallReq)
	}
}

func TestRecallMemories_RejectsEmptyIds(t *testing.T) {
	b := newBridge(&fakeClient{})

	if _, _, err := b.recallMemories(context.Background(), nil, recallMemoriesInput{}); err == nil {
		t.Fatal("expected an error for empty ids")
	}
}

func TestSearchMemories_MapsRequest(t *testing.T) {
	f := &fakeClient{searchRes: &contract.GetMemoriesResponse{}}
	b := newBridge(f)

	if _, _, err := b.searchMemories(context.Background(), nil, searchMemoriesInput{
		Query:     "hello",
		Limit:     5,
		Group:     "g",
		EventId:   "e",
		Reinforce: true,
	}); err != nil {
		t.Fatalf("searchMemories returned error: %v", err)
	}

	if f.searchReq.GetQuery() != "hello" || f.searchReq.GetLimit() != 5 ||
		f.searchReq.GetGroup() != "g" || f.searchReq.GetEventId() != "e" || !f.searchReq.GetReinforce() {
		t.Fatalf("search request not mapped: %+v", f.searchReq)
	}
}

func TestSearchMemories_RejectsEmptyQuery(t *testing.T) {
	b := newBridge(&fakeClient{})

	if _, _, err := b.searchMemories(context.Background(), nil, searchMemoriesInput{}); err == nil {
		t.Fatal("expected an error for empty query")
	}
}

func TestListMemories_MapsFiltersAndTotal(t *testing.T) {
	f := &fakeClient{getMemoriesRes: &contract.GetMemoriesResponse{
		Memories:   []*contract.Memory{{Id: "m1"}},
		TotalCount: 42,
	}}
	b := newBridge(f)

	_, out, err := b.listMemories(context.Background(), nil, listMemoriesInput{
		Group:           "g",
		SignificanceMin: 1,
		SignificanceMax: 9,
		OrderBy:         "timestamp",
		Limit:           10,
		Offset:          5,
	})
	if err != nil {
		t.Fatalf("listMemories returned error: %v", err)
	}

	if out.TotalCount != 42 || len(out.Memories) != 1 {
		t.Fatalf("unexpected output: %+v", out)
	}

	if f.getMemoriesReq.GetGroup() != "g" || f.getMemoriesReq.GetSignificanceMin() != 1 ||
		f.getMemoriesReq.GetSignificanceMax() != 9 || f.getMemoriesReq.GetOrderBy() != "timestamp" ||
		f.getMemoriesReq.GetLimit() != 10 || f.getMemoriesReq.GetOffset() != 5 {
		t.Fatalf("filters not mapped: %+v", f.getMemoriesReq)
	}
}

func TestCreateEvent_MapsRequestAndResponse(t *testing.T) {
	f := &fakeClient{storeEventRes: &contract.StoreEventResponse{Id: "e1", Rejected: false}}
	b := newBridge(f)

	_, out, err := b.createEvent(context.Background(), nil, createEventInput{
		Name:         "deploy",
		Description:  "prod deploy",
		Significance: 4,
		Group:        "ops",
	})
	if err != nil {
		t.Fatalf("createEvent returned error: %v", err)
	}

	if out.Id != "e1" {
		t.Fatalf("unexpected output: %+v", out)
	}

	if f.storeEventReq.GetName() != "deploy" || f.storeEventReq.GetDescription() != "prod deploy" ||
		f.storeEventReq.GetSignificance() != 4 || f.storeEventReq.GetGroup() != "ops" {
		t.Fatalf("event request not mapped: %+v", f.storeEventReq)
	}
}

func TestCreateEvent_RejectsEmptyName(t *testing.T) {
	b := newBridge(&fakeClient{})

	if _, _, err := b.createEvent(context.Background(), nil, createEventInput{}); err == nil {
		t.Fatal("expected an error for empty name")
	}
}

func TestListEvents_MapsResponse(t *testing.T) {
	f := &fakeClient{getEventsRes: &contract.GetEventsResponse{
		Events:     []*contract.Event{{Id: "e1", Name: "n1", Significance: 2}},
		TotalCount: 3,
	}}
	b := newBridge(f)

	_, out, err := b.listEvents(context.Background(), nil, listEventsInput{Group: "ops"})
	if err != nil {
		t.Fatalf("listEvents returned error: %v", err)
	}

	if out.TotalCount != 3 || len(out.Events) != 1 || out.Events[0].Name != "n1" {
		t.Fatalf("unexpected output: %+v", out)
	}

	if f.getEventsReq.GetGroup() != "ops" {
		t.Fatalf("group filter not mapped: %+v", f.getEventsReq)
	}
}

func TestGetSummarizationCandidates_MapsResponse(t *testing.T) {
	f := &fakeClient{candidatesRes: &contract.GetSummarizationCandidatesResponse{
		Candidates: []*contract.SummarizationCandidate{
			{EventId: "e1", EventName: "n1", MemoryCount: 12},
		},
	}}
	b := newBridge(f)

	_, out, err := b.getSummarizationCandidates(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("getSummarizationCandidates returned error: %v", err)
	}

	if len(out.Candidates) != 1 || out.Candidates[0].EventId != "e1" || out.Candidates[0].MemoryCount != 12 {
		t.Fatalf("unexpected output: %+v", out)
	}
}

// TestServer_EndToEnd stands up the MCP server over an in-memory transport, connects a client, and
// exercises tool discovery and a real tool call end to end - proving schema inference does not
// panic and the structured output round-trips to a client.
func TestServer_EndToEnd(t *testing.T) {
	ctx := context.Background()

	f := &fakeClient{storeMemoryRes: &contract.StoreMemoryResponse{Id: "m1"}}
	server := newServer(newBridge(f), "test")

	serverT, clientT := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)

	clientSession, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string]bool{
		"store_memory":                 false,
		"recall_memories":              false,
		"search_memories":              false,
		"list_memories":                false,
		"create_event":                 false,
		"list_events":                  false,
		"get_summarization_candidates": false,
	}

	for _, v := range tools.Tools {
		if _, ok := want[v.Name]; ok {
			want[v.Name] = true
		}
	}

	for k, seen := range want {
		if !seen {
			t.Errorf("tool %q was not registered", k)
		}
	}

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "store_memory",
		Arguments: map[string]any{"body": "hi", "significance": 5},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if res.IsError {
		t.Fatalf("tool call reported an error: %+v", res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}

	var out storeMemoryOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}

	if out.Id != "m1" {
		t.Fatalf("unexpected structured output: %+v", out)
	}

	if f.storeMemoryReq.GetBody() != "hi" || f.storeMemoryReq.GetSignificance() != 5 {
		t.Fatalf("request not mapped through the transport: %+v", f.storeMemoryReq)
	}
}
