package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeOllama stands in for an Ollama server. It records every request it received so a test can
// assert on batching and truncation, and returns a vector per input.
type fakeOllama struct {
	requests []embedRequest
	dims     int
}

// handler returns an http.Handler answering /api/embed with dims-wide vectors.
func (f *fakeOllama) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("request to %q, want /api/embed", r.URL.Path)
		}

		var req embedRequest

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding the request: %s", err)
		}

		f.requests = append(f.requests, req)

		dims := f.dims
		if dims == 0 {
			dims = 3
		}

		embeddings := make([][]float32, 0, len(req.Input))

		for i := range req.Input {
			vector := make([]float32, dims)
			for d := range vector {
				vector[d] = float32(i + d + 1)
			}

			embeddings = append(embeddings, vector)
		}

		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: embeddings})
	})
}

// newTestOllama wires an Ollama embedder to a fake server, returning both.
func newTestOllama(t *testing.T, cfg Config) (*Ollama, *fakeOllama) {
	t.Helper()

	fake := &fakeOllama{}
	srv := httptest.NewServer(fake.handler(t))

	t.Cleanup(srv.Close)

	cfg.Address = srv.URL

	if cfg.Model == "" {
		cfg.Model = "nomic-embed-text"
	}

	embedder, err := NewOllama(cfg)
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	return embedder, fake
}

func TestNewOllamaRequiresAddressAndModel(t *testing.T) {
	if _, err := NewOllama(Config{Model: "nomic-embed-text"}); err == nil {
		t.Error("expected an error for a missing address")
	}

	if _, err := NewOllama(Config{Address: "http://localhost:11434"}); err == nil {
		t.Error("expected an error for a missing model")
	}

	// Whitespace-only is missing, not present-but-blank.
	if _, err := NewOllama(Config{Address: "  ", Model: "  "}); err == nil {
		t.Error("expected an error for whitespace-only configuration")
	}
}

// An unreachable server must not fail construction: embedding is optional and best-effort, so a
// model server that is down cannot be allowed to stop the service starting.
func TestNewOllamaDoesNotCheckConnectivity(t *testing.T) {
	embedder, err := NewOllama(Config{Address: "http://127.0.0.1:1", Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama against an unreachable address: %s", err)
	}

	if !embedder.Enabled() {
		t.Error("a constructed embedder reports itself disabled")
	}
}

func TestNewOllamaTrimsTrailingSlash(t *testing.T) {
	embedder, err := NewOllama(Config{Address: "http://localhost:11434/", Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if embedder.address != "http://localhost:11434" {
		t.Errorf("address %q, want the trailing slash trimmed", embedder.address)
	}
}

func TestOllamaEmbedHappyPath(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{})

	vectors, err := embedder.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if len(vectors) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vectors))
	}

	for i, vector := range vectors {
		if len(vector) != 3 {
			t.Errorf("vector %d has %d dimensions, want 3", i, len(vector))
		}
	}

	if len(fake.requests) != 1 {
		t.Fatalf("the server saw %d requests, want 1", len(fake.requests))
	}

	if got := fake.requests[0].Model; got != "nomic-embed-text" {
		t.Errorf("request carried model %q, want nomic-embed-text", got)
	}

	if got := fake.requests[0].Input; len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("request carried input %v, want [first second]", got)
	}
}

// Model() is part of the contract because a stored vector is meaningless without knowing which
// model produced it.
func TestOllamaReportsItsModel(t *testing.T) {
	embedder, _ := newTestOllama(t, Config{Model: "mxbai-embed-large"})

	if embedder.Model() != "mxbai-embed-large" {
		t.Errorf("Model() = %q, want mxbai-embed-large", embedder.Model())
	}
}

// Embedding nothing is not an error - a caller with no indexable bodies should not have to
// special-case the call.
func TestOllamaEmbedNoTexts(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{})

	vectors, err := embedder.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed(nil): %s", err)
	}

	if len(vectors) != 0 {
		t.Errorf("got %d vectors for no input, want 0", len(vectors))
	}

	if len(fake.requests) != 0 {
		t.Errorf("the server saw %d requests for no input, want 0", len(fake.requests))
	}
}

// Batching is what makes a whole-store backfill viable; the order across batches must still match
// the input, since the caller maps vectors back to memories positionally.
func TestOllamaEmbedBatches(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{BatchSize: 2})

	texts := []string{"a", "b", "c", "d", "e"}

	vectors, err := embedder.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if len(vectors) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(vectors), len(texts))
	}

	if len(fake.requests) != 3 {
		t.Fatalf("the server saw %d requests, want 3 (2+2+1)", len(fake.requests))
	}

	var seen []string
	for _, req := range fake.requests {
		seen = append(seen, req.Input...)
	}

	for i := range texts {
		if seen[i] != texts[i] {
			t.Errorf("batching reordered the input: got %v, want %v", seen, texts)

			break
		}
	}
}

// A text longer than the model's context is truncated here rather than silently by the model, and
// on a rune boundary so the request stays valid UTF-8.
func TestOllamaEmbedTruncatesLongText(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{MaxTextBytes: 10})

	if _, err := embedder.Embed(context.Background(), []string{strings.Repeat("x", 50)}); err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if got := len(fake.requests[0].Input[0]); got != 10 {
		t.Errorf("sent %d bytes, want it truncated to 10", got)
	}
}

func TestOllamaEmbedTruncatesOnRuneBoundary(t *testing.T) {
	// Three-byte runes, so a 10-byte cap lands mid-rune and must fall back to 9.
	embedder, fake := newTestOllama(t, Config{MaxTextBytes: 10})

	if _, err := embedder.Embed(context.Background(), []string{strings.Repeat("€", 10)}); err != nil {
		t.Fatalf("Embed: %s", err)
	}

	sent := fake.requests[0].Input[0]

	if len(sent) != 9 {
		t.Errorf("sent %d bytes, want 9 (cut back to the rune boundary)", len(sent))
	}

	if !isValidUTF8(sent) {
		t.Errorf("truncation produced invalid UTF-8: %q", sent)
	}
}

// isValidUTF8 reports whether s is well-formed UTF-8, without importing unicode/utf8 into the
// assertion (which would just re-run the implementation's own helper).
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}

	return true
}

func TestOllamaEmbedServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("model server exploded"))
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"anything"})
	if err == nil {
		t.Fatal("expected the server error to surface")
	}

	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention the status code", err)
	}
}

// Ollama reports some failures (an uninstalled model, most often) in the body of a 200, so the
// error field has to be checked as well as the status.
func TestOllamaEmbedModelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Error: `model "nomic-embed-text" not found`})
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if _, err := embedder.Embed(context.Background(), []string{"anything"}); err == nil {
		t.Fatal("expected the model error to surface despite the 200")
	}
}

// A response that cannot be aligned to the inputs has no safe partial interpretation: returning
// the short list would silently misalign every vector after the gap against its memory.
func TestOllamaEmbedRejectsCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2, 3}}})
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	_, err = embedder.Embed(context.Background(), []string{"one", "two"})
	if err == nil {
		t.Fatal("expected a count mismatch to be rejected")
	}

	if !strings.Contains(err.Error(), "1 embeddings for 2 inputs") {
		t.Errorf("error %q does not describe the mismatch", err)
	}
}

func TestOllamaEmbedRejectsEmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{}}})
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if _, err := embedder.Embed(context.Background(), []string{"anything"}); err == nil {
		t.Fatal("expected an empty vector to be rejected")
	}
}

func TestOllamaEmbedMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not json"))
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if _, err := embedder.Embed(context.Background(), []string{"anything"}); err == nil {
		t.Fatal("expected a malformed response to be rejected")
	}
}

// A failing batch must fail the whole call rather than returning what it managed - see the
// all-or-nothing note on Embed.
func TestOllamaEmbedFailsWholeCallOnALaterBatch(t *testing.T) {
	var seen int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++

		if seen > 1 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2, 3}}})
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text", BatchSize: 1})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	vectors, err := embedder.Embed(context.Background(), []string{"one", "two"})
	if err == nil {
		t.Fatal("expected the failing second batch to fail the call")
	}

	if vectors != nil {
		t.Errorf("got %d vectors alongside the error, want none", len(vectors))
	}
}

// The caller's deadline has to reach the HTTP call, or a slow model server would outlive the RPC
// that triggered it.
func TestOllamaEmbedRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {

		case <-r.Context().Done():

		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()

	if _, err := embedder.Embed(ctx, []string{"anything"}); err == nil {
		t.Fatal("expected the cancelled context to fail the call")
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the call took %s; the context deadline did not reach the HTTP request", elapsed)
	}
}

// The embedder's own timeout bounds a call the caller did not bound.
func TestOllamaEmbedAppliesItsOwnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {

		case <-r.Context().Done():

		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{
		Address: srv.URL,
		Model:   "nomic-embed-text",
		Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	started := time.Now()

	if _, err := embedder.Embed(context.Background(), []string{"anything"}); err == nil {
		t.Fatal("expected the embedder's own timeout to fail the call")
	}

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("the call took %s, past the configured 50ms timeout", elapsed)
	}
}

// The vectors a real model returns are what a search would compare, so a round trip through this
// client must preserve them intact rather than reordering or reshaping.
func TestOllamaEmbedPreservesVectorValues(t *testing.T) {
	want := [][]float32{{0.1, -0.2, 0.3}, {-1, 0, 1}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: want})
	}))
	defer srv.Close()

	embedder, err := NewOllama(Config{Address: srv.URL, Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	vectors, err := embedder.Embed(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	for i := range want {
		for d := range want[i] {
			if vectors[i][d] != want[i][d] {
				t.Errorf("vector %d dimension %d = %f, want %f", i, d, vectors[i][d], want[i][d])
			}
		}
	}

	// And they must be usable by the comparison a search would sort on.
	if got := Cosine(vectors[0], vectors[0]); math.Abs(got-1) > 1e-6 {
		t.Errorf("a returned vector is not self-similar: Cosine = %f, want 1", got)
	}
}

// A defaulted config must produce a working embedder, so the defaults are exercised rather than
// merely declared.
func TestOllamaDefaultsAreApplied(t *testing.T) {
	embedder, _ := newTestOllama(t, Config{})

	if embedder.batchSize != defaultBatchSize {
		t.Errorf("batchSize %d, want the default %d", embedder.batchSize, defaultBatchSize)
	}

	if embedder.maxTextBytes != defaultMaxTextBytes {
		t.Errorf("maxTextBytes %d, want the default %d", embedder.maxTextBytes, defaultMaxTextBytes)
	}

	if embedder.client.Timeout != defaultTimeout {
		t.Errorf("timeout %s, want the default %s", embedder.client.Timeout, defaultTimeout)
	}
}

func TestOllamaEmbedUnreachableServer(t *testing.T) {
	embedder, err := NewOllama(Config{Address: "http://127.0.0.1:1", Model: "nomic-embed-text"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if _, err := embedder.Embed(context.Background(), []string{"anything"}); err == nil {
		t.Fatal("expected an unreachable server to fail the call")
	}
}

// A dimension count differing from the store's is the failure mode a model change causes, and the
// client must at least report the dimensions faithfully so a caller can detect it.
func TestOllamaEmbedReportsModelDimensions(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{})
	fake.dims = 768

	vectors, err := embedder.Embed(context.Background(), []string{"anything"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if len(vectors[0]) != 768 {
		t.Errorf("got %d dimensions, want the model's 768", len(vectors[0]))
	}

	if fmt.Sprint(len(vectors)) != "1" {
		t.Errorf("got %d vectors, want 1", len(vectors))
	}
}

// The vector width has to be committed to before any vector exists - the k-NN mapping fixes it at
// index creation - so a mismatch is a configuration error worth naming precisely rather than
// letting the cluster reject every document.
func TestOllamaEmbedRejectsWrongDimensions(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{Dimensions: 768})
	fake.dims = 384

	_, err := embedder.Embed(context.Background(), []string{"anything"})
	if err == nil {
		t.Fatal("expected a dimension mismatch to be rejected")
	}

	for _, want := range []string{"384", "768", "--backfill-search --reindex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestOllamaEmbedAcceptsMatchingDimensions(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{Dimensions: 384})
	fake.dims = 384

	vectors, err := embedder.Embed(context.Background(), []string{"anything"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if len(vectors[0]) != 384 {
		t.Errorf("got %d dimensions, want 384", len(vectors[0]))
	}

	if embedder.Dimensions() != 384 {
		t.Errorf("Dimensions() = %d, want 384", embedder.Dimensions())
	}
}

// Zero disables the check, so a deployment that does not know its model's width still works.
func TestOllamaEmbedSkipsTheDimensionCheckWhenUnset(t *testing.T) {
	embedder, fake := newTestOllama(t, Config{})
	fake.dims = 1024

	if _, err := embedder.Embed(context.Background(), []string{"anything"}); err != nil {
		t.Fatalf("Embed with no configured dimension: %s", err)
	}
}
