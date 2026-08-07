package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

// writeTempFile writes content to a temp file and returns its path.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// echoServer returns a fake gateway URL that replies to every request with an empty JSON object.
func echoServer(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))

	t.Cleanup(server.Close)

	return server.URL
}

// runArgs invokes run() over the HTTP transport pointed at address, with extra args appended after
// the command tokens, returning run's error.
func runArgs(t *testing.T, address string, args ...string) error {
	t.Helper()

	var out bytes.Buffer

	full := append([]string{"--transport", "http", "--address", address}, args...)

	return run(context.Background(), full, &out, &out)
}

// validArgs is the minimal valid argument set for each command, shared by the success and
// error-propagation tables so every handler is driven the same way.
func validArgs(batchFile string) []struct {
	key  string
	args []string
} {
	return []struct {
		key  string
		args []string
	}{
		{"memory store", []string{"--body", "b"}},
		{"memory update", []string{"--id", "m", "--body", "b"}},
		{"memory delete", []string{"m"}},
		{"memory list", nil},
		{"memory recall", []string{"m"}},
		{"memory search", []string{"--query", "q"}},
		{"event create", []string{"--name", "n"}},
		{"event end", []string{"--id", "e"}},
		{"event significance", []string{"--id", "e", "--significance", "2"}},
		{"event merge", []string{"--from", "a", "--to", "b"}},
		{"event delete", []string{"--id", "e"}},
		{"event get", []string{"--id", "e"}},
		{"event list", []string{"--memories"}},
		{"whoami", nil},
		{"sleep", nil},
		{"purge", []string{"--yes"}},
		{"summary candidates", nil},
		{"summary replace", []string{"--event-id", "e", "--body", "s"}},
		{"summary summarise", []string{"--event-id", "e"}},
		{"export", nil},
		{"import", []string{"--object-key", "k"}},
		{"import-batch", []string{"--file", batchFile}},
		{"transfer", nil},
		{"clear", []string{"--manifest-id", "m"}},
	}
}

// TestAllHandlersPropagateError drives every command with valid args but a failing client, so each
// handler's post-RPC "return err" branch is covered.
func TestAllHandlersPropagateError(t *testing.T) {
	batchFile := writeTempFile(t, "{}")
	wantErr := errors.New("rpc-boom")

	for _, tc := range validArgs(batchFile) {
		_, _, err := runCommand(t, tc.key, tc.args, &fakeClient{err: wantErr})
		if !errors.Is(err, wantErr) {
			t.Errorf("%s: err = %v, want rpc-boom", tc.key, err)
		}
	}
}

// TestRunValidationError covers run()'s path where a handler returns a validation error before any
// RPC (the client is built but the command fails fast).
func TestRunValidationError(t *testing.T) {
	server := echoServer(t)

	err := runArgs(t, server, "memory", "store") // no --body
	if err == nil || !strings.Contains(err.Error(), "body is required") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunFlagParseError covers run()'s flag-parse error branch.
func TestRunFlagParseError(t *testing.T) {
	server := echoServer(t)

	err := runArgs(t, server, "whoami", "--nope")
	if err == nil {
		t.Fatal("expected a flag parse error")
	}
}

// TestRunRPCError covers run() surfacing an RPC error from the transport.
func TestRunRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":14,"message":"unavailable"}`))
	}))

	t.Cleanup(server.Close)

	err := runArgs(t, server.URL, "whoami")
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunBadClientConfig covers run() failing while building the client (unknown transport).
func TestRunBadClientConfig(t *testing.T) {
	err := runArgs(t, "http://ignored", "whoami", "--transport", "smoke-signal")
	if err == nil || !strings.Contains(err.Error(), "unknown transport") {
		t.Fatalf("err = %v", err)
	}
}

// TestHTTPClientDecodeError covers do()'s response-decode error branch.
func TestHTTPClientDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not json"))
	}))

	t.Cleanup(server.Close)

	client := &httpClient{baseURL: server.URL, http: &http.Client{Timeout: 5 * time.Second}}

	if _, err := client.WhoAmI(context.Background(), &contract.EmptyRequest{}); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v", err)
	}
}

// TestHTTPClientRequestBuildError covers do()'s http.NewRequestWithContext error branch (an
// unparseable target URL).
func TestHTTPClientRequestBuildError(t *testing.T) {
	client := &httpClient{baseURL: "http://bad host", http: &http.Client{Timeout: time.Second}}

	if _, err := client.WhoAmI(context.Background(), &contract.EmptyRequest{}); err == nil || !strings.Contains(err.Error(), "build request") {
		t.Fatalf("err = %v, want a build-request error", err)
	}
}

// TestHTTPClientTransportError covers do()'s http.Do error branch (nothing listening).
func TestHTTPClientTransportError(t *testing.T) {
	client := &httpClient{baseURL: "http://127.0.0.1:1", http: &http.Client{Timeout: time.Second}}

	if _, err := client.Sleep(context.Background(), &contract.EmptyRequest{}); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err = %v, want a transport error", err)
	}
}

// TestQueryValuesNonScalar covers queryValues' default branch, where a non-scalar field (a repeated
// message) is rendered as its raw JSON rather than a plain scalar.
func TestQueryValuesNonScalar(t *testing.T) {
	event := &contract.Event{
		Group: "g",
		Links: []*contract.Link{{Id: "e2", Significance: 3}},
	}

	values, err := queryValues(event, nil)
	if err != nil {
		t.Fatalf("queryValues: %v", err)
	}

	if values.Get("group") != "g" {
		t.Fatalf("group = %q", values.Get("group"))
	}

	if !strings.Contains(values.Get("links"), "e2") {
		t.Fatalf("links query = %q, want the raw JSON array", values.Get("links"))
	}
}

// TestQueryValuesNilMessage covers queryValues' nil short-circuit.
func TestQueryValuesNilMessage(t *testing.T) {
	if values, err := queryValues(nil, nil); err != nil || values != nil {
		t.Fatalf("queryValues(nil) = %v, %v; want nil, nil", values, err)
	}
}

// errReadCloser is a response body that fails on read, to drive do()'s io.ReadAll error branch.
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errReadCloser) Close() error             { return nil }

// stubTransport returns a fixed response for every request.
type stubTransport struct{ resp *http.Response }

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) { return s.resp, nil }

// TestHTTPClientReadBodyError covers do()'s io.ReadAll error branch via a transport whose response
// body errors on read (a network-timing-free way to exercise the branch).
func TestHTTPClientReadBodyError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Body: errReadCloser{}, Header: make(http.Header)}
	client := &httpClient{baseURL: "http://example", http: &http.Client{Transport: stubTransport{resp: resp}}}

	if _, err := client.WhoAmI(context.Background(), &contract.EmptyRequest{}); err == nil || !strings.Contains(err.Error(), "read response") {
		t.Fatalf("err = %v, want a read-response error", err)
	}
}

// TestNewGRPCClientWithTLSAndToken covers the TLS-credential and token-interceptor branches of
// newGRPCClient.
func TestNewGRPCClientWithTLSAndToken(t *testing.T) {
	client, closeClient, err := newGRPCClient(Config{
		Transport: "grpc",
		Address:   "localhost:50051",
		Token:     "t",
		TLS:       TLSConfig{Enabled: true},
	})
	if err != nil {
		t.Fatalf("newGRPCClient: %v", err)
	}

	if client == nil {
		t.Fatal("client is nil")
	}

	_ = closeClient()
}

// TestNewGRPCClientBadTLS covers newGRPCClient's credential-build error branch.
func TestNewGRPCClientBadTLS(t *testing.T) {
	if _, _, err := newGRPCClient(Config{Transport: "grpc", TLS: TLSConfig{Enabled: true, Cert: "only-cert"}}); err == nil {
		t.Fatal("expected a credentials error")
	}
}

// TestNewHTTPClientBadTLS covers newHTTPClient's transport-build error branch.
func TestNewHTTPClientBadTLS(t *testing.T) {
	if _, _, err := newHTTPClient(Config{Transport: "http", Address: "x", TLS: TLSConfig{Enabled: true, Key: "only-key"}}); err == nil {
		t.Fatal("expected a transport error")
	}
}

// TestRenderJSONFallbackForEmptyRequest covers render's default protojson branch for a type with no
// bespoke text form.
func TestRenderTextFallback(t *testing.T) {
	var sb strings.Builder

	r := &renderer{out: &sb}

	// EmptyRequest has no bespoke case, so renderText falls through to protojson.
	if err := r.render(&contract.EmptyRequest{}); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(sb.String(), "{}") {
		t.Fatalf("fallback output = %q", sb.String())
	}
}
