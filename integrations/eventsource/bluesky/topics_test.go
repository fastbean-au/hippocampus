package bluesky

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

func linkMsg(uri string, external string, text string) bridge.Message {
	msg := bridge.Message{
		Subject: CollectionPost,
		Data:    []byte(text),
		Headers: map[string]string{
			hdrCollection: CollectionPost,
			hdrOperation:  operationCreate,
			hdrURI:        uri,
		},
	}

	if external != "" {
		msg.Headers[hdrExternalURI] = external
	}

	return msg
}

// TestExtractTermsPrefersTheURLSlug is the whole idea: a news URL's path is a keyword list a human
// wrote, already tokenised on hyphens, and it beats anything extracted from the headline.
func TestExtractTermsPrefersTheURLSlug(t *testing.T) {
	msg := linkMsg("at://x/app.bsky.feed.post/a",
		"https://www.motherjones.com/politics/2026/08/samuel-alito-ethics-conflicts-interest-fossil/",
		"Supreme Court justice gained millions")

	got := map[string]bool{}
	for _, term := range extractTerms(msg) {
		got[term] = true
	}

	for _, want := range []string{"samuel", "alito", "ethics", "conflicts", "interest", "fossil"} {
		if !got[want] {
			t.Errorf("term %q missing from %v", want, got)
		}
	}

	// The section name and the date are furniture, not topic.
	for _, unwanted := range []string{"politics", "2026", "08", "www", "motherjones"} {
		if got[unwanted] {
			t.Errorf("term %q should have been dropped", unwanted)
		}
	}

	// The headline is not consulted when there is a slug.
	if got["supreme"] {
		t.Error("the headline was tokenised despite a link card being present")
	}
}

// TestExtractTermsFallsBackToTheText: 9% of posts carry no link card, and relating none of them is
// worse than relating them noisily.
func TestExtractTermsFallsBackToTheText(t *testing.T) {
	msg := linkMsg("at://x/app.bsky.feed.post/a", "",
		"Klobuchar wins the Minnesota governor primary")

	got := map[string]bool{}
	for _, term := range extractTerms(msg) {
		got[term] = true
	}

	if !got["klobuchar"] || !got["minnesota"] {
		t.Errorf("expected the headline's terms, got %v", got)
	}

	// Tokens under minTermLength carry no topic. "wins" is exactly four and is legitimately kept -
	// the cut-off is length, not a judgement about the word.
	if got["the"] {
		t.Errorf("a token below the length floor survived: %v", got)
	}
}

func TestTokeniseRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		gone []string
	}{
		{name: "lowercases", in: "Alito ETHICS", want: []string{"alito", "ethics"}},
		{name: "drops purely numeric tokens", in: "budget-2026-08-11", want: []string{"budget"}, gone: []string{"2026", "08"}},
		{name: "drops stop terms", in: "world/politics/gaza", want: []string{"gaza"}, gone: []string{"world", "politics"}},
		{name: "deduplicates", in: "alito-alito-alito", want: []string{"alito"}},
		{name: "keeps non-ascii whole", in: "wahlkampf-über-berlin", want: []string{"wahlkampf", "berlin"}},
		{name: "empty input", in: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := map[string]bool{}
			for _, v := range tokenise(c.in) {
				got[v] = true
			}

			for _, w := range c.want {
				if !got[w] {
					t.Errorf("term %q missing from %v", w, got)
				}
			}

			for _, g := range c.gone {
				if got[g] {
					t.Errorf("term %q should have been dropped, got %v", g, got)
				}
			}
		})
	}
}

func TestTokeniseIsBounded(t *testing.T) {
	long := ""
	for i := range 200 {
		long += "term" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "-"
	}

	if got := len(tokenise(long)); got > maxTermsPerPost {
		t.Errorf("produced %d terms, want at most %d", got, maxTermsPerPost)
	}
}

func TestTopicIndexRelates(t *testing.T) {
	x := newTopicIndex(100)

	x.Add("a", []string{"alito", "ethics", "fossil"})
	x.Add("b", []string{"alito", "ethics", "court"})
	x.Add("c", []string{"klobuchar", "minnesota"})

	// Two shared terms with a and none with c.
	got := x.Related([]string{"alito", "ethics", "senate"}, 2, 8, 100)

	found := map[string]bool{}
	for _, id := range got {
		found[id] = true
	}

	if !found["a"] || !found["b"] {
		t.Errorf("related = %v, want both a and b", got)
	}

	if found["c"] {
		t.Errorf("related = %v, should not include the unrelated c", got)
	}
}

// TestTopicIndexRequiresTheMinimumOverlap: one shared term relates half a news corpus, which is why
// the floor is two.
func TestTopicIndexRequiresTheMinimumOverlap(t *testing.T) {
	x := newTopicIndex(100)
	x.Add("a", []string{"alito", "ethics"})

	if got := x.Related([]string{"alito", "senate"}, 2, 8, 100); len(got) != 0 {
		t.Errorf("related = %v on one shared term, want none", got)
	}

	if got := x.Related([]string{"alito", "senate"}, 1, 8, 100); len(got) != 1 {
		t.Errorf("related = %v at minShared 1, want a", got)
	}
}

// TestTopicIndexIgnoresCommonTerms is the cheap stand-in for IDF: without it a section name that
// survived the stop list would relate every post to every other.
func TestTopicIndexIgnoresCommonTerms(t *testing.T) {
	x := newTopicIndex(100)

	for i := range 20 {
		x.Add(string(rune('a'+i)), []string{"everywhere", "unique" + string(rune('a'+i))})
	}

	// "everywhere" is in all 20; with a cap of 5 it must contribute nothing.
	if got := x.Related([]string{"everywhere", "uniquea"}, 2, 8, 5); len(got) != 0 {
		t.Errorf("related = %v, want none once the common term is ignored", got)
	}

	// With a cap above its frequency it counts again.
	if got := x.Related([]string{"everywhere", "uniquea"}, 2, 8, 100); len(got) != 1 {
		t.Errorf("related = %v, want a when the cap allows the common term", got)
	}
}

func TestTopicIndexPrefersTheStrongestOverlap(t *testing.T) {
	x := newTopicIndex(100)

	x.Add("weak", []string{"alito", "ethics"})
	x.Add("strong", []string{"alito", "ethics", "fossil", "court"})

	got := x.Related([]string{"alito", "ethics", "fossil", "court"}, 2, 1, 100)

	if len(got) != 1 || got[0] != "strong" {
		t.Errorf("related = %v, want the strongest overlap first", got)
	}
}

func TestTopicIndexEvictsAndBounds(t *testing.T) {
	x := newTopicIndex(2)

	x.Add("a", []string{"alito", "ethics"})
	x.Add("b", []string{"alito", "ethics"})
	x.Add("c", []string{"alito", "ethics"})

	if x.Len() != 2 {
		t.Errorf("index holds %d, want the bound of 2", x.Len())
	}

	got := x.Related([]string{"alito", "ethics"}, 2, 8, 100)

	for _, id := range got {
		if id == "a" {
			t.Error("the evicted memory is still related")
		}
	}

	// The posting lists must shrink with the eviction, not leak.
	x.mu.Lock()
	postings := len(x.terms["alito"])
	x.mu.Unlock()

	if postings != 2 {
		t.Errorf("posting list holds %d ids, want 2", postings)
	}
}

func TestTopicIndexIgnoresEmptyAndDuplicateAdds(t *testing.T) {
	x := newTopicIndex(10)

	x.Add("", []string{"alito"})
	x.Add("a", nil)
	x.Add("a", []string{"alito", "ethics"})
	x.Add("a", []string{"different", "terms"})

	if x.Len() != 1 {
		t.Errorf("index holds %d, want 1", x.Len())
	}

	// The second Add for the same id is ignored, so the original terms stand.
	if got := x.Related([]string{"different", "terms"}, 2, 8, 100); len(got) != 0 {
		t.Errorf("related = %v, want the duplicate Add to have been ignored", got)
	}
}

func TestTopicIndexZeroSizeStillHoldsOne(t *testing.T) {
	x := newTopicIndex(0)
	x.Add("a", []string{"alito", "ethics"})

	if x.Len() != 1 {
		t.Errorf("index holds %d, want 1", x.Len())
	}
}

// TestLinksForNeverRelatesAPostToItself pins the ordering in linksFor: lookup first, index after.
func TestLinksForNeverRelatesAPostToItself(t *testing.T) {
	b := New(Config{TopicLinks: true}, nil)

	msg := linkMsg("at://x/app.bsky.feed.post/a",
		"https://example.com/politics/2026/08/samuel-alito-ethics-fossil/", "")

	if links := b.linksFor(msg, "at://x/app.bsky.feed.post/a"); len(links) != 0 {
		t.Errorf("links = %v, want none for the first post seen", links)
	}

	// A second, related post gets the first as a link.
	msg2 := linkMsg("at://x/app.bsky.feed.post/b",
		"https://other.example/us/samuel-alito-ethics-supreme/", "")

	links := b.linksFor(msg2, "at://x/app.bsky.feed.post/b")

	if len(links) != 1 || links[0].GetId() != "at://x/app.bsky.feed.post/a" {
		t.Fatalf("links = %v, want the first post", links)
	}

	if links[0].GetSignificance() != b.cfg.TopicLinkSignificance {
		t.Errorf("link significance = %d, want %d", links[0].GetSignificance(), b.cfg.TopicLinkSignificance)
	}
}

func TestLinksForIsOffWhenTopicLinksAreDisabled(t *testing.T) {
	b := New(Config{}, nil)

	msg := linkMsg("at://x/app.bsky.feed.post/a", "https://example.com/a-b-c-topic-terms/", "")

	if links := b.linksFor(msg, "at://x/app.bsky.feed.post/a"); links != nil {
		t.Errorf("links = %v, want none when topic linking is off", links)
	}
}

func TestLinksForIgnoresAnEmptyIdOrTermlessPost(t *testing.T) {
	b := New(Config{TopicLinks: true}, nil)

	if links := b.linksFor(linkMsg("", "", "text"), ""); links != nil {
		t.Errorf("links = %v, want none for an empty id", links)
	}

	if links := b.linksFor(linkMsg("at://x/a", "", "hi"), "at://x/a"); links != nil {
		t.Errorf("links = %v, want none when nothing tokenises", links)
	}
}

// TestTopicFrequencyCapHasAFloor: a freshly-started bridge indexes two posts, in which every term is
// in 100% of the corpus - without a floor it would relate nothing, ever, until the index filled.
func TestTopicFrequencyCapHasAFloor(t *testing.T) {
	b := New(Config{TopicLinks: true, TopicMaxFrequencyPercent: 2}, nil)

	if got := b.topicFrequencyCap(); got != minFrequencyCap {
		t.Errorf("cap = %d on an empty index, want the floor %d", got, minFrequencyCap)
	}

	for i := range 1000 {
		b.topics.Add(string(rune(i)), []string{"term"})
	}

	if got := b.topicFrequencyCap(); got != 20 {
		t.Errorf("cap = %d at 1000 indexed with 2%%, want 20", got)
	}
}

// TestFirehosePostsAreLinkedAfterTheWrite is the ordering that matters: links are issued separately
// so a target the store has already forgotten cannot fail the post itself.
func TestFirehosePostsAreLinkedAfterTheWrite(t *testing.T) {
	client := &fakeClient{}

	b := testBridge(t, Config{Events: EventsNone, TopicLinks: true}, client)

	// Two posts about the same story, via slugs that share four terms.
	frames := [][]byte{
		postFrameWithLink(10, "a", "first report", "https://one.example/us/samuel-alito-ethics-fossil-court/"),
		postFrameWithLink(20, "b", "second report", "https://two.example/news/samuel-alito-ethics-fossil-supreme/"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := b.consume(ctx, &fakeStream{frames: frames}, 0); err != nil {
		t.Fatalf("consume: %s", err)
	}

	memories, _, _, _ := client.snapshot()

	if len(memories) != 2 {
		t.Fatalf("stored %d memories, want 2", len(memories))
	}

	// The links did NOT ride on the create.
	for _, m := range memories {
		if len(m.GetLinks()) != 0 {
			t.Errorf("memory %q carried links on its create; they must be a separate call", m.GetId())
		}
	}

	linked := client.linkedMemories()

	if len(linked) != 1 {
		t.Fatalf("issued %d link calls, want 1 (only the second post has a neighbour)", len(linked))
	}

	if linked[0].id != "at://did:plc:abc/app.bsky.feed.post/b" {
		t.Errorf("linked %q, want the second post", linked[0].id)
	}

	if len(linked[0].links) != 1 || linked[0].links[0].GetId() != "at://did:plc:abc/app.bsky.feed.post/a" {
		t.Errorf("links = %v, want the first post", linked[0].links)
	}
}

// TestLinkFailuresNeverFailThePost: linking is enrichment, and a neighbour consolidated a moment ago
// must not cost us the post.
func TestLinkFailuresNeverFailThePost(t *testing.T) {
	client := &fakeClient{linkErr: errUnavailable}

	b := testBridge(t, Config{Events: EventsNone, TopicLinks: true}, client)

	frames := [][]byte{
		postFrameWithLink(10, "a", "first", "https://one.example/us/samuel-alito-ethics-fossil-court/"),
		postFrameWithLink(20, "b", "second", "https://two.example/news/samuel-alito-ethics-fossil-supreme/"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	last, err := b.consume(ctx, &fakeStream{frames: frames}, 0)
	if err != nil {
		t.Fatalf("a link failure must not fail the frame: %s", err)
	}

	if last != 20 {
		t.Errorf("cursor = %d, want the frames acknowledged despite the link failure", last)
	}

	if memories, _, _, _ := client.snapshot(); len(memories) != 2 {
		t.Errorf("stored %d memories, want both", len(memories))
	}
}

// TestBackfillAttachesLinksToTheImport: an import applies links in a second pass once every row
// exists, so the seed relates itself without several hundred extra calls.
func TestBackfillAttachesLinksToTheImport(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		postWithLink("a", "first report", "https://one.example/us/samuel-alito-ethics-fossil-court/"),
		postWithLink("b", "second report", "https://two.example/news/samuel-alito-ethics-fossil-supreme/"),
	}})

	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:      EventsNone,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
		TopicLinks:  true,
	}, client)

	if err := b.backfillFeed(context.Background()); err != nil {
		t.Fatalf("backfillFeed: %s", err)
	}

	batches := client.importedBatches()

	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("imported %v, want one batch of two", batches)
	}

	// The second post links to the first, and it rides ON the import rather than costing a call.
	if len(batches[0][1].GetLinks()) != 1 {
		t.Errorf("second memory carries %d links, want 1", len(batches[0][1].GetLinks()))
	}

	if len(client.linkedMemories()) != 0 {
		t.Error("a backfill should issue no separate link calls")
	}
}

// TestFeedPollLinksEachStoredPost covers the poll path's linking, which unlike the backfill issues a
// separate call per related post.
func TestFeedPollLinksEachStoredPost(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		postWithLink("a", "first report", "https://one.example/us/samuel-alito-ethics-fossil-court/"),
		postWithLink("b", "second report", "https://two.example/news/samuel-alito-ethics-fossil-supreme/"),
	}})

	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:      EventsNone,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
		TopicLinks:  true,
	}, client)

	if err := b.pollFeed(context.Background()); err != nil {
		t.Fatalf("pollFeed: %s", err)
	}

	linked := client.linkedMemories()

	if len(linked) != 1 {
		t.Fatalf("issued %d link calls, want 1 (only the second post has a neighbour)", len(linked))
	}

	if linked[0].id != "at://did:plc:news/app.bsky.feed.post/b" {
		t.Errorf("linked %q, want the second post", linked[0].id)
	}
}

// TestFeedPollLinkFailuresAreAbsorbed: linking is enrichment, so a failure must not fail the poll.
func TestFeedPollLinkFailuresAreAbsorbed(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		postWithLink("a", "first", "https://one.example/us/samuel-alito-ethics-fossil-court/"),
		postWithLink("b", "second", "https://two.example/news/samuel-alito-ethics-fossil-supreme/"),
	}})

	client := &fakeClient{linkErr: errUnavailable}

	b := testBridge(t, Config{
		Events:      EventsNone,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
		TopicLinks:  true,
	}, client)

	if err := b.pollFeed(context.Background()); err != nil {
		t.Errorf("a link failure must not fail the poll: %s", err)
	}

	if len(client.storedMemories()) != 2 {
		t.Errorf("stored %d memories, want both despite the link failure", len(client.storedMemories()))
	}
}

// TestLinkingIsSkippedEntirelyWhenDisabled: the poll and backfill paths must not even build links.
func TestLinkingIsSkippedEntirelyWhenDisabled(t *testing.T) {
	av := newFakeAppView(t, [][]feedItem{{
		postWithLink("a", "first", "https://one.example/us/samuel-alito-ethics-fossil-court/"),
		postWithLink("b", "second", "https://two.example/news/samuel-alito-ethics-fossil-supreme/"),
	}})

	client := &fakeClient{}

	b := testBridge(t, Config{
		Events:      EventsNone,
		Feed:        "at://did:plc:x/app.bsky.feed.generator/news",
		FeedAppView: av.server.URL,
	}, client)

	if err := b.backfillFeed(context.Background()); err != nil {
		t.Fatalf("backfillFeed: %s", err)
	}

	if err := b.pollFeed(context.Background()); err != nil {
		t.Fatalf("pollFeed: %s", err)
	}

	if len(client.linkedMemories()) != 0 {
		t.Error("link calls were issued despite topic linking being off")
	}

	for _, batch := range client.importedBatches() {
		for _, m := range batch {
			if len(m.GetLinks()) != 0 {
				t.Errorf("memory %q carried links despite topic linking being off", m.GetId())
			}
		}
	}
}

// TestTopicIndexEvictionRemovesTheTermEntirely covers the branch where a term's last posting goes.
func TestTopicIndexEvictionRemovesTheTermEntirely(t *testing.T) {
	x := newTopicIndex(1)

	x.Add("a", []string{"soleterm", "other"})
	x.Add("b", []string{"different", "terms"})

	x.mu.Lock()
	_, stillThere := x.terms["soleterm"]
	x.mu.Unlock()

	if stillThere {
		t.Error("the evicted memory's sole term should have been removed from the index")
	}
}
