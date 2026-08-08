package bridge

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

func fixedNow(t time.Time) func() time.Time {
	return func() time.Time {
		return t
	}
}

func TestDefaultTransformer_EmptyConfigDefaults(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tr := NewDefaultTransformer(TransformConfig{nowFn: fixedNow(now)})

	mems, err := tr.Transform(Message{Subject: "orders.created", Data: []byte("hello")})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if len(mems) != 1 {
		t.Fatalf("want 1 memory, got %d", len(mems))
	}

	m := mems[0]

	if m.GetBody() != "hello" {
		t.Errorf("body = %q, want %q", m.GetBody(), "hello")
	}

	if m.GetSignificance() != 1 {
		t.Errorf("significance = %d, want 1 (floored default)", m.GetSignificance())
	}

	if m.GetGroup() != "" {
		t.Errorf("group = %q, want empty (subject-as-group is off in the zero-value config)", m.GetGroup())
	}

	if m.GetTimeStamp() != now.UnixNano() {
		t.Errorf("timestamp = %d, want now %d", m.GetTimeStamp(), now.UnixNano())
	}

	if m.GetIsBinary() == contract.Bool_TRUE {
		t.Errorf("is_binary should be unset for text body")
	}
}

func TestDefaultTransformer_EmptyBodyPlaceholder(t *testing.T) {
	tr := NewDefaultTransformer(TransformConfig{EmptyBody: "(none)"})

	mems, err := tr.Transform(Message{Subject: "s", Data: nil})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if mems[0].GetBody() != "(none)" {
		t.Errorf("body = %q, want placeholder", mems[0].GetBody())
	}
}

func TestDefaultTransformer_BinaryBase64(t *testing.T) {
	raw := []byte{0x00, 0x01, 0x02, 0xff}
	tr := NewDefaultTransformer(TransformConfig{Binary: true})

	mems, err := tr.Transform(Message{Subject: "s", Data: raw})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if got, want := mems[0].GetBody(), base64.StdEncoding.EncodeToString(raw); got != want {
		t.Errorf("body = %q, want base64 %q", got, want)
	}

	if mems[0].GetIsBinary() != contract.Bool_TRUE {
		t.Errorf("is_binary = %v, want TRUE", mems[0].GetIsBinary())
	}
}

func TestDefaultTransformer_MaxBodyBytesTruncates(t *testing.T) {
	tr := NewDefaultTransformer(TransformConfig{MaxBodyBytes: 3})

	mems, err := tr.Transform(Message{Subject: "s", Data: []byte("abcdefgh")})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if mems[0].GetBody() != "abc" {
		t.Errorf("body = %q, want truncated 'abc'", mems[0].GetBody())
	}
}

func TestDefaultTransformer_SignificanceHeaderOverride(t *testing.T) {
	tr := NewDefaultTransformer(TransformConfig{Significance: 2, SignificanceHeader: "x-sig"})

	mems, err := tr.Transform(Message{
		Subject: "s",
		Data:    []byte("x"),
		Headers: map[string]string{"x-sig": "7"},
	})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if mems[0].GetSignificance() != 7 {
		t.Errorf("significance = %d, want 7 from header", mems[0].GetSignificance())
	}
}

func TestDefaultTransformer_SignificanceHeaderInvalidFallsBack(t *testing.T) {
	tr := NewDefaultTransformer(TransformConfig{Significance: 3, SignificanceHeader: "x-sig"})

	for _, raw := range []string{"", "nope", "-1", "0"} {
		mems, err := tr.Transform(Message{
			Subject: "s",
			Data:    []byte("x"),
			Headers: map[string]string{"x-sig": raw},
		})
		if err != nil {
			t.Fatalf("Transform: %v", err)
		}

		if mems[0].GetSignificance() != 3 {
			t.Errorf("raw %q: significance = %d, want fallback 3", raw, mems[0].GetSignificance())
		}
	}
}

func TestDefaultTransformer_GroupResolution(t *testing.T) {
	tests := []struct {
		name string
		cfg  TransformConfig
		msg  Message
		want string
	}{
		{
			name: "explicit group wins over subject",
			cfg:  TransformConfig{Group: "fixed", GroupFromSubject: true},
			msg:  Message{Subject: "subj"},
			want: "fixed",
		},
		{
			name: "header overrides configured group",
			cfg:  TransformConfig{Group: "fixed", GroupHeader: "g"},
			msg:  Message{Subject: "subj", Headers: map[string]string{"g": "hdr"}},
			want: "hdr",
		},
		{
			name: "subject fallback when enabled",
			cfg:  TransformConfig{GroupFromSubject: true},
			msg:  Message{Subject: "subj"},
			want: "subj",
		},
		{
			name: "no group when disabled and unset",
			cfg:  TransformConfig{GroupFromSubject: false},
			msg:  Message{Subject: "subj"},
			want: "",
		},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			tr := NewDefaultTransformer(v.cfg)

			mems, err := tr.Transform(v.msg)
			if err != nil {
				t.Fatalf("Transform: %v", err)
			}

			if mems[0].GetGroup() != v.want {
				t.Errorf("group = %q, want %q", mems[0].GetGroup(), v.want)
			}
		})
	}
}

func TestDefaultTransformer_FutureTimestampClamped(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	future := now.Add(time.Hour)
	tr := NewDefaultTransformer(TransformConfig{nowFn: fixedNow(now)})

	mems, err := tr.Transform(Message{Subject: "s", Data: []byte("x"), Timestamp: future})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if mems[0].GetTimeStamp() != now.UnixNano() {
		t.Errorf("timestamp = %d, want clamped to now %d", mems[0].GetTimeStamp(), now.UnixNano())
	}
}

func TestDefaultTransformer_PastTimestampPreserved(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	past := now.Add(-time.Hour)
	tr := NewDefaultTransformer(TransformConfig{nowFn: fixedNow(now)})

	mems, err := tr.Transform(Message{Subject: "s", Data: []byte("x"), Timestamp: past})
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}

	if mems[0].GetTimeStamp() != past.UnixNano() {
		t.Errorf("timestamp = %d, want past %d", mems[0].GetTimeStamp(), past.UnixNano())
	}
}

// stringerHeader exercises the fmt.Stringer branch of HeaderString.
type stringerHeader struct{}

func (stringerHeader) String() string { return "stringed" }

func TestHeaderString(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{"plain", "plain"},
		{[]byte("bytes"), "bytes"},
		{stringerHeader{}, "stringed"},
		{42, "42"},
	}

	for _, v := range tests {
		if got := HeaderString(v.in); got != v.want {
			t.Errorf("HeaderString(%v) = %q, want %q", v.in, got, v.want)
		}
	}
}

// TestDefaultTransformerMetadata covers the header selection. It is an allowlist plus an optional
// prefix rather than "copy every header", deliberately: broker headers are unbounded and mostly
// machinery, so copying them all would fill each memory's metadata budget with trace context and
// delivery counts nobody asked for.
func TestDefaultTransformerMetadata(t *testing.T) {
	msg := Message{
		Subject: "events.orders",
		Data:    []byte("payload"),
		Headers: map[string]string{
			"X-Source":      "slack",
			"X-Project":     "apollo",
			"traceparent":   "00-abc-def-01",
			"hippo-team":    "platform",
			"hippo-tier":    "1",
			"hippo-":        "empty key after the prefix",
			"hippo-ignored": "",
			"Not-Selected":  "no",
		},
	}

	cases := []struct {
		name string
		cfg  TransformConfig
		want map[string]string
	}{
		{
			"nothing configured means no metadata",
			TransformConfig{},
			nil,
		},
		{
			"fixed labels only",
			TransformConfig{Metadata: map[string]string{"env": "prod"}},
			map[string]string{"env": "prod"},
		},
		{
			"named headers only, keys normalised",
			TransformConfig{MetadataHeaders: []string{"X-Source", "X-Project"}},
			map[string]string{"x-source": "slack", "x-project": "apollo"},
		},
		{
			"an unlisted header is never copied",
			TransformConfig{MetadataHeaders: []string{"X-Source"}},
			map[string]string{"x-source": "slack"},
		},
		{
			// The prefix is the selector, not part of the label, so it is stripped.
			"prefix selects and strips",
			TransformConfig{MetadataHeaderPrefix: "hippo-"},
			map[string]string{"team": "platform", "tier": "1"},
		},
		{
			"a header selected by name overrides a fixed label",
			TransformConfig{
				Metadata:        map[string]string{"x-source": "fixed"},
				MetadataHeaders: []string{"X-Source"},
			},
			map[string]string{"x-source": "slack"},
		},
		{
			"a missing named header is simply absent",
			TransformConfig{MetadataHeaders: []string{"X-Absent"}},
			nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			memories, err := NewDefaultTransformer(c.cfg).Transform(msg)
			if err != nil {
				t.Fatalf("Transform: %s", err)
			}

			if len(memories) != 1 {
				t.Fatalf("expected one memory, got %d", len(memories))
			}

			if got := memories[0].GetMetadata(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected %#v, got %#v", c.want, got)
			}
		})
	}
}

// TestNormaliseMetadataKey pins the header-name rewriting. Header names vary by broker in exactly
// the ways the service's key charset forbids, so normalising here is what keeps a legitimate
// delivery from being rejected over a name the operator did not choose.
func TestNormaliseMetadataKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"X-Source", "x-source"},
		{"content.type", "content.type"},
		{"a b", "a_b"},
		{"ns:key", "ns:key"},
		{"path/to", "path/to"},
		{"UPPER", "upper"},
		{"  padded  ", "padded"},
		{"9lives", "9lives"},

		// A key must begin alphanumeric, so leading punctuation is dropped rather than substituted -
		// a leading '_' would itself be invalid.
		{"-leading", "leading"},
		{"__x", "x"},

		// Nothing usable at all.
		{"", ""},
		{"---", ""},
	}

	for _, c := range cases {
		if got := normaliseMetadataKey(c.in); got != c.want {
			t.Errorf("normaliseMetadataKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// An over-long name is truncated rather than dropped.
	long := strings.Repeat("k", types.MaxMetadataKeyLength+10)
	if got := normaliseMetadataKey(long); len(got) != types.MaxMetadataKeyLength {
		t.Errorf("expected an over-long key to be truncated to %d, got %d", types.MaxMetadataKeyLength, len(got))
	}
}

// TestBoundMetadataDropsRatherThanFails is the delivery-safety property: an over-eager header
// selection is trimmed to what the service will accept, never turned into an error. A transform
// error is the adapter's cue to nack, which on an at-least-once broker would redeliver the same
// message forever - and the message is not what is at fault.
func TestBoundMetadataDropsRatherThanFails(t *testing.T) {
	headers := make(map[string]string, types.MaxMetadataKeys*2)
	for i := range types.MaxMetadataKeys * 2 {
		headers[fmt.Sprintf("hippo-k%03d", i)] = "v"
	}

	// One value far over the per-entry cap, and one that is fine.
	headers["hippo-huge"] = strings.Repeat("v", types.MaxMetadataValueLength+1)
	headers["hippo-ok"] = "kept"

	memories, err := NewDefaultTransformer(TransformConfig{MetadataHeaderPrefix: "hippo-"}).Transform(Message{
		Subject: "s", Data: []byte("payload"), Headers: headers,
	})
	if err != nil {
		t.Fatalf("an over-eager selection must not fail the delivery: %s", err)
	}

	got := memories[0].GetMetadata()

	if len(got) > types.MaxMetadataKeys {
		t.Errorf("expected at most %d keys, got %d", types.MaxMetadataKeys, len(got))
	}

	if _, present := got["huge"]; present {
		t.Error("an over-long value should have been dropped")
	}

	// Deterministic: keys are considered in sorted order, so the same input always keeps the same
	// entries rather than whichever the map iteration happened to reach first.
	for range 5 {
		again, err := NewDefaultTransformer(TransformConfig{MetadataHeaderPrefix: "hippo-"}).Transform(Message{
			Subject: "s", Data: []byte("payload"), Headers: headers,
		})
		if err != nil {
			t.Fatalf("Transform: %s", err)
		}

		if !reflect.DeepEqual(again[0].GetMetadata(), got) {
			t.Fatal("which metadata survived the cap varied between runs")
		}
	}
}

// TestTransformedMetadataPassesServiceValidation closes the loop: whatever the transformer produces
// must be something the service will actually accept, or every delivery would be rejected at the
// far end for a reason the bridge could have prevented.
func TestTransformedMetadataPassesServiceValidation(t *testing.T) {
	headers := map[string]string{
		"X-Source":    "slack",
		"traceparent": "00-abc-def-01",
		"hippo-A B":   "awkward name",
	}

	memories, err := NewDefaultTransformer(TransformConfig{
		Metadata:             map[string]string{"env": "prod"},
		MetadataHeaders:      []string{"X-Source", "traceparent"},
		MetadataHeaderPrefix: "hippo-",
	}).Transform(Message{Subject: "s", Data: []byte("payload"), Headers: headers})
	if err != nil {
		t.Fatalf("Transform: %s", err)
	}

	if err := types.ValidateMetadata(memories[0].GetMetadata(), "memory"); err != nil {
		t.Errorf("the transformer produced metadata the service would reject: %s", err)
	}
}
