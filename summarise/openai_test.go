package summarise

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

// TestNewOpenAI_RequiresAddressAndModel verifies construction rejects a missing address or model,
// trims a trailing slash, and contacts nothing.
func TestNewOpenAI_RequiresAddressAndModel(t *testing.T) {
	if _, err := NewOpenAI(Config{Model: "m"}); err == nil {
		t.Error("expected error for missing address")
	}

	if _, err := NewOpenAI(Config{Address: "https://api.openai.com/v1"}); err == nil {
		t.Error("expected error for missing model")
	}

	o, err := NewOpenAI(Config{Address: "https://api.openai.com/v1/", Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("NewOpenAI: %s", err)
	}

	if o.address != "https://api.openai.com/v1" {
		t.Errorf("trailing slash not trimmed: %q", o.address)
	}

	if !o.Enabled() {
		t.Error("Enabled() should be true")
	}

	if o.systemPrompt != defaultSystemPrompt || o.maxBodies != defaultMaxBodies || o.promptCharLimit != defaultPromptCharLimit {
		t.Error("defaults were not applied to the optional fields")
	}
}

// TestOpenAISummarise_HappyPath verifies the request shape - path, model, the two messages, no
// stream - and that the trimmed content of the first choice is returned.
func TestOpenAISummarise_HappyPath(t *testing.T) {
	var (
		gotReq  chatRequest
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

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"  a concise summary  "}}]}`)
	}))

	defer srv.Close()

	o, err := NewOpenAI(Config{Address: srv.URL + "/v1", Model: "gpt-4o-mini", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("NewOpenAI: %s", err)
	}

	got, err := o.Summarise(context.Background(), Request{EventName: "deploy", Group: "svc", Bodies: []string{"one", "two"}})
	if err != nil {
		t.Fatalf("Summarise: %s", err)
	}

	if got != "a concise summary" {
		t.Errorf("summary = %q, want the trimmed content", got)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want the endpoint appended to the configured base", gotPath)
	}

	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want the configured key as a bearer token", gotAuth)
	}

	if gotReq.Model != "gpt-4o-mini" || gotReq.Stream {
		t.Errorf("request = %+v, want the configured model and no streaming", gotReq)
	}

	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Role != "user" {
		t.Fatalf("messages = %+v, want a system turn then a user turn", gotReq.Messages)
	}

	if gotReq.Messages[0].Content != defaultSystemPrompt {
		t.Error("the system turn does not carry the system prompt")
	}

	// The prompt must be the shared one, so the provider cannot change how much of the store leaves
	// the process.
	if want := buildPrompt(Request{EventName: "deploy", Group: "svc", Bodies: []string{"one", "two"}}, defaultMaxBodies, defaultPromptCharLimit); gotReq.Messages[1].Content != want {
		t.Errorf("user turn = %q, want the shared buildPrompt output %q", gotReq.Messages[1].Content, want)
	}
}

// TestOpenAISummarise_TemperatureOmittedWhenZero pins that a zero temperature is left out of the
// request rather than sent as 0, since several current models reject an explicit value.
func TestOpenAISummarise_TemperatureOmittedWhenZero(t *testing.T) {
	for _, tc := range []struct {
		name        string
		temperature float64
		wantPresent bool
	}{
		{name: "zero is omitted", temperature: 0, wantPresent: false},
		{name: "a set value is sent", temperature: 0.2, wantPresent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)

				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"s"}}]}`)
			}))

			defer srv.Close()

			o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m", Temperature: tc.temperature})

			if _, err := o.Summarise(context.Background(), Request{Bodies: []string{"b"}}); err != nil {
				t.Fatalf("Summarise: %s", err)
			}

			if _, present := body["temperature"]; present != tc.wantPresent {
				t.Errorf("temperature present = %v, want %v", present, tc.wantPresent)
			}
		})
	}
}

// TestOpenAISummarise_NoBodies verifies an empty request is refused before any HTTP call.
func TestOpenAISummarise_NoBodies(t *testing.T) {
	o, _ := NewOpenAI(Config{Address: "http://127.0.0.1:0", Model: "m"})

	if _, err := o.Summarise(context.Background(), Request{}); err == nil {
		t.Error("expected error with no bodies")
	}
}

// TestOpenAISummarise_Failures covers every way a call can fail with a response in hand: the
// structured error envelope on a non-2xx, the same envelope arriving with a 200 (which some
// gateways do), no choices, and an empty completion.
func TestOpenAISummarise_Failures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "the error envelope is preferred over the raw body",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"model 'nope' does not exist","type":"invalid_request_error"}}`,
			want:   "model 'nope' does not exist",
		},
		{
			name:   "an envelope arriving with a 200 is still an error",
			status: http.StatusOK,
			body:   `{"error":{"message":"upstream refused"}}`,
			want:   "upstream refused",
		},
		{
			name:   "no choices",
			status: http.StatusOK,
			body:   `{"choices":[]}`,
			want:   "returned no choices",
		},
		{
			name:   "an empty completion",
			status: http.StatusOK,
			body:   `{"choices":[{"message":{"content":"   "}}]}`,
			want:   "returned an empty summary",
		},
		{
			name:   "an undecodable body on a non-2xx is reported verbatim",
			status: http.StatusBadGateway,
			body:   "upstream connect error",
			want:   "upstream connect error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			defer srv.Close()

			o, _ := NewOpenAI(Config{Address: srv.URL, Model: "m"})

			_, err := o.Summarise(context.Background(), Request{Bodies: []string{"b"}})
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestOpenAISummarise_AuthFailureNamesTheKey verifies a 401 says which setting is at fault, and
// distinguishes a rejected key from no key at all - the two need different fixes.
func TestOpenAISummarise_AuthFailureNamesTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	}))

	defer srv.Close()

	withKey, _ := NewOpenAI(Config{Address: srv.URL, Model: "m", APIKey: "sk-wrong"})

	_, err := withKey.Summarise(context.Background(), Request{Bodies: []string{"b"}})
	if err == nil || !strings.Contains(err.Error(), "rejected the configured llm.apiKey") {
		t.Errorf("with a key, error = %v, want it to name the rejected key", err)
	}

	withoutKey, _ := NewOpenAI(Config{Address: srv.URL, Model: "m"})

	_, err = withoutKey.Summarise(context.Background(), Request{Bodies: []string{"b"}})
	if err == nil || !strings.Contains(err.Error(), "no llm.apiKey is set") {
		t.Errorf("without a key, error = %v, want it to say none is configured", err)
	}
}

// TestOpenAISummarise_Unreachable verifies a transport failure names the address.
func TestOpenAISummarise_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	address := srv.URL

	srv.Close()

	o, _ := NewOpenAI(Config{Address: address, Model: "m"})

	_, err := o.Summarise(context.Background(), Request{Bodies: []string{"b"}})
	if err == nil || !strings.Contains(err.Error(), address) {
		t.Errorf("error = %v, want it to name the unreachable address", err)
	}
}

// TestOpenAIPing covers the probe's four outcomes, including the two that are deliberately more
// forgiving than the Ollama probe's: an endpoint that does not serve /models at all, and one whose
// list cannot be read.
func TestOpenAIPing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		wantDegrade bool
	}{
		{name: "the model is served", status: http.StatusOK, body: `{"data":[{"id":"gpt-4o-mini"},{"id":"other"}]}`},
		{name: "the model is missing from a served list", status: http.StatusOK, body: `{"data":[{"id":"other"}]}`, wantDegrade: true},
		{name: "no model list is served", status: http.StatusNotFound, body: "not found"},
		{name: "the method is not allowed", status: http.StatusMethodNotAllowed, body: ""},
		{name: "an empty list proves nothing", status: http.StatusOK, body: `{"data":[]}`},
		{name: "an unreadable list proves nothing", status: http.StatusOK, body: "not json"},
		{name: "a rejected key is an outright error", status: http.StatusUnauthorized, body: `{"error":{"message":"bad key"}}`, wantErr: true},
		{name: "any other failure is an error", status: http.StatusInternalServerError, body: "boom", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}

				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))

			defer srv.Close()

			o, _ := NewOpenAI(Config{Address: srv.URL, Model: "gpt-4o-mini", APIKey: "sk-test"})

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

// TestOpenAIPing_Unreachable verifies a dead endpoint is reported as unreachable rather than
// degraded - the two mean different things to the topology view.
func TestOpenAIPing_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	address := srv.URL

	srv.Close()

	o, _ := NewOpenAI(Config{Address: address, Model: "m"})

	err := o.Ping(context.Background())
	if err == nil || errors.Is(err, ErrDegraded) {
		t.Errorf("error = %v, want an unreachable error rather than a degraded one", err)
	}
}
