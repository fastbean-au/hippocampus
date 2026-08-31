package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// searchOutboxTable is the transactional outbox: one row per memory whose search-index document
// still needs deleting.
const searchOutboxTable = "search_outbox"

// outboxClaimLimit bounds one drain pass. Small enough that a pass is quick and a failure re-does
// little, large enough that a backlog drains at a useful rate.
const outboxClaimLimit = 500

// The outbox exists because the OpenSearch backend is the only one whose deletes can be LOST.
//
// Propagation to OpenSearch is an asynchronous, bounded, best-effort queue: when it is full the
// operation is discarded at the enqueue boundary. That is survivable for an index operation - the
// memory still exists, so the reconciliation sweep re-indexes it - and unsurvivable for a delete,
// because nothing afterwards knows the document should have gone. Under any sustained write rate
// above the queue's drain rate the index therefore accumulates stale documents forever; measured on
// a live deployment, twenty-one documents for every row the store actually held.
//
// A larger queue does not fix it. Any queue is finite, and a write rate above the drain rate
// exhausts it; the loss is at the enqueue boundary, so durability of the queue's CONTENTS would not
// help either - the dropped operation never entered it.
//
// So the delete is recorded in the SAME TRANSACTION as the memory delete, and a worker drains the
// record afterwards. There is no boundary at which it can be dropped, and a crash between the delete
// and its propagation replays on restart rather than losing it. Backpressure becomes table growth,
// which is visible and bounded by policy, instead of silent divergence that only a manual reindex
// can repair.
//
// This is not a novel guarantee for the product: the SQLite FTS backend has always had it, because
// its deletes are an AFTER DELETE trigger and so are transactional by construction. The outbox
// brings OpenSearch up to what the built-in backend already provides.
//
// Only deletes. An index operation's loss is self-correcting, and a row per memory WRITE would put
// real cost on the write path for no gain.

// searchOutboxDDL is the CREATE TABLE for the outbox in the active dialect.
//
// seq is a surrogate monotonic key rather than the memory id, for the same reason the forgotten log
// uses one: an id can be stored, deleted, stored again and deleted again, and a primary key on id
// would collapse two genuinely distinct deletions into one. It also gives the drain stable keyset
// ordering, so a pass always makes progress.
func (d *DB) searchOutboxDDL() string {
	dialect := d.dialect()

	// The id takes the dialect's id type, so the document deleted is keyed exactly as the memory
	// that asked for it was.
	return `CREATE TABLE IF NOT EXISTS ` + searchOutboxTable + ` (
		seq        ` + dialect.autoIncrementPK + `,
		id         ` + dialect.idType + ` NOT NULL,
		queued_at  ` + dialect.bigintType + ` NOT NULL DEFAULT 0
	)`
}

// initSearchOutbox creates the outbox table and its index, idempotently.
//
// Created whether or not an OpenSearch backend is configured, for the same reason the forgotten log
// is: enabling the backend on a running deployment then needs no migration step, and rows already
// queued stay drainable if it is turned off and on again.
func (d *DB) initSearchOutbox() error {
	log.Trace("func() db.initSearchOutbox")

	if _, err := d.sql.Exec(d.searchOutboxDDL()); err != nil {
		log.Errorf("failed to create the search outbox table: %s", err.Error())

		return err
	}

	// queued_at carries the age cap's cutoff. seq is already the primary key, so the drain's
	// ordering needs no index of its own.
	if err := d.ensureIndex(searchOutboxTable, "idx_"+searchOutboxTable+"_queued_at", `(queued_at)`); err != nil {
		return err
	}

	return nil
}

// queueSearchDeletes records, inside the caller's transaction, that these ids' documents must be
// removed from the search index.
//
// Being in the caller's transaction is the whole point: it commits with the delete or not at all, so
// the outbox can never claim a memory went that is still there, and a crash can never lose a delete
// that did.
//
// A failure here FAILS the delete, deliberately. The alternative - swallowing it - is what the
// in-memory queue already does, and reintroduces exactly the silent divergence this exists to
// prevent. On Postgres it could not be best-effort anyway: a failed statement aborts the whole
// transaction.
func (d *DB) queueSearchDeletes(tx *sql.Tx, ids []string) error {
	if !d.searchOutbox || len(ids) == 0 {

		return nil
	}

	now := time.Now().UnixNano()

	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))
		chunk := ids[start:end]

		query := `INSERT INTO ` + searchOutboxTable + ` (id, queued_at) VALUES `
		args := make([]any, 0, len(chunk)*2)

		for i, id := range chunk {
			if i > 0 {
				query += ", "
			}

			query += "(?, ?)"

			args = append(args, id, now)
		}

		if _, err := tx.Exec(d.rebind(query), args...); err != nil {
			log.Errorf("failed to queue search deletes: %s", err.Error())

			return err
		}
	}

	return nil
}

// SearchOutboxEntry is one queued deletion.
type SearchOutboxEntry struct {
	Seq int64
	ID  string
}

// ClaimSearchDeletes reads the oldest queued deletions, without removing them.
//
// Read-then-confirm rather than delete-then-apply: a row is only removed once the index has actually
// accepted the deletion (ConfirmSearchDeletes), so a crash mid-pass replays the work instead of
// losing it. Re-applying is harmless - deleting a document that is already gone is a no-op - which
// is what makes at-least-once the right delivery guarantee here.
func (d *DB) ClaimSearchDeletes(ctx context.Context, limit int) ([]SearchOutboxEntry, error) {
	log.Trace("func() db.ClaimSearchDeletes")

	if !d.searchOutbox {

		return nil, nil
	}

	if limit <= 0 {
		limit = outboxClaimLimit
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT seq, id FROM `+searchOutboxTable+` ORDER BY seq LIMIT ?`,
		limit,
	)
	if err != nil {
		log.Errorf("failed to claim search deletes: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []SearchOutboxEntry

	for rows.Next() {
		var v SearchOutboxEntry

		if err := rows.Scan(&v.Seq, &v.ID); err != nil {

			return nil, err
		}

		out = append(out, v)
	}

	return out, rows.Err()
}

// ConfirmSearchDeletes removes rows whose deletions the index has accepted.
func (d *DB) ConfirmSearchDeletes(ctx context.Context, seqs []int64) error {
	log.Trace("func() db.ConfirmSearchDeletes")

	if len(seqs) == 0 {

		return nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	for start := 0; start < len(seqs); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(seqs))
		chunk := seqs[start:end]

		args := make([]any, len(chunk))

		for i, v := range chunk {
			args[i] = v
		}

		if _, err := d.exec(
			ctx,
			`DELETE FROM `+searchOutboxTable+` WHERE seq IN (`+placeholders(len(chunk))+`)`,
			args...,
		); err != nil {
			log.Errorf("failed to confirm search deletes: %s", err.Error())

			return err
		}
	}

	return nil
}

// PruneSearchOutbox drops rows older than maxAge, and trims the oldest beyond maxRows.
//
// The outbox must not be able to eat the store it lives in. An index that is unreachable for a long
// time would otherwise grow it without bound - so the policy is that a delete which has waited this
// long is abandoned to the reconciliation sweep, which removes stale documents whatever put them
// there. That is the sweep's whole purpose: it is the backstop, and this is the case it backs.
func (d *DB) PruneSearchOutbox(ctx context.Context, maxAge time.Duration, maxRows int64) (int64, error) {
	log.Trace("func() db.PruneSearchOutbox")

	if !d.searchOutbox {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var pruned int64

	if maxAge > 0 {
		res, err := d.exec(
			ctx,
			`DELETE FROM `+searchOutboxTable+` WHERE queued_at < ?`,
			time.Now().Add(-maxAge).UnixNano(),
		)
		if err != nil {
			log.Errorf("failed to prune the search outbox by age: %s", err.Error())

			return pruned, err
		}

		if n, err := res.RowsAffected(); err == nil {
			pruned += n
		}
	}

	if maxRows > 0 {
		// Keep the NEWEST maxRows: an older queued delete has had longer for the sweep to have
		// noticed it anyway.
		res, err := d.exec(
			ctx,
			`DELETE FROM `+searchOutboxTable+` WHERE seq <= (
				SELECT MIN(seq) FROM (
					SELECT seq FROM `+searchOutboxTable+` ORDER BY seq DESC LIMIT ?
				) AS keep
			)  - 1`,
			maxRows,
		)
		if err != nil {
			log.Errorf("failed to prune the search outbox by row count: %s", err.Error())

			return pruned, err
		}

		if n, err := res.RowsAffected(); err == nil {
			pruned += n
		}
	}

	return pruned, nil
}

// SearchOutboxDepth is how many deletions are waiting, for the metric and for tests.
//
// Gated like the rest: a store not using the outbox reports nothing waiting, which is the truth, and
// the read-only opens (which skip initSchema) never query a table they cannot be sure exists.
func (d *DB) SearchOutboxDepth(ctx context.Context) (int64, error) {
	if !d.searchOutbox {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var n int64

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+searchOutboxTable).Scan(&n); err != nil {

		return 0, fmt.Errorf("counting the search outbox: %w", err)
	}

	return n, nil
}

// outboxRowBytes is the flat per-row allowance searchOutboxBytes charges the queue, in the mould of
// tombstoneRowBytes: an id, a timestamp, a surrogate key and their page overhead. A rough figure is
// the right kind of figure here - it is subtracted from a whole-file measurement to keep the queue
// from influencing eviction, not reported to anybody.
const outboxRowBytes = 96

// searchOutboxBytes estimates what the queue occupies, for UsedBytes to subtract on SQLite.
//
// A count times an allowance rather than a measurement, for the reason the forgotten log's
// equivalent gives: this runs inside the capacity check on every sleep cycle, and a scan there would
// put the cost of the queue on the path that exists to bound the store.
func (d *DB) searchOutboxBytes(ctx context.Context) int64 {
	if !d.searchOutbox {

		return 0
	}

	var count int64

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+searchOutboxTable).Scan(&count); err != nil {
		log.Warnf("failed to measure the search outbox, counting it as stored bytes: %s", err.Error())

		return 0
	}

	return count * outboxRowBytes
}

// SetSearchOutbox enables the transactional outbox for search-index deletions.
//
// Called once at startup from main, before the server begins serving, and only when the active
// search backend actually needs draining - which today means OpenSearch. It is deliberately not
// derived inside the db package: the storage layer knows nothing about which backend is configured,
// and the two that do not need it need it for different reasons (the SQLite FTS index deletes
// transactionally through a trigger; the no-op backend holds nothing to delete).
//
// Off, nothing is queued and nothing is drained - so enabling it on an existing store starts
// recording from that point, and disabling it stops both the recording and the trimming. Whatever
// either transition leaves behind is the reconciliation sweep's to find, which is exactly why that
// sweep is the backstop rather than the mechanism.
func (d *DB) SetSearchOutbox(enabled bool) {
	log.Tracef("func() db.SetSearchOutbox(%t)", enabled)

	d.searchOutbox = enabled
}
