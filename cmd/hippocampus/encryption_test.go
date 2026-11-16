package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/seal"
	"github.com/fastbean-au/hippocampus/types"
)

// encodedTestKey returns a fresh random key, base64 as an operator would configure it.
func encodedTestKey(t *testing.T) string {
	t.Helper()

	key := make([]byte, seal.KeyBytes)

	if _, err := rand.Read(key); err != nil {
		t.Fatalf("drawing a key: %s", err)
	}

	return base64.StdEncoding.EncodeToString(key)
}

// keyIdOf is the id the service derives for a configured key.
func keyIdOf(t *testing.T, encoded string) string {
	t.Helper()

	key, err := seal.ParseKey(encoded)
	if err != nil {
		t.Fatalf("parsing a test key: %s", err)
	}

	return seal.KeyID(key)
}

func TestKeyringFromConfig(t *testing.T) {
	active := encodedTestKey(t)
	retiredOne := encodedTestKey(t)
	retiredTwo := encodedTestKey(t)

	t.Run("nothing configured is no ring", func(t *testing.T) {
		ring, err := keyringFromConfig(encryptionConfig{})
		if err != nil || ring != nil {
			t.Errorf("got %v, %v; want no ring and no error", ring, err)
		}
	})

	t.Run("enabled seals under the key and opens under the retired ones", func(t *testing.T) {
		ring, err := keyringFromConfig(encryptionConfig{
			Enabled:     true,
			Key:         active,
			RetiredKeys: retiredOne + ", " + retiredTwo + "\n",
		})
		if err != nil {
			t.Fatalf("keyringFromConfig: %s", err)
		}

		if ring.Active() != keyIdOf(t, active) {
			t.Errorf("active key %q, want %q", ring.Active(), keyIdOf(t, active))
		}

		for _, key := range []string{retiredOne, retiredTwo} {
			if !ring.Holds(keyIdOf(t, key)) {
				t.Errorf("the ring does not hold retired key %s", keyIdOf(t, key))
			}
		}
	})

	t.Run("disabled still opens under the configured key", func(t *testing.T) {
		ring, err := keyringFromConfig(encryptionConfig{Key: active})
		if err != nil {
			t.Fatalf("keyringFromConfig: %s", err)
		}

		if ring.Active() != "" {
			t.Errorf("a disabled ring seals under %q", ring.Active())
		}

		if !ring.Holds(keyIdOf(t, active)) {
			t.Error("a disabled ring cannot open what its key sealed")
		}
	})

	refusals := map[string]encryptionConfig{
		"enabled without a key":           {Enabled: true},
		"enabled with only retired keys":  {Enabled: true, RetiredKeys: retiredOne},
		"a key that is not base64":        {Enabled: true, Key: "not base64!"},
		"a key of the wrong length":       {Enabled: true, Key: base64.StdEncoding.EncodeToString([]byte("short"))},
		"a retired key that is not valid": {Enabled: true, Key: active, RetiredKeys: retiredOne + ",garbage!"},
	}

	for name, cfg := range refusals {
		t.Run(name, func(t *testing.T) {
			if _, err := keyringFromConfig(cfg); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestEncryptionProblems(t *testing.T) {
	cases := []struct {
		name    string
		set     map[string]any
		problem string
	}{
		{name: "off is fine", set: map[string]any{}},
		{name: "on with a key is fine", set: map[string]any{"storage.encryption.enabled": true, "storage.encryption.key": encodedTestKey(t)}},
		{name: "on without a key", set: map[string]any{"storage.encryption.enabled": true}, problem: "needs storage.encryption.key"},
		{
			name: "on beside an explicit content index",
			set: map[string]any{
				"storage.encryption.enabled":  true,
				"storage.encryption.key":      encodedTestKey(t),
				"search.contentIndex.enabled": true,
			},
			problem: "search.contentIndex.enabled cannot be true",
		},
		{
			name: "an explicit false content index is fine",
			set: map[string]any{
				"storage.encryption.enabled":  true,
				"storage.encryption.key":      encodedTestKey(t),
				"search.contentIndex.enabled": false,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			for k, v := range testCase.set {
				viper.Set(k, v)
			}

			problems := errors.Join(encryptionProblems()...)

			switch {

			case testCase.problem == "" && problems != nil:
				t.Errorf("unexpected problem: %s", problems)

			case testCase.problem != "" && (problems == nil || !strings.Contains(problems.Error(), testCase.problem)):
				t.Errorf("got %v, want a problem containing %q", problems, testCase.problem)

			}
		})
	}
}

// TestContentIndexSettingUnderEncryption: encryption drops the index whatever opensearch says, since
// the index is a plaintext copy of the bodies being sealed.
func TestContentIndexSettingUnderEncryption(t *testing.T) {
	for _, openSearch := range []bool{false, true} {
		viper.Reset()

		viper.Set("storage.encryption.enabled", true)
		viper.Set("opensearch.enabled", openSearch)

		if enabled, reason := contentIndexSetting(); enabled || !strings.Contains(reason, "storage.encryption.enabled") {
			t.Errorf("opensearch.enabled %v: contentIndexSetting() = %v, %q; want off, naming encryption", openSearch, enabled, reason)
		}
	}

	viper.Reset()
}

// TestRunReencrypt drives the --reencrypt mode over a file-backed store holding a body written in
// the clear, and shows the body sealed afterwards: unreadable without the key, intact with it.
func TestRunReencrypt(t *testing.T) {
	dir := t.TempDir()
	key := encodedTestKey(t)

	plain, err := db.New(dir, db.WithoutContentIndex())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}

	if _, err := plain.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "written in the clear"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	_ = plain.Close()

	cfg := reencryptConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: dir,
		Encryption:       encryptionConfig{Enabled: true, Key: key},
		NoContentIndex:   true,
	}

	if err := runReencrypt(cfg); err != nil {
		t.Fatalf("runReencrypt: %s", err)
	}

	sealed, err := db.New(dir, db.WithoutContentIndex())
	if err != nil {
		t.Fatalf("reopening the store: %s", err)
	}

	t.Cleanup(func() { _ = sealed.Close() })

	if _, err := sealed.GetMemoriesByIds(context.Background(), []string{"m1"}); !errors.Is(err, seal.ErrUnknownKey) {
		t.Errorf("reading without the key: got %v, want ErrUnknownKey - the body was not sealed", err)
	}

	ring, err := keyringFromConfig(cfg.Encryption)
	if err != nil {
		t.Fatalf("keyringFromConfig: %s", err)
	}

	sealed.SetKeyring(ring)

	memories, err := sealed.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil || len(*memories) != 1 || (*memories)[0].Body != "written in the clear" {
		t.Errorf("reading with the key: %+v, %v", memories, err)
	}
}

func TestRunReencryptRefusals(t *testing.T) {
	cases := map[string]reencryptConfig{
		"no active key": {StorageDriver: "sqlite", StorageDirectory: t.TempDir(), Encryption: encryptionConfig{Key: encodedTestKey(t)}},
		"a bad key":     {StorageDriver: "sqlite", StorageDirectory: t.TempDir(), Encryption: encryptionConfig{Enabled: true, Key: "nope!"}},
		"an unknown driver": {
			StorageDriver: "oracle",
			Encryption:    encryptionConfig{Enabled: true, Key: encodedTestKey(t)},
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := runReencrypt(cfg); err == nil {
				t.Error("runReencrypt succeeded")
			}
		})
	}
}

// TestRunReencryptServerDriverOpenFailure covers the server-driver opens: a DSN nothing answers on
// fails the mode rather than reporting nothing to do.
func TestRunReencryptServerDriverOpenFailure(t *testing.T) {
	cases := map[string]reencryptConfig{
		"postgres": {StorageDriver: "postgres", PostgresDSN: "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"},
		"mysql":    {StorageDriver: "mysql", MySQLDSN: "nobody:none@tcp(127.0.0.1:1)/none?timeout=1s"},
	}

	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			v.Encryption = encryptionConfig{Enabled: true, Key: encodedTestKey(t)}

			if err := runReencrypt(v); err == nil || !strings.Contains(err.Error(), "failed to open database") {
				t.Errorf("runReencrypt error = %v, want the open failure", err)
			}
		})
	}
}

// TestRunReencryptReportsABodyItCannotOpen covers a store holding a body sealed under a key the
// configuration no longer carries: the mode stops and says so, rather than skipping the row.
func TestRunReencryptReportsABodyItCannotOpen(t *testing.T) {
	dir := t.TempDir()

	lost, err := keyringFromConfig(encryptionConfig{Enabled: true, Key: encodedTestKey(t)})
	if err != nil {
		t.Fatalf("keyringFromConfig: %s", err)
	}

	store, err := db.New(dir, db.WithoutContentIndex())
	if err != nil {
		t.Fatalf("opening the store: %s", err)
	}

	store.SetKeyring(lost)

	if _, err := store.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "sealed under a lost key"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	_ = store.Close()

	err = runReencrypt(reencryptConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: dir,
		Encryption:       encryptionConfig{Enabled: true, Key: encodedTestKey(t)},
		NoContentIndex:   true,
	})
	if !errors.Is(err, seal.ErrUnknownKey) {
		t.Errorf("runReencrypt error = %v, want ErrUnknownKey", err)
	}
}

// TestLogEncryption covers the startup line in each configuration: nothing for no keys at all, the
// held keys for a ring that only opens, and the active key - plus the OpenSearch warning - for one
// that seals.
//
// Not parallel: it replaces the global logger's hooks.
func TestLogEncryption(t *testing.T) {
	key := encodedTestKey(t)

	readOnly, err := keyringFromConfig(encryptionConfig{Key: key})
	if err != nil {
		t.Fatalf("keyringFromConfig: %s", err)
	}

	active, err := keyringFromConfig(encryptionConfig{Enabled: true, Key: key})
	if err != nil {
		t.Fatalf("keyringFromConfig: %s", err)
	}

	cases := []struct {
		name       string
		ring       *seal.Keyring
		openSearch bool
		want       []string
	}{
		{name: "no keys", ring: nil},
		{name: "read only", ring: readOnly, want: []string{"storage encryption is off"}},
		{name: "sealing", ring: active, want: []string{"sealed at rest under key " + keyIdOf(t, key)}},
		{name: "sealing beside opensearch", ring: active, openSearch: true, want: []string{"sealed at rest", "the cluster indexes every body in the clear"}},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			log.SetLevel(log.InfoLevel)

			logEncryption(v.ring, v.openSearch)

			entries := hook.AllEntries()

			if len(entries) != len(v.want) {
				t.Fatalf("logged %d lines, want %d", len(entries), len(v.want))
			}

			for i, want := range v.want {
				if !strings.Contains(entries[i].Message, want) {
					t.Errorf("line %d = %q, want it to contain %q", i, entries[i].Message, want)
				}
			}
		})
	}
}
