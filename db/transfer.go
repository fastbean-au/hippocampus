package db

import (
	"context"
	"database/sql"
	"encoding/json"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/types"
)

// MemoryRecallSnapshot pairs a memory id with the recall state observed when an export or
// transfer captured it. It is the exported shape of the manifest entries Clear acts on:
// ClearMemories deletes a memory only while its recall state still matches, so a memory recalled
// (or re-created) after being captured survives to the next run instead of being deleted on
// stale data.
type MemoryRecallSnapshot struct {
	Id           string
	TimeRecalled int64
	RecallCount  int32
}

// GetMemoriesPage returns up to limit memories whose id sorts after afterId, in ascending id
// order — keyset pagination for export and transfer, so no long-running query is held across the
// whole table (the SQLite pool has a single connection). Unlike GetIndexableMemoriesPage this
// returns every memory, binary included: an archive must carry the whole store.
func (d *DB) GetMemoriesPage(ctx context.Context, afterId string, limit int) ([]types.Memory, error) {
	log.Trace("func() db.GetMemoriesPage")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT `+memoryColumns+` FROM `+memoriesFrom+` WHERE id > ? ORDER BY id LIMIT ?`,
		afterId,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var memories []types.Memory

	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}

		memories = append(memories, m)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return memories, nil
}

// GetEventsPage returns up to limit events whose id sorts after afterId, in ascending id order —
// the event half of the export/transfer pagination.
func (d *DB) GetEventsPage(ctx context.Context, afterId string, limit int) ([]types.Event, error) {
	log.Trace("func() db.GetEventsPage")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT `+eventColumns+` FROM `+eventsFrom+` WHERE id > ? ORDER BY id LIMIT ?`,
		afterId,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []types.Event

	for rows.Next() {
		event, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return events, nil
}

// ImportMemories upserts the given memories by id with every column taken from the input — a
// full-state data migration, unlike UpdateMemory's only-non-zero-values-overwrite rule — inside
// a single transaction. Re-importing the same rows is idempotent. Returns the number of rows
// written.
func (d *DB) ImportMemories(ctx context.Context, memories []types.Memory) (int, error) {
	log.Trace("func() db.ImportMemories")

	if len(memories) == 0 {
		return 0, nil
	}

	// The ELSE-less full overwrite still qualifies nothing but the excluded/new row, so both
	// dialect arms stay unambiguous. significance travels as the rank on the wire and is resolved
	// to a registry level id per row (find-or-create) below.
	query := `INSERT INTO memories (` + memoryStoredColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			timestamp     = excluded.timestamp,
			significance_level_id = excluded.significance_level_id,
			event_id      = excluded.event_id,
			body          = excluded.body,
			is_binary     = excluded.is_binary,
			time_recalled = excluded.time_recalled,
			recall_count  = excluded.recall_count,
			is_summary    = excluded.is_summary,
			group_name    = excluded.group_name`

	// MySQL has no ON CONFLICT; ON DUPLICATE KEY UPDATE with the 8.0.20+ row alias is the same
	// upsert, with 'new' standing in for 'excluded'.
	if d.driver == driverMySQL {
		query = `INSERT INTO memories (` + memoryStoredColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			timestamp     = new.timestamp,
			significance_level_id = new.significance_level_id,
			event_id      = new.event_id,
			body          = new.body,
			is_binary     = new.is_binary,
			time_recalled = new.time_recalled,
			recall_count  = new.recall_count,
			is_summary    = new.is_summary,
			group_name    = new.group_name`
	}

	// The registry lock serializes level find-or-create against concurrent writers on the server
	// drivers (a no-op on SQLite's single connection).
	releaseLock, err := d.acquireRegistryLock(ctx)
	if err != nil {
		return 0, err
	}
	defer releaseLock()

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	for _, memory := range memories {
		levelID, err := d.importLevelID(ctx, tx, memory.Significance)
		if err != nil {
			_ = tx.Rollback()

			return 0, err
		}

		if _, err := tx.Exec(
			d.rebind(query),
			memory.Id,
			memory.TimeStamp,
			levelID,
			memory.EventId,
			[]byte(memory.Body),
			memory.IsBinary,
			memory.TimeRecalled,
			memory.RecallCount,
			memory.IsSummary,
			memory.Group,
		); err != nil {
			_ = tx.Rollback()

			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return len(memories), nil
}

// importLevelID resolves a wire significance (rank) to a registry level id argument for import
// upserts, find-or-creating the level inside the caller's transaction; a non-positive rank is
// unranked (NULL).
func (d *DB) importLevelID(ctx context.Context, tx *sql.Tx, significance int32) (any, error) {
	if significance <= 0 {
		return nil, nil
	}

	id, err := d.findOrCreateLevelTx(ctx, tx, significance)
	if err != nil {
		return nil, err
	}

	return id, nil
}

// ImportEvents upserts the given events by id with every column taken from the input, inside a
// single transaction — the event half of ImportMemories. The relationship significance is
// recomputed from the relationships, matching CreateEvent. Returns the number of rows written.
func (d *DB) ImportEvents(ctx context.Context, events []types.Event) (int, error) {
	log.Trace("func() db.ImportEvents")

	if len(events) == 0 {
		return 0, nil
	}

	query := `INSERT INTO events (` + eventStoredColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			time_start                = excluded.time_start,
			time_end                  = excluded.time_end,
			significance_level_id     = excluded.significance_level_id,
			name                      = excluded.name,
			description               = excluded.description,
			memories_consolidated     = excluded.memories_consolidated,
			relationship_significance = excluded.relationship_significance,
			relationships             = excluded.relationships,
			group_name                = excluded.group_name`

	if d.driver == driverMySQL {
		query = `INSERT INTO events (` + eventStoredColumns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			time_start                = new.time_start,
			time_end                  = new.time_end,
			significance_level_id     = new.significance_level_id,
			name                      = new.name,
			description               = new.description,
			memories_consolidated     = new.memories_consolidated,
			relationship_significance = new.relationship_significance,
			relationships             = new.relationships,
			group_name                = new.group_name`
	}

	releaseLock, err := d.acquireRegistryLock(ctx)
	if err != nil {
		return 0, err
	}
	defer releaseLock()

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	for _, event := range events {
		event.RelationshipSignificance = event.CalculateRelationshipSignificance()

		relationships, err := json.Marshal(event.Relationships)
		if err != nil {
			_ = tx.Rollback()

			return 0, err
		}

		levelID, err := d.importLevelID(ctx, tx, event.Significance)
		if err != nil {
			_ = tx.Rollback()

			return 0, err
		}

		if _, err := tx.Exec(
			d.rebind(query),
			event.Id,
			event.TimeStart,
			event.TimeEnd,
			levelID,
			event.Name,
			event.Description,
			event.MemoriesConsolidated,
			event.RelationshipSignificance,
			string(relationships),
			event.Group,
		); err != nil {
			_ = tx.Rollback()

			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return len(events), nil
}

// ClearMemories deletes each captured memory only while its recall state still matches the
// snapshot the export/transfer took, funnelling through the same atomic check-and-delete the
// consolidation and eviction scans use — including its post-commit search-index delete
// propagation. Returns the number of rows actually deleted.
func (d *DB) ClearMemories(ctx context.Context, snapshots []MemoryRecallSnapshot) (int, error) {
	log.Trace("func() db.ClearMemories")

	items := make([]memoryRecallSnapshot, len(snapshots))
	for i, snapshot := range snapshots {
		items[i] = memoryRecallSnapshot{
			id:           snapshot.Id,
			timeRecalled: snapshot.TimeRecalled,
			recallCount:  snapshot.RecallCount,
		}
	}

	deletedIds, err := d.deleteMemoriesIfUnrecalled(ctx, items)

	return len(deletedIds), err
}
