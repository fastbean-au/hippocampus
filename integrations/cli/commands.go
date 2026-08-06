package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/fastbean-au/hippocampus/contract"
)

// handler runs a single command against an already-built client, reading its command-specific flags
// from fs and writing its response through r.
type handler func(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error

// command describes one CLI subcommand: a one-line summary, an argument/flag hint for the usage
// text, a function that registers its command-specific flags, and the handler that runs it.
type command struct {
	summary string
	hint    string
	flags   func(*pflag.FlagSet)
	run     handler
}

// commands is the full command registry keyed by its invocation path ("memory store", "event
// list", "whoami", ...). Dispatch matches the two-word key first, then the one-word key.
func commands() map[string]command {
	return map[string]command{
		"memory store": {
			summary: "store a new memory",
			hint:    "--body <text> [--significance N] [--event-id ID] [--group G]",
			flags:   memoryWriteFlags,
			run:     runMemoryStore,
		},
		"memory update": {
			summary: "apply a partial update to an existing memory",
			hint:    "--id ID [--body <text>] [--significance N] [--group G]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the memory to update (required)")
				memoryContentFlags(fs)
				addPlacementFlags(fs)
			},
			run: runMemoryUpdate,
		},
		"memory delete": {
			summary: "delete memories by id",
			hint:    "--id ID [--id ID ...] | ID [ID ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.StringSlice("id", nil, "memory id to delete (repeatable; ids may also be positional args)")
			},
			run: runMemoryDelete,
		},
		"memory list": {
			summary: "list memories with optional filters",
			hint:    "[--group G] [--significance-min N] [--limit N] [--offset N]",
			flags:   memoryFilterFlags,
			run:     runMemoryList,
		},
		"memory recall": {
			summary: "recall memories by id (reinforces them)",
			hint:    "--id ID [--id ID ...] | ID [ID ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.StringSlice("id", nil, "memory id to recall (repeatable; ids may also be positional args)")
			},
			run: runMemoryRecall,
		},
		"memory search": {
			summary: "search memories via the content-search index",
			hint:    "--query <text> [--mode keyword|semantic|hybrid] [--limit N] [--event-id ID] [--reinforce]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("query", "", "search query (required)")
				fs.Int32("limit", 0, "maximum results (0 selects the server default)")
				fs.String("event-id", "", "restrict matches to a single event")
				fs.String("group", "", "restrict matches to a group label")
				fs.Bool("reinforce", false, "route matches through recall, reinforcing them")
				fs.String("mode", "keyword", "how to match: keyword, semantic, or hybrid (semantic and hybrid need the service to have an embedding model and OpenSearch)")
			},
			run: runMemorySearch,
		},
		"memory explain": {
			summary: "show where memories stand against the consolidation rules",
			hint:    "--id ID [--id ID ...] | ID [ID ...] [--curve-significance N]",
			flags: func(fs *pflag.FlagSet) {
				fs.StringSlice("id", nil, "memory id to explain (repeatable; ids may also be positional args)")
				fs.Float64("curve-significance", 0, "also project the decay curve for this combined significance")
				fs.Float64("curve-days", 0, "with --curve-significance: days to project over (0 lets the server choose a span that shows the crossing)")
				fs.Int32("curve-points", 0, "with --curve-significance: samples to return (default 60, max 500)")
			},
			run: runMemoryExplain,
		},
		"event create": {
			summary: "create a new event",
			hint:    "--name <name> [--significance N] [--group G] [--time-start RFC3339]",
			flags:   eventWriteFlags,
			run:     runEventCreate,
		},
		"event end": {
			summary: "set an event's end time",
			hint:    "--id ID [--time-end RFC3339]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to end (required)")
				fs.String("time-end", "", "end time as RFC3339 (defaults to now)")
			},
			run: runEventEnd,
		},
		"event significance": {
			summary: "change an event's significance",
			hint:    "--id ID [--significance N | --place-mode ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to update (required)")
				fs.Int32("significance", 0, "new absolute significance (>= 1 to take effect)")
				addPlacementFlags(fs)
			},
			run: runEventSignificance,
		},
		"event merge": {
			summary: "re-point every memory of one event onto another",
			hint:    "--from ID --to ID",
			flags: func(fs *pflag.FlagSet) {
				fs.String("from", "", "event whose memories are moved (required)")
				fs.String("to", "", "surviving event; must already exist (required)")
			},
			run: runEventMerge,
		},
		"event delete": {
			summary: "delete an event (optionally its memories too)",
			hint:    "--id ID [--memories]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to delete (required)")
				fs.Bool("memories", false, "also delete the event's memories (otherwise they are detached)")
			},
			run: runEventDelete,
		},
		"event get": {
			summary: "fetch a single event by id",
			hint:    "--id ID [--memories]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to fetch (required)")
				fs.Bool("memories", false, "also load the event's memories")
			},
			run: runEventGet,
		},
		"event list": {
			summary: "list events with optional filters",
			hint:    "[--group G] [--significance-min N] [--limit N] [--memories]",
			flags:   eventFilterFlags,
			run:     runEventList,
		},
		"whoami": {
			summary: "report the caller's identity and effective tier",
			flags:   func(*pflag.FlagSet) {},
			run:     runWhoAmI,
		},
		"sleep": {
			summary: "trigger a consolidation cycle now",
			hint:    "[--dry-run]",
			flags: func(fs *pflag.FlagSet) {
				fs.Bool("dry-run", false, "report what a cycle would forget without deleting anything")
				fs.Int32("limit", 0, "with --dry-run: memories to detail (default 100, max 1000)")
			},
			run: runSleep,
		},
		"purge": {
			summary: "delete every event and memory (destructive)",
			hint:    "--yes",
			flags:   func(fs *pflag.FlagSet) { fs.Bool("yes", false, "confirm the irreversible purge") },
			run:     runPurge,
		},
		"summary candidates": {
			summary: "list events worth condensing into a summary",
			flags:   func(*pflag.FlagSet) {},
			run:     runSummaryCandidates,
		},
		"summary replace": {
			summary: "replace an event's memories with a caller-supplied summary",
			hint:    "--event-id ID --body <text> [--significance N]",
			flags: func(fs *pflag.FlagSet) {
				// memoryContentFlags registers --event-id; here it names the event whose memories are
				// replaced (the server sets the summary memory's own event_id to it regardless).
				memoryContentFlags(fs)
				fs.Bool("binary", false, "mark the summary body as binary (never content-indexed)")
				addPlacementFlags(fs)
			},
			run: runSummaryReplace,
		},
		"summary summarise": {
			summary: "generate and store a summary with the embedded LLM",
			hint:    "--event-id ID [--significance N]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("event-id", "", "event to summarise (required)")
				fs.Int32("significance", 0, "significance for the summary memory (0 defaults to the highest replaced)")
				addPlacementFlags(fs)
			},
			run: runSummarise,
		},
		"export": {
			summary: "snapshot the store into an S3 archive object",
			hint:    "[--clear]",
			flags: func(fs *pflag.FlagSet) {
				fs.Bool("clear", false, "delete the captured records after a successful upload")
			},
			run: runExport,
		},
		"import": {
			summary: "import an archive object from S3",
			hint:    "--object-key KEY",
			flags:   func(fs *pflag.FlagSet) { fs.String("object-key", "", "S3 object key of the archive (required)") },
			run:     runImport,
		},
		"import-batch": {
			summary: "upsert full-state rows from a JSON ImportBatchRequest file",
			hint:    "--file PATH ('-' for stdin)",
			flags: func(fs *pflag.FlagSet) {
				fs.String("file", "", "JSON ImportBatchRequest file, or '-' for stdin (required)")
			},
			run: runImportBatch,
		},
		"transfer": {
			summary: "stream the whole store into a centralised instance",
			hint:    "[--clear]",
			flags: func(fs *pflag.FlagSet) {
				fs.Bool("clear", false, "delete the captured records once the target has accepted them")
			},
			run: runTransfer,
		},
		"clear": {
			summary: "delete exactly the records captured by an export/transfer run",
			hint:    "--manifest-id ID",
			flags: func(fs *pflag.FlagSet) {
				fs.String("manifest-id", "", "manifest id returned by export/transfer (required)")
			},
			run: runClear,
		},
		"completion": {
			summary: "print a shell completion script (bash|zsh|fish)",
			hint:    "bash|zsh|fish",
			flags:   func(*pflag.FlagSet) {},
			run:     runCompletionCmd,
		},
	}
}

// --- shared flag registration ---------------------------------------------------------------------

// memoryContentFlags registers the content fields shared by memory store/update and summary replace.
func memoryContentFlags(fs *pflag.FlagSet) {
	fs.String("body", "", "memory body text")
	fs.String("body-file", "", "read the body from a file, or '-' for stdin")
	fs.Int32("significance", 0, "significance; 0 leaves it unranked (or unchanged on update)")
	fs.String("event-id", "", "associate the memory with an event")
	fs.String("group", "", "freeform grouping/context label")
	fs.String("timestamp", "", "memory timestamp as RFC3339 (defaults to now on create)")
}

// memoryWriteFlags is the memory-store flag set: content plus id, binary, and placement.
func memoryWriteFlags(fs *pflag.FlagSet) {
	fs.String("id", "", "memory id (auto-generated UUID when omitted)")
	memoryContentFlags(fs)
	fs.Bool("binary", false, "mark the body as binary (never content-indexed)")
	addPlacementFlags(fs)
}

// eventWriteFlags is the event-create flag set.
func eventWriteFlags(fs *pflag.FlagSet) {
	fs.String("id", "", "event id (auto-generated UUID when omitted)")
	fs.String("name", "", "event name (required)")
	fs.String("description", "", "event description")
	fs.Int32("significance", 0, "significance; 0 leaves it unranked")
	fs.String("group", "", "freeform grouping/context label")
	fs.String("time-start", "", "start time as RFC3339 (defaults to now)")
	fs.String("time-end", "", "end time as RFC3339 (0/unset means not ended)")
	fs.StringSlice("relationship", nil, "related event as 'eventID:significance' (repeatable)")
	addPlacementFlags(fs)
}

func memoryFilterFlags(fs *pflag.FlagSet) {
	fs.String("timestamp-min", "", "inclusive lower bound on time_stamp (RFC3339)")
	fs.String("timestamp-max", "", "inclusive upper bound on time_stamp (RFC3339)")
	fs.Int32("significance-min", 0, "inclusive lower bound on significance (0 = no bound)")
	fs.Int32("significance-max", 0, "inclusive upper bound on significance (0 = no bound)")
	fs.String("group", "", "restrict to a group label")
	fs.String("order-by", "", "'significance' or 'timestamp'")
	fs.Int32("limit", 0, "page size (0 selects the server default)")
	fs.Int32("offset", 0, "rows to skip for pagination")
	fs.String("extremum", "", "'highest' or 'lowest' significance tie (ignores the significance range)")
}

func eventFilterFlags(fs *pflag.FlagSet) {
	fs.String("time-start-min", "", "inclusive lower bound on time_start (RFC3339)")
	fs.String("time-start-max", "", "inclusive upper bound on time_start (RFC3339)")
	fs.String("time-end-min", "", "inclusive lower bound on time_end (RFC3339)")
	fs.String("time-end-max", "", "inclusive upper bound on time_end (RFC3339)")
	fs.Int32("significance-min", 0, "inclusive lower bound on significance (0 = no bound)")
	fs.Int32("significance-max", 0, "inclusive upper bound on significance (0 = no bound)")
	fs.String("group", "", "restrict to a group label")
	fs.String("order-by", "", "'significance' or 'timestamp'")
	fs.Int32("limit", 0, "page size (0 selects the server default)")
	fs.Int32("offset", 0, "rows to skip for pagination")
	fs.Bool("memories", false, "include each event's memories")
	fs.String("extremum", "", "'highest' or 'lowest' significance tie (ignores the significance range)")
}

// addPlacementFlags registers the shared significance-placement flags.
func addPlacementFlags(fs *pflag.FlagSet) {
	fs.String("place-mode", "", "significance placement: 'above', 'below', or 'between'")
	fs.Int32("place-anchor", 0, "placement anchor significance value")
	fs.String("place-anchor-id", "", "placement anchor by an existing item's id")
	fs.Int32("place-upper", 0, "placement upper bound (between mode)")
	fs.String("place-upper-id", "", "placement upper bound by an existing item's id (between mode)")
}

// --- handlers -------------------------------------------------------------------------------------

func runMemoryStore(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	memory, err := memoryFromFlags(fs, str(fs, "id"))
	if err != nil {
		return err
	}

	if memory.GetBody() == "" {
		return fmt.Errorf("a memory body is required (--body or --body-file)")
	}

	memory.IsBinary = boolValue(fs, "binary")

	resp, err := client.StoreMemory(ctx, memory)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runMemoryUpdate(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	memory, err := memoryFromFlags(fs, id)
	if err != nil {
		return err
	}

	resp, err := client.UpdateMemory(ctx, memory)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runMemoryDelete(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	ids := idArgs(fs)
	if len(ids) == 0 {
		return fmt.Errorf("at least one memory id is required (--id or positional args)")
	}

	resp, err := client.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: ids})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// runMemoryExplain reports where memories stand against the consolidation rules, and optionally the
// decay curve of the configuration deciding it. Either half may stand alone: ids without a curve
// value just those memories, while --curve-significance with no ids asks only what the current
// configuration does - which is how an operator tunes it without a store to try it on.
func runMemoryExplain(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	ids := idArgs(fs)
	significance := f64(fs, "curve-significance")

	if len(ids) == 0 && significance <= 0 {
		return fmt.Errorf("at least one memory id (--id or positional args) or a --curve-significance is required")
	}

	req := &contract.ExplainConsolidationRequest{MemoryIds: ids}

	if significance > 0 {
		req.Curve = &contract.DecayCurveRequest{
			Significance: significance,
			MaxAgeDays:   f64(fs, "curve-days"),
			Points:       i32(fs, "curve-points"),
		}
	}

	resp, err := client.ExplainConsolidation(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runMemoryList(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	tsMin, err := parseTime(fs, "timestamp-min")
	if err != nil {
		return err
	}

	tsMax, err := parseTime(fs, "timestamp-max")
	if err != nil {
		return err
	}

	ext, err := extremumFromFlags(fs)
	if err != nil {
		return err
	}

	req := &contract.GetMemoriesRequest{
		TimestampMin:         tsMin,
		TimestampMax:         tsMax,
		SignificanceMin:      i32(fs, "significance-min"),
		SignificanceMax:      i32(fs, "significance-max"),
		Group:                str(fs, "group"),
		OrderBy:              str(fs, "order-by"),
		Limit:                i32(fs, "limit"),
		Offset:               i32(fs, "offset"),
		SignificanceExtremum: ext,
	}

	resp, err := client.GetMemories(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runMemoryRecall(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	ids := idArgs(fs)
	if len(ids) == 0 {
		return fmt.Errorf("at least one memory id is required (--id or positional args)")
	}

	resp, err := client.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: ids})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// searchModes maps the --mode flag onto the RPC enum.
var searchModes = map[string]contract.SearchMode{
	"":         contract.SearchMode_SEARCH_MODE_UNSPECIFIED,
	"keyword":  contract.SearchMode_SEARCH_MODE_KEYWORD,
	"semantic": contract.SearchMode_SEARCH_MODE_SEMANTIC,
	"hybrid":   contract.SearchMode_SEARCH_MODE_HYBRID,
}

func runMemorySearch(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	query := str(fs, "query")
	if query == "" {
		return fmt.Errorf("--query is required")
	}

	// An unknown mode is an error rather than a silent fall back to keyword: an operator who asked
	// for meaning and got word matching would have no way to tell.
	mode, ok := searchModes[strings.ToLower(strings.TrimSpace(str(fs, "mode")))]
	if !ok {
		return fmt.Errorf("--mode must be keyword, semantic, or hybrid")
	}

	req := &contract.SearchMemoriesRequest{
		Query:     query,
		Limit:     i32(fs, "limit"),
		EventId:   str(fs, "event-id"),
		Reinforce: b(fs, "reinforce"),
		Group:     str(fs, "group"),
		Mode:      mode,
	}

	resp, err := client.SearchMemories(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventCreate(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	name := str(fs, "name")
	if name == "" {
		return fmt.Errorf("--name is required")
	}

	timeStart, err := parseTime(fs, "time-start")
	if err != nil {
		return err
	}

	timeEnd, err := parseTime(fs, "time-end")
	if err != nil {
		return err
	}

	rels, err := relationshipsFromFlags(fs)
	if err != nil {
		return err
	}

	place, err := placementFromFlags(fs)
	if err != nil {
		return err
	}

	event := &contract.Event{
		Id:            str(fs, "id"),
		Name:          name,
		Description:   str(fs, "description"),
		Significance:  i32(fs, "significance"),
		Group:         str(fs, "group"),
		TimeStart:     timeStart,
		TimeEnd:       timeEnd,
		Relationships: rels,
		Placement:     place,
	}

	resp, err := client.StoreEvent(ctx, event)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventEnd(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	timeEnd, err := parseTime(fs, "time-end")
	if err != nil {
		return err
	}

	resp, err := client.EndEvent(ctx, &contract.EndEventRequest{Id: id, TimeEnd: timeEnd})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventSignificance(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	place, err := placementFromFlags(fs)
	if err != nil {
		return err
	}

	req := &contract.UpdateEventSignificanceRequest{
		Id:           id,
		Significance: i32(fs, "significance"),
		Placement:    place,
	}

	resp, err := client.UpdateEventSignificance(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventMerge(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	from := str(fs, "from")
	to := str(fs, "to")

	if from == "" || to == "" {
		return fmt.Errorf("both --from and --to are required")
	}

	resp, err := client.MergeEvents(ctx, &contract.MergeEventsRequest{MergeFrom: from, MergeTo: to})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventDelete(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	resp, err := client.DeleteEvent(ctx, &contract.DeleteEventRequest{Id: id, Memories: b(fs, "memories")})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventGet(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	resp, err := client.GetEventById(ctx, &contract.GetEventByIdRequest{Id: id, Memories: b(fs, "memories")})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventList(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	tsStartMin, err := parseTime(fs, "time-start-min")
	if err != nil {
		return err
	}

	tsStartMax, err := parseTime(fs, "time-start-max")
	if err != nil {
		return err
	}

	tsEndMin, err := parseTime(fs, "time-end-min")
	if err != nil {
		return err
	}

	tsEndMax, err := parseTime(fs, "time-end-max")
	if err != nil {
		return err
	}

	ext, err := extremumFromFlags(fs)
	if err != nil {
		return err
	}

	req := &contract.GetEventsRequest{
		TimeStartMin:         tsStartMin,
		TimeStartMax:         tsStartMax,
		TimeEndMin:           tsEndMin,
		TimeEndMax:           tsEndMax,
		SignificanceMin:      i32(fs, "significance-min"),
		SignificanceMax:      i32(fs, "significance-max"),
		Group:                str(fs, "group"),
		OrderBy:              str(fs, "order-by"),
		Limit:                i32(fs, "limit"),
		Offset:               i32(fs, "offset"),
		Memories:             b(fs, "memories"),
		SignificanceExtremum: ext,
	}

	resp, err := client.GetEvents(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runWhoAmI(ctx context.Context, client contract.HippocampusClient, _ *pflag.FlagSet, r *renderer) error {
	resp, err := client.WhoAmI(ctx, &contract.EmptyRequest{})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// runSleep triggers a cycle, or - with --dry-run - reports what one would forget. The dry run is a
// separate RPC rather than a flag on Sleep (so the two can be authorised apart), but it reads
// naturally as a flag on the command an operator already knows, so that is how the CLI spells it.
func runSleep(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	if b(fs, "dry-run") {
		resp, err := client.PreviewConsolidation(ctx, &contract.PreviewConsolidationRequest{
			Limit: i32(fs, "limit"),
		})
		if err != nil {
			return err
		}

		return r.render(resp)
	}

	resp, err := client.Sleep(ctx, &contract.EmptyRequest{})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runPurge(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	if !b(fs, "yes") {
		return fmt.Errorf("purge deletes every event and memory; re-run with --yes to confirm")
	}

	resp, err := client.Purge(ctx, &contract.EmptyRequest{})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runSummaryCandidates(ctx context.Context, client contract.HippocampusClient, _ *pflag.FlagSet, r *renderer) error {
	resp, err := client.GetSummarisationCandidates(ctx, &contract.EmptyRequest{})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runSummaryReplace(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	eventID := str(fs, "event-id")
	if eventID == "" {
		return fmt.Errorf("--event-id is required")
	}

	summary, err := memoryFromFlags(fs, "")
	if err != nil {
		return err
	}

	if summary.GetBody() == "" {
		return fmt.Errorf("a summary body is required (--body or --body-file)")
	}

	summary.IsBinary = boolValue(fs, "binary")

	req := &contract.ReplaceMemoriesWithSummaryRequest{EventId: eventID, Summary: summary}

	resp, err := client.ReplaceMemoriesWithSummary(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runSummarise(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	eventID := str(fs, "event-id")
	if eventID == "" {
		return fmt.Errorf("--event-id is required")
	}

	place, err := placementFromFlags(fs)
	if err != nil {
		return err
	}

	req := &contract.SummariseMemoriesRequest{
		EventId:      eventID,
		Significance: i32(fs, "significance"),
		Placement:    place,
	}

	resp, err := client.SummariseMemories(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runExport(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	resp, err := client.Export(ctx, &contract.ExportRequest{Clear: b(fs, "clear")})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runImport(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	objectKey := str(fs, "object-key")
	if objectKey == "" {
		return fmt.Errorf("--object-key is required")
	}

	resp, err := client.Import(ctx, &contract.ImportRequest{ObjectKey: objectKey})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runImportBatch(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	path := str(fs, "file")
	if path == "" {
		return fmt.Errorf("--file is required")
	}

	data, err := readFileOrStdin(path)
	if err != nil {
		return err
	}

	req := &contract.ImportBatchRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, req); err != nil {
		return fmt.Errorf("failed to parse %s as an ImportBatchRequest: %w", path, err)
	}

	resp, err := client.ImportBatch(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runTransfer(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	resp, err := client.Transfer(ctx, &contract.TransferRequest{Clear: b(fs, "clear")})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runClear(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	manifestID := str(fs, "manifest-id")
	if manifestID == "" {
		return fmt.Errorf("--manifest-id is required")
	}

	resp, err := client.Clear(ctx, &contract.ClearRequest{ManifestId: manifestID})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// --- flag helpers ---------------------------------------------------------------------------------

// memoryFromFlags builds a Memory from the shared content flags. It does not set is_binary (store
// and summary-replace set that themselves; update never does) or validate the body.
func memoryFromFlags(fs *pflag.FlagSet, id string) (*contract.Memory, error) {
	body, err := memoryBody(fs)
	if err != nil {
		return nil, err
	}

	timestamp, err := parseTime(fs, "timestamp")
	if err != nil {
		return nil, err
	}

	place, err := placementFromFlags(fs)
	if err != nil {
		return nil, err
	}

	return &contract.Memory{
		Id:           id,
		Body:         body,
		Significance: i32(fs, "significance"),
		EventId:      str(fs, "event-id"),
		Group:        str(fs, "group"),
		TimeStamp:    timestamp,
		Placement:    place,
	}, nil
}

// memoryBody reads the body from --body-file (or stdin for '-') when set, otherwise --body.
func memoryBody(fs *pflag.FlagSet) (string, error) {
	if path := str(fs, "body-file"); path != "" {
		data, err := readFileOrStdin(path)
		if err != nil {
			return "", err
		}

		return string(data), nil
	}

	return str(fs, "body"), nil
}

// placementFromFlags builds a SignificancePlacement from the --place-* flags, or nil when no mode is
// set.
func placementFromFlags(fs *pflag.FlagSet) (*contract.SignificancePlacement, error) {
	mode := str(fs, "place-mode")
	if mode == "" {
		return nil, nil
	}

	var placementMode contract.SignificancePlacement_Mode

	switch mode {

	case "above":
		placementMode = contract.SignificancePlacement_ABOVE

	case "below":
		placementMode = contract.SignificancePlacement_BELOW

	case "between":
		placementMode = contract.SignificancePlacement_BETWEEN

	default:
		return nil, fmt.Errorf("invalid --place-mode %q (expected 'above', 'below', or 'between')", mode)
	}

	return &contract.SignificancePlacement{
		Mode:     placementMode,
		Anchor:   i32(fs, "place-anchor"),
		AnchorId: str(fs, "place-anchor-id"),
		Upper:    i32(fs, "place-upper"),
		UpperId:  str(fs, "place-upper-id"),
	}, nil
}

// extremumFromFlags maps the --extremum flag onto the SignificanceExtremum enum.
func extremumFromFlags(fs *pflag.FlagSet) (contract.SignificanceExtremum, error) {
	switch value := str(fs, "extremum"); value {

	case "":
		return contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_UNSPECIFIED, nil

	case "highest":
		return contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST, nil

	case "lowest":
		return contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST, nil

	default:
		return 0, fmt.Errorf("invalid --extremum %q (expected 'highest' or 'lowest')", value)
	}
}

// relationshipsFromFlags parses the repeatable --relationship 'eventID:significance' flag.
func relationshipsFromFlags(fs *pflag.FlagSet) ([]*contract.Relationship, error) {
	raw := strs(fs, "relationship")
	if len(raw) == 0 {
		return nil, nil
	}

	out := make([]*contract.Relationship, 0, len(raw))

	for _, entry := range raw {
		eventID, sigText, ok := strings.Cut(entry, ":")
		if !ok || eventID == "" {
			return nil, fmt.Errorf("invalid --relationship %q (want 'eventID:significance')", entry)
		}

		sig, err := strconv.Atoi(sigText)
		if err != nil {
			return nil, fmt.Errorf("invalid --relationship %q: significance %q is not an integer", entry, sigText)
		}

		out = append(out, &contract.Relationship{EventId: eventID, Significance: int32(sig)})
	}

	return out, nil
}

// idArgs collects ids from both the repeatable --id flag and any positional arguments.
func idArgs(fs *pflag.FlagSet) []string {
	ids := strs(fs, "id")

	return append(ids, fs.Args()...)
}

// boolValue maps a bool flag onto the tri-state contract.Bool (TRUE when set, UNSPECIFIED when not).
func boolValue(fs *pflag.FlagSet, name string) contract.Bool {
	if b(fs, name) {
		return contract.Bool_TRUE
	}

	return contract.Bool_UNSPECIFIED
}

// parseTime reads an RFC3339 flag and returns its UnixNano value; an empty flag yields 0 (the
// server then defaults or treats it as no bound, depending on the field).
func parseTime(fs *pflag.FlagSet, name string) (int64, error) {
	value := str(fs, name)
	if value == "" {
		return 0, nil
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("invalid --%s %q (want RFC3339, e.g. 2006-01-02T15:04:05Z): %w", name, value, err)
	}

	return parsed.UnixNano(), nil
}

// readFileOrStdin reads path, treating '-' as stdin.
func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("failed to read stdin: %w", err)
		}

		return data, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	return data, nil
}

// str/i32/b/strs are convenience accessors over a parsed flag set; the lookups never fail for flags
// this package defines, so the error is discarded.
func str(fs *pflag.FlagSet, name string) string {
	value, _ := fs.GetString(name)

	return value
}

func i32(fs *pflag.FlagSet, name string) int32 {
	value, _ := fs.GetInt32(name)

	return value
}

func b(fs *pflag.FlagSet, name string) bool {
	value, _ := fs.GetBool(name)

	return value
}

func strs(fs *pflag.FlagSet, name string) []string {
	value, _ := fs.GetStringSlice(name)

	return value
}

func f64(fs *pflag.FlagSet, name string) float64 {
	value, _ := fs.GetFloat64(name)

	return value
}

// sortedCommandKeys returns the command keys in a stable order for usage listings.
func sortedCommandKeys(cmds map[string]command) []string {
	keys := make([]string, 0, len(cmds))
	for k := range cmds {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
