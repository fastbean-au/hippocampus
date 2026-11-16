package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/seal"
	"github.com/fastbean-au/hippocampus/seal/kms"
)

// encryptionConfig is the storage.encryption.* block: whether new bodies are sealed, the key they
// are sealed under, and the retired keys that still open what earlier keys sealed.
type encryptionConfig struct {
	Enabled bool

	// Key is the active key, base64. With Enabled false it is still read, as a key to open with:
	// turning encryption off stops sealing, and must not make what was sealed unreadable.
	Key string

	// RetiredKeys is a list of base64 keys separated by commas or whitespace. A string rather than a
	// list so it can be injected as one environment variable, which is how a secret should arrive.
	RetiredKeys string

	// LedgerCheck is what a start does about a key the store's key ledger names and the ring does not
	// hold: "refuse" to start, or "warn" and serve, every row under the key failing as it is read.
	LedgerCheck string

	// KMS is the storage.encryption.kms.* block. A provider in place of Key makes the active key a
	// data key the store keeps wrapped by that KMS (openKeyring).
	KMS kms.Config
}

// ledgerCheckPolicies are the values storage.encryption.ledgerCheck accepts.
var ledgerCheckPolicies = []string{"refuse", "warn"}

// encryptionConfigFromViper reads the storage.encryption.* keys.
func encryptionConfigFromViper() encryptionConfig {
	return encryptionConfig{
		Enabled:     viper.GetBool("storage.encryption.enabled"),
		Key:         viper.GetString("storage.encryption.key"),
		RetiredKeys: viper.GetString("storage.encryption.retiredKeys"),
		LedgerCheck: viper.GetString("storage.encryption.ledgerCheck"),
		KMS: kms.Config{
			Provider: viper.GetString("storage.encryption.kms.provider"),
			Key:      viper.GetString("storage.encryption.kms.keyId"),
			Endpoint: viper.GetString("storage.encryption.kms.endpoint"),
			Token:    viper.GetString("storage.encryption.kms.token"),
		},
	}
}

// usesKMS reports whether the store's data keys are wrapped by a KMS.
func (c encryptionConfig) usesKMS() bool {
	return c.KMS.Provider != ""
}

// configuredKeys is what the configuration itself holds: the key new bodies are sealed under, when
// one is configured and encryption is on, and the keys held only to open.
type configuredKeys struct {
	active  []byte
	retired [][]byte
}

// keysFromConfig decodes the keys the configuration holds directly. A KMS-backed configuration
// holds no active key here - it is a data key only the store and the KMS together can produce - so
// it is valid with encryption on and storage.encryption.key unset.
//
// The active key is the configured key only while encryption is enabled. Disabled, the same key is
// held as retired, so a store written while it was on goes on reading after it is turned off.
func keysFromConfig(cfg encryptionConfig) (configuredKeys, error) {
	var keys configuredKeys

	retired := strings.FieldsFunc(cfg.RetiredKeys, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})

	if cfg.Enabled && cfg.Key != "" && cfg.usesKMS() {
		return keys, fmt.Errorf("storage.encryption.key and storage.encryption.kms.provider both name the key new bodies are sealed under; set one of them (a key sealed under before moving to a KMS belongs in storage.encryption.retiredKeys)")
	}

	if cfg.Enabled && cfg.Key == "" && !cfg.usesKMS() {
		if len(retired) > 0 {
			return keys, fmt.Errorf("storage.encryption.enabled needs storage.encryption.key or storage.encryption.kms.provider; storage.encryption.retiredKeys only opens what earlier keys sealed")
		}

		return keys, fmt.Errorf("storage.encryption.enabled needs storage.encryption.key (generate one with `openssl rand -base64 %d`) or storage.encryption.kms.provider", seal.KeyBytes)
	}

	if cfg.Key != "" {
		key, err := seal.ParseKey(cfg.Key)
		if err != nil {
			return keys, fmt.Errorf("storage.encryption.key: %w", err)
		}

		if cfg.Enabled {
			keys.active = key
		} else {
			keys.retired = append(keys.retired, key)
		}
	}

	for i, encoded := range retired {
		key, err := seal.ParseKey(encoded)
		if err != nil {
			return keys, fmt.Errorf("storage.encryption.retiredKeys entry %d: %w", i+1, err)
		}

		keys.retired = append(keys.retired, key)
	}

	return keys, nil
}

// newKeyring builds a ring from keys, or nil when there are none - the default, which seals nothing
// and opens nothing sealed.
func newKeyring(keys configuredKeys) (*seal.Keyring, error) {
	if keys.active == nil && len(keys.retired) == 0 {
		return nil, nil
	}

	ring, err := seal.NewKeyring(keys.active, keys.retired)
	if err != nil {
		return nil, fmt.Errorf("storage.encryption: %w", err)
	}

	return ring, nil
}

// keyringFromConfig builds the ring the configuration alone describes, without the store: no KMS
// data key is in it.
func keyringFromConfig(cfg encryptionConfig) (*seal.Keyring, error) {
	keys, err := keysFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	return newKeyring(keys)
}

// newKeyWrapper builds the KMS client. A variable so a test can stand a fake KMS in for one.
var newKeyWrapper = kms.New

// openKeyring builds the ring a store is opened with, checks it against the store's key ledger, and
// hands it to the store. Every open that reads or writes bodies goes through it: the service, and
// each offline mode.
//
// With a KMS configured it adds the store's data keys (kmsDataKeys), and, with encryption on, makes
// one of them the active key - generating and wrapping it the first time. Then the check item 205
// of TODO-3 asked for: a key the ledger names that the ring does not hold is refused (or warned
// about, under storage.encryption.ledgerCheck) now, rather than surfacing as a failed read of
// whichever row happens to be under it. Last, the active key is recorded, before the store seals
// anything under it, which is what lets the next start make the same check.
func openKeyring(ctx context.Context, database *db.DB, cfg encryptionConfig) (*seal.Keyring, error) {
	log.Trace("func() openKeyring")

	keys, err := keysFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	if cfg.usesKMS() {
		wrapper, err := newKeyWrapper(ctx, cfg.KMS)
		if err != nil {
			return nil, fmt.Errorf("storage.encryption.kms: %w", err)
		}

		dataKeys, err := kmsDataKeys(ctx, database, wrapper, cfg.Enabled)
		if err != nil {
			return nil, err
		}

		keys.active = dataKeys.active
		keys.retired = append(keys.retired, dataKeys.retired...)
	}

	ring, err := newKeyring(keys)
	if err != nil {
		return nil, err
	}

	if err := checkKeyLedger(ctx, database, ring, cfg.LedgerCheck); err != nil {
		return nil, err
	}

	if ring.Active() != "" {
		if err := database.RecordKey(ctx, ring.Active()); err != nil {
			return nil, err
		}
	}

	database.SetKeyring(ring)

	return ring, nil
}

// dataKeyContext is the additional data a data key is wrapped with: its own id, so a wrapped key
// copied onto another ledger row does not unwrap.
func dataKeyContext(id string) []byte {
	return []byte("hippocampus data key:" + id)
}

// kmsDataKeys unwraps the store's data keys. The active one, when sealing is on, is the oldest
// undrained data key wrapped under the configured key-encryption key; the rest are held to open.
//
// A data key that will not unwrap is not held, and the ledger check then says so - unless it is
// wrapped under the configured key-encryption key and sealing is on. That is refused outright,
// because the alternative is generating a second data key every time the KMS has a bad moment.
func kmsDataKeys(ctx context.Context, database *db.DB, wrapper kms.Wrapper, sealing bool) (configuredKeys, error) {
	log.Trace("func() kmsDataKeys")

	var keys configuredKeys

	// Twice at most: a data key is generated only when the first pass finds none to seal under, and
	// the second pass then picks whichever the ledger holds first - which, when two replicas
	// generated one each at the same moment, is the same key for both.
	for attempt := 0; attempt < 2; attempt++ {
		keys = configuredKeys{}

		entries, err := database.KeyLedger(ctx)
		if err != nil {
			return keys, fmt.Errorf("reading the store's KMS-wrapped data keys: %w", err)
		}

		for _, entry := range entries {
			if entry.WrappedKey == nil || entry.Drained() {
				continue
			}

			configured := entry.Provider == wrapper.Provider() && entry.KMSKey == wrapper.Key()

			if entry.Provider != wrapper.Provider() {
				log.Warnf("data key %s is wrapped by %s, not the configured %s, and is not held",
					entry.KeyId,
					entry.Provider,
					wrapper.Provider(),
				)

				continue
			}

			dataKey, err := wrapper.Unwrap(ctx, entry.WrappedKey, entry.KMSKey, dataKeyContext(entry.KeyId))
			if err == nil && seal.KeyID(dataKey) != entry.KeyId {
				err = fmt.Errorf("it unwrapped to a different key than the ledger names")
			}

			if err != nil {
				if configured && sealing {
					return keys, fmt.Errorf("unwrapping data key %s with %s key %s: %w", entry.KeyId, entry.Provider, entry.KMSKey, err)
				}

				log.Warnf("data key %s (wrapped by %s key %s) did not unwrap, and is not held: %s",
					entry.KeyId,
					entry.Provider,
					entry.KMSKey,
					err.Error(),
				)

				continue
			}

			if configured && sealing && keys.active == nil {
				keys.active = dataKey

				continue
			}

			keys.retired = append(keys.retired, dataKey)
		}

		if !sealing || keys.active != nil {
			break
		}

		if attempt == 0 {
			if err := generateDataKey(ctx, database, wrapper); err != nil {
				return keys, err
			}
		}
	}

	if sealing && keys.active == nil {
		return keys, fmt.Errorf("no data key wrapped by %s key %s is in the store after generating one", wrapper.Provider(), wrapper.Key())
	}

	if sealing {
		log.Infof("the active data key %s is wrapped by %s key %s", seal.KeyID(keys.active), wrapper.Provider(), wrapper.Key())
	}

	return keys, nil
}

// generateDataKey draws a data key, has the KMS wrap it, and records the wrapped form. The plain key
// is not kept: kmsDataKeys unwraps it from the ledger like any other, so a key the KMS cannot give
// back is found now rather than at the next start.
func generateDataKey(ctx context.Context, database *db.DB, wrapper kms.Wrapper) error {
	log.Trace("func() generateDataKey")

	dataKey := make([]byte, seal.KeyBytes)

	// crypto/rand.Read does not fail on any supported platform; the check is for the signature.
	if _, err := rand.Read(dataKey); err != nil {
		return fmt.Errorf("drawing a data key: %w", err)
	}

	id := seal.KeyID(dataKey)

	wrapped, err := wrapper.Wrap(ctx, dataKey, dataKeyContext(id))
	if err != nil {
		return fmt.Errorf("wrapping a new data key: %w", err)
	}

	log.Infof("generated data key %s, wrapped by %s key %s", id, wrapper.Provider(), wrapper.Key())

	return database.RecordWrappedKey(ctx, db.KeyLedgerEntry{
		KeyId:      id,
		Provider:   wrapper.Provider(),
		KMSKey:     wrapper.Key(),
		WrappedKey: wrapped,
	})
}

// checkKeyLedger compares a ring against the keys the store's ledger says rows may be sealed under.
//
// A read-only tool opening a store an older build last migrated finds no ledger, and makes no
// check: there is nothing to check against, and the reads that follow fail on a missing key as they
// always did.
func checkKeyLedger(ctx context.Context, database *db.DB, ring *seal.Keyring, policy string) error {
	log.Trace("func() checkKeyLedger")

	if !database.KeyLedgerAvailable() {
		log.Debug("the store has no key ledger yet, so the keys it holds rows under are not checked")

		return nil
	}

	entries, err := database.KeyLedger(ctx)
	if err != nil {
		return fmt.Errorf("reading the key ledger: %w", err)
	}

	var missing []string

	for _, entry := range entries {
		if entry.Drained() || ring.Holds(entry.KeyId) {
			continue
		}

		missing = append(missing, entry.KeyId)
	}

	if len(missing) == 0 {
		return nil
	}

	problem := fmt.Sprintf("the store holds rows sealed under %d key(s) this configuration does not hold (%s), and every one of those rows will fail to read. Add the key to storage.encryption.key or storage.encryption.retiredKeys, or give the service access to the KMS key that wrapped it; if the key is lost for good, storage.encryption.ledgerCheck \"warn\" starts without it",
		len(missing),
		strings.Join(missing, ", "),
	)

	if policy == "warn" {
		log.Warn(problem)

		return nil
	}

	return errors.New(problem)
}

// encryptionProblems checks the storage.encryption.* keys, for storeProblems.
func encryptionProblems() []error {
	var problems []error

	cfg := encryptionConfigFromViper()

	if _, err := keyringFromConfig(cfg); err != nil {
		problems = append(problems, err)
	}

	if cfg.usesKMS() {
		problems = append(problems, cfg.KMS.Problems()...)
	}

	// Unset reads as the default, refuse, which is what checkKeyLedger does with anything but warn.
	if cfg.LedgerCheck != "" && !slices.Contains(ledgerCheckPolicies, cfg.LedgerCheck) {
		problems = append(problems, fmt.Errorf("storage.encryption.ledgerCheck '%s' is not one of %s",
			cfg.LedgerCheck,
			strings.Join(ledgerCheckPolicies, ", "),
		))
	}

	// contentIndexSetting already drops the index under encryption; this refuses the configuration
	// that asks for both, rather than silently overruling it, because whoever set it expected a
	// search that this store will no longer answer.
	if viper.GetBool("storage.encryption.enabled") && viper.IsSet("search.contentIndex.enabled") &&
		viper.GetBool("search.contentIndex.enabled") {
		problems = append(problems, fmt.Errorf("search.contentIndex.enabled cannot be true with storage.encryption.enabled: the index is a plaintext copy of every body it holds, which would leave the store as readable as it was without encryption; search with opensearch.enabled instead, or leave the key unset"))
	}

	return problems
}

// logEncryption says once, at startup, what the store seals and with which key - and, with
// OpenSearch on, that the cluster holds what the store no longer does in the clear.
func logEncryption(ring *seal.Keyring, openSearch bool) {
	ids := ring.IDs()

	if ring.Active() == "" {
		if len(ids) > 0 {
			log.Infof("storage encryption is off: new bodies are stored in the clear, and %d key(s) are held to read what was sealed (%s)",
				len(ids),
				strings.Join(ids, ", "),
			)
		}

		return
	}

	log.Infof("memory bodies, queued callbacks and export archives are sealed at rest under key %s (%d retired key(s) held to read older rows)",
		ring.Active(),
		len(ids)-1,
	)

	if openSearch {
		log.Warn("storage encryption is on with opensearch.enabled: the cluster indexes every body in the clear, so encrypt it at rest on its own side - the service cannot")
	}
}

// reencryptConfig carries what the --reencrypt mode needs, read from viper in main.go.
type reencryptConfig struct {
	StorageDriver    string
	StorageDirectory string
	PostgresDSN      string
	MySQLDSN         string
	Encryption       encryptionConfig

	// NoContentIndex is contentIndexSetting's decision, which the open must honour for the reason
	// backfillConfig.NoContentIndex gives: it runs initSchema, and an open that ignored it would
	// rebuild the plaintext index encryption exists to drop.
	NoContentIndex bool
}

// runReencrypt is the --reencrypt mode: every memory body and queued callback not sealed under the
// active key - written before encryption was enabled, or under a key since retired - is rewritten
// under it. It is what makes enabling encryption cover a store that already holds data, and what
// lets a retired key be removed from the configuration at all.
//
// It writes to the store, so on the embedded driver the storage lock refuses it while the service
// runs. On the server drivers it may run beside a live instance: each row is rewritten only if it
// still holds the bytes that were read, so a row written meanwhile is left as it was written. The
// open is not a consolidating one, for the reason rebuildContentSearch gives.
func runReencrypt(cfg reencryptConfig) error {
	if !cfg.Encryption.Enabled {
		return fmt.Errorf("--reencrypt needs storage.encryption.enabled and storage.encryption.key or storage.encryption.kms.provider: there is no key to re-encrypt under")
	}

	if _, err := keysFromConfig(cfg.Encryption); err != nil {
		return err
	}

	var (
		database *db.DB
		err      error
	)

	opts := contentIndexOptions(!cfg.NoContentIndex)

	switch cfg.StorageDriver {

	case "sqlite":
		database, err = db.New(cfg.StorageDirectory, opts...)

	case "postgres":
		database, err = db.NewPostgres(cfg.PostgresDSN, false, opts...)

	case "mysql":
		database, err = db.NewMySQL(cfg.MySQLDSN, false, opts...)

	default:
		return fmt.Errorf("unknown storage.driver '%s' (expected 'sqlite', 'postgres', or 'mysql')", cfg.StorageDriver)

	}

	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	defer func() { _ = database.Close() }()

	ctx := context.Background()

	ring, err := openKeyring(ctx, database, cfg.Encryption)
	if err != nil {
		return err
	}

	log.Infof("re-encrypting every memory body and queued callback not sealed under key %s", ring.Active())

	started := time.Now()

	stats, err := database.ReencryptBodies(ctx)
	if err != nil {
		return fmt.Errorf("re-encryption stopped after %d memories (it is safe to rerun): %w", stats.Memories, err)
	}

	log.Infof("re-encrypted %d memories and %d queued callbacks in %s; %d rows written meanwhile were left as written",
		stats.Memories,
		stats.Callbacks,
		time.Since(started).Round(time.Millisecond),
		stats.Skipped,
	)

	// What lets a retired key come out of the configuration: the ledger stops asking for a key once
	// no row is under it.
	drained, err := database.DrainKeys(ctx, ring.Active())
	if err != nil {
		return fmt.Errorf("re-encryption finished, but the key ledger was not updated (it is safe to rerun): %w", err)
	}

	if len(drained) > 0 {
		log.Infof("no row is sealed under key(s) %s any more; they can be removed from the configuration (keep them wherever an older backup or archive may be restored)",
			strings.Join(drained, ", "),
		)
	}

	return nil
}
