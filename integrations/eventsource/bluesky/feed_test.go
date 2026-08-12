package bluesky

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// fakeAppView serves getFeed from canned pages, recording the queries it was called with.
type fakeAppView struct {
	server *httptest.Server

	mu      sync.Mutex
	queries []string
	calls   int

	pages  [][]feedItem
	status int
	body   string
}

func newFakeAppView(t *testing.T, pages [][]feedItem) *fakeAppView {
	t.Helper()

	f := &fakeAppView{pages: pages}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.queries = append(f.queries, r.URL.RawQuery)
		status, body := f.status, f.body
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))

			return
		}

		// The cursor is simply the index of the next page, so pagination is assertable.
		idx := 0

		if c := r.URL.Query().Get("cursor"); c != "" {
			_, _ = fmt.Sscanf(c, "%d", &idx)
		}

		resp := feedResponse{}

		if idx < len(f.pages) {
			resp.Feed = f.pages[idx]

			if idx+1 < len(f.pages) {
				resp.Cursor = fmt.Sprintf("%d", idx+1)
			}
		}

		_ = json.NewEncoder(w).Encode(resp)
	}))

	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeAppView) snapshot() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.queries))
	copy(out, f.queries)

	return f.calls, out
}

// post builds one hydrated feed item.
func post(rkey string, text string, likes int, reposts int) feedItem {
	return feedItem{Post: &postView{
		URI:    "at://did:plc:news/app.bsky.feed.post/" + rkey,
		CID:    "bafy" + rkey,
		Author: &author{DID: "did:plc:news", Handle: "reuters.com"},
		Record: &record{
			Text:      text,
			CreatedAt: "2026-08-12T00:00:00Z",
			Langs:     []string{"en"},
		},
		LikeCount:   likes,
		RepostCount: reposts,
	}}
}

// postWithLink builds a feed item carrying an external link card.
func postWithLink(rkey string, text string, uri string) feedItem {
	item := post(rkey, text, 1, 0)
	item.Post.Record.Embed = &embed{
		Type:     "app.bsky.embed.external",
		External: &external{URI: uri},
	}

	return item
}

func testFeed(t *testing.T, av *fakeAppView, seed bool) *feedSource {
	t.Helper()

	tr := NewTransformer(bridge.TransformConfig{Significance: 10, Group: "news"}, Options{})

	return newFeedSource(Config{
		Feed:            "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:     av.server.URL,
		FeedSeedRecalls: seed,
	}, tr)
}

func TestFeedBackfillPagesTheWholeFeed(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{
		{post("a", "headline one", 6, 1), post("b", "headline two", 0, 0)},
		{post("c", "headline three", 500, 100)},
	})

	mems, err := testFeed(t, av, true).Backfill(context.Background())
	if err != nil {
		t.Fatalf("Backfill: %s", err)
	}

	if len(mems) != 3 {
		t.Fatalf("got %d memories, want 3 across both pages", len(mems))
	}

	if mems[0].Memory.GetId() != "at://did:plc:news/app.bsky.feed.post/a" {
		t.Errorf("id = %q, want the post's at:// URI", mems[0].Memory.GetId())
	}

	calls, queries := av.snapshot()

	// Two pages plus the one that reports no further cursor.
	if calls != 2 {
		t.Errorf("getFeed called %d times, want 2", calls)
	}

	if !strings.Contains(queries[0], "feed=at%3A%2F%2F") {
		t.Errorf("first query %q did not name the feed", queries[0])
	}

	if strings.Contains(queries[0], "cursor=") {
		t.Errorf("first query %q should carry no cursor", queries[0])
	}

	if !strings.Contains(queries[1], "cursor=1") {
		t.Errorf("second query %q did not follow the cursor", queries[1])
	}
}

// TestFeedBackfillSeedsDampedRecallCounts is the point of seeding: a backfilled post's likes are all
// in the past, so the firehose will never report them and the store would otherwise show several
// hundred headlines as equally untouched.
func TestFeedBackfillSeedsDampedRecallCounts(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		post("none", "no engagement", 0, 0),
		post("some", "a little", 6, 0),
		post("viral", "a lot", 5000, 658),
	}})

	mems, err := testFeed(t, av, true).Backfill(context.Background())
	if err != nil {
		t.Fatalf("Backfill: %s", err)
	}

	got := map[string]int32{}
	for _, m := range mems {
		got[rkeyOf(m.Memory.GetId())] = m.Memory.GetRecallCount()
	}

	if got["none"] != 0 {
		t.Errorf("unengaged recall count = %d, want 0", got["none"])
	}

	if got["some"] != 2 {
		t.Errorf("6-like recall count = %d, want 2", got["some"])
	}

	// The damping is what stops a viral post becoming permanently unforgettable: 5,658 engagements
	// must land in single digits, not in the thousands.
	if got["viral"] < 8 || got["viral"] > 10 {
		t.Errorf("viral recall count = %d, want single digits", got["viral"])
	}

	if got["viral"] <= got["some"] {
		t.Error("damping must still rank a viral post above a lightly-liked one")
	}
}

func TestFeedSeedingCanBeTurnedOff(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "headline", 500, 20)}})

	mems, err := testFeed(t, av, false).Backfill(context.Background())
	if err != nil {
		t.Fatalf("Backfill: %s", err)
	}

	if mems[0].Memory.GetRecallCount() != 0 {
		t.Errorf("recall count = %d, want 0 when seeding is off", mems[0].Memory.GetRecallCount())
	}
}

// TestFeedPollNeverSeeds: polling re-reads the same posts, and a seeded count written through an
// upsert would roll live reinforcement back to whatever the feed last reported.
func TestFeedPollNeverSeeds(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "headline", 900, 90)}})

	mems, err := testFeed(t, av, true).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %s", err)
	}

	if len(mems) != 1 {
		t.Fatalf("got %d memories, want 1", len(mems))
	}

	if mems[0].Memory.GetRecallCount() != 0 {
		t.Errorf("poll produced recall count %d, want 0 even with seeding configured",
			mems[0].Memory.GetRecallCount())
	}
}

// TestFeedPollReadsOnlyTheFirstPage: polling wants what is new, not the whole feed again.
func TestFeedPollReadsOnlyTheFirstPage(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{
		{post("a", "one", 1, 0)},
		{post("b", "two", 1, 0)},
	})

	if _, err := testFeed(t, av, false).Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %s", err)
	}

	if calls, _ := av.snapshot(); calls != 1 {
		t.Errorf("getFeed called %d times, want 1", calls)
	}
}

// TestFeedUsesTheSameTransformer pins the reuse that keeps the two sources consistent: a post read
// over HTTP must be filtered exactly as one read off the firehose.
func TestFeedUsesTheSameTransformer(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		post("keep", "a proper headline", 5, 0),
		post("empty", "", 5, 0),
		post("short", "hi", 5, 0),
	}})

	tr := NewTransformer(bridge.TransformConfig{Significance: 10}, Options{MinTextBytes: 8})

	src := newFeedSource(Config{
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
	}, tr)

	mems, err := src.Backfill(context.Background())
	if err != nil {
		t.Fatalf("Backfill: %s", err)
	}

	if len(mems) != 1 {
		t.Fatalf("got %d memories, want only the one passing the transformer's filters", len(mems))
	}

	if rkeyOf(mems[0].Memory.GetId()) != "keep" {
		t.Errorf("kept %q, want the long-enough headline", rkeyOf(mems[0].Memory.GetId()))
	}
}

func TestFeedCarriesTheAuthorHandle(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "a headline", 1, 0)}})

	tr := NewTransformer(bridge.TransformConfig{
		Significance:    10,
		MetadataHeaders: []string{hdrHandle, hdrDID},
	}, Options{})

	src := newFeedSource(Config{FeedAppView: av.server.URL}, tr)

	mems, err := src.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %s", err)
	}

	// A DID is unreadable; the handle names the news organisation, and only the feed path has it.
	if got := mems[0].Memory.GetMetadata()[hdrHandle]; got != "reuters.com" {
		t.Errorf("handle metadata = %q, want reuters.com", got)
	}
}

func TestFeedSkipsMalformedItems(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		{Post: nil},
		{Post: &postView{URI: "at://x/app.bsky.feed.post/a"}}, // no record, no author
		post("ok", "a real headline", 1, 0),
	}})

	mems, err := testFeed(t, av, false).Backfill(context.Background())
	if err != nil {
		t.Fatalf("Backfill: %s", err)
	}

	if len(mems) != 1 {
		t.Errorf("got %d memories, want only the well-formed one", len(mems))
	}
}

func TestFeedErrors(t *testing.T) {
	t.Run("a non-200 is reported with the body", func(t *testing.T) {
		av := newFakeAppView(t, nil)
		av.status = http.StatusBadRequest
		av.body = `{"error":"UnknownFeed"}`

		_, err := testFeed(t, av, false).Poll(context.Background())
		if err == nil {
			t.Fatal("expected the error status to be reported")
		}

		if !strings.Contains(err.Error(), "UnknownFeed") {
			t.Errorf("error %q does not carry the AppView's message", err)
		}
	})

	t.Run("an undecodable body is reported", func(t *testing.T) {
		av := newFakeAppView(t, nil)
		av.status = http.StatusOK
		av.body = "not json"

		if _, err := testFeed(t, av, false).Poll(context.Background()); err == nil {
			t.Error("expected an undecodable response to be reported")
		}
	})

	t.Run("an unreachable AppView is reported", func(t *testing.T) {
		tr := NewTransformer(bridge.TransformConfig{Significance: 10}, Options{})
		src := newFeedSource(Config{FeedAppView: "http://127.0.0.1:1"}, tr)

		if _, err := src.Poll(context.Background()); err == nil {
			t.Error("expected the connection failure to be reported")
		}
	})

	t.Run("a malformed AppView URL is reported", func(t *testing.T) {
		tr := NewTransformer(bridge.TransformConfig{Significance: 10}, Options{})
		src := newFeedSource(Config{FeedAppView: "://not a url"}, tr)

		if _, err := src.Poll(context.Background()); err == nil {
			t.Error("expected a malformed URL to be reported")
		}
	})

	t.Run("a cancelled context stops a backfill", func(t *testing.T) {
		av := newFakeAppView(t, [][]feedItem{{post("a", "one", 1, 0)}})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := testFeed(t, av, false).Backfill(ctx); err == nil {
			t.Error("expected the cancellation to be reported")
		}
	})
}

func TestSeededRecallCount(t *testing.T) {
	cases := []struct {
		likes   int
		reposts int
		want    int32
	}{
		{likes: 0, reposts: 0, want: 0},
		{likes: 1, reposts: 0, want: 1},
		{likes: 6, reposts: 0, want: 2},
		{likes: 3, reposts: 3, want: 2}, // reposts count alongside likes
		{likes: 50, reposts: 0, want: 4},
		{likes: 5000, reposts: 658, want: 9},
	}

	for _, c := range cases {
		got := seededRecallCount(&postView{LikeCount: c.likes, RepostCount: c.reposts})

		if got != c.want {
			t.Errorf("seededRecallCount(%d likes, %d reposts) = %d, want %d",
				c.likes, c.reposts, got, c.want)
		}
	}

	// Replies are deliberately excluded: a reply is its own post, and on a news feed it is as often
	// disagreement as endorsement.
	if got := seededRecallCount(&postView{ReplyCount: 100}); got != 0 {
		t.Errorf("replies alone produced %d, want 0", got)
	}
}

// TestBridgeFeedModeSkipsFirehosePosts: in feed mode the feed decides what is stored, so a post
// arriving on the firehose must not be written - but a reply must still reinforce its parent, or
// that engagement is silently lost.
func TestBridgeFeedModeSkipsFirehosePosts(t *testing.T) {
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events: EventsNone,
		Recall: true,
		Feed:   "at://did:plc:x/app.bsky.feed.generator/news",
	}, client)

	parent := "at://did:plc:news/app.bsky.feed.post/target"
	extra := fmt.Sprintf(`,"reply":{"root":{"uri":%q},"parent":{"uri":%q}}`, parent, parent)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	frames := [][]byte{
		postJSON(10, "a", "a firehose post", ""),
		postJSON(20, "b", "a firehose reply", extra),
	}

	if _, err := b.consume(ctx, &fakeStream{frames: frames}, 0); err != nil {
		t.Fatalf("consume: %s", err)
	}

	memories, _, recalled, _ := client.snapshot()

	if len(memories) != 0 {
		t.Errorf("stored %d firehose posts, want 0 in feed mode", len(memories))
	}

	if len(recalled) != 1 || recalled[0] != parent {
		t.Errorf("recalled %v, want the reply's parent reinforced", recalled)
	}
}

// TestBridgeFeedModeStillHonoursDeletes: the feed supplies posts, but a withdrawal is still a
// withdrawal and only the firehose reports it.
func TestBridgeFeedModeStillHonoursDeletes(t *testing.T) {
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:        EventsNone,
		HonourDeletes: true,
		Feed:          "at://did:plc:x/app.bsky.feed.generator/news",
	}, client)

	del := []byte(`{"did":"did:plc:abc","cursor":9,"kind":"commit","commit":{"operation":"delete",
	  "collection":"app.bsky.feed.post","rkey":"a"}}`)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := b.consume(ctx, &fakeStream{frames: [][]byte{del}}, 0); err != nil {
		t.Fatalf("consume: %s", err)
	}

	if _, _, _, deleted := client.snapshot(); len(deleted) != 1 {
		t.Errorf("deleted %v, want the withdrawal honoured in feed mode too", deleted)
	}
}

func TestNewBuildsAFeedSourceOnlyWhenConfigured(t *testing.T) {
	if New(Config{}, nil).feedMode() {
		t.Error("feed mode should be off with no --feed")
	}

	b := New(Config{Feed: "at://did:plc:x/app.bsky.feed.generator/news"}, bridge.NewStore(
		&fakeClient{}, bridge.TransformerFunc(nil), 0, "bluesky"))

	if !b.feedMode() {
		t.Error("feed mode should be on when --feed is set")
	}

	if b.cfg.FeedPollInterval <= 0 {
		t.Error("the poll interval should have defaulted")
	}
}

// TestRunFeedBackfillsThenPolls drives the feed goroutine end to end against a fake AppView: the
// backfill imports with seeded recalls, and the subsequent poll stores without them.
func TestRunFeedBackfillsThenPolls(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "a headline", 6, 0)}})
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:           EventsNone,
		Feed:             "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:      av.server.URL,
		FeedBackfill:     true,
		FeedSeedRecalls:  true,
		FeedPollInterval: 20 * time.Millisecond,
	}, client)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		b.runFeed(ctx)
		close(done)
	}()

	// Wait for the backfill import plus at least one poll.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(client.importedBatches()) > 0 && len(client.storedMemories()) > 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	select {

	case <-done:

	case <-time.After(3 * time.Second):
		t.Fatal("runFeed did not return on cancellation")
	}

	imported := client.importedBatches()

	if len(imported) != 1 || len(imported[0]) != 1 {
		t.Fatalf("imported %v, want one seeded batch of one", imported)
	}

	if imported[0][0].GetRecallCount() != 2 {
		t.Errorf("seeded recall count = %d, want the damped 2", imported[0][0].GetRecallCount())
	}

	// The poll path must write through StoreMemory, never a second import that would clobber it.
	if len(client.storedMemories()) == 0 {
		t.Error("the poll never stored anything")
	}
}

// TestRunFeedSurvivesAFailingAppView: the feed is a secondary source, and an AppView having a bad
// minute must not take down a bridge whose engagement stream is still flowing.
func TestRunFeedSurvivesAFailingAppView(t *testing.T) {
	av := newFakeAppView(t, nil)
	av.status = http.StatusInternalServerError
	av.body = "boom"

	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:           EventsNone,
		Feed:             "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:      av.server.URL,
		FeedBackfill:     true,
		FeedPollInterval: 10 * time.Millisecond,
	}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})

	go func() {
		b.runFeed(ctx)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(3 * time.Second):
		t.Fatal("runFeed did not return despite the AppView failing")
	}

	if calls, _ := av.snapshot(); calls < 2 {
		t.Errorf("AppView called %d times, want the poller to have retried", calls)
	}
}

func TestBackfillFeedSkipsTheImportWhenTheFeedIsEmpty(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{}})
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:      EventsNone,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
	}, client)

	if err := b.backfillFeed(context.Background()); err != nil {
		t.Fatalf("backfillFeed: %s", err)
	}

	if len(client.importedBatches()) != 0 {
		t.Error("an empty feed should issue no import")
	}
}

func TestPollFeedSkipsTheWriteWhenTheFeedIsEmpty(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{}})
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:      EventsNone,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
	}, client)

	if err := b.pollFeed(context.Background()); err != nil {
		t.Fatalf("pollFeed: %s", err)
	}

	if len(client.storedMemories()) != 0 {
		t.Error("an empty feed should issue no writes")
	}
}

func TestFeedWriteFailuresPropagate(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "a headline", 1, 0)}})

	t.Run("backfill", func(t *testing.T) {
		client := &fakeClient{importErr: errors.New("unavailable")}

		b := testBridge(t, Config{Events: EventsNone, Feed: "at://f", FeedAppView: av.server.URL}, client)

		if err := b.backfillFeed(context.Background()); err == nil {
			t.Error("expected the import failure to propagate")
		}
	})

	t.Run("poll", func(t *testing.T) {
		client := &fakeClient{storeErr: errors.New("unavailable")}

		b := testBridge(t, Config{Events: EventsNone, Feed: "at://f", FeedAppView: av.server.URL}, client)

		if err := b.pollFeed(context.Background()); err == nil {
			t.Error("expected the store failure to propagate")
		}
	})
}

// TestFeedMessageCarriesEveryRecordField covers the projection branches a plain headline does not
// reach: multiple languages, an embed, and a reply root.
func TestFeedMessageCarriesEveryRecordField(t *testing.T) {
	item := post("a", "a headline", 1, 0)
	item.Post.Record.Langs = []string{"en", "fr", "de"}
	item.Post.Record.Embed = &embed{Type: "app.bsky.embed.external"}
	item.Post.Record.Reply = &reply{Root: &ref{URI: "at://did:plc:root/app.bsky.feed.post/rr"}}

	av := newFakeAppView(t, [][]feedItem{{item}})

	tr := NewTransformer(bridge.TransformConfig{
		Significance:    10,
		MetadataHeaders: []string{hdrLangs, hdrEmbed},
	}, Options{Events: EventsThread})

	src := newFeedSource(Config{FeedAppView: av.server.URL}, tr)

	mems, err := src.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %s", err)
	}

	md := mems[0].Memory.GetMetadata()

	if md[hdrLangs] != "en,fr,de" {
		t.Errorf("langs metadata = %q, want the joined list", md[hdrLangs])
	}

	if md[hdrEmbed] != "app.bsky.embed.external" {
		t.Errorf("embed metadata = %q", md[hdrEmbed])
	}

	// Thread mode must file a feed-sourced reply under its root exactly as a firehose one.
	if mems[0].Memory.GetEventId() != "at://did:plc:root/app.bsky.feed.post/rr" {
		t.Errorf("event id = %q, want the reply root", mems[0].Memory.GetEventId())
	}
}

func TestFeedItemWithoutACIDOrTimestamp(t *testing.T) {
	item := post("a", "a headline", 1, 0)
	item.Post.CID = ""
	item.Post.Record.CreatedAt = ""
	item.Post.Record.Langs = nil

	av := newFakeAppView(t, [][]feedItem{{item}})

	mems, err := testFeed(t, av, false).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %s", err)
	}

	// A missing createdAt leaves the timestamp zero, which the shared transformer fills with now.
	if mems[0].Memory.GetTimeStamp() <= 0 {
		t.Errorf("timestamp = %d, want the transformer's fallback to now", mems[0].Memory.GetTimeStamp())
	}
}

func TestFeedUnparseableCreatedAtFallsBackToNow(t *testing.T) {
	item := post("a", "a headline", 1, 0)
	item.Post.Record.CreatedAt = "not a date"

	av := newFakeAppView(t, [][]feedItem{{item}})

	mems, err := testFeed(t, av, false).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %s", err)
	}

	if mems[0].Memory.GetTimeStamp() <= 0 {
		t.Error("an unparseable createdAt should fall back to now, not to zero")
	}
}

// TestRunStartsTheFeedPoller drives feed mode through Run itself, so the goroutine wiring is covered
// rather than only runFeed in isolation.
func TestRunStartsTheFeedPoller(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "a headline", 6, 0)}})
	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:           EventsNone,
		Feed:             "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:      av.server.URL,
		FeedBackfill:     true,
		FeedSeedRecalls:  true,
		FeedPollInterval: time.Hour,
		ReconnectBackoff: time.Millisecond,
	}, client)

	ctx, cancel := context.WithCancel(context.Background())

	// Keep the firehose side inert so only the feed does anything.
	b.dial = func(dialCtx context.Context, cfg Config, cursor int64) (stream, error) {
		return &fakeStream{}, nil
	}

	done := make(chan error, 1)

	go func() { done <- b.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(client.importedBatches()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %s", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if len(client.importedBatches()) != 1 {
		t.Errorf("imported %d batches, want the backfill to have run through Run",
			len(client.importedBatches()))
	}
}

func TestJoinCommas(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{in: nil, want: ""},
		{in: []string{"en"}, want: "en"},
		{in: []string{"en", "fr", "de"}, want: "en,fr,de"},
	}

	for _, c := range cases {
		if got := joinCommas(c.in); got != c.want {
			t.Errorf("joinCommas(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// replyPost builds a feed item that is a reply to root, which is what a news account continuing its
// own thread looks like in a curated feed.
func replyPost(rkey string, text string, root string) feedItem {
	item := post(rkey, text, 0, 0)
	item.Post.Record.Reply = &reply{
		Root:   &ref{URI: root},
		Parent: &ref{URI: root},
	}

	return item
}

// TestPollFeedOpensTheThreadOfAReplyAndStoresThePageAfterIt is the regression test for a stalled
// feed poll.
//
// Under --events thread the Transformer puts a reply's memory in its thread ROOT's event, and the
// service refuses a memory naming an event it does not hold. The feed path never opened that event
// (only the firehose path did), so the reply was refused - and the refusal aborted the write of the
// whole page, leaving every post after it in the page unwritten. Because a poll returns the same
// page each time, the next tick stopped at the same post, and the store never grew past it.
//
// Both halves are asserted here: the thread is opened, and the posts after the reply are stored.
func TestPollFeedOpensTheThreadOfAReplyAndStoresThePageAfterIt(t *testing.T) {
	const root = "at://did:plc:news/app.bsky.feed.post/root"

	av := newFakeAppView(t, [][]feedItem{{
		post("a", "first headline", 0, 0),
		replyPost("b", "the thread continues", root),
		post("c", "third headline", 0, 0),
	}})

	client := &fakeClient{strictEvents: true}

	b := testBridge(t, Config{
		Events:      EventsThread,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
		Group:       "news",
	}, client)

	if err := b.pollFeed(context.Background()); err != nil {
		t.Fatalf("pollFeed: %s", err)
	}

	stored, events, _, _ := client.snapshot()

	if len(stored) != 3 {
		t.Fatalf("stored %d memories, want all three of the page", len(stored))
	}

	if stored[2].GetId() != "at://did:plc:news/app.bsky.feed.post/c" {
		t.Errorf("last stored memory is %q, want the page to have continued past the reply", stored[2].GetId())
	}

	if len(events) != 1 || events[0].GetId() != root {
		t.Fatalf("events = %v, want the reply's thread root to have been opened", events)
	}

	if events[0].GetGroup() != "news" {
		t.Errorf("thread event group = %q, want the bridge's configured group", events[0].GetGroup())
	}

	// One event per thread, not one per post: the roots cache means a second poll of the same page
	// costs no further StoreEvent calls.
	if err := b.pollFeed(context.Background()); err != nil {
		t.Fatalf("second pollFeed: %s", err)
	}

	if _, events, _, _ = client.snapshot(); len(events) != 1 {
		t.Errorf("StoreEvent called %d times over two polls, want the roots cache to have covered the second", len(events))
	}
}

// TestPollFeedSkipsAReplyWhoseThreadCannotBeOpened: opening the thread is best-effort, and what
// makes that acceptable is that the memory it belongs to is then skipped on its own rather than
// taking the rest of the page with it.
func TestPollFeedSkipsAReplyWhoseThreadCannotBeOpened(t *testing.T) {
	const root = "at://did:plc:news/app.bsky.feed.post/root"

	av := newFakeAppView(t, [][]feedItem{{
		replyPost("b", "the thread continues", root),
		post("c", "third headline", 0, 0),
	}})

	client := &fakeClient{strictEvents: true, eventErr: errUnavailable}

	b := testBridge(t, Config{
		Events:      EventsThread,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
	}, client)

	if err := b.pollFeed(context.Background()); err != nil {
		t.Fatalf("pollFeed should survive an event that cannot be opened, got %s", err)
	}

	stored, _, _, _ := client.snapshot()

	if len(stored) != 1 || stored[0].GetId() != "at://did:plc:news/app.bsky.feed.post/c" {
		t.Fatalf("stored %v, want only the post after the skipped reply", stored)
	}
}

// TestSeedFeedRetriesPastAColdStart: the seed runs once, at startup, which is exactly when the IdP
// it mints its token from may still be booting. Without a retry that race leaves the deployment
// permanently unseeded, with nothing but one Warn line to say so.
func TestSeedFeedRetriesPastAColdStart(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "headline", 3, 0)}})

	// Two failures then success, standing in for an IdP that is not yet answering: the bearer
	// interceptor fails the RPC before it reaches the service.
	client := &fakeClient{importErr: errors.New("obtaining bearer token: connection refused")}

	b := testBridge(t, Config{
		Events:          EventsNone,
		Feed:            "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:     av.server.URL,
		FeedBackfill:    true,
		FeedSeedRecalls: true,
	}, client)

	go func() {
		time.Sleep(20 * time.Millisecond)

		client.mu.Lock()
		client.importErr = nil
		client.mu.Unlock()
	}()

	b.seedFeed(context.Background())

	if len(client.importedBatches()) != 1 {
		t.Fatalf("imported %d batches, want the seed to have retried until it succeeded",
			len(client.importedBatches()))
	}

	if calls, _ := av.snapshot(); calls < 2 {
		t.Errorf("AppView called %d times, want the whole seed to have been re-run", calls)
	}
}

// TestSeedFeedGivesUpAndLetsPollingContinue: a retry that never succeeds must still hand over to
// live polling rather than blocking the feed goroutine for the life of the process.
func TestSeedFeedGivesUpAndLetsPollingContinue(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "headline", 3, 0)}})
	client := &fakeClient{importErr: errors.New("still down")}

	b := testBridge(t, Config{
		Events:       EventsNone,
		Feed:         "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:  av.server.URL,
		FeedBackfill: true,
	}, client)

	b.backfillAttempts = 3

	done := make(chan struct{})

	go func() {
		b.seedFeed(context.Background())
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(5 * time.Second):
		t.Fatal("seedFeed did not give up")
	}

	if calls, _ := av.snapshot(); calls != 3 {
		t.Errorf("AppView called %d times, want exactly the three attempts", calls)
	}
}

// TestSeedFeedStopsOnCancellation: the retry must not hold a shutting-down bridge open.
func TestSeedFeedStopsOnCancellation(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{post("a", "headline", 3, 0)}})
	client := &fakeClient{importErr: errors.New("still down")}

	b := testBridge(t, Config{
		Events:       EventsNone,
		Feed:         "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView:  av.server.URL,
		FeedBackfill: true,
	}, client)

	b.backfillBackoff = time.Hour

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		b.seedFeed(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {

	case <-done:

	case <-time.After(3 * time.Second):
		t.Fatal("seedFeed did not return on cancellation")
	}
}
