package hippocampus

import (
	"context"
	"errors"
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

	// An extremum names one tier for the whole deletion, resolved once here. found is false when
	// the rest of the filter selects nothing, so there is no tier to name and nothing to delete.
	filter, found, err := s.pinMemoryExtremum(ctx, filter)
	if err != nil {
		return fail(err)
	}

	complete = !found

	maxDeletions := in.GetMaxDeletions()

	for found {
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

	// See DeleteMemoriesByFilter: an extremum is resolved once, to one tier.
	filter, found, err := s.pinEventExtremum(ctx, filter)
	if err != nil {
		return fail(err)
	}

	complete = !found

	maxDeletions := in.GetMaxDeletions()

	for found {
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

// clearEventMemories empties one event as it is deleted, either by deleting its memories or by
// clearing their event_id, and reports how many memories it moved. DeleteEvent and
// DeleteEventsByFilter both go through it, and it keeps the search index in step.
//
// For a group-scoped caller it acts on the caller's memories only (TODO-3 item 139). An event in
// their scope can still hold another group's memory - an unscoped or multi-group writer can attach
// one - and deleting the caller's event must not delete that memory. The memory is detached instead,
// since a memory naming a deleted event is the one state no consolidation pass can see through. The
// count covers the caller's memories only, so it does not tell them the other one exists - which is
// also why this does not refuse instead: a refusal would say the same thing.
func (s *Server) clearEventMemories(ctx context.Context, eventId string, deleteMemories bool) (int64, error) {
	groups, bound := s.scopedGroups(ctx)

	if !bound {
		return s.clearAllEventMemories(ctx, eventId, deleteMemories)
	}

	// Read before anything moves: these are the ids the index has to be told about, and the count
	// the caller is given.
	inScope, err := s.db.MemoryIdsMatching(ctx, db.MemoryFilter{EventId: eventId, Groups: groups})
	if err != nil {
		return 0, err
	}

	var deleted int64

	if deleteMemories {
		cnt, err := s.db.DeleteEventMemories(ctx, eventId, groups)
		if err != nil {
			return 0, err
		}

		deleted = int64(cnt)

		tel.memoriesDeleted.Add(ctx, deleted)
		s.searchIdx().DeleteMemories(inScope)
	}

	// Whatever is left is either the caller's own (when not deleting) or another group's: detached
	// either way, because the event is going.
	if _, err := s.db.UnsetMemoriesEventId(ctx, eventId); err != nil {
		return deleted, err
	}

	s.searchIdx().SetEventId(eventId, "")

	if deleteMemories {
		return deleted, nil
	}

	return int64(len(inScope)), nil
}

// clearAllEventMemories is clearEventMemories for an unscoped caller: every memory of the event,
// whatever its group.
func (s *Server) clearAllEventMemories(ctx context.Context, eventId string, deleteMemories bool) (int64, error) {
	if deleteMemories {
		cnt, err := s.db.DeleteEventMemories(ctx, eventId, nil)
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

// pinMemoryExtremum resolves a filter's significance_extremum to the one significance it names
// now, and returns the filter selecting exactly that tier instead. A filter without an extremum is
// returned unchanged, with found true.
//
// This is what keeps the listing the dry run when an extremum is set. The extremum is a sub-select
// over the store as it stands, and the deletion loop selects again after every batch, so left
// unresolved each batch's deletion made the next tier the extremum and the loop ran on until
// nothing matched at all - the listing showing one tier and the deletion taking every memory the
// rest of the filter selected (TODO-3 item 137).
//
// The tier is read off one matching row rather than asked for as an aggregate, because the
// matching query already IS that aggregate: any row it returns carries the extremum. found is
// false when nothing matches. A row that vanishes between the two reads - a concurrent deletion of
// the one row probed - is reported as Aborted rather than retried, since the request can simply be
// sent again and will resolve against whatever the store then holds.
func (s *Server) pinMemoryExtremum(ctx context.Context, filter db.MemoryFilter) (db.MemoryFilter, bool, error) {
	if filter.SignificanceExtremum == db.SignificanceExtremumNone {
		return filter, true, nil
	}

	probe := filter
	probe.Limit = 1

	ids, err := s.db.MemoryIdsMatching(ctx, probe)
	if err != nil || len(ids) == 0 {
		return filter, false, err
	}

	memories, err := s.db.GetMemoriesByIds(ctx, ids)
	if err != nil {
		return filter, false, err
	}

	if len(*memories) == 0 {
		return filter, false, status.Error(grpccodes.Aborted, "the store changed while resolving significance_extremum; retry the request")
	}

	significance := (*memories)[0].Significance

	filter.SignificanceExtremum = db.SignificanceExtremumNone
	filter.SignificanceEquals = &significance

	return filter, true, nil
}

// pinEventExtremum is pinMemoryExtremum for events.
func (s *Server) pinEventExtremum(ctx context.Context, filter db.EventFilter) (db.EventFilter, bool, error) {
	if filter.SignificanceExtremum == db.SignificanceExtremumNone {
		return filter, true, nil
	}

	probe := filter
	probe.Limit = 1

	ids, err := s.db.EventIdsMatching(ctx, probe)
	if err != nil || len(ids) == 0 {
		return filter, false, err
	}

	event, err := s.db.GetEvent(ctx, ids[0])
	if err != nil {
		if errors.Is(err, db.ErrEventNotFound) {
			return filter, false, status.Error(grpccodes.Aborted, "the store changed while resolving significance_extremum; retry the request")
		}

		return filter, false, err
	}

	significance := event.Significance

	filter.SignificanceExtremum = db.SignificanceExtremumNone
	filter.SignificanceEquals = &significance

	return filter, true, nil
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
