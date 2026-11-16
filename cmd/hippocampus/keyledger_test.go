package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/seal"
	"github.com/fastbean-au/hippocampus/seal/kms"
	"github.com/fastbean-au/hippocampus/types"
)

// fakeKMS is a KMS that wraps by XOR with a pad of its own, bound to the additional data. It counts
// what it is asked, and can be told to refuse every unwrap, which is a KMS the service has lost
// access to.
type fakeKMS struct {
	key     string
	pad     []byte
	refuse  bool
	wraps   int
	unwraps int

	// refuseKey refuses unwraps under that one key-encryption key, and failWrap every wrap.
	refuseKey string
	failWrap  bool
}

func (f *fakeKMS) Provider() string {
	return "vault"
}

func (f *fakeKMS) Key() string {
	return f.key
}

func (f *fakeKMS) Wrap(_ context.Context, dataKey []byte, aad []byte) ([]byte, error) {
	f.wraps++

	if f.failWrap {
		return nil, errors.New("the fake KMS refuses to wrap")
	}

	wrapped := append(bytes.Clone(aad), 0)

	for i, v := range dataKey {
		wrapped = append(wrapped, v^f.pad[i%len(f.pad)])
	}

	return wrapped, nil
}

func (f *fakeKMS) Unwrap(_ context.Context, wrapped []byte, key string, aad []byte) ([]byte, error) {
	f.unwraps++

	prefix := append(bytes.Clone(aad), 0)

	if f.refuse || key == f.refuseKey || !bytes.HasPrefix(wrapped, prefix) {
		return nil, kms.ErrUnwrap
	}

	dataKey := make([]byte, 0, len(wrapped)-len(prefix))

	for i, v := range wrapped[len(prefix):] {
		dataKey = append(dataKey, v^f.pad[i%len(f.pad)])
	}

	return dataKey, nil
}

// useFakeKMS stands f in for every KMS the test builds.
func useFakeKMS(t *testing.T, f *fakeKMS) {
	t.Helper()

	previous := newKeyWrapper

	newKeyWrapper = func(_ context.Context, _ kms.Config) (kms.Wrapper, error) {
		return f, nil
	}

	t.Cleanup(func() { newKeyWrapper = previous })
}

// kmsConfig is an encryption block sealing under the fake KMS's data key.
func kmsConfig(f *fakeKMS) encryptionConfig {
	return encryptionConfig{Enabled: true, KMS: kms.Config{Provider: f.Provider(), Key: f.Key()}}
}

// openTestStore opens a SQLite store in dir.
func openTestStore(t *testing.T, dir string) *db.DB {
	t.Helper()

	database, err := db.New(dir, db.WithoutContentIndex())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}

	return database
}

// TestOpenKeyringWithAKMS is the envelope: the first start generates a data key and keeps it only
// wrapped, every later start unwraps the same one, and a body sealed under it reads back.
//
// Not parallel: it replaces newKeyWrapper.
func TestOpenKeyringWithAKMS(t *testing.T) {
	f := &fakeKMS{key: "transit/hippocampus", pad: []byte("a pad of the fake KMS's own")}

	useFakeKMS(t, f)

	dir := t.TempDir()
	ctx := context.Background()

	first := openTestStore(t, dir)

	ring, err := openKeyring(ctx, first, kmsConfig(f))
	if err != nil {
		t.Fatalf("openKeyring: %s", err)
	}

	if ring.Active() == "" || f.wraps != 1 {
		t.Fatalf("the first start sealed under %q after %d wraps; want a data key and one wrap", ring.Active(), f.wraps)
	}

	entries, err := first.KeyLedger(ctx)
	if err != nil || len(entries) != 1 || entries[0].KeyId != ring.Active() || entries[0].KMSKey != f.Key() {
		t.Fatalf("the ledger after the first start: %+v, %v", entries, err)
	}

	if _, err := first.CreateMemory(ctx, types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "sealed by a data key"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	_ = first.Close()

	second := openTestStore(t, dir)

	t.Cleanup(func() { _ = second.Close() })

	again, err := openKeyring(ctx, second, kmsConfig(f))
	if err != nil {
		t.Fatalf("openKeyring again: %s", err)
	}

	if again.Active() != ring.Active() || f.wraps != 1 {
		t.Errorf("the second start sealed under %q after %d wraps; want %q and still one", again.Active(), f.wraps, ring.Active())
	}

	memories, err := second.GetMemoriesByIds(ctx, []string{"m1"})
	if err != nil || len(*memories) != 1 || (*memories)[0].Body != "sealed by a data key" {
		t.Errorf("reading back: %+v, %v", memories, err)
	}
}

// TestOpenKeyringRefusesAKMSThatWillNotUnwrap is the case that must not generate a second data key:
// a KMS that refuses on one start stops the start, and leaves the ledger as it was.
//
// Not parallel: it replaces newKeyWrapper.
func TestOpenKeyringRefusesAKMSThatWillNotUnwrap(t *testing.T) {
	f := &fakeKMS{key: "transit/hippocampus", pad: []byte("pad")}

	useFakeKMS(t, f)

	database := openTestStore(t, t.TempDir())

	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()

	if _, err := openKeyring(ctx, database, kmsConfig(f)); err != nil {
		t.Fatalf("openKeyring: %s", err)
	}

	f.refuse = true

	if _, err := openKeyring(ctx, database, kmsConfig(f)); !errors.Is(err, kms.ErrUnwrap) {
		t.Errorf("openKeyring with the KMS refusing: %v, want ErrUnwrap", err)
	}

	// Opening only, the same refusal is the ledger check's to report.
	opening := kmsConfig(f)
	opening.Enabled = false

	if _, err := openKeyring(ctx, database, opening); err == nil || !strings.Contains(err.Error(), "does not hold") {
		t.Errorf("opening with the KMS refusing: %v, want the ledger check's refusal", err)
	}

	if entries, _ := database.KeyLedger(ctx); len(entries) != 1 || f.wraps != 1 {
		t.Errorf("after the refusals the ledger holds %d keys after %d wraps; want 1 and 1", len(entries), f.wraps)
	}
}

// TestOpenKeyringChecksTheLedger is the startup check: a store holding rows under a key the
// configuration has lost is refused before it serves, or started with a warning under "warn".
func TestOpenKeyringChecksTheLedger(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	lost := encodedTestKey(t)

	database := openTestStore(t, dir)

	t.Cleanup(func() { _ = database.Close() })

	if _, err := openKeyring(ctx, database, encryptionConfig{Enabled: true, Key: lost}); err != nil {
		t.Fatalf("openKeyring: %s", err)
	}

	replaced := encryptionConfig{Enabled: true, Key: encodedTestKey(t)}

	err := func() error {
		_, err := openKeyring(ctx, database, replaced)

		return err
	}()
	if err == nil || !strings.Contains(err.Error(), keyIdOf(t, lost)) {
		t.Fatalf("openKeyring without the lost key: %v, want a refusal naming %s", err, keyIdOf(t, lost))
	}

	unconfigured := func() error {
		_, err := openKeyring(ctx, database, encryptionConfig{})

		return err
	}()
	if unconfigured == nil {
		t.Error("a store with sealed rows opened with no encryption configured at all")
	}

	replaced.LedgerCheck = "warn"

	if _, err := openKeyring(ctx, database, replaced); err != nil {
		t.Errorf("openKeyring under warn: %s", err)
	}

	// The start under warn recorded its own key, so the ledger now names that one as well.
	retained := encryptionConfig{Enabled: true, Key: replaced.Key, RetiredKeys: lost}

	if _, err := openKeyring(ctx, database, retained); err != nil {
		t.Errorf("openKeyring holding the old key as retired: %s", err)
	}
}

// TestRunReencryptDrainsTheRetiredKey is the end of a rotation: after --reencrypt, the retired key
// can come out of the configuration and the store still starts.
func TestRunReencryptDrainsTheRetiredKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old := encodedTestKey(t)
	current := encodedTestKey(t)

	database := openTestStore(t, dir)

	if _, err := openKeyring(ctx, database, encryptionConfig{Enabled: true, Key: old}); err != nil {
		t.Fatalf("openKeyring: %s", err)
	}

	if _, err := database.CreateMemory(ctx, types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "under the old key"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	_ = database.Close()

	if err := runReencrypt(reencryptConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: dir,
		Encryption:       encryptionConfig{Enabled: true, Key: current, RetiredKeys: old},
		NoContentIndex:   true,
	}); err != nil {
		t.Fatalf("runReencrypt: %s", err)
	}

	reopened := openTestStore(t, dir)

	t.Cleanup(func() { _ = reopened.Close() })

	if _, err := openKeyring(ctx, reopened, encryptionConfig{Enabled: true, Key: current}); err != nil {
		t.Errorf("starting without the drained key: %s", err)
	}
}

// TestKeysFromConfigWithAKMS covers the configurations a KMS adds: one with no key of its own is
// valid, and one naming both a key and a KMS is refused, since both would be the active key.
func TestKeysFromConfigWithAKMS(t *testing.T) {
	withKMS := encryptionConfig{Enabled: true, KMS: kms.Config{Provider: "aws", Key: "alias/hippocampus"}}

	keys, err := keysFromConfig(withKMS)
	if err != nil || keys.active != nil {
		t.Errorf("a KMS with no key: %+v, %v; want no configured active key and no error", keys, err)
	}

	withKMS.RetiredKeys = encodedTestKey(t)

	if keys, err := keysFromConfig(withKMS); err != nil || len(keys.retired) != 1 {
		t.Errorf("a KMS with a retired key: %+v, %v", keys, err)
	}

	withKMS.Key = encodedTestKey(t)

	if _, err := keysFromConfig(withKMS); err == nil || !strings.Contains(err.Error(), "set one of them") {
		t.Errorf("a key and a KMS together: %v, want a refusal", err)
	}

	// Disabled, a key beside a KMS is only another key to open with.
	withKMS.Enabled = false

	if keys, err := keysFromConfig(withKMS); err != nil || keys.active != nil || len(keys.retired) != 2 {
		t.Errorf("disabled with a key and a KMS: %+v, %v", keys, err)
	}
}

// TestEncryptionProblemsWithAKMS covers what --check-config adds for the new keys.
//
// Not parallel: it sets viper keys.
func TestEncryptionProblemsWithAKMS(t *testing.T) {
	cases := []struct {
		name    string
		set     map[string]any
		problem string
	}{
		{
			name: "a KMS is a key",
			set:  map[string]any{"storage.encryption.enabled": true, "storage.encryption.kms.provider": "aws", "storage.encryption.kms.keyId": "alias/h"},
		},
		{
			name:    "an unknown provider",
			set:     map[string]any{"storage.encryption.enabled": true, "storage.encryption.kms.provider": "azure", "storage.encryption.kms.keyId": "k"},
			problem: "is not one of aws, gcp, vault",
		},
		{
			name:    "vault without its address",
			set:     map[string]any{"storage.encryption.enabled": true, "storage.encryption.kms.provider": "vault", "storage.encryption.kms.keyId": "transit/h", "storage.encryption.kms.token": "t"},
			problem: "the Vault address",
		},
		{
			name:    "an unknown ledger check",
			set:     map[string]any{"storage.encryption.ledgerCheck": "ignore"},
			problem: "storage.encryption.ledgerCheck 'ignore'",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			viper.Reset()

			t.Cleanup(viper.Reset)

			for k, v := range c.set {
				viper.Set(k, v)
			}

			problems := errors.Join(encryptionProblems()...)

			if c.problem == "" {
				if problems != nil {
					t.Errorf("unexpected problem: %s", problems)
				}

				return
			}

			if problems == nil || !strings.Contains(problems.Error(), c.problem) {
				t.Errorf("problems = %v, want one containing %q", problems, c.problem)
			}
		})
	}
}

// TestDataKeyContextNamesTheKey pins the additional data a data key is wrapped with: a change to it
// would leave every store's wrapped data key unable to unwrap.
func TestDataKeyContextNamesTheKey(t *testing.T) {
	if got := string(dataKeyContext("0a1b2c3d")); got != "hippocampus data key:0a1b2c3d" {
		t.Errorf("dataKeyContext = %q", got)
	}

	if seal.KeyBytes != 32 {
		t.Errorf("seal.KeyBytes = %d; a data key is drawn at that length", seal.KeyBytes)
	}
}

// TestOpenKeyringAcrossAKMSKeyChange is a move to another key-encryption key: the next start
// generates a data key under the new one and still holds the old data key, until the service loses
// access to the old KMS key - when the ledger check says so.
//
// Not parallel: it replaces newKeyWrapper.
func TestOpenKeyringAcrossAKMSKeyChange(t *testing.T) {
	f := &fakeKMS{key: "transit/old", pad: []byte("pad")}

	useFakeKMS(t, f)

	database := openTestStore(t, t.TempDir())

	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()

	old, err := openKeyring(ctx, database, kmsConfig(f))
	if err != nil {
		t.Fatalf("openKeyring: %s", err)
	}

	f.key = "transit/new"

	moved, err := openKeyring(ctx, database, kmsConfig(f))
	if err != nil {
		t.Fatalf("openKeyring under the new key: %s", err)
	}

	if moved.Active() == old.Active() || !moved.Holds(old.Active()) || f.wraps != 2 {
		t.Errorf("after the move the ring seals under %s, holds the old data key %t, after %d wraps",
			moved.Active(),
			moved.Holds(old.Active()),
			f.wraps,
		)
	}

	f.refuseKey = "transit/old"

	if _, err := openKeyring(ctx, database, kmsConfig(f)); err == nil || !strings.Contains(err.Error(), old.Active()) {
		t.Errorf("with the old KMS key gone: %v, want the ledger check naming %s", err, old.Active())
	}
}

// TestOpenKeyringWithAnotherProvidersDataKey is a store whose data key another provider wrapped: it
// is not held, and the ledger check names it.
//
// Not parallel: it replaces newKeyWrapper.
func TestOpenKeyringWithAnotherProvidersDataKey(t *testing.T) {
	f := &fakeKMS{key: "transit/hippocampus", pad: []byte("pad")}

	useFakeKMS(t, f)

	database := openTestStore(t, t.TempDir())

	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()

	if err := database.RecordWrappedKey(ctx, db.KeyLedgerEntry{KeyId: "0a1b2c3d", Provider: "aws", KMSKey: "alias/h", WrappedKey: []byte("x")}); err != nil {
		t.Fatalf("RecordWrappedKey: %s", err)
	}

	if _, err := openKeyring(ctx, database, kmsConfig(f)); err == nil || !strings.Contains(err.Error(), "0a1b2c3d") {
		t.Errorf("openKeyring: %v, want the ledger check naming the other provider's key", err)
	}
}

// TestOpenKeyringReportsAFailedWrap is a first start whose KMS will not wrap: no data key, and so
// no start.
//
// Not parallel: it replaces newKeyWrapper.
func TestOpenKeyringReportsAFailedWrap(t *testing.T) {
	f := &fakeKMS{key: "transit/hippocampus", pad: []byte("pad"), failWrap: true}

	useFakeKMS(t, f)

	database := openTestStore(t, t.TempDir())

	t.Cleanup(func() { _ = database.Close() })

	if _, err := openKeyring(context.Background(), database, kmsConfig(f)); err == nil || !strings.Contains(err.Error(), "wrapping a new data key") {
		t.Errorf("openKeyring: %v, want the wrap failure", err)
	}
}
