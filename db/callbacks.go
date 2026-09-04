package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// callbackQueueTable is the persisted callback queue: one row per delivery still waiting to be
// sent to whatever the deployment has configured to receive it.
const callbackQueueTable = "callback_queue"

// callbackClaimLimit bounds one dispatch pass when the caller names no limit. Small enough that a
// pass is quick and a failure re-does little, large enough that a backlog drains at a useful rate.
const callbackClaimLimit = 100

// callbackChunkSize bounds one multi-row INSERT of deliveries, matching the delete path's chunking.
const callbackChunkSize = deleteChunkSize

// The callback queue exists for the reason the search outbox does, arrived at from the other side.
//
// A callback about a DELETION cannot be reconstructed after the fact. Every other thing the service
// tells the outside world can be re-derived by asking again: a memory that failed to index is still
// in the store for the reconcile sweep to find, a stale search document is found by the reverse
// sweep. But once a memory is gone, the only record that it ever existed is one somebody chose to
// keep - and a notification dropped at an enqueue boundary is a fact about the store that nothing
// can recover. An in-memory queue is therefore not an option at any size, exactly as db/outbox.go
// argues for index deletes.
//
// So the delivery is recorded in the SAME TRANSACTION as the deletion that produced it, and a
// worker drains it afterwards. A crash between the delete and the delivery replays on restart. A
// receiver that is down for an hour is an hour of table growth, which is visible, bounded by policy
// and self-correcting, rather than an hour of silent loss.
//
// Two things are deliberately different from the outbox.
//
// A row is a BATCH, not an item. The outbox queues one id per row because a document delete is one
// operation; a callback is an HTTP request, and one request per forgotten memory would turn a cycle
// that forgets ten thousand into ten thousand POSTs. So the payload is a rendered list, and the
// chunking that produces it is the same mechanism the sleep-cycle delivery needs to bound its own
// id list.
//
// And a row carries RETRY STATE (attempts, next_attempt_at). The outbox needs none: its sink is a
// cluster the same operator runs, and a failed pass simply retries on the next tick. A callback
// receiver belongs to somebody else, and hammering an endpoint that is refusing every second is
// both rude and useless. Backoff lives on the row rather than in the worker so it survives a
// restart, which is when a receiver's outage is most likely to still be in progress.

// CallbackKind names what a delivery is about. It is stored as an integer so the column is cheap and
// the ordering stable; the wire spelling belongs to the notify package.
type CallbackKind int

const (
	// CallbackKindNone is the zero value and is never stored.
	CallbackKindNone CallbackKind = iota

	// CallbackKindMemoryForgotten reports memories that have been deleted.
	CallbackKindMemoryForgotten

	// CallbackKindEventForgotten reports events that have been deleted.
	CallbackKindEventForgotten

	// CallbackKindSleepCompleted reports that a sleep cycle has finished.
	CallbackKindSleepCompleted
)

// DeleteCause names why records were deleted.
//
// Deliberately NOT an extension of ForgetRule. That enum belongs to the forgotten log, is projected
// onto the wire by GetForgottenMemories, and means "which decay rule took this" - a question that
// has no answer for a client-initiated delete. Widening it to carry causes that are not decay at
// all would make the forgotten log's own filter answer a different question than it does today.
type DeleteCause int

const (
	// CauseNone is the zero value: a deletion that raises no callback.
	CauseNone DeleteCause = iota

	// CauseConsolidation is the value-based sleep-cycle pass.
	CauseConsolidation

	// CauseEviction is the capacity-pressure pass.
	CauseEviction

	// CauseClient is an explicit delete from a caller.
	CauseClient

	// CauseClear is the second half of an Export/Transfer move.
	CauseClear

	// CauseCascade is a memory going because its event did, or an event because its last memory did.
	CauseCascade

	// CauseSummaryReplace is memories replaced by a summary memory.
	CauseSummaryReplace

	// CausePurge is Purge - everything, at an operator's explicit request.
	CausePurge
)

// decay reports whether a cause is one of the two forgetting passes. It is what
// callbacks.allDeletions widens past: off, only a decay cause enqueues.
func (c DeleteCause) decay() bool {
	return c == CauseConsolidation || c == CauseEviction
}

// CallbackPolicy is how the store is told whether to record deliveries, and what to put in them.
// Read from viper in main.go and applied once at startup, like TombstonePolicy beside it.
type CallbackPolicy struct {
	// Enabled gates the recording. It is set only when a sink is configured AND something will
	// drain the queue - the outbox's lesson, which the drain worker applies: a store queueing
	// deliveries nothing sends is strictly worse than one queueing none.
	Enabled bool

	// AllDeletions widens the feed from the two decay passes to every deletion, each delivery
	// carrying its cause.
	AllDeletions bool

	// IncludeBodies adds the memory body to each item. It costs a body read per deleted memory and
	// real space in the queue, so it is off by default.
	IncludeBodies bool

	// MaxBodyBytes caps one included body. A body over it is omitted and flagged, never truncated:
	// a receiver cannot tell a truncated body from a whole one, which makes truncation worse than
	// omission. Non-positive means no cap.
	MaxBodyBytes int

	// MemoryEvents and EventEvents select which of the two deletion callbacks are recorded. Both
	// default on where the feature is on at all: an operator who has configured a receiver wants the
	// callbacks, and having to enable each one afterwards would be a second switch for one decision.
	// They exist so a receiver that only cares about one can say so here rather than filter, which
	// is the difference between not writing the rows and writing them to throw away.
	MemoryEvents bool
	EventEvents  bool
}

// wantsKind reports whether this kind of deletion callback is recorded at all.
func (p CallbackPolicy) wantsKind(kind CallbackKind) bool {
	switch kind {

	case CallbackKindMemoryForgotten:
		return p.MemoryEvents

	case CallbackKindEventForgotten:
		return p.EventEvents

	}

	return true
}

// wants reports whether a deletion with this cause should be recorded.
func (p CallbackPolicy) wants(cause DeleteCause) bool {
	if !p.Enabled || cause == CauseNone {
		return false
	}

	return p.AllDeletions || cause.decay()
}

// SetCallbackPolicy applies the callback configuration to the store.
//
// Called once at startup from main, before the server begins serving. Off, nothing is queued and
// nothing is trimmed - so enabling it on an existing store starts recording from that point, and
// disabling it stops the recording without destroying what was already queued. Emptying the queue
// is always the explicit request (DeleteCallbackQueue), for the reason the forgotten log gives.
func (d *DB) SetCallbackPolicy(policy CallbackPolicy) {
	log.Tracef("func() db.SetCallbackPolicy(%t)", policy.Enabled)

	d.callbacks = policy
}

// CallbacksEnabled reports whether deliveries are being recorded. The consolidation passes consult
// it to decide whether to retain the ids they delete, so a store with callbacks off pays nothing.
func (d *DB) CallbacksEnabled() bool {
	return d.callbacks.Enabled && d.callbackTable
}

// CallbackItem is one record a delivery is about, as the store holds it.
type CallbackItem struct {
	Id           string `json:"id"`
	EventId      string `json:"event_id,omitempty"`
	Group        string `json:"group,omitempty"`
	Significance int32  `json:"significance,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Body         string `json:"body,omitempty"`
	BodyOmitted  bool   `json:"body_omitted,omitempty"`
}

// CallbackCycle is the sleep-cycle summary a completion delivery carries.
type CallbackCycle struct {
	Trigger                 string `json:"trigger"`
	StartedAt               int64  `json:"started_at"`
	DurationMillis          int64  `json:"duration_millis"`
	MemoriesConsolidated    int    `json:"memories_consolidated"`
	EventsConsolidated      int    `json:"events_consolidated"`
	MemoriesEvicted         int    `json:"memories_evicted"`
	EventsEvicted           int    `json:"events_evicted"`
	BytesFreed              int64  `json:"bytes_freed"`
	SummarisationCandidates int    `json:"summarisation_candidates"`
	Success                 bool   `json:"success"`
	Failure                 string `json:"failure,omitempty"`
}

// CallbackPayload is what a row's blob decodes to: everything the sink needs that the typed columns
// do not carry. Keeping it as one encoded blob is what lets the queue hold a body-carrying batch
// without the schema growing a column per field the notify package might one day want.
type CallbackPayload struct {
	Items []CallbackItem `json:"items,omitempty"`
	Cycle *CallbackCycle `json:"cycle,omitempty"`
}

// CallbackDelivery is one queued delivery, as claimed by the drain worker or listed by the RPC.
//
// Payload is populated by ClaimCallbacks and left empty by GetCallbackQueue: the listing is an
// operator's view of a backlog, and the payload may carry memory bodies.
type CallbackDelivery struct {
	Seq           int64
	Kind          CallbackKind
	Cause         DeleteCause
	CycleId       int64
	Chunk         int
	Chunks        int
	ItemCount     int
	QueuedAt      int64
	Attempts      int
	NextAttemptAt int64
	Payload       CallbackPayload
}

// callbackQueueDDL is the CREATE TABLE for the callback queue in the active dialect.
//
// seq is a surrogate monotonic key for the same reasons the forgotten log and the outbox have one:
// nothing else here is unique (a cycle produces several deliveries, an id can be forgotten twice),
// it gives the drain stable keyset ordering so a pass always makes progress, and it gives the row
// cap an exact cutoff that queued_at cannot, a whole batch sharing one timestamp.
func (d *DB) callbackQueueDDL() string {
	dialect := d.dialect()

	return `CREATE TABLE IF NOT EXISTS ` + callbackQueueTable + ` (
		seq             ` + dialect.autoIncrementPK + `,
		kind            INTEGER NOT NULL DEFAULT 0,
		cause           INTEGER NOT NULL DEFAULT 0,
		cycle_id        ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		chunk           INTEGER NOT NULL DEFAULT 0,
		chunks          INTEGER NOT NULL DEFAULT 0,
		item_count      INTEGER NOT NULL DEFAULT 0,
		payload         ` + dialect.blobType + `,
		is_compressed   ` + dialect.boolType + ` NOT NULL DEFAULT ` + dialect.boolFalse + `,
		queued_at       ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		attempts        INTEGER NOT NULL DEFAULT 0,
		next_attempt_at ` + dialect.bigintType + ` NOT NULL DEFAULT 0
	)`
}

// initCallbackQueue creates the queue table and its index, idempotently.
//
// Created whether or not a sink is configured, exactly as the outbox and the forgotten log are:
// enabling callbacks on a running deployment then needs no migration step, and anything already
// queued stays drainable if the feature is turned off and on again.
func (d *DB) initCallbackQueue() error {
	log.Trace("func() db.initCallbackQueue")

	if _, err := d.sql.Exec(d.callbackQueueDDL()); err != nil {
		log.Errorf("failed to create the callback queue table: %s", err.Error())

		return err
	}

	// The drain claims on next_attempt_at and the age cap cuts on queued_at; seq is already the
	// primary key, so the ordering needs no index of its own.
	if err := d.ensureIndex(callbackQueueTable, "idx_"+callbackQueueTable+"_next_attempt", `(next_attempt_at)`); err != nil {
		return err
	}

	if err := d.ensureIndex(callbackQueueTable, "idx_"+callbackQueueTable+"_queued_at", `(queued_at)`); err != nil {
		return err
	}

	d.callbackTable = true

	return nil
}

// queueCallbacks records deliveries inside the caller's transaction.
//
// Being in the caller's transaction is the whole point: it commits with the deletion or not at all,
// so the queue can never announce a memory that is still there, and a crash can never lose a
// notification for one that went.
//
// A failure here FAILS the delete, deliberately, for the reason db/outbox.go gives: swallowing it
// reintroduces the silent divergence the queue exists to prevent, and on a server dialect it could
// not be best-effort anyway - a failed statement aborts the whole transaction.
func (d *DB) queueCallbacks(tx *sql.Tx, deliveries []CallbackDelivery) error {
	if !d.callbacks.Enabled || !d.callbackTable || len(deliveries) == 0 {

		return nil
	}

	now := time.Now().UnixNano()

	for start := 0; start < len(deliveries); start += callbackChunkSize {
		end := min(start+callbackChunkSize, len(deliveries))
		chunk := deliveries[start:end]

		query := `INSERT INTO ` + callbackQueueTable + ` (
			kind, cause, cycle_id, chunk, chunks, item_count, payload, is_compressed,
			queued_at, next_attempt_at
		) VALUES `

		args := make([]any, 0, len(chunk)*10)

		for i, delivery := range chunk {
			encoded, compressed, err := encodeCallbackPayload(delivery.Payload)
			if err != nil {

				return err
			}

			if i > 0 {
				query += ", "
			}

			query += "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

			queuedAt := delivery.QueuedAt
			if queuedAt == 0 {
				queuedAt = now
			}

			args = append(args,
				int(delivery.Kind),
				int(delivery.Cause),
				delivery.CycleId,
				delivery.Chunk,
				delivery.Chunks,
				delivery.ItemCount,
				encoded,
				compressed,
				queuedAt,
				// Due immediately: a fresh delivery has no backoff to serve.
				queuedAt,
			)
		}

		if _, err := tx.Exec(d.rebind(query), args...); err != nil {
			log.Errorf("failed to queue callbacks: %s", err.Error())

			return err
		}
	}

	return nil
}

// QueueCallbacks records deliveries outside any caller's transaction, for the events a sleep cycle
// reports about itself rather than about a row it just deleted.
//
// The completion delivery has no transaction to join - there is no single statement it commits
// with - so it takes one of its own. That is the honest boundary: a crash between the last delete
// and this leaves the per-batch deliveries queued and the completion missing, which is a receiver
// seeing what went without being told the cycle ended, rather than the reverse.
func (d *DB) QueueCallbacks(ctx context.Context, deliveries []CallbackDelivery) error {
	log.Trace("func() db.QueueCallbacks")

	if !d.callbacks.Enabled || !d.callbackTable || len(deliveries) == 0 {

		return nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		log.Errorf("failed to begin a callback queue transaction: %s", err.Error())

		return err
	}

	if err := d.queueCallbacks(tx, deliveries); err != nil {
		_ = tx.Rollback()

		return err
	}

	return tx.Commit()
}

// encodeCallbackPayload renders a payload for storage, gzipping it when that actually helps.
//
// The decision is recorded per row rather than taken from the current configuration, exactly as
// memory bodies do it, so a queue written under one setting reads correctly under another - and a
// compression failure stores the payload verbatim rather than failing the delete, since verbatim is
// always a valid representation.
func encodeCallbackPayload(payload CallbackPayload) ([]byte, bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Errorf("failed to encode a callback payload: %s", err.Error())

		return nil, false, err
	}

	packed, err := gzipBytes(raw)
	if err != nil {
		log.Warnf("failed to compress a callback payload, storing it verbatim: %s", err.Error())

		return raw, false, nil
	}

	if len(packed) >= len(raw) {
		return raw, false, nil
	}

	return packed, true, nil
}

// decodeCallbackPayload is the inverse, driven by the row's own flag.
func decodeCallbackPayload(stored []byte, isCompressed bool) (CallbackPayload, error) {
	var payload CallbackPayload

	raw, err := decompressBody(stored, isCompressed)
	if err != nil {
		return payload, fmt.Errorf("decompressing a callback payload: %w", err)
	}

	if raw == "" {
		return payload, nil
	}

	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return payload, fmt.Errorf("decoding a callback payload: %w", err)
	}

	return payload, nil
}

// ClaimCallbacks reads the oldest deliveries that are due, without removing them.
//
// Read-then-confirm rather than delete-then-send: a row is removed only once the receiver has
// actually accepted the delivery (ConfirmCallbacks), so a crash mid-pass replays the work instead
// of losing it. That makes delivery at-least-once, which is the honest guarantee - a receiver must
// be prepared for a repeat, and the cycle id plus the chunk numbering are what let it recognise one.
//
// "Due" is what separates this from the outbox's claim: a delivery that failed carries a backoff
// deadline, and until it passes the row is skipped rather than retried on every pass.
func (d *DB) ClaimCallbacks(ctx context.Context, limit int, now int64) ([]CallbackDelivery, error) {
	log.Trace("func() db.ClaimCallbacks")

	if !d.callbacks.Enabled || !d.callbackTable {

		return nil, nil
	}

	if limit <= 0 {
		limit = callbackClaimLimit
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT seq, kind, cause, cycle_id, chunk, chunks, item_count, payload, is_compressed,
			queued_at, attempts, next_attempt_at
		FROM `+callbackQueueTable+`
		WHERE next_attempt_at <= ?
		ORDER BY seq LIMIT ?`,
		now,
		limit,
	)
	if err != nil {
		log.Errorf("failed to claim callbacks: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []CallbackDelivery

	for rows.Next() {
		var (
			v            CallbackDelivery
			kind         int
			cause        int
			stored       []byte
			isCompressed bool
		)

		if err := rows.Scan(
			&v.Seq,
			&kind,
			&cause,
			&v.CycleId,
			&v.Chunk,
			&v.Chunks,
			&v.ItemCount,
			&stored,
			&isCompressed,
			&v.QueuedAt,
			&v.Attempts,
			&v.NextAttemptAt,
		); err != nil {
			log.Errorf("failed to scan a queued callback: %s", err.Error())

			return nil, err
		}

		payload, err := decodeCallbackPayload(stored, isCompressed)
		if err != nil {
			// A payload that cannot be decoded can never be delivered, so failing the whole pass
			// would wedge the queue behind it forever. It is skipped, logged, and left for the
			// caps to remove - the same treatment a permanently-rejected delivery gets.
			log.Warnf("skipping callback delivery %d: %s", v.Seq, err.Error())

			continue
		}

		v.Kind = CallbackKind(kind)
		v.Cause = DeleteCause(cause)
		v.Payload = payload

		out = append(out, v)
	}

	return out, rows.Err()
}

// ConfirmCallbacks removes rows whose deliveries the receiver has accepted.
func (d *DB) ConfirmCallbacks(ctx context.Context, seqs []int64) error {
	log.Trace("func() db.ConfirmCallbacks")

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
			`DELETE FROM `+callbackQueueTable+` WHERE seq IN (`+placeholders(len(chunk))+`)`,
			args...,
		); err != nil {
			log.Errorf("failed to confirm callbacks: %s", err.Error())

			return err
		}
	}

	return nil
}

// DeferCallbacks records a failed attempt and sets when the delivery may next be tried.
//
// The backoff lives on the row rather than in the worker so that it survives a restart, which is
// precisely when a receiver's outage is most likely to still be in progress: a worker holding it in
// memory would come back up and hammer an endpoint it has already learnt is refusing.
func (d *DB) DeferCallbacks(ctx context.Context, seqs []int64, nextAttemptAt int64) error {
	log.Trace("func() db.DeferCallbacks")

	if len(seqs) == 0 {

		return nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	for start := 0; start < len(seqs); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(seqs))
		chunk := seqs[start:end]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, nextAttemptAt)

		for _, v := range chunk {
			args = append(args, v)
		}

		if _, err := d.exec(
			ctx,
			`UPDATE `+callbackQueueTable+`
			SET attempts = attempts + 1, next_attempt_at = ?
			WHERE seq IN (`+placeholders(len(chunk))+`)`,
			args...,
		); err != nil {
			log.Errorf("failed to defer callbacks: %s", err.Error())

			return err
		}
	}

	return nil
}

// PruneCallbackQueue drops deliveries older than maxAge, and trims the oldest beyond maxRows.
//
// The queue must not be able to eat the store it lives in. A receiver that is unreachable for a long
// time would otherwise grow it without bound - so the policy is that a delivery which has waited
// this long is abandoned. There is no backstop for it, unlike the outbox's sweep, which is why it is
// counted and logged at Warn rather than dropped quietly: an operator whose receiver has been down
// for a day has genuinely lost notifications, and that is a thing to be told.
func (d *DB) PruneCallbackQueue(ctx context.Context, maxAge time.Duration, maxRows int64) (int64, error) {
	log.Trace("func() db.PruneCallbackQueue")

	if !d.callbacks.Enabled || !d.callbackTable {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var pruned int64

	if maxAge > 0 {
		res, err := d.exec(
			ctx,
			`DELETE FROM `+callbackQueueTable+` WHERE queued_at < ?`,
			time.Now().Add(-maxAge).UnixNano(),
		)
		if err != nil {
			log.Errorf("failed to prune the callback queue by age: %s", err.Error())

			return pruned, err
		}

		if n, err := res.RowsAffected(); err == nil {
			pruned += n
		}
	}

	if maxRows > 0 {
		// Keep the NEWEST maxRows. The nested subquery with an alias is not decoration: a dialect
		// that refuses a subquery reading the table it is deleting from accepts it through a
		// derived table, so this one statement serves all three.
		res, err := d.exec(
			ctx,
			`DELETE FROM `+callbackQueueTable+` WHERE seq <= (
				SELECT MIN(seq) FROM (
					SELECT seq FROM `+callbackQueueTable+` ORDER BY seq DESC LIMIT ?
				) AS keep
			)  - 1`,
			maxRows,
		)
		if err != nil {
			log.Errorf("failed to prune the callback queue by row count: %s", err.Error())

			return pruned, err
		}

		if n, err := res.RowsAffected(); err == nil {
			pruned += n
		}
	}

	return pruned, nil
}

// CallbackQueueDepth is how many deliveries are waiting, for the metric and the RPC.
//
// Gated like the rest: a store not using callbacks reports nothing waiting, which is the truth, and
// the read-only opens (which skip initSchema) never query a table they cannot be sure exists.
func (d *DB) CallbackQueueDepth(ctx context.Context) (int64, error) {
	if !d.callbackTable {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var n int64

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+callbackQueueTable).Scan(&n); err != nil {

		return 0, fmt.Errorf("counting the callback queue: %w", err)
	}

	return n, nil
}

// CallbackQueueFilter selects rows from the callback queue for the listing RPC. The zero value asks
// for the newest page.
type CallbackQueueFilter struct {
	Kind     CallbackKind
	AfterSeq int64
	Limit    int
}

// callbackQueueDefaultLimit and callbackQueueMaxLimit bound one page of the listing, mirroring the
// forgotten log's bounds.
const (
	callbackQueueDefaultLimit = 100
	callbackQueueMaxLimit     = 1000
)

// CallbackQueueLimit normalises a requested page size: non-positive selects the default, anything
// above the cap is clamped down to it. Exported so the RPC layer and the query agree on what a
// request resolves to.
func CallbackQueueLimit(requested int) int {
	if requested <= 0 {
		return callbackQueueDefaultLimit
	}

	if requested > callbackQueueMaxLimit {
		return callbackQueueMaxLimit
	}

	return requested
}

// GetCallbackQueue reads one page of the queue, oldest first - the order the drain will send them,
// which is the order an operator looking at a backlog wants.
//
// It never returns a payload. A queued delivery may carry memory bodies, and the listing exists so
// that a purge is not a blind operation, not so that the queue becomes a second way to read the
// store's contents.
func (d *DB) GetCallbackQueue(ctx context.Context, filter CallbackQueueFilter) ([]CallbackDelivery, error) {
	log.Trace("func() db.GetCallbackQueue")

	if !d.callbackTable {

		return nil, nil
	}

	query := `SELECT seq, kind, cause, cycle_id, chunk, chunks, item_count, queued_at, attempts,
		next_attempt_at FROM ` + callbackQueueTable + ` WHERE 1 = 1`

	var args []any

	if filter.Kind != CallbackKindNone {
		query += ` AND kind = ?`
		args = append(args, int(filter.Kind))
	}

	if filter.AfterSeq > 0 {
		query += ` AND seq > ?`
		args = append(args, filter.AfterSeq)
	}

	query += ` ORDER BY seq LIMIT ?`
	args = append(args, CallbackQueueLimit(filter.Limit))

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		log.Errorf("failed to read the callback queue: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []CallbackDelivery

	for rows.Next() {
		var (
			v     CallbackDelivery
			kind  int
			cause int
		)

		if err := rows.Scan(
			&v.Seq,
			&kind,
			&cause,
			&v.CycleId,
			&v.Chunk,
			&v.Chunks,
			&v.ItemCount,
			&v.QueuedAt,
			&v.Attempts,
			&v.NextAttemptAt,
		); err != nil {
			log.Errorf("failed to scan a queued callback: %s", err.Error())

			return nil, err
		}

		v.Kind = CallbackKind(kind)
		v.Cause = DeleteCause(cause)

		out = append(out, v)
	}

	return out, rows.Err()
}

// OldestQueuedCallback returns when the oldest waiting delivery was recorded, or 0 when the queue is
// empty. It is the figure that tells an operator whether a backlog is minutes or days old, which
// the depth alone does not.
func (d *DB) OldestQueuedCallback(ctx context.Context) (int64, error) {
	if !d.callbackTable {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	var oldest sql.NullInt64

	if err := d.queryRow(ctx, `SELECT MIN(queued_at) FROM `+callbackQueueTable).Scan(&oldest); err != nil {

		return 0, fmt.Errorf("reading the oldest queued callback: %w", err)
	}

	if !oldest.Valid {
		return 0, nil
	}

	return oldest.Int64, nil
}

// DeleteCallbackQueue empties the queue, or the part of it queued before a cutoff.
//
// Not gated on the policy, unlike the pruning: the automatic caps only apply while recording is
// enabled, so turning callbacks off deliberately leaves what was already queued in place - and
// removing it then has to be possible, or a configuration change would strand rows nothing can
// reach. This is that request.
func (d *DB) DeleteCallbackQueue(ctx context.Context, before int64) (int64, error) {
	log.Trace("func() db.DeleteCallbackQueue")

	if !d.callbackTable {

		return 0, nil
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	query := `DELETE FROM ` + callbackQueueTable

	var args []any

	if before > 0 {
		query += ` WHERE queued_at < ?`
		args = append(args, before)
	}

	res, err := d.exec(ctx, query, args...)
	if err != nil {
		log.Errorf("failed to delete the callback queue: %s", err.Error())

		return 0, err
	}

	n, err := res.RowsAffected()
	if err != nil {

		return 0, nil
	}

	return n, nil
}

// callbackRowBytes is the flat per-row allowance callbackQueueBytes charges the queue, in the mould
// of tombstoneRowBytes and outboxRowBytes. It is larger than either because a row carries a rendered
// payload rather than an id, and larger again when bodies are included - but a rough figure is the
// right kind of figure here: it is subtracted from a whole-file measurement to keep the queue from
// influencing eviction, not reported to anybody.
const callbackRowBytes = 512

// callbackQueueBytes estimates what the queue occupies, for UsedBytes to subtract on SQLite.
//
// This exclusion is not tidiness, and it matters more here than for either of its predecessors. The
// queue grows PRECISELY when deliveries are backing up - a receiver is down - and it can carry
// memory bodies. Without the exclusion, SQLite's page accounting would count the record of
// notifications that could not be sent as stored bytes, raise capacity pressure, and evict live
// memories to make room for the news that memories were evicted.
//
// A count times an allowance rather than a measurement, for the reason the other two give: this runs
// inside the capacity check on every sleep cycle, and a scan there would put the cost of the queue
// on the path that exists to bound the store.
func (d *DB) callbackQueueBytes(ctx context.Context) int64 {
	if !d.callbackTable {

		return 0
	}

	var count int64

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+callbackQueueTable).Scan(&count); err != nil {
		log.Warnf("failed to measure the callback queue, counting it as stored bytes: %s", err.Error())

		return 0
	}

	return count * callbackRowBytes
}

// memoryDelivery builds one delivery from a capture and the ids that were actually deleted.
//
// Driven by deletedIds rather than by the capture, which is a superset: a memory recalled between
// the capture and the delete is spared by the delete's own guard, and announcing it would tell a
// receiver something untrue about a memory that is still there.
//
// It reports ok false for an empty batch, so a chunk that deleted nothing queues nothing.
func memoryDelivery(captured map[string]tombstoneRow, deletedIds []string, reason forgetReason, cycleId int64) (CallbackDelivery, bool) {
	if len(deletedIds) == 0 {
		return CallbackDelivery{}, false
	}

	items := make([]CallbackItem, 0, len(deletedIds))

	for _, id := range deletedIds {
		row, ok := captured[id]
		if !ok {
			// The capture missed it, which should not happen - but an id with no row is still a
			// memory that went, and the id is the part a receiver cannot do without.
			items = append(items, CallbackItem{Id: id})

			continue
		}

		items = append(items, CallbackItem{
			Id:           row.id,
			EventId:      row.eventId,
			Group:        row.group,
			Significance: row.significance,
			Bytes:        row.bytes,
			Body:         row.body,
			BodyOmitted:  row.bodyOmitted,
		})
	}

	return CallbackDelivery{
		Kind:      CallbackKindMemoryForgotten,
		Cause:     reason.cause,
		CycleId:   cycleId,
		ItemCount: len(items),
		Payload:   CallbackPayload{Items: items},
	}, true
}

// queueEventCallbacks records that events have been deleted, inside the caller's transaction.
//
// An event carries no body, so there is nothing to gate on includeBodies here - the id, the group
// and the name are the whole of what an event's disappearance amounts to.
func (d *DB) queueEventCallbacks(tx *sql.Tx, items []CallbackItem, cause DeleteCause, cycleId int64) error {
	if !d.wantsEventCallbacks(cause) || len(items) == 0 {

		return nil
	}

	return d.queueCallbacks(tx, []CallbackDelivery{{
		Kind:      CallbackKindEventForgotten,
		Cause:     cause,
		CycleId:   cycleId,
		ItemCount: len(items),
		Payload:   CallbackPayload{Items: items},
	}})
}

// eventItemIds is the ids of a captured event set, for the cycle's collection.
func eventItemIds(items []CallbackItem) []string {
	ids := make([]string, 0, len(items))

	for _, item := range items {
		ids = append(ids, item.Id)
	}

	return ids
}

// captureEvents reads what a callback about these events should carry, before they are deleted.
//
// Like the memory capture it runs inside the delete's transaction and before the delete, because
// the group is only readable while the row still exists - and unlike it, this is the whole cost:
// there is no body column to decide about.
func (d *DB) captureEvents(tx *sql.Tx, ids []string) ([]CallbackItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	args := make([]any, len(ids))

	for i, id := range ids {
		args[i] = id
	}

	rows, err := tx.Query(
		d.rebind(
			`SELECT e.id, e.group_name, COALESCE(l.level_rank, 0)
			FROM events e LEFT JOIN significance_levels l ON l.id = e.significance_level_id
			WHERE e.id IN (`+placeholders(len(ids))+`)`,
		),
		args...,
	)
	if err != nil {
		log.Errorf("failed to capture events for a callback: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var out []CallbackItem

	for rows.Next() {
		var item CallbackItem

		if err := rows.Scan(&item.Id, &item.Group, &item.Significance); err != nil {
			log.Errorf("failed to scan an event for a callback: %s", err.Error())

			return nil, err
		}

		out = append(out, item)
	}

	return out, rows.Err()
}

// captureMemoryCallback reads what a callback about these memory ids should carry, before they are
// deleted, and returns the delivery to queue after the delete.
//
// The id-addressing delete paths use it rather than the scan-driven one: they have no snapshots and
// no computed value, and the capture is also what tells them which ids actually EXIST. That matters
// for the same reason the recall guard does on the decay path - announcing an id the store never
// held would be a receiver acting on a deletion that did not happen.
//
// Returns ok false when there is nothing to say, so a caller can queue unconditionally.
func (d *DB) captureMemoryCallback(
	tx *sql.Tx,
	memoryIds []string,
	cause DeleteCause,
) (CallbackDelivery, bool, error) {
	if !d.wantsMemoryCallbacks(cause) || len(memoryIds) == 0 {

		return CallbackDelivery{}, false, nil
	}

	captured, err := d.captureMemoryRows(tx, memoryIds, nil, d.callbacks.IncludeBodies)
	if err != nil {

		return CallbackDelivery{}, false, err
	}

	// The captured set, not the requested one: an id the store does not hold produced no row.
	present := make([]string, 0, len(captured))

	for _, id := range memoryIds {
		if _, ok := captured[id]; ok {
			present = append(present, id)
		}
	}

	delivery, ok := memoryDelivery(captured, present, forgetReason{cause: cause}, d.cycleId(cause))

	return delivery, ok, nil
}

// wantsMemoryCallbacks and wantsEventCallbacks are the two gates the delete paths consult, each
// combining the feature switch, the cause filter and the per-kind toggle into one question.
func (d *DB) wantsMemoryCallbacks(cause DeleteCause) bool {
	return d.callbackTable && d.callbacks.wants(cause) && d.callbacks.wantsKind(CallbackKindMemoryForgotten)
}

func (d *DB) wantsEventCallbacks(cause DeleteCause) bool {
	return d.callbackTable && d.callbacks.wants(cause) && d.callbacks.wantsKind(CallbackKindEventForgotten)
}

// captureEventCallback reads what a callback about these events should carry, before they are
// deleted, returning nil when nothing is to be announced so a caller can queue unconditionally.
func (d *DB) captureEventCallback(tx *sql.Tx, eventIds []string, cause DeleteCause) ([]CallbackItem, error) {
	if !d.wantsEventCallbacks(cause) || len(eventIds) == 0 {

		return nil, nil
	}

	return d.captureEvents(tx, eventIds)
}

// maxCycleIds bounds how many ids one cycle's collection holds for the completion delivery.
//
// A cycle that forgets a million memories must not turn into a million strings in memory on the way
// to describing itself. Past the bound the collection stops growing and records that it did, so the
// completion delivery says "these, and N more" rather than silently reporting a short list as
// complete.
const maxCycleIds = 100_000

// cycleCollection is the ids one sleep cycle has forgotten so far, plus the cycle's own id.
//
// It exists because the sleep-completion callback carries a list of ids, and the ids are only known
// inside the four passes - the counts are all that come back out, deliberately, so that
// GetConsolidationStatus can stay reader-tier and counts-only. Rather than widen four Store methods
// (and their eighty-odd call sites) to carry a slice that is nil on every deployment with callbacks
// off, the cycle opens a collection around itself and takes it back at the end.
//
// Three things keep that safe. Only a DECAY cause contributes, so a client's delete landing mid-cycle
// is never reported as something the cycle forgot. Only one cycle runs at a time (sleepOnce's
// singleflight), so the collection is never shared between two. And it is opened and closed by the
// same function, so its lifetime is one call rather than the process.
type cycleCollection struct {
	id         int64
	memoryIds  []string
	eventIds   []string
	memoryMore int
	eventMore  int
}

// BeginCallbackCycle opens a collection for one sleep cycle, stamping cycleId on every delivery the
// passes queue until EndCallbackCycle. A non-positive id, or a store with callbacks off, collects
// nothing.
func (d *DB) BeginCallbackCycle(cycleId int64) {
	log.Tracef("func() db.BeginCallbackCycle(%d)", cycleId)

	d.cycleMu.Lock()
	defer d.cycleMu.Unlock()

	if !d.callbacks.Enabled || !d.callbackTable || cycleId <= 0 {
		d.cycle = nil

		return
	}

	d.cycle = &cycleCollection{id: cycleId}
}

// EndCallbackCycle closes the collection and returns what it gathered: the memory ids, the event
// ids, and how many of each were dropped past the bound.
func (d *DB) EndCallbackCycle() (memoryIds []string, eventIds []string, memoryMore int, eventMore int) {
	log.Trace("func() db.EndCallbackCycle")

	d.cycleMu.Lock()
	defer d.cycleMu.Unlock()

	if d.cycle == nil {
		return nil, nil, 0, 0
	}

	collected := d.cycle
	d.cycle = nil

	return collected.memoryIds, collected.eventIds, collected.memoryMore, collected.eventMore
}

// cycleId returns the id to stamp on a delivery with this cause, or 0 outside a cycle.
func (d *DB) cycleId(cause DeleteCause) int64 {
	if !cause.decay() {
		return 0
	}

	d.cycleMu.Lock()
	defer d.cycleMu.Unlock()

	if d.cycle == nil {
		return 0
	}

	return d.cycle.id
}

// collectForgotten adds to the open cycle's collection, if there is one and this cause belongs to it.
func (d *DB) collectForgotten(cause DeleteCause, memoryIds []string, eventIds []string) {
	if !cause.decay() || (len(memoryIds) == 0 && len(eventIds) == 0) {
		return
	}

	d.cycleMu.Lock()
	defer d.cycleMu.Unlock()

	if d.cycle == nil {
		return
	}

	d.cycle.memoryIds, d.cycle.memoryMore = appendBounded(d.cycle.memoryIds, memoryIds, d.cycle.memoryMore)
	d.cycle.eventIds, d.cycle.eventMore = appendBounded(d.cycle.eventIds, eventIds, d.cycle.eventMore)
}

// appendBounded appends what fits under maxCycleIds and counts the rest.
func appendBounded(into []string, from []string, dropped int) ([]string, int) {
	if len(from) == 0 {
		return into, dropped
	}

	room := maxCycleIds - len(into)
	if room <= 0 {
		return into, dropped + len(from)
	}

	if len(from) <= room {
		return append(into, from...), dropped
	}

	return append(into, from[:room]...), dropped + (len(from) - room)
}
