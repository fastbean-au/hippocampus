// Package search provides the optional secondary content-search index. The primary
// store (db.Store) remains the sole system of record: the index never participates in existence,
// consolidation, or recall decisions. All index mutations are asynchronous, best-effort, and
// one-way (primary -> index); the only synchronous call is Search, whose results are always
// round-tripped through the primary store by the caller.
package search

import (
	"context"
	"errors"

	"github.com/fastbean-au/hippocampus/types"
)

// ErrDisabled is returned by Search when no search index is configured (opensearch.enabled is
// false).
var ErrDisabled = errors.New("content search is not enabled (opensearch.enabled is false)")

// Doc is the indexed projection of a memory. Recall state (time_recalled/recall_count) is
// deliberately excluded: the index never participates in reinforcement decisions, so recalls
// need no propagation.
type Doc struct {
	Id           string `json:"-"` // becomes the document _id, not a mapped field
	Body         string `json:"body"`
	EventId      string `json:"event_id"`
	Significance int32  `json:"significance"`
	Timestamp    int64  `json:"timestamp"`
	IsSummary    bool   `json:"is_summary"`
	Group        string `json:"group"`

	// Metadata is the memory's metadata rendered as sorted "key=value" strings - a flat keyword
	// array, deliberately not a nested object.
	//
	// Metadata keys are client-supplied, and an object mapping would mint a new index field per
	// distinct key: a client generating a key per request would exhaust the cluster's field limit
	// (index.mapping.total_fields.limit, 1000 by default) and break indexing for every memory, not
	// just its own. A keyword array cannot, term-filters exactly, ANDs naturally as several term
	// filters, and is byte-identical to the wire form of a metadata filter - so a filter becomes a
	// term with no conversion. The cost is that values cannot be prefix- or range-queried, which
	// the exact-match-only filter design already rules out.
	Metadata []string `json:"metadata,omitempty"`

	// Vector is the body's embedding, when semantic search is configured. It is omitted when
	// absent so a deployment without an embedder indexes exactly the document it always did.
	//
	// Vectors live here and NOT in the primary store, deliberately. A 768-dimension embedding is
	// around 3 KiB per memory, which on a store whose whole purpose is managing a bounded capacity
	// would compete with the memories themselves for the space body compression exists to save.
	// The cost of that choice is that rebuilding the index re-embeds rather than re-reads, so a
	// rebuild needs the model server up - which is the same trade the index already makes by being
	// rebuildable from the primary store rather than authoritative.
	Vector []float32 `json:"vector,omitempty"`
}

// DocFromMemory maps a memory onto its indexed projection. Callers must not index binary
// memories (the body is opaque); the write-through hooks enforce that.
func DocFromMemory(in types.Memory) Doc {
	return Doc{
		Id:           in.Id,
		Body:         in.Body,
		EventId:      in.EventId,
		Significance: in.Significance,
		Timestamp:    in.TimeStamp,
		IsSummary:    in.IsSummary,
		Group:        in.Group,
		Metadata:     types.MetadataToTerms(in.Metadata),
	}
}

// ErrSemanticUnavailable is returned by Search when a vector query is asked of a backend that has
// no vector index. Distinct from ErrDisabled, which means no content search at all: a caller
// getting this one has search, just not by meaning.
var ErrSemanticUnavailable = errors.New("semantic search is not available on this backend (it requires opensearch.enabled and an embedding model)")

// Query carries the parameters of one content search. EventId and Group restrict matches when
// non-empty.
//
// Text and Vector are the two ways of matching, and which is used is the caller's choice rather
// than the backend's: a Vector runs a k-NN query, otherwise Text runs a keyword query. The caller
// supplies the vector already computed, because embedding is a slow, fallible call to a model
// server and this package deliberately knows nothing about one - see the embed package.
type Query struct {
	Text    string
	Vector  []float32
	EventId string
	Group   string
	Limit   int

	// Metadata restricts matches to memories carrying every one of these key/value pairs. It is
	// applied INSIDE the index rather than to the results, like EventId and Group: post-filtering
	// would silently shrink a page below the caller's limit, and would interact badly with the
	// ranking layer's over-fetch, which is headroom rather than a guarantee.
	Metadata map[string]string

	// Groups is the caller's group scope: the set of group labels this caller may see at all,
	// distinct from Group, which is the single label they chose to filter by. The two compose as a
	// conjunction. Empty means unscoped - every group - so a backend must treat it the way it
	// treats an absent filter, not as "match nothing".
	//
	// It is applied inside the index for the same reasons Metadata is, and one more: the shortfall
	// from post-filtering would itself report how much of the store the caller cannot see.
	Groups []string
}

// Hit is one search match: the memory's id and how well its body matched.
//
// Score is normalised by each backend so that HIGHER IS ALWAYS MORE RELEVANT, because the two
// backends disagree about that natively - FTS5's bm25 rank is negative and more negative is
// better, OpenSearch's _score is positive and higher is better. Fixing the direction at the
// backend boundary is what lets everything above it treat the two the same.
//
// What is deliberately NOT promised is that the values are comparable between backends, or
// meaningful in absolute terms: bm25 raw scores and OpenSearch's are on unrelated scales. Only
// the ORDER, and the relative gaps within one result set, carry meaning. Callers that combine
// this with other signals must therefore normalise within the result set rather than assume a
// range (see the re-ranking in the hippocampus package).
type Hit struct {
	Id    string
	Score float64
}

// Index is the secondary content-search contract. Every mutating method returns immediately and
// reports no error: propagation is best-effort, since the index is rebuildable and stale entries
// are harmless (reads are re-verified against the primary store).
//
// How that is honoured is the implementation's business, and the two real backends honour it
// differently. OpenSearch enqueues to a worker, so a full queue or an unreachable cluster drops
// the operation with a warning. SQL maintains its index inside the primary write itself and so
// implements every mutator as a no-op - there is nothing left to propagate by the time one could
// be called. Callers must therefore not read a mutator returning as evidence that anything was
// queued, only that the index has been given whatever it needs.
type Index interface {
	// IndexMemory adds or replaces the document for a memory.
	IndexMemory(doc Doc)

	// DeleteMemories removes the documents with the given memory ids.
	DeleteMemories(ids []string)

	// DeleteByEventId removes every document associated with an event.
	DeleteByEventId(eventId string)

	// SetEventId rewrites the event id on every document currently carrying fromEventId; an
	// empty toEventId detaches them.
	SetEventId(fromEventId string, toEventId string)

	// Purge removes every document.
	Purge()

	// Search returns the memories whose body matches the query text, most relevant first,
	// optionally restricted to a single event and/or group. The caller must fetch the returned
	// ids from the primary store; ids that no longer exist there are stale index entries to be
	// dropped.
	Search(ctx context.Context, query Query) ([]Hit, error)

	// Enabled reports whether a real index is configured; the no-op implementation returns
	// false.
	Enabled() bool

	// SupportsVectors reports whether this backend can answer a Query carrying a Vector. It is a
	// property of the deployment - the backend, its configuration, and the state of the live index
	// - never of the caller, so it is safe to report to every client (see WhoAmI).
	SupportsVectors() bool

	// Close drains pending operations and releases resources.
	Close() error
}

// noop is the disabled implementation: the service behaves exactly as it does without a search
// index configured.
type noop struct{}

// NewNoop returns the disabled search index.
func NewNoop() Index {
	return noop{}
}

func (noop) IndexMemory(doc Doc) {}

func (noop) DeleteMemories(ids []string) {}

func (noop) DeleteByEventId(eventId string) {}

func (noop) SetEventId(fromEventId string, toEventId string) {}

func (noop) Purge() {}

func (noop) Search(ctx context.Context, query Query) ([]Hit, error) {
	return nil, ErrDisabled
}

func (noop) Enabled() bool {
	return false
}

func (noop) SupportsVectors() bool {
	return false
}

func (noop) Close() error {
	return nil
}

// Compile-time check that noop satisfies Index.
var _ Index = noop{}
