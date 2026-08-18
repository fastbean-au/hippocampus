package hippocampus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/archive"
	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/summarise"
)

// sleepSingleflightKey is the sole key used with Server.sleepGroup: every caller wanting a sleep
// cycle joins the same in-flight call rather than starting a concurrent one.
const sleepSingleflightKey = "sleep"

// The three things that can start a sleep cycle, as reported by CycleReport.trigger. They are
// distinguishable because they mean different things operationally: a run of "wal" triggers says
// the write rate is outpacing the schedule, and a store consolidating only on "manual" is one
// whose timed cycle is disabled or wedged.
const (
	triggerTimer  = "timer"
	triggerManual = "manual"
	triggerWAL    = "wal"
)

// hippocampusServicePrefix scopes the purge gate to the Hippocampus service, mirroring the same
// check in the auth interceptor and the RPC metrics: the health service must stay answerable while
// a purge runs. It is the proto package plus the service name, so it changes whenever the proto's
// package does - TestServicePrefixMatchesDescriptor holds it to the generated descriptor.
const hippocampusServicePrefix = "/hippocampus.v1.Hippocampus/"

// mapError is the RPC layer's final error-mapping seam: it turns a storage-layer error into an
// appropriate gRPC status before it reaches a client on either transport. It is applied at handler
// return sites (not as an interceptor) because the HTTP gateway calls these handlers directly and
// never runs the gRPC interceptor chain, so mapping there covers both transports at once.
//
// Known cases get a precise, correctly-coded status; anything else is masked as codes.Internal with
// a generic message and the real detail logged server-side. This keeps a raw driver string (schema
// and column names, SQL text) from leaking to a caller - which previously happened because most
// handlers returned storage errors unwrapped, surfacing as a gRPC Unknown carrying driver text (and
// an HTTP 500 body via the gateway).
//
//   - nil                     -> nil
//   - an existing gRPC status -> unchanged (handlers set InvalidArgument/NotFound/... deliberately)
//   - context canceled        -> Canceled
//   - context deadline        -> DeadlineExceeded
//   - db.IsDuplicateKey       -> AlreadyExists (generic message; the constraint detail is logged)
//   - db.IsWriteConflict      -> Aborted (retryable: a MySQL deadlock/lock-wait that outlived the
//     driver's retries, which clients should re-issue rather than read as a lost write)
//   - anything else           -> Internal (generic message; detail logged server-side only)
func mapError(err error) error {
	if err == nil {
		return nil
	}

	// A handler that already produced a status (validation, not-found, precondition, ...) said
	// exactly what it meant; pass it through untouched. status.FromError unwraps a %w-wrapped status.
	if _, ok := status.FromError(err); ok {
		return err
	}

	switch {

	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")

	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")

	case db.IsDuplicateKey(err):
		log.Warnf("duplicate-key write rejected: %s", err.Error())

		return status.Error(codes.AlreadyExists, "a record with that id already exists")

	case db.IsWriteConflict(err):
		return status.Error(codes.Aborted, "write conflict, retry the request")

	default:
		log.Errorf("internal error handling request: %s", err.Error())

		return status.Error(codes.Internal, "internal error")

	}
}

// mapWriteError is the narrower mapper used only by the Export/Transfer/Clear clear-failure paths,
// where the returned error is a deliberate, admin-facing operational message that must reach the
// caller intact: it names the manifest id to retry the delete with (see items 19.7/23.3) and stays
// errors.Is-comparable to the underlying failure. So, unlike mapError, it does not mask - it maps a
// storage write conflict to a retryable Aborted and otherwise returns err unchanged. These are
// admin-only operations (clear rights are effectively admin rights), so the trade-off of a more
// detailed error against the general surface's masking is deliberate.
func mapWriteError(err error) error {
	if err == nil {
		return nil
	}

	if db.IsWriteConflict(err) {
		return status.Error(codes.Aborted, err.Error())
	}

	return err
}

// walCheckInterval is how often autoSleep polls the on-disk WAL size when
// consolidation.walTriggerBytes is configured. It reads the filesystem directly rather than the
// database, so polling it far more often than sleep.periodSeconds costs nothing. A var (not const)
// so tests can shorten it.
var walCheckInterval = 5 * time.Second

type Consolidation struct {
	defaultEventSignificanceValue      int32
	defaultEventSignificancePercentile float64
	minimumAgeInDays                   int
	minimumRetentionInDays             int
	aggressiveness                     float64
	deletionThreshold                  float64
	method                             int
	unitsOfAgeInDays                   float64
	linkSignificanceWeight             float64
	recallSignificanceWeight           float64

	// linkRecallPropagation is spreading activation: the fraction of the way toward "just recalled"
	// that a recalled memory's direct neighbours have their decay clocks advanced. 0 (the default)
	// disables it, so recall reinforces only what was actually asked for; 1 would reinforce a
	// neighbour as strongly as the memory itself, which is why the sensible settings are small.
	// Never touches recall_count - see db.ReinforceLinkedMemories.
	linkRecallPropagation    float64
	capacityMemories         int
	capacityPressureExponent float64
	capacityPressure         float64
	capacityBytes            int64
	capacityBytesFloor       int64
	// lastUsedBytes caches the used-bytes reading eviction took at the end of the previous sleep
	// cycle, so the next cycle's capacity-pressure calculation can reuse it instead of scanning the
	// tables a second time. Written and read only from the sleep cycle, which
	// singleflight serialises, so it needs no lock.
	lastUsedBytes              int64
	walTriggerBytes            int64
	summarisationMinMemories   int
	summarisationMinAgeInDays  int
	summarisationMaxCandidates int

	// autoSummarise (ollama.autoSummarise) makes the sleep cycle summarise the candidates the scan
	// identifies with the embedded LLM, instead of only surfacing them via
	// GetSummarisationCandidates for a client to summarise. It has effect only when a real
	// summariser is configured (ollama.enabled) and the candidate scan is enabled
	// (summarisationMinMemories > 0). Off by default: enabling the LLM must not silently start
	// rewriting stored memories.
	autoSummarise bool

	// tombstones (consolidation.tombstones.enabled) mirrors the storage layer's forgotten-log
	// policy, which is where the feature actually lives (db/tombstone.go). The RPC layer keeps its
	// own copy of the one flag because GetForgottenMemories has to report whether the service is
	// recording: an empty log otherwise cannot be told from a log nobody is writing.
	tombstones bool
}

type Server struct {
	contract.UnimplementedHippocampusServer
	db db.Store

	// search is the optional secondary content-search index; nil (as in tests constructing a
	// Server directly) behaves as the disabled no-op via searchIdx().
	search search.Index

	// summarise is the optional embedded-LLM summariser backing SummariseMemories and the sleep
	// cycle's auto-summarisation; nil (as in tests constructing a Server directly) behaves as the
	// disabled no-op via summariser().
	summarise summarise.Summariser

	// embed is the optional text embedder backing semantic search; nil (as in tests constructing a
	// Server directly) behaves as the disabled no-op via embedder().
	embed embed.Embedder

	// purgeInProgress is written by Purge and read by InterceptorBlockWhenPurgeInProgress from
	// every RPC's own goroutine, so it must be an atomic rather than a plain bool.
	purgeInProgress atomic.Bool

	// nextSleep is when the timed cycle is next due (UnixNano), or 0 when none is scheduled - a
	// non-positive sleep.periodSeconds, or after Stop. Written only by the autoSleep goroutine and
	// read by GetConsolidationStatus from each caller's own goroutine, so it is an atomic for the
	// same reason purgeInProgress is.
	//
	// It is NOT a prediction of the next time anything happens: checkWALTrigger deliberately does
	// not reset the timer, so an out-of-cycle WAL-triggered sleep can run before this without
	// changing it. GetConsolidationStatus reports wal_trigger_enabled alongside so a client can say
	// "or sooner" rather than implying the schedule is the only thing that can fire.
	nextSleep atomic.Int64

	// sleepInProgress is set for the duration of a cycle, inside sleepOnce's singleflight closure,
	// so a caller that JOINED a running cycle sees the same true a caller that started one does.
	sleepInProgress atomic.Bool

	// sleepPeriod is sleep.periodSeconds as a duration, kept for GetConsolidationStatus to report.
	// Non-positive means no timed cycle at all - a supported mode for an instance driven only by the
	// Sleep RPC or the WAL trigger.
	sleepPeriod time.Duration

	// lastCycle is what the most recent completed cycle did, or nil until one has run in this
	// process. An atomic.Pointer rather than a mutex because it is written once per cycle, read on
	// every status poll, and immutable after publication - so readers never block the sleep
	// goroutine and never see a half-filled report.
	//
	// Deliberately in memory only. A figure surviving the process that produced it would describe a
	// cycle run under configuration that may since have changed.
	lastCycle atomic.Pointer[cycleReport]

	sleepReset                chan bool
	minimumEventSignificance  int32
	minimumMemorySignificance int32
	maxMemoryBodyLength       int
	consolidation             Consolidation

	// readerRecallReinforces (auth.readerRecallReinforces) decides whether a reader-tier caller's
	// RecallMemories / reinforcing SearchMemories actually reinforces the memories or is downgraded
	// to a plain read. Writer/admin callers always reinforce; when authorisation is not in effect
	// (auth disabled) recall reinforces as it always has. See mayReinforce.
	readerRecallReinforces bool

	// ranking (search.significanceWeight / search.recallWeight) blends the store's own view of a
	// memory's worth into SearchMemories' result order. The zero value leaves the search backend's
	// relevance order untouched, which is how a Server constructed directly in a test behaves.
	ranking rankingWeights

	// consolidationEnabled reflects consolidation.enabled: true (the default) means this instance
	// runs the sleep cycle - the timed loop, the WAL trigger, and the manual Sleep RPC. False makes
	// it a read/write replica in a horizontally scaled deployment: New starts no sleep
	// route and Sleep rejects the RPC, and main.go correspondingly opens the shared database without
	// the single-consolidator lock.
	consolidationEnabled bool

	// sleepGroup ensures the autoSleep timer and manual Sleep RPCs never run sleep() concurrently
	// with each other: a caller arriving while a cycle is already in flight joins it and shares
	// its result instead of starting a second, overlapping cycle.
	sleepGroup singleflight.Group

	// previewGroup collapses concurrent PreviewConsolidation calls asking for the same sample size
	// onto one scan. It is deliberately a SEPARATE group from sleepGroup: a preview must never join
	// a real cycle (it would be describing a run that is at that moment deleting), so the two
	// cannot share a key, and the preview's independence from the cycle is the whole design. What
	// this group protects against is the other direction - a stream of concurrent previews each
	// running its own full scan and, on SQLite's deliberately single connection, crowding the
	// cycle's own queries.
	previewGroup singleflight.Group

	// explainState caches the decision snapshot ExplainConsolidation values memories against, and
	// explainGroup collapses concurrent refreshes of it onto one. Unlike a preview - occasional, and
	// asked precisely when someone wants the current truth - this RPC is called once per console
	// page, and the snapshot costs two full scans to compute; see cachedDecisionSnapshot. Guarded by
	// explainStateMu, since it is written from whichever RPC goroutine happens to find it stale.
	explainState   decisionState
	explainStateMu sync.Mutex
	explainGroup   singleflight.Group

	// stopSleep / sleepStopped / stopOnce coordinate shutdown of the autoSleep goroutine. Stop
	// closes stopSleep and waits for sleepStopped; because the loop only re-enters its select
	// between cycles, that wait also drains any in-flight cycle, so no consolidation is mid-scan
	// when the database is closed next. nil when the server was built without New (some tests).
	stopSleep    chan struct{}
	sleepStopped chan struct{}
	stopOnce     sync.Once

	// summarisationCandidates is refreshed by the sleep cycle and read by
	// GetSummarisationCandidates, so access is guarded by summarisationCandidatesMu.
	summarisationCandidates   []db.SummarisationCandidate
	summarisationCandidatesMu sync.RWMutex

	// reconcileInterval / reconcileBatchSize configure the periodic search-index reconciliation
	// sweep (reconcile.go): the sweep re-indexes the primary store so any document a dropped,
	// crashed, or timed-out index operation missed is healed on its own. A non-positive interval
	// disables it. stopReconcile / reconcileStopped coordinate its shutdown exactly as
	// stopSleep / sleepStopped do for autoSleep; both are nil when the sweep is not running (search
	// disabled, this is a replica, or the interval is non-positive).
	reconcileInterval  time.Duration
	reconcileBatchSize int
	stopReconcile      chan struct{}
	reconcileStopped   chan struct{}

	// objects is the optional S3 object store backing the Export/Import RPCs; nil (s3.bucket not
	// configured) makes both fail with FAILED_PRECONDITION.
	objects archive.ObjectStore

	// topology is the deployment view's configuration plus the pre-redacted, immutable description
	// of what this instance is attached to; topologyProbes is the background prober's latest round
	// of statuses, and stopTopology / topologyStopped coordinate its shutdown exactly as
	// stopSleep / sleepStopped do for autoSleep. Both channels are nil when nothing is being probed.
	//
	// An atomic.Pointer rather than a mutex for the same reason lastCycle is one: it is written once
	// per round, read on every view, and the map is replaced wholesale and never mutated after
	// publication - so a reader never blocks the prober and never sees a half-written round.
	topology        Topology
	topologyProbes  atomic.Pointer[map[string]topologyProbeResult]
	stopTopology    chan struct{}
	topologyStopped chan struct{}

	// peers is the instance registry's latest round (see peers.go): the other instances sharing this
	// store, the store attributes describing the set of them, and any deployment-wide warning. Held
	// the same way and for the same reasons as topologyProbes; nil until the first round, and
	// permanently where there is no registry (SQLite, or topology.heartbeatSeconds 0).
	// stopHeartbeat / heartbeatStopped coordinate its goroutine's shutdown, and are nil when it is
	// not running.
	peers            atomic.Pointer[peerSnapshot]
	stopHeartbeat    chan struct{}
	heartbeatStopped chan struct{}

	// lastWarnings is what deploymentWarnings reported on the previous round, so a warning is logged
	// when it appears and when it clears rather than on every round. Touched only by the heartbeat
	// goroutine, so it needs no lock.
	lastWarnings []string

	// transfer carries the Transfer RPC's target settings and the page/batch size shared by all
	// export paths.
	transfer Transfer

	// manifests holds what recent Export/Transfer runs captured, keyed by manifest id, so Clear
	// can delete exactly those records. In-memory only: a restart discards them, and the oldest
	// are evicted beyond manifestCacheLimit. Guarded by manifestsMu.
	manifests   map[string]*transferManifest
	manifestIds []string
	manifestsMu sync.Mutex
}

type Transfer struct {
	targetAddress   string
	token           string
	tls             bool
	batchSize       int
	maxBatchBytes   int
	maxManifestRows int
	keyPrefix       string

	// TLS trust options mirroring the opensearch.tls block, so a transfer to a target serving a
	// private-CA or mutual-TLS certificate can verify it. All empty/false by default, in which case
	// TLS (when enabled) verifies against the system certificate pool, the previous behaviour.
	tlsCACertFile         string
	tlsCertFile           string
	tlsKeyFile            string
	tlsInsecureSkipVerify bool
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

// mayReinforce reports whether the caller behind ctx may reinforce recalled memories (reset the
// decay clock, raise the recall count). Writer and admin tiers always may; a reader may only when
// auth.readerRecallReinforces is set. When no tier is on the context - authorisation is not in
// effect because authentication is disabled - recall reinforces as it always has, so an unsecured
// instance is unchanged.
func (s *Server) mayReinforce(ctx context.Context) bool {
	tier, ok := auth.TierFromContext(ctx)

	if !ok {
		return true
	}

	if tier >= auth.TierWriter {
		return true
	}

	return s.readerRecallReinforces
}

// Dependencies carries the collaborators a Server is built over. It is a struct rather than a
// parameter list because the optional ones only accumulate - a search index, an object store, a
// summariser, an embedder - and each addition would otherwise lengthen a signature every call site
// has to restate, including the ones that want none of them.
//
// Every field but DB is optional and nil-safe: the accessors (searchIdx, summariser, embedder)
// substitute the package's no-op implementation, so a Server built with only a store behaves as a
// deployment with none of the optional components configured. That is what lets tests construct
// one with a single field set.
type Dependencies struct {
	// DB is the primary store, and the only required dependency.
	DB db.Store

	// Search is the secondary content-search index (nil -> disabled).
	Search search.Index

	// Objects is the archive object store backing Export/Import/Transfer (nil -> unconfigured).
	Objects archive.ObjectStore

	// Summariser is the optional embedded LLM used to condense an event's memories (nil ->
	// disabled).
	Summariser summarise.Summariser

	// Embedder is the optional text embedder backing semantic search (nil -> disabled).
	Embedder embed.Embedder

	// Version is the build identification main.go already derived (runtime/debug.ReadBuildInfo),
	// passed in rather than re-read here because the -ldflags override that makes a release report
	// its tag lives in package main and nowhere else. Reported by GetTopology on the self node;
	// empty is harmless.
	Version string
}

func New(deps Dependencies) *Server {
	log.Trace("func() hippocampus.New()")

	reset := make(chan bool, 1)

	s := &Server{
		db:        deps.DB,
		search:    deps.Search,
		summarise: deps.Summariser,
		embed:     deps.Embedder,
		objects:   deps.Objects,
		manifests: make(map[string]*transferManifest),
		transfer: Transfer{
			targetAddress:         viper.GetString("transfer.targetAddress"),
			token:                 viper.GetString("transfer.token"),
			tls:                   transferTLSEnabled(),
			batchSize:             viper.GetInt("transfer.batchSize"),
			maxBatchBytes:         viper.GetInt("transfer.maxBatchBytes"),
			maxManifestRows:       viper.GetInt("transfer.maxManifestRows"),
			keyPrefix:             viper.GetString("s3.keyPrefix"),
			tlsCACertFile:         viper.GetString("transfer.tls.caCertFile"),
			tlsCertFile:           viper.GetString("transfer.tls.certFile"),
			tlsKeyFile:            viper.GetString("transfer.tls.keyFile"),
			tlsInsecureSkipVerify: viper.GetBool("transfer.tls.insecureSkipVerify"),
		},
		sleepReset:                reset,
		minimumEventSignificance:  viper.GetInt32("event.minimumSignificance"),
		minimumMemorySignificance: viper.GetInt32("memory.minimumSignificance"),
		maxMemoryBodyLength:       viper.GetInt("memory.limit.sizeBytes"),
		readerRecallReinforces:    viper.GetBool("auth.readerRecallReinforces"),
		ranking: rankingWeights{
			significance: viper.GetFloat64("search.significanceWeight"),
			recall:       viper.GetFloat64("search.recallWeight"),
		},
		consolidation: Consolidation{
			defaultEventSignificanceValue:      viper.GetInt32("consolidation.defaultEventSignificanceValue"),
			defaultEventSignificancePercentile: viper.GetFloat64("consolidation.defaultEventSignificancePercentile"),
			minimumAgeInDays:                   viper.GetInt("consolidation.minimumAgeInDays"),
			minimumRetentionInDays:             viper.GetInt("consolidation.minimumRetentionInDays"),
			aggressiveness:                     viper.GetFloat64("consolidation.aggressiveness"),
			deletionThreshold:                  viper.GetFloat64("consolidation.deletionThreshold"),
			method:                             viper.GetInt("consolidation.method"),
			unitsOfAgeInDays:                   viper.GetFloat64("consolidation.unitsOfAgeInDays"),
			linkSignificanceWeight:             viper.GetFloat64("consolidation.linkSignificanceWeight"),
			linkRecallPropagation:              viper.GetFloat64("consolidation.linkRecallPropagation"),
			recallSignificanceWeight:           viper.GetFloat64("consolidation.recallSignificanceWeight"),
			capacityMemories:                   viper.GetInt("consolidation.capacityMemories"),
			capacityPressureExponent:           viper.GetFloat64("consolidation.capacityPressureExponent"),
			capacityPressure:                   1.0,
			capacityBytes:                      viper.GetInt64("consolidation.capacityBytes"),
			capacityBytesFloor:                 viper.GetInt64("consolidation.capacityBytesFloor"),
			walTriggerBytes:                    viper.GetInt64("consolidation.walTriggerBytes"),
			summarisationMinMemories:           viper.GetInt("consolidation.summarisationMinMemories"),
			summarisationMinAgeInDays:          viper.GetInt("consolidation.summarisationMinAgeInDays"),
			summarisationMaxCandidates:         viper.GetInt("consolidation.summarisationMaxCandidates"),
			autoSummarise:                      viper.GetBool("ollama.autoSummarise"),
			tombstones:                         viper.GetBool("consolidation.tombstones.enabled"),
		},
	}

	s.consolidationEnabled = viper.GetBool("consolidation.enabled")

	// Mirror the server-side auth-without-TLS warning for the Transfer client: a token configured
	// without transfer.tls is sent as a plaintext bearer credential to the target, where anyone on
	// the wire can lift it. Warn rather than fail - TLS may be terminated by a sidecar/mesh between
	// this instance and the target - but make the exposure visible in the logs.
	if s.transfer.token != "" && !s.transfer.tls {
		log.Warn("transfer.token is configured without transfer.tls: the bearer token will be sent in plaintext to the transfer target")
	}

	s.stopSleep = make(chan struct{})
	s.sleepStopped = make(chan struct{})

	period := time.Duration(viper.GetInt("sleep.periodSeconds")) * time.Second

	// Kept on the server so GetConsolidationStatus can report the schedule this instance is on.
	// Recorded before the replica branch below zeroes it, since what a replica reports is
	// consolidation_enabled false rather than a period of zero.
	s.sleepPeriod = period

	if !s.consolidationEnabled {
		// Read/write replica: no sleep route runs on this instance. Zeroing the period
		// drops the timed case out of autoSleep's select, and zeroing walTriggerBytes stops it from
		// setting up the WAL-size poll; the manual Sleep RPC is rejected in Sleep(). autoSleep is
		// still started so Stop() has a goroutine to drain, keeping shutdown uniform.
		log.Info("consolidation.enabled is false: this instance runs no sleep cycles (read/write only); another instance must run consolidation against the shared database")

		period = 0
		s.consolidation.walTriggerBytes = 0
	}

	s.autoSleep(reset, period)

	s.startReconcile(deps.Search)

	// Built last, so the specs describe the server as it finally is - the search backend in
	// particular is only decided by the dependency that was handed in, and the reconcile interval is
	// resolved a few lines above this.
	s.topology = topologyFromViper(deps.Version)
	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	s.startTopologyProber()

	// After the prober, because it is the half of the view that describes instances this process has
	// never spoken to - and it needs the topology config the two lines above resolved.
	s.startInstanceHeartbeat()

	return s
}

// startReconcile launches the periodic search-index reconciliation sweep when it is warranted: a
// real search index is configured, this is the single consolidating instance (so replicas do not
// duplicate the sweep, and there is exactly one owner of index maintenance), and a positive
// opensearch.reconcileIntervalSeconds is set. Otherwise it is a no-op and Stop has nothing extra to
// drain. See reconcile.go.
func (s *Server) startReconcile(searchIndex search.Index) {
	s.reconcileInterval = time.Duration(viper.GetInt("opensearch.reconcileIntervalSeconds")) * time.Second
	s.reconcileBatchSize = viper.GetInt("opensearch.reconcileBatchSize")

	if s.reconcileBatchSize <= 0 {
		s.reconcileBatchSize = defaultReconcileBatchSize
	}

	if !s.consolidationEnabled || s.reconcileInterval <= 0 || searchIndex == nil || !searchIndex.Enabled() {
		return
	}

	s.stopReconcile = make(chan struct{})
	s.reconcileStopped = make(chan struct{})

	go s.reconcileLoop()
}

// Stop shuts the autoSleep goroutine down (and the search-index reconciliation sweep and the
// deployment-topology prober, when running) and waits for them to exit. Because the sleep loop only re-enters its select between cycles,
// that wait also drains any sleep cycle already in flight (started by the timer or the WAL trigger
// just before shutdown), so nothing is mid-consolidation when the caller closes the database next.
// Safe to call more than once, and a no-op when the server was built without New (autoSleep never
// started). Call it after the gRPC server's GracefulStop (which drains RPC-initiated cycles) and
// before closing the database.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		// First, so this instance leaves the shared registry while the database is still open: a
		// clean exit should disappear from its peers' views immediately rather than be reported
		// unreachable for several intervals, which is what an instance that could not say so gets.
		s.stopInstanceHeartbeat()

		s.stopTopologyProber()

		if s.stopReconcile != nil {
			close(s.stopReconcile)
			<-s.reconcileStopped
		}

		if s.stopSleep == nil {
			return
		}

		close(s.stopSleep)
		<-s.sleepStopped
	})
}

func (s *Server) autoSleep(reset chan bool, period time.Duration) {
	log.Debug("starting autoSleep")

	if period <= 0 {
		log.Info("sleep.periodSeconds <= 0: automatic timed sleep cycles are disabled (manual Sleep RPC and any WAL trigger still run)")
	}

	// Recorded here rather than beside the timer below, because the timer is created inside the
	// goroutine: a status poll arriving in the window before that goroutine is scheduled would
	// otherwise see 0 and report "no timed cycle" on an instance that has one. The few microseconds
	// this is early by are immaterial to a countdown displayed in seconds.
	if period > 0 {
		s.nextSleep.Store(time.Now().Add(period).UnixNano())
	}

	go func() {
		defer close(s.sleepStopped)

		// Nothing is due once this goroutine has gone, so a status poll racing shutdown must not
		// report a fire that will never come.
		defer s.nextSleep.Store(0)

		// A nil channel blocks forever, so leaving walCheck nil when the feature is disabled
		// cleanly drops that case out of the select below.
		var walCheck <-chan time.Time

		if s.consolidation.walTriggerBytes > 0 {
			ticker := time.NewTicker(walCheckInterval)
			defer ticker.Stop()

			walCheck = ticker.C
		}

		// The timed cycle uses a single long-lived timer, reset after each fire, not a fresh
		// time.After per loop iteration. Recreating it every iteration meant the walCheck ticker -
		// firing every walCheckInterval, more often than the period - restarted the countdown before
		// it could elapse, so with walTriggerBytes enabled the timed cycle never fired. A
		// non-positive period leaves sleepCh nil (timed sleep disabled), blocking that case forever.
		var sleepCh <-chan time.Time
		var timer *time.Timer

		if period > 0 {
			timer = time.NewTimer(period)
			defer timer.Stop()

			sleepCh = timer.C
		}

		// resetTimer is the single chokepoint for every restart of the countdown - after a cycle
		// fires, and on the sleepReset nudge a manual Sleep sends - which is why nextSleep is
		// recorded here rather than at those call sites. A non-positive period leaves timer nil and
		// returns early, so nextSleep stays 0 and a client renders "no timed cycle" rather than a
		// countdown to nothing.
		resetTimer := func() {
			if timer == nil {
				return
			}

			if !timer.Stop() {
				select {

				case <-timer.C:
				default:
				}
			}

			timer.Reset(period)
			s.nextSleep.Store(time.Now().Add(period).UnixNano())
		}

		for {

			// Priority check: if Stop signalled shutdown while the previous cycle was running, exit
			// before starting another one, even when the timer is also ready (a tiny period makes the
			// timer fire immediately, so the main select alone could keep looping).
			select {

			case <-s.stopSleep:
				return

			default:
			}

			select {

			case <-s.stopSleep:
				return

			case <-reset:
				resetTimer()

				continue

			case <-sleepCh:
				_ = s.sleepOnce(triggerTimer)
				resetTimer()

			case <-walCheck:
				s.checkWALTrigger()
			}
		}
	}()
}

// checkWALTrigger runs an out-of-cycle sleep when the on-disk WAL has grown past
// consolidation.walTriggerBytes, so the checkpoint at the end of every sleep cycle runs sooner
// than the next timed cycle instead of letting the WAL keep accumulating between them.
func (s *Server) checkWALTrigger() {
	walBytes, err := s.db.WALBytes()
	if err != nil {
		log.Warnf("failed to read WAL size for the trigger check: %s", err.Error())

		return
	}

	if walBytes < s.consolidation.walTriggerBytes {
		return
	}

	log.Infof(
		"WAL size %d bytes exceeds trigger threshold %d bytes, triggering an out-of-cycle sleep",
		walBytes,
		s.consolidation.walTriggerBytes,
	)

	_ = s.sleepOnce(triggerWAL)
}

// sleepOnce runs a sleep cycle via sleepGroup, so a call arriving while one is already in flight
// (from the autoSleep timer or a concurrent Sleep RPC) joins it and shares its result rather than
// starting a second, overlapping cycle.
//
// trigger names what this call would start the cycle FOR. It reaches the recorded report only when
// this call is the one that runs it: a caller that joins an in-flight cycle shares that cycle's
// result, and the report describes the cycle that ran rather than the call that observed it.
func (s *Server) sleepOnce(trigger string) error {
	_, err, _ := s.sleepGroup.Do(sleepSingleflightKey, func() (any, error) {
		// Set inside the closure, so it covers exactly the cycle and is true for a caller that
		// joined one as much as for the caller that started it.
		s.sleepInProgress.Store(true)
		defer s.sleepInProgress.Store(false)

		return nil, s.sleep(trigger)
	})

	return err
}

// =============================================================================
// Other
// =============================================================================

func (s *Server) Sleep(ctx context.Context, in *contract.EmptyRequest) (*contract.GeneralResponse, error) {
	log.Debug("Sleep()")
	var res contract.GeneralResponse

	// A read/write replica must never run a consolidation cycle: it does not hold the
	// single-consolidator lock, so letting it sleep would race the consolidating instance against
	// shared data. Reject the RPC rather than silently no-op, so a misdirected call is
	// visible to the caller.
	if !s.consolidationEnabled {
		return &res, status.Error(codes.FailedPrecondition, "consolidation is disabled on this instance")
	}

	// The cycle is store-global - one capacity target, one pressure, one eviction ranking across
	// every group - so there is no such thing as sleeping a partition. See hippocampus/scope.go.
	if err := s.requireUnbound(ctx, "Sleep"); err != nil {
		return &res, err
	}

	err := s.sleepOnce(triggerManual)
	if err == nil {

		// Nudge the autoSleep timer to restart its interval. Non-blocking: the buffer holds one
		// pending reset, so if a reset is already queued (or autoSleep is mid-cycle and not yet
		// reading), dropping this one is harmless - the timer keeps its existing schedule.
		select {
		case s.sleepReset <- true:
		default:
		}

		res.Ok = true
	}

	return &res, mapError(err)
}

// Purge deletes all events and memories. Any error is returned to the caller; a subsequent purge
// can be attempted.
//
// purgeInProgress blocks RPCs that arrive after the purge begins, but a write already past the
// interceptor when Purge runs can commit after the DELETE, so a row written concurrently with a
// Purge may survive it. This is deliberate - Purge is not a barrier and does not drain in-flight
// writes; run it when writers are quiesced if an empty store must be guaranteed.
func (s *Server) Purge(ctx context.Context, in *contract.EmptyRequest) (*contract.GeneralResponse, error) {
	log.Debug("Purge()")
	var res contract.GeneralResponse

	// Refused to a scoped caller rather than narrowed to their partition. A group-scoped purge would
	// report success for an operation whose name promises the whole store, and it would also have to
	// make the purge gate below group-aware - otherwise one group's purge would spend its duration
	// returning Unavailable to every other group.
	if err := s.requireUnbound(ctx, "Purge"); err != nil {
		return &res, err
	}

	s.purgeInProgress.Store(true)

	err := s.db.Purge(ctx)

	s.purgeInProgress.Store(false)

	tel.purges.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

	if err != nil {
		return &res, mapError(err)
	}

	s.searchIdx().Purge()

	res.Ok = true

	return &res, nil
}

func (s *Server) InterceptorBlockWhenPurgeInProgress(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	if s.purgeInProgress.Load() && strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
		log.Trace("ignoring request - purge in progress")

		return nil, status.Error(codes.Unavailable, "purge in progress")
	}

	return handler(ctx, req)
}

// HTTPMiddlewareBlockWhenPurgeInProgress is the HTTP counterpart to
// InterceptorBlockWhenPurgeInProgress. The gateway calls the server's methods directly and never
// runs the gRPC interceptor chain, so without this a /v1/... request would slip straight through
// while a purge is running. It rejects every request with 503 while a purge is in progress, except
// the paths in openPaths (exact match - the health probe and the static OpenAPI document, which
// must stay reachable). Closed by default like auth.HTTPMiddleware: a gateway endpoint added later
// is blocked during purge without anyone having to remember to list it.
func (s *Server) HTTPMiddlewareBlockWhenPurgeInProgress(next http.Handler, openPaths []string) http.Handler {
	open := make(map[string]bool, len(openPaths))
	for _, p := range openPaths {
		open[p] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.purgeInProgress.Load() && !open[r.URL.Path] {
			log.Trace("rejecting request - purge in progress")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "purge in progress"})

			return
		}

		next.ServeHTTP(w, r)
	})
}
