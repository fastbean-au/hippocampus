package bridge

import (
	"context"
	"fmt"

	"github.com/fastbean-au/hippocampus/observability"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// The Store methods here are separate from store.go's Handle deliberately: Handle is the path all
// five adapters have always taken and is unchanged by any of this, and keeping the additions in
// their own file makes that trivially checkable in a diff.
//
// What they share is one rule, which is what makes a reinforcing bridge able to hold no state at
// all: an id the store does not have is never an error. The service's recall is an UPDATE ... WHERE
// id IN (...) that matches nothing; its create is a plain INSERT, so a duplicate surfaces as
// AlreadyExists; its delete reports Ok false. So a bridge can reinforce, open, and retract by id
// without ever asking first whether the id is there - which is the difference between an adapter
// that needs a database of what it has written and one that needs nothing.

// Recall reinforces the named memories: each one's decay clock resets to now and its recall count
// rises, which together raise its effective significance for the next consolidation cycle.
//
// An id the store does not hold is a SILENT NO-OP, so a caller reinforcing from an engagement stream
// needs no record of what it has previously written and no lookup before asking. That is the whole
// reason this method can exist without state. It also means the returned count is not an error
// signal but a HIT RATE: how much of the stream landed on something the store still remembers.
//
// The bridge's token must be UNSCOPED and at least WRITER tier. A group-scoped token turns an id the
// store does not hold into NotFound for the whole batch (the service scope-checks the ids before
// recalling them, so "no such memory" and "not yours" are deliberately indistinguishable), and a
// reader-tier token gets a plain non-reinforcing read unless the deployment sets
// auth.readerRecallReinforces. NotFound is nonetheless absorbed here rather than returned, so a
// misconfigured token degrades to "reinforcement stops working" rather than "the bridge stops
// consuming".
func (s *Store) Recall(ctx context.Context, ids []string) (int, error) {
	log.Trace("Store.Recall()")

	if len(ids) == 0 {
		return 0, nil
	}

	ctx, cancel := s.callContext(ctx)
	defer cancel()

	tel.recallBatch.Record(ctx, int64(len(ids)), observability.WithGroup(attribute.String(attrBroker, s.broker)))

	// include_linked stays false: it would pull back one-hop neighbours that are NOT reinforced,
	// inflating both the response and the hit count below with memories nothing engaged with.
	resp, err := s.client.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: ids})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			s.recordRecall(ctx, OutcomeMissing, len(ids))

			log.WithField("ids", len(ids)).
				Debug("recall reported no such memory; the bridge's token is group-scoped")

			return 0, nil
		}

		s.recordRecall(ctx, OutcomeFailed, len(ids))

		return 0, err
	}

	hits := len(resp.GetMemories())

	s.recordRecall(ctx, OutcomeReinforced, hits)
	s.recordRecall(ctx, OutcomeMissing, len(ids)-hits)

	return hits, nil
}

// Forget deletes the named memories, for an upstream that has retracted them. Ids the store does not
// hold are a no-op (the service reports Ok false rather than an error), so this is as stateless as
// Recall.
//
// It exists because decay and deletion answer different questions: decay is about significance,
// deletion is about consent. A bridge carrying records an upstream can withdraw needs both.
func (s *Store) Forget(ctx context.Context, ids []string) error {
	log.Trace("Store.Forget()")

	if len(ids) == 0 {
		return nil
	}

	ctx, cancel := s.callContext(ctx)
	defer cancel()

	if _, err := s.client.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: ids}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}

		return err
	}

	return nil
}

// EnsureEvent creates ev if it is not already there.
//
// AlreadyExists is a SUCCESS, and that is the load-bearing part: the service's event create is a
// plain INSERT, so a duplicate id surfaces as AlreadyExists, which collapses "create the event if it
// is missing" and "replay this frame after a reconnect" into one rule and one code path. A caller
// therefore needs neither an existence check before nor a cache to be correct - a cache only saves
// the round trip.
func (s *Store) EnsureEvent(ctx context.Context, ev *contract.Event) error {
	log.Trace("Store.EnsureEvent()")

	ctx, cancel := s.callContext(ctx)
	defer cancel()

	resp, err := s.client.StoreEvent(ctx, ev)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			s.recordEvent(ctx, OutcomeExists)

			return nil
		}

		s.recordEvent(ctx, OutcomeFailed)

		return err
	}

	if resp.GetRejected() {
		s.recordEvent(ctx, OutcomeRejected)

		return nil
	}

	s.recordEvent(ctx, OutcomeCreated)

	return nil
}

// HandleEvent transforms msg and stores the resulting memories as ev's NESTED memories, so an event
// and the memory that opens it cost one RPC rather than two. AlreadyExists is a success, as in
// EnsureEvent.
//
// Nested memories are best-effort service-side: the event is committed first, so a nested memory
// that fails validation or hits a store error is logged there and surfaces here only as a shortfall
// in memory_count. This therefore cannot tell a memory declined for insignificance from one that hit
// a transport error, and treats any shortfall as Rejected - a success, nothing to redeliver. A
// caller needing that distinction should EnsureEvent then Handle, and pay the extra round trip.
func (s *Store) HandleEvent(ctx context.Context, msg Message, ev *contract.Event) error {
	log.Trace("Store.HandleEvent()")

	mems, err := s.transformer.Transform(msg)
	if err != nil {
		s.recordEvent(ctx, OutcomeFailed)

		return err
	}

	if len(mems) == 0 {
		s.recordEvent(ctx, OutcomeFiltered)

		return nil
	}

	ev.Memories = mems

	ctx, cancel := s.callContext(ctx)
	defer cancel()

	resp, err := s.client.StoreEvent(ctx, ev)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			s.recordEvent(ctx, OutcomeExists)

			// The EVENT already existing says nothing about its memories, and returning here would
			// silently drop them. That is not a corner case: an event is routinely opened by
			// something other than the record that owns it - a reply arriving before the post it
			// replies to opens that post's event, and when the post itself turns up its own memory
			// would vanish. So fall back to storing them individually, which is idempotent for the
			// same reason (a duplicate memory is AlreadyExists too, absorbed in storeEach).
			//
			// The stamp below is what keeps that fallback equivalent to the nested write rather than
			// merely non-lossy. A nested memory carries no event_id of its own - the service stamps
			// the event's id on it as it creates them - so storing them individually without it
			// wrote them LOOSE, and the very case this path exists for (the post that opens a thread
			// arriving after a reply already opened its event) produced an event whose own opening
			// post was not among its memories. Only an unset one is stamped: a transformer naming a
			// different event meant it.
			for _, v := range mems {
				if v != nil && v.GetEventId() == "" {
					v.EventId = ev.GetId()
				}
			}

			return s.storeEach(ctx, mems)
		}

		s.recordEvent(ctx, OutcomeFailed)

		return err
	}

	if resp.GetRejected() || int(resp.GetMemoryCount()) < len(mems) {
		s.recordEvent(ctx, OutcomeRejected)

		return nil
	}

	s.recordEvent(ctx, OutcomeCreated)

	attrs := observability.WithGroup(attribute.String(attrBroker, s.broker))

	for _, v := range mems {
		tel.bodyBytes.Record(ctx, int64(len(v.GetBody())), attrs)
	}

	tel.memories.Add(ctx, int64(len(mems)), observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, OutcomeStored),
	))

	return nil
}

// StoreMemories writes each memory, treating one the store already holds as a success.
//
// That is what a POLLED source needs, as against a streamed one: re-reading a ranked feed hands back
// the same posts every time, and only the ones that are new should be written. Letting AlreadyExists
// mean "already have it" turns re-reading into a no-op with no bookmark to keep - and crucially it
// leaves the existing memory ALONE, so reinforcement it has accumulated since is not overwritten.
// ImportMemories would clobber exactly that, which is why polling must not use it.
func (s *Store) StoreMemories(ctx context.Context, mems []*contract.Memory) error {
	return s.storeEach(ctx, mems)
}

// ImportMemories upserts memories WITH their recall history, in chunks.
//
// This is for seeding: a source that reports engagement the bridge never witnessed (a feed handing
// back a post's like count) can express it as a recall count, which is the only field that means
// "returned to this many times". StoreMemory refuses to carry that, on purpose.
//
// It is a FULL-STATE upsert, so importing an id the store already holds REPLACES it - including its
// recall count. Use it to seed once; use StoreMemories for anything repeated, or live reinforcement
// will be silently rolled back to whatever the source last reported.
func (s *Store) ImportMemories(ctx context.Context, mems []*contract.Memory) (int, error) {
	log.Trace("Store.ImportMemories()")

	imported := 0

	for start := 0; start < len(mems); start += importChunkSize {
		end := min(start+importChunkSize, len(mems))

		n, err := s.importChunk(ctx, mems[start:end])
		if err != nil {
			return imported, err
		}

		imported += n
	}

	return imported, nil
}

// importChunkSize bounds one ImportBatch so a large seed cannot build a message over the receive
// frame limit. Feed pages are a hundred posts, so this costs no extra round trips in practice.
const importChunkSize = 100

func (s *Store) importChunk(ctx context.Context, mems []*contract.Memory) (int, error) {
	ctx, cancel := s.callContext(ctx)
	defer cancel()

	resp, err := s.client.ImportBatch(ctx, &contract.ImportBatchRequest{Memories: mems})
	if err != nil {
		return 0, err
	}

	n := int(resp.GetMemoriesImported())

	attrs := observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, OutcomeStored),
	)

	tel.memories.Add(ctx, int64(n), attrs)

	return n, nil
}

// storeEach writes memories one at a time, for the paths where nesting them in an event create was
// not possible. A memory the store already holds is AlreadyExists, which is a success here for the
// same reason it is on the event: the id is the upstream record's, so a replayed frame writing the
// same memory twice is exactly what at-least-once delivery is expected to do.
//
// A memory naming an event the store does not have is skipped rather than failing the batch, and
// that is the difference between a batch that makes progress and one that cannot. The service
// refuses such a memory with FailedPrecondition (it would be a dangling reference), and a bridge
// writing a POLLED source hits it routinely: the source hands back the same page every read, so if
// one orphaned memory in it aborts the write, every memory after that one in the page is never
// written - and the next poll returns the same page and stops in the same place. That is a
// permanent stall dressed up as a transient error, and no amount of retrying clears it. Skipping
// costs one memory that could not have been stored anyway; aborting costs all of them.
//
// Narrowed to memories that actually name an event, so a FailedPrecondition arising from anything
// else still fails the batch and is still reported.
func (s *Store) storeEach(ctx context.Context, mems []*contract.Memory) error {
	for i, v := range mems {
		if v == nil {
			continue
		}

		_, err := s.store(ctx, v)
		if err == nil {
			continue
		}

		if status.Code(err) == codes.AlreadyExists {
			continue
		}

		if status.Code(err) == codes.FailedPrecondition && v.GetEventId() != "" {
			s.recordOrphaned(ctx)

			log.WithFields(log.Fields{
				"id":       v.GetId(),
				"event_id": v.GetEventId(),
			}).
				Debug("memory skipped: the event it names is not in the store")

			continue
		}

		return fmt.Errorf("storing memory %d of %d: %w", i+1, len(mems), err)
	}

	return nil
}

// recordOrphaned counts one memory skipped for naming an event the store does not hold.
func (s *Store) recordOrphaned(ctx context.Context) {
	tel.memories.Add(ctx, 1, observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, OutcomeOrphaned),
	))
}

// Link relates one memory to others, best-effort.
//
// It is deliberately a SEPARATE call rather than links attached to the create, and the reason is
// that a link target must exist: attaching them to the write would mean a target the store has since
// forgotten fails the whole write, and in a store whose entire job is forgetting that is not a corner
// case. Issued afterwards, the worst outcome is that this one call reports NotFound and the memory
// is stored unrelated - which is why NotFound is absorbed here rather than returned.
//
// A backfill is the exception and should attach links to its ImportBatch instead: an import applies
// links in a second pass once every row in the batch exists, so intra-batch targets resolve and no
// extra call is needed.
func (s *Store) Link(ctx context.Context, id string, links []*contract.Link) error {
	if id == "" || len(links) == 0 {
		return nil
	}

	ctx, cancel := s.callContext(ctx)
	defer cancel()

	_, err := s.client.LinkMemories(ctx, &contract.LinkMemoriesRequest{Id: id, Links: links})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.WithField("id", id).
				Debug("linking skipped: a target has already been forgotten")

			return nil
		}

		return err
	}

	return nil
}

// callContext applies the Store's per-call timeout, shared by every method here and by store(). The
// returned cancel is always non-nil, so a caller can defer it unconditionally.
func (s *Store) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.callTimeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, s.callTimeout)
}

// recordRecall reports n ids as having had one outcome. A zero count is skipped so a fully-hitting
// batch does not publish a "missing" series of zeroes.
func (s *Store) recordRecall(ctx context.Context, outcome string, n int) {
	if n <= 0 {
		return
	}

	tel.recalls.Add(ctx, int64(n), observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, outcome),
	))
}

func (s *Store) recordEvent(ctx context.Context, outcome string) {
	tel.events.Add(ctx, 1, observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, outcome),
	))
}
