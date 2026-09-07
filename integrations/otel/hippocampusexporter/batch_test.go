// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/plog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestPushLogs_OneCallPerBatch is the point of the batch write: a collector batch that used to cost
// one RPC per record now costs one.
func TestPushLogs_OneCallPerBatch(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	records := make([]logRecord, 0, 20)
	for i := range 20 {
		records = append(records, logRecord{service: "s", body: fmt.Sprintf("line %d", i), sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)})
	}

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(records...)))

	assert.Equal(t, []int{20}, fake.batches, "20 records must be one call, not 20")
	assert.Len(t, fake.memories, 20)
}

// TestPushLogs_SplitsAtTheServiceCap pins that a batch larger than the service accepts is split
// rather than refused - the collector's batch size is not this exporter's to dictate.
func TestPushLogs_SplitsAtTheServiceCap(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	records := make([]logRecord, 0, maxBatchMemories+3)
	for i := range maxBatchMemories + 3 {
		records = append(records, logRecord{service: "s", body: fmt.Sprintf("line %d", i), sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)})
	}

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(records...)))

	assert.Equal(t, []int{maxBatchMemories, 3}, fake.batches)
	assert.Len(t, fake.memories, maxBatchMemories+3)
}

// TestPushLogs_FallsBackWhenUnsupported covers an exporter pointed at a service predating
// StoreMemories: it must keep exporting, and must not re-probe on every batch.
func TestPushLogs_FallsBackWhenUnsupported(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{storeMemsErr: status.Error(codes.Unimplemented, "unknown method StoreMemories")}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	logs := buildLogs(
		logRecord{service: "s", body: "one", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
		logRecord{service: "s", body: "two", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
	)

	require.NoError(t, e.pushLogs(context.Background(), logs))
	require.NoError(t, e.pushLogs(context.Background(), logs))

	assert.Len(t, fake.batches, 1, "the fallback must latch rather than fail a call per export")
	assert.Len(t, fake.memories, 4, "every record must still be stored through StoreMemory")
}

// TestPushLogs_PermanentPerMemoryFailureIsNotRetried is the half of the error policy that matters
// most: a batch in which some records landed must not be handed back for retry, or the ones that
// landed are stored twice.
func TestPushLogs_PermanentPerMemoryFailureIsNotRetried(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{failWith: []codes.Code{codes.OK, codes.InvalidArgument}}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	err := e.pushLogs(context.Background(), buildLogs(
		logRecord{service: "s", body: "good", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
		logRecord{service: "s", body: "bad", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
	))

	assert.NoError(t, err, "a partial success must not be retried")
	assert.Len(t, fake.memories, 1)
}

// TestPushLogs_WhollyTransientBatchIsRetried is the other half: nothing landed and every failure
// was transient, so the batch is worth sending again and safe to.
func TestPushLogs_WhollyTransientBatchIsRetried(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{failWith: []codes.Code{codes.Unavailable, codes.Aborted}}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	err := e.pushLogs(context.Background(), buildLogs(
		logRecord{service: "s", body: "one", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
		logRecord{service: "s", body: "two", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
	))

	assert.Error(t, err)
	assert.Empty(t, fake.memories)
}
