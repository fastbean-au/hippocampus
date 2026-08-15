package main

import (
	"context"
	"errors"
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
			hint:    "--body <text> [--significance N] [--event-id ID] [--group G] [--metadata k=v]",
			flags:   memoryWriteFlags,
			run:     runMemoryStore,
		},
		"memory update": {
			summary: "apply a partial update to an existing memory",
			hint:    "--id ID [--body <text>] [--significance N] [--group G] [--metadata k=v] [--clear-metadata]",
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
			hint:    "[--group G] [--metadata k=v] [--recalled false] [--significance-min N] [--limit N]",
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
		"memory link": {
			summary: "link a memory to other memories",
			hint:    "--id ID --link TARGET:SIGNIFICANCE [--link TARGET:SIGNIFICANCE ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the memory the links start from (required)")
				fs.StringSlice("link", nil, "linked memory as 'memoryID:significance' (repeatable)")
			},
			run: runMemoryLink,
		},
		"memory unlink": {
			summary: "remove links between a memory and other memories",
			hint:    "--id ID --target ID [--target ID ...] | --id ID TARGET [TARGET ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the memory the links start from (required)")
				fs.StringSlice("target", nil, "memory id to unlink (repeatable; ids may also be positional args)")
			},
			run: runMemoryUnlink,
		},
		"memory links": {
			summary: "list a memory's links",
			hint:    "--id ID [--direction both|outbound|inbound]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the memory whose links to list (required)")
				fs.String("direction", "", "which links to return: both (default), outbound, or inbound")
			},
			run: runMemoryLinks,
		},
		"memory search": {
			summary: "search memories via the content-search index",
			hint:    "--query <text> [--mode keyword|semantic|hybrid] [--metadata k=v] [--limit N] [--reinforce]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("query", "", "search query (required)")
				fs.Int32("limit", 0, "maximum results (0 selects the server default)")
				fs.String("event-id", "", "restrict matches to a single event")
				fs.String("group", "", "restrict matches to a group label")
				fs.StringSlice("metadata", nil, "restrict matches to memories carrying this 'key=value' label (repeatable; all must match)")
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
		"event link": {
			summary: "link an event to other events",
			hint:    "--id ID --link TARGET:SIGNIFICANCE [--link TARGET:SIGNIFICANCE ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event the links start from (required)")
				fs.StringSlice("link", nil, "linked event as 'eventID:significance' (repeatable)")
			},
			run: runEventLink,
		},
		"event unlink": {
			summary: "remove links between an event and other events",
			hint:    "--id ID --target ID [--target ID ...] | --id ID TARGET [TARGET ...]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event the links start from (required)")
				fs.StringSlice("target", nil, "event id to unlink (repeatable; ids may also be positional args)")
			},
			run: runEventUnlink,
		},
		"event links": {
			summary: "list an event's links",
			hint:    "--id ID [--direction both|outbound|inbound]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event whose links to list (required)")
				fs.String("direction", "", "which links to return: both (default), outbound, or inbound")
			},
			run: runEventLinks,
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
			hint:    "--id ID [--memories|--memory-counts]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to fetch (required)")
				fs.Bool("memories", false, "also load the event's memories")
				fs.Bool("memory-counts", false, "report how many memories the event holds, without transferring them")
			},
			run: runEventGet,
		},
		"event list": {
			summary: "list events with optional filters",
			hint:    "[--group G] [--significance-min N] [--limit N] [--memories|--memory-counts]",
			flags:   eventFilterFlags,
			run:     runEventList,
		},
		"whoami": {
			summary: "report the caller's identity and effective tier",
			flags:   func(*pflag.FlagSet) {},
			run:     runWhoAmI,
		},
		"status": {
			summary: "report the consolidation cycle's schedule and its last result",
			flags:   func(*pflag.FlagSet) {},
			run:     runConsolidationStatus,
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
		"forgotten list": {
			summary: "list memories the sleep cycle forgot, and why",
			hint:    "[--memory-id ID] [--group G] [--rule consolidation|eviction] [--since T] [--limit N]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("memory-id", "", "a specific memory: did it exist, and when did it go")
				fs.String("event-id", "", "only memories that belonged to this event")
				fs.String("group", "", "only memories in this group")
				fs.String("rule", "", "which path took them: consolidation or eviction (default both)")
				fs.String("since", "", "only memories forgotten at or after this RFC3339 time")
				fs.String("until", "", "only memories forgotten before this RFC3339 time")
				fs.Int64("after-seq", 0, "pagination: the next_seq reported by the previous page")
				fs.Int32("limit", 0, "records to return (default 100, max 1000)")
			},
			run: runForgottenList,
		},
		"forgotten clear": {
			summary: "delete records from the forgotten log (destructive)",
			hint:    "--before T | --all",
			flags: func(fs *pflag.FlagSet) {
				fs.String("before", "", "delete records forgotten before this RFC3339 time")
				fs.Bool("all", false, "delete every record in the log")
			},
			run: runForgottenClear,
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
	fs.StringSlice("metadata", nil, "metadata label as 'key=value' (repeatable)")
	fs.Bool("clear-metadata", false, "on update, remove all metadata (an empty --metadata means 'leave unchanged')")
	fs.Bool("clear-group", false, "on update, reset the group to empty (an empty --group means 'leave unchanged')")
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
	fs.StringSlice("metadata", nil, "metadata label as 'key=value' (repeatable)")
	fs.StringSlice("link", nil, "linked event as 'eventID:significance' (repeatable)")
	addPlacementFlags(fs)
}

// memoryOrderByValues and eventOrderByValues are the order_by values the service accepts for each
// listing, used for the flag help and for shell completion.
//
// They are hand-copied from db/order.go rather than imported: this module depends on the root one
// for the contract alone, and importing db would pull all three storage drivers into a client
// binary. That copy is safe here in a way it would not be in the service, because the CLI does not
// validate - it sends the string on and the server is the only enforcement point - so a stale entry
// costs a wrong suggestion, never a wrongly-ordered page.
var (
	memoryOrderByValues = []string{
		"group", "id", "link_significance", "recall_count", "significance", "time_recalled", "timestamp",
	}
	eventOrderByValues = []string{
		"group", "id", "link_significance", "name", "significance", "time_end", "timestamp",
	}
)

func memoryFilterFlags(fs *pflag.FlagSet) {
	fs.String("timestamp-min", "", "inclusive lower bound on time_stamp (RFC3339)")
	fs.String("timestamp-max", "", "inclusive upper bound on time_stamp (RFC3339)")
	fs.Int32("significance-min", 0, "inclusive lower bound on significance (0 = no bound)")
	fs.Int32("significance-max", 0, "inclusive upper bound on significance (0 = no bound)")
	fs.String("group", "", "restrict to a group label")
	fs.String("order-by", "", "sort field: "+strings.Join(memoryOrderByValues, ", "))
	fs.String("order-dir", "", "'asc' or 'desc'; omitted uses the sort field's natural direction")
	fs.Int32("limit", 0, "page size (0 selects the server default)")
	fs.Int32("offset", 0, "rows to skip for pagination")
	fs.String("extremum", "", "'highest' or 'lowest' significance tie (ignores the significance range)")
	fs.StringSlice("metadata", nil, "restrict to memories carrying this 'key=value' label (repeatable; all must match)")
	fs.String("recalled", "", "'true' for memories recalled at least once, 'false' for those never recalled")
	fs.String("summary", "", "'true' for summary memories only, 'false' to exclude them")
	fs.String("binary", "", "'true' for binary memories only, 'false' to exclude them")
	fs.Int32("recall-count-min", 0, "inclusive lower bound on recall_count (0 = no bound)")
	fs.Int32("recall-count-max", 0, "inclusive upper bound on recall_count (0 = no bound)")
	fs.String("recalled-after", "", "inclusive lower bound on time_recalled (RFC3339); never-recalled memories are excluded")
	fs.String("recalled-before", "", "inclusive upper bound on time_recalled (RFC3339); never-recalled memories are excluded")
	fs.String("event", "", "restrict to one event's memories (the paged way to read them)")
	fs.String("has-event", "", "'true' for memories associated with an event, 'false' for those with none")
}

func eventFilterFlags(fs *pflag.FlagSet) {
	fs.String("time-start-min", "", "inclusive lower bound on time_start (RFC3339)")
	fs.String("time-start-max", "", "inclusive upper bound on time_start (RFC3339)")
	fs.String("time-end-min", "", "inclusive lower bound on time_end (RFC3339)")
	fs.String("time-end-max", "", "inclusive upper bound on time_end (RFC3339)")
	fs.Int32("significance-min", 0, "inclusive lower bound on significance (0 = no bound)")
	fs.Int32("significance-max", 0, "inclusive upper bound on significance (0 = no bound)")
	fs.String("group", "", "restrict to a group label")
	fs.String("order-by", "", "sort field: "+strings.Join(eventOrderByValues, ", "))
	fs.String("order-dir", "", "'asc' or 'desc'; omitted uses the sort field's natural direction")
	fs.Int32("limit", 0, "page size (0 selects the server default)")
	fs.Int32("offset", 0, "rows to skip for pagination")
	fs.Bool("memories", false, "include each event's memories")
	fs.Bool("memory-counts", false, "report how many memories each event holds, without transferring them")
	fs.String("extremum", "", "'highest' or 'lowest' significance tie (ignores the significance range)")
	fs.StringSlice("metadata", nil, "restrict to events carrying this 'key=value' label (repeatable; all must match)")
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

// The link handlers. Memories and events take the same three shapes, so each pair differs only in
// which RPC it calls - the parsing, validation and rendering are shared.

func runMemoryLink(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id, links, err := linkArgs(fs, "memory")
	if err != nil {
		return err
	}

	resp, err := client.LinkMemories(ctx, &contract.LinkMemoriesRequest{Id: id, Links: links})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventLink(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id, links, err := linkArgs(fs, "event")
	if err != nil {
		return err
	}

	resp, err := client.LinkEvents(ctx, &contract.LinkEventsRequest{Id: id, Links: links})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runMemoryUnlink(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id, targets, err := unlinkArgs(fs, "memory")
	if err != nil {
		return err
	}

	resp, err := client.UnlinkMemories(ctx, &contract.UnlinkMemoriesRequest{Id: id, Ids: targets})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventUnlink(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id, targets, err := unlinkArgs(fs, "event")
	if err != nil {
		return err
	}

	resp, err := client.UnlinkEvents(ctx, &contract.UnlinkEventsRequest{Id: id, Ids: targets})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runMemoryLinks(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	direction, err := linkDirection(fs)
	if err != nil {
		return err
	}

	resp, err := client.GetMemoryLinks(ctx, &contract.GetMemoryLinksRequest{Id: id, Direction: direction})
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventLinks(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	direction, err := linkDirection(fs)
	if err != nil {
		return err
	}

	resp, err := client.GetEventLinks(ctx, &contract.GetEventLinksRequest{Id: id, Direction: direction})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// linkArgs reads the near end and the link set shared by memory link and event link.
func linkArgs(fs *pflag.FlagSet, kind string) (string, []*contract.Link, error) {
	id := str(fs, "id")
	if id == "" {
		return "", nil, fmt.Errorf("--id is required")
	}

	links, err := linksFromFlags(fs)
	if err != nil {
		return "", nil, err
	}

	if len(links) == 0 {
		return "", nil, fmt.Errorf("at least one --link '%sID:significance' is required", kind)
	}

	return id, links, nil
}

// unlinkArgs reads the near end and the targets shared by memory unlink and event unlink. Targets
// come from --target or positional args, matching how the delete commands take ids.
func unlinkArgs(fs *pflag.FlagSet, kind string) (string, []string, error) {
	id := str(fs, "id")
	if id == "" {
		return "", nil, fmt.Errorf("--id is required")
	}

	targets := append(strs(fs, "target"), fs.Args()...)
	if len(targets) == 0 {
		return "", nil, fmt.Errorf("at least one %s id to unlink is required (--target or positional args)", kind)
	}

	return id, targets, nil
}

// linkDirection maps the --direction flag onto the contract enum. An unset value means both, which
// is what the graph being valued symmetrically makes the useful default.
func linkDirection(fs *pflag.FlagSet) (contract.LinkDirection, error) {
	switch value := strings.ToLower(str(fs, "direction")); value {

	case "", "both":
		return contract.LinkDirection_LINK_DIRECTION_BOTH, nil

	case "outbound":
		return contract.LinkDirection_LINK_DIRECTION_OUTBOUND, nil

	case "inbound":
		return contract.LinkDirection_LINK_DIRECTION_INBOUND, nil

	default:
		return 0, fmt.Errorf("invalid --direction %q (expected 'both', 'outbound' or 'inbound')", value)

	}
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

	recalledAfter, err := parseTime(fs, "recalled-after")
	if err != nil {
		return err
	}

	recalledBefore, err := parseTime(fs, "recalled-before")
	if err != nil {
		return err
	}

	recalled, err := triStateFromFlag(fs, "recalled")
	if err != nil {
		return err
	}

	isSummary, err := triStateFromFlag(fs, "summary")
	if err != nil {
		return err
	}

	isBinary, err := triStateFromFlag(fs, "binary")
	if err != nil {
		return err
	}

	hasEvent, err := triStateFromFlag(fs, "has-event")
	if err != nil {
		return err
	}

	orderDir, err := orderDirFromFlags(fs)
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
		OrderDir:             orderDir,
		Limit:                i32(fs, "limit"),
		Offset:               i32(fs, "offset"),
		SignificanceExtremum: ext,

		EventId: str(fs, "event"),

		Metadata:        strs(fs, "metadata"),
		Recalled:        recalled,
		HasEvent:        hasEvent,
		IsSummary:       isSummary,
		IsBinary:        isBinary,
		RecallCountMin:  i32(fs, "recall-count-min"),
		RecallCountMax:  i32(fs, "recall-count-max"),
		TimeRecalledMin: recalledAfter,
		TimeRecalledMax: recalledBefore,
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
		Metadata:  strs(fs, "metadata"),
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

	links, err := linksFromFlags(fs)
	if err != nil {
		return err
	}

	place, err := placementFromFlags(fs)
	if err != nil {
		return err
	}

	metadata, err := parseMetadata(strs(fs, "metadata"))
	if err != nil {
		return err
	}

	event := &contract.Event{
		Id:           str(fs, "id"),
		Name:         name,
		Description:  str(fs, "description"),
		Significance: i32(fs, "significance"),
		Group:        str(fs, "group"),
		TimeStart:    timeStart,
		TimeEnd:      timeEnd,
		Links:        links,
		Placement:    place,
		Metadata:     metadata,
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

	resp, err := client.GetEventById(ctx, &contract.GetEventByIdRequest{
		Id:           id,
		Memories:     b(fs, "memories"),
		MemoryCounts: b(fs, "memory-counts"),
	})
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

	orderDir, err := orderDirFromFlags(fs)
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
		OrderDir:             orderDir,
		Limit:                i32(fs, "limit"),
		Offset:               i32(fs, "offset"),
		Memories:             b(fs, "memories"),
		MemoryCounts:         b(fs, "memory-counts"),
		SignificanceExtremum: ext,
		Metadata:             strs(fs, "metadata"),
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

// runConsolidationStatus reports when the next cycle is due and what the last one did. Reader-tier
// and cheap (it reads in-memory state, no scan), so it is the safe thing to run against a busy
// instance when the question is "is this thing forgetting anything at all".
func runConsolidationStatus(ctx context.Context, client contract.HippocampusClient, _ *pflag.FlagSet, r *renderer) error {
	resp, err := client.GetConsolidationStatus(ctx, &contract.EmptyRequest{})
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

// runForgottenList reads the forgotten log - what the sleep cycle actually deleted, as against
// PreviewConsolidation's account of what it would delete next.
func runForgottenList(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	since, err := parseTime(fs, "since")
	if err != nil {
		return err
	}

	until, err := parseTime(fs, "until")
	if err != nil {
		return err
	}

	rule, err := forgetRuleFromFlag(fs, "rule")
	if err != nil {
		return err
	}

	resp, err := client.GetForgottenMemories(ctx, &contract.GetForgottenMemoriesRequest{
		MemoryId: str(fs, "memory-id"),
		EventId:  str(fs, "event-id"),
		Group:    str(fs, "group"),
		Rule:     rule,
		Since:    since,
		Until:    until,
		AfterSeq: i64(fs, "after-seq"),
		Limit:    i32(fs, "limit"),
	})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// runForgottenClear empties the log. It requires --before or --all for the same reason the RPC
// does: this is the operation that destroys the record of what was destroyed, and it must never be
// something a bare command does by default.
func runForgottenClear(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	before, err := parseTime(fs, "before")
	if err != nil {
		return err
	}

	all := b(fs, "all")

	if before == 0 && !all {
		return fmt.Errorf("clearing the forgotten log requires --before or --all")
	}

	resp, err := client.DeleteForgottenMemories(ctx, &contract.DeleteForgottenMemoriesRequest{
		BeforeTime: before,
		All:        all,
	})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// forgetRuleFromFlag maps the --rule flag onto the wire enum; an empty flag means either rule.
func forgetRuleFromFlag(fs *pflag.FlagSet, name string) (contract.ForgetRule, error) {
	switch strings.ToLower(str(fs, name)) {

	case "":
		return contract.ForgetRule_FORGET_RULE_UNSPECIFIED, nil

	case "consolidation":
		return contract.ForgetRule_FORGET_RULE_CONSOLIDATION, nil

	case "eviction":
		return contract.ForgetRule_FORGET_RULE_EVICTION, nil

	}

	return 0, fmt.Errorf("invalid --%s %q (want consolidation or eviction)", name, str(fs, name))
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

	metadata, err := parseMetadata(strs(fs, "metadata"))
	if err != nil {
		return nil, err
	}

	return &contract.Memory{
		Id:            id,
		Body:          body,
		Significance:  i32(fs, "significance"),
		EventId:       str(fs, "event-id"),
		Group:         str(fs, "group"),
		TimeStamp:     timestamp,
		Placement:     place,
		Metadata:      metadata,
		ClearMetadata: b(fs, "clear-metadata"),
		ClearGroup:    b(fs, "clear-group"),
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

// orderDirFromFlags maps the --order-dir flag onto the SortDirection enum. An omitted flag leaves it
// UNSPECIFIED, which the service reads as the sort field's own natural direction rather than as
// ascending - see GetMemoriesRequest.order_by for which that is per field.
func orderDirFromFlags(fs *pflag.FlagSet) (contract.SortDirection, error) {
	switch value := str(fs, "order-dir"); value {

	case "":
		return contract.SortDirection_SORT_DIRECTION_UNSPECIFIED, nil

	case "asc":
		return contract.SortDirection_SORT_DIRECTION_ASC, nil

	case "desc":
		return contract.SortDirection_SORT_DIRECTION_DESC, nil

	default:
		return 0, fmt.Errorf("invalid --order-dir %q (expected 'asc' or 'desc')", value)
	}
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

// linksFromFlags parses the repeatable --link 'id:significance' flag, used by the create commands
// and by memory/event link.
func linksFromFlags(fs *pflag.FlagSet) ([]*contract.Link, error) {
	return parseLinks(strs(fs, "link"))
}

// parseMetadata turns repeated 'key=value' entries into the metadata map a write carries, splitting
// on the FIRST '=' so a value may contain one. It is the write-side counterpart of the filter flags,
// which pass the same strings through untouched - the RPC's list routes take them packed because a
// map cannot be bound from a URL query string.
func parseMetadata(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	out := make(map[string]string, len(raw))

	for _, entry := range raw {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid --metadata %q (want 'key=value')", entry)
		}

		if existing, seen := out[key]; seen && existing != value {
			return nil, fmt.Errorf("--metadata %q was given twice with different values", key)
		}

		out[key] = value
	}

	return out, nil
}

// triStateFromFlag reads a 'true'/'false' string flag as the contract's tri-state, leaving it unset
// when the flag is empty. A pflag bool could not express this: --summary=false and an omitted
// --summary would both arrive as false, so "only the non-summaries" would be unaskable.
func triStateFromFlag(fs *pflag.FlagSet, name string) (contract.Bool, error) {
	switch strings.ToLower(strings.TrimSpace(str(fs, name))) {

	case "":
		return contract.Bool_UNSPECIFIED, nil

	case "true":
		return contract.Bool_TRUE, nil

	case "false":
		return contract.Bool_FALSE, nil

	default:
		return contract.Bool_UNSPECIFIED, fmt.Errorf("--%s must be 'true' or 'false'", name)

	}
}

// parseLinks turns 'id:significance' entries into Links. A missing significance is an error rather
// than a default: a link's weight is what it does to the decay maths, and silently choosing one for
// the caller would be choosing how hard something is to forget.
func parseLinks(raw []string) ([]*contract.Link, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	out := make([]*contract.Link, 0, len(raw))

	for _, entry := range raw {
		id, sigText, ok := strings.Cut(entry, ":")
		if !ok || id == "" {
			return nil, fmt.Errorf("invalid --link %q (want 'id:significance')", entry)
		}

		// Parsed at the contract's own width rather than as an int and narrowed: on a 64-bit
		// platform int32(strconv.Atoi(...)) truncates silently, and 4294967296 would arrive at the
		// server as a perfectly valid link significance of 0.
		sig, err := strconv.ParseInt(sigText, 10, 32)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf("invalid --link %q: significance %q is out of range (must fit in a 32-bit integer)", entry, sigText)
			}

			return nil, fmt.Errorf("invalid --link %q: significance %q is not an integer", entry, sigText)
		}

		out = append(out, &contract.Link{Id: id, Significance: int32(sig)})
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

func i64(fs *pflag.FlagSet, name string) int64 {
	value, _ := fs.GetInt64(name)

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
