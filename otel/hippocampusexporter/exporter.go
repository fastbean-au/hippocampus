// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter // import "github.com/fastbean-au/hippocampus/otel/hippocampusexporter"

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
)

// hippoClient is the narrow slice of the generated Hippocampus gRPC client the exporter uses, so
// tests can substitute a fake. *contract.hippocampusClient satisfies it structurally.
type hippoClient interface {
	StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error)
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

// pushLogs converts each log record to a Hippocampus memory (and, when configured, attaches it to a
// keyed event) and stores it. A transport error is returned so exporterhelper retries the batch; a
// significance-drop (StoreMemoryResponse.Rejected) is counted, not treated as an error.
func (e *hippoExporter) pushLogs(ctx context.Context, ld plog.Logs) error {
	var errs error

	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		resAttrs := rl.Resource().Attributes()
		sls := rl.ScopeLogs()

		for j := 0; j < sls.Len(); j++ {
			lrs := sls.At(j).LogRecords()

			for k := 0; k < lrs.Len(); k++ {
				if err := e.storeRecord(ctx, resAttrs, lrs.At(k)); err != nil {
					errs = errors.Join(errs, err)
				}
			}
		}
	}

	return errs
}

func (e *hippoExporter) storeRecord(ctx context.Context, resAttrs pcommon.Map, lr plog.LogRecord) error {
	ts := e.recordTime(lr)
	group := e.lookup(resAttrs, lr.Attributes(), e.cfg.GroupFrom, e.cfg.DefaultGroup)

	mem := &contract.Memory{
		Body:         e.recordBody(lr),
		Significance: e.significance(lr),
		TimeStamp:    ts.UnixNano(),
		Group:        group,
	}

	if e.cfg.CreateEvents {
		if eid := e.eventID(ctx, resAttrs, lr, ts, group); eid != "" {
			mem.EventId = eid
		}
	}

	resp, err := e.client.StoreMemory(ctx, mem)
	if err != nil {

		return fmt.Errorf("storing memory: %w", err)
	}

	if resp.GetRejected() {
		e.logger.Debug("memory dropped below minimum significance",
			zap.Int32("significance", mem.GetSignificance()),
			zap.String("group", group))
	}

	return nil
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
func (e *hippoExporter) eventID(ctx context.Context, resAttrs pcommon.Map, lr plog.LogRecord, ts time.Time, group string) string {
	joinedKey, name, bucket := e.eventKey(resAttrs, lr, ts)

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
func (e *hippoExporter) eventKey(resAttrs pcommon.Map, lr plog.LogRecord, ts time.Time) (string, string, string) {
	parts := make([]string, 0, len(e.cfg.EventKeyFrom))
	for _, name := range e.cfg.EventKeyFrom {
		parts = append(parts, e.lookup(resAttrs, lr.Attributes(), name, e.cfg.DefaultGroup))
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

// lookup resolves an attribute by name, preferring the record's own attributes over the resource's,
// falling back to def when absent or empty.
func (e *hippoExporter) lookup(resAttrs pcommon.Map, recAttrs pcommon.Map, name string, def string) string {
	if name != "" {
		if v, ok := recAttrs.Get(name); ok {
			if s := v.AsString(); s != "" {

				return s
			}
		}

		if v, ok := resAttrs.Get(name); ok {
			if s := v.AsString(); s != "" {

				return s
			}
		}
	}

	return def
}

// bearerTokenInterceptor stamps "authorization: Bearer <token>" onto every RPC, matching the
// service's auth interceptor, mirroring cmd/hippocampus-mcp.
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
