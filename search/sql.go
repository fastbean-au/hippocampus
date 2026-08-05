package search

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/db"
)

// ContentStore is the primary store's content-search surface, satisfied by *db.DB. Declaring it
// here as an interface rather than taking *db.DB keeps the SQL backend testable with a fake, the
// same way db.Server inverts the dependency in the other direction.
type ContentStore interface {
	// SearchMemoryHits returns the memories whose body matches the query, most relevant first.
	SearchMemoryHits(ctx context.Context, query db.ContentQuery) ([]db.ContentHit, error)

	// ContentSearchAvailable reports whether this store can answer content searches at all.
	ContentSearchAvailable() bool

	// RebuildContentSearch empties and repopulates the index from the primary store.
	RebuildContentSearch(ctx context.Context) error
}

// SQL is the content-search backend built into the primary store: an FTS5 index in the same SQLite
// database, rather than a separate cluster. It exists so that SearchMemories works out of the box,
// which it did not while the only backend was OpenSearch and OpenSearch was off by default.
//
// Every mutating method is a no-op, and that is the whole point rather than an omission. The
// OpenSearch implementation needs them because it is a second system that has to be told what
// happened; this index lives inside the writes themselves - maintained by the db package's write
// helpers, and for deletes by a trigger - so by the time any of these could be called, the index
// is already correct. Wiring them up would double-index.
//
// It follows that this backend has none of the failure modes the queue-and-worker one has: nothing
// to overflow, no ordering to get wrong, no reconciliation sweep to run. What it does have is a
// narrower reach - SQLite only - which is why Enabled consults the store rather than assuming.
type SQL struct {
	store ContentStore
}

// NewSQL returns the primary-store-backed content-search index. It reports an error rather than a
// disabled index when the store cannot support content search, so main can say why in a log line
// instead of leaving an operator to discover the absence through an empty search result.
func NewSQL(store ContentStore) (*SQL, error) {
	log.Trace("func() search.NewSQL")

	if !store.ContentSearchAvailable() {
		return nil, db.ErrContentSearchUnavailable
	}

	return &SQL{store: store}, nil
}

func (s *SQL) IndexMemory(doc Doc) {}

func (s *SQL) DeleteMemories(ids []string) {}

func (s *SQL) DeleteByEventId(eventId string) {}

// SetEventId is a no-op like the rest: the index holds no event id at all. Filtering by event is a
// join to the memories table at query time (see db.SearchMemoryIds), so a memory moving between
// events needs nothing propagated - the next search sees the new event id because it reads it from
// the primary store.
func (s *SQL) SetEventId(fromEventId string, toEventId string) {}

func (s *SQL) Purge() {}

// Search returns the matching memories, most relevant first, for the caller to re-read from the
// primary store. The store already flips bm25's sign, so the scores arrive in Hit's
// higher-is-better convention and pass straight through.
func (s *SQL) Search(ctx context.Context, query Query) ([]Hit, error) {
	log.Trace("func() search.SQL.Search")

	// Refused rather than silently answered as a keyword search: a caller who asked for meaning and
	// got word matching would have no way to tell, and would conclude semantic search works badly
	// rather than that it is absent.
	if len(query.Vector) > 0 {
		return nil, ErrSemanticUnavailable
	}

	found, err := s.store.SearchMemoryHits(ctx, db.ContentQuery{
		Text:    query.Text,
		EventId: query.EventId,
		Group:   query.Group,
		Limit:   query.Limit,
	})
	if err != nil {
		return nil, err
	}

	hits := make([]Hit, 0, len(found))

	for _, hit := range found {
		hits = append(hits, Hit{Id: hit.Id, Score: hit.Score})
	}

	return hits, nil
}

// Rebuild empties and repopulates the index from the primary store - what --backfill-search runs
// against this backend. It is not part of Index (the OpenSearch backend's equivalents are likewise
// concrete methods, reached by the backfill tool alone).
func (s *SQL) Rebuild(ctx context.Context) error {
	log.Trace("func() search.SQL.Rebuild")

	return s.store.RebuildContentSearch(ctx)
}

// SupportsVectors is false: this backend is an FTS5 keyword index with no vector support, and
// adding one would mean either a cgo extension (which would cost the pure-Go build every
// deployment target depends on) or a brute-force scan whose cost grows with the store. Semantic
// search is therefore an OpenSearch capability - a deliberate trade of feature parity for the
// embedded deployment having nothing to run alongside.
func (s *SQL) SupportsVectors() bool {
	return false
}

func (s *SQL) Enabled() bool {
	return true
}

// Close releases nothing: the index's lifetime is the database's, and the database is closed by
// its owner.
func (s *SQL) Close() error {
	return nil
}

// Compile-time check that SQL satisfies Index.
var _ Index = (*SQL)(nil)
