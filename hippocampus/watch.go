package hippocampus

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// The Watch RPC (TODO-3 item 193): the store's changes, streamed over the connection a client
// already holds.
//
// What follows describes the feed with watch.changeLog.enabled off, which is the default. With it
// on, the same hub is fed from the durable change log instead, and the stream can resume; see
// watchlog.go (TODO-3 item 204).
//
// It is LIVE AND BEST-EFFORT, and that was the decision the item left open. The callback queue's seq
// looked like the obvious cursor for a resumable stream, but it is not one: a queue row is deleted
// once its receiver accepts it, rows are written only on the consolidating instance, and only while
// callbacks.* is configured. Resuming would need a durable change log of its own - a table on every
// write, with its own retention - which is a larger thing than this item and is not what the
// integrations asking for it need. An agent's local cache, a broker bridge, the Bluesky bridge
// reopening a forgotten thread: each wants to hear about changes while it is connected, and re-reads
// the store after a gap, which `dropped` tells it to do.
//
// The feed is per instance. The store hands each committed change to this process's hub (db's
// ChangeSink), so a watcher on a replica sees that replica's writes and the consolidating instance's
// watchers see what the sleep cycle forgets. A cross-instance feed would need the store to carry
// the changes between processes, which is the durable log again.
//
// A watcher never slows the store. Publish runs on the goroutine that committed the change, after the
// commit, so it only ever offers an event to each watcher's buffer and counts what does not fit.

// watchBuffer is how many events one watcher may have waiting before further ones are dropped. A
// var so tests can shrink it.
var watchBuffer = 256

// watchKinds are the kinds a watcher may ask for, and what an empty request carries. At-risk is not
// among them: it costs a preview scan per cycle, and is a callbacks.* delivery only.
var watchKinds = map[contract.CallbackKind]bool{
	contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED:    true,
	contract.CallbackKind_CALLBACK_KIND_MEMORY_UPDATED:   true,
	contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN: true,
	contract.CallbackKind_CALLBACK_KIND_EVENT_FORGOTTEN:  true,
	contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED:  true,
}

// watcher is one subscribed stream.
type watcher struct {
	kinds  map[contract.CallbackKind]bool
	groups []string
	scoped bool

	events  chan *contract.WatchEvent
	dropped atomic.Int64
}

// wants reports whether this watcher takes events of a kind.
func (w *watcher) wants(kind contract.CallbackKind) bool {
	return w.kinds[kind]
}

// visible filters items to the watcher's scope. An unscoped watcher sees them all.
func (w *watcher) visible(items []*contract.WatchItem) []*contract.WatchItem {
	if !w.scoped {

		return items
	}

	out := make([]*contract.WatchItem, 0, len(items))

	for _, item := range items {
		if !auth.GroupInScope(w.groups, item.GetGroup()) {
			continue
		}

		out = append(out, item)
	}

	return out
}

// filter is the watcher's copy of an event: nothing when the watcher does not take its kind, its
// items narrowed to the watcher's scope otherwise. A change wholly outside the scope is no event at
// all, rather than an empty one: an empty event would tell the watcher that something happened
// elsewhere. A cycle's completion carries no items and goes to every watcher that takes the kind -
// it is store-global counts, which GetConsolidationStatus serves a scoped caller too.
func (w *watcher) filter(event *contract.WatchEvent) (*contract.WatchEvent, bool) {
	if !w.wants(event.GetKind()) {

		return nil, false
	}

	if event.GetKind() == contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED {

		return event, true
	}

	visible := w.visible(event.GetItems())

	if len(visible) == 0 {

		return nil, false
	}

	return &contract.WatchEvent{
		Kind:    event.GetKind(),
		Cause:   event.GetCause(),
		CycleId: event.GetCycleId(),
		Time:    event.GetTime(),
		Seq:     event.GetSeq(),
		Items:   visible,
		Cycle:   event.GetCycle(),
	}, true
}

// offer hands an event to the watcher without waiting, counting it as dropped when the buffer is
// full.
func (w *watcher) offer(ctx context.Context, event *contract.WatchEvent) {
	select {

	case w.events <- event:

	default:
		w.dropped.Add(1)

		tel.watchDropped.Add(ctx, 1)

	}
}

// watchHub is the set of subscribed watchers, and the store's db.ChangeSink.
type watchHub struct {
	mu       sync.Mutex
	watchers map[*watcher]struct{}
	closed   chan struct{}
	once     sync.Once

	count atomic.Int64
}

func newWatchHub() *watchHub {
	return &watchHub{
		watchers: make(map[*watcher]struct{}),
		closed:   make(chan struct{}),
	}
}

// Watching is the store's gate on capturing a change at all, so it must stay a single load.
func (h *watchHub) Watching() bool {
	return h.count.Load() > 0
}

// subscribe adds a watcher, refusing it once the hub is closed.
func (h *watchHub) subscribe(w *watcher) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	select {

	case <-h.closed:
		return false

	default:

	}

	h.watchers[w] = struct{}{}

	tel.watchSubscribers.Record(context.Background(), h.count.Add(1))

	return true
}

// unsubscribe removes a watcher.
func (h *watchHub) unsubscribe(w *watcher) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.watchers[w]; !ok {

		return
	}

	delete(h.watchers, w)

	tel.watchSubscribers.Record(context.Background(), h.count.Add(-1))
}

// close ends every stream, and refuses new ones. Called before the gRPC server's graceful stop,
// which would otherwise wait on streams that never end by themselves.
func (h *watchHub) close() {
	if h == nil {

		return
	}

	h.once.Do(func() {
		close(h.closed)
	})
}

// snapshot is the watchers to publish to, taken under the lock and offered to outside it.
func (h *watchHub) snapshot() []*watcher {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]*watcher, 0, len(h.watchers))

	for w := range h.watchers {
		out = append(out, w)
	}

	return out
}

// Publish offers the store's committed deliveries to every watcher that wants them. It is the
// in-process feed, used while the change log is off.
func (h *watchHub) Publish(deliveries []db.CallbackDelivery) {
	if len(h.snapshot()) == 0 {

		return
	}

	ctx := context.Background()
	now := time.Now().UnixNano()

	for _, delivery := range deliveries {
		h.publishEvent(ctx, &contract.WatchEvent{
			Kind:    callbackKinds[delivery.Kind],
			Cause:   deleteCauses[delivery.Cause],
			CycleId: delivery.CycleId,
			Time:    now,
			Items:   watchItems(delivery.Payload.Items),
		})
	}
}

// publishEvent offers one event to every watcher, each receiving its own filtered copy. Both feeds
// publish through it: the commit path's Publish, and the change log's reader.
func (h *watchHub) publishEvent(ctx context.Context, event *contract.WatchEvent) {
	if h == nil {

		return
	}

	for _, w := range h.snapshot() {
		filtered, ok := w.filter(event)
		if !ok {
			continue
		}

		w.offer(ctx, filtered)
	}
}

// announceCycle tells watchers a sleep cycle has finished: through the change log when there is
// one, so every instance's watchers hear it, and in process otherwise.
func (s *Server) announceCycle(ctx context.Context, cycleId int64, report *cycleReport) {
	if s.changes != nil {
		s.logCycle(ctx, cycleId, report)

		return
	}

	s.watch.publishEvent(ctx, &contract.WatchEvent{
		Kind:    contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED,
		CycleId: cycleId,
		Time:    time.Now().UnixNano(),
		Cycle:   cycleReportToProto(report),
	})
}

// startWatch registers the hub with the store, so it captures changes while anybody is watching.
// Only the concrete store can be told, through the seam SetCallbackPolicy uses.
func (s *Server) startWatch() {
	log.Trace("func() startWatch")

	s.watch = newWatchHub()

	if store, ok := s.db.(interface{ SetChangeSink(db.ChangeSink) }); ok {
		store.SetChangeSink(s.watch)
	}

	s.startChangeReader()
}

// CloseWatchers ends every Watch stream with Unavailable, so a client reconnects to an instance that
// is still serving. main calls it ahead of the gRPC server's graceful stop, which waits for every
// open stream and would otherwise wait on these until its timeout.
func (s *Server) CloseWatchers() {
	log.Trace("func() CloseWatchers")

	s.watch.close()
}

// Watch streams the store's changes as they commit. See the notes at the top of this file.
func (s *Server) Watch(in *contract.WatchRequest, stream grpc.ServerStreamingServer[contract.WatchEvent]) error {
	log.Debug("Watch()")

	ctx := stream.Context()

	kinds, err := requestedWatchKinds(in.GetKinds())
	if err != nil {

		return err
	}

	groups, scoped := s.scopedGroups(ctx)

	w := &watcher{
		kinds:  kinds,
		groups: groups,
		scoped: scoped,
		events: make(chan *contract.WatchEvent, watchBuffer),
	}

	if s.watch == nil {

		return status.Error(codes.Unavailable, "the service is shutting down")
	}

	// The cursor is read BEFORE subscribing, and the replay below runs after. Read after, it could
	// land on a change the reader had settled but not yet offered, and the stream would skip that
	// change when it arrived; read before, anything settled in between is replayed, and anything also
	// buffered is skipped as already sent.
	cursor, resuming, err := s.openCursor(ctx, in)
	if err != nil {

		return err
	}

	if !s.watch.subscribe(w) {

		return status.Error(codes.Unavailable, "the service is shutting down")
	}

	defer s.watch.unsubscribe(w)

	// Headers go out as soon as the watcher is subscribed, rather than with the first event, so a
	// client can wait on them and know that nothing it does afterwards will be missed. With the
	// change log they also say where in it the stream starts.
	header := metadata.MD{}

	if cursor != nil {
		header.Set(cursorHeader, cursorHeaderValue(cursor))
	}

	if err := stream.SendHeader(header); err != nil {

		return err
	}

	// The span covers the subscription rather than each event, since a stream can stay open for days
	// and a span that long says nothing; the events it sent are summed on it when it ends.
	ctx, span := tel.tracer.Start(ctx, "watch")
	defer span.End()

	span.AddEvent("watch subscribed", trace.WithAttributes(
		attribute.Bool("scoped", scoped),
		attribute.Bool("resuming", resuming),
		attribute.Bool("change_log", cursor != nil),
	))

	var sent int64

	defer func() {
		span.SetAttributes(attribute.Int64("events_sent", sent))
	}()

	// The resume, or for a live stream whatever settled while it subscribed - usually nothing. The
	// cursor was checked as it was opened.
	if cursor != nil {
		replayed, err := s.replay(ctx, w, cursor, stream)

		sent += replayed

		if err != nil {

			return err
		}

		if resuming {
			span.AddEvent("watch resumed", trace.WithAttributes(attribute.Int64("replayed", replayed)))
		}
	}

	for {
		select {

		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()

		case <-s.watch.closed:
			span.AddEvent("watch closed by shutdown")

			return status.Error(codes.Unavailable, "the service is shutting down")

		case event := <-w.events:
			if cursor != nil {
				caught, err := s.catchUp(ctx, w, cursor, stream)

				sent += caught

				if err != nil {

					return err
				}

				if caught > 0 {
					span.AddEvent("watch caught up", trace.WithAttributes(attribute.Int64("replayed", caught)))
				}

				// Already sent by a replay, or by the catch-up just now.
				if event.GetSeq() <= cursor.seq {
					continue
				}

				cursor.seq = event.GetSeq()
			}

			event.Dropped = w.dropped.Swap(0)

			if event.GetDropped() > 0 {
				span.AddEvent("watch events dropped", trace.WithAttributes(attribute.Int64("dropped", event.GetDropped())))
			}

			if err := stream.Send(event); err != nil {

				return err
			}

			sent++

		}
	}
}

// catchUp replays what a watcher's full buffer dropped, from the change log, and reports how many
// events it sent. A no-op while nothing has been dropped.
//
// The event the stream just took predates every drop - the buffer is a queue, and a drop is what
// happens when it is full - so replaying from the cursor covers it and everything dropped after it,
// and the stream then skips the buffered copies up to where the replay reached.
func (s *Server) catchUp(
	ctx context.Context,
	w *watcher,
	cursor *watchCursor,
	stream grpc.ServerStreamingServer[contract.WatchEvent],
) (int64, error) {
	var sent int64

	for w.dropped.Swap(0) > 0 {
		// The cursor may be old by now, on a stream whose kinds are rare; if the caps have taken it,
		// so may they have what was dropped, and the watcher must re-read.
		if err := s.checkCursor(ctx, cursor.seq); err != nil {

			return sent, err
		}

		replayed, err := s.replay(ctx, w, cursor, stream)

		sent += replayed

		if err != nil {

			return sent, err
		}
	}

	return sent, nil
}

// requestedWatchKinds resolves a request's kinds, refusing one Watch does not carry. Empty is every
// kind it does.
func requestedWatchKinds(requested []contract.CallbackKind) (map[contract.CallbackKind]bool, error) {
	if len(requested) == 0 {

		return watchKinds, nil
	}

	out := make(map[contract.CallbackKind]bool, len(requested))

	for _, kind := range requested {
		if !watchKinds[kind] {

			return nil, status.Errorf(codes.InvalidArgument, "Watch does not carry %s", kind)
		}

		out[kind] = true
	}

	return out, nil
}
