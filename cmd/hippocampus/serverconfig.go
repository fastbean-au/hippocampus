package main

import (
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/hippocampus"
)

// serverConfig builds the service's configuration from viper (TODO-3 item 177). Every key the
// hippocampus package acts on is read here, so that package reads no configuration of its own and
// its tests can build a server from a literal. authMethod is the scheme already resolved by the
// caller, with the deprecated auth.enabled taken into account.
func serverConfig(authMethod string) hippocampus.Config {
	// An unknown spelling has already failed startup (validateCallbackConfig), so the error here
	// cannot be reached with a policy that matters.
	backlogPolicy, _ := hippocampus.ParseBacklogPolicy(viper.GetString("callbacks.backlogPolicy"))

	var components []hippocampus.TopologyComponent

	if err := viper.UnmarshalKey("topology.components", &components); err != nil {
		// Not fatal, and deliberately so: a malformed list is already rejected at startup by
		// configProblems, so reaching here means the two disagree. Losing the declared half of a
		// diagnostic view is not a reason to take the store down with it.
		log.Errorf("failed to read topology.components: %s", err.Error())

		components = nil
	}

	driver := viper.GetString("storage.driver")

	var dsn string

	switch driver {

	case "postgres":
		dsn = viper.GetString("storage.postgres.dsn")

	case "mysql":
		dsn = viper.GetString("storage.mysql.dsn")

	}

	return hippocampus.Config{
		Port:        viper.GetInt("port"),
		SleepPeriod: seconds("sleep.periodSeconds"),

		Consolidation: hippocampus.ConsolidationConfig{
			Enabled:     viper.GetBool("consolidation.enabled"),
			Standby:     viper.GetBool("consolidation.standby"),
			StandbyPoll: seconds("consolidation.standbyPollSeconds"),

			Method:                   viper.GetInt("consolidation.method"),
			Aggressiveness:           viper.GetFloat64("consolidation.aggressiveness"),
			DeletionThreshold:        viper.GetFloat64("consolidation.deletionThreshold"),
			UnitsOfAgeInDays:         viper.GetFloat64("consolidation.unitsOfAgeInDays"),
			MinimumAgeInDays:         viper.GetInt("consolidation.minimumAgeInDays"),
			MinimumRetentionInDays:   viper.GetInt("consolidation.minimumRetentionInDays"),
			MaximumRetentionInDays:   viper.GetInt("consolidation.maximumRetentionInDays"),
			LinkSignificanceWeight:   viper.GetFloat64("consolidation.linkSignificanceWeight"),
			LinkRecallPropagation:    viper.GetFloat64("consolidation.linkRecallPropagation"),
			RecallSignificanceWeight: viper.GetFloat64("consolidation.recallSignificanceWeight"),

			DefaultEventSignificanceValue:      viper.GetInt32("consolidation.defaultEventSignificanceValue"),
			DefaultEventSignificancePercentile: viper.GetFloat64("consolidation.defaultEventSignificancePercentile"),

			CapacityMemories:           viper.GetInt("consolidation.capacityMemories"),
			CapacityPressureExponent:   viper.GetFloat64("consolidation.capacityPressureExponent"),
			CapacityBytes:              viper.GetInt64("consolidation.capacityBytes"),
			CapacityBytesFloor:         viper.GetInt64("consolidation.capacityBytesFloor"),
			CapacityExternalBytes:      viper.GetInt64("consolidation.capacityExternalBytes"),
			CapacityExternalBytesFloor: viper.GetInt64("consolidation.capacityExternalBytesFloor"),
			WALTriggerBytes:            viper.GetInt64("consolidation.walTriggerBytes"),

			SummarisationMinMemories:   viper.GetInt("consolidation.summarisationMinMemories"),
			SummarisationMinAgeInDays:  viper.GetInt("consolidation.summarisationMinAgeInDays"),
			SummarisationMaxCandidates: viper.GetInt("consolidation.summarisationMaxCandidates"),
			AutoSummarise:              viper.GetBool("llm.autoSummarise"),

			Tombstones:                 viper.GetBool("consolidation.tombstones.enabled"),
			SignificanceLevelRetention: days("consolidation.significanceLevels.unusedRetentionInDays"),
		},

		Transfer: hippocampus.TransferConfig{
			TargetAddress:         viper.GetString("transfer.targetAddress"),
			Token:                 viper.GetString("transfer.token"),
			TLS:                   transferTLSEnabled(),
			BatchSize:             viper.GetInt("transfer.batchSize"),
			MaxBatchBytes:         viper.GetInt("transfer.maxBatchBytes"),
			MaxManifestRows:       viper.GetInt("transfer.maxManifestRows"),
			KeyPrefix:             viper.GetString("s3.keyPrefix"),
			TLSCACertFile:         viper.GetString("transfer.tls.caCertFile"),
			TLSCertFile:           viper.GetString("transfer.tls.certFile"),
			TLSKeyFile:            viper.GetString("transfer.tls.keyFile"),
			TLSInsecureSkipVerify: viper.GetBool("transfer.tls.insecureSkipVerify"),
		},

		Listing: hippocampus.ListingConfig{
			CountCacheTTL: seconds("listing.countCacheSeconds"),
		},

		Write: hippocampus.WriteConfig{
			MinimumEventSignificance:  viper.GetInt32("event.minimumSignificance"),
			MinimumMemorySignificance: viper.GetInt32("memory.minimumSignificance"),
			MaxMemoryBytes:            viper.GetInt("memory.limit.sizeBytes"),
		},

		Search: hippocampus.SearchConfig{
			SignificanceWeight: viper.GetFloat64("search.significanceWeight"),
			RecallWeight:       viper.GetFloat64("search.recallWeight"),
		},

		Reconcile: hippocampus.ReconcileConfig{
			Interval:   seconds("opensearch.reconcileIntervalSeconds"),
			BatchSize:  viper.GetInt("opensearch.reconcileBatchSize"),
			StaleSweep: viper.GetBool("opensearch.staleSweep"),
		},

		Outbox: db.QueueBounds{
			MaxAge:   hours("opensearch.outbox.maxAgeHours"),
			MaxRows:  int64(viper.GetInt("opensearch.outbox.maxRows")),
			MaxBytes: viper.GetInt64("opensearch.outbox.maxBytes"),
		},

		Callbacks: hippocampus.CallbacksConfig{
			Bounds: db.QueueBounds{
				MaxAge:   hours("callbacks.maxAgeHours"),
				MaxRows:  int64(viper.GetInt("callbacks.maxRows")),
				MaxBytes: viper.GetInt64("callbacks.maxBytes"),
			},
			BatchSize:         viper.GetInt("callbacks.batchSize"),
			RetryBaseBackoff:  seconds("callbacks.retryBaseBackoffSeconds"),
			RetryMaxBackoff:   seconds("callbacks.retryMaxBackoffSeconds"),
			MaxIdsPerDelivery: viper.GetInt("callbacks.maxIdsPerDelivery"),
			BacklogPolicy:     backlogPolicy,

			SleepCompleted:  viper.GetBool("callbacks.events.sleepCompleted"),
			MemoriesAtRisk:  viper.GetBool("callbacks.events.memoriesAtRisk"),
			MemoryForgotten: viper.GetBool("callbacks.events.memoryForgotten"),
			EventForgotten:  viper.GetBool("callbacks.events.eventForgotten"),
			MemoryWrites:    viper.GetBool("callbacks.events.memoryWrites"),

			AtRiskLimit:  viper.GetInt("callbacks.atRiskLimit"),
			AtRiskMargin: viper.GetFloat64("callbacks.atRiskMargin"),

			AllDeletions:  viper.GetBool("callbacks.allDeletions"),
			IncludeBodies: viper.GetBool("callbacks.includeBodies"),
			MaxBodyBytes:  viper.GetInt("callbacks.maxBodyBytes"),
		},

		ScheduledExport: hippocampus.ScheduledExportConfig{
			Interval: hours("archive.scheduledExport.intervalHours"),
			Keep:     viper.GetInt("archive.scheduledExport.keep"),
		},

		Topology: hippocampus.TopologyConfig{
			Enabled:             viper.GetBool("topology.enabled"),
			MinimumTier:         viper.GetString("topology.minimumTier"),
			ProbeInterval:       seconds("topology.probeIntervalSeconds"),
			ProbeTimeout:        seconds("topology.probeTimeoutSeconds"),
			ProbeTransferTarget: viper.GetBool("topology.probeTransferTarget"),
			Heartbeat:           seconds("topology.heartbeatSeconds"),
			Components:          components,
		},

		Deployment: hippocampus.Deployment{
			BindAddress:        viper.GetString("bindAddress"),
			GatewayPort:        viper.GetInt("gateway.port"),
			GatewayBindAddress: viper.GetString("gateway.bindAddress"),
			TLSEnabled:         viper.GetBool("tls.enabled"),
			RateLimitEnabled:   viper.GetBool("rateLimit.enabled"),

			AuthMethod:  authMethod,
			AuthIssuer:  viper.GetString("auth.issuer"),
			AuthJWKSURL: viper.GetString("auth.jwksUrl"),

			StorageDriver:    driver,
			StorageDSN:       dsn,
			StorageDirectory: viper.GetString("storage.directory"),
			Compression:      viper.GetBool("storage.compression.enabled"),

			OpenSearchAddresses: viper.GetStringSlice("opensearch.addresses"),
			OpenSearchIndex:     viper.GetString("opensearch.index"),

			LLMProvider:         viper.GetString("llm.provider"),
			LLMAddress:          viper.GetString("llm.address"),
			LLMModel:            viper.GetString("llm.model"),
			EmbeddingProvider:   viper.GetString("llm.embedding.provider"),
			EmbeddingAddress:    viper.GetString("llm.embedding.address"),
			EmbeddingDimensions: viper.GetInt("llm.embedding.dimensions"),

			S3Endpoint: viper.GetString("s3.endpoint"),
			S3Bucket:   viper.GetString("s3.bucket"),
			S3Region:   viper.GetString("s3.region"),

			TracingEnabled: viper.GetBool("observability.tracing.enabled"),
			MetricsEnabled: viper.GetBool("observability.metrics.enabled"),
			OTLPEndpoint:   viper.GetString("observability.otlp.endpoint"),

			CallbacksEnabled:       viper.GetBool("callbacks.enabled"),
			CallbacksURL:           viper.GetString("callbacks.url"),
			CallbacksIncludeBodies: viper.GetBool("callbacks.includeBodies"),
			CallbacksAllDeletions:  viper.GetBool("callbacks.allDeletions"),
			CallbacksTokenSet:      viper.GetString("callbacks.token") != "",
			CallbacksSigned:        viper.GetString("callbacks.signingSecret") != "",
		},

		ReaderRecallReinforces: viper.GetBool("auth.readerRecallReinforces"),
	}
}

// resolveAuthMethod returns the authentication scheme in force: auth.method, or for a configuration
// that predates it the deprecated boolean auth.enabled - true is hmac, with a warning - and none
// otherwise. It is resolved once, here, so the listeners and the topology view cannot disagree.
func resolveAuthMethod() string {
	method := viper.GetString("auth.method")
	if method != "" {
		return method
	}

	if viper.GetBool("auth.enabled") {
		log.Warn("auth.enabled is deprecated - set auth.method to 'hmac' instead")

		return "hmac"
	}

	return "none"
}

// transferTLSEnabled reports whether the Transfer client should dial over TLS. It accepts both the
// legacy scalar form (transfer.tls: true) and the block form introduced with the trust options
// (transfer.tls.enabled: true), so existing configs keep working while the block gains caCertFile,
// certFile/keyFile, and insecureSkipVerify.
func transferTLSEnabled() bool {
	switch v := viper.Get("transfer.tls").(type) {

	case bool:
		return v

	default:
		return viper.GetBool("transfer.tls.enabled")

	}
}

// seconds, hours and days read an integer key in that unit as a duration.
func seconds(key string) time.Duration {
	return time.Duration(viper.GetInt(key)) * time.Second
}

func hours(key string) time.Duration {
	return time.Duration(viper.GetInt(key)) * time.Hour
}

func days(key string) time.Duration {
	return time.Duration(viper.GetInt(key)) * 24 * time.Hour
}
