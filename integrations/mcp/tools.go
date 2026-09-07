package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	UpdateMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	SearchMemories(ctx context.Context, in *contract.SearchMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	GetMemories(ctx context.Context, in *contract.GetMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error)
	EndEvent(ctx context.Context, in *contract.EndEventRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	GetEvents(ctx context.Context, in *contract.GetEventsRequest, opts ...grpc.CallOption) (*contract.GetEventsResponse, error)
	GetSummarisationCandidates(ctx context.Context, in *contract.EmptyRequest, opts ...grpc.CallOption) (*contract.GetSummarisationCandidatesResponse, error)
	LinkMemories(ctx context.Context, in *contract.LinkMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	UnlinkMemories(ctx context.Context, in *contract.UnlinkMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	GetMemoryLinks(ctx context.Context, in *contract.GetMemoryLinksRequest, opts ...grpc.CallOption) (*contract.GetLinksResponse, error)
	UpdateEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	GetEventById(ctx context.Context, in *contract.GetEventByIdRequest, opts ...grpc.CallOption) (*contract.GetEventResponse, error)
	LinkEvents(ctx context.Context, in *contract.LinkEventsRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	UnlinkEvents(ctx context.Context, in *contract.UnlinkEventsRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	GetEventLinks(ctx context.Context, in *contract.GetEventLinksRequest, opts ...grpc.CallOption) (*contract.GetLinksResponse, error)
	WhoAmI(ctx context.Context, in *contract.EmptyRequest, opts ...grpc.CallOption) (*contract.WhoAmIResponse, error)
	GetConsolidationStatus(ctx context.Context, in *contract.EmptyRequest, opts ...grpc.CallOption) (*contract.GetConsolidationStatusResponse, error)
	ExplainConsolidation(ctx context.Context, in *contract.ExplainConsolidationRequest, opts ...grpc.CallOption) (*contract.ExplainConsolidationResponse, error)
	GetSignificanceLevels(ctx context.Context, in *contract.GetSignificanceLevelsRequest, opts ...grpc.CallOption) (*contract.GetSignificanceLevelsResponse, error)
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

// newServer builds the MCP server and registers the curated tool set. The surface is the per-item
// memory-and-event operations an LLM needs to give, retrieve, revise, and forget memories -
// including update_memory and a by-id delete_memories, both writer-tier operations no broader than
// the store_memory already exposed here. The administrative, destructive, and bulk data-movement
// RPCs (Purge, Sleep, Export/Import/Transfer/Clear, event deletion/merge) are intentionally not
// exposed, so a model cannot wipe or exfiltrate a store through this bridge. What a given token may
// actually do is enforced by the service's role tiers (reader/writer/admin): a reader-scoped token
// is refused every mutation regardless of which tools are registered here.
//
// The three introspection tools at the end are the newest, and their inclusion follows the same
// rule rather than bending it. whoami is the FEATURE-DETECTION RPC and its absence was already
// costing this bridge: search_memories offers semantic and hybrid modes and could only discover
// that a deployment serves neither by having a search rejected, which is exactly what search_modes
// exists to prevent. explain_consolidation and consolidation_status are both reader tier and
// neither enumerates: the first answers only about ids the caller supplies and could already read
// in full through list_memories, and the second names no record at all. Between them they let an
// agent ask the one question this store exists to answer - is this memory about to go, and how long
// has it got - which none of the other twenty tools can.
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
		Name: "update_memory",
		Description: "Revise an existing memory by id: only the fields you set are changed (body, " +
			"significance, group, event_id, metadata) and omitted fields keep their stored values. " +
			"Significance 0 leaves the existing significance unchanged (it cannot reset a memory to " +
			"unranked), and a memory's binary/summary nature cannot be changed here. Metadata REPLACES " +
			"the stored labels wholesale rather than merging with them, and because an omitted field " +
			"means 'leave unchanged', removing labels or a group needs clear_metadata/clear_group. Use " +
			"this to correct or amend a memory you already stored rather than storing a duplicate. An " +
			"unknown id is reported as not found; no memory is created.",
	}, b.updateMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "delete_memories",
		Description: "Delete memories by id (unknown ids are silently ignored). This is a by-id " +
			"scalpel, not a bulk wipe: it can only remove memories you explicitly name. Use it to " +
			"forget something on demand rather than waiting for it to decay out of the store.",
	}, b.deleteMemories)

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
		Description: "List memories filtered by group, significance range, and metadata labels, with " +
			"paging. Ordered by timestamp (most recent first) unless order_by says otherwise. Set " +
			"recalled=false to find the memories that have never been recalled - the closest thing " +
			"to asking what is about to be forgotten. A read-only browse that does not reinforce " +
			"anything - use recall_memories when you actually retrieve a memory.",
	}, b.listMemories)

	mcp.AddTool(server, &mcp.Tool{
		Name: "link_memories",
		Description: "Associate one memory with others, each link carrying a significance. Links are " +
			"how memories remind the store of one another: a linked memory decays more slowly, and " +
			"both ends of a link gain from it. Both memories must already exist. Linking the same " +
			"pair again re-weights that link rather than adding a second one.",
	}, b.linkMemories)

	mcp.AddTool(server, &mcp.Tool{
		Name: "unlink_memories",
		Description: "Remove the links between one memory and the memories you name, in either " +
			"direction. Unknown targets are silently ignored. Use this when an association no longer " +
			"holds - the memories themselves are untouched.",
	}, b.unlinkMemories)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_memory_links",
		Description: "List what a memory is linked to, and its total link significance. By default " +
			"both directions are returned; narrow with direction=outbound (links this memory declared) " +
			"or inbound (links others declared to it). A read-only browse that reinforces nothing.",
	}, b.getMemoryLinks)

	mcp.AddTool(server, &mcp.Tool{
		Name: "create_event",
		Description: "Create an event: a named time span that memories can be grouped under. " +
			"Associate memories with the returned event id (via store_memory's event_id) to keep " +
			"related memories together and let them reinforce one another during consolidation.",
	}, b.createEvent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "end_event",
		Description: "Close an event by setting its end time, defaulting to now. An event that is " +
			"never ended stores an end time of 0, which sorts it as the oldest-ended rather than " +
			"the most recent, and leaves it looking open forever. Ending one does not delete or " +
			"change its memories.",
	}, b.endEvent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "update_event",
		Description: "Revise an existing event by id: only the fields you set are changed (name, " +
			"description, significance, group, metadata, start/end times) and omitted fields keep " +
			"their stored values. Metadata REPLACES the stored labels wholesale rather than merging, " +
			"and because an omitted field means 'leave unchanged', removing labels or a group needs " +
			"clear_metadata/clear_group. Use this to correct an event's name or enrich its labels " +
			"rather than creating a second event for the same span. An unknown id is reported as not " +
			"found; no event is created.",
	}, b.updateEvent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_event",
		Description: "Fetch a single event by id, with its memory count and optionally its links. " +
			"Use this when a memory names an event_id and you need what that event actually is - its " +
			"name, description and span. It does NOT return the event's memories: ask list_memories " +
			"with that event_id, which pages them instead of putting every body in one reply.",
	}, b.getEvent)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_events",
		Description: "List events filtered by group, significance range, name substring, open/ended " +
			"state and link neighbourhood, with paging. Ordered by timestamp (most recently started " +
			"first) unless order_by says otherwise. Set ended=false to find the events still running.",
	}, b.listEvents)

	mcp.AddTool(server, &mcp.Tool{
		Name: "link_events",
		Description: "Associate one event with others, each link carrying a significance. Event links " +
			"work exactly as memory links do and feed the same decay maths: an event's links raise " +
			"the effective significance of every memory under it, so linking two events slows both " +
			"sets of memories' decay. Both events must already exist.",
	}, b.linkEvents)

	mcp.AddTool(server, &mcp.Tool{
		Name: "unlink_events",
		Description: "Remove the links between one event and the events you name, in either " +
			"direction. Unknown targets are silently ignored. The events themselves are untouched.",
	}, b.unlinkEvents)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_event_links",
		Description: "List what an event is linked to, and its total link significance. By default " +
			"both directions are returned; narrow with direction=outbound or inbound. A read-only " +
			"browse that reinforces nothing.",
	}, b.getEventLinks)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_summarisation_candidates",
		Description: "List events whose memories have accumulated and gone quiet long enough to be " +
			"worth condensing into a single summary. Identified by the most recent consolidation " +
			"cycle; empty unless the service is configured to scan for them.",
	}, b.getSummarisationCandidates)

	mcp.AddTool(server, &mcp.Tool{
		Name: "significance_levels",
		Description: "List the significance values currently in use in this store, ascending. Read " +
			"this before choosing a significance for a new memory or event: it shows the scale this " +
			"store actually uses, so a new item can be ranked against what is already there instead " +
			"of against a number you invented. Two adjacent values have no room between them.",
	}, b.significanceLevels)

	mcp.AddTool(server, &mcp.Tool{
		Name: "explain_consolidation",
		Description: "Ask where specific memories stand against the forgetting rules: each one's " +
			"computed value, the threshold it is measured against, how many days since its decay " +
			"clock last reset, whether a cycle running now would forget it, and how many days it has " +
			"left (-1 means not due within the projected horizon). This is how you find out that a " +
			"memory is about to go - recall_memories on it resets its clock and keeps it. Reads only " +
			"the ids you name; at most 200 per call.",
	}, b.explainConsolidation)

	mcp.AddTool(server, &mcp.Tool{
		Name: "consolidation_status",
		Description: "Report whether this instance forgets anything at all, when the next cycle is " +
			"due, and what the last one removed. Names no individual memory. Use it to tell 'nothing " +
			"has been forgotten' apart from 'nothing is doing the forgetting' - a replica reports " +
			"consolidation_enabled=false and never runs a cycle.",
	}, b.consolidationStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name: "whoami",
		Description: "Report what this connection can do: the caller's role (reader/writer/admin) " +
			"and group scope, and what the deployment can serve - which search_memories modes work " +
			"here, whether the service summarises, whether it consolidates, and its version. Call " +
			"this before using semantic or hybrid search rather than discovering an unavailable mode " +
			"by having the search rejected.",
	}, b.whoAmI)

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

	Metadata map[string]string `json:"metadata,omitempty"`
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
	MemoryCount  int32  `json:"memory_count"`

	Metadata map[string]string `json:"metadata,omitempty"`
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
		Metadata:     in.GetMetadata(),
	}
}

// metadataFilterPairs packs a metadata filter map into the repeated "key=value" strings the RPC
// takes. The tools expose a map because that is the shape a model produces naturally and the shape
// metadata is written in; the RPC takes strings because its list routes are HTTP GETs, and a map
// cannot be bound from a URL query string.
func metadataFilterPairs(metadata map[string]string) []string {
	if len(metadata) == 0 {
		return nil
	}

	keys := make([]string, 0, len(metadata))

	for k := range metadata {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))

	for _, k := range keys {
		pairs = append(pairs, k+"="+metadata[k])
	}

	return pairs
}

// triStateFilter maps an optional bool onto the contract's tri-state, so a tool can express "only
// the false ones" - which a plain bool cannot, since omitting it and setting it to false would both
// arrive as false.
func triStateFilter(in *bool) contract.Bool {
	switch {

	case in == nil:
		return contract.Bool_UNSPECIFIED

	case *in:
		return contract.Bool_TRUE

	default:
		return contract.Bool_FALSE

	}
}

// sortDirection maps a listing tool's order_dir string onto the contract's enum. Anything other
// than the two names it documents is treated as unset rather than rejected: the direction only
// reorders a page a model already asked for, so failing the call over a typo costs it the answer
// while falling back to the sort field's natural direction still gives one.
func sortDirection(in string) contract.SortDirection {
	switch in {

	case "asc":
		return contract.SortDirection_SORT_DIRECTION_ASC

	case "desc":
		return contract.SortDirection_SORT_DIRECTION_DESC

	default:
		return contract.SortDirection_SORT_DIRECTION_UNSPECIFIED

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
		Metadata:     in.GetMetadata(),
		Description:  in.GetDescription(),
		Significance: in.GetSignificance(),
		Group:        in.GetGroup(),
		TimeStart:    in.GetTimeStart(),
		TimeEnd:      in.GetTimeEnd(),
		MemoryCount:  in.GetMemoryCount(),
	}
}

// --- store_memory ---

type storeMemoryInput struct {
	Body         string `json:"body" jsonschema:"the memory content to store (required, non-empty text)"`
	Significance int32  `json:"significance,omitempty" jsonschema:"how important the memory is; higher is more significant and survives longer; 0 leaves it unranked"`
	Group        string `json:"group,omitempty" jsonschema:"optional freeform grouping/context label (system, subsystem, owner, ...)"`
	EventId      string `json:"event_id,omitempty" jsonschema:"optional id of an event to associate this memory with"`

	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"optional key/value labels for multi-dimensional classification, e.g. {\"source\": \"slack\", \"project\": \"apollo\"}; keys are letters, digits and . _ : / - only; at most 32 pairs"`
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
		Metadata:     in.Metadata,
	})
	if err != nil {
		return nil, storeMemoryOutput{}, fmt.Errorf("StoreMemory failed: %w", err)
	}

	return nil, storeMemoryOutput{Id: res.GetId(), Rejected: res.GetRejected()}, nil
}

// --- update_memory ---

type updateMemoryInput struct {
	Id           string `json:"id" jsonschema:"id of the memory to update (required)"`
	Body         string `json:"body,omitempty" jsonschema:"new memory content; omit to leave the body unchanged"`
	Significance int32  `json:"significance,omitempty" jsonschema:"new significance; higher survives longer; 0 leaves the existing significance unchanged"`
	Group        string `json:"group,omitempty" jsonschema:"new grouping/context label; omit to leave the group unchanged"`
	EventId      string `json:"event_id,omitempty" jsonschema:"associate the memory with this event; omit to leave the association unchanged"`

	Metadata      map[string]string `json:"metadata,omitempty" jsonschema:"new key/value labels; REPLACES the stored set wholesale rather than merging, and omitting it leaves them unchanged - use clear_metadata to remove them"`
	ClearMetadata bool              `json:"clear_metadata,omitempty" jsonschema:"remove all of the memory's metadata; takes precedence over any metadata sent alongside it"`
	ClearGroup    bool              `json:"clear_group,omitempty" jsonschema:"reset the memory's group to empty; an empty group otherwise means leave unchanged"`
}

type updateMemoryOutput struct {
	Ok bool `json:"ok" jsonschema:"true when the update was applied"`
}

func (b *bridge) updateMemory(ctx context.Context, _ *mcp.CallToolRequest, in updateMemoryInput) (*mcp.CallToolResult, updateMemoryOutput, error) {
	if in.Id == "" {
		return nil, updateMemoryOutput{}, fmt.Errorf("id is required")
	}

	if in.Body == "" && in.Significance == 0 && in.Group == "" && in.EventId == "" &&
		len(in.Metadata) == 0 && !in.ClearMetadata && !in.ClearGroup {
		return nil, updateMemoryOutput{}, fmt.Errorf(
			"at least one field (body, significance, group, event_id, metadata, clear_metadata, clear_group) must be set to update",
		)
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.UpdateMemory(callCtx, &contract.Memory{
		Id:            in.Id,
		Body:          in.Body,
		Significance:  in.Significance,
		Group:         in.Group,
		EventId:       in.EventId,
		Metadata:      in.Metadata,
		ClearMetadata: in.ClearMetadata,
		ClearGroup:    in.ClearGroup,
	})
	if err != nil {
		return nil, updateMemoryOutput{}, fmt.Errorf("UpdateMemory failed: %w", err)
	}

	return nil, updateMemoryOutput{Ok: res.GetOk()}, nil
}

// --- delete_memories ---

type deleteMemoriesInput struct {
	Ids []string `json:"ids" jsonschema:"ids of the memories to delete (required, non-empty); unknown ids are silently ignored"`
}

type deleteMemoriesOutput struct {
	Ok bool `json:"ok" jsonschema:"true when the delete was applied"`
}

func (b *bridge) deleteMemories(ctx context.Context, _ *mcp.CallToolRequest, in deleteMemoriesInput) (*mcp.CallToolResult, deleteMemoriesOutput, error) {
	if len(in.Ids) == 0 {
		return nil, deleteMemoriesOutput{}, fmt.Errorf("ids is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.DeleteMemories(callCtx, &contract.DeleteMemoriesRequest{Ids: in.Ids})
	if err != nil {
		return nil, deleteMemoriesOutput{}, fmt.Errorf("DeleteMemories failed: %w", err)
	}

	return nil, deleteMemoriesOutput{Ok: res.GetOk()}, nil
}

// --- recall_memories ---

type recallMemoriesInput struct {
	Ids           []string `json:"ids" jsonschema:"ids of the memories to recall and reinforce (required, non-empty)"`
	IncludeLinked bool     `json:"include_linked,omitempty" jsonschema:"also return the memories linked to those recalled, one hop away. They are returned as an associative recall and are not themselves reinforced by it"`
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

	res, err := b.client.RecallMemories(callCtx, &contract.RecallMemoriesRequest{
		Ids:           in.Ids,
		IncludeLinked: in.IncludeLinked,
	})
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
	Mode      string `json:"mode,omitempty" jsonschema:"how to match: keyword (the default) matches the words in the body; semantic matches by meaning; hybrid does both and fuses them. semantic and hybrid need the service to have an embedding model and OpenSearch configured, and are rejected otherwise"`

	IncludeLinked bool `json:"include_linked,omitempty" jsonschema:"also return the memories linked to each match, one hop away, appended after the ranked results"`

	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"optional: restrict matches to memories carrying ALL of these key/value labels exactly"`
}

// searchModes maps the tool's mode string onto the RPC enum. An unknown value is an error rather
// than a silent fall back to keyword: a model that asked for meaning and got word matching would
// have no way to tell.
var searchModes = map[string]contract.SearchMode{
	"":         contract.SearchMode_SEARCH_MODE_UNSPECIFIED,
	"keyword":  contract.SearchMode_SEARCH_MODE_KEYWORD,
	"semantic": contract.SearchMode_SEARCH_MODE_SEMANTIC,
	"hybrid":   contract.SearchMode_SEARCH_MODE_HYBRID,
}

func (b *bridge) searchMemories(ctx context.Context, _ *mcp.CallToolRequest, in searchMemoriesInput) (*mcp.CallToolResult, memoriesOutput, error) {
	if in.Query == "" {
		return nil, memoriesOutput{}, fmt.Errorf("query is required")
	}

	mode, ok := searchModes[strings.ToLower(strings.TrimSpace(in.Mode))]
	if !ok {
		return nil, memoriesOutput{}, fmt.Errorf("unknown mode %q (expected keyword, semantic, or hybrid)", in.Mode)
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.SearchMemories(callCtx, &contract.SearchMemoriesRequest{
		Query:     in.Query,
		Limit:     in.Limit,
		Group:     in.Group,
		EventId:   in.EventId,
		Reinforce: in.Reinforce,
		Mode:      mode,

		IncludeLinked: in.IncludeLinked,
		Metadata:      metadataFilterPairs(in.Metadata),
	})
	if err != nil {
		return nil, memoriesOutput{}, fmt.Errorf("SearchMemories failed: %w", err)
	}

	return nil, memoriesOutput{Memories: toMemoryViews(res.GetMemories())}, nil
}

// --- list_memories ---

// The order_by descriptions below name the accepted values AND the service's default, both
// hand-copied from db/order.go rather than imported - this module depends on the root one for the
// contract alone, and importing db would pull all three storage drivers into a client binary, which
// is the same trade integrations/cli made for the same lists.
//
// The two halves of that copy are not equally forgiving. A stale VALUE costs a wrong suggestion, and
// the service rejects it. A stale DEFAULT costs a wrong page: a jsonschema description is what the
// model reads to decide whether to send order_by at all, so a model told the default is significance
// has no reason to send anything and is silently served timestamp order. When defaultMemoryOrderBy
// or defaultEventOrderBy moves in db/order.go, these strings must move with it.

type listMemoriesInput struct {
	Group           string `json:"group,omitempty" jsonschema:"optional: restrict to memories carrying this group label"`
	SignificanceMin int32  `json:"significance_min,omitempty" jsonschema:"inclusive lower bound on significance; 0 means no bound"`
	SignificanceMax int32  `json:"significance_max,omitempty" jsonschema:"inclusive upper bound on significance; 0 means no bound"`
	OrderBy         string `json:"order_by,omitempty" jsonschema:"sort field: 'timestamp' (the default), 'significance', 'time_recalled', 'recall_count', 'link_significance', 'group', or 'id'"`
	OrderDir        string `json:"order_dir,omitempty" jsonschema:"'asc' or 'desc'; omit to use the sort field's natural direction (descending for the magnitude and time fields, ascending for group and id)"`
	Limit           int32  `json:"limit,omitempty" jsonschema:"page size; 0 selects the service default (25), capped at 200"`
	Offset          int32  `json:"offset,omitempty" jsonschema:"rows to skip for paging"`

	StoredAfter  string `json:"stored_after,omitempty" jsonschema:"optional: only memories stored at or after this RFC3339 timestamp (e.g. 2026-08-12T00:00:00Z)"`
	StoredBefore string `json:"stored_before,omitempty" jsonschema:"optional: only memories stored at or before this RFC3339 timestamp"`

	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"optional: restrict to memories carrying ALL of these key/value labels exactly"`
	Recalled *bool             `json:"recalled,omitempty" jsonschema:"optional: false returns only memories that have never been recalled, true only those recalled at least once; omit for no restriction"`
	EventId  string            `json:"event_id,omitempty" jsonschema:"optional: restrict to one event's memories; this is the paged way to read them, and an empty value applies no restriction rather than matching the event-less"`
	HasEvent *bool             `json:"has_event,omitempty" jsonschema:"optional: false returns only memories belonging to no event, true only those belonging to one; omit for no restriction"`
}

type memoriesPageOutput struct {
	Memories   []memoryView `json:"memories"`
	TotalCount int32        `json:"total_count" jsonschema:"memories matching the filter ignoring paging, for pagination"`
}

func (b *bridge) listMemories(ctx context.Context, _ *mcp.CallToolRequest, in listMemoriesInput) (*mcp.CallToolResult, memoriesPageOutput, error) {
	storedAfter, err := parseToolTime(in.StoredAfter, "stored_after")
	if err != nil {
		return nil, memoriesPageOutput{}, err
	}

	storedBefore, err := parseToolTime(in.StoredBefore, "stored_before")
	if err != nil {
		return nil, memoriesPageOutput{}, err
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetMemories(callCtx, &contract.GetMemoriesRequest{
		TimestampMin:    storedAfter,
		TimestampMax:    storedBefore,
		Group:           in.Group,
		SignificanceMin: in.SignificanceMin,
		SignificanceMax: in.SignificanceMax,
		OrderBy:         in.OrderBy,
		OrderDir:        sortDirection(in.OrderDir),
		Limit:           in.Limit,
		Offset:          in.Offset,
		Metadata:        metadataFilterPairs(in.Metadata),
		Recalled:        triStateFilter(in.Recalled),
		EventId:         in.EventId,
		HasEvent:        triStateFilter(in.HasEvent),
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

	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"optional key/value labels for multi-dimensional classification; keys are letters, digits and . _ : / - only; at most 32 pairs"`
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
		Metadata:     in.Metadata,
	})
	if err != nil {
		return nil, createEventOutput{}, fmt.Errorf("StoreEvent failed: %w", err)
	}

	return nil, createEventOutput{Id: res.GetId(), Rejected: res.GetRejected()}, nil
}

// --- update_event ---

type updateEventInput struct {
	Id           string `json:"id" jsonschema:"id of the event to update (required); an unknown id is reported as not found"`
	Name         string `json:"name,omitempty" jsonschema:"new name; omit to leave it unchanged"`
	Description  string `json:"description,omitempty" jsonschema:"new description; omit to leave it unchanged"`
	Significance int32  `json:"significance,omitempty" jsonschema:"new significance; 0 leaves the existing significance unchanged"`
	Group        string `json:"group,omitempty" jsonschema:"new group label; omit to leave it unchanged, or set clear_group to remove it"`
	TimeStart    string `json:"time_start,omitempty" jsonschema:"new start time as an RFC3339 timestamp (e.g. 2026-08-19T14:30:00Z); omit to leave it unchanged"`
	TimeEnd      string `json:"time_end,omitempty" jsonschema:"new end time as an RFC3339 timestamp; omit to leave it unchanged (use end_event to close an event at the current time)"`

	Metadata      map[string]string `json:"metadata,omitempty" jsonschema:"replacement labels; a non-empty map REPLACES the stored labels wholesale rather than merging, and an empty or omitted map leaves them unchanged"`
	ClearMetadata bool              `json:"clear_metadata,omitempty" jsonschema:"remove all of the event's metadata; needed because an empty metadata map means 'leave unchanged'"`
	ClearGroup    bool              `json:"clear_group,omitempty" jsonschema:"reset the event's group to empty; needed because an empty group means 'leave unchanged'"`
}

func (b *bridge) updateEvent(ctx context.Context, _ *mcp.CallToolRequest, in updateEventInput) (*mcp.CallToolResult, okOutput, error) {
	if in.Id == "" {
		return nil, okOutput{}, fmt.Errorf("id is required")
	}

	timeStart, err := parseToolTime(in.TimeStart, "time_start")
	if err != nil {
		return nil, okOutput{}, err
	}

	timeEnd, err := parseToolTime(in.TimeEnd, "time_end")
	if err != nil {
		return nil, okOutput{}, err
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.UpdateEvent(callCtx, &contract.Event{
		Id:            in.Id,
		Name:          in.Name,
		Description:   in.Description,
		Significance:  in.Significance,
		Group:         in.Group,
		TimeStart:     timeStart,
		TimeEnd:       timeEnd,
		Metadata:      in.Metadata,
		ClearMetadata: in.ClearMetadata,
		ClearGroup:    in.ClearGroup,
	})
	if err != nil {
		return nil, okOutput{}, fmt.Errorf("UpdateEvent failed: %w", err)
	}

	return nil, okOutput{Ok: res.GetOk()}, nil
}

// --- get_event ---

type getEventInput struct {
	Id    string `json:"id" jsonschema:"id of the event to fetch (required)"`
	Links bool   `json:"links,omitempty" jsonschema:"also return the links this event declared to other events"`
}

// getEventOutput carries the event plus, optionally, its links. The memories are deliberately NOT
// available here: GetEventById can return them all in one message, which is the shape that puts
// every body of a large event into a model's context at once. list_memories with event_id pages
// them instead.
type getEventOutput struct {
	Event eventView      `json:"event"`
	Links []linkEdgeView `json:"links,omitempty"`
}

func (b *bridge) getEvent(ctx context.Context, _ *mcp.CallToolRequest, in getEventInput) (*mcp.CallToolResult, getEventOutput, error) {
	if in.Id == "" {
		return nil, getEventOutput{}, fmt.Errorf("id is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetEventById(callCtx, &contract.GetEventByIdRequest{
		Id:    in.Id,
		Links: in.Links,

		// Asked for unconditionally, for the reason list_events asks for it: how much an event holds
		// is most of what decides whether a model should open it, and the count reads no bodies.
		MemoryCounts: true,
	})
	if err != nil {
		return nil, getEventOutput{}, fmt.Errorf("GetEventById failed: %w", err)
	}

	out := getEventOutput{Event: toEventView(res.GetEvent())}

	for _, l := range res.GetEvent().GetLinks() {
		out.Links = append(out.Links, linkEdgeView{
			Id:           l.GetId(),
			Significance: l.GetSignificance(),

			// Event.links carries the OUTBOUND edges only (see the contract), so there is nothing to
			// read a direction from - get_event_links is what answers both directions.
			Direction: "outbound",
		})
	}

	return nil, out, nil
}

// --- end_event ---

type endEventInput struct {
	Id      string `json:"id" jsonschema:"id of the event to close (required)"`
	TimeEnd string `json:"time_end,omitempty" jsonschema:"when the event ended, as an RFC3339 timestamp (e.g. 2026-08-19T14:30:00Z); omit to use the service's current time"`
}

type endEventOutput struct {
	Ok bool `json:"ok" jsonschema:"true when the event was ended"`
}

func (b *bridge) endEvent(ctx context.Context, _ *mcp.CallToolRequest, in endEventInput) (*mcp.CallToolResult, endEventOutput, error) {
	if in.Id == "" {
		return nil, endEventOutput{}, fmt.Errorf("id is required")
	}

	timeEnd, err := parseToolTime(in.TimeEnd, "time_end")
	if err != nil {
		return nil, endEventOutput{}, err
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.EndEvent(callCtx, &contract.EndEventRequest{Id: in.Id, TimeEnd: timeEnd})
	if err != nil {
		return nil, endEventOutput{}, fmt.Errorf("EndEvent failed: %w", err)
	}

	return nil, endEventOutput{Ok: res.GetOk()}, nil
}

// parseToolTime turns one of the RFC3339 timestamps the tool schemas accept into the UnixNano the
// RPCs take. The tools deliberately speak RFC3339 rather than nanoseconds: a model can write a date
// but cannot reliably arrive at 1755561600000000000, and a filter bound silently wrong by three
// orders of magnitude returns a plausible-looking empty page rather than an error. An empty value
// means "no bound", which is what 0 means to every one of these fields.
func parseToolTime(value string, field string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an RFC3339 timestamp such as 2026-08-19T14:30:00Z: %w", field, err)
	}

	return parsed.UnixNano(), nil
}

// --- list_events ---

// order_by's values and default are hand-copied from db/order.go - see listMemoriesInput.
type listEventsInput struct {
	Group           string `json:"group,omitempty" jsonschema:"optional: restrict to events carrying this group label"`
	SignificanceMin int32  `json:"significance_min,omitempty" jsonschema:"inclusive lower bound on significance; 0 means no bound"`
	SignificanceMax int32  `json:"significance_max,omitempty" jsonschema:"inclusive upper bound on significance; 0 means no bound"`
	OrderBy         string `json:"order_by,omitempty" jsonschema:"sort field: 'timestamp' (the default, the event's start), 'significance', 'time_end', 'name', 'link_significance', 'group', or 'id'"`
	OrderDir        string `json:"order_dir,omitempty" jsonschema:"'asc' or 'desc'; omit to use the sort field's natural direction (descending for the magnitude and time fields, ascending for name, group and id)"`
	Limit           int32  `json:"limit,omitempty" jsonschema:"page size; 0 selects the service default (25), capped at 200"`
	Offset          int32  `json:"offset,omitempty" jsonschema:"rows to skip for paging"`

	StartedAfter  string `json:"started_after,omitempty" jsonschema:"optional: only events starting at or after this RFC3339 timestamp (e.g. 2026-08-12T00:00:00Z)"`
	StartedBefore string `json:"started_before,omitempty" jsonschema:"optional: only events starting at or before this RFC3339 timestamp"`
	EndedAfter    string `json:"ended_after,omitempty" jsonschema:"optional: only events that have ENDED, at or after this RFC3339 timestamp; an event still open never matches either end bound"`
	EndedBefore   string `json:"ended_before,omitempty" jsonschema:"optional: only events that have ENDED, at or before this RFC3339 timestamp; use ended=false to ask about the ones still open"`

	Ended        *bool  `json:"ended,omitempty" jsonschema:"optional: true for events that have ended, false for those still running; omit for both. This is the only way to ask for open events - they store an end time of 0, which the two bounds above read as no bound"`
	NameContains string `json:"name_contains,omitempty" jsonschema:"optional: only events whose name contains this text, matched case-insensitively. A substring match, not a content search - event names and descriptions are in no search index"`
	LinkedTo     string `json:"linked_to,omitempty" jsonschema:"optional: only the events one hop from this event id, in either direction. Errors if that event does not exist"`

	Metadata map[string]string `json:"metadata,omitempty" jsonschema:"optional: restrict to events carrying ALL of these key/value labels exactly"`
}

type eventsPageOutput struct {
	Events     []eventView `json:"events"`
	TotalCount int32       `json:"total_count" jsonschema:"events matching the filter ignoring paging, for pagination"`
}

func (b *bridge) listEvents(ctx context.Context, _ *mcp.CallToolRequest, in listEventsInput) (*mcp.CallToolResult, eventsPageOutput, error) {
	bounds := map[string]int64{}

	for field, value := range map[string]string{
		"started_after":  in.StartedAfter,
		"started_before": in.StartedBefore,
		"ended_after":    in.EndedAfter,
		"ended_before":   in.EndedBefore,
	} {
		parsed, err := parseToolTime(value, field)
		if err != nil {
			return nil, eventsPageOutput{}, err
		}

		bounds[field] = parsed
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetEvents(callCtx, &contract.GetEventsRequest{
		TimeStartMin:    bounds["started_after"],
		TimeStartMax:    bounds["started_before"],
		TimeEndMin:      bounds["ended_after"],
		TimeEndMax:      bounds["ended_before"],
		Group:           in.Group,
		SignificanceMin: in.SignificanceMin,
		SignificanceMax: in.SignificanceMax,
		OrderBy:         in.OrderBy,
		OrderDir:        sortDirection(in.OrderDir),
		Limit:           in.Limit,
		Offset:          in.Offset,
		Metadata:        metadataFilterPairs(in.Metadata),
		Ended:           triStateFilter(in.Ended),
		NameContains:    in.NameContains,
		LinkedTo:        in.LinkedTo,

		// Always asked for, never exposed as an input: how much an event holds is most of what
		// decides whether a model should open it, and the count costs one aggregate that reads no
		// bodies. `memories` is deliberately still not requested - that would put every body of
		// every event on the page into the model's context.
		MemoryCounts: true,
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

// --- get_summarisation_candidates ---

type summarisationCandidateView struct {
	EventId     string `json:"event_id"`
	EventName   string `json:"event_name"`
	MemoryCount int32  `json:"memory_count"`
}

type summarisationCandidatesOutput struct {
	Candidates []summarisationCandidateView `json:"candidates"`

	// Reported so a model can tell "nothing is due yet" from "this service will never offer
	// anything", which an empty list alone cannot say - and which decide whether asking again later
	// is worth doing.
	ScanEnabled bool `json:"scan_enabled" jsonschema:"false when this service does not scan for candidates at all, in which case an empty list is permanent and asking again will not help"`
}

func (b *bridge) getSummarisationCandidates(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, summarisationCandidatesOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetSummarisationCandidates(callCtx, &contract.EmptyRequest{})
	if err != nil {
		return nil, summarisationCandidatesOutput{}, fmt.Errorf("GetSummarisationCandidates failed: %w", err)
	}

	out := make([]summarisationCandidateView, 0, len(res.GetCandidates()))

	for _, v := range res.GetCandidates() {
		out = append(out, summarisationCandidateView{
			EventId:     v.GetEventId(),
			EventName:   v.GetEventName(),
			MemoryCount: v.GetMemoryCount(),
		})
	}

	return nil, summarisationCandidatesOutput{Candidates: out, ScanEnabled: res.GetScanEnabled()}, nil
}

// okOutput is the plain acknowledgement the link mutations return: they change the graph rather
// than producing anything, so there is nothing else worth reporting.
type okOutput struct {
	Ok bool `json:"ok" jsonschema:"true when the change was applied"`
}

// --- link_memories / unlink_memories / get_memory_links ---
//
// Only the memory half of the link surface is exposed. Event links are an operator concern - the
// bridge already omits the event delete/merge RPCs for the same reason - and a model given both
// graphs would have to reason about which one it was editing on every call.

type linkMemoriesInput struct {
	Id    string          `json:"id" jsonschema:"the memory the links start from (required); it must already exist"`
	Links []linkViewInput `json:"links" jsonschema:"the memories to link to and how strongly (required); each target must already exist"`
}

type linkViewInput struct {
	Id           string `json:"id" jsonschema:"the memory to link to (required)"`
	Significance int32  `json:"significance" jsonschema:"how strong the association is, 0 to 1000000; higher slows both memories' decay more"`
}

// linkTargets validates a link tool's input and converts it to the wire type. Memories and events
// take the identical shape - the two link surfaces differ only in which RPC they call - so the
// checking lives here rather than twice.
func linkTargets(id string, in []linkViewInput) ([]*contract.Link, error) {
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}

	if len(in) == 0 {
		return nil, fmt.Errorf("links is required")
	}

	links := make([]*contract.Link, 0, len(in))

	for _, l := range in {
		if l.Id == "" {
			return nil, fmt.Errorf("every link needs an id")
		}

		links = append(links, &contract.Link{Id: l.Id, Significance: l.Significance})
	}

	return links, nil
}

// toLinksOutput projects a GetLinks response, shared by get_memory_links and get_event_links.
func toLinksOutput(res *contract.GetLinksResponse) linksOutput {
	links := make([]linkEdgeView, 0, len(res.GetLinks()))

	for _, edge := range res.GetLinks() {
		links = append(links, linkEdgeView{
			Id:           edge.GetId(),
			Significance: edge.GetSignificance(),
			Direction:    linkDirectionName(edge.GetDirection()),
			Created:      edge.GetCreated(),
		})
	}

	return linksOutput{Links: links, LinkSignificance: res.GetLinkSignificance()}
}

func (b *bridge) linkMemories(ctx context.Context, _ *mcp.CallToolRequest, in linkMemoriesInput) (*mcp.CallToolResult, okOutput, error) {
	links, err := linkTargets(in.Id, in.Links)
	if err != nil {
		return nil, okOutput{}, err
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.LinkMemories(callCtx, &contract.LinkMemoriesRequest{Id: in.Id, Links: links})
	if err != nil {
		return nil, okOutput{}, fmt.Errorf("LinkMemories failed: %w", err)
	}

	return nil, okOutput{Ok: res.GetOk()}, nil
}

type unlinkMemoriesInput struct {
	Id  string   `json:"id" jsonschema:"the memory the links start from (required)"`
	Ids []string `json:"ids" jsonschema:"the memories to unlink from it (required); unknown ids are ignored"`
}

func (b *bridge) unlinkMemories(ctx context.Context, _ *mcp.CallToolRequest, in unlinkMemoriesInput) (*mcp.CallToolResult, okOutput, error) {
	if in.Id == "" {
		return nil, okOutput{}, fmt.Errorf("id is required")
	}

	if len(in.Ids) == 0 {
		return nil, okOutput{}, fmt.Errorf("ids is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.UnlinkMemories(callCtx, &contract.UnlinkMemoriesRequest{Id: in.Id, Ids: in.Ids})
	if err != nil {
		return nil, okOutput{}, fmt.Errorf("UnlinkMemories failed: %w", err)
	}

	return nil, okOutput{Ok: res.GetOk()}, nil
}

type getMemoryLinksInput struct {
	Id        string `json:"id" jsonschema:"the memory whose links to list (required)"`
	Direction string `json:"direction,omitempty" jsonschema:"which links to return: both (the default), outbound (links this memory declared), or inbound (links others declared to it)"`
}

// linkDirections maps the tool's direction string onto the RPC enum. An unknown value is an error
// rather than a silent fall back to both: a caller that asked for one direction and got the other
// would have no way to tell.
var linkDirections = map[string]contract.LinkDirection{
	"":         contract.LinkDirection_LINK_DIRECTION_BOTH,
	"both":     contract.LinkDirection_LINK_DIRECTION_BOTH,
	"outbound": contract.LinkDirection_LINK_DIRECTION_OUTBOUND,
	"inbound":  contract.LinkDirection_LINK_DIRECTION_INBOUND,
}

// linkEdgeView is the plain-struct projection of a contract.LinkEdge, for the same reason
// memoryView exists.
type linkEdgeView struct {
	Id           string `json:"id"`
	Significance int32  `json:"significance"`
	Direction    string `json:"direction"`
	Created      int64  `json:"created"`
}

type linksOutput struct {
	Links            []linkEdgeView `json:"links"`
	LinkSignificance int64          `json:"link_significance"`
}

func (b *bridge) getMemoryLinks(ctx context.Context, _ *mcp.CallToolRequest, in getMemoryLinksInput) (*mcp.CallToolResult, linksOutput, error) {
	if in.Id == "" {
		return nil, linksOutput{}, fmt.Errorf("id is required")
	}

	direction, ok := linkDirections[strings.ToLower(in.Direction)]
	if !ok {
		return nil, linksOutput{}, fmt.Errorf("unknown direction %q (want both, outbound or inbound)", in.Direction)
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetMemoryLinks(callCtx, &contract.GetMemoryLinksRequest{Id: in.Id, Direction: direction})
	if err != nil {
		return nil, linksOutput{}, fmt.Errorf("GetMemoryLinks failed: %w", err)
	}

	return nil, toLinksOutput(res), nil
}

// --- link_events / unlink_events / get_event_links ---
//
// The event half of the graph, which the bridge carried for memories only. The asymmetry was worth
// closing for the reason event links exist at all: an event's links raise the effective
// significance of every memory under it, so this is the coarser of the two levers a model has over
// what the store keeps.

type linkEventsInput struct {
	Id    string          `json:"id" jsonschema:"the event the links start from (required); it must already exist"`
	Links []linkViewInput `json:"links" jsonschema:"the events to link to and how strongly (required); each target must already exist"`
}

func (b *bridge) linkEvents(ctx context.Context, _ *mcp.CallToolRequest, in linkEventsInput) (*mcp.CallToolResult, okOutput, error) {
	links, err := linkTargets(in.Id, in.Links)
	if err != nil {
		return nil, okOutput{}, err
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.LinkEvents(callCtx, &contract.LinkEventsRequest{Id: in.Id, Links: links})
	if err != nil {
		return nil, okOutput{}, fmt.Errorf("LinkEvents failed: %w", err)
	}

	return nil, okOutput{Ok: res.GetOk()}, nil
}

type unlinkEventsInput struct {
	Id  string   `json:"id" jsonschema:"the event the links start from (required)"`
	Ids []string `json:"ids" jsonschema:"the events to unlink from it (required); unknown ids are ignored"`
}

func (b *bridge) unlinkEvents(ctx context.Context, _ *mcp.CallToolRequest, in unlinkEventsInput) (*mcp.CallToolResult, okOutput, error) {
	if in.Id == "" {
		return nil, okOutput{}, fmt.Errorf("id is required")
	}

	if len(in.Ids) == 0 {
		return nil, okOutput{}, fmt.Errorf("ids is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.UnlinkEvents(callCtx, &contract.UnlinkEventsRequest{Id: in.Id, Ids: in.Ids})
	if err != nil {
		return nil, okOutput{}, fmt.Errorf("UnlinkEvents failed: %w", err)
	}

	return nil, okOutput{Ok: res.GetOk()}, nil
}

type getEventLinksInput struct {
	Id        string `json:"id" jsonschema:"the event whose links to list (required)"`
	Direction string `json:"direction,omitempty" jsonschema:"which links to return: both (the default), outbound (links this event declared), or inbound (links others declared to it)"`
}

func (b *bridge) getEventLinks(ctx context.Context, _ *mcp.CallToolRequest, in getEventLinksInput) (*mcp.CallToolResult, linksOutput, error) {
	if in.Id == "" {
		return nil, linksOutput{}, fmt.Errorf("id is required")
	}

	direction, ok := linkDirections[strings.ToLower(in.Direction)]
	if !ok {
		return nil, linksOutput{}, fmt.Errorf("unknown direction %q (want both, outbound or inbound)", in.Direction)
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetEventLinks(callCtx, &contract.GetEventLinksRequest{Id: in.Id, Direction: direction})
	if err != nil {
		return nil, linksOutput{}, fmt.Errorf("GetEventLinks failed: %w", err)
	}

	return nil, toLinksOutput(res), nil
}

func linkDirectionName(d contract.LinkDirection) string {
	switch d {

	case contract.LinkDirection_LINK_DIRECTION_OUTBOUND:
		return "outbound"

	case contract.LinkDirection_LINK_DIRECTION_INBOUND:
		return "inbound"

	default:
		return "both"

	}
}

// --- significance_levels ---
//
// The scale, not the records ranked on it. A model choosing a significance for a new memory was
// otherwise inventing a number against a store whose scheme it could not see, which is the same
// problem SignificancePlacement has and the reason this RPC exists.

type significanceLevelsInput struct {
	SignificanceMin int32 `json:"significance_min,omitempty" jsonschema:"optional inclusive lower bound; 0 means no bound"`
	SignificanceMax int32 `json:"significance_max,omitempty" jsonschema:"optional inclusive upper bound; 0 means no bound"`
	Limit           int32 `json:"limit,omitempty" jsonschema:"page size; 0 selects the service default (200), capped at 1000"`
	Offset          int32 `json:"offset,omitempty" jsonschema:"values to skip for paging"`
}

type significanceLevelsOutput struct {
	Significances []int32 `json:"significances" jsonschema:"the distinct significance values in use, ascending; two adjacent values have no room between them"`
	TotalCount    int32   `json:"total_count" jsonschema:"values matching the bounds, ignoring paging"`
}

func (b *bridge) significanceLevels(ctx context.Context, _ *mcp.CallToolRequest, in significanceLevelsInput) (*mcp.CallToolResult, significanceLevelsOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetSignificanceLevels(callCtx, &contract.GetSignificanceLevelsRequest{
		SignificanceMin: in.SignificanceMin,
		SignificanceMax: in.SignificanceMax,
		Limit:           in.Limit,
		Offset:          in.Offset,
	})
	if err != nil {
		return nil, significanceLevelsOutput{}, fmt.Errorf("GetSignificanceLevels failed: %w", err)
	}

	return nil, significanceLevelsOutput{
		Significances: res.GetSignificances(),
		TotalCount:    res.GetTotalCount(),
	}, nil
}

// --- explain_consolidation ---

type explainConsolidationInput struct {
	Ids []string `json:"ids" jsonschema:"the memories to value (required); at most 200 per call"`
}

// memoryValuationView is the projection of one memory's standing. It carries the fields an agent
// can act on and drops the decomposition of effective significance (the four link terms), which
// explains a number rather than changing what to do about it - the console renders those, a model
// does not need them in its context.
type memoryValuationView struct {
	Id                    string  `json:"id"`
	EventId               string  `json:"event_id,omitempty"`
	Significance          int32   `json:"significance" jsonschema:"the memory's own stored significance"`
	EffectiveSignificance float64 `json:"effective_significance" jsonschema:"what the decay acts on: the memory's significance plus its event's, its links' damped contribution, and its recall count"`
	Value                 float64 `json:"value" jsonschema:"the computed decayed value"`
	Threshold             float64 `json:"threshold" jsonschema:"the threshold value is compared against, already scaled by how full the store is"`
	AgeDays               float64 `json:"age_days" jsonschema:"days since the decay clock last reset - creation, or the most recent recall"`
	RecallCount           int32   `json:"recall_count"`
	WouldConsolidate      bool    `json:"would_consolidate" jsonschema:"a cycle running now would forget this memory"`
	Retained              bool    `json:"retained" jsonschema:"inside the store's hard retention window, so nothing may take it whatever its value"`
	BelowMinimumAge       bool    `json:"below_minimum_age" jsonschema:"too young for value-based consolidation, whatever its value"`
	DaysUntilForgotten    float64 `json:"days_until_forgotten" jsonschema:"projected days left, holding today's threshold and assuming no further recall; 0 means already due, -1 means not due within the projected horizon"`
}

type explainConsolidationOutput struct {
	Valuations []memoryValuationView `json:"valuations"`

	DeletionThreshold float64 `json:"deletion_threshold" jsonschema:"the threshold in force, scaled by capacity pressure"`
	CapacityPressure  float64 `json:"capacity_pressure" jsonschema:"multiplier applied to the threshold by how full the store is; 1.0 means no effect"`
	MemoryCount       int32   `json:"memory_count" jsonschema:"memories currently stored"`
}

func (b *bridge) explainConsolidation(ctx context.Context, _ *mcp.CallToolRequest, in explainConsolidationInput) (*mcp.CallToolResult, explainConsolidationOutput, error) {
	if len(in.Ids) == 0 {
		return nil, explainConsolidationOutput{}, fmt.Errorf("ids is required")
	}

	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	// The curve is deliberately not requested. It describes the CONFIGURATION rather than any
	// memory, and it is up to 500 points of it - a shape to draw, which is the console's job, not
	// something an agent acts on.
	res, err := b.client.ExplainConsolidation(callCtx, &contract.ExplainConsolidationRequest{MemoryIds: in.Ids})
	if err != nil {
		return nil, explainConsolidationOutput{}, fmt.Errorf("ExplainConsolidation failed: %w", err)
	}

	out := explainConsolidationOutput{
		DeletionThreshold: res.GetDeletionThreshold(),
		CapacityPressure:  res.GetCapacityPressure(),
		MemoryCount:       res.GetMemoryCount(),
		Valuations:        make([]memoryValuationView, 0, len(res.GetValuations())),
	}

	for _, v := range res.GetValuations() {
		out.Valuations = append(out.Valuations, memoryValuationView{
			Id:                    v.GetId(),
			EventId:               v.GetEventId(),
			Significance:          v.GetSignificance(),
			EffectiveSignificance: v.GetEffectiveSignificance(),
			Value:                 v.GetValue(),
			Threshold:             v.GetThreshold(),
			AgeDays:               v.GetAgeDays(),
			RecallCount:           v.GetRecallCount(),
			WouldConsolidate:      v.GetWouldConsolidate(),
			Retained:              v.GetRetained(),
			BelowMinimumAge:       v.GetBelowMinimumAge(),
			DaysUntilForgotten:    v.GetDaysUntilForgotten(),
		})
	}

	return nil, out, nil
}

// --- consolidation_status ---

type consolidationStatusOutput struct {
	ConsolidationEnabled bool  `json:"consolidation_enabled" jsonschema:"false on a replica, which never runs a cycle - its store is consolidated by another instance"`
	PeriodSeconds        int64 `json:"period_seconds" jsonschema:"how often a cycle runs; 0 means no timed cycle at all"`
	NextSleepAt          int64 `json:"next_sleep_at" jsonschema:"UnixNano the next timed cycle is due; 0 when none is scheduled"`
	SleepInProgress      bool  `json:"sleep_in_progress"`

	LastCycleAt           int64 `json:"last_cycle_at,omitempty" jsonschema:"UnixNano the last completed cycle began; 0 if none has run in this process"`
	MemoriesConsolidated  int32 `json:"memories_consolidated" jsonschema:"memories the last cycle forgot for low value"`
	MemoriesEvicted       int32 `json:"memories_evicted" jsonschema:"memories the last cycle forgot to stay under its capacity target"`
	EventsConsolidated    int32 `json:"events_consolidated"`
	LastCycleSucceeded    bool  `json:"last_cycle_succeeded"`
	SummarisationCandidat int32 `json:"summarisation_candidates" jsonschema:"events the last cycle flagged as worth condensing"`
}

func (b *bridge) consolidationStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, consolidationStatusOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.GetConsolidationStatus(callCtx, &contract.EmptyRequest{})
	if err != nil {
		return nil, consolidationStatusOutput{}, fmt.Errorf("GetConsolidationStatus failed: %w", err)
	}

	last := res.GetLastCycle()

	return nil, consolidationStatusOutput{
		ConsolidationEnabled:  res.GetConsolidationEnabled(),
		PeriodSeconds:         res.GetPeriodSeconds(),
		NextSleepAt:           res.GetNextSleepAt(),
		SleepInProgress:       res.GetSleepInProgress(),
		LastCycleAt:           last.GetStartedAt(),
		MemoriesConsolidated:  last.GetMemoriesConsolidated(),
		MemoriesEvicted:       last.GetMemoriesEvicted(),
		EventsConsolidated:    last.GetEventsConsolidated(),
		LastCycleSucceeded:    last.GetSuccess(),
		SummarisationCandidat: last.GetSummarisationCandidates(),
	}, nil
}

// --- whoami ---

type whoAmIOutput struct {
	Role        string   `json:"role" jsonschema:"the caller's effective tier: reader, writer or admin"`
	ClientId    string   `json:"client_id,omitempty"`
	AuthEnabled bool     `json:"auth_enabled"`
	Groups      []string `json:"groups,omitempty" jsonschema:"the group labels this token may reach"`
	GroupScoped bool     `json:"group_scoped" jsonschema:"true when a scope is in force; read THIS rather than whether groups is empty, since empty means the whole store"`

	Version              string   `json:"version,omitempty" jsonschema:"the service build this bridge is talking to"`
	SearchModes          []string `json:"search_modes" jsonschema:"the search_memories modes this deployment can serve; an empty list means content search is unavailable entirely"`
	SummariserEnabled    bool     `json:"summariser_enabled"`
	ConsolidationEnabled bool     `json:"consolidation_enabled" jsonschema:"false on a replica, where nothing is forgetting and the decay tools report no schedule"`
}

// searchModeNames renders the search-mode enum as the strings search_memories' mode input takes, so
// what whoami reports can be passed straight back in rather than translated.
func searchModeNames(in []contract.SearchMode) []string {
	out := make([]string, 0, len(in))

	for _, mode := range in {
		switch mode {

		case contract.SearchMode_SEARCH_MODE_KEYWORD:
			out = append(out, "keyword")

		case contract.SearchMode_SEARCH_MODE_SEMANTIC:
			out = append(out, "semantic")

		case contract.SearchMode_SEARCH_MODE_HYBRID:
			out = append(out, "hybrid")

		}
	}

	return out
}

func (b *bridge) whoAmI(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoAmIOutput, error) {
	callCtx, cancel := b.callContext(ctx)
	defer cancel()

	res, err := b.client.WhoAmI(callCtx, &contract.EmptyRequest{})
	if err != nil {
		return nil, whoAmIOutput{}, fmt.Errorf("WhoAmI failed: %w", err)
	}

	return nil, whoAmIOutput{
		Role:                 res.GetRole(),
		ClientId:             res.GetClientId(),
		AuthEnabled:          res.GetAuthEnabled(),
		Groups:               res.GetGroups(),
		GroupScoped:          res.GetGroupScoped(),
		Version:              res.GetVersion(),
		SearchModes:          searchModeNames(res.GetSearchModes()),
		SummariserEnabled:    res.GetSummariserEnabled(),
		ConsolidationEnabled: res.GetConsolidationEnabled(),
	}, nil
}
