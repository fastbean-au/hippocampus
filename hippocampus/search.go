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

	// Reachable only where no backend could be built at all: the SQL backend covers the default
	// (SQLite) deployment without any configuration, so this now means a driver that has no
	// content search yet and no OpenSearch cluster configured either.
	if !idx.Enabled() {
		return &res, status.Error(codes.FailedPrecondition,
			"content search is not available: this storage driver has no built-in content search - enable opensearch.enabled")
	}

	if in.GetQuery() == "" {
		return &res, fmt.Errorf("query must be provided")
	}

	limit := int(in.GetLimit())
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	// When ranking is active the backend is asked for more candidates than the caller wanted, so
	// significance and recall have room to promote a memory into the returned page; rankMemories
	// truncates back to limit afterwards.
	hits, err := idx.Search(ctx, search.Query{
		Text:    in.GetQuery(),
		EventId: in.GetEventId(),
		Group:   in.GetGroup(),
		Limit:   s.ranking.candidateLimit(limit),
	})
	if err != nil {
		return &res, mapError(err)
	}

	if len(hits) == 0 {
		return &res, nil
	}

	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Id)
	}

	// A reinforcing search is only honoured when the caller may reinforce: a reader for whom
	// reinforcement is disabled (auth.readerRecallReinforces) gets a plain read instead, matching
	// RecallMemories.
	reinforce := in.GetReinforce() && s.mayReinforce(ctx)

	// Read the candidates first, without reinforcing any of them. Ranking needs each candidate's
	// significance and recall count to decide the order, and those have to be read before the
	// winners are known - so a reinforcing search cannot simply recall what it fetched. Recalling
	// the candidate set would reset the decay clock on memories the caller was never shown, which
	// is a real change to what the store forgets, made on their behalf and without their knowledge.
	// The rows the primary store no longer holds drop out here, so stale index entries need no
	// special handling.
	memories, err := s.db.GetMemoriesByIds(ctx, ids)
	if err != nil {
		return &res, mapError(err)
	}

	// Order by relevance blended with significance and recall, and truncate to what the caller
	// asked for.
	ranked := rankMemories(hits, *memories, s.ranking, limit)

	// Only now, against exactly the memories being returned, is reinforcement applied. The recall
	// re-reads them, so the response carries their updated recall state rather than the pre-recall
	// snapshot fetched above.
	if reinforce && len(ranked) > 0 {
		ranked, err = s.reinforceRanked(ctx, ranked)
		if err != nil {
			return &res, mapError(err)
		}
	}

	tel.memoriesSearched.Add(ctx, int64(len(ranked)), metric.WithAttributes(attribute.Bool("reinforce", reinforce)))

	if reinforce {
		tel.memoriesRecalled.Add(ctx, int64(len(ranked)))
	}

	ms := make([]*contract.Memory, 0, len(ranked))

	for _, memory := range ranked {
		ms = append(ms, memory.ToProto())
	}

	res.Memories = ms
	res.TotalCount = int32(len(ms))

	return &res, nil
}

// reinforceRanked recalls exactly the memories being returned, and rebuilds the result in the same
// order with the recalled rows, so the caller sees the recall state their own call just produced.
//
// A memory that vanished between the ranking read and the recall is dropped rather than returned
// with a stale body: RecallMemories only returns what it actually reinforced.
func (s *Server) reinforceRanked(ctx context.Context, ranked []types.Memory) ([]types.Memory, error) {
	ids := make([]string, 0, len(ranked))
	for _, memory := range ranked {
		ids = append(ids, memory.Id)
	}

	recalled, err := s.db.RecallMemories(ctx, ids)
	if err != nil {
		return nil, err
	}

	byId := make(map[string]types.Memory, len(*recalled))
	for _, memory := range *recalled {
		byId[memory.Id] = memory
	}

	out := make([]types.Memory, 0, len(ranked))

	for _, memory := range ranked {
		updated, ok := byId[memory.Id]
		if !ok {
			continue
		}

		out = append(out, updated)
	}

	return out, nil
}
