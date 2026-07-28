package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewOllama_RequiresAddressAndModel verifies construction rejects a missing address or model
// but succeeds with both, without contacting any server.
func TestNewOllama_RequiresAddressAndModel(t *testing.T) {
	if _, err := NewOllama(Config{Model: "m"}); err == nil {
		t.Error("expected error for missing address")
	}

	if _, err := NewOllama(Config{Address: "http://localhost:11434"}); err == nil {
		t.Error("expected error for missing model")
	}

	o, err := NewOllama(Config{Address: "http://localhost:11434/", Model: "llama3.2"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if o.address != "http://localhost:11434" {
		t.Errorf("trailing slash not trimmed: %q", o.address)
	}

	if !o.Enabled() {
		t.Error("Enabled() should be true")
	}
}

// TestOllamaSummarize_HappyPath verifies the request shape and that the response's summary is
// returned, using a fake Ollama server.
func TestOllamaSummarize_HappyPath(t *testing.T) {
	var gotReq generateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}

		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotReq); err != nil {
			t.Errorf("decode request: %s", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generateResponse{Response: "  a concise summary  ", Done: true})
	}))

	defer srv.Close()

	o, err := NewOllama(Config{Address: srv.URL, Model: "llama3.2", Temperature: 0.1})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	summary, err := o.Summarize(context.Background(), Request{
		EventName: "deploy",
		Group:     "svc-a",
		Bodies:    []string{"first line", "second line"},
	})
	if err != nil {
		t.Fatalf("Summarize: %s", err)
	}

	if summary != "a concise summary" {
		t.Errorf("summary not trimmed/returned: %q", summary)
	}

	if gotReq.Model != "llama3.2" || gotReq.Stream {
		t.Errorf("unexpected request: %+v", gotReq)
	}

	if !strings.Contains(gotReq.Prompt, "first line") || !strings.Contains(gotReq.Prompt, "deploy") {
		t.Errorf("prompt missing content: %q", gotReq.Prompt)
	}

	if gotReq.Options["temperature"] != 0.1 {
		t.Errorf("temperature not passed: %+v", gotReq.Options)
	}
}

// TestOllamaSummarize_ServerError verifies a non-2xx status becomes an error.
func TestOllamaSummarize_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))

	defer srv.Close()

	o, _ := NewOllama(Config{Address: srv.URL, Model: "m"})

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected error on 500")
	}
}

// TestOllamaSummarize_ModelError verifies an Ollama-side error field becomes an error even on 200.
func TestOllamaSummarize_ModelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(generateResponse{Error: "model 'nope' not found"})
	}))

	defer srv.Close()

	o, _ := NewOllama(Config{Address: srv.URL, Model: "nope"})

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected error when response carries an error field")
	}
}

// TestOllamaSummarize_EmptyResponse verifies an empty response is rejected rather than returned as
// a valid (empty) summary.
func TestOllamaSummarize_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(generateResponse{Response: "   ", Done: true})
	}))

	defer srv.Close()

	o, _ := NewOllama(Config{Address: srv.URL, Model: "m"})

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected error on empty response")
	}
}

// TestOllamaSummarize_NoBodies verifies a request with nothing to summarise fails before any HTTP
// call.
func TestOllamaSummarize_NoBodies(t *testing.T) {
	o, _ := NewOllama(Config{Address: "http://127.0.0.1:0", Model: "m"})

	if _, err := o.Summarize(context.Background(), Request{}); err == nil {
		t.Error("expected error with no bodies")
	}
}

// TestBuildPrompt_RespectsLimits verifies maxBodies and promptCharLimit both bound the prompt.
func TestBuildPrompt_RespectsLimits(t *testing.T) {
	o, _ := NewOllama(Config{Address: "http://x", Model: "m", MaxBodies: 2})

	prompt := o.buildPrompt(Request{Bodies: []string{"a", "b", "c", "d"}})
	if strings.Contains(prompt, "3. c") {
		t.Errorf("maxBodies not respected: %q", prompt)
	}

	o2, _ := NewOllama(Config{Address: "http://x", Model: "m", PromptCharLimit: 3})

	prompt2 := o2.buildPrompt(Request{Bodies: []string{"aa", "bb"}})
	if strings.Contains(prompt2, "bb") {
		t.Errorf("promptCharLimit not respected: %q", prompt2)
	}
}

// TestNoop verifies the disabled summariser reports disabled and errors.
func TestNoop(t *testing.T) {
	n := NewNoop()

	if n.Enabled() {
		t.Error("noop should not be enabled")
	}

	if _, err := n.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err != ErrDisabled {
		t.Errorf("expected ErrDisabled, got %v", err)
	}
}

// TestOllamaSummarize_InvalidJSON verifies an undecodable response body becomes an error.
func TestOllamaSummarize_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not json"))
	}))

	defer srv.Close()

	o, _ := NewOllama(Config{Address: srv.URL, Model: "m"})

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected error decoding a non-JSON response")
	}
}

// TestOllamaSummarize_MarshalError verifies an unmarshalable request (a non-finite temperature,
// which reaches the options map) surfaces the marshal error rather than being sent.
func TestOllamaSummarize_MarshalError(t *testing.T) {
	o, _ := NewOllama(Config{Address: "http://127.0.0.1:0", Model: "m", Temperature: math.Inf(1)})

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected marshal error for a non-finite temperature")
	}
}

// TestOllamaSummarize_BadAddress verifies an address carrying an invalid control character fails
// when the request is built, before any network call.
func TestOllamaSummarize_BadAddress(t *testing.T) {
	o := &Ollama{
		address:         "http://ex\x7fample",
		model:           "m",
		maxBodies:       defaultMaxBodies,
		promptCharLimit: defaultPromptCharLimit,
		client:          &http.Client{},
	}

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected error building a request with a bad address")
	}
}

// errReadCloser returns an error partway through Read, simulating a truncated/broken response body.
type errReadCloser struct{}

func (errReadCloser) Read(p []byte) (int, error) { return 0, errors.New("read failed") }

func (errReadCloser) Close() error { return nil }

// errRoundTripper returns a 200 response whose body errors on Read.
type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       errReadCloser{},
		Header:     make(http.Header),
	}, nil
}

// TestOllamaSummarize_BodyReadError verifies a failure reading the response body becomes an error.
func TestOllamaSummarize_BodyReadError(t *testing.T) {
	o, _ := NewOllama(Config{Address: "http://ollama", Model: "m"})
	o.client = &http.Client{Transport: errRoundTripper{}}

	if _, err := o.Summarize(context.Background(), Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected error reading a broken response body")
	}
}

// TestOllamaSummarize_ContextDeadline verifies the caller's context deadline aborts the call.
func TestOllamaSummarize_ContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(generateResponse{Response: "late", Done: true})
	}))

	defer srv.Close()

	o, _ := NewOllama(Config{Address: srv.URL, Model: "m"})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if _, err := o.Summarize(ctx, Request{Bodies: []string{"x"}}); err == nil {
		t.Error("expected context-deadline error")
	}
}
