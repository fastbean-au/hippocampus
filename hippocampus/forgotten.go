package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// The forgotten log, RPC side. The record itself lives in db/tombstone.go, which is also where the
// design is written down; this is the pair of RPCs that read it and empty it.
//
// Two things decided here rather than there.
//
// The read is group-scoped by PREDICATE, like any other listing: a tombstone carries the group its
// memory had, so a bound caller sees their own partition's losses and nothing else. That is why
// neither RPC is scopeUnbound despite both being admin - unlike PreviewConsolidation, which
// enumerates the whole store by design, this answers about records that were already the caller's.
//
// The delete refuses an empty request. Every other field is optional, so "delete before nothing"
// would otherwise read as "delete everything", and the one operation that destroys the record of
// what was destroyed should not be reachable by omission.

// GetForgottenMemories reads one page of the forgotten log, newest first.
//
// A store that is not recording returns an empty page with enabled false rather than an error:
// "nothing was written down" is the true answer to what was forgotten, and a client showing the
// log needs to distinguish it from "nothing has been forgotten", which is what the flag is for.
func (s *Server) GetForgottenMemories(
	ctx context.Context,
	in *contract.GetForgottenMemoriesRequest,
) (*contract.GetForgottenMemoriesResponse, error) {
	log.Debug("GetForgottenMemories()")

	ctx, span := tel.tracer.Start(ctx, "get_forgotten_memories")
	defer span.End()

	groups, _ := s.scopedGroups(ctx)

	filter := db.ForgottenFilter{
		MemoryId: in.GetMemoryId(),
		EventId:  in.GetEventId(),
		Group:    in.GetGroup(),
		Rule:     forgetRuleOf(in.GetRule()),
		Since:    in.GetSince(),
		Until:    in.GetUntil(),
		AfterSeq: in.GetAfterSeq(),
		Limit:    int(in.GetLimit()),
		Groups:   groups,
	}

	// A caller asking for a group outside their scope gets an empty page rather than an error,
	// matching how every other filtered listing treats it: the predicate is the conjunction of what
	// they asked for and what they may see.
	forgotten, err := s.db.GetForgottenMemories(ctx, filter)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	total, err := s.db.CountForgottenMemories(ctx, groups)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	res := contract.GetForgottenMemoriesResponse{
		Memories: make([]*contract.ForgottenMemory, 0, len(forgotten)),
		Total:    total,
		Enabled:  s.consolidation.tombstones,
	}

	for _, f := range forgotten {
		res.Memories = append(res.Memories, &contract.ForgottenMemory{
			Seq:          f.Seq,
			Id:           f.Id,
			EventId:      f.EventId,
			Group:        f.Group,
			Significance: f.Significance,
			Value:        f.Value,
			Threshold:    f.Threshold,
			BodyBytes:    f.Bytes,
			Rule:         forgetRules[f.Rule],
			TimeStamp:    f.TimeStamp,
			TimeRecalled: f.TimeRecalled,
			RecallCount:  f.RecallCount,
			ForgottenAt:  f.ForgottenAt,
		})
	}

	// next_seq is set only on a full page. A short page is the end of the log, and reporting a
	// cursor there would have the client fetch one more empty page to discover that.
	if len(forgotten) == db.ForgottenLimit(int(in.GetLimit())) {
		res.NextSeq = forgotten[len(forgotten)-1].Seq
	}

	span.AddEvent("forgotten_log_read", trace.WithAttributes(
		attribute.Int("records", len(forgotten)),
		attribute.Int64("total", total),
	))

	return &res, nil
}

// DeleteForgottenMemories empties the forgotten log, or the part of it older than before_time.
//
// It requires one of before_time or all. The automatic caps never empty the log and stop applying
// the moment recording is turned off (see db.PruneTombstones), which is the whole reason this
// exists - so it must be an explicit act, not something a default-valued request can do.
func (s *Server) DeleteForgottenMemories(
	ctx context.Context,
	in *contract.DeleteForgottenMemoriesRequest,
) (*contract.DeleteForgottenMemoriesResponse, error) {
	log.Debug("DeleteForgottenMemories()")

	ctx, span := tel.tracer.Start(ctx, "delete_forgotten_memories")
	defer span.End()

	if in.GetBeforeTime() <= 0 && !in.GetAll() {
		return nil, status.Error(
			grpccodes.InvalidArgument,
			"emptying the forgotten log requires before_time or all: an empty request will not be read as 'delete everything'",
		)
	}

	groups, _ := s.scopedGroups(ctx)

	deleted, err := s.db.DeleteForgottenMemories(ctx, in.GetBeforeTime(), groups)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapWriteError(mapError(err))
	}

	log.Infof("deleted %d records from the forgotten log", deleted)

	tel.tombstonesDeleted.Add(ctx, deleted, metric.WithAttributes(attribute.Bool("manual", true)))
	span.AddEvent("forgotten_log_deleted", trace.WithAttributes(attribute.Int64("deleted", deleted)))

	return &contract.DeleteForgottenMemoriesResponse{Deleted: deleted}, nil
}

// forgetRuleOf maps the wire enum onto the storage layer's rule - the inverse of forgetRules, and
// used only as a filter, where UNSPECIFIED means "either rule" rather than "no rule".
func forgetRuleOf(rule contract.ForgetRule) db.ForgetRule {
	switch rule {

	case contract.ForgetRule_FORGET_RULE_CONSOLIDATION:
		return db.ForgetRuleConsolidation

	case contract.ForgetRule_FORGET_RULE_EVICTION:
		return db.ForgetRuleEviction

	}

	return db.ForgetRuleNone
}

// pruneTombstones applies the forgotten log's configured caps, at the end of every sleep cycle.
//
// It sits beside preserve() rather than inside it because the two compact different things: one
// returns freed pages to the filesystem, this bounds a table. Best-effort, like the summarisation
// scan - a log that could not be trimmed is a tidiness problem, not a reason to report the cycle
// as failed - and a no-op unless the log is both enabled and configured with a cap.
func (s *Server) pruneTombstones(ctx context.Context) {
	log.Debug("pruneTombstones()")

	if !s.consolidation.tombstones {
		return
	}

	ctx, span := tel.tracer.Start(ctx, "prune_tombstones")
	defer span.End()

	pruned, err := s.db.PruneTombstones(ctx)
	if err != nil {
		log.Warnf("failed to prune the forgotten log: %s", err.Error())
		span.RecordError(err)

		return
	}

	if pruned > 0 {
		log.Infof("pruned %d records from the forgotten log", pruned)

		tel.tombstonesDeleted.Add(ctx, pruned, metric.WithAttributes(attribute.Bool("manual", false)))
		span.AddEvent("forgotten_log_pruned", trace.WithAttributes(attribute.Int64("pruned", pruned)))
	}

	// The log's size after trimming, so a dashboard can see it flatten against its cap - or fail to.
	// Measured here rather than on every read because it is a count over the whole log, and this is
	// once per cycle; the whole store is counted (nil scope) because the gauge describes the store,
	// not a caller.
	held, err := s.db.CountForgottenMemories(ctx, nil)
	if err != nil {
		log.Warnf("failed to measure the forgotten log: %s", err.Error())

		return
	}

	tel.tombstones.Record(ctx, held)
}
