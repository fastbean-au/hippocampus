package db

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// This file is the whole of what the package knows about the differences between the SQL dialects
// it speaks. Everything else - the queries, the consolidation scans, the schema, the accounting - is
// written once and reads its dialect-specific fragments from here.
//
// That boundary is the point, and it is enforced: TestDialectKnowledgeIsConfined fails the build if
// any file in the package outside the declared dialect files compares d.driver. There are two of
// those files, and the other is metadata.go - the JSON accessors and the byte-length expression are
// dialect knowledge too, and live beside the reasoning about NULL-versus-empty-string that they
// cannot be understood apart from. A fourth dialect is a row in the table below plus an arm in each
// of that file's two switches.
//
// Before this file existed the same
// knowledge was spelled out at fifty-odd call sites across thirteen files, three CREATE TABLE
// statements at a time, which had two consequences worth stating plainly. Adding a fourth dialect
// meant finding all fifty. And a dialect that was merely UNTESTED on a path looked exactly like one
// that was handled there - which is how spreading activation came to be silently inert on Postgres.
//
// The design is a table rather than an interface with three implementations. Almost every
// difference is one token - a column type, a function name, a capability - and a table puts all
// three answers on adjacent lines where they can be compared, rather than in three files where they
// can quietly drift. The handful of differences that are structural rather than lexical (the upsert
// form, index management, the registry lock) stay as switches, but they stay HERE.

// dialect describes one SQL dialect. Every field is either a fragment spliced into shared SQL or a
// capability the shared code branches on; there is no behaviour in the struct itself.
type dialect struct {
	// name is the dialect's name for logs and test failures.
	name string

	// --- Placeholders --------------------------------------------------------------------------

	// numberedPlaceholders is set for a dialect wanting $1, $2 ... rather than the package's shared
	// ? style. See rebind.
	numberedPlaceholders bool

	// --- Column types --------------------------------------------------------------------------
	//
	// Spliced into the shared CREATE TABLE templates. The three dialects differ only in these, which
	// is why the templates can be written once.

	// idType is an indexed identifier column compared BYTE FOR BYTE.
	//
	// Both halves of that are load-bearing on MySQL, and neither is on the other two. It cannot use
	// an unbounded TEXT as a primary key or in a full-column index, hence the length - 255
	// characters comfortably holds the generated UUIDs and stays inside InnoDB's utf8mb4 index-width
	// limit. And its default collation (utf8mb4_0900_ai_ci) is case- and accent-insensitive, under
	// which ids differing only in case would be the SAME KEY: client ids "abc" and "ABC" would
	// collide (a duplicate key on create, a silent merge on import) and keyset pagination would walk
	// a different order, so the same archive would change record identity across drivers.
	//
	// group_name takes this type too, for the same two reasons: it is indexed, and group scoping
	// compares it byte-for-byte (see auth.GroupInScope).
	idType string

	// labelType is a short indexed text column that is not an id (a hostname, a version, a role).
	// It shares idType's indexability constraint but not its collation requirement.
	labelType string

	// textType is an unbounded, unindexed text column.
	textType string

	// blobType is an opaque byte column - memory bodies, which may hold a gzip stream.
	blobType string

	// bigintType is a 64-bit integer column. Nanosecond timestamps and byte counts are stored in
	// these, so an INTEGER that infers 32 bits is not an option: Postgres types a whole expression,
	// including the placeholder compared against it, from the column, so a narrow column makes the
	// arithmetic overflow at runtime rather than the schema complain at startup.
	bigintType string

	// doubleType is a 64-bit floating point column.
	doubleType string

	// boolType is a boolean column. Bound as a Go bool everywhere, which is correct against all
	// three whether the storage is an integer or a native boolean.
	boolType string

	// jsonType is the metadata column's type. It must be NULL-able with no DEFAULT wherever it is
	// used; see the schema comment on metadata.
	jsonType string

	// boolFalse is the false literal a boolean column defaults to. Kept separate from boolType
	// rather than folded into it because the dialect storing booleans as integers wants the integer
	// literal, and writing FALSE there would change the column's declared type and so its affinity.
	boolFalse string

	// textDefaultEmpty is the DEFAULT clause giving a text column the empty string, INCLUDING its
	// leading space, or empty where the dialect forbids a default on a text column at all. Every
	// insert in the package supplies those columns explicitly, so the absence costs nothing.
	textDefaultEmpty string

	// blobDefaultEmpty is the same for a byte column, whose empty literal all three spell
	// differently where they allow one.
	blobDefaultEmpty string

	// autoIncrementPK is a server-assigned monotonic surrogate primary key, spelled three
	// completely different ways.
	autoIncrementPK string

	// --- Expression fragments -----------------------------------------------------------------

	// greatestFunc is the SCALAR two-argument maximum. SQLite spells it MAX, whose two-argument
	// form is a scalar function; the server dialects' MAX is aggregate-only and the scalar is
	// GREATEST.
	greatestFunc string

	// --- Capabilities ---------------------------------------------------------------------------

	// singleWriter is set for a dialect admitting one writer at a time, which therefore cannot
	// deadlock against itself and needs no write retry.
	singleWriter bool

	// returning is set for a dialect supporting INSERT/UPDATE/DELETE ... RETURNING. Where it is
	// not, the affected rows have to be read in a separate statement inside the same transaction.
	returning bool

	// upsertExcluded names the row alias carrying the proposed values inside an upsert's update
	// clause. Empty means the dialect wants MySQL's ON DUPLICATE KEY UPDATE form instead of the
	// standard ON CONFLICT one; see upsert.
	upsertExcluded string

	// indexIfNotExists is set for a dialect supporting CREATE INDEX IF NOT EXISTS and DROP INDEX IF
	// EXISTS. Where it is not, existence has to be probed first.
	indexIfNotExists bool

	// addColumnIfNotExists is set for a dialect supporting ALTER TABLE ... ADD COLUMN IF NOT
	// EXISTS natively, making the probe in addColumnIfMissing unnecessary there.
	addColumnIfNotExists bool

	// pageAccounting is set where UsedBytes measures the storage's own pages rather than estimating
	// from the live rows. Only the embedded dialect can: no server's file-size measure ever shrinks
	// after a delete, so eviction would chase a figure that cannot drop.
	pageAccounting bool

	// compacts is set where Preserve does real work (an incremental vacuum and a WAL checkpoint).
	// Both servers reclaim space on their own schedule and the call is a no-op there.
	compacts bool

	// walFile is set where there is a client-visible write-ahead log file to stat, which is what
	// consolidation.walTriggerBytes needs.
	walFile bool

	// contentSearch is set where the FTS5 content index is available - the embedded dialect only,
	// since it is a SQLite virtual table rather than anything portable.
	contentSearch bool

	// instanceRegistry is set where the peer registry table is kept. Deliberately off for the
	// embedded dialect: it is single-instance by construction, so the table would have exactly one
	// row, and its page-based UsedBytes would let the record of the deployment raise capacity
	// pressure and evict live memories to make room for itself.
	instanceRegistry bool

	// countsChangedRows is set for a dialect whose UPDATE reports rows CHANGED rather than rows
	// MATCHED, so a no-op update is indistinguishable from one that matched nothing and existence
	// has to be confirmed separately.
	countsChangedRows bool

	// idCollationMigration is set where an id column's collation is a property that can be wrong on
	// a database created by an older version and has to be corrected in place. Only the dialect
	// whose default collation is case-insensitive has one - the others compare byte-for-byte with no
	// collation to state. See the id_collation migration.
	idCollationMigration bool
}

// dialects is the table. One row per dialect; a fourth is a fourth row plus whatever structural
// helpers below do not already cover it.
var dialects = map[driver]*dialect{

	driverSQLite: {
		name:                 "sqlite",
		numberedPlaceholders: false,
		idType:               "TEXT",
		labelType:            "TEXT",
		textType:             "TEXT",
		blobType:             "BLOB",
		// SQLite's INTEGER is a variable-width signed integer up to 64 bits, so it is already the
		// 64-bit column the server dialects have to say BIGINT for.
		bigintType:           "INTEGER",
		doubleType:           "REAL",
		boolType:             "INTEGER",
		jsonType:             "TEXT",
		boolFalse:            "0",
		textDefaultEmpty:     " DEFAULT ''",
		blobDefaultEmpty:     " DEFAULT x''",
		autoIncrementPK:      "INTEGER PRIMARY KEY AUTOINCREMENT",
		greatestFunc:         "MAX",
		singleWriter:         true,
		returning:            true,
		upsertExcluded:       "excluded",
		indexIfNotExists:     true,
		addColumnIfNotExists: false,
		pageAccounting:       true,
		compacts:             true,
		walFile:              true,
		contentSearch:        true,
		instanceRegistry:     false,
		countsChangedRows:    false,
		idCollationMigration: false,
	},

	driverPostgres: {
		name:                 "postgres",
		numberedPlaceholders: true,
		idType:               "TEXT",
		labelType:            "TEXT",
		textType:             "TEXT",
		blobType:             "BYTEA",
		bigintType:           "BIGINT",
		doubleType:           "DOUBLE PRECISION",
		boolType:             "BOOLEAN",
		jsonType:             "JSONB",
		boolFalse:            "FALSE",
		textDefaultEmpty:     " DEFAULT ''",
		blobDefaultEmpty:     " DEFAULT ''::bytea",
		autoIncrementPK:      "BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY",
		greatestFunc:         "GREATEST",
		singleWriter:         false,
		returning:            true,
		upsertExcluded:       "excluded",
		indexIfNotExists:     true,
		addColumnIfNotExists: true,
		pageAccounting:       false,
		compacts:             false,
		walFile:              false,
		contentSearch:        false,
		instanceRegistry:     true,
		countsChangedRows:    false,
		idCollationMigration: false,
	},

	driverMySQL: {
		name:                 "mysql",
		numberedPlaceholders: false,
		idType:               "VARCHAR(255) COLLATE " + mysqlBinaryCollation,
		labelType:            "VARCHAR(255)",
		textType:             "TEXT",
		blobType:             "LONGBLOB",
		bigintType:           "BIGINT",
		doubleType:           "DOUBLE",
		boolType:             "BOOLEAN",
		jsonType:             "JSON",
		boolFalse:            "FALSE",
		// MySQL allows only expression defaults on TEXT and BLOB columns, so neither gets one.
		textDefaultEmpty: "",
		blobDefaultEmpty: "",
		autoIncrementPK:  "BIGINT AUTO_INCREMENT PRIMARY KEY",
		greatestFunc:     "GREATEST",
		singleWriter:     false,
		// MySQL has no RETURNING in any form - not on INSERT, UPDATE or DELETE.
		returning: false,
		// Empty selects the ON DUPLICATE KEY UPDATE form; the row alias it names is supplied by
		// upsert itself.
		upsertExcluded:       "",
		indexIfNotExists:     false,
		addColumnIfNotExists: false,
		pageAccounting:       false,
		compacts:             false,
		walFile:              false,
		contentSearch:        false,
		instanceRegistry:     true,
		countsChangedRows:    true,
		idCollationMigration: true,
	},
}

// dialect returns the active dialect's description. The driver field is set once at construction
// and never changes, so this needs no lock and cannot fail: an unknown driver value would be a
// programming error in a constructor, not a runtime condition, and is reported as one.
func (d *DB) dialect() *dialect {
	dialect, ok := dialects[d.driver]
	if !ok {
		log.Panicf("no dialect registered for driver %d - a constructor set an unknown driver", d.driver)
	}

	return dialect
}

// rebind converts the package's shared ?-style placeholders to the numbered style a dialect wants.
// Queries throughout the package are written with ?; the dialects that consume them as-is get the
// query back untouched. None of the package's SQL carries a literal '?' inside a string, so a plain
// character scan is sufficient.
func (d *DB) rebind(query string) string {
	if !d.dialect().numberedPlaceholders {
		return query
	}

	var b strings.Builder

	n := 0

	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			b.WriteByte(query[i])

			continue
		}

		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}

	return b.String()
}

// greatest is the scalar two-argument maximum over the named expressions - the decay clock, which
// is the later of a row's creation and its last recall. Every consolidation, preview, retention and
// reinforcement scan ages from it, so it must mean the same thing on every dialect.
func (d *DB) greatest(a string, b string) string {
	return d.dialect().greatestFunc + `(` + a + `, ` + b + `)`
}

// upsertSpec describes an INSERT that re-writes the row when its key is already present. Every
// upsert in the package is this shape, which is why they share one builder rather than each
// carrying its own pair of dialect arms.
type upsertSpec struct {
	// table is the target table.
	table string

	// columns is the inserted column list, comma-separated, in the same spelling the package's
	// SELECTs use so a spec can be written against an existing column constant. The generated
	// VALUES list is sized from it.
	columns string

	// key is the conflicting unique or primary key.
	key []string

	// update names the columns re-written from the proposed row when the key already exists. A
	// column in columns but not here keeps its stored value - which is how a re-link re-weights an
	// edge without disturbing when it was created.
	//
	// Empty means keep the stored row entirely: an insert that loses the race leaves the winner's
	// row exactly as it was, which is what a ledger recording when something FIRST happened needs.
	update []string

	// values overrides the generated single-row VALUES list. The bulk import paths build one tuple
	// per row and pass the whole clause here; an empty string means one row of placeholders is
	// generated from the column count.
	values string
}

// upsert renders an upsertSpec in the active dialect.
//
// The two forms differ in more than spelling. The standard one names the conflicting key explicitly
// and refers to the proposed row through a fixed alias; MySQL infers the key from whichever unique
// index the insert violated and needs the proposed row aliased by the statement itself (the AS
// clause, which is why MySQL 8.0.20 is the floor - the older VALUES() spelling is deprecated).
// Inferring the key is a real difference and not merely a syntactic one, but every upsert here has
// exactly one unique key, so the two agree.
func (d *DB) upsert(spec upsertSpec) string {
	values := spec.values

	if values == "" {
		values = `(` + placeholders(strings.Count(spec.columns, ",")+1) + `)`
	}

	insert := `INSERT INTO ` + spec.table + ` (` + spec.columns + `)
		VALUES ` + values

	alias := d.dialect().upsertExcluded
	if alias == "" {
		alias = "new"
	}

	// Keeping the stored row is the third form, and the two dialects disagree about it more than
	// they do about updating. One says so directly; the other has no DO NOTHING and expresses it as
	// an assignment that changes nothing, which is the idiom rather than a trick.
	if len(spec.update) == 0 {
		if d.dialect().upsertExcluded == "" {
			key := spec.key[0]

			return insert + `
		ON DUPLICATE KEY UPDATE ` + key + ` = ` + key
		}

		return insert + `
		ON CONFLICT (` + strings.Join(spec.key, ", ") + `) DO NOTHING`
	}

	assignments := make([]string, len(spec.update))
	for i, column := range spec.update {
		assignments[i] = column + " = " + alias + "." + column
	}

	if d.dialect().upsertExcluded == "" {
		return insert + ` AS ` + alias + `
		ON DUPLICATE KEY UPDATE ` + strings.Join(assignments, ", ")
	}

	return insert + `
		ON CONFLICT (` + strings.Join(spec.key, ", ") + `) DO UPDATE SET ` + strings.Join(assignments, ", ")
}

// columnProbe is the query asking whether a table has a given column, bound to (table, column).
// SQLite exposes a table's shape as the pragma_table_info table-valued function; both server
// dialects have information_schema but disagree about how the current schema is named. Written in
// the shared ? style, so the caller rebinds like any other query.
func (d *DB) columnProbe() string {
	switch d.driver {

	case driverMySQL:
		return `SELECT column_name FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`

	case driverPostgres:
		return `SELECT column_name FROM information_schema.columns
			WHERE table_schema = CURRENT_SCHEMA() AND table_name = ? AND column_name = ?`

	default:
		return `SELECT name FROM pragma_table_info(?) WHERE name = ?`

	}
}

// ensureIndex creates an index if it is not already present, idempotently, on any dialect. Runs at
// init time on the bare handle, like the rest of the schema work.
//
// The dialects that support CREATE INDEX IF NOT EXISTS simply use it. MySQL has no such form and no
// way to ask for one, so it probes information_schema first - which is a race in principle and not
// in practice: schema init runs before the instance serves, and the server drivers already admit
// exactly one consolidating instance.
func (d *DB) ensureIndex(table string, name string, columns string) error {
	definition := `CREATE INDEX ` + name + ` ON ` + table + ` ` + columns

	if d.dialect().indexIfNotExists {
		if _, err := d.sql.Exec(`CREATE INDEX IF NOT EXISTS ` + name + ` ON ` + table + ` ` + columns); err != nil {
			log.Errorf("failed to create index '%s' on '%s': %s", name, table, err.Error())

			return err
		}

		return nil
	}

	exists, err := d.indexExists(table, name)
	if err != nil {
		return err
	}

	if exists {
		return nil
	}

	if _, err := d.sql.Exec(definition); err != nil {
		log.Errorf("failed to create index '%s' on '%s': %s", name, table, err.Error())

		return err
	}

	return nil
}

// dropIndexIfExists removes an index if it is present, idempotently, on any dialect. The same
// split as ensureIndex, plus MySQL's DROP INDEX naming the table where the others do not.
func (d *DB) dropIndexIfExists(table string, name string) error {
	if d.dialect().indexIfNotExists {
		if _, err := d.sql.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
			log.Errorf("failed to drop index '%s': %s", name, err.Error())

			return err
		}

		return nil
	}

	exists, err := d.indexExists(table, name)
	if err != nil {
		return err
	}

	if !exists {
		return nil
	}

	if _, err := d.sql.Exec(`DROP INDEX ` + name + ` ON ` + table); err != nil {
		log.Errorf("failed to drop index '%s' on '%s': %s", name, table, err.Error())

		return err
	}

	return nil
}

// indexExists probes for an index by name on the dialects that cannot ask for one conditionally.
// Only reached from ensureIndex and dropIndexIfExists, and only where indexIfNotExists is false.
func (d *DB) indexExists(table string, name string) (bool, error) {
	var count int

	probe := `SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`

	if err := d.sql.QueryRow(d.rebind(probe), table, name).Scan(&count); err != nil {
		log.Errorf("failed to check for index '%s' on '%s': %s", name, table, err.Error())

		return false, err
	}

	return count > 0, nil
}

// namedLock takes a session-scoped cross-instance lock on the supplied connection and returns its
// release. Both server dialects have one and spell it completely differently - one takes a numeric
// key, the other a string it must be told to scope to the current schema - which is the whole reason
// this lives here rather than at either of its two call sites. The embedded dialect needs no lock at
// all, its single connection already serialising every writer, and its callers say so before
// reaching here.
//
// The lock is held by the SESSION, so the connection must stay checked out for as long as the lock
// is wanted; every caller pins one and closes it after releasing.
func (d *DB) namedLock(ctx context.Context, conn *sql.Conn, key int64, name string) (func(), error) {
	switch d.driver {

	case driverPostgres:
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
			return nil, err
		}

		return func() {
			_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		}, nil

	case driverMySQL:
		// Scoped to the schema, because a MySQL named lock is server-global: two deployments sharing
		// one server would otherwise serialise against each other.
		if _, err := conn.ExecContext(ctx, `SELECT GET_LOCK(CONCAT(?, ':', DATABASE()), ?)`, name, int(schemaLockTimeout.Seconds())); err != nil {
			return nil, err
		}

		return func() {
			_, _ = conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(CONCAT(?, ':', DATABASE()))`, name)
		}, nil

	}

	return func() {}, nil
}

// reacquireInstanceLock retakes the single-consolidator lock on a fresh pinned connection after the
// old session died. Only the server dialects hold one; see verifyInstanceLock.
func (d *DB) reacquireInstanceLock() error {
	switch d.driver {

	case driverPostgres:
		return d.acquireInstanceLock()

	case driverMySQL:
		return d.acquireMySQLInstanceLock()

	}

	return nil
}

// coreSchemaStatements is the CREATE TABLE for events and memories - the two tables holding
// everything the service stores - rendered for the active dialect.
//
// One template rather than three, and that is the point rather than a tidying. The three copies it
// replaced differed ONLY in the type of each column, so keeping them apart bought nothing and cost
// the one failure this schema cannot afford: a column added to one dialect's copy and forgotten in
// another, which produces a store that opens, serves, and is missing a field on exactly one backend.
//
// Returned as separate statements because one of the dialects rejects a multi-statement string
// unless its DSN opts in, and requiring that of every deployment for startup DDL alone is not worth
// it.
func (d *DB) coreSchemaStatements() []string {
	dialect := d.dialect()

	boolean := dialect.boolType + ` NOT NULL DEFAULT ` + dialect.boolFalse
	bigint := dialect.bigintType + ` NOT NULL DEFAULT 0`
	label := dialect.idType + ` NOT NULL DEFAULT ''`
	text := dialect.textType + ` NOT NULL` + dialect.textDefaultEmpty

	return []string{
		`CREATE TABLE IF NOT EXISTS events (
			id                        ` + dialect.idType + ` PRIMARY KEY,
			time_start                ` + bigint + `,
			time_end                  ` + bigint + `,
			significance_level_id     ` + dialect.bigintType + `,
			name                      ` + text + `,
			description               ` + text + `,
			memories_consolidated     ` + boolean + `,
			link_significance         ` + bigint + `,
			group_name                ` + label + `,
			metadata                  ` + dialect.jsonType + `
		)`,

		`CREATE TABLE IF NOT EXISTS memories (
			id                    ` + dialect.idType + ` PRIMARY KEY,
			timestamp             ` + bigint + `,
			significance_level_id ` + dialect.bigintType + `,
			event_id              ` + label + `,
			is_binary             ` + boolean + `,
			time_recalled         ` + bigint + `,
			recall_count          INTEGER NOT NULL DEFAULT 0,
			is_summary            ` + boolean + `,
			group_name            ` + label + `,
			is_compressed         ` + boolean + `,
			link_significance     ` + bigint + `,
			body                  ` + dialect.blobType + ` NOT NULL` + dialect.blobDefaultEmpty + `,
			metadata              ` + dialect.jsonType + `
		)`,
	}
}

// migrateCoreColumns adds the columns that arrived after events and memories were first created, so
// a database written by an older version of the service is migrated in place on startup. CREATE
// TABLE IF NOT EXISTS alone would silently skip every one of them on a table that already exists.
//
// Shared for the same reason coreSchemaStatements is: three lists that must stay in lockstep are
// three chances to forget one, and a forgotten entry only fails on the backend nobody ran.
func (d *DB) migrateCoreColumns() error {
	log.Trace("func() db.migrateCoreColumns")

	for _, column := range d.coreColumnMigrations() {
		if err := d.addColumnIfMissing(column.table, column.column, column.definition); err != nil {
			log.Errorf("failed to migrate %s.%s for %s: %s", column.table, column.column, column.why, err.Error())

			return err
		}
	}

	return nil
}

// coreColumn is one column added to events or memories after that table was first created.
type coreColumn struct {
	table      string
	column     string
	definition string

	// why names the feature the column arrived with, for the log line when adding it fails.
	why string
}

// coreColumnMigrations is the list migrateCoreColumns applies, rendered for the active dialect. It
// is a function of its own rather than a literal inside the loop so the schema-fixture drift guard
// can read the pairs out of it, and so a test can count them.
func (d *DB) coreColumnMigrations() []coreColumn {
	dialect := d.dialect()

	boolean := dialect.boolType + ` NOT NULL DEFAULT ` + dialect.boolFalse
	label := dialect.idType + ` NOT NULL DEFAULT ''`

	return []coreColumn{
		{"memories", "is_summary", boolean,
			"summaries, so a replaced event does not immediately requalify as a candidate"},

		// Bodies written before compression existed are all uncompressed, which is what the
		// column's default already says of them, so adding it is the whole migration.
		{"memories", "is_compressed", boolean, "body compression"},

		// Named group_name rather than group because GROUP is a reserved word in every dialect the
		// service speaks. It takes the id type so it stays indexable and compares byte-for-byte,
		// like the ids it is scoped alongside.
		{"memories", "group_name", label, "group labels"},
		{"events", "group_name", label, "group labels"},

		// The significance registry columns (see significance.go); backfilled from the old per-item
		// significance column by migrateSignificanceToLevels, after which the covering index is
		// rebuilt on significance_level_id.
		{"memories", "significance_level_id", dialect.bigintType, "the significance registry"},
		{"events", "significance_level_id", dialect.bigintType, "the significance registry"},

		// The link graph's denormalised aggregate (see link.go). 0 is right for a database that
		// predates links: it has none, and initLinkTables creates the graph empty.
		{"memories", "link_significance", dialect.bigintType + ` NOT NULL DEFAULT 0`, "the link graph"},
		{"events", "link_significance", dialect.bigintType + ` NOT NULL DEFAULT 0`, "the link graph"},

		// Metadata (see types/metadata.go) is deliberately NULL-able with no default, unlike
		// group_name beside it, and must stay that way on every dialect: a JSON accessor raises
		// "malformed JSON" on an empty string but returns NULL for NULL, so an ''-defaulted column
		// would make the FIRST metadata-filtered query fail against every row written before this
		// migration - a failure invisible to any fresh-database test.
		{"memories", "metadata", dialect.jsonType, "metadata"},
		{"events", "metadata", dialect.jsonType, "metadata"},
	}
}

// dialectFiles names the files permitted to know which dialect is active. Everything else in the
// package reads a fragment or a capability from the table above; see TestDialectKnowledgeIsConfined,
// which is what keeps that true.
var dialectFiles = map[string]bool{
	"dialect.go": true,

	// The JSON accessors and the metadata byte-length expression. They live apart from the table
	// because neither is a single token: each is an expression whose shape differs per dialect, and
	// each is inseparable from a paragraph about why (the NULL-versus-empty-string trap, and MySQL's
	// mandatory COLLATE on an unquoted JSON value).
	"metadata.go": true,
}
