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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
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

	// Metadata is stamped on every memory, before any per-message header labels are added.
	Metadata map[string]string

	// SubjectMetadataKey, when set, records the message's subject/topic as a metadata label under
	// this key, in addition to whatever Group does with it. Empty (the default) records nothing, so
	// an existing bridge is unchanged.
	//
	// It exists because the subject is the one classification the bridge has that was previously
	// only expressible as the group - and group is now also an access boundary when the service
	// issues group-scoped tokens. A scoped token may write only the groups it holds, so
	// GroupFromSubject and a scoped token are mutually exclusive: every message would be refused.
	// Setting Group to the token's own label and SubjectMetadataKey to "subject" keeps the routing
	// information without asking the group to be two things at once, which is the same division
	// metadata was introduced for.
	//
	// The value is the raw subject, not normalised - only the KEY has a restricted charset. An
	// over-long subject is dropped by the metadata bounds rather than truncated, like any other
	// label.
	SubjectMetadataKey string

	// MetadataHeaders names the message headers to copy onto each memory's metadata, and
	// MetadataHeaderPrefix copies every header whose name carries that prefix (the prefix is
	// stripped from the resulting key).
	//
	// Both are opt-in selections rather than "copy every header", deliberately. Broker headers are
	// unbounded and mostly machinery - trace context, delivery counts, redelivery flags, per-broker
	// internals - so copying them all would fill each memory's metadata budget with noise, and the
	// keys would be attacker- or infrastructure-controlled rather than the operator's choice.
	MetadataHeaders      []string
	MetadataHeaderPrefix string

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
		Metadata:     t.metadata(msg),
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
		cut := t.cfg.MaxBodyBytes

		// A memory body is a proto3 string and so must be valid UTF-8. Cutting at an arbitrary byte
		// splits any multi-byte character that straddles the budget, and the resulting message does
		// not fail validation - it fails to MARSHAL ("string field contains invalid UTF-8"), before
		// it ever reaches the service. Because the fault is in the message itself, redelivery cannot
		// clear it: on an at-least-once broker it is a poison message that is nacked and retried
		// forever. So the cut backs up to the last rune boundary. A binary body is base64-encoded
		// below and carries no such constraint, which is why it keeps the exact byte budget.
		if !t.cfg.Binary {
			for cut > 0 && !utf8.RuneStart(data[cut]) {
				cut--
			}
		}

		data = data[:cut]
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

// metadata builds a memory's metadata: the fixed labels first, then the selected headers, which
// override a fixed label of the same key.
//
// Keys are normalised to the service's metadata charset and anything that still does not fit is
// dropped, as is anything over the per-entry or total caps - a header the operator asked for but
// the service would reject must not fail the delivery, because the message is not at fault and a
// nack would redeliver it forever. Dropped selections are logged at Warn so a misconfiguration is
// visible rather than silent.
func (t *DefaultTransformer) metadata(msg Message) map[string]string {
	if len(t.cfg.Metadata) == 0 && len(t.cfg.MetadataHeaders) == 0 &&
		t.cfg.MetadataHeaderPrefix == "" && t.cfg.SubjectMetadataKey == "" {
		return nil
	}

	out := make(map[string]string, len(t.cfg.Metadata)+len(t.cfg.MetadataHeaders)+1)

	for k, v := range t.cfg.Metadata {
		if key := normaliseMetadataKey(k); key != "" {
			out[key] = v
		}
	}

	// The subject goes in before the header-derived labels, so an operator who has deliberately
	// mapped a header onto the same key wins - the header is the more specific instruction.
	if t.cfg.SubjectMetadataKey != "" && msg.Subject != "" {
		if key := normaliseMetadataKey(t.cfg.SubjectMetadataKey); key != "" {
			out[key] = msg.Subject
		}
	}

	for _, name := range t.cfg.MetadataHeaders {
		v, ok := msg.Headers[name]
		if !ok || v == "" {
			continue
		}

		if key := normaliseMetadataKey(name); key != "" {
			out[key] = v
		}
	}

	if prefix := t.cfg.MetadataHeaderPrefix; prefix != "" {
		for name, v := range msg.Headers {
			if v == "" || !strings.HasPrefix(name, prefix) {
				continue
			}

			// The prefix is the selector, not part of the label, so it is stripped: a header named
			// hippo-project becomes project.
			if key := normaliseMetadataKey(strings.TrimPrefix(name, prefix)); key != "" {
				out[key] = v
			}
		}
	}

	return boundMetadata(out)
}

// normaliseMetadataKey rewrites a broker header name into the service's metadata key charset
// ([A-Za-z0-9][A-Za-z0-9._:/-]*), lowercasing it and replacing anything else with '_'. It returns
// "" for a name with no usable leading character, since a key must start alphanumeric.
//
// Header names vary by broker in exactly the ways the charset forbids - MQTT user properties are
// arbitrary, AMQP allows spaces, Kafka keys are raw bytes - so normalising here is what keeps a
// legitimate delivery from being rejected for a name the operator did not choose.
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

// boundMetadata drops entries until the map fits the service's caps, so an over-eager header
// selection is trimmed rather than rejected by the service on every message. Keys are considered in
// sorted order, so which entries survive is deterministic rather than a function of map iteration.
func boundMetadata(metadata map[string]string) map[string]string {
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

		if len(v) > types.MaxMetadataValueLength {
			log.Warnf("eventsource: dropping metadata label '%s': value is %d bytes (max %d)", k, len(v), types.MaxMetadataValueLength)

			continue
		}

		if len(out) >= types.MaxMetadataKeys {
			log.Warnf("eventsource: dropping metadata label '%s': already at the %d-key limit", k, types.MaxMetadataKeys)

			continue
		}

		// A rough per-entry serialised cost: the two quoted strings, the colon, and the separator.
		cost := len(k) + len(v) + 6

		if total+cost > types.MaxMetadataBytes {
			log.Warnf("eventsource: dropping metadata label '%s': would exceed the %d-byte metadata limit", k, types.MaxMetadataBytes)

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
