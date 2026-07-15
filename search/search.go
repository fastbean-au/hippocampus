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
	}
}

// Query carries the parameters of one content search. Text is required; EventId and Group
// restrict matches when non-empty.
type Query struct {
	Text    string
	EventId string
	Group   string
	Limit   int
}

// Index is the secondary content-search contract. Every mutating method enqueues and returns
// immediately: it never fails, never blocks the caller, and is applied best-effort - a full
// queue or an unreachable cluster drops the operation with a warning rather than surfacing an
// error, since the index is rebuildable and stale entries are harmless (reads are re-verified
// against the primary store).
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

	// Search returns the ids of memories whose body matches the query text, most relevant
	// first, optionally restricted to a single event and/or group. The caller must fetch the
	// returned ids from the primary store; ids that no longer exist there are stale index
	// entries to be dropped.
	Search(ctx context.Context, query Query) ([]string, error)

	// Enabled reports whether a real index is configured; the no-op implementation returns
	// false.
	Enabled() bool

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

func (noop) Search(ctx context.Context, query Query) ([]string, error) {
	return nil, ErrDisabled
}

func (noop) Enabled() bool {
	return false
}

func (noop) Close() error {
	return nil
}

// Compile-time check that noop satisfies Index.
var _ Index = noop{}
