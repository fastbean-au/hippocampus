package db

import (
	"context"
	"database/sql"
	"time"

	log "github.com/sirupsen/logrus"
)

// The registry's own forgetting.
//
// significance_levels gains a row for every distinct significance value ever written and, until
// this file existed, lost one only to a Purge. That makes it the single thing in this store that
// grows with the store's HISTORY rather than with its contents: a producer writing varied
// significance leaves a level behind for every value it ever used, long after the last memory
// carrying that value has been forgotten. One live deployment held 29,001 rows against 211,657
// memories, and nothing would ever have removed one of them (TODO-2 item 128).
//
// So the sleep cycle reaps levels nothing refers to any more. Three things carry it.
//
// FIRST, and it is the whole reason this is not a one-line DELETE: a level is handed out BEFORE
// anything references it. ResolveSignificanceLevel's fast path is a lock-free read that returns an
// existing level's id, and the memory carrying that id is inserted afterwards - so a reap that
// deleted every unreferenced level would, sooner or later, delete one in that gap. Nothing would
// fail. There is no foreign key, so the INSERT succeeds, and the memory is stored pointing at a
// level that no longer exists - which every read resolves to rank 0 (rankOf, and the LEFT JOIN in
// memoriesFrom). A memory would silently lose its significance and be forgotten early, in a store
// whose entire job is to decide what to forget by significance.
//
// The mark is what closes that. A level is not deleted on the cycle that finds it unused: that
// cycle stamps unused_since on it, and only a LATER cycle, finding it still unreferenced and marked
// for longer than the grace, deletes it. A hand-out of a marked level clears the mark and checks
// that it actually cleared something (findLevel), so:
//
//   - a hand-out of an unmarked level is a hand-out of a level no reap can touch for a full grace
//     period, because a reap must mark it first and a mark is not old enough to act on;
//   - a hand-out of a marked level either wins the race - the mark is cleared, and the reap's
//     DELETE carries `unused_since < cutoff` in its own predicate, so it matches nothing - or loses
//     it, and the UPDATE reports no row changed, which sends the writer to the slow path to create
//     the level again.
//
// Either way no write ever stores an id that is not there. The grace is days and the window
// between a hand-out and its INSERT is bounded by storage.queryTimeoutSeconds, so the margin is
// not a close-run thing; the mark is what makes the argument exact rather than probable.
//
// SECOND, the grace is not only race insurance - it is the answer to the one thing item 128 warned
// about. The registry is the scale SignificancePlacement positions against and GetSignificanceLevels
// reports, so a value that is unused today is not necessarily meaningless, and reaping it changes
// the answer a client gets. consolidation.significanceLevels.unusedRetentionInDays is how long a
// level survives having nothing left that carries it; 0 keeps the old behaviour, which is that the
// registry remembers every value forever.
//
// THIRD, it is measured rather than probed. There is no index ON memories.significance_level_id -
// the covering index leads on event_id - so asking "does anything reference this level" per level
// would be a table scan per level. The referenced set is instead collected in ONE pass per table,
// and the mark and the delete then name ids. That also means the reference check is read-then-act,
// which is safe for exactly the reason above: the delete's own predicate is the mark, not the
// reference.
//
// One pass is still a pass over the memories table, and that is the cost this carries. It is
// affordable because the column is IN the covering index, so the scan need not touch a body - the
// embedded dialect walks it as a covering-index scan - and because the cycle it runs in already
// scans that table once per pass to decide what to consolidate.
//
// Best-effort at the RPC layer, like the forgotten log's prune: a registry that could not be
// trimmed is tidiness, not a reason to report the cycle as failed.

// significanceLevelUnusedColumn records when a reap first found a level with nothing carrying it.
// NULL means in use, or not yet known to be unused - the two are deliberately the same state, since
// a hand-out clears the mark without knowing which one it was restoring.
const significanceLevelUnusedColumn = "unused_since"

// initSignificanceLevelMark adds that column to a registry created before it existed. The CREATE
// TABLE carries it for a new store; this is the other half, and it is a no-op on every startup
// after the first.
func (d *DB) initSignificanceLevelMark() error {
	log.Trace("func() db.initSignificanceLevelMark")

	return d.addColumnIfMissing(significanceLevelsTable, significanceLevelUnusedColumn, d.dialect().bigintType)
}

// SignificanceRegistry is the registry's size and what the last pass did to it.
//
// Levels is reported whether or not anything is being reaped, and that is the point of reporting it
// at all: a deployment that has turned the reap off is exactly the one whose registry grows without
// bound, and before this there was no figure anywhere that said so. Reaped is how many rows the
// pass removed, and is 0 both when there was nothing to remove and when the reap is disabled - the
// two are told apart by whether Levels is going anywhere.
type SignificanceRegistry struct {
	Levels int64
	Reaped int64
}

// ReapSignificanceLevels removes registry levels that nothing refers to and that have carried the
// unused mark for longer than grace, marks the ones that have just become unused, and reports what
// the registry is left holding.
//
// A non-positive grace disables the reaping - no reference scan, no mark, no delete - and leaves
// only the count, so a deployment that wants the registry to remember every value it has ever seen
// pays one aggregate a cycle for the option.
//
// It runs under the registry lock, so it cannot interleave with a gap-open, a compaction or an
// import's find-or-create. The lock-free hand-out path is the one thing it does race, and the mark
// is what makes that safe; see the file comment.
func (d *DB) ReapSignificanceLevels(ctx context.Context, grace time.Duration) (SignificanceRegistry, error) {
	log.Trace("func() db.ReapSignificanceLevels")

	if grace <= 0 {
		return d.countSignificanceLevels(ctx)
	}

	release, err := d.acquireRegistryLock(ctx)
	if err != nil {
		return SignificanceRegistry{}, err
	}

	defer release()

	levels, err := d.significanceLevelMarks(ctx)
	if err != nil {
		return SignificanceRegistry{}, err
	}

	if len(levels) == 0 {
		return SignificanceRegistry{}, nil
	}

	referenced, err := d.referencedSignificanceLevels(ctx)
	if err != nil {
		return SignificanceRegistry{}, err
	}

	now := time.Now().UnixNano()
	cutoff := now - grace.Nanoseconds()

	var (
		reapable  []int64
		unused    []int64
		inUse     []int64
		markValue = sql.NullInt64{Int64: now, Valid: true}
	)

	for id, since := range levels {
		switch {

		// Marked and carrying something again. The hand-out paths clear the mark themselves, so
		// this is normally empty; it is here because a reference landing between the two reads
		// below would otherwise leave a level marked while in use, and its grace would then be
		// spent rather than restarted when it next falls idle.
		case referenced[id] && since.Valid:
			inUse = append(inUse, id)

		case referenced[id]:
			// In use and unmarked: the ordinary state, and nothing to write.

		case !since.Valid:
			unused = append(unused, id)

		case since.Int64 < cutoff:
			reapable = append(reapable, id)

		}
	}

	// The report is built before the error is checked, and from the count that was actually
	// deleted, because deleteSignificanceLevels chunks: a failure part way through has still removed
	// rows, and the caller publishes a gauge off this. Reporting the pre-delete size there would
	// leave a registry looking larger than it is until the next cycle.
	deleted, err := d.deleteSignificanceLevels(ctx, reapable, cutoff)

	registry := SignificanceRegistry{Levels: int64(len(levels)) - deleted, Reaped: deleted}

	if err != nil {
		return registry, err
	}

	if err := d.markSignificanceLevels(ctx, inUse, sql.NullInt64{}); err != nil {
		return registry, err
	}

	if err := d.markSignificanceLevels(ctx, unused, markValue); err != nil {
		return registry, err
	}

	return registry, nil
}

// countSignificanceLevels reports the registry's size alone, for a deployment that has turned the
// reaping off. One aggregate over a table with one row per distinct significance value, which is
// the cheapest thing in the cycle.
func (d *DB) countSignificanceLevels(ctx context.Context) (SignificanceRegistry, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var levels int64

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+significanceLevelsTable).Scan(&levels); err != nil {
		log.Errorf("failed to count the significance registry: %s", err.Error())

		return SignificanceRegistry{}, err
	}

	return SignificanceRegistry{Levels: levels}, nil
}

// significanceLevelMarks reads the whole registry as id -> unused_since. The registry is one row
// per distinct significance value, which is the small table this file exists to keep small, so it
// is read entire rather than filtered.
func (d *DB) significanceLevelMarks(ctx context.Context) (map[int64]sql.NullInt64, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, `SELECT id, `+significanceLevelUnusedColumn+` FROM `+significanceLevelsTable)
	if err != nil {
		log.Errorf("failed to read the significance registry: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	marks := make(map[int64]sql.NullInt64)

	for rows.Next() {
		var (
			id    int64
			since sql.NullInt64
		)

		if err := rows.Scan(&id, &since); err != nil {
			log.Errorf("failed to scan a significance level: %s", err.Error())

			return nil, err
		}

		marks[id] = since
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read the significance registry: %s", err.Error())

		return nil, err
	}

	return marks, nil
}

// referencedSignificanceLevels collects the level ids the two item tables actually carry.
//
// One DISTINCT pass per table rather than a correlated existence check per level, because there is
// no index on either significance_level_id column: the memories one is at least available from the
// covering index, which carries it, and the events one is over a far smaller table. Both are the
// same order of cost as the scans the cycle already runs, and they run once between them rather
// than once per statement below.
func (d *DB) referencedSignificanceLevels(ctx context.Context) (map[int64]bool, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	referenced := make(map[int64]bool)

	for _, table := range []string{"memories", "events"} {
		rows, err := d.query(
			ctx,
			`SELECT DISTINCT significance_level_id FROM `+table+` WHERE significance_level_id IS NOT NULL`,
		)
		if err != nil {
			log.Errorf("failed to read the significance levels %s refer to: %s", table, err.Error())

			return nil, err
		}

		for rows.Next() {
			var id int64

			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()

				log.Errorf("failed to scan a referenced significance level: %s", err.Error())

				return nil, err
			}

			referenced[id] = true
		}

		if err := rows.Err(); err != nil {
			_ = rows.Close()

			log.Errorf("failed to read the significance levels %s refer to: %s", table, err.Error())

			return nil, err
		}

		_ = rows.Close()
	}

	return referenced, nil
}

// deleteSignificanceLevels removes the named levels, and carries the mark's cutoff in its own
// predicate.
//
// That predicate is not a belt-and-braces re-check of what the caller already decided: it is the
// whole race argument. A hand-out that clears a level's mark between this function's caller reading
// it and this statement running makes the level unmatched here, so the writer's id stays valid. See
// the file comment.
func (d *DB) deleteSignificanceLevels(ctx context.Context, ids []int64, cutoff int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	var deleted int64

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk)+1)
		for _, id := range chunk {
			args = append(args, id)
		}

		args = append(args, cutoff)

		result, err := d.exec(
			ctx,
			`DELETE FROM `+significanceLevelsTable+
				` WHERE id IN (`+placeholders(len(chunk))+`)`+
				` AND `+significanceLevelUnusedColumn+` IS NOT NULL`+
				` AND `+significanceLevelUnusedColumn+` < ?`,
			args...,
		)
		if err != nil {
			log.Errorf("failed to reap significance levels: %s", err.Error())

			return deleted, err
		}

		affected, err := result.RowsAffected()
		if err != nil {
			log.Errorf("failed to count the significance levels reaped: %s", err.Error())

			return deleted, err
		}

		deleted += affected
	}

	return deleted, nil
}

// markSignificanceLevels writes the unused mark on the named levels - a timestamp to start their
// grace, or the invalid (NULL) value to end it.
func (d *DB) markSignificanceLevels(ctx context.Context, ids []int64, since sql.NullInt64) error {
	if len(ids) == 0 {
		return nil
	}

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, since)

		for _, id := range chunk {
			args = append(args, id)
		}

		if _, err := d.exec(
			ctx,
			`UPDATE `+significanceLevelsTable+` SET `+significanceLevelUnusedColumn+` = ?`+
				` WHERE id IN (`+placeholders(len(chunk))+`)`,
			args...,
		); err != nil {
			log.Errorf("failed to mark significance levels: %s", err.Error())

			return err
		}
	}

	return nil
}

// claimSignificanceLevel clears a level's unused mark on the way to handing it out, and reports
// whether the level was still there to clear it on.
//
// It is the writer's half of the race the file comment describes, and the return value is the part
// that matters: RowsAffected of 0 means a reap took the level first (or another writer claimed it
// in the same instant), and the caller must not hand that id to a write. A level that was not
// marked at all never reaches here - the ordinary hand-out is a read and nothing else.
func (d *DB) claimSignificanceLevel(ctx context.Context, id int64) (bool, error) {
	result, err := d.exec(
		ctx,
		`UPDATE `+significanceLevelsTable+` SET `+significanceLevelUnusedColumn+` = NULL`+
			` WHERE id = ? AND `+significanceLevelUnusedColumn+` IS NOT NULL`,
		id,
	)
	if err != nil {
		log.Errorf("failed to claim significance level %d: %s", id, err.Error())

		return false, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		log.Errorf("failed to confirm the claim of significance level %d: %s", id, err.Error())

		return false, err
	}

	return affected > 0, nil
}
