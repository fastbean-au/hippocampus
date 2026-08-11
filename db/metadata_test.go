package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// memoryIds returns the ids of a result page in sorted order, so an assertion can name the expected
// set without depending on the ordering the filter's ORDER BY happens to produce.
func memoryIds(memories *[]types.Memory) []string {
	ids := make([]string, 0, len(*memories))

	for _, m := range *memories {
		ids = append(ids, m.Id)
	}

	sort.Strings(ids)

	return ids
}

func eventIds(events *[]types.Event) []string {
	ids := make([]string, 0, len(*events))

	for _, e := range *events {
		ids = append(ids, e.Id)
	}

	sort.Strings(ids)

	return ids
}

// TestMemoryMetadataRoundTrip verifies metadata survives a write and read back, including the
// characters a naive encoder would mangle, and that an empty map comes back as nil rather than an
// empty one - so a store round trip leaves a memory exactly as it was written.
func TestMemoryMetadataRoundTrip(t *testing.T) {
	db := newTestDB(t)

	metadata := map[string]string{
		"source":  "slack",
		"project": `a "quoted" value with an = sign`,
		"author":  "Ann-Sofie Ø",
	}

	if _, err := db.CreateMemory(context.Background(), types.Memory{
		Id: "m1", TimeStamp: 100, Significance: 5, Body: "body", Metadata: metadata,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := db.CreateMemory(context.Background(), types.Memory{
		Id: "m2", TimeStamp: 200, Significance: 5, Body: "no metadata",
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	got, err := db.GetMemoriesByIds(context.Background(), []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	for _, m := range *got {
		switch m.Id {

		case "m1":
			if !reflect.DeepEqual(m.Metadata, metadata) {
				t.Errorf("metadata did not round trip: %#v", m.Metadata)
			}

		case "m2":
			if m.Metadata != nil {
				t.Errorf("expected a memory written without metadata to read back nil, got %#v", m.Metadata)
			}

		}
	}
}

// TestEventMetadataRoundTrip is the event half - CreateEvent spells its INSERT column list inline
// rather than deriving it from eventStoredColumns, so it is the one most likely to be missed.
func TestEventMetadataRoundTrip(t *testing.T) {
	db := newTestDB(t)

	metadata := map[string]string{"source": "calendar", "team": "platform"}

	if _, err := db.CreateEvent(context.Background(), types.Event{
		Id: "e1", TimeStart: 100, Significance: 5, Name: "an event", Metadata: metadata,
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	got, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if !reflect.DeepEqual(got.Metadata, metadata) {
		t.Errorf("metadata did not round trip: %#v", got.Metadata)
	}
}

// TestMetadataFilterAgainstRowsWithoutMetadata is the regression that matters most in this package.
//
// The column is NULL-able rather than defaulting to the empty string - unlike group_name beside it
// - because SQLite's json_extract raises "malformed JSON" on one. Had it followed group_name's
// pattern, every row written before the migration would hold an empty string and the FIRST
// metadata-filtered query against an upgraded store would fail outright, which no fresh-database
// test would ever catch. This inserts rows through a path that sets no metadata at all and then
// filters on it.
func TestMetadataFilterAgainstRowsWithoutMetadata(t *testing.T) {
	db := newTestDB(t)

	for _, m := range []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "no metadata a"},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "no metadata b"},
	} {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// The query must succeed and simply match nothing - NULL equals nothing on any dialect.
	got, err := db.GetMemories(context.Background(), MemoryFilter{
		Metadata: map[string]string{"source": "slack"},
	})
	if err != nil {
		t.Fatalf("a metadata filter over rows without metadata must not error: %s", err)
	}

	if len(*got) != 0 {
		t.Errorf("expected no matches, got %+v", *got)
	}

	total, err := db.CountMemoriesFiltered(context.Background(), MemoryFilter{
		Metadata: map[string]string{"source": "slack"},
	})
	if err != nil {
		t.Fatalf("CountMemoriesFiltered over rows without metadata must not error: %s", err)
	}

	if total != 0 {
		t.Errorf("expected a count of 0, got %d", total)
	}

	// The same must hold for events.
	if _, err := db.CreateEvent(context.Background(), types.Event{
		Id: "e1", TimeStart: 100, Significance: 5, Name: "no metadata",
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	events, err := db.GetEvents(context.Background(), EventFilter{
		Metadata: map[string]string{"source": "slack"},
	})
	if err != nil {
		t.Fatalf("an event metadata filter over rows without metadata must not error: %s", err)
	}

	if len(*events) != 0 {
		t.Errorf("expected no event matches, got %+v", *events)
	}
}

// TestGetMemories_MetadataFilter covers the predicate itself: one pair, two pairs conjoined, a key
// present with the wrong value, and a key that is absent entirely.
func TestGetMemories_MetadataFilter(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "a", Metadata: map[string]string{"source": "slack", "project": "x"}},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "b", Metadata: map[string]string{"source": "slack", "project": "y"}},
		{Id: "m3", TimeStamp: 300, Significance: 5, Body: "c", Metadata: map[string]string{"source": "email", "project": "x"}},
		{Id: "m4", TimeStamp: 400, Significance: 5, Body: "d"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	cases := []struct {
		name   string
		filter map[string]string
		want   []string
	}{
		{"one pair", map[string]string{"source": "slack"}, []string{"m1", "m2"}},
		{"two pairs are conjoined", map[string]string{"source": "slack", "project": "x"}, []string{"m1"}},
		{"wrong value", map[string]string{"source": "carrier-pigeon"}, nil},
		{"absent key", map[string]string{"nothere": "x"}, nil},
		{"key present on some rows only", map[string]string{"project": "x"}, []string{"m1", "m3"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.GetMemories(context.Background(), MemoryFilter{Metadata: c.filter})
			if err != nil {
				t.Fatalf("GetMemories: %s", err)
			}

			// memoryIds always returns a non-nil slice, so an expectation of "nothing" is written as
			// nil and compared by length rather than by DeepEqual.
			if ids := memoryIds(got); len(ids) != len(c.want) || (len(c.want) > 0 && !reflect.DeepEqual(ids, c.want)) {
				t.Errorf("expected %v, got %v", c.want, ids)
			}

			// The count must agree with the page, or pagination would advertise a different result
			// set from the one it serves - the two share memoryFilterConditions for exactly this.
			total, err := db.CountMemoriesFiltered(context.Background(), MemoryFilter{Metadata: c.filter})
			if err != nil {
				t.Fatalf("CountMemoriesFiltered: %s", err)
			}

			if total != len(c.want) {
				t.Errorf("expected a count of %d, got %d", len(c.want), total)
			}
		})
	}
}

// TestGetEvents_MetadataFilter is the event twin of the memory filter test.
func TestGetEvents_MetadataFilter(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", TimeStart: 100, Significance: 5, Name: "a", Metadata: map[string]string{"team": "platform"}},
		{Id: "e2", TimeStart: 200, Significance: 5, Name: "b", Metadata: map[string]string{"team": "billing"}},
		{Id: "e3", TimeStart: 300, Significance: 5, Name: "c"},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	got, err := db.GetEvents(context.Background(), EventFilter{Metadata: map[string]string{"team": "platform"}})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if ids := eventIds(got); !reflect.DeepEqual(ids, []string{"e1"}) {
		t.Errorf("expected [e1], got %v", ids)
	}

	total, err := db.CountEventsFiltered(context.Background(), EventFilter{Metadata: map[string]string{"team": "platform"}})
	if err != nil {
		t.Fatalf("CountEventsFiltered: %s", err)
	}

	if total != 1 {
		t.Errorf("expected a count of 1, got %d", total)
	}
}

// TestMetadataFilterKeysWithPathCharacters pins that a key containing '.', ':', '/' or '-'
// addresses the member of that literal name. Those characters are legal in a key and are also JSON
// path syntax, so the path must quote the key - json_extract('{"a.b":"c"}', '$.a.b') is NULL where
// '$."a.b"' is "c". Getting this wrong would silently match nothing rather than error.
func TestMetadataFilterKeysWithPathCharacters(t *testing.T) {
	db := newTestDB(t)

	metadata := map[string]string{
		"a.b":         "dotted",
		"ns:key":      "colonned",
		"path/to/key": "slashed",
		"kebab-key":   "hyphenated",
	}

	if _, err := db.CreateMemory(context.Background(), types.Memory{
		Id: "m1", TimeStamp: 100, Significance: 5, Body: "body", Metadata: metadata,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	for k, v := range metadata {
		got, err := db.GetMemories(context.Background(), MemoryFilter{Metadata: map[string]string{k: v}})
		if err != nil {
			t.Fatalf("GetMemories(%s): %s", k, err)
		}

		if len(*got) != 1 || (*got)[0].Id != "m1" {
			t.Errorf("filtering on key %q did not match the memory carrying it: %+v", k, *got)
		}
	}
}

// TestMetadataFilterComposesWithSignificanceExtremum is the early-return regression.
//
// memoryFilterConditions returns early in its extremum arm, and the extremum's own subquery is
// built by recursing into the same function. A predicate added below that return would be dropped
// from the outer query AND missing from the subquery, turning "the highest significance among the
// slack memories" into "the highest significance overall, then filtered to slack" - a different
// answer, and usually an empty one. Here m3 is the store's highest but carries other metadata, so a
// dropped predicate would return m3 (or nothing) instead of m2.
func TestMetadataFilterComposesWithSignificanceExtremum(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 3, Body: "a", Metadata: map[string]string{"source": "slack"}},
		{Id: "m2", TimeStamp: 200, Significance: 7, Body: "b", Metadata: map[string]string{"source": "slack"}},
		{Id: "m3", TimeStamp: 300, Significance: 20, Body: "c", Metadata: map[string]string{"source": "email"}},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	filter := MemoryFilter{
		Metadata:             map[string]string{"source": "slack"},
		SignificanceExtremum: SignificanceExtremumHighest,
	}

	got, err := db.GetMemories(context.Background(), filter)
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Id != "m2" {
		t.Fatalf("expected only m2 (highest among the slack memories), got %+v", *got)
	}

	total, err := db.CountMemoriesFiltered(context.Background(), filter)
	if err != nil {
		t.Fatalf("CountMemoriesFiltered: %s", err)
	}

	if total != 1 {
		t.Errorf("expected a count of 1, got %d", total)
	}
}

// TestRecallStateFilterComposesWithSignificanceExtremum is the same regression for the recall-state
// predicates, which sit in the same block above the early return.
func TestRecallStateFilterComposesWithSignificanceExtremum(t *testing.T) {
	db := newTestDB(t)

	// The store's most significant memory is the recalled one, so "highest among the never-recalled"
	// is m2 while "highest overall" is m3 - a dropped predicate would return m3, or nothing.
	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 3, Body: "a"},
		{Id: "m2", TimeStamp: 200, Significance: 7, Body: "b"},
		{Id: "m3", TimeStamp: 300, Significance: 20, Body: "c", RecallCount: 1, TimeRecalled: 1_000},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	got, err := db.GetMemories(context.Background(), MemoryFilter{
		Recalled:             TriStateFalse,
		SignificanceExtremum: SignificanceExtremumHighest,
	})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Id != "m2" {
		t.Fatalf("expected only m2 (highest among the never-recalled), got %+v", *got)
	}
}

// TestGetMemories_RecallStateFilters covers the tri-state and range predicates over the recall
// columns - the half of item 58 that needed no schema change.
func TestGetMemories_RecallStateFilters(t *testing.T) {
	db := newTestDB(t)

	// Recall state is set at creation rather than by calling RecallMemories, which stamps
	// time.Now() - the predicate is what is under test, and fixed values make the range bounds
	// exact rather than relative to the wall clock.
	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "never recalled"},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "recalled once", RecallCount: 1, TimeRecalled: 5_000},
		{Id: "m3", TimeStamp: 300, Significance: 5, Body: "recalled twice", RecallCount: 2, TimeRecalled: 9_000},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	cases := []struct {
		name   string
		filter MemoryFilter
		want   []string
	}{
		// The question item 58 names: recall_count_max cannot ask it, because 0 there means "no
		// bound" under the package's usual rule.
		{"never recalled", MemoryFilter{Recalled: TriStateFalse}, []string{"m1"}},
		{"recalled at least once", MemoryFilter{Recalled: TriStateTrue}, []string{"m2", "m3"}},
		{"unset applies no restriction", MemoryFilter{Recalled: TriStateUnset}, []string{"m1", "m2", "m3"}},
		{"recall count lower bound", MemoryFilter{RecallCountMin: 2}, []string{"m3"}},
		{"recall count upper bound", MemoryFilter{RecallCountMax: 1}, []string{"m1", "m2"}},
		{"recalled since", MemoryFilter{TimeRecalledMin: 6_000}, []string{"m3"}},
		// Not [m1 m2]: a never-recalled memory has time_recalled 0, and "recalled before X" that
		// answered with memories never recalled at all would be a trap rather than a filter.
		{"recalled before", MemoryFilter{TimeRecalledMax: 6_000}, []string{"m2"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.GetMemories(context.Background(), c.filter)
			if err != nil {
				t.Fatalf("GetMemories: %s", err)
			}

			if ids := memoryIds(got); !reflect.DeepEqual(ids, c.want) {
				t.Errorf("expected %v, got %v", c.want, ids)
			}

			total, err := db.CountMemoriesFiltered(context.Background(), c.filter)
			if err != nil {
				t.Fatalf("CountMemoriesFiltered: %s", err)
			}

			if total != len(c.want) {
				t.Errorf("expected a count of %d, got %d", len(c.want), total)
			}
		})
	}
}

// TestGetMemories_RecalledBeforeExcludesNeverRecalled pins the one interaction worth stating
// outright: time_recalled is 0 for a never-recalled memory, so TimeRecalledMax alone would sweep
// them all in if the predicate were written naively. TimeRecalledMin's own comment promises the
// converse, that a positive lower bound excludes them.
func TestGetMemories_RecalledBeforeExcludesNeverRecalled(t *testing.T) {
	db := newTestDB(t)

	for _, m := range []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "never recalled"},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "recalled", RecallCount: 1, TimeRecalled: 5_000},
	} {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	got, err := db.GetMemories(context.Background(), MemoryFilter{TimeRecalledMin: 1})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if ids := memoryIds(got); !reflect.DeepEqual(ids, []string{"m2"}) {
		t.Errorf("a positive time_recalled lower bound must exclude never-recalled memories, got %v", ids)
	}
}

// TestGetMemories_IsBinaryAndIsSummaryFilters covers the other two tri-states. Both filter boolean
// columns, which are INTEGER on SQLite but BOOLEAN on the server drivers - the value is bound as a
// Go bool so one predicate is correct on all three.
func TestGetMemories_IsBinaryAndIsSummaryFilters(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "plain"},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "binary", IsBinary: true},
		{Id: "m3", TimeStamp: 300, Significance: 5, Body: "summary", IsSummary: true},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	cases := []struct {
		name   string
		filter MemoryFilter
		want   []string
	}{
		{"binary only", MemoryFilter{IsBinary: TriStateTrue}, []string{"m2"}},
		{"non-binary only", MemoryFilter{IsBinary: TriStateFalse}, []string{"m1", "m3"}},
		{"summaries only", MemoryFilter{IsSummary: TriStateTrue}, []string{"m3"}},
		{"non-summaries only", MemoryFilter{IsSummary: TriStateFalse}, []string{"m1", "m2"}},
		{"both unset", MemoryFilter{}, []string{"m1", "m2", "m3"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.GetMemories(context.Background(), c.filter)
			if err != nil {
				t.Fatalf("GetMemories: %s", err)
			}

			if ids := memoryIds(got); !reflect.DeepEqual(ids, c.want) {
				t.Errorf("expected %v, got %v", c.want, ids)
			}
		})
	}
}

// TestGetMemories_EventFilters covers the paged way to read one event's memories, and the tri-state
// beside it. The pair exists because an event-less memory stores an empty event_id, which is also
// EventId's "no bound" value - so one field cannot ask both questions, exactly as Recalled cannot be
// folded into RecallCountMax.
func TestGetMemories_EventFilters(t *testing.T) {
	db := newTestDB(t)

	for _, e := range []types.Event{
		{Id: "e1", TimeStart: 50, Significance: 5, Name: "one"},
		{Id: "e2", TimeStart: 60, Significance: 5, Name: "two"},
	} {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "first of e1", EventId: "e1"},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "second of e1", EventId: "e1"},
		{Id: "m3", TimeStamp: 300, Significance: 5, Body: "of e2", EventId: "e2"},
		{Id: "m4", TimeStamp: 400, Significance: 5, Body: "no event at all"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	cases := []struct {
		name   string
		filter MemoryFilter
		want   []string
	}{
		{"one event's memories", MemoryFilter{EventId: "e1"}, []string{"m1", "m2"}},
		{"another event's", MemoryFilter{EventId: "e2"}, []string{"m3"}},
		{"an unknown event matches nothing", MemoryFilter{EventId: "nope"}, []string{}},
		// The trap the pair exists for: an empty EventId must not mean "the event-less ones".
		{"empty event id applies no restriction", MemoryFilter{EventId: ""}, []string{"m1", "m2", "m3", "m4"}},
		{"event-less only", MemoryFilter{HasEvent: TriStateFalse}, []string{"m4"}},
		{"evented only", MemoryFilter{HasEvent: TriStateTrue}, []string{"m1", "m2", "m3"}},
		{"has_event unset applies no restriction", MemoryFilter{HasEvent: TriStateUnset}, []string{"m1", "m2", "m3", "m4"}},
		// Composition with the other predicates, and with the significance extremum - the latter is
		// what the "add every predicate ABOVE the extremum block" comment in memoryFilterConditions
		// is protecting, since a clause added below it would not reach the subquery either.
		{"composes with a time bound", MemoryFilter{EventId: "e1", TimeStampMin: 150}, []string{"m2"}},
		{"composes with the extremum", MemoryFilter{EventId: "e1", SignificanceExtremum: SignificanceExtremumHighest}, []string{"m1", "m2"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.GetMemories(context.Background(), c.filter)
			if err != nil {
				t.Fatalf("GetMemories: %s", err)
			}

			if ids := memoryIds(got); !reflect.DeepEqual(ids, c.want) {
				t.Errorf("expected %v, got %v", c.want, ids)
			}

			total, err := db.CountMemoriesFiltered(context.Background(), c.filter)
			if err != nil {
				t.Fatalf("CountMemoriesFiltered: %s", err)
			}

			if total != len(c.want) {
				t.Errorf("expected a count of %d, got %d", len(c.want), total)
			}
		})
	}
}

// TestUpdateMemoryMetadata pins the update semantics: a non-empty map replaces the stored map
// wholesale rather than merging per key, an absent map leaves it untouched, and ClearMetadata is
// the only way to remove it - the map's own emptiness cannot say so, because an absent map and an
// explicitly empty one are indistinguishable on the wire.
func TestUpdateMemoryMetadata(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateMemory(context.Background(), types.Memory{
		Id: "m1", TimeStamp: 100, Significance: 5, Body: "body", Group: "billing",
		Metadata: map[string]string{"source": "slack", "project": "x"},
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	read := func() types.Memory {
		t.Helper()

		got, err := db.GetMemoriesByIds(context.Background(), []string{"m1"})
		if err != nil {
			t.Fatalf("GetMemoriesByIds: %s", err)
		}

		if len(*got) != 1 {
			t.Fatalf("expected the memory to still exist, got %+v", *got)
		}

		return (*got)[0]
	}

	// A body-only update leaves metadata and group alone.
	if _, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: "new body"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if got := read(); len(got.Metadata) != 2 || got.Group != "billing" {
		t.Errorf("an update carrying neither field must leave both: %#v / %q", got.Metadata, got.Group)
	}

	// A non-empty map replaces wholesale: the key not mentioned is gone, not merged through.
	if _, err := db.UpdateMemory(context.Background(), types.Memory{
		Id: "m1", Metadata: map[string]string{"source": "email"},
	}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if got := read(); !reflect.DeepEqual(got.Metadata, map[string]string{"source": "email"}) {
		t.Errorf("expected the stored map to be replaced wholesale, got %#v", got.Metadata)
	}

	// ClearMetadata removes it, and ClearGroup does the same for the group.
	if _, err := db.UpdateMemory(context.Background(), types.Memory{
		Id: "m1", ClearMetadata: true, ClearGroup: true,
	}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	got := read()

	if got.Metadata != nil {
		t.Errorf("expected ClearMetadata to remove the metadata, got %#v", got.Metadata)
	}

	if got.Group != "" {
		t.Errorf("expected ClearGroup to reset the group, got %q", got.Group)
	}

	// And a cleared row is still a row a metadata filter can be run against - it is NULL now, not
	// an empty string, which is what keeps json_extract from raising "malformed JSON".
	if _, err := db.GetMemories(context.Background(), MemoryFilter{
		Metadata: map[string]string{"source": "email"},
	}); err != nil {
		t.Fatalf("filtering after a clear must not error: %s", err)
	}
}

// TestUpdateEventMetadata is the event half of the update semantics.
func TestUpdateEventMetadata(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{
		Id: "e1", TimeStart: 100, Significance: 5, Name: "an event", Group: "billing",
		Metadata: map[string]string{"team": "platform", "tier": "1"},
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := db.UpdateEvent(context.Background(), types.Event{
		Id: "e1", Metadata: map[string]string{"team": "billing"},
	}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	}

	got, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if !reflect.DeepEqual(got.Metadata, map[string]string{"team": "billing"}) {
		t.Errorf("expected the stored map to be replaced wholesale, got %#v", got.Metadata)
	}

	if _, err := db.UpdateEvent(context.Background(), types.Event{
		Id: "e1", ClearMetadata: true, ClearGroup: true,
	}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	}

	got, err = db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.Metadata != nil || got.Group != "" {
		t.Errorf("expected both fields cleared, got %#v / %q", got.Metadata, got.Group)
	}
}

// TestImportMetadataRoundTrip covers the transfer path, whose upserts spell their SET lists by hand
// on both dialect arms - a missed column there would silently drop metadata on every import rather
// than failing, which is the failure mode the archive most needs not to have.
func TestImportMetadataRoundTrip(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "a", Metadata: map[string]string{"source": "slack"}},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "b"},
	}

	events := []types.Event{
		{Id: "e1", TimeStart: 100, Significance: 5, Name: "an event", Metadata: map[string]string{"team": "platform"}},
	}

	// Imported twice: an import is an upsert, so this also pins that re-importing is idempotent
	// rather than, say, appending to the stored map.
	for range 2 {
		if _, err := db.ImportMemories(context.Background(), memories); err != nil {
			t.Fatalf("ImportMemories: %s", err)
		}

		if _, err := db.ImportEvents(context.Background(), events); err != nil {
			t.Fatalf("ImportEvents: %s", err)
		}
	}

	got, err := db.GetMemoriesByIds(context.Background(), []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	for _, m := range *got {
		switch m.Id {

		case "m1":
			if !reflect.DeepEqual(m.Metadata, map[string]string{"source": "slack"}) {
				t.Errorf("import dropped or altered the metadata: %#v", m.Metadata)
			}

		case "m2":
			if m.Metadata != nil {
				t.Errorf("expected no metadata on m2, got %#v", m.Metadata)
			}

		}
	}

	event, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if !reflect.DeepEqual(event.Metadata, map[string]string{"team": "platform"}) {
		t.Errorf("import dropped or altered the event metadata: %#v", event.Metadata)
	}
}

// TestMetadataCountsTowardEvictionBytes checks metadata is measured by the store's byte accounting
// rather than absorbed into the flat per-row overhead. UsedBytes and EvictMemories' freed-bytes
// estimate are required to be exact complements (see CLAUDE.md); if eviction under-counted what a
// deletion frees, it would delete more rows than the capacity target actually needs.
func TestMetadataCountsTowardEvictionBytes(t *testing.T) {
	metadata := map[string]string{"source": "slack", "project": "hippocampus", "author": "sean"}

	freedFor := func(t *testing.T, m types.Memory) int64 {
		t.Helper()

		db := newTestDB(t)

		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}

		// The stub consolidates and retains nothing, so eviction alone decides - and asking for one
		// byte evicts the single memory and reports everything its deletion frees.
		_, _, freed, err := db.EvictMemories(context.Background(), &decisionServer{}, 1)
		if err != nil {
			t.Fatalf("EvictMemories: %s", err)
		}

		return freed
	}

	bare := freedFor(t, types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "body"})
	withMetadata := freedFor(t, types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "body", Metadata: metadata})

	expected := int64(types.MetadataSerialisedLen(metadata))

	if withMetadata-bare != expected {
		t.Errorf(
			"expected eviction to count the metadata's %d bytes, but the estimate rose by %d (%d vs %d)",
			expected, withMetadata-bare, withMetadata, bare,
		)
	}
}

// TestMetadataFilterAgainstAPreMigrationDatabase is the migration half of the regression above, and
// the one a fresh-database test structurally cannot make: it builds a store whose memories and
// events tables predate the metadata column entirely, writes rows into it, and only then opens it
// with the current code so initSchema's addColumnIfMissing runs against real pre-existing rows.
//
// That ordering is the whole point. Had the column followed group_name's empty-string-defaulted
// pattern, every one of those rows would hold an empty string and the first metadata-filtered query
// would fail with "malformed JSON" - on an upgraded store only, which is exactly the deployment
// that
// cannot afford to find out. Verified once by hand against a real pre-change database; this keeps
// it.
func TestMetadataFilterAgainstAPreMigrationDatabase(t *testing.T) {
	directory := t.TempDir()

	// The pre-metadata schema, cut down to the columns that existed then. Opened directly rather
	// than through New so initSchema does not run and add the column before the rows are written.
	old, err := sql.Open("sqlite", filepath.Join(directory, DataFile))
	if err != nil {
		t.Fatalf("failed to open the pre-migration database: %s", err)
	}

	if _, err := old.Exec(`
		CREATE TABLE memories (
			id            TEXT PRIMARY KEY,
			timestamp     INTEGER NOT NULL DEFAULT 0,
			significance_level_id INTEGER,
			event_id      TEXT NOT NULL DEFAULT '',
			is_binary     INTEGER NOT NULL DEFAULT 0,
			time_recalled INTEGER NOT NULL DEFAULT 0,
			recall_count  INTEGER NOT NULL DEFAULT 0,
			is_summary    INTEGER NOT NULL DEFAULT 0,
			group_name    TEXT NOT NULL DEFAULT '',
			body          BLOB NOT NULL DEFAULT x''
		);

		CREATE TABLE events (
			id                        TEXT PRIMARY KEY,
			time_start                INTEGER NOT NULL DEFAULT 0,
			time_end                  INTEGER NOT NULL DEFAULT 0,
			significance_level_id     INTEGER,
			name                      TEXT NOT NULL DEFAULT '',
			description               TEXT NOT NULL DEFAULT '',
			memories_consolidated     INTEGER NOT NULL DEFAULT 0,
			group_name                TEXT NOT NULL DEFAULT ''
		);

		INSERT INTO memories (id, timestamp, body) VALUES ('m1', 100, 'an old memory');
		INSERT INTO memories (id, timestamp, body) VALUES ('m2', 200, 'another old memory');
		INSERT INTO events (id, time_start, name) VALUES ('e1', 100, 'an old event');
	`); err != nil {
		t.Fatalf("failed to build the pre-migration schema: %s", err)
	}

	if err := old.Close(); err != nil {
		t.Fatalf("failed to close the pre-migration database: %s", err)
	}

	// Now open it properly, which migrates it in place.
	migrated, err := New(directory)
	if err != nil {
		t.Fatalf("failed to open the pre-migration database with the current schema: %s", err)
	}

	t.Cleanup(func() { _ = migrated.Close() })

	memories, err := migrated.GetMemories(context.Background(), MemoryFilter{
		Metadata: map[string]string{"source": "slack"},
	})
	if err != nil {
		t.Fatalf("a metadata filter against a migrated store must not error: %s", err)
	}

	if len(*memories) != 0 {
		t.Errorf("expected no matches from rows that predate metadata, got %+v", *memories)
	}

	events, err := migrated.GetEvents(context.Background(), EventFilter{
		Metadata: map[string]string{"team": "platform"},
	})
	if err != nil {
		t.Fatalf("an event metadata filter against a migrated store must not error: %s", err)
	}

	if len(*events) != 0 {
		t.Errorf("expected no event matches, got %+v", *events)
	}

	// The pre-existing rows must still read back, with nil metadata rather than an empty map.
	all, err := migrated.GetMemories(context.Background(), MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*all) != 2 {
		t.Fatalf("expected both pre-migration memories to survive, got %+v", *all)
	}

	for _, m := range *all {
		if m.Metadata != nil {
			t.Errorf("expected nil metadata on pre-migration memory %s, got %#v", m.Id, m.Metadata)
		}
	}

	// And a write carrying metadata works against the migrated store, so the column is genuinely
	// usable rather than merely present.
	if _, err := migrated.UpdateMemory(context.Background(), types.Memory{
		Id: "m1", Metadata: map[string]string{"source": "slack"},
	}); err != nil {
		t.Fatalf("UpdateMemory against a migrated store: %s", err)
	}

	matched, err := migrated.GetMemories(context.Background(), MemoryFilter{
		Metadata: map[string]string{"source": "slack"},
	})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*matched) != 1 || (*matched)[0].Id != "m1" {
		t.Errorf("expected the newly tagged memory to match, got %+v", *matched)
	}
}
