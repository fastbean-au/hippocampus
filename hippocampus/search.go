package hippocampus

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// defaultSearchLimit is the number of results returned when the request does not specify a
// positive limit.
const defaultSearchLimit = 10

// searchIdx returns the configured search index, or the disabled no-op when none was injected
// (as in tests constructing a Server directly), so callers never need a nil check.
func (s *Server) searchIdx() search.Index {
	if s.search == nil {
		return search.NewNoop()
	}

	return s.search
}

// SearchMemories finds memories by body content via the optional secondary search index, then
// re-reads the matches from the primary store, which remains authoritative - ids the index
// returns that the primary store no longer holds are stale entries and are silently dropped.
// When reinforce is set the matches are recalled (reinforcing them) rather than merely fetched.
func (s *Server) SearchMemories(ctx context.Context, in *contract.SearchMemoriesRequest) (*contract.GetMemoriesResponse, error) {
	log.Trace("func() SearchMemories")

	var res contract.GetMemoriesResponse

	idx := s.searchIdx()

	if !idx.Enabled() {
		return &res, status.Error(codes.FailedPrecondition, "content search is not enabled (opensearch.enabled)")
	}

	if in.GetQuery() == "" {
		return &res, fmt.Errorf("query must be provided")
	}

	limit := int(in.GetLimit())
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	ids, err := idx.Search(ctx, search.Query{
		Text:    in.GetQuery(),
		EventId: in.GetEventId(),
		Group:   in.GetGroup(),
		Limit:   limit,
	})
	if err != nil {
		return &res, mapError(err)
	}

	if len(ids) == 0 {
		return &res, nil
	}

	// Both fetch paths return only rows the primary store still holds, so stale index entries
	// drop out here without any special handling.
	var memories *[]types.Memory

	if in.GetReinforce() {
		memories, err = s.db.RecallMemories(ctx, ids)
	} else {
		memories, err = s.db.GetMemoriesByIds(ctx, ids)
	}

	if err != nil {
		return &res, mapError(err)
	}

	tel.memoriesSearched.Add(ctx, int64(len(*memories)), metric.WithAttributes(attribute.Bool("reinforce", in.GetReinforce())))

	if in.GetReinforce() {
		tel.memoriesRecalled.Add(ctx, int64(len(*memories)))
	}

	// Return results in the index's relevance order, not the fetch order.
	byId := make(map[string]types.Memory, len(*memories))
	for _, memory := range *memories {
		byId[memory.Id] = memory
	}

	ms := make([]*contract.Memory, 0, len(*memories))

	for _, id := range ids {
		m, ok := byId[id]
		if !ok {
			continue
		}

		ms = append(ms, m.ToProto())
	}

	res.Memories = ms

	return &res, nil
}
