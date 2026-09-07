// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter // import "github.com/fastbean-au/hippocampus/integrations/otel/hippocampusexporter"

import (
	"sort"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"

	"github.com/fastbean-au/hippocampus/types"
)

// traceMetadataKey and spanMetadataKey are the labels TraceMetadata records under. They are fixed
// rather than configurable because they are what a reader will filter on
// (`GetMemories --metadata trace_id=...`), and a per-deployment name would make that query
// deployment-specific for no gain.
const (
	traceMetadataKey = "trace_id"
	spanMetadataKey  = "span_id"
)

// recordAttributes bundles the three attribute maps one log record is described by, most specific
// last. It exists so the lookup and metadata helpers take one parameter rather than three, and so
// precedence is decided in one place: a record's own attribute beats its scope's, which beats the
// resource's.
type recordAttributes struct {
	resource pcommon.Map
	scope    pcommon.Map
	record   pcommon.Map
}

// lookup resolves an attribute by name, most specific first, falling back to def when absent or
// empty.
func (a recordAttributes) lookup(name string, def string) string {
	if name == "" {
		return def
	}

	for _, m := range []pcommon.Map{a.record, a.scope, a.resource} {
		if v, ok := m.Get(name); ok {
			if s := v.AsString(); s != "" {
				return s
			}
		}
	}

	return def
}

// each visits every attribute across the three maps, least specific first, so a caller writing into
// one output map ends up with the most specific value for a repeated name.
func (a recordAttributes) each(visit func(name string, value pcommon.Value)) {
	for _, m := range []pcommon.Map{a.resource, a.scope, a.record} {
		for name, value := range m.All() {
			visit(name, value)
		}
	}
}

// metadata builds one memory's metadata: the fixed labels first, then the trace ids, then the
// selected attributes, which override a fixed label of the same key.
//
// Anything the service would reject is dropped rather than sent: a label the operator asked for but
// that does not fit must not fail the record, because the record is not at fault and the pipeline
// would retry it forever. Drops are logged at debug - a log exporter dropping a label at warn on
// every record would be its own noise source - except the configuration-level ones, which
// Config.Validate has already refused at startup.
func (e *hippoExporter) metadata(attrs recordAttributes, lr plog.LogRecord) map[string]string {
	cfg := e.cfg

	if len(cfg.Metadata) == 0 && len(cfg.MetadataFrom) == 0 && cfg.MetadataPrefix == "" && !cfg.TraceMetadata {
		return nil
	}

	out := make(map[string]string, len(cfg.Metadata)+len(cfg.MetadataFrom)+2)

	for k, v := range cfg.Metadata {
		if key := normaliseMetadataKey(k); key != "" {
			out[key] = v
		}
	}

	// The trace ids go in before the attribute-derived labels, so an operator who has deliberately
	// mapped an attribute onto the same key wins - the explicit selection is the more specific
	// instruction. An unset id is all-zero in pdata, and IsEmpty is what distinguishes it from a
	// real one.
	if cfg.TraceMetadata {
		if id := lr.TraceID(); !id.IsEmpty() {
			out[traceMetadataKey] = hexID(id[:])
		}

		if id := lr.SpanID(); !id.IsEmpty() {
			out[spanMetadataKey] = hexID(id[:])
		}
	}

	for _, name := range cfg.MetadataFrom {
		if v := attrs.lookup(name, ""); v != "" {
			if key := normaliseMetadataKey(name); key != "" {
				out[key] = v
			}
		}
	}

	if prefix := cfg.MetadataPrefix; prefix != "" {
		attrs.each(func(name string, value pcommon.Value) {
			if !strings.HasPrefix(name, prefix) {
				return
			}

			v := value.AsString()
			if v == "" {
				return
			}

			// The prefix is the selector, not part of the label, so it is stripped: an attribute
			// named app.memory.project becomes project.
			if key := normaliseMetadataKey(strings.TrimPrefix(name, prefix)); key != "" {
				out[key] = v
			}
		})
	}

	return e.boundMetadata(out)
}

// hexID renders a trace or span id as lower-case hex, the form every other tool prints it in and
// therefore the form a reader will paste into a metadata filter.
func hexID(b []byte) string {
	const digits = "0123456789abcdef"

	out := make([]byte, 0, len(b)*2)

	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}

	return string(out)
}

// normaliseMetadataKey rewrites an attribute name into the service's metadata key charset
// ([A-Za-z0-9][A-Za-z0-9._:/-]*), lowercasing it and replacing anything else with '_'. It returns
// "" for a name with no usable leading character, since a key must start alphanumeric.
//
// Semantic-convention names mostly pass through untouched (http.status_code, k8s.pod.name), which
// is the point: the key a reader filters on should be the attribute they already know.
func normaliseMetadataKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))

	var b strings.Builder

	for i := 0; i < len(name); i++ {
		c := name[i]

		alphanumeric := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')

		switch {

		case alphanumeric:
			b.WriteByte(c)

		case b.Len() == 0:
			// A key must begin alphanumeric, so leading punctuation is dropped rather than
			// substituted - a leading '_' would itself be invalid.
			continue

		case c == '.' || c == '_' || c == ':' || c == '/' || c == '-':
			b.WriteByte(c)

		default:
			b.WriteByte('_')

		}
	}

	key := b.String()

	if len(key) > types.MaxMetadataKeyLength {
		key = key[:types.MaxMetadataKeyLength]
	}

	return key
}

// boundMetadata drops whatever exceeds the service's per-value, key-count and total-size bounds,
// deterministically: keys are considered in sorted order so the same record always yields the same
// labels, rather than a map-iteration lottery per export.
func (e *hippoExporter) boundMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	keys := make([]string, 0, len(metadata))

	for k := range metadata {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	out := make(map[string]string, len(metadata))
	total := 0

	for _, k := range keys {
		v := metadata[k]

		switch {

		case len(v) > types.MaxMetadataValueLength:
			e.logger.Debug("dropping metadata label: value too long",
				zap.String("key", k), zap.Int("bytes", len(v)), zap.Int("max", types.MaxMetadataValueLength))

			continue

		case len(out) >= types.MaxMetadataKeys:
			e.logger.Debug("dropping metadata label: at the key limit",
				zap.String("key", k), zap.Int("max", types.MaxMetadataKeys))

			continue

		}

		// A rough per-entry serialised cost: the two quoted strings, the colon, and the separator.
		cost := len(k) + len(v) + 6

		if total+cost > types.MaxMetadataBytes {
			e.logger.Debug("dropping metadata label: would exceed the metadata size limit",
				zap.String("key", k), zap.Int("max", types.MaxMetadataBytes))

			continue
		}

		out[k] = v
		total += cost
	}

	if len(out) == 0 {
		return nil
	}

	return out
}
