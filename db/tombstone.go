package db

import (
	"context"
	"database/sql"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// The forgotten log (tombstones).
//
// A memory the sleep cycle forgets is simply gone: nothing records that it existed, when it went,
// or which of the two deletion paths took it. This file is the optional record of that - one row
// per forgotten memory, carrying the decision the cycle made and the numbers behind it, but never
// the body. A tombstone says something was forgotten; it is deliberately not an undelete.
//
// Four decisions shape it.
//
//  1. It records FORGETTING, not deletion. The write hook sits inside
//     deleteMemoriesIfUnrecalled, which is the single chokepoint for consolidation, eviction and
//     Clear - so the caller names the rule, and Clear (which is data movement, not forgetting)
//     names none and writes nothing. Deletions a client asked for by id (DeleteMemories,
//     DeleteEventMemories, ReplaceMemoriesWithSummary) never reach that chokepoint and are not
//     recorded either: the client already knows what it deleted.
//
//  2. The log must not eat the store it lives in. An unbounded table inside the same database is
//     itself subject to capacity accounting, so it would consume the headroom that drives
//     forgetting and make eviction chase its own log. Two things prevent that: PruneTombstones
//     bounds it (row cap and/or age cap) at the end of every sleep cycle, and UsedBytes excludes
//     its estimated size on SQLite - the server drivers already count only live memory/event/link
//     rows, so they exclude it for free.
//
//  3. Disabling the feature deletes nothing. The policy gates the WRITE only; pruning is gated on
//     it too, so turning tombstones off leaves whatever was already recorded in place to be read
//     and then removed deliberately, via DeleteForgottenMemories. A configuration change must
//     never destroy a record somebody kept.
//
//  4. Recording is part of the delete transaction. A failure therefore fails that batch rather
//     than forgetting silently - the memories survive to the next cycle, which is the only
//     outcome consistent with having asked for a record in the first place. (It could not be
//     best-effort in any case: on Postgres a failed statement aborts the whole transaction.)
//
// Tombstones are per MEMORY. An event deleted because its last memory went is implied by its
// memories' rows, and an empty event consolidated on its own is not recorded at all.

// tombstonesTable is the forgotten log's table name.
const tombstonesTable = "memory_tombstones"

// dayInNanoseconds is the age cap's unit. It is the same number as hippocampus.DAY_IN_NANOSECONDS,
// declared again here because the storage layer cannot import the service that sits above it.
const dayInNanoseconds int64 = 86400 * 1000000000

// tombstoneRowBytes is the flat allowance UsedBytes charges each tombstone when excluding the log
// from the store's measured size on SQLite (see tombstoneBytes). It is an estimate rather than a
// measurement on purpose: the alternative is summing the stored string lengths, which is a scan of
// the whole log on every reading - a cost item 25.9 is the standing reminder about - to refine a
// figure that is only ever subtracted from a page-count approximation anyway. It covers the two
// ids, the group, the numeric columns and the two index entries.
const tombstoneRowBytes = 192

// tombstoneChunkSize caps how many tombstones are written in one INSERT. It matches
// deleteChunkSize because the two run in lockstep - one insert per delete chunk - and the column
// count keeps the bound parameters well inside every driver's limit.
const tombstoneChunkSize = deleteChunkSize

// TombstonePolicy is the forgotten log's configuration, set once at startup via SetTombstonePolicy
// (all viper reads stay in main.go). The zero value records nothing, which is the default: the log
// costs storage and a write per forgotten memory, so it is opt-in.
//
// MaxRows and MaxAgeInDays bound the log independently - a row exceeding either is pruned - and a
// non-positive value disables that bound. Both disabled means an unbounded log, which is supported
// but warned about at startup, since it is the shape that eats the store.
type TombstonePolicy struct {
	Enabled      bool
	MaxRows      int
	MaxAgeInDays int
}

// SetTombstonePolicy installs the forgotten log's policy. Called once at startup from main, before
// the server begins serving, so it needs no lock - the same treatment SetCompression gets.
func (d *DB) SetTombstonePolicy(policy TombstonePolicy) {
	d.tombstones = policy
}

// ForgottenMemory is one tombstone: a memory that was forgotten, and the decision that took it.
// Value and Threshold are the two sides of the comparison as they stood at that moment - the
// threshold moves with capacity pressure, so recording it is what keeps Value interpretable later.
// There is no body: see the file comment.
type ForgottenMemory struct {
	Seq          int64
	Id           string
	EventId      string
	Group        string
	Significance int32
	Value        float64
	Threshold    float64
	Bytes        int64
	Rule         ForgetRule
	TimeStamp    int64
	TimeRecalled int64
	RecallCount  int32
	ForgottenAt  int64
}

// ForgottenFilter selects rows from the forgotten log. Every field is optional; the zero value
// asks for the whole log, newest first, bounded by Limit.
//
// Groups is the caller's group scope, which is separate from Group (a filter the caller asked
// for): an empty Groups means unrestricted, exactly as everywhere else in the package.
type ForgottenFilter struct {
	MemoryId string
	EventId  string
	Group    string
	Rule     ForgetRule
	Since    int64
	Until    int64
	AfterSeq int64
	Limit    int
	Groups   []string
}

// forgottenDefaultLimit and forgottenMaxLimit bound one page of the forgotten log, mirroring the
// preview's sample bounds.
const (
	forgottenDefaultLimit = 100
	forgottenMaxLimit     = 1000
)

// ForgottenLimit normalises a requested page size: non-positive selects the default, anything
// above the cap is clamped down to it. Exported so the RPC layer and the query agree on what a
// request resolves to.
func ForgottenLimit(requested int) int {
	if requested <= 0 {
		return forgottenDefaultLimit
	}

	if requested > forgottenMaxLimit {
		return forgottenMaxLimit
	}

	return requested
}

// forgetReason is why a batch of memories is being deleted. It travels into
// deleteMemoriesIfUnrecalled so the chokepoint can record the decision without knowing which pass
// called it; the zero value (ForgetRuleNone) means "not forgetting", and writes no tombstones.
type forgetReason struct {
	rule      ForgetRule
	threshold float64
}

// recording reports whether a batch carrying this reason writes to the forgotten log. The passes
// consult it to decide whether to compute each selected memory's value at all, so a store with the
// log off pays for none of this beyond the check itself.
func (r forgetReason) recording() bool {
	return r.rule != ForgetRuleNone
}

// forgetReasonFor is how a consolidation or eviction pass names why it is deleting. It returns the
// zero reason - which records nothing - whenever the log is off, so the gate is applied once per
// pass rather than once per row.
func (d *DB) forgetReasonFor(rule ForgetRule, s Server) forgetReason {
	if !d.tombstones.Enabled || !d.tombstoneTable {
		return forgetReason{}
	}

	return forgetReason{rule: rule, threshold: s.DeletionThreshold()}
}

// tombstoneDDL is the CREATE TABLE for the forgotten log in the active dialect.
//
// seq is a surrogate monotonic key rather than the memory's id, for two reasons: an id can be
// stored, forgotten, stored again and forgotten again - two genuinely distinct events, which a
// primary key on id would collapse into one (and would need a three-dialect upsert to do it) - and
// a monotonic key gives the row cap an exact cutoff and the reader stable keyset pagination, both
// of which forgotten_at cannot, since a whole batch shares one timestamp.
func (d *DB) tombstoneDDL() string {
	switch d.driver {

	case driverPostgres:
		return `CREATE TABLE IF NOT EXISTS ` + tombstonesTable + ` (
			seq            BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
			id             TEXT NOT NULL,
			event_id       TEXT NOT NULL DEFAULT '',
			group_name     TEXT NOT NULL DEFAULT '',
			significance   INTEGER NOT NULL DEFAULT 0,
			value          DOUBLE PRECISION NOT NULL DEFAULT 0,
			threshold      DOUBLE PRECISION NOT NULL DEFAULT 0,
			body_bytes     BIGINT NOT NULL DEFAULT 0,
			rule           INTEGER NOT NULL DEFAULT 0,
			timestamp      BIGINT NOT NULL DEFAULT 0,
			time_recalled  BIGINT NOT NULL DEFAULT 0,
			recall_count   INTEGER NOT NULL DEFAULT 0,
			forgotten_at   BIGINT NOT NULL DEFAULT 0
		)`

	case driverMySQL:
		// The id/event_id/group_name collation matches the memories table's, so a tombstone is
		// looked up and scoped byte-for-byte the way the memory it records was.
		return `CREATE TABLE IF NOT EXISTS ` + tombstonesTable + ` (
			seq            BIGINT AUTO_INCREMENT PRIMARY KEY,
			id             VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
			event_id       VARCHAR(255) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
			group_name     VARCHAR(255) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
			significance   INTEGER NOT NULL DEFAULT 0,
			value          DOUBLE NOT NULL DEFAULT 0,
			threshold      DOUBLE NOT NULL DEFAULT 0,
			body_bytes     BIGINT NOT NULL DEFAULT 0,
			rule           INTEGER NOT NULL DEFAULT 0,
			timestamp      BIGINT NOT NULL DEFAULT 0,
			time_recalled  BIGINT NOT NULL DEFAULT 0,
			recall_count   INTEGER NOT NULL DEFAULT 0,
			forgotten_at   BIGINT NOT NULL DEFAULT 0
		)`

	default:
		return `CREATE TABLE IF NOT EXISTS ` + tombstonesTable + ` (
			seq            INTEGER PRIMARY KEY AUTOINCREMENT,
			id             TEXT NOT NULL,
			event_id       TEXT NOT NULL DEFAULT '',
			group_name     TEXT NOT NULL DEFAULT '',
			significance   INTEGER NOT NULL DEFAULT 0,
			value          REAL NOT NULL DEFAULT 0,
			threshold      REAL NOT NULL DEFAULT 0,
			body_bytes     INTEGER NOT NULL DEFAULT 0,
			rule           INTEGER NOT NULL DEFAULT 0,
			timestamp      INTEGER NOT NULL DEFAULT 0,
			time_recalled  INTEGER NOT NULL DEFAULT 0,
			recall_count   INTEGER NOT NULL DEFAULT 0,
			forgotten_at   INTEGER NOT NULL DEFAULT 0
		)`
	}
}

// initTombstones creates the forgotten log's table and indexes, idempotently. The table is created
// whether or not the policy enables it, so enabling the feature on a running deployment needs no
// migration step and disabling it leaves the recorded rows readable.
//
// Two indexes: forgotten_at for the age cap's cutoff and the "what went in this window" query, and
// id for "did this memory exist, and when did it go" - the question the log answers that nothing
// else can.
func (d *DB) initTombstones() error {
	log.Trace("func() db.initTombstones")

	if _, err := d.sql.Exec(d.tombstoneDDL()); err != nil {
		log.Errorf("failed to create the forgotten log table: %s", err.Error())

		return err
	}

	indexes := []struct {
		name    string
		columns string
	}{
		{"idx_" + tombstonesTable + "_forgotten_at", "(forgotten_at)"},
		{"idx_" + tombstonesTable + "_id", "(id)"},
	}

	for _, index := range indexes {
		definition := `CREATE INDEX ` + index.name + ` ON ` + tombstonesTable + ` ` + index.columns

		if d.driver == driverMySQL {
			if err := d.createMySQLIndexIfMissing(tombstonesTable, index.name, definition); err != nil {
				return err
			}

			continue
		}

		if _, err := d.sql.Exec(`CREATE INDEX IF NOT EXISTS ` + index.name + ` ON ` + tombstonesTable + ` ` + index.columns); err != nil {
			log.Errorf("failed to create forgotten log index '%s': %s", index.name, err.Error())

			return err
		}
	}

	d.tombstoneTable = true

	return nil
}

// tombstoneRow is one captured memory, read from the row before it is deleted.
type tombstoneRow struct {
	id           string
	eventId      string
	group        string
	significance int32
	value        float64
	bytes        int64
	timeStamp    int64
	timeRecalled int64
	recallCount  int32
}

// recordTombstones captures the rows a delete chunk is about to remove. It runs INSIDE the delete's
// transaction and BEFORE the delete, because the columns a tombstone carries - group, event,
// significance rank, stored size - are only readable while the row still exists.
//
// It reads them from the row rather than from the scan that selected it deliberately: the two
// consolidation scans stay on the covering index precisely because they never read group_name or
// length(body), and making them do so to feed an optional log would slow every cycle on every
// deployment. This touches only the handful of rows being deleted, by primary key.
//
// The capture is a superset - a memory recalled between this and the delete is spared by the
// delete's own guard - so writeTombstones is given the ids that actually went and writes only
// those. Value comes from the snapshot, being the number the pass computed to select the row.
func (d *DB) recordTombstones(tx *sql.Tx, chunk []memoryRecallSnapshot) (map[string]tombstoneRow, error) {
	ids := make([]any, 0, len(chunk))
	values := make(map[string]float64, len(chunk))

	for _, item := range chunk {
		ids = append(ids, item.id)
		values[item.id] = item.value
	}

	// The significance registry is joined rather than snapshotted in Go: the rank is what a reader
	// of the log wants (the visible significance), and it must be frozen at this moment because a
	// later compaction renumbers the registry underneath the id.
	rows, err := tx.Query(
		d.rebind(
			`SELECT m.id, m.event_id, m.group_name, COALESCE(l.level_rank, 0),
				length(m.body) + `+d.metadataBytesExpr("m.")+`,
				m.timestamp, m.time_recalled, m.recall_count
			FROM memories m LEFT JOIN significance_levels l ON l.id = m.significance_level_id
			WHERE m.id IN (`+placeholders(len(ids))+`)`,
		),
		ids...,
	)
	if err != nil {
		log.Errorf("failed to capture tombstones: %s", err.Error())

		return nil, err
	}
	defer func() { _ = rows.Close() }()

	captured := make(map[string]tombstoneRow, len(chunk))

	for rows.Next() {
		var row tombstoneRow

		if err := rows.Scan(
			&row.id,
			&row.eventId,
			&row.group,
			&row.significance,
			&row.bytes,
			&row.timeStamp,
			&row.timeRecalled,
			&row.recallCount,
		); err != nil {
			log.Errorf("failed to scan a tombstone: %s", err.Error())

			return nil, err
		}

		row.value = values[row.id]
		captured[row.id] = row
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to capture tombstones: %s", err.Error())

		return nil, err
	}

	return captured, nil
}

// writeTombstones inserts one row per id that was actually deleted, in the same transaction as the
// delete - so the log and the store can never disagree about what went.
func (d *DB) writeTombstones(
	tx *sql.Tx,
	captured map[string]tombstoneRow,
	deletedIds []string,
	reason forgetReason,
) error {
	if len(captured) == 0 || len(deletedIds) == 0 {
		return nil
	}

	forgottenAt := time.Now().UnixNano()

	for start := 0; start < len(deletedIds); start += tombstoneChunkSize {
		end := min(start+tombstoneChunkSize, len(deletedIds))

		tuples := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*12)

		for _, id := range deletedIds[start:end] {
			row, ok := captured[id]
			if !ok {
				continue
			}

			tuples = append(tuples, `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			args = append(args,
				row.id,
				row.eventId,
				row.group,
				row.significance,
				row.value,
				reason.threshold,
				row.bytes,
				int(reason.rule),
				row.timeStamp,
				row.timeRecalled,
				row.recallCount,
				forgottenAt,
			)
		}

		if len(tuples) == 0 {
			continue
		}

		if _, err := tx.Exec(
			d.rebind(
				`INSERT INTO `+tombstonesTable+` (
					id, event_id, group_name, significance, value, threshold, body_bytes, rule,
					timestamp, time_recalled, recall_count, forgotten_at
				) VALUES `+strings.Join(tuples, ", "),
			),
			args...,
		); err != nil {
			log.Errorf("failed to write tombstones: %s", err.Error())

			return err
		}
	}

	return nil
}

// GetForgottenMemories reads one page of the forgotten log, newest first. It never returns a body:
// a tombstone is a record that something was forgotten, not a copy of it.
func (d *DB) GetForgottenMemories(ctx context.Context, filter ForgottenFilter) ([]ForgottenMemory, error) {
	log.Trace("func() db.GetForgottenMemories")

	query := `SELECT seq, id, event_id, group_name, significance, value, threshold, body_bytes,
		rule, timestamp, time_recalled, recall_count, forgotten_at
		FROM ` + tombstonesTable + ` WHERE 1 = 1`

	var args []any

	if filter.MemoryId != "" {
		query += ` AND id = ?`
		args = append(args, filter.MemoryId)
	}

	if filter.EventId != "" {
		query += ` AND event_id = ?`
		args = append(args, filter.EventId)
	}

	if filter.Group != "" {
		query += ` AND group_name = ?`
		args = append(args, filter.Group)
	}

	if filter.Rule != ForgetRuleNone {
		query += ` AND rule = ?`
		args = append(args, int(filter.Rule))
	}

	if filter.Since > 0 {
		query += ` AND forgotten_at >= ?`
		args = append(args, filter.Since)
	}

	if filter.Until > 0 {
		query += ` AND forgotten_at < ?`
		args = append(args, filter.Until)
	}

	// Keyset pagination on the surrogate key: the caller passes back the lowest seq it has seen and
	// gets the next page below it. forgotten_at could not do this - a whole batch shares one.
	if filter.AfterSeq > 0 {
		query += ` AND seq < ?`
		args = append(args, filter.AfterSeq)
	}

	query, args = appendGroupScope(query, args, "", filter.Groups)

	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, ForgottenLimit(filter.Limit))

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		log.Errorf("failed to read the forgotten log: %s", err.Error())

		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var forgotten []ForgottenMemory

	for rows.Next() {
		var f ForgottenMemory
		var rule int

		if err := rows.Scan(
			&f.Seq,
			&f.Id,
			&f.EventId,
			&f.Group,
			&f.Significance,
			&f.Value,
			&f.Threshold,
			&f.Bytes,
			&rule,
			&f.TimeStamp,
			&f.TimeRecalled,
			&f.RecallCount,
			&f.ForgottenAt,
		); err != nil {
			log.Errorf("failed to scan a forgotten memory: %s", err.Error())

			return nil, err
		}

		f.Rule = ForgetRule(rule)
		forgotten = append(forgotten, f)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read the forgotten log: %s", err.Error())

		return nil, err
	}

	return forgotten, nil
}

// CountForgottenMemories reports how many tombstones the log holds within the caller's scope. It
// backs the console's "showing N of M" and the log-size gauge; an empty scope counts the lot.
func (d *DB) CountForgottenMemories(ctx context.Context, groups []string) (int64, error) {
	log.Trace("func() db.CountForgottenMemories")

	query := `SELECT COUNT(*) FROM ` + tombstonesTable + ` WHERE 1 = 1`

	query, args := appendGroupScope(query, nil, "", groups)

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var count int64

	if err := d.queryRow(ctx, query, args...).Scan(&count); err != nil {
		log.Errorf("failed to count the forgotten log: %s", err.Error())

		return 0, err
	}

	return count, nil
}

// DeleteForgottenMemories is the manual cleanup: it deletes tombstones older than before (UnixNano),
// or the whole log when before is 0, within the caller's group scope. Returns how many went.
//
// It is manual on purpose. The automatic bounds (PruneTombstones) only ever apply the caps the
// operator configured while the feature is on; turning the feature OFF must not destroy what was
// already recorded, so emptying the log is always somebody's explicit decision.
func (d *DB) DeleteForgottenMemories(ctx context.Context, before int64, groups []string) (int64, error) {
	log.Trace("func() db.DeleteForgottenMemories")

	query := `DELETE FROM ` + tombstonesTable + ` WHERE 1 = 1`

	var args []any

	if before > 0 {
		query += ` AND forgotten_at < ?`
		args = append(args, before)
	}

	query, args = appendGroupScope(query, args, "", groups)

	result, err := d.exec(ctx, query, args...)
	if err != nil {
		log.Errorf("failed to delete from the forgotten log: %s", err.Error())

		return 0, err
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		log.Errorf("failed to count deleted tombstones: %s", err.Error())

		return 0, err
	}

	return deleted, nil
}

// PruneTombstones applies the configured bounds to the forgotten log, and is called at the end of
// every sleep cycle. It returns the number of rows removed.
//
// It is a no-op while the feature is disabled - including when the log still holds rows written
// while it was on. That is the difference between the caps and the cleanup: the caps are what an
// operator asked to be enforced on a log being written, whereas a log nobody is writing to is
// simply a record, and records are removed on purpose (DeleteForgottenMemories), not as a side
// effect of a configuration change.
//
// The row cap resolves to a seq cutoff read separately rather than as a subquery on the table
// being deleted from, which MySQL forbids outright.
func (d *DB) PruneTombstones(ctx context.Context) (int64, error) {
	log.Trace("func() db.PruneTombstones")

	if !d.tombstones.Enabled || !d.tombstoneTable {
		return 0, nil
	}

	var pruned int64

	if d.tombstones.MaxAgeInDays > 0 {
		cutoff := time.Now().UnixNano() - int64(d.tombstones.MaxAgeInDays)*dayInNanoseconds

		removed, err := d.DeleteForgottenMemories(ctx, cutoff, nil)
		if err != nil {
			return pruned, err
		}

		pruned += removed
	}

	if d.tombstones.MaxRows <= 0 {
		return pruned, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// The seq of the oldest row worth keeping: skip MaxRows-1 rows from the newest and take the
	// next. No row means the log is already inside the cap.
	var cutoff int64

	err := d.queryRow(
		ctx,
		`SELECT seq FROM `+tombstonesTable+` ORDER BY seq DESC LIMIT 1 OFFSET ?`,
		d.tombstones.MaxRows-1,
	).Scan(&cutoff)

	if err == sql.ErrNoRows {
		return pruned, nil
	}

	if err != nil {
		log.Errorf("failed to find the forgotten log's row cap cutoff: %s", err.Error())

		return pruned, err
	}

	result, err := d.exec(ctx, `DELETE FROM `+tombstonesTable+` WHERE seq < ?`, cutoff)
	if err != nil {
		log.Errorf("failed to prune the forgotten log to its row cap: %s", err.Error())

		return pruned, err
	}

	removed, err := result.RowsAffected()
	if err != nil {
		log.Errorf("failed to count pruned tombstones: %s", err.Error())

		return pruned, err
	}

	return pruned + removed, nil
}

// tombstoneBytes estimates what the forgotten log occupies, so UsedBytes can exclude it on SQLite.
//
// The exclusion is the point: SQLite's UsedBytes is page accounting over the whole database file,
// so without this the log would count toward the capacity target, raise capacity pressure, and
// make the store evict live memories to make room for the record of memories it already evicted.
// The server drivers need no equivalent - usedBytesLiveRows counts the memories, events and link
// rows explicitly, so the log is outside it already.
//
// It is a count multiplied by a flat allowance rather than a sum of the stored lengths: see
// tombstoneRowBytes.
func (d *DB) tombstoneBytes(ctx context.Context) int64 {
	if !d.tombstoneTable {
		return 0
	}

	var count int64

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+tombstonesTable).Scan(&count); err != nil {
		log.Warnf("failed to measure the forgotten log, counting it as stored bytes: %s", err.Error())

		return 0
	}

	return count * tombstoneRowBytes
}
