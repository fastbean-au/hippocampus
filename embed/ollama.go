package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultTimeout bounds one embedding call. Embedding is far cheaper than generation - it is a
	// single forward pass with no token-by-token decoding - so this is much tighter than the
	// summariser's two minutes. It has to be: embedding sits on the write path, where the
	// summariser does not.
	defaultTimeout = 30 * time.Second

	// defaultBatchSize caps how many texts go in one request to the model server. Batching matters
	// for a backfill over a whole store, where one request per memory would be dominated by round
	// trips; the cap keeps a single request from growing unboundedly and timing out as a unit,
	// which would lose the whole batch rather than one memory's worth of work.
	defaultBatchSize = 32

	// defaultMaxTextBytes caps the length of one text sent for embedding. Every model has a context
	// window and silently truncates past it, so a body longer than this is truncated here instead -
	// where it can be documented and reasoned about, rather than at a boundary the service cannot
	// see. Bodies this long are also the ones whose embedding least represents them.
	defaultMaxTextBytes = 8192
)

// Config configures the Ollama-backed embedder. Address and Model are required; the rest take
// defaults when zero.
type Config struct {
	// Address is the base URL of the Ollama server, e.g. "http://localhost:11434".
	Address string

	// Model is the Ollama embedding model tag, e.g. "nomic-embed-text". It must be an embedding
	// model rather than a generation model, and it must not change without re-embedding the store:
	// vectors from two models are not comparable, and usually do not even share a dimension count.
	Model string

	// Timeout bounds one embedding HTTP call (0 -> defaultTimeout).
	Timeout time.Duration

	// BatchSize caps texts per request (0 -> defaultBatchSize).
	BatchSize int

	// MaxTextBytes truncates any single text before sending (0 -> defaultMaxTextBytes).
	MaxTextBytes int

	// Dimensions is the vector width the model is expected to produce. It is not a request - the
	// model produces whatever it produces - but a check, because the width has to be committed to
	// elsewhere before any vector exists: the OpenSearch k-NN mapping fixes it at index creation.
	// Validating here turns a configuration mistake into one clear error naming both numbers,
	// instead of a cluster rejecting every document for a reason the operator has to work out.
	// Zero disables the check.
	Dimensions int
}

// Ollama is an Embedder backed by a running Ollama server, reached over its /api/embed endpoint.
// It holds no state beyond the resolved config and an http.Client, so one instance is safe for
// concurrent use.
//
// Like the summariser, this is a small hand-rolled client rather than an Ollama SDK: the endpoint
// is one JSON POST, and the alternative is a dependency tree for a request struct with three
// fields.
type Ollama struct {
	address      string
	model        string
	batchSize    int
	maxTextBytes int
	dimensions   int
	client       *http.Client
}

// embedRequest is the body of an Ollama POST /api/embed call. Input takes a list, which is what
// makes batching a single round trip.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the response from /api/embed. Embeddings comes back in input order. Error
// carries a server-side message (an unknown model, most commonly) that can accompany a 200.
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// NewOllama builds an Ollama embedder from cfg, applying defaults for the optional fields.
//
// It fails only on unusable configuration - a missing address or model - and makes no connectivity
// check: an unreachable server must not prevent startup, since embedding is optional and
// best-effort. The consequence is that a wrong address or a model that is not installed surfaces
// on first use rather than at boot, which is the same trade the summariser makes.
func NewOllama(cfg Config) (*Ollama, error) {
	address := strings.TrimRight(strings.TrimSpace(cfg.Address), "/")

	if address == "" {
		return nil, fmt.Errorf("ollama.embedding.address must be set when embedding is enabled")
	}

	model := strings.TrimSpace(cfg.Model)

	if model == "" {
		return nil, fmt.Errorf("ollama.embedding.model must be set when embedding is enabled")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	maxTextBytes := cfg.MaxTextBytes
	if maxTextBytes <= 0 {
		maxTextBytes = defaultMaxTextBytes
	}

	o := &Ollama{
		address:      address,
		model:        model,
		batchSize:    batchSize,
		maxTextBytes: maxTextBytes,
		dimensions:   cfg.Dimensions,
		client:       &http.Client{Timeout: timeout},
	}

	return o, nil
}

// Embed returns one vector per input text, in the same order, batching requests at BatchSize.
//
// It is all-or-nothing: a failed batch fails the whole call rather than returning a partial result
// with holes in it. A caller storing these has to know which memory each vector belongs to, and a
// short or gappy result silently misaligns that mapping - the kind of bug that surfaces much later
// as inexplicably wrong search results.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([]Vector, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	vectors := make([]Vector, 0, len(texts))

	for start := 0; start < len(texts); start += o.batchSize {
		end := min(start+o.batchSize, len(texts))

		batch, err := o.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}

		vectors = append(vectors, batch...)
	}

	return vectors, nil
}

// embedBatch sends one request and returns its vectors.
func (o *Ollama) embedBatch(ctx context.Context, texts []string) ([]Vector, error) {
	input := make([]string, 0, len(texts))

	for _, text := range texts {
		input = append(input, o.truncate(text))
	}

	buf, err := json.Marshal(embedRequest{Model: o.model, Input: input})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ollama embed request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.address+"/api/embed", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("failed to build ollama embed request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama embed request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read ollama embed response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to decode ollama embed response: %w", err)
	}

	if out.Error != "" {
		return nil, fmt.Errorf("ollama error: %s", out.Error)
	}

	// A count mismatch means the response cannot be aligned to the inputs at all, so there is no
	// safe partial answer to return - see the all-or-nothing note on Embed.
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama returned %d embeddings for %d inputs", len(out.Embeddings), len(texts))
	}

	vectors := make([]Vector, 0, len(out.Embeddings))

	for i, values := range out.Embeddings {
		if len(values) == 0 {
			return nil, fmt.Errorf("ollama returned an empty embedding at position %d", i)
		}

		if o.dimensions > 0 && len(values) != o.dimensions {
			return nil, fmt.Errorf(
				"model '%s' produced %d-dimension vectors but %d is configured: set ollama.embedding.dimensions to match the model, and rebuild the index (--backfill-search --reindex) since its vector width is fixed at creation",
				o.model,
				len(values),
				o.dimensions,
			)
		}

		vectors = append(vectors, Vector(values))
	}

	return vectors, nil
}

// truncate caps one text at maxTextBytes, cutting on a rune boundary so the request stays valid
// UTF-8 - a body split mid-rune would be rejected by json.Marshal's replacement behaviour or
// tokenise as garbage.
func (o *Ollama) truncate(text string) string {
	if len(text) <= o.maxTextBytes {
		return text
	}

	cut := o.maxTextBytes

	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}

	return text[:cut]
}

// utf8RuneStart reports whether b begins a UTF-8 rune (i.e. is not a continuation byte).
func utf8RuneStart(b byte) bool {
	return b&0xC0 != 0x80
}

// Model reports the model tag vectors are produced with.
func (o *Ollama) Model() string {
	return o.model
}

// Dimensions reports the configured vector width, or 0 when unset.
func (o *Ollama) Dimensions() int {
	return o.dimensions
}

// Enabled reports that a real embedder is configured.
func (o *Ollama) Enabled() bool {
	return true
}

// ErrDegraded marks a dependency that answered but reported a problem of its own, as distinct from
// one that could not be reached at all. See summarise.ErrDegraded, which this deliberately mirrors:
// each optional integration stays self-contained rather than sharing a package for one sentinel,
// and hippocampus/topology_probe.go maps all of them onto the one status.
var ErrDegraded = errors.New("dependency is degraded")

// Ping reports whether the Ollama server is reachable and carries the configured embedding model,
// for the deployment topology view. It is not part of the Embedder interface; callers assert for it
// optionally.
//
// A missing model is degraded rather than unreachable, for the reason the summariser's Ping gives -
// and it matters more here, because an embedder that cannot embed does not fail loudly: writes keep
// succeeding (indexing is best-effort and asynchronous) and only semantic search quietly returns
// nothing.
func (o *Ollama) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.address+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("failed to build the request: %w", err)
	}

	res, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama is unreachable at %s: %w", o.address, err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("ollama at %s returned %s", o.address, res.Status)
	}

	var tags tagsResponse

	if err := json.NewDecoder(res.Body).Decode(&tags); err != nil {
		return fmt.Errorf("failed to read the model list from %s: %w", o.address, err)
	}

	if !tags.has(o.model) {
		return fmt.Errorf("%w: model %q is not installed on the ollama server at %s", ErrDegraded, o.model, o.address)
	}

	return nil
}

// tagsResponse is the response from Ollama's GET /api/tags: the models the server has pulled.
type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// has reports whether the server carries a model, comparing tags the way Ollama itself resolves
// them: an untagged name means ":latest", so a configuration of "nomic-embed-text" is satisfied by
// an installed "nomic-embed-text:latest" and vice versa.
func (t tagsResponse) has(model string) bool {
	want := strings.TrimSuffix(model, ":latest")

	for _, installed := range t.Models {
		if strings.TrimSuffix(installed.Name, ":latest") == want {
			return true
		}
	}

	return false
}

// Compile-time check that *Ollama satisfies Embedder.
var _ Embedder = (*Ollama)(nil)
