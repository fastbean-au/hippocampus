// Package bridge is the reusable core shared by the event-sourcing broker bridges (NATS, MQTT,
// RabbitMQ, Kafka). Each broker adapter is a thin consumer that hands every delivered message to a
// bridge.Store, which turns it into one or more Hippocampus memories via a Transformer and writes
// them over gRPC. The Transformer seam is the extension point the TODO calls for: a broker adapter
// ships a DefaultTransformer for the common "one message becomes one memory" case, and an embedding
// program can supply its own callback to shape the payload however it likes.
package bridge

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

// Message is a broker-agnostic delivery. Each adapter normalises its native message onto this shape
// before handing it to the Store, so the transform and store logic never depends on a particular
// broker's client types.
type Message struct {
	// Subject is the broker's routing label for the delivery - a NATS subject, an MQTT topic, a
	// RabbitMQ routing key, or a Kafka topic. It is the natural default for a memory's group.
	Subject string

	// Data is the raw message payload as delivered by the broker.
	Data []byte

	// Headers carries any broker-provided headers/attributes, best-effort and stringified. A
	// Transformer may read a significance or group override from here.
	Headers map[string]string

	// Timestamp is the broker-provided message time when one is available, otherwise the zero value
	// (in which case the Transformer falls back to the current time).
	Timestamp time.Time
}

// Transformer converts a delivered broker message into zero or more Hippocampus memories. Returning
// an empty slice with a nil error drops the message silently (a valid outcome - e.g. a filtered or
// heartbeat message); returning an error causes the Store to report the delivery as failed so the
// adapter can nack/redeliver it. A Transformer must be safe for concurrent use.
type Transformer interface {
	Transform(msg Message) ([]*contract.Memory, error)
}

// TransformerFunc adapts a plain function to the Transformer interface, so an embedding program can
// pass a closure without declaring a type.
type TransformerFunc func(msg Message) ([]*contract.Memory, error)

// Transform calls the wrapped function.
func (f TransformerFunc) Transform(msg Message) ([]*contract.Memory, error) {
	return f(msg)
}

// TransformConfig tunes the DefaultTransformer. Every field has a usable zero value, so the default
// transformer works with an empty config: each message becomes one text memory whose group is the
// message subject and whose significance is DefaultSignificance (which itself defaults to 1 when
// left at 0).
type TransformConfig struct {
	// Significance is the significance stamped on every memory unless a per-message override is read
	// from SignificanceHeader. A value <= 0 is treated as 1 so a memory is never rejected purely
	// because the bridge forgot to rank it.
	Significance int32

	// SignificanceHeader, when set, names a message header whose integer value overrides
	// Significance for that message. A missing, empty, or unparseable header falls back to
	// Significance.
	SignificanceHeader string

	// Group is the group label stamped on every memory. When empty, GroupFromSubject decides.
	Group string

	// GroupFromSubject uses the message subject as the group when Group is empty. Its zero value is
	// off (so a bare TransformConfig produces no group); each adapter's cmd defaults the
	// corresponding flag to on, since subject-as-group is the useful default.
	GroupFromSubject bool

	// GroupHeader, when set, names a message header whose value overrides Group/GroupFromSubject
	// for that message. A missing or empty header falls back to the configured group.
	GroupHeader string

	// Binary marks the produced memories as binary (is_binary=TRUE, never content-indexed) and
	// base64-encodes the payload into the body, since a proto3 string body must be valid UTF-8.
	// Leave false for text payloads (JSON, plain text, log lines).
	Binary bool

	// MaxBodyBytes, when > 0, truncates an over-long payload to this many bytes before it becomes a
	// memory body (measured on the raw payload, before any base64 expansion). 0 means no limit.
	MaxBodyBytes int

	// EmptyBody is substituted when a message payload is empty, so the service's non-empty-body
	// rule never rejects an otherwise-valid delivery. Defaults to "(empty message)".
	EmptyBody string

	// nowFn is injectable for deterministic tests; nil means time.Now.
	nowFn func() time.Time
}

// DefaultTransformer maps one broker message to exactly one memory, driven by a TransformConfig. It
// is the out-of-the-box behaviour every adapter's cmd ships; a program embedding an adapter can
// supply any other Transformer instead.
type DefaultTransformer struct {
	cfg TransformConfig
}

// NewDefaultTransformer returns a DefaultTransformer, applying the zero-value defaults (significance
// floored to 1, group-from-subject on, empty-body placeholder) so an empty config is usable.
func NewDefaultTransformer(cfg TransformConfig) *DefaultTransformer {
	if cfg.Significance <= 0 {
		cfg.Significance = 1
	}

	if cfg.EmptyBody == "" {
		cfg.EmptyBody = "(empty message)"
	}

	if cfg.nowFn == nil {
		cfg.nowFn = time.Now
	}

	return &DefaultTransformer{cfg: cfg}
}

// Transform builds a single memory from the message.
func (t *DefaultTransformer) Transform(msg Message) ([]*contract.Memory, error) {
	body, isBinary := t.body(msg.Data)

	mem := &contract.Memory{
		Body:         body,
		Significance: t.significance(msg),
		TimeStamp:    t.timestamp(msg).UnixNano(),
		Group:        t.group(msg),
	}

	if isBinary {
		mem.IsBinary = contract.Bool_TRUE
	}

	return []*contract.Memory{mem}, nil
}

// body renders the payload into a memory body, truncating, base64-encoding (when Binary), and
// substituting the empty-body placeholder as configured. The bool reports whether the body was
// encoded as binary.
func (t *DefaultTransformer) body(data []byte) (string, bool) {
	if t.cfg.MaxBodyBytes > 0 && len(data) > t.cfg.MaxBodyBytes {
		data = data[:t.cfg.MaxBodyBytes]
	}

	if len(data) == 0 {
		return t.cfg.EmptyBody, false
	}

	if t.cfg.Binary {
		return base64.StdEncoding.EncodeToString(data), true
	}

	return string(data), false
}

// significance resolves the per-message significance, preferring a valid SignificanceHeader value
// over the configured default.
func (t *DefaultTransformer) significance(msg Message) int32 {
	if t.cfg.SignificanceHeader != "" {
		if raw, ok := msg.Headers[t.cfg.SignificanceHeader]; ok && raw != "" {
			if v, err := strconv.ParseInt(raw, 10, 32); err == nil && v > 0 {
				return int32(v)
			}
		}
	}

	return t.cfg.Significance
}

// group resolves the per-message group: an explicit GroupHeader wins, then the configured Group,
// then the subject when GroupFromSubject is set.
func (t *DefaultTransformer) group(msg Message) string {
	if t.cfg.GroupHeader != "" {
		if v, ok := msg.Headers[t.cfg.GroupHeader]; ok && v != "" {
			return v
		}
	}

	if t.cfg.Group != "" {
		return t.cfg.Group
	}

	if t.cfg.GroupFromSubject {
		return msg.Subject
	}

	return ""
}

// timestamp resolves the memory timestamp, falling back to now when the broker gave none and
// clamping a future timestamp back to now so the service's clock-skew guard never rejects the
// write (mirroring the OTEL exporter's recordTime).
func (t *DefaultTransformer) timestamp(msg Message) time.Time {
	now := t.cfg.nowFn()
	if msg.Timestamp.IsZero() {
		return now
	}

	if msg.Timestamp.After(now) {
		return now
	}

	return msg.Timestamp
}

// HeaderString stringifies a native broker header value so an adapter can populate Message.Headers
// uniformly, whatever concrete type the client library hands it (string, []byte, fmt.Stringer, or
// anything else via %v).
func HeaderString(v any) string {
	switch t := v.(type) {

	case string:
		return t

	case []byte:
		return string(t)

	case fmt.Stringer:
		return t.String()

	default:
		return fmt.Sprintf("%v", v)
	}
}
