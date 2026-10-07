package hippocampus

import (
	"time"

	"github.com/fastbean-au/hippocampus/db"
)

// Config is everything the service reads from its configuration, built by the caller - main.go,
// from viper - and handed to New (TODO-3 item 177). The package reads no configuration of its own,
// which is what lets a test build a server from a literal instead of through global viper state,
// and so what lets the package's tests run in parallel.
//
// It mirrors the configuration keys one for one, so a field's meaning is the key's; see
// docs/configuration.md. A zero field means what an unset key means. Defaults belong to the caller
// (setStartupDefaults), except where New has always applied its own fallback for a zero value.
//
// New keeps only what the server needs afterwards. Deployment in particular is used once, to
// describe the deployment in the topology view, and is not retained: it carries DSNs and endpoint
// URLs, which are redacted as the view is built so the server never holds a credential for that
// purpose.
type Config struct {
	// Port is the gRPC listener's port, which makes this instance's id unique on its host.
	Port int

	// SleepPeriod is sleep.periodSeconds; zero or negative disables the timed cycle.
	SleepPeriod time.Duration

	Consolidation   ConsolidationConfig
	Transfer        TransferConfig
	Listing         ListingConfig
	Write           WriteConfig
	Search          SearchConfig
	Reconcile       ReconcileConfig
	Outbox          db.QueueBounds
	Callbacks       CallbacksConfig
	ScheduledExport ScheduledExportConfig
	Topology        TopologyConfig
	Deployment      Deployment

	// ReaderRecallReinforces is auth.readerRecallReinforces.
	ReaderRecallReinforces bool
}

// ConsolidationConfig is the consolidation.* block, plus llm.autoSummarise, which is a sleep-cycle
// step.
type ConsolidationConfig struct {
	Enabled     bool
	Standby     bool
	StandbyPoll time.Duration

	Method                   int
	Aggressiveness           float64
	DeletionThreshold        float64
	UnitsOfAgeInDays         float64
	MinimumAgeInDays         int
	MinimumRetentionInDays   int
	MaximumRetentionInDays   int
	LinkSignificanceWeight   float64
	LinkRecallPropagation    float64
	RecallSignificanceWeight float64

	DefaultEventSignificanceValue      int32
	DefaultEventSignificancePercentile float64

	CapacityMemories           int
	CapacityPressureExponent   float64
	CapacityBytes              int64
	CapacityBytesFloor         int64
	CapacityExternalBytes      int64
	CapacityExternalBytesFloor int64
	WALTriggerBytes            int64

	SummarisationMinMemories   int
	SummarisationMinAgeInDays  int
	SummarisationMaxCandidates int
	AutoSummarise              bool

	// Tombstones is consolidation.tombstones.enabled.
	Tombstones bool

	// SignificanceLevelRetention is consolidation.significanceLevels.unusedRetentionInDays.
	SignificanceLevelRetention time.Duration
}

// TransferConfig is the transfer.* block plus s3.keyPrefix, which names the archives Export writes.
type TransferConfig struct {
	TargetAddress   string
	Token           string
	TLS             bool
	BatchSize       int
	MaxBatchBytes   int
	MaxManifestRows int
	KeyPrefix       string

	TLSCACertFile         string
	TLSCertFile           string
	TLSKeyFile            string
	TLSInsecureSkipVerify bool
}

// ListingConfig is the listing.* block.
type ListingConfig struct {
	// CountCacheTTL is listing.countCacheSeconds.
	CountCacheTTL time.Duration
}

// WriteConfig is what gates a write: event.minimumSignificance, memory.minimumSignificance and
// memory.limit.sizeBytes.
type WriteConfig struct {
	MinimumEventSignificance  int32
	MinimumMemorySignificance int32
	MaxMemoryBytes            int
}

// SearchConfig is the ranking weights, search.significanceWeight and search.recallWeight.
type SearchConfig struct {
	SignificanceWeight float64
	RecallWeight       float64
}

// ReconcileConfig is the OpenSearch reconciliation sweep: opensearch.reconcileIntervalSeconds,
// opensearch.reconcileBatchSize and opensearch.staleSweep.
type ReconcileConfig struct {
	Interval   time.Duration
	BatchSize  int
	StaleSweep bool
}

// CallbacksConfig is the callbacks.* block as far as the server's queue and dispatcher need it. The
// sink itself - its URL and credentials - is built by the caller and passed as
// Dependencies.Notifier.
type CallbacksConfig struct {
	Bounds            db.QueueBounds
	BatchSize         int
	RetryBaseBackoff  time.Duration
	RetryMaxBackoff   time.Duration
	MaxIdsPerDelivery int
	BacklogPolicy     BacklogPolicy

	SleepCompleted  bool
	MemoriesAtRisk  bool
	MemoryForgotten bool
	EventForgotten  bool
	MemoryWrites    bool

	AtRiskLimit  int
	AtRiskMargin float64

	AllDeletions  bool
	IncludeBodies bool
	MaxBodyBytes  int
}

// ScheduledExportConfig is archive.scheduledExport.*.
type ScheduledExportConfig struct {
	Interval time.Duration
	Keep     int
}

// TopologyConfig is the topology.* block.
type TopologyConfig struct {
	Enabled             bool
	MinimumTier         string
	ProbeInterval       time.Duration
	ProbeTimeout        time.Duration
	ProbeTransferTarget bool
	Heartbeat           time.Duration
	Components          []TopologyComponent
}

// Deployment describes the rest of the deployment for the topology view: settings the server does
// not act on but reports, so an operator need not read the configuration file to see them. Used
// once, while New builds the view, and never retained - see Config.
type Deployment struct {
	BindAddress        string
	GatewayPort        int
	GatewayBindAddress string
	TLSEnabled         bool
	RateLimitEnabled   bool

	// AuthMethod is the scheme in force - none, hmac or idp - with the deprecated auth.enabled
	// already resolved, so the view cannot disagree with what is enforced.
	AuthMethod  string
	AuthIssuer  string
	AuthJWKSURL string

	StorageDriver    string
	StorageDSN       string
	StorageDirectory string
	Compression      bool

	OpenSearchAddresses []string
	OpenSearchIndex     string

	LLMProvider         string
	LLMAddress          string
	LLMModel            string
	EmbeddingProvider   string
	EmbeddingAddress    string
	EmbeddingDimensions int

	S3Endpoint string
	S3Bucket   string
	S3Region   string

	TracingEnabled bool
	MetricsEnabled bool
	OTLPEndpoint   string

	CallbacksEnabled       bool
	CallbacksURL           string
	CallbacksIncludeBodies bool
	CallbacksAllDeletions  bool
	CallbacksTokenSet      bool
	CallbacksSigned        bool
}
