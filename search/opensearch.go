package search

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
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
// cannot stall the queue indefinitely. The default; overridable per instance via Config.ApplyTimeout.
const applyTimeout = 10 * time.Second

// applyMaxAttempts caps how many times the worker tries one operation against the cluster before
// giving up. Every operation is idempotent (documents are keyed by memory id, deletes and the
// event rewrites converge), so a transient failure - a brief network blip, a node restart, a
// rolling upgrade - is safe to retry, and a few spaced attempts turn most would-be drops into
// eventual successes. A persistently failing operation is still healed later by the periodic
// reconciliation sweep and by --backfill-search. The default; overridable via Config.ApplyMaxAttempts.
const applyMaxAttempts = 4

// applyRetryBaseBackoff is the wait before the second attempt; it doubles (with jitter) each
// further attempt so a struggling cluster is not hammered. The default; overridable via
// Config.ApplyRetryBaseBackoff. A var so tests can shorten it.
var applyRetryBaseBackoff = 250 * time.Millisecond

// closeDrainTimeout bounds how long Close waits for the worker to drain the queue at shutdown. The
// default; overridable via Config.CloseDrainTimeout.
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

// vectorIndexMapping is indexMapping plus the k-NN vector field, used when a vector dimension is
// configured. It is a separate whole mapping rather than an addition to the one above because
// "index.knn" is a STATIC index setting: it can only be set at creation. An index created without
// it cannot be upgraded in place by putting a new field mapping, which is why ensureIndex detects
// that case and reports it rather than half-succeeding (see there).
//
// The dimension is fixed at creation and comes from the model, so changing the embedding model
// means recreating the index - the same constraint that makes the model tag worth recording.
const vectorIndexMapping = `{
	"settings": {
		"number_of_shards": 1,
		"number_of_replicas": 0,
		"index.knn": true
	},
	"mappings": { "properties": {
		"body":         { "type": "text" },
		"event_id":     { "type": "keyword" },
		"significance": { "type": "integer" },
		"timestamp":    { "type": "long" },
		"is_summary":   { "type": "boolean" },
		"group":        { "type": "keyword" },
		"vector":       {
			"type": "knn_vector",
			"dimension": %d,
			"method": {
				"name": "hnsw",
				"space_type": "cosinesimil",
				"engine": "lucene"
			}
		}
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

	// VectorDimension is the embedding model's dimension count. Zero (the default) means no vector
	// field is mapped and semantic queries are rejected, so a deployment without an embedder gets
	// exactly the index it always had.
	VectorDimension int

	// Worker tuning. Each is optional: a zero value falls back to the package default
	// (applyTimeout, applyMaxAttempts, applyRetryBaseBackoff, closeDrainTimeout). Raise them for a
	// slower cluster where the defaults drop too many operations before the reconciliation sweep
	// heals them.
	ApplyTimeout          time.Duration
	ApplyMaxAttempts      int
	ApplyRetryBaseBackoff time.Duration
	CloseDrainTimeout     time.Duration

	// TLS carries the optional transport-security settings applied to an https:// cluster.
	TLS TLSConfig

	// Transport overrides the HTTP transport; used by unit tests to fake the cluster. When set it
	// takes precedence over TLS (the fake cluster needs no real transport security).
	Transport http.RoundTripper
}

// TLSConfig carries the optional TLS settings for the OpenSearch connection. Every field is
// empty/false by default, in which case the client relies on the address scheme alone (an
// https:// address verifies against the system certificate pool with no customisation), matching
// the behaviour before this block existed.
type TLSConfig struct {
	// CACertFile is a PEM bundle of certificate authorities to trust for the server certificate,
	// used in place of the system pool. Set it to trust a cluster serving a certificate signed by
	// a private CA - including the OpenSearch security plugin's self-signed demo certificates.
	CACertFile string

	// CertFile and KeyFile are a client certificate/key pair for mutual TLS. Both must be set
	// together, or neither.
	CertFile string
	KeyFile  string

	// InsecureSkipVerify disables server certificate verification entirely. It is a
	// development-only escape hatch for self-signed certificates - prefer CACertFile in
	// production, where an unverified connection offers no protection against interception.
	InsecureSkipVerify bool
}

// build assembles the *tls.Config from the TLS settings, or returns a nil config when no TLS
// customisation is requested (the client's default behaviour then applies unchanged). It fails on
// an unreadable or empty CA bundle, a half-configured client certificate pair, or an unloadable
// key pair.
func (c TLSConfig) build() (*tls.Config, error) {
	if c.CACertFile == "" && c.CertFile == "" && c.KeyFile == "" && !c.InsecureSkipVerify {
		return nil, nil
	}

	if (c.CertFile == "") != (c.KeyFile == "") {
		return nil, fmt.Errorf("opensearch tls client certificate requires both certFile and keyFile, or neither")
	}

	out := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.InsecureSkipVerify,
	}

	if c.CACertFile != "" {
		pem, err := os.ReadFile(c.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("reading opensearch CA cert file %q: %w", c.CACertFile, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("opensearch CA cert file %q contained no valid certificates", c.CACertFile)
		}

		out.RootCAs = pool
	}

	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading opensearch client certificate: %w", err)
		}

		out.Certificates = []tls.Certificate{cert}
	}

	return out, nil
}

// buildTransport chooses the HTTP transport for the OpenSearch client. A test-supplied
// cfg.Transport (a fake cluster) wins outright. Otherwise, when the TLS block requests any
// customisation, it clones the default transport - keeping its pooling, timeouts, and proxy
// behaviour - and installs the configured *tls.Config. With no TLS customisation it returns nil
// so the client keeps its own default transport, exactly as before.
func buildTransport(cfg Config) (http.RoundTripper, error) {
	if cfg.Transport != nil {
		return cfg.Transport, nil
	}

	tlsConfig, err := cfg.TLS.build()
	if err != nil {
		return nil, err
	}

	if tlsConfig == nil {
		return nil, nil
	}

	if cfg.TLS.InsecureSkipVerify {
		log.Warn("opensearch tls certificate verification is disabled (insecureSkipVerify) - do not use in production")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig

	return transport, nil
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

	// Worker tuning, resolved from Config (or the package defaults) once at construction.
	applyTimeout          time.Duration
	applyMaxAttempts      int
	applyRetryBaseBackoff time.Duration
	closeDrainTimeout     time.Duration

	closed atomic.Bool

	// indexReady records that ensureIndex has succeeded at least once, so a cluster that comes
	// up after the service does still gets the explicit mapping before the first document lands.
	indexReady atomic.Bool

	// vectorDimension is the configured embedding dimension; 0 means no vector field.
	vectorDimension int

	// vectorReady records that the live index actually carries the vector field. It is distinct
	// from vectorDimension being set, because an index created before semantic search was enabled
	// has no vector field and cannot gain one in place - so configuration alone does not make
	// k-NN queries answerable. Semantic search is refused until a --reindex rebuilds the index.
	vectorReady atomic.Bool
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

	if cfg.ApplyTimeout <= 0 {
		cfg.ApplyTimeout = applyTimeout
	}

	if cfg.ApplyMaxAttempts <= 0 {
		cfg.ApplyMaxAttempts = applyMaxAttempts
	}

	if cfg.ApplyRetryBaseBackoff <= 0 {
		cfg.ApplyRetryBaseBackoff = applyRetryBaseBackoff
	}

	if cfg.CloseDrainTimeout <= 0 {
		cfg.CloseDrainTimeout = closeDrainTimeout
	}

	transport, err := buildTransport(cfg)
	if err != nil {
		log.Errorf("invalid opensearch tls configuration: %s", err.Error())

		return nil, err
	}

	client, err := opensearchapi.NewClient(opensearchapi.Config{
		Client: opensearch.Config{
			Addresses: cfg.Addresses,
			Username:  cfg.Username,
			Password:  cfg.Password,
			Transport: transport,
		},
	})
	if err != nil {
		log.Errorf("failed to create opensearch client: %s", err.Error())

		return nil, err
	}

	o := &OpenSearch{
		client:                client,
		index:                 cfg.Index,
		queue:                 make(chan op, cfg.QueueSize),
		stop:                  make(chan struct{}),
		done:                  make(chan struct{}),
		applyTimeout:          cfg.ApplyTimeout,
		applyMaxAttempts:      cfg.ApplyMaxAttempts,
		applyRetryBaseBackoff: cfg.ApplyRetryBaseBackoff,
		closeDrainTimeout:     cfg.CloseDrainTimeout,
		vectorDimension:       cfg.VectorDimension,
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

		// The vector field is the one thing that cannot be added to an existing index, so an index
		// that predates semantic search has to be detected rather than patched.
		o.checkVectorField(ctx)

		return nil
	}

	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("failed to check for index '%s': %w", o.index, err)
	}

	if _, err := o.client.Indices.Create(ctx, opensearchapi.IndicesCreateReq{
		Index: o.index,
		Body:  strings.NewReader(o.mapping()),
	}); err != nil {
		return fmt.Errorf("failed to create index '%s': %w", o.index, err)
	}

	log.Infof("created opensearch index '%s'", o.index)

	o.indexReady.Store(true)
	o.vectorReady.Store(o.vectorDimension > 0)

	return nil
}

// mapping returns the index mapping to create with: the vector-carrying one when a dimension is
// configured, otherwise the original.
func (o *OpenSearch) mapping() string {
	if o.vectorDimension <= 0 {
		return indexMapping
	}

	return fmt.Sprintf(vectorIndexMapping, o.vectorDimension)
}

// checkVectorField determines whether an EXISTING index actually carries the vector field, and
// records the answer for Search to consult.
//
// This exists because "index.knn" is a static setting: an index created before semantic search was
// configured cannot gain a k-NN field by any in-place update, so having a dimension configured is
// not sufficient to answer a vector query. Discovering that at query time would mean a cluster
// error per search; discovering it here means one clear log line at startup naming the fix.
//
// It never fails startup. A cluster that is unreachable or slow at boot must not stop the service,
// and the same check runs again on the next ensureIndex.
func (o *OpenSearch) checkVectorField(ctx context.Context) {
	if o.vectorDimension <= 0 {
		return
	}

	resp, err := o.client.Indices.Mapping.Get(ctx, &opensearchapi.MappingGetReq{Indices: []string{o.index}})
	if err != nil {
		log.Warnf("could not read the mapping of index '%s' to check for the vector field: %s", o.index, err.Error())

		return
	}

	mapping, ok := resp.Indices[o.index]
	if !ok {
		log.Warnf("opensearch returned no mapping for index '%s'", o.index)

		return
	}

	present := mappingHasVectorField(mapping.Mappings)

	o.vectorReady.Store(present)

	if !present {
		log.Warnf(
			"opensearch index '%s' has no vector field, so semantic search is unavailable: the index predates it and cannot gain one in place (index.knn is fixed at creation) - run --backfill-search --reindex to rebuild it",
			o.index,
		)
	}
}

// mappingHasVectorField reports whether an index's mapping JSON describes a knn_vector "vector"
// property. Split out so the parsing is testable without a cluster.
//
// It answers false on anything it cannot parse, which is the safe direction: refusing semantic
// search on an index that does support it costs a clear log line and a --reindex, where allowing
// it on one that does not would fail every query at the cluster.
func mappingHasVectorField(raw json.RawMessage) bool {
	var mapping struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(raw, &mapping); err != nil {
		return false
	}

	return mapping.Properties["vector"].Type == "knn_vector"
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

// applyWithRetry applies one operation, retrying a transient failure up to applyMaxAttempts times
// with a jittered exponential backoff before giving up. Only when every attempt fails is the
// operation dropped (logged and counted), rather than on the first hiccup as before - so a brief
// cluster blip no longer silently loses a write. A drop is still recoverable: the reconciliation
// sweep and --backfill-search re-index whatever was missed. The backoff waits abort as soon as the
// index is closing so shutdown is not delayed.
func (o *OpenSearch) applyWithRetry(v op) {
	var err error

RETRY:
	for attempt := range o.applyMaxAttempts {
		err = o.applyOnce(v)
		if err == nil {
			return
		}

		if attempt == o.applyMaxAttempts-1 {
			break RETRY
		}

		backoff := o.applyRetryBaseBackoff*time.Duration(1<<attempt) + time.Duration(rand.Int63n(int64(o.applyRetryBaseBackoff)))

		select {

		case <-o.stop:
			// Draining at shutdown: do not sleep out the backoff. Give up on this attempt now; the
			// reconcile sweep or --backfill-search heals anything left behind.
			break RETRY

		case <-time.After(backoff):
		}
	}

	log.Warnf("dropping opensearch %s operation after %d attempts: %s", v.kind, o.applyMaxAttempts, err.Error())
	tel.dropped.Add(context.Background(), 1, metric.WithAttributes(attribute.String("op", v.kind.String())))
}

// applyOnce runs a single attempt at one operation: it makes sure the index exists, then applies
// the operation, each bounded by applyTimeout. It returns the error (nil on success) so
// applyWithRetry can decide whether to retry.
func (o *OpenSearch) applyOnce(v op) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.applyTimeout)
	defer cancel()

	if !o.indexReady.Load() {
		if err := o.ensureIndex(ctx); err != nil {
			return fmt.Errorf("index not ready: %w", err)
		}
	}

	return o.apply(ctx, v)
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

	ctx, cancel := context.WithTimeout(ctx, o.applyTimeout)
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

	ctx, cancel := context.WithTimeout(ctx, o.applyTimeout)
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
func (o *OpenSearch) Search(ctx context.Context, query Query) ([]Hit, error) {
	log.Trace("func() search.Search")

	// A vector query needs the field to actually exist on the live index, which configuration alone
	// does not guarantee - see checkVectorField.
	if len(query.Vector) > 0 && !o.vectorReady.Load() {
		return nil, ErrSemanticUnavailable
	}

	// Build the whole request as a map and marshal it once, so query.Text, EventId, and Group are
	// all escaped correctly by json.Marshal - fmt's %q would emit escapes (\a, \v, \x07, ...) that
	// JSON rejects, and a crafted value could otherwise alter the query structure.
	var filters []any

	if query.EventId != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"event_id": query.EventId}})
	}

	if query.Group != "" {
		filters = append(filters, map[string]any{"term": map[string]any{"group": query.Group}})
	}

	body, err := json.Marshal(map[string]any{
		"query":   o.searchQuery(query, filters),
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

	// _score is already higher-is-better, which is Hit's convention, so it needs no adjustment -
	// unlike the SQL backend's bm25 rank.
	hits := make([]Hit, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		hits = append(hits, Hit{Id: hit.ID, Score: float64(hit.Score)})
	}

	return hits, nil
}

// searchQuery builds the query clause: a k-NN vector search when the caller supplied a vector,
// otherwise the keyword match. The two are never combined into one clause - a bool query summing a
// bm25 score and a cosine similarity would be adding numbers on unrelated scales - so hybrid search
// runs both separately and fuses their rankings above this package.
func (o *OpenSearch) searchQuery(query Query, filters []any) map[string]any {
	if len(query.Vector) > 0 {
		knn := map[string]any{
			"vector": query.Vector,
			"k":      query.Limit,
		}

		// The lucene engine supports filtering during the k-NN traversal, so the filter narrows the
		// search rather than discarding results after it - which is what stops a group-scoped
		// semantic search from returning fewer hits than asked for.
		if len(filters) > 0 {
			knn["filter"] = map[string]any{"bool": map[string]any{"filter": filters}}
		}

		return map[string]any{"knn": map[string]any{"vector": knn}}
	}

	boolQuery := map[string]any{
		"must": []any{
			map[string]any{"match": map[string]any{"body": query.Text}},
		},
	}

	if len(filters) > 0 {
		boolQuery["filter"] = filters
	}

	return map[string]any{"bool": boolQuery}
}

// SupportsVectors reports whether this index can answer a semantic query: a dimension is configured
// AND the live index actually carries the vector field.
func (o *OpenSearch) SupportsVectors() bool {
	return o.vectorDimension > 0 && o.vectorReady.Load()
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

	case <-time.After(o.closeDrainTimeout):
		return fmt.Errorf("timed out draining the opensearch queue")
	}
}

// Compile-time check that *OpenSearch satisfies Index.
var _ Index = (*OpenSearch)(nil)
