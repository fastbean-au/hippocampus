package bluesky

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// feedPostURI is the id of the post newFakeAppView's `post` helper produces, which is what a feed
// read puts into the capture index.
const feedPostURI = "at://did:plc:news/app.bsky.feed.post/p1"

// postJSONFrom is postJSON with the author's DID under the caller's control, which is what the
// --feed-authors tests turn on: the whole question there is whose repository the record was written
// in.
func postJSONFrom(did string, cursor int64, rkey string, text string, extra string) []byte {
	return []byte(fmt.Sprintf(`{"did":%q,"cursor":%d,"kind":"commit","commit":{
	  "operation":"create","collection":"app.bsky.feed.post","rkey":%q,
	  "record":{"text":%q,"createdAt":"2026-08-12T00:00:00Z"%s}}}`, did, cursor, rkey, text, extra))
}

// replyExtra is the reply block of a post record.
func replyExtra(root string, parent string) string {
	return fmt.Sprintf(`,"reply":{"root":{"uri":%q},"parent":{"uri":%q}}`, root, parent)
}

// captureBridge is a feed-mode bridge with the capture rules under test, seeded from a real feed
// read so the indexes hold what a live bridge's would.
func captureBridge(t *testing.T, cfg Config, client *fakeClient) *Bridge {
	t.Helper()

	cfg.Feed = "at://did:plc:x/app.bsky.feed.generator/news"
	cfg.Recall = true

	av := newFakeAppView(t, [][]feedItem{{post("p1", "a headline", 3, 1)}})

	b := testBridge(t, cfg, client)
	b.feed = testFeed(t, av, false)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := b.pollFeed(ctx); err != nil {
		t.Fatalf("pollFeed: %s", err)
	}

	return b
}

// consumeFrames drives one canned set of frames through the consumer. The stream reports a clean end
// after them so the call returns rather than waiting out the context.
func consumeFrames(t *testing.T, b *Bridge, frames [][]byte) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := b.consume(ctx, &fakeStream{frames: frames, err: errStreamEnded}, 0); err == nil {
		t.Fatal("expected the stream end to return the consumer")
	}
}

// errStreamEnded ends a canned stream. Every frame before it has been handled by then, since consume
// stops at the first failure.
var errStreamEnded = errors.New("stream ended")

// capturedFrom counts the memories stored for one firehose post, which is never the same thing as
// counting every memory the client saw: the feed read that seeded the capture index stored its own
// post through the same fake.
func capturedFrom(client *fakeClient, id string) int {
	n := 0

	for _, v := range client.storedMemories() {
		if v.GetId() == id {
			n++
		}
	}

	return n
}

// TestCaptureRepliesStoresRepliesToHeldThreads covers the rule --capture-replies exists for: in feed
// mode a firehose post is normally reinforcement only, but a reply to a thread this bridge holds is
// the response half of that thread and is stored as well.
//
// The negative case matters as much as the positive one - a reply to somebody else's post must stay
// unstored, or capture would quietly turn a curated feed back into the firehose.
func TestCaptureRepliesStoresRepliesToHeldThreads(t *testing.T) {
	other := "at://did:plc:stranger/app.bsky.feed.post/zz"

	cases := []struct {
		name  string
		extra string
		want  bool
	}{
		{name: "a reply to a held post", extra: replyExtra(feedPostURI, feedPostURI), want: true},
		{
			name:  "a reply deeper in a held thread",
			extra: replyExtra(feedPostURI, "at://did:plc:someone/app.bsky.feed.post/mid"),
			want:  true,
		},
		{name: "a reply whose parent alone is held", extra: replyExtra(other, feedPostURI), want: true},
		{name: "a reply to a thread we do not hold", extra: replyExtra(other, other), want: false},
		{name: "a top-level post by a stranger", extra: "", want: false},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			client := &fakeClient{}
			b := captureBridge(t, Config{Events: EventsNone, CaptureReplies: true}, client)

			consumeFrames(t, b, [][]byte{postJSONFrom("did:plc:stranger", 10, "r1", "a reply", v.extra)})

			stored := capturedFrom(client, "at://did:plc:stranger/app.bsky.feed.post/r1")

			if v.want && stored != 1 {
				t.Errorf("stored the reply %d times, want it captured once", stored)
			}

			if !v.want && stored != 0 {
				t.Errorf("stored the reply %d times, want it left to reinforcement alone", stored)
			}
		})
	}
}

// TestCaptureRepliesStillReinforces pins that capture ADDS to the existing behaviour rather than
// replacing it: a captured reply is both a memory of its own and engagement with its parent, exactly
// as an uncaptured one is engagement alone.
func TestCaptureRepliesStillReinforces(t *testing.T) {
	client := &fakeClient{}
	b := captureBridge(t, Config{Events: EventsNone, CaptureReplies: true}, client)

	consumeFrames(t, b, [][]byte{
		postJSONFrom("did:plc:stranger", 10, "r1", "a reply", replyExtra(feedPostURI, feedPostURI)),
	})

	_, _, recalled, _ := client.snapshot()

	if len(recalled) != 1 || recalled[0] != feedPostURI {
		t.Errorf("recalled %v, want the captured reply to reinforce its parent too", recalled)
	}
}

// TestCaptureRepliesBuildsTheConversationInThreadMode is the point of the feature: with --events
// thread the feed's post is the thread's event and a captured reply becomes a memory IN it, which is
// the "post as event, responses as memories" shape.
func TestCaptureRepliesBuildsTheConversationInThreadMode(t *testing.T) {
	client := &fakeClient{}
	b := captureBridge(t, Config{Events: EventsThread, CaptureReplies: true}, client)

	consumeFrames(t, b, [][]byte{
		postJSONFrom("did:plc:stranger", 10, "r1", "a reply", replyExtra(feedPostURI, feedPostURI)),
	})

	reply := "at://did:plc:stranger/app.bsky.feed.post/r1"

	_, events, _, _ := client.snapshot()

	if len(events) != 1 || events[0].GetId() != feedPostURI {
		t.Fatalf("created events %v, want the held post's thread ensured", events)
	}

	if capturedFrom(client, reply) != 1 {
		t.Fatalf("the reply was not captured")
	}

	for _, v := range client.storedMemories() {
		if v.GetId() != reply {
			continue
		}

		if v.GetEventId() != feedPostURI {
			t.Errorf("the reply's event = %q, want the thread it answers", v.GetEventId())
		}

		break
	}
}

// TestCaptureIndexHoldsOnlyFeedPosts pins the eviction property the index is bounded on: replies
// captured through it are never added to it, so one busy thread cannot displace the other posts the
// remaining threads are matched on. A reply to a captured reply is still captured, because it
// carries the same thread root.
func TestCaptureIndexHoldsOnlyFeedPosts(t *testing.T) {
	client := &fakeClient{}
	b := captureBridge(t, Config{Events: EventsNone, CaptureReplies: true}, client)

	reply := "at://did:plc:stranger/app.bsky.feed.post/r1"

	consumeFrames(t, b, [][]byte{
		postJSONFrom("did:plc:stranger", 10, "r1", "a reply", replyExtra(feedPostURI, feedPostURI)),
		postJSONFrom("did:plc:other", 20, "r2", "a reply to the reply", replyExtra(feedPostURI, reply)),
	})

	if b.stored.Contains(reply) {
		t.Error("a captured reply was added to the capture index, which lets a busy thread evict the feed's own posts")
	}

	if capturedFrom(client, reply) != 1 || capturedFrom(client, "at://did:plc:other/app.bsky.feed.post/r2") != 1 {
		t.Error("expected both levels of the conversation to be captured")
	}
}

// TestFeedAuthorsStoresPostsByFeedAuthors covers the second capture rule: the accounts a feed is
// made of are followed on the firehose, so what they post between the feed's picks is stored too.
func TestFeedAuthorsStoresPostsByFeedAuthors(t *testing.T) {
	cases := []struct {
		name string
		did  string
		want bool
	}{
		{name: "an author the feed surfaced", did: "did:plc:news", want: true},
		{name: "anybody else", did: "did:plc:stranger", want: false},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			client := &fakeClient{}
			b := captureBridge(t, Config{Events: EventsNone, FeedAuthors: true}, client)

			consumeFrames(t, b, [][]byte{postJSONFrom(v.did, 10, "x1", "an unpicked post", "")})

			got := capturedFrom(client, "at://"+v.did+"/app.bsky.feed.post/x1") == 1

			if got != v.want {
				t.Errorf("stored = %v, want %v", got, v.want)
			}
		})
	}
}

// TestFeedAuthorsIgnoresARepliersOwnRepo is the limit worth pinning, because it is the thing the
// flag looks like it does and does not: following the feed's authors brings in what THEY write, and
// a reply to them is written in the replier's repository. Only --capture-replies reaches that.
func TestFeedAuthorsIgnoresARepliersOwnRepo(t *testing.T) {
	client := &fakeClient{}
	b := captureBridge(t, Config{Events: EventsNone, FeedAuthors: true}, client)

	consumeFrames(t, b, [][]byte{
		postJSONFrom("did:plc:stranger", 10, "r1", "a reply to the news", replyExtra(feedPostURI, feedPostURI)),
	})

	if capturedFrom(client, "at://did:plc:stranger/app.bsky.feed.post/r1") != 0 {
		t.Error("--feed-authors stored a reply written by somebody else; that is --capture-replies' job")
	}
}

// TestCaptureRulesComposeWithTheFeedBackfill pins that the indexes are populated by the startup
// backfill as well as by polling - a bridge that only captured after its first poll would ignore
// every reply to the posts it started with.
func TestCaptureRulesComposeWithTheFeedBackfill(t *testing.T) {
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:         EventsNone,
		Feed:           "at://did:plc:x/app.bsky.feed.generator/news",
		CaptureReplies: true,
		FeedAuthors:    true,
	}, client)

	av := newFakeAppView(t, [][]feedItem{{post("p1", "a headline", 3, 1)}})
	b.feed = testFeed(t, av, false)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := b.backfillFeed(ctx); err != nil {
		t.Fatalf("backfillFeed: %s", err)
	}

	if !b.stored.Contains(feedPostURI) {
		t.Error("the backfilled post is not in the capture index")
	}

	if !b.authors.Contains("did:plc:news") {
		t.Error("the backfilled post's author is not in the author index")
	}
}

// TestCaptureIndexesAreOffByDefault pins that neither index is built unless asked for: they are read
// on every post frame on the open firehose, and their absence is what makes that a nil check.
func TestCaptureIndexesAreOffByDefault(t *testing.T) {
	b := New(Config{}, nil)

	if b.stored != nil || b.authors != nil {
		t.Error("expected both capture indexes to be nil when neither flag is set")
	}

	if b.captureFromFirehose(toMessage(mustDecode(t, postJSON(10, "a", "a post", replyExtra("root", "parent"))))) {
		t.Error("expected no post to be captured with both rules off")
	}

	sized := New(Config{CaptureReplies: true, FeedAuthors: true}, nil)

	if sized.cfg.CaptureIndexSize != 5000 || sized.cfg.FeedAuthorsMax != 500 {
		t.Errorf("capture index sizes = %d/%d, want the defaults",
			sized.cfg.CaptureIndexSize, sized.cfg.FeedAuthorsMax)
	}
}

// TestRememberFeedPostsToleratesAnEmptyBatch covers the guard that keeps the indexes off the hot
// path when neither is configured.
func TestRememberFeedPostsToleratesAnEmptyBatch(t *testing.T) {
	New(Config{}, nil).rememberFeedPosts([]feedMemory{{}})

	b := New(Config{CaptureReplies: true, FeedAuthors: true}, nil)

	b.rememberFeedPosts([]feedMemory{{}})

	if b.stored.Contains("") || b.authors.Contains("") {
		t.Error("an empty id was added to a capture index")
	}
}
