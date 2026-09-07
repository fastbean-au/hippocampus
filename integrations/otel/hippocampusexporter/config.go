// Copyright The Hippocampus Authors
// SPDX-License-Identifier: Apache-2.0

package hippocampusexporter // import "github.com/fastbean-au/hippocampus/integrations/otel/hippocampusexporter"

import (
	"fmt"

	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/exporter/exporterhelper"

	"github.com/fastbean-au/hippocampus/types"
)

// Event bucketing modes for EventBucket.
const (
	eventBucketNone = "none"
	eventBucketHour = "hour"
	eventBucketDay  = "day"
)

// bodyFromRecordBody is the sentinel BodyFrom value meaning "use the log record's Body()".
const bodyFromRecordBody = "body"

// SignificanceConfig maps log severities onto Hippocampus memory significances. A record's
// significance is the value for its severity bucket, jittered by up to +/-Jitter and clamped to
// [Min, Max]. Higher significance survives the decay cycle longer.
type SignificanceConfig struct {
	Trace   int32 `mapstructure:"trace"`
	Debug   int32 `mapstructure:"debug"`
	Info    int32 `mapstructure:"info"`
	Warn    int32 `mapstructure:"warn"`
	Error   int32 `mapstructure:"error"`
	Fatal   int32 `mapstructure:"fatal"`
	Default int32 `mapstructure:"default"`

	Jitter int32 `mapstructure:"jitter"`
	Min    int32 `mapstructure:"min"`
	Max    int32 `mapstructure:"max"`
}

// Config is the Hippocampus exporter configuration.
type Config struct {
	// ClientConfig carries the gRPC endpoint, TLS trust options, and any static headers. The
	// endpoint is the Hippocampus service's gRPC address (default localhost:50051).
	configgrpc.ClientConfig `mapstructure:",squash"`

	TimeoutConfig             exporterhelper.TimeoutConfig `mapstructure:",squash"`
	configretry.BackOffConfig `mapstructure:"retry_on_failure"`
	QueueConfig               configoptional.Optional[exporterhelper.QueueBatchConfig] `mapstructure:"sending_queue"`

	// Token, when set, is stamped onto every RPC as an "authorization: Bearer <token>" metadata
	// header, mirroring the service's own bearer-token clients. Prefer this over a static header so
	// the value is treated as a secret in logs.
	Token configopaque.String `mapstructure:"token"`

	// CreateEvents toggles memories+events (true, the default) versus memories-only (false).
	CreateEvents bool `mapstructure:"create_events"`

	// GroupFrom names the resource/log attribute whose value populates each memory's group label
	// (default "service.name"); DefaultGroup is used when that attribute is absent.
	GroupFrom    string `mapstructure:"group_from"`
	DefaultGroup string `mapstructure:"default_group"`

	// EventKeyFrom is the ordered list of resource/log attribute names joined to form the event
	// key that decides which records share an event (default ["service.name"]). EventBucket
	// appends an optional time bucket (none|hour|day, default day). EventNameTemplate renders the
	// human-readable Event.name from "{key}" and "{bucket}".
	EventKeyFrom      []string `mapstructure:"event_key_from"`
	EventBucket       string   `mapstructure:"event_bucket"`
	EventNameTemplate string   `mapstructure:"event_name_template"`
	EventSignificance int32    `mapstructure:"event_significance"`

	// BodyFrom selects the memory body source: "body" (default, the log record Body) or the name
	// of an attribute to read instead. PrefixSeverity prepends "[SEVERITY] " to the body.
	BodyFrom       string `mapstructure:"body_from"`
	PrefixSeverity bool   `mapstructure:"prefix_severity"`

	// Metadata is stamped on every memory, before any attribute-derived labels are added.
	Metadata map[string]string `mapstructure:"metadata"`

	// MetadataFrom names the attributes to copy onto each memory's metadata, and MetadataPrefix
	// copies every attribute whose name carries that prefix (the prefix is stripped from the
	// resulting key). Both read the record's own attributes, its scope's, and the resource's, the
	// most specific winning.
	//
	// Both are opt-in selections rather than "copy every attribute", for the reason the broker
	// bridges' MetadataHeaders documents: a log record's attribute set is unbounded and largely
	// machinery, so copying it wholesale would fill each memory's metadata budget with noise and
	// hand the key space to whatever instrumented the application rather than to the operator.
	MetadataFrom   []string `mapstructure:"metadata_from"`
	MetadataPrefix string   `mapstructure:"metadata_prefix"`

	// TraceMetadata records each record's trace and span ids as the metadata labels "trace_id" and
	// "span_id" (default true). It is on by default, and is the one selection with no analogue in
	// the broker bridges: correlating a memory that survived the decay cycle back to the trace that
	// produced it is the reason a log pipeline would choose this store, and a caller cannot ask for
	// it after the fact - GetMemories filters on metadata, so an unrecorded id is unfindable.
	TraceMetadata bool `mapstructure:"trace_metadata"`

	Significance SignificanceConfig `mapstructure:"significance"`
}

// Validate checks the configuration for internal consistency.
func (c *Config) Validate() error {
	if c.Endpoint == "" {
		return fmt.Errorf("endpoint must be set to the Hippocampus gRPC address (e.g. localhost:50051)")
	}

	switch c.EventBucket {

	case eventBucketNone, eventBucketHour, eventBucketDay:
		// ok

	default:
		return fmt.Errorf("event_bucket must be one of %q, %q, %q; got %q", eventBucketNone, eventBucketHour, eventBucketDay, c.EventBucket)
	}

	if c.CreateEvents && len(c.EventKeyFrom) == 0 {
		return fmt.Errorf("event_key_from must name at least one attribute when create_events is true")
	}

	s := c.Significance
	if s.Min < 0 || s.Max < 0 {
		return fmt.Errorf("significance.min and significance.max must be non-negative")
	}

	if s.Min > s.Max {
		return fmt.Errorf("significance.min (%d) must not exceed significance.max (%d)", s.Min, s.Max)
	}

	if s.Jitter < 0 {
		return fmt.Errorf("significance.jitter must be non-negative")
	}

	// A fixed label the service would refuse is a configuration error, not a per-record one: it is
	// the operator's own value, it is on every memory, and reporting it at startup is the only
	// chance to report it once rather than once per record.
	for k, v := range c.Metadata {
		if normaliseMetadataKey(k) == "" {
			return fmt.Errorf("metadata key %q normalises to nothing; a key must start with a letter or digit", k)
		}

		if len(v) > types.MaxMetadataValueLength {
			return fmt.Errorf("metadata value for key %q is %d bytes (max %d)", k, len(v), types.MaxMetadataValueLength)
		}
	}

	if len(c.Metadata) > types.MaxMetadataKeys {
		return fmt.Errorf("metadata carries %d keys (max %d)", len(c.Metadata), types.MaxMetadataKeys)
	}

	return nil
}
