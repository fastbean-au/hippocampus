// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/fastbean-au/hippocampus/types"
)

// TestMetadata_TraceIdsByDefault is the promotion the whole feature exists for: a memory that
// survives the decay cycle must still name the trace that produced it, and only a metadata label
// can be filtered on afterwards.
func TestMetadata_TraceIdsByDefault(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	ld := buildLogs(logRecord{service: "s", body: "b", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)})
	lr := ld.ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	lr.SetTraceID(pcommon.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10})
	lr.SetSpanID(pcommon.SpanID{0xa0, 0xb1, 0xc2, 0xd3, 0xe4, 0xf5, 0x06, 0x17})

	require.NoError(t, e.pushLogs(context.Background(), ld))
	require.Len(t, fake.memories, 1)

	md := fake.memories[0].GetMetadata()
	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", md[traceMetadataKey])
	assert.Equal(t, "a0b1c2d3e4f50617", md[spanMetadataKey])
}

// TestMetadata_UnsetTraceIdsAreOmitted verifies an unsampled or untraced record carries no
// all-zero label, which would be worse than no label at all - it filters as a real trace.
func TestMetadata_UnsetTraceIdsAreOmitted(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(
		logRecord{service: "s", body: "b", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour)},
	)))

	assert.Empty(t, fake.memories[0].GetMetadata())
}

// TestMetadata_ThreeShapes covers the fixed labels, the named selection and the prefix sweep
// together, including the precedence between them and the resource/scope/record precedence a
// selection resolves through.
func TestMetadata_ThreeShapes(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	cfg.TraceMetadata = false
	cfg.Metadata = map[string]string{"pipeline": "logs", "http.status_code": "fixed"}
	cfg.MetadataFrom = []string{"http.status_code", "k8s.pod.name", "absent.attribute"}
	cfg.MetadataPrefix = "app.memory."

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "s")
	rl.Resource().Attributes().PutStr("k8s.pod.name", "api-7f9")
	rl.Resource().Attributes().PutStr("http.status_code", "from-resource")

	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().Attributes().PutStr("app.memory.library", "checkout")

	lr := sl.LogRecords().AppendEmpty()
	lr.Body().SetStr("b")
	lr.SetSeverityNumber(plog.SeverityNumberError)
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Hour)))
	lr.Attributes().PutInt("http.status_code", 503)
	lr.Attributes().PutStr("app.memory.tenant", "acme")

	require.NoError(t, e.pushLogs(context.Background(), ld))
	require.Len(t, fake.memories, 1)

	assert.Equal(t, map[string]string{
		"pipeline":         "logs",
		"http.status_code": "503",
		"k8s.pod.name":     "api-7f9",
		"tenant":           "acme",
		"library":          "checkout",
	}, fake.memories[0].GetMetadata(), "the record's own attribute must beat the resource's, and a selection must beat a fixed label")
}

// TestMetadata_KeysAreNormalised checks an attribute name outside the service's key charset is
// rewritten rather than sent and refused, which would fail a record nobody can fix.
func TestMetadata_KeysAreNormalised(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	cfg.TraceMetadata = false
	cfg.MetadataFrom = []string{"Weird Name!"}

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "b", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
		attrs: map[string]string{"Weird Name!": "v"},
	})))

	assert.Equal(t, map[string]string{"weird_name_": "v"}, fake.memories[0].GetMetadata())
}

// TestMetadata_OverLongValueIsDropped verifies the bounds are applied here rather than discovered
// at the service: a label the store would refuse must not fail the record carrying it.
func TestMetadata_OverLongValueIsDropped(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.CreateEvents = false
	cfg.TraceMetadata = false
	cfg.MetadataFrom = []string{"stack", "kept"}

	fake := &fakeClient{}
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e := newTestExporter(t, cfg, fake, now)

	require.NoError(t, e.pushLogs(context.Background(), buildLogs(logRecord{
		service: "s", body: "b", sevNum: plog.SeverityNumberInfo, ts: now.Add(-time.Hour),
		attrs: map[string]string{
			"stack": strings.Repeat("x", types.MaxMetadataValueLength+1),
			"kept":  "yes",
		},
	})))

	assert.Equal(t, map[string]string{"kept": "yes"}, fake.memories[0].GetMetadata())
}

// TestConfigValidate_Metadata covers the fixed labels being an operator error rather than a
// per-record one: they are on every memory, so they are worth refusing once at startup.
func TestConfigValidate_Metadata(t *testing.T) {
	cfg := defaultTestConfig()
	cfg.Metadata = map[string]string{"!!": "v"}
	assert.Error(t, cfg.Validate())

	cfg = defaultTestConfig()
	cfg.Metadata = map[string]string{"k": strings.Repeat("x", types.MaxMetadataValueLength+1)}
	assert.Error(t, cfg.Validate())

	cfg = defaultTestConfig()
	cfg.Metadata = map[string]string{}

	for i := range types.MaxMetadataKeys + 1 {
		cfg.Metadata[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}

	assert.Error(t, cfg.Validate())
}
