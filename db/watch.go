package db

import (
	"database/sql"
	"sync"
	"sync/atomic"
)

// The change feed behind the Watch RPC (TODO-3 item 193) is the callback queue's deliveries, handed
// to an in-process sink after the transaction that produced them commits.
//
// It reuses the deliveries rather than describing changes a second way because they are already
// built at exactly the right points: inside each write and delete, from the rows actually affected,
// with the group a scoped watcher is filtered by captured while the row still exists. What differs
// is the guarantee. The queue is durable, so its rows commit with the change. The feed is live and
// best-effort, so a delivery is STAGED against its transaction and published only once that
// transaction has committed - a watcher must never hear about a change a rollback undid - and
// discarded when it does not.
//
// Watching costs what the narrowest callbacks configuration costs: a primary-key read of the rows a
// delete or a write touches, and only while somebody is subscribed. With nobody watching every gate
// below is false and the paths are exactly what they were.

// ChangeSink receives the store's committed changes. Watching is asked before each change is
// captured, so it must be cheap; Publish is called after the commit, on the committing goroutine,
// and must not block.
type ChangeSink interface {
	Watching() bool
	Publish(deliveries []CallbackDelivery)
}

// changeFeed is the sink and the deliveries staged against open transactions.
type changeFeed struct {
	sink ChangeSink

	// staged is keyed by transaction. pending counts the entries so that the cleanup every
	// transaction runs (see beginTx) takes no lock while nothing is staged, which is always the case
	// with nobody watching.
	mu      sync.Mutex
	staged  map[*sql.Tx][]CallbackDelivery
	pending atomic.Int64
}

// SetChangeSink registers the sink the Watch RPC subscribes through. Called once at startup, before
// serving, like SetMemoryDeleteObserver.
func (d *DB) SetChangeSink(sink ChangeSink) {
	d.changes.sink = sink
}

// watching reports whether a change committed now must be captured for the feed: because a watcher
// on this instance would hear it, or because the change log records every change whoever is
// watching (changelog.go).
func (d *DB) watching() bool {
	return d.ChangeLogging() || d.changes.sink != nil && d.changes.sink.Watching()
}

// recordChanges is how a write or delete path reports its deliveries: recorded for the change feed,
// and queued for the callbacks the policy asks for. It runs inside the caller's transaction, which
// must then commit through d.commit.
//
// The feed takes one of two routes and never both. With the change log on, the delivery is written
// to it, and every instance's reader publishes it from there - this one included, so publishing it
// in process as well would hand this instance's watchers each change twice. With it off, it is
// staged for this instance's watchers alone.
func (d *DB) recordChanges(tx *sql.Tx, deliveries []CallbackDelivery) error {
	if len(deliveries) == 0 {

		return nil
	}

	switch {

	case d.ChangeLogging():
		if err := d.logChanges(tx, deliveries); err != nil {

			return err
		}

	case d.watching():
		d.stageChanges(tx, deliveries)

	}

	return d.queueCallbacks(tx, d.queueable(deliveries))
}

// queueable is the deliveries the callback policy records. A path captures when either a callback
// or a watcher wants the change, so what it captured for a watcher alone is filtered out here.
func (d *DB) queueable(deliveries []CallbackDelivery) []CallbackDelivery {
	out := make([]CallbackDelivery, 0, len(deliveries))

	for _, delivery := range deliveries {
		switch delivery.Kind {

		case CallbackKindMemoryForgotten, CallbackKindEventForgotten:
			if !d.callbacks.wants(delivery.Cause) || !d.callbacks.wantsKind(delivery.Kind) {
				continue
			}

		case CallbackKindMemoryStored, CallbackKindMemoryUpdated:
			if !d.callbacks.WriteEvents {
				continue
			}

		}

		out = append(out, delivery)
	}

	return out
}

// withoutBodies copies deliveries with their bodies dropped: they are captured only for
// callbacks.includeBodies, and the feed never carries one.
func withoutBodies(deliveries []CallbackDelivery) []CallbackDelivery {
	out := make([]CallbackDelivery, 0, len(deliveries))

	for _, delivery := range deliveries {
		items := make([]CallbackItem, len(delivery.Payload.Items))

		for i, item := range delivery.Payload.Items {
			item.Body = ""
			item.BodyOmitted = false
			items[i] = item
		}

		delivery.Payload = CallbackPayload{Items: items}

		out = append(out, delivery)
	}

	return out
}

// stageChanges holds deliveries against their transaction until it ends.
func (d *DB) stageChanges(tx *sql.Tx, deliveries []CallbackDelivery) {
	stripped := withoutBodies(deliveries)

	d.changes.mu.Lock()
	defer d.changes.mu.Unlock()

	if d.changes.staged == nil {
		d.changes.staged = make(map[*sql.Tx][]CallbackDelivery)
	}

	if _, ok := d.changes.staged[tx]; !ok {
		d.changes.pending.Add(1)
	}

	d.changes.staged[tx] = append(d.changes.staged[tx], stripped...)
}

// takeStaged removes and returns what is staged against a transaction.
func (d *DB) takeStaged(tx *sql.Tx) []CallbackDelivery {
	if d.changes.pending.Load() == 0 {

		return nil
	}

	d.changes.mu.Lock()
	defer d.changes.mu.Unlock()

	deliveries, ok := d.changes.staged[tx]
	if !ok {

		return nil
	}

	delete(d.changes.staged, tx)
	d.changes.pending.Add(-1)

	return deliveries
}

// commit commits a transaction that may have staged changes, and publishes them once it has. A
// failed commit discards them: the change did not happen.
func (d *DB) commit(tx *sql.Tx) error {
	err := tx.Commit()

	deliveries := d.takeStaged(tx)

	if err != nil {

		return err
	}

	if len(deliveries) > 0 && d.changes.sink != nil {
		d.changes.sink.Publish(deliveries)
	}

	return nil
}
