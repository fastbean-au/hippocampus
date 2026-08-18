package hippocampus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// Page-size bounds for the GetMemories listing: an unset (0) limit selects the default, and
// anything larger than the cap is clamped so a single request can't pull the whole store.
const (
	defaultMemoryPageSize = 25
	maxMemoryPageSize     = 200
)

// triState maps the contract's tri-state boolean onto the db package's equivalent, for the list
// filters over boolean columns. UNSPECIFIED means no restriction rather than false.
func triState(in contract.Bool) db.TriState {
	switch in {

	case contract.Bool_TRUE:
		return db.TriStateTrue

	case contract.Bool_FALSE:
		return db.TriStateFalse

	default:
		return db.TriStateUnset

	}
}

// sortDirection maps the contract's sort direction onto the db package's equivalent, for the
// GetMemories/GetEvents listings. UNSPECIFIED means the order_by field's natural direction rather
// than ascending - see db/order.go for which that is per field.
func sortDirection(in contract.SortDirection) db.SortDirection {
	switch in {

	case contract.SortDirection_SORT_DIRECTION_ASC:
		return db.SortDirectionAscending

	case contract.SortDirection_SORT_DIRECTION_DESC:
		return db.SortDirectionDescending

	default:
		return db.SortDirectionNatural

	}
}

func (s *Server) StoreMemory(ctx context.Context, in *contract.Memory) (*contract.StoreMemoryResponse, error) {
	var res contract.StoreMemoryResponse

	memory := types.MemoryFromProto(in)

	// NOTE: memory.Metadata is in scope from here on, and must never reach a metric attribute, a
	// span attribute, or a log field. It is client-supplied and unbounded in cardinality - one
	// caller tagging memories with a request id would mint a new time series per write. Every
	// attribute in this package is a bool or a small closed enum (see hippocampus/telemetry.go), and
	// the "reason" values below are fixed literals for exactly that reason. Filter on metadata; do
	// not measure by it.
	if err := memory.ValidateInsert(s.maxMemoryBodyLength, false); err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, status.Error(codes.InvalidArgument, err.Error())
	}

	// The minimum-significance gate applies only to an absolute positive significance: an unranked
	// create (significance 0) is a deliberate "rank it later", and a placement is a deliberate
	// relative ranking - neither is the insignificant-write the gate drops.
	if !hasPlacement(in.GetPlacement()) && in.GetSignificance() > 0 && in.GetSignificance() < s.minimumMemorySignificance {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "insignificant")))

		// Quietly forgotten, like a brain that does not retain the insignificant: no error, empty
		// id, and rejected set so the caller can tell this apart from a store that just returned no
		// id. See StoreMemoryResponse in the contract.
		res.Rejected = true

		return &res, nil
	}

	// Stamp the group the caller's scope permits. For a scoped caller naming nothing this fills in
	// their sole group, so a bound writer never creates a record it cannot then read back.
	group, err := s.writeGroup(ctx, memory.Group)
	if err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, err
	}

	memory.Group = group

	// A memory referencing an event that does not exist would be a dangling reference: no
	// consolidation pass could see it through its event, so reject it rather than create one.
	// Event-less memories (empty event_id) are unaffected.
	if memory.EventId != "" {
		// Checked before existence, so an event outside the caller's scope reports NotFound rather
		// than the FailedPrecondition below - which would otherwise distinguish "no such event" from
		// "an event you cannot see", and so confirm the latter exists.
		if err := s.scopeEventIds(ctx, []string{memory.EventId}); err != nil {
			tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

			return &res, err
		}

		exists, err := s.db.EventExists(ctx, memory.EventId)
		if err != nil {
			return &res, mapError(err)
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

	// Resolve the requested significance/placement to a registry level, stamping the level id (for
	// storage) and the resolved rank (for the search index) onto the memory.
	if err := s.resolveMemorySignificance(ctx, in, &memory); err != nil {
		if errors.Is(err, db.ErrInvalidPlacement) {
			tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

			return &res, status.Error(codes.InvalidArgument, err.Error())
		}

		return &res, mapError(err)
	}

	memory.SetDefaults()

	// Links are checked before the memory is written, so a create naming a target that does not
	// exist fails without leaving the memory behind. They are written after, because the near end
	// has to exist first.
	if err := s.checkLinkTargets(ctx, s.memoryLinks(), memory.Id, memory.Links); err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return &res, err
	}

	id, err := s.db.CreateMemory(ctx, memory)
	res.Id = id

	if err == nil {
		tel.memoriesStored.Add(ctx, 1)
		tel.memoryBodyBytes.Record(ctx, int64(len(memory.Body)))

		s.storeLinks(ctx, s.memoryLinks(), memory.Id, memory.Links)

		// Binary memories are never indexed - the body is opaque to content search.
		if !memory.IsBinary {
			s.indexMemory(ctx, memory)
		}
	}

	return &res, mapError(err)
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

	// The memory being updated must be in the caller's scope.
	if err := s.scopeMemoryIds(ctx, []string{in.GetId()}); err != nil {
		return &res, err
	}

	// A group on an update MOVES the memory, so it is checked against the scope rather than
	// defaulted: a scoped caller may re-file a record within its own partition but must not be able
	// to push one out of it, which would delete the record as far as they could ever tell and hand
	// it to whoever holds the destination group. An update naming no group leaves it untouched,
	// which is why writeGroup's "stamp the sole group" case is deliberately not used here - it would
	// silently re-file every memory a multi-group caller touched.
	//
	// ClearGroup is refused for a scoped caller for the same reason: it moves the record to the
	// unnamed group, which is outside every scope.
	if groups, bound := s.scopedGroups(ctx); bound {
		if memory.Group != "" && !auth.GroupInScope(groups, memory.Group) {
			return &res, status.Errorf(codes.PermissionDenied, "group %q is outside this token's scope", memory.Group)
		}

		if memory.ClearGroup {
			return &res, status.Error(codes.PermissionDenied, "a group-scoped token cannot clear a record's group, which would move it outside its own scope")
		}
	}

	// Re-pointing a memory at an event that does not exist would create a dangling reference; reject
	// it rather than let the update produce an immortal memory. A partial update
	// that leaves event_id unset (empty) does not touch the memory's event, so it is unaffected.
	if memory.EventId != "" {
		if err := s.scopeEventIds(ctx, []string{memory.EventId}); err != nil {
			return &res, err
		}

		exists, err := s.db.EventExists(ctx, memory.EventId)
		if err != nil {
			return &res, mapError(err)
		}

		if !exists {
			return &res, status.Errorf(codes.FailedPrecondition, "event '%s' does not exist", memory.EventId)
		}
	}

	// A placement is resolved to a level id here; an absolute significance is resolved by the store
	// (UpdateMemory), and an absent one leaves significance unchanged.
	if err := s.resolveMemorySignificance(ctx, in, &memory); err != nil {
		if errors.Is(err, db.ErrInvalidPlacement) {
			return &res, status.Error(codes.InvalidArgument, err.Error())
		}

		return &res, mapError(err)
	}

	ok, err := s.db.UpdateMemory(ctx, memory)
	if err != nil {
		return &res, mapError(err)
	}

	if !ok {
		return &res, status.Errorf(codes.NotFound, "memory '%s' not found", in.GetId())
	}

	// Re-index from the memory's full current state (the update was partial, so the request alone
	// does not carry it). Binary memories are never indexed; the stored is_binary flag - which this
	// RPC does not change - decides, so the caller's content must match it.
	if updated, err := s.db.GetMemoriesByIds(ctx, []string{in.GetId()}); err == nil && len(*updated) == 1 {
		if m := (*updated)[0]; !m.IsBinary {
			s.indexMemory(ctx, m)
		}
	}

	res.Ok = true

	return &res, nil
}

func (s *Server) DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest) (*contract.GeneralResponse, error) {
	var res contract.GeneralResponse

	// Collapse duplicate ids before both the delete and the success check: the store deletes each
	// row once, so a repeated id would make cnt < len(ids) and report Ok: false despite everything
	// asked for being gone.
	ids := db.DedupeIds(in.GetIds())

	if len(ids) == 0 {
		return &res, nil
	}

	if err := s.scopeMemoryIds(ctx, ids); err != nil {
		return &res, err
	}

	cnt, err := s.db.DeleteMemories(ctx, ids)

	tel.memoriesDeleted.Add(ctx, int64(cnt))

	if err == nil {
		s.searchIdx().DeleteMemories(ids)
	}

	if cnt == len(ids) {
		res.Ok = true
	}

	return &res, mapError(err)
}

// RecallMemories returns the requested memories and, for callers permitted to reinforce, resets
// each memory's decay clock and increments its recall count (raising its effective significance
// during consolidation). A reader-tier caller for whom reinforcement is disabled
// (auth.readerRecallReinforces) instead gets a plain, non-reinforcing read - recall stays available
// to read-only users without the write side effect. See mayReinforce.
func (s *Server) RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest) (*contract.GetMemoriesResponse, error) {
	var res contract.GetMemoriesResponse

	ids := in.GetIds()

	if len(ids) == 0 {
		return &res, nil
	}

	// Checked before the recall, not after: recall is a WRITE (it resets the decay clock and raises
	// the recall count), so filtering out-of-scope memories from the response would leave the caller
	// having reinforced records they may not see - a memory another group could then never forget.
	if err := s.scopeMemoryIds(ctx, ids); err != nil {
		return &res, err
	}

	reinforce := s.mayReinforce(ctx)

	var memories *[]types.Memory
	var err error

	if reinforce {
		memories, err = s.db.RecallMemories(ctx, ids)
	} else {
		log.Trace("recall reinforcement suppressed for reader role")

		memories, err = s.db.GetMemoriesByIds(ctx, ids)
	}

	if err != nil {
		return &res, mapError(err)
	}

	if reinforce {
		tel.memoriesRecalled.Add(ctx, int64(len(*memories)))

		// Spreading activation: what the recalled memories are associated with is pulled back from
		// the threshold a little too. Only on the reinforcing path - a read that deliberately did
		// not reset the caller's own decay clock must not reset anyone else's.
		s.reinforceLinked(ctx, ids)
	}

	recalled := *memories

	// Associative recall: what these memories remind the store of. Appended after the memories
	// actually asked for, and never reinforced by being returned - see linkedMemories.
	if in.GetIncludeLinked() {
		recalled = append(recalled, s.linkedMemories(ctx, ids)...)
	}

	ms := make([]*contract.Memory, len(recalled))
	for i, m := range recalled {
		ms[i] = m.ToProto()
	}
	res.Memories = ms

	return &res, nil
}

// ReplaceMemoriesWithSummary deletes every memory associated with an event and replaces them
// with a single caller-supplied summary memory, in one transaction. The service has no
// visibility into memory content (bodies are opaque), so it cannot generate the summary itself —
// the caller must supply it, typically after reviewing the event via GetSummarisationCandidates
// or GetEventById. The summary is validated and checked against the minimum significance before
// anything is deleted, so a rejected summary leaves the original memories untouched.
func (s *Server) ReplaceMemoriesWithSummary(ctx context.Context, in *contract.ReplaceMemoriesWithSummaryRequest) (*contract.ReplaceMemoriesWithSummaryResponse, error) {
	var res contract.ReplaceMemoriesWithSummaryResponse

	eventId := in.GetEventId()
	if eventId == "" {
		return &res, fmt.Errorf("event_id must be provided")
	}

	if err := s.scopeEventIds(ctx, []string{eventId}); err != nil {
		return &res, err
	}

	if _, err := s.db.GetEvent(ctx, eventId); err != nil {
		if errors.Is(err, db.ErrEventNotFound) {
			return &res, status.Errorf(codes.NotFound, "event '%s' not found", eventId)
		}

		return &res, mapError(err)
	}

	id, replaced, err := s.insertSummary(ctx, eventId, in.GetSummary())
	if err != nil {
		return &res, err
	}

	res.Id = id
	res.MemoriesReplaced = int32(replaced)

	return &res, nil
}

// insertSummary validates summaryProto, resolves its significance/placement, and replaces every
// memory of eventId with a single summary memory built from it, in one transaction. The caller
// must have verified the event exists. It is the shared body of ReplaceMemoriesWithSummary
// (caller-supplied summary) and SummariseMemories (LLM-generated summary): same validation,
// minimum-significance gate, recall-state reset, significance resolution, telemetry, and search
// write-through. The returned error is already an appropriate gRPC status (InvalidArgument for a
// rejected summary, else mapError'd), ready to return unchanged.
func (s *Server) insertSummary(ctx context.Context, eventId string, summaryProto *contract.Memory) (string, int, error) {
	summary := types.MemoryFromProto(summaryProto)
	summary.EventId = eventId
	summary.IsSummary = true

	// The summary is a fresh memory: like StoreMemory it must not inherit client-supplied recall
	// state, or it would start already reinforced.
	summary.TimeRecalled = 0
	summary.RecallCount = 0

	if err := summary.ValidateInsert(s.maxMemoryBodyLength, false); err != nil {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "invalid")))

		return "", 0, status.Error(codes.InvalidArgument, err.Error())
	}

	if !hasPlacement(summaryProto.GetPlacement()) && summary.Significance > 0 && summary.Significance < s.minimumMemorySignificance {
		tel.memoriesRejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "insignificant")))

		return "", 0, status.Error(codes.InvalidArgument, "summary significance below minimum")
	}

	// Resolve the summary's significance/placement to a registry level before it is inserted.
	if err := s.resolveMemorySignificance(ctx, summaryProto, &summary); err != nil {
		if errors.Is(err, db.ErrInvalidPlacement) {
			return "", 0, status.Error(codes.InvalidArgument, err.Error())
		}

		return "", 0, mapError(err)
	}

	summary.SetDefaults()

	replaced, err := s.db.ReplaceMemoriesWithSummary(ctx, eventId, summary)
	if err != nil {
		return "", 0, mapError(err)
	}

	tel.memoriesSummarised.Add(ctx, int64(replaced))
	tel.summariesCreated.Add(ctx, 1)

	// The event has just been condensed, so it is no longer worth offering. Done here rather than in
	// each caller because this is the one path every summarisation takes - the two RPCs and the sleep
	// cycle's auto-summarisation alike - and an event the service itself summarised is the one case
	// where "a snapshot that may have gone stale" is something it can simply not be wrong about.
	s.dropSummarisationCandidate(eventId)

	// The single FIFO worker guarantees the event-scoped delete lands before the summary's
	// index write, so the replaced memories cannot outlive the summary in the index.
	s.searchIdx().DeleteByEventId(eventId)
	s.indexMemory(ctx, summary)

	return summary.Id, replaced, nil
}

// dropSummarisationCandidate removes one event from the cached candidate list, so
// GetSummarisationCandidates stops offering an event that no longer has unsummarised memories to
// condense. A no-op for an event that was never a candidate, which is the common case: most
// summarisation is a client acting on its own judgement rather than on the scan's.
func (s *Server) dropSummarisationCandidate(eventId string) {
	s.summarisationCandidatesMu.Lock()
	defer s.summarisationCandidatesMu.Unlock()

	kept := s.summarisationCandidates[:0]

	for i := range s.summarisationCandidates {
		if s.summarisationCandidates[i].EventId == eventId {
			continue
		}

		kept = append(kept, s.summarisationCandidates[i])
	}

	s.summarisationCandidates = kept
}

// GetSummarisationCandidates returns the events identified by the most recent sleep cycle as
// having accumulated enough quiet, unsummarised memories to be worth condensing via
// ReplaceMemoriesWithSummary. The list is a point-in-time snapshot: it is only refreshed when
// consolidation.summarisationMinMemories is configured, and may include events that have since
// changed.
func (s *Server) GetSummarisationCandidates(ctx context.Context, in *contract.EmptyRequest) (*contract.GetSummarisationCandidatesResponse, error) {
	var res contract.GetSummarisationCandidatesResponse

	groups, bound := s.scopedGroups(ctx)

	s.summarisationCandidatesMu.RLock()
	defer s.summarisationCandidatesMu.RUnlock()

	// The cache is store-wide (the scan must see every group, or a group it skipped would never be
	// summarised for anyone), so a scoped caller is served the subset carrying a group it holds.
	// Filtering here rather than at the scan is also why this cannot shorten a page: the response is
	// the whole list, not a limited one.
	cs := make([]*contract.SummarisationCandidate, 0, len(s.summarisationCandidates))

	for _, c := range s.summarisationCandidates {
		if bound && !auth.GroupInScope(groups, c.Group) {
			continue
		}

		cs = append(cs, &contract.SummarisationCandidate{
			EventId:     c.EventId,
			EventName:   c.EventName,
			MemoryCount: int32(c.MemoryCount),
		})
	}

	res.Candidates = cs

	// Reported alongside the list because an empty list on its own cannot say which of the two
	// reasons produced it, and they call for opposite responses from a client: wait, or stop asking.
	// Both conditions are required - the scan is a step of the sleep cycle, so a replica
	// (consolidation.enabled false) never populates the cache however the threshold is set.
	res.ScanEnabled = s.consolidationEnabled && s.consolidation.summarisationMinMemories > 0

	return &res, nil
}

// countMemories answers a listing's TotalCount, through the count cache when one is configured.
//
// The cache is consulted here rather than in the db layer because it is a policy about how fresh a
// total needs to be, not a fact about storage - and because the db layer is the seam a future
// non-SQL backend implements, which should not have to reimplement a cache to be correct.
func (s *Server) countMemories(ctx context.Context, filter db.MemoryFilter) (int, error) {
	key := filterCacheKey("memories", filter)

	if total, ok := s.listingCounts.get(key); ok {
		return total, nil
	}

	total, err := s.db.CountMemoriesFiltered(ctx, filter)
	if err != nil {
		return 0, err
	}

	s.listingCounts.put(key, total)

	return total, nil
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

	if in.GetRecallCountMax() > 0 && in.GetRecallCountMin() > 0 && in.GetRecallCountMax() < in.GetRecallCountMin() {
		return &res, fmt.Errorf("RecallCountMax must be greater than or equal to RecallCountMin")
	}

	if in.GetTimeRecalledMax() > 0 && in.GetTimeRecalledMin() > 0 && in.GetTimeRecalledMax() < in.GetTimeRecalledMin() {
		return &res, fmt.Errorf("TimeRecalledMax must be greater than or equal to TimeRecalledMin")
	}

	// Filter keys are validated here, not only at the db layer, because an unvalidated key reaches
	// the driver as part of a JSON path: MySQL's JSON_EXTRACT raises ER_INVALID_JSON_PATH on a
	// malformed one, which would surface as Internal rather than as the caller's mistake.
	metadata, err := types.ParseMetadataFilters(in.GetMetadata())
	if err != nil {
		return &res, status.Error(codes.InvalidArgument, err.Error())
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

	// InvalidArgument rather than a bare error, which the interceptor would surface as Unknown (a
	// 500 over the gateway): naming an unsortable column is the caller's mistake, and the message
	// lists the columns that are. The neighbouring range checks above predate this and still
	// report Unknown.
	if !db.ValidMemoryOrderBy(orderBy) {
		return &res, status.Errorf(codes.InvalidArgument,
			"OrderBy must be one of %s", strings.Join(db.MemoryOrderByValues(), ", "))
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
		TimeStampMin:         in.GetTimestampMin(),
		TimeStampMax:         in.GetTimestampMax(),
		SignificanceMin:      in.GetSignificanceMin(),
		SignificanceMax:      in.GetSignificanceMax(),
		SignificanceExtremum: extremum,
		Group:                in.GetGroup(),
		OrderBy:              orderBy,
		OrderDirection:       sortDirection(in.GetOrderDir()),
		Limit:                limit,
		Offset:               offset,

		EventId: in.GetEventId(),

		Metadata:        metadata,
		Recalled:        triState(in.GetRecalled()),
		HasEvent:        triState(in.GetHasEvent()),
		IsSummary:       triState(in.GetIsSummary()),
		IsBinary:        triState(in.GetIsBinary()),
		RecallCountMin:  in.GetRecallCountMin(),
		RecallCountMax:  in.GetRecallCountMax(),
		TimeRecalledMin: in.GetTimeRecalledMin(),
		TimeRecalledMax: in.GetTimeRecalledMax(),
	}

	// The caller's group scope, applied as a predicate so it narrows the candidates before the
	// LIMIT and the total count - both of which would otherwise report on records the caller cannot
	// see. It composes with the Group filter above rather than replacing it.
	filter.Groups, _ = s.scopedGroups(ctx)

	// linked_to narrows the listing to one memory's direct neighbours, resolved to ids and passed
	// down as a filter so it composes with the time/significance/group filters and with pagination.
	if linkedTo := in.GetLinkedTo(); linkedTo != "" {
		// The anchor must be one the caller can see; its neighbours are then narrowed by the scope
		// predicate on the filter, so a link reaching out of the partition contributes nothing.
		if err := s.scopeMemoryIds(ctx, []string{linkedTo}); err != nil {
			return &res, err
		}

		missing, err := s.db.MissingMemoryIds(ctx, []string{linkedTo})
		if err != nil {
			return &res, mapError(err)
		}

		if len(missing) > 0 {
			return &res, status.Errorf(codes.NotFound, "no such memory: %s", linkedTo)
		}

		linked, err := s.db.LinkedMemoryIds(ctx, []string{linkedTo})
		if err != nil {
			return &res, mapError(err)
		}

		// No neighbours is an empty page, not an unrestricted one: an empty id set left on the
		// filter would read as "no id restriction" and return the whole store.
		if len(linked) == 0 {
			return &res, nil
		}

		filter.Ids = linked
	}

	memories, err := s.db.GetMemories(ctx, filter)
	if err != nil {
		return &res, mapError(err)
	}

	// The page is fetched BEFORE the total, because it can make the total free.
	//
	// TotalCount is a second unbounded pass over the same predicate - it ignores Limit and Offset by
	// definition - and on a large store it costs far more than the page it accompanies: 32 ms against
	// the page's 0.26 ms at 100,000 memories, so it is effectively the whole request (TODO 74.3).
	// Indexing the predicate is not the way out; measured, an index that makes the count 57x faster
	// makes the page 181x slower by pulling the planner off the listing index, and the pair comes out
	// worse than it started.
	//
	// This is the case where the count is not needed at all: a page that came back SHORT has run off
	// the end of what the filter matches, so nothing follows it. Exact, not an estimate, and it covers
	// the shapes where a total is most often asked for - a narrow filter, an id or link traversal, a
	// small store - without touching the general case.
	//
	// At offset 0 the page IS the whole result set and its length is the total. At a positive offset
	// the same argument runs one step along: OFFSET skipped exactly that many MATCHING rows to reach a
	// window that then ran out, so the total is the offset plus what came back - which answers the last
	// page of any traversal for free. That second form needs the page to be NON-EMPTY: an empty page
	// says only that the total is at most the offset, and a bound is not an answer.
	//
	// Both are exact as of this query in the same sense the page is, and no more: a concurrent write
	// can move the result set underneath offset pagination whether the total is counted or derived.
	total := len(*memories)

	switch {

	case len(*memories) >= filter.Limit, filter.Offset > 0 && len(*memories) == 0:
		total, err = s.countMemories(ctx, filter)
		if err != nil {
			return &res, mapError(err)
		}

	case filter.Offset > 0:
		total = filter.Offset + len(*memories)

	}

	if in.GetLinks() {
		s.attachMemoryLinks(ctx, *memories)
	}

	ms := make([]*contract.Memory, len(*memories))
	for i, m := range *memories {
		ms[i] = m.ToProto()
	}
	res.Memories = ms
	res.TotalCount = int32(total)

	return &res, nil
}
