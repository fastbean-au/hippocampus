package hippocampus

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// Page-size bounds for the GetEvents listing: an unset (0) limit selects the default, and anything
// larger than the cap is clamped so a single request can't pull the whole store.
const (
	defaultEventPageSize = 25
	maxEventPageSize     = 200
)

func (s *Server) StoreEvent(ctx context.Context, in *contract.Event) (*contract.StoreEventResponse, error) {
	var res contract.StoreEventResponse

	event := types.EventFromProto(in)

	// Defaults before validation so a zero time_start defaults to now rather than failing
	// validation (time_start is optional on create, like Memory's time_stamp).
	event.SetDefaults()

	if err := event.Validate(false); err != nil {
		tel.eventsRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, mapError(err)
	}

	// The minimum-significance gate applies only to an absolute positive significance: an unranked
	// create (significance 0) is a deliberate "rank it later", and a placement is a deliberate
	// relative ranking - neither is the insignificant-write the gate drops.
	if !hasPlacement(in.GetPlacement()) && in.GetSignificance() > 0 && in.GetSignificance() < s.minimumEventSignificance {
		tel.eventsRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "insignificant")))

		// Quietly forgotten, like a brain that does not retain the insignificant: no error, empty
		// id, no nested memories stored, and rejected set so the caller can tell this apart from a
		// store that just returned no id. See StoreEventResponse in the contract.
		res.Rejected = true

		return &res, nil
	}

	// Resolve the requested significance/placement to a registry level before the event is created.
	if err := s.resolveEventSignificance(ctx, in.GetSignificance(), in.GetPlacement(), &event); err != nil {
		if errors.Is(err, db.ErrInvalidPlacement) {
			tel.eventsRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

			return &res, status.Error(codes.InvalidArgument, err.Error())
		}

		return &res, mapError(err)
	}

	id, err := s.db.CreateEvent(ctx, event)
	if err != nil {
		return &res, mapError(err)
	}
	res.Id = id

	tel.eventsStored.Add(ctx, 1)

	// Nested memories are best-effort: the event is already committed, so a nested memory that
	// fails validation, is dropped for insignificance, or hits a store error cannot roll it back.
	// memory_count reports how many were actually retained. A real store error is logged here - the
	// nested StoreMemory calls bypass the gRPC interceptor chain, so a failure would otherwise be
	// entirely silent - but it does not fail the event create. See StoreEventResponse in the
	// contract.
	if in.Memories != nil {
		c := 0
		for _, m := range in.Memories {
			if m.EventId == "" {
				m.EventId = id
			}

			mres, err := s.StoreMemory(ctx, m)
			if err != nil {
				log.Warnf("StoreEvent: nested memory for event '%s' failed: %s", id, err.Error())

				continue
			}

			if mres.GetRejected() || mres.GetId() == "" {

				continue
			}

			c++
		}
		res.MemoryCount = int32(c)
	}

	return &res, nil
}

func (s *Server) EndEvent(ctx context.Context, in *contract.EndEventRequest) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	if in.GetId() == "" {

		return &res, status.Error(codes.InvalidArgument, "id must be provided")
	}

	t := in.GetTimeEnd()
	if t == 0 {
		t = time.Now().UnixNano()
	}

	e := types.Event{
		Id:      in.GetId(),
		TimeEnd: t,
	}

	ok, err := s.db.UpdateEvent(ctx, e)
	if err != nil {

		return &res, mapError(err)
	}

	if !ok {

		return &res, status.Errorf(codes.NotFound, "event '%s' not found", in.GetId())
	}

	res.Ok = true

	return &res, nil
}

func (s *Server) UpdateEventSignificance(ctx context.Context, in *contract.UpdateEventSignificanceRequest) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	if in.GetId() == "" {

		return &res, status.Error(codes.InvalidArgument, "id must be provided")
	}

	e := types.Event{
		Id:           in.GetId(),
		Significance: in.GetSignificance(),
	}

	// Resolve the requested significance/placement to a registry level; a partial update with
	// neither leaves the event's significance unchanged.
	if err := s.resolveEventSignificance(ctx, in.GetSignificance(), in.GetPlacement(), &e); err != nil {
		if errors.Is(err, db.ErrInvalidPlacement) {

			return &res, status.Error(codes.InvalidArgument, err.Error())
		}

		return &res, mapError(err)
	}

	ok, err := s.db.UpdateEvent(ctx, e)
	if err != nil {

		return &res, mapError(err)
	}

	if !ok {

		return &res, status.Errorf(codes.NotFound, "event '%s' not found", in.GetId())
	}

	res.Ok = true

	return &res, nil
}

func (s *Server) MergeEvents(ctx context.Context, in *contract.MergeEventsRequest) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	tid := in.GetMergeTo()
	fid := in.GetMergeFrom()

	if tid == "" || fid == "" {
		return &res, fmt.Errorf("both 'merge_from' and 'merge_to' must be provided")
	}

	// The merge re-points merge_from's memories at merge_to. If merge_to does not exist, every one
	// of those memories becomes a dangling reference in a single call, so verify it first.
	// merge_from need not exist - an absent one simply matches no memories, and any
	// memories still pointing at it are healed onto the real merge_to.
	exists, err := s.db.EventExists(ctx, tid)
	if err != nil {
		return &res, mapError(err)
	}

	if !exists {

		return &res, status.Errorf(codes.FailedPrecondition, "merge_to event '%s' does not exist", tid)
	}

	err = s.db.MergeEventMemories(ctx, tid, fid)

	if err == nil {
		tel.eventsMerged.Add(ctx, 1)
		s.searchIdx().SetEventId(fid, tid)
		res.Ok = true
	}

	return &res, mapError(err)
}

func (s *Server) DeleteEvent(ctx context.Context, in *contract.DeleteEventRequest) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	eid := in.GetId()

	// An empty id must be rejected before it reaches the store: with memories: true it would run
	// DELETE FROM memories WHERE event_id = '', deleting every memory not associated with any event
	// (and mirroring that wipe into the search index).
	if eid == "" {

		return &res, status.Error(codes.InvalidArgument, "id must be provided")
	}

	deleted, err := s.db.DeleteEvent(ctx, eid)
	if err != nil {
		return &res, mapError(err)
	}

	// An unknown id deletes nothing; report NotFound rather than success, matching EndEvent and
	// UpdateEventSignificance. The memory cleanup below is skipped for a nonexistent event.
	if !deleted {

		return &res, status.Errorf(codes.NotFound, "event '%s' not found", eid)
	}

	tel.eventsDeleted.Add(ctx, 1)

	if in.GetMemories() {
		cnt, err := s.db.DeleteEventMemories(ctx, eid)
		if err != nil {
			return &res, mapError(err)
		}

		tel.memoriesDeleted.Add(ctx, int64(cnt))
		s.searchIdx().DeleteByEventId(eid)
	} else {
		if _, err := s.db.UnsetMemoriesEventId(ctx, eid); err != nil {

			return &res, mapError(err)
		}

		s.searchIdx().SetEventId(eid, "")
	}

	res.Ok = true

	return &res, nil
}

func (s *Server) GetEventById(ctx context.Context, in *contract.GetEventByIdRequest) (*contract.GetEventResponse, error) {
	var res contract.GetEventResponse

	eid := in.GetId()

	event, err := s.db.GetEvent(ctx, eid)
	if err != nil {
		if errors.Is(err, db.ErrEventNotFound) {

			return &res, status.Errorf(codes.NotFound, "event '%s' not found", eid)
		}

		return &res, mapError(err)
	}
	res.Event = event.ToProto()

	if in.GetMemories() {
		memories, err := s.db.GetMemoriesByEventId(ctx, eid)
		if err != nil {
			return &res, mapError(err)
		}

		ms := make([]*contract.Memory, len(*memories))
		for i, m := range *memories {
			ms[i] = m.ToProto()
		}
		res.Event.Memories = ms
	}

	return &res, nil
}

func (s *Server) GetEvents(ctx context.Context, in *contract.GetEventsRequest) (*contract.GetEventsResponse, error) {
	var res contract.GetEventsResponse

	// Validate request
	if in.GetSignificanceMax() > 0 && in.GetSignificanceMin() > 0 && in.GetSignificanceMax() < in.GetSignificanceMin() {
		return &res, fmt.Errorf("SignificanceMax must be greater than or equal to SignificanceMin")
	}

	if in.GetTimeStartMax() > 0 && in.GetTimeStartMin() > 0 && in.GetTimeStartMax() < in.GetTimeStartMin() {
		return &res, fmt.Errorf("TimeStartMax must be greater than or equal to TimeStartMin")
	}

	if in.GetTimeEndMax() > 0 && in.GetTimeEndMin() > 0 && in.GetTimeEndMax() < in.GetTimeEndMin() {
		return &res, fmt.Errorf("TimeEndMax must be greater than or equal to TimeEndMin")
	}

	if in.GetTimeStartMin() > 0 && in.GetTimeEndMin() > 0 && in.GetTimeEndMin() < in.GetTimeStartMin() {
		return &res, fmt.Errorf("TimeEndMin must be greater than or equal to TimeStartMin")
	}

	if in.GetTimeStartMax() > 0 && in.GetTimeEndMax() > 0 && in.GetTimeEndMax() < in.GetTimeStartMax() {
		return &res, fmt.Errorf("TimeEndMax must be greater than or equal to TimeStartMax")
	}

	extremum := db.SignificanceExtremumNone

	switch in.GetSignificanceExtremum() {

	case contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST:
		extremum = db.SignificanceExtremumHighest

	case contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST:
		extremum = db.SignificanceExtremumLowest

	}

	if extremum != db.SignificanceExtremumNone && (in.GetSignificanceMin() > 0 || in.GetSignificanceMax() > 0) {
		return &res, fmt.Errorf("SignificanceExtremum cannot be combined with SignificanceMin/SignificanceMax")
	}

	orderBy := in.GetOrderBy()

	switch orderBy {

	case "", "significance", "timestamp":
		// supported

	default:
		return &res, fmt.Errorf("OrderBy must be \"significance\" or \"timestamp\"")

	}

	limit := int(in.GetLimit())

	if limit <= 0 {
		limit = defaultEventPageSize
	}

	if limit > maxEventPageSize {
		limit = maxEventPageSize
	}

	offset := int(in.GetOffset())

	if offset < 0 {
		offset = 0
	}

	filter := db.EventFilter{
		TimeStartMin:         in.GetTimeStartMin(),
		TimeStartMax:         in.GetTimeStartMax(),
		TimeEndMin:           in.GetTimeEndMin(),
		TimeEndMax:           in.GetTimeEndMax(),
		SignificanceMin:      in.GetSignificanceMin(),
		SignificanceMax:      in.GetSignificanceMax(),
		SignificanceExtremum: extremum,
		Group:                in.GetGroup(),
		OrderBy:              orderBy,
		Limit:                limit,
		Offset:               offset,
	}

	total, err := s.db.CountEventsFiltered(ctx, filter)
	if err != nil {
		return &res, mapError(err)
	}

	events, err := s.db.GetEvents(ctx, filter)
	if err != nil {
		return &res, mapError(err)
	}

	es := make([]*contract.Event, len(*events))
	for i, e := range *events {
		es[i] = e.ToProto()
	}

	// Attach memories in a single batched query rather than one GetMemoriesByEventId per event (an
	// N+1 that was up to 200 extra queries per page, serialised on SQLite's single connection).
	// Group the result back onto its event by event_id.
	if in.GetMemories() && len(*events) > 0 {
		eventIds := make([]string, len(*events))
		indexByEventId := make(map[string]int, len(*events))

		for i, e := range *events {
			eventIds[i] = e.Id
			indexByEventId[e.Id] = i
		}

		memories, err := s.db.GetMemoriesByEventIds(ctx, eventIds)
		if err != nil {
			return &res, mapError(err)
		}

		for _, m := range *memories {
			if i, ok := indexByEventId[m.EventId]; ok {
				es[i].Memories = append(es[i].Memories, m.ToProto())
			}
		}
	}

	res.Events = es
	res.TotalCount = int32(total)

	return &res, nil
}
