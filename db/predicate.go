package db

import (
	"context"

	log "github.com/sirupsen/logrus"
)

// Predicate deletion: selecting records by a filter rather than by id.
//
// The three functions here are the storage half of DeleteMemoriesByFilter/DeleteEventsByFilter.
// Two of them resolve a filter to ids and the third deletes a batch of events; nothing here
// deletes memories, because the existing by-id chokepoint already does everything a predicate
// delete needs.
//
// That is the design decision worth recording. The obvious implementation is a single
// DELETE ... WHERE <predicate>, and it is wrong here: every memory deletion in this package has to
// prune the link graph, queue the search index's delete inside the same transaction, capture a
// callback delivery from columns that are only readable while the rows still exist, and - when the
// forgotten log is on - record a tombstone per row. A predicate DELETE reaches none of that, and
// the failure is silent: the rows go and the link aggregates, the index and the log quietly
// disagree with the store forever after.
//
// So the RPC layer resolves the predicate to ids in bounded batches and hands each batch to
// deleteMemoriesByIds (or, for events, to DeleteEvents below). Each batch is one transaction, the
// selection always takes the FIRST matching ids rather than an offset, and the deletion of a batch
// is what makes the next selection return the next one - so the loop makes progress without
// pagination, which is exactly what a client-side page-and-delete loop cannot do while the store
// is changing underneath it.

// MemoryIdsMatching returns the ids of the memories matching the filter, ordered by id and bounded
// by filter.Limit (which must be positive - an unbounded id list over a large store is precisely
// what the batching exists to avoid).
//
// It is the selection half of a predicate delete and reads nothing but the id column, so it never
// touches a memory body. Ordering by id is not a caller-visible ordering: it makes each batch
// deterministic and keeps the ids in the same order every locking statement in this package uses.
func (d *DB) MemoryIdsMatching(ctx context.Context, filter MemoryFilter) ([]string, error) {
	log.Trace("func() db.MemoryIdsMatching")

	where, args := d.memoryFilterConditions(filter)

	// The significance-carrying view is only needed when the filter actually selects on
	// significance, exactly as CountMemoriesFiltered decides it.
	from := `memories`

	if filterNeedsSignificance(filter) {
		from = memoriesFrom
	}

	query := `SELECT id FROM ` + from + where + ` ORDER BY id`

	if filter.Limit > 0 {
		query += ` LIMIT ?`

		args = append(args, filter.Limit)
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanIds(rows)
}

// EventIdsForMemories returns the distinct, non-empty event ids the given memories belong to.
//
// It exists for the delete-by-filter path's delete_empty_events option, and must be called BEFORE
// the memories are deleted: their event_id is only readable while the rows are still there. The
// empty event id is dropped rather than returned, since "no event" is not an event to consider
// deleting.
func (d *DB) EventIdsForMemories(ctx context.Context, ids []string) ([]string, error) {
	log.Trace("func() db.EventIdsForMemories")

	if len(ids) == 0 {
		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	seen := make(map[string]struct{}, len(ids))

	var out []string

	// Chunked on the same bound every other IN (...) statement here uses, so a large batch cannot
	// exceed a driver's parameter limit.
	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}

		rows, err := d.query(
			ctx,
			`SELECT DISTINCT event_id FROM memories WHERE id IN (`+placeholders(len(chunk))+`) AND event_id <> ''`,
			args...,
		)
		if err != nil {
			return nil, err
		}

		found, err := scanIds(rows)

		_ = rows.Close()

		if err != nil {
			return nil, err
		}

		for _, id := range found {
			if _, ok := seen[id]; ok {
				continue
			}

			seen[id] = struct{}{}
			out = append(out, id)
		}
	}

	return out, nil
}

// EventIdsMatching is MemoryIdsMatching's counterpart for events, on the same terms.
func (d *DB) EventIdsMatching(ctx context.Context, filter EventFilter) ([]string, error) {
	log.Trace("func() db.EventIdsMatching")

	where, args := d.eventFilterConditions(filter)

	from := `events`

	if eventFilterNeedsSignificance(filter) {
		from = eventsFrom
	}

	query := `SELECT id FROM ` + from + where + ` ORDER BY id`

	if filter.Limit > 0 {
		query += ` LIMIT ?`

		args = append(args, filter.Limit)
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanIds(rows)
}

// DeleteEvents deletes the given events in chunked IN (...) batches inside a single transaction,
// returning the number of rows deleted. It is DeleteEvent's batch form and does exactly what that
// function does per row - capture the callback delivery before the delete, prune the link graph
// after it - rather than a loop over it, which would be a transaction and a callback delivery per
// event.
//
// It does NOT touch the events' memories. Every caller decides that first (delete them, or clear
// their event_id) and reaches here with the events already empty, which is also why there is no
// emptiness guard: DeleteEventIfEmpty exists for the scans that decide emptiness from a snapshot,
// and a caller that has just emptied these events itself is not in that position.
func (d *DB) DeleteEvents(ctx context.Context, ids []string) (int, error) {
	log.Trace("func() db.DeleteEvents")

	var deleted int

	err := d.withTxRetry(ctx, "delete events by id", func() error {
		var attemptErr error

		deleted, attemptErr = d.deleteEventsOnce(ctx, ids)

		return attemptErr
	})

	return deleted, err
}

// deleteEventsOnce is one attempt at the transaction above; it owns its transaction from BEGIN to
// COMMIT so a caller can replay it, and must not be called directly.
func (d *DB) deleteEventsOnce(ctx context.Context, ids []string) (int, error) {
	cnt := 0

	if len(ids) == 0 {
		return 0, nil
	}

	tx, cancel, err := d.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	ids = lockOrderedIDs(ids)

	// Captured before the delete, because the columns a callback carries are only readable while
	// the rows still exist. A no-op unless callbacks.allDeletions is on, this being an explicit
	// request rather than decay - the same call DeleteEvent makes.
	captured, err := d.captureEventCallback(tx, ids, CauseClient)
	if err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}

		res, err := tx.Exec(d.rebind(`DELETE FROM events WHERE id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			_ = tx.Rollback()

			return 0, err
		}

		if n, err := res.RowsAffected(); err == nil {
			cnt += int(n)
		}
	}

	// Inside the same transaction, so the link rows and the aggregate the consolidation scans read
	// never disagree with the events that are actually there.
	if err := d.pruneEventLinks(tx, ids); err != nil {
		_ = tx.Rollback()

		return 0, err
	}

	if cnt > 0 {
		if err := d.queueEventCallbacks(tx, captured, CauseClient, 0); err != nil {
			_ = tx.Rollback()

			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}

	return cnt, nil
}
