package db

import (
	"bytes"
	"context"
	"slices"
	"testing"
)

// ledgerIds returns the ids the ledger records, and which of them are drained.
func ledgerIds(t *testing.T, d *DB) ([]string, map[string]bool) {
	t.Helper()

	entries, err := d.KeyLedger(context.Background())
	if err != nil {
		t.Fatalf("KeyLedger: %s", err)
	}

	ids := make([]string, 0, len(entries))
	drained := map[string]bool{}

	for _, entry := range entries {
		ids = append(ids, entry.KeyId)
		drained[entry.KeyId] = entry.Drained()
	}

	slices.Sort(ids)

	return ids, drained
}

// TestKeyLedgerSeedsFromAnExistingStore is migration 19 on a store that already holds sealed rows:
// the ledger it builds names every key a body or a queued payload is under, and none that only
// plaintext rows are "under".
func TestKeyLedgerSeedsFromAnExistingStore(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, IncludeBodies: true})

	createSecret(t, d, "plain")

	first, firstKey := testKeyring(t)

	d.SetKeyring(first)
	createSecret(t, d, "first")

	second, _ := testKeyring(t, firstKey)

	d.SetKeyring(second)
	queueOne(t, d, bodyDelivery("m1", "a body"))

	// The store as an older build left it: no ledger at all.
	if _, err := d.sql.Exec(`DROP TABLE ` + keyLedgerTable); err != nil {
		t.Fatalf("dropping the ledger: %s", err)
	}

	for range 2 {
		if err := d.initKeyLedger(); err != nil {
			t.Fatalf("initKeyLedger: %s", err)
		}
	}

	ids, _ := ledgerIds(t, d)
	want := []string{first.Active(), second.Active()}

	slices.Sort(want)

	if !slices.Equal(ids, want) {
		t.Errorf("the seeded ledger records %v, want %v", ids, want)
	}
}

// TestKeyLedgerRebuildsAnInterruptedBuild is the completion trace: a build table left by a run that
// stopped before its rename is discarded and the ledger built again, rather than mistaken for one.
func TestKeyLedgerRebuildsAnInterruptedBuild(t *testing.T) {
	d := newTestDB(t)

	ring, _ := testKeyring(t)

	d.SetKeyring(ring)
	createSecret(t, d, "sealed")

	statements := []string{
		`DROP TABLE ` + keyLedgerTable,
		d.keyLedgerDDL(keyLedgerBuildTable),
	}

	for _, statement := range statements {
		if _, err := d.sql.Exec(statement); err != nil {
			t.Fatalf("building the interrupted state: %s", err)
		}
	}

	if err := d.initKeyLedger(); err != nil {
		t.Fatalf("initKeyLedger: %s", err)
	}

	if ids, _ := ledgerIds(t, d); !slices.Equal(ids, []string{ring.Active()}) {
		t.Errorf("the rebuilt ledger records %v, want [%s]", ids, ring.Active())
	}

	if d.tableExists(keyLedgerBuildTable) {
		t.Error("the build table survived the rename")
	}
}

// TestRecordKeyUndrainsAReturningKey covers the two writes a starting instance makes: a new key is
// recorded once, and a drained key that comes back as the active one is in the check again.
func TestRecordKeyUndrainsAReturningKey(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	ring, _ := testKeyring(t)

	for range 2 {
		if err := d.RecordKey(ctx, ring.Active()); err != nil {
			t.Fatalf("RecordKey: %s", err)
		}
	}

	drained, err := d.DrainKeys(ctx, "another")
	if err != nil || !slices.Equal(drained, []string{ring.Active()}) {
		t.Fatalf("DrainKeys drained %v, %v; want the unused key", drained, err)
	}

	if _, isDrained := ledgerIds(t, d); !isDrained[ring.Active()] {
		t.Fatal("the key is not drained")
	}

	if err := d.RecordKey(ctx, ring.Active()); err != nil {
		t.Fatalf("RecordKey: %s", err)
	}

	ids, isDrained := ledgerIds(t, d)

	if len(ids) != 1 || isDrained[ring.Active()] {
		t.Errorf("after recording it again the ledger holds %v, drained %v", ids, isDrained)
	}
}

// TestDrainKeysKeepsAKeyStillInUse is what makes draining safe beside a live instance: a key a row
// is still sealed under is not drained, whatever the active key is.
func TestDrainKeysKeepsAKeyStillInUse(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	old, oldKey := testKeyring(t)

	d.SetKeyring(old)
	createSecret(t, d, "old")

	current, _ := testKeyring(t, oldKey)

	for _, id := range []string{old.Active(), current.Active()} {
		if err := d.RecordKey(ctx, id); err != nil {
			t.Fatalf("RecordKey: %s", err)
		}
	}

	d.SetKeyring(current)

	if drained, err := d.DrainKeys(ctx, current.Active()); err != nil || len(drained) != 0 {
		t.Fatalf("with a row still under the old key DrainKeys drained %v, %v", drained, err)
	}

	if _, err := d.ReencryptBodies(ctx); err != nil {
		t.Fatalf("ReencryptBodies: %s", err)
	}

	drained, err := d.DrainKeys(ctx, current.Active())
	if err != nil || !slices.Equal(drained, []string{old.Active()}) {
		t.Fatalf("after re-encryption DrainKeys drained %v, %v; want the old key", drained, err)
	}

	// Draining again changes nothing.
	if drained, err := d.DrainKeys(ctx, current.Active()); err != nil || len(drained) != 0 {
		t.Errorf("a second DrainKeys drained %v, %v", drained, err)
	}
}

// TestRecordWrappedKeyRoundTrips is the KMS half: a wrapped data key reads back as it was written,
// and recording the same id again keeps the first row.
func TestRecordWrappedKeyRoundTrips(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	entry := KeyLedgerEntry{
		KeyId:      "0a1b2c3d",
		Provider:   "vault",
		KMSKey:     "transit/hippocampus",
		WrappedKey: []byte("vault:v1:wrapped"),
	}

	if err := d.RecordWrappedKey(ctx, entry); err != nil {
		t.Fatalf("RecordWrappedKey: %s", err)
	}

	again := entry
	again.WrappedKey = []byte("a different wrapping")

	if err := d.RecordWrappedKey(ctx, again); err != nil {
		t.Fatalf("RecordWrappedKey again: %s", err)
	}

	entries, err := d.KeyLedger(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("KeyLedger: %+v, %v", entries, err)
	}

	got := entries[0]

	if got.KeyId != entry.KeyId || got.Provider != entry.Provider || got.KMSKey != entry.KMSKey ||
		!bytes.Equal(got.WrappedKey, entry.WrappedKey) || got.CreatedAt == 0 || got.Drained() {
		t.Errorf("read back %+v, want %+v", got, entry)
	}
}
