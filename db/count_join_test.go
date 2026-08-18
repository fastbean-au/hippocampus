package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestFilteredCountsMatchTheJoinedForm is the guard the count optimisation needs.
//
// CountMemoriesFiltered and CountEventsFiltered now count against the bare table whenever the filter
// asks nothing of significance, skipping the LEFT JOIN that only supplies that one column. The risk
// is not that the join is needed - it is that some future predicate starts reading a column
// memoriesFrom renames or synthesises, at which point the two forms silently disagree and a listing
// reports a total that does not match its own rows.
//
// So every case runs both forms over the same seeded store and requires the same answer, including
// the significance filters that must still take the joined path.
func TestFilteredCountsMatchTheJoinedForm(t *testing.T) {
	// Run against every driver that is available, not just SQLite. The bare-table path is the one
	// place a dialect could disagree with the joined form without anything failing: MySQL compares
	// group_name and metadata under an explicit binary collation, and a subquery that carried a
	// different one would make the two counts differ on case alone. The server drivers skip
	// themselves when their DSN is unset, exactly as the rest of the package's integration tests do.
	drivers := map[string]func(t *testing.T) *DB{
		"sqlite":   newTestDB,
		"postgres": newPostgresTestDB,
		"mysql":    newMySQLTestDB,
	}

	for name, open := range drivers {
		t.Run(name, func(t *testing.T) {
			filteredCountsMatchTheJoinedForm(t, open(t))
		})
	}
}

func filteredCountsMatchTheJoinedForm(t *testing.T, d *DB) {
	t.Helper()

	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	now := time.Now().UnixNano()

	// Two groups, two significances, half attached to an event, one summary, one binary, one
	// recalled - so each predicate below has both matching and non-matching rows to separate.
	if _, err := d.CreateEvent(ctx, types.Event{
		Id:           "event-1",
		TimeStart:    now,
		Significance: 60,
		Name:         "an event",
		Group:        "group-a",
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := d.CreateEvent(ctx, types.Event{
		Id:           "event-2",
		TimeStart:    now,
		Significance: 20,
		Name:         "another event",
		Group:        "group-b",
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for i := 0; i < 12; i++ {
		memory := types.Memory{
			Id:           fmt.Sprintf("memory-%02d", i),
			TimeStamp:    now + int64(i),
			Significance: int32(10 + (i%3)*30),
			Body:         fmt.Sprintf("body %d", i),
			Group:        fmt.Sprintf("group-%c", 'a'+rune(i%2)),
			Metadata:     map[string]string{"source": fmt.Sprintf("source-%d", i%3)},
		}

		if i%2 == 0 {
			memory.EventId = "event-1"
		}

		if _, err := d.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	if _, err := d.RecallMemories(ctx, []string{"memory-00", "memory-01"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	memoryCases := []struct {
		name   string
		filter MemoryFilter
	}{
		{"unfiltered", MemoryFilter{}},
		{"group", MemoryFilter{Group: "group-a"}},
		{"scope", MemoryFilter{Groups: []string{"group-b"}}},
		{"scope and group", MemoryFilter{Group: "group-a", Groups: []string{"group-a", "group-b"}}},
		{"time range", MemoryFilter{TimeStampMin: now + 4, TimeStampMax: now + 9}},
		{"event", MemoryFilter{EventId: "event-1"}},
		{"ids", MemoryFilter{Ids: []string{"memory-00", "memory-03", "memory-11"}}},
		{"metadata", MemoryFilter{Metadata: map[string]string{"source": "source-1"}}},
		{"recalled", MemoryFilter{Recalled: TriStateTrue}},
		{"not recalled", MemoryFilter{Recalled: TriStateFalse}},
		// The three that must still take the joined path.
		{"significance min", MemoryFilter{SignificanceMin: 40}},
		{"significance max", MemoryFilter{SignificanceMax: 40}},
		{"significance extremum", MemoryFilter{SignificanceExtremum: SignificanceExtremumHighest}},
		{"significance and group", MemoryFilter{SignificanceMin: 40, Group: "group-a"}},
	}

	for _, c := range memoryCases {
		t.Run("memories/"+c.name, func(t *testing.T) {
			got, err := d.CountMemoriesFiltered(ctx, c.filter)
			if err != nil {
				t.Fatalf("CountMemoriesFiltered: %s", err)
			}

			where, args := d.memoryFilterConditions(c.filter)

			var want int

			if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+memoriesFrom+where, args...).Scan(&want); err != nil {
				t.Fatalf("joined count: %s", err)
			}

			if got != want {
				t.Errorf("count disagrees with the joined form: got %d, want %d", got, want)
			}

			// A count nothing matches would pass the comparison above while testing nothing, so the
			// cases are also required to be discriminating.
			if want == 0 {
				t.Errorf("filter matched nothing - the case proves nothing")
			}
		})
	}

	eventCases := []struct {
		name   string
		filter EventFilter
	}{
		{"unfiltered", EventFilter{}},
		{"group", EventFilter{Group: "group-a"}},
		{"scope", EventFilter{Groups: []string{"group-b"}}},
		{"time range", EventFilter{TimeStartMin: now - 1, TimeStartMax: now + 1}},
		{"significance min", EventFilter{SignificanceMin: 40}},
		{"significance extremum", EventFilter{SignificanceExtremum: SignificanceExtremumLowest}},
	}

	for _, c := range eventCases {
		t.Run("events/"+c.name, func(t *testing.T) {
			got, err := d.CountEventsFiltered(ctx, c.filter)
			if err != nil {
				t.Fatalf("CountEventsFiltered: %s", err)
			}

			where, args := d.eventFilterConditions(c.filter)

			var want int

			if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+eventsFrom+where, args...).Scan(&want); err != nil {
				t.Fatalf("joined count: %s", err)
			}

			if got != want {
				t.Errorf("count disagrees with the joined form: got %d, want %d", got, want)
			}

			if want == 0 {
				t.Errorf("filter matched nothing - the case proves nothing")
			}
		})
	}
}
