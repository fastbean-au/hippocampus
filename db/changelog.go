package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// The change log (TODO-3 item 204): the store's changes, durably, so the Watch RPC can resume from a
// cursor and any instance sharing the store can serve every instance's changes.
//
// Item 193 made Watch live and best-effort, fed in process from the deliveries each write and
// delete already builds (watch.go). That feed has two gaps this closes. A client that reconnects
// cannot ask for what it missed, and on PostgreSQL or MySQL a watcher sees only what its own
// instance commits - forgetting on the consolidating instance, a write on whichever replica served
// it. Both need the changes written somewhere every instance can read, which is a row per change.
//
// Five decisions shape it.
//
//  1. It is OPT-IN (watch.changeLog.enabled). A row per write is a real cost on the write path,
//     and most deployments - one instance, watchers that re-read after a gap - do not need it. Off,
//     nothing here runs and the in-process feed is exactly what it was.
//
//  2. A row is one DELIVERY, not one item: what the in-process feed publishes as one event is one
//     row, so a deletion of five hundred memories is a single insert. The payload is the delivery's
//     items, bodies stripped, as JSON - the feed never carries a body, so the log never holds one,
//     and what it does hold (ids, groups, significance ranks, sizes) is what the memories table
//     holds in plaintext beside it. It is not sealed under storage.encryption for that reason.
//
//  3. It is written in the SAME transaction as the change, through recordChanges, so a change that
//     commits always has its row and one that rolls back never does. The completion of a sleep
//     cycle has no transaction to join and takes its own (AppendChange), the boundary
//     QueueCallbacks draws for the same delivery.
//
//  4. It lives BESIDE the capacity target, like the forgotten log: excluded from UsedBytes, bounded
//     by its own row, byte and age caps, which the consolidating instance applies at the end of
//     every cycle (PruneChangeLog). A log inside the target would make every write evict.
//
//  5. Instances fan out by POLLING it (hippocampus/watchlog.go), not by LISTEN/NOTIFY. Polling is
//     one indexed range read per interval, the same on all three dialects, and it is what any
//     notification scheme would fall back to on a reconnect anyway. NOTIFY would cut the latency
//     on Postgres alone; it can be added under the same cursor without changing anything a client
//     sees.
//
// seq is the cursor, and it is only a cursor once the reader allows for commit order. Sequence
// numbers are handed out at insert and become visible at commit, so on a server dialect seq 7 can
// be readable while seq 6, taken by a slower transaction, is not yet - and a reader that advanced
// past 7 would never see 6. The reader in the service holds a gap open for a grace period before
// concluding that the missing number was rolled back (see hippocampus/watchlog.go); nothing here
// needs to know about it, except that ReadChanges takes an upper bound so a resuming watcher reads
// only what that reader has already settled. SQLite has one writer and no such gaps.

// changeLogTable is the change log's table name.
const changeLogTable = "change_log"

// changeLogRowOverheadBytes is the allowance charged per row beyond its payload, which is measured
// at insert as the callback queue's is: six numeric columns, the row header, and the recorded_at
// index entry. Measured on SQLite against rows of eight items, where it is within a few bytes of the
// pages the rows actually took (TestTheChangeLogIsExcludedFromUsedBytes).
const changeLogRowOverheadBytes = 80

// ChangeLogPolicy is the change log's configuration, set once at startup via SetChangeLogPolicy (all
// viper reads stay in main). The zero value records nothing, which is the default.
//
// The three bounds are independent and any may be zero, meaning unbounded on that side; all three
// zero is an unbounded log, which is warned about at startup.
type ChangeLogPolicy struct {
	Enabled  bool
	MaxRows  int64
	MaxBytes int64
	MaxAge   time.Duration
}

// Bounds projects the policy onto the shape the prune and the report are written against.
func (p ChangeLogPolicy) Bounds() QueueBounds {
	return QueueBounds{
		MaxAge:   p.MaxAge,
		MaxRows:  p.MaxRows,
		MaxBytes: p.MaxBytes,
	}
}

// SetChangeLogPolicy installs the change log's policy. Called once at startup, before serving, so it
// needs no lock - the treatment SetTombstonePolicy gets.
func (d *DB) SetChangeLogPolicy(policy ChangeLogPolicy) {
	d.changeLog = policy
}

// ChangeLogging reports whether this store records its changes into the change log. Every instance
// sharing a store must agree on it, since an instance that does not record leaves its writes out of
// every other instance's feed.
func (d *DB) ChangeLogging() bool {
	return d.changeLog.Enabled && d.changeLogTable
}

// Change is one row of the change log: one delivery, as it committed.
//
// Report is opaque here. It is the finished cycle's report for a SLEEP_COMPLETED change, encoded by
// the service, which owns that shape; the storage layer only keeps it.
type Change struct {
	Seq        int64
	Kind       CallbackKind
	Cause      DeleteCause
	CycleId    int64
	RecordedAt int64
	Items      []CallbackItem
	Report     []byte
}

// changePayload is what a row's payload column decodes to.
type changePayload struct {
	Items  []CallbackItem `json:"items,omitempty"`
	Report []byte         `json:"report,omitempty"`
}

// changeLogDDL is the CREATE TABLE for the change log in the active dialect.
func (d *DB) changeLogDDL() string {
	dialect := d.dialect()

	return `CREATE TABLE IF NOT EXISTS ` + changeLogTable + ` (
		seq           ` + dialect.autoIncrementPK + `,
		kind          INTEGER NOT NULL DEFAULT 0,
		cause         INTEGER NOT NULL DEFAULT 0,
		cycle_id      ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		recorded_at   ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		payload       ` + dialect.blobType + `,
		payload_bytes ` + dialect.bigintType + ` NOT NULL DEFAULT 0
	)`
}

// initChangeLog is migration 20: the change log's table and its age index, idempotently. Created
// whether or not the policy enables it, as the forgotten log is, so turning it on needs no migration
// and turning it off leaves the rows readable. seq is the primary key, which is every read but the
// age cap's.
func (d *DB) initChangeLog() error {
	log.Trace("func() db.initChangeLog")

	if _, err := d.sql.Exec(d.changeLogDDL()); err != nil {
		log.Errorf("failed to create the change log table: %s", err.Error())

		return err
	}

	if err := d.ensureIndex(changeLogTable, "idx_"+changeLogTable+"_recorded_at", `(recorded_at)`); err != nil {

		return err
	}

	d.changeLogTable = true

	return nil
}

// changeExecer is what a change is inserted through: the caller's transaction, or the pool.
type changeExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// logChanges records deliveries into the change log inside the caller's transaction. A failure
// fails the change, for the reason queueCallbacks gives: on a server dialect a failed statement
// aborts the transaction anyway, and a change committed without its row is a gap in every watcher's
// view that nothing would ever report.
func (d *DB) logChanges(tx *sql.Tx, deliveries []CallbackDelivery) error {
	now := time.Now().UnixNano()

	for _, delivery := range withoutBodies(deliveries) {
		change := Change{
			Kind:       delivery.Kind,
			Cause:      delivery.Cause,
			CycleId:    delivery.CycleId,
			RecordedAt: now,
			Items:      delivery.Payload.Items,
		}

		if err := d.insertChange(context.Background(), tx, change); err != nil {

			return err
		}
	}

	return nil
}

// insertChange writes one row.
func (d *DB) insertChange(ctx context.Context, exec changeExecer, change Change) error {
	payload, err := json.Marshal(changePayload{Items: change.Items, Report: change.Report})
	if err != nil {

		return fmt.Errorf("encoding a change log payload: %w", err)
	}

	if _, err := exec.ExecContext(
		ctx,
		d.rebind(`INSERT INTO `+changeLogTable+` (kind, cause, cycle_id, recorded_at, payload, payload_bytes) VALUES (?, ?, ?, ?, ?, ?)`),
		int(change.Kind),
		int(change.Cause),
		change.CycleId,
		change.RecordedAt,
		payload,
		int64(len(payload)),
	); err != nil {
		log.Errorf("failed to record a change: %s", err.Error())

		return err
	}

	return nil
}

// AppendChange records a change that has no transaction to join: a sleep cycle's completion. A
// no-op while the log is off.
func (d *DB) AppendChange(ctx context.Context, change Change) error {
	log.Trace("func() db.AppendChange")

	if !d.ChangeLogging() {

		return nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if change.RecordedAt == 0 {
		change.RecordedAt = time.Now().UnixNano()
	}

	return d.insertChange(ctx, d.sql, change)
}

// ReadChanges returns the changes after afterSeq, oldest first, at most limit of them. A positive
// throughSeq is an inclusive upper bound, which is how a resuming watcher reads only what the
// service's reader has settled (see the file comment); zero or less is unbounded.
func (d *DB) ReadChanges(ctx context.Context, afterSeq int64, throughSeq int64, limit int) ([]Change, error) {
	if !d.changeLogTable {

		return nil, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	query := `SELECT seq, kind, cause, cycle_id, recorded_at, payload FROM ` + changeLogTable + ` WHERE seq > ?`
	args := []any{afterSeq}

	if throughSeq > 0 {
		query += ` AND seq <= ?`
		args = append(args, throughSeq)
	}

	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		log.Errorf("failed to read the change log: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []Change

	for rows.Next() {
		var (
			change  Change
			kind    int
			cause   int
			payload []byte
		)

		if err := rows.Scan(&change.Seq, &kind, &cause, &change.CycleId, &change.RecordedAt, &payload); err != nil {

			return nil, err
		}

		change.Kind = CallbackKind(kind)
		change.Cause = DeleteCause(cause)

		var decoded changePayload

		// A row that cannot be decoded is still a change, and is handed on with no items rather
		// than stopping the feed at it: a reader that stopped would hold every later change back.
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &decoded); err != nil {
				log.Warnf("change log row %d could not be decoded, passing it on empty: %s", change.Seq, err.Error())
			}
		}

		change.Items = decoded.Items
		change.Report = decoded.Report

		out = append(out, change)
	}

	return out, rows.Err()
}

// ChangeLogHead is the newest seq in the log, or 0 when it is empty.
func (d *DB) ChangeLogHead(ctx context.Context) (int64, error) {
	if !d.changeLogTable {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var head sql.NullInt64

	if err := d.queryRow(ctx, `SELECT MAX(seq) FROM `+changeLogTable).Scan(&head); err != nil {
		log.Errorf("failed to read the change log's head: %s", err.Error())

		return 0, err
	}

	return head.Int64, nil
}

// HasChange reports whether the log still holds the row at seq. It is how a resume is judged: a
// cursor is a seq the client was handed, so the row existed, and if it is gone the caps have
// removed it - along with an unknown number of the changes after it.
func (d *DB) HasChange(ctx context.Context, seq int64) (bool, error) {
	if !d.changeLogTable {

		return false, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var found int

	err := d.queryRow(ctx, `SELECT 1 FROM `+changeLogTable+` WHERE seq = ?`, seq).Scan(&found)

	if err == sql.ErrNoRows {

		return false, nil
	}

	if err != nil {
		log.Errorf("failed to look up change %d: %s", seq, err.Error())

		return false, err
	}

	return true, nil
}

// PruneChangeLog applies the policy's caps and reports how many rows went. A no-op while the log is
// off, for the reason PruneTombstones is: disabling a record must not delete it.
//
// The row and byte caps are the callback queue's statements with nothing retained, and they share
// its shape for its reasons: a derived table so MySQL accepts the subquery, and a running total over
// payload_bytes so the byte cap bounds exactly the figure AncillaryStorage reports.
func (d *DB) PruneChangeLog(ctx context.Context) (int64, error) {
	log.Trace("func() db.PruneChangeLog")

	if !d.ChangeLogging() {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	bounds := d.changeLog.Bounds()

	var pruned int64

	if bounds.MaxAge > 0 {
		res, err := d.exec(ctx, `DELETE FROM `+changeLogTable+` WHERE recorded_at < ?`, time.Now().Add(-bounds.MaxAge).UnixNano())
		if err != nil {
			log.Errorf("failed to prune the change log by age: %s", err.Error())

			return pruned, err
		}

		if n, err := res.RowsAffected(); err == nil {
			pruned += n
		}
	}

	if bounds.MaxRows > 0 {
		res, err := d.exec(
			ctx,
			`DELETE FROM `+changeLogTable+` WHERE seq <= (
				SELECT MIN(seq) FROM (
					SELECT seq FROM `+changeLogTable+` ORDER BY seq DESC LIMIT ?
				) AS keep
			) - 1`,
			bounds.MaxRows,
		)
		if err != nil {
			log.Errorf("failed to prune the change log by row count: %s", err.Error())

			return pruned, err
		}

		if n, err := res.RowsAffected(); err == nil {
			pruned += n
		}
	}

	if bounds.MaxBytes <= 0 {

		return pruned, nil
	}

	var cutoff sql.NullInt64

	if err := d.queryRow(
		ctx,
		`SELECT MIN(seq) FROM (
			SELECT seq, SUM(payload_bytes + ?) OVER (ORDER BY seq DESC) AS held
			FROM `+changeLogTable+`
		) AS totals WHERE held <= ?`,
		int64(changeLogRowOverheadBytes),
		bounds.MaxBytes,
	).Scan(&cutoff); err != nil {
		log.Errorf("failed to find the change log's byte cap cutoff: %s", err.Error())

		return pruned, err
	}

	// No cutoff is an empty log, or a newest row larger than the whole cap on its own, which is kept
	// for the reason pruneCallbackQueueBytes keeps one: a cap that discarded every change the moment
	// it was written would be a feed that silently carries nothing.
	if !cutoff.Valid {
		if err := d.queryRow(ctx, `SELECT MAX(seq) FROM `+changeLogTable).Scan(&cutoff); err != nil {
			log.Errorf("failed to read the change log's newest row: %s", err.Error())

			return pruned, err
		}

		if !cutoff.Valid {

			return pruned, nil
		}
	}

	res, err := d.exec(ctx, `DELETE FROM `+changeLogTable+` WHERE seq < ?`, cutoff.Int64)
	if err != nil {
		log.Errorf("failed to prune the change log to its byte cap: %s", err.Error())

		return pruned, err
	}

	if n, err := res.RowsAffected(); err == nil {
		pruned += n
	}

	return pruned, nil
}

// changeLogProbe is the change log's half of AncillaryStorage and of the UsedBytes exclusion.
func (d *DB) changeLogProbe() ancillaryProbe {
	return ancillaryProbe{
		exists:        d.changeLogTable,
		recording:     d.ChangeLogging(),
		table:         changeLogTable,
		rowBytes:      changeLogRowOverheadBytes,
		payloadColumn: payloadBytesColumn,
		ageColumn:     "recorded_at",
	}
}

// changeLogBytes is what UsedBytes subtracts for the change log on SQLite. See tombstoneBytes.
func (d *DB) changeLogBytes(ctx context.Context) int64 {
	return d.excludedBytes(ctx, d.changeLogProbe())
}
