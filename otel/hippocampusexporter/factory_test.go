// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/fastbean-au/hippocampus/otel/hippocampusexporter/internal/metadata"
)

func TestNewFactory(t *testing.T) {
	f := NewFactory()
	assert.Equal(t, metadata.Type, f.Type())

	_, err := f.CreateLogs(context.Background(), exportertest.NewNopSettings(metadata.Type), createDefaultConfig())
	require.NoError(t, err)
}

func TestCreateLogsExporter_WrongConfigType(t *testing.T) {
	_, err := createLogsExporter(context.Background(), exportertest.NewNopSettings(metadata.Type), nil)
	require.Error(t, err)
}

func TestShutdown_NoClientClosesCleanly(t *testing.T) {
	e := newExporter(defaultTestConfig(), exportertest.NewNopSettings(metadata.Type))
	assert.NoError(t, e.shutdown(context.Background()))
}

// TestShutdown_EndsOpenEvents verifies the shutdown path ends any events the exporter left open.
func TestShutdown_EndsOpenEvents(t *testing.T) {
	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, defaultTestConfig(), fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "auth", body: "x", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
	})))
	require.Len(t, fake.events, 1)

	require.NoError(t, e.shutdown(context.Background()))
	require.Len(t, fake.ended, 1, "open event must be ended on shutdown")
	assert.Equal(t, "e1", fake.ended[0].GetId())
}

func TestDefaultJitterWithinBounds(t *testing.T) {
	assert.Equal(t, int32(0), defaultJitter(0))
	assert.Equal(t, int32(0), defaultJitter(-5))

	for range 1000 {
		v := defaultJitter(100)
		assert.GreaterOrEqual(t, v, int32(-100))
		assert.LessOrEqual(t, v, int32(100))
	}
}

func TestSeverityLabelFromNumber(t *testing.T) {
	lr := plog.NewLogRecord()
	lr.SetSeverityNumber(plog.SeverityNumberError3)
	assert.Equal(t, "ERROR", severityLabel(lr))

	lr2 := plog.NewLogRecord()
	assert.Empty(t, severityLabel(lr2), "unspecified severity with no text has no label")

	// Empty-body records get a placeholder rather than being rejected downstream.
	e := newExporter(defaultTestConfig(), exportertest.NewNopSettings(metadata.Type))
	assert.Equal(t, "(empty log record)", e.recordBody(plog.NewLogRecord()))
}

func TestSignificanceRespectsClamp(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Significance.Max = 100 // force clamp below the FATAL base
	e := newExporter(cfg, exportertest.NewNopSettings(metadata.Type))
	e.jitterFn = func(int32) int32 { return 0 }

	lr := plog.NewLogRecord()
	lr.SetSeverityNumber(plog.SeverityNumberFatal)
	assert.Equal(t, int32(100), e.significance(lr))
}
