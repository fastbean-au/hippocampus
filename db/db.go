package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"

	"github.com/fastbean-au/hippocampus/types"
)

// DataFile is the name of the SQLite database file within the storage directory.
const DataFile = "hippocampus.db"

// instanceLockCheckInterval is how often the server drivers ping the pinned lock connection. The
// ping doubles as activity keeping the session from being reaped by a server-side idle timeout
// (e.g. MySQL's wait_timeout), and detects a session that died anyway so the lock can be retaken
// or the process can fail-stop before a second instance runs concurrently against the same data.
const instanceLockCheckInterval = 60 * time.Second

// instanceLockCheckTimeout bounds a single keepalive ping and reacquisition attempt.
const instanceLockCheckTimeout = 10 * time.Second

// instanceLockAcquireTimeout is how long acquiring the instance lock waits for the named lock to
// become free before concluding another instance holds it. It is deliberately non-zero: MySQL
// releases a GET_LOCK lock when its owning session ends, but a cleanly closed session's teardown
// (and thus its lock release) is asynchronous on the server, so a legitimate restart or failover -
// or one test opening right after the previous one closed - could otherwise be refused while the
// prior session's lock is still momentarily lingering. Waiting a few seconds bridges that gap while
// still failing fast enough against a genuinely running second instance.
const instanceLockAcquireTimeout = 5 * time.Second

// serverConnMaxLifetime caps how long a pooled connection (server drivers) is reused before being
// recycled, kept well under common server idle timeouts so the pool never hands out a connection
// the server has already closed. It does not reap the pinned lock connection, which is never
// returned to the pool while held (go-sql-driver's README recommends this be under wait_timeout).
const serverConnMaxLifetime = 5 * time.Minute

// driver identifies which SQL dialect the DB speaks. Both backends run through database/sql, and
// nearly all of the query and consolidation logic is identical, so the dialect is a field on one
// shared implementation rather than a second copy of it. The genuinely divergent pieces — DDL,
// placeholder style, and the compaction/size-accounting methods — branch on this.
type driver int

const (
	driverSQLite driver = iota
	driverPostgres
	driverMySQL
)

type DB struct {
	sql    *sql.DB
	driver driver

	// walFilePath is the on-disk WAL file's path, empty for the server drivers and for the
	// in-memory database used by tests (neither has one). Set once in New and never changed.
	walFilePath string

	// lockConn pins the session holding the instance lock (a Postgres advisory lock or a MySQL
	// GET_LOCK lock) for the lifetime of the process; both lock kinds are session-scoped, so the
	// lock would silently vanish if its connection were returned to the pool. Nil for the SQLite
	// driver.
	lockConn *sql.Conn

	// lockFile is the SQLite driver's equivalent: the open handle holding the inter-process file
	// lock on storage.directory (see lock.go), released by Close. Nil for the server drivers, for
	// the in-memory database, and for the read-only opens, none of which take it.
	lockFile *instanceLockFile

	// memoryDeleteObserver, when set, is invoked after a consolidation/eviction transaction
	// commits with the ids of the memory rows actually deleted, so the optional secondary search
	// index can be told about deletions the RPC layer never sees. Nil means no propagation. Set
	// once at startup, before serving, and never changed.
	memoryDeleteObserver func(ids []string)

	// readOnly marks a database opened for read-only tooling (NewSQLiteReadOnly, for
	// --backfill-search). Preserve becomes a no-op so Close does not attempt a WAL checkpoint or
	// incremental vacuum against a database a live service instance may own. SQLite only.
	readOnly bool

	// stopKeepalive / keepaliveStopped coordinate the instance-lock keepalive goroutine (server
	// drivers only; nil otherwise). Close signals stopKeepalive and waits for keepaliveStopped
	// before releasing lockConn, so the keepalive never races Close over lockConn.
	stopKeepalive    chan struct{}
	keepaliveStopped chan struct{}

	// queryTimeout bounds how long any single statement or transaction may run, applied inside the
	// exec/query/queryRow helpers and the transaction begins via a context deadline. Zero (the
	// default) disables it, preserving the previous unbounded behaviour. It exists so a hung or
	// unreachable database (network partition, storage stall, lock pileup) fails each operation
	// after a bounded time rather than blocking the calling goroutine — and its pooled connection —
	// indefinitely. Set once at startup via SetQueryTimeout, before serving, so it needs no lock.
	queryTimeout time.Duration

	// tombstones is the forgotten log's policy (see tombstone.go). The zero value records nothing,
	// which is the default. Set once at startup via SetTombstonePolicy, before serving, so it needs
	// no lock - the same treatment compression gets.
	tombstones TombstonePolicy

	// tombstoneTable records that initTombstones has run, so the read-only opens (which skip
	// initSchema entirely) never query a table they cannot be sure exists.
	tombstoneTable bool

	// compression is the write-side memory-body compression policy (see compress.go). The zero
	// value stores every body verbatim. It governs writes only — reads follow each row's own
	// is_compressed flag — and is set once at startup via SetCompression, before serving, so it
	// needs no lock.
	compression compression
}

// SetQueryTimeout sets the per-operation timeout (see the queryTimeout field). Called once at
// startup from main before the server begins serving; a non-positive duration leaves it disabled.
func (d *DB) SetQueryTimeout(timeout time.Duration) {
	if timeout <= 0 {
		return
	}

	d.queryTimeout = timeout
}

// SetPoolLimits caps the connection pool for the server drivers (postgres/mysql). database/sql
// otherwise allows an unlimited number of open connections (with an idle cap of 2), so a burst of
// concurrent RPCs opens as many connections as the burst is wide - on a shared database one hot
// replica can exhaust the server's connection slots and starve every other instance (and the
// instance-lock keepalive's reacquisition path). Called once at startup from main before serving; a
// non-positive maxOpenConns leaves the pool unbounded, and a non-positive maxIdleConns defaults to
// maxOpenConns so a steady load does not churn connections open and closed. The pinned lock
// connection counts as one of the open connections, so maxOpenConns must exceed 1. SQLite caps
// itself at one connection in New and never calls this.
func (d *DB) SetPoolLimits(maxOpenConns int, maxIdleConns int) {
	if maxOpenConns <= 0 {
		return
	}

	if maxIdleConns <= 0 {
		maxIdleConns = maxOpenConns
	}

	d.sql.SetMaxOpenConns(maxOpenConns)
	d.sql.SetMaxIdleConns(maxIdleConns)
}

// opContext derives the context bounding a single operation from the caller's context, so both the
// caller's own deadline/cancellation (an RPC's ctx) and the server-side queryTimeout apply -
// whichever fires first. When no timeout is configured it returns the parent unchanged with a no-op
// cancel, so callers can unconditionally `ctx, cancel := d.opContext(ctx); defer cancel()`. The
// caller owns the context's lifetime: for the row-returning helpers the deferred cancel must
// outlive iteration, which it does because the read methods consume their rows before returning
// (the consolidation scans already collect-then-close).
func (d *DB) opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if d.queryTimeout <= 0 {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, d.queryTimeout)
}

// startLockKeepalive launches the goroutine that keeps the instance lock alive and healthy for a
// server driver. It is a no-op when there is no lock connection (SQLite, and the read-only opens).
// The goroutine pings the pinned lock connection on a fixed interval and, if the lock is confirmed
// lost and cannot be retaken, fail-stops the process rather than let a second instance run
// concurrently against the same database.
func (d *DB) startLockKeepalive() {
	if d.lockConn == nil {
		return
	}

	d.stopKeepalive = make(chan struct{})
	d.keepaliveStopped = make(chan struct{})

	go func() {
		defer close(d.keepaliveStopped)

		ticker := time.NewTicker(instanceLockCheckInterval)
		defer ticker.Stop()

		for {
			select {

			case <-d.stopKeepalive:
				return

			case <-ticker.C:
				if err := d.verifyInstanceLock(); err != nil {
					log.Fatalf("lost the single-instance lock and could not reacquire it, exiting to avoid running concurrently with another instance: %s", err.Error())
				}
			}
		}
	}()
}

// verifyInstanceLock pings the pinned lock connection; if it has died - taking the session-scoped
// lock with it - it attempts exactly one reacquisition on a fresh pinned connection. It returns an
// error only when the lock is confirmed unheld and cannot be retaken (another instance holds it,
// or the database is unreachable), which the keepalive treats as fatal: continuing would risk two
// instances mutating the same database. A healthy connection, or a successful reacquisition,
// returns nil. Only ever called from the keepalive goroutine (and directly by tests), so its
// mutation of lockConn does not race Close, which stops the goroutine before touching lockConn.
func (d *DB) verifyInstanceLock() error {
	if d.lockConn == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), instanceLockCheckTimeout)
	defer cancel()

	if err := d.lockConn.PingContext(ctx); err == nil {
		return nil
	}

	log.Warn("instance lock connection is unhealthy - attempting to reacquire the lock")

	// The old session (and its lock) are gone; drop the dead connection and try to retake the lock
	// on a fresh pinned one via the driver's own acquisition path.
	_ = d.lockConn.Close()
	d.lockConn = nil

	switch d.driver {

	case driverPostgres:
		return d.acquireInstanceLock()

	case driverMySQL:
		return d.acquireMySQLInstanceLock()
	}

	return nil
}

// SetMemoryDeleteObserver registers the function called with the ids of memories deleted by the
// consolidation and eviction scans. It is deliberately on the concrete DB rather than the Store
// interface: it exists solely for the optional search index, and other backends are free to
// provide the same propagation differently.
func (d *DB) SetMemoryDeleteObserver(fn func(ids []string)) {
	d.memoryDeleteObserver = fn
}

// rebind converts ?-style placeholders to the $N style Postgres requires. Queries throughout the
// package are written with ?, the shared style; SQLite consumes them as-is. None of the package's
// SQL carries a literal '?' inside a string, so a plain character scan is sufficient.
func (d *DB) rebind(query string) string {
	if d.driver != driverPostgres {
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

// exec, query, and queryRow wrap the underlying database handle, rebinding placeholders for the
// active dialect and bounding each call by queryTimeout. All SQL in the package goes through these
// (or rebinds explicitly when running inside a transaction, which BeginTx bounds instead).
//
// exec owns its context fully: the statement completes before it returns, so the timeout context
// is created and cancelled here. query and queryRow return rows consumed by the caller after they
// return, so the caller must supply a context whose lifetime spans that consumption — every caller
// derives one with `ctx, cancel := d.opContext(ctx); defer cancel()`.
func (d *DB) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	bound := d.rebind(query)

	var res sql.Result

	// A single autocommit statement is safe to retry: a MySQL deadlock/lock-wait timeout rolls it
	// back whole, so a transient conflict re-runs rather than surfacing as a lost write. No-op on
	// the other drivers. See withWriteRetry.
	err := d.withWriteRetry(ctx, func() error {
		var execErr error

		res, execErr = d.sql.ExecContext(ctx, bound, args...)

		return execErr
	})

	return res, err
}

// beginTx opens a transaction bounded by queryTimeout. The returned cancel must be deferred by the
// caller: database/sql watches the context for the transaction's whole life and rolls it back if
// the context is cancelled, so cancelling on return both releases the timer and guarantees an
// abandoned transaction is not left open. When no timeout is configured the context is a plain
// background context and cancel is a no-op.
func (d *DB) beginTx(ctx context.Context) (*sql.Tx, context.CancelFunc, error) {
	ctx, cancel := d.opContext(ctx)

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	return tx, cancel, nil
}

func (d *DB) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.sql.QueryContext(ctx, d.rebind(query), args...)
}

func (d *DB) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return d.sql.QueryRowContext(ctx, d.rebind(query), args...)
}

// MemoryConsolidationCandidate carries everything the consolidation decision needs to know about
// a memory and its associated event.
//
// The two link significances are kept apart rather than added together because the value
// calculation damps each separately: an event's standing among events and a memory's standing among
// memories are different quantities, and folding them into one logarithm would let either mask the
// other. Both are read from the denormalised aggregate on the row, so the scans never join to the
// link tables.
type MemoryConsolidationCandidate struct {
	EventSignificance      int32
	MemorySignificance     int32
	EventLinkSignificance  int64
	MemoryLinkSignificance int64
	Timestamp              int64
	TimeRecalled           int64
	RecallCount            int32
}

// EventConsolidationCandidate carries everything the consolidation decision needs to know about
// an event that has no associated memories.
type EventConsolidationCandidate struct {
	Significance     int32
	LinkSignificance int64
	TimeStart        int64
	TimeEnd          int64
}

// MemoryFilter narrows a GetMemories query. A zero value on any field leaves that dimension
// unconstrained; Group matches the memory's group label exactly. SignificanceExtremum, when set,
// takes over from SignificanceMin/SignificanceMax (see memoryFilterConditions) rather than
// combining with them - callers should leave the range fields zero when it is set.
type MemoryFilter struct {
	TimeStampMin         int64
	TimeStampMax         int64
	SignificanceMin      int32
	SignificanceMax      int32
	SignificanceExtremum SignificanceExtremum
	Group                string
	OrderBy              string
	OrderDirection       SortDirection
	Limit                int
	Offset               int

	// Ids restricts the result to these memories, on top of every other field. It backs the
	// linked-to filter, which resolves a memory's neighbours and passes them here so traversal
	// composes with the other filters and with pagination. Empty means unrestricted, so a caller
	// holding an empty set must short-circuit rather than pass it.
	Ids []string

	// Groups is the CALLER'S SCOPE, and is a different thing from Group beside it: Group is the
	// filter a client asked for, Groups is the set of groups that client is permitted to see at all
	// (auth.Claims.Groups). They compose as a conjunction, so a client asking for a group outside
	// its scope matches nothing - an empty page, which is the correct answer and deliberately not
	// an error, since reporting one would confirm the group exists.
	//
	// Empty means unrestricted, per this struct's usual rule. That default is load-bearing: every
	// server-owned scan (the sleep cycle, the reconcile sweep, the search backfill) leaves it unset
	// and must keep seeing the whole store, since the decay dynamics are store-global and a
	// consolidation pass that skipped a group would simply never forget it.
	Groups []string

	// Metadata restricts the result to memories carrying every one of these key/value pairs
	// exactly - a conjunction, not a match on any. Empty means unrestricted. The predicate is
	// unindexed, exactly as Group's is.
	Metadata map[string]string

	// EventId restricts the result to one event's memories. Empty means unrestricted, per this
	// struct's usual rule - which is why HasEvent exists beside it: an event-less memory stores an
	// empty event_id, so this field cannot ask for those without the empty string meaning two things
	// at once. It is the paged counterpart to GetMemoriesForEvent, which returns a whole event's
	// memories in one unbounded slice.
	EventId string

	// Recalled, HasEvent, IsSummary and IsBinary are tri-state because the columns they filter are
	// boolean, or in HasEvent's case boolean in effect: a Go bool could not distinguish "only the
	// never-recalled ones" from "no restriction", and never-recalled is the question that filter
	// exists to answer. RecallCountMin/Max and TimeRecalledMin/Max follow the package's usual
	// 0-means-no-bound rule, which is exactly why Recalled is needed alongside them - RecallCountMax
	// of 0 reads as unbounded.
	Recalled        TriState
	HasEvent        TriState
	IsSummary       TriState
	IsBinary        TriState
	RecallCountMin  int32
	RecallCountMax  int32
	TimeRecalledMin int64
	TimeRecalledMax int64
}

// EventFilter narrows a GetEvents query. A zero value on any field leaves that dimension
// unconstrained; Group matches the event's group label exactly. SignificanceExtremum, when set,
// takes over from SignificanceMin/SignificanceMax (see eventFilterConditions) rather than
// combining with them - callers should leave the range fields zero when it is set.
type EventFilter struct {
	TimeStartMin         int64
	TimeStartMax         int64
	TimeEndMin           int64
	TimeEndMax           int64
	SignificanceMin      int32
	SignificanceMax      int32
	SignificanceExtremum SignificanceExtremum
	Group                string
	OrderBy              string
	OrderDirection       SortDirection
	Limit                int
	Offset               int

	// Metadata restricts the result to events carrying every one of these key/value pairs exactly,
	// as MemoryFilter.Metadata does for memories.
	Metadata map[string]string

	// Groups is the caller's scope, exactly as MemoryFilter.Groups is for memories - distinct from
	// Group, which is the filter the client asked for. See that field for why empty means the whole
	// store.
	Groups []string
}

// SignificanceExtremum mirrors contract.SignificanceExtremum without the db package depending on
// the contract package (see SignificancePlacement for the same pattern).
type SignificanceExtremum int

const (
	SignificanceExtremumNone SignificanceExtremum = iota
	SignificanceExtremumHighest
	SignificanceExtremumLowest
)

// SortDirection mirrors contract.SortDirection without the db package depending on the contract
// package (see SignificanceExtremum for the same pattern). SortDirectionNatural is the zero value
// because a filter's zero value must mean "no caller preference", which for ordering is each
// order_by value's own natural direction - see order.go.
type SortDirection int

const (
	SortDirectionNatural SortDirection = iota
	SortDirectionAscending
	SortDirectionDescending
)

// TriState mirrors contract.Bool without the db package depending on the contract package, for the
// filters over boolean columns. The three-valued form is what lets a filter say "only the false
// ones" - a plain bool would make that indistinguishable from asking for no restriction at all.
type TriState int

const (
	TriStateUnset TriState = iota
	TriStateFalse
	TriStateTrue
)

// SummarisationCandidate identifies an event whose memories have accumulated enough, and gone
// quiet for long enough, to be worth condensing into a single summary memory via
// ReplaceMemoriesWithSummary.
type SummarisationCandidate struct {
	EventId     string
	EventName   string
	MemoryCount int

	// Group is the event's group label, carried so GetSummarisationCandidates can serve a
	// group-scoped caller from the same store-wide cache the sleep cycle populates. The scan itself
	// must stay unscoped - a group it skipped would never be summarised for anyone - so the
	// narrowing happens on the way out, which is only possible if the group travels with the
	// candidate.
	Group string
}

type Server interface {
	ShouldConsolidateMemory(MemoryConsolidationCandidate) bool
	ShouldConsolidateEvent(EventConsolidationCandidate) bool

	// MemoryValue returns the memory's current decayed value, used by capacity eviction to rank
	// memories from least to most valuable.
	MemoryValue(MemoryConsolidationCandidate) float64

	// MemoryRetained reports whether a memory is still within its minimum retention window, so
	// capacity eviction must exclude it from the candidate pool even when the store is over its
	// byte target — the retention floor overrides the capacity limit.
	MemoryRetained(MemoryConsolidationCandidate) bool

	// DeletionThreshold is the capacity-pressure-scaled value a memory must stay above to survive
	// consolidation, as it stands for this cycle. It is not a decision — the passes still ask
	// ShouldConsolidateMemory — but the forgotten log records it beside each memory's value, since
	// the threshold moves with pressure and a value with nothing to measure it against is not a
	// record of anything.
	DeletionThreshold() float64
}

// Store is the storage-backend contract hippocampus.Server and stats.Start depend on, satisfied
// today by *DB. It covers exactly the methods those callers currently use, so a second backend
// (e.g. a client/server SQL database) can be swapped in without touching call sites.
//
// UsedBytes, WALBytes, and Preserve carry SQLite-specific semantics today (PRAGMA page accounting,
// an on-disk WAL file, incremental vacuum). A non-SQLite implementation is free to give them
// different mechanics as long as UsedBytes/Preserve keep meaning "logical bytes used"/"compact",
// and WALBytes returns 0 where there is no comparable on-disk WAL to measure (as it already does
// for the in-memory database used by tests).
// Every method that performs database work takes a context.Context as its first parameter, so an
// RPC's own deadline or cancellation reaches the driver and aborts server-side work (bounded
// further by storage.queryTimeout inside the db layer). WALBytes (a filesystem stat) and Close (a
// lifecycle call) take none because neither issues a query.
type Store interface {
	CreateMemory(ctx context.Context, memory types.Memory) (string, error)
	UpdateMemory(ctx context.Context, memory types.Memory) (bool, error)
	DeleteMemories(ctx context.Context, ids []string) (int, error)
	RecallMemories(ctx context.Context, ids []string) (*[]types.Memory, error)
	ReplaceMemoriesWithSummary(ctx context.Context, eventId string, summary types.Memory) (int, error)
	GetMemories(ctx context.Context, filter MemoryFilter) (*[]types.Memory, error)
	GetMemoriesByEventId(ctx context.Context, eventId string) (*[]types.Memory, error)
	GetMemoriesByEventIds(ctx context.Context, eventIds []string) (*[]types.Memory, error)
	CountMemoriesByEventIds(ctx context.Context, eventIds []string, groups []string) (map[string]int, error)
	GetMemoriesByIds(ctx context.Context, ids []string) (*[]types.Memory, error)
	CountMemories(ctx context.Context) (int, int)
	CountMemoriesFiltered(ctx context.Context, filter MemoryFilter) (int, error)

	CreateEvent(ctx context.Context, event types.Event) (string, error)
	UpdateEvent(ctx context.Context, event types.Event) (bool, error)
	DeleteEvent(ctx context.Context, id string) (bool, error)
	EventExists(ctx context.Context, id string) (bool, error)
	GetEvent(ctx context.Context, id string) (*types.Event, error)
	GetEvents(ctx context.Context, filter EventFilter) (*[]types.Event, error)
	CountEvents(ctx context.Context) int
	CountEventsFiltered(ctx context.Context, filter EventFilter) (int, error)
	MergeEventMemories(ctx context.Context, toEventId string, fromEventId string) error
	DeleteEventMemories(ctx context.Context, eventId string) (int, error)
	UnsetMemoriesEventId(ctx context.Context, eventId string) (int, error)
	CalculateSignificancePercentile(ctx context.Context, percent float64) (float64, error)

	// ResolveSignificanceLevel maps a requested significance (an absolute value or a relative
	// placement) to a registry level id, creating levels and opening gaps as needed. The RPC layer
	// calls it before a create/update so the store receives a resolved SignificanceLevelID.
	ResolveSignificanceLevel(ctx context.Context, spec SignificanceSpec) (sql.NullInt64, int32, error)

	// CompactSignificanceLevels renumbers the significance registry to consecutive ranks when it has
	// inflated toward the int32 ceiling; a best-effort maintenance step run from the sleep cycle.
	CompactSignificanceLevels(ctx context.Context) error

	ConsolidateMemories(ctx context.Context, s Server) (int, error)
	ConsolidateEventMemories(ctx context.Context, s Server) (int, int, int, error)
	ConsolidateEvents(ctx context.Context, s Server) (int, error)
	EvictMemories(ctx context.Context, s Server, freeBytes int64) (int, int, int64, error)

	// PreviewConsolidation reports what the four passes above would delete, and deletes nothing.
	PreviewConsolidation(ctx context.Context, s Server, opts PreviewOptions) (ConsolidationPreview, error)

	// RetainedStats counts the memories inside the minimum retention window, and their stored size.
	RetainedStats(ctx context.Context, cutoff int64) (int, int64, error)

	// The forgotten log (see tombstone.go). Writing is not on this interface: it happens inside the
	// delete chokepoint the consolidation and eviction passes already funnel through, so nothing
	// above the package can record a tombstone for a memory that did not actually go. PruneTombstones
	// applies the configured caps and is called at the end of each sleep cycle; DeleteForgottenMemories
	// is the manual cleanup, which is deliberately the only way to empty a log that is no longer
	// being written.
	GetForgottenMemories(ctx context.Context, filter ForgottenFilter) ([]ForgottenMemory, error)
	CountForgottenMemories(ctx context.Context, groups []string) (int64, error)
	DeleteForgottenMemories(ctx context.Context, before int64, groups []string) (int64, error)
	PruneTombstones(ctx context.Context) (int64, error)

	// GetMemoryConsolidationCandidates returns the consolidation decision inputs for named memories,
	// so ExplainConsolidation can value them without scanning the store.
	GetMemoryConsolidationCandidates(ctx context.Context, ids []string) ([]IdentifiedMemoryCandidate, error)

	FindSummarisationCandidates(ctx context.Context, minMemories int, maxTimestamp int64, limit int) ([]SummarisationCandidate, error)

	// The link graph (see link.go). Mutation is per item; the aggregate the consolidation scans read
	// is maintained by the store, never supplied by a caller. MissingMemoryIds/MissingEventIds back
	// the RPC layer's existence check, which is what keeps links from dangling - a dangling edge
	// would leave significance counted for one end forever.
	LinkMemories(ctx context.Context, id string, links []types.Link) error
	UnlinkMemories(ctx context.Context, id string, targets []string) error
	GetMemoryLinks(ctx context.Context, id string, direction types.LinkDirection) ([]types.LinkEdge, int64, error)
	LinkEvents(ctx context.Context, id string, links []types.Link) error
	UnlinkEvents(ctx context.Context, id string, targets []string) error
	GetEventLinks(ctx context.Context, id string, direction types.LinkDirection) ([]types.LinkEdge, int64, error)
	MissingMemoryIds(ctx context.Context, ids []string) ([]string, error)
	MissingEventIds(ctx context.Context, ids []string) ([]string, error)

	// The group-scope counterpart to the two above, for the RPCs that address rows by id and so
	// have no filter to carry the caller's scope (see scope.go).
	MemoryIdsOutsideGroups(ctx context.Context, ids []string, groups []string) ([]string, error)
	EventIdsOutsideGroups(ctx context.Context, ids []string, groups []string) ([]string, error)
	LinksForMemories(ctx context.Context, ids []string) (map[string][]types.Link, error)
	LinksForEvents(ctx context.Context, ids []string) (map[string][]types.Link, error)

	// LinkedMemoryIds returns the memories one hop from those named, backing associative retrieval
	// and spreading activation. ReinforceLinkedMemories is the spreading activation itself: it
	// advances a decay clock and deliberately leaves recall_count alone.
	LinkedMemoryIds(ctx context.Context, ids []string) ([]string, error)
	ReinforceLinkedMemories(ctx context.Context, ids []string, fraction float64) error

	// ImportMemoryLinks/ImportEventLinks are the import's second pass, applied once every row in the
	// batch exists so a link's target may legitimately appear after the item declaring it. They
	// report how many links were written and how many were dropped for a target that is in neither
	// the batch nor the store.
	ImportMemoryLinks(ctx context.Context, links map[string][]types.Link) (int, int, error)
	ImportEventLinks(ctx context.Context, links map[string][]types.Link) (int, int, error)

	// Export/transfer surface (see transfer.go): keyset pagination over the whole store,
	// full-state import upserts, and the manifest-scoped clear primitives.
	// The trailing groups is the caller's group scope (nil for the server-owned walks). It is on
	// these two rather than only on the filters because Export/Transfer/Clear reach the store
	// through them and must not carry a scoped caller past its own partition.
	GetMemoriesPage(ctx context.Context, afterId string, limit int, groups []string) ([]types.Memory, error)
	GetEventsPage(ctx context.Context, afterId string, limit int, groups []string) ([]types.Event, error)
	ImportMemories(ctx context.Context, memories []types.Memory) (int, error)
	ImportEvents(ctx context.Context, events []types.Event) (int, error)
	ClearMemories(ctx context.Context, snapshots []MemoryRecallSnapshot) (int, error)
	DeleteEventIfEmpty(ctx context.Context, id string) (bool, error)

	UsedBytes(ctx context.Context) (int64, error)
	WALBytes() (int64, error)
	Preserve(ctx context.Context) error
	Purge(ctx context.Context) error
	Ping(ctx context.Context) error
	Close() error
}

// Compile-time check that *DB satisfies Store.
var _ Store = (*DB)(nil)

// New opens (creating if necessary) the SQLite database in the given directory. An empty
// directory selects an in-memory database, used by tests. The database runs in WAL mode, so
// every acknowledged write is durable; there is no snapshot cycle.
//
// A file-backed open also takes the inter-process storage lock (see lock.go) and fails if another
// process already holds it, so a second consolidating instance pointed at the same directory stops
// at startup rather than running a second decay/eviction schedule against one store. The in-memory
// database (an empty directory) has no file to guard and takes no lock.
func New(directory string) (*DB, error) {
	log.Trace("func() NewDB")

	dsn := "file::memory:"

	var (
		walFilePath string
		lockFile    *instanceLockFile
	)

	if directory != "" {
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			log.Tracef("creating database directory '%s'", directory)

			if err := os.MkdirAll(directory, 0740); err != nil {
				log.Errorf("failed to create database data directory '%s': %s", directory, err)

				return nil, err
			}
		}

		// Not logged here: the refusal is already a complete, self-describing message, and main
		// logs it fatally - matching NewPostgres/NewMySQL, which return their lock refusal
		// unlogged for the same reason.
		lock, err := acquireStorageLock(directory)
		if err != nil {
			return nil, err
		}

		lockFile = lock

		dataFilePath := path.Join(directory, DataFile)
		dsn = "file:" + dataFilePath
		walFilePath = dataFilePath + "-wal"
	}

	dsn += "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Errorf("failed to open database: %s", err.Error())

		if lockFile != nil {
			lockFile.release()
		}

		return nil, err
	}

	// A single connection keeps the in-memory database alive for its whole lifetime and, for the
	// file-backed database, sidesteps writer contention within this process (inter-process
	// exclusion is the storage lock's job, not the pool's).
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	d := &DB{sql: sqlDB, walFilePath: walFilePath, lockFile: lockFile}

	if err := d.initSchema(); err != nil {
		_ = sqlDB.Close()

		if lockFile != nil {
			lockFile.release()
		}

		return nil, err
	}

	return d, nil
}

// NewSQLiteReadOnly opens the SQLite database in the given directory read-only, for tooling (the
// --backfill-search CLI mode) that only reads and so may run alongside a live service instance
// without contending for writes. Unlike New it opens with mode=ro (writes are rejected by SQLite),
// runs no initSchema (no DDL or VACUUM), and skips Preserve on Close (no WAL checkpoint or
// incremental vacuum) - all three of which would otherwise write to a database the live service
// owns. Mirrors NewPostgresReadOnly/NewMySQLReadOnly. The database must already exist: a read-only
// open cannot create it, and there would be nothing to index from anyway.
func NewSQLiteReadOnly(directory string) (*DB, error) {
	log.Trace("func() NewSQLiteReadOnly")

	if directory == "" {
		return nil, fmt.Errorf("a storage directory is required for a read-only sqlite open")
	}

	// mode=ro rejects writes at the SQLite level; busy_timeout lets a read briefly wait out a live
	// writer's lock rather than failing immediately. No journal_mode pragma - it would try to write
	// page 1; a mode=ro connection reads a WAL database through the existing -wal/-shm files.
	dsn := "file:" + path.Join(directory, DataFile) + "?mode=ro&_pragma=busy_timeout(5000)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Errorf("failed to open database read-only: %s", err.Error())

		return nil, err
	}

	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// sql.Open is lazy; Ping forces the file open so a missing database fails here rather than on
	// the first query.
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		log.Errorf("failed to open database read-only: %s", err.Error())

		return nil, err
	}

	return &DB{sql: sqlDB, driver: driverSQLite, readOnly: true}, nil
}

func (d *DB) initSchema() error {
	log.Trace("func() db.initSchema")

	// auto_vacuum can only be changed while the database is completely empty, and the
	// journal-mode pragma in the DSN has already initialised page 1 by the time this runs, so
	// setting the pragma alone never takes effect. Setting it and then running VACUUM rebuilds
	// the file with the pending mode; without it every incremental_vacuum in Preserve is a
	// silent no-op and the file never shrinks.
	if _, err := d.sql.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		log.Errorf("failed to set auto_vacuum: %s", err.Error())

		return err
	}

	var autoVacuum int
	if err := d.sql.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		log.Errorf("failed to read auto_vacuum: %s", err.Error())

		return err
	}

	if autoVacuum != 2 {
		log.Info("rebuilding database to enable incremental auto_vacuum")

		if _, err := d.sql.Exec(`VACUUM`); err != nil {
			log.Errorf("failed to vacuum database to enable auto_vacuum: %s", err.Error())

			return err
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS events (
		id                        TEXT PRIMARY KEY,
		time_start                INTEGER NOT NULL DEFAULT 0,
		time_end                  INTEGER NOT NULL DEFAULT 0,
		significance_level_id     INTEGER,
		name                      TEXT NOT NULL DEFAULT '',
		description               TEXT NOT NULL DEFAULT '',
		memories_consolidated     INTEGER NOT NULL DEFAULT 0,
		link_significance         INTEGER NOT NULL DEFAULT 0,
		group_name                TEXT NOT NULL DEFAULT '',
		metadata                  TEXT
	);

	CREATE TABLE IF NOT EXISTS memories (
		id            TEXT PRIMARY KEY,
		timestamp     INTEGER NOT NULL DEFAULT 0,
		significance_level_id INTEGER,
		event_id      TEXT NOT NULL DEFAULT '',
		is_binary     INTEGER NOT NULL DEFAULT 0,
		time_recalled INTEGER NOT NULL DEFAULT 0,
		recall_count  INTEGER NOT NULL DEFAULT 0,
		is_summary    INTEGER NOT NULL DEFAULT 0,
		group_name    TEXT NOT NULL DEFAULT '',
		is_compressed INTEGER NOT NULL DEFAULT 0,
		link_significance INTEGER NOT NULL DEFAULT 0,
		body          BLOB NOT NULL DEFAULT x'',
		metadata      TEXT
	);
	`

	if _, err := d.sql.Exec(schema); err != nil {
		log.Errorf("failed to initialise database schema: %s", err.Error())

		return err
	}

	// The significance registry (see significance.go). One shared registry backs both tables; the
	// covering index is created after the significance_level_id columns are guaranteed to exist
	// (below), so a database migrated in place from the old per-item significance column gets the
	// index rebuilt on the new column.
	if _, err := d.sql.Exec(d.significanceLevelsDDL()); err != nil {
		log.Errorf("failed to initialise significance registry: %s", err.Error())

		return err
	}

	if err := d.addColumnIfMissing("memories", "is_summary", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// Bodies written before compression existed are all uncompressed, which is exactly what the
	// column's default says of them, so no backfill is needed beyond adding the column.
	if err := d.addColumnIfMissing("memories", "is_compressed", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// The column is named group_name rather than group because GROUP is a reserved word in every
	// dialect the service speaks.
	if err := d.addColumnIfMissing("memories", "group_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("events", "group_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("memories", "significance_level_id", "INTEGER"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("events", "significance_level_id", "INTEGER"); err != nil {
		return err
	}

	// The link graph's denormalised aggregate. It defaults to 0, which is exactly right for a
	// database that predates links: it has none, and initLinkTables creates the graph empty.
	if err := d.addColumnIfMissing("memories", "link_significance", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("events", "link_significance", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// Metadata (see types/metadata.go) is deliberately NULL-able with no default, unlike group_name
	// beside it. json_extract raises "malformed JSON" on an empty string but returns NULL for NULL,
	// so a column defaulting to '' would make the FIRST metadata-filtered query fail against every
	// row written before this migration ran. NULL is the value all three dialects' JSON accessors
	// agree means "no metadata here", so a row without it is uniformly excluded by a key predicate.
	if err := d.addColumnIfMissing("memories", "metadata", "TEXT"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("events", "metadata", "TEXT"); err != nil {
		return err
	}

	if err := d.initLinkTables(); err != nil {
		return err
	}

	if err := d.dropLegacyRelationshipColumns(); err != nil {
		return err
	}

	if err := d.migrateSignificanceToLevels(); err != nil {
		return err
	}

	if err := d.ensureCoveringIndex(); err != nil {
		return err
	}

	// The forgotten log (see tombstone.go). Created whether or not the policy enables it, so
	// turning it on needs no migration and turning it off leaves what was recorded readable.
	if err := d.initTombstones(); err != nil {
		return err
	}

	// Last, because it reads memory bodies to populate itself on a store that predates it, and so
	// wants every column and index above it already in place.
	if err := d.initContentSearch(); err != nil {
		return err
	}

	return nil
}

// checkReadOnlyTables verifies the events and memories tables are present without running any DDL,
// so a read-only tool open (NewPostgresReadOnly/NewMySQLReadOnly) fails fast against a database the
// service has never initialised - mirroring NewSQLiteReadOnly's fail-fast on a missing file -
// instead of running the schema initialiser's ALTER TABLE probes, which can take locks (Postgres'
// brief ACCESS EXCLUSIVE) or trigger a long rebuild (MySQL's collation MODIFY) against a live
// service the tool is meant to run beside. A trivial no-row SELECT errors on a
// missing table on both server dialects, and doubles as a connectivity check.
func (d *DB) checkReadOnlyTables() error {
	log.Trace("func() db.checkReadOnlyTables")

	for _, table := range []string{"events", "memories"} {
		rows, err := d.sql.Query(`SELECT 1 FROM ` + table + ` WHERE 1 = 0`)
		if err != nil {
			return fmt.Errorf("read-only open: table '%s' is not available (has the service initialised this database?): %w", table, err)
		}

		_ = rows.Close()
	}

	return nil
}

// addColumnIfMissing adds a column to an existing table when it is not already present, so a
// schema change introduced after the table was first created still applies to a database
// written by an older version of the service. CREATE TABLE IF NOT EXISTS alone would silently
// skip the change for any table that already exists. Used by the SQLite and MySQL schema
// initialisers; Postgres supports ADD COLUMN IF NOT EXISTS natively.
func (d *DB) addColumnIfMissing(table string, column string, definition string) error {
	log.Trace("func() db.addColumnIfMissing")

	probe := `SELECT name FROM pragma_table_info(?) WHERE name = ?`
	if d.driver == driverMySQL {
		probe = `SELECT column_name FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`
	}

	rows, err := d.sql.Query(probe, table, column)
	if err != nil {
		log.Errorf("failed to check for column '%s' on table '%s': %s", column, table, err.Error())

		return err
	}

	exists := rows.Next()

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		log.Errorf("failed to check for column '%s' on table '%s': %s", column, table, err.Error())

		return err
	}

	_ = rows.Close()

	if exists {
		return nil
	}

	log.Infof("adding column '%s' to table '%s'", column, table)

	if _, err := d.sql.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		log.Errorf("failed to add column '%s' to table '%s': %s", column, table, err.Error())

		return err
	}

	return nil
}

// UsedBytes returns the store's logical live size — the figure compared against the byte
// capacity target, so space already freed by consolidation but not yet reclaimed must not count
// against it. For SQLite that is the database's pages excluding the freelist (the size the file
// would have after a full compaction); for the server drivers it is estimated from the live rows
// themselves (see usedBytesLiveRows), since no file-size measure on either server ever shrinks
// after deletes.
func (d *DB) UsedBytes(ctx context.Context) (int64, error) {
	log.Trace("func() db.UsedBytes")

	if d.driver != driverSQLite {
		return d.usedBytesLiveRows(ctx)
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var pageCount, freelistCount, pageSize int64

	if err := d.queryRow(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		log.Errorf("failed to read page_count: %s", err.Error())

		return 0, err
	}

	if err := d.queryRow(ctx, `PRAGMA freelist_count`).Scan(&freelistCount); err != nil {
		log.Errorf("failed to read freelist_count: %s", err.Error())

		return 0, err
	}

	if err := d.queryRow(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		log.Errorf("failed to read page_size: %s", err.Error())

		return 0, err
	}

	// The forgotten log is excluded: page accounting measures the whole file, so counting it would
	// let the record of what was evicted drive the eviction of live memories. The server drivers
	// count live memory/event/link rows explicitly and so exclude it already. See tombstoneBytes.
	used := (pageCount-freelistCount)*pageSize - d.tombstoneBytes(ctx)

	return max(used, 0), nil
}

// WALBytes returns the current size in bytes of the on-disk WAL file, or 0 for the server
// drivers and the in-memory database used by tests (neither has a client-visible WAL file).
// Unlike UsedBytes this reads the filesystem directly rather than querying the database, so
// checking it does not contend with the single connection.
func (d *DB) WALBytes() (int64, error) {
	log.Trace("func() db.WALBytes")

	if d.walFilePath == "" {
		return 0, nil
	}

	info, err := os.Stat(d.walFilePath)
	if os.IsNotExist(err) {
		return 0, nil
	}

	if err != nil {
		log.Errorf("failed to stat WAL file '%s': %s", d.walFilePath, err.Error())

		return 0, err
	}

	return info.Size(), nil
}

// Preserve is called at the end of each sleep cycle. For SQLite, WAL mode makes every write
// durable as it happens, so this only compacts: the WAL is checkpointed and truncated, and pages
// freed by consolidation are returned to the filesystem. For the server drivers it is a no-op —
// Postgres's autovacuum and InnoDB's background purge already reclaim dead rows continuously,
// and imitating SQLite's app-driven compaction (VACUUM FULL, OPTIMIZE TABLE) would take an
// exclusive table lock for no benefit.
func (d *DB) Preserve(ctx context.Context) error {
	log.Trace("func() db.Preserve")

	if d.driver != driverSQLite || d.readOnly {
		return nil
	}

	if _, err := d.exec(ctx, `PRAGMA incremental_vacuum`); err != nil {
		log.Errorf("failed to run incremental vacuum: %s", err.Error())

		return err
	}

	if _, err := d.exec(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Errorf("failed to checkpoint WAL: %s", err.Error())

		return err
	}

	return nil
}

// Ping checks that the database is reachable and responsive, bounded by the caller's context. It
// backs the readiness probe (HTTP /readyz and the gRPC health status) so a dead or unreachable
// database is reported as not-ready rather than the instance looking healthy while every RPC
// fails. It is deliberately cheap - a driver-level round trip, not a query.
func (d *DB) Ping(ctx context.Context) error {
	return d.sql.PingContext(ctx)
}

// Close checkpoints and closes the database. For the server drivers it also releases the
// instance lock by closing the session that holds it.
func (d *DB) Close() error {
	log.Trace("func() db.Close")

	if err := d.Preserve(context.Background()); err != nil {
		log.Errorf("failed to preserve database before closing: %s", err.Error())
	}

	// Stop the instance-lock keepalive and wait for it to exit before releasing the lock
	// connection, so it never races Close over lockConn nor tries to reacquire during shutdown.
	if d.stopKeepalive != nil {
		close(d.stopKeepalive)
		<-d.keepaliveStopped
		d.stopKeepalive = nil
	}

	if d.lockConn != nil {
		if err := d.lockConn.Close(); err != nil {
			log.Errorf("failed to close instance lock connection: %s", err.Error())
		}

		// Cleared so a second Close (e.g. a test's deferred cleanup after an explicit close)
		// doesn't try to close the connection again.
		d.lockConn = nil
	}

	err := d.sql.Close()

	// Released last, after every connection is gone: the storage lock must outlive the Preserve
	// checkpoint above and the pool's own teardown, so no write of ours lands after another
	// process has been let in. Cleared for the same reason as lockConn.
	if d.lockFile != nil {
		d.lockFile.release()
		d.lockFile = nil
	}

	return err
}

// Purge deletes all events and memories in a single transaction, then compacts the database.
func (d *DB) Purge(ctx context.Context) error {
	log.Info("func() db.Purge()")

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		log.Errorf("failed to purge - beginning transaction: %s", err.Error())

		return err
	}
	defer cancel()

	// Links first: they reference both tables, and nothing survives to have an aggregate
	// recalculated, so the wholesale empty is all that is needed.
	if err := d.purgeLinks(tx); err != nil {
		_ = tx.Rollback()

		return err
	}

	if _, err := tx.Exec(`DELETE FROM memories`); err != nil {
		log.Errorf("failed to purge - deleting memories: %s", err.Error())
		_ = tx.Rollback()

		return err
	}

	if _, err := tx.Exec(`DELETE FROM events`); err != nil {
		log.Errorf("failed to purge - deleting events: %s", err.Error())
		_ = tx.Rollback()

		return err
	}

	// Purge deletes everything, so the significance registry goes too - otherwise orphan levels
	// would accumulate across purges (harmless but untidy, and non-deterministic for tests that
	// purge to a clean slate).
	if _, err := tx.Exec(`DELETE FROM significance_levels`); err != nil {
		log.Errorf("failed to purge - deleting significance levels: %s", err.Error())
		_ = tx.Rollback()

		return err
	}

	// The forgotten log goes with it. This is the one automatic emptying of the log, and it is not
	// an exception to "cleanup is manual": Purge is itself the explicit, operator-initiated request
	// to leave nothing behind, and a log of what a now-empty store used to hold is not a record
	// anybody asked to keep.
	if d.tombstoneTable {
		if _, err := tx.Exec(`DELETE FROM ` + tombstonesTable); err != nil {
			log.Errorf("failed to purge - deleting the forgotten log: %s", err.Error())
			_ = tx.Rollback()

			return err
		}
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("failed to purge - committing transaction: %s", err.Error())

		return err
	}

	if err := d.Preserve(ctx); err != nil {
		log.Errorf("failed to purge - compacting database: %s", err.Error())

		return err
	}

	return nil
}
