// Package bluesky is the Bluesky/atproto adapter for the Hippocampus event-sourcing bridges. It
// consumes the public Jetstream websocket - Bluesky's JSON projection of the atproto firehose - and
// maps it onto the store's two dynamics at once: a post becomes a memory, and the engagement that
// follows it (a like, a repost, a reply) becomes a RECALL of that memory.
//
// Recall is what makes this adapter different from the other four. A memory's id is the post's at://
// URI, so a like's subject.uri IS the id to reinforce: the bridge holds no map, keeps no state, and
// does no lookup. A like for a post it never ingested, or one the store has already forgotten, costs
// one UPDATE that matches no rows. Every post therefore arrives with the same significance, and what
// survives is only what people came back to.
//
// # Delivery semantics
//
// Jetstream has no per-message ack; the resume point is a cursor. Memory writes are AT-LEAST-ONCE,
// cursor-gated: the cursor advances only after a frame has been fully handled, a store failure
// retries the frame in place after a backoff, and a frame still failing after MaxRetries drops the
// connection so the socket reopens at the last good cursor and replays from the failure. Replay is
// safe because the id is the URI - a duplicate write returns AlreadyExists, which the bridge counts
// as a success. Jetstream's own delivery is documented at-least-once, so duplicates arrive on the
// happy path too and one rule covers both.
//
// # Batched recalls are best-effort
//
// Likes arrive at hundreds per second and RecallMemories is bulk, so ids are buffered and flushed
// together. A frame counts as handled once its id is buffered, so a crash inside the window loses at
// most one window of reinforcement. That is deliberate: a lost like is a memory that decays slightly
// sooner, not a memory that is wrong, and paying an RPC per like to avoid it would cost two orders
// of magnitude more traffic than the reinforcement is worth. RecallBatchSize 0 restores synchronous,
// at-least-once recalls.
//
// # Gaps
//
// Jetstream's cursor lookback is bounded (36 hours at the time of writing) and /subscribe SILENTLY
// CLAMPS a cursor older than that to the oldest event it still holds. A bridge down for longer
// resumes at the window's edge and the gap is skipped without an error. This is a stream of the
// present, not a ledger.
//
// # Jetstream is a convenience service
//
// Jetstream is operated by Bluesky and is not part of the protocol: it is single-node, unauthenticated,
// and could be rate-limited or withdrawn. The canonical firehose
// (com.atproto.sync.subscribeRepos) is DAG-CBOR-encoded Merkle Search Tree blocks in CAR files, and
// consuming it would mean an MST implementation, a CAR reader and a CBOR codec - three substantial
// dependencies in a module whose whole premise is that broker client trees stay small. URL is
// therefore configurable (Jetstream is self-hostable), and a subscribeRepos consumer would be a
// DIFFERENT adapter rather than a rewrite of this one: bridge.Message is where that substitution
// happens.
package bluesky

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// DefaultURL is Bluesky's public Jetstream endpoint.
const DefaultURL = "wss://jetstream2.us-east.bsky.network/subscribe"

// MaxCollections is Jetstream's server-side limit on wantedCollections. Exported because the
// command validates the flag against it before dialling: exceeding it is otherwise a server-side
// rejection, which is a confusing way to find out about a typo in a long list.
const MaxCollections = 100

// Event name/description caps, mirroring the service's own (types/event.go).
const (
	maxEventNameLength        = 256
	maxEventDescriptionLength = 1024
)

// Config describes the Jetstream subscription and how its frames map onto the store.
type Config struct {
	// URL is the Jetstream /subscribe endpoint. Empty uses DefaultURL.
	URL string

	// Collections are the record NSIDs to subscribe to (Jetstream's wantedCollections; wildcards
	// allowed, at most MaxCollections). Empty subscribes to everything, which on the public firehose
	// is a great deal more than this bridge knows what to do with.
	Collections []string

	// DIDs, when set, restricts the subscription to those repositories (wantedDids). Empty is the
	// whole network. This is the flag that turns a firehose bridge into a follow-a-few-accounts
	// bridge, which is the sane thing to point at a personal instance.
	DIDs []string

	// Cursor is the resume point: a Jetstream sequence number or a unix-microsecond timestamp, which
	// the server tells apart by magnitude. 0 starts at the live tip.
	Cursor int64

	// Feed, when set, is the at:// URI of a feed generator to take POSTS from instead of the
	// firehose. Engagement still comes from Jetstream: the feed decides what is worth storing, the
	// firehose reports what people did with it. A curated feed trades volume for legibility - tens
	// of posts an hour, every one of them readable - which is what a hosted demo wants.
	Feed string

	// FeedAppView is the AppView serving getFeed. Empty uses DefaultAppView, which is public and
	// unauthenticated.
	FeedAppView string

	// FeedPollInterval is how often the feed is re-read for new posts.
	FeedPollInterval time.Duration

	// FeedBackfill reads the whole feed once at startup, so the store is populated immediately
	// rather than after hours of trickle.
	FeedBackfill bool

	// FeedSeedRecalls carries a backfilled post's observed engagement across as a damped recall
	// count. Without it every seeded memory looks equally untouched, because its likes happened
	// before the bridge was watching and the firehose will never report them.
	FeedSeedRecalls bool

	// CaptureReplies stores a firehose post that replies to a thread this bridge already holds,
	// instead of treating it as reinforcement alone. It is what turns --events thread from a
	// structure into a conversation: the feed's post opens the event, and the public's replies to it
	// become memories in that event.
	//
	// Feed mode only, and only on an unfiltered subscription. In firehose mode every post is stored
	// already; under --dids a reply written by anyone else is never delivered to filter (wantedDids
	// selects on the repo the record was written in, and a reply lives in the replier's).
	CaptureReplies bool

	// CaptureIndexSize bounds the set of stored feed posts a reply is matched against. It holds only
	// what the feed produced, never the replies captured through it, so eviction follows the feed's
	// own recency rather than whichever thread is busiest.
	CaptureIndexSize int

	// FeedAuthors stores a firehose post whose author the feed has surfaced, so an account the feed
	// is made of is followed rather than only the posts that feed picked. The DIDs are derived from
	// the feed itself on every read - a feed already hands back post.author.did - so this needs no
	// account list to maintain and follows the feed's editorial choices as they change.
	//
	// Note what it cannot do: it does NOT bring in replies BY OTHERS to those accounts (that is
	// CaptureReplies), because those are records in the repliers' repositories.
	FeedAuthors bool

	// FeedAuthorsMax bounds the derived author set.
	FeedAuthorsMax int

	// TopicLinks relates posts that share topic terms - see topics.go. It is what makes
	// consolidation.linkRecallPropagation do anything: with links, a like on one outlet's coverage
	// pulls the others back from the threshold too.
	TopicLinks bool

	// TopicIndexSize bounds how many memories the term index remembers. It is the one genuinely
	// stateful thing in the bridge, and losing it only stops links being made.
	TopicIndexSize int

	// TopicMinShared is how many terms two posts must share to be related. Two is the useful floor:
	// one shared term relates half the corpus, three relates almost nothing.
	TopicMinShared int

	// TopicMaxLinks bounds the links one post is given, well under the service's own cap of 128.
	TopicMaxLinks int

	// TopicMaxFrequencyPercent is the document-frequency cutoff: a term carried by more than this
	// percentage of the indexed memories is a section name rather than a topic, and is ignored. It is
	// the cheap stand-in for IDF.
	TopicMaxFrequencyPercent int

	// TopicLinkSignificance is the significance each topic link carries.
	TopicLinkSignificance int32

	// Events selects thread modelling: EventsNone stores every post as a standalone memory,
	// EventsThread opens an event per thread root so a reply is a memory of the root's event.
	Events string

	// Group is stamped on the events this bridge opens, so a scoped reader sees a thread and its
	// posts together rather than one without the other. It is deliberately the CONFIGURED group
	// rather than one derived from a member post: a thread spans many authors, so inheriting the
	// group of whichever post happened to open it would be arbitrary.
	Group string

	// Recall enables reinforcement from the engagement stream. RecallBatchSize and RecallBatchWindow
	// bound the buffer; a zero size makes each recall a synchronous RPC.
	Recall            bool
	RecallBatchSize   int
	RecallBatchWindow time.Duration

	// HonourDeletes maps an upstream record deletion onto a memory deletion. Deleting a post is the
	// only withdrawal a person has; a bridge that keeps the copy has quietly turned a public post
	// into a permanent private archive. Decay is about significance, deletion is about consent.
	HonourDeletes bool

	// RootCacheSize bounds the known-thread-roots cache, which is a pure optimisation - see
	// idCache.
	RootCacheSize int

	// ErrorBackoff is the pause before retrying a frame whose store failed; MaxRetries is how many
	// times before the connection is dropped and replayed from the last good cursor.
	ErrorBackoff time.Duration
	MaxRetries   int

	// ReconnectBackoff and ReconnectMaxBackoff bound the exponential reconnect backoff.
	ReconnectBackoff    time.Duration
	ReconnectMaxBackoff time.Duration

	// ReadTimeout bounds the wait for the next frame. The firehose is never idle, so silence means a
	// black-holed connection rather than a quiet topic, and reconnecting is the right answer.
	ReadTimeout time.Duration
}

// stream is the slice of a Jetstream connection the Bridge uses, so consume can be exercised with a
// canned channel of frames and no network at all.
//
// Next takes a context, unlike the underlying websocket read, which takes only a deadline. Pushing
// cancellation into the seam is what keeps consume a plain, synchronous, fully cancellable loop: the
// real implementation translates the context and the read timeout into a deadline plus a watchdog,
// and a fake simply selects on ctx.Done alongside its channel.
type stream interface {
	Next(ctx context.Context) ([]byte, error)
	Close() error
}

// dialFunc opens a stream at the given resume cursor. Injected on the Bridge so the reconnect loop
// can be driven end to end - including asserting the cursor the second dial was given - without a
// server.
type dialFunc func(ctx context.Context, cfg Config, cursor int64) (stream, error)

// Bridge consumes Jetstream into a bridge.Store, optionally taking its posts from a curated feed
// instead of the firehose.
type Bridge struct {
	cfg    Config
	store  *bridge.Store
	roots  *idCache
	recall *recallBuffer
	feed   *feedSource
	topics *topicIndex

	// stored and authors are the two firehose-capture indexes, nil when their feature is off - see
	// captureFromFirehose. Both are written by the feed poller and read by the Jetstream consumer,
	// which is the reason idCache locks.
	stored  *idCache
	authors *idCache

	// dial is overridable in tests.
	dial dialFunc
}

// feedMode reports whether posts come from a feed rather than the firehose. In feed mode the
// firehose is still consumed - for engagement, and for the deletions that honour a withdrawal - but
// a post arriving on it is not stored, because the feed is what decides what is worth storing.
func (b *Bridge) feedMode() bool {
	return b.feed != nil
}

// New builds a Bluesky Bridge from its config and the shared store, like every other adapter.
//
// The Transformer is not passed in: it reaches the bridge inside the Store, and the thread-event
// path never needs to run it itself - HandleEvent transforms the message and declines to open an
// event at all when the transformer filters the post.
func New(cfg Config, store *bridge.Store) *Bridge {
	if cfg.URL == "" {
		cfg.URL = DefaultURL
	}

	if cfg.Events == "" {
		cfg.Events = EventsNone
	}

	if cfg.ErrorBackoff <= 0 {
		cfg.ErrorBackoff = time.Second
	}

	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}

	if cfg.ReconnectMaxBackoff < cfg.ReconnectBackoff {
		cfg.ReconnectMaxBackoff = 30 * time.Second
	}

	if cfg.FeedPollInterval <= 0 {
		cfg.FeedPollInterval = time.Minute
	}

	if cfg.TopicIndexSize <= 0 {
		cfg.TopicIndexSize = 5000
	}

	if cfg.TopicMinShared <= 0 {
		cfg.TopicMinShared = 2
	}

	if cfg.TopicMaxLinks <= 0 {
		cfg.TopicMaxLinks = 8
	}

	if cfg.TopicMaxFrequencyPercent <= 0 {
		cfg.TopicMaxFrequencyPercent = 2
	}

	if cfg.TopicLinkSignificance <= 0 {
		cfg.TopicLinkSignificance = 50
	}

	if cfg.CaptureIndexSize <= 0 {
		cfg.CaptureIndexSize = 5000
	}

	if cfg.FeedAuthorsMax <= 0 {
		cfg.FeedAuthorsMax = 500
	}

	b := &Bridge{
		cfg:   cfg,
		store: store,
		roots: newIDCache(cfg.RootCacheSize),
		dial:  defaultDial,
	}

	if cfg.Recall {
		b.recall = newRecallBuffer(store, cfg.RecallBatchSize, cfg.RecallBatchWindow)
	}

	// The feed shares the Store's own Transformer, so a post read over HTTP is filtered, bounded and
	// clamped by the identical instance that handles one off the firehose.
	if cfg.Feed != "" {
		b.feed = newFeedSource(cfg, store.Transformer())
	}

	if cfg.TopicLinks {
		b.topics = newTopicIndex(cfg.TopicIndexSize)
	}

	// Both capture indexes are built whenever their flag is set, feed mode or not: the flags are
	// meaningless outside it (see captureFromFirehose), but refusing to build them here would trade a
	// warning at startup for a nil dereference at frame rate.
	if cfg.CaptureReplies {
		b.stored = newIDCache(cfg.CaptureIndexSize)
	}

	if cfg.FeedAuthors {
		b.authors = newIDCache(cfg.FeedAuthorsMax)
	}

	return b
}

// Run consumes the firehose until ctx is cancelled, reconnecting with exponential backoff and
// resuming from the last cursor it fully handled.
func (b *Bridge) Run(ctx context.Context) error {
	log.Trace("bluesky.Bridge.Run()")

	// The flusher is stopped through a derived context rather than the caller's, so it is shut down
	// (and gets to make its final flush) even if serve returns for a reason of its own.
	flushCtx, stopFlusher := context.WithCancel(ctx)
	defer stopFlusher()

	var wg sync.WaitGroup

	if b.recall != nil {
		wg.Add(1)

		go func() {
			defer wg.Done()

			b.recall.Run(flushCtx)
		}()
	}

	if b.feedMode() {
		wg.Add(1)

		go func() {
			defer wg.Done()

			b.runFeed(flushCtx)
		}()
	}

	log.WithFields(log.Fields{
		"url":             b.cfg.URL,
		"collections":     b.cfg.Collections,
		"dids":            len(b.cfg.DIDs),
		"events":          b.cfg.Events,
		"recall":          b.cfg.Recall,
		"feed":            b.cfg.Feed,
		"capture_replies": b.cfg.CaptureReplies,
		"feed_authors":    b.cfg.FeedAuthors,
	}).
		Info("Bluesky bridge consuming")

	err := b.serve(ctx)

	stopFlusher()
	wg.Wait()

	return err
}

// runFeed seeds the store from the feed and then re-reads it for new posts until ctx is cancelled.
//
// Every failure here is logged and retried on the next tick rather than returned: the feed is a
// secondary source, and an AppView having a bad minute should not take down a bridge whose
// engagement stream is still flowing.
func (b *Bridge) runFeed(ctx context.Context) {
	if b.cfg.FeedBackfill {
		if err := b.backfillFeed(ctx); err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("seeding from the feed failed; continuing with live polling")
		}
	}

	ticker := time.NewTicker(b.cfg.FeedPollInterval)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-ticker.C:
			if err := b.pollFeed(ctx); err != nil && ctx.Err() == nil {
				log.WithError(err).Warn("reading the feed failed; retrying on the next tick")
			}
		}
	}
}

// backfillFeed seeds the store from the whole feed, once.
func (b *Bridge) backfillFeed(ctx context.Context) error {
	mems, err := b.feed.Backfill(ctx)
	if err != nil {
		return err
	}

	if len(mems) == 0 {
		return nil
	}

	// A backfill is the one place links ride ON the write: an import applies them in a second pass
	// once every row in the batch exists, so a link to a post later in the same seed resolves rather
	// than dangling, and several hundred posts are related without several hundred extra calls.
	b.attachLinks(mems)

	// ImportMemories, not StoreMemories: this is the one write that must carry the seeded recall
	// counts, and it is safe to upsert precisely because it happens once, before anything live has
	// reinforced any of these.
	imported, err := b.store.ImportMemories(ctx, memoriesOf(mems))
	if err != nil {
		return err
	}

	b.rememberFeedPosts(mems)

	log.WithFields(log.Fields{"posts": len(mems), "imported": imported, "seeded": b.cfg.FeedSeedRecalls}).
		Info("seeded the store from the feed")

	return nil
}

// pollFeed stores whatever the newest page holds that the store does not already have.
func (b *Bridge) pollFeed(ctx context.Context) error {
	mems, err := b.feed.Poll(ctx)
	if err != nil {
		return err
	}

	if len(mems) == 0 {
		return nil
	}

	// StoreMemories, never ImportMemories: a feed hands back the same posts every read, and an
	// upsert would roll each one's accumulated reinforcement back to nothing.
	if err := b.store.StoreMemories(ctx, memoriesOf(mems)); err != nil {
		return err
	}

	b.rememberFeedPosts(mems)
	b.linkMemories(ctx, mems)

	return nil
}

// memoriesOf drops the messages the feed carried alongside its memories.
func memoriesOf(in []feedMemory) []*contract.Memory {
	out := make([]*contract.Memory, 0, len(in))

	for _, v := range in {
		out = append(out, v.Memory)
	}

	return out
}

// linkStored relates a post that has just been written, best-effort.
//
// After the write rather than on it, because a link target must exist and the store's whole job is
// forgetting: attaching links to the create would let a neighbour consolidated a minute ago fail the
// write itself. Afterwards, the worst case is one skipped link.
func (b *Bridge) linkStored(ctx context.Context, msg bridge.Message) {
	if b.topics == nil {
		return
	}

	id := msg.Headers[hdrURI]

	links := b.linksFor(msg, id)
	if len(links) == 0 {
		return
	}

	if err := b.store.Link(ctx, id, links); err != nil {
		log.WithError(err).WithField("id", id).
			Debug("relating a post to its neighbours failed; it is stored unrelated")
	}
}

// linkMemories does the same for a batch the feed produced, using the message each memory was built
// from rather than reconstructing one from what happened to reach metadata.
func (b *Bridge) linkMemories(ctx context.Context, mems []feedMemory) {
	if b.topics == nil {
		return
	}

	for _, v := range mems {
		if v.Memory == nil {
			continue
		}

		links := b.linksFor(v.Message, v.Memory.GetId())
		if len(links) == 0 {
			continue
		}

		if err := b.store.Link(ctx, v.Memory.GetId(), links); err != nil {
			log.WithError(err).WithField("id", v.Memory.GetId()).
				Debug("relating a post to its neighbours failed; it is stored unrelated")
		}
	}
}

// serve is the reconnect loop. Split from Run so the flusher's lifecycle is not entangled with it,
// and from consume so a test can assert which cursor a reconnect resumed at.
func (b *Bridge) serve(ctx context.Context) error {
	cursor := b.cfg.Cursor
	backoff := b.cfg.ReconnectBackoff

	for {
		// Checked BEFORE the dial, not only after it. A websocket dial on an already-cancelled
		// context is not reliably instant - it can get as far as a DNS lookup and a TCP connect - so
		// testing afterwards means a bridge shut down during startup still reaches out to the
		// network once, and a test with a cancelled context blocks on a real host.
		if ctx.Err() != nil {
			return nil
		}

		s, err := b.dial(ctx, b.cfg, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			log.WithError(err).WithField("cursor", cursor).
				Warn("connecting to Jetstream failed; retrying after backoff")

			if sleepErr := sleep(ctx, backoff); sleepErr != nil {
				return nil
			}

			backoff = growBackoff(backoff, b.cfg.ReconnectMaxBackoff)

			continue
		}

		// A connection that opened is a connection that worked, whatever happens to it next.
		backoff = b.cfg.ReconnectBackoff

		last, err := b.consume(ctx, s, cursor)

		_ = s.Close()

		if last > cursor {
			cursor = last
		}

		if ctx.Err() != nil {
			return nil
		}

		log.WithError(err).WithField("cursor", cursor).
			Warn("Jetstream connection ended; reconnecting from the last handled cursor")

		if sleepErr := sleep(ctx, backoff); sleepErr != nil {
			return nil
		}

		backoff = growBackoff(backoff, b.cfg.ReconnectMaxBackoff)
	}
}

// consume drives the read/decode/dispatch loop for one connection, returning the highest cursor it
// fully handled. Split out so it can be exercised with a fake stream: every branch below - decode
// failure, unhandled kind, store failure and retry, retry exhaustion - is reachable from a canned
// slice of frames with no network.
func (b *Bridge) consume(ctx context.Context, s stream, cursor int64) (int64, error) {
	for {
		frame, err := s.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return cursor, nil
			}

			return cursor, fmt.Errorf("reading from Jetstream: %w", err)
		}

		var ev event

		if err := json.Unmarshal(frame, &ev); err != nil {
			// A malformed frame can never become valid, and holding the connection while we think
			// about it is how a consumer gets dropped for being slow. Jetstream's frames are machine
			// generated, so this is a protocol change rather than a data problem - hence Warn with
			// the frame, which is the only useful thing to say about it.
			log.WithError(err).WithField("frame", truncate(string(frame), 256)).
				Warn("skipping an undecodable Jetstream frame")

			cursor = advance(cursor, ev.cursor())

			continue
		}

		// identity, account and (on v2) sync frames arrive unconditionally whatever
		// wantedCollections says, and more kinds may be added. They are protocol traffic, not
		// messages, so they are not counted as messages handled.
		if ev.Kind != kindCommit || ev.Commit == nil {
			cursor = advance(cursor, ev.cursor())

			continue
		}

		if err := b.handleWithRetries(ctx, &ev); err != nil {
			if ctx.Err() != nil {
				return cursor, nil
			}

			// The cursor is deliberately NOT advanced past this frame, so reconnecting replays from
			// the failure rather than skipping it.
			return cursor, err
		}

		cursor = advance(cursor, ev.cursor())
	}
}

// handleWithRetries dispatches one frame, retrying in place before giving up on the connection. A
// transient service failure is far more likely than a permanently poisonous frame, so retrying
// where we stand is cheaper than a reconnect - but a frame that will never store must not wedge the
// loop, hence the bound.
func (b *Bridge) handleWithRetries(ctx context.Context, ev *event) error {
	var err error

	for attempt := 0; ; attempt++ {
		if err = b.dispatch(ctx, ev); err == nil {
			return nil
		}

		if attempt >= b.cfg.MaxRetries {
			return err
		}

		log.WithError(err).WithFields(log.Fields{
			"attempt":    attempt + 1,
			"collection": ev.Commit.Collection,
		}).
			Warn("handling a Jetstream frame failed; retrying")

		if sleepErr := sleep(ctx, b.cfg.ErrorBackoff); sleepErr != nil {
			return sleepErr
		}
	}
}

// dispatch routes one commit.
//
// Posts go through the Store (so hippocampus.bridge.messages counts posts, including the ones the
// transformer filters), and engagement goes to the recall buffer (so hippocampus.bridge.recalls
// counts reinforcement). Running likes through the Store as well would add several hundred
// `filtered` messages a second that say nothing the recall counter does not say better.
func (b *Bridge) dispatch(ctx context.Context, ev *event) error {
	c := ev.Commit
	msg := toMessage(ev)

	switch c.Collection {

	case CollectionPost:
		return b.dispatchPost(ctx, msg, c.Operation)

	case CollectionLike, CollectionRepost:
		if c.Operation != operationCreate {
			// An unlike does not un-reinforce. There is no such operation and there should not be:
			// reinforcement is a fact about the past, not a mutable count.
			return nil
		}

		return b.reinforce(ctx, msg.Headers[hdrSubject])

	default:
		return nil
	}
}

func (b *Bridge) dispatchPost(ctx context.Context, msg bridge.Message, operation string) error {
	if operation == operationDelete {
		if !b.cfg.HonourDeletes {
			return nil
		}

		// Flushed first so a buffered like cannot reinforce a memory this call is about to delete.
		// Deletes are rare, so the extra flush costs nothing and removes a genuinely confusing race.
		if b.recall != nil {
			if err := b.recall.Flush(ctx); err != nil {
				return err
			}
		}

		return b.store.Forget(ctx, []string{msg.Headers[hdrURI]})
	}

	// In feed mode the feed decides what is worth storing, so a post arriving on the firehose is not
	// stored - unless one of the capture rules claims it - but it is still ENGAGEMENT when it is a
	// reply, and the reply's parent may well be one of the feed's posts. Dropping the frame entirely
	// would silently lose that reinforcement.
	if !b.feedMode() || b.captureFromFirehose(msg) {
		if err := b.storePost(ctx, msg, msg.Headers[hdrReplyRoot]); err != nil {
			return err
		}

		b.linkStored(ctx, msg)
	}

	// A reply is engagement with what it replies to, exactly as a like is.
	return b.reinforce(ctx, msg.Headers[hdrReplyParent])
}

// captureFromFirehose reports whether a post the FEED did not select should nonetheless be stored.
//
// Two independent reasons, either sufficient, both answered from memory because this runs on every
// post frame on the open firehose:
//
//   - --capture-replies: the post replies to a thread this bridge holds. The root is tried first
//     because it names the thread rather than the immediately preceding post, so it still matches
//     however deep in the conversation the reply sits; the parent is tried after it for the case
//     where the thread's root was never stored but one of its replies was.
//   - --feed-authors: the post's author is an account this feed has surfaced, so it is one of the
//     accounts the feed is made of saying something the feed did not pick up.
//
// A false answer leaves the frame exactly where it was: not stored, still reinforcing whatever it
// replies to.
func (b *Bridge) captureFromFirehose(msg bridge.Message) bool {
	if b.stored != nil {
		if root := msg.Headers[hdrReplyRoot]; root != "" && b.stored.Contains(root) {
			return true
		}

		if parent := msg.Headers[hdrReplyParent]; parent != "" && b.stored.Contains(parent) {
			return true
		}
	}

	if b.authors != nil {
		if did := msg.Headers[hdrDID]; did != "" && b.authors.Contains(did) {
			return true
		}
	}

	return false
}

// rememberFeedPosts records what a feed read produced, for the capture rules to match against.
//
// Only the feed's own posts go in, never the replies captured through them: the index is bounded,
// and letting one busy thread's replies fill it would evict the very posts the other threads are
// matched on. Nothing is lost by that - a reply to a captured reply carries the same thread ROOT,
// which is still here.
//
// A memory the service declined (below its minimum significance) is remembered like any other,
// since the batch writes do not report which. The cost is capturing a reply to a post the store
// does not hold, which stores one memory that opens its own thread rather than joining one - the
// same thing that happens to any reply whose root has been consolidated away.
func (b *Bridge) rememberFeedPosts(mems []feedMemory) {
	if b.stored == nil && b.authors == nil {
		return
	}

	for _, v := range mems {
		if b.stored != nil && v.Memory.GetId() != "" {
			b.stored.Add(v.Memory.GetId())
		}

		if b.authors == nil {
			continue
		}

		if did := v.Message.Headers[hdrDID]; did != "" {
			b.authors.Add(did)
		}
	}
}

// storePost writes one post, opening its thread's event when thread modelling is on.
func (b *Bridge) storePost(ctx context.Context, msg bridge.Message, root string) error {
	if b.cfg.Events != EventsThread {
		return b.store.Handle(ctx, msg)
	}

	// A top-level post opens its own thread: the event and the memory that opens it are one RPC.
	if root == "" || root == msg.Headers[hdrURI] {
		if err := b.store.HandleEvent(ctx, msg, b.threadEvent(msg)); err != nil {
			return err
		}

		b.roots.Add(msg.Headers[hdrURI])

		return nil
	}

	// A reply's root is usually a post we never saw - it may predate this connection, or the store
	// may have forgotten it - so ensure-first is right here. The optimistic alternative (store, catch
	// FailedPrecondition, create, retry) costs three RPCs whenever the root is absent, which on a
	// short retention window is most of the time.
	if !b.roots.Contains(root) {
		if err := b.store.EnsureEvent(ctx, b.rootEvent(root, msg)); err != nil {
			return err
		}

		b.roots.Add(root)
	}

	err := b.store.Handle(ctx, msg)

	// The one case ensure-first does not cover: a root we cached can be consolidated by the store's
	// own sleep cycle between our seeing it and our writing to it. Handle wraps the status with %w,
	// which status.FromError unwraps, so the code survives the wrap.
	if status.Code(err) == codes.FailedPrecondition {
		b.roots.Remove(root)

		if ensureErr := b.store.EnsureEvent(ctx, b.rootEvent(root, msg)); ensureErr != nil {
			return ensureErr
		}

		b.roots.Add(root)

		// One retry, never two: a second FailedPrecondition means something other than a race.
		return b.store.Handle(ctx, msg)
	}

	return err
}

// reinforce buffers one engagement, when recall is enabled and there is something to reinforce.
func (b *Bridge) reinforce(ctx context.Context, id string) error {
	if b.recall == nil || id == "" {
		return nil
	}

	return b.recall.Add(ctx, id)
}

// threadEvent is the event a top-level post opens.
func (b *Bridge) threadEvent(msg bridge.Message) *contract.Event {
	name := truncate(string(msg.Data), maxEventNameLength)
	if strings.TrimSpace(name) == "" {
		name = "post " + msg.Headers[hdrRkey]
	}

	return &contract.Event{
		Id:        msg.Headers[hdrURI],
		Name:      name,
		TimeStart: eventStart(msg.Timestamp),
		Group:     b.cfg.Group,
	}
}

// rootEvent is the event created for a thread whose root post this bridge never saw.
//
// TimeStart is the REPLY's time, because the root's own is unknown: it is a defensible approximation
// that keeps the event's decay clock roughly right, and it errs recent rather than ancient, which is
// the safer direction for something that is demonstrably still being replied to.
func (b *Bridge) rootEvent(root string, msg bridge.Message) *contract.Event {
	return &contract.Event{
		Id:          root,
		Name:        "thread " + rkeyOf(root),
		Description: truncate(root, maxEventDescriptionLength),
		TimeStart:   eventStart(msg.Timestamp),
		Group:       b.cfg.Group,
	}
}

// eventStart clamps an event's start into the past.
//
// Unlike a memory's timestamp, the service does NOT reject a future-dated event - Event.Validate has
// no clock-skew guard - and an event with a negative age never decays. createdAt is client-supplied
// and routinely wrong, so the clamp has to happen here.
func eventStart(t time.Time) int64 {
	now := time.Now()

	if t.IsZero() || t.After(now) {
		return now.UnixNano()
	}

	return t.UnixNano()
}

// rkeyOf returns the record key from an at:// URI, or the whole string if it has no path.
func rkeyOf(u string) string {
	if i := strings.LastIndex(u, "/"); i >= 0 && i < len(u)-1 {
		return u[i+1:]
	}

	return u
}

// truncate shortens s to at most max BYTES without ever splitting a rune.
//
// Both halves of that are load-bearing, and getting either wrong fails in production but not in a
// fixture. The caps this serves are the service's, and the service counts BYTES (types/event.go
// checks len(e.Name)); counting runes instead passes a 256-emoji post through at over a kilobyte and
// has every event refused - which is precisely what the first live run against the firehose did.
// Slicing raw bytes is equally wrong: splitting a multi-byte character yields a string that is not
// valid UTF-8, which a proto3 string field cannot carry at all.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}

	// Walk back from the byte budget to the nearest rune boundary.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}

// advance moves the cursor forward, never backwards: frames arrive in order, but a frame carrying no
// cursor at all must not reset the resume point to zero and replay the lookback window.
func advance(cursor int64, next int64) int64 {
	if next > cursor {
		return next
	}

	return cursor
}

func growBackoff(current time.Duration, max time.Duration) time.Duration {
	next := current * 2

	if next > max {
		return max
	}

	return next
}

// sleep waits for d or until ctx is cancelled, returning ctx.Err() if cancelled first.
//
// Duplicated from the Kafka adapter rather than shared: it is twelve lines, and hoisting it into
// bridge would couple five adapters to a utility package for the sake of not repeating a select.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {

	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}
