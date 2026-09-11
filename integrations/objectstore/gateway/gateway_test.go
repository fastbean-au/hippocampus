package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

// fakeTap records the ids the gateway reinforces.
type fakeTap struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (f *fakeTap) Touch(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ids = append(f.ids, id)

	return f.err
}

func (f *fakeTap) touched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.ids...)
}

func newGateway(t *testing.T, cfg Config) (*Gateway, *objects.Memory, *fakeTap) {
	t.Helper()

	store := objects.NewMemory("payloads")
	store.Put("traces/one.json", []byte(`{"trace":1}`), time.Now())

	tap := &fakeTap{}

	cfg.Store = store
	cfg.Tap = tap

	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	return gw, store, tap
}

func TestRedirectModeSendsTheReaderToAPresignedURL(t *testing.T) {
	gw, _, tap := newGateway(t, Config{})

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/o/traces/one.json", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", recorder.Code)
	}

	if location := recorder.Header().Get("Location"); !strings.Contains(location, "traces/one.json") {
		t.Errorf("expected the location to name the object, got %q", location)
	}

	// A short-lived credential must not be cached and handed to somebody else.
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("expected the redirect not to be cacheable, got %q", got)
	}

	if got := tap.touched(); len(got) != 1 || got[0] != "payloads/traces/one.json" {
		t.Errorf("expected the read to reinforce the derived id, got %v", got)
	}
}

func TestProxyModeStreamsTheObject(t *testing.T) {
	gw, _, tap := newGateway(t, Config{Mode: ModeProxy})

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/o/traces/one.json", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	if body := recorder.Body.String(); body != `{"trace":1}` {
		t.Errorf("expected the object's body, got %q", body)
	}

	if got := recorder.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("expected the store's content type to be forwarded, got %q", got)
	}

	if len(tap.touched()) != 1 {
		t.Error("expected the read to be reinforced")
	}
}

// Answering a range request with the whole body and a 200 is a wrong answer rather than a degraded
// one, which is why proxy mode forwards the header and the store's status.
func TestProxyModeForwardsARangeRequest(t *testing.T) {
	gw, _, _ := newGateway(t, Config{Mode: ModeProxy})

	request := httptest.NewRequest(http.MethodGet, "/o/traces/one.json", nil)
	request.Header.Set("Range", "bytes=2-")

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", recorder.Code)
	}

	if body := recorder.Body.String(); body != `trace":1}` {
		t.Errorf("expected the requested range, got %q", body)
	}

	if got := recorder.Header().Get("Content-Range"); got == "" {
		t.Error("expected a Content-Range header")
	}
}

func TestProxyModeReportsAMissingObject(t *testing.T) {
	gw, _, _ := newGateway(t, Config{Mode: ModeProxy})

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/o/traces/absent.json", nil))

	if recorder.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", recorder.Code)
	}
}

func TestAFailingStoreIsABadGateway(t *testing.T) {
	gw, store, _ := newGateway(t, Config{})
	store.PresignErr = fmt.Errorf("the bucket is unreachable")

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/o/traces/one.json", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", recorder.Code)
	}
}

// A HEAD is a metadata read, not a read of the payload: reinforcing it would double every recall a
// client that HEADs before it GETs makes.
func TestHeadDoesNotReinforce(t *testing.T) {
	gw, _, tap := newGateway(t, Config{Mode: ModeProxy})

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/o/traces/one.json", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}

	if recorder.Body.Len() != 0 {
		t.Errorf("expected no body on a HEAD, got %d bytes", recorder.Body.Len())
	}

	if got := tap.touched(); len(got) != 0 {
		t.Errorf("expected no reinforcement, got %v", got)
	}
}

// Serving the object is the job; reinforcement is secondary. A store that is down must cost a decay
// clock its update, not a reader their file.
func TestAFailingTapDoesNotFailTheRead(t *testing.T) {
	gw, _, tap := newGateway(t, Config{})
	tap.err = fmt.Errorf("the service is down")

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/o/traces/one.json", nil))

	if recorder.Code != http.StatusFound {
		t.Errorf("expected the read to be served anyway, got %d", recorder.Code)
	}
}

func TestAnUnmappableKeyIsServedButNotReinforced(t *testing.T) {
	gw, store, tap := newGateway(t, Config{Mode: ModeProxy})

	key := strings.Repeat("k", 300)
	store.Put(key, []byte("payload"), time.Now())

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/o/"+key, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected the object to be served, got %d", recorder.Code)
	}

	if got := tap.touched(); len(got) != 0 {
		t.Errorf("expected no reinforcement for an object that could never have a memory, got %v", got)
	}
}

func TestTheInboundTokenIsRequiredWhenConfigured(t *testing.T) {
	gw, _, tap := newGateway(t, Config{AuthToken: "sekrit"})

	cases := []struct {
		name   string
		header string
		status int
	}{
		{name: "no header", header: "", status: http.StatusUnauthorized},
		{name: "the wrong token", header: "Bearer nope", status: http.StatusUnauthorized},
		{name: "not a bearer", header: "Basic sekrit", status: http.StatusUnauthorized},
		{name: "the right token", header: "Bearer sekrit", status: http.StatusFound},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/o/traces/one.json", nil)

			if v.header != "" {
				request.Header.Set("Authorization", v.header)
			}

			recorder := httptest.NewRecorder()
			gw.Handler().ServeHTTP(recorder, request)

			if recorder.Code != v.status {
				t.Errorf("expected %d, got %d", v.status, recorder.Code)
			}
		})
	}

	// The refused requests must not have reinforced anything either.
	if got := tap.touched(); len(got) != 1 {
		t.Errorf("expected only the authorised read to reinforce, got %v", got)
	}
}

func TestRecallEndpointAcceptsKeys(t *testing.T) {
	gw, _, tap := newGateway(t, Config{})

	body := strings.NewReader(`{"keys":["traces/one.json","` + strings.Repeat("k", 300) + `"]}`)

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/recall", body))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", recorder.Code)
	}

	var response recallResponse

	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("could not parse the response: %s", err.Error())
	}

	if response.Accepted != 1 || response.Unmappable != 1 {
		t.Errorf("expected one accepted and one unmappable, got %+v", response)
	}

	if got := tap.touched(); len(got) != 1 || got[0] != "payloads/traces/one.json" {
		t.Errorf("expected the mappable key to be reinforced, got %v", got)
	}
}

func TestRecallEndpointRefusesRubbish(t *testing.T) {
	gw, _, _ := newGateway(t, Config{})

	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: "{"},
		{name: "too many keys", body: `{"keys":` + manyKeys(maxRecallKeys+1) + `}`},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/recall", strings.NewReader(v.body)))

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", recorder.Code)
			}
		})
	}
}

func TestRecallEndpointHonoursTheInboundToken(t *testing.T) {
	gw, _, _ := newGateway(t, Config{AuthToken: "sekrit"})

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/recall", strings.NewReader(`{"keys":["a"]}`)))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", recorder.Code)
	}
}

func TestNewValidatesItsConfiguration(t *testing.T) {
	store := objects.NewMemory("payloads")

	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "no store", cfg: Config{Tap: &fakeTap{}}},
		{name: "no tap", cfg: Config{Store: store}},
		{name: "an unknown mode", cfg: Config{Store: store, Tap: &fakeTap{}, Mode: Mode("sideways")}},
		{name: "a prefix with no leading slash", cfg: Config{Store: store, Tap: &fakeTap{}, PathPrefix: "o/"}},
		{name: "a prefix with no trailing slash", cfg: Config{Store: store, Tap: &fakeTap{}, PathPrefix: "/o"}},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if _, err := New(v.cfg); err == nil {
				t.Error("expected the configuration to be refused")
			}
		})
	}
}

func TestAConfiguredPathPrefixIsServed(t *testing.T) {
	gw, _, tap := newGateway(t, Config{PathPrefix: "/objects/"})

	recorder := httptest.NewRecorder()
	gw.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/objects/traces/one.json", nil))

	if recorder.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", recorder.Code)
	}

	if len(tap.touched()) != 1 {
		t.Error("expected the read to be reinforced")
	}
}

// The gateway is the tap's front door, so an end-to-end pass over a real listener is worth having:
// it is the only place the ServeMux patterns, the escaping and the redirect meet.
func TestOverARealListener(t *testing.T) {
	gw, _, tap := newGateway(t, Config{})

	server := httptest.NewServer(gw.Handler())
	defer server.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	response, err := client.Get(server.URL + "/o/traces/one.json")
	if err != nil {
		t.Fatalf("the request failed: %s", err.Error())
	}

	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, response.Body)

	if response.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", response.StatusCode)
	}

	if got := tap.touched(); len(got) != 1 {
		t.Errorf("expected one reinforcement, got %v", got)
	}
}

func manyKeys(n int) string {
	keys := make([]string, 0, n)

	for i := range n {
		keys = append(keys, fmt.Sprintf("%q", fmt.Sprintf("k%d", i)))
	}

	return "[" + strings.Join(keys, ",") + "]"
}
