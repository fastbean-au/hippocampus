package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

	updateMemoryReq *contract.Memory
	updateMemoryRes *contract.GeneralResponse

	deleteMemoriesReq *contract.DeleteMemoriesRequest
	deleteMemoriesRes *contract.GeneralResponse

	recallReq *contract.RecallMemoriesRequest
	recallRes *contract.GetMemoriesResponse

	searchReq *contract.SearchMemoriesRequest
	searchRes *contract.GetMemoriesResponse

	getMemoriesReq *contract.GetMemoriesRequest
	getMemoriesRes *contract.GetMemoriesResponse

	storeEventReq *contract.Event
	storeEventRes *contract.StoreEventResponse

	endEventReq *contract.EndEventRequest
	endEventRes *contract.GeneralResponse

	getEventsReq *contract.GetEventsRequest
	getEventsRes *contract.GetEventsResponse

	candidatesRes *contract.GetSummarisationCandidatesResponse

	linkReq   *contract.LinkMemoriesRequest
	unlinkReq *contract.UnlinkMemoriesRequest
	linksReq  *contract.GetMemoryLinksRequest
	linksRes  *contract.GetLinksResponse

	err error
}

func (f *fakeClient) LinkMemories(_ context.Context, in *contract.LinkMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.linkReq = in

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) UnlinkMemories(_ context.Context, in *contract.UnlinkMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.unlinkReq = in

	return &contract.GeneralResponse{Ok: true}, f.err
}

func (f *fakeClient) GetMemoryLinks(_ context.Context, in *contract.GetMemoryLinksRequest, _ ...grpc.CallOption) (*contract.GetLinksResponse, error) {
	f.linksReq = in

	return f.linksRes, f.err
}

func (f *fakeClient) StoreMemory(_ context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.storeMemoryReq = in

	return f.storeMemoryRes, f.err
}

func (f *fakeClient) UpdateMemory(_ context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.updateMemoryReq = in

	return f.updateMemoryRes, f.err
}

func (f *fakeClient) DeleteMemories(_ context.Context, in *contract.DeleteMemoriesRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.deleteMemoriesReq = in

	return f.deleteMemoriesRes, f.err
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

func (f *fakeClient) EndEvent(_ context.Context, in *contract.EndEventRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.endEventReq = in

	return f.endEventRes, f.err
}

func (f *fakeClient) GetEvents(_ context.Context, in *contract.GetEventsRequest, _ ...grpc.CallOption) (*contract.GetEventsResponse, error) {
	f.getEventsReq = in

	return f.getEventsRes, f.err
}

func (f *fakeClient) GetSummarisationCandidates(_ context.Context, _ *contract.EmptyRequest, _ ...grpc.CallOption) (*contract.GetSummarisationCandidatesResponse, error) {
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

	if _, _, err := b.updateMemory(ctx, nil, updateMemoryInput{Id: "m1", Body: "x"}); err == nil {
		t.Error("updateMemory should propagate the RPC error")
	}

	if _, _, err := b.deleteMemories(ctx, nil, deleteMemoriesInput{Ids: []string{"m1"}}); err == nil {
		t.Error("deleteMemories should propagate the RPC error")
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

	if _, _, err := b.getSummarisationCandidates(ctx, nil, struct{}{}); err == nil {
		t.Error("getSummarisationCandidates should propagate the RPC error")
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

func TestUpdateMemory_MapsRequestAndResponse(t *testing.T) {
	f := &fakeClient{updateMemoryRes: &contract.GeneralResponse{Ok: true}}
	b := newBridge(f)

	_, out, err := b.updateMemory(context.Background(), nil, updateMemoryInput{
		Id:           "m1",
		Body:         "revised",
		Significance: 8,
		Group:        "notes",
		EventId:      "e1",
	})
	if err != nil {
		t.Fatalf("updateMemory returned error: %v", err)
	}

	if !out.Ok {
		t.Fatalf("unexpected output: %+v", out)
	}

	if f.updateMemoryReq.GetId() != "m1" || f.updateMemoryReq.GetBody() != "revised" ||
		f.updateMemoryReq.GetSignificance() != 8 || f.updateMemoryReq.GetGroup() != "notes" ||
		f.updateMemoryReq.GetEventId() != "e1" {
		t.Fatalf("request not mapped through: %+v", f.updateMemoryReq)
	}
}

func TestUpdateMemory_RejectsMissingId(t *testing.T) {
	f := &fakeClient{}
	b := newBridge(f)

	if _, _, err := b.updateMemory(context.Background(), nil, updateMemoryInput{Body: "x"}); err == nil {
		t.Fatal("expected an error when id is missing")
	}

	if f.updateMemoryReq != nil {
		t.Fatal("UpdateMemory should not have been called without an id")
	}
}

func TestUpdateMemory_RejectsNoFields(t *testing.T) {
	f := &fakeClient{}
	b := newBridge(f)

	if _, _, err := b.updateMemory(context.Background(), nil, updateMemoryInput{Id: "m1"}); err == nil {
		t.Fatal("expected an error when no updatable field is set")
	}

	if f.updateMemoryReq != nil {
		t.Fatal("UpdateMemory should not have been called with nothing to update")
	}
}

func TestDeleteMemories_MapsRequestAndResponse(t *testing.T) {
	f := &fakeClient{deleteMemoriesRes: &contract.GeneralResponse{Ok: true}}
	b := newBridge(f)

	_, out, err := b.deleteMemories(context.Background(), nil, deleteMemoriesInput{Ids: []string{"m1", "m2"}})
	if err != nil {
		t.Fatalf("deleteMemories returned error: %v", err)
	}

	if !out.Ok {
		t.Fatalf("unexpected output: %+v", out)
	}

	if len(f.deleteMemoriesReq.GetIds()) != 2 || f.deleteMemoriesReq.GetIds()[0] != "m1" {
		t.Fatalf("ids not mapped through: %+v", f.deleteMemoriesReq)
	}
}

func TestDeleteMemories_RejectsEmptyIds(t *testing.T) {
	f := &fakeClient{}
	b := newBridge(f)

	if _, _, err := b.deleteMemories(context.Background(), nil, deleteMemoriesInput{}); err == nil {
		t.Fatal("expected an error for empty ids")
	}

	if f.deleteMemoriesReq != nil {
		t.Fatal("DeleteMemories should not have been called with no ids")
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
		Events:     []*contract.Event{{Id: "e1", Name: "n1", Significance: 2, MemoryCount: 7}},
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

	if out.Events[0].MemoryCount != 7 {
		t.Fatalf("memory count not projected: %+v", out.Events[0])
	}

	if f.getEventsReq.GetGroup() != "ops" {
		t.Fatalf("group filter not mapped: %+v", f.getEventsReq)
	}

	// Counts are always asked for and bodies never are: the count is what tells a model whether an
	// event is worth opening, while `memories` would put every body on the page into its context.
	if !f.getEventsReq.GetMemoryCounts() || f.getEventsReq.GetMemories() {
		t.Fatalf("expected memory_counts and not memories: %+v", f.getEventsReq)
	}
}

func TestGetSummarisationCandidates_MapsResponse(t *testing.T) {
	f := &fakeClient{candidatesRes: &contract.GetSummarisationCandidatesResponse{
		Candidates: []*contract.SummarisationCandidate{
			{EventId: "e1", EventName: "n1", MemoryCount: 12},
		},
	}}
	b := newBridge(f)

	_, out, err := b.getSummarisationCandidates(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("getSummarisationCandidates returned error: %v", err)
	}

	if len(out.Candidates) != 1 || out.Candidates[0].EventId != "e1" || out.Candidates[0].MemoryCount != 12 {
		t.Fatalf("unexpected output: %+v", out)
	}
}

// TestGetSummarisationCandidates_ProjectsScanEnabled pins the flag that tells an empty list apart
// from a service that will never produce one - the difference between asking again later and not.
func TestGetSummarisationCandidates_ProjectsScanEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		f := &fakeClient{candidatesRes: &contract.GetSummarisationCandidatesResponse{ScanEnabled: enabled}}
		b := newBridge(f)

		_, out, err := b.getSummarisationCandidates(context.Background(), nil, struct{}{})
		if err != nil {
			t.Fatalf("getSummarisationCandidates returned error: %v", err)
		}

		if out.ScanEnabled != enabled {
			t.Errorf("scan_enabled = %v, want %v", out.ScanEnabled, enabled)
		}
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

	// The comparison is EXACT in both directions. Only checking that the expected tools are present
	// is what let three link tools and end_event be registered without the tool table in
	// docs/mcp.md or the surface described in CLAUDE.md gaining them - and on this bridge the
	// registered set is a security statement (no Purge, no Export, no event deletion), so a tool
	// arriving unremarked is exactly what wants noticing.
	want := map[string]bool{
		"store_memory":                 false,
		"update_memory":                false,
		"delete_memories":              false,
		"recall_memories":              false,
		"search_memories":              false,
		"list_memories":                false,
		"link_memories":                false,
		"unlink_memories":              false,
		"get_memory_links":             false,
		"create_event":                 false,
		"end_event":                    false,
		"list_events":                  false,
		"get_summarisation_candidates": false,
	}

	for _, v := range tools.Tools {
		if _, ok := want[v.Name]; !ok {
			t.Errorf("tool %q is registered but not expected - add it here and to the table in docs/mcp.md, or unregister it", v.Name)

			continue
		}

		want[v.Name] = true
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

// --- the link tools ---

func TestLinkMemories(t *testing.T) {
	fake := &fakeClient{}
	b := &bridge{client: fake, callTimeout: time.Second}

	_, out, err := b.linkMemories(context.Background(), nil, linkMemoriesInput{
		Id: "m1",
		Links: []linkViewInput{
			{Id: "m2", Significance: 5},
			{Id: "m3", Significance: 7},
		},
	})
	if err != nil {
		t.Fatalf("linkMemories: %s", err)
	}

	if !out.Ok {
		t.Error("expected ok")
	}

	if fake.linkReq.GetId() != "m1" || len(fake.linkReq.GetLinks()) != 2 {
		t.Fatalf("unexpected request: %+v", fake.linkReq)
	}

	if fake.linkReq.GetLinks()[1].GetId() != "m3" || fake.linkReq.GetLinks()[1].GetSignificance() != 7 {
		t.Errorf("unexpected links: %+v", fake.linkReq.GetLinks())
	}
}

func TestLinkMemoriesValidation(t *testing.T) {
	b := &bridge{client: &fakeClient{}, callTimeout: time.Second}

	cases := []struct {
		name string
		in   linkMemoriesInput
	}{
		{"no id", linkMemoriesInput{Links: []linkViewInput{{Id: "m2"}}}},
		{"no links", linkMemoriesInput{Id: "m1"}},
		{"link without an id", linkMemoriesInput{Id: "m1", Links: []linkViewInput{{Significance: 3}}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := b.linkMemories(context.Background(), nil, c.in); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestUnlinkMemories(t *testing.T) {
	fake := &fakeClient{}
	b := &bridge{client: fake, callTimeout: time.Second}

	if _, _, err := b.unlinkMemories(context.Background(), nil, unlinkMemoriesInput{Id: "m1", Ids: []string{"m2"}}); err != nil {
		t.Fatalf("unlinkMemories: %s", err)
	}

	if fake.unlinkReq.GetId() != "m1" || len(fake.unlinkReq.GetIds()) != 1 {
		t.Fatalf("unexpected request: %+v", fake.unlinkReq)
	}

	for _, in := range []unlinkMemoriesInput{{Ids: []string{"m2"}}, {Id: "m1"}} {
		if _, _, err := b.unlinkMemories(context.Background(), nil, in); err == nil {
			t.Errorf("expected an error for %+v", in)
		}
	}
}

func TestGetMemoryLinks(t *testing.T) {
	fake := &fakeClient{linksRes: &contract.GetLinksResponse{
		LinkSignificance: 12,
		Links: []*contract.LinkEdge{
			{Id: "m2", Significance: 5, Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND, Created: 42},
			{Id: "m3", Significance: 7, Direction: contract.LinkDirection_LINK_DIRECTION_INBOUND},
		},
	}}
	b := &bridge{client: fake, callTimeout: time.Second}

	_, out, err := b.getMemoryLinks(context.Background(), nil, getMemoryLinksInput{Id: "m1", Direction: "outbound"})
	if err != nil {
		t.Fatalf("getMemoryLinks: %s", err)
	}

	if fake.linksReq.GetDirection() != contract.LinkDirection_LINK_DIRECTION_OUTBOUND {
		t.Errorf("direction not passed through: %v", fake.linksReq.GetDirection())
	}

	if out.LinkSignificance != 12 || len(out.Links) != 2 {
		t.Fatalf("unexpected output: %+v", out)
	}

	if out.Links[0].Direction != "outbound" || out.Links[1].Direction != "inbound" {
		t.Errorf("directions not projected: %+v", out.Links)
	}
}

// TestGetMemoryLinksUnknownDirection pins that an unrecognised direction is refused rather than
// quietly treated as "both" - a caller that asked for one direction and silently got the other
// would have no way to tell.
func TestGetMemoryLinksUnknownDirection(t *testing.T) {
	b := &bridge{client: &fakeClient{}, callTimeout: time.Second}

	if _, _, err := b.getMemoryLinks(context.Background(), nil, getMemoryLinksInput{Id: "m1", Direction: "sideways"}); err == nil {
		t.Error("expected an error for an unknown direction")
	}

	if _, _, err := b.getMemoryLinks(context.Background(), nil, getMemoryLinksInput{Direction: "both"}); err == nil {
		t.Error("expected an error when no id was given")
	}
}

// TestRecallIncludeLinkedPassesThrough pins the associative-recall flag reaching the RPC.
func TestRecallIncludeLinkedPassesThrough(t *testing.T) {
	fake := &fakeClient{recallRes: &contract.GetMemoriesResponse{}}
	b := &bridge{client: fake, callTimeout: time.Second}

	if _, _, err := b.recallMemories(context.Background(), nil, recallMemoriesInput{
		Ids:           []string{"m1"},
		IncludeLinked: true,
	}); err != nil {
		t.Fatalf("recallMemories: %s", err)
	}

	if !fake.recallReq.GetIncludeLinked() {
		t.Error("include_linked did not reach the RPC")
	}
}

// TestMetadataReachesTheRPCs verifies metadata travels on every tool that writes it, as a proto map
// on the write path and as packed "key=value" strings on the filter path - the two shapes are
// different because the list RPCs are HTTP GETs, whose query strings cannot carry a map.
func TestMetadataReachesTheRPCs(t *testing.T) {
	ctx := context.Background()
	metadata := map[string]string{"source": "slack", "project": "apollo"}

	t.Run("store_memory", func(t *testing.T) {
		f := &fakeClient{storeMemoryRes: &contract.StoreMemoryResponse{Id: "m1"}}
		b := newBridge(f)

		if _, _, err := b.storeMemory(ctx, nil, storeMemoryInput{Body: "b", Metadata: metadata}); err != nil {
			t.Fatalf("storeMemory: %s", err)
		}

		if !reflect.DeepEqual(f.storeMemoryReq.GetMetadata(), metadata) {
			t.Errorf("StoreMemory received %#v, want %#v", f.storeMemoryReq.GetMetadata(), metadata)
		}
	})

	t.Run("update_memory", func(t *testing.T) {
		f := &fakeClient{}
		b := newBridge(f)

		if _, _, err := b.updateMemory(ctx, nil, updateMemoryInput{
			Id: "m1", Metadata: metadata, ClearGroup: true,
		}); err != nil {
			t.Fatalf("updateMemory: %s", err)
		}

		if !reflect.DeepEqual(f.updateMemoryReq.GetMetadata(), metadata) {
			t.Errorf("UpdateMemory received %#v, want %#v", f.updateMemoryReq.GetMetadata(), metadata)
		}

		if !f.updateMemoryReq.GetClearGroup() {
			t.Error("UpdateMemory did not carry clear_group")
		}
	})

	t.Run("create_event", func(t *testing.T) {
		f := &fakeClient{}
		b := newBridge(f)

		if _, _, err := b.createEvent(ctx, nil, createEventInput{Name: "e", Metadata: metadata}); err != nil {
			t.Fatalf("createEvent: %s", err)
		}

		if !reflect.DeepEqual(f.storeEventReq.GetMetadata(), metadata) {
			t.Errorf("StoreEvent received %#v, want %#v", f.storeEventReq.GetMetadata(), metadata)
		}
	})

	// The filter path packs the map into sorted pairs.
	want := []string{"project=apollo", "source=slack"}

	t.Run("list_memories", func(t *testing.T) {
		f := &fakeClient{}
		b := newBridge(f)

		if _, _, err := b.listMemories(ctx, nil, listMemoriesInput{Metadata: metadata}); err != nil {
			t.Fatalf("listMemories: %s", err)
		}

		if !reflect.DeepEqual(f.getMemoriesReq.GetMetadata(), want) {
			t.Errorf("GetMemories received %#v, want %#v", f.getMemoriesReq.GetMetadata(), want)
		}
	})

	t.Run("search_memories", func(t *testing.T) {
		f := &fakeClient{}
		b := newBridge(f)

		if _, _, err := b.searchMemories(ctx, nil, searchMemoriesInput{Query: "q", Metadata: metadata}); err != nil {
			t.Fatalf("searchMemories: %s", err)
		}

		if !reflect.DeepEqual(f.searchReq.GetMetadata(), want) {
			t.Errorf("SearchMemories received %#v, want %#v", f.searchReq.GetMetadata(), want)
		}
	})

	t.Run("list_events", func(t *testing.T) {
		f := &fakeClient{}
		b := newBridge(f)

		if _, _, err := b.listEvents(ctx, nil, listEventsInput{Metadata: metadata}); err != nil {
			t.Fatalf("listEvents: %s", err)
		}

		if !reflect.DeepEqual(f.getEventsReq.GetMetadata(), want) {
			t.Errorf("GetEvents received %#v, want %#v", f.getEventsReq.GetMetadata(), want)
		}
	})
}

// TestUpdateMemoryAcceptsMetadataOnlyUpdates covers the "no fields set" guard. It enumerates the
// updatable fields by hand, so a new one that is not added to it makes an otherwise valid update
// fail with a misleading message.
func TestUpdateMemoryAcceptsMetadataOnlyUpdates(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		input updateMemoryInput
	}{
		{"metadata only", updateMemoryInput{Id: "m1", Metadata: map[string]string{"source": "slack"}}},
		{"clear metadata only", updateMemoryInput{Id: "m1", ClearMetadata: true}},
		{"clear group only", updateMemoryInput{Id: "m1", ClearGroup: true}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBridge(&fakeClient{})

			if _, _, err := b.updateMemory(ctx, nil, c.input); err != nil {
				t.Fatalf("expected the update to be accepted, got: %s", err)
			}
		})
	}

	// An update setting nothing at all is still rejected.
	b := newBridge(&fakeClient{})

	if _, _, err := b.updateMemory(ctx, nil, updateMemoryInput{Id: "m1"}); err == nil {
		t.Error("expected an update with no fields set to be rejected")
	}
}

// TestMemoryViewCarriesMetadata verifies metadata reaches the MCP host on the way back out.
func TestMemoryViewCarriesMetadata(t *testing.T) {
	metadata := map[string]string{"source": "slack"}

	view := toMemoryView(&contract.Memory{Id: "m1", Body: "b", Metadata: metadata})
	if !reflect.DeepEqual(view.Metadata, metadata) {
		t.Errorf("memoryView carried %#v, want %#v", view.Metadata, metadata)
	}

	if got := toMemoryView(&contract.Memory{Id: "m1"}).Metadata; got != nil {
		t.Errorf("expected no metadata for a memory without any, got %#v", got)
	}

	eview := toEventView(&contract.Event{Id: "e1", Name: "e", Metadata: metadata})
	if !reflect.DeepEqual(eview.Metadata, metadata) {
		t.Errorf("eventView carried %#v, want %#v", eview.Metadata, metadata)
	}
}

// TestTriStateFilter pins the optional-bool mapping: a tool must be able to say "only the false
// ones", which is exactly what a plain bool cannot express.
func TestTriStateFilter(t *testing.T) {
	yes, no := true, false

	if got := triStateFilter(nil); got != contract.Bool_UNSPECIFIED {
		t.Errorf("nil should be UNSPECIFIED, got %v", got)
	}

	if got := triStateFilter(&yes); got != contract.Bool_TRUE {
		t.Errorf("true should be TRUE, got %v", got)
	}

	if got := triStateFilter(&no); got != contract.Bool_FALSE {
		t.Errorf("false should be FALSE, got %v", got)
	}
}

// TestListSortDirectionMapping pins the order_dir string→enum mapping on both listing tools,
// including the deliberate fallback: an unrecognised direction is treated as unset rather than
// failing the call.
func TestListSortDirectionMapping(t *testing.T) {
	cases := []struct {
		in   string
		want contract.SortDirection
	}{
		{"asc", contract.SortDirection_SORT_DIRECTION_ASC},
		{"desc", contract.SortDirection_SORT_DIRECTION_DESC},
		{"", contract.SortDirection_SORT_DIRECTION_UNSPECIFIED},
		{"sideways", contract.SortDirection_SORT_DIRECTION_UNSPECIFIED},
	}

	for _, c := range cases {
		f := &fakeClient{
			getMemoriesRes: &contract.GetMemoriesResponse{},
			getEventsRes:   &contract.GetEventsResponse{},
		}
		b := newBridge(f)

		if _, _, err := b.listMemories(context.Background(), nil,
			listMemoriesInput{OrderBy: "recall_count", OrderDir: c.in}); err != nil {
			t.Fatalf("listMemories(%q): %v", c.in, err)
		}

		if got := f.getMemoriesReq.GetOrderDir(); got != c.want {
			t.Errorf("listMemories order_dir %q = %v, want %v", c.in, got, c.want)
		}

		if got := f.getMemoriesReq.GetOrderBy(); got != "recall_count" {
			t.Errorf("listMemories order_by = %q, want recall_count", got)
		}

		if _, _, err := b.listEvents(context.Background(), nil,
			listEventsInput{OrderBy: "name", OrderDir: c.in}); err != nil {
			t.Fatalf("listEvents(%q): %v", c.in, err)
		}

		if got := f.getEventsReq.GetOrderDir(); got != c.want {
			t.Errorf("listEvents order_dir %q = %v, want %v", c.in, got, c.want)
		}

		if got := f.getEventsReq.GetOrderBy(); got != "name" {
			t.Errorf("listEvents order_by = %q, want name", got)
		}
	}
}

// TestEndEvent_MapsRequestAndResponse covers the tool that closes an event. The bridge could
// create events and never end one, so every event a model opened stored an end time of 0 - which
// sorts as the oldest-ended rather than the most recent, and reads as open forever.
func TestEndEvent_MapsRequestAndResponse(t *testing.T) {
	f := &fakeClient{endEventRes: &contract.GeneralResponse{Ok: true}}
	b := newBridge(f)

	_, out, err := b.endEvent(context.Background(), nil, endEventInput{Id: "e1", TimeEnd: "2026-08-19T14:30:00Z"})
	if err != nil {
		t.Fatalf("endEvent returned error: %v", err)
	}

	if !out.Ok {
		t.Fatalf("unexpected output: %+v", out)
	}

	want := time.Date(2026, 8, 19, 14, 30, 0, 0, time.UTC).UnixNano()

	if f.endEventReq.GetId() != "e1" || f.endEventReq.GetTimeEnd() != want {
		t.Fatalf("request not mapped through: %+v", f.endEventReq)
	}
}

func TestEndEvent_RequiresAnId(t *testing.T) {
	f := &fakeClient{}

	if _, _, err := newBridge(f).endEvent(context.Background(), nil, endEventInput{}); err == nil {
		t.Fatal("expected an error for a missing id")
	}

	if f.endEventReq != nil {
		t.Fatal("EndEvent should not have been called without an id")
	}
}

// TestEndEvent_OmittedTimeMeansNow pins the difference between "not given" and "the epoch": the RPC
// reads 0 as "use the server's clock", so an omitted time must stay 0 rather than becoming a
// parsed zero Time (which would end the event in year 1).
func TestEndEvent_OmittedTimeMeansNow(t *testing.T) {
	f := &fakeClient{endEventRes: &contract.GeneralResponse{Ok: true}}

	if _, _, err := newBridge(f).endEvent(context.Background(), nil, endEventInput{Id: "e1"}); err != nil {
		t.Fatalf("endEvent returned error: %v", err)
	}

	if f.endEventReq.GetTimeEnd() != 0 {
		t.Fatalf("time_end = %d, want 0 so the service uses its own clock", f.endEventReq.GetTimeEnd())
	}
}

// TestListingTimeFiltersAreParsed covers the RFC3339 bounds on both listings. The tools speak
// RFC3339 while the RPCs take UnixNano deliberately - a model can write a date but not reliably
// arrive at a nanosecond epoch, and a bound wrong by three orders of magnitude returns a
// plausible empty page rather than an error.
func TestListingTimeFiltersAreParsed(t *testing.T) {
	from := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	t.Run("memories", func(t *testing.T) {
		f := &fakeClient{getMemoriesRes: &contract.GetMemoriesResponse{}}

		_, _, err := newBridge(f).listMemories(context.Background(), nil, listMemoriesInput{
			StoredAfter:  from.Format(time.RFC3339),
			StoredBefore: to.Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("listMemories returned error: %v", err)
		}

		if f.getMemoriesReq.GetTimestampMin() != from.UnixNano() || f.getMemoriesReq.GetTimestampMax() != to.UnixNano() {
			t.Fatalf("bounds not mapped through: %+v", f.getMemoriesReq)
		}
	})

	t.Run("events", func(t *testing.T) {
		f := &fakeClient{getEventsRes: &contract.GetEventsResponse{}}

		_, _, err := newBridge(f).listEvents(context.Background(), nil, listEventsInput{
			StartedAfter:  from.Format(time.RFC3339),
			StartedBefore: to.Format(time.RFC3339),
			EndedAfter:    from.Format(time.RFC3339),
			EndedBefore:   to.Format(time.RFC3339),
		})
		if err != nil {
			t.Fatalf("listEvents returned error: %v", err)
		}

		if f.getEventsReq.GetTimeStartMin() != from.UnixNano() ||
			f.getEventsReq.GetTimeStartMax() != to.UnixNano() ||
			f.getEventsReq.GetTimeEndMin() != from.UnixNano() ||
			f.getEventsReq.GetTimeEndMax() != to.UnixNano() {
			t.Fatalf("bounds not mapped through: %+v", f.getEventsReq)
		}
	})

	t.Run("a malformed bound names the field", func(t *testing.T) {
		f := &fakeClient{}

		_, _, err := newBridge(f).listMemories(context.Background(), nil, listMemoriesInput{StoredAfter: "last tuesday"})
		if err == nil {
			t.Fatal("expected an error for a malformed timestamp")
		}

		if !strings.Contains(err.Error(), "stored_after") {
			t.Errorf("the error does not name the field: %v", err)
		}

		if f.getMemoriesReq != nil {
			t.Error("GetMemories was called with an unparsed bound")
		}
	})

	t.Run("omitted bounds stay unset", func(t *testing.T) {
		f := &fakeClient{getMemoriesRes: &contract.GetMemoriesResponse{}}

		if _, _, err := newBridge(f).listMemories(context.Background(), nil, listMemoriesInput{}); err != nil {
			t.Fatalf("listMemories returned error: %v", err)
		}

		if f.getMemoriesReq.GetTimestampMin() != 0 || f.getMemoriesReq.GetTimestampMax() != 0 {
			t.Errorf("an omitted bound became a filter: %+v", f.getMemoriesReq)
		}
	})
}

// TestEveryToolIsDocumented holds the tool table in docs/mcp.md to the tools actually registered.
// The table is what an operator reads to decide whether to point a model at this bridge, and the
// registered set is a deliberately narrow slice of the RPC surface - so a tool that appears in one
// and not the other misdescribes what a model can reach. It drifted exactly that way once already:
// the three link tools and end_event were registered while the prose still described nine.
func TestEveryToolIsDocumented(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "docs", "mcp.md"))
	if err != nil {
		t.Fatalf("failed to read docs/mcp.md: %s", err.Error())
	}

	document := string(source)

	server := newServer(newBridge(&fakeClient{}), "test")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}

	defer func() { _ = serverSession.Close() }()

	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	defer func() { _ = clientSession.Close() }()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools.Tools) == 0 {
		t.Fatal("no tools were registered")
	}

	for _, tool := range tools.Tools {
		if !strings.Contains(document, "`"+tool.Name+"`") {
			t.Errorf("tool %q is registered but named nowhere in docs/mcp.md", tool.Name)
		}
	}
}
