package db

import (
	"context"
	"testing"
)

// Purge's three auxiliary emptyings, driven through their failure arms.
//
// Each is gated on the table having been created, and each rolls the whole transaction back rather
// than committing a store that has lost its records and kept the queues describing them. Nothing on
// the happy path reaches those arms - the statements are three unconditional DELETEs against tables
// this package created - so the only way to execute them is to take the table away underneath.
//
// That is worth doing rather than leaving alone, because the rollback is the property: a Purge that
// committed after failing to empty the callback queue would leave a queue of deliveries about
// records that no longer exist, to be retried against a receiver forever.

// purgeWithMissingTable enables one of the auxiliary tables, drops it, and asserts Purge refuses.
func purgeWithMissingTable(t *testing.T, enable func(*DB), table string) {
	t.Helper()

	database := newTestDB(t)
	enable(database)

	if _, err := database.sql.Exec(`DROP TABLE ` + table); err != nil {
		t.Fatalf("DROP TABLE %s: %s", table, err)
	}

	if err := database.Purge(context.Background()); err == nil {
		t.Errorf("expected Purge to fail when %s cannot be emptied", table)
	}
}

// TestPurgeRollsBackWhenTheForgottenLogCannotBeEmptied covers the tombstone arm. Purge is the one
// automatic emptying of the log, and not an exception to "cleanup is manual": it is itself the
// explicit request to leave nothing behind.
func TestPurgeRollsBackWhenTheForgottenLogCannotBeEmptied(t *testing.T) {
	purgeWithMissingTable(t, func(d *DB) {
		d.SetTombstonePolicy(TombstonePolicy{Enabled: true})
	}, tombstonesTable)
}

// TestPurgeRollsBackWhenTheSearchOutboxCannotBeEmptied covers the outbox arm. A queued deletion is
// only meaningful against a store that still holds the rest, and Purge clears the index wholesale -
// so every deletion the queue was holding has already happened.
func TestPurgeRollsBackWhenTheSearchOutboxCannotBeEmptied(t *testing.T) {
	purgeWithMissingTable(t, func(d *DB) {
		d.SetSearchOutbox(true)
	}, searchOutboxTable)
}

// TestPurgeRollsBackWhenTheCallbackQueueCannotBeEmptied covers the callback arm, whose failure is
// the one with a consequence outside this process: a delivery describing a record that no longer
// exists is a notification nobody can reconcile.
func TestPurgeRollsBackWhenTheCallbackQueueCannotBeEmptied(t *testing.T) {
	purgeWithMissingTable(t, func(d *DB) {
		d.SetCallbackPolicy(CallbackPolicy{Enabled: true, MemoryEvents: true})
	}, callbackQueueTable)
}
