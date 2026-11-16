package db

import (
	"context"
	"fmt"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
)

// The key ledger: one row for every key a body or a queued callback payload has ever been sealed
// under (encrypt.go), and, for a key that came from a KMS, the data key itself in its wrapped form.
//
// It exists for the check a per-row key id cannot make on its own. Every row names its key, so a
// row under a key the ring does not hold fails when it is READ - which on a large store may be
// weeks after the configuration that lost the key was deployed. The ledger answers the question
// at startup instead, from one row per key rather than a scan of every memory: a ring missing a
// key the ledger names is refused (or warned about) before the store serves.
//
// A key is recorded before anything is sealed under it, by every instance as it starts, so the
// ledger can name a key no row holds yet but never the reverse. --reencrypt is what takes a key
// back out of the check: once a run has moved every row off it, DrainKeys marks it drained, and a
// ring no longer needs to hold it. A drained key that comes back as the active one is undrained by
// the instance recording it.
//
// For a KMS-backed configuration the same table holds the store's data keys. A random data key is
// generated once, wrapped by the KMS - so the key-encryption key never leaves it - and kept here
// beside its id. That is the envelope encryption the seal package deliberately does not do while
// the key-encryption key lives in the process: here it does not, and the wrapping is what keeps it
// out.

// keyLedgerTable is the ledger.
const keyLedgerTable = "encryption_keys"

// keyLedgerBuildTable is where migration 19 builds the ledger before renaming it into place.
const keyLedgerBuildTable = "encryption_keys_build"

// KeyLedgerEntry is one key the ledger records.
type KeyLedgerEntry struct {
	// KeyId is the seal key id (seal.KeyID), the value body_key_id and payload_key_id carry.
	KeyId string

	// Provider and KMSKey name the KMS and the key-encryption key that wrapped WrappedKey. All three
	// are empty for a key that was configured directly.
	Provider   string
	KMSKey     string
	WrappedKey []byte

	// CreatedAt is when the ledger first recorded the key, in Unix nanoseconds.
	CreatedAt int64

	// DrainedAt is when --reencrypt last found no row under the key, or zero while rows may still
	// be sealed under it.
	DrainedAt int64
}

// Drained reports whether no row is sealed under the key any more.
func (e KeyLedgerEntry) Drained() bool {
	return e.DrainedAt != 0
}

// keyLedgerDDL is the ledger's CREATE TABLE, under the name it is built under.
//
// wrapped_key and kms_key are NULL-able with no DEFAULT: one dialect refuses a literal default on
// an unbounded column, and a directly configured key has neither.
func (d *DB) keyLedgerDDL(table string) string {
	dialect := d.dialect()

	return `CREATE TABLE IF NOT EXISTS ` + table + ` (
		key_id      ` + dialect.idType + ` PRIMARY KEY,
		provider    ` + dialect.labelType + ` NOT NULL DEFAULT '',
		kms_key     ` + dialect.textType + `,
		wrapped_key ` + dialect.blobType + `,
		created_at  ` + dialect.bigintType + ` NOT NULL DEFAULT 0,
		drained_at  ` + dialect.bigintType + ` NOT NULL DEFAULT 0
	)`
}

// initKeyLedger is migration 19: the ledger, seeded with every key id a store already holds rows
// under.
//
// The seeding is a scan of both sealed columns, which has no index to stay on, so it must run once
// rather than on every startup - and a migration here has to detect its own completion (schema.go).
// The trace it leaves is the table itself: the ledger is built under another name, seeded, and only
// then renamed into place, so a run that stops half way leaves no ledger and the next start builds
// it again from the beginning. A ledger that existed but had missed a key would be worse than none:
// the startup check would pass a ring that cannot read the store.
func (d *DB) initKeyLedger() error {
	log.Trace("func() db.initKeyLedger")

	if d.tableExists(keyLedgerTable) {
		return nil
	}

	statements := []string{
		`DROP TABLE IF EXISTS ` + keyLedgerBuildTable,
		d.keyLedgerDDL(keyLedgerBuildTable),
	}

	for _, statement := range statements {
		if _, err := d.sql.Exec(statement); err != nil {
			log.Errorf("failed to create the key ledger: %s", err.Error())

			return err
		}
	}

	ids, err := d.sealedKeyIds(context.Background())
	if err != nil {
		return err
	}

	query := d.rebind(d.upsert(upsertSpec{
		table:   keyLedgerBuildTable,
		columns: `key_id, created_at`,
		key:     []string{"key_id"},
	}))

	now := time.Now().UnixNano()

	for _, id := range ids {
		if _, err := d.sql.Exec(query, id, now); err != nil {
			log.Errorf("failed to seed the key ledger with key %s: %s", id, err.Error())

			return err
		}
	}

	if _, err := d.sql.Exec(`ALTER TABLE ` + keyLedgerBuildTable + ` RENAME TO ` + keyLedgerTable); err != nil {
		log.Errorf("failed to rename the key ledger into place: %s", err.Error())

		return err
	}

	if len(ids) > 0 {
		log.Infof("the key ledger records %d key(s) the store already holds sealed rows under", len(ids))
	}

	return nil
}

// tableExists reports whether a table can be read. A no-row SELECT errors on a missing table on
// every dialect, which is the probe checkReadOnlyTables uses; an error for any other reason reads
// as absent too, and the CREATE that follows then reports it.
func (d *DB) tableExists(table string) bool {
	rows, err := d.sql.Query(`SELECT 1 FROM ` + table + ` WHERE 1 = 0`)
	if err != nil {
		return false
	}

	_ = rows.Close()

	return true
}

// sealedKeyIds returns every key id a memory body or a queued callback payload is sealed under,
// sorted. It is a scan of both tables, so it runs only where a scan is already being paid for: the
// ledger's seeding, and the end of a --reencrypt run.
func (d *DB) sealedKeyIds(ctx context.Context) ([]string, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	seen := map[string]bool{}

	queries := []string{
		`SELECT DISTINCT body_key_id FROM memories WHERE body_key_id <> ''`,
		`SELECT DISTINCT payload_key_id FROM ` + callbackQueueTable + ` WHERE payload_key_id <> ''`,
	}

	for _, query := range queries {
		if err := d.collectKeyIds(ctx, query, seen); err != nil {
			return nil, err
		}
	}

	ids := make([]string, 0, len(seen))

	for k := range seen {
		ids = append(ids, k)
	}

	sort.Strings(ids)

	return ids, nil
}

// collectKeyIds adds the key ids one query returns to seen.
func (d *DB) collectKeyIds(ctx context.Context, query string, seen map[string]bool) error {
	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		log.Errorf("failed to read the key ids rows are sealed under: %s", err.Error())

		return err
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id string

		if err := rows.Scan(&id); err != nil {
			return err
		}

		seen[id] = true
	}

	return rows.Err()
}

// KeyLedgerAvailable reports whether the store has a key ledger. Every read-write open creates one;
// a read-only tool open of a store an older build last migrated finds none.
func (d *DB) KeyLedgerAvailable() bool {
	return d.tableExists(keyLedgerTable)
}

// KeyLedger returns every key the ledger records, oldest first.
func (d *DB) KeyLedger(ctx context.Context) ([]KeyLedgerEntry, error) {
	log.Trace("func() db.KeyLedger")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.sql.QueryContext(ctx,
		`SELECT key_id, provider, kms_key, wrapped_key, created_at, drained_at FROM `+keyLedgerTable+`
		ORDER BY created_at, key_id`,
	)
	if err != nil {
		log.Errorf("failed to read the key ledger: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var entries []KeyLedgerEntry

	for rows.Next() {
		var (
			entry  KeyLedgerEntry
			kmsKey *string
		)

		if err := rows.Scan(&entry.KeyId, &entry.Provider, &kmsKey, &entry.WrappedKey, &entry.CreatedAt, &entry.DrainedAt); err != nil {
			return nil, err
		}

		if kmsKey != nil {
			entry.KMSKey = *kmsKey
		}

		entries = append(entries, entry)
	}

	return entries, rows.Err()
}

// RecordKey records that new values are about to be sealed under a key, before any are. A key the
// ledger already names keeps its row, and is undrained: whatever --reencrypt last found, rows are
// about to be written under it again.
func (d *DB) RecordKey(ctx context.Context, id string) error {
	log.Trace("func() db.RecordKey")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	query := d.upsert(upsertSpec{
		table:   keyLedgerTable,
		columns: `key_id, created_at, drained_at`,
		key:     []string{"key_id"},
		update:  []string{"drained_at"},
	})

	if _, err := d.sql.ExecContext(ctx, d.rebind(query), id, time.Now().UnixNano(), 0); err != nil {
		log.Errorf("failed to record key %s in the key ledger: %s", id, err.Error())

		return fmt.Errorf("recording key %s in the key ledger: %w", id, err)
	}

	return nil
}

// RecordWrappedKey records a KMS-wrapped data key. An entry whose id the ledger already holds is
// left as it is: the id is derived from the key, so the same id is the same key, and the row that
// recorded it first keeps its creation time.
func (d *DB) RecordWrappedKey(ctx context.Context, entry KeyLedgerEntry) error {
	log.Trace("func() db.RecordWrappedKey")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	query := d.upsert(upsertSpec{
		table:   keyLedgerTable,
		columns: `key_id, provider, kms_key, wrapped_key, created_at, drained_at`,
		key:     []string{"key_id"},
	})

	if _, err := d.sql.ExecContext(ctx,
		d.rebind(query),
		entry.KeyId,
		entry.Provider,
		entry.KMSKey,
		entry.WrappedKey,
		time.Now().UnixNano(),
		0,
	); err != nil {
		log.Errorf("failed to record wrapped key %s in the key ledger: %s", entry.KeyId, err.Error())

		return fmt.Errorf("recording wrapped key %s in the key ledger: %w", entry.KeyId, err)
	}

	return nil
}

// DrainKeys marks drained every key the ledger records that no row is sealed under any more, other
// than active, and returns their ids. It is the last step of --reencrypt, which is what moves rows
// off a key; run any earlier and it drains nothing a ring could not already read.
//
// It scans for the keys still in use rather than trusting the run's own count, because a live
// instance on a server dialect may have written under an older key meanwhile, and that row would
// otherwise be under a key the check no longer asks for.
func (d *DB) DrainKeys(ctx context.Context, active string) ([]string, error) {
	log.Trace("func() db.DrainKeys")

	inUse, err := d.sealedKeyIds(ctx)
	if err != nil {
		return nil, err
	}

	used := map[string]bool{active: true}

	for _, id := range inUse {
		used[id] = true
	}

	entries, err := d.KeyLedger(ctx)
	if err != nil {
		return nil, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	now := time.Now().UnixNano()

	var drained []string

	for _, entry := range entries {
		if entry.Drained() || used[entry.KeyId] {
			continue
		}

		if _, err := d.sql.ExecContext(ctx,
			d.rebind(`UPDATE `+keyLedgerTable+` SET drained_at = ? WHERE key_id = ? AND drained_at = 0`),
			now,
			entry.KeyId,
		); err != nil {
			log.Errorf("failed to mark key %s drained: %s", entry.KeyId, err.Error())

			return drained, err
		}

		drained = append(drained, entry.KeyId)
	}

	return drained, nil
}
