package main

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/search"
)

// backfillConfig carries everything the --backfill-search CLI mode needs, read from viper in
// main.go.
type backfillConfig struct {
	StorageDriver    string
	StorageDirectory string
	PostgresDSN      string
	MySQLDSN         string
	Search           search.Config
	Reindex          bool
	BatchSize        int

	// Embed, when enabled, produces the vectors that make the rebuilt index searchable by meaning.
	// A backfill that omitted them would produce a keyword-only index - silently, since a document
	// without a vector is a perfectly valid document.
	Embed embed.Embedder
}

// rebuildContentSearch rebuilds the primary store's own content-search index - the --backfill-search
// mode when OpenSearch is not configured.
//
// Unlike backfillSearch below, this one is rarely needed, and saying so is more useful than
// offering it silently: the store's index is maintained inside the writes themselves and is
// populated automatically at startup whenever it is found empty on a non-empty store (the upgrade
// case). What it is for is the one drift this backend can suffer - an index write that failed and
// was logged rather than failing its memory - which leaves gaps that the empty-index check will
// never notice.
//
// It differs from backfillSearch in one way that matters operationally: it WRITES to the service's
// own database, so unlike the OpenSearch backfill it must not be run beside a live instance. The
// database is opened read-write for that reason, and the warning below says so.
func rebuildContentSearch(storageDriver string, storageDirectory string) {
	if storageDriver != "sqlite" {
		log.Fatalf(
			"--backfill-search has nothing to rebuild: storage.driver '%s' has no built-in content search, and opensearch.enabled is false",
			storageDriver,
		)
	}

	log.Warn("rebuilding the content search index writes to the database - stop the service first if it is running")

	database, err := db.New(storageDirectory)
	if err != nil {
		log.Fatalf("failed to open database: %s", err.Error())
	}

	started := time.Now()

	if err := database.RebuildContentSearch(context.Background()); err != nil {
		_ = database.Close()

		log.Fatalf("failed to rebuild the content search index: %s", err.Error())
	}

	log.Infof("content search index rebuilt in %s", time.Since(started).Round(time.Millisecond))

	_ = database.Close()
}

// backfillSearch rebuilds the content-search index from the primary store: every non-binary
// memory is re-indexed, keyed by id, so runs are idempotent (each write overwrites the same
// document). With Reindex set the index is deleted and recreated first, which also removes stale
// documents for memories the primary store no longer has. Writes are synchronous and sequential —
// this is a recovery tool, not a hot path — and the process exits via log.Fatalf on the first
// error, since a rerun is always safe.
//
// The tool may run alongside a live service instance (the SQLite open is read-only via
// NewSQLiteReadOnly, so it neither writes DDL nor checkpoints; the Postgres and MySQL opens skip
// the instance lock). The caveat: a memory the live service
// deletes mid-backfill can be re-indexed after its deletion propagated, leaving a stale document
// behind. Stale documents never surface in search results (ids are re-verified against the
// primary store) and the next --reindex run clears them.
func backfillSearch(cfg backfillConfig) {
	log.Info("backfilling the opensearch content-search index from the primary store")

	var database *db.DB
	var err error

	switch cfg.StorageDriver {

	case "sqlite":
		database, err = db.NewSQLiteReadOnly(cfg.StorageDirectory)

	case "postgres":
		database, err = db.NewPostgresReadOnly(cfg.PostgresDSN)

	case "mysql":
		database, err = db.NewMySQLReadOnly(cfg.MySQLDSN)

	default:
		log.Fatalf("unknown storage.driver '%s' (expected 'sqlite', 'postgres', or 'mysql')", cfg.StorageDriver)
	}

	if err != nil {
		log.Fatalf("failed to open database: %s", err.Error())
	}

	idx, err := search.NewOpenSearch(cfg.Search)
	if err != nil {
		log.Fatalf("failed to initialise opensearch: %s", err.Error())
	}

	ctx := context.Background()

	if cfg.Reindex {
		log.Info("deleting and recreating the index")

		if err := idx.RecreateIndex(ctx); err != nil {
			log.Fatalf("failed to recreate the index: %s", err.Error())
		}
	}

	started := time.Now()
	indexed := 0
	afterId := ""

	for {
		memories, err := database.GetIndexableMemoriesPage(ctx, afterId, cfg.BatchSize)
		if err != nil {
			log.Fatalf("failed to read memories after id '%s': %s", afterId, err.Error())
		}

		if len(memories) == 0 {
			break
		}

		for _, memory := range memories {
			doc := search.DocFromMemory(memory)

			// Vectors are not stored in the primary store (see search.Doc), so a rebuild re-embeds
			// rather than re-reads. That makes the model server a hard dependency of a backfill when
			// semantic search is configured - and a failure here aborts rather than continues,
			// because a half-embedded index is worse than no rebuild: it looks complete while
			// answering semantic queries from a fraction of the store.
			if cfg.Embed != nil && cfg.Embed.Enabled() && !memory.IsBinary && memory.Body != "" {
				vectors, err := cfg.Embed.Embed(ctx, []string{memory.Body})
				if err != nil {
					log.Fatalf(
						"failed to embed memory '%s' (%d indexed so far; the backfill is safe to rerun): %s",
						memory.Id,
						indexed,
						err.Error(),
					)
				}

				doc.Vector = vectors[0]
			}

			if err := idx.IndexMemorySync(ctx, doc); err != nil {
				log.Fatalf(
					"failed to index memory '%s' (%d indexed so far; the backfill is safe to rerun): %s",
					memory.Id,
					indexed,
					err.Error(),
				)
			}

			indexed++

			if indexed%10000 == 0 {
				log.Infof("indexed %d memories so far", indexed)
			}
		}

		afterId = memories[len(memories)-1].Id
	}

	log.Infof("backfill complete: indexed %d memories in %s", indexed, time.Since(started).Round(time.Millisecond))

	if err := idx.Close(); err != nil {
		log.Errorf("failed to close search index cleanly: %s", err.Error())
	}

	_ = database.Close()
}
