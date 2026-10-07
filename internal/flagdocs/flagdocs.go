// Package flagdocs holds a command's flags to the documentation page that describes it (TODO-3 item
// 175). It exists for the integration commands - the broker bridges, the ingestor and the
// object-storage agents - whose flags are their whole interface, and whose pages had fallen behind
// them: an operator reads the page, not --help, to find out that a flag exists, so a flag missing
// from it is a flag nobody sets.
//
// It is a test helper, and lives under internal/ so that only this repository's modules can import
// it; Go's internal rule is by import path, so the integration modules qualify.
package flagdocs

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// documentedFlagPattern matches a backticked flag in a table cell: `--name`, or `--prefix*` for a
// family such as `--source-tls*`.
var documentedFlagPattern = regexp.MustCompile("`--([a-z0-9][a-z0-9-]*)(\\*?)`")

// Documented reports, as test errors, every flag in fs that no table row on the page names. A row
// is any line beginning with '|'; a flag counts as named when the row carries it in backticks, or
// carries a `--prefix*` family the flag belongs to. except lists flags deliberately left out, with
// the reason, and an entry naming a flag fs does not define fails too, so the list cannot go stale.
func Documented(t *testing.T, fs *pflag.FlagSet, page string, except map[string]string) {
	t.Helper()

	source, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("reading %s: %s", page, err)
	}

	names := map[string]bool{}
	prefixes := []string{}

	for _, line := range strings.Split(string(source), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}

		for _, match := range documentedFlagPattern.FindAllStringSubmatch(line, -1) {
			if match[2] == "*" {
				prefixes = append(prefixes, match[1])

				continue
			}

			names[match[1]] = true
		}
	}

	var missing []string

	fs.VisitAll(func(flag *pflag.Flag) {
		if _, excused := except[flag.Name]; excused || names[flag.Name] {
			return
		}

		for _, prefix := range prefixes {
			if strings.HasPrefix(flag.Name, prefix) {
				return
			}
		}

		missing = append(missing, flag.Name)
	})

	sort.Strings(missing)

	for _, name := range missing {
		t.Errorf("--%s is not in any flag table in %s: document it there", name, page)
	}

	for name, reason := range except {
		if fs.Lookup(name) == nil {
			t.Errorf("--%s is excused from %s (%q) but is no longer a flag - remove the exception", name, page, reason)
		}
	}
}
