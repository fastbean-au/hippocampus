package main

import (
	"github.com/spf13/pflag"
)

// linkCommands returns the commands that add, remove and list links between memories and between
// events.
func linkCommands() map[string]command {
	return map[string]command{
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
	}
}
