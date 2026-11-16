package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/seal"
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
}

// encryptionConfigFromViper reads the storage.encryption.* keys.
func encryptionConfigFromViper() encryptionConfig {
	return encryptionConfig{
		Enabled:     viper.GetBool("storage.encryption.enabled"),
		Key:         viper.GetString("storage.encryption.key"),
		RetiredKeys: viper.GetString("storage.encryption.retiredKeys"),
	}
}

// keyringFromConfig builds the ring the store and the archive path share. It returns nil when no key
// is configured at all - the default, which seals nothing and opens nothing sealed.
//
// The active key is the configured key only while encryption is enabled. Disabled, the same key is
// held as retired, so a store written while it was on goes on reading after it is turned off.
func keyringFromConfig(cfg encryptionConfig) (*seal.Keyring, error) {
	retired := strings.FieldsFunc(cfg.RetiredKeys, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})

	if cfg.Key == "" && len(retired) == 0 {
		if cfg.Enabled {
			return nil, fmt.Errorf("storage.encryption.enabled needs storage.encryption.key (generate one with `openssl rand -base64 %d`)", seal.KeyBytes)
		}

		return nil, nil
	}

	var active []byte

	retiredKeys := make([][]byte, 0, len(retired)+1)

	if cfg.Key != "" {
		key, err := seal.ParseKey(cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("storage.encryption.key: %w", err)
		}

		if cfg.Enabled {
			active = key
		} else {
			retiredKeys = append(retiredKeys, key)
		}
	}

	if cfg.Enabled && active == nil {
		return nil, fmt.Errorf("storage.encryption.enabled needs storage.encryption.key; storage.encryption.retiredKeys only opens what earlier keys sealed")
	}

	for i, encoded := range retired {
		key, err := seal.ParseKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("storage.encryption.retiredKeys entry %d: %w", i+1, err)
		}

		retiredKeys = append(retiredKeys, key)
	}

	ring, err := seal.NewKeyring(active, retiredKeys)
	if err != nil {
		return nil, fmt.Errorf("storage.encryption: %w", err)
	}

	return ring, nil
}

// encryptionProblems checks the storage.encryption.* keys, for storeProblems.
func encryptionProblems() []error {
	var problems []error

	if _, err := keyringFromConfig(encryptionConfigFromViper()); err != nil {
		problems = append(problems, err)
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
	ring, err := keyringFromConfig(cfg.Encryption)
	if err != nil {
		return err
	}

	if ring.Active() == "" {
		return fmt.Errorf("--reencrypt needs storage.encryption.enabled and storage.encryption.key: there is no key to re-encrypt under")
	}

	var database *db.DB

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

	database.SetKeyring(ring)

	log.Infof("re-encrypting every memory body and queued callback not sealed under key %s", ring.Active())

	started := time.Now()

	stats, err := database.ReencryptBodies(context.Background())
	if err != nil {
		return fmt.Errorf("re-encryption stopped after %d memories (it is safe to rerun): %w", stats.Memories, err)
	}

	log.Infof("re-encrypted %d memories and %d queued callbacks in %s; %d rows written meanwhile were left as written",
		stats.Memories,
		stats.Callbacks,
		time.Since(started).Round(time.Millisecond),
		stats.Skipped,
	)

	return nil
}
