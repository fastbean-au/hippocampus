package summarise

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOllama_Ping covers the deployment-topology probe. The middle case is the reason it asks
// /api/tags instead of merely opening a connection: a running server that has never pulled the
// model answers everything except a generation, so it looks perfectly healthy right up until the
// first summarisation fails. That is reported as degraded rather than unreachable, because the
// server is there and `ollama pull` is the fix.
func TestOllama_Ping(t *testing.T) {
	for name, tc := range map[string]struct {
		status       int
		body         string
		wantErr      bool
		wantDegraded bool
	}{
		"the model is installed": {
			status: http.StatusOK,
			body:   `{"models":[{"name":"llama3.2:latest"},{"name":"other"}]}`,
		},
		"the model is missing": {
			status:       http.StatusOK,
			body:         `{"models":[{"name":"other"}]}`,
			wantErr:      true,
			wantDegraded: true,
		},
		"the server is unhappy": {
			status:  http.StatusInternalServerError,
			body:    "boom",
			wantErr: true,
		},
		"the response is not json": {
			status:  http.StatusOK,
			body:    "not json",
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/tags" {
					t.Errorf("Ping requested %q, want /api/tags", r.URL.Path)
				}

				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))

			t.Cleanup(server.Close)

			o, err := NewOllama(Config{Address: server.URL, Model: "llama3.2"})
			if err != nil {
				t.Fatalf("NewOllama: %s", err)
			}

			err = o.Ping(context.Background())

			if tc.wantErr && err == nil {
				t.Fatal("Ping reported success")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("Ping: %s", err)
			}

			if degraded := errors.Is(err, ErrDegraded); degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v (err: %v)", degraded, tc.wantDegraded, err)
			}
		})
	}
}

// TestOllama_PingUnreachable covers a server that is not there at all - the case the status must
// distinguish from a server missing its model.
func TestOllama_PingUnreachable(t *testing.T) {
	o, err := NewOllama(Config{Address: "http://127.0.0.1:1", Model: "llama3.2"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	err = o.Ping(context.Background())

	if err == nil {
		t.Fatal("Ping reported success against an address with nothing listening")
	}

	if errors.Is(err, ErrDegraded) {
		t.Error("an unreachable server was reported as degraded; the two want different responses")
	}
}

// TestTagsResponseMatchesUntaggedModels pins the tag resolution: Ollama treats an untagged name as
// ":latest", so a configuration of "llama3.2" is satisfied by an installed "llama3.2:latest". A
// literal comparison would report every correctly configured deployment as degraded.
func TestTagsResponseMatchesUntaggedModels(t *testing.T) {
	installed := tagsResponse{Models: []struct {
		Name string `json:"name"`
	}{{Name: "llama3.2:latest"}}}

	if !installed.has("llama3.2") {
		t.Error("an untagged configuration did not match an installed :latest")
	}

	if !installed.has("llama3.2:latest") {
		t.Error("an exact tag did not match itself")
	}

	if installed.has("llama3.1") {
		t.Error("a different model matched")
	}
}

// TestOllama_PingUnbuildableRequest covers the one branch a reachable server cannot exercise: an
// address that is not a URL at all. It is here because the alternative is an error path that has
// never once been run, in a function whose whole job is to report what is wrong.
func TestOllama_PingUnbuildableRequest(t *testing.T) {
	o, err := NewOllama(Config{Address: "http://\x7f invalid", Model: "llama3.2"})
	if err != nil {
		t.Fatalf("NewOllama: %s", err)
	}

	if err := o.Ping(context.Background()); err == nil {
		t.Error("Ping reported success for an address that cannot be requested")
	}
}
