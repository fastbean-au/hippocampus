// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/otel/hippocampusexporter/internal/metadata"
)

// fakeClient records the requests the exporter issues and lets a test inject responses/errors.
type fakeClient struct {
	mu sync.Mutex

	memories []*contract.Memory
	events   []*contract.Event
	ended    []*contract.EndEventRequest

	nextEventID int
	storeMemErr error
	rejectMem   bool
	storeEvtErr error
	emptyEvtID  bool
}

func (f *fakeClient) StoreMemory(_ context.Context, in *contract.Memory, _ ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.storeMemErr != nil {

		return nil, f.storeMemErr
	}

	f.memories = append(f.memories, in)

	return &contract.StoreMemoryResponse{Id: "m", Rejected: f.rejectMem}, nil
}

func (f *fakeClient) StoreEvent(_ context.Context, in *contract.Event, _ ...grpc.CallOption) (*contract.StoreEventResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.storeEvtErr != nil {

		return nil, f.storeEvtErr
	}

	f.events = append(f.events, in)

	if f.emptyEvtID {

		return &contract.StoreEventResponse{}, nil
	}

	f.nextEventID++

	return &contract.StoreEventResponse{Id: fmt.Sprintf("e%d", f.nextEventID)}, nil
}

func (f *fakeClient) EndEvent(_ context.Context, in *contract.EndEventRequest, _ ...grpc.CallOption) (*contract.GeneralResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ended = append(f.ended, in)

	return &contract.GeneralResponse{Ok: true}, nil
}

func newTestExporter(t *testing.T, cfg *Config, fake hippoClient, now time.Time) *hippoExporter {
	t.Helper()

	e := newExporter(cfg, exportertest.NewNopSettings(metadata.Type))
	e.client = fake
	e.nowFn = func() time.Time { return now }
	e.jitterFn = func(int32) int32 { return 0 }

	return e
}

// logRecord is a small builder for a single-record plog.Logs.
type logRecord struct {
	service string
	body    string
	sevNum  plog.SeverityNumber
	sevText string
	ts      time.Time
	attrs   map[string]string
}

func buildLogs(records ...logRecord) plog.Logs {
	ld := plog.NewLogs()

	for _, r := range records {
		rl := ld.ResourceLogs().AppendEmpty()
		if r.service != "" {
			rl.Resource().Attributes().PutStr("service.name", r.service)
		}

		lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		lr.Body().SetStr(r.body)
		lr.SetSeverityNumber(r.sevNum)
		if r.sevText != "" {
			lr.SetSeverityText(r.sevText)
		}

		if !r.ts.IsZero() {
			lr.SetTimestamp(pcommon.NewTimestampFromTime(r.ts))
		}

		for k, v := range r.attrs {
			lr.Attributes().PutStr(k, v)
		}
	}

	return ld
}

func defaultTestConfig() *Config {
	return createDefaultConfig().(*Config)
}

func TestPushLogs_MemoriesOnly(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	ts := now.Add(-time.Hour)
	err := e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "auth", body: "login ok", sevNum: plog.SeverityNumberInfo, ts: ts,
	}))
	require.NoError(t, err)

	require.Len(t, fake.memories, 1)
	require.Empty(t, fake.events)

	m := fake.memories[0]
	assert.Equal(t, "login ok", m.GetBody())
	assert.Equal(t, int32(6000), m.GetSignificance())
	assert.Equal(t, "auth", m.GetGroup())
	assert.Equal(t, ts.UnixNano(), m.GetTimeStamp())
	assert.Empty(t, m.GetEventId())
}

func TestPushLogs_SeverityToSignificanceMonotonic(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	recs := []logRecord{
		{service: "s", body: "d", sevNum: plog.SeverityNumberDebug, ts: now.Add(-time.Hour)},
		{service: "s", body: "i", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
		{service: "s", body: "w", sevNum: plog.SeverityNumberWarn, ts: now.Add(-time.Hour)},
		{service: "s", body: "e", sevNum: plog.SeverityNumberError, ts: now.Add(-time.Hour)},
		{service: "s", body: "f", sevNum: plog.SeverityNumberFatal, ts: now.Add(-time.Hour)},
	}
	require.NoError(t, e.pushLogs(context.Background(), buildLogs(recs...)))

	require.Len(t, fake.memories, 5)
	for i := 1; i < len(fake.memories); i++ {
		assert.Less(t, fake.memories[i-1].GetSignificance(), fake.memories[i].GetSignificance(),
			"significance must rise with severity")
	}
}

func TestPushLogs_SeverityFromText(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "boom", sevText: "error", ts: now.Add(-time.Hour),
	})))

	require.Len(t, fake.memories, 1)
	assert.Equal(t, int32(28000), fake.memories[0].GetSignificance())
}

func TestPushLogs_FutureTimestampClamped(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "future", sevNum: plog.SeverityNumberInfo, ts: now.Add(time.Hour),
	})))

	require.Len(t, fake.memories, 1)
	assert.Equal(t, now.UnixNano(), fake.memories[0].GetTimeStamp(), "future timestamp must clamp to now")
}

func TestPushLogs_MissingTimestampUsesNow(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "no-ts", sevNum: plog.SeverityNumberInfo,
	})))

	require.Len(t, fake.memories, 1)
	assert.Equal(t, now.UnixNano(), fake.memories[0].GetTimeStamp())
}

func TestPushLogs_EventsSameKeySameDayShareEvent(t *testing.T) {
	cfg := defaultTestConfig() // create_events true, key service.name, bucket day

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	day := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	recs := []logRecord{
		{service: "auth", body: "a", sevNum: plog.SeverityNumberInfo, ts: day.Add(1 * time.Hour)},
		{service: "auth", body: "b", sevNum: plog.SeverityNumberInfo, ts: day.Add(2 * time.Hour)},
	}
	require.NoError(t, e.pushLogs(context.Background(), buildLogs(recs...)))

	require.Len(t, fake.events, 1, "same service+day must open exactly one event")
	require.Len(t, fake.memories, 2)
	assert.Equal(t, fake.events[0].GetName(), "auth — 2026-07-20")
	assert.Equal(t, "e1", fake.memories[0].GetEventId())
	assert.Equal(t, "e1", fake.memories[1].GetEventId())
	assert.Empty(t, fake.ended)
}

func TestPushLogs_EventsBucketRollEndsPrior(t *testing.T) {
	cfg := defaultTestConfig()

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	recs := []logRecord{
		{service: "auth", body: "day1", sevNum: plog.SeverityNumberInfo, ts: time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)},
		{service: "auth", body: "day2", sevNum: plog.SeverityNumberInfo, ts: time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC)},
	}
	require.NoError(t, e.pushLogs(context.Background(), buildLogs(recs...)))

	require.Len(t, fake.events, 2, "day roll must open a second event")
	require.Len(t, fake.ended, 1, "day roll must end the prior event")
	assert.Equal(t, "e1", fake.ended[0].GetId())
	assert.Equal(t, "e1", fake.memories[0].GetEventId())
	assert.Equal(t, "e2", fake.memories[1].GetEventId())
}

func TestPushLogs_ConfigurableCompositeEventKey(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.EventKeyFrom = []string{"service.name", "host.name"}
	cfg.EventBucket = eventBucketNone

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	ts := now.Add(-time.Hour)
	recs := []logRecord{
		{service: "auth", body: "h1", sevNum: plog.SeverityNumberInfo, ts: ts, attrs: map[string]string{"host.name": "node-1"}},
		{service: "auth", body: "h2", sevNum: plog.SeverityNumberInfo, ts: ts, attrs: map[string]string{"host.name": "node-2"}},
		{service: "auth", body: "h1-again", sevNum: plog.SeverityNumberInfo, ts: ts, attrs: map[string]string{"host.name": "node-1"}},
	}
	require.NoError(t, e.pushLogs(context.Background(), buildLogs(recs...)))

	require.Len(t, fake.events, 2, "distinct host keys must open distinct events")
	assert.Equal(t, "auth:node-1", fake.events[0].GetName(), "no bucket -> name is the joined key")
	assert.Equal(t, "e1", fake.memories[0].GetEventId())
	assert.Equal(t, "e2", fake.memories[1].GetEventId())
	assert.Equal(t, "e1", fake.memories[2].GetEventId(), "same composite key reuses its event")
}

func TestPushLogs_RejectedIsNotAnError(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{rejectMem: true}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	err := e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "low", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
	}))
	assert.NoError(t, err, "a significance drop must not fail the batch")
	assert.Len(t, fake.memories, 1)
}

func TestPushLogs_StoreErrorPropagates(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{storeMemErr: errors.New("unavailable")}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	err := e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "x", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
	}))
	require.Error(t, err, "a transport error must fail the batch so exporterhelper retries")
	assert.Contains(t, err.Error(), "unavailable")
}

func TestPushLogs_EventFailureFallsBackToMemoryOnly(t *testing.T) {
	cfg := defaultTestConfig()

	fake := &fakeClient{storeEvtErr: errors.New("event down")}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	err := e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "auth", body: "x", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
	}))
	require.NoError(t, err, "a failed event must not fail the log write")
	require.Len(t, fake.memories, 1)
	assert.Empty(t, fake.memories[0].GetEventId(), "memory stored without an event link")
}

func TestPushLogs_GroupFallbackToDefault(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	cfg.DefaultGroup = "fallback"

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		body: "no-service", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
	})))

	require.Len(t, fake.memories, 1)
	assert.Equal(t, "fallback", fake.memories[0].GetGroup())
}

func TestPrefixSeverity(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	cfg.PrefixSeverity = true

	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "hello", sevText: "warn", ts: now.Add(-time.Hour),
	})))

	require.Len(t, fake.memories, 1)
	assert.Equal(t, "[WARN] hello", fake.memories[0].GetBody())
}
