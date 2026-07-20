package hippocampus

import (
	"context"
	"encoding/json"
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
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
)

// sleepSingleflightKey is the sole key used with Server.sleepGroup: every caller wanting a sleep
// cycle joins the same in-flight call rather than starting a concurrent one.
const sleepSingleflightKey = "sleep"

// mapWriteError maps a storage-layer write conflict (db.ErrWriteConflict - a MySQL deadlock or
// lock-wait timeout that survived the driver's retries) to a gRPC Aborted status, which clients
// treat as retryable, so the write surfaces as a transient conflict rather than an opaque Unknown
// (which would look like a lost write). Any other error is returned unchanged. Applied at the
// write RPCs so both the gRPC and HTTP-gateway transports get the mapping (the gateway calls these
// handlers directly and never runs the gRPC interceptor chain).
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
	relationshipSignificanceWeight     float64
	recallSignificanceWeight           float64
	capacityMemories                   int
	capacityPressureExponent           float64
	capacityPressure                   float64
	capacityBytes                      int64
	capacityBytesFloor                 int64
	// lastUsedBytes caches the used-bytes reading eviction took at the end of the previous sleep
	// cycle, so the next cycle's capacity-pressure calculation can reuse it instead of scanning the
	// tables a second time. Written and read only from the sleep cycle, which
	// singleflight serialises, so it needs no lock.
	lastUsedBytes              int64
	walTriggerBytes            int64
	summarizationMinMemories   int
	summarizationMinAgeInDays  int
	summarizationMaxCandidates int
}

type Server struct {
	contract.UnimplementedHippocampusServer
	db db.Store

	// search is the optional secondary content-search index; nil (as in tests constructing a
	// Server directly) behaves as the disabled no-op via searchIdx().
	search search.Index

	// purgeInProgress is written by Purge and read by InterceptorBlockWhenPurgeInProgress from
	// every RPC's own goroutine, so it must be an atomic rather than a plain bool.
	purgeInProgress atomic.Bool

	sleepReset                chan bool
	minimumEventSignificance  int32
	minimumMemorySignificance int32
	maxMemoryBodyLength       int
	consolidation             Consolidation

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

	// stopSleep / sleepStopped / stopOnce coordinate shutdown of the autoSleep goroutine. Stop
	// closes stopSleep and waits for sleepStopped; because the loop only re-enters its select
	// between cycles, that wait also drains any in-flight cycle, so no consolidation is mid-scan
	// when the database is closed next. nil when the server was built without New (some tests).
	stopSleep    chan struct{}
	sleepStopped chan struct{}
	stopOnce     sync.Once

	// summarizationCandidates is refreshed by the sleep cycle and read by
	// GetSummarizationCandidates, so access is guarded by summarizationCandidatesMu.
	summarizationCandidates   []db.SummarizationCandidate
	summarizationCandidatesMu sync.RWMutex

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

func New(db db.Store, searchIndex search.Index, objects archive.ObjectStore) *Server {
	log.Trace("func() hippocampus.New()")

	reset := make(chan bool, 1)

	s := &Server{
		db:        db,
		search:    searchIndex,
		objects:   objects,
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
		consolidation: Consolidation{
			defaultEventSignificanceValue:      viper.GetInt32("consolidation.defaultEventSignificanceValue"),
			defaultEventSignificancePercentile: viper.GetFloat64("consolidation.defaultEventSignificancePercentile"),
			minimumAgeInDays:                   viper.GetInt("consolidation.minimumAgeInDays"),
			minimumRetentionInDays:             viper.GetInt("consolidation.minimumRetentionInDays"),
			aggressiveness:                     viper.GetFloat64("consolidation.aggressiveness"),
			deletionThreshold:                  viper.GetFloat64("consolidation.deletionThreshold"),
			method:                             viper.GetInt("consolidation.method"),
			unitsOfAgeInDays:                   viper.GetFloat64("consolidation.unitsOfAgeInDays"),
			relationshipSignificanceWeight:     viper.GetFloat64("consolidation.relationshipSignificanceWeight"),
			recallSignificanceWeight:           viper.GetFloat64("consolidation.recallSignificanceWeight"),
			capacityMemories:                   viper.GetInt("consolidation.capacityMemories"),
			capacityPressureExponent:           viper.GetFloat64("consolidation.capacityPressureExponent"),
			capacityPressure:                   1.0,
			capacityBytes:                      viper.GetInt64("consolidation.capacityBytes"),
			capacityBytesFloor:                 viper.GetInt64("consolidation.capacityBytesFloor"),
			walTriggerBytes:                    viper.GetInt64("consolidation.walTriggerBytes"),
			summarizationMinMemories:           viper.GetInt("consolidation.summarizationMinMemories"),
			summarizationMinAgeInDays:          viper.GetInt("consolidation.summarizationMinAgeInDays"),
			summarizationMaxCandidates:         viper.GetInt("consolidation.summarizationMaxCandidates"),
		},
	}

	s.consolidationEnabled = viper.GetBool("consolidation.enabled")

	s.stopSleep = make(chan struct{})
	s.sleepStopped = make(chan struct{})

	period := time.Duration(viper.GetInt("sleep.periodSeconds")) * time.Second

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

	s.startReconcile(searchIndex)

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

// Stop shuts the autoSleep goroutine (and the search-index reconciliation sweep, when running)
// down and waits for them to exit. Because the sleep loop only re-enters its select between cycles,
// that wait also drains any sleep cycle already in flight (started by the timer or the WAL trigger
// just before shutdown), so nothing is mid-consolidation when the caller closes the database next.
// Safe to call more than once, and a no-op when the server was built without New (autoSleep never
// started). Call it after the gRPC server's GracefulStop (which drains RPC-initiated cycles) and
// before closing the database.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
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

	go func() {
		defer close(s.sleepStopped)

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
				_ = s.sleepOnce()
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

	_ = s.sleepOnce()
}

// sleepOnce runs a sleep cycle via sleepGroup, so a call arriving while one is already in flight
// (from the autoSleep timer or a concurrent Sleep RPC) joins it and shares its result rather than
// starting a second, overlapping cycle.
func (s *Server) sleepOnce() error {
	_, err, _ := s.sleepGroup.Do(sleepSingleflightKey, func() (any, error) {
		return nil, s.sleep()
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

	err := s.sleepOnce()
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

	return &res, err
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

	s.purgeInProgress.Store(true)

	err := s.db.Purge(ctx)

	s.purgeInProgress.Store(false)

	tel.purges.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

	if err != nil {
		return &res, err
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
	if s.purgeInProgress.Load() && strings.HasPrefix(info.FullMethod, "/proto.Hippocampus/") {
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
