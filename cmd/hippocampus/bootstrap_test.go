package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
)

// The tests in this file drive each subsystem's init function from a literal config, which is the
// point of TODO-3 item 199's split: before it, each of these failure paths needed a full run().
// Those that assert on a log line are not parallel, since they install a hook on the global logger.

// captureWarnings installs a hook on the global logger for the rest of the test.
func captureWarnings(t *testing.T) *logtest.Hook {
	t.Helper()

	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)

	level := log.GetLevel()
	log.SetLevel(log.WarnLevel)
	t.Cleanup(func() { log.SetLevel(level) })

	return hook
}

// warned reports whether any captured warning contains want.
func warned(hook *logtest.Hook, want string) bool {
	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, want) {
			return true
		}
	}

	return false
}

// sqliteStoreConfig is a store config that opens a fresh SQLite store in a temporary directory.
func sqliteStoreConfig(t *testing.T) storeConfig {
	t.Helper()

	return storeConfig{
		Driver:       "sqlite",
		Directory:    t.TempDir(),
		Consolidate:  true,
		ContentIndex: true,
	}
}

func TestInitStore_UnknownDriver(t *testing.T) {
	t.Parallel()

	_, err := initStore(storeConfig{Driver: "oracle"})
	if err == nil || !strings.Contains(err.Error(), "unknown storage.driver 'oracle'") {
		t.Fatalf("expected an unknown driver to be refused by name, got %v", err)
	}
}

// TestInitStore_WALTriggerOnAServerDriver is refused before any connection is attempted, so the
// DSNs here are never dialled.
func TestInitStore_WALTriggerOnAServerDriver(t *testing.T) {
	t.Parallel()

	for _, driver := range []string{"postgres", "mysql"} {
		_, err := initStore(storeConfig{Driver: driver, WALTriggerBytes: 1024})
		if err == nil || !strings.Contains(err.Error(), "storage.driver '"+driver+"'") {
			t.Errorf("%s: expected consolidation.walTriggerBytes to be refused, got %v", driver, err)
		}
	}
}

// TestInitStore_ReplicaOnSQLiteWarns: a non-consolidating SQLite instance is not a replica of
// anything, just a store that never forgets.
//
// Not parallel: it hooks the global logger.
func TestInitStore_ReplicaOnSQLiteWarns(t *testing.T) {
	hook := captureWarnings(t)

	cfg := sqliteStoreConfig(t)
	cfg.Consolidate = false

	database, err := initStore(cfg)
	if err != nil {
		t.Fatalf("initStore: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	if !warned(hook, "consolidation.enabled is false with storage.driver 'sqlite'") {
		t.Error("expected a warning that a non-consolidating SQLite instance never forgets")
	}
}

// TestInitStore_UnboundedForgottenLogWarns: a forgotten log with no bound grows without one.
//
// Not parallel: it hooks the global logger.
func TestInitStore_UnboundedForgottenLogWarns(t *testing.T) {
	hook := captureWarnings(t)

	cfg := sqliteStoreConfig(t)
	cfg.Tombstones = db.TombstonePolicy{Enabled: true}

	database, err := initStore(cfg)
	if err != nil {
		t.Fatalf("initStore: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	if !warned(hook, "will grow without bound") {
		t.Error("expected a warning for a forgotten log with no bound")
	}
}

func TestUnboundedTombstones(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		policy db.TombstonePolicy
		want   bool
	}{
		{name: "off", policy: db.TombstonePolicy{}, want: false},
		{name: "on with no bound", policy: db.TombstonePolicy{Enabled: true}, want: true},
		{name: "row bound", policy: db.TombstonePolicy{Enabled: true, MaxRows: 1}, want: false},
		{name: "byte bound", policy: db.TombstonePolicy{Enabled: true, MaxBytes: 1}, want: false},
		{name: "age bound", policy: db.TombstonePolicy{Enabled: true, MaxAgeInDays: 1}, want: false},
	}

	for _, c := range cases {
		if got := unboundedTombstones(c.policy); got != c.want {
			t.Errorf("%s: got %t, want %t", c.name, got, c.want)
		}
	}
}

// TestInitSearchIndex_BadOpenSearchConfig: an unusable OpenSearch configuration fails startup,
// where an unreachable cluster would not.
func TestInitSearchIndex_BadOpenSearchConfig(t *testing.T) {
	t.Parallel()

	database, err := initStore(sqliteStoreConfig(t))
	if err != nil {
		t.Fatalf("initStore: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	_, err = initSearchIndex(searchIndexConfig{
		OpenSearchEnabled: true,
		OpenSearch: search.Config{
			Addresses: []string{"https://127.0.0.1:1"},
			TLS:       search.TLSConfig{CertFile: "/cert-with-no-matching-key.pem"},
		},
	}, database)
	if err == nil || !strings.Contains(err.Error(), "failed to initialise opensearch") {
		t.Fatalf("expected the opensearch configuration to be refused, got %v", err)
	}
}

// TestInitSearchIndex_FallsBackToTheStore: with OpenSearch off, the store's own index answers.
func TestInitSearchIndex_FallsBackToTheStore(t *testing.T) {
	t.Parallel()

	database, err := initStore(sqliteStoreConfig(t))
	if err != nil {
		t.Fatalf("initStore: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	idx, err := initSearchIndex(searchIndexConfig{ContentIndex: true}, database)
	if err != nil {
		t.Fatalf("initSearchIndex: %s", err)
	}

	t.Cleanup(func() { _ = idx.Close() })

	if _, ok := idx.(*search.SQL); !ok {
		t.Errorf("expected the store's own index, got %T", idx)
	}
}

func TestInitSummariser(t *testing.T) {
	t.Parallel()

	if _, err := initSummariser(summariserConfig{}); err != nil {
		t.Errorf("a disabled summariser should build the no-op, got %s", err)
	}

	_, err := initSummariser(summariserConfig{Enabled: true, Provider: providerOllama})
	if err == nil || !strings.Contains(err.Error(), "ollama summariser") {
		t.Errorf("expected a summariser with no address or model to be refused, got %v", err)
	}
}

func TestInitEmbedder_RequiresOpenSearch(t *testing.T) {
	t.Parallel()

	_, err := initEmbedder(embedderSetup{Enabled: true, Provider: providerOllama})
	if err == nil || !strings.Contains(err.Error(), "requires opensearch.enabled") {
		t.Fatalf("expected an embedder without opensearch to be refused, got %v", err)
	}
}

func TestInitObjectStore(t *testing.T) {
	t.Parallel()

	store, err := initObjectStore(context.Background(), objectStoreConfig{})
	if err != nil || store != nil {
		t.Errorf("with nothing configured, expected no store and no error, got %v, %v", store, err)
	}

	path := filepath.Join(t.TempDir(), "not-a-directory")

	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %s", err)
	}

	_, err = initObjectStore(context.Background(), objectStoreConfig{Directory: path})
	if err == nil || !strings.Contains(err.Error(), "filesystem object store") {
		t.Errorf("expected a file in place of the archive directory to be refused, got %v", err)
	}
}

func TestInitNotifier_MissingURL(t *testing.T) {
	t.Parallel()

	_, err := initNotifier(callbackSinkConfig{Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "callback sink") {
		t.Fatalf("expected a callback sink with no URL to be refused, got %v", err)
	}
}

func TestInitServerTLS(t *testing.T) {
	t.Parallel()

	tlsConf, stop, err := initServerTLS(serverTLSConfig{})
	if err != nil || tlsConf != nil || stop == nil {
		t.Fatalf("with TLS off, expected no config, a usable stop and no error, got %v, %v", tlsConf, err)
	}

	stop()

	_, _, err = initServerTLS(serverTLSConfig{
		Enabled:  true,
		CertFile: "/nonexistent/cert.pem",
		KeyFile:  "/nonexistent/key.pem",
	})
	if err == nil || !strings.Contains(err.Error(), "failed to load TLS credentials") {
		t.Errorf("expected a missing certificate to be refused, got %v", err)
	}
}

func TestInitAuth(t *testing.T) {
	t.Parallel()

	setup, err := initAuth(authConfig{Method: "none"})
	if err != nil || setup.enabled() {
		t.Errorf("auth.method none should build no verifier, got enabled=%t, %v", setup.enabled(), err)
	}

	if _, err := initAuth(authConfig{Method: "bogus"}); err == nil || !strings.Contains(err.Error(), "unknown auth.method 'bogus'") {
		t.Errorf("expected an unknown method to be refused by name, got %v", err)
	}

	if _, err := initAuth(authConfig{Method: "hmac"}); err == nil {
		t.Error("expected hmac with no signing secret to be refused")
	}

	if _, err := initAuth(authConfig{Method: "idp"}); err == nil {
		t.Error("expected idp with neither a JWKS URL nor an issuer to be refused")
	}
}

// TestInitAuth_BadTierOverride: the authoriser is built last, after the revocation list, which must
// not be left running when the authoriser refuses the configuration.
func TestInitAuth_BadTierOverride(t *testing.T) {
	t.Parallel()

	revocations := filepath.Join(t.TempDir(), "revocations.json")

	if err := os.WriteFile(revocations, []byte(`{"jtis":[],"clients":[]}`), 0o600); err != nil {
		t.Fatalf("writing the fixture: %s", err)
	}

	_, err := initAuth(authConfig{
		Method:         "hmac",
		HMAC:           auth.HMACConfig{LegacySecret: "test-secret"},
		RevocationFile: revocations,
		TierOverride:   map[string]string{"GetTopology": "superuser"},
		TLSEnabled:     true,
		BuiltinRoles:   true,
	})
	if err == nil || !strings.Contains(err.Error(), "failed to initialise authorisation") {
		t.Fatalf("expected an unknown tier to be refused, got %v", err)
	}
}
