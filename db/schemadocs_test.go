package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The row-layout table in docs/consolidation.md ("What a sleep cycle reads") is a second copy of
// the memories schema, and it is the copy a reader consults to understand why a sleep cycle costs
// what it does. Its claim is not merely that the columns exist but that six specific ones are in
// the covering index and the rest are not - which is the whole argument for the scans never
// reading a body. A column added to the table without being added to the diagram would leave the
// page quietly describing a row shape the service stopped having, and a column that joined or left
// the index would leave it asserting the opposite of the truth while still looking plausible.
//
// So the page is held to memoryStoredColumns and coveringIndexColumns, which are themselves what
// the INSERT and the CREATE INDEX are built from. There is no third place for the three to drift
// apart in.

// documentedColumnPattern matches one row of that table: the column name in inline code, then its
// band, then its covering-index membership.
var documentedColumnPattern = regexp.MustCompile("^\\|\\s*`(\\w+)`\\s*\\|([^|]*)\\|([^|]*)\\|")

func TestDocumentedRowMatchesTheSchema(t *testing.T) {
	documented := documentedMemoryColumns(t)
	stored := namedColumns(memoryStoredColumns)
	covering := namedColumns(coveringIndexColumns)

	// link_significance is a physical column of the table but deliberately absent from
	// memoryStoredColumns, being maintained by the link graph rather than supplied by a write.
	stored["link_significance"] = true

	for column, membership := range documented {
		switch {

		case !stored[column]:
			t.Errorf("the row-layout table in docs/consolidation.md carries '%s', which is not a "+
				"column of the memories table - it is either misspelled or no longer stored", column)

		case membership == "primary key" && column != "id":
			t.Errorf("column '%s' is documented as the primary key, which is 'id'", column)

		case membership != "primary key" && covering[column] != (membership == "yes"):
			t.Errorf("column '%s' is documented as '%s' for covering-index membership, but %s "+
				"the covering index (%s)", column, membership, indexVerdict(covering[column]),
				coveringIndexName)

		}
	}

	for column := range stored {
		if _, present := documented[column]; !present {
			t.Errorf("column '%s' is missing from the row-layout table in docs/consolidation.md; "+
				"the page describes a memories row that no longer matches the schema", column)
		}
	}
}

// indexVerdict phrases a membership for the failure message above.
func indexVerdict(in bool) string {
	if in {
		return "it is in"
	}

	return "it is not in"
}

// documentedMemoryColumns reads the table, returning each documented column against the
// covering-index membership the page claims for it.
func documentedMemoryColumns(t *testing.T) map[string]string {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "docs", "consolidation.md"))
	if err != nil {
		t.Fatalf("failed to read the consolidation guide: %s", err.Error())
	}

	columns := map[string]string{}

	for _, line := range strings.Split(string(source), "\n") {
		row := documentedColumnPattern.FindStringSubmatch(line)
		if row == nil {
			continue
		}

		columns[row[1]] = strings.TrimSpace(row[3])
	}

	if len(columns) == 0 {
		t.Fatal("found no row-layout table rows in docs/consolidation.md - the table's shape changed")
	}

	return columns
}

// namedColumns splits a column list - an INSERT's or a CREATE INDEX's, parenthesised or not - into
// a set of the names it carries.
func namedColumns(list string) map[string]bool {
	columns := map[string]bool{}

	for _, column := range strings.Split(strings.Trim(strings.TrimSpace(list), "()"), ",") {
		if name := strings.TrimSpace(column); name != "" {
			columns[name] = true
		}
	}

	return columns
}
