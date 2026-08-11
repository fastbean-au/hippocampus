package db

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/types"
)

// deleteChunkSize caps the number of parameters in a single IN (...) clause, keeping batches
// well inside SQLite's bound-parameter limit.
const deleteChunkSize = 500

// evictionRowOverheadBytes is the allowance added to a memory's body length when estimating the
// bytes its deletion will free, covering the remaining columns and the index entries.
const evictionRowOverheadBytes = 256

// memoryColumns is the read projection. significance is the level's rank, exposed by memoriesFrom's
// join to the registry; scanMemory reads it into types.Memory.Significance. Use it with memoriesFrom
// as the FROM source, never the bare memories table (which has no significance column).
// metadata sits ahead of link_significance so this list and memoryReturningColumns (which appends
// link_significance to memoryStoredColumns) share one tail order, and scanMemory/scanMemoryStored
// therefore read their last three columns identically.
const memoryColumns = `id, timestamp, significance, event_id, body, is_binary, time_recalled, recall_count, is_summary, group_name, is_compressed, metadata, link_significance`

// memoryStoredColumns is the physical column list of the memories table (significance_level_id, not
// the removed significance): used for INSERT. link_significance is deliberately absent - it is
// maintained by the link graph rather than supplied by a write, so it must not appear in an insert's
// column list.
const memoryStoredColumns = `id, timestamp, significance_level_id, event_id, body, is_binary, time_recalled, recall_count, is_summary, group_name, is_compressed, metadata`

// memoryReturningColumns is memoryStoredColumns plus the link aggregate, for UPDATE ... RETURNING,
// which reads rather than writes and so wants every column a caller sees. scanMemoryStored reads it.
const memoryReturningColumns = memoryStoredColumns + `, link_significance`

// memoryValuePlaceholders is the VALUES list matching memoryStoredColumns, so an INSERT's
// placeholder count cannot drift from the column list as columns are added.
var memoryValuePlaceholders = `(` + placeholders(strings.Count(memoryStoredColumns, ",")+1) + `)`

// memoriesFrom is the read source for memoryColumns: the memories table LEFT JOINed to the
// significance registry and aliased back to "memories", so WHERE/ORDER clauses naming bare columns
// (id, event_id, significance, ...) need no change. An unranked (NULL) level reads as significance 0.
const memoriesFrom = `(SELECT m.id, m.timestamp, COALESCE(l.level_rank, 0) AS significance, m.event_id,
	m.body, m.is_binary, m.time_recalled, m.recall_count, m.is_summary, m.group_name, m.is_compressed,
	m.link_significance, m.metadata
	FROM memories m LEFT JOIN significance_levels l ON l.id = m.significance_level_id) AS memories`

// placeholders returns a comma-separated list of n SQL parameter placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func scanMemory(rows *sql.Rows) (types.Memory, error) {
	var m types.Memory
	var body []byte
	var isCompressed bool
	var metadata any

	if err := rows.Scan(
		&m.Id,
		&m.TimeStamp,
		&m.Significance,
		&m.EventId,
		&body,
		&m.IsBinary,
		&m.TimeRecalled,
		&m.RecallCount,
		&m.IsSummary,
		&m.Group,
		&isCompressed,
		&metadata,
		&m.LinkSignificance,
	); err != nil {
		return m, err
	}

	// Scanned into an any because the column is NULL-able and comes back as []byte on SQLite and
	// MySQL but either []byte or string on Postgres; UnmarshalMetadata resolves all of those, and a
	// row with no metadata reads as nil rather than an empty map.
	decodedMetadata, err := types.UnmarshalMetadata(metadata)
	if err != nil {
		log.Errorf("failed to decode metadata of memory '%s': %s", m.Id, err.Error())

		return m, err
	}

	m.Metadata = decodedMetadata

	// Decompression is driven by the row's own flag, so a store holding a mix of compressed and
	// uncompressed rows - which any store whose compression setting has ever changed will - reads
	// back uniformly.
	decompressed, err := decompressBody(body, isCompressed)
	if err != nil {
		log.Errorf("failed to decompress body of memory '%s': %s", m.Id, err.Error())

		return m, err
	}

	m.Body = decompressed

	return m, nil
}

// scanMemoryStored reads memoryReturningColumns, carrying the raw significance_level_id into
// SignificanceLevelID. Significance (the rank) is left 0 for the caller to fill from a registry
// snapshot (fillRanks) - used where a join is unavailable, e.g. RETURNING.
func scanMemoryStored(rows *sql.Rows) (types.Memory, error) {
	var m types.Memory
	var body []byte
	var levelID sql.NullInt64
	var isCompressed bool
	var metadata any

	if err := rows.Scan(
		&m.Id,
		&m.TimeStamp,
		&levelID,
		&m.EventId,
		&body,
		&m.IsBinary,
		&m.TimeRecalled,
		&m.RecallCount,
		&m.IsSummary,
		&m.Group,
		&isCompressed,
		&metadata,
		&m.LinkSignificance,
	); err != nil {
		return m, err
	}

	decodedMetadata, err := types.UnmarshalMetadata(metadata)
	if err != nil {
		log.Errorf("failed to decode metadata of memory '%s': %s", m.Id, err.Error())

		return m, err
	}

	m.Metadata = decodedMetadata

	decompressed, err := decompressBody(body, isCompressed)
	if err != nil {
		log.Errorf("failed to decompress body of memory '%s': %s", m.Id, err.Error())

		return m, err
	}

	m.Body = decompressed

	if levelID.Valid {
		id := levelID.Int64
		m.SignificanceLevelID = &id
	}

	return m, nil
}

// levelIDArg turns a nullable level id into a SQL argument: a real id, or NULL when unranked.
func levelIDArg(id *int64) any {
	if id == nil {
		return nil
	}

	return *id
}

// fillRanks resolves each memory's SignificanceLevelID to its rank via a registry snapshot, setting
// Significance. Used by paths that read the physical row (RETURNING) rather than the joined view.
func (d *DB) fillRanks(ctx context.Context, memories []types.Memory) error {
	ranks, err := d.loadSignificanceRanks(ctx)
	if err != nil {
		return err
	}

	for i := range memories {
		var levelID sql.NullInt64

		if memories[i].SignificanceLevelID != nil {
			levelID = nullInt64(*memories[i].SignificanceLevelID)
		}

		memories[i].Significance = rankOf(ranks, levelID)
	}

	return nil
}

// CreateMemory creates a memory record, returning the id and an error. The significance level is
// resolved by the caller (ResolveSignificanceLevel) and carried on SignificanceLevelID; nil is
// unranked.
func (d *DB) CreateMemory(ctx context.Context, memory types.Memory) (string, error) {
	log.Trace("func() db.CreateMemory")

	levelID, err := d.ensureSignificanceLevel(ctx, memory.Significance, memory.SignificanceLevelID)
	if err != nil {
		return "", err
	}

	memory.SignificanceLevelID = levelID

	body, isCompressed := d.compressBody(memory.Body, memory.IsBinary)

	metadata, err := types.MarshalMetadata(memory.Metadata)
	if err != nil {
		log.Errorf("failed to encode metadata of memory '%s': %s", memory.Id, err.Error())

		return "", err
	}

	_, err = d.exec(ctx,
		`INSERT INTO memories (`+memoryStoredColumns+`) VALUES `+memoryValuePlaceholders,
		memory.Id,
		memory.TimeStamp,
		levelIDArg(memory.SignificanceLevelID),
		memory.EventId,
		body,
		memory.IsBinary,
		memory.TimeRecalled,
		memory.RecallCount,
		memory.IsSummary,
		memory.Group,
		isCompressed,
		metadata,
	)
	if err != nil {
		return memory.Id, err
	}

	// The index is fed the plain body, not the (possibly compressed) bytes just written. A failure
	// here does not fail the create: the memory is stored, and an unindexed memory is a
	// rebuildable gap, not a lost write.
	_ = d.indexMemoryContent(ctx, memory.Id, memory.Body, memory.IsBinary)

	return memory.Id, nil
}

// UpdateMemory applies a partial update to an existing memory: only fields carrying a non-zero
// value overwrite the stored row. It does not create the memory when the id is unknown - it returns
// (false, nil) so callers can surface NotFound rather than inserting a phantom row (the same
// treatment UpdateEvent received). Returns whether a matching memory existed.
//
// It deliberately does not touch is_binary or is_summary: those are set at creation and by
// ReplaceMemoriesWithSummary respectively, and are outside the partial-update surface (see #22).
func (d *DB) UpdateMemory(ctx context.Context, memory types.Memory) (bool, error) {
	log.Trace("func() db.UpdateMemory")

	// Build the SET list from only the fields carrying a value, mirroring db.UpdateEvent's
	// conditional-overwrite semantics without an upsert. Portable across all three dialects.
	var (
		sets []string
		args []any
	)

	if memory.TimeStamp > 0 {
		sets = append(sets, `timestamp = ?`)
		args = append(args, memory.TimeStamp)
	}

	// Significance is changed when the caller supplied a placement-resolved level id, or an absolute
	// value > 0 (resolved here). A nil level id with a non-positive value leaves significance
	// untouched, so a partial update of other fields does not silently unrank the memory.
	levelID := memory.SignificanceLevelID

	if levelID == nil && memory.Significance > 0 {
		resolved, err := d.ensureSignificanceLevel(ctx, memory.Significance, nil)
		if err != nil {
			return false, err
		}

		levelID = resolved
	}

	if levelID != nil {
		sets = append(sets, `significance_level_id = ?`)
		args = append(args, *levelID)
	}

	if memory.EventId != "" {
		sets = append(sets, `event_id = ?`)
		args = append(args, memory.EventId)
	}

	// A new body carries a new compression decision, so is_compressed is always written alongside
	// body - never left to describe the body it replaced. The decision needs to know whether the
	// memory is binary, and is_binary is outside this partial-update surface (the caller's copy of
	// it is not authoritative), so it is read from the stored row.
	// bodyChanged also drives the content-search reindex below, which needs the same is_binary the
	// compression decision needed.
	var (
		bodyChanged  bool
		bodyIsBinary bool
	)

	if len(memory.Body) > 0 {
		isBinary, err := d.memoryIsBinary(ctx, memory.Id)
		if err != nil {
			return false, err
		}

		body, isCompressed := d.compressBody(memory.Body, isBinary)

		sets = append(sets, `body = ?`, `is_compressed = ?`)
		args = append(args, body, isCompressed)

		bodyChanged = true
		bodyIsBinary = isBinary
	}

	// ClearGroup is checked ahead of the value, and wins: an empty group otherwise means "leave
	// unchanged" under this function's non-zero-means-change rule, so without an explicit
	// instruction there would be no way to unset a group once set.
	if memory.ClearGroup {
		sets = append(sets, `group_name = ?`)
		args = append(args, "")
	} else if memory.Group != "" {
		sets = append(sets, `group_name = ?`)
		args = append(args, memory.Group)
	}

	// Metadata replaces wholesale rather than merging per key, and ClearMetadata is its counterpart
	// to ClearGroup - an absent map and an explicitly empty one are the same on the wire, so the map
	// cannot say "remove everything" itself. Cleared to NULL rather than '{}' for the reason the
	// column is NULL-able at all: see the schema comment in initSchema.
	if memory.ClearMetadata {
		sets = append(sets, `metadata = ?`)
		args = append(args, nil)
	} else if len(memory.Metadata) > 0 {
		metadata, err := types.MarshalMetadata(memory.Metadata)
		if err != nil {
			log.Errorf("failed to encode metadata of memory '%s': %s", memory.Id, err.Error())

			return false, err
		}

		sets = append(sets, `metadata = ?`)
		args = append(args, metadata)
	}

	// Nothing to change: there is no UPDATE to learn existence from, so probe for it directly.
	if len(sets) == 0 {
		return d.memoryExists(ctx, memory.Id)
	}

	args = append(args, memory.Id)

	res, err := d.exec(ctx, `UPDATE memories SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return false, err
	}

	existed, err := d.updatedRowExisted(ctx, res, "memories", memory.Id)
	if err != nil {
		return existed, err
	}

	// Only once the row is known to exist: reindexing an id that matched nothing would delete
	// whatever a memory of that id used to have and put nothing back.
	if existed && bodyChanged {
		_ = d.reindexMemoryContent(ctx, memory.Id, memory.Body, bodyIsBinary)
	}

	return existed, nil
}

// memoryIsBinary reports whether the stored memory is binary, which decides whether a body being
// written over it may be compressed. A missing row reports false: the UPDATE that follows will
// match nothing, and UpdateMemory reports the absence from the UPDATE itself.
func (d *DB) memoryIsBinary(ctx context.Context, id string) (bool, error) {
	var isBinary sql.NullBool

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT is_binary FROM memories WHERE id = ?`, id).Scan(&isBinary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}

		return false, err
	}

	return isBinary.Bool, nil
}

// memoryExists reports whether a memory with the given id exists.
func (d *DB) memoryExists(ctx context.Context, id string) (bool, error) {
	var exists bool

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memories WHERE id = ?)`, id).Scan(&exists); err != nil {
		return false, err
	}

	return exists, nil
}

// updatedRowExisted reports whether the UPDATE that produced res matched an existing row, taking
// existence from the UPDATE itself rather than a separate probe - so a concurrent delete cannot land
// between an existence check and the UPDATE and make the caller report success for a row that no
// longer exists. RowsAffected counts matched rows on SQLite and Postgres, so it is
// authoritative there. MySQL instead counts changed rows and reports 0 when an UPDATE matches a row
// but leaves every column unchanged, so a 0 there is ambiguous and needs one existence probe to tell
// "missing" from "matched but unchanged". table is the (constant, not user-supplied) table name.
func (d *DB) updatedRowExisted(ctx context.Context, res sql.Result, table string, id string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	if n > 0 {
		return true, nil
	}

	if d.driver != driverMySQL {
		return false, nil
	}

	var exists bool

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id = ?)`, id).Scan(&exists); err != nil {
		return false, err
	}

	return exists, nil
}

// DeleteMemory deletes a single memory by id. See UpdateMemory's note: it has no production caller
// yet (only tests).
func (d *DB) DeleteMemory(ctx context.Context, id string) error {
	log.Trace("func() db.DeleteMemory")

	_, err := d.deleteMemoriesByIds(ctx, []string{id})

	return err
}

func (d *DB) DeleteMemories(ctx context.Context, ids []string) (int, error) {
	log.Trace("func() db.DeleteMemories")

	return d.deleteMemoriesByIds(ctx, ids)
}

// deleteMemoriesByIds deletes the given memories in chunked IN (...) batches inside a single
// transaction, returning the number of rows deleted.
func (d *DB) deleteMemoriesByIds(ctx context.Context, ids []string) (int, error) {
	cnt := 0

	if len(ids) == 0 {
		return 0, nil
	}

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := start + deleteChunkSize
		if end > len(ids) {
			end = len(ids)
		}

		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}

		res, err := tx.Exec(d.rebind(`DELETE FROM memories WHERE id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			_ = tx.Rollback()

			return 0, err
		}

		if n, err := res.RowsAffected(); err == nil {
			cnt += int(n)
		}
	}

	// Inside the same transaction, so the link rows and the aggregate the consolidation scans read
	// never disagree with the memories that are actually there.
	if err := d.pruneMemoryLinks(tx, ids); err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return cnt, nil
}

// memoryRecallSnapshot pairs a memory id with the recall state observed during a consolidation or
// eviction scan.
type memoryRecallSnapshot struct {
	id           string
	timeRecalled int64
	recallCount  int32
}

// deleteMemoriesIfUnrecalled deletes each memory only if its time_recalled/recall_count still
// match the scanned snapshot, inside a single transaction. Consolidation and eviction decide a
// memory should be deleted from a scan taken before the delete runs; a concurrent RecallMemories
// call in that gap reinforces the memory and should protect it. Checking the recall state as part
// of the delete closes that window, so a memory recalled mid-scan survives instead of being deleted
// on stale data. Returns the ids of the rows actually deleted (callers wanting a count take len);
// eviction also needs the exact ids so it only counts freed bytes for memories that really went,
// not ones the recall-race guard skipped.
//
// On the server drivers the snapshots are processed in deleteChunkSize batches, each a single
// guarded statement rather than one DELETE per row: there a large
// consolidation/eviction/clear was a network round trip per row, so batching cuts the round trips
// ~500x. SQLite keeps the per-row path: each guarded DELETE is a primary-key lookup that measured
// faster than a row-value IN batch, and its single local connection has no round trip to amortise.
//
// After the transaction commits, the memory-delete observer (when set) is invoked with the ids
// of the rows actually deleted, so the optional search index learns about deletions that never
// pass through the RPC layer. All three consolidation/eviction paths funnel through here, so
// this is the single propagation point for them.
func (d *DB) deleteMemoriesIfUnrecalled(ctx context.Context, items []memoryRecallSnapshot) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	deletedIds := make([]string, 0, len(items))

	for start := 0; start < len(items); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(items))

		ids, err := d.deleteChunkIfUnrecalled(tx, items[start:end])
		if err != nil {
			_ = tx.Rollback()

			return nil, err
		}

		deletedIds = append(deletedIds, ids...)
	}

	// Only the ids that actually went: a memory the recall-race guard spared still exists, and its
	// links must survive with it. This is the single point consolidation, eviction and Clear all
	// funnel through, so it is the only place those three paths need to prune.
	if err := d.pruneMemoryLinks(tx, deletedIds); err != nil {
		_ = tx.Rollback()

		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	if d.memoryDeleteObserver != nil && len(deletedIds) > 0 {
		d.memoryDeleteObserver(deletedIds)
	}

	return deletedIds, nil
}

// deleteChunkIfUnrecalled deletes one chunk of snapshots whose recall state still matches. The
// guard - matching (id, time_recalled, recall_count) against the snapshot - is what leaves a memory
// recalled since the scan in place, preserving the race-safety. It returns exactly the ids
// deleted, which callers need for the search-index observer and eviction's freed-bytes accounting.
//
// SQLite deletes row by row (fast primary-key lookups, no round trips to batch away). The server
// drivers batch the whole chunk into one guarded statement to cut the per-row network round trip:
// Postgres deletes and reports the affected ids in one DELETE ... RETURNING, while MySQL - which has
// no DELETE ... RETURNING - locks the matching rows with SELECT ... FOR UPDATE (closing the window
// against a recall landing between the select and the delete) and deletes them by id.
func (d *DB) deleteChunkIfUnrecalled(tx *sql.Tx, chunk []memoryRecallSnapshot) ([]string, error) {
	if d.driver == driverSQLite {
		return deleteChunkPerRow(tx, chunk)
	}

	tuples := make([]string, len(chunk))
	args := make([]any, 0, len(chunk)*3)

	for i, item := range chunk {
		tuples[i] = "(?, ?, ?)"
		args = append(args, item.id, item.timeRecalled, item.recallCount)
	}

	guard := `(id, time_recalled, recall_count) IN (` + strings.Join(tuples, ", ") + `)`

	if d.driver == driverMySQL {
		return d.deleteChunkMySQL(tx, guard, args)
	}

	rows, err := tx.Query(d.rebind(`DELETE FROM memories WHERE `+guard+` RETURNING id`), args...)
	if err != nil {
		return nil, err
	}

	return scanIds(rows)
}

// deleteChunkPerRow is the SQLite arm of deleteChunkIfUnrecalled: one guarded, primary-key-indexed
// DELETE per row (SQLite uses ? placeholders directly, so no rebind), returning the ids of the rows
// that actually matched their snapshot.
func deleteChunkPerRow(tx *sql.Tx, chunk []memoryRecallSnapshot) ([]string, error) {
	var deleted []string

	for _, item := range chunk {
		res, err := tx.Exec(
			`DELETE FROM memories WHERE id = ? AND time_recalled = ? AND recall_count = ?`,
			item.id,
			item.timeRecalled,
			item.recallCount,
		)
		if err != nil {
			return nil, err
		}

		if n, err := res.RowsAffected(); err == nil && n > 0 {
			deleted = append(deleted, item.id)
		}
	}

	return deleted, nil
}

// deleteChunkMySQL is deleteChunkIfUnrecalled's MySQL arm: SELECT ... FOR UPDATE locks exactly the
// rows still matching the snapshot (so a concurrent recall cannot slip in between the select and
// the delete), then they are deleted by id - MySQL having no DELETE ... RETURNING to do it in one.
func (d *DB) deleteChunkMySQL(tx *sql.Tx, guard string, args []any) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM memories WHERE `+guard+` FOR UPDATE`, args...)
	if err != nil {
		return nil, err
	}

	ids, err := scanIds(rows)
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return nil, nil
	}

	delArgs := make([]any, len(ids))
	for i, id := range ids {
		delArgs[i] = id
	}

	if _, err := tx.Exec(`DELETE FROM memories WHERE id IN (`+placeholders(len(ids))+`)`, delArgs...); err != nil {
		return nil, err
	}

	return ids, nil
}

// scanIds reads a single id column from rows into a slice, closing rows before returning (so the
// next statement on the same single-connection transaction can run).
func scanIds(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()

	var ids []string

	for rows.Next() {
		var id string

		if err := rows.Scan(&id); err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// DeleteEventMemories deletes every memory belonging to an event. It reads the ids first rather
// than deleting straight off event_id: the link graph is keyed on memory id, so pruning needs to
// know what went. The read and the delete share one transaction, so a memory attached to the event
// in between is not deleted with its links left behind.
func (d *DB) DeleteEventMemories(ctx context.Context, eventId string) (int, error) {
	log.Trace("func() db.DeleteEventMemories")

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	ids, err := d.memoryIdsForEvent(tx, eventId)
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	res, err := tx.Exec(d.rebind(`DELETE FROM memories WHERE event_id = ?`), eventId)
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if err := d.pruneMemoryLinks(tx, ids); err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	cnt, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return int(cnt), nil
}

// memoryIdsForEvent lists an event's memory ids inside a transaction, for the delete paths that
// need to prune links but only know the event. Rows are drained and closed before the caller
// issues its own write - the SQLite pool holds one connection.
func (d *DB) memoryIdsForEvent(tx *sql.Tx, eventId string) ([]string, error) {
	rows, err := tx.Query(d.rebind(`SELECT id FROM memories WHERE event_id = ?`), eventId)
	if err != nil {
		return nil, err
	}

	var ids []string

	for rows.Next() {
		var id string

		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()

			return nil, err
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()

		return nil, err
	}

	_ = rows.Close()

	return ids, nil
}

func (d *DB) UnsetMemoriesEventId(ctx context.Context, eventId string) (int, error) {
	log.Trace("func() db.UnsetMemoryEventId")

	res, err := d.exec(ctx, `UPDATE memories SET event_id = '' WHERE event_id = ?`, eventId)
	if err != nil {
		return 0, err
	}

	cnt, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	return int(cnt), nil
}

// RecallMemories returns the memories with the given ids, reinforcing each one as a side effect:
// the recall time is set to now and the recall count is incremented. The returned memories
// reflect the reinforced values.
//
// The ids are chunked at deleteChunkSize (like GetMemoriesByIds/deleteMemoriesByIds) so a bulk
// recall of tens of thousands of ids cannot build a single oversized IN (...) that blows the
// dialect's bound-parameter limit and fails the whole call. Duplicate ids are
// collapsed first, so an id repeated across a chunk boundary is still reinforced exactly once -
// matching the single-statement IN, which a set membership test already dedupes.
func (d *DB) RecallMemories(ctx context.Context, ids []string) (*[]types.Memory, error) {
	log.Trace("func() db.RecallMemories")

	var memories []types.Memory

	if len(ids) == 0 {
		return &memories, nil
	}

	ids = DedupeIds(ids)
	now := time.Now().UnixNano()

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		var (
			chunkMemories *[]types.Memory
			err           error
		)

		// MySQL has no UPDATE ... RETURNING at all, so its arm reinforces then reads back in one
		// transaction; the others reinforce and return in a single statement.
		if d.driver == driverMySQL {
			chunkMemories, err = d.recallMemoriesMySQL(ctx, chunk, now)
		} else {
			chunkMemories, err = d.recallMemoriesReturning(ctx, chunk, now)
		}

		if err != nil {
			return nil, err
		}

		memories = append(memories, *chunkMemories...)
	}

	return &memories, nil
}

// recallMemoriesReturning reinforces one chunk of ids and returns the reinforced rows via
// UPDATE ... RETURNING (SQLite and Postgres). now is passed in so every chunk of a single recall
// stamps the same recall time.
func (d *DB) recallMemoriesReturning(ctx context.Context, ids []string, now int64) (*[]types.Memory, error) {
	var memories []types.Memory

	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for _, id := range ids {
		args = append(args, id)
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`UPDATE memories SET time_recalled = ?, recall_count = recall_count + 1
		WHERE id IN (`+placeholders(len(ids))+`)
		RETURNING `+memoryReturningColumns,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		m, err := scanMemoryStored(rows)
		if err != nil {
			return nil, err
		}

		memories = append(memories, m)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	_ = rows.Close()

	// RETURNING cannot join, so it yields the raw level id; resolve each row's rank into
	// Significance before handing the memories back.
	if err := d.fillRanks(ctx, memories); err != nil {
		return nil, err
	}

	return &memories, nil
}

// DedupeIds returns ids with duplicates removed, preserving first-occurrence order.
func DedupeIds(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))

	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}
		out = append(out, id)
	}

	return out
}

// recallMemoriesMySQL is RecallMemories' MySQL arm: reinforce, then read the reinforced rows back
// inside the same transaction, which is what UPDATE ... RETURNING does in one statement on the
// other dialects. The transaction sees its own update, so the returned memories carry the new
// recall state, and a row deleted between the two statements simply drops out of the result the
// same way RETURNING would have omitted it.
func (d *DB) recallMemoriesMySQL(ctx context.Context, ids []string, now int64) (*[]types.Memory, error) {
	log.Trace("func() db.recallMemoriesMySQL")

	var memories []types.Memory

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	updateArgs := make([]any, 0, len(ids)+1)
	updateArgs = append(updateArgs, now)

	selectArgs := make([]any, 0, len(ids))

	for _, id := range ids {
		updateArgs = append(updateArgs, id)
		selectArgs = append(selectArgs, id)
	}

	if _, err := tx.Exec(
		`UPDATE memories SET time_recalled = ?, recall_count = recall_count + 1
		WHERE id IN (`+placeholders(len(ids))+`)`,
		updateArgs...,
	); err != nil {
		_ = tx.Rollback()

		return nil, err
	}

	rows, err := tx.Query(
		`SELECT `+memoryColumns+` FROM `+memoriesFrom+` WHERE id IN (`+placeholders(len(ids))+`)`,
		selectArgs...,
	)
	if err != nil {
		_ = tx.Rollback()

		return nil, err
	}

	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			_ = rows.Close()
			_ = tx.Rollback()

			return nil, err
		}

		memories = append(memories, m)
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		_ = tx.Rollback()

		return nil, err
	}

	_ = rows.Close()

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &memories, nil
}

// GetMemoriesByIds returns the memories with the given ids without reinforcing them, in no
// particular order; ids with no matching row are simply absent from the result. It backs the
// non-reinforcing arm of SearchMemories, where ids come from the secondary search index and any
// that no longer exist in the primary store are stale entries to be dropped.
func (d *DB) GetMemoriesByIds(ctx context.Context, ids []string) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemoriesByIds")

	var memories []types.Memory

	if len(ids) == 0 {
		return &memories, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// Chunked like deleteMemoriesByIds to stay well inside bound-parameter limits.
	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))

		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}

		rows, err := d.query(ctx, `SELECT `+memoryColumns+` FROM `+memoriesFrom+` WHERE id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			m, err := scanMemory(rows)
			if err != nil {
				_ = rows.Close()

				return nil, err
			}

			memories = append(memories, m)
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()

			return nil, err
		}

		_ = rows.Close()
	}

	return &memories, nil
}

// GetIndexableMemoriesPage returns up to limit non-binary memories whose id sorts after afterId,
// in ascending id order — keyset pagination for the search-index backfill tool, so the tool never
// holds one long-running query across the whole table (the SQLite pool has a single connection).
// Binary memories are excluded because they are never indexed. Like SetMemoryDeleteObserver, this
// is deliberately on the concrete DB rather than the Store interface: it exists solely for the
// optional search index.
func (d *DB) GetIndexableMemoriesPage(ctx context.Context, afterId string, limit int) ([]types.Memory, error) {
	log.Trace("func() db.GetIndexableMemoriesPage")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT `+memoryColumns+` FROM `+memoriesFrom+` WHERE id > ? AND NOT is_binary ORDER BY id LIMIT ?`,
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

// GetMemoriesByEventIds returns the memories belonging to any of the given event ids in a single
// query, so a caller listing a page of events with their memories makes one round trip instead of
// one per event. The caller groups the result by EventId. The id set is bounded by the
// event page size, so a single IN (...) stays well inside the bound-parameter limits.
func (d *DB) GetMemoriesByEventIds(ctx context.Context, eventIds []string) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemoriesByEventIds")

	var memories []types.Memory

	if len(eventIds) == 0 {
		return &memories, nil
	}

	args := make([]any, len(eventIds))
	for i, id := range eventIds {
		args[i] = id
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, `SELECT `+memoryColumns+` FROM `+memoriesFrom+` WHERE event_id IN (`+placeholders(len(eventIds))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

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

	return &memories, nil
}

func (d *DB) GetMemoriesByEventId(ctx context.Context, eventId string) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemoriesByEventId")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, `SELECT `+memoryColumns+` FROM `+memoriesFrom+` WHERE event_id = ?`, eventId)
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

	return &memories, nil
}

func (d *DB) MergeEventMemories(ctx context.Context, toEventId string, fromEventId string) error {
	log.Trace("func() db.MergeEventMemories")

	_, err := d.exec(ctx, `UPDATE memories SET event_id = ? WHERE event_id = ?`, toEventId, fromEventId)

	return err
}

// ReplaceMemoriesWithSummary deletes every memory associated with eventId and inserts the given
// summary memory in their place, all within a single transaction. Returns the number of memories
// replaced.
func (d *DB) ReplaceMemoriesWithSummary(ctx context.Context, eventId string, summary types.Memory) (int, error) {
	log.Trace("func() db.ReplaceMemoriesWithSummary")

	// Resolve the summary's significance level before the delete/insert transaction (level
	// resolution runs in its own transaction).
	levelID, err := d.ensureSignificanceLevel(ctx, summary.Significance, summary.SignificanceLevelID)
	if err != nil {
		return 0, err
	}

	summary.SignificanceLevelID = levelID

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	// Read the ids before the delete: the summary replaces these memories, so their links go with
	// them, and the link graph is keyed on memory id rather than on the event.
	replacedIds, err := d.memoryIdsForEvent(tx, eventId)
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	res, err := tx.Exec(d.rebind(`DELETE FROM memories WHERE event_id = ?`), eventId)
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	replaced, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if err := d.pruneMemoryLinks(tx, replacedIds); err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	body, isCompressed := d.compressBody(summary.Body, summary.IsBinary)

	metadata, err := types.MarshalMetadata(summary.Metadata)
	if err != nil {
		_ = tx.Rollback()

		log.Errorf("failed to encode metadata of summary memory '%s': %s", summary.Id, err.Error())

		return 0, err
	}

	if _, err := tx.Exec(
		d.rebind(`INSERT INTO memories (`+memoryStoredColumns+`) VALUES `+memoryValuePlaceholders),
		summary.Id,
		summary.TimeStamp,
		levelIDArg(summary.SignificanceLevelID),
		summary.EventId,
		body,
		summary.IsBinary,
		summary.TimeRecalled,
		summary.RecallCount,
		summary.IsSummary,
		summary.Group,
		isCompressed,
		metadata,
	); err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	// The replaced memories' index entries went with them inside the transaction, via the delete
	// trigger; only the summary that took their place needs adding.
	_ = d.indexMemoryContent(ctx, summary.Id, summary.Body, summary.IsBinary)

	return int(replaced), nil
}

// FindSummarisationCandidates returns events whose memory count is at least minMemories and
// whose most recently touched memory (by creation or recall) is older than maxTimestamp, ordered
// by memory count descending. limit caps the number of rows returned; 0 leaves it unbounded.
// is_summary memories are excluded, so an event only reappears once fresh, unsummarised memories
// have accumulated again.
func (d *DB) FindSummarisationCandidates(ctx context.Context, minMemories int, maxTimestamp int64, limit int) ([]SummarisationCandidate, error) {
	log.Trace("func() db.FindSummarisationCandidates")

	// SQLite's two-argument MAX is a scalar function; Postgres and MySQL spell the same thing
	// GREATEST (their MAX is aggregate-only).
	greatest := `MAX(m.timestamp, m.time_recalled)`
	if d.driver != driverSQLite {
		greatest = `GREATEST(m.timestamp, m.time_recalled)`
	}

	// e.group_name rides along so the served list can be narrowed to a group-scoped caller without a
	// second lookup; the scan itself stays store-wide (see SummarisationCandidate.Group).
	query := `
		SELECT m.event_id, e.name, COUNT(*), e.group_name
		FROM memories m INNER JOIN events e ON e.id = m.event_id
		WHERE m.event_id != '' AND NOT m.is_summary
		GROUP BY m.event_id, e.name, e.group_name
		HAVING COUNT(*) >= ? AND MAX(` + greatest + `) < ?
		ORDER BY COUNT(*) DESC`

	args := []any{minMemories, maxTimestamp}

	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var candidates []SummarisationCandidate

	for rows.Next() {
		var c SummarisationCandidate

		if err := rows.Scan(&c.EventId, &c.EventName, &c.MemoryCount, &c.Group); err != nil {
			return nil, err
		}

		candidates = append(candidates, c)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return candidates, nil
}

// memoryOrderClauses maps the API order_by values to fixed, injection-safe ORDER BY clauses. The
// order_by string is never interpolated into SQL directly — only these constant clauses are. A
// stable id tiebreaker keeps offset pagination deterministic across pages.
var memoryOrderClauses = map[string]string{
	"significance": `significance DESC, timestamp DESC, id ASC`,
	"timestamp":    `timestamp DESC, id ASC`,
}

const defaultMemoryOrderBy = "significance"

// memoryFilterConditions builds the shared WHERE clause and its args for the memory filter, so
// GetMemories and CountMemoriesFiltered stay in lock-step over the exact same predicate.
//
// SignificanceExtremum, when set, replaces the SignificanceMin/SignificanceMax range check with an
// equality match against the highest (or lowest) significance value among memories matching the
// other filters - computed via a subquery built from this same function (with the extremum and
// range fields cleared), so the "other filters" stay identical between the two.
//
// IMPORTANT: every predicate other than the significance range must be added ABOVE the extremum
// block, which returns early. A clause added below it would be silently dropped from any request
// that also set an extremum - and, worse, would not compose into the subquery, so "the highest
// significance among the never-recalled memories" would quietly become "the highest significance
// overall, filtered to the never-recalled ones", which is a different and usually empty answer.
//
// It is a method rather than a package function because the metadata predicate is dialect-specific
// (see metadata.go).
func (d *DB) memoryFilterConditions(filter MemoryFilter) (string, []any) {
	query := ` WHERE 1=1`
	var args []any

	if filter.TimeStampMin > 0 {
		query += ` AND timestamp >= ?`
		args = append(args, filter.TimeStampMin)
	}

	if filter.TimeStampMax > 0 {
		query += ` AND timestamp <= ?`
		args = append(args, filter.TimeStampMax)
	}

	if filter.Group != "" {
		query += ` AND group_name = ?`
		args = append(args, filter.Group)
	}

	// The caller's group scope, conjoined with (not replacing) the Group filter above: asking for a
	// group outside the scope must return nothing rather than widen it.
	query, args = appendGroupScope(query, args, "", filter.Groups)

	// Ids restricts the result to a known set - the linked-to filter resolves a memory's neighbours
	// and passes them here, so link traversal composes with every other filter and with pagination
	// rather than being a separate listing path. An empty (nil) slice is no restriction; a caller
	// with an empty set must short-circuit rather than ask for "in nothing", which no dialect
	// spells the same way.
	if len(filter.Ids) > 0 {
		query += ` AND id IN (` + placeholders(len(filter.Ids)) + `)`

		for _, id := range filter.Ids {
			args = append(args, id)
		}
	}

	// Metadata is a conjunction of per-key predicates; the dialect differences live in metadata.go.
	// A memory with no metadata holds NULL, which equals nothing on any dialect, so it is correctly
	// excluded by any key predicate without a special case.
	query, args = d.appendMetadataConditions(query, args, "", filter.Metadata)

	// Recall state. Recalled is the tri-state that answers "what have I never recalled?", which the
	// count range cannot: RecallCountMax of 0 reads as no bound under the package's usual rule.
	switch filter.Recalled {

	case TriStateFalse:
		query += ` AND recall_count = 0`

	case TriStateTrue:
		query += ` AND recall_count > 0`

	}

	if filter.RecallCountMin > 0 {
		query += ` AND recall_count >= ?`
		args = append(args, filter.RecallCountMin)
	}

	if filter.RecallCountMax > 0 {
		query += ` AND recall_count <= ?`
		args = append(args, filter.RecallCountMax)
	}

	// Both bounds are asked only of memories that have actually been recalled. A never-recalled
	// memory has time_recalled = 0, so the lower bound excludes it naturally, but the upper bound
	// would sweep in every never-recalled memory in the store - "recalled before Tuesday" answering
	// with memories that were never recalled at all. The explicit > 0 makes the two symmetric: this
	// pair asks "of the memories that were recalled, which fall in this window", and Recalled is
	// what asks about the never-recalled ones.
	if filter.TimeRecalledMin > 0 {
		query += ` AND time_recalled >= ?`
		args = append(args, filter.TimeRecalledMin)
	}

	if filter.TimeRecalledMax > 0 {
		query += ` AND time_recalled > 0 AND time_recalled <= ?`
		args = append(args, filter.TimeRecalledMax)
	}

	// The boolean columns are bound as Go bools rather than 0/1 literals: they are INTEGER on
	// SQLite but BOOLEAN on Postgres and MySQL, and a bound bool is correct against all three.
	if arg, ok := triStateArg(filter.IsSummary); ok {
		query += ` AND is_summary = ?`
		args = append(args, arg)
	}

	if arg, ok := triStateArg(filter.IsBinary); ok {
		query += ` AND is_binary = ?`
		args = append(args, arg)
	}

	if filter.SignificanceExtremum != SignificanceExtremumNone {
		aggregate := "MAX"
		if filter.SignificanceExtremum == SignificanceExtremumLowest {
			aggregate = "MIN"
		}

		subFilter := filter
		subFilter.SignificanceExtremum = SignificanceExtremumNone
		subFilter.SignificanceMin = 0
		subFilter.SignificanceMax = 0
		subWhere, subArgs := d.memoryFilterConditions(subFilter)

		query += ` AND significance = (SELECT ` + aggregate + `(significance) FROM ` + memoriesFrom + subWhere + `)`
		args = append(args, subArgs...)

		return query, args
	}

	if filter.SignificanceMin > 0 {
		query += ` AND significance >= ?`
		args = append(args, filter.SignificanceMin)
	}

	if filter.SignificanceMax > 0 {
		query += ` AND significance <= ?`
		args = append(args, filter.SignificanceMax)
	}

	return query, args
}

// CountMemoriesFiltered returns the number of memories matching the filter, ignoring Limit/Offset
// so the caller can size pagination.
func (d *DB) CountMemoriesFiltered(ctx context.Context, filter MemoryFilter) (int, error) {
	log.Trace("func() db.CountMemoriesFiltered")

	where, args := d.memoryFilterConditions(filter)

	var count int

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+memoriesFrom+where, args...).Scan(&count); err != nil {
		return 0, err
	}

	return count, nil
}

func (d *DB) GetMemories(ctx context.Context, filter MemoryFilter) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemories")

	where, args := d.memoryFilterConditions(filter)

	order, ok := memoryOrderClauses[filter.OrderBy]
	if !ok {
		order = memoryOrderClauses[defaultMemoryOrderBy]
	}

	query := `SELECT ` + memoryColumns + ` FROM ` + memoriesFrom + where + ` ORDER BY ` + order

	// OFFSET is only valid alongside LIMIT in SQLite/MySQL, so both are gated on a positive limit.
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)

		if filter.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, filter.Offset)
		}
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
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

	return &memories, nil
}

// CountMemories returns the number of memories with an event and the number without. A count of
// -1 indicates the count could not be determined.
func (d *DB) CountMemories(ctx context.Context) (int, int) {
	log.Trace("func() db.CountMemories")

	var with, without int

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// COUNT over a CASE with no ELSE counts the rows where the condition holds — the portable
	// spelling of COUNT(*) FILTER (WHERE ...), which MySQL does not support.
	err := d.queryRow(
		ctx,
		`SELECT
			COUNT(CASE WHEN event_id != '' THEN 1 END),
			COUNT(CASE WHEN event_id = '' THEN 1 END)
		FROM memories`,
	).Scan(&with, &without)
	if err != nil {
		log.Errorf("failed to count memories: %s", err.Error())

		return -1, -1
	}

	return with, without
}

// ConsolidateMemories evaluates every memory that has no associated event and deletes those the
// server decides should be consolidated. The scan reads only the covering index; memory bodies
// are never loaded.
func (d *DB) ConsolidateMemories(ctx context.Context, s Server) (int, error) {
	log.Trace("func() db.ConsolidateMemories")

	ranks, err := d.loadSignificanceRanks(ctx)
	if err != nil {
		log.Errorf("failed to load significance registry: %s", err.Error())

		return 0, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// significance_level_id is read from the covering index and translated to its rank via the
	// registry snapshot in Go, so the scan stays on the covering index and never reads memory bodies.
	// link_significance is in that index for the same reason: it is a per-row input to the value
	// calculation, so reading it from the index keeps the scan off the table entirely.
	rows, err := d.query(
		ctx,
		`SELECT id, timestamp, significance_level_id, time_recalled, recall_count, link_significance
		FROM memories WHERE event_id = ''`,
	)
	if err != nil {
		log.Errorf("failed to consolidate memories: %s", err.Error())

		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var deletions []memoryRecallSnapshot

	for rows.Next() {
		var id string
		var levelID sql.NullInt64
		var candidate MemoryConsolidationCandidate

		if err := rows.Scan(
			&id,
			&candidate.Timestamp,
			&levelID,
			&candidate.TimeRecalled,
			&candidate.RecallCount,
			&candidate.MemoryLinkSignificance,
		); err != nil {
			log.Errorf("failed to scan memory for consolidation: %s", err.Error())

			return 0, err
		}

		candidate.MemorySignificance = rankOf(ranks, levelID)

		if s.ShouldConsolidateMemory(candidate) {
			deletions = append(deletions, memoryRecallSnapshot{
				id:           id,
				timeRecalled: candidate.TimeRecalled,
				recallCount:  candidate.RecallCount,
			})
		}
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to consolidate memories: %s", err.Error())

		return 0, err
	}

	_ = rows.Close()

	deletedIds, err := d.deleteMemoriesIfUnrecalled(ctx, deletions)
	if err != nil {
		log.Errorf("failed to delete consolidated memories: %s", err.Error())

		return len(deletedIds), err
	}

	return len(deletedIds), nil
}

// EvictMemories deletes the least valuable memories until an estimated freeBytes bytes have been
// reclaimed. It backs the capacity target: unlike the consolidation passes it applies no
// minimum-age protection — the storage bound must be achievable no matter how fresh the store is
// — but the value ranking still sends the most significant and most recently recalled memories
// to the back of the queue. Events stripped of their last memory are deleted; events losing only
// some of their memories are flagged as consolidated. Unlike the consolidation scans this reads
// body lengths, but SQLite serves length() from the record header without loading the content.
// Returns the number of memories deleted, the number of events deleted, and the estimated bytes
// freed.
func (d *DB) EvictMemories(ctx context.Context, s Server, freeBytes int64) (int, int, int64, error) {
	log.Trace("func() db.EvictMemories")

	if freeBytes <= 0 {
		return 0, 0, 0, nil
	}

	type evictionCandidate struct {
		id           string
		eventId      string
		size         int64
		value        float64
		timeRecalled int64
		recallCount  int32
	}

	ranks, err := d.loadSignificanceRanks(ctx)
	if err != nil {
		log.Errorf("failed to load significance registry: %s", err.Error())

		return 0, 0, 0, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// The memories_consolidated fallback is bound rather than a literal: the column is INTEGER
	// on SQLite but BOOLEAN on Postgres, and a bound false coalesces cleanly against both. The
	// memory and event significance level ids are translated to ranks via the registry snapshot.
	rows, err := d.query(
		ctx,
		`SELECT m.id, m.timestamp, m.significance_level_id, m.time_recalled, m.recall_count, m.event_id,
			e.significance_level_id, COALESCE(e.link_significance, 0), m.link_significance,
			COALESCE(e.memories_consolidated, ?), length(m.body) + `+d.metadataBytesExpr("m.")+`
		FROM memories m LEFT JOIN events e ON e.id = m.event_id`,
		false,
	)
	if err != nil {
		log.Errorf("failed to evict memories: %s", err.Error())

		return 0, 0, 0, err
	}
	defer func() { _ = rows.Close() }()

	var evictionCandidates []evictionCandidate
	memoriesPerEvent := make(map[string]int)
	consolidatedEvents := make(map[string]bool)

	for rows.Next() {
		var c evictionCandidate
		var candidate MemoryConsolidationCandidate
		var consolidated bool
		var memoryLevelID, eventLevelID sql.NullInt64

		if err := rows.Scan(
			&c.id,
			&candidate.Timestamp,
			&memoryLevelID,
			&candidate.TimeRecalled,
			&candidate.RecallCount,
			&c.eventId,
			&eventLevelID,
			&candidate.EventLinkSignificance,
			&candidate.MemoryLinkSignificance,
			&consolidated,
			&c.size,
		); err != nil {
			log.Errorf("failed to scan memory for eviction: %s", err.Error())

			return 0, 0, 0, err
		}

		candidate.MemorySignificance = rankOf(ranks, memoryLevelID)
		candidate.EventSignificance = rankOf(ranks, eventLevelID)

		// Count every memory toward its event's total BEFORE the retention filter below. An event is
		// deleted only once all of its memories have been evicted; a retained memory must keep its
		// event alive, so it has to be counted here even though it will never be an eviction
		// candidate - otherwise the event could be seen as fully evicted and deleted out from under
		// the memory that survived, leaving it dangling.
		if c.eventId != "" {
			memoriesPerEvent[c.eventId]++
			consolidatedEvents[c.eventId] = consolidated
		}

		// A memory still inside its minimum retention window is protected from eviction even when the
		// store is over its byte target: the retention floor overrides the capacity limit, so leave
		// it out of the candidate pool entirely rather than merely ranking it last.
		if s.MemoryRetained(candidate) {
			continue
		}

		c.value = s.MemoryValue(candidate)
		c.timeRecalled = candidate.TimeRecalled
		c.recallCount = candidate.RecallCount
		evictionCandidates = append(evictionCandidates, c)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to evict memories: %s", err.Error())

		return 0, 0, 0, err
	}

	_ = rows.Close()

	sort.Slice(evictionCandidates, func(i int, j int) bool {
		return evictionCandidates[i].value < evictionCandidates[j].value
	})

	var deletions []memoryRecallSnapshot
	eventIdByMemory := make(map[string]string)
	freedById := make(map[string]int64)
	var selected int64

	for _, c := range evictionCandidates {
		if selected >= freeBytes {
			break
		}

		rowBytes := c.size + evictionRowOverheadBytes
		selected += rowBytes
		freedById[c.id] = rowBytes
		deletions = append(deletions, memoryRecallSnapshot{
			id:           c.id,
			timeRecalled: c.timeRecalled,
			recallCount:  c.recallCount,
		})

		if c.eventId != "" {
			eventIdByMemory[c.id] = c.eventId
		}
	}

	deletedIds, err := d.deleteMemoriesIfUnrecalled(ctx, deletions)
	if err != nil {
		log.Errorf("failed to delete evicted memories: %s", err.Error())

		return 0, 0, 0, err
	}

	// Everything below is derived from the rows ACTUALLY deleted (deletedIds), not the selection.
	// The recall-race guard in deleteMemoriesIfUnrecalled may skip a selected candidate (recalled
	// since the scan), so counting from the selection would overstate the freed bytes and, worse,
	// flag an event as consolidated when none of its memories actually went - or count it toward the
	// all-evicted event-delete test.
	countMemories := len(deletedIds)
	var freed int64
	evictedPerEvent := make(map[string]int)

	for _, id := range deletedIds {
		freed += freedById[id]

		if eid, ok := eventIdByMemory[id]; ok {
			evictedPerEvent[eid]++
		}
	}

	// Delete events whose memories were all evicted, otherwise set MemoriesConsolidated. A
	// concurrent write can have attached a fresh memory to the event since the scan above, or a
	// concurrent recall can have kept one of its memories out of countMemories's deletions;
	// DeleteEventIfEmpty re-checks live state so the event only goes if it's actually empty. These
	// per-event cleanups are best-effort - retErr surfaces the first failure for the sleep cycle's
	// success metric without stopping the remaining events.
	countEvents := 0
	var retErr error

	for id, evicted := range evictedPerEvent {
		deleted := false

		if evicted == memoriesPerEvent[id] {
			var err error

			deleted, err = d.DeleteEventIfEmpty(ctx, id)
			if err != nil {
				log.Errorf("failed to delete event '%s' after eviction: %s", id, err.Error())

				if retErr == nil {
					retErr = err
				}
			}
		}

		if deleted {
			countEvents++

			continue
		}

		if !consolidatedEvents[id] {
			if err := d.setEventConsolidated(ctx, id); err != nil {
				log.Errorf("failed to set MemoriesConsolidated for event '%s' after eviction: %s", id, err.Error())

				if retErr == nil {
					retErr = err
				}
			}
		}
	}

	return countMemories, countEvents, freed, retErr
}

// ConsolidateEventMemories evaluates every memory carrying an event_id, deleting those the server
// decides should be consolidated. An event whose memories are all deleted is deleted with them; an
// event losing only some of its memories is flagged as consolidated. A memory whose event_id names
// a nonexistent event (a dangling reference) is caught by the LEFT JOIN and
// evaluated as if it were event-less (no event significance, no relationship significance, via the
// COALESCE defaults), so it decays like any other memory instead of being immortal; its phantom
// event id is never entered into the event bookkeeping, so the events-seen count and the per-event
// cleanups stay confined to events that actually exist. Returns the number of memories deleted,
// the number of (real) events seen, and the number of events deleted.
func (d *DB) ConsolidateEventMemories(ctx context.Context, s Server) (int, int, int, error) {
	log.Trace("func() db.ConsolidateEventMemories")

	type EventDeletion struct {
		undeletedMemory bool
		consolidated    bool
	}

	eventDeletions := make(map[string]EventDeletion)
	var memoryDeletions []memoryRecallSnapshot

	ranks, err := d.loadSignificanceRanks(ctx)
	if err != nil {
		log.Errorf("failed to load significance registry: %s", err.Error())

		return 0, 0, 0, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// LEFT JOIN, not INNER: an INNER JOIN silently drops memories whose event no longer exists, so
	// they are never evaluated by any pass and can never decay. The COALESCE defaults (mirroring
	// EvictMemories) let such a memory be scored as event-less; the bound false covers the
	// INTEGER-on-SQLite / BOOLEAN-on-Postgres memories_consolidated column. e.id is selected purely
	// to tell a real event (non-null) from a dangling reference (null). The memory and event
	// significance level ids are translated to ranks via the registry snapshot.
	rows, err := d.query(
		ctx,
		`SELECT m.id, m.timestamp, m.significance_level_id, m.time_recalled, m.recall_count, m.event_id,
			e.significance_level_id, COALESCE(e.link_significance, 0), m.link_significance,
			COALESCE(e.memories_consolidated, ?), e.id
		FROM memories m LEFT JOIN events e ON e.id = m.event_id
		WHERE m.event_id != ''`,
		false,
	)
	if err != nil {
		log.Errorf("failed to consolidate memories: %s", err.Error())

		return 0, 0, 0, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id, eventId string
		var consolidated bool
		var joinedEventId sql.NullString
		var memoryLevelID, eventLevelID sql.NullInt64
		var candidate MemoryConsolidationCandidate

		if err := rows.Scan(
			&id,
			&candidate.Timestamp,
			&memoryLevelID,
			&candidate.TimeRecalled,
			&candidate.RecallCount,
			&eventId,
			&eventLevelID,
			&candidate.EventLinkSignificance,
			&candidate.MemoryLinkSignificance,
			&consolidated,
			&joinedEventId,
		); err != nil {
			log.Errorf("failed to scan memory for consolidation: %s", err.Error())

			return 0, 0, 0, err
		}

		candidate.MemorySignificance = rankOf(ranks, memoryLevelID)
		candidate.EventSignificance = rankOf(ranks, eventLevelID)

		// A dangling reference has no event row to delete or flag; treat the memory purely as a
		// consolidation candidate and keep it out of the event bookkeeping entirely.
		eventExists := joinedEventId.Valid

		if eventExists {
			if _, ok := eventDeletions[eventId]; !ok {
				eventDeletions[eventId] = EventDeletion{consolidated: consolidated}
			}
		}

		if s.ShouldConsolidateMemory(candidate) {
			memoryDeletions = append(memoryDeletions, memoryRecallSnapshot{
				id:           id,
				timeRecalled: candidate.TimeRecalled,
				recallCount:  candidate.RecallCount,
			})
		} else if eventExists {
			eventDeletion := eventDeletions[eventId]
			eventDeletion.undeletedMemory = true
			eventDeletions[eventId] = eventDeletion
		}
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to consolidate memories: %s", err.Error())

		return 0, 0, 0, err
	}

	_ = rows.Close()

	// retErr carries the first failure encountered from here on. The bulk delete and the per-event
	// cleanup below are best-effort - a failure on one event must not stop the others - so they log
	// and carry on, but the error is still surfaced so the sleep cycle's success metric reflects it.
	deletedIds, retErr := d.deleteMemoriesIfUnrecalled(ctx, memoryDeletions)
	if retErr != nil {
		log.Errorf("failed to delete consolidated memories: %s", retErr.Error())
	}

	countMemories := len(deletedIds)

	// Delete events where all memories have been deleted, otherwise, set MemoriesConsolidated.
	// DeleteEventIfEmpty re-checks live state, since a concurrent write can have attached a fresh
	// memory to the event, or a concurrent recall can have kept one of its memories alive, since
	// the scan above ran.
	countEventsDeleted := 0

	for id, event := range eventDeletions {
		deleted := false

		if !event.undeletedMemory {
			var err error

			deleted, err = d.DeleteEventIfEmpty(ctx, id)
			if err != nil {
				log.Errorf("failed to delete event '%s' for memory consolidation: %s", id, err.Error())

				if retErr == nil {
					retErr = err
				}
			}
		}

		if deleted {
			countEventsDeleted++

			continue
		}

		if !event.consolidated {
			if err := d.setEventConsolidated(ctx, id); err != nil {
				log.Errorf("failed to set MemoriesConsolidated for event '%s' during memory consolidation: %s", id, err.Error())

				if retErr == nil {
					retErr = err
				}
			}
		}
	}

	return countMemories, len(eventDeletions), countEventsDeleted, retErr
}
