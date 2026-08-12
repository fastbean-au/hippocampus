package bluesky

import (
	"encoding/json"
	"testing"
	"time"
)

// postFrame is a real-shaped Jetstream frame for a top-level post, used as the base for most of the
// tests here.
const postFrame = `{
  "did": "did:plc:abc123",
  "time_us": 1725911162329308,
  "cursor": 4210,
  "kind": "commit",
  "commit": {
    "rev": "3l3qo2vutsw2b",
    "operation": "create",
    "collection": "app.bsky.feed.post",
    "rkey": "3l3qo2vutsw2b",
    "cid": "bafyrei",
    "record": {
      "$type": "app.bsky.feed.post",
      "text": "hello world",
      "createdAt": "2026-08-12T01:02:03Z",
      "langs": ["en", "fr"],
      "embed": {"$type": "app.bsky.embed.images"}
    }
  }
}`

func decode(t *testing.T, raw string) *event {
	t.Helper()

	var ev event

	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decoding frame: %s", err)
	}

	return &ev
}

func TestToMessageProjectsAPost(t *testing.T) {
	msg := toMessage(decode(t, postFrame))

	if msg.Subject != CollectionPost {
		t.Errorf("Subject = %q, want %q", msg.Subject, CollectionPost)
	}

	// The body is the text alone: the frame envelope has no business in a memory body.
	if string(msg.Data) != "hello world" {
		t.Errorf("Data = %q, want %q", msg.Data, "hello world")
	}

	want := map[string]string{
		hdrDID:        "did:plc:abc123",
		hdrRkey:       "3l3qo2vutsw2b",
		hdrURI:        "at://did:plc:abc123/app.bsky.feed.post/3l3qo2vutsw2b",
		hdrCID:        "bafyrei",
		hdrCollection: CollectionPost,
		hdrOperation:  operationCreate,
		hdrLang:       "en",
		hdrLangs:      "en,fr",
		hdrEmbed:      "app.bsky.embed.images",
		hdrCreatedAt:  "2026-08-12T01:02:03Z",
	}

	for k, v := range want {
		if msg.Headers[k] != v {
			t.Errorf("header %q = %q, want %q", k, msg.Headers[k], v)
		}
	}
}

// TestToMessageTimestampIsCreatedAtNotWitnessTime is the one that matters most here: time_us is when
// the RELAY saw the record. Using it would put the store's decay clock on Jetstream's clock rather
// than on when the post was written.
func TestToMessageTimestampIsCreatedAtNotWitnessTime(t *testing.T) {
	msg := toMessage(decode(t, postFrame))

	want := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)

	if !msg.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %s, want %s", msg.Timestamp, want)
	}

	// time_us as a timestamp would land in September 2024, which is the mistake being guarded.
	if msg.Timestamp.UnixMicro() == 1725911162329308 {
		t.Error("Timestamp came from time_us, not createdAt")
	}
}

// TestToMessageUnparseableCreatedAtLeavesZero: a zero Timestamp is bridge.Message's documented "use
// now", which is the right fallback for a field the client controls and routinely gets wrong.
func TestToMessageUnparseableCreatedAtLeavesZero(t *testing.T) {
	raw := `{"did":"did:plc:a","kind":"commit","commit":{"operation":"create",
	  "collection":"app.bsky.feed.post","rkey":"r","record":{"text":"x","createdAt":"not a date"}}}`

	msg := toMessage(decode(t, raw))

	if !msg.Timestamp.IsZero() {
		t.Errorf("Timestamp = %s, want the zero time", msg.Timestamp)
	}

	// The raw value is still published, so a consumer can see what the client claimed.
	if msg.Headers[hdrCreatedAt] != "not a date" {
		t.Errorf("created-at header = %q", msg.Headers[hdrCreatedAt])
	}
}

func TestToMessageProjectsAReply(t *testing.T) {
	raw := `{"did":"did:plc:a","kind":"commit","commit":{"operation":"create",
	  "collection":"app.bsky.feed.post","rkey":"r","record":{"text":"a reply","createdAt":"2026-08-12T00:00:00Z",
	  "reply":{"root":{"uri":"at://did:plc:root/app.bsky.feed.post/rr"},
	           "parent":{"uri":"at://did:plc:parent/app.bsky.feed.post/pp"}}}}}`

	msg := toMessage(decode(t, raw))

	if msg.Headers[hdrReplyRoot] != "at://did:plc:root/app.bsky.feed.post/rr" {
		t.Errorf("reply-root = %q", msg.Headers[hdrReplyRoot])
	}

	if msg.Headers[hdrReplyParent] != "at://did:plc:parent/app.bsky.feed.post/pp" {
		t.Errorf("reply-parent = %q", msg.Headers[hdrReplyParent])
	}
}

func TestToMessageProjectsALike(t *testing.T) {
	raw := `{"did":"did:plc:liker","kind":"commit","commit":{"operation":"create",
	  "collection":"app.bsky.feed.like","rkey":"lk","record":{"$type":"app.bsky.feed.like",
	  "createdAt":"2026-08-12T00:00:00Z","subject":{"uri":"at://did:plc:a/app.bsky.feed.post/pp","cid":"bafy"}}}}`

	msg := toMessage(decode(t, raw))

	if msg.Headers[hdrSubject] != "at://did:plc:a/app.bsky.feed.post/pp" {
		t.Errorf("subject = %q", msg.Headers[hdrSubject])
	}

	if msg.Subject != CollectionLike {
		t.Errorf("Subject = %q, want %q", msg.Subject, CollectionLike)
	}
}

// TestToMessageDeleteCarriesNoRecord: a delete frame has no record at all, so every record-derived
// header must simply be absent rather than the projection panicking on a nil.
func TestToMessageDeleteCarriesNoRecord(t *testing.T) {
	raw := `{"did":"did:plc:a","kind":"commit","commit":{"operation":"delete",
	  "collection":"app.bsky.feed.post","rkey":"r"}}`

	msg := toMessage(decode(t, raw))

	if msg.Headers[hdrOperation] != operationDelete {
		t.Errorf("operation = %q", msg.Headers[hdrOperation])
	}

	if msg.Headers[hdrURI] != "at://did:plc:a/app.bsky.feed.post/r" {
		t.Errorf("uri = %q", msg.Headers[hdrURI])
	}

	if len(msg.Data) != 0 || !msg.Timestamp.IsZero() {
		t.Errorf("Data = %q, Timestamp = %s; want empty and zero", msg.Data, msg.Timestamp)
	}
}

// TestCursorPrefersSequenceOverWitnessTime pins the v1/v2 fallback. The server distinguishes the two
// forms by magnitude, so handing back whichever it sent is always a valid resume point.
func TestCursorPrefersSequenceOverWitnessTime(t *testing.T) {
	if got := decode(t, postFrame).cursor(); got != 4210 {
		t.Errorf("cursor = %d, want the v2 sequence 4210", got)
	}

	v1 := &event{TimeUS: 1725911162329308}

	if got := v1.cursor(); got != 1725911162329308 {
		t.Errorf("cursor = %d, want the v1 time_us fallback", got)
	}
}

func TestURI(t *testing.T) {
	got := uri("did:plc:abc", CollectionPost, "rkey1")

	if want := "at://did:plc:abc/app.bsky.feed.post/rkey1"; got != want {
		t.Errorf("uri = %q, want %q", got, want)
	}
}

// TestDecodeToleratesUnhandledKinds: identity and account frames arrive unconditionally on
// /subscribe whatever wantedCollections says, and v2 adds more. Decoding must not fail on them.
func TestDecodeToleratesUnhandledKinds(t *testing.T) {
	for _, raw := range []string{
		`{"did":"did:plc:a","kind":"identity","identity":{"handle":"a.bsky.social"}}`,
		`{"did":"did:plc:a","kind":"account","account":{"active":true}}`,
		`{"did":"did:plc:a","kind":"somethingNew","whatever":{"x":1}}`,
	} {
		ev := decode(t, raw)

		if ev.Kind == kindCommit {
			t.Errorf("frame %q decoded as a commit", raw)
		}

		if ev.Commit != nil {
			t.Errorf("frame %q produced a commit", raw)
		}
	}
}
