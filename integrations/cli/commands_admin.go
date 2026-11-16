package main

import (
	"github.com/spf13/pflag"
)

// adminCommands returns the commands that inspect and operate the service itself: identity,
// consolidation, the forgotten log, the callback queue, the purge and shell completion.
func adminCommands() map[string]command {
	return map[string]command{
		"whoami": {
			summary: "report the caller's identity and effective tier",
			flags:   func(*pflag.FlagSet) {},
			run:     runWhoAmI,
		},
		"significance levels": {
			summary: "list the significance values in use (the anchors a placement can name)",
			hint:    "[--significance-min N] [--significance-max N] [--limit N]",
			flags: func(fs *pflag.FlagSet) {
				fs.Int32("significance-min", 0, "inclusive lower bound (0 = no bound)")
				fs.Int32("significance-max", 0, "inclusive upper bound (0 = no bound)")
				fs.Int32("limit", 0, "page size (0 selects the server default of 200; capped at 1000)")
				fs.Int32("offset", 0, "values to skip for pagination")
			},
			run: runSignificanceLevels,
		},
		"status": {
			summary: "report the consolidation cycle's schedule and its last result",
			flags:   func(*pflag.FlagSet) {},
			run:     runConsolidationStatus,
		},
		"topology": {
			summary: "report the deployment this instance is part of, and each component's health",
			hint:    "[--all]",
			flags: func(fs *pflag.FlagSet) {
				fs.Bool("all", false, "include components that are not configured")
			},
			run: runTopology,
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
			hint:    "[--memory-id ID] [--group G] [--rule consolidation|eviction|expiry] [--since T] [--limit N]",
			flags: func(fs *pflag.FlagSet) {
				fs.String("memory-id", "", "a specific memory: did it exist, and when did it go")
				fs.String("event-id", "", "only memories that belonged to this event")
				fs.String("group", "", "only memories in this group")
				fs.String("rule", "", "which path took them: consolidation, eviction or expiry (default all)")
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
		"callbacks queue": {
			summary: "show the outbound callback queue",
			flags: func(fs *pflag.FlagSet) {
				fs.String("kind", "", "only this kind: memory-forgotten, event-forgotten, sleep-completed, memories-at-risk, memory-stored or memory-updated")
				fs.Int64("after-seq", 0, "pagination: the next_seq reported by the previous page")
				fs.Int32("limit", 0, "deliveries to return (default 100, max 1000)")
			},
			run: runCallbackQueue,
		},
		"watch": {
			summary:   "follow the store's changes as they commit, until interrupted (gRPC only)",
			hint:      "[--kind K]... [--resume [--after-seq N]]",
			streaming: true,
			flags: func(fs *pflag.FlagSet) {
				fs.StringSlice("kind", nil, "only this kind (repeatable): memory-stored, memory-updated, memory-forgotten, event-forgotten or sleep-completed")
				fs.Bool("resume", false, "replay the change log after --after-seq before going live (needs watch.changeLog.enabled)")
				fs.Int64("after-seq", 0, "with --resume, the last seq received; 0 replays everything the log holds")
			},
			run: runWatch,
		},
		"callbacks clear": {
			summary: "discard pending callbacks (destructive)",
			hint:    "--before T | --all",
			flags: func(fs *pflag.FlagSet) {
				fs.String("before", "", "discard deliveries queued before this RFC3339 time")
				fs.Bool("all", false, "discard every pending delivery")
			},
			run: runCallbackClear,
		},
		"purge": {
			summary: "delete every event and memory (destructive)",
			hint:    "--yes",
			flags:   func(fs *pflag.FlagSet) { fs.Bool("yes", false, "confirm the irreversible purge") },
			run:     runPurge,
		},
		"completion": {
			summary: "print a shell completion script (bash|zsh|fish)",
			hint:    "bash|zsh|fish",
			flags:   func(*pflag.FlagSet) {},
			run:     runCompletionCmd,
		},
	}
}
