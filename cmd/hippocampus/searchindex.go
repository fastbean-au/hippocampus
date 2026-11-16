package main

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
)

// vectorDimensionFromViper reports the embedding width to map the k-NN field with, or 0 when
// semantic search is not configured. It reads llm.embedding.* rather than opensearch.* because
// the width is a property of the model, not of the cluster - the cluster merely has to be told it
// before the first vector arrives, since a k-NN mapping fixes its dimension at index creation.
func vectorDimensionFromViper() int {
	if !viper.GetBool("llm.embedding.enabled") {
		return 0
	}

	return viper.GetInt("llm.embedding.dimensions")
}

// searchConfigFromViper builds the OpenSearch client config from the opensearch.* viper keys,
// including the opensearch.tls.* transport-security block. Both the server bootstrap and the
// --backfill-search CLI mode use it, so the two paths stay in step.
func searchConfigFromViper() search.Config {

	return search.Config{
		Addresses: viper.GetStringSlice("opensearch.addresses"),
		Username:  viper.GetString("opensearch.username"),
		Password:  viper.GetString("opensearch.password"),
		Index:     viper.GetString("opensearch.index"),
		QueueSize: viper.GetInt("opensearch.queueSize"),

		// The k-NN field is only mapped when semantic search is on, so an index built for a
		// deployment without an embedder is exactly the index it always was.
		VectorDimension: vectorDimensionFromViper(),

		// Worker tuning; each 0 (the default) falls back to the search package's own default.
		ApplyTimeout:          time.Duration(viper.GetInt("opensearch.applyTimeoutSeconds")) * time.Second,
		ApplyMaxAttempts:      viper.GetInt("opensearch.applyMaxAttempts"),
		ApplyRetryBaseBackoff: time.Duration(viper.GetInt("opensearch.applyRetryBaseBackoffMillis")) * time.Millisecond,
		CloseDrainTimeout:     time.Duration(viper.GetInt("opensearch.closeDrainTimeoutSeconds")) * time.Second,

		TLS: search.TLSConfig{
			CACertFile:         viper.GetString("opensearch.tls.caCertFile"),
			CertFile:           viper.GetString("opensearch.tls.certFile"),
			KeyFile:            viper.GetString("opensearch.tls.keyFile"),
			InsecureSkipVerify: viper.GetBool("opensearch.tls.insecureSkipVerify"),
		},
	}
}

// searchIndexConfig is what initSearchIndex needs to choose and build the content-search index.
type searchIndexConfig struct {
	OpenSearchEnabled bool
	OpenSearch        search.Config

	// ContentIndex and ContentIndexReason are the store's own index decision (storeConfig's), which
	// decides what an unavailable index is reported as.
	ContentIndex       bool
	ContentIndexReason string
}

// setSearchDefaults applies the search.* and opensearch.* defaults.
func setSearchDefaults() {
	// Search result ranking. Both default to a mild influence rather than off: a store that knows
	// how significant each memory is, and how often it has been recalled, should use that when
	// deciding what to show first - text relevance alone is what a search engine without those
	// signals would do. Mild, because relevance still has to lead: at these weights significance
	// and recall reorder near-equal matches rather than override a clearly better one. Set both to
	// 0 for pure relevance order.
	viper.SetDefault("search.significanceWeight", 0.3)
	viper.SetDefault("search.recallWeight", 0.2)

	viper.SetDefault("opensearch.index", "hippocampus-memories")
	viper.SetDefault("opensearch.queueSize", 1024)
	viper.SetDefault("opensearch.reconcileIntervalSeconds", 3600)
	viper.SetDefault("opensearch.reconcileBatchSize", 500)

	// The reverse half of that sweep - removing index documents whose memory the store no longer
	// holds - defaults ON. It is the backstop for every way a delete can fail to reach the index,
	// and without it the divergence only ever grows: a live deployment was measured holding
	// twenty-one documents per row the store actually had. The outbox caps below bound the queue
	// when the index cannot keep up; reaching them is what hands work to this sweep.
	viper.SetDefault("opensearch.staleSweep", true)
	viper.SetDefault("opensearch.outbox.maxRows", 1000000)
	viper.SetDefault("opensearch.outbox.maxAgeHours", 24)
}

// searchProblems checks the opensearch.* keys.
func searchProblems() []error {
	var problems []error

	// A negative value is meaningless (0 disables the bound), and is refused rather than read as
	// unbounded.
	if maxBytes := viper.GetInt64("opensearch.outbox.maxBytes"); maxBytes < 0 {
		problems = append(problems, fmt.Errorf("opensearch.outbox.maxBytes must not be negative, got %d", maxBytes))
	}

	return problems
}

// searchIndexConfigFromViper reads the search index's configuration. The content-index decision is
// passed in rather than read again, so the store and its search index cannot disagree about it.
func searchIndexConfigFromViper(contentIndex bool, contentIndexReason string) searchIndexConfig {
	log.Trace("func() searchIndexConfigFromViper")

	cfg := searchIndexConfig{
		OpenSearchEnabled:  viper.GetBool("opensearch.enabled"),
		ContentIndex:       contentIndex,
		ContentIndexReason: contentIndexReason,
	}

	if cfg.OpenSearchEnabled {
		cfg.OpenSearch = searchConfigFromViper()
	}

	return cfg
}

// initSearchIndex builds the secondary content-search index. OpenSearch when it is configured;
// otherwise the store's own index, which needs no configuration and no cluster, so SearchMemories
// works out of the box on every driver. A store that cannot carry one leaves the no-op in place, and
// that is logged rather than left to be discovered through an empty search result. Construction of
// the OpenSearch client only fails on unusable configuration (e.g. a malformed address) - an
// unreachable cluster must not prevent startup, since that index is best-effort by design.
//
// The index is registered as the store's delete observer before it is returned. The caller closes
// it.
func initSearchIndex(cfg searchIndexConfig, database *db.DB) (search.Index, error) {
	log.Trace("func() initSearchIndex")

	searchIndex := search.NewNoop()

	switch {

	case cfg.OpenSearchEnabled:
		log.Debug("initialising opensearch")

		idx, err := search.NewOpenSearch(cfg.OpenSearch)
		if err != nil {
			return nil, fmt.Errorf("failed to initialise opensearch: %w", err)
		}

		searchIndex = idx

		log.Debug("opensearch initialised")

	default:
		idx, err := search.NewSQL(database)
		if err != nil {
			// Two ways to arrive here, and they want different advice. One is a store that cannot
			// carry the index; the other is an operator who turned it off and left OpenSearch off
			// too, which is a coherent choice - a store nobody searches by content - but leaves
			// SearchMemories refusing, so it is named rather than reported as a fault.
			if !cfg.ContentIndex {
				log.Warnf(
					"content search is unavailable, so SearchMemories will be rejected: the store keeps no content-search index (%s) and opensearch.enabled is false",
					cfg.ContentIndexReason,
				)

				break
			}

			log.Warnf(
				"content search is unavailable, so SearchMemories will be rejected: %s (enable opensearch.enabled to add it)",
				err.Error(),
			)

			break
		}

		searchIndex = idx

		log.Debug("content search initialised on the primary store")

	}

	// Consolidation and eviction delete memories inside the db layer, where the RPC-level
	// write-through hooks never see them; the observer closes that gap.
	database.SetMemoryDeleteObserver(searchIndex.DeleteMemories)

	return searchIndex, nil
}
