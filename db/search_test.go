package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// storeMemory creates a memory for the content-search tests, failing the test on error.
func storeMemory(t *testing.T, d *DB, id string, body string, group string) {
	t.Helper()

	memory := types.Memory{
		Id:           id,
		TimeStamp:    1,
		Significance: 5,
		Body:         body,
		Group:        group,
	}

	if _, err := d.CreateMemory(context.Background(), memory); err != nil {
		t.Fatalf("CreateMemory(%s): %s", id, err)
	}
}

// searchIds runs a content search and returns just the ids, in relevance order, failing the test
// on error. Most tests care only about which memories matched and in what order; the ones about
// scoring call SearchMemoryHits directly.
func searchIds(t *testing.T, d *DB, query ContentQuery) []string {
	t.Helper()

	if query.Limit == 0 {
		query.Limit = 10
	}

	hits, err := d.SearchMemoryHits(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchMemoryHits(%q): %s", query.Text, err)
	}

	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Id)
	}

	return ids
}

// newContentSearchDB is newTestDB for this file: the FTS5 content index is a SQLite-only feature
// (see ContentSearchAvailable), so every test here is about SQLite specifically rather than about
// behaviour the three dialects must agree on, and skips under the shared suite's other dialects.
func newContentSearchDB(t *testing.T) *DB {
	t.Helper()

	requireSQLite(t)

	return newTestDB(t)
}

// ftsRowCount reports how many rows the FTS index holds, so tests can assert on the index itself
// rather than only on what search returns through it.
func ftsRowCount(t *testing.T, d *DB) int {
	t.Helper()

	var n int

	if err := d.sql.QueryRow(`SELECT count(*) FROM ` + contentSearchTable).Scan(&n); err != nil {
		t.Fatalf("counting the index: %s", err)
	}

	return n
}

func TestContentSearchFindsAndFilters(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	if !d.ContentSearchAvailable() {
		t.Fatal("content search should be available on the SQLite driver")
	}

	storeMemory(t, d, "m1", "the deployment failed on the staging cluster", "ops")
	storeMemory(t, d, "m2", "lunch was quite good today", "personal")
	storeMemory(t, d, "m3", "deployment rollback completed cleanly", "ops")

	ids := searchIds(t, d, ContentQuery{Text: "deployment"})
	if len(ids) != 2 {
		t.Fatalf("search for 'deployment': got %v, want the two deployment memories", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "lunch"}); len(ids) != 1 || ids[0] != "m2" {
		t.Errorf("search for 'lunch': got %v, want [m2]", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "nothingmatchesthis"}); len(ids) != 0 {
		t.Errorf("search for an absent term: got %v, want none", ids)
	}
}

// A group or event filter must restrict the matches, since both are how a caller scopes a search
// to one slice of the store.
func TestContentSearchAppliesFilters(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	if _, err := d.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "an event", TimeStart: 1, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	storeMemory(t, d, "m1", "deployment notes", "ops")
	storeMemory(t, d, "m2", "deployment notes", "personal")

	memory := types.Memory{Id: "m3", TimeStamp: 1, Significance: 5, Body: "deployment notes", EventId: "e1"}
	if _, err := d.CreateMemory(context.Background(), memory); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Group: "ops"}); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("group-filtered search: got %v, want [m1]", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment", EventId: "e1"}); len(ids) != 1 || ids[0] != "m3" {
		t.Errorf("event-filtered search: got %v, want [m3]", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Group: "nobody"}); len(ids) != 0 {
		t.Errorf("search filtered to an unused group: got %v, want none", ids)
	}

	// The caller's group scope is a separate predicate from the Group filter, and the two must
	// behave the same way here as they do in the OpenSearch backend - which is why this mirrors
	// TestOpenSearchIntegration_RoundTrip's scope cases case for case. A backend that disagreed
	// about what a scope means would make SearchMemories return different records depending on
	// which one a deployment happened to configure.
	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Groups: []string{"personal"}}); len(ids) != 1 || ids[0] != "m2" {
		t.Errorf("scoped search: got %v, want [m2]", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Groups: []string{"ops", "personal"}}); len(ids) != 2 {
		t.Errorf("search scoped to both groups: got %v, want two matches", ids)
	}

	// An empty scope reads as unscoped: all three, including the group-less m3.
	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Groups: nil}); len(ids) != 3 {
		t.Errorf("an empty scope must match everything: got %v, want three matches", ids)
	}

	// Scope and filter conjoin rather than the filter widening the scope.
	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Group: "ops", Groups: []string{"personal"}}); len(ids) != 0 {
		t.Errorf("a group filter outside the scope must match nothing: got %v", ids)
	}
}

// Limit must bound the result set: an unbounded content search over a large store is exactly the
// kind of query that has to stay bounded.
func TestContentSearchRespectsLimit(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		storeMemory(t, d, id, "deployment notes", "ops")
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Limit: 2}); len(ids) != 2 {
		t.Errorf("limited search: got %d results, want 2", len(ids))
	}

	// A non-positive limit takes the default rather than returning everything or nothing.
	if ids := searchIds(t, d, ContentQuery{Text: "deployment", Limit: -1}); len(ids) != 5 {
		t.Errorf("search with a negative limit: got %d results, want the default's 5", len(ids))
	}
}

// The whole point of ranking by bm25 rather than returning matches in storage order: a memory that
// is more about the term should come first.
func TestContentSearchRanksByRelevance(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "diluted", "deployment "+strings.Repeat("unrelated words here ", 50), "ops")
	storeMemory(t, d, "focused", "deployment deployment deployment", "ops")

	ids := searchIds(t, d, ContentQuery{Text: "deployment"})
	if len(ids) != 2 {
		t.Fatalf("expected both memories to match, got %v", ids)
	}

	if ids[0] != "focused" {
		t.Errorf("ranking: got %v, want the focused memory first", ids)
	}
}

// Binary bodies are client-encoded and opaque, so they must never be indexed - the same rule the
// OpenSearch path follows.
func TestContentSearchSkipsBinaryMemories(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	memory := types.Memory{Id: "bin", TimeStamp: 1, Significance: 5, Body: "deployment payload", IsBinary: true}
	if _, err := d.CreateMemory(context.Background(), memory); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment"}); len(ids) != 0 {
		t.Errorf("binary memory was indexed: got %v, want none", ids)
	}

	if n := ftsRowCount(t, d); n != 0 {
		t.Errorf("index holds %d rows for a binary-only store, want 0", n)
	}
}

// Compression is on by default and rewrites the stored body to a gzip stream. Indexing must
// therefore take the plain body from inside the storage boundary - an index built from the stored
// column would tokenise gzip bytes and match nothing.
func TestContentSearchIndexesCompressedBodies(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	d.SetCompression(true, compressionMinBytesFloor)

	// Comfortably over the threshold, and repetitive so it certainly compresses.
	body := "deployment " + strings.Repeat("the quick brown fox jumps over the lazy dog ", 20)

	storeMemory(t, d, "big", body, "ops")

	var isCompressed bool
	if err := d.sql.QueryRow(`SELECT is_compressed FROM memories WHERE id = 'big'`).Scan(&isCompressed); err != nil {
		t.Fatalf("reading is_compressed: %s", err)
	}

	if !isCompressed {
		t.Fatal("test precondition: the body should have been stored compressed")
	}

	if ids := searchIds(t, d, ContentQuery{Text: "deployment"}); len(ids) != 1 || ids[0] != "big" {
		t.Errorf("search over a compressed body: got %v, want [big]", ids)
	}
}

// Updating a body must retire the old text as well as index the new one, or a memory keeps
// matching something it no longer says.
func TestContentSearchReindexesOnUpdate(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "the original wording", "ops")

	existed, err := d.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: "completely different phrasing"})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if !existed {
		t.Fatal("UpdateMemory reported the memory as absent")
	}

	if ids := searchIds(t, d, ContentQuery{Text: "original"}); len(ids) != 0 {
		t.Errorf("the replaced body still matches: got %v, want none", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "phrasing"}); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("the new body does not match: got %v, want [m1]", ids)
	}

	// One row, not two: the reindex must replace rather than accumulate.
	if n := ftsRowCount(t, d); n != 1 {
		t.Errorf("index holds %d rows after an update, want 1", n)
	}
}

// An update that does not carry a body must leave the index alone rather than blanking it.
func TestContentSearchUpdateWithoutBodyKeepsIndex(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "the original wording", "ops")

	if _, err := d.UpdateMemory(context.Background(), types.Memory{Id: "m1", Group: "moved"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "original"}); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("a group-only update disturbed the index: got %v, want [m1]", ids)
	}
}

// Updating a body on an id that does not exist must not touch the index at all. Without the
// existence check this deletes nothing and inserts nothing (the INSERT ... SELECT matches no row),
// but the test pins that it stays that way.
func TestContentSearchUpdateOfAbsentMemoryIndexesNothing(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "the original wording", "ops")

	existed, err := d.UpdateMemory(context.Background(), types.Memory{Id: "ghost", Body: "never stored"})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if existed {
		t.Fatal("UpdateMemory reported a nonexistent memory as present")
	}

	if n := ftsRowCount(t, d); n != 1 {
		t.Errorf("index holds %d rows, want only the one real memory's", n)
	}
}

// Deletion is handled by a trigger rather than by any call site, so it must hold for every path
// that removes a memory - including the consolidation/eviction path, which never goes near the
// index maintenance helpers.
func TestContentSearchTriggerCoversEveryDeletePath(t *testing.T) {
	ctx := context.Background()

	t.Run("DeleteMemories", func(t *testing.T) {
		d := newContentSearchDB(t)
		defer func() { _ = d.Close() }()

		storeMemory(t, d, "m1", "forgettable content", "ops")

		if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
			t.Fatalf("DeleteMemories: %s", err)
		}

		if n := ftsRowCount(t, d); n != 0 {
			t.Errorf("index holds %d rows after a delete, want 0", n)
		}
	})

	t.Run("Purge", func(t *testing.T) {
		d := newContentSearchDB(t)
		defer func() { _ = d.Close() }()

		storeMemory(t, d, "m1", "forgettable content", "ops")

		if err := d.Purge(ctx); err != nil {
			t.Fatalf("Purge: %s", err)
		}

		if n := ftsRowCount(t, d); n != 0 {
			t.Errorf("index holds %d rows after a purge, want 0", n)
		}
	})

	t.Run("consolidation", func(t *testing.T) {
		d := newContentSearchDB(t)
		defer func() { _ = d.Close() }()

		storeMemory(t, d, "m1", "forgettable content", "ops")

		if _, err := d.ConsolidateMemories(ctx, &stubServer{consolidateMemories: true}); err != nil {
			t.Fatalf("ConsolidateMemories: %s", err)
		}

		if n := ftsRowCount(t, d); n != 0 {
			t.Errorf("index holds %d rows after consolidation, want 0", n)
		}
	})

	t.Run("DeleteEventMemories", func(t *testing.T) {
		d := newContentSearchDB(t)
		defer func() { _ = d.Close() }()

		if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "an event", TimeStart: 1, Significance: 1}); err != nil {
			t.Fatalf("CreateEvent: %s", err)
		}

		memory := types.Memory{Id: "m1", TimeStamp: 1, Significance: 5, Body: "forgettable content", EventId: "e1"}
		if _, err := d.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}

		if _, err := d.DeleteEventMemories(ctx, "e1"); err != nil {
			t.Fatalf("DeleteEventMemories: %s", err)
		}

		if n := ftsRowCount(t, d); n != 0 {
			t.Errorf("index holds %d rows after deleting an event's memories, want 0", n)
		}
	})
}

// Summary replacement deletes an event's memories and inserts one in their place; the index must
// end up describing the summary and nothing else.
func TestContentSearchFollowsSummaryReplacement(t *testing.T) {
	ctx := context.Background()

	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "an event", TimeStart: 1, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, id := range []string{"m1", "m2"} {
		memory := types.Memory{Id: id, TimeStamp: 1, Significance: 5, Body: "individual detail " + id, EventId: "e1"}
		if _, err := d.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	summary := types.Memory{Id: "s1", TimeStamp: 2, Significance: 5, Body: "the condensed overview", EventId: "e1", IsSummary: true}
	if _, err := d.ReplaceMemoriesWithSummary(ctx, "e1", summary); err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "individual"}); len(ids) != 0 {
		t.Errorf("replaced memories still match: got %v, want none", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "condensed"}); len(ids) != 1 || ids[0] != "s1" {
		t.Errorf("the summary does not match: got %v, want [s1]", ids)
	}
}

// Import is an upsert, so importing over an existing memory must replace its index entry rather
// than add a second one for the same row.
func TestContentSearchFollowsImport(t *testing.T) {
	ctx := context.Background()

	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "the original wording", "ops")

	imported := []types.Memory{{Id: "m1", TimeStamp: 1, Significance: 5, Body: "the imported wording", Group: "ops"}}
	if _, err := d.ImportMemories(ctx, imported); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "original"}); len(ids) != 0 {
		t.Errorf("the overwritten body still matches: got %v, want none", ids)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "imported"}); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("the imported body does not match: got %v, want [m1]", ids)
	}

	if n := ftsRowCount(t, d); n != 1 {
		t.Errorf("index holds %d rows after an upsert, want 1", n)
	}
}

// A store written before content search existed gains an empty index on the upgrade startup. It
// must be populated then, or every pre-existing memory is silently unfindable.
func TestContentSearchPopulatesOnUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	d, err := New(dir)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	storeMemory(t, d, "m1", "written before the index existed", "ops")

	// Simulate the pre-upgrade store: drop the index and its trigger, leaving the memories behind
	// exactly as a database written by an older version would have them.
	for _, ddl := range []string{`DROP TRIGGER ` + contentSearchTrigger, `DROP TABLE ` + contentSearchTable} {
		if _, err := d.sql.Exec(ddl); err != nil {
			t.Fatalf("%s: %s", ddl, err)
		}
	}

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatalf("reopening: %s", err)
	}
	defer func() { _ = reopened.Close() }()

	if ids := searchIds(t, reopened, ContentQuery{Text: "written"}); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("a memory predating the index is not findable: got %v, want [m1]", ids)
	}

	// And a subsequent restart must not re-run the backfill, since the index is no longer empty.
	if err := reopened.RebuildContentSearch(ctx); err != nil {
		t.Fatalf("RebuildContentSearch: %s", err)
	}

	if n := ftsRowCount(t, reopened); n != 1 {
		t.Errorf("index holds %d rows after a rebuild, want 1", n)
	}
}

// RebuildContentSearch is the recovery path for the one drift this backend can suffer: an index
// write that failed and was logged rather than failing its memory.
func TestRebuildContentSearchRepairsAGap(t *testing.T) {
	ctx := context.Background()

	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "findable content", "ops")
	storeMemory(t, d, "m2", "also findable content", "ops")

	// Punch a hole in the index the way a failed index write would.
	if _, err := d.sql.Exec(`DELETE FROM ` + contentSearchTable + ` WHERE rowid = (SELECT rowid FROM memories WHERE id = 'm1')`); err != nil {
		t.Fatalf("removing an index entry: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "findable"}); len(ids) != 1 {
		t.Fatalf("test precondition: expected one findable memory, got %v", ids)
	}

	if err := d.RebuildContentSearch(ctx); err != nil {
		t.Fatalf("RebuildContentSearch: %s", err)
	}

	if ids := searchIds(t, d, ContentQuery{Text: "findable"}); len(ids) != 2 {
		t.Errorf("after a rebuild: got %v, want both memories", ids)
	}
}

// FTS5's MATCH argument is a query language, so raw user text can be a syntax error or can reach
// into the query's structure. None of these may error, and none may match a memory the plain words
// would not have matched.
func TestContentSearchSanitisesHostileQueries(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "deployment notes", "ops")
	storeMemory(t, d, "m2", "unrelated material", "ops")

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "trailing operator", query: "deployment AND", want: 1},
		{name: "unbalanced quote", query: `"deployment`, want: 1},
		{name: "column filter", query: "body:deployment", want: 1},
		{name: "prefix star", query: "deployment*", want: 1},
		// Tokenises to NEAR OR deployment OR notes, so it matches the deployment memory as the
		// bare words would and the operator does nothing.
		{name: "NEAR expression", query: "NEAR(deployment notes)", want: 1},
		{name: "bare hyphen", query: "-", want: 0},
		{name: "punctuation only", query: "!!! ???", want: 0},
		{name: "whitespace only", query: "   ", want: 0},
		{name: "empty", query: "", want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hits, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: test.query, Limit: 10})
			if err != nil {
				t.Fatalf("query %q errored: %s", test.query, err)
			}

			if len(hits) != test.want {
				t.Errorf("query %q: got %d results (%v), want %d", test.query, len(hits), hits, test.want)
			}
		})
	}
}

// The tokens are OR-ed, matching the OpenSearch backend's "match" query, so the two backends agree
// on which memories match and not merely on how they rank them.
func TestContentSearchOrsItsTokens(t *testing.T) {
	d := newContentSearchDB(t)
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m1", "deployment", "ops")
	storeMemory(t, d, "m2", "rollback", "ops")

	if ids := searchIds(t, d, ContentQuery{Text: "deployment rollback"}); len(ids) != 2 {
		t.Errorf("multi-token search: got %v, want both memories", ids)
	}
}

func TestFtsMatchExpression(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "single word", in: "deployment", want: `"deployment"`},
		{name: "two words", in: "deployment failed", want: `"deployment" OR "failed"`},
		{name: "operators are neutralised", in: "a AND b", want: `"a" OR "AND" OR "b"`},
		{name: "punctuation is dropped", in: "it's a co-operative!", want: `"it" OR "s" OR "a" OR "co" OR "operative"`},
		{name: "digits are kept", in: "error 500", want: `"error" OR "500"`},
		{name: "quotes cannot escape", in: `he said "hi"`, want: `"he" OR "said" OR "hi"`},
		{name: "empty", in: "", want: ""},
		{name: "punctuation only", in: "-*-", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ftsMatchExpression(test.in); got != test.want {
				t.Errorf("ftsMatchExpression(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// A read-only open never runs the DDL that creates the index, so it must report the capability as
// absent rather than fail at query time with a missing-table error.
func TestContentSearchUnavailableOnReadOnlyOpen(t *testing.T) {
	dir := t.TempDir()

	d, err := New(dir)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	storeMemory(t, d, "m1", "some content", "ops")

	if err := d.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	readOnly, err := NewSQLiteReadOnly(dir)
	if err != nil {
		t.Fatalf("NewSQLiteReadOnly: %s", err)
	}
	defer func() { _ = readOnly.Close() }()

	if readOnly.ContentSearchAvailable() {
		t.Error("a read-only open should not report content search as available")
	}

	if _, err := readOnly.SearchMemoryHits(context.Background(), ContentQuery{Text: "content"}); !errors.Is(err, ErrContentSearchUnavailable) {
		t.Errorf("SearchMemoryHits on a read-only open: got %v, want ErrContentSearchUnavailable", err)
	}
}
