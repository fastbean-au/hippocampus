package bridge

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
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
