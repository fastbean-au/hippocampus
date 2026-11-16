package main

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/seal"
)

// warnIfOverCapacity reports a store that is already past its byte capacity target as the service
// starts, naming both figures.
//
// It exists for one upgrade in particular. Until 0.45.0 the per-row allowance behind UsedBytes was a
// single 256-byte constant on all three drivers, and on the server drivers it under-counted real
// disk by three to five times; the allowances are now measured per dialect, so the same store
// against the same consolidation.capacityBytes reports a much larger figure than it did yesterday
// and the first cycle after the upgrade is the one that evicts hard. A deployment that has re-derived
// its capacity target from real disk sees nothing; one that has not is told before the cycle runs
// rather than after.
//
// Only when a byte target is configured, since UsedBytes is a scan of the two tables on the server
// drivers and is otherwise never consulted.
func warnIfOverCapacity(database db.Store, capacityBytes int64) {
	log.Trace("func() warnIfOverCapacity")

	if capacityBytes <= 0 {
		return
	}

	used, err := database.UsedBytes(context.Background())
	if err != nil {
		// Not fatal: this is advice, and the sleep cycle reads the same figure for itself.
		log.Warnf("could not read the store's used bytes to check it against the capacity target: %s", err.Error())

		return
	}

	if used <= capacityBytes {
		return
	}

	log.Warnf(
		"the store is already over its capacity target - used_bytes %d against consolidation.capacityBytes %d - "+
			"so the next consolidation cycle will evict until it is back under; if this is unexpected after an "+
			"upgrade, see the 0.45.0 release note on the per-row storage allowances",
		used, capacityBytes,
	)
}

// contentIndexSetting decides whether this store keeps its own content-search index, and returns
// the reason alongside it so the startup line can say which of the two it took.
//
// Derived rather than fixed, on exactly the reflectionSetting precedent above: a capability whose
// sensible default is a function of another key, and one that is cheaper to derive than to make an
// operator discover. A deployment running OpenSearch has a second index answering every search, and
// main selects one backend - so the store's own index is written on every memory, before
// compression ever sees the body, and read by nothing for the life of the store. It is the largest
// non-body cost the store carries on every dialect; on MySQL, where a FULLTEXT index is an index on
// a COLUMN, it is a second uncompressed copy of every body beside the compressed one.
//
// viper.IsSet is what makes the derived default overridable in BOTH directions, which matters more
// here than it does for reflection. Keeping the index alongside OpenSearch is a real choice - it is
// what a deployment falls back to when the cluster is unreachable, and what makes turning OpenSearch
// off again free - and a plain GetBool could only ever express "off".
//
// Encryption takes precedence over both, and is not overridable: the index is a plaintext copy of
// every body it holds, so a store sealing its bodies beside one would protect nothing. An explicit
// true alongside it is refused by storeProblems rather than honoured here.
func contentIndexSetting() (bool, string) {
	if viper.GetBool("storage.encryption.enabled") {
		return false, "storage.encryption.enabled: the index is a plaintext copy of every body, so it is dropped"
	}

	if viper.IsSet("search.contentIndex.enabled") {
		return viper.GetBool("search.contentIndex.enabled"), "set by search.contentIndex.enabled"
	}

	if viper.GetBool("opensearch.enabled") {
		return false, "the default with opensearch.enabled: that index answers every search, so this one would be written and never read"
	}

	return true, "the default with opensearch.enabled false"
}

// contentIndexOptions turns that decision into the constructor options the db package takes. A
// slice rather than a bool because the decision has to reach initSchema, which runs inside the
// constructor - see db.Option.
func contentIndexOptions(enabled bool) []db.Option {
	if enabled {
		return nil
	}

	return []db.Option{db.WithoutContentIndex()}
}

// storeConfig is everything initStore needs to open the store and set it up, read from the
// storage.* and consolidation.* keys by storeConfigFromViper.
type storeConfig struct {
	Driver      string
	Directory   string
	PostgresDSN string
	MySQLDSN    string

	// Consolidate is consolidation.enabled. It is passed to the server drivers so a replica does
	// not contend for the single-consolidator lock.
	Consolidate bool

	WALTriggerBytes int64
	CapacityBytes   int64

	MaxOpenConns int
	MaxIdleConns int
	QueryTimeout time.Duration

	CompressionEnabled  bool
	CompressionMinBytes int

	// Encryption is the storage.encryption.* block, from which run builds Keyring with
	// keyringFromConfig before the store is opened.
	Encryption encryptionConfig
	Keyring    *seal.Keyring

	// OpenSearch is opensearch.enabled, read here only to say at startup that the cluster holds a
	// plaintext copy of what the store seals.
	OpenSearch bool

	Tombstones db.TombstonePolicy

	// ContentIndex and ContentIndexReason are contentIndexSetting's decision and why it was taken.
	ContentIndex       bool
	ContentIndexReason string
}

// setStoreDefaults applies the storage.* defaults.
func setStoreDefaults() {
	viper.SetDefault("storage.driver", "sqlite")
	viper.SetDefault("storage.pool.maxOpenConns", 25)
	viper.SetDefault("storage.queryTimeoutSeconds", 60)
	viper.SetDefault("storage.compression.enabled", true)
	viper.SetDefault("storage.compression.minBytes", 512)

	// Off, because before it the service sealed nothing. The two keys take no default, as no
	// secret does: there is no value the service could default one to that would be secret.
	viper.SetDefault("storage.encryption.enabled", false)

	// Off, because before it the service rebuilt nothing. The two bounds match
	// HippocampusIndexBloated's, so with rebuilds on that alert fires only for an index the
	// rebuilds are not reaching (TODO-3 item 183).
	viper.SetDefault("storage.indexRebuild.enabled", false)
	viper.SetDefault("storage.indexRebuild.minBytes", 16777216)
	viper.SetDefault("storage.indexRebuild.maxBytesPerEntry", 512)

	// One of the four keys validateConfig refuses at zero; see setConsolidationDefaults for why
	// they are defaulted at all.
	viper.SetDefault("storage.directory", "./data")
}

// storeProblems checks the storage.* keys.
func storeProblems() []error {
	var problems []error

	// An empty index is a page or two whatever it holds, so under a floor near zero every empty index
	// divides out to thousands of bytes an entry and is rebuilt on every cycle, to no effect. A
	// mebibyte is far above any empty index and far below any index worth rebuilding.
	if viper.GetBool("storage.indexRebuild.enabled") {
		if minBytes := viper.GetInt64("storage.indexRebuild.minBytes"); minBytes < 1048576 {
			problems = append(problems, fmt.Errorf("storage.indexRebuild.minBytes must be at least 1048576 (1 MiB), got %d: below that an empty index reads as bloated and is rebuilt every cycle", minBytes))
		}

		if perEntry := viper.GetInt64("storage.indexRebuild.maxBytesPerEntry"); perEntry <= 0 {
			problems = append(problems, fmt.Errorf("storage.indexRebuild.maxBytesPerEntry must be greater than 0, got %d", perEntry))
		}
	}

	problems = append(problems, encryptionProblems()...)

	// An empty storage.directory selects db.New's in-memory mode, which is intended for tests
	// only - every write is lost on restart. Refuse it for the sqlite driver so it can never be
	// reached by a real deployment. The postgres/mysql drivers use their own DSN
	// keys, not storage.directory.
	if viper.GetString("storage.driver") == "sqlite" && viper.GetString("storage.directory") == "" {
		problems = append(problems, fmt.Errorf("storage.directory must be set for storage.driver 'sqlite' (an empty directory selects the test-only in-memory database)"))
	}

	return problems
}

// storeConfigFromViper reads the store's configuration.
func storeConfigFromViper() storeConfig {
	log.Trace("func() storeConfigFromViper")

	// Resolved before the store is opened rather than beside the search backend, because it
	// governs schema: initContentSearch runs inside the constructor, and creates or drops the index
	// according to it.
	contentIndex, contentIndexReason := contentIndexSetting()

	return storeConfig{
		Driver:              viper.GetString("storage.driver"),
		Directory:           viper.GetString("storage.directory"),
		PostgresDSN:         viper.GetString("storage.postgres.dsn"),
		MySQLDSN:            viper.GetString("storage.mysql.dsn"),
		Consolidate:         viper.GetBool("consolidation.enabled"),
		WALTriggerBytes:     viper.GetInt64("consolidation.walTriggerBytes"),
		CapacityBytes:       viper.GetInt64("consolidation.capacityBytes"),
		MaxOpenConns:        viper.GetInt("storage.pool.maxOpenConns"),
		MaxIdleConns:        viper.GetInt("storage.pool.maxIdleConns"),
		QueryTimeout:        time.Duration(viper.GetInt("storage.queryTimeoutSeconds")) * time.Second,
		CompressionEnabled:  viper.GetBool("storage.compression.enabled"),
		CompressionMinBytes: viper.GetInt("storage.compression.minBytes"),
		Encryption:          encryptionConfigFromViper(),
		OpenSearch:          viper.GetBool("opensearch.enabled"),
		Tombstones: db.TombstonePolicy{
			Enabled:      viper.GetBool("consolidation.tombstones.enabled"),
			MaxRows:      viper.GetInt("consolidation.tombstones.maxRows"),
			MaxBytes:     viper.GetInt64("consolidation.tombstones.maxBytes"),
			MaxAgeInDays: viper.GetInt("consolidation.tombstones.maxAgeInDays"),
		},
		ContentIndex:       contentIndex,
		ContentIndexReason: contentIndexReason,
	}
}

// initStore opens the store the configuration selects and applies its pool, timeout, compression
// and forgotten-log settings. The caller owns the returned store and closes it.
func initStore(cfg storeConfig) (*db.DB, error) {
	log.Trace("func() initStore")

	// consolidation.enabled (defaulted true in setStartupDefaults) selects whether this instance
	// runs the sleep cycle. In a horizontally scaled deployment against a shared postgres/mysql
	// database, exactly one instance runs with it true - it takes the single-consolidator instance
	// lock and runs consolidation - while the rest run it false as read/write replicas that skip
	// the lock and never sleep. Passed to the server driver so a replica does not contend for the
	// lock, and to the gRPC server so it neither starts the sleep loop nor accepts the manual
	// Sleep RPC.
	if !cfg.Consolidate && cfg.Driver == "sqlite" {
		// SQLite is a single embedded file that cannot be shared between processes, so a
		// non-consolidating SQLite instance is not a horizontal-scaling replica - it is just an
		// instance that never forgets on its own. Warn so a misconfiguration expecting shared-store
		// scaling is visible; horizontal scaling requires the postgres or mysql driver.
		log.Warn("consolidation.enabled is false with storage.driver 'sqlite': SQLite cannot be shared between instances, so this instance simply never runs consolidation; horizontal scaling requires the postgres or mysql driver")
	}

	contentIndexOpts := contentIndexOptions(cfg.ContentIndex)

	if cfg.ContentIndex {
		log.Infof("the store keeps its own content-search index (%s)", cfg.ContentIndexReason)
	} else {
		log.Infof("the store keeps no content-search index of its own (%s)", cfg.ContentIndexReason)
	}

	var (
		database *db.DB
		err      error
	)

	switch cfg.Driver {

	case "sqlite":
		database, err = db.New(cfg.Directory, contentIndexOpts...)

	case "postgres", "mysql":
		// WAL-triggered sleep is SQLite-specific (it exists to force a checkpoint when the
		// on-disk WAL file outgrows its trigger); neither server driver has a client-visible WAL
		// file to measure. Failing fast beats accepting the config and silently never triggering.
		// consolidation.capacityBytes works with every driver.
		if cfg.WALTriggerBytes > 0 {
			return nil, fmt.Errorf("consolidation.walTriggerBytes is not supported with storage.driver '%s'", cfg.Driver)
		}

		if cfg.Driver == "postgres" {
			database, err = db.NewPostgres(cfg.PostgresDSN, cfg.Consolidate, contentIndexOpts...)
		} else {
			database, err = db.NewMySQL(cfg.MySQLDSN, cfg.Consolidate, contentIndexOpts...)
		}

		if err != nil {
			break
		}

		// Cap the connection pool on the server drivers so a burst of concurrent RPCs cannot
		// exhaust the shared database's connection slots (database/sql defaults to unlimited open
		// connections). SQLite caps itself at one connection, so this is skipped there. Sum
		// maxOpenConns across every instance in a replicated deployment must stay under the
		// server's max_connections.
		database.SetPoolLimits(cfg.MaxOpenConns, cfg.MaxIdleConns)

	default:
		return nil, fmt.Errorf("unknown storage.driver '%s' (expected 'sqlite', 'postgres', or 'mysql')", cfg.Driver)

	}

	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Bound how long any single statement or transaction may run, so a hung or unreachable database
	// fails an operation after a bounded time rather than blocking the RPC goroutine and its pooled
	// connection indefinitely. Defaults to 60s (comfortably above a full consolidation scan at the
	// benchmarked sizes); 0 disables it. When set it must exceed the longest legitimate operation
	// (notably a full consolidation scan), or a cycle could be aborted mid-scan.
	database.SetQueryTimeout(cfg.QueryTimeout)

	// Store memory bodies compressed, trading CPU on the read/write paths for storage - which is the
	// resource the whole service manages to, so it buys retention. On by default: measured against
	// the surrounding database work it costs a few percent, well under what it saves. It governs
	// writes only: each row records whether it was compressed, so enabling or disabling it on an
	// existing store leaves every row already written perfectly readable.
	database.SetCompression(cfg.CompressionEnabled, cfg.CompressionMinBytes)

	// Seal bodies at rest, after compression. Like compression it governs writes only: each row
	// records the key it was sealed under, so a ring holding the retired keys reads every row
	// whatever it was written with.
	database.SetKeyring(cfg.Keyring)
	logEncryption(cfg.Keyring, cfg.OpenSearch)

	// The forgotten log: a record of what the sleep cycle deleted, and why. Off by default, and
	// bounded when on - an unbounded log inside the store it describes would consume the headroom
	// that drives forgetting.
	if unboundedTombstones(cfg.Tombstones) {
		log.Warn(
			"the forgotten log is enabled with none of consolidation.tombstones.maxRows, maxBytes " +
				"or maxAgeInDays set, so it will grow without bound; it is excluded from the " +
				"capacity target but not from the disk",
		)
	}

	database.SetTombstonePolicy(cfg.Tombstones)

	warnIfOverCapacity(database, cfg.CapacityBytes)

	return database, nil
}

// unboundedTombstones reports a forgotten log that is on with none of its three bounds set.
func unboundedTombstones(policy db.TombstonePolicy) bool {
	return policy.Enabled && policy.MaxRows <= 0 && policy.MaxBytes <= 0 && policy.MaxAgeInDays <= 0
}
