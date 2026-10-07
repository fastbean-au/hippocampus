package hippocampus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
)

// versionedDependency is a dependency that can be pinged and asked for its version, counting how
// often each happens - which is the whole of what the version cache is about.
type versionedDependency struct {
	mu         sync.Mutex
	pingErr    error
	version    string
	versionErr error
	pings      int
	asks       int
}

func (v *versionedDependency) Ping(context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.pings++

	return v.pingErr
}

func (v *versionedDependency) Version(context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.asks++

	return v.version, v.versionErr
}

func (v *versionedDependency) set(pingErr error, version string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.pingErr = pingErr
	v.version = version
}

func (v *versionedDependency) counts() (int, int) {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.pings, v.asks
}

// probedSearchServer is a topology server whose search node is probed by the given probe.
func probedSearchServer(t *testing.T) *Server {
	t.Helper()

	s := newTopologyServer(t)

	for i := range s.topology.nodes {
		if s.topology.nodes[i].id == topologyNodeSearch {
			s.topology.nodes[i].probe = true
		}
	}

	return s
}

func searchNode(t *testing.T, s *Server) *contract.TopologyNode {
	t.Helper()

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	return nodesById(res)[topologyNodeSearch]
}

// TestProbedVersionReachesTheNode is the end-to-end form of item 134 for a probed dependency: the
// version a dependency reports arrives on its node.
func TestProbedVersionReachesTheNode(t *testing.T) {
	s := probedSearchServer(t)
	dependency := &versionedDependency{version: "opensearch 2.19.1"}

	s.probeTopologyOnce(map[string]topologyProbe{topologyNodeSearch: pingerProbe(topologyNodeSearch, dependency)})

	if got := searchNode(t, s).GetVersion(); got != "opensearch 2.19.1" {
		t.Errorf("version = %q, want the dependency's own report", got)
	}
}

// TestProbedVersionIsCached pins why the version is cached: for most dependencies it costs a
// request of its own, and asking every round would double what this process sends to everything it
// depends on. It is asked on the first round, not on the next while the dependency stays healthy,
// again after a failed round (a restart is usually an upgrade), and again once the refresh has
// passed - the case a rolling upgrade that never fails a probe depends on.
func TestProbedVersionIsCached(t *testing.T) {
	s := probedSearchServer(t)
	dependency := &versionedDependency{version: "Ollama 0.6.1"}
	probers := map[string]topologyProbe{topologyNodeSearch: pingerProbe(topologyNodeSearch, dependency)}

	s.probeTopologyOnce(probers)
	s.probeTopologyOnce(probers)

	if pings, asks := dependency.counts(); pings != 2 || asks != 1 {
		t.Fatalf("after two healthy rounds: %d pings and %d version reads, want 2 and 1", pings, asks)
	}

	// Down: no version read (the ping failed first), but the last known version stays on the node -
	// the status beside it already says the reading is not live.
	dependency.set(errors.New("connection refused"), "Ollama 0.6.2")
	s.probeTopologyOnce(probers)

	node := searchNode(t, s)

	if node.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE {
		t.Fatalf("status = %s, want UNREACHABLE", node.GetStatus())
	}

	if node.GetVersion() != "Ollama 0.6.1" {
		t.Errorf("an unreachable dependency reports version %q, want the last one it reported", node.GetVersion())
	}

	// Back, upgraded: the round after a failure asks again.
	dependency.set(nil, "Ollama 0.6.2")
	s.probeTopologyOnce(probers)

	if got := searchNode(t, s).GetVersion(); got != "Ollama 0.6.2" {
		t.Errorf("after recovering, version = %q, want the new one", got)
	}

	// A rolling upgrade never fails a probe, so only the refresh notices it.
	dependency.set(nil, "Ollama 0.6.3")
	s.probeTopologyOnce(probers)

	if got := searchNode(t, s).GetVersion(); got != "Ollama 0.6.2" {
		t.Errorf("inside the refresh, version = %q; it was asked for again too soon", got)
	}

	results := s.topologyProbeResults()
	aged := results[topologyNodeSearch]
	aged.versionAskedAt = aged.versionAskedAt.Add(-topologyVersionRefresh)
	results[topologyNodeSearch] = aged
	s.topologyProbes.Store(&results)

	s.probeTopologyOnce(probers)

	if got := searchNode(t, s).GetVersion(); got != "Ollama 0.6.3" {
		t.Errorf("after the refresh, version = %q, want the upgraded one", got)
	}
}

// TestUnreadableVersionIsNotAFailure covers a dependency that serves correctly and will not say
// which build it is: it is healthy, carries no version, and is asked again only once per refresh.
func TestUnreadableVersionIsNotAFailure(t *testing.T) {
	s := probedSearchServer(t)
	dependency := &versionedDependency{versionErr: errors.New("404 Not Found")}
	probers := map[string]topologyProbe{topologyNodeSearch: pingerProbe(topologyNodeSearch, dependency)}

	s.probeTopologyOnce(probers)
	s.probeTopologyOnce(probers)

	node := searchNode(t, s)

	if node.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_OK {
		t.Errorf("status = %s; a version that cannot be read must not make a dependency unhealthy", node.GetStatus())
	}

	if node.GetVersion() != "" {
		t.Errorf("version = %q, want empty", node.GetVersion())
	}

	if _, asks := dependency.counts(); asks != 1 {
		t.Errorf("asked for the version %d times in two rounds, want once", asks)
	}
}

// TestPingerProbeWithoutAVersion covers a dependency with nothing to report (an S3 bucket, an
// OpenAI-compatible provider): the probe works exactly as it did before versions existed.
func TestPingerProbeWithoutAVersion(t *testing.T) {
	probe := pingerProbe("objects", pingOnly{})

	version, err := probe(context.Background(), true)
	if err != nil || version != "" {
		t.Errorf("probe = (%q, %v), want (\"\", nil)", version, err)
	}
}

type pingOnly struct{}

func (pingOnly) Ping(context.Context) error { return nil }

// TestStoreReportsItsEngineVersion runs the store's probe against a real embedded store.
func TestStoreReportsItsEngineVersion(t *testing.T) {
	s := newTopologyServer(t)

	version, err := s.probeStore(context.Background(), true)
	if err != nil {
		t.Fatalf("probeStore: %s", err)
	}

	if !strings.HasPrefix(version, "SQLite ") {
		t.Errorf("version = %q, want the SQLite library's version", version)
	}

	if version, _ := s.probeStore(context.Background(), false); version != "" {
		t.Errorf("an unwanted version was read anyway: %q", version)
	}
}

// TestDeclaredComponentVersionReachesTheNode covers the free path: a declared component's /readyz
// body carries its version, so it arrives every round with no request of its own.
func TestDeclaredComponentVersionReachesTheNode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ready","component":"nats-bridge","version":"v0.52.0"}`))
	}))

	t.Cleanup(server.Close)

	s := declaredServer(t, TopologyComponent{Name: "nats-bridge", Kind: "bridge", HealthURL: server.URL})

	s.probeTopologyOnce(s.topologyProbers())

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	if got := nodesById(res)["declared:nats-bridge"].GetVersion(); got != "v0.52.0" {
		t.Errorf("version = %q, want the one the component's /readyz reported", got)
	}
}

// TestObservedCallerReportsItsVersion covers the one route a caller with no address can take to the
// diagram: it says which build it is in a header, on either transport, and the node shows it. A
// call that omits the header does not clear it, and a value not fit to show is dropped.
func TestObservedCallerReportsItsVersion(t *testing.T) {
	s := newTopologyServer(t)

	s.topology.authMethod = "hmac"

	call := func(ctx context.Context) {
		if _, err := s.InterceptorObserveCaller(ctx, nil, observedRPCInfo(), passthroughHandler); err != nil {
			t.Fatalf("InterceptorObserveCaller: %s", err)
		}
	}

	version := func() string {
		res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
		if err != nil {
			t.Fatalf("GetTopology: %s", err)
		}

		return nodesById(res)["observed:hippo-cli"].GetVersion()
	}

	ctx := observedContext("hippo-cli", []string{"admin"}, nil)

	call(metadata.NewIncomingContext(ctx, metadata.Pairs(contract.ClientVersionHeader, "hippo/0.52.0")))

	if got := version(); got != "hippo/0.52.0" {
		t.Fatalf("version = %q, want the one the client reported over gRPC", got)
	}

	call(ctx)

	if got := version(); got != "hippo/0.52.0" {
		t.Errorf("a call without the header changed the version to %q", got)
	}

	call(metadata.NewIncomingContext(ctx, metadata.Pairs(contract.ClientVersionHeader, "hippo/1\x00")))

	if got := version(); got != "hippo/0.52.0" {
		t.Errorf("an unprintable version replaced the last good one: %q", got)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/memories", nil).WithContext(ctx)
	request.Header.Set(contract.ClientVersionHeader, "hippo/0.53.0")

	s.HTTPMiddlewareObserveCaller(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), request)

	if got := version(); got != "hippo/0.53.0" {
		t.Errorf("version = %q, want the one the client reported over the gateway", got)
	}
}

// TestDeclaredComponentTakesItsCallerVersion covers a declared component with no health port of its
// own - an MCP bridge on stdio - whose only version is the one it reports as a caller.
func TestDeclaredComponentTakesItsCallerVersion(t *testing.T) {
	s := declaredServer(t, TopologyComponent{Name: "claude-mcp", Kind: "mcp", HealthURL: "http://127.0.0.1:1"})

	s.topology.authMethod = "hmac"

	ctx := metadata.NewIncomingContext(
		observedContext("claude-mcp", []string{"writer"}, nil),
		metadata.Pairs(contract.ClientVersionHeader, "hippocampus-mcp/0.52.0"),
	)

	if _, err := s.InterceptorObserveCaller(ctx, nil, observedRPCInfo(), passthroughHandler); err != nil {
		t.Fatalf("InterceptorObserveCaller: %s", err)
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	if got := nodesById(res)["declared:claude-mcp"].GetVersion(); got != "hippocampus-mcp/0.52.0" {
		t.Errorf("version = %q, want the one it reported as a caller", got)
	}
}

// TestTransferOutgoingContextCarriesThisVersion covers the other side of the header: this instance
// is itself a caller the Transfer target holds no address for.
func TestTransferOutgoingContextCarriesThisVersion(t *testing.T) {
	s := newTopologyServer(t)
	s.transfer.token = "secret"

	md, _ := metadata.FromOutgoingContext(s.transferOutgoingContext(context.Background()))

	if got := md.Get(contract.ClientVersionHeader); len(got) != 1 || got[0] != "hippocampus/v1.2.3" {
		t.Errorf("version header = %v, want [hippocampus/v1.2.3]", got)
	}

	if got := md.Get("authorization"); len(got) != 1 || got[0] != "Bearer secret" {
		t.Errorf("authorization = %v, want the transfer token", got)
	}
}

// TestWantTopologyVersion pins the three reasons to ask.
func TestWantTopologyVersion(t *testing.T) {
	now := time.Now()
	ok := contract.TopologyStatus_TOPOLOGY_STATUS_OK

	cases := map[string]struct {
		last topologyProbeResult
		seen bool
		want bool
	}{
		"never probed":     {seen: false, want: true},
		"last round down":  {seen: true, last: topologyProbeResult{status: contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE, versionAskedAt: now}, want: true},
		"recently asked":   {seen: true, last: topologyProbeResult{status: ok, versionAskedAt: now.Add(-time.Minute)}, want: false},
		"refresh due":      {seen: true, last: topologyProbeResult{status: ok, versionAskedAt: now.Add(-topologyVersionRefresh)}, want: true},
		"healthy, unasked": {seen: true, last: topologyProbeResult{status: ok}, want: true},
	}

	for name, tc := range cases {
		if got := wantTopologyVersion(tc.last, tc.seen, now); got != tc.want {
			t.Errorf("%s: want = %v, expected %v", name, got, tc.want)
		}
	}
}
