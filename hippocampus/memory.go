package hippocampus

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// Page-size bounds for the GetMemories listing: an unset (0) limit selects the default, and
// anything larger than the cap is clamped so a single request can't pull the whole store.
const (
	defaultMemoryPageSize = 25
	maxMemoryPageSize     = 200
)

func (s *Server) StoreMemory(ctx context.Context, in *contract.Memory) (*contract.StoreMemoryResponse, error) {
	var res contract.StoreMemoryResponse

	memory := types.MemoryFromProto(in)

	if err := memory.ValidateInsert(s.maxMemoryBodyLength, false); err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, status.Error(codes.InvalidArgument, err.Error())
	}

	if in.GetSignificance() < s.minimumMemorySignificance {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "insignificant")))

		// Quietly forgotten, like a brain that does not retain the insignificant: no error, empty
		// id, and rejected set so the caller can tell this apart from a store that just returned no
		// id. See StoreMemoryResponse in the contract.
		res.Rejected = true

		return &res, nil
	}

	// A memory referencing an event that does not exist would be a dangling reference: no
	// consolidation pass could see it through its event, so reject it rather than create one.
	// Event-less memories (empty event_id) are unaffected.
	if memory.EventId != "" {
		exists, err := s.db.EventExists(ctx, memory.EventId)
		if err != nil {
			return &res, err
		}

		if !exists {
			tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

			return &res, status.Errorf(codes.FailedPrecondition, "event '%s' does not exist", memory.EventId)
		}
	}

	// A fresh memory is never pre-reinforced: discard any client-supplied recall state so a create
	// cannot arrive already boosted (recall_count) or with its decay clock pre-set (time_recalled),
	// which would make it unforgettable. Reinforcement happens only through RecallMemories. The
	// nested-memory path in StoreEvent routes through here too, so it is covered. Import/ImportBatch
	// deliberately carry recall history and do not pass through this handler.
	memory.TimeRecalled = 0
	memory.RecallCount = 0

	memory.SetDefaults()

	id, err := s.db.CreateMemory(ctx, memory)
	res.Id = id

	if err == nil {
		tel.memoriesStored.Add(ctx, 1)
		tel.memoryBodyBytes.Record(ctx, int64(len(memory.Body)))

		// Binary memories are never indexed - the body is opaque to content search.
		if !memory.IsBinary {
			s.searchIdx().IndexMemory(search.DocFromMemory(memory))
		}
	}

	return &res, mapWriteError(err)
}

// UpdateMemory applies a partial update to an existing memory: only the content fields carrying a
// value (significance, body, event_id, group, time_stamp) overwrite the stored row. is_binary and
// is_summary are not updatable here - they are set at creation and by ReplaceMemoriesWithSummary -
// so the caller must keep any updated content consistent with the memory's existing is_binary and
// is_summary flags: a non-binary memory is re-indexed for content search below, so its new body
// must stay valid text, and a summary memory should retain summary-appropriate content. An unknown
// id returns NotFound rather than creating a phantom memory (mirroring UpdateEvent).
func (s *Server) UpdateMemory(ctx context.Context, in *contract.Memory) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	if in.GetId() == "" {

		return &res, status.Error(codes.InvalidArgument, "id must be provided")
	}

	memory := types.MemoryFromProto(in)

	if err := memory.ValidateInsert(s.maxMemoryBodyLength, true); err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, status.Error(codes.InvalidArgument, err.Error())
	}

	// Re-pointing a memory at an event that does not exist would create a dangling reference; reject
	// it rather than let the update produce an immortal memory. A partial update
	// that leaves event_id unset (empty) does not touch the memory's event, so it is unaffected.
	if memory.EventId != "" {
		exists, err := s.db.EventExists(ctx, memory.EventId)
		if err != nil {
			return &res, err
		}

		if !exists {

			return &res, status.Errorf(codes.FailedPrecondition, "event '%s' does not exist", memory.EventId)
		}
	}

	ok, err := s.db.UpdateMemory(ctx, memory)
	if err != nil {

		return &res, mapWriteError(err)
	}

	if !ok {

		return &res, status.Errorf(codes.NotFound, "memory '%s' not found", in.GetId())
	}

	// Re-index from the memory's full current state (the update was partial, so the request alone
	// does not carry it). Binary memories are never indexed; the stored is_binary flag - which this
	// RPC does not change - decides, so the caller's content must match it.
	if updated, err := s.db.GetMemoriesByIds(ctx, []string{in.GetId()}); err == nil && len(*updated) == 1 {
		if m := (*updated)[0]; !m.IsBinary {
			s.searchIdx().IndexMemory(search.DocFromMemory(m))
		}
	}

	res.Ok = true

	return &res, nil
}

func (s *Server) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	// TODO: list of ids should be made unique

	ids := in.GetIds()

	if len(ids) == 0 {
		return &res, nil
	}

	cnt, err := s.db.DeleteMemories(ctx, ids)

	tel.memoriesDeleted.Add(ctx, int64(cnt))

	if err == nil {
		s.searchIdx().DeleteMemories(ids)
	}

	if cnt == len(ids) {
		res.Ok = true
	}

	return &res, mapWriteError(err)
}

// RecallMemories returns the requested memories and reinforces each one: its recall time is set
// to now (resetting its decay clock) and its recall count is incremented (raising its effective
// significance during consolidation).
func (s *Server) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest) (*contract.GetMemoriesResponse, error) {
	var res contract.GetMemoriesResponse

	ids := in.GetIds()

	if len(ids) == 0 {
		return &res, nil
	}

	memories, err := s.db.RecallMemories(ctx, ids)
	if err != nil {
		return &res, err
	}

	tel.memoriesRecalled.Add(ctx, int64(len(*memories)))

	ms := make([]*contract.Memory, len(*memories))
	for i, m := range *memories {
		ms[i] = m.ToProto()
	}
	res.Memories = ms

	return &res, nil
}

// ReplaceMemoriesWithSummary deletes every memory associated with an event and replaces them
// with a single caller-supplied summary memory, in one transaction. The service has no
// visibility into memory content (bodies are opaque), so it cannot generate the summary itself —
// the caller must supply it, typically after reviewing the event via GetSummarizationCandidates
// or GetEventById. The summary is validated and checked against the minimum significance before
// anything is deleted, so a rejected summary leaves the original memories untouched.
func (s *Server) ReplaceMemoriesWithSummary(ctx context.Context, in *contract.ReplaceMemoriesWithSummaryRequest) (*contract.ReplaceMemoriesWithSummaryResponse, error) {
	var res contract.ReplaceMemoriesWithSummaryResponse

	eventId := in.GetEventId()
	if eventId == "" {
		return &res, fmt.Errorf("event_id must be provided")
	}

	if _, err := s.db.GetEvent(ctx, eventId); err != nil {
		if errors.Is(err, db.ErrEventNotFound) {

			return &res, status.Errorf(codes.NotFound, "event '%s' not found", eventId)
		}

		return &res, err
	}

	summary := types.MemoryFromProto(in.GetSummary())
	summary.EventId = eventId
	summary.IsSummary = true

	// The summary is a fresh memory: like StoreMemory it must not inherit client-supplied recall
	// state, or it would start already reinforced.
	summary.TimeRecalled = 0
	summary.RecallCount = 0

	if err := summary.ValidateInsert(s.maxMemoryBodyLength, false); err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, status.Error(codes.InvalidArgument, err.Error())
	}

	if summary.Significance < s.minimumMemorySignificance {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "insignificant")))

		return &res, fmt.Errorf("summary significance below minimum")
	}

	summary.SetDefaults()

	replaced, err := s.db.ReplaceMemoriesWithSummary(ctx, eventId, summary)
	if err != nil {
		return &res, err
	}

	res.Id = summary.Id
	res.MemoriesReplaced = int32(replaced)

	tel.memoriesSummarized.Add(ctx, int64(replaced))
	tel.summariesCreated.Add(ctx, 1)

	// The single FIFO worker guarantees the event-scoped delete lands before the summary's
	// index write, so the replaced memories cannot outlive the summary in the index.
	s.searchIdx().DeleteByEventId(eventId)
	s.searchIdx().IndexMemory(search.DocFromMemory(summary))

	return &res, nil
}

// GetSummarizationCandidates returns the events identified by the most recent sleep cycle as
// having accumulated enough quiet, unsummarized memories to be worth condensing via
// ReplaceMemoriesWithSummary. The list is a point-in-time snapshot: it is only refreshed when
// consolidation.summarizationMinMemories is configured, and may include events that have since
// changed.
func (s *Server) GetSummarizationCandidates(ctx context.Context, in *contract.EmptyRequest) (*contract.GetSummarizationCandidatesResponse, error) {
	var res contract.GetSummarizationCandidatesResponse

	s.summarizationCandidatesMu.RLock()
	defer s.summarizationCandidatesMu.RUnlock()

	cs := make([]*contract.SummarizationCandidate, len(s.summarizationCandidates))
	for i, c := range s.summarizationCandidates {
		cs[i] = &contract.SummarizationCandidate{
			EventId:     c.EventId,
			EventName:   c.EventName,
			MemoryCount: int32(c.MemoryCount),
		}
	}
	res.Candidates = cs

	return &res, nil
}

func (s *Server) GetMemories(ctx context.Context, in *contract.GetMemoriesRequest) (*contract.GetMemoriesResponse, error) {
	var res contract.GetMemoriesResponse

	// Validate request
	if in.GetSignificanceMax() > 0 && in.GetSignificanceMin() > 0 && in.GetSignificanceMax() < in.GetSignificanceMin() {
		return &res, fmt.Errorf("SignificanceMax must be greater than or equal to SignificanceMin")
	}

	if in.GetTimestampMax() > 0 && in.GetTimestampMin() > 0 && in.GetTimestampMax() < in.GetTimestampMin() {
		return &res, fmt.Errorf("TimestampMax must be greater than or equal to TimestampMin")
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
		limit = defaultMemoryPageSize
	}

	if limit > maxMemoryPageSize {
		limit = maxMemoryPageSize
	}

	offset := int(in.GetOffset())

	if offset < 0 {
		offset = 0
	}

	filter := db.MemoryFilter{
		TimeStampMin:    in.GetTimestampMin(),
		TimeStampMax:    in.GetTimestampMax(),
		SignificanceMin: in.GetSignificanceMin(),
		SignificanceMax: in.GetSignificanceMax(),
		Group:           in.GetGroup(),
		OrderBy:         orderBy,
		Limit:           limit,
		Offset:          offset,
	}

	total, err := s.db.CountMemoriesFiltered(ctx, filter)
	if err != nil {
		return &res, err
	}

	memories, err := s.db.GetMemories(ctx, filter)
	if err != nil {
		return &res, err
	}

	ms := make([]*contract.Memory, len(*memories))
	for i, m := range *memories {
		ms[i] = m.ToProto()
	}
	res.Memories = ms
	res.TotalCount = int32(total)

	return &res, nil
}
