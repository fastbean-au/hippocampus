package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docs/cli.md lists the commands in five tables, and that listing is the only place a user meets
// the surface - `hippo` prints its own usage, but nobody reads usage to find out that a command
// exists. A command absent from the tables is therefore a command nobody runs: `status`
// (GetConsolidationStatus) shipped and stayed unmentioned, so the one RPC that answers "when is the
// next cycle" was reachable from the console and from nowhere else a person would look.
//
// This is the CLI's counterpart to the MCP bridge's TestEveryToolIsDocumented, and it compares in
// both directions for the same reason: a table naming a command that no longer exists sends a
// reader to an error.

// documentedCommandPattern matches the first cell of a command table row.
var documentedCommandPattern = regexp.MustCompile("^\\|\\s*`([a-z][a-z-]*(?: [a-z-]+)?)`\\s*\\|")

// undocumentedCommands are registered but deliberately absent from the tables, with the reason.
var undocumentedCommands = map[string]string{
	"completion": "documented as a section of its own (Shell completion), which shows the per-shell invocation a table cell cannot; the hidden __complete callback it generates is not a registered command at all",
}

func TestEveryCommandIsDocumented(t *testing.T) {
	documented := documentedCommands(t)

	for name := range commands() {
		if _, excused := undocumentedCommands[name]; excused {
			continue
		}

		if !documented[name] {
			t.Errorf("command '%s' is not in any command table in docs/cli.md", name)
		}
	}

	registered := commands()

	for name := range documented {
		if _, present := registered[name]; !present {
			t.Errorf("docs/cli.md documents '%s', which is not a registered command", name)
		}
	}
}

// TestNoStaleUndocumentedCommands rejects an exception for a command that no longer exists.
func TestNoStaleUndocumentedCommands(t *testing.T) {
	registered := commands()

	for name, reason := range undocumentedCommands {
		if _, present := registered[name]; !present {
			t.Errorf("'%s' is excused from the documentation check ('%s') but is no longer a "+
				"command - remove the exception", name, reason)
		}
	}
}

func documentedCommands(t *testing.T) map[string]bool {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatalf("failed to read the CLI guide: %s", err.Error())
	}

	names := make(map[string]bool)

	for _, line := range strings.Split(string(source), "\n") {
		match := documentedCommandPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		names[match[1]] = true
	}

	if len(names) == 0 {
		t.Fatal("found no command table rows in docs/cli.md - the tables' shape changed")
	}

	return names
}
