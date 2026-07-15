package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	opensearch "github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// applyTimeout bounds each operation the worker applies against the cluster, so one hung request
// cannot stall the queue indefinitely.
const applyTimeout = 10 * time.Second

// closeDrainTimeout bounds how long Close waits for the worker to drain the queue at shutdown.
const closeDrainTimeout = 5 * time.Second

// indexMapping is the explicit mapping for the memories index. The memory id is the document
// _id, not a mapped field; recall state is deliberately absent (see Doc).
const indexMapping = `{
	"settings": { "number_of_shards": 1, "number_of_replicas": 0 },
	"mappings": { "properties": {
		"body":         { "type": "text" },
		"event_id":     { "type": "keyword" },
		"significance": { "type": "integer" },
		"timestamp":    { "type": "long" },
		"is_summary":   { "type": "boolean" },
		"group":        { "type": "keyword" }
	}}
}`

// groupMapping adds the group field to an index created before the field existed. Putting a
// mapping for a new field is a legal, idempotent update; without it, dynamic mapping would type
// the field as text and the term filter in Search would never match.
const groupMapping = `{ "properties": { "group": { "type": "keyword" } } }`

// Config carries the OpenSearch connection settings, read from viper in main.go.
type Config struct {
	Addresses []string
	Username  string
	Password  string
	Index     string
	QueueSize int

	// Transport overrides the HTTP transport; used by unit tests to fake the cluster.
	Transport http.RoundTripper
}

type opKind int

const (
	opIndex opKind = iota
	opDeleteIds
	opDeleteByEvent
	opSetEventId
	opPurge
)

func (k opKind) String() string {
	switch k {

	case opIndex:
		return "index"

	case opDeleteIds:
		return "delete_ids"

	case opDeleteByEvent:
		return "delete_by_event"

	case opSetEventId:
		return "set_event_id"

	case opPurge:
		return "purge"
	}

	return "unknown"
}

// op is one queued index mutation.
type op struct {
	kind      opKind
	doc       Doc
	ids       []string
	eventId   string
	toEventId string
}

// OpenSearch is the real search index: a thin client plus a single worker goroutine applying
// queued mutations in FIFO order. One worker is a correctness property, not a limitation - the
// delete-then-index pair emitted by ReplaceMemoriesWithSummary, and any create-then-delete pair
// for the same memory, must never be reordered.
type OpenSearch struct {
	client *opensearchapi.Client
	index  string

	queue chan op
	stop  chan struct{}
	done  chan struct{}

	closed atomic.Bool

	// indexReady records that ensureIndex has succeeded at least once, so a cluster that comes
	// up after the service does still gets the explicit mapping before the first document lands.
	indexReady atomic.Bool
}

// NewOpenSearch builds the client, best-effort creates the index, and starts the worker. It
// fails only on unusable configuration (e.g. a malformed address): an unreachable cluster logs a
// warning and the service starts anyway, with the worker retrying the index bootstrap before
// applying operations.
func NewOpenSearch(cfg Config) (*OpenSearch, error) {
	log.Trace("func() search.NewOpenSearch")

	if cfg.Index == "" {
		cfg.Index = "hippocampus-memories"
	}

	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}

	client, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses: cfg.Addresses,
			Username:  cfg.Username,
			Password:  cfg.Password,
			Transport: cfg.Transport,
		},
	})
	if err != nil {
		log.Errorf("failed to create opensearch client: %s", err.Error())

		return nil, err
	}

	o := &OpenSearch{
		client: client,
		index:  cfg.Index,
		queue:  make(chan op, cfg.QueueSize),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}

	if err := o.ensureIndex(context.Background()); err != nil {
		log.Warnf("opensearch index not ready at startup (will retry): %s", err.Error())
	}

	go o.worker()

	return o, nil
}

// ensureIndex creates the index with its explicit mapping when it does not already exist.
func (o *OpenSearch) ensureIndex(ctx context.Context) error {
	log.Trace("func() search.ensureIndex")

	resp, err := o.client.Indices.Exists(ctx, opensearchapi.IndicesExistsReq{Indices: []string{o.index}})

	if err == nil {
		// The index may predate fields added to the mapping since it was created; put them in
		// place so filters on them behave (see groupMapping).
		if _, err := o.client.Indices.Mapping.Put(ctx, opensearchapi.MappingPutReq{
			Indices: []string{o.index},
			Body:    strings.NewReader(groupMapping),
		}); err != nil {
			return fmt.Errorf("failed to update mapping on index '%s': %w", o.index, err)
		}

		o.indexReady.Store(true)

		return nil
	}

	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("failed to check for index '%s': %w", o.index, err)
	}

	if _, err := o.client.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: o.index,
		Body:  strings.NewReader(indexMapping),
	}); err != nil {
		return fmt.Errorf("failed to create index '%s': %w", o.index, err)
	}

	log.Infof("created opensearch index '%s'", o.index)

	o.indexReady.Store(true)

	return nil
}

// enqueue adds an operation to the queue without ever blocking the caller: when the queue is
// full the operation is dropped with a warning. The index is best-effort and rebuildable, and a
// stale document is harmless on read (results are re-verified against the primary store).
func (o *OpenSearch) enqueue(v op) {
	if o.closed.Load() {
		return
	}

	select {

	case o.queue <- v:

	default:
		log.Warnf("opensearch queue full - dropping %s operation", v.kind)
		tel.dropped.Add(context.Background(), 1, metric.WithAttributes(attribute.String("op", v.kind.String())))
	}
}

// worker applies queued operations in FIFO order until stopped, then drains what remains.
func (o *OpenSearch) worker() {
	defer close(o.done)

	for {
		select {

		case <-o.stop:
			for {
				select {

				case v := <-o.queue:
					o.applyWithRetry(v)

				default:
					return
				}
			}

		case v := <-o.queue:
			o.applyWithRetry(v)
		}
	}
}

// applyWithRetry makes sure the index exists before applying an operation; failures are logged
// and the operation abandoned (best-effort by design).
func (o *OpenSearch) applyWithRetry(v op) {
	ctx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()

	if !o.indexReady.Load() {
		if err := o.ensureIndex(ctx); err != nil {
			log.Warnf("opensearch index still not ready - dropping %s operation: %s", v.kind, err.Error())
			tel.dropped.Add(ctx, 1, metric.WithAttributes(attribute.String("op", v.kind.String())))

			return
		}
	}

	if err := o.apply(ctx, v); err != nil {
		log.Warnf("failed to apply opensearch %s operation: %s", v.kind, err.Error())
	}
}

// apply executes one operation synchronously. The integration tests call it directly so their
// assertions do not depend on queue timing.
func (o *OpenSearch) apply(ctx context.Context, v op) error {
	switch v.kind {

	case opIndex:
		body, err := json.Marshal(v.doc)
		if err != nil {
			tel.indexed.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", false)))

			return fmt.Errorf("failed to marshal document '%s': %w", v.doc.Id, err)
		}

		_, err = o.client.Index(ctx, opensearchapi.IndexReq{
			Index:      o.index,
			DocumentID: v.doc.Id,
			Body:       strings.NewReader(string(body)),
		})

		tel.indexed.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

		if err != nil {
			return fmt.Errorf("failed to index document '%s': %w", v.doc.Id, err)
		}

		return nil

	case opDeleteIds:
		for _, id := range v.ids {
			resp, err := o.client.Document.Delete(ctx, opensearchapi.DocumentDeleteReq{
				Index:      o.index,
				DocumentID: id,
			})

			// A 404 means the document was never indexed (e.g. binary memory, dropped op, or
			// written while the cluster was down) - nothing to delete.
			if err != nil && resp != nil && resp.Inspect().Response != nil &&
				resp.Inspect().Response.StatusCode == http.StatusNotFound {
				continue
			}

			tel.deleted.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

			if err != nil {
				return fmt.Errorf("failed to delete document '%s': %w", id, err)
			}
		}

		return nil

	case opDeleteByEvent:
		// delete_by_query only sees refreshed documents; without the refresh, documents indexed
		// moments earlier (e.g. the memories a summary replaces) would survive the delete.
		if err := o.refresh(ctx); err != nil {
			return err
		}

		// Build the body as a map and marshal it: fmt's %q emits escapes (\a, \v, \x07, ...) that
		// JSON does not accept, so an event id carrying a rare control character would produce a
		// malformed query. json.Marshal escapes every input correctly.
		query, err := json.Marshal(map[string]any{
			"query": map[string]any{"term": map[string]any{"event_id": v.eventId}},
		})
		if err != nil {
			return fmt.Errorf("failed to marshal delete query for event '%s': %w", v.eventId, err)
		}

		_, err = o.client.Document.DeleteByQuery(ctx, opensearchapi.DocumentDeleteByQueryReq{
			Indices: []string{o.index},
			Body:    strings.NewReader(string(query)),
			Params:  opensearchapi.DocumentDeleteByQueryParams{Conflicts: "proceed"},
		})

		tel.deleted.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

		if err != nil {
			return fmt.Errorf("failed to delete documents for event '%s': %w", v.eventId, err)
		}

		return nil

	case opSetEventId:
		if err := o.refresh(ctx); err != nil {
			return err
		}

		// Marshal a map rather than interpolate with %q (see opDeleteByEvent) so an event id with a
		// control character can neither break the JSON nor alter the query structure.
		body, err := json.Marshal(map[string]any{
			"query": map[string]any{"term": map[string]any{"event_id": v.eventId}},
			"script": map[string]any{
				"lang":   "painless",
				"source": "ctx._source.event_id = params.to",
				"params": map[string]any{"to": v.toEventId},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to marshal update query for event '%s': %w", v.eventId, err)
		}

		if _, err := o.client.UpdateByQuery(ctx, opensearchapi.UpdateByQueryReq{
			Indices: []string{o.index},
			Body:    strings.NewReader(string(body)),
			Params:  opensearchapi.UpdateByQueryParams{Conflicts: "proceed"},
		}); err != nil {
			return fmt.Errorf("failed to move documents from event '%s' to '%s': %w", v.eventId, v.toEventId, err)
		}

		return nil

	case opPurge:
		// Deleting and recreating the index is instant and avoids a match_all delete-by-query.
		if _, err := o.client.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{o.index}}); err != nil {
			return fmt.Errorf("failed to delete index '%s': %w", o.index, err)
		}

		o.indexReady.Store(false)

		return o.ensureIndex(ctx)
	}

	return fmt.Errorf("unknown operation kind %d", v.kind)
}

func (o *OpenSearch) refresh(ctx context.Context) error {
	if _, err := o.client.Indices.Refresh(ctx, &opensearchapi.IndicesRefreshReq{Indices: []string{o.index}}); err != nil {
		return fmt.Errorf("failed to refresh index '%s': %w", o.index, err)
	}

	return nil
}

func (o *OpenSearch) IndexMemory(doc Doc) {
	log.Trace("func() search.IndexMemory")

	o.enqueue(op{kind: opIndex, doc: doc})
}

func (o *OpenSearch) DeleteMemories(ids []string) {
	log.Trace("func() search.DeleteMemories")

	if len(ids) == 0 {
		return
	}

	o.enqueue(op{kind: opDeleteIds, ids: ids})
}

func (o *OpenSearch) DeleteByEventId(eventId string) {
	log.Trace("func() search.DeleteByEventId")

	o.enqueue(op{kind: opDeleteByEvent, eventId: eventId})
}

func (o *OpenSearch) SetEventId(fromEventId string, toEventId string) {
	log.Trace("func() search.SetEventId")

	o.enqueue(op{kind: opSetEventId, eventId: fromEventId, toEventId: toEventId})
}

func (o *OpenSearch) Purge() {
	log.Trace("func() search.Purge")

	o.enqueue(op{kind: opPurge})
}

// IndexMemorySync indexes one document synchronously, bypassing the queue and returning the
// error, bounded by the same per-operation timeout the worker uses. It exists for the backfill
// CLI mode, which needs to know whether each write landed; the service's own write path must keep
// using IndexMemory (asynchronous, never blocking, FIFO-ordered against deletes).
func (o *OpenSearch) IndexMemorySync(ctx context.Context, doc Doc) error {
	log.Trace("func() search.IndexMemorySync")

	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()

	if !o.indexReady.Load() {
		if err := o.ensureIndex(ctx); err != nil {
			return err
		}
	}

	return o.apply(ctx, op{kind: opIndex, doc: doc})
}

// RecreateIndex synchronously deletes and recreates the index, removing every document —
// including stale entries for memories the primary store no longer has. It backs the --reindex
// flag of the backfill CLI mode.
func (o *OpenSearch) RecreateIndex(ctx context.Context) error {
	log.Trace("func() search.RecreateIndex")

	ctx, cancel := context.WithTimeout(ctx, applyTimeout)
	defer cancel()

	if !o.indexReady.Load() {
		if err := o.ensureIndex(ctx); err != nil {
			return err
		}
	}

	return o.apply(ctx, op{kind: opPurge})
}

// Search returns the ids of memories whose body matches the query, most relevant first. This is
// the only synchronous cluster call the service itself makes; the *Sync methods above exist only
// for the backfill CLI mode.
func (o *OpenSearch) Search(ctx context.Context, query Query) ([]string, error) {
	log.Trace("func() search.Search")

	// Build the whole request as a map and marshal it once, so query.Text, EventId, and Group are
	// all escaped correctly by json.Marshal - fmt's %q would emit escapes (\a, \v, \x07, ...) that
	// JSON rejects, and a crafted value could otherwise alter the query structure.
	boolQuery := map[string]any{
		"must": []any{
			map[string]any{"match": map[string]any{"body": query.Text}},
		},
	}

	var filters []any

	if query.EventId != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"event_id": query.EventId}})
	}

	if query.Group != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"group": query.Group}})
	}

	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}

	body, err := json.Marshal(map[string]any{
		"query":   map[string]any{"bool": boolQuery},
		"size":    query.Limit,
		"_source": false,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query: %w", err)
	}

	resp, err := o.client.Search(ctx, &opensearchapi.SearchReq{
		Indices: []string{o.index},
		Body:    strings.NewReader(string(body)),
	})

	tel.queries.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

	if err != nil {
		log.Errorf("opensearch query failed: %s", err.Error())

		return nil, fmt.Errorf("search failed: %w", err)
	}

	ids := make([]string, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		ids = append(ids, hit.ID)
	}

	return ids, nil
}

func (o *OpenSearch) Enabled() bool {
	return true
}

// Close stops accepting operations and waits for the worker to drain the queue, up to a timeout.
func (o *OpenSearch) Close() error {
	log.Trace("func() search.Close")

	if o.closed.Swap(true) {
		return nil
	}

	close(o.stop)

	select {

	case <-o.done:
		return nil

	case <-time.After(closeDrainTimeout):
		return fmt.Errorf("timed out draining the opensearch queue")
	}
}

// Compile-time check that *OpenSearch satisfies Index.
var _ Index = (*OpenSearch)(nil)
