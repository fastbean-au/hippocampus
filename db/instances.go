package db

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// The instance registry: one row per Hippocampus instance sharing this store, refreshed on a timer.
//
// It exists because the horizontally-scaled deployment - one consolidator plus N replicas over a
// shared database - is currently invisible from inside. An instance cannot name its own peers, and
// the single-consolidator lock proves only that SOMEBODY holds it, not who, and not how many
// replicas are attached. So a deployment can come up with every instance configured
// `consolidation.enabled: false`, in which case nothing forgets, nothing evicts, and the store
// simply grows - and nothing in the service says so today.
//
// Four decisions shape it.
//
//  1. The id is deterministic (hostname:port), not random per process. A restart then UPSERTS its
//     own row and leaves no ghost to age out - which in a rolling deployment would otherwise mean
//     most of the rows on the board are corpses. Two processes cannot share a host and a port, so
//     the id cannot collide.
//
//  2. It exists on the server drivers only. SQLite is single-instance by construction (the
//     hippocampus.lock flock enforces it), so the table would hold exactly one row restating what
//     the process already knows, and would add WAL churn to the embedded deployment to do it. It
//     would also be counted by SQLite's page-based UsedBytes, so the record of the deployment would
//     raise capacity pressure and evict live memories to make room for itself - the forgotten log's
//     lesson, and one worth not learning twice. On the server drivers usedBytesLiveRows counts
//     named tables, so this one is excluded by construction and needs no exclusion code.
//
//  3. Fixed columns, never a JSON blob. The metadata column's dialect divergence (json_extract
//     versus jsonb, and the NULL-versus-empty-string trap) is real work, and this table holds four
//     booleans and a role.
//
//  4. A row prunes itself. Each heartbeat deletes rows last seen more than staleMultiple of THEIR
//     OWN interval ago - per row, not against the writer's interval, so an instance heartbeating
//     slower than its peers is not repeatedly deleted and resurrected, which presents as a peer
//     flickering in and out of the view for no visible reason.
//
// Nothing here is a control plane: the registry is written by each instance about itself and read
// only to be displayed. No instance ever acts on another's row.

// instancesTable is the registry's table name.
const instancesTable = "instances"

// The roles an instance reports. They mirror consolidation.enabled and nothing else: exactly one
// instance in a deployment may run sleep cycles, and which one is the question this table exists to
// answer.
const (
	InstanceRoleConsolidator = "consolidator"
	InstanceRoleReplica      = "replica"
)

// instanceStaleMultiple is how many of an instance's own heartbeat intervals may pass before its
// row is pruned. Four rather than one or two so that a slow round, a brief partition, or a restart
// does not delete a healthy instance - and so that a genuinely dead one stays visible, and reported
// stale, for a window before it disappears. A peer that vanishes silently is indistinguishable from
// one that was never there.
const instanceStaleMultiple = 4

// Instance is one instance's registry row.
//
// The four capability flags are what make a replica running without the search index visible AS
// SUCH: two instances answering the same RPCs with different features enabled is a real and
// otherwise entirely silent misconfiguration.
type Instance struct {
	// Id is hostname:port - deterministic, so a restart replaces its own row (see the file comment).
	Id string

	Hostname string
	Version  string

	// Role is InstanceRoleConsolidator or InstanceRoleReplica.
	Role string

	// StartedAt and LastSeen are UnixNano. StartedAt is written on every heartbeat rather than only
	// on the first, so it survives the row being pruned and recreated during a long partition.
	StartedAt int64
	LastSeen  int64

	// HeartbeatSeconds is the interval this instance writes on, stored so the pruning cutoff can be
	// computed per row (see the file comment).
	HeartbeatSeconds int

	Search     bool
	Summariser bool
	Embedder   bool
	Gateway    bool
}

// Stale reports whether this row has not been refreshed within twice its own interval, as at now
// (UnixNano). Twice rather than once absorbs one missed beat - a slow query, a scheduling delay -
// while still marking a genuinely absent instance well before instanceStaleMultiple prunes it.
//
// It lives here rather than in the caller because the interval it judges against is this row's, and
// a reader that assumed its own would be wrong about exactly the instance worth being right about.
func (i Instance) Stale(now int64) bool {
	seconds := i.HeartbeatSeconds
	if seconds <= 0 {
		return false
	}

	return now-i.LastSeen > 2*int64(seconds)*int64(time.Second)
}

// InstanceRegistryAvailable reports whether this store keeps the instance registry. False on SQLite,
// which is single-instance by construction and has no table (see the file comment), and false on the
// read-only opens, which run no schema initialisation and so must not query a table they cannot be
// sure exists - the same guard the forgotten log takes.
func (d *DB) InstanceRegistryAvailable() bool {
	return d.instanceTable
}

// initInstances creates the registry table, idempotently. Called only from the two server drivers'
// schema initialisers; SQLite never creates it.
func (d *DB) initInstances() error {
	log.Trace("func() db.initInstances")

	if _, err := d.sql.Exec(d.instancesDDL()); err != nil {
		log.Errorf("failed to create the instance registry table: %s", err.Error())

		return err
	}

	// last_seen carries both the pruning cutoff and the staleness the view renders, and is the only
	// column ever filtered on - the reads are a full listing of a table with as many rows as there
	// are instances.
	if err := d.ensureIndex(instancesTable, "idx_"+instancesTable+"_last_seen", `(last_seen)`); err != nil {
		return err
	}

	d.instanceTable = true

	return nil
}

// instancesDDL is the CREATE TABLE for the registry in the active dialect. There is no SQLite
// branch: the table is never created there, and offering one would invite it to be.
func (d *DB) instancesDDL() string {
	dialect := d.dialect()

	// id takes the dialect's id type, which on MySQL collates like the memory and event ids beside
	// it, so two hostnames differing only in case are two instances here as they would be anywhere
	// else in the schema.
	//
	// heartbeat_seconds is the 64-bit integer type rather than INTEGER, and that is not cosmetic: it
	// is multiplied by a nanosecond scale in the pruning predicate, and Postgres types that whole
	// expression - including the placeholder compared against it - from the column. As an INTEGER it
	// infers int4, and then both the product and the UnixNano bound overflow, which is a runtime
	// failure on every prune rather than a schema complaint at startup.
	return `CREATE TABLE IF NOT EXISTS ` + instancesTable + ` (
		id                ` + dialect.idType + ` PRIMARY KEY,
		hostname          ` + dialect.labelType + ` NOT NULL DEFAULT '',
		version           ` + dialect.labelType + ` NOT NULL DEFAULT '',
		role              ` + dialect.labelType + ` NOT NULL DEFAULT '',
		started_at        ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		last_seen         ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		heartbeat_seconds ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		has_search        ` + dialect.boolType + ` NOT NULL DEFAULT FALSE,
		has_summariser    ` + dialect.boolType + ` NOT NULL DEFAULT FALSE,
		has_embedder      ` + dialect.boolType + ` NOT NULL DEFAULT FALSE,
		has_gateway       ` + dialect.boolType + ` NOT NULL DEFAULT FALSE
	)`
}

// instanceUpsert is the upsert for a registry row. Every column but the id is refreshed, including
// started_at and the capability flags: an instance that restarts with a different configuration must
// not be described by the row its previous incarnation wrote.
func (d *DB) instanceUpsert() string {
	return d.upsert(upsertSpec{
		table: instancesTable,
		columns: `id, hostname, version, role, started_at, last_seen, heartbeat_seconds,
			has_search, has_summariser, has_embedder, has_gateway`,
		key: []string{"id"},
		update: []string{
			"hostname", "version", "role", "started_at", "last_seen", "heartbeat_seconds",
			"has_search", "has_summariser", "has_embedder", "has_gateway",
		},
	})
}

// Heartbeat records this instance's liveness and prunes rows whose instances have stopped writing
// theirs. A no-op where there is no registry (SQLite, and the read-only opens).
//
// The prune runs beside the write rather than on a schedule of its own because there is no other
// occasion for it: nothing else in the service reads or writes this table, and a registry pruned
// only by a process that has exited is not pruned at all.
//
// A failure is the caller's to treat as best-effort - see the heartbeat loop's comment. Nothing in
// the service depends on this table, and a store that cannot take a liveness write has larger
// problems that its own node will already be reporting.
func (d *DB) Heartbeat(ctx context.Context, instance Instance) error {
	log.Trace("func() db.Heartbeat")

	if !d.instanceTable {
		return nil
	}

	if _, err := d.exec(
		ctx,
		d.instanceUpsert(),
		instance.Id,
		instance.Hostname,
		instance.Version,
		instance.Role,
		instance.StartedAt,
		instance.LastSeen,
		instance.HeartbeatSeconds,
		instance.Search,
		instance.Summariser,
		instance.Embedder,
		instance.Gateway,
	); err != nil {
		log.Errorf("failed to write the instance heartbeat: %s", err.Error())

		return err
	}

	// Each row is judged against its own interval, so a peer heartbeating on a longer one is not
	// deleted by a faster peer and immediately recreated by itself. A row with no interval recorded
	// is left alone rather than pruned on a guess: nothing this service writes has one, so such a
	// row can only have come from somewhere this code should not be deleting on behalf of.
	if _, err := d.exec(
		ctx,
		`DELETE FROM `+instancesTable+`
		WHERE heartbeat_seconds > 0
		AND last_seen < ? - (heartbeat_seconds * ? * 1000000000)`,
		instance.LastSeen,
		instanceStaleMultiple,
	); err != nil {
		log.Errorf("failed to prune stale instance rows: %s", err.Error())

		return err
	}

	return nil
}

// ListInstances returns every instance heartbeating against this store, including the caller's own
// row, ordered by id so a view built from it does not reshuffle between polls. Empty where there is
// no registry.
//
// The caller's own row is included deliberately: the counts that matter - how many instances are
// attached, and how many of them consolidate - are counts of the whole deployment, and a list that
// silently omitted the instance being asked would make every one of them wrong by one.
func (d *DB) ListInstances(ctx context.Context) ([]Instance, error) {
	log.Trace("func() db.ListInstances")

	if !d.instanceTable {
		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT id, hostname, version, role, started_at, last_seen, heartbeat_seconds,
			has_search, has_summariser, has_embedder, has_gateway
		FROM `+instancesTable+`
		ORDER BY id`,
	)
	if err != nil {
		log.Errorf("failed to list instances: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	out := []Instance{}

	for rows.Next() {
		var instance Instance

		if err := rows.Scan(
			&instance.Id,
			&instance.Hostname,
			&instance.Version,
			&instance.Role,
			&instance.StartedAt,
			&instance.LastSeen,
			&instance.HeartbeatSeconds,
			&instance.Search,
			&instance.Summariser,
			&instance.Embedder,
			&instance.Gateway,
		); err != nil {
			log.Errorf("failed to scan an instance row: %s", err.Error())

			return nil, err
		}

		out = append(out, instance)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read the instance rows: %s", err.Error())

		return nil, err
	}

	return out, nil
}

// DeregisterInstance removes this instance's row. Called on a clean shutdown, so a deliberately
// stopped instance disappears from the view immediately rather than lingering as an unreachable
// peer for four intervals - the staleness window exists for instances that stopped WITHOUT saying
// so, which is the case nobody can report on their behalf.
//
// Best-effort by construction: it takes its own context because the caller's is generally already
// cancelled by the time shutdown reaches here.
func (d *DB) DeregisterInstance(ctx context.Context, id string) error {
	log.Trace("func() db.DeregisterInstance")

	if !d.instanceTable {
		return nil
	}

	if _, err := d.exec(ctx, `DELETE FROM `+instancesTable+` WHERE id = ?`, id); err != nil {
		log.Errorf("failed to remove the instance registry row: %s", err.Error())

		return err
	}

	return nil
}
