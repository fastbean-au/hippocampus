package db

import (
	"database/sql"
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

const memoryColumns = `id, timestamp, significance, event_id, body, is_binary, time_recalled, recall_count, is_summary, group_name`

// placeholders returns a comma-separated list of n SQL parameter placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func scanMemory(rows *sql.Rows) (types.Memory, error) {
	var m types.Memory
	var body []byte

	if err := rows.Scan(&m.Id, &m.TimeStamp, &m.Significance, &m.EventId, &body, &m.IsBinary, &m.TimeRecalled, &m.RecallCount, &m.IsSummary, &m.Group); err != nil {
		return m, err
	}

	m.Body = string(body)

	return m, nil
}

// CreateMemory creates a memory record, returning the id and an error
func (d *DB) CreateMemory(memory types.Memory) (string, error) {
	log.Trace("func() db.CreateMemory")

	_, err := d.exec(
		`INSERT INTO memories (`+memoryColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		memory.Id,
		memory.TimeStamp,
		memory.Significance,
		memory.EventId,
		[]byte(memory.Body),
		memory.IsBinary,
		memory.TimeRecalled,
		memory.RecallCount,
		memory.IsSummary,
		memory.Group,
	)

	return memory.Id, err
}

// UpdateMemory applies a partial update to an existing memory: only fields carrying a non-zero
// value overwrite the stored row. It does not create the memory when the id is unknown - it returns
// (false, nil) so callers can surface NotFound rather than inserting a phantom row (the same
// treatment UpdateEvent received). Returns whether a matching memory existed.
//
// It deliberately does not touch is_binary or is_summary: those are set at creation and by
// ReplaceMemoriesWithSummary respectively, and are outside the partial-update surface (see #22).
func (d *DB) UpdateMemory(memory types.Memory) (bool, error) {
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

	if memory.Significance > 0 {
		sets = append(sets, `significance = ?`)
		args = append(args, memory.Significance)
	}

	if memory.EventId != "" {
		sets = append(sets, `event_id = ?`)
		args = append(args, memory.EventId)
	}

	if len(memory.Body) > 0 {
		sets = append(sets, `body = ?`)
		args = append(args, []byte(memory.Body))
	}

	if memory.Group != "" {
		sets = append(sets, `group_name = ?`)
		args = append(args, memory.Group)
	}

	// Nothing to change: there is no UPDATE to learn existence from, so probe for it directly.
	if len(sets) == 0 {
		return d.memoryExists(memory.Id)
	}

	args = append(args, memory.Id)

	res, err := d.exec(`UPDATE memories SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return false, err
	}

	return d.updatedRowExisted(res, "memories", memory.Id)
}

// memoryExists reports whether a memory with the given id exists.
func (d *DB) memoryExists(id string) (bool, error) {
	var exists bool

	if err := d.queryRow(`SELECT EXISTS(SELECT 1 FROM memories WHERE id = ?)`, id).Scan(&exists); err != nil {
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
func (d *DB) updatedRowExisted(res sql.Result, table string, id string) (bool, error) {
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

	if err := d.queryRow(`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id = ?)`, id).Scan(&exists); err != nil {
		return false, err
	}

	return exists, nil
}

// DeleteMemory deletes a single memory by id. See UpdateMemory's note: it has no production caller
// yet (only tests).
func (d *DB) DeleteMemory(id string) error {
	log.Trace("func() db.DeleteMemory")

	_, err := d.exec(`DELETE FROM memories WHERE id = ?`, id)

	return err
}

func (d *DB) DeleteMemories(ids []string) (int, error) {
	log.Trace("func() db.DeleteMemories")

	return d.deleteMemoriesByIds(ids)
}

// deleteMemoriesByIds deletes the given memories in chunked IN (...) batches inside a single
// transaction, returning the number of rows deleted.
func (d *DB) deleteMemoriesByIds(ids []string) (int, error) {
	cnt := 0

	if len(ids) == 0 {
		return 0, nil
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}

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
func (d *DB) deleteMemoriesIfUnrecalled(items []memoryRecallSnapshot) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}

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

func (d *DB) DeleteEventMemories(eventId string) (int, error) {
	log.Trace("func() db.DeleteEventMemories")

	res, err := d.exec(`DELETE FROM memories WHERE event_id = ?`, eventId)
	if err != nil {
		return 0, err
	}

	cnt, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	return int(cnt), nil
}

func (d *DB) UnsetMemoriesEventId(eventId string) (int, error) {
	log.Trace("func() db.UnsetMemoryEventId")

	res, err := d.exec(`UPDATE memories SET event_id = '' WHERE event_id = ?`, eventId)
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
func (d *DB) RecallMemories(ids []string) (*[]types.Memory, error) {
	log.Trace("func() db.RecallMemories")

	var memories []types.Memory

	if len(ids) == 0 {
		return &memories, nil
	}

	ids = dedupeIds(ids)
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
			chunkMemories, err = d.recallMemoriesMySQL(chunk, now)
		} else {
			chunkMemories, err = d.recallMemoriesReturning(chunk, now)
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
func (d *DB) recallMemoriesReturning(ids []string, now int64) (*[]types.Memory, error) {
	var memories []types.Memory

	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := d.query(
		`UPDATE memories SET time_recalled = ?, recall_count = recall_count + 1
		WHERE id IN (`+placeholders(len(ids))+`)
		RETURNING `+memoryColumns,
		args...,
	)
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

// dedupeIds returns ids with duplicates removed, preserving first-occurrence order.
func dedupeIds(ids []string) []string {
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
func (d *DB) recallMemoriesMySQL(ids []string, now int64) (*[]types.Memory, error) {
	log.Trace("func() db.recallMemoriesMySQL")

	var memories []types.Memory

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}

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
		`SELECT `+memoryColumns+` FROM memories WHERE id IN (`+placeholders(len(ids))+`)`,
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
func (d *DB) GetMemoriesByIds(ids []string) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemoriesByIds")

	var memories []types.Memory

	if len(ids) == 0 {
		return &memories, nil
	}

	// Chunked like deleteMemoriesByIds to stay well inside bound-parameter limits.
	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))

		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}

		rows, err := d.query(`SELECT `+memoryColumns+` FROM memories WHERE id IN (`+placeholders(len(chunk))+`)`, args...)
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
func (d *DB) GetIndexableMemoriesPage(afterId string, limit int) ([]types.Memory, error) {
	log.Trace("func() db.GetIndexableMemoriesPage")

	rows, err := d.query(
		`SELECT `+memoryColumns+` FROM memories WHERE id > ? AND NOT is_binary ORDER BY id LIMIT ?`,
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
func (d *DB) GetMemoriesByEventIds(eventIds []string) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemoriesByEventIds")

	var memories []types.Memory

	if len(eventIds) == 0 {
		return &memories, nil
	}

	args := make([]any, len(eventIds))
	for i, id := range eventIds {
		args[i] = id
	}

	rows, err := d.query(`SELECT `+memoryColumns+` FROM memories WHERE event_id IN (`+placeholders(len(eventIds))+`)`, args...)
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

func (d *DB) GetMemoriesByEventId(eventId string) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemoriesByEventId")

	rows, err := d.query(`SELECT `+memoryColumns+` FROM memories WHERE event_id = ?`, eventId)
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

func (d *DB) MergeEventMemories(toEventId string, fromEventId string) error {
	log.Trace("func() db.MergeEventMemories")

	_, err := d.exec(`UPDATE memories SET event_id = ? WHERE event_id = ?`, toEventId, fromEventId)

	return err
}

// ReplaceMemoriesWithSummary deletes every memory associated with eventId and inserts the given
// summary memory in their place, all within a single transaction. Returns the number of memories
// replaced.
func (d *DB) ReplaceMemoriesWithSummary(eventId string, summary types.Memory) (int, error) {
	log.Trace("func() db.ReplaceMemoriesWithSummary")

	tx, err := d.sql.Begin()
	if err != nil {
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

	if _, err := tx.Exec(
		d.rebind(`INSERT INTO memories (`+memoryColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		summary.Id,
		summary.TimeStamp,
		summary.Significance,
		summary.EventId,
		[]byte(summary.Body),
		summary.IsBinary,
		summary.TimeRecalled,
		summary.RecallCount,
		summary.IsSummary,
		summary.Group,
	); err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return int(replaced), nil
}

// FindSummarizationCandidates returns events whose memory count is at least minMemories and
// whose most recently touched memory (by creation or recall) is older than maxTimestamp, ordered
// by memory count descending. limit caps the number of rows returned; 0 leaves it unbounded.
// is_summary memories are excluded, so an event only reappears once fresh, unsummarized memories
// have accumulated again.
func (d *DB) FindSummarizationCandidates(minMemories int, maxTimestamp int64, limit int) ([]SummarizationCandidate, error) {
	log.Trace("func() db.FindSummarizationCandidates")

	// SQLite's two-argument MAX is a scalar function; Postgres and MySQL spell the same thing
	// GREATEST (their MAX is aggregate-only).
	greatest := `MAX(m.timestamp, m.time_recalled)`
	if d.driver != driverSQLite {
		greatest = `GREATEST(m.timestamp, m.time_recalled)`
	}

	query := `
		SELECT m.event_id, e.name, COUNT(*)
		FROM memories m INNER JOIN events e ON e.id = m.event_id
		WHERE m.event_id != '' AND NOT m.is_summary
		GROUP BY m.event_id, e.name
		HAVING COUNT(*) >= ? AND MAX(` + greatest + `) < ?
		ORDER BY COUNT(*) DESC`

	args := []any{minMemories, maxTimestamp}

	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := d.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var candidates []SummarizationCandidate

	for rows.Next() {
		var c SummarizationCandidate

		if err := rows.Scan(&c.EventId, &c.EventName, &c.MemoryCount); err != nil {
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
func memoryFilterConditions(filter MemoryFilter) (string, []any) {
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

	if filter.SignificanceMin > 0 {
		query += ` AND significance >= ?`
		args = append(args, filter.SignificanceMin)
	}

	if filter.SignificanceMax > 0 {
		query += ` AND significance <= ?`
		args = append(args, filter.SignificanceMax)
	}

	if filter.Group != "" {
		query += ` AND group_name = ?`
		args = append(args, filter.Group)
	}

	return query, args
}

// CountMemoriesFiltered returns the number of memories matching the filter, ignoring Limit/Offset
// so the caller can size pagination.
func (d *DB) CountMemoriesFiltered(filter MemoryFilter) (int, error) {
	log.Trace("func() db.CountMemoriesFiltered")

	where, args := memoryFilterConditions(filter)

	var count int

	if err := d.queryRow(`SELECT COUNT(*) FROM memories`+where, args...).Scan(&count); err != nil {
		return 0, err
	}

	return count, nil
}

func (d *DB) GetMemories(filter MemoryFilter) (*[]types.Memory, error) {
	log.Trace("func() db.GetMemories")

	where, args := memoryFilterConditions(filter)

	order, ok := memoryOrderClauses[filter.OrderBy]
	if !ok {
		order = memoryOrderClauses[defaultMemoryOrderBy]
	}

	query := `SELECT ` + memoryColumns + ` FROM memories` + where + ` ORDER BY ` + order

	// OFFSET is only valid alongside LIMIT in SQLite/MySQL, so both are gated on a positive limit.
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)

		if filter.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, filter.Offset)
		}
	}

	rows, err := d.query(query, args...)
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
func (d *DB) CountMemories() (int, int) {
	log.Trace("func() db.CountMemories")

	var with, without int

	// COUNT over a CASE with no ELSE counts the rows where the condition holds — the portable
	// spelling of COUNT(*) FILTER (WHERE ...), which MySQL does not support.
	err := d.queryRow(
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
func (d *DB) ConsolidateMemories(s Server) (int, error) {
	log.Trace("func() db.ConsolidateMemories")

	rows, err := d.query(
		`SELECT id, timestamp, significance, time_recalled, recall_count
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
		var candidate MemoryConsolidationCandidate

		if err := rows.Scan(&id, &candidate.Timestamp, &candidate.MemorySignificance, &candidate.TimeRecalled, &candidate.RecallCount); err != nil {
			log.Errorf("failed to scan memory for consolidation: %s", err.Error())

			return 0, err
		}

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

	deletedIds, err := d.deleteMemoriesIfUnrecalled(deletions)
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
func (d *DB) EvictMemories(s Server, freeBytes int64) (int, int, int64, error) {
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

	// The memories_consolidated fallback is bound rather than a literal: the column is INTEGER
	// on SQLite but BOOLEAN on Postgres, and a bound false coalesces cleanly against both.
	rows, err := d.query(
		`SELECT m.id, m.timestamp, m.significance, m.time_recalled, m.recall_count, m.event_id,
			COALESCE(e.significance, 0), COALESCE(e.relationship_significance, 0),
			COALESCE(e.memories_consolidated, ?), length(m.body)
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

		if err := rows.Scan(
			&c.id,
			&candidate.Timestamp,
			&candidate.MemorySignificance,
			&candidate.TimeRecalled,
			&candidate.RecallCount,
			&c.eventId,
			&candidate.EventSignificance,
			&candidate.RelationshipSignificance,
			&consolidated,
			&c.size,
		); err != nil {
			log.Errorf("failed to scan memory for eviction: %s", err.Error())

			return 0, 0, 0, err
		}

		c.value = s.MemoryValue(candidate)
		c.timeRecalled = candidate.TimeRecalled
		c.recallCount = candidate.RecallCount
		evictionCandidates = append(evictionCandidates, c)

		if c.eventId != "" {
			memoriesPerEvent[c.eventId]++
			consolidatedEvents[c.eventId] = consolidated
		}
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

	deletedIds, err := d.deleteMemoriesIfUnrecalled(deletions)
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

			deleted, err = d.DeleteEventIfEmpty(id)
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
			if err := d.setEventConsolidated(id); err != nil {
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
func (d *DB) ConsolidateEventMemories(s Server) (int, int, int, error) {
	log.Trace("func() db.ConsolidateEventMemories")

	type EventDeletion struct {
		undeletedMemory bool
		consolidated    bool
	}

	eventDeletions := make(map[string]EventDeletion)
	var memoryDeletions []memoryRecallSnapshot

	// LEFT JOIN, not INNER: an INNER JOIN silently drops memories whose event no longer exists, so
	// they are never evaluated by any pass and can never decay. The COALESCE defaults (mirroring
	// EvictMemories) let such a memory be scored as event-less; the bound false covers the
	// INTEGER-on-SQLite / BOOLEAN-on-Postgres memories_consolidated column. e.id is selected purely
	// to tell a real event (non-null) from a dangling reference (null).
	rows, err := d.query(
		`SELECT m.id, m.timestamp, m.significance, m.time_recalled, m.recall_count, m.event_id,
			COALESCE(e.significance, 0), COALESCE(e.relationship_significance, 0),
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
		var candidate MemoryConsolidationCandidate

		if err := rows.Scan(
			&id,
			&candidate.Timestamp,
			&candidate.MemorySignificance,
			&candidate.TimeRecalled,
			&candidate.RecallCount,
			&eventId,
			&candidate.EventSignificance,
			&candidate.RelationshipSignificance,
			&consolidated,
			&joinedEventId,
		); err != nil {
			log.Errorf("failed to scan memory for consolidation: %s", err.Error())

			return 0, 0, 0, err
		}

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
	deletedIds, retErr := d.deleteMemoriesIfUnrecalled(memoryDeletions)
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

			deleted, err = d.DeleteEventIfEmpty(id)
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
			if err := d.setEventConsolidated(id); err != nil {
				log.Errorf("failed to set MemoriesConsolidated for event '%s' during memory consolidation: %s", id, err.Error())

				if retErr == nil {
					retErr = err
				}
			}
		}
	}

	return countMemories, len(eventDeletions), countEventsDeleted, retErr
}
