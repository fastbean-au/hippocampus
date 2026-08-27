package db

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The stored schema is one of the four things CHANGELOG.md's Compatibility section promises a
// version number covers, and it was the only one with no mechanical guard behind it: the contract
// has buf breaking, the config keys have configkeys_test.go, the archive format has its own
// versioned header, and this had a sentence. What stands behind that sentence is ten
// addColumnIfMissing calls, the MySQL collation migration, the legacy relationship columns dropped
// on sight, initContentSearch's backfill, and migrateSignificanceToLevels - the one migration that
// moves DATA rather than adding a column.
//
// These tests open a store written by a released binary and assert the rows are still READABLE,
// FILTERABLE and CONSOLIDATABLE on HEAD. That is deliberately behavioural rather than structural:
// asserting a column exists proves nothing its own ALTER TABLE does not already say, and the one
// migration defect this repo has actually found - metadata defaulting to '' rather than NULL, where
// SQLite's json_extract raises "malformed JSON" on the empty string - would have passed a column
// check and failed every fresh-database test in the package to notice it. See TODO item 78.
//
// Fixtures are produced by scripts/schema-fixtures.sh, which builds and runs the released binary
// and seeds it over its own gateway. They are never hand-written: a hand-written old schema is a
// second copy of the schema and drifts like every other copy in this repo.

// schemaFixtures is one entry per distinct released SQLite schema, named by the LAST tag that wrote
// it - the version somebody would actually be upgrading from. Every tag between two entries creates
// a byte-identical schema, so one fixture covers the band.
var schemaFixtures = []struct {
	tag string

	// migrations names what initSchema must do to this fixture that it need not do to the one
	// after it. It is documentation for a failure message, not something the test executes.
	migrations string
}{
	{"v0.4.0", "migrateSignificanceToLevels (data), significance_level_id on both tables, covering index rebuilt"},
	{"v0.22.0", "memories.is_compressed"},
	{"v0.23.0", "initContentSearch (FTS backfill over a non-empty store)"},
	{"v0.25.0", "initLinkTables, dropLegacyRelationshipColumns, link_significance and metadata on both tables"},
	{"v0.31.0", "initTombstones"},
	{"v0.34.0", "initSearchOutbox"},
	{"v0.37.0", "none - the control; migrating this must be a no-op"},
}

// seededMemory is a row scripts/schema-fixtures.sh writes into every fixture, with the values it
// wrote. Every field here is one a migration could plausibly corrupt: significance survived a move
// into a separate registry table, the body survived compression arriving, and the recall state is
// what the decay clock ages from.
type seededMemory struct {
	id           string
	significance int32
	eventId      string
	group        string
	isBinary     bool
	recallCount  int32
	bodyContains string
}

var seededMemories = []seededMemory{
	{id: "mem-alpha-1", significance: 50, eventId: "evt-alpha", bodyContains: "quick brown fox"},
	{id: "mem-alpha-2", significance: 20, eventId: "evt-alpha", bodyContains: "lazy dog"},
	{id: "mem-alpha-3", significance: 80, eventId: "evt-alpha", recallCount: 1, bodyContains: "liquor jugs"},
	{id: "mem-beta-1", significance: 40, eventId: "evt-beta", group: "beta", bodyContains: "black quartz"},
	{id: "mem-binary", significance: 45, group: "alpha", isBinary: true, bodyContains: "YmluYXJ5"},
	{id: "mem-long", significance: 55, group: "alpha", bodyContains: "compressible body"},
	{id: "mem-loose-1", significance: 70, group: "alpha", bodyContains: "daft zebras"},
	{id: "mem-loose-2", significance: 10, bodyContains: "low-significance"},
}

// The shape of the seeded set, as the three consolidation passes see it. evt-alpha holds three
// memories and evt-beta one, so both events are walked by the second pass and neither reaches the
// third; the remaining four memories are event-less and belong to the first.
const (
	seededMemoriesWithEvent  = 4
	seededEvents             = 2
	seededEventsWithMemories = 2

	// The fixed instant scripts/schema-fixtures.sh seeds from, 2026-01-01T00:00:00Z. Fixed rather
	// than relative so a fixture is reproducible and a test can assert exact values.
	seededBaseTimestamp = 1767225600000000000
)

// openFixture copies a fixture into a temp directory and opens it there. The copy is the point:
// New runs initSchema, which migrates IN PLACE, so opening the committed file directly would
// rewrite the fixture and make every subsequent run test the migration of an already-migrated
// store - passing forever, including after the migration broke.
func openFixture(t *testing.T, tag string) *DB {
	t.Helper()

	source := filepath.Join("testdata", "schema", tag, "hippocampus.db")

	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("failed to read fixture %s (regenerate with scripts/schema-fixtures.sh): %s", source, err)
	}

	directory := t.TempDir()

	if err := os.WriteFile(filepath.Join(directory, "hippocampus.db"), body, 0o600); err != nil {
		t.Fatalf("failed to stage fixture: %s", err)
	}

	database, err := New(directory)
	if err != nil {
		t.Fatalf("failed to open a %s store on HEAD: %s", tag, err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return database
}

// assertSeededRowsReadBack is the core assertion: a store written by an older release
// opens on HEAD with every row intact and every field the migrations moved still saying what it
// said. It runs against each schema band, so a migration that corrupts a value rather than failing
// outright is caught at the fixture whose schema it acts on.
func assertSeededRowsReadBack(t *testing.T, database *DB, label string, migrations string) {
	t.Helper()

	ctx := context.Background()

	memories, err := database.GetMemories(ctx, MemoryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("GetMemories after migrating %s failed (%s): %s", label, migrations, err)
	}

	byId := map[string]seededMemory{}
	for _, memory := range *memories {
		byId[memory.Id] = seededMemory{
			id:           memory.Id,
			significance: memory.Significance,
			eventId:      memory.EventId,
			group:        memory.Group,
			isBinary:     memory.IsBinary,
			recallCount:  memory.RecallCount,
			bodyContains: memory.Body,
		}
	}

	if len(byId) != len(seededMemories) {
		t.Fatalf("migrating %s left %d memories, want %d", label, len(byId), len(seededMemories))
	}

	for _, want := range seededMemories {
		got, ok := byId[want.id]
		if !ok {
			t.Errorf("%s is missing after migrating %s", want.id, label)

			continue
		}

		// Significance is the one that moved tables. Before v0.5.0 it was an integer column
		// on the row; migrateSignificanceToLevels lifts every distinct value into the shared
		// registry and repoints the row at a level id, so a fixture from v0.4.0 exercises
		// that path and must still read back the number it was written with.
		if got.significance != want.significance {
			t.Errorf("%s significance is %d after migrating %s, want %d",
				want.id, got.significance, label, want.significance)
		}

		if got.eventId != want.eventId {
			t.Errorf("%s event_id is %q after migrating %s, want %q",
				want.id, got.eventId, label, want.eventId)
		}

		if got.group != want.group {
			t.Errorf("%s group is %q after migrating %s, want %q",
				want.id, got.group, label, want.group)
		}

		if got.isBinary != want.isBinary {
			t.Errorf("%s is_binary is %v after migrating %s, want %v",
				want.id, got.isBinary, label, want.isBinary)
		}

		// The decay clock ages from the most recent recall, so losing this in a migration
		// would silently make a reinforced memory as forgettable as an untouched one.
		if got.recallCount != want.recallCount {
			t.Errorf("%s recall_count is %d after migrating %s, want %d",
				want.id, got.recallCount, label, want.recallCount)
		}

		// Bodies written before compression existed are stored uncompressed and flagged so;
		// reads follow the per-row flag rather than the current configuration, which is what
		// lets a mixed store read correctly. A fixture from >= v0.23.0 carries both kinds.
		if !strings.Contains(got.bodyContains, want.bodyContains) {
			t.Errorf("%s body after migrating %s does not contain %q: %q",
				want.id, label, want.bodyContains, got.bodyContains)
		}
	}
}

// assertMetadataFilterIsSafe is the regression that names the whole class.
//
// metadata is NULL-able with no DEFAULT on all three dialects, unlike group_name beside it, because
// SQLite's json_extract raises "malformed JSON" on an empty string but returns NULL for NULL. A
// column defaulting to ” would therefore make the FIRST metadata-filtered query fail against every
// row written before the migration ran - a failure no fresh-database test in this package can see,
// since a fresh database has no pre-migration rows.
//
// TestMetadataFilterAgainstAPreMigrationDatabase already pins that for a hand-built old schema.
// This pins it against the schemas that were actually released, including the three that predate
// the metadata column entirely.
func assertMetadataFilterIsSafe(t *testing.T, database *DB, label string, migrations string) {
	t.Helper()

	ctx := context.Background()

	// Every seeded row predates metadata, so the correct answer is an empty page. An ERROR
	// is the defect; zero rows is the point.
	memories, err := database.GetMemories(ctx, MemoryFilter{
		Metadata: map[string]string{"source": "slack"},
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("metadata-filtered read of a migrated %s store failed: %s", label, err)
	}

	if len(*memories) != 0 {
		t.Errorf("metadata filter matched %d rows on a %s store whose rows carry none",
			len(*memories), label)
	}

	// The same query with no metadata predicate must still see everything, so an empty
	// result above is the filter working rather than the store reading as empty.
	all, err := database.GetMemories(ctx, MemoryFilter{Limit: 100})
	if err != nil {
		t.Fatalf("unfiltered read of a migrated %s store failed: %s", label, err)
	}

	if len(*all) != len(seededMemories) {
		t.Errorf("unfiltered read of a migrated %s store returned %d rows, want %d",
			label, len(*all), len(seededMemories))
	}
}

// assertContentSearchBackfilled covers initContentSearch's upgrade case: a store
// written by a version without the FTS index gains it on this startup, and must be populated from
// the existing rows rather than left empty. An empty index answers every search with nothing, which
// is indistinguishable from a store holding no match - so this is the other failure that reports
// itself as an ordinary empty result.
func assertContentSearchBackfilled(t *testing.T, database *DB, label string, migrations string) {
	t.Helper()

	ctx := context.Background()

	if !database.ContentSearchAvailable() {
		t.Skip("content search unavailable on this store")
	}

	hits, err := database.SearchMemoryHits(ctx, ContentQuery{Text: "zebras", Limit: 10})
	if err != nil {
		t.Fatalf("content search on a migrated %s store failed: %s", label, err)
	}

	if len(hits) != 1 || hits[0].Id != "mem-loose-1" {
		ids := make([]string, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.Id)
		}

		sort.Strings(ids)
		t.Errorf("searching a migrated %s store for \"zebras\" returned %v, want [mem-loose-1] "+
			"(an empty result here means the FTS index was created but never backfilled)",
			label, ids)
	}

	// A binary body is never indexed, whether it was written before or after the index
	// existed - so the backfill must honour the same rule the write path does.
	binary, err := database.SearchMemoryHits(ctx, ContentQuery{Text: "YmluYXJ5", Limit: 10})
	if err != nil {
		t.Fatalf("content search for a binary body on a migrated %s store failed: %s", label, err)
	}

	for _, hit := range binary {
		if hit.Id == "mem-binary" {
			t.Errorf("the backfill indexed a binary body on a migrated %s store", label)
		}
	}
}

// assertStoreIsConsolidatable is the third of the three verbs, and the one that matters
// most to a forgetting store: migrated rows must be VISIBLE TO THE DECAY PATH. A row that reads
// back correctly but that the consolidation scans cannot see is immortal by accident - which is
// exactly what item 23.2 found for memories with a dangling event_id, and is invisible to any test
// that only reads.
func assertStoreIsConsolidatable(t *testing.T, database *DB, label string, migrations string) {
	t.Helper()

	ctx := context.Background()

	// A server that consolidates nothing: the scans must still walk every row and reach a
	// decision on it. What is asserted is the walk, not the verdict.
	seen := &countingServer{}

	// All three passes, in the order sleep() runs them - memories without events, memories
	// with events, then events without memories. Running only the first would leave the
	// four memories that belong to an event unvisited, which is precisely the state this
	// test exists to detect.
	looseDeleted, err := database.ConsolidateMemories(ctx, seen)
	if err != nil {
		t.Fatalf("first consolidation pass over a migrated %s store failed: %s", label, err)
	}

	eventMemoriesDeleted, eventsSeen, eventsDeleted, err := database.ConsolidateEventMemories(ctx, seen)
	if err != nil {
		t.Fatalf("second consolidation pass over a migrated %s store failed: %s", label, err)
	}

	emptyEventsDeleted, err := database.ConsolidateEvents(ctx, seen)
	if err != nil {
		t.Fatalf("third consolidation pass over a migrated %s store failed: %s", label, err)
	}

	if deleted := looseDeleted + eventMemoriesDeleted + eventsDeleted + emptyEventsDeleted; deleted != 0 {
		t.Errorf("the consolidation passes over a migrated %s store deleted %d records the "+
			"decider kept", label, deleted)
	}

	// The whole point: every migrated row must be OFFERED to the decider. A row the scans
	// never reach is immortal by accident whatever it reads back as, which is exactly what
	// item 23.2 found for memories with a dangling event_id - and no read-only assertion
	// can see it.
	if seen.memories != len(seededMemories) {
		t.Errorf("the consolidation passes over a migrated %s store considered %d memories, want %d",
			label, seen.memories, len(seededMemories))
	}

	// The second pass reports the events it walked; the third is offered only those with no
	// memories left, which after a pass that deleted nothing is just the one seeded empty
	// event.
	if eventsSeen != seededEventsWithMemories {
		t.Errorf("the second consolidation pass over a migrated %s store walked %d events, want %d",
			label, eventsSeen, seededEventsWithMemories)
	}

	if seen.events != seededEvents-seededEventsWithMemories {
		t.Errorf("the third consolidation pass over a migrated %s store considered %d empty events, want %d",
			label, seen.events, seededEvents-seededEventsWithMemories)
	}

	// CountMemories splits on event membership, so this also pins that the event_id column
	// survived: four of the seeded memories belong to an event and four do not.
	withEvent, withoutEvent := database.CountMemories(ctx)
	if withEvent != seededMemoriesWithEvent {
		t.Errorf("a migrated %s store counts %d memories with an event, want %d",
			label, withEvent, seededMemoriesWithEvent)
	}

	if withoutEvent != len(seededMemories)-seededMemoriesWithEvent {
		t.Errorf("a migrated %s store counts %d memories without an event, want %d",
			label, withoutEvent, len(seededMemories)-seededMemoriesWithEvent)
	}
}

// TestSchemaUpgradeReadsEverySeededRow runs assertSeededRowsReadBack against every SQLite fixture.
func TestSchemaUpgradeReadsEverySeededRow(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			assertSeededRowsReadBack(t, openFixture(t, fixture.tag), fixture.tag, fixture.migrations)
		})
	}
}

// TestSchemaUpgradeMetadataFilterAgainstEveryFixture runs assertMetadataFilterIsSafe against every SQLite fixture.
func TestSchemaUpgradeMetadataFilterAgainstEveryFixture(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			assertMetadataFilterIsSafe(t, openFixture(t, fixture.tag), fixture.tag, fixture.migrations)
		})
	}
}

// TestSchemaUpgradeContentSearchIsBackfilled runs assertContentSearchBackfilled against every SQLite fixture.
func TestSchemaUpgradeContentSearchIsBackfilled(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			assertContentSearchBackfilled(t, openFixture(t, fixture.tag), fixture.tag, fixture.migrations)
		})
	}
}

// TestSchemaUpgradeStoreIsConsolidatable runs assertStoreIsConsolidatable against every SQLite fixture.
func TestSchemaUpgradeStoreIsConsolidatable(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			assertStoreIsConsolidatable(t, openFixture(t, fixture.tag), fixture.tag, fixture.migrations)
		})
	}
}

// countingServer answers "keep everything" and records how many rows the scans offered it.
type countingServer struct {
	memories int
	events   int
}

func (s *countingServer) ShouldConsolidateMemory(candidate MemoryConsolidationCandidate) bool {
	s.memories++

	return false
}

func (s *countingServer) ShouldConsolidateEvent(candidate EventConsolidationCandidate) bool {
	s.events++

	return false
}

func (s *countingServer) MemoryValue(candidate MemoryConsolidationCandidate) float64 { return 0 }

func (s *countingServer) MemoryRetained(candidate MemoryConsolidationCandidate) bool { return false }

func (s *countingServer) DeletionThreshold() float64 { return 0 }

// schemaInits is the three schema-init functions, each with its own migration list. THERE BEING
// THREE is the fact this guard exists to keep in view: initSchema is SQLite's, and a fixture set
// built for it alone leaves initPostgresSchema and initMySQLSchema unguarded — which is how
// initInstances (server-only) and the whole of Postgres's native ADD COLUMN IF NOT EXISTS path came
// to have no fixture behind them at all.
//
// Each entry maps a migration to the newest fixture written BEFORE it existed — the fixture that
// exercises it. notReleasedBefore marks one with no released predecessor.
var schemaInits = []struct {
	file     string
	function string
	fixture  string // the artefact under testdata/schema/<tag>/ that this dialect's fixtures use
	declared map[string]string
	exempt   map[string]string
}{
	{
		file:     "db.go",
		function: "initSchema",
		fixture:  "hippocampus.db",
		declared: map[string]string{
			"memories.is_summary":            "v0.4.0",
			"memories.is_compressed":         "v0.22.0",
			"memories.group_name":            "v0.4.0",
			"events.group_name":              "v0.4.0",
			"memories.significance_level_id": "v0.4.0",
			"events.significance_level_id":   "v0.4.0",
			"memories.link_significance":     "v0.25.0",
			"events.link_significance":       "v0.25.0",
			"memories.metadata":              "v0.25.0",
			"events.metadata":                "v0.25.0",
			"initLinkTables":                 "v0.25.0",
			"dropLegacyRelationshipColumns":  "v0.25.0",
			"migrateSignificanceToLevels":    "v0.4.0",
			"initTombstones":                 "v0.31.0",
			"initSearchOutbox":               "v0.34.0",
			"initContentSearch":              "v0.23.0",

			// The two indexes are rebuilt from whatever columns exist rather than migrating data,
			// so every fixture exercises them; the oldest is named because it is the one that
			// proves the covering index survives being rebuilt onto significance_level_id.
			"ensureCoveringIndex": "v0.4.0",
			"ensureListingIndex":  "v0.4.0",
		},
		exempt: map[string]string{
			"significanceLevelsDDL": "returns DDL for the CREATE TABLE above it; not itself a migration step",
		},
	},
	{
		file:     "postgres.go",
		function: "initPostgresSchema",
		fixture:  "postgres.sql",
		declared: map[string]string{
			"memories.is_summary":            "v0.4.0",
			"memories.is_compressed":         "v0.22.0",
			"memories.group_name":            "v0.4.0",
			"events.group_name":              "v0.4.0",
			"memories.significance_level_id": "v0.4.0",
			"events.significance_level_id":   "v0.4.0",
			"memories.link_significance":     "v0.25.0",
			"events.link_significance":       "v0.25.0",
			"memories.metadata":              "v0.25.0",
			"events.metadata":                "v0.25.0",
			"initLinkTables":                 "v0.25.0",
			"dropLegacyRelationshipColumns":  "v0.25.0",
			"migrateSignificanceToLevels":    "v0.4.0",
			"initTombstones":                 "v0.31.0",
			"initSearchOutbox":               "v0.34.0",
			"ensureCoveringIndex":            "v0.4.0",
			"ensureListingIndex":             "v0.4.0",

			// The peers table (topology phase 3) shipped in v0.34.0, so v0.31.0 is the newest
			// schema that predates it.
			"initInstances": "v0.31.0",
		},
		exempt: map[string]string{
			"significanceLevelsDDL": "returns DDL for the CREATE TABLE above it; not itself a migration step",
		},
	},
	{
		file:     "mysql.go",
		function: "initMySQLSchema",
		fixture:  "mysql.sql",
		declared: map[string]string{
			"memories.is_summary":            "v0.4.0",
			"memories.is_compressed":         "v0.22.0",
			"memories.group_name":            "v0.4.0",
			"events.group_name":              "v0.4.0",
			"memories.significance_level_id": "v0.4.0",
			"events.significance_level_id":   "v0.4.0",
			"memories.link_significance":     "v0.25.0",
			"events.link_significance":       "v0.25.0",
			"memories.metadata":              "v0.25.0",
			"events.metadata":                "v0.25.0",
			"initLinkTables":                 "v0.25.0",
			"dropLegacyRelationshipColumns":  "v0.25.0",
			"migrateSignificanceToLevels":    "v0.4.0",
			"initTombstones":                 "v0.31.0",
			"initSearchOutbox":               "v0.34.0",
			"ensureCoveringIndex":            "v0.4.0",
			"ensureListingIndex":             "v0.4.0",
			"initInstances":                  "v0.31.0",

			// The binary collation was pinned before v0.1.0, so no RELEASED schema predates it and
			// no fixture can drive the migration itself. What the fixtures pin instead is the
			// property it guarantees, on every released schema —
			// TestSchemaUpgradeMySQLCollation.
			"setMySQLColumnCollationIfNeeded": notReleasedBefore,
		},
		exempt: map[string]string{
			"significanceLevelsDDL": "returns DDL for the CREATE TABLE above it; not itself a migration step",
		},
	},
}

// notReleasedBefore marks a migration with no released predecessor schema: it was added in the same
// release as the table or column it acts on (or before the first release), so there is no older
// store that needs it and no fixture can exercise it.
const notReleasedBefore = "-"

// TestEverySchemaMigrationHasAFixture is the drift guard, and it is the half of this file that
// survives contact with the next migration.
//
// The fixtures are only as good as their coverage: a migration added after v0.34.0 has no fixture
// predating it, so nothing would exercise it and every other test here would go on passing. This
// reads the migrations each schema-init function actually performs OUT OF THE SOURCE and requires
// each to be declared — so adding one fails the build until somebody decides whether a new fixture
// is needed.
//
// It is deliberately two-directional, the shape 74.6 and 74.7 settled on: an undeclared migration
// fails, and so does a declaration whose migration has gone. The second direction is what stops the
// lists becoming a record of what these functions used to do.
func TestEverySchemaMigrationHasAFixture(t *testing.T) {
	for _, init := range schemaInits {
		t.Run(init.function, func(t *testing.T) {
			performed := migrationsIn(t, init.file, init.function)

			for _, migration := range performed {
				if _, ok := init.exempt[migration]; ok {
					continue
				}

				tag, ok := init.declared[migration]
				if !ok {
					t.Errorf("%s performs %q but no fixture is declared for it.\n"+
						"Decide which released schema predates it and add it to schemaInits; if none "+
						"does, name notReleasedBefore. If a new fixture is needed, generate it with "+
						"scripts/schema-fixtures.sh --driver all and add the tag to schemaFixtures.",
						init.function, migration)

					continue
				}

				if tag == notReleasedBefore {
					continue
				}

				if _, err := os.Stat(filepath.Join("testdata", "schema", tag, init.fixture)); err != nil {
					t.Errorf("%q is declared against fixture %s/%s, which is not on disk: %s",
						migration, tag, init.fixture, err)
				}
			}

			// The reverse direction: a declaration whose migration is no longer performed.
			for migration := range init.declared {
				if !performs(performed, migration) {
					t.Errorf("a fixture is declared for %q but %s no longer performs it — remove the "+
						"declaration (and consider whether its fixture is still earning its place)",
						migration, init.function)
				}
			}

			for migration := range init.exempt {
				if !performs(performed, migration) {
					t.Errorf("%q is exempted but %s no longer calls it — remove the exemption",
						migration, init.function)
				}
			}
		})
	}
}

// TestEveryFixtureCoversEveryDriver pins that a fixture tag carries an artefact for all three
// drivers. A tag added for one driver and forgotten for the others would leave that dialect's
// declarations pointing at a file which, being absent, makes its subtest skip rather than fail.
func TestEveryFixtureCoversEveryDriver(t *testing.T) {
	for _, fixture := range schemaFixtures {
		for _, artefact := range []string{"hippocampus.db", "postgres.sql", "mysql.sql"} {
			path := filepath.Join("testdata", "schema", fixture.tag, artefact)

			if _, err := os.Stat(path); err != nil {
				t.Errorf("fixture %s has no %s — generate it with "+
					"scripts/schema-fixtures.sh --driver all %s", fixture.tag, artefact, fixture.tag)
			}
		}
	}
}

// performs reports whether a schema-init function's extracted migration list names this one.
func performs(migrations []string, want string) bool {
	for _, migration := range migrations {
		if migration == want {
			return true
		}
	}

	return false
}

// migrationsIn reads the migration steps out of a schema-init function's source rather than taking
// a list on trust — the list IS what drifts, which is the entire lesson of 74.7.
//
// Three shapes are recognised, because the three dialects express a column addition three ways:
// an addColumnIfMissing call (SQLite and MySQL, which probe first) is named "<table>.<column>";
// Postgres's native ALTER TABLE ... ADD COLUMN IF NOT EXISTS lives in a SQL string literal and is
// named the same way; and any other method called on the receiver is named by the method.
func migrationsIn(t *testing.T, file string, function string) []string {
	t.Helper()

	fileSet := token.NewFileSet()

	parsed, err := parser.ParseFile(fileSet, file, nil, 0)
	if err != nil {
		t.Fatalf("failed to parse %s: %s", file, err)
	}

	var (
		body     *ast.BlockStmt
		receiver string
	)

	for _, declaration := range parsed.Decls {
		declared, ok := declaration.(*ast.FuncDecl)
		if !ok || declared.Name.Name != function || declared.Recv == nil {
			continue
		}

		if len(declared.Recv.List) != 1 || len(declared.Recv.List[0].Names) != 1 {
			t.Fatalf("%s's receiver is not a single named value; this guard reads calls on it", function)
		}

		body = declared.Body
		receiver = declared.Recv.List[0].Names[0].Name

		break
	}

	if body == nil {
		t.Fatalf("%s not found in %s — this guard is reading the wrong function", function, file)
	}

	var migrations []string

	ast.Inspect(body, func(node ast.Node) bool {
		// Postgres adds its columns in raw SQL, so the migration is inside a string literal rather
		// than in a call. Reading only calls would report that function as performing four
		// migrations when it performs fourteen.
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			for _, match := range addColumnPattern.FindAllStringSubmatch(literal.Value, -1) {
				migrations = append(migrations, match[1]+"."+match[2])
			}

			return true
		}

		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// Only calls directly on the RECEIVER. d.sql.Exec and the like are a nested selector, and
		// keying on "a selector over any identifier" would sweep up log.Errorf beside them — which
		// is the difference between a guard and a list of every function initSchema calls.
		identifier, ok := selector.X.(*ast.Ident)
		if !ok || identifier.Name != receiver {
			return true
		}

		if selector.Sel.Name == "addColumnIfMissing" {
			if len(call.Args) < 2 {
				t.Fatalf("addColumnIfMissing called with %d arguments — this guard reads the first two",
					len(call.Args))
			}

			migrations = append(migrations, stringLiteral(t, call.Args[0])+"."+stringLiteral(t, call.Args[1]))

			return true
		}

		migrations = append(migrations, selector.Sel.Name)

		return true
	})

	return migrations
}

// addColumnPattern matches Postgres's native column addition inside a SQL string literal.
var addColumnPattern = regexp.MustCompile(`(?i)ALTER TABLE\s+(\w+)\s+ADD COLUMN IF NOT EXISTS\s+(\w+)`)

// stringLiteral returns the value of a string-literal argument, failing when it is not one — a
// migration named by a variable would make this guard silently incomplete.
func stringLiteral(t *testing.T, expression ast.Expr) string {
	t.Helper()

	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		t.Fatalf("addColumnIfMissing is called with a non-literal argument, which this guard cannot "+
			"read: %#v", expression)
	}

	return strings.Trim(literal.Value, `"`)
}
