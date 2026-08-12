package bluesky

import (
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// postMessage builds the projection of a post frame directly, so the transform tests do not depend
// on the JSON decoding covered in jetstream_test.go.
func postMessage(text string, headers map[string]string) bridge.Message {
	msg := bridge.Message{
		Subject: CollectionPost,
		Data:    []byte(text),
		Headers: map[string]string{
			hdrCollection: CollectionPost,
			hdrOperation:  operationCreate,
			hdrDID:        "did:plc:abc123",
			hdrRkey:       "3l3qo2vutsw2b",
			hdrURI:        "at://did:plc:abc123/app.bsky.feed.post/3l3qo2vutsw2b",
		},
	}

	for k, v := range headers {
		msg.Headers[k] = v
	}

	return msg
}

func TestTransformYieldsAMemoryIdentifiedByItsURI(t *testing.T) {
	tr := NewTransformer(bridge.TransformConfig{Significance: 10, Group: "bluesky"}, Options{})

	mems, err := tr.Transform(postMessage("hello world", nil))
	if err != nil {
		t.Fatalf("Transform: %s", err)
	}

	if len(mems) != 1 {
		t.Fatalf("got %d memories, want 1", len(mems))
	}

	// The id being the URI is what makes reinforcement stateless and replay idempotent.
	if mems[0].GetId() != "at://did:plc:abc123/app.bsky.feed.post/3l3qo2vutsw2b" {
		t.Errorf("Id = %q", mems[0].GetId())
	}

	if mems[0].GetBody() != "hello world" {
		t.Errorf("Body = %q", mems[0].GetBody())
	}

	if mems[0].GetSignificance() != 10 {
		t.Errorf("Significance = %d, want 10", mems[0].GetSignificance())
	}

	if mems[0].GetGroup() != "bluesky" {
		t.Errorf("Group = %q, want bluesky", mems[0].GetGroup())
	}
}

func TestTransformDrops(t *testing.T) {
	cases := []struct {
		name string
		msg  bridge.Message
		opts Options
	}{
		{
			name: "a like",
			msg: bridge.Message{Subject: CollectionLike, Headers: map[string]string{
				hdrCollection: CollectionLike, hdrOperation: operationCreate,
			}},
		},
		{
			name: "a repost",
			msg: bridge.Message{Subject: CollectionRepost, Headers: map[string]string{
				hdrCollection: CollectionRepost, hdrOperation: operationCreate,
			}},
		},
		{
			name: "a post deletion",
			msg:  postMessage("gone", map[string]string{hdrOperation: operationDelete}),
		},
		{
			name: "a post with no text",
			msg:  postMessage("", nil),
		},
		{
			name: "a post below the minimum length",
			msg:  postMessage("hi", nil),
			opts: Options{MinTextBytes: 8},
		},
		{
			name: "a post in an unwanted language",
			msg:  postMessage("bonjour", map[string]string{hdrLangs: "fr,de"}),
			opts: Options{Langs: []string{"en"}},
		},
		{
			name: "a post whose URI exceeds the service's id cap",
			msg: postMessage("hello", map[string]string{
				hdrURI: "at://did:web:" + strings.Repeat("x", 200) + "/app.bsky.feed.post/r",
			}),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTransformer(bridge.TransformConfig{Significance: 10}, c.opts)

			mems, err := tr.Transform(c.msg)
			if err != nil {
				t.Fatalf("Transform: %s", err)
			}

			if len(mems) != 0 {
				t.Errorf("got %d memories, want none", len(mems))
			}
		})
	}
}

// TestTransformDropsEmptyTextRatherThanSubstituting is the reason the length checks run before the
// inner transformer: DefaultTransformer.body would otherwise turn a text-less post into a memory
// whose body reads "(empty message)", which is right for a broker heartbeat and wrong here.
func TestTransformDropsEmptyTextRatherThanSubstituting(t *testing.T) {
	tr := NewTransformer(bridge.TransformConfig{Significance: 10}, Options{})

	mems, _ := tr.Transform(postMessage("", nil))

	for _, v := range mems {
		if strings.Contains(v.GetBody(), "empty message") {
			t.Fatalf("an empty post became a placeholder memory: %q", v.GetBody())
		}
	}
}

func TestTransformKeepsUpdatesAndUntaggedLanguages(t *testing.T) {
	cases := []struct {
		name string
		msg  bridge.Message
		opts Options
	}{
		{
			name: "an edit to a post is still that post",
			msg:  postMessage("edited", map[string]string{hdrOperation: operationUpdate}),
		},
		{
			// langs is optional in the lexicon, so dropping untagged posts would silently discard a
			// large slice of the firehose.
			name: "a post declaring no language at all",
			msg:  postMessage("hello", nil),
			opts: Options{Langs: []string{"en"}},
		},
		{
			name: "a post declaring one wanted language among several",
			msg:  postMessage("hello", map[string]string{hdrLangs: "fr,en,de"}),
			opts: Options{Langs: []string{"en"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTransformer(bridge.TransformConfig{Significance: 10}, c.opts)

			mems, err := tr.Transform(c.msg)
			if err != nil {
				t.Fatalf("Transform: %s", err)
			}

			if len(mems) != 1 {
				t.Errorf("got %d memories, want 1", len(mems))
			}
		})
	}
}

func TestTransformThreadMode(t *testing.T) {
	root := "at://did:plc:root/app.bsky.feed.post/rr"

	cases := []struct {
		name    string
		events  string
		headers map[string]string
		want    string
	}{
		{
			name:    "a reply joins its thread root's event",
			events:  EventsThread,
			headers: map[string]string{hdrReplyRoot: root},
			want:    root,
		},
		{
			name:   "a top-level post has no event of its own here",
			events: EventsThread,
			want:   "",
		},
		{
			name:    "events=none leaves even a reply standalone",
			events:  EventsNone,
			headers: map[string]string{hdrReplyRoot: root},
			want:    "",
		},
		{
			name:   "an over-long root URI is not used as an event id",
			events: EventsThread,
			headers: map[string]string{
				hdrReplyRoot: "at://did:web:" + strings.Repeat("x", 200) + "/app.bsky.feed.post/r",
			},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTransformer(bridge.TransformConfig{Significance: 10}, Options{Events: c.events})

			mems, err := tr.Transform(postMessage("hello", c.headers))
			if err != nil {
				t.Fatalf("Transform: %s", err)
			}

			if len(mems) != 1 {
				t.Fatalf("got %d memories, want 1", len(mems))
			}

			if mems[0].GetEventId() != c.want {
				t.Errorf("EventId = %q, want %q", mems[0].GetEventId(), c.want)
			}
		})
	}
}

// TestTransformClampsAFutureCreatedAt covers the reuse this wrapper exists for: createdAt is
// client-supplied and routinely ahead of real time, and the service rejects anything more than five
// minutes in the future. The shared transformer's clamp is what makes that safe, so the wrapper must
// keep delegating to it rather than stamping the timestamp itself.
func TestTransformClampsAFutureCreatedAt(t *testing.T) {
	tr := NewTransformer(bridge.TransformConfig{Significance: 10}, Options{})

	msg := postMessage("hello", nil)
	msg.Timestamp = time.Now().Add(72 * time.Hour)

	before := time.Now()

	mems, err := tr.Transform(msg)
	if err != nil {
		t.Fatalf("Transform: %s", err)
	}

	after := time.Now()

	got := mems[0].GetTimeStamp()

	if got < before.UnixNano() || got > after.UnixNano() {
		t.Errorf("timestamp %d was not clamped into [%d, %d]", got, before.UnixNano(), after.UnixNano())
	}
}

func TestTransformCarriesSelectedHeadersAsMetadata(t *testing.T) {
	tr := NewTransformer(bridge.TransformConfig{
		Significance:    10,
		MetadataHeaders: []string{hdrDID, hdrLang},
	}, Options{})

	mems, err := tr.Transform(postMessage("hello", map[string]string{hdrLang: "en"}))
	if err != nil {
		t.Fatalf("Transform: %s", err)
	}

	md := mems[0].GetMetadata()

	if md[hdrDID] != "did:plc:abc123" {
		t.Errorf("metadata %q = %q", hdrDID, md[hdrDID])
	}

	if md[hdrLang] != "en" {
		t.Errorf("metadata %q = %q", hdrLang, md[hdrLang])
	}
}

func TestSplitCommas(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "en", want: []string{"en"}},
		{in: "en,fr", want: []string{"en", "fr"}},
		{in: ",en,,fr,", want: []string{"en", "fr"}},
	}

	for _, c := range cases {
		got := splitCommas(c.in)

		if len(got) != len(c.want) {
			t.Errorf("splitCommas(%q) = %v, want %v", c.in, got, c.want)

			continue
		}

		for i, v := range got {
			if v != c.want[i] {
				t.Errorf("splitCommas(%q)[%d] = %q, want %q", c.in, i, v, c.want[i])
			}
		}
	}
}
