package hippocampus

import (
	"context"
	"sort"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// A selected Export or Transfer moves part of a store rather than all of it (TODO-3 item 159): the
// memories a MemorySelection matches, and the events they belong to. Before it, the only way to
// export one group, one tier or one time range was to export everything and filter the archive
// offline - and with clear set, the only way to MOVE part of a store was to move all of it.
//
// The selection is the same predicate DeleteMemoriesByFilter takes, built by the same
// memorySelectionFilter, so the GetMemories listing with those fields is the dry run of what the
// archive will hold - exactly as it is for the predicate deletion.

// transferSelection builds the filter a selected walk runs on, or nil for the whole store. An
// absent or empty selection is the whole store, as it was before the field existed: unlike the
// predicate deletion, whose empty filter is refused because "everything" is Purge, an Export of
// everything is the ordinary request.
func (s *Server) transferSelection(ctx context.Context, in *contract.MemorySelection) (*db.MemoryFilter, error) {
	log.Trace("func() transferSelection")

	if in == nil {
		return nil, nil
	}

	filter, err := memorySelectionFilter(in)
	if err != nil {
		if _, ok := status.FromError(err); ok {
			return nil, err
		}

		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if isZeroFilter(filter) {
		return nil, nil
	}

	return &filter, nil
}

// walkSelection is walkStore for a selection. It cannot page the two tables independently as the
// whole-store walk does, because which events belong in the archive depends on which memories were
// selected - so it resolves the memory ids first (ids only, never a body), derives the event ids
// from them, and then emits events before memories, the order an import needs.
//
// The events are those the selected memories belong to, plus, when the selection names a group,
// every event carrying that group: a group's empty events are part of the group, and a group
// moved with clear set should not leave them behind on the source.
func (s *Server) walkSelection(
	ctx context.Context,
	selection db.MemoryFilter,
	onEvents func([]types.Event) error,
	onMemories func([]types.Memory) error,
) (*transferManifest, int, int, error) {
	log.Trace("func() walkSelection")

	batchSize := s.transfer.batchSize
	if batchSize <= 0 {
		batchSize = defaultTransferBatchSize
	}

	maxRows := s.transfer.maxManifestRows

	groups, _ := s.scopedGroups(ctx)
	selection.Groups = groups

	// An extremum names one tier for the whole walk, resolved once, exactly as the predicate
	// deletion does: resolved per page it would follow the tier downward as clear removed it.
	selection, found, err := s.pinMemoryExtremum(ctx, selection)
	if err != nil {
		return nil, 0, 0, err
	}

	if maxRows > 0 && found {
		if total, err := s.db.CountMemoriesFiltered(ctx, selection); err == nil && total > maxRows {
			return nil, 0, 0, manifestTooLargeError(total, maxRows)
		}
	}

	var memoryIds []string

	for found {
		selection.Limit = batchSize

		page, err := s.db.MemoryIdsMatching(ctx, selection)
		if err != nil {
			return nil, 0, 0, err
		}

		memoryIds = append(memoryIds, page...)

		if maxRows > 0 && len(memoryIds) > maxRows {
			return nil, 0, 0, manifestTooLargeError(len(memoryIds), maxRows)
		}

		if len(page) < batchSize {
			break
		}

		selection.IdAfter = page[len(page)-1]
	}

	eventIds, err := s.selectedEventIds(ctx, selection, memoryIds, batchSize)
	if err != nil {
		return nil, 0, 0, err
	}

	if maxRows > 0 && len(eventIds)+len(memoryIds) > maxRows {
		return nil, 0, 0, manifestTooLargeError(len(eventIds)+len(memoryIds), maxRows)
	}

	manifest := &transferManifest{id: uuid.New().String()}
	events := 0
	memories := 0

	for start := 0; start < len(eventIds); start += batchSize {
		chunk := eventIds[start:min(start+batchSize, len(eventIds))]

		page, err := s.db.GetEvents(ctx, db.EventFilter{Ids: chunk, Groups: groups, Limit: len(chunk)})
		if err != nil {
			return nil, 0, 0, err
		}

		if len(*page) == 0 {
			continue
		}

		sort.Slice(*page, func(i int, j int) bool { return (*page)[i].Id < (*page)[j].Id })

		for _, event := range *page {
			manifest.eventIds = append(manifest.eventIds, event.Id)
		}

		s.attachEventLinks(ctx, *page)

		if err := onEvents(*page); err != nil {
			return nil, 0, 0, err
		}

		events += len(*page)
	}

	for start := 0; start < len(memoryIds); start += batchSize {
		chunk := memoryIds[start:min(start+batchSize, len(memoryIds))]

		// A memory deleted since its id was read is simply absent, as a row deleted behind the
		// whole-store walk's cursor is.
		page, err := s.db.GetMemoriesByIds(ctx, chunk)
		if err != nil {
			return nil, 0, 0, err
		}

		if len(*page) == 0 {
			continue
		}

		sort.Slice(*page, func(i int, j int) bool { return (*page)[i].Id < (*page)[j].Id })

		for _, memory := range *page {
			manifest.memories = append(manifest.memories, db.MemoryRecallSnapshot{
				Id:           memory.Id,
				TimeRecalled: memory.TimeRecalled,
				RecallCount:  memory.RecallCount,
			})
		}

		s.attachMemoryLinks(ctx, *page)

		if err := onMemories(*page); err != nil {
			return nil, 0, 0, err
		}

		memories += len(*page)
	}

	return manifest, events, memories, nil
}

// selectedEventIds returns, sorted and without duplicates, the events a selected walk carries: those
// the selected memories belong to, and every event in the selection's group when it names one.
func (s *Server) selectedEventIds(
	ctx context.Context,
	selection db.MemoryFilter,
	memoryIds []string,
	batchSize int,
) ([]string, error) {
	log.Trace("func() selectedEventIds")

	referenced, err := s.db.EventIdsForMemories(ctx, memoryIds)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(referenced))

	for _, id := range referenced {
		seen[id] = struct{}{}
	}

	if selection.Group != "" {
		filter := db.EventFilter{Group: selection.Group, Groups: selection.Groups, Limit: batchSize}

		for {
			page, err := s.db.EventIdsMatching(ctx, filter)
			if err != nil {
				return nil, err
			}

			for _, id := range page {
				seen[id] = struct{}{}
			}

			if len(page) < batchSize {
				break
			}

			filter.IdAfter = page[len(page)-1]
		}
	}

	ids := make([]string, 0, len(seen))

	for k := range seen {
		ids = append(ids, k)
	}

	sort.Strings(ids)

	return ids, nil
}
