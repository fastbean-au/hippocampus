package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/types"
)

// Links are directed, significance-carrying associations - memory to memory, or event to event.
// One row is one edge; the pair is the primary key, so linking the same pair twice updates that
// edge's significance rather than adding a second one.
//
// Two things about this file carry the design.
//
// First, value is symmetric while storage is directed. Both ends of a link gain its significance in
// the decay maths, so every aggregate here sums `from_id = ? OR to_id = ?`, and the to_id index
// exists precisely so the second half of that is not a scan. Direction survives in the rows because
// an association genuinely has a direction a client may care to read back (LinkEdge.Direction) - it
// simply does not affect what a link is worth.
//
// Second, the aggregate on the item row (memories.link_significance / events.link_significance) is
// what the consolidation scans read, so those scans stay on the covering index and never join to
// this table. It is maintained by recomputing it for exactly the ids whose links changed, never by
// applying deltas: a recompute is one statement, is self-correcting if anything ever does go astray,
// and costs the same as a delta at the sizes MaxLinks bounds it to. Every mutation in this file ends
// in a recalculate for the ids it touched, and every path that deletes an item calls prune, which
// does the same for the surviving ends.
const (
	memoryLinksTable = "memory_links"
	eventLinksTable  = "event_links"
)

// linkGraph names one graph: the table holding its edges and the table holding the items those
// edges connect. Both graphs are otherwise identical, so every operation below is written once
// against this rather than twice against memories and events.
type linkGraph struct {
	links string
	items string
	kind  string
}

var (
	memoryGraph = linkGraph{links: memoryLinksTable, items: "memories", kind: "memory"}
	eventGraph  = linkGraph{links: eventLinksTable, items: "events", kind: "event"}
)

// linkTableDDL is the CREATE TABLE for a link graph in the active dialect. The composite primary
// key is what makes a re-link an update rather than a duplicate edge; the id columns mirror the
// item tables' own (VARCHAR(255) COLLATE utf8mb4_bin on MySQL, which cannot index unbounded TEXT
// and would otherwise compare ids case-insensitively).
func (d *DB) linkTableDDL(table string) string {
	switch d.driver {

	case driverPostgres:
		return `CREATE TABLE IF NOT EXISTS ` + table + ` (
			from_id      TEXT NOT NULL,
			to_id        TEXT NOT NULL,
			significance INTEGER NOT NULL DEFAULT 0,
			created      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (from_id, to_id)
		)`

	case driverMySQL:
		return `CREATE TABLE IF NOT EXISTS ` + table + ` (
			from_id      VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
			to_id        VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
			significance INTEGER NOT NULL DEFAULT 0,
			created      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (from_id, to_id)
		)`

	default:
		return `CREATE TABLE IF NOT EXISTS ` + table + ` (
			from_id      TEXT NOT NULL,
			to_id        TEXT NOT NULL,
			significance INTEGER NOT NULL DEFAULT 0,
			created      INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (from_id, to_id)
		)`

	}
}

// initLinkTables creates both link graphs and their reverse indexes. The primary key already
// serves lookups by from_id; the extra index is what makes the inbound half of every symmetric
// aggregate an index lookup rather than a table scan.
func (d *DB) initLinkTables() error {
	log.Trace("func() db.initLinkTables")

	for _, table := range []string{memoryLinksTable, eventLinksTable} {
		if _, err := d.sql.Exec(d.linkTableDDL(table)); err != nil {
			log.Errorf("failed to create link table '%s': %s", table, err.Error())

			return err
		}

		index := "idx_" + table + "_to"

		if d.driver == driverMySQL {
			if err := d.createMySQLIndexIfMissing(table, index, `CREATE INDEX `+index+` ON `+table+` (to_id)`); err != nil {
				return err
			}

			continue
		}

		if _, err := d.sql.Exec(`CREATE INDEX IF NOT EXISTS ` + index + ` ON ` + table + ` (to_id)`); err != nil {
			log.Errorf("failed to create link index '%s': %s", index, err.Error())

			return err
		}
	}

	return nil
}

// dropLegacyRelationshipColumns removes the JSON relationships column and its denormalised sum from
// events. Event relationships became event links - the same mechanism as memory links, in the same
// tables, valued the same way - and the two representations cannot both be authoritative. Nothing
// is migrated out of the old column: the change predates any deployment whose data was worth
// carrying across, and a half-migration that silently reinterpreted significances under the new
// damped maths would be worse than starting the graph empty.
//
// Guarded on the column still existing, so it is a no-op on a fresh database and on every startup
// after the first.
func (d *DB) dropLegacyRelationshipColumns() error {
	log.Trace("func() db.dropLegacyRelationshipColumns")

	for _, column := range []string{"relationships", "relationship_significance"} {
		exists, err := d.columnExists("events", column)
		if err != nil {
			return err
		}

		if !exists {
			continue
		}

		log.Infof("dropping legacy events.%s (replaced by the link graph)", column)

		if _, err := d.sql.Exec(`ALTER TABLE events DROP COLUMN ` + column); err != nil {
			log.Errorf("failed to drop legacy column events.%s: %s", column, err.Error())

			return err
		}
	}

	return nil
}

// recalculateLinkSignificance recomputes the denormalised aggregate for the named ids from the link
// rows themselves, in one statement. It is deliberately a recompute rather than a delta: it is the
// single point every mutation and every prune funnels through, so it cannot drift from the rows it
// summarises whatever order writes arrive in. A NULL sum (an item with no links left) coalesces to
// 0 rather than leaving the previous value behind.
//
// It runs inside the caller's transaction so the aggregate is never visible out of step with the
// edges it describes.
func (d *DB) recalculateLinkSignificance(tx *sql.Tx, graph linkGraph, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		// The chunk is bound three times: twice inside the correlated subquery is not possible
		// (it correlates on the outer row, not on the chunk), so only the outer IN list is bound.
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}

		update := `UPDATE ` + graph.items + ` SET link_significance = COALESCE((
				SELECT SUM(significance) FROM ` + graph.links + `
				WHERE from_id = ` + graph.items + `.id OR to_id = ` + graph.items + `.id
			), 0)
			WHERE id IN (` + placeholders(len(chunk)) + `)`

		if _, err := tx.Exec(d.rebind(update), args...); err != nil {
			log.Errorf("failed to recalculate %s link significance: %s", graph.kind, err.Error())

			return err
		}
	}

	return nil
}

// pruneLinks removes every link touching the named (about to be, or just, deleted) items and
// recalculates the aggregate for the items at the far end that survive. It is called from every
// path that deletes a memory or an event - consolidation, eviction, the delete RPCs, event
// deletion, summary replacement - because the aggregate the consolidation scans read must not go on
// counting a link to something that is gone.
//
// Order matters: the far ends are collected first (while the rows still exist), the rows are then
// deleted, and only then is the aggregate recomputed - so the recompute reads the state after the
// deletion rather than before it. Ids in the deletion set are excluded from the recompute; their
// own rows are about to disappear, and on the eviction path they may already have.
//
// Runs inside the caller's transaction, and collects each result set fully before issuing a write:
// the SQLite pool is capped at one connection, so a write issued while rows are open would deadlock.
func (d *DB) pruneLinks(tx *sql.Tx, graph linkGraph, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	// A store with no links at all is the common case, and consolidation deletes in chunks of 500 -
	// so without this guard a link-free store paid two statements and two full argument slices per
	// chunk to discover, 200 times over on a large cycle, that there was nothing to prune. One
	// indexed existence probe replaces all of it. When links do exist the probe is one extra query
	// against work that is already proportional to the deletion, which is a trade worth making in
	// the direction of the store that has none.
	empty, err := d.linkTableEmpty(tx, graph)
	if err != nil {
		return err
	}

	if empty {
		return nil
	}

	deleting := make(map[string]bool, len(ids))
	for _, id := range ids {
		deleting[id] = true
	}

	survivors := make(map[string]bool)

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		// Bound twice - once for from_id, once for to_id - because a link touching a doomed item
		// may point either way.
		args := make([]any, 0, len(chunk)*2)
		for range 2 {
			for _, id := range chunk {
				args = append(args, id)
			}
		}

		list := placeholders(len(chunk))
		where := `from_id IN (` + list + `) OR to_id IN (` + list + `)`

		rows, err := tx.Query(d.rebind(`SELECT from_id, to_id FROM `+graph.links+` WHERE `+where), args...)
		if err != nil {
			log.Errorf("failed to read %s links for prune: %s", graph.kind, err.Error())

			return err
		}

		matched := false

		for rows.Next() {
			var fromId, toId string

			matched = true

			if err := rows.Scan(&fromId, &toId); err != nil {
				_ = rows.Close()
				log.Errorf("failed to scan %s link for prune: %s", graph.kind, err.Error())

				return err
			}

			for _, id := range []string{fromId, toId} {
				if !deleting[id] {
					survivors[id] = true
				}
			}
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			log.Errorf("failed to read %s links for prune: %s", graph.kind, err.Error())

			return err
		}

		_ = rows.Close()

		// Nothing matched this chunk, so there is nothing for the delete to remove: the read has
		// already answered the question the delete would ask.
		if !matched {
			continue
		}

		if _, err := tx.Exec(d.rebind(`DELETE FROM `+graph.links+` WHERE `+where), args...); err != nil {
			log.Errorf("failed to delete %s links for prune: %s", graph.kind, err.Error())

			return err
		}
	}

	if len(survivors) == 0 {
		return nil
	}

	remaining := make([]string, 0, len(survivors))
	for id := range survivors {
		remaining = append(remaining, id)
	}

	return d.recalculateLinkSignificance(tx, graph, remaining)
}

// linkTableEmpty reports whether a graph holds no edges at all, as one indexed existence probe. It
// is the guard that keeps the link feature free for a store that does not use it.
func (d *DB) linkTableEmpty(tx *sql.Tx, graph linkGraph) (bool, error) {
	rows, err := tx.Query(`SELECT 1 FROM ` + graph.links + ` LIMIT 1`)
	if err != nil {
		log.Errorf("failed to probe %s links: %s", graph.kind, err.Error())

		return false, err
	}

	any := rows.Next()

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		log.Errorf("failed to probe %s links: %s", graph.kind, err.Error())

		return false, err
	}

	_ = rows.Close()

	return !any, nil
}

// pruneMemoryLinks and pruneEventLinks are pruneLinks bound to each graph, so call sites in the
// delete paths read as what they are rather than carrying a graph argument around.
func (d *DB) pruneMemoryLinks(tx *sql.Tx, ids []string) error {
	return d.pruneLinks(tx, memoryGraph, ids)
}

func (d *DB) pruneEventLinks(tx *sql.Tx, ids []string) error {
	return d.pruneLinks(tx, eventGraph, ids)
}

// linkUpsert is the INSERT ... ON CONFLICT for a link row in the active dialect. Re-linking a pair
// updates that edge's significance; created is left at its original value, so it keeps meaning
// "when this association was first made" across a re-weighting.
func (d *DB) linkUpsert(table string) string {
	if d.driver == driverMySQL {
		return `INSERT INTO ` + table + ` (from_id, to_id, significance, created) VALUES (?, ?, ?, ?) AS new
			ON DUPLICATE KEY UPDATE significance = new.significance`
	}

	return `INSERT INTO ` + table + ` (from_id, to_id, significance, created) VALUES (?, ?, ?, ?)
		ON CONFLICT (from_id, to_id) DO UPDATE SET significance = excluded.significance`
}

// createLinks upserts a set of links from one item and recalculates the aggregate for every id
// involved - the near end and each far end, all of which gain the significance. The caller has
// already validated the set and confirmed every id exists.
func (d *DB) createLinks(ctx context.Context, graph linkGraph, id string, links []types.Link) error {
	if len(links) == 0 {
		return nil
	}

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		log.Errorf("failed to create %s links - beginning transaction: %s", graph.kind, err.Error())

		return err
	}
	defer cancel()

	now := time.Now().UnixNano()
	touched := make([]string, 0, len(links)+1)
	touched = append(touched, id)

	for _, l := range links {
		if _, err := tx.Exec(d.rebind(d.linkUpsert(graph.links)), id, l.Id, l.Significance, now); err != nil {
			log.Errorf("failed to create %s link: %s", graph.kind, err.Error())
			_ = tx.Rollback()

			return err
		}

		touched = append(touched, l.Id)
	}

	if err := d.recalculateLinkSignificance(tx, graph, touched); err != nil {
		_ = tx.Rollback()

		return err
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("failed to create %s links - committing: %s", graph.kind, err.Error())

		return err
	}

	return nil
}

// deleteLinks removes the links between one item and each of the named targets, in either
// direction - unlinking is symmetric because value is, and a caller asking to unlink A from B does
// not mean "only if I was the one who declared it". Unknown targets simply match nothing.
func (d *DB) deleteLinks(ctx context.Context, graph linkGraph, id string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		log.Errorf("failed to delete %s links - beginning transaction: %s", graph.kind, err.Error())

		return err
	}
	defer cancel()

	for start := 0; start < len(targets); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(targets))
		chunk := targets[start:end]

		args := make([]any, 0, len(chunk)*2+2)
		args = append(args, id)

		for _, t := range chunk {
			args = append(args, t)
		}

		args = append(args, id)

		for _, t := range chunk {
			args = append(args, t)
		}

		list := placeholders(len(chunk))
		where := `(from_id = ? AND to_id IN (` + list + `)) OR (to_id = ? AND from_id IN (` + list + `))`

		if _, err := tx.Exec(d.rebind(`DELETE FROM `+graph.links+` WHERE `+where), args...); err != nil {
			log.Errorf("failed to delete %s links: %s", graph.kind, err.Error())
			_ = tx.Rollback()

			return err
		}
	}

	if err := d.recalculateLinkSignificance(tx, graph, append([]string{id}, targets...)); err != nil {
		_ = tx.Rollback()

		return err
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("failed to delete %s links - committing: %s", graph.kind, err.Error())

		return err
	}

	return nil
}

// getLinks returns one item's links in the requested direction, together with the item's total link
// significance across both directions - the figure the decay maths damps, which is why it is
// reported even when the caller narrowed the direction.
func (d *DB) getLinks(
	ctx context.Context,
	graph linkGraph,
	id string,
	direction types.LinkDirection,
) ([]types.LinkEdge, int64, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// Both halves are selected and unioned rather than filtered in Go: each half is an index
	// lookup (the primary key for outbound, idx_<table>_to for inbound), and the direction column
	// is a constant per half rather than something to work out per row.
	var query string
	var args []any

	outbound := `SELECT to_id, significance, created, 1 FROM ` + graph.links + ` WHERE from_id = ?`
	inbound := `SELECT from_id, significance, created, 0 FROM ` + graph.links + ` WHERE to_id = ?`

	switch direction {

	case types.LinkDirectionOutbound:
		query, args = outbound, []any{id}

	case types.LinkDirectionInbound:
		query, args = inbound, []any{id}

	default:
		query, args = outbound+` UNION ALL `+inbound, []any{id, id}

	}

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		log.Errorf("failed to read %s links: %s", graph.kind, err.Error())

		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var edges []types.LinkEdge
	var total int64

	for rows.Next() {
		var edge types.LinkEdge
		var isOutbound bool

		if err := rows.Scan(&edge.Id, &edge.Significance, &edge.Created, &isOutbound); err != nil {
			log.Errorf("failed to scan %s link: %s", graph.kind, err.Error())

			return nil, 0, err
		}

		edge.Direction = types.LinkDirectionInbound
		if isOutbound {
			edge.Direction = types.LinkDirectionOutbound
		}

		edges = append(edges, edge)
		total += int64(edge.Significance)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read %s links: %s", graph.kind, err.Error())

		return nil, 0, err
	}

	// A narrowed direction saw only part of the graph, so the total has to come from the stored
	// aggregate instead of the rows just read.
	if direction != types.LinkDirectionBoth {
		_ = rows.Close()

		if total, err = d.linkSignificanceOf(ctx, graph, id); err != nil {
			return nil, 0, err
		}
	}

	return edges, total, nil
}

// linkSignificanceOf reads one item's stored aggregate. A missing row reports 0 rather than an
// error; callers have already established the item exists.
func (d *DB) linkSignificanceOf(ctx context.Context, graph linkGraph, id string) (int64, error) {
	var total int64

	err := d.queryRow(ctx, `SELECT link_significance FROM `+graph.items+` WHERE id = ?`, id).Scan(&total)
	if err == sql.ErrNoRows {
		return 0, nil
	}

	if err != nil {
		log.Errorf("failed to read %s link significance: %s", graph.kind, err.Error())

		return 0, err
	}

	return total, nil
}

// LinkMemories adds or updates links from one memory to others.
func (d *DB) LinkMemories(ctx context.Context, id string, links []types.Link) error {
	log.Trace("func() db.LinkMemories")

	return d.createLinks(ctx, memoryGraph, id, links)
}

// UnlinkMemories removes the links between one memory and the named memories, in either direction.
func (d *DB) UnlinkMemories(ctx context.Context, id string, targets []string) error {
	log.Trace("func() db.UnlinkMemories")

	return d.deleteLinks(ctx, memoryGraph, id, targets)
}

// GetMemoryLinks returns a memory's links and its total link significance.
func (d *DB) GetMemoryLinks(ctx context.Context, id string, direction types.LinkDirection) ([]types.LinkEdge, int64, error) {
	log.Trace("func() db.GetMemoryLinks")

	return d.getLinks(ctx, memoryGraph, id, direction)
}

// LinkEvents adds or updates links from one event to others.
func (d *DB) LinkEvents(ctx context.Context, id string, links []types.Link) error {
	log.Trace("func() db.LinkEvents")

	return d.createLinks(ctx, eventGraph, id, links)
}

// UnlinkEvents removes the links between one event and the named events, in either direction.
func (d *DB) UnlinkEvents(ctx context.Context, id string, targets []string) error {
	log.Trace("func() db.UnlinkEvents")

	return d.deleteLinks(ctx, eventGraph, id, targets)
}

// GetEventLinks returns an event's links and its total link significance.
func (d *DB) GetEventLinks(ctx context.Context, id string, direction types.LinkDirection) ([]types.LinkEdge, int64, error) {
	log.Trace("func() db.GetEventLinks")

	return d.getLinks(ctx, eventGraph, id, direction)
}

// MissingIds returns which of the named ids do not exist in the graph's item table, so a link write
// can be rejected with NotFound naming what was wrong rather than silently creating an edge to
// nothing. Links must not dangle: the aggregate is maintained per item, and an edge to a
// non-existent id would be significance counted for one end forever.
func (d *DB) MissingIds(ctx context.Context, graph linkGraph, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	found := make(map[string]bool, len(ids))

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}

		rows, err := d.query(ctx, `SELECT id FROM `+graph.items+` WHERE id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			log.Errorf("failed to check %s ids: %s", graph.kind, err.Error())

			return nil, err
		}

		for rows.Next() {
			var id string

			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				log.Errorf("failed to scan %s id: %s", graph.kind, err.Error())

				return nil, err
			}

			found[id] = true
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			log.Errorf("failed to check %s ids: %s", graph.kind, err.Error())

			return nil, err
		}

		_ = rows.Close()
	}

	var missing []string

	for _, id := range ids {
		if !found[id] {
			missing = append(missing, id)
		}
	}

	return missing, nil
}

// MissingMemoryIds and MissingEventIds expose MissingIds for each graph, so the RPC layer does not
// need the unexported linkGraph value.
func (d *DB) MissingMemoryIds(ctx context.Context, ids []string) ([]string, error) {
	return d.MissingIds(ctx, memoryGraph, ids)
}

func (d *DB) MissingEventIds(ctx context.Context, ids []string) ([]string, error) {
	return d.MissingIds(ctx, eventGraph, ids)
}

// LinkedMemoryIds returns the ids one hop from the named memories in either direction, excluding
// the named memories themselves. It backs associative retrieval (RecallMemories/SearchMemories
// include_linked, the GetMemories linked_to filter) and spreading activation, all of which are
// one-hop by design: a second hop turns "what does this remind me of" into most of the store.
func (d *DB) LinkedMemoryIds(ctx context.Context, ids []string) ([]string, error) {
	log.Trace("func() db.LinkedMemoryIds")

	return d.linkedIds(ctx, memoryGraph, ids)
}

func (d *DB) linkedIds(ctx context.Context, graph linkGraph, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	seed := make(map[string]bool, len(ids))
	for _, id := range ids {
		seed[id] = true
	}

	neighbours := make(map[string]bool)
	var ordered []string

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk)*2)
		for range 2 {
			for _, id := range chunk {
				args = append(args, id)
			}
		}

		list := placeholders(len(chunk))
		query := `SELECT to_id FROM ` + graph.links + ` WHERE from_id IN (` + list + `)
			UNION ALL
			SELECT from_id FROM ` + graph.links + ` WHERE to_id IN (` + list + `)`

		rows, err := d.query(ctx, query, args...)
		if err != nil {
			log.Errorf("failed to read linked %s ids: %s", graph.kind, err.Error())

			return nil, err
		}

		for rows.Next() {
			var id string

			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				log.Errorf("failed to scan linked %s id: %s", graph.kind, err.Error())

				return nil, err
			}

			if seed[id] || neighbours[id] {
				continue
			}

			neighbours[id] = true
			ordered = append(ordered, id)
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			log.Errorf("failed to read linked %s ids: %s", graph.kind, err.Error())

			return nil, err
		}

		_ = rows.Close()
	}

	return ordered, nil
}

// LinksForMemories returns each named memory's OUTBOUND links, keyed by memory id, for populating
// Memory.Links on a read that asked for them and for the archive walk.
//
// Outbound only, deliberately. Link is defined as one directed edge from the item carrying it, so
// this is the honest reading of the field - and it is what makes an archive round-trip faithful.
// Returning both directions would put every edge in the archive twice, once under each end, and an
// import would then write two directed rows where one existed and double the aggregate. A caller
// wanting the full picture asks GetMemoryLinks, which takes a direction and defaults to both.
func (d *DB) LinksForMemories(ctx context.Context, ids []string) (map[string][]types.Link, error) {
	log.Trace("func() db.LinksForMemories")

	return d.linksFor(ctx, memoryGraph, ids)
}

// LinksForEvents is LinksForMemories for events, and outbound-only for the same reason.
func (d *DB) LinksForEvents(ctx context.Context, ids []string) (map[string][]types.Link, error) {
	log.Trace("func() db.LinksForEvents")

	return d.linksFor(ctx, eventGraph, ids)
}

func (d *DB) linksFor(ctx context.Context, graph linkGraph, ids []string) (map[string][]types.Link, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	links := make(map[string][]types.Link, len(ids))

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}

		list := placeholders(len(chunk))
		query := `SELECT from_id, to_id, significance FROM ` + graph.links + ` WHERE from_id IN (` + list + `)`

		rows, err := d.query(ctx, query, args...)
		if err != nil {
			log.Errorf("failed to read %s links: %s", graph.kind, err.Error())

			return nil, err
		}

		for rows.Next() {
			var owner string
			var link types.Link

			if err := rows.Scan(&owner, &link.Id, &link.Significance); err != nil {
				_ = rows.Close()
				log.Errorf("failed to scan %s link: %s", graph.kind, err.Error())

				return nil, err
			}

			links[owner] = append(links[owner], link)
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()
			log.Errorf("failed to read %s links: %s", graph.kind, err.Error())

			return nil, err
		}

		_ = rows.Close()
	}

	return links, nil
}

// ReinforceLinkedMemories is spreading activation: recalling a memory partially reinforces what it
// is associated with. It advances each named memory's decay clock a fraction of the way to now -
//
//	time_recalled = time_recalled + fraction * (now - time_recalled)
//
// - so a neighbour of something recalled often keeps being pulled back from the threshold without
// ever being treated as recalled itself.
//
// recall_count is deliberately untouched. It is the record of direct recalls, read by the value
// calculation's recall term, by search ranking, and by the recalled counter; inflating it here
// would quietly change all three, and would make an association indistinguishable from an actual
// retrieval. The decay clock is the whole of the effect.
//
// A memory whose time_recalled is already at or beyond the computed point is left alone, so this
// can never move a clock backwards - a directly recalled memory that is also someone's neighbour
// keeps its full reinforcement.
func (d *DB) ReinforceLinkedMemories(ctx context.Context, ids []string, fraction float64) error {
	log.Trace("func() db.ReinforceLinkedMemories")

	if len(ids) == 0 || fraction <= 0 {
		return nil
	}

	if fraction > 1 {
		fraction = 1
	}

	now := time.Now().UnixNano()

	// A memory that has never been recalled decays from its creation timestamp, so that is the
	// point the fraction moves from - otherwise a never-recalled memory would jump from the epoch
	// and land almost at now, reinforcing it far more than a frequently recalled one.
	greatest := `MAX(timestamp, time_recalled)`
	if d.driver != driverSQLite {
		greatest = `GREATEST(timestamp, time_recalled)`
	}

	advanced := greatest + ` + CAST((? - ` + greatest + `) * ? AS ` + d.bigintType() + `)`

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk)+3)
		args = append(args, now, fraction, now)

		for _, id := range chunk {
			args = append(args, id)
		}

		update := `UPDATE ` + memoryGraph.items + ` SET time_recalled = ` + advanced + `
			WHERE ` + greatest + ` < ? AND id IN (` + placeholders(len(chunk)) + `)`

		if _, err := d.exec(ctx, update, args...); err != nil {
			log.Errorf("failed to reinforce linked memories: %s", err.Error())

			return err
		}
	}

	return nil
}

// bigintType names the 64-bit integer type a CAST must target in the active dialect. MySQL's CAST
// spells it SIGNED and rejects BIGINT.
func (d *DB) bigintType() string {
	if d.driver == driverMySQL {
		return "SIGNED"
	}

	return "BIGINT"
}

// ImportMemoryLinks and ImportEventLinks write the link half of an import, after every memory and
// event in the batch has been written. They are a second pass for a reason worth keeping: an
// archive is a set of rows in no particular order, so a link's target routinely appears later in
// the stream than the memory declaring it. Applying links as each row lands would drop every
// forward reference; applying them once the rows are all in place drops none.
//
// A link whose far end is in neither the batch nor the store is dropped rather than failing the
// import: the alternative is refusing a partial archive outright, and the far end being absent is
// exactly what a partial archive looks like. The count of dropped links is returned so the caller
// can log it.
func (d *DB) ImportMemoryLinks(ctx context.Context, links map[string][]types.Link) (int, int, error) {
	log.Trace("func() db.ImportMemoryLinks")

	return d.importLinks(ctx, memoryGraph, links)
}

func (d *DB) ImportEventLinks(ctx context.Context, links map[string][]types.Link) (int, int, error) {
	log.Trace("func() db.ImportEventLinks")

	return d.importLinks(ctx, eventGraph, links)
}

func (d *DB) importLinks(ctx context.Context, graph linkGraph, links map[string][]types.Link) (int, int, error) {
	if len(links) == 0 {
		return 0, 0, nil
	}

	// Every id the batch mentions, at either end, checked in one pass rather than per link.
	involved := make(map[string]bool, len(links))
	for owner, set := range links {
		involved[owner] = true

		for _, l := range set {
			involved[l.Id] = true
		}
	}

	ids := make([]string, 0, len(involved))
	for id := range involved {
		ids = append(ids, id)
	}

	missingIds, err := d.MissingIds(ctx, graph, ids)
	if err != nil {
		return 0, 0, err
	}

	missing := make(map[string]bool, len(missingIds))
	for _, id := range missingIds {
		missing[id] = true
	}

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		log.Errorf("failed to import %s links - beginning transaction: %s", graph.kind, err.Error())

		return 0, 0, err
	}
	defer cancel()

	now := time.Now().UnixNano()
	touched := make(map[string]bool)
	written := 0
	dropped := 0

	for owner, set := range links {
		if missing[owner] {
			dropped += len(set)

			continue
		}

		for _, l := range set {
			// A self-link is dropped rather than rejected for the same reason a missing far end is:
			// an import replays what an archive holds, and refusing the whole batch over one edge
			// that validation would never have accepted is the wrong trade.
			if missing[l.Id] || l.Id == owner {
				dropped++

				continue
			}

			if _, err := tx.Exec(d.rebind(d.linkUpsert(graph.links)), owner, l.Id, l.Significance, now); err != nil {
				log.Errorf("failed to import %s link: %s", graph.kind, err.Error())
				_ = tx.Rollback()

				return 0, 0, err
			}

			touched[owner] = true
			touched[l.Id] = true
			written++
		}
	}

	if len(touched) > 0 {
		recalculate := make([]string, 0, len(touched))
		for id := range touched {
			recalculate = append(recalculate, id)
		}

		if err := d.recalculateLinkSignificance(tx, graph, recalculate); err != nil {
			_ = tx.Rollback()

			return 0, 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("failed to import %s links - committing: %s", graph.kind, err.Error())

		return 0, 0, err
	}

	return written, dropped, nil
}

// purgeLinks empties both graphs wholesale, for Purge. Nothing survives a purge, so there is no
// aggregate left to recalculate.
func (d *DB) purgeLinks(tx *sql.Tx) error {
	for _, table := range []string{memoryLinksTable, eventLinksTable} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			log.Errorf("failed to purge %s: %s", table, err.Error())

			return fmt.Errorf("purging %s: %w", table, err)
		}
	}

	return nil
}
