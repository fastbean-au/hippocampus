package main

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fastbean-au/hippocampus/contract"
)

// hippoClient is the slice of the generated gRPC client the tool handlers use. Narrowing the
// dependency to an interface (rather than the concrete contract.HippocampusClient) lets a test
// drive the handlers with a fake, without a live service or a real network connection.
type hippoClient interface {
	StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error)
	RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	SearchMemories(ctx context.Context, in *contract.SearchMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	GetMemories(ctx context.Context, in *contract.GetMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error)
	GetEvents(ctx context.Context, in *contract.GetEventsRequest, opts ...grpc.CallOption) (*contract.GetEventsResponse, error)
	GetSummarizationCandidates(ctx context.Context, in *contract.EmptyRequest, opts ...grpc.CallOption) (*contract.GetSummarizationCandidatesResponse, error)
}

// bridge holds the gRPC client every tool handler dispatches through, plus the per-call timeout
// bounding each request. It carries no other state - a restart or a second concurrent session sees
// the same live store through the same client.
type bridge struct {
	client      hippoClient
	callTimeout time.Duration
}

// callContext derives a per-call context from the tool-call context, bounding the gRPC request by
// callTimeout so a hung or unreachable service fails a tool call after a bounded time rather than
// stalling the MCP session. A non-positive timeout leaves the parent context unbounded.
func (b *bridge) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.callTimeout <= 0 {

		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, b.callTimeout)
}

// newServer builds the MCP server and registers the curated tool set. The surface is deliberately
// the memory-and-event operations an LLM needs to give and retrieve memories; the destructive and
// administrative RPCs (Purge, Export/Import/Transfer/Clear, event deletion/merge) are intentionally
// not exposed, so a model cannot wipe or exfiltrate a store through this bridge.
func newServer(b *bridge, serverVersion string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "hippocampus",
		Title:   "Hippocampus memory",
		Version: serverVersion,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "store_memory",
		Description: "Store a new memory (a piece of text worth remembering) in Hippocampus. " +
			"Less-significant memories are forgotten over time, so set significance to reflect how " +
			"important the memory is. Returns the stored memory's id, or rejected=true if the " +
			"service dropped it for being below its minimum significance.",
	}, b.storeMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "recall_memories",
		Description: "Fetch memories by id and reinforce them: recalling resets each memory's decay " +
			"clock and raises its effective significance, so it is remembered longer. Use this when " +
			"you are genuinely retrieving a memory, not merely browsing.",
	}, b.recallMemories)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_memories",
		Description: "Search memories by their content (requires the service's content-search index " +
			"to be enabled). By default this does not reinforce the matches; set reinforce=true to " +
			"recall them as well. Results are ordered by relevance.",
	}, b.searchMemories)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_memories",
		Description: "List memories filtered by group and significance range, ordered by significance " +
			"or timestamp, with paging. A read-only browse that does not reinforce anything - use " +
			"recall_memories when you actually retrieve a memory.",
	}, b.listMemories)

	mcp.AddTool(server, &mcp.Tool{
		Name: "create_event",
		Description: "Create an event: a named time span that memories can be grouped under. " +
			"Associate memories with the returned event id (via store_memory's event_id) to keep " +
			"related memories together and let them reinforce one another during consolidation.",
	}, b.createEvent)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_events",
		Description: "List events filtered by group and significance range, ordered by significance or timestamp, with paging.",
	}, b.listEvents)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_summarization_candidates",
		Description: "List events whose memories have accumulated and gone quiet long enough to be " +
			"worth condensing into a single summary. Identified by the most recent consolidation " +
			"cycle; empty unless the service is configured to scan for them.",
	}, b.getSummarizationCandidates)

	return server
}

// memoryView is the plain-struct projection of a contract.Memory returned to the MCP host. The
// generated proto message carries unexported bookkeeping fields that would produce a noisy,
// inaccurate JSON schema, so tools return this instead.
type memoryView struct {
	Id           string `json:"id"`
	Body         string `json:"body"`
	Significance int32  `json:"significance"`
	EventId      string `json:"event_id,omitempty"`
	Group        string `json:"group,omitempty"`
	TimeStamp    int64  `json:"time_stamp"`
	TimeRecalled int64  `json:"time_recalled,omitempty"`
	RecallCount  int32  `json:"recall_count"`
	IsSummary    bool   `json:"is_summary,omitempty"`
	IsBinary     bool   `json:"is_binary,omitempty"`
}

// eventView is the plain-struct projection of a contract.Event, mirroring memoryView.
type eventView struct {
	Id           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Significance int32  `json:"significance"`
	Group        string `json:"group,omitempty"`
	TimeStart    int64  `json:"time_start"`
	TimeEnd      int64  `json:"time_end,omitempty"`
}

func toMemoryView(in *contract.Memory) memoryView {

	return memoryView{
		Id:           in.GetId(),
		Body:         in.GetBody(),
		Significance: in.GetSignificance(),
		EventId:      in.GetEventId(),
		Group:        in.GetGroup(),
		TimeStamp:    in.GetTimeStamp(),
		TimeRecalled: in.GetTimeRecalled(),
		RecallCount:  in.GetRecallCount(),
		IsSummary:    in.GetIsSummary(),
		IsBinary:     in.GetIsBinary() == contract.Bool_TRUE,
	}
}

func toMemoryViews(in []*contract.Memory) []memoryView {
	out := make([]memoryView, 0, len(in))

	for _, v := range in {
		out = append(out, toMemoryView(v))
	}

	return out
}

func toEventView(in *contract.Event) eventView {

	return eventView{
		Id:           in.GetId(),
		Name:         in.GetName(),
		Description:  in.GetDescription(),
		Significance: in.GetSignificance(),
		Group:        in.GetGroup(),
		TimeStart:    in.GetTimeStart(),
		TimeEnd:      in.GetTimeEnd(),
	}
}

// --- store_memory ---

type storeMemoryInput struct {
	Body         string `json:"body" jsonschema:"the memory content to store (required, non-empty text)"`
	Significance int32  `json:"significance,omitempty" jsonschema:"how important the memory is; higher is more significant and survives longer; 0 leaves it unranked"`
	Group        string `json:"group,omitempty" jsonschema:"optional freeform grouping/context label (system, subsystem, owner, ...)"`
	EventId      string `json:"event_id,omitempty" jsonschema:"optional id of an event to associate this memory with"`
}

type storeMemoryOutput struct {
	Id       string `json:"id" jsonschema:"the stored memory's id; empty when the memory was rejected"`
	Rejected bool   `json:"rejected" jsonschema:"true when the memory was dropped for being below the service's minimum significance"`
}

func (b *bridge) storeMemory(ctx context.Context, _ *mcp.CallToolRequest, in storeMemoryInput) (*mcp.CallToolResult, storeMemoryOutput, error) {
	if in.Body == "" {

		return nil, storeMemoryOutput{}, fmt.Errorf("body is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.StoreMemory(callCtx, &contract.Memory{
		Body:         in.Body,
		Significance: in.Significance,
		Group:        in.Group,
		EventId:      in.EventId,
	})
	if err != nil {

		return nil, storeMemoryOutput{}, fmt.Errorf("StoreMemory failed: %w", err)
	}

	return nil, storeMemoryOutput{Id: res.GetId(), Rejected: res.GetRejected()}, nil
}

// --- recall_memories ---

type recallMemoriesInput struct {
	Ids []string `json:"ids" jsonschema:"ids of the memories to recall and reinforce (required, non-empty)"`
}

type memoriesOutput struct {
	Memories []memoryView `json:"memories"`
}

func (b *bridge) recallMemories(ctx context.Context, _ *mcp.CallToolRequest, in recallMemoriesInput) (*mcp.CallToolResult, memoriesOutput, error) {
	if len(in.Ids) == 0 {

		return nil, memoriesOutput{}, fmt.Errorf("ids is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.RecallMemories(callCtx, &contract.RecallMemoriesRequest{Ids: in.Ids})
	if err != nil {

		return nil, memoriesOutput{}, fmt.Errorf("RecallMemories failed: %w", err)
	}

	return nil, memoriesOutput{Memories: toMemoryViews(res.GetMemories())}, nil
}

// --- search_memories ---

type searchMemoriesInput struct {
	Query     string `json:"query" jsonschema:"the content to search for (required)"`
	Limit     int32  `json:"limit,omitempty" jsonschema:"maximum results; 0 selects the service default (10)"`
	Group     string `json:"group,omitempty" jsonschema:"optional: restrict matches to memories carrying this group label"`
	EventId   string `json:"event_id,omitempty" jsonschema:"optional: restrict matches to a single event"`
	Reinforce bool   `json:"reinforce,omitempty" jsonschema:"when true, recall (reinforce) the matched memories rather than merely fetching them"`
}

func (b *bridge) searchMemories(ctx context.Context, _ *mcp.CallToolRequest, in searchMemoriesInput) (*mcp.CallToolResult, memoriesOutput, error) {
	if in.Query == "" {

		return nil, memoriesOutput{}, fmt.Errorf("query is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.SearchMemories(callCtx, &contract.SearchMemoriesRequest{
		Query:     in.Query,
		Limit:     in.Limit,
		Group:     in.Group,
		EventId:   in.EventId,
		Reinforce: in.Reinforce,
	})
	if err != nil {

		return nil, memoriesOutput{}, fmt.Errorf("SearchMemories failed: %w", err)
	}

	return nil, memoriesOutput{Memories: toMemoryViews(res.GetMemories())}, nil
}

// --- list_memories ---

type listMemoriesInput struct {
	Group           string `json:"group,omitempty" jsonschema:"optional: restrict to memories carrying this group label"`
	SignificanceMin int32  `json:"significance_min,omitempty" jsonschema:"inclusive lower bound on significance; 0 means no bound"`
	SignificanceMax int32  `json:"significance_max,omitempty" jsonschema:"inclusive upper bound on significance; 0 means no bound"`
	OrderBy         string `json:"order_by,omitempty" jsonschema:"'significance' (the default) or 'timestamp'"`
	Limit           int32  `json:"limit,omitempty" jsonschema:"page size; 0 selects the service default (25), capped at 200"`
	Offset          int32  `json:"offset,omitempty" jsonschema:"rows to skip for paging"`
}

type memoriesPageOutput struct {
	Memories   []memoryView `json:"memories"`
	TotalCount int32        `json:"total_count" jsonschema:"memories matching the filter ignoring paging, for pagination"`
}

func (b *bridge) listMemories(ctx context.Context, _ *mcp.CallToolRequest, in listMemoriesInput) (*mcp.CallToolResult, memoriesPageOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetMemories(callCtx, &contract.GetMemoriesRequest{
		Group:           in.Group,
		SignificanceMin: in.SignificanceMin,
		SignificanceMax: in.SignificanceMax,
		OrderBy:         in.OrderBy,
		Limit:           in.Limit,
		Offset:          in.Offset,
	})
	if err != nil {

		return nil, memoriesPageOutput{}, fmt.Errorf("GetMemories failed: %w", err)
	}

	return nil, memoriesPageOutput{
		Memories:   toMemoryViews(res.GetMemories()),
		TotalCount: res.GetTotalCount(),
	}, nil
}

// --- create_event ---

type createEventInput struct {
	Name         string `json:"name" jsonschema:"the event's name (required, non-empty)"`
	Description  string `json:"description,omitempty" jsonschema:"optional longer description of the event"`
	Significance int32  `json:"significance,omitempty" jsonschema:"how important the event is; higher is more significant; 0 leaves it unranked"`
	Group        string `json:"group,omitempty" jsonschema:"optional freeform grouping/context label"`
}

type createEventOutput struct {
	Id       string `json:"id" jsonschema:"the stored event's id; empty when the event was rejected"`
	Rejected bool   `json:"rejected" jsonschema:"true when the event was dropped for being below the service's minimum significance"`
}

func (b *bridge) createEvent(ctx context.Context, _ *mcp.CallToolRequest, in createEventInput) (*mcp.CallToolResult, createEventOutput, error) {
	if in.Name == "" {

		return nil, createEventOutput{}, fmt.Errorf("name is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.StoreEvent(callCtx, &contract.Event{
		Name:         in.Name,
		Description:  in.Description,
		Significance: in.Significance,
		Group:        in.Group,
	})
	if err != nil {

		return nil, createEventOutput{}, fmt.Errorf("StoreEvent failed: %w", err)
	}

	return nil, createEventOutput{Id: res.GetId(), Rejected: res.GetRejected()}, nil
}

// --- list_events ---

type listEventsInput struct {
	Group           string `json:"group,omitempty" jsonschema:"optional: restrict to events carrying this group label"`
	SignificanceMin int32  `json:"significance_min,omitempty" jsonschema:"inclusive lower bound on significance; 0 means no bound"`
	SignificanceMax int32  `json:"significance_max,omitempty" jsonschema:"inclusive upper bound on significance; 0 means no bound"`
	OrderBy         string `json:"order_by,omitempty" jsonschema:"'significance' (the default) or 'timestamp'"`
	Limit           int32  `json:"limit,omitempty" jsonschema:"page size; 0 selects the service default (25), capped at 200"`
	Offset          int32  `json:"offset,omitempty" jsonschema:"rows to skip for paging"`
}

type eventsPageOutput struct {
	Events     []eventView `json:"events"`
	TotalCount int32       `json:"total_count" jsonschema:"events matching the filter ignoring paging, for pagination"`
}

func (b *bridge) listEvents(ctx context.Context, _ *mcp.CallToolRequest, in listEventsInput) (*mcp.CallToolResult, eventsPageOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetEvents(callCtx, &contract.GetEventsRequest{
		Group:           in.Group,
		SignificanceMin: in.SignificanceMin,
		SignificanceMax: in.SignificanceMax,
		OrderBy:         in.OrderBy,
		Limit:           in.Limit,
		Offset:          in.Offset,
	})
	if err != nil {

		return nil, eventsPageOutput{}, fmt.Errorf("GetEvents failed: %w", err)
	}

	out := make([]eventView, 0, len(res.GetEvents()))

	for _, v := range res.GetEvents() {
		out = append(out, toEventView(v))
	}

	return nil, eventsPageOutput{Events: out, TotalCount: res.GetTotalCount()}, nil
}

// --- get_summarization_candidates ---

type summarizationCandidateView struct {
	EventId     string `json:"event_id"`
	EventName   string `json:"event_name"`
	MemoryCount int32  `json:"memory_count"`
}

type summarizationCandidatesOutput struct {
	Candidates []summarizationCandidateView `json:"candidates"`
}

func (b *bridge) getSummarizationCandidates(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, summarizationCandidatesOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetSummarizationCandidates(callCtx, &contract.EmptyRequest{})
	if err != nil {

		return nil, summarizationCandidatesOutput{}, fmt.Errorf("GetSummarizationCandidates failed: %w", err)
	}

	out := make([]summarizationCandidateView, 0, len(res.GetCandidates()))

	for _, v := range res.GetCandidates() {
		out = append(out, summarizationCandidateView{
			EventId:     v.GetEventId(),
			EventName:   v.GetEventName(),
			MemoryCount: v.GetMemoryCount(),
		})
	}

	return nil, summarizationCandidatesOutput{Candidates: out}, nil
}
