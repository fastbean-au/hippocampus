// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/exporter/exportertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"google.golang.org/grpc"
	grpcmd "google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/otel/hippocampusexporter/internal/metadata"
)

func TestStartDialsAndShutdownCloses(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Token = "secret" // exercise the bearer-interceptor dial option

	e := newExporter(cfg, exportertest.NewNopSettings(metadata.Type))
	require.NoError(t, e.start(context.Background(), componenttest.NewNopHost()))
	require.NotNil(t, e.client)
	require.NotNil(t, e.conn)

	assert.NoError(t, e.shutdown(context.Background()))
}

func TestStartBadTLSFails(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.TLS.CAFile = "/nonexistent/ca.pem" // ToClientConn must fail building credentials

	e := newExporter(cfg, exportertest.NewNopSettings(metadata.Type))
	assert.Error(t, e.start(context.Background(), componenttest.NewNopHost()))
}

func TestBearerTokenInterceptorAppendsAuth(t *testing.T) {
	var got []string
	invoker := func(ctx context.Context, _ string, _ any, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := grpcmd.FromOutgoingContext(ctx)
		got = md.Get("authorization")

		return nil
	}

	err := bearerTokenInterceptor("tok")(context.Background(), "/svc/Method", nil, nil, nil, invoker)
	require.NoError(t, err)
	assert.Equal(t, []string{"Bearer tok"}, got)
}

func TestTimeBucket(t *testing.T) {
	ts := time.Date(2026, 7, 24, 13, 5, 0, 0, time.UTC)
	assert.Equal(t, "2026-07-24", timeBucket(ts, eventBucketDay))
	assert.Equal(t, "2026-07-24T13", timeBucket(ts, eventBucketHour))
	assert.Empty(t, timeBucket(ts, eventBucketNone))
}

func TestSeverityFromTextAllTiers(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	cases := map[string]int32{
		"trace": 1000, "debug": 2000, "info": 6000, "notice": 6000,
		"warning": 16000, "err": 28000, "critical": 28000,
		"fatal": 32000, "emergency": 32000, "panic": 32000,
		"nonsense": 6000, // -> default
	}
	for text, want := range cases {
		fake.memories = nil
		require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
			service: "s", body: "b", sevText: text, ts: now.Add(-time.Hour),
		})))
		require.Len(t, fake.memories, 1)
		assert.Equal(t, want, fake.memories[0].GetSignificance(), "text %q", text)
	}
}

func TestSeverityBucketNumberRanges(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	// The *4 variants must map to the same tier as the base number.
	cases := map[plog.SeverityNumber]int32{
		plog.SeverityNumberTrace4: 1000,
		plog.SeverityNumberDebug4: 2000,
		plog.SeverityNumberInfo4:  6000,
		plog.SeverityNumberWarn4:  16000,
		plog.SeverityNumberError4: 28000,
		plog.SeverityNumberFatal4: 32000,
	}
	for num, want := range cases {
		fake.memories = nil
		require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
			service: "s", body: "b", sevNum: num, ts: now.Add(-time.Hour),
		})))
		require.Len(t, fake.memories, 1)
		assert.Equal(t, want, fake.memories[0].GetSignificance())
	}
}

func TestSeverityLabelFromNumbers(t *testing.T) {
	cases := map[plog.SeverityNumber]string{
		plog.SeverityNumberTrace: "TRACE",
		plog.SeverityNumberDebug: "DEBUG",
		plog.SeverityNumberInfo:  "INFO",
		plog.SeverityNumberWarn:  "WARN",
		plog.SeverityNumberError: "ERROR",
		plog.SeverityNumberFatal: "FATAL",
	}
	for num, want := range cases {
		lr := plog.NewLogRecord()
		lr.SetSeverityNumber(num)
		assert.Equal(t, want, severityLabel(lr))
	}
}

func TestRecordBodyFromAttribute(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	cfg.BodyFrom = "message"
	fake := &fakeClient{}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "ignored-body", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
		attrs: map[string]string{"message": "from-attr"},
	})))
	require.Len(t, fake.memories, 1)
	assert.Equal(t, "from-attr", fake.memories[0].GetBody())
}

func TestSignificanceClampLowerBound(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Significance.Min = 5000 // DEBUG base 2000 must clamp up to 5000
	e := newExporter(cfg, exportertest.NewNopSettings(metadata.Type))
	e.jitterFn = func(int32) int32 { return 0 }

	lr := plog.NewLogRecord()
	lr.SetSeverityNumber(plog.SeverityNumberDebug)
	assert.Equal(t, int32(5000), e.significance(lr))
}
