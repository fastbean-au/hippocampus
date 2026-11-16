package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/seal"
	"github.com/fastbean-au/hippocampus/types"
)

// secretBody is a body no store holds by accident, long enough to clear the compression threshold,
// and repetitive, so a plaintext copy anywhere in a file is found by a substring search.
var secretBody = strings.Repeat("the launch code is swordfish. ", 40)

// testKeyring builds a ring sealing under a fresh key, and returns the key so a later ring can
// carry it as retired.
func testKeyring(t *testing.T, retired ...[]byte) (*seal.Keyring, []byte) {
	t.Helper()

	key := make([]byte, seal.KeyBytes)

	if _, err := rand.Read(key); err != nil {
		t.Fatalf("drawing a key: %s", err)
	}

	ring, err := seal.NewKeyring(key, retired)
	if err != nil {
		t.Fatalf("building a ring: %s", err)
	}

	return ring, key
}

// readOnlyKeyring is a ring that opens under keys but seals nothing.
func readOnlyKeyring(t *testing.T, keys ...[]byte) *seal.Keyring {
	t.Helper()

	ring, err := seal.NewKeyring(nil, keys)
	if err != nil {
		t.Fatalf("building a read-only ring: %s", err)
	}

	return ring
}

// storedKeyId reads a memory's body_key_id straight from the row.
func storedKeyId(t *testing.T, d *DB, id string) string {
	t.Helper()

	var keyId string

	if err := d.queryRow(context.Background(), `SELECT body_key_id FROM memories WHERE id = ?`, id).Scan(&keyId); err != nil {
		t.Fatalf("reading body_key_id: %s", err)
	}

	return keyId
}

// createSecret stores secretBody under id.
func createSecret(t *testing.T, d *DB, id string) {
	t.Helper()

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 5, Body: secretBody}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}
}

// readBody reads a memory back through the ordinary read path.
func readBody(t *testing.T, d *DB, id string) (string, error) {
	t.Helper()

	memories, err := d.GetMemoriesByIds(context.Background(), []string{id})
	if err != nil {
		return "", err
	}

	if len(*memories) != 1 {
		t.Fatalf("read %d memories for %s, want 1", len(*memories), id)
	}

	return (*memories)[0].Body, nil
}

// storeFiles concatenates every file of a file-backed SQLite store - the database and its WAL, where
// a write sits until a checkpoint - which is what a copied store directory gives away.
func storeFiles(t *testing.T, dir string) []byte {
	t.Helper()

	var all []byte

	for _, name := range []string{DataFile, DataFile + "-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			t.Fatalf("reading %s: %s", name, err)
		}

		all = append(all, data...)
	}

	return all
}

// TestEncryptedBodyIsNotInTheDatabaseFile is TODO-3 item 195's first property: with encryption on,
// a copied store does not give the bodies away. The control half stores the same body unsealed in a
// second store and finds it, so the search is shown able to find what it is looking for - a test that
// could not see plaintext would pass this for nothing. Compression is off in both, since a gzipped
// body would hide from a substring search with no encryption at all.
//
// SQLite only: it is the dialect whose store is a file this test can read. The server dialects are
// covered at the column by TestEncryptedBodyIsSealedInTheRow.
func TestEncryptedBodyIsNotInTheDatabaseFile(t *testing.T) {
	ring, _ := testKeyring(t)

	open := func(t *testing.T, keyring *seal.Keyring) (*DB, string) {
		t.Helper()

		dir := t.TempDir()

		// With encryption on the service opens the store without its content index, which is a
		// plaintext copy of every body (see contentIndexSetting); this is that open.
		d, err := New(dir, WithoutContentIndex())
		if err != nil {
			t.Fatalf("opening a file-backed store: %s", err)
		}

		d.SetKeyring(keyring)

		return d, dir
	}

	sealed, sealedDir := open(t, ring)
	createSecret(t, sealed, "m1")

	plain, plainDir := open(t, nil)
	createSecret(t, plain, "m1")

	// Closed before reading, so whatever the store would leave on disk is there to be read.
	_ = sealed.Close()
	_ = plain.Close()

	if !bytes.Contains(storeFiles(t, plainDir), []byte("swordfish")) {
		t.Fatal("the unsealed control store does not hold its body in the clear, so this test cannot see plaintext")
	}

	if bytes.Contains(storeFiles(t, sealedDir), []byte("swordfish")) {
		t.Error("the encrypted store's files hold the body in the clear")
	}

	reopened, err := New(sealedDir, WithoutContentIndex())
	if err != nil {
		t.Fatalf("reopening the encrypted store: %s", err)
	}

	t.Cleanup(func() { _ = reopened.Close() })

	reopened.SetKeyring(ring)

	body, err := readBody(t, reopened, "m1")
	if err != nil {
		t.Fatalf("reading the body back: %s", err)
	}

	if body != secretBody {
		t.Error("the body read back is not the body stored")
	}
}

// TestEncryptedBodyIsSealedInTheRow is the same property on every dialect, at the column: the stored
// bytes do not carry the body, the row names its key, and the read path hands back the body intact -
// through every write path that stores one.
func TestEncryptedBodyIsSealedInTheRow(t *testing.T) {
	d := newTestDB(t)
	ring, _ := testKeyring(t)

	d.SetKeyring(ring)

	ctx := context.Background()

	createSecret(t, d, "created")

	createSecret(t, d, "updated")

	if _, err := d.UpdateMemory(ctx, types.Memory{Id: "updated", Body: secretBody + " updated"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if _, err := d.ImportMemories(ctx, []types.Memory{{Id: "imported", TimeStamp: 100, Significance: 5, Body: secretBody}}); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	binary := types.Memory{Id: "binary", TimeStamp: 100, Significance: 5, Body: secretBody, IsBinary: true}

	if _, err := d.CreateMemory(ctx, binary); err != nil {
		t.Fatalf("CreateMemory (binary): %s", err)
	}

	want := map[string]string{
		"created":  secretBody,
		"updated":  secretBody + " updated",
		"imported": secretBody,
		"binary":   secretBody,
	}

	for id, body := range want {
		stored, _ := storedBody(t, d, id)

		if bytes.Contains(stored, []byte("swordfish")) {
			t.Errorf("%s: the stored body is in the clear", id)
		}

		if keyId := storedKeyId(t, d, id); keyId != ring.Active() {
			t.Errorf("%s: sealed under %q, want the active key %q", id, keyId, ring.Active())
		}

		got, err := readBody(t, d, id)
		if err != nil {
			t.Errorf("%s: reading it back: %s", id, err)

			continue
		}

		if got != body {
			t.Errorf("%s: read back a different body", id)
		}
	}
}

// TestSummaryBodyIsSealed covers the one write path TestEncryptedBodyIsSealedInTheRow cannot reach
// by a plain call: the summary a replacement inserts.
func TestSummaryBodyIsSealed(t *testing.T) {
	d := newTestDB(t)
	ring, _ := testKeyring(t)

	d.SetKeyring(ring)

	ctx := context.Background()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", TimeStart: 1, TimeEnd: 2, Significance: 5, Name: "trip"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "one"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	summary := types.Memory{Id: "s1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: secretBody, IsSummary: true}

	if _, err := d.ReplaceMemoriesWithSummary(ctx, "e1", nil, summary); err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	stored, _ := storedBody(t, d, "s1")

	if bytes.Contains(stored, []byte("swordfish")) {
		t.Error("the summary's stored body is in the clear")
	}

	if got, err := readBody(t, d, "s1"); err != nil || got != secretBody {
		t.Errorf("read the summary back as %q, %v", got, err)
	}
}

// TestRowUnderARetiredKeyStillReads is TODO-3 item 195's rotation property: after the active key
// moves on, a row written under the old one still reads, so long as the old key is kept as retired;
// new writes take the new key; and with the old key dropped altogether the row says which key it
// needs rather than failing to authenticate.
func TestRowUnderARetiredKeyStillReads(t *testing.T) {
	d := newTestDB(t)
	old, oldKey := testKeyring(t)

	d.SetKeyring(old)
	createSecret(t, d, "before")

	rotated, _ := testKeyring(t, oldKey)

	d.SetKeyring(rotated)
	createSecret(t, d, "after")

	if got, err := readBody(t, d, "before"); err != nil || got != secretBody {
		t.Fatalf("reading a row under the retired key: %q, %v", got, err)
	}

	if keyId := storedKeyId(t, d, "after"); keyId != rotated.Active() || keyId == old.Active() {
		t.Errorf("a write after rotation was sealed under %q, want the new key %q", keyId, rotated.Active())
	}

	forgetful, _ := testKeyring(t)

	d.SetKeyring(forgetful)

	if _, err := readBody(t, d, "before"); !errors.Is(err, seal.ErrUnknownKey) {
		t.Errorf("reading a row whose key is gone: got %v, want ErrUnknownKey", err)
	}
}

// TestPlaintextRowsReadWithEncryptionOn is the upgrade case: a store with rows written before
// encryption was enabled reads them as they are, and a ring that has stopped sealing still opens
// what it sealed - which is what makes encryption safe to enable and to disable on a live store.
func TestPlaintextRowsReadWithEncryptionOn(t *testing.T) {
	d := newTestDB(t)

	createSecret(t, d, "plain")

	ring, key := testKeyring(t)

	d.SetKeyring(ring)
	createSecret(t, d, "sealed")

	if got, err := readBody(t, d, "plain"); err != nil || got != secretBody {
		t.Errorf("a plaintext row with encryption on: %q, %v", got, err)
	}

	d.SetKeyring(readOnlyKeyring(t, key))
	createSecret(t, d, "after")

	if keyId := storedKeyId(t, d, "after"); keyId != "" {
		t.Errorf("a ring with no active key sealed a write under %q", keyId)
	}

	if got, err := readBody(t, d, "sealed"); err != nil || got != secretBody {
		t.Errorf("a sealed row with encryption off: %q, %v", got, err)
	}
}

// TestSealedBodyMovedToAnotherRowFails is what binding a body to its id buys: someone who can write
// the table but does not hold the key cannot pass one memory's body off as another's.
func TestSealedBodyMovedToAnotherRowFails(t *testing.T) {
	d := newTestDB(t)
	ring, _ := testKeyring(t)

	d.SetKeyring(ring)

	createSecret(t, d, "m1")

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m2", TimeStamp: 100, Significance: 5, Body: "harmless"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	moved, _ := storedBody(t, d, "m1")

	if _, err := d.sql.Exec(d.rebind(`UPDATE memories SET body = ? WHERE id = ?`), moved, "m2"); err != nil {
		t.Fatalf("moving the body: %s", err)
	}

	if _, err := readBody(t, d, "m2"); !errors.Is(err, seal.ErrTampered) {
		t.Errorf("reading a body moved from another row: got %v, want ErrTampered", err)
	}
}

// TestReencryptBodiesMovesEveryRowToTheActiveKey is the --reencrypt mode: plaintext rows and rows
// under a retired key are rewritten under the active one, after which the retired key is not needed
// at all - which is the point of running it.
func TestReencryptBodiesMovesEveryRowToTheActiveKey(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createSecret(t, d, "plain")

	old, oldKey := testKeyring(t)

	d.SetKeyring(old)
	createSecret(t, d, "old")

	rotated, newKey := testKeyring(t, oldKey)

	d.SetKeyring(rotated)
	createSecret(t, d, "current")

	stats, err := d.ReencryptBodies(ctx)
	if err != nil {
		t.Fatalf("ReencryptBodies: %s", err)
	}

	if stats.Memories != 2 || stats.Skipped != 0 {
		t.Errorf("rewrote %d memories and skipped %d, want 2 and 0", stats.Memories, stats.Skipped)
	}

	// The retired key is dropped: everything must now open under the new key alone.
	onlyNew, err := seal.NewKeyring(newKey, nil)
	if err != nil {
		t.Fatalf("building the new-key-only ring: %s", err)
	}

	d.SetKeyring(onlyNew)

	for _, id := range []string{"plain", "old", "current"} {
		if keyId := storedKeyId(t, d, id); keyId != onlyNew.Active() {
			t.Errorf("%s is under %q after re-encryption, want %q", id, keyId, onlyNew.Active())
		}

		if got, err := readBody(t, d, id); err != nil || got != secretBody {
			t.Errorf("%s after re-encryption: %q, %v", id, got, err)
		}
	}

	// A second run has nothing to do.
	again, err := d.ReencryptBodies(ctx)
	if err != nil || again.Memories != 0 {
		t.Errorf("a second run rewrote %d memories, %v", again.Memories, err)
	}

	d.SetKeyring(readOnlyKeyring(t, newKey))

	if _, err := d.ReencryptBodies(ctx); err == nil {
		t.Error("re-encrypting with no active key succeeded")
	}
}

// TestReencryptLeavesARowRewrittenMeanwhile is the guard that makes the mode safe beside a live
// instance: a row whose body changed after it was read is left as the newer write left it.
func TestReencryptLeavesARowRewrittenMeanwhile(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createSecret(t, d, "m1")

	candidates, err := d.reencryptCandidates(ctx, "nonexistent", "")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("reading candidates: %d, %v", len(candidates), err)
	}

	if _, err := d.UpdateMemory(ctx, types.Memory{Id: "m1", Body: "written meanwhile"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	ring, _ := testKeyring(t)

	d.SetKeyring(ring)

	rewritten, err := d.reencryptMemory(ctx, candidates[0])
	if err != nil {
		t.Fatalf("reencryptMemory: %s", err)
	}

	if rewritten {
		t.Error("a row written meanwhile was overwritten with the body read before it")
	}

	if got, _ := readBody(t, d, "m1"); got != "written meanwhile" {
		t.Errorf("the row now reads %q", got)
	}
}

// TestCallbackPayloadIsSealed covers the queue: a forgotten memory's body rides in its callback
// payload, and the queue lives in the same store, so it must not be the plaintext copy encryption
// otherwise removes.
func TestCallbackPayloadIsSealed(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})
	ring, key := testKeyring(t)

	d.SetKeyring(ring)

	queueOne(t, d, bodyDelivery("m1", secretBody))

	var (
		stored []byte
		keyId  string
	)

	if err := d.queryRow(context.Background(), `SELECT payload, payload_key_id FROM `+callbackQueueTable).Scan(&stored, &keyId); err != nil {
		t.Fatalf("reading the queued payload: %s", err)
	}

	if bytes.Contains(stored, []byte("swordfish")) {
		t.Error("the queued payload is in the clear")
	}

	if keyId != ring.Active() {
		t.Errorf("the payload is under %q, want %q", keyId, ring.Active())
	}

	d.SetKeyring(readOnlyKeyring(t, key))

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	if len(claimed) != 1 || claimed[0].DecodeErr != nil || claimed[0].Payload.Items[0].Body != secretBody {
		t.Fatalf("claimed %+v", claimed)
	}
}

// TestCallbackUnderAnUnknownKeyStaysQueued is the one decode failure the queue does not abandon: the
// payload is intact and the remedy is configuration, so it waits for the key rather than being lost.
func TestCallbackUnderAnUnknownKeyStaysQueued(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})
	ring, key := testKeyring(t)

	d.SetKeyring(ring)
	queueOne(t, d, bodyDelivery("m1", secretBody))

	d.SetKeyring(nil)

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	if len(claimed) != 0 {
		t.Fatalf("a payload under an unknown key was handed back: %+v", claimed)
	}

	d.SetKeyring(readOnlyKeyring(t, key))

	claimed, err = d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil || len(claimed) != 1 || claimed[0].DecodeErr != nil {
		t.Fatalf("once the key is restored: %+v, %v", claimed, err)
	}
}

// TestReencryptRewritesQueuedCallbacks is the queue's half of the --reencrypt mode.
func TestReencryptRewritesQueuedCallbacks(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, bodyDelivery("m1", secretBody))

	ring, key := testKeyring(t)

	d.SetKeyring(ring)

	stats, err := d.ReencryptBodies(context.Background())
	if err != nil {
		t.Fatalf("ReencryptBodies: %s", err)
	}

	if stats.Callbacks != 1 {
		t.Errorf("rewrote %d callbacks, want 1", stats.Callbacks)
	}

	d.SetKeyring(readOnlyKeyring(t, key))

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil || len(claimed) != 1 || claimed[0].Payload.Items[0].Body != secretBody {
		t.Fatalf("after re-encryption: %+v, %v", claimed, err)
	}

	if queueBytes(t, d) == 0 {
		t.Error("the rewritten payload's size was not recorded")
	}
}

// TestBodyEncryptionMigratesAPreMigrationQueue builds a queue without payload_key_id - the shape of
// every queue before migration 18 - and migrates it: the rows already queued read as unsealed, which
// is what they are. No released fixture carries a queue, so this is what exercises the migration.
func TestBodyEncryptionMigratesAPreMigrationQueue(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, bodyDelivery("m1", "a body"))

	if _, err := d.sql.Exec(`ALTER TABLE ` + callbackQueueTable + ` DROP COLUMN payload_key_id`); err != nil {
		t.Fatalf("failed to build the pre-migration queue: %s", err)
	}

	for range 2 {
		if err := d.migrateBodyEncryption(); err != nil {
			t.Fatalf("migrateBodyEncryption: %s", err)
		}
	}

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil || len(claimed) != 1 || claimed[0].Payload.Items[0].Body != "a body" {
		t.Fatalf("after the migration: %+v, %v", claimed, err)
	}
}
