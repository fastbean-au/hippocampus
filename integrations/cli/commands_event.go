package main

import (
	"github.com/spf13/pflag"
)

// eventCommands returns the commands that write and read events.
func eventCommands() map[string]command {
	return map[string]command{
		"event create": {
			summary: "create a new event",
			hint:    "--name <name> [--significance N] [--group G] [--time-start RFC3339]",
			flags:   eventWriteFlags,
			run:     runEventCreate,
		},
		"event update": {
			summary: "apply a partial update to an existing event",
			hint:    "--id ID [--name N] [--description D] [--significance N] [--group G] [--metadata k=v] [--clear-metadata]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to update (required)")
				eventContentFlags(fs)
				addPlacementFlags(fs)
			},
			run: runEventUpdate,
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
			hint:    "--id ID [--memories | --if-empty]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to delete (required)")
				fs.Bool("memories", false, "also delete the event's memories (otherwise they are detached)")
				fs.Bool("if-empty", false, "delete the event only while it holds none of your memories; otherwise fail and change nothing")
			},
			run: runEventDelete,
		},
		"event get": {
			summary: "fetch a single event by id",
			hint:    "--id ID [--memories|--memory-counts] [--links]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("id", "", "id of the event to fetch (required)")
				fs.Bool("memories", false, "also load the event's memories")
				fs.Bool("memory-counts", false, "report how many memories the event holds, without transferring them")
				fs.Bool("links", false, "also load the event's outbound links")
			},
			run: runEventGet,
		},
		"event delete-by-filter": {
			summary: "delete every event matching a filter (destructive)",
			hint:    "--yes [--group G] [--ended true] [--name-contains S] [--max-deletions N] [--delete-memories]",
			flags:   eventDeleteFilterFlags,
			run:     runEventDeleteByFilter,
		},
		"event list": {
			summary: "list events with optional filters",
			hint:    "[--group G] [--ended false] [--name-contains T] [--linked-to ID] [--limit N] [--memories|--memory-counts]",
			flags:   eventFilterFlags,
			run:     runEventList,
		},
	}
}
