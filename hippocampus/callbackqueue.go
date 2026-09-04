package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// The outbound callback queue, RPC side. The queue itself lives in db/callbacks.go and the
// dispatcher in callbacks.go; this is the pair of RPCs that inspect it and empty it.
//
// Three things decided here rather than there.
//
// Both are ADMIN and both are scopeUnbound, unlike the forgotten log's pair - and the difference is
// not a sensitivity judgement, it is that there is nothing to scope BY. A delivery batches every
// memory one delete chunk removed, and a chunk routinely spans groups, so a row carries no single
// group a predicate could select on. Splitting deliveries per group to make one possible would
// undo the batching the queue depends on to keep a ten-thousand-memory cycle from becoming ten
// thousand HTTP requests. So a group-scoped caller is refused, on the same footing as
// PreviewConsolidation: the queue is infrastructure, not records.
//
// The read never returns a payload. A queued delivery may carry memory bodies when
// callbacks.includeBodies is set, and this RPC exists so an operator can see a backlog - not so the
// queue becomes a second, unscoped way to read the store.
//
// The delete refuses an empty request, exactly as DeleteForgottenMemories does and for a sharper
// reason: emptying this queue does not discard a record of something, it discards the notification
// itself, and nothing else will ever send it.

// GetCallbackQueue reports what the callback queue is holding, oldest first.
//
// A store that is not recording returns an empty page with enabled false rather than an error, for
// the reason the forgotten log gives: an empty queue is ambiguous without the flag - everything has
// been delivered, or nothing is being queued at all.
func (s *Server) GetCallbackQueue(
	ctx context.Context,
	in *contract.GetCallbackQueueRequest,
) (*contract.GetCallbackQueueResponse, error) {
	log.Debug("GetCallbackQueue()")

	ctx, span := tel.tracer.Start(ctx, "get_callback_queue")
	defer span.End()

	if err := s.requireUnbound(ctx, "GetCallbackQueue"); err != nil {
		return nil, err
	}

	filter := db.CallbackQueueFilter{
		Kind:     callbackKindOf(in.GetKind()),
		AfterSeq: in.GetAfterSeq(),
		Limit:    int(in.GetLimit()),
	}

	deliveries, err := s.db.GetCallbackQueue(ctx, filter)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	depth, err := s.db.CallbackQueueDepth(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	oldest, err := s.db.OldestQueuedCallback(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	out := &contract.GetCallbackQueueResponse{
		Deliveries:     make([]*contract.QueuedCallback, 0, len(deliveries)),
		Depth:          depth,
		Enabled:        s.callbacksEnabled,
		OldestQueuedAt: oldest,
	}

	for _, delivery := range deliveries {
		out.Deliveries = append(out.Deliveries, &contract.QueuedCallback{
			Seq:           delivery.Seq,
			Kind:          callbackKinds[delivery.Kind],
			Cause:         deleteCauses[delivery.Cause],
			CycleId:       delivery.CycleId,
			Chunk:         int32(delivery.Chunk),
			Chunks:        int32(delivery.Chunks),
			ItemCount:     int32(delivery.ItemCount),
			QueuedAt:      delivery.QueuedAt,
			Attempts:      int32(delivery.Attempts),
			NextAttemptAt: delivery.NextAttemptAt,
		})
	}

	// Only on a full page, matching the forgotten log: a short page is the last one, so offering a
	// cursor would make a client fetch an empty page to discover it had finished.
	if len(deliveries) == db.CallbackQueueLimit(int(in.GetLimit())) {
		out.NextSeq = deliveries[len(deliveries)-1].Seq
	}

	span.AddEvent("callback_queue_read", trace.WithAttributes(
		attribute.Int("returned", len(deliveries)),
		attribute.Int64("depth", depth),
	))

	return out, nil
}

// DeleteCallbackQueue empties the callback queue, or the part of it queued before a cutoff.
//
// It requires one of before_time or all. The automatic caps stop applying the moment callbacks are
// turned off (see db.PruneCallbackQueue), which is why this exists - and what it discards is not a
// record of a notification but the notification itself, so it must be an explicit act rather than
// something a default-valued request can do.
func (s *Server) DeleteCallbackQueue(
	ctx context.Context,
	in *contract.DeleteCallbackQueueRequest,
) (*contract.DeleteCallbackQueueResponse, error) {
	log.Debug("DeleteCallbackQueue()")

	ctx, span := tel.tracer.Start(ctx, "delete_callback_queue")
	defer span.End()

	if err := s.requireUnbound(ctx, "DeleteCallbackQueue"); err != nil {
		return nil, err
	}

	if in.GetBeforeTime() <= 0 && !in.GetAll() {
		return nil, status.Error(
			grpccodes.InvalidArgument,
			"emptying the callback queue requires before_time or all: an empty request will not be read as 'discard every pending notification'",
		)
	}

	deleted, err := s.db.DeleteCallbackQueue(ctx, in.GetBeforeTime())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapWriteError(mapError(err))
	}

	if deleted > 0 {
		log.Warnf("discarded %d pending callbacks - these notifications will not be delivered", deleted)
	}

	span.AddEvent("callback_queue_deleted", trace.WithAttributes(attribute.Int64("deleted", deleted)))

	return &contract.DeleteCallbackQueueResponse{Deleted: deleted}, nil
}

// callbackKinds and deleteCauses project the storage layer's enums onto the wire's.
var (
	callbackKinds = map[db.CallbackKind]contract.CallbackKind{
		db.CallbackKindNone:            contract.CallbackKind_CALLBACK_KIND_UNSPECIFIED,
		db.CallbackKindMemoryForgotten: contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN,
		db.CallbackKindEventForgotten:  contract.CallbackKind_CALLBACK_KIND_EVENT_FORGOTTEN,
		db.CallbackKindSleepCompleted:  contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED,
	}

	deleteCauses = map[db.DeleteCause]contract.DeleteCause{
		db.CauseNone:           contract.DeleteCause_DELETE_CAUSE_UNSPECIFIED,
		db.CauseConsolidation:  contract.DeleteCause_DELETE_CAUSE_CONSOLIDATION,
		db.CauseEviction:       contract.DeleteCause_DELETE_CAUSE_EVICTION,
		db.CauseClient:         contract.DeleteCause_DELETE_CAUSE_CLIENT,
		db.CauseClear:          contract.DeleteCause_DELETE_CAUSE_CLEAR,
		db.CauseCascade:        contract.DeleteCause_DELETE_CAUSE_CASCADE,
		db.CauseSummaryReplace: contract.DeleteCause_DELETE_CAUSE_SUMMARY_REPLACE,
		db.CausePurge:          contract.DeleteCause_DELETE_CAUSE_PURGE,
	}
)

// callbackKindOf maps the wire enum onto the storage layer's kind - the inverse of callbackKinds,
// and used only as a filter, where UNSPECIFIED means "every kind" rather than "no kind".
func callbackKindOf(kind contract.CallbackKind) db.CallbackKind {
	switch kind {

	case contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN:
		return db.CallbackKindMemoryForgotten

	case contract.CallbackKind_CALLBACK_KIND_EVENT_FORGOTTEN:
		return db.CallbackKindEventForgotten

	case contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED:
		return db.CallbackKindSleepCompleted

	}

	return db.CallbackKindNone
}
