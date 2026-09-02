package db

import (
	"strings"
	"testing"
)

// TestListingIndexServesTheOrdering is the assertion the index exists for: the paged listing must
// walk it rather than sort. A plan carrying "TEMP B-TREE FOR ORDER BY" means the index stopped
// matching the ORDER BY clause - which is what happens if either the columns or their DIRECTIONS
// drift from memoryOrderClauses, and which nothing else would catch, since the query stays correct
// and merely becomes O(store) again.
func TestListingIndexServesTheOrdering(t *testing.T) {
	d, err := New("")
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = d.Close() })

	rows, err := d.sql.Query(`EXPLAIN QUERY PLAN SELECT ` + memoryColumns + ` FROM ` + memoriesFrom +
		` ORDER BY ` + resolveOrder(memoryOrderClauses, "timestamp", SortDirectionNatural, defaultMemoryOrderBy) +
		` LIMIT 50`)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %s", err)
	}

	defer func() { _ = rows.Close() }()

	var plan []string

	for rows.Next() {
		var id, parent, notUsed int
		var detail string

		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan: %s", err)
		}

		plan = append(plan, detail)
	}

	joined := strings.Join(plan, " | ")

	if !strings.Contains(joined, listingIndexName) {
		t.Errorf("the timestamp listing does not use %s; plan was: %s", listingIndexName, joined)
	}

	if strings.Contains(strings.ToUpper(joined), "TEMP B-TREE") {
		t.Errorf("the timestamp listing still sorts; plan was: %s", joined)
	}
}
