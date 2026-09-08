package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAI is an Embedder backed by any endpoint speaking the OpenAI embeddings API - the same set
// the summariser's OpenAI client covers, and for the same reason: a deployment that already has a
// sanctioned model endpoint should not have to run a second one to get semantic search.
//
// The two halves stay independent. Nothing requires the summariser and the embedder to share a
// provider, an address or a key, and a deployment generating against a hosted model while embedding
// against a local Ollama is both reasonable and cheaper.
type OpenAI struct {
	address      string
	model        string
	apiKey       string
	batchSize    int
	maxTextBytes int
	dimensions   int
	client       *http.Client
}

// openAIEmbedRequest is the body of a POST {address}/embeddings call. Input takes a list, which is
// what makes batching a single round trip.
//
// Dimensions is sent only when configured AND supported - see the note on NewOpenAI. It is
// omitempty because a zero would be rejected outright, and because most models do not accept the
// field at all.
type openAIEmbedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

// openAIEmbedResponse is the response from /embeddings. Data comes back with an explicit Index per
// entry rather than in guaranteed input order, which is the one place this API differs from
// Ollama's in a way that matters.
type openAIEmbedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`

	Error *apiError `json:"error"`
}

// apiError is the OpenAI error envelope.
type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// String renders the envelope for an error message, preferring the human-readable message.
func (e *apiError) String() string {
	if e == nil {
		return ""
	}

	if e.Message != "" {
		return e.Message
	}

	return strings.TrimSpace(e.Type + " " + e.Code)
}

// NewOpenAI builds an OpenAI-compatible embedder from cfg, applying the same defaults NewOllama
// does, and makes no connectivity check for the same reason.
//
// Address is the base the endpoint path is appended to and is taken exactly as given, so it normally
// ends in /v1 - see summarise.NewOpenAI, which documents why it is not guessed at.
//
// Dimensions keeps the meaning it has for the Ollama embedder: a check on what the model produces,
// not a request. It is additionally SENT as the API's optional `dimensions` parameter, which the
// Matryoshka-trained models (text-embedding-3 and its kin) honour by truncating their output. That
// is safe in both directions - a model that ignores the field returns its native width and the
// existing check catches the mismatch - and it is what lets an operator match an index created at
// 768 without changing model.
func NewOpenAI(cfg Config) (*OpenAI, error) {
	address := strings.TrimRight(strings.TrimSpace(cfg.Address), "/")

	if address == "" {
		return nil, fmt.Errorf("llm.embedding.address must be set when embedding is enabled")
	}

	model := strings.TrimSpace(cfg.Model)

	if model == "" {
		return nil, fmt.Errorf("llm.embedding.model must be set when embedding is enabled")
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

	o := &OpenAI{
		address:      address,
		model:        model,
		apiKey:       strings.TrimSpace(cfg.APIKey),
		batchSize:    batchSize,
		maxTextBytes: maxTextBytes,
		dimensions:   cfg.Dimensions,
		client:       &http.Client{Timeout: timeout},
	}

	return o, nil
}

// Embed returns one vector per input text, in the same order, batching requests at BatchSize. It is
// all-or-nothing for the reason the Ollama embedder's Embed documents: a short or gappy result
// silently misaligns the caller's vector-to-memory mapping.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([]Vector, error) {
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

// embedBatch sends one request and returns its vectors, in input order.
func (o *OpenAI) embedBatch(ctx context.Context, texts []string) ([]Vector, error) {
	input := make([]string, 0, len(texts))

	for _, text := range texts {
		input = append(input, truncate(text, o.maxTextBytes))
	}

	buf, err := json.Marshal(openAIEmbedRequest{Model: o.model, Input: input, Dimensions: o.dimensions})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal the embeddings request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.address+"/embeddings", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("failed to build the embeddings request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	setBearer(httpReq, o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embeddings request to %s failed: %w", o.address, err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read the embeddings response: %w", err)
	}

	var out openAIEmbedResponse

	decodeErr := json.Unmarshal(raw, &out)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError(o.address, resp.StatusCode, out.Error, raw, o.apiKey != "")
	}

	if decodeErr != nil {
		return nil, fmt.Errorf("failed to decode the embeddings response: %w", decodeErr)
	}

	if out.Error != nil {
		return nil, fmt.Errorf("the model server at %s reported an error: %s", o.address, out.Error)
	}

	// A count mismatch means the response cannot be aligned to the inputs at all, so there is no
	// safe partial answer to return - see the all-or-nothing note on Embed.
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("the model server at %s returned %d embeddings for %d inputs", o.address, len(out.Data), len(texts))
	}

	// Placed by Index rather than appended in arrival order. The API documents an index per entry
	// precisely because the order is not guaranteed, and a batch returned out of order would
	// mis-assign every vector in it - silently, and in a way that only ever surfaces much later as
	// search results that are wrong rather than absent.
	vectors := make([]Vector, len(out.Data))

	for _, entry := range out.Data {
		if entry.Index < 0 || entry.Index >= len(vectors) {
			return nil, fmt.Errorf("the model server at %s returned an out-of-range embedding index %d", o.address, entry.Index)
		}

		if vectors[entry.Index] != nil {
			return nil, fmt.Errorf("the model server at %s returned embedding index %d twice", o.address, entry.Index)
		}

		if len(entry.Embedding) == 0 {
			return nil, fmt.Errorf("the model server at %s returned an empty embedding at position %d", o.address, entry.Index)
		}

		if o.dimensions > 0 && len(entry.Embedding) != o.dimensions {
			return nil, fmt.Errorf(
				"model '%s' produced %d-dimension vectors but %d is configured: set llm.embedding.dimensions to match the model, and rebuild the index (--backfill-search --reindex) since its vector width is fixed at creation",
				o.model,
				len(entry.Embedding),
				o.dimensions,
			)
		}

		vectors[entry.Index] = Vector(entry.Embedding)
	}

	return vectors, nil
}

// Model reports the model tag vectors are produced with.
func (o *OpenAI) Model() string {
	return o.model
}

// Dimensions reports the configured vector width, or 0 when unset.
func (o *OpenAI) Dimensions() int {
	return o.dimensions
}

// Enabled reports that a real embedder is configured.
func (o *OpenAI) Enabled() bool {
	return true
}

// Ping reports whether the endpoint is reachable and serves the configured model, for the
// deployment topology view. It mirrors summarise.OpenAI.Ping exactly, including its tolerance of an
// endpoint that does not implement /models and its refusal to treat a rejected key as merely
// degraded; see that method for the reasoning.
//
// It matters more here than for the summariser, for the reason the Ollama probe gives: an embedder
// that cannot embed does not fail loudly. Writes keep succeeding, because indexing is best-effort
// and asynchronous, and only semantic search quietly returns nothing.
func (o *OpenAI) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.address+"/models", nil)
	if err != nil {
		return fmt.Errorf("failed to build the request: %w", err)
	}

	setBearer(req, o.apiKey)

	res, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("the model server is unreachable at %s: %w", o.address, err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}

	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))

		return statusError(o.address, res.StatusCode, nil, raw, o.apiKey != "")
	}

	var list modelsResponse

	if err := json.NewDecoder(res.Body).Decode(&list); err != nil || len(list.Data) == 0 {
		return nil
	}

	if !list.has(o.model) {
		return fmt.Errorf("%w: model %q is not served by the endpoint at %s", ErrDegraded, o.model, o.address)
	}

	return nil
}

// modelsResponse is the response from GET {address}/models.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// has reports whether the endpoint serves a model, comparing exactly - there is no ":latest"
// convention here, unlike Ollama's native API.
func (m modelsResponse) has(model string) bool {
	for _, served := range m.Data {
		if served.ID == model {
			return true
		}
	}

	return false
}

// setBearer adds an Authorization header when a key is configured. Shared with the Ollama client,
// which sends it too when one is set - see Config.APIKey.
func setBearer(req *http.Request, apiKey string) {
	if apiKey == "" {
		return
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
}

// statusError renders a non-2xx response, preferring a decoded error envelope over the raw body and
// naming the likely cause for the two status codes that have one. See summarise.statusError, which
// this mirrors; each optional integration stays self-contained rather than sharing a package for a
// pair of helpers.
func statusError(address string, code int, envelope *apiError, raw []byte, haveKey bool) error {
	detail := envelope.String()
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}

	switch code {

	case http.StatusUnauthorized, http.StatusForbidden:
		if !haveKey {
			return fmt.Errorf("the model server at %s rejected the request with %d and no llm.embedding.apiKey is set: %s", address, code, detail)
		}

		return fmt.Errorf("the model server at %s rejected the configured llm.embedding.apiKey with %d: %s", address, code, detail)

	}

	return fmt.Errorf("the model server at %s returned status %d: %s", address, code, detail)
}

// Compile-time check that *OpenAI satisfies Embedder.
var _ Embedder = (*OpenAI)(nil)
