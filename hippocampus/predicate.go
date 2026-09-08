package hippocampus

import (
	"context"
	"reflect"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// Deletion by predicate: DeleteMemoriesByFilter and DeleteEventsByFilter.
//
// The gap these close is offboarding. The deletion surface was DeleteMemories (by id), DeleteEvent
// (one event), Purge (everything, refused to a scoped caller) and Clear (exactly what a prior
// Export or Transfer captured) - so removing one group's records meant exporting it to an object
// store with clear set, or transferring it to a second live instance with clear set, or paging
// GetMemories and deleting by id while the store changed underneath, and then deleting the
// now-empty events one at a time because nothing deleted events by predicate at all. Group scoping
// is the documented soft-partition story for a tenant; "hard isolation is one instance per tenant"
// is an answer to isolation and not to deletion, and the deployments that took the soft partition
// still have to remove a partition eventually.
//
// The objection to predicate deletion, which item 74.6 raised in a related context, is that it
// invites deleting more than was meant. Four things answer it, and none of them is a warning in the
// documentation:
//
//   - AN EMPTY FILTER IS REFUSED. Not clamped, not treated as "everything" - refused, with the
//     reason named. Deleting everything is Purge, which is a separate RPC that a scoped caller
//     cannot reach at all, and no defaulted field should be able to arrive there by accident.
//     DeleteForgottenMemories set the precedent and it is the right one.
//   - THE LISTING IS THE DRY RUN, and provably the same predicate: both RPCs build their filter
//     with the function GetMemories/GetEvents build theirs with (see selection.go), from request
//     messages whose fields are named identically. So GetMemories with the same fields answers
//     "what would this delete", and its total_count answers "how much", at reader tier.
//     There is deliberately no dry_run FLAG - authorisation is per-RPC, so a flag on a destructive
//     call could never be tiered apart from it, which is exactly the reasoning that made
//     PreviewConsolidation its own RPC rather than a flag on Sleep.
//   - MAX_DELETIONS BOUNDS ONE CALL, and the response says whether the filter is now exhausted, so
//     a cautious operator can take a hundred rows, look at what happened, and send the same request
//     again.
//   - BOTH ARE ADMIN TIER, and both are scoped: a group-bound token deletes within its own
//     partition and can no more reach another group's records here than through any listing.
//
// The mechanism is a loop, not a DELETE ... WHERE. See db/predicate.go for why the storage layer
// refuses to offer the latter.

// deleteByFilterBatch is how many records one round of the loop selects and deletes. It is the
// chunk size the by-id delete already uses internally, so a batch is one transaction there rather
// than several, and it bounds the ids held in memory at any moment regardless of how much the
// filter matches.
const deleteByFilterBatch = 500

// DeleteMemoriesByFilter deletes every memory the filter matches, optionally removing the events
// its deletions leave empty.
//
// Each round selects the FIRST deleteByFilterBatch matching ids and deletes them, so the deletion
// of one batch is what makes the next selection return the next one. That is why there is no
// pagination here and no offset: a client-side page-and-delete loop has to keep an offset that the
// deletions themselves invalidate, which is the race this RPC exists to remove.
func (s *Server) DeleteMemoriesByFilter(
	ctx context.Context,
	in *contract.DeleteMemoriesByFilterRequest,
) (*contract.DeleteMemoriesByFilterResponse, error) {
	log.Debug("DeleteMemoriesByFilter()")

	ctx, span := tel.tracer.Start(ctx, "delete_memories_by_filter")
	defer span.End()

	res := &contract.DeleteMemoriesByFilterResponse{}

	filter, err := memorySelectionFilter(in)
	if err != nil {
		return res, err
	}

	if isZeroFilter(filter) {
		return res, status.Error(
			grpccodes.InvalidArgument,
			"deleting memories by filter requires at least one selecting field: an empty filter will not be read as 'delete everything' (that is Purge)",
		)
	}

	// The caller's group scope, conjoined with whatever they filtered by, exactly as the listing
	// applies it - so a bound caller drains their own partition and can reach no further.
	filter.Groups, _ = s.scopedGroups(ctx)

	var (
		deleted       int64
		eventsDeleted int64
		complete      bool
	)

	// One failure path, and it carries the running counts rather than zeroes. Rows deleted before
	// the failure are gone whether or not the call finished, and reporting nothing would leave the
	// caller believing an interrupted deletion had not started - the worst of the three possible
	// answers.
	fail := func(err error) (*contract.DeleteMemoriesByFilterResponse, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		res.MemoriesDeleted = deleted
		res.EventsDeleted = eventsDeleted

		return res, mapWriteError(mapError(err))
	}

	maxDeletions := in.GetMaxDeletions()

	for {
		batch := deleteByFilterBatch

		if maxDeletions > 0 {
			remaining := maxDeletions - deleted

			if remaining <= 0 {
				break
			}

			if remaining < int64(batch) {
				batch = int(remaining)
			}
		}

		filter.Limit = batch

		ids, err := s.db.MemoryIdsMatching(ctx, filter)
		if err != nil {
			return fail(err)
		}

		if len(ids) == 0 {
			complete = true

			break
		}

		// Read before the delete: a memory's event_id is only there while the memory is.
		var eventIds []string

		if in.GetDeleteEmptyEvents() {
			eventIds, err = s.db.EventIdsForMemories(ctx, ids)
			if err != nil {
				return fail(err)
			}
		}

		cnt, err := s.db.DeleteMemories(ctx, ids)

		deleted += int64(cnt)
		tel.memoriesDeleted.Add(ctx, int64(cnt))

		if err != nil {
			return fail(err)
		}

		s.searchIdx().DeleteMemories(ids)

		eventsDeleted += s.deleteEmptiedEvents(ctx, eventIds)

		// A round that selected rows and deleted none has made no progress, and looping on the same
		// selection forever is a far worse failure than stopping short. It takes a concurrent delete
		// of exactly this batch to reach here, so the honest answer is to report what went and leave
		// complete false - the same request re-sent picks up wherever the store now is.
		if cnt == 0 {
			break
		}
	}

	if deleted > 0 || eventsDeleted > 0 {
		s.listingCounts.reset()
	}

	log.Infof("DeleteMemoriesByFilter deleted %d memories and %d emptied events", deleted, eventsDeleted)

	span.AddEvent("memories_deleted_by_filter", trace.WithAttributes(
		attribute.Int64("memories_deleted", deleted),
		attribute.Int64("events_deleted", eventsDeleted),
		attribute.Bool("complete", complete),
	))

	res.MemoriesDeleted = deleted
	res.EventsDeleted = eventsDeleted
	res.Complete = complete

	return res, nil
}

// deleteEmptiedEvents removes each of the given events that the deletion just left with no
// memories, and returns how many actually went.
//
// DeleteEventIfEmpty rather than DeleteEvents, because these events were not selected by the
// caller: an event whose memories were only partly matched by the filter must survive, and asking
// for emptiness in the same statement as the delete is what makes that safe against a memory
// written into it while the loop is running. Best-effort per event, like Clear's own cleanup - a
// tidy-up failure is not a reason to fail a deletion that has already happened.
func (s *Server) deleteEmptiedEvents(ctx context.Context, eventIds []string) int64 {
	var deleted int64

	for _, id := range eventIds {
		gone, err := s.db.DeleteEventIfEmpty(ctx, id, db.CauseClient)
		if err != nil {
			log.Errorf("failed to delete emptied event '%s': %s", id, err.Error())

			continue
		}

		if gone {
			deleted++
			tel.eventsDeleted.Add(ctx, 1)
		}
	}

	return deleted
}

// DeleteEventsByFilter deletes every event the filter matches, and either deletes each event's
// memories with it or leaves them standing with no event.
//
// The memories are dealt with first and the event only afterwards, so a failure part way through
// leaves memories whose event is still there rather than memories pointing at an event that is
// not - a dangling event_id being the one state no consolidation pass can see through.
func (s *Server) DeleteEventsByFilter(
	ctx context.Context,
	in *contract.DeleteEventsByFilterRequest,
) (*contract.DeleteEventsByFilterResponse, error) {
	log.Debug("DeleteEventsByFilter()")

	ctx, span := tel.tracer.Start(ctx, "delete_events_by_filter")
	defer span.End()

	res := &contract.DeleteEventsByFilterResponse{}

	filter, err := eventSelectionFilter(in)
	if err != nil {
		return res, err
	}

	if isZeroFilter(filter) {
		return res, status.Error(
			grpccodes.InvalidArgument,
			"deleting events by filter requires at least one selecting field: an empty filter will not be read as 'delete every event' (delete_memories says what to do with what is selected, not what to select)",
		)
	}

	filter.Groups, _ = s.scopedGroups(ctx)

	var (
		deleted  int64
		memories int64
		orphaned int64
		complete bool
	)

	// See DeleteMemoriesByFilter for why the failure path carries the running counts.
	fail := func(err error) (*contract.DeleteEventsByFilterResponse, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		res.EventsDeleted = deleted
		res.MemoriesDeleted = memories
		res.MemoriesOrphaned = orphaned

		return res, mapWriteError(mapError(err))
	}

	maxDeletions := in.GetMaxDeletions()

	for {
		batch := deleteByFilterBatch

		if maxDeletions > 0 {
			remaining := maxDeletions - deleted

			if remaining <= 0 {
				break
			}

			if remaining < int64(batch) {
				batch = int(remaining)
			}
		}

		filter.Limit = batch

		ids, err := s.db.EventIdsMatching(ctx, filter)
		if err != nil {
			return fail(err)
		}

		if len(ids) == 0 {
			complete = true

			break
		}

		for _, id := range ids {
			moved, err := s.clearEventMemories(ctx, id, in.GetDeleteMemories())
			if err != nil {
				return fail(err)
			}

			if in.GetDeleteMemories() {
				memories += moved
			} else {
				orphaned += moved
			}
		}

		cnt, err := s.db.DeleteEvents(ctx, ids)

		deleted += int64(cnt)
		tel.eventsDeleted.Add(ctx, int64(cnt))

		if err != nil {
			return fail(err)
		}

		// No progress; see the memory loop for why this stops rather than spins.
		if cnt == 0 {
			break
		}
	}

	if deleted > 0 || memories > 0 || orphaned > 0 {
		s.listingCounts.reset()
	}

	log.Infof(
		"DeleteEventsByFilter deleted %d events, %d memories, orphaning %d",
		deleted, memories, orphaned,
	)

	span.AddEvent("events_deleted_by_filter", trace.WithAttributes(
		attribute.Int64("events_deleted", deleted),
		attribute.Int64("memories_deleted", memories),
		attribute.Int64("memories_orphaned", orphaned),
		attribute.Bool("complete", complete),
	))

	res.EventsDeleted = deleted
	res.MemoriesDeleted = memories
	res.MemoriesOrphaned = orphaned
	res.Complete = complete

	return res, nil
}

// clearEventMemories empties one event ahead of its deletion, either by deleting its memories or by
// clearing their event_id, and reports how many memories it moved. It is the same choice
// DeleteEvent makes, and it keeps the search index in step exactly as that handler does.
func (s *Server) clearEventMemories(ctx context.Context, eventId string, deleteMemories bool) (int64, error) {
	if deleteMemories {
		cnt, err := s.db.DeleteEventMemories(ctx, eventId)
		if err != nil {
			return 0, err
		}

		tel.memoriesDeleted.Add(ctx, int64(cnt))
		s.searchIdx().DeleteByEventId(eventId)

		return int64(cnt), nil
	}

	cnt, err := s.db.UnsetMemoriesEventId(ctx, eventId)
	if err != nil {
		return 0, err
	}

	s.searchIdx().SetEventId(eventId, "")

	return int64(cnt), nil
}

// isZeroFilter reports whether a built filter selects nothing at all - that is, whether it is
// still its own zero value.
//
// Asked of the BUILT filter rather than of the request's fields, and that is the point: the
// selection builders set nothing but selecting dimensions, so "no field was set" and "the filter
// equals its zero value" are the same statement. A selecting field added to the filter in future is
// therefore covered here without anybody remembering to extend a list of field checks - which is
// the failure this guards against, since a forgotten field would make an otherwise-empty request
// delete the whole store.
func isZeroFilter[T any](filter T) bool {
	var zero T

	return reflect.DeepEqual(filter, zero)
}
