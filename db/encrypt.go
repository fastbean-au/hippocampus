package db

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/seal"
)

// Memory bodies, and the callback payloads that can carry them, are optionally sealed at rest (see
// the seal package). Encryption sits at the same storage boundary compression does and in the same
// order: a body is compressed and then sealed on the way in, and opened and then decompressed on
// the way out, so everything above the db package - the RPCs, the summariser, export and import -
// still sees the plain body, and ciphertext, which does not compress, is never handed to gzip.
//
// The decision is recorded per row, as compression's is. memories.body_key_id names the key a body
// was sealed under, empty for one that was not, and reads follow the row rather than the current
// configuration: a store holding plaintext rows from before encryption was enabled, or rows under a
// key that has since been retired, reads uniformly so long as the ring holds every key named.
//
// What is NOT sealed is everything a query has to read: ids, timestamps, significance, groups,
// event names and descriptions, links and metadata. Metadata was the close call. It is filtered on
// inside the database, by every dialect's JSON accessor, so sealing it would turn every metadata
// filter into a full read of the table; it is documented instead as the plaintext it is, beside the
// group label.
//
// The consolidation scans are untouched: they never read a body, so they never open one.

// bodyContext is the additional data a memory body is sealed with: its own id, so a sealed body
// moved onto another row - by anyone who can write the table but does not hold the key - fails to
// open rather than reading as that row's body.
func bodyContext(id string) []byte {
	return []byte("memory body\x00" + id)
}

// callbackPayloadContext is the additional data a queued callback payload is sealed with. A queue
// row's seq is assigned by the insert, so it cannot be bound to; the context separates payloads
// from bodies, which is what stops one being passed off as the other.
var callbackPayloadContext = []byte("callback payload")

// encodedBody is a memory body as it is written: the bytes for the body column, and the two per-row
// decisions recorded beside them.
type encodedBody struct {
	bytes      []byte
	compressed bool
	keyId      string
}

// SetKeyring sets the key ring bodies are sealed and opened with (see the seal package). Called once
// at startup, before the store serves, so it needs no lock - the same treatment SetCompression gets.
// A ring with no active key seals nothing new but still opens what earlier configurations sealed; a
// nil ring opens nothing sealed at all.
func (d *DB) SetKeyring(keyring *seal.Keyring) {
	d.keyring = keyring
}

// Keyring returns the ring SetKeyring set, which the archive path seals and opens with too.
func (d *DB) Keyring() *seal.Keyring {
	return d.keyring
}

// encodeBody turns a memory body into what is stored for it: compressed by the compression policy,
// then sealed under the active key, if there is one. A binary body is sealed like any other - it is
// skipped by compression because it may already be compressed, which says nothing about whether it
// is secret.
//
// Unlike a compression failure, a sealing failure fails the write: storing the body in the clear
// instead would be exactly the leak encryption was enabled to prevent.
func (d *DB) encodeBody(id string, body string, isBinary bool) (encodedBody, error) {
	packed, compressed := d.compressBody(body, isBinary)

	sealed, keyId, err := d.keyring.Seal(packed, bodyContext(id))
	if err != nil {
		log.Errorf("failed to seal the body of memory '%s': %s", id, err.Error())

		return encodedBody{}, err
	}

	return encodedBody{bytes: sealed, compressed: compressed, keyId: keyId}, nil
}

// decodeBody turns a stored body back into the one the rest of the service sees, driven by the row's
// own key id and compression flag.
func (d *DB) decodeBody(id string, stored []byte, isCompressed bool, keyId string) (string, error) {
	packed, err := d.keyring.Open(stored, keyId, bodyContext(id))
	if err != nil {
		return "", fmt.Errorf("opening the body of memory '%s': %w", id, err)
	}

	return decompressBody(packed, isCompressed)
}

// reencryptBatchSize is how many memories ReencryptBodies reads per pass.
const reencryptBatchSize = 500

// ReencryptStats reports what ReencryptBodies did.
type ReencryptStats struct {
	// Memories and Callbacks are the rows rewritten under the active key.
	Memories  int
	Callbacks int

	// Skipped is the rows that changed between being read and being rewritten - written again by a
	// live instance - and were left as that instance wrote them.
	Skipped int
}

// reencryptCandidate is one row ReencryptBodies has read and is about to rewrite.
type reencryptCandidate struct {
	id         string
	stored     []byte
	compressed bool
	keyId      string
	isBinary   bool
}

// ReencryptBodies rewrites every memory body, and every queued callback payload, that is not sealed
// under the active key: plaintext rows written before encryption was enabled, and rows under a key
// that has since been retired. It is the --reencrypt mode, and what lets a retired key be removed
// from the configuration at all.
//
// Each row is rewritten only if it still holds exactly the bytes that were read, so a row a live
// instance has written in the meantime is left alone rather than overwritten with the older body.
// That makes it safe beside a live instance on a server dialect; on the embedded one the storage
// lock refuses to let it run beside one at all.
//
// A ring with no active key has nothing to rewrite to, and is refused.
func (d *DB) ReencryptBodies(ctx context.Context) (ReencryptStats, error) {
	log.Trace("func() db.ReencryptBodies")

	var stats ReencryptStats

	active := d.keyring.Active()
	if active == "" {
		return stats, fmt.Errorf("there is no active key to re-encrypt under")
	}

	afterId := ""

	for {
		candidates, err := d.reencryptCandidates(ctx, active, afterId)
		if err != nil {
			return stats, err
		}

		if len(candidates) == 0 {
			break
		}

		for _, candidate := range candidates {
			rewritten, err := d.reencryptMemory(ctx, candidate)
			if err != nil {
				return stats, err
			}

			if rewritten {
				stats.Memories++
			} else {
				stats.Skipped++
			}
		}

		afterId = candidates[len(candidates)-1].id
	}

	callbacks, skipped, err := d.reencryptCallbacks(ctx, active)
	if err != nil {
		return stats, err
	}

	stats.Callbacks = callbacks
	stats.Skipped += skipped

	return stats, nil
}

// reencryptCandidates reads the next batch of memories not sealed under the active key. The rows are
// collected and the result closed before any is rewritten: the embedded dialect's pool is one
// connection, and an UPDATE issued while this query is open would wait on it forever.
func (d *DB) reencryptCandidates(ctx context.Context, active string, afterId string) ([]reencryptCandidate, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT id, body, is_compressed, body_key_id, is_binary FROM memories
		WHERE body_key_id <> ? AND id > ? ORDER BY id LIMIT ?`,
		active,
		afterId,
		reencryptBatchSize,
	)
	if err != nil {
		log.Errorf("failed to read memories to re-encrypt: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var candidates []reencryptCandidate

	for rows.Next() {
		var candidate reencryptCandidate

		if err := rows.Scan(
			&candidate.id,
			&candidate.stored,
			&candidate.compressed,
			&candidate.keyId,
			&candidate.isBinary,
		); err != nil {
			log.Errorf("failed to scan a memory to re-encrypt: %s", err.Error())

			return nil, err
		}

		candidates = append(candidates, candidate)
	}

	return candidates, rows.Err()
}

// reencryptMemory rewrites one memory's body under the active key, reporting whether the row still
// held what was read and so was rewritten.
func (d *DB) reencryptMemory(ctx context.Context, candidate reencryptCandidate) (bool, error) {
	body, err := d.decodeBody(candidate.id, candidate.stored, candidate.compressed, candidate.keyId)
	if err != nil {
		log.Errorf("failed to open memory '%s' to re-encrypt it: %s", candidate.id, err.Error())

		return false, err
	}

	encoded, err := d.encodeBody(candidate.id, body, candidate.isBinary)
	if err != nil {
		return false, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	res, err := d.sql.ExecContext(
		ctx,
		d.rebind(`UPDATE memories SET body = ?, is_compressed = ?, body_key_id = ?
			WHERE id = ? AND body_key_id = ? AND body = ?`),
		encoded.bytes,
		encoded.compressed,
		encoded.keyId,
		candidate.id,
		candidate.keyId,
		candidate.stored,
	)
	if err != nil {
		log.Errorf("failed to re-encrypt memory '%s': %s", candidate.id, err.Error())

		return false, err
	}

	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return changed == 1, nil
}

// reencryptCallbacks rewrites the queued callback payloads not sealed under the active key. The queue
// is bounded, so it is read in one pass.
func (d *DB) reencryptCallbacks(ctx context.Context, active string) (int, int, error) {
	if !d.callbackTable {
		return 0, 0, nil
	}

	type queued struct {
		seq        int64
		stored     []byte
		compressed bool
		keyId      string
	}

	var pending []queued

	if err := func() error {
		ctx, cancel := d.opContext(ctx)
		defer cancel()

		rows, err := d.query(
			ctx,
			`SELECT seq, payload, is_compressed, payload_key_id FROM `+callbackQueueTable+`
			WHERE payload_key_id <> ? ORDER BY seq`,
			active,
		)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var row queued

			if err := rows.Scan(&row.seq, &row.stored, &row.compressed, &row.keyId); err != nil {
				return err
			}

			pending = append(pending, row)
		}

		return rows.Err()
	}(); err != nil {
		log.Errorf("failed to read callbacks to re-encrypt: %s", err.Error())

		return 0, 0, err
	}

	rewritten := 0
	skipped := 0

	for _, row := range pending {
		payload, err := d.openCallbackPayload(row.stored, row.compressed, row.keyId)
		if err != nil {
			log.Errorf("failed to open queued callback %d to re-encrypt it: %s", row.seq, err.Error())

			return rewritten, skipped, err
		}

		encoded, compressed, keyId, err := d.sealCallbackPayload(payload)
		if err != nil {
			return rewritten, skipped, err
		}

		changed, err := d.rewriteCallbackPayload(ctx, row.seq, row.keyId, rewrittenPayload{
			bytes:      encoded,
			compressed: compressed,
			keyId:      keyId,
		})
		if err != nil {
			return rewritten, skipped, err
		}

		if changed {
			rewritten++
		} else {
			skipped++
		}
	}

	return rewritten, skipped, nil
}

// rewrittenPayload is a callback payload re-encoded under the active key.
type rewrittenPayload struct {
	bytes      []byte
	compressed bool
	keyId      string
}

// rewriteCallbackPayload replaces one queued payload, if the row is still under the key it was read
// under. A row already delivered and confirmed is simply gone, which reads as not rewritten.
func (d *DB) rewriteCallbackPayload(ctx context.Context, seq int64, previousKeyId string, payload rewrittenPayload) (bool, error) {
	ctx, cancel := d.opContext(ctx)
	defer cancel()

	res, err := d.sql.ExecContext(
		ctx,
		d.rebind(`UPDATE `+callbackQueueTable+` SET payload = ?, payload_bytes = ?, is_compressed = ?, payload_key_id = ?
			WHERE seq = ? AND payload_key_id = ?`),
		payload.bytes,
		int64(len(payload.bytes)),
		payload.compressed,
		payload.keyId,
		seq,
		previousKeyId,
	)
	if err != nil {
		log.Errorf("failed to re-encrypt queued callback %d: %s", seq, err.Error())

		return false, err
	}

	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return changed == 1, nil
}

// sealCallbackPayload renders a callback payload for the queue: encoded and compressed by
// encodeCallbackPayload, then sealed under the active key. A payload can carry memory bodies
// (callbacks.includeBodies), and the queue lives in the same store they do, so leaving it in the
// clear would make the queue the copy of every forgotten body that a leaked store still gives away.
func (d *DB) sealCallbackPayload(payload CallbackPayload) ([]byte, bool, string, error) {
	encoded, compressed, err := encodeCallbackPayload(payload)
	if err != nil {
		return nil, false, "", err
	}

	sealed, keyId, err := d.keyring.Seal(encoded, callbackPayloadContext)
	if err != nil {
		log.Errorf("failed to seal a callback payload: %s", err.Error())

		return nil, false, "", err
	}

	return sealed, compressed, keyId, nil
}

// openCallbackPayload is the inverse of sealCallbackPayload, driven by the row's own key id and flag.
func (d *DB) openCallbackPayload(stored []byte, isCompressed bool, keyId string) (CallbackPayload, error) {
	encoded, err := d.keyring.Open(stored, keyId, callbackPayloadContext)
	if err != nil {
		return CallbackPayload{}, fmt.Errorf("opening a callback payload: %w", err)
	}

	return decodeCallbackPayload(encoded, isCompressed)
}

// migrateBodyEncryption adds callback_queue.payload_key_id to a store whose queue predates it. Every
// payload already queued was written in the clear, which is what the column's empty default says of
// it, so adding it is the whole migration.
//
// memories.body_key_id arrived in the same release but is a core column (migration 2): the
// content-search migrations read bodies to populate themselves, and a body read now reads it.
func (d *DB) migrateBodyEncryption() error {
	log.Trace("func() db.migrateBodyEncryption")

	label := d.dialect().idType + ` NOT NULL DEFAULT ''`

	if err := d.addColumnIfMissing(callbackQueueTable, "payload_key_id", label); err != nil {
		log.Errorf("failed to migrate %s.payload_key_id for body encryption: %s", callbackQueueTable, err.Error())

		return err
	}

	return nil
}
