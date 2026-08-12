package bluesky

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// The curated-feed source: posts come from an atproto FEED GENERATOR instead of the firehose.
//
// A feed generator is a ranked list someone maintains, read over HTTP (app.bsky.feed.getFeed), not a
// websocket of repo commits. So this is a poller, and it does not replace the Jetstream consumer -
// it replaces only the question of WHICH posts to store. Engagement still arrives on the firehose,
// and the recall path is untouched, because a like names its target by at:// URI regardless of how
// that post reached the store. That is the same statelessness that makes the firehose bridge work,
// paying off a second time: two sources, one reinforcement mechanism, no correlation to maintain.
//
// The trade against the firehose is volume for legibility. A curated feed delivers tens of posts an
// hour rather than tens a second, so the store is small and the decay clock wants to run in hours
// rather than minutes - but every memory in it is a headline a person can read, which is what makes
// it the right source for something hosted and looked at once.

// DefaultAppView is Bluesky's public, unauthenticated AppView. getFeed on a public feed needs no
// token there, which is what lets this run with no Bluesky account at all.
const DefaultAppView = "https://public.api.bsky.app"

// feedPageLimit is the server's maximum page size for getFeed.
const feedPageLimit = 100

// feedMaxPages bounds a backfill, so a feed that paginates further than expected cannot spin.
const feedMaxPages = 50

// feedHTTPTimeout bounds one getFeed call.
const feedHTTPTimeout = 30 * time.Second

// feedBodyLimit caps one response.
const feedBodyLimit = 8 << 20

// feedSource reads posts from a feed generator via an AppView.
type feedSource struct {
	uri        string
	appView    string
	httpClient *http.Client
	transform  bridge.Transformer
	seedRecall bool
}

// feedResponse is the getFeed reply.
type feedResponse struct {
	Feed   []feedItem `json:"feed"`
	Cursor string     `json:"cursor"`
}

type feedItem struct {
	Post *postView `json:"post"`
}

// postView is the hydrated post getFeed returns. Unlike a firehose commit it carries the ENGAGEMENT
// COUNTS, which is what makes seeding a backfill possible at all.
type postView struct {
	URI    string  `json:"uri"`
	CID    string  `json:"cid"`
	Author *author `json:"author"`
	Record *record `json:"record"`

	LikeCount   int `json:"likeCount"`
	RepostCount int `json:"repostCount"`
	ReplyCount  int `json:"replyCount"`
}

type author struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
}

// newFeedSource builds a poller for one feed.
func newFeedSource(cfg Config, transform bridge.Transformer) *feedSource {
	appView := cfg.FeedAppView
	if appView == "" {
		appView = DefaultAppView
	}

	return &feedSource{
		uri:        cfg.Feed,
		appView:    appView,
		httpClient: &http.Client{Timeout: feedHTTPTimeout},
		transform:  transform,
		seedRecall: cfg.FeedSeedRecalls,
	}
}

// fetch reads one page.
func (f *feedSource) fetch(ctx context.Context, cursor string) (*feedResponse, error) {
	endpoint, err := url.Parse(f.appView)
	if err != nil {
		return nil, fmt.Errorf("parsing the AppView URL %q: %w", f.appView, err)
	}

	endpoint.Path = "/xrpc/app.bsky.feed.getFeed"

	q := url.Values{}
	q.Set("feed", f.uri)
	q.Set("limit", strconv.Itoa(feedPageLimit))

	if cursor != "" {
		q.Set("cursor", cursor)
	}

	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building getFeed request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling getFeed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, feedBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("reading the getFeed response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getFeed returned %d: %s", resp.StatusCode, truncate(string(body), 256))
	}

	var out feedResponse

	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding the getFeed response: %w", err)
	}

	return &out, nil
}

// Backfill reads the whole feed and returns memories carrying SEEDED recall counts.
//
// A backfilled post's likes are all in the past, so the firehose will never report them: without
// seeding, a store that starts with several hundred headlines shows every one of them as equally
// untouched, and the model looks like it is doing nothing until hours of live engagement accumulate.
// Seeding is what makes the first thing a visitor sees the actual shape of the thing.
func (f *feedSource) Backfill(ctx context.Context) ([]*contract.Memory, error) {
	var (
		out    []*contract.Memory
		cursor string
	)

	for page := range feedMaxPages {
		resp, err := f.fetch(ctx, cursor)
		if err != nil {
			return out, err
		}

		mems, err := f.memories(resp.Feed, f.seedRecall)
		if err != nil {
			return out, err
		}

		out = append(out, mems...)

		cursor = resp.Cursor

		if cursor == "" || len(resp.Feed) == 0 {
			log.WithFields(log.Fields{"pages": page + 1, "memories": len(out)}).
				Debug("feed backfill reached the end of the feed")

			break
		}
	}

	return out, nil
}

// Poll reads the newest page, for the memories a live run should add.
//
// It returns them WITHOUT seeded recall counts and the caller stores them with StoreMemories, not
// ImportMemories: a feed hands back the same posts on every read, and importing would replace each
// one - rolling its live reinforcement back to whatever the feed last reported. Letting
// AlreadyExists mean "already have it" is what makes polling need no bookmark and no dedupe state.
func (f *feedSource) Poll(ctx context.Context) ([]*contract.Memory, error) {
	resp, err := f.fetch(ctx, "")
	if err != nil {
		return nil, err
	}

	return f.memories(resp.Feed, false)
}

// memories turns feed items into memories, through the SAME Transformer the firehose path uses - so
// the language filter, the minimum length, the id cap, the metadata handling and the timestamp clamp
// all behave identically whichever source a post arrived from.
func (f *feedSource) memories(items []feedItem, seed bool) ([]*contract.Memory, error) {
	out := make([]*contract.Memory, 0, len(items))

	for _, item := range items {
		p := item.Post
		if p == nil || p.Record == nil || p.Author == nil {
			continue
		}

		mems, err := f.transform.Transform(f.message(p))
		if err != nil {
			return nil, err
		}

		for _, m := range mems {
			if m == nil {
				continue
			}

			if seed {
				m.RecallCount = seededRecallCount(p)
			}

			out = append(out, m)
		}
	}

	return out, nil
}

// message projects a hydrated feed post onto the same bridge.Message shape toMessage builds from a
// firehose commit, so both sources reach the Transformer identically.
func (f *feedSource) message(p *postView) bridge.Message {
	msg := bridge.Message{
		Subject: CollectionPost,
		Data:    []byte(p.Record.Text),
		Headers: map[string]string{
			hdrDID:        p.Author.DID,
			hdrURI:        p.URI,
			hdrCollection: CollectionPost,
			hdrOperation:  operationCreate,
			hdrRkey:       rkeyOf(p.URI),
			hdrHandle:     p.Author.Handle,
		},
	}

	if p.CID != "" {
		msg.Headers[hdrCID] = p.CID
	}

	if p.Record.CreatedAt != "" {
		msg.Headers[hdrCreatedAt] = p.Record.CreatedAt

		if t, err := time.Parse(time.RFC3339, p.Record.CreatedAt); err == nil {
			msg.Timestamp = t
		}
	}

	if len(p.Record.Langs) > 0 {
		msg.Headers[hdrLang] = p.Record.Langs[0]
		msg.Headers[hdrLangs] = joinCommas(p.Record.Langs)
	}

	if p.Record.Embed != nil && p.Record.Embed.Type != "" {
		msg.Headers[hdrEmbed] = p.Record.Embed.Type
	}

	if p.Record.Reply != nil && p.Record.Reply.Root != nil {
		msg.Headers[hdrReplyRoot] = p.Record.Reply.Root.URI
	}

	return msg
}

// seededRecallCount damps a post's observed engagement into a recall count.
//
// The damping is not decoration. Effective significance rises LINEARLY with recall count
// (recallSignificanceWeight x count), so passing a like count straight through would give a post
// with five thousand likes an effective significance in the tens of thousands - unforgettable, for
// as long as the store exists, which is not a demonstration of a decay model. log1p compresses four
// orders of magnitude of engagement into single digits, which still ranks them correctly against
// each other and still lets the biggest of them eventually fall.
//
// Reposts count alongside likes because both are the same gesture for this purpose: someone came
// back to it. Replies are deliberately excluded - a reply is its own post, and on a news feed it is
// as often disagreement as endorsement.
func seededRecallCount(p *postView) int32 {
	engagement := p.LikeCount + p.RepostCount
	if engagement <= 0 {
		return 0
	}

	return int32(math.Round(math.Log1p(float64(engagement))))
}

// joinCommas is strings.Join without the import, matching splitCommas in transform.go.
func joinCommas(in []string) string {
	out := ""

	for i, v := range in {
		if i > 0 {
			out += ","
		}

		out += v
	}

	return out
}
