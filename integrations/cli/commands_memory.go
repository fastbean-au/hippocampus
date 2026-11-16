package main

import (
	"github.com/spf13/pflag"
)

// memoryCommands returns the commands that write, read and summarise memories.
func memoryCommands() map[string]command {
	return map[string]command{
		"memory store": {
			summary: "store a new memory",
			hint:    "--body <text> [--significance N] [--event-id ID] [--group G] [--metadata k=v]",
			flags:   memoryWriteFlags,
			run:     runMemoryStore,
		},
		"memory store-batch": {
			summary: "store many memories from a JSON StoreMemoriesRequest file",
			hint:    "--file PATH ('-' for stdin)",
			flags: func(fs *pflag.FlagSet) {
				fs.String("file", "", "JSON StoreMemoriesRequest file, or '-' for stdin (required)")
			},
			run: runMemoryStoreBatch,
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
		"memory delete-by-filter": {
			summary: "delete every memory matching a filter (destructive)",
			hint:    "--yes [--group G] [--metadata k=v] [--significance-max N] [--max-deletions N] [--delete-empty-events]",
			flags:   memoryDeleteFilterFlags,
			run:     runMemoryDeleteByFilter,
		},
		"memory list": {
			summary: "list memories with optional filters",
			hint:    "[--id ID ...] [--group G] [--metadata k=v] [--recalled false] [--significance-min N] [--limit N]",
			flags: func(fs *pflag.FlagSet) {
				memoryFilterFlags(fs)

				// Listing-only: delete-by-filter shares memoryFilterFlags and selects by predicate,
				// never by id.
				fs.StringSlice("id", nil, "restrict to this memory id (repeatable, at most 200): reads by id WITHOUT reinforcing, unlike 'memory recall'")
			},
			run: runMemoryList,
		},
		"memory recall": {
			summary: "recall memories by id (reinforces them)",
			hint:    "--id ID [--id ID ...] | ID [ID ...] [--include-linked]",
			flags: func(fs *pflag.FlagSet) {
				fs.StringSlice("id", nil, "memory id to recall (repeatable; ids may also be positional args)")
				fs.Bool("include-linked", false, "also return the memories linked to those recalled, one hop, in either direction; they are an associative recall and are not themselves reinforced")
			},
			run: runMemoryRecall,
		},
		"memory search": {
			summary: "search memories via the content-search index",
			hint:    "--query <text> [--mode keyword|semantic|hybrid] [--metadata k=v] [--limit N] [--reinforce] [--include-linked]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("query", "", "search query (required)")
				fs.Int32("limit", 0, "maximum results (0 selects the server default)")
				fs.String("event-id", "", "restrict matches to a single event")
				fs.String("group", "", "restrict matches to a group label")
				fs.StringSlice("metadata", nil, "restrict matches to memories carrying this 'key=value' label (repeatable; all must match)")
				fs.Bool("reinforce", false, "route matches through recall, reinforcing them")
				fs.String("mode", "keyword", "how to match: keyword, semantic, or hybrid (semantic and hybrid need the service to have an embedding model and OpenSearch)")
				fs.Bool("include-linked", false, "also return the memories linked to each match, one hop, in either direction; appended after the ranked matches and never counted as matches themselves")
				fs.Int32("offset", 0, "matches to skip, for the next page of the ranked results (at most 1000)")
				fs.String("timestamp-min", "", "only matches stored at or after this time (RFC3339)")
				fs.String("timestamp-max", "", "only matches stored at or before this time (RFC3339)")
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
	}
}
