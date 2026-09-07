// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter // import "github.com/fastbean-au/hippocampus/integrations/otel/hippocampusexporter"

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// hippoClient is the narrow slice of the generated Hippocampus gRPC client the exporter uses, so
// tests can substitute a fake. *contract.hippocampusClient satisfies it structurally.
type hippoClient interface {
	StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error)
	StoreMemories(ctx context.Context, in *contract.StoreMemoriesRequest, opts ...grpc.CallOption) (*contract.StoreMemoriesResponse, error)
	StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error)
	EndEvent(ctx context.Context, in *contract.EndEventRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
}

// eventState tracks the currently-open event for one event key so records with the same key share
// it and a bucket roll ends the prior event.
type eventState struct {
	id     string
	bucket string
	lastTS int64
}

type hippoExporter struct {
	cfg    *Config
	set    exporter.Settings
	logger *zap.Logger

	conn   *grpc.ClientConn
	client hippoClient

	// nowFn and jitterFn are injectable so tests are deterministic.
	nowFn    func() time.Time
	jitterFn func(spread int32) int32

	// batchUnsupported latches when the service answers StoreMemories with Unimplemented, which is
	// how a build predating that RPC presents.
	batchUnsupported atomic.Bool

	mu     sync.Mutex
	events map[string]*eventState
}

func newExporter(cfg *Config, set exporter.Settings) *hippoExporter {
	return &hippoExporter{
		cfg:      cfg,
		set:      set,
		logger:   set.Logger,
		nowFn:    time.Now,
		jitterFn: defaultJitter,
		events:   map[string]*eventState{},
	}
}

// defaultJitter returns a random offset in [-spread, +spread].
func defaultJitter(spread int32) int32 {
	if spread <= 0 {
		return 0
	}

	return rand.Int32N(2*spread+1) - spread
}

func (e *hippoExporter) start(ctx context.Context, host component.Host) error {
	var opts []configgrpc.ToClientConnOption
	if token := string(e.cfg.Token); token != "" {
		opts = append(opts, configgrpc.WithGrpcDialOption(grpc.WithChainUnaryInterceptor(bearerTokenInterceptor(token))))
	}

	conn, err := e.cfg.ToClientConn(ctx, host.GetExtensions(), e.set.TelemetrySettings, opts...)
	if err != nil {
		return fmt.Errorf("dialling Hippocampus at %q: %w", e.cfg.Endpoint, err)
	}

	e.conn = conn
	e.client = contract.NewHippocampusClient(conn)

	return nil
}

func (e *hippoExporter) shutdown(ctx context.Context) error {
	// Best-effort: close any events still open so the store isn't left with dangling open spans.
	if e.client != nil {
		e.mu.Lock()
		for _, st := range e.events {
			if st.id == "" {
				continue
			}

			if _, err := e.client.EndEvent(ctx, &contract.EndEventRequest{Id: st.id, TimeEnd: st.lastTS}); err != nil {
				e.logger.Debug("ending event on shutdown failed", zap.String("event_id", st.id), zap.Error(err))
			}
		}
		e.events = map[string]*eventState{}
		e.mu.Unlock()
	}

	if e.conn != nil {
		return e.conn.Close()
	}

	return nil
}

// maxBatchMemories bounds one StoreMemories call. It is the service's own per-call cap; a larger
// collector batch is split rather than refused.
const maxBatchMemories = 500

// pushLogs converts each log record to a Hippocampus memory (and, when configured, attaches it to a
// keyed event) and stores the lot with StoreMemories.
//
// One call per batch rather than one per record: exporterhelper has already batched these, and
// sending them one at a time spent a round trip, an interceptor chain, a rate-limit token and a
// transaction on each. The batch write validates every memory individually, so the collector's
// batch is not an all-or-nothing unit either.
//
// A transport error is returned so exporterhelper retries the batch. A per-memory failure is not:
// re-sending the batch would duplicate every memory that did land, since a fresh memory carries no
// client-chosen id to make the retry idempotent. The exception is a call in which nothing landed
// and every failure was retryable, which is a batch worth retrying and safe to.
func (e *hippoExporter) pushLogs(ctx context.Context, ld plog.Logs) error {
	memories := make([]*contract.Memory, 0, ld.LogRecordCount())

	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		sls := rl.ScopeLogs()

		for j := 0; j < sls.Len(); j++ {
			sl := sls.At(j)
			attrs := recordAttributes{
				resource: rl.Resource().Attributes(),
				scope:    sl.Scope().Attributes(),
				record:   pcommon.NewMap(),
			}

			lrs := sl.LogRecords()

			for k := 0; k < lrs.Len(); k++ {
				lr := lrs.At(k)
				attrs.record = lr.Attributes()

				memories = append(memories, e.memoryFor(ctx, attrs, lr))
			}
		}
	}

	if len(memories) == 0 {
		return nil
	}

	var errs error

	for start := 0; start < len(memories); start += maxBatchMemories {
		end := min(start+maxBatchMemories, len(memories))

		if err := e.storeBatch(ctx, memories[start:end]); err != nil {
			errs = errors.Join(errs, err)
		}
	}

	return errs
}

// memoryFor maps one log record onto a memory, resolving its event first when events are enabled.
func (e *hippoExporter) memoryFor(ctx context.Context, attrs recordAttributes, lr plog.LogRecord) *contract.Memory {
	ts := e.recordTime(lr)
	group := attrs.lookup(e.cfg.GroupFrom, e.cfg.DefaultGroup)

	mem := &contract.Memory{
		Body:         e.recordBody(lr),
		Significance: e.significance(lr),
		TimeStamp:    ts.UnixNano(),
		Group:        group,
		Metadata:     e.metadata(attrs, lr),
	}

	if e.cfg.CreateEvents {
		if eid := e.eventID(ctx, attrs, lr, ts, group); eid != "" {
			mem.EventId = eid
		}
	}

	return mem
}

// storeBatch writes one chunk, falling back to a memory-at-a-time loop against a service too old to
// carry StoreMemories. The fallback latches: an instance does not grow the RPC while it is running,
// and probing once per batch would cost a failed call per export for the life of the process.
func (e *hippoExporter) storeBatch(ctx context.Context, memories []*contract.Memory) error {
	if e.batchUnsupported.Load() {
		return e.storeEach(ctx, memories)
	}

	resp, err := e.client.StoreMemories(ctx, &contract.StoreMemoriesRequest{Memories: memories})
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			e.batchUnsupported.Store(true)
			e.logger.Info("service does not serve StoreMemories; falling back to one call per record",
				zap.Error(err))

			return e.storeEach(ctx, memories)
		}

		return fmt.Errorf("storing %d memories: %w", len(memories), err)
	}

	return e.reportResults(memories, resp)
}

// reportResults logs what the batch write did with each memory, and reports the one case worth
// retrying: nothing was written and every failure was transient.
func (e *hippoExporter) reportResults(memories []*contract.Memory, resp *contract.StoreMemoriesResponse) error {
	if resp.GetRejected() > 0 {
		e.logger.Debug("memories dropped below minimum significance", zap.Int32("count", resp.GetRejected()))
	}

	if resp.GetFailed() == 0 {
		return nil
	}

	retryable := true

	for i, result := range resp.GetResults() {
		code := codes.Code(result.GetCode())
		if code == codes.OK {
			continue
		}

		group := ""
		if i < len(memories) {
			group = memories[i].GetGroup()
		}

		e.logger.Warn("memory rejected by the store",
			zap.String("code", code.String()),
			zap.String("error", result.GetError()),
			zap.String("group", group))

		retryable = retryable && isRetryable(code)
	}

	// Only a call in which NOTHING landed is safe to hand back for retry; a partial success would
	// be re-sent in full and stored twice.
	if retryable && resp.GetStored() == 0 && resp.GetRejected() == 0 {
		return fmt.Errorf("storing %d memories: every memory failed transiently", len(memories))
	}

	return nil
}

// isRetryable reports whether a per-memory failure is worth sending again. A validation fault, a
// missing event or a refused group will fail identically forever; a timeout or an unavailable
// backend will not.
func isRetryable(code codes.Code) bool {
	switch code {

	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true

	default:
		return false

	}
}

// storeEach is the pre-batch path, kept for a service that does not serve StoreMemories.
func (e *hippoExporter) storeEach(ctx context.Context, memories []*contract.Memory) error {
	var errs error

	for _, mem := range memories {
		resp, err := e.client.StoreMemory(ctx, mem)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("storing memory: %w", err))

			continue
		}

		if resp.GetRejected() {
			e.logger.Debug("memory dropped below minimum significance",
				zap.Int32("significance", mem.GetSignificance()),
				zap.String("group", mem.GetGroup()))
		}
	}

	return errs
}

// recordTime resolves the record timestamp (falling back to the observed timestamp) and clamps a
// future timestamp to now, so the service's clock-skew guard never rejects the write.
func (e *hippoExporter) recordTime(lr plog.LogRecord) time.Time {
	ts := lr.Timestamp()
	if ts == 0 {
		ts = lr.ObservedTimestamp()
	}

	now := e.nowFn()
	if ts == 0 {
		return now
	}

	t := ts.AsTime()
	if t.After(now) {
		return now
	}

	return t
}

func (e *hippoExporter) recordBody(lr plog.LogRecord) string {
	body := ""
	if e.cfg.BodyFrom != "" && e.cfg.BodyFrom != bodyFromRecordBody {
		if v, ok := lr.Attributes().Get(e.cfg.BodyFrom); ok {
			body = v.AsString()
		}
	}

	if body == "" {
		body = lr.Body().AsString()
	}

	if body == "" {
		body = "(empty log record)"
	}

	if e.cfg.PrefixSeverity {
		if sev := severityLabel(lr); sev != "" {
			body = fmt.Sprintf("[%s] %s", sev, body)
		}
	}

	return body
}

// significance maps the record's severity to a configured value, jittered and clamped.
func (e *hippoExporter) significance(lr plog.LogRecord) int32 {
	base := e.cfg.Significance.Default

	switch severityBucket(lr.SeverityNumber(), lr.SeverityText()) {

	case plog.SeverityNumberTrace:
		base = e.cfg.Significance.Trace

	case plog.SeverityNumberDebug:
		base = e.cfg.Significance.Debug

	case plog.SeverityNumberInfo:
		base = e.cfg.Significance.Info

	case plog.SeverityNumberWarn:
		base = e.cfg.Significance.Warn

	case plog.SeverityNumberError:
		base = e.cfg.Significance.Error

	case plog.SeverityNumberFatal:
		base = e.cfg.Significance.Fatal
	}

	return clamp(base+e.jitterFn(e.cfg.Significance.Jitter), e.cfg.Significance.Min, e.cfg.Significance.Max)
}

// eventID returns the id of the event this record belongs to, creating one (and ending the prior
// event for the same key when the bucket rolls) as needed. Best-effort: on any failure it returns
// "" and the memory is stored without an event link.
func (e *hippoExporter) eventID(ctx context.Context, attrs recordAttributes, lr plog.LogRecord, ts time.Time, group string) string {
	joinedKey, name, bucket := e.eventKey(attrs, ts)

	e.mu.Lock()
	defer e.mu.Unlock()

	if st, ok := e.events[joinedKey]; ok {
		if st.bucket == bucket {
			if ts.UnixNano() > st.lastTS {
				st.lastTS = ts.UnixNano()
			}

			return st.id
		}

		// Bucket rolled: end the prior event before opening a new one.
		if st.id != "" {
			if _, err := e.client.EndEvent(ctx, &contract.EndEventRequest{Id: st.id, TimeEnd: st.lastTS}); err != nil {
				e.logger.Debug("ending rolled event failed", zap.String("event_id", st.id), zap.Error(err))
			}
		}

		delete(e.events, joinedKey)
	}

	resp, err := e.client.StoreEvent(ctx, &contract.Event{
		Name:         name,
		Group:        group,
		TimeStart:    ts.UnixNano(),
		Significance: clamp(e.cfg.EventSignificance+e.jitterFn(e.cfg.Significance.Jitter), e.cfg.Significance.Min, e.cfg.Significance.Max),
	})
	if err != nil {
		e.logger.Debug("creating event failed; storing memory without event", zap.String("event_key", joinedKey), zap.Error(err))

		return ""
	}

	if resp.GetId() == "" {
		return ""
	}

	e.events[joinedKey] = &eventState{id: resp.GetId(), bucket: bucket, lastTS: ts.UnixNano()}

	return resp.GetId()
}

// eventKey computes the joined key (from EventKeyFrom), the rendered event name, and the time
// bucket suffix for a record.
func (e *hippoExporter) eventKey(attrs recordAttributes, ts time.Time) (string, string, string) {
	parts := make([]string, 0, len(e.cfg.EventKeyFrom))
	for _, name := range e.cfg.EventKeyFrom {
		parts = append(parts, attrs.lookup(name, e.cfg.DefaultGroup))
	}

	joined := strings.Join(parts, ":")
	bucket := timeBucket(ts, e.cfg.EventBucket)

	name := e.cfg.EventNameTemplate
	if name == "" || bucket == "" {
		name = joined
	} else {
		name = strings.ReplaceAll(name, "{key}", joined)
		name = strings.ReplaceAll(name, "{bucket}", bucket)
	}

	// The state map is keyed by the stable joined key; the bucket is compared separately so a roll
	// ends the prior event and opens a fresh one under the same key.
	return joined, name, bucket
}

// bearerTokenInterceptor stamps "authorization: Bearer <token>" onto every RPC, matching the
// service's auth interceptor, mirroring integrations/mcp.
func bearerTokenInterceptor(token string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req any, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// timeBucket renders the bucket suffix for a timestamp under the configured mode.
func timeBucket(ts time.Time, mode string) string {
	switch mode {

	case eventBucketDay:
		return ts.UTC().Format("2006-01-02")

	case eventBucketHour:
		return ts.UTC().Format("2006-01-02T15")

	default:
		return ""
	}
}

// severityBucket collapses a SeverityNumber to its representative bucket constant, falling back to
// the (case-insensitive) SeverityText when the number is unspecified.
func severityBucket(n plog.SeverityNumber, text string) plog.SeverityNumber {
	switch {

	case n >= plog.SeverityNumberTrace && n <= plog.SeverityNumberTrace4:
		return plog.SeverityNumberTrace

	case n >= plog.SeverityNumberDebug && n <= plog.SeverityNumberDebug4:
		return plog.SeverityNumberDebug

	case n >= plog.SeverityNumberInfo && n <= plog.SeverityNumberInfo4:
		return plog.SeverityNumberInfo

	case n >= plog.SeverityNumberWarn && n <= plog.SeverityNumberWarn4:
		return plog.SeverityNumberWarn

	case n >= plog.SeverityNumberError && n <= plog.SeverityNumberError4:
		return plog.SeverityNumberError

	case n >= plog.SeverityNumberFatal && n <= plog.SeverityNumberFatal4:
		return plog.SeverityNumberFatal
	}

	return severityFromText(text)
}

func severityFromText(text string) plog.SeverityNumber {
	switch strings.ToUpper(strings.TrimSpace(text)) {

	case "TRACE":
		return plog.SeverityNumberTrace

	case "DEBUG":
		return plog.SeverityNumberDebug

	case "INFO", "INFORMATION", "NOTICE":
		return plog.SeverityNumberInfo

	case "WARN", "WARNING":
		return plog.SeverityNumberWarn

	case "ERROR", "ERR", "CRITICAL", "CRIT":
		return plog.SeverityNumberError

	case "FATAL", "EMERGENCY", "ALERT", "PANIC":
		return plog.SeverityNumberFatal
	}

	return plog.SeverityNumberUnspecified
}

// severityLabel is the uppercase level name used when PrefixSeverity is set.
func severityLabel(lr plog.LogRecord) string {
	if t := strings.TrimSpace(lr.SeverityText()); t != "" {
		return strings.ToUpper(t)
	}

	switch severityBucket(lr.SeverityNumber(), "") {

	case plog.SeverityNumberTrace:
		return "TRACE"

	case plog.SeverityNumberDebug:
		return "DEBUG"

	case plog.SeverityNumberInfo:
		return "INFO"

	case plog.SeverityNumberWarn:
		return "WARN"

	case plog.SeverityNumberError:
		return "ERROR"

	case plog.SeverityNumberFatal:
		return "FATAL"
	}

	return ""
}

func clamp(v int32, lo int32, hi int32) int32 {
	if v < lo {
		return lo
	}

	if v > hi {
		return hi
	}

	return v
}
