package db

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
	{"v0.34.0", "none - the control; migrating this must be a no-op"},
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

// TestSchemaUpgradeReadsEverySeededRow is the core assertion: a store written by an older release
// opens on HEAD with every row intact and every field the migrations moved still saying what it
// said. It runs against each schema band, so a migration that corrupts a value rather than failing
// outright is caught at the fixture whose schema it acts on.
func TestSchemaUpgradeReadsEverySeededRow(t *testing.T) {
	ctx := context.Background()

	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := openFixture(t, fixture.tag)

			memories, err := database.GetMemories(ctx, MemoryFilter{Limit: 100})
			if err != nil {
				t.Fatalf("GetMemories after migrating %s failed (%s): %s", fixture.tag, fixture.migrations, err)
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
				t.Fatalf("migrating %s left %d memories, want %d", fixture.tag, len(byId), len(seededMemories))
			}

			for _, want := range seededMemories {
				got, ok := byId[want.id]
				if !ok {
					t.Errorf("%s is missing after migrating %s", want.id, fixture.tag)

					continue
				}

				// Significance is the one that moved tables. Before v0.5.0 it was an integer column
				// on the row; migrateSignificanceToLevels lifts every distinct value into the shared
				// registry and repoints the row at a level id, so a fixture from v0.4.0 exercises
				// that path and must still read back the number it was written with.
				if got.significance != want.significance {
					t.Errorf("%s significance is %d after migrating %s, want %d",
						want.id, got.significance, fixture.tag, want.significance)
				}

				if got.eventId != want.eventId {
					t.Errorf("%s event_id is %q after migrating %s, want %q",
						want.id, got.eventId, fixture.tag, want.eventId)
				}

				if got.group != want.group {
					t.Errorf("%s group is %q after migrating %s, want %q",
						want.id, got.group, fixture.tag, want.group)
				}

				if got.isBinary != want.isBinary {
					t.Errorf("%s is_binary is %v after migrating %s, want %v",
						want.id, got.isBinary, fixture.tag, want.isBinary)
				}

				// The decay clock ages from the most recent recall, so losing this in a migration
				// would silently make a reinforced memory as forgettable as an untouched one.
				if got.recallCount != want.recallCount {
					t.Errorf("%s recall_count is %d after migrating %s, want %d",
						want.id, got.recallCount, fixture.tag, want.recallCount)
				}

				// Bodies written before compression existed are stored uncompressed and flagged so;
				// reads follow the per-row flag rather than the current configuration, which is what
				// lets a mixed store read correctly. A fixture from >= v0.23.0 carries both kinds.
				if !strings.Contains(got.bodyContains, want.bodyContains) {
					t.Errorf("%s body after migrating %s does not contain %q: %q",
						want.id, fixture.tag, want.bodyContains, got.bodyContains)
				}
			}
		})
	}
}

// TestSchemaUpgradeMetadataFilterAgainstEveryFixture is the regression that names the whole class.
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
func TestSchemaUpgradeMetadataFilterAgainstEveryFixture(t *testing.T) {
	ctx := context.Background()

	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := openFixture(t, fixture.tag)

			// Every seeded row predates metadata, so the correct answer is an empty page. An ERROR
			// is the defect; zero rows is the point.
			memories, err := database.GetMemories(ctx, MemoryFilter{
				Metadata: map[string]string{"source": "slack"},
				Limit:    100,
			})
			if err != nil {
				t.Fatalf("metadata-filtered read of a migrated %s store failed: %s", fixture.tag, err)
			}

			if len(*memories) != 0 {
				t.Errorf("metadata filter matched %d rows on a %s store whose rows carry none",
					len(*memories), fixture.tag)
			}

			// The same query with no metadata predicate must still see everything, so an empty
			// result above is the filter working rather than the store reading as empty.
			all, err := database.GetMemories(ctx, MemoryFilter{Limit: 100})
			if err != nil {
				t.Fatalf("unfiltered read of a migrated %s store failed: %s", fixture.tag, err)
			}

			if len(*all) != len(seededMemories) {
				t.Errorf("unfiltered read of a migrated %s store returned %d rows, want %d",
					fixture.tag, len(*all), len(seededMemories))
			}
		})
	}
}

// TestSchemaUpgradeContentSearchIsBackfilled covers initContentSearch's upgrade case: a store
// written by a version without the FTS index gains it on this startup, and must be populated from
// the existing rows rather than left empty. An empty index answers every search with nothing, which
// is indistinguishable from a store holding no match - so this is the other failure that reports
// itself as an ordinary empty result.
func TestSchemaUpgradeContentSearchIsBackfilled(t *testing.T) {
	ctx := context.Background()

	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := openFixture(t, fixture.tag)

			if !database.ContentSearchAvailable() {
				t.Skip("content search unavailable on this store")
			}

			hits, err := database.SearchMemoryHits(ctx, ContentQuery{Text: "zebras", Limit: 10})
			if err != nil {
				t.Fatalf("content search on a migrated %s store failed: %s", fixture.tag, err)
			}

			if len(hits) != 1 || hits[0].Id != "mem-loose-1" {
				ids := make([]string, 0, len(hits))
				for _, hit := range hits {
					ids = append(ids, hit.Id)
				}

				sort.Strings(ids)
				t.Errorf("searching a migrated %s store for \"zebras\" returned %v, want [mem-loose-1] "+
					"(an empty result here means the FTS index was created but never backfilled)",
					fixture.tag, ids)
			}

			// A binary body is never indexed, whether it was written before or after the index
			// existed - so the backfill must honour the same rule the write path does.
			binary, err := database.SearchMemoryHits(ctx, ContentQuery{Text: "YmluYXJ5", Limit: 10})
			if err != nil {
				t.Fatalf("content search for a binary body on a migrated %s store failed: %s", fixture.tag, err)
			}

			for _, hit := range binary {
				if hit.Id == "mem-binary" {
					t.Errorf("the backfill indexed a binary body on a migrated %s store", fixture.tag)
				}
			}
		})
	}
}

// TestSchemaUpgradeStoreIsConsolidatable is the third of the three verbs, and the one that matters
// most to a forgetting store: migrated rows must be VISIBLE TO THE DECAY PATH. A row that reads
// back correctly but that the consolidation scans cannot see is immortal by accident - which is
// exactly what item 23.2 found for memories with a dangling event_id, and is invisible to any test
// that only reads.
func TestSchemaUpgradeStoreIsConsolidatable(t *testing.T) {
	ctx := context.Background()

	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := openFixture(t, fixture.tag)

			// A server that consolidates nothing: the scans must still walk every row and reach a
			// decision on it. What is asserted is the walk, not the verdict.
			seen := &countingServer{}

			// All three passes, in the order sleep() runs them - memories without events, memories
			// with events, then events without memories. Running only the first would leave the
			// four memories that belong to an event unvisited, which is precisely the state this
			// test exists to detect.
			looseDeleted, err := database.ConsolidateMemories(ctx, seen)
			if err != nil {
				t.Fatalf("first consolidation pass over a migrated %s store failed: %s", fixture.tag, err)
			}

			eventMemoriesDeleted, eventsSeen, eventsDeleted, err := database.ConsolidateEventMemories(ctx, seen)
			if err != nil {
				t.Fatalf("second consolidation pass over a migrated %s store failed: %s", fixture.tag, err)
			}

			emptyEventsDeleted, err := database.ConsolidateEvents(ctx, seen)
			if err != nil {
				t.Fatalf("third consolidation pass over a migrated %s store failed: %s", fixture.tag, err)
			}

			if deleted := looseDeleted + eventMemoriesDeleted + eventsDeleted + emptyEventsDeleted; deleted != 0 {
				t.Errorf("the consolidation passes over a migrated %s store deleted %d records the "+
					"decider kept", fixture.tag, deleted)
			}

			// The whole point: every migrated row must be OFFERED to the decider. A row the scans
			// never reach is immortal by accident whatever it reads back as, which is exactly what
			// item 23.2 found for memories with a dangling event_id - and no read-only assertion
			// can see it.
			if seen.memories != len(seededMemories) {
				t.Errorf("the consolidation passes over a migrated %s store considered %d memories, want %d",
					fixture.tag, seen.memories, len(seededMemories))
			}

			// The second pass reports the events it walked; the third is offered only those with no
			// memories left, which after a pass that deleted nothing is just the one seeded empty
			// event.
			if eventsSeen != seededEventsWithMemories {
				t.Errorf("the second consolidation pass over a migrated %s store walked %d events, want %d",
					fixture.tag, eventsSeen, seededEventsWithMemories)
			}

			if seen.events != seededEvents-seededEventsWithMemories {
				t.Errorf("the third consolidation pass over a migrated %s store considered %d empty events, want %d",
					fixture.tag, seen.events, seededEvents-seededEventsWithMemories)
			}

			// CountMemories splits on event membership, so this also pins that the event_id column
			// survived: four of the seeded memories belong to an event and four do not.
			withEvent, withoutEvent := database.CountMemories(ctx)
			if withEvent != seededMemoriesWithEvent {
				t.Errorf("a migrated %s store counts %d memories with an event, want %d",
					fixture.tag, withEvent, seededMemoriesWithEvent)
			}

			if withoutEvent != len(seededMemories)-seededMemoriesWithEvent {
				t.Errorf("a migrated %s store counts %d memories without an event, want %d",
					fixture.tag, withoutEvent, len(seededMemories)-seededMemoriesWithEvent)
			}
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

// TestEverySchemaMigrationHasAFixture is the drift guard, and it is the half of this file that
// survives contact with the next migration.
//
// The fixtures above are only as good as their coverage: a migration added after v0.34.0 has no
// fixture predating it, so nothing here would exercise it and every test in this file would go on
// passing. This reads the migrations initSchema actually performs out of the source and requires
// each to be declared below against the newest fixture that predates it — so adding one fails the
// build until somebody decides whether a new fixture is needed.
//
// It is deliberately two-directional, the shape 74.6 and 74.7 settled on: an undeclared migration
// fails, and so does a declaration whose migration has gone. The second direction is what stops the
// list becoming a record of what initSchema used to do.
func TestEverySchemaMigrationHasAFixture(t *testing.T) {
	// Each migration initSchema performs, against the newest fixture written BEFORE it existed —
	// i.e. the fixture that exercises it. A migration whose predecessor schema was never released
	// (one added and shipped in the same release as the table it touches) names notReleasedBefore.
	declared := map[string]string{
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
		"initContentSearch":              "v0.23.0",

		// The two indexes are rebuilt from whatever columns exist rather than migrating data, so
		// every fixture exercises them; the oldest is named because it is the one that proves the
		// covering index survives being rebuilt onto significance_level_id.
		"ensureCoveringIndex": "v0.4.0",
		"ensureListingIndex":  "v0.4.0",
	}

	// Calls inside initSchema that are not migrations, each with the reason it is exempt. Kept
	// beside the declarations rather than filtered silently, so a new helper lands in a list
	// somebody reads instead of disappearing.
	exempt := map[string]string{
		"significanceLevelsDDL": "returns DDL for the CREATE TABLE above it; not itself a migration step",
	}

	performed := migrationsInInitSchema(t)

	for _, migration := range performed {
		if _, ok := exempt[migration]; ok {
			continue
		}

		tag, ok := declared[migration]
		if !ok {
			t.Errorf("initSchema performs %q but no fixture is declared for it.\n"+
				"Decide which released schema predates it and add it to `declared` above; if that "+
				"schema was never released, name notReleasedBefore. If a new fixture is needed, "+
				"generate it with scripts/schema-fixtures.sh and add it to schemaFixtures.", migration)

			continue
		}

		if tag == notReleasedBefore {
			continue
		}

		if _, err := os.Stat(filepath.Join("testdata", "schema", tag, "hippocampus.db")); err != nil {
			t.Errorf("%q is declared against fixture %s, which is not on disk: %s", migration, tag, err)
		}
	}

	// The reverse direction: a declaration whose migration initSchema no longer performs.
	for migration := range declared {
		found := false

		for _, name := range performed {
			if name == migration {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("a fixture is declared for %q but initSchema no longer performs it — remove the "+
				"declaration (and consider whether its fixture is still earning its place)", migration)
		}
	}

	for migration := range exempt {
		found := false

		for _, name := range performed {
			if name == migration {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("%q is exempted but initSchema no longer calls it — remove the exemption", migration)
		}
	}
}

// notReleasedBefore marks a migration with no released predecessor schema: it was added in the same
// release as the table or column it acts on, so there is no older store that needs it and no
// fixture can exercise it.
const notReleasedBefore = "-"

// migrationsInInitSchema reads the migration steps out of initSchema's source rather than taking a
// list on trust — the list IS what drifts, which is the entire lesson of 74.7. An
// addColumnIfMissing call is named "<table>.<column>"; any other method called on the receiver is
// named by the method.
func migrationsInInitSchema(t *testing.T) []string {
	t.Helper()

	fileSet := token.NewFileSet()

	file, err := parser.ParseFile(fileSet, "db.go", nil, 0)
	if err != nil {
		t.Fatalf("failed to parse db.go: %s", err)
	}

	var (
		body     *ast.BlockStmt
		receiver string
	)

	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "initSchema" || function.Recv == nil {
			continue
		}

		if len(function.Recv.List) != 1 || len(function.Recv.List[0].Names) != 1 {
			t.Fatalf("initSchema's receiver is not a single named value; this guard reads calls on it")
		}

		body = function.Body
		receiver = function.Recv.List[0].Names[0].Name

		break
	}

	if body == nil {
		t.Fatal("initSchema not found in db.go — this guard is reading the wrong function")
	}

	var migrations []string

	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// Only calls directly on the RECEIVER. d.sql.Exec and the like are a nested selector, and
		// keying on "a selector over any identifier" would sweep up log.Errorf beside them —
		// which is the difference between a guard and a list of every function initSchema calls.
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
