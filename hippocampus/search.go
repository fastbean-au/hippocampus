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
	"github.com/fastbean-au/hippocampus/embed"
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
	hits, err := s.searchHits(ctx, in, s.ranking.candidateLimit(limit))
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

		// Spreading activation, over exactly the memories returned - the same set reinforceRanked
		// recalled, not the wider candidate pool.
		s.reinforceLinked(ctx, idsOfMemories(ranked))
	}

	// Associative retrieval, appended after the ranked matches: these were not matched by the
	// query, are not counted in the ranking, and are never reinforced by being returned.
	if in.GetIncludeLinked() && len(ranked) > 0 {
		ranked = append(ranked, s.linkedMemories(ctx, idsOfMemories(ranked))...)
	}

	ms := make([]*contract.Memory, 0, len(ranked))

	for _, memory := range ranked {
		ms = append(ms, memory.ToProto())
	}

	res.Memories = ms
	res.TotalCount = int32(len(ms))

	return &res, nil
}

// searchHits runs the request's search mode against the index and returns the ranked hits.
//
// The mode dispatch lives here rather than in the search package because hybrid needs two searches
// and a fusion, and because the query embedding is the RPC layer's to obtain - the search backends
// deliberately know nothing about an embedder.
func (s *Server) searchHits(ctx context.Context, in *contract.SearchMemoriesRequest, limit int) ([]search.Hit, error) {
	idx := s.searchIdx()

	base := search.Query{
		Text:    in.GetQuery(),
		EventId: in.GetEventId(),
		Group:   in.GetGroup(),
		Limit:   limit,
	}

	switch in.GetMode() {

	case contract.SearchMode_SEARCH_MODE_SEMANTIC:
		vector, err := s.embedQuery(ctx, in.GetQuery())
		if err != nil {
			return nil, err
		}

		base.Vector = vector

		return idx.Search(ctx, base)

	case contract.SearchMode_SEARCH_MODE_HYBRID:
		vector, err := s.embedQuery(ctx, in.GetQuery())
		if err != nil {
			return nil, err
		}

		keyword, err := idx.Search(ctx, base)
		if err != nil {
			return nil, err
		}

		semantic := base
		semantic.Vector = vector

		vectorHits, err := idx.Search(ctx, semantic)
		if err != nil {
			return nil, err
		}

		// Fused by rank, not score: a bm25 relevance and a cosine similarity are not on comparable
		// scales, so only the orderings can be combined. See fusion.go.
		return fuseHits(keyword, vectorHits), nil

	default:
		// UNSPECIFIED and KEYWORD alike: an existing caller that never sets the field gets exactly
		// the search it always got.
		return idx.Search(ctx, base)

	}
}

// embedQuery turns the caller's query text into a vector, refusing clearly when the deployment
// cannot do it at all.
//
// The two refusals are separate on purpose. Having no embedder and having no vector index are
// different misconfigurations with different fixes, and a caller told only "semantic search is
// unavailable" would have to guess which. Both are FailedPrecondition rather than Internal: nothing
// has gone wrong, the deployment simply does not offer what was asked for.
func (s *Server) embedQuery(ctx context.Context, text string) ([]float32, error) {
	embedder := s.embedder()

	if !embedder.Enabled() {
		return nil, status.Error(codes.FailedPrecondition,
			"semantic search is not available: no embedding model is configured (ollama.embedding.enabled)")
	}

	if !s.searchIdx().SupportsVectors() {
		return nil, status.Error(codes.FailedPrecondition,
			"semantic search is not available: this search backend has no vector index (it requires opensearch.enabled, and an index built with the vector field - run --backfill-search --reindex if OpenSearch was enabled before the embedding model)")
	}

	vectors, err := embedder.Embed(ctx, []string{text})
	if err != nil {
		log.Errorf("failed to embed the search query: %s", err.Error())

		return nil, status.Error(codes.Unavailable, "the embedding model could not be reached")
	}

	if len(vectors) != 1 {
		return nil, status.Error(codes.Internal, "the embedding model returned no vector for the query")
	}

	return vectors[0], nil
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

// embedder returns the configured text embedder, or the disabled no-op when none was injected (as
// in tests constructing a Server directly), so callers never need a nil check - the same shape as
// searchIdx() and summariser().
func (s *Server) embedder() embed.Embedder {
	if s.embed == nil {
		return embed.NewNoop()
	}

	return s.embed
}

// searchModes reports which SearchMemories modes this deployment can serve, for WhoAmI. It depends
// only on the configured backends, never on the caller, so every caller gets the same answer.
func (s *Server) searchModes() []contract.SearchMode {
	idx := s.searchIdx()

	if !idx.Enabled() {
		return nil
	}

	modes := []contract.SearchMode{contract.SearchMode_SEARCH_MODE_KEYWORD}

	// Both halves are required, and neither implies the other: an embedder without a vector index
	// has nowhere to put the vectors, and a vector index without an embedder has nothing to put in
	// it.
	if s.embedder().Enabled() && idx.SupportsVectors() {
		modes = append(modes,
			contract.SearchMode_SEARCH_MODE_SEMANTIC,
			contract.SearchMode_SEARCH_MODE_HYBRID,
		)
	}

	return modes
}

// indexMemory embeds a memory's body (when semantic search is configured) and hands the document
// to the search index. Every RPC-layer write-through goes through here rather than calling
// IndexMemory directly, for a reason that is easy to get wrong: search.Doc carries the vector, so
// re-indexing a memory without one REPLACES its document with a vectorless copy. A path that
// forgot to embed would not fail - it would quietly make that memory unfindable by meaning.
//
// Embedding is a network call to the model server, so enabling semantic search puts that server on
// the write path and adds its latency to a store. That is deliberate rather than incidental: the
// alternative, embedding asynchronously after the write returns, would race the delete propagation
// the index's FIFO worker exists to order - a memory stored and immediately deleted could be
// re-indexed after its own deletion. The memory itself is already committed by the time this runs,
// so the cost of a slow or unreachable model server is latency and a missing vector, never a lost
// write.
func (s *Server) indexMemory(ctx context.Context, memory types.Memory) {
	doc := search.DocFromMemory(memory)

	if vector, ok := s.embedBody(ctx, memory); ok {
		doc.Vector = vector
	}

	s.searchIdx().IndexMemory(doc)
}

// embedBody returns the embedding of a memory's body, and whether there is one.
//
// It reports failure rather than returning it, because no caller can act on it: the memory is
// already stored, the index is best-effort, and a rebuild can supply the vector later. Binary
// bodies are skipped for the same reason they are never indexed - they are opaque, and embedding
// base64 would produce a vector describing the encoding rather than the content.
func (s *Server) embedBody(ctx context.Context, memory types.Memory) ([]float32, bool) {
	embedder := s.embedder()

	if !embedder.Enabled() || memory.IsBinary || memory.Body == "" {
		return nil, false
	}

	if !s.searchIdx().SupportsVectors() {
		return nil, false
	}

	vectors, err := embedder.Embed(ctx, []string{memory.Body})
	if err != nil {
		log.Warnf("failed to embed memory '%s' for semantic search: %s", memory.Id, err.Error())

		return nil, false
	}

	if len(vectors) != 1 {
		return nil, false
	}

	return vectors[0], true
}
