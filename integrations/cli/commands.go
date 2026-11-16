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

// commandAreas are the per-area halves of the registry, each in its own commands_<area>.go file.
// Their keys are disjoint; TestCommandAreasAreDisjoint holds that, since merging would otherwise
// let one area's entry silently replace another's.
var commandAreas = []func() map[string]command{
	memoryCommands,
	linkCommands,
	eventCommands,
	adminCommands,
	transferCommands,
}

// commands is the full command registry keyed by its invocation path ("memory store", "event
// list", "whoami", ...). Dispatch matches the two-word key first, then the one-word key.
func commands() map[string]command {
	out := make(map[string]command)

	for _, area := range commandAreas {
		for k, v := range area() {
			out[k] = v
		}
	}

	return out
}

// --- shared flag registration ---------------------------------------------------------------------

// memoryContentFlags registers the content fields shared by memory store/update and summary replace.
func memoryContentFlags(fs *pflag.FlagSet) {
	fs.String("body", "", "memory body text")
	fs.String("body-file", "", "read the body from a file, or '-' for stdin")
	fs.Int32("significance", 0, "significance; 0 leaves it unranked (or unchanged on update)")
	fs.String("event-id", "", "associate the memory with an event")
	fs.String("group", "", "freeform grouping/context label")
	fs.Int64("external-bytes", 0, "size of a payload held in another system that this memory points at; feeds consolidation.capacityExternalBytes (0 leaves it unchanged on update)")
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

// eventContentFlags is the set of event fields a create and an update both take, split out for the
// reason memoryContentFlags is: the two commands share every field, and the pair of clearing flags
// is meaningful only on the update - exactly as it is for memories, where StoreMemory ignores them.
func eventContentFlags(fs *pflag.FlagSet) {
	fs.String("name", "", "event name (required on create)")
	fs.String("description", "", "event description")
	fs.Int32("significance", 0, "significance; 0 leaves it unranked (or unchanged on update)")
	fs.String("group", "", "freeform grouping/context label")
	fs.String("time-start", "", "start time as RFC3339 (defaults to now on create)")
	fs.String("time-end", "", "end time as RFC3339 (0/unset means not ended)")
	fs.StringSlice("metadata", nil, "metadata label as 'key=value' (repeatable)")
	fs.Bool("clear-metadata", false, "on update, remove all metadata (an empty --metadata means 'leave unchanged')")
	fs.Bool("clear-group", false, "on update, reset the group to empty (an empty --group means 'leave unchanged')")
}

// eventWriteFlags is the event-create flag set: content plus id, links, and placement.
func eventWriteFlags(fs *pflag.FlagSet) {
	fs.String("id", "", "event id (auto-generated UUID when omitted)")
	eventContentFlags(fs)
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

// memoryDeleteFilterFlags and eventDeleteFilterFlags are the listing filters minus everything that
// only shapes a page - order-by, order-dir, limit, offset, links - plus the two flags a deletion
// has of its own. They are separate registrars rather than the listing's, because reusing it would
// offer a --limit that looks like a bound on the deletion and is not one.
func memoryDeleteFilterFlags(fs *pflag.FlagSet) {
	fs.Bool("yes", false, "confirm the irreversible deletion")
	memorySelectionFlags(fs)
	fs.Int64("max-deletions", 0, "stop after deleting this many memories (0 = no bound)")
	fs.Bool("delete-empty-events", false, "also delete each event the deletion leaves with no memories")
}

// memorySelectionFlags are the predicate a deletion, an export and a transfer all select memories
// by - the contract's MemorySelection, and the listing's filters minus everything that only shapes
// a page - so 'hippo memory list' with the same flags is the dry run of each.
func memorySelectionFlags(fs *pflag.FlagSet) {
	fs.String("timestamp-min", "", "inclusive lower bound on time_stamp (RFC3339)")
	fs.String("timestamp-max", "", "inclusive upper bound on time_stamp (RFC3339)")
	fs.Int32("significance-min", 0, "inclusive lower bound on significance (0 = no bound)")
	fs.Int32("significance-max", 0, "inclusive upper bound on significance (0 = no bound)")
	fs.String("group", "", "restrict to a group label")
	fs.String("extremum", "", "'highest' or 'lowest' significance tie (ignores the significance range)")
	fs.StringSlice("metadata", nil, "restrict to memories carrying this 'key=value' label (repeatable; all must match)")
	fs.String("recalled", "", "'true' for memories recalled at least once, 'false' for those never recalled")
	fs.String("summary", "", "'true' for summary memories only, 'false' to exclude them")
	fs.String("binary", "", "'true' for binary memories only, 'false' to exclude them")
	fs.Int32("recall-count-min", 0, "inclusive lower bound on recall_count (0 = no bound)")
	fs.Int32("recall-count-max", 0, "inclusive upper bound on recall_count (0 = no bound)")
	fs.String("recalled-after", "", "inclusive lower bound on time_recalled (RFC3339); never-recalled memories are excluded")
	fs.String("recalled-before", "", "inclusive upper bound on time_recalled (RFC3339); never-recalled memories are excluded")
	fs.String("event", "", "restrict to one event's memories")
	fs.String("has-event", "", "'true' for memories associated with an event, 'false' for those with none")
}

func eventDeleteFilterFlags(fs *pflag.FlagSet) {
	fs.Bool("yes", false, "confirm the irreversible deletion")
	fs.String("time-start-min", "", "inclusive lower bound on time_start (RFC3339)")
	fs.String("time-start-max", "", "inclusive upper bound on time_start (RFC3339)")
	fs.String("time-end-min", "", "inclusive lower bound on time_end (RFC3339)")
	fs.String("time-end-max", "", "inclusive upper bound on time_end (RFC3339)")
	fs.Int32("significance-min", 0, "inclusive lower bound on significance (0 = no bound)")
	fs.Int32("significance-max", 0, "inclusive upper bound on significance (0 = no bound)")
	fs.String("group", "", "restrict to a group label")
	fs.String("extremum", "", "'highest' or 'lowest' significance tie (ignores the significance range)")
	fs.StringSlice("metadata", nil, "restrict to events carrying this 'key=value' label (repeatable; all must match)")
	fs.String("ended", "", "'true' for events that have ended, 'false' for those still running")
	fs.String("name-contains", "", "restrict to events whose name contains this substring (case-insensitive)")
	fs.String("description-contains", "", "restrict to events whose description contains this substring (case-insensitive)")
	fs.Int64("max-deletions", 0, "stop after deleting this many events (0 = no bound)")
	fs.Bool("delete-memories", false, "delete each event's memories with it; without this they survive with no event")
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
	fs.String("ended", "", "'true' for events that have ended, 'false' for those still running")
	fs.String("name-contains", "", "restrict to events whose name contains this substring (case-insensitive)")
	fs.String("description-contains", "", "restrict to events whose description contains this substring (case-insensitive)")
	fs.String("linked-to", "", "restrict to the events one hop from this event id, in either direction")
	fs.Bool("links", false, "include each event's outbound links")
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

// runMemoryStoreBatch sends a batch through the write path. It takes a file rather than flags
// because a batch of unrelated memories has nothing in common to express as flags - which is the
// same reason import-batch takes one.
func runMemoryStoreBatch(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	path := str(fs, "file")
	if path == "" {
		return fmt.Errorf("--file is required")
	}

	data, err := readFileOrStdin(path)
	if err != nil {
		return err
	}

	req := &contract.StoreMemoriesRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, req); err != nil {
		return fmt.Errorf("failed to parse %s as a StoreMemoriesRequest: %w", path, err)
	}

	resp, err := client.StoreMemories(ctx, req)
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

		Ids: strs(fs, "id"),
	}

	resp, err := client.GetMemories(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

// runMemoryDeleteByFilter deletes by predicate. --yes is required for the reason purge requires it:
// the request is not a list of ids the operator has read, so a mistyped filter is not visible in
// the command itself. `hippo memory list` with the same filter flags is the dry run - the service
// builds one predicate for both.
func runMemoryDeleteByFilter(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	if !b(fs, "yes") {
		return fmt.Errorf("deleting by filter is irreversible; run 'hippo memory list' with the same filters to see what matches, then re-run with --yes")
	}

	sel, err := memorySelectionFromFlags(fs)
	if err != nil {
		return err
	}

	resp, err := client.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{
		TimestampMin:         sel.GetTimestampMin(),
		TimestampMax:         sel.GetTimestampMax(),
		SignificanceMin:      sel.GetSignificanceMin(),
		SignificanceMax:      sel.GetSignificanceMax(),
		Group:                sel.GetGroup(),
		SignificanceExtremum: sel.GetSignificanceExtremum(),
		Metadata:             sel.GetMetadata(),
		Recalled:             sel.GetRecalled(),
		RecallCountMin:       sel.GetRecallCountMin(),
		RecallCountMax:       sel.GetRecallCountMax(),
		TimeRecalledMin:      sel.GetTimeRecalledMin(),
		TimeRecalledMax:      sel.GetTimeRecalledMax(),
		IsSummary:            sel.GetIsSummary(),
		IsBinary:             sel.GetIsBinary(),
		EventId:              sel.GetEventId(),
		HasEvent:             sel.GetHasEvent(),
		MaxDeletions:         i64(fs, "max-deletions"),
		DeleteEmptyEvents:    b(fs, "delete-empty-events"),
	})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// memorySelectionFromFlags reads memorySelectionFlags into the contract's MemorySelection. Nothing
// set yields an empty selection, which an export or transfer reads as the whole store and a
// deletion refuses.
func memorySelectionFromFlags(fs *pflag.FlagSet) (*contract.MemorySelection, error) {
	tsMin, err := parseTime(fs, "timestamp-min")
	if err != nil {
		return nil, err
	}

	tsMax, err := parseTime(fs, "timestamp-max")
	if err != nil {
		return nil, err
	}

	ext, err := extremumFromFlags(fs)
	if err != nil {
		return nil, err
	}

	recalledAfter, err := parseTime(fs, "recalled-after")
	if err != nil {
		return nil, err
	}

	recalledBefore, err := parseTime(fs, "recalled-before")
	if err != nil {
		return nil, err
	}

	recalled, err := triStateFromFlag(fs, "recalled")
	if err != nil {
		return nil, err
	}

	isSummary, err := triStateFromFlag(fs, "summary")
	if err != nil {
		return nil, err
	}

	isBinary, err := triStateFromFlag(fs, "binary")
	if err != nil {
		return nil, err
	}

	hasEvent, err := triStateFromFlag(fs, "has-event")
	if err != nil {
		return nil, err
	}

	return &contract.MemorySelection{
		TimestampMin:         tsMin,
		TimestampMax:         tsMax,
		SignificanceMin:      i32(fs, "significance-min"),
		SignificanceMax:      i32(fs, "significance-max"),
		Group:                str(fs, "group"),
		SignificanceExtremum: ext,
		Metadata:             strs(fs, "metadata"),
		Recalled:             recalled,
		RecallCountMin:       i32(fs, "recall-count-min"),
		RecallCountMax:       i32(fs, "recall-count-max"),
		TimeRecalledMin:      recalledAfter,
		TimeRecalledMax:      recalledBefore,
		IsSummary:            isSummary,
		IsBinary:             isBinary,
		EventId:              str(fs, "event"),
		HasEvent:             hasEvent,
	}, nil
}

// runEventDeleteByFilter is the events' half; `hippo event list` with the same filters is its dry
// run.
func runEventDeleteByFilter(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	if !b(fs, "yes") {
		return fmt.Errorf("deleting by filter is irreversible; run 'hippo event list' with the same filters to see what matches, then re-run with --yes")
	}

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

	ended, err := triStateFromFlag(fs, "ended")
	if err != nil {
		return err
	}

	resp, err := client.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{
		TimeStartMin:         tsStartMin,
		TimeStartMax:         tsStartMax,
		TimeEndMin:           tsEndMin,
		TimeEndMax:           tsEndMax,
		SignificanceMin:      i32(fs, "significance-min"),
		SignificanceMax:      i32(fs, "significance-max"),
		Group:                str(fs, "group"),
		SignificanceExtremum: ext,
		Metadata:             strs(fs, "metadata"),
		Ended:                ended,
		NameContains:         str(fs, "name-contains"),
		DescriptionContains:  str(fs, "description-contains"),
		MaxDeletions:         i64(fs, "max-deletions"),
		DeleteMemories:       b(fs, "delete-memories"),
	})
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

	resp, err := client.RecallMemories(ctx, &contract.RecallMemoriesRequest{
		Ids:           ids,
		IncludeLinked: b(fs, "include-linked"),
	})
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

	tsMin, err := parseTime(fs, "timestamp-min")
	if err != nil {
		return err
	}

	tsMax, err := parseTime(fs, "timestamp-max")
	if err != nil {
		return err
	}

	req := &contract.SearchMemoriesRequest{
		Query:     query,
		Limit:     i32(fs, "limit"),
		EventId:   str(fs, "event-id"),
		Reinforce: b(fs, "reinforce"),
		Group:     str(fs, "group"),
		Mode:      mode,
		Metadata:  strs(fs, "metadata"),

		IncludeLinked: b(fs, "include-linked"),

		Offset:       i32(fs, "offset"),
		TimestampMin: tsMin,
		TimestampMax: tsMax,
	}

	resp, err := client.SearchMemories(ctx, req)
	if err != nil {
		return err
	}

	return r.render(resp)
}

func runEventCreate(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	if str(fs, "name") == "" {
		return fmt.Errorf("--name is required")
	}

	event, err := eventFromFlags(fs, str(fs, "id"))
	if err != nil {
		return err
	}

	links, err := linksFromFlags(fs)
	if err != nil {
		return err
	}

	event.Links = links

	resp, err := client.StoreEvent(ctx, event)
	if err != nil {
		return err
	}

	return r.render(resp)
}

// runEventUpdate is runMemoryUpdate's counterpart, and the reason `event end` and
// `event significance` are not enough: those two reach one field each, and every other field an
// event carries was unreachable from any client until UpdateEvent landed.
func runEventUpdate(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	id := str(fs, "id")
	if id == "" {
		return fmt.Errorf("--id is required")
	}

	event, err := eventFromFlags(fs, id)
	if err != nil {
		return err
	}

	resp, err := client.UpdateEvent(ctx, event)
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

	resp, err := client.DeleteEvent(ctx, &contract.DeleteEventRequest{Id: id, Memories: b(fs, "memories"), IfEmpty: b(fs, "if-empty")})
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
		Links:        b(fs, "links"),
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

	ended, err := triStateFromFlag(fs, "ended")
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
		Ended:                ended,
		NameContains:         str(fs, "name-contains"),
		DescriptionContains:  str(fs, "description-contains"),
		LinkedTo:             str(fs, "linked-to"),
		Links:                b(fs, "links"),
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

// runSignificanceLevels lists the significance values in use. It is the anchor list the placement
// flags position against: --place-anchor names a value, and until this existed the only way to know
// which values existed was to have chosen them.
func runSignificanceLevels(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	req := &contract.GetSignificanceLevelsRequest{
		SignificanceMin: i32(fs, "significance-min"),
		SignificanceMax: i32(fs, "significance-max"),
		Limit:           i32(fs, "limit"),
		Offset:          i32(fs, "offset"),
	}

	resp, err := client.GetSignificanceLevels(ctx, req)
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

// runTopology reports the deployment as the instance understands it. The filtering is done here
// rather than server-side because the response is small and one operator wanting the disabled
// components listed should not change what another one is served.
//
// It is the terminal counterpart of the console's Deployment tab, and the more useful of the two
// when the question is "which of these two addresses is the consolidator" - which is a question
// asked from a terminal, over ssh, about a deployment whose console may be exactly what is
// unreachable.
func runTopology(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	resp, err := client.GetTopology(ctx, &contract.EmptyRequest{})
	if err != nil {
		return err
	}

	if !b(fs, "all") {
		kept := make([]*contract.TopologyNode, 0, len(resp.GetNodes()))
		ids := make(map[string]bool, len(resp.GetNodes()))

		for _, node := range resp.GetNodes() {
			if node.GetStatus() == contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED {
				continue
			}

			kept = append(kept, node)
			ids[node.GetId()] = true
		}

		// An edge to a component that is no longer listed describes a relationship the reader
		// cannot see either end of, so it goes with it.
		edges := make([]*contract.TopologyEdge, 0, len(resp.GetEdges()))

		for _, edge := range resp.GetEdges() {
			if ids[edge.GetFromId()] && ids[edge.GetToId()] {
				edges = append(edges, edge)
			}
		}

		resp.Nodes = kept
		resp.Edges = edges
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

// runCallbackQueue shows what the outbound callback queue is holding.
//
// The kind flag takes a hyphenated spelling rather than the wire enum's SHOUTING_SNAKE, matching
// what --rule does for the forgotten log: an operator types what reads as a word, and the mapping
// lives in one place.
func runCallbackQueue(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	kind, err := callbackKindFromFlag(fs, "kind")
	if err != nil {
		return err
	}

	resp, err := client.GetCallbackQueue(ctx, &contract.GetCallbackQueueRequest{
		Kind:     kind,
		AfterSeq: i64(fs, "after-seq"),
		Limit:    i32(fs, "limit"),
	})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// runCallbackClear discards pending deliveries. It requires --before or --all for the same reason
// the RPC does, and a sharper one than the forgotten log's: what this destroys is not a record of a
// notification but the notification itself, and nothing else will ever send it.
func runCallbackClear(ctx context.Context, client contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	before, err := parseTime(fs, "before")
	if err != nil {
		return err
	}

	all := b(fs, "all")

	if before == 0 && !all {
		return fmt.Errorf("clearing the callback queue requires --before or --all")
	}

	resp, err := client.DeleteCallbackQueue(ctx, &contract.DeleteCallbackQueueRequest{
		BeforeTime: before,
		All:        all,
	})
	if err != nil {
		return err
	}

	return r.render(resp)
}

// callbackKindFromFlag maps the operator-facing spelling onto the wire enum. An empty flag means
// every kind, which is what the RPC reads UNSPECIFIED as.
func callbackKindFromFlag(fs *pflag.FlagSet, name string) (contract.CallbackKind, error) {
	switch strings.ToLower(strings.TrimSpace(str(fs, name))) {

	case "":
		return contract.CallbackKind_CALLBACK_KIND_UNSPECIFIED, nil

	case "memory-forgotten", "memory":
		return contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN, nil

	case "event-forgotten", "event":
		return contract.CallbackKind_CALLBACK_KIND_EVENT_FORGOTTEN, nil

	case "sleep-completed", "sleep":
		return contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED, nil

	case "memories-at-risk", "at-risk":
		return contract.CallbackKind_CALLBACK_KIND_MEMORIES_AT_RISK, nil

	case "memory-stored", "stored":
		return contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED, nil

	case "memory-updated", "updated":
		return contract.CallbackKind_CALLBACK_KIND_MEMORY_UPDATED, nil

	}

	return 0, fmt.Errorf(
		"--%s must be memory-forgotten, event-forgotten, sleep-completed, memories-at-risk, memory-stored or memory-updated",
		name,
	)
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

	case "expiry":
		return contract.ForgetRule_FORGET_RULE_EXPIRY, nil

	}

	return 0, fmt.Errorf("invalid --%s %q (want consolidation, eviction or expiry)", name, str(fs, name))
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
	sel, err := memorySelectionFromFlags(fs)
	if err != nil {
		return err
	}

	resp, err := client.Export(ctx, &contract.ExportRequest{Clear: b(fs, "clear"), Memories: sel})
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
	sel, err := memorySelectionFromFlags(fs)
	if err != nil {
		return err
	}

	resp, err := client.Transfer(ctx, &contract.TransferRequest{Clear: b(fs, "clear"), Memories: sel})
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
		ExternalBytes: i64(fs, "external-bytes"),
		TimeStamp:     timestamp,
		Placement:     place,
		Metadata:      metadata,
		ClearMetadata: b(fs, "clear-metadata"),
		ClearGroup:    b(fs, "clear-group"),
	}, nil
}

// eventFromFlags builds an Event from the shared content flags, for the create and the update
// alike. Links are deliberately not read here: they are a create-only input (the update ignores
// them, since LinkEvents/UnlinkEvents is where the graph is edited), so the caller adds them.
func eventFromFlags(fs *pflag.FlagSet, id string) (*contract.Event, error) {
	timeStart, err := parseTime(fs, "time-start")
	if err != nil {
		return nil, err
	}

	timeEnd, err := parseTime(fs, "time-end")
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

	return &contract.Event{
		Id:            id,
		Name:          str(fs, "name"),
		Description:   str(fs, "description"),
		Significance:  i32(fs, "significance"),
		Group:         str(fs, "group"),
		TimeStart:     timeStart,
		TimeEnd:       timeEnd,
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
