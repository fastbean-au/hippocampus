package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewOpenAIEmbedder_RequiresAddressAndModel verifies construction rejects a missing address or
// model, trims a trailing slash, and applies the optional defaults.
func TestNewOpenAIEmbedder_RequiresAddressAndModel(t *testing.T) {
	if _, err := NewOpenAI(Config{Model: "m"}); err == nil {
		t.Error("expected error for missing address")
	}

	if _, err := NewOpenAI(Config{Address: "https://api.openai.com/v1"}); err == nil {
		t.Error("expected error for missing model")
	}

	o, err := NewOpenAI(Config{Address: "https://api.openai.com/v1/", Model: "text-embedding-3-small"})
	if err != nil {
		t.Fatalf("NewOpenAI: %s", err)
	}

	if o.address != "https://api.openai.com/v1" {
		t.Errorf("trailing slash not trimmed: %q", o.address)
	}

	if o.batchSize != defaultBatchSize || o.maxTextBytes != defaultMaxTextBytes {
		t.Error("defaults were not applied to the optional fields")
	}

	if !o.Enabled() || o.Model() != "text-embedding-3-small" {
		t.Error("Enabled()/Model() do not report the configured embedder")
	}
}

// TestOpenAIEmbed_HappyPath verifies the request shape - path, model, the inputs, the dimensions
// parameter and the bearer token - and that vectors come back in input order.
func TestOpenAIEmbed_HappyPath(t *testing.T) {
	var (
		gotReq  openAIEmbedRequest
		gotAuth string
		gotPath string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotReq); err != nil {
			t.Errorf("decode request: %s", err)
		}

		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}]}`)
	}))

	defer srv.Close()

	o, _ := NewOpenAI(Config{Address: srv.URL + "/v1", Model: "text-embedding-3-small", APIKey: "sk-test", Dimensions: 2})

	vectors, err := o.Embed(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if len(vectors) != 2 || vectors[0][0] != 1 || vectors[1][0] != 3 {
		t.Errorf("vectors = %v, want them in input order", vectors)
	}

	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q, want the endpoint appended to the configured base", gotPath)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the configured key as a bearer token", gotAuth)
	}

	if gotReq.Model != "text-embedding-3-small" || len(gotReq.Input) != 2 {
		t.Errorf("request = %+v, want the configured model and both inputs", gotReq)
	}

	// Sent so a Matryoshka model can be truncated to match an index created at another width.
	if gotReq.Dimensions != 2 {
		t.Errorf("dimensions = %d, want the configured width to be sent", gotReq.Dimensions)
	}
}

// TestOpenAIEmbed_DimensionsOmittedWhenUnset verifies an unset width is left out of the request
// rather than sent as 0, which every provider rejects.
func TestOpenAIEmbed_DimensionsOmittedWhenUnset(t *testing.T) {
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1]}]}`)
	}))

	defer srv.Close()

	o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m"})

	if _, err := o.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if _, present := body["dimensions"]; present {
		t.Error("dimensions was sent despite being unset")
	}
}

// TestOpenAIEmbed_PlacesByIndex is the one behaviour that genuinely differs from the Ollama client:
// this API returns an explicit index per entry and does not promise input order, so a response
// arriving out of order must still align to its inputs. Appending in arrival order would
// mis-assign every vector in the batch, silently.
func TestOpenAIEmbed_PlacesByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":2,"embedding":[30]},{"index":0,"embedding":[10]},{"index":1,"embedding":[20]}]}`)
	}))

	defer srv.Close()

	o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m"})

	vectors, err := o.Embed(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	for i, want := range []float32{10, 20, 30} {
		if vectors[i][0] != want {
			t.Errorf("vectors[%d] = %v, want the entry whose index is %d", i, vectors[i], i)
		}
	}
}

// TestOpenAIEmbed_Batches verifies BatchSize splits the work into several requests and that the
// results are concatenated in order across them.
func TestOpenAIEmbed_Batches(t *testing.T) {
	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++

		var req openAIEmbedRequest

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)

		var out openAIEmbedResponse

		for i := range req.Input {
			out.Data = append(out.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: []float32{float32(requests)}})
		}

		_ = json.NewEncoder(w).Encode(out)
	}))

	defer srv.Close()

	o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m", BatchSize: 2})

	vectors, err := o.Embed(context.Background(), []string{"a", "b", "c", "d", "e"})
	if err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if len(vectors) != 5 {
		t.Fatalf("got %d vectors, want 5", len(vectors))
	}

	if requests != 3 {
		t.Errorf("made %d requests, want 3 for five texts at a batch size of 2", requests)
	}

	// The third batch is the single trailing text, so its marker must be 3.
	if vectors[4][0] != 3 {
		t.Errorf("the last vector came from batch %v, want the third", vectors[4][0])
	}
}

// TestOpenAIEmbed_Truncates verifies one text is capped at MaxTextBytes on a rune boundary, using
// the same shared helper the Ollama client uses.
func TestOpenAIEmbed_Truncates(t *testing.T) {
	var got openAIEmbedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)

		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1]}]}`)
	}))

	defer srv.Close()

	o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m", MaxTextBytes: 5})

	// Four three-byte runes: a byte cap of 5 must cut back to the first rune boundary at or below
	// it, leaving one rune rather than a broken second one.
	if _, err := o.Embed(context.Background(), []string{"日本語文"}); err != nil {
		t.Fatalf("Embed: %s", err)
	}

	if got.Input[0] != "日" {
		t.Errorf("input = %q, want it cut on a rune boundary", got.Input[0])
	}
}

// TestOpenAIEmbed_Failures covers every response that cannot be turned into an aligned set of
// vectors. Each is all-or-nothing: a partial result would misalign the caller's vector-to-memory
// mapping and surface much later as wrong search results rather than absent ones.
func TestOpenAIEmbed_Failures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "a short response cannot be aligned",
			status: http.StatusOK,
			body:   `{"data":[{"index":0,"embedding":[1]}]}`,
			want:   "returned 1 embeddings for 2 inputs",
		},
		{
			name:   "an out-of-range index",
			status: http.StatusOK,
			body:   `{"data":[{"index":0,"embedding":[1]},{"index":7,"embedding":[2]}]}`,
			want:   "out-of-range embedding index 7",
		},
		{
			name:   "a repeated index leaves a hole",
			status: http.StatusOK,
			body:   `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`,
			want:   "embedding index 0 twice",
		},
		{
			name:   "an empty embedding",
			status: http.StatusOK,
			body:   `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[]}]}`,
			want:   "empty embedding at position 1",
		},
		{
			name:   "the error envelope is preferred over the raw body",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"unknown model"}}`,
			want:   "unknown model",
		},
		{
			name:   "an envelope arriving with a 200 is still an error",
			status: http.StatusOK,
			body:   `{"error":{"message":"upstream refused"}}`,
			want:   "upstream refused",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			defer srv.Close()

			o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m"})

			_, err := o.Embed(context.Background(), []string{"a", "b"})
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestOpenAIEmbed_DimensionMismatchNamesTheFix verifies the width check reports both numbers and
// names the rebuild, since the index's vector width is fixed at creation and cannot be altered in
// place.
func TestOpenAIEmbed_DimensionMismatchNamesTheFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,2,3]}]}`)
	}))

	defer srv.Close()

	o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m", Dimensions: 768})

	_, err := o.Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, want := range []string{"produced 3-dimension vectors", "768 is configured", "llm.embedding.dimensions", "--backfill-search --reindex"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestOpenAIEmbed_AuthFailureNamesTheKey verifies a 401 names the embedding key specifically -
// the summariser and the embedder are configured separately and need not share a provider.
func TestOpenAIEmbed_AuthFailureNamesTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	}))

	defer srv.Close()

	withKey, _ := NewOpenAI(Config{Address: srv.URL, Model: "m", APIKey: "sk-wrong"})

	_, err := withKey.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "rejected the configured llm.embedding.apiKey") {
		t.Errorf("with a key, error = %v, want it to name the rejected key", err)
	}

	withoutKey, _ := NewOpenAI(Config{Address: srv.URL, Model: "m"})

	_, err = withoutKey.Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "no llm.embedding.apiKey is set") {
		t.Errorf("without a key, error = %v, want it to say none is configured", err)
	}
}

// TestOpenAIEmbed_Empty verifies no texts means no request and no error.
func TestOpenAIEmbed_Empty(t *testing.T) {
	o, _ := NewOpenAI(Config{Address: "http://127.0.0.1:0", Model: "m"})

	vectors, err := o.Embed(context.Background(), nil)
	if err != nil || vectors != nil {
		t.Errorf("Embed(nil) = %v, %v; want no vectors and no error", vectors, err)
	}
}

// TestOpenAIEmbedPing covers the probe's outcomes, mirroring the summariser's.
func TestOpenAIEmbedPing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		wantDegrade bool
	}{
		{name: "the model is served", status: http.StatusOK, body: `{"data":[{"id":"text-embedding-3-small"}]}`},
		{name: "the model is missing from a served list", status: http.StatusOK, body: `{"data":[{"id":"other"}]}`, wantDegrade: true},
		{name: "no model list is served", status: http.StatusNotFound, body: "not found"},
		{name: "an empty list proves nothing", status: http.StatusOK, body: `{"data":[]}`},
		{name: "a rejected key is an outright error", status: http.StatusUnauthorized, body: `{"error":{"message":"bad key"}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			defer srv.Close()

			o, _ := NewOpenAI(Config{Address: srv.URL, Model: "text-embedding-3-small"})

			err := o.Ping(context.Background())

			switch {

			case tc.wantDegrade:
				if !errors.Is(err, ErrDegraded) {
					t.Errorf("error = %v, want it to wrap ErrDegraded", err)
				}

			case tc.wantErr:
				if err == nil || errors.Is(err, ErrDegraded) {
					t.Errorf("error = %v, want a plain error", err)
				}

			default:
				if err != nil {
					t.Errorf("Ping: %s", err)
				}

			}
		})
	}
}
