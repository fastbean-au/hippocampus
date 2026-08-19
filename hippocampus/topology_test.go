package hippocampus

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/summarise"
)

// TestRedactEndpoint is the security test for the whole view: the topology RPC defaults to the
// reader tier, and the only reason that is defensible is that no address reaching a caller carries
// a credential. Every shape below is one a real deployment can configure - all three drivers'
// connection strings, both Postgres forms, an authenticated OpenSearch URL - and each one hides a
// secret in a different position.
func TestRedactEndpoint(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"empty":              {raw: "", want: ""},
		"bare host and port": {raw: "localhost:11434", want: "localhost:11434"},
		"filesystem path":    {raw: "/var/lib/hippocampus", want: "/var/lib/hippocampus"},
		"http with no auth":  {raw: "http://opensearch:9200", want: "http://opensearch:9200"},
		"https with basic auth": {
			raw:  "https://admin:hunter2@opensearch.internal:9200",
			want: "https://opensearch.internal:9200",
		},
		"postgres url": {
			raw:  "postgres://hippo:s3cret@db.internal:5432/hippocampus?sslmode=require",
			want: "postgres://db.internal:5432/hippocampus",
		},
		"postgres url without credentials": {
			raw:  "postgres://db.internal:5432/hippocampus",
			want: "postgres://db.internal:5432/hippocampus",
		},
		"postgres keyword dsn": {
			raw:  "host=db.internal port=5432 dbname=hippocampus user=hippo password=s3cret sslmode=require",
			want: "db.internal:5432/hippocampus",
		},
		"postgres keyword dsn with only a host": {
			raw:  "host=db.internal password=s3cret",
			want: "db.internal",
		},
		"mysql dsn": {
			raw:  "hippo:s3cret@tcp(db.internal:3306)/hippocampus?parseTime=true",
			want: "tcp(db.internal:3306)/hippocampus",
		},
		"mysql dsn without credentials": {
			raw:  "tcp(db.internal:3306)/hippocampus",
			want: "tcp(db.internal:3306)/hippocampus",
		},
		"unparseable falls back to the blunt form": {
			raw:  "://user:pw@host/db?password=x",
			want: "://host/db",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := redactEndpoint(tc.raw)

			if got != tc.want {
				t.Errorf("redactEndpoint(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestRedactEndpointNeverLeaksASecret is the property the table above only samples. Whatever the
// shape, a secret placed in any of the positions a connection string offers must not survive - so
// this asserts the absence rather than the exact output, which is what actually matters and what
// would still hold if a new driver arrived with a fourth syntax.
func TestRedactEndpointNeverLeaksASecret(t *testing.T) {
	const secret = "sup3rs3cret"

	for _, raw := range []string{
		"postgres://hippo:" + secret + "@db:5432/hippocampus",
		"postgres://hippo:pw@db:5432/hippocampus?sslpassword=" + secret,
		"host=db port=5432 dbname=hippo password=" + secret,
		"host=db port=5432 dbname=hippo sslpassword=" + secret,
		"hippo:" + secret + "@tcp(db:3306)/hippocampus",
		"hippo:pw@tcp(db:3306)/hippocampus?tls=" + secret,
		"https://admin:" + secret + "@opensearch:9200",
		"https://opensearch:9200?token=" + secret,
	} {
		if redacted := redactEndpoint(raw); strings.Contains(redacted, secret) {
			t.Errorf("redactEndpoint(%q) leaked the secret: %q", raw, redacted)
		}
	}
}

// TestTopologyStatusFor covers the distinction the three ErrDegraded sentinels exist for: a
// dependency that answered and is unhappy is not the same operational problem as one that cannot be
// reached, and an operator sent to look at the wrong one loses the time it takes to find out.
func TestTopologyStatusFor(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want contract.TopologyStatus
	}{
		"healthy":               {err: nil, want: contract.TopologyStatus_TOPOLOGY_STATUS_OK},
		"unreachable":           {err: errors.New("connection refused"), want: contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE},
		"degraded index":        {err: errors.New("wrapped: " + search.ErrDegraded.Error()), want: contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE},
		"degraded search":       {err: errors.Join(search.ErrDegraded, errors.New("red")), want: contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED},
		"degraded summariser":   {err: errors.Join(summarise.ErrDegraded, errors.New("no model")), want: contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED},
		"degraded embedder":     {err: errors.Join(embed.ErrDegraded, errors.New("no model")), want: contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED},
		"context deadline":      {err: context.DeadlineExceeded, want: contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE},
		"empty error is a fact": {err: errors.New(""), want: contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE},
	} {
		t.Run(name, func(t *testing.T) {
			got, detail := topologyStatusFor(tc.err)

			if got != tc.want {
				t.Errorf("topologyStatusFor(%v) = %s, want %s", tc.err, got, tc.want)
			}

			if tc.err != nil && detail == "" && tc.err.Error() != "" {
				t.Error("a failing probe reported no detail; an operator is left with a colour and no reason")
			}
		})
	}
}

// newTopologyServer builds a server with the topology view enabled over an in-memory store, with
// every optional dependency absent - the default embedded deployment, which is the shape most
// installations run and the one whose diagram must still be worth looking at.
func newTopologyServer(t *testing.T) *Server {
	t.Helper()

	viper.Set("topology.enabled", true)
	viper.Set("storage.driver", "sqlite")
	viper.Set("storage.directory", "/var/lib/hippocampus")
	viper.Set("port", 50051)
	viper.Set("gateway.port", 8080)

	t.Cleanup(func() {
		for _, key := range []string{
			"topology.enabled", "storage.driver", "storage.directory", "port", "gateway.port",
			"topology.minimumTier", "opensearch.addresses", "opensearch.index", "auth.method",
			"observability.metrics.enabled", "observability.otlp.endpoint", "storage.postgres.dsn",
		} {
			viper.Set(key, nil)
		}
	})

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	s := &Server{db: database, consolidationEnabled: true}
	s.topology = topologyFromViper("v1.2.3")
	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	return s
}

// nodesById indexes a response for the assertions below.
func nodesById(res *contract.GetTopologyResponse) map[string]*contract.TopologyNode {
	out := make(map[string]*contract.TopologyNode, len(res.GetNodes()))

	for _, node := range res.GetNodes() {
		out[node.GetId()] = node
	}

	return out
}

// TestGetTopologyDescribesTheDefaultDeployment covers the shape an embedded install produces: this
// instance, its store, and the store-backed search index that comes with it - each sourced as
// something the instance knows first-hand rather than something it was told.
func TestGetTopologyDescribesTheDefaultDeployment(t *testing.T) {
	s := newTopologyServer(t)

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	self, ok := nodes[topologyNodeSelf]
	if !ok {
		t.Fatal("the response has no node for this instance")
	}

	if self.GetSource() != contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_SELF {
		t.Errorf("this instance is sourced %s, want SELF", self.GetSource())
	}

	if self.GetVersion() != "v1.2.3" {
		t.Errorf("this instance reports version %q, want the build it was given", self.GetVersion())
	}

	if role := attributeValue(self, "role"); role != "consolidator" {
		t.Errorf("role = %q, want consolidator", role)
	}

	store, ok := nodes[topologyNodeStore]
	if !ok {
		t.Fatal("the response has no node for the primary store")
	}

	if store.GetName() != "SQLite" {
		t.Errorf("store name = %q, want SQLite", store.GetName())
	}

	if store.GetDetail() != "/var/lib/hippocampus" {
		t.Errorf("store detail = %q, want the configured directory", store.GetDetail())
	}

	if res.GetProbeIntervalSeconds() <= 0 {
		t.Error("probe_interval_seconds is not positive; a polling client has nothing to pace itself by")
	}
}

// TestGetTopologyReportsDisabledComponents pins the decision to include optional components that
// are switched off. "Why is semantic search returning nothing" is answered by a disabled Embedder
// node naming the key that enables it, and by nothing else this service says - so dropping these
// nodes would quietly remove the view's most useful answer.
func TestGetTopologyReportsDisabledComponents(t *testing.T) {
	s := newTopologyServer(t)

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	for _, id := range []string{topologyNodeSummariser, topologyNodeEmbedder, topologyNodeObjects, topologyNodeTransfer, topologyNodeIdP, topologyNodeCollector} {
		node, ok := nodes[id]
		if !ok {
			t.Errorf("the response has no node for %q", id)

			continue
		}

		if node.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED {
			t.Errorf("%q is %s with nothing configured, want DISABLED", id, node.GetStatus())
		}

		if attributeValue(node, "enable_with") == "" && attributeValue(node, "probing") == "" {
			t.Errorf("%q is disabled but names neither the key that enables it nor why it is unprobed", id)
		}
	}
}

// unpingableObjectStore is an archive.ObjectStore that deliberately does NOT implement a Ping, which is
// the case pingerProbe exists for: a dependency that is configured and live but has nothing to
// check reports no status rather than a made-up one.
type unpingableObjectStore struct{}

func (unpingableObjectStore) Put(context.Context, string, io.Reader) error { return nil }

func (unpingableObjectStore) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// TestGetTopologyDescribesAFullyConfiguredDeployment is the other end of the range from the default
// embedded install: every optional component switched on, which is the shape the centralised
// deployment takes. What it checks is that each one is reported as configured and probed, and that
// the address shown is the redacted one - a builder that forgets redactEndpoint is invisible until
// somebody reads a password off a console.
func TestGetTopologyDescribesAFullyConfiguredDeployment(t *testing.T) {
	s := newTopologyServer(t)

	index, err := search.NewOpenSearch(search.Config{
		Addresses: []string{"https://admin:hunter2@opensearch.internal:9200"},
		Index:     "hippocampus-memories",
	})
	if err != nil {
		t.Fatalf("search.NewOpenSearch: %s", err)
	}

	t.Cleanup(func() { _ = index.Close() })

	summariser, err := summarise.NewOllama(summarise.Config{Address: "http://ollama:11434", Model: "llama3.2"})
	if err != nil {
		t.Fatalf("summarise.NewOllama: %s", err)
	}

	embedder, err := embed.NewOllama(embed.Config{Address: "http://ollama:11434", Model: "nomic-embed-text", Dimensions: 768})
	if err != nil {
		t.Fatalf("embed.NewOllama: %s", err)
	}

	s.search = index
	s.summarise = summariser
	s.embed = embedder
	s.objects = unpingableObjectStore{}
	s.transfer.targetAddress = "central.internal:50051"

	viper.Set("opensearch.addresses", []string{"https://admin:hunter2@opensearch.internal:9200"})
	viper.Set("opensearch.index", "hippocampus-memories")
	viper.Set("auth.method", "idp")
	viper.Set("auth.issuer", "https://idp.example.com/realms/hippo")
	viper.Set("observability.metrics.enabled", true)
	viper.Set("observability.otlp.endpoint", "otel-lgtm:4317")

	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	for _, id := range []string{topologyNodeSearch, topologyNodeSummariser, topologyNodeEmbedder, topologyNodeObjects, topologyNodeTransfer, topologyNodeIdP, topologyNodeCollector} {
		node, ok := nodes[id]
		if !ok {
			t.Errorf("the response has no node for %q", id)

			continue
		}

		if node.GetStatus() == contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED {
			t.Errorf("%q is DISABLED with the component configured", id)
		}

		if node.GetSource() != contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED {
			t.Errorf("%q is sourced %s, want CONFIGURED", id, node.GetSource())
		}
	}

	if detail := nodes[topologyNodeSearch].GetDetail(); strings.Contains(detail, "hunter2") {
		t.Errorf("the search node carries the cluster password: %q", detail)
	}

	if name := nodes[topologyNodeSearch].GetName(); name != "OpenSearch" {
		t.Errorf("search backend reported as %q, want OpenSearch", name)
	}

	// The probers are built from the specs, so this is what actually decides which nodes get a
	// status. The object store is the interesting entry: it is configured and live, but the fake
	// cannot be pinged, so it must not appear.
	probers := s.topologyProbers()

	for _, id := range []string{topologyNodeStore, topologyNodeSearch, topologyNodeSummariser, topologyNodeEmbedder} {
		if _, ok := probers[id]; !ok {
			t.Errorf("%q is probeable but has no probe", id)
		}
	}

	if _, ok := probers[topologyNodeObjects]; ok {
		t.Error("the object store has a probe despite its implementation having no Ping")
	}

	if _, ok := probers[topologyNodeTransfer]; ok {
		t.Error("the transfer target is probed without topology.probeTransferTarget")
	}
}

// TestSearchNodeReportsTheStoreBackedIndex covers the backend distinction no configuration key
// states: with OpenSearch off, SQLite still has a working content-search index and the server
// drivers have none, and only the live dependency says which happened.
func TestSearchNodeReportsTheStoreBackedIndex(t *testing.T) {
	s := newTopologyServer(t)

	index, err := search.NewSQL(s.db.(*db.DB))
	if err != nil {
		t.Skipf("content search is unavailable on this store: %s", err)
	}

	s.search = index
	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	node := nodesById(res)[topologyNodeSearch]

	if node.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_OK {
		t.Errorf("the store-backed index is %s, want OK", node.GetStatus())
	}

	if backend := attributeValue(node, "backend"); backend != "sql" {
		t.Errorf("backend = %q, want sql", backend)
	}

	// It shares the store's connection, so probing it would only be a second way of asking whether
	// the store is up - which the store node already answers.
	if _, ok := s.topologyProbers()[topologyNodeSearch]; ok {
		t.Error("the store-backed index has a probe of its own; the store node already reports that")
	}
}

// TestReplicaOmitsTheConsolidationSettings covers the one place the self node changes shape: a
// replica runs no cycle, and reporting the numbers it does not act on would send an operator to
// tune the wrong instance.
func TestReplicaOmitsTheConsolidationSettings(t *testing.T) {
	s := newTopologyServer(t)
	s.consolidationEnabled = false
	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	self := nodesById(res)[topologyNodeSelf]

	if role := attributeValue(self, "role"); role != "replica" {
		t.Errorf("role = %q, want replica", role)
	}

	if method := attributeValue(self, "consolidation_method"); method != "" {
		t.Errorf("a replica reports consolidation_method %q; it runs no cycle", method)
	}
}

// TestProbeTransferTargetReportsAnUnreachableTarget covers the opt-in probe. It uses an address
// nothing is listening on, which is the case that matters: the probe must return an error rather
// than block, since a transfer target is the one dependency on the far side of somebody else's
// network.
func TestProbeTransferTargetReportsAnUnreachableTarget(t *testing.T) {
	s := newTopologyServer(t)
	s.transfer.targetAddress = "127.0.0.1:1"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.probeTransferTarget(ctx); err == nil {
		t.Error("probing an address with nothing listening reported success")
	}
}

// TestRedactEndpointsJoinsAndRedacts covers the list form used for a multi-node cluster, including
// that an empty entry is dropped rather than rendered as a stray separator.
func TestRedactEndpointsJoinsAndRedacts(t *testing.T) {
	got := redactEndpoints([]string{"https://admin:pw@one:9200", "", "https://two:9200"})

	if got != "https://one:9200, https://two:9200" {
		t.Errorf("redactEndpoints = %q", got)
	}
}

// TestTopologyEdgesTerminateOnNodes is the structural guard: an edge naming a node that is not in
// the response draws a line to nowhere, which a diagram renders as either a crash or a stray arrow.
// It is the failure mode a hand-written edge list has, so it is the one worth a test.
func TestTopologyEdgesTerminateOnNodes(t *testing.T) {
	s := newTopologyServer(t)

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	for _, edge := range res.GetEdges() {
		if _, ok := nodes[edge.GetFromId()]; !ok {
			t.Errorf("edge %s -> %s starts at a node that is not in the response", edge.GetFromId(), edge.GetToId())
		}

		if _, ok := nodes[edge.GetToId()]; !ok {
			t.Errorf("edge %s -> %s ends at a node that is not in the response", edge.GetFromId(), edge.GetToId())
		}
	}
}

// TestGetTopologyRedactsTheStoreDSN is the end-to-end form of the redaction test: it is not enough
// that redactEndpoint is correct if a builder forgets to call it, and the store is the node most
// likely to be handed a password.
func TestGetTopologyRedactsTheStoreDSN(t *testing.T) {
	s := newTopologyServer(t)

	viper.Set("storage.driver", "postgres")
	viper.Set("storage.postgres.dsn", "postgres://hippo:sup3rs3cret@db.internal:5432/hippocampus")

	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	store := nodesById(res)[topologyNodeStore]

	if store == nil {
		t.Fatal("the response has no node for the primary store")
	}

	if strings.Contains(store.GetDetail(), "sup3rs3cret") {
		t.Fatalf("the store node carries the DSN password: %q", store.GetDetail())
	}

	if !strings.Contains(store.GetDetail(), "db.internal") {
		t.Errorf("the store node lost its host as well as its password: %q", store.GetDetail())
	}
}

// TestGetTopologyRefusedWhenDisabled covers the switch: the RPC reports a precondition failure
// rather than an empty deployment, since a client cannot tell "nothing is configured" from
// "nobody is answering" out of an empty node list.
func TestGetTopologyRefusedWhenDisabled(t *testing.T) {
	s := newTopologyServer(t)
	s.topology.enabled = false

	if _, err := s.GetTopology(context.Background(), &contract.EmptyRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GetTopology = %v, want FailedPrecondition", err)
	}
}

// TestTopologyTierReportedByWhoAmI covers the client's side of the configurable tier: WhoAmI has to
// report what this deployment requires, because the console decides whether to offer the view at
// all from it - and an empty string has to mean "switched off" rather than "unset", or a disabled
// deployment would show a tab that always fails.
func TestTopologyTierReportedByWhoAmI(t *testing.T) {
	s := newTopologyServer(t)

	if tier := s.topologyTier(); tier != auth.TierReader.String() {
		t.Errorf("an unconfigured tier reports %q, want the policy default %q", tier, auth.TierReader)
	}

	s.topology.minimumTier = "Admin"

	if tier := s.topologyTier(); tier != auth.TierAdmin.String() {
		t.Errorf("a configured tier reports %q, want it normalised to %q", tier, auth.TierAdmin)
	}

	s.topology.enabled = false

	if tier := s.topologyTier(); tier != "" {
		t.Errorf("a disabled view reports tier %q, want empty", tier)
	}
}

// TestTopologyProberPublishesResults covers the prober's contract with the RPC: statuses come from
// a background round, and the RPC only reads them. The hanging probe is the point - it is bounded
// by the probe timeout rather than by the caller, which is the whole reason probing does not happen
// inside the handler.
func TestTopologyProberPublishesResults(t *testing.T) {
	s := newTopologyServer(t)
	s.topology.probeTimeout = 50 * time.Millisecond

	probers := map[string]topologyProbe{
		topologyNodeStore: func(context.Context) error { return nil },
		topologyNodeSearch: func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		},
	}

	// The specs decide which nodes read a probe result, so the search node has to be probed for its
	// status to come through.
	for i := range s.topology.nodes {
		if s.topology.nodes[i].id == topologyNodeSearch {
			s.topology.nodes[i].probe = true
		}
	}

	started := time.Now()

	s.probeTopologyOnce(probers)

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("a round took %s; the hanging probe was not bounded by the probe timeout", elapsed)
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	if got := nodes[topologyNodeStore].GetStatus(); got != contract.TopologyStatus_TOPOLOGY_STATUS_OK {
		t.Errorf("the store is %s after a successful probe, want OK", got)
	}

	if nodes[topologyNodeStore].GetCheckedAt() == 0 {
		t.Error("the store reports no checked_at after a probe; a client cannot tell a fresh status from a never-run one")
	}

	if got := nodes[topologyNodeSearch].GetStatus(); got != contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE {
		t.Errorf("the hanging dependency is %s, want UNREACHABLE", got)
	}
}

// TestTopologyProbeResultsBeforeTheFirstRound covers the window between startup and the first
// probe: the RPC must answer, and must answer with "not checked" rather than with a status nobody
// established.
func TestTopologyProbeResultsBeforeTheFirstRound(t *testing.T) {
	s := newTopologyServer(t)

	if results := s.topologyProbeResults(); results == nil {
		t.Fatal("topologyProbeResults returned nil; callers are documented not to need a guard")
	}

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	store := nodesById(res)[topologyNodeStore]

	if store.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_UNSPECIFIED {
		t.Errorf("the store is %s before any probe has run, want UNSPECIFIED", store.GetStatus())
	}

	if store.GetCheckedAt() != 0 {
		t.Error("the store reports a checked_at before any probe has run")
	}
}

// TestTopologyProberStops covers shutdown: Stop must drain the prober like it drains the sleep and
// reconcile loops, or a probe outlives the database it pings.
func TestTopologyProberStops(t *testing.T) {
	s := newTopologyServer(t)
	s.topology.probeInterval = 10 * time.Millisecond

	s.startTopologyProber()

	if s.stopTopology == nil {
		t.Fatal("the prober did not start with a probeable dependency configured")
	}

	done := make(chan struct{})

	go func() {
		s.stopTopologyProber()
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(5 * time.Second):
		t.Fatal("stopTopologyProber did not return; shutdown would hang")

	}
}

// TestTopologyValueRenderers covers the small renderers, which exist because a bare zero in this
// view is ambiguous in exactly the places it matters: "0" in a capacity row could mean no limit or
// no memories allowed, and an empty bind address could mean unconfigured or every interface. Each
// pair below is one of those readings against its opposite.
func TestTopologyValueRenderers(t *testing.T) {
	if got := countDescription(0); got != "unset" {
		t.Errorf("countDescription(0) = %q, want unset", got)
	}

	if got := countDescription(-1); got != "unset" {
		t.Errorf("countDescription(-1) = %q, want unset", got)
	}

	if got := countDescription(42); got != "42" {
		t.Errorf("countDescription(42) = %q", got)
	}

	if got := periodDescription(0); got != "disabled" {
		t.Errorf("periodDescription(0) = %q, want disabled", got)
	}

	if got := periodDescription(90 * time.Second); got != "1m30s" {
		t.Errorf("periodDescription(90s) = %q", got)
	}

	if got := enabledDescription(false); got != "disabled" {
		t.Errorf("enabledDescription(false) = %q", got)
	}

	if got := heldDescription(false); !strings.Contains(got, "another") {
		t.Errorf("heldDescription(false) = %q; a replica must not read as holding the lock", got)
	}

	if got := heldDescription(true); !strings.Contains(got, "this") {
		t.Errorf("heldDescription(true) = %q", got)
	}
}

// TestAuthMethodDescription pins the resolution of the deprecated boolean. The view must agree with
// what is actually enforced, and auth.enabled true with no auth.method is hmac - a view reporting
// "none" there would say the instance is open when it is not.
func TestAuthMethodDescription(t *testing.T) {
	t.Cleanup(func() {
		viper.Set("auth.method", nil)
		viper.Set("auth.enabled", nil)
	})

	viper.Set("auth.method", "")
	viper.Set("auth.enabled", false)

	if got := authMethodDescription(); got != "none" {
		t.Errorf("authMethodDescription = %q, want none", got)
	}

	viper.Set("auth.enabled", true)

	if got := authMethodDescription(); got != "hmac" {
		t.Errorf("the deprecated auth.enabled resolves to %q, want hmac", got)
	}

	viper.Set("auth.method", "idp")

	if got := authMethodDescription(); got != "idp" {
		t.Errorf("authMethodDescription = %q, want idp", got)
	}
}

// TestBindAddressRendersEveryInterface covers the reading that matters: an unset bind address in a
// config file looks like "not configured" and in a listener means every interface, and the second
// is the one an operator needs to be shown.
func TestBindAddressRendersEveryInterface(t *testing.T) {
	t.Cleanup(func() { viper.Set("bindAddress", nil) })

	viper.Set("bindAddress", "")

	if got := bindAddressOrAll("bindAddress"); got != "0.0.0.0" {
		t.Errorf("an unset bind address renders as %q, want 0.0.0.0", got)
	}

	viper.Set("bindAddress", "127.0.0.1")

	if got := bindAddressOrAll("bindAddress"); got != "127.0.0.1" {
		t.Errorf("bindAddressOrAll = %q", got)
	}
}

// TestGatewayDisabledIsReportedAsSuch covers the zero port, which is a supported mode rather than a
// mistake - and one that takes the console, the OpenAPI document and the HTTP probes with it.
func TestGatewayDisabledIsReportedAsSuch(t *testing.T) {
	t.Cleanup(func() { viper.Set("gateway.port", nil) })

	viper.Set("gateway.port", 0)

	if got := gatewayDescription(); got != "disabled" {
		t.Errorf("gatewayDescription with no port = %q, want disabled", got)
	}
}

// attributeValue reads one attribute off a node, or "" when it carries none.
func attributeValue(node *contract.TopologyNode, key string) string {
	for _, attribute := range node.GetAttributes() {
		if attribute.GetKey() == key {
			return attribute.GetValue()
		}
	}

	return ""
}

// --------------------------------------------------------------- declared components

// declaredServer builds a server whose deployment includes components an operator has declared.
func declaredServer(t *testing.T, components ...TopologyComponent) *Server {
	t.Helper()

	s := newTopologyServer(t)
	s.topology.components = components
	s.topology.nodes, s.topology.edges = s.buildTopologySpecs()

	return s
}

// TestDeclaredComponentsAppearAsDeclared covers the half of the deployment an instance cannot
// discover. What matters is the source: a declared component is not something this instance found,
// and a client that rendered it as though it were would be presenting a survey it never made.
func TestDeclaredComponentsAppearAsDeclared(t *testing.T) {
	s := declaredServer(t,
		TopologyComponent{Name: "nats-bridge", Kind: "bridge", HealthURL: "http://nats-bridge:8090"},
		TopologyComponent{Name: "ingestor", Kind: "ingestor", HealthURL: "http://ingestor:8090"},
	)

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	nodes := nodesById(res)

	bridge, ok := nodes["declared:nats-bridge"]
	if !ok {
		t.Fatalf("the declared bridge has no node; got %v", slices.Sorted(maps.Keys(nodes)))
	}

	if bridge.GetSource() != contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_DECLARED {
		t.Errorf("a declared component is sourced %s, want DECLARED", bridge.GetSource())
	}

	if bridge.GetKind() != contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_BRIDGE {
		t.Errorf("kind = %s, want BRIDGE", bridge.GetKind())
	}

	if ingestor := nodes["declared:ingestor"]; ingestor.GetKind() != contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_INGESTOR {
		t.Errorf("the ingestor's kind = %s, want INGESTOR", ingestor.GetKind())
	}

	// The id is namespaced so an operator naming a component "store" cannot quietly replace the
	// primary store's node on the diagram.
	if _, ok := nodes["nats-bridge"]; ok {
		t.Error("a declared component claimed an unnamespaced id")
	}
}

// TestDeclaredComponentEdgesPointInward is the one thing the declared half really adds, and the one
// thing easy to get backwards. A bridge holds an address for this service; this service holds only a
// health endpoint for it, which is not the connection being drawn. Every other edge in the graph
// runs outward, so an inward one is what makes the picture a deployment rather than a dependency
// list.
func TestDeclaredComponentEdgesPointInward(t *testing.T) {
	s := declaredServer(t, TopologyComponent{Name: "mqtt-bridge", Kind: "bridge", HealthURL: "http://mqtt:8090"})

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	found := false

	for _, edge := range res.GetEdges() {
		if edge.GetFromId() != "declared:mqtt-bridge" {
			continue
		}

		found = true

		if edge.GetToId() != topologyNodeSelf {
			t.Errorf("the declared edge runs to %q, want this instance", edge.GetToId())
		}
	}

	if !found {
		t.Error("the declared component has no edge; it would be drawn attached to nothing")
	}

	// And the reverse must not exist: this instance does not dial a bridge.
	for _, edge := range res.GetEdges() {
		if edge.GetFromId() == topologyNodeSelf && edge.GetToId() == "declared:mqtt-bridge" {
			t.Error("an outbound edge to a declared component was drawn; the connection runs the other way")
		}
	}
}

// TestDeclaredComponentsAreAlwaysProbed pins that a declared component gets a probe. One without is
// a comment in a config file rendered as a live component, which is why healthUrl is required.
func TestDeclaredComponentsAreAlwaysProbed(t *testing.T) {
	s := declaredServer(t, TopologyComponent{Name: "cli-host", Kind: "client", HealthURL: "http://host:9000"})

	if _, ok := s.topologyProbers()["declared:cli-host"]; !ok {
		t.Error("a declared component has no probe")
	}
}

// TestHealthProbeURL covers the path rule: the common case is an operator writing the address the
// bridge serves its health on and nothing else, so a bare URL gets /readyz appended; a URL carrying
// a path belongs to something behind a proxy and is used as written.
func TestHealthProbeURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://bridge:8090":          "http://bridge:8090/readyz",
		"http://bridge:8090/":         "http://bridge:8090/readyz",
		"https://bridge.internal":     "https://bridge.internal/readyz",
		"http://proxy/bridge/healthz": "http://proxy/bridge/healthz",
		"":                            "",
	} {
		if got := healthProbeURL(raw); got != want {
			t.Errorf("healthProbeURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestProbeHealthEndpoint covers the three outcomes, and the 503 is the point of the whole
// exercise: the bridges and the ingestor already serve a per-dependency breakdown at /readyz, so a
// component that is running and cannot reach its own broker says so, instead of rendering
// identically to one that cannot reach us.
func TestProbeHealthEndpoint(t *testing.T) {
	for name, tc := range map[string]struct {
		status       int
		body         string
		wantErr      bool
		wantDegraded bool
		wantDetail   string
	}{
		"ready": {
			status: http.StatusOK,
			body:   `{"status":"ready","component":"nats-bridge","dependencies":{"hippocampus":"ok"}}`,
		},
		"not ready, and says which end": {
			status:       http.StatusServiceUnavailable,
			body:         `{"status":"not ready","component":"nats-bridge","dependencies":{"hippocampus":"unreachable","broker":"ok"}}`,
			wantErr:      true,
			wantDegraded: true,
			wantDetail:   "hippocampus",
		},
		"not ready with no breakdown": {
			status:       http.StatusServiceUnavailable,
			body:         `{}`,
			wantErr:      true,
			wantDegraded: true,
			wantDetail:   "not ready",
		},
		"not ready and not even JSON": {
			status:       http.StatusServiceUnavailable,
			body:         "Service Unavailable",
			wantErr:      true,
			wantDegraded: true,
			wantDetail:   "not ready",
		},
		// A wrong URL is a configuration mistake rather than a component that is down, and the code
		// is what makes the two distinguishable.
		"wrong path": {
			status:     http.StatusNotFound,
			body:       "not found",
			wantErr:    true,
			wantDetail: "404",
		},
		// Anything that answers 200 is healthy, whether or not it is one of ours.
		"something else entirely": {
			status: http.StatusOK,
			body:   "<html>fine</html>",
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))

			t.Cleanup(server.Close)

			err := probeHealthEndpoint(context.Background(), server.URL+"/readyz")

			if tc.wantErr && err == nil {
				t.Fatal("the probe reported success")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("the probe failed: %s", err)
			}

			if degraded := errors.Is(err, errDeclaredDegraded); degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v (err: %v)", degraded, tc.wantDegraded, err)
			}

			if tc.wantDetail != "" && !strings.Contains(err.Error(), tc.wantDetail) {
				t.Errorf("the error does not mention %q: %s", tc.wantDetail, err)
			}
		})
	}
}

// TestProbeHealthEndpointUnreachable covers a component that is not answering at all - which must be
// distinguishable from one answering 503, since they are different problems with different owners.
func TestProbeHealthEndpointUnreachable(t *testing.T) {
	err := probeHealthEndpoint(context.Background(), "http://127.0.0.1:1/readyz")

	if err == nil {
		t.Fatal("the probe reported success against a port with nothing listening")
	}

	if errors.Is(err, errDeclaredDegraded) {
		t.Error("an unreachable component was reported as degraded")
	}

	if err := probeHealthEndpoint(context.Background(), ""); err == nil {
		t.Error("an empty health URL reported success")
	}
}

// TestDeclaredComponentStatusReachesTheResponse is the end-to-end form: a component reporting itself
// not ready must arrive at the caller as DEGRADED with its own explanation, not as a bare colour.
func TestDeclaredComponentStatusReachesTheResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not ready","dependencies":{"hippocampus":"unreachable"}}`))
	}))

	t.Cleanup(server.Close)

	s := declaredServer(t, TopologyComponent{Name: "bridge", Kind: "bridge", HealthURL: server.URL})

	s.probeTopologyOnce(s.topologyProbers())

	res, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	node := nodesById(res)["declared:bridge"]

	if node.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED {
		t.Fatalf("the component is %s, want DEGRADED", node.GetStatus())
	}

	if !strings.Contains(node.GetStatusDetail(), "hippocampus") {
		t.Errorf("the status does not name the failing dependency: %q", node.GetStatusDetail())
	}

	if node.GetCheckedAt() == 0 {
		t.Error("the component reports no checked_at after a probe")
	}
}

// TestProbeRoundRunsConcurrently pins the reason the round stopped being sequential. With the
// declared list operator-controlled, a sequential round no longer fits inside its own interval - so
// this asserts a round of slow probes takes closer to one timeout than to N of them.
func TestProbeRoundRunsConcurrently(t *testing.T) {
	s := newTopologyServer(t)
	s.topology.probeTimeout = 200 * time.Millisecond

	probers := make(map[string]topologyProbe, topologyProbeConcurrency)

	for i := range topologyProbeConcurrency {
		probers[strconv.Itoa(i)] = func(ctx context.Context) error {
			<-ctx.Done()

			return ctx.Err()
		}
	}

	started := time.Now()

	s.probeTopologyOnce(probers)

	// Sequentially this would be topologyProbeConcurrency x 200ms; concurrently it is one 200ms
	// window plus scheduling. Half the sequential cost is a wide enough margin to be stable on a
	// loaded machine while still failing if the concurrency is ever removed.
	if elapsed := time.Since(started); elapsed > time.Duration(topologyProbeConcurrency)*100*time.Millisecond {
		t.Errorf("a round of %d slow probes took %s; they ran sequentially", len(probers), elapsed)
	}

	if got := len(s.topologyProbeResults()); got != len(probers) {
		t.Errorf("published %d results for %d probes", got, len(probers))
	}
}

// TestTopologyComponentKinds pins the accepted kind names. They are a contract with every deployed
// config file - main.go's validation refuses anything else at startup - so the set is derived from
// the table here rather than restated there, and this is what stops the two drifting.
func TestTopologyComponentKinds(t *testing.T) {
	kinds := TopologyComponentKinds()

	if !slices.IsSorted(kinds) {
		t.Errorf("the kind list is unsorted, so a validation message would reorder itself: %v", kinds)
	}

	for _, want := range []string{"bridge", "client", "ingestor", "mcp"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("the kind %q is no longer accepted; every config file naming it now fails to start", want)
		}
	}

	if len(kinds) != len(topologyComponentKinds) {
		t.Errorf("TopologyComponentKinds returned %d of %d kinds", len(kinds), len(topologyComponentKinds))
	}
}

// TestDeclaredNodeFallsBackOnAnUnknownKind covers the branch validateConfig makes unreachable in a
// running service. Drawing a generic client beats drawing nothing if a kind ever does get through -
// a component missing from the diagram is far harder to notice than one with the wrong shape.
func TestDeclaredNodeFallsBackOnAnUnknownKind(t *testing.T) {
	spec := declaredNodeSpec(TopologyComponent{Name: "odd", Kind: "broker", HealthURL: "http://x:1"})

	if spec.kind != contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_CLIENT {
		t.Errorf("kind = %s, want the CLIENT fallback", spec.kind)
	}

	if !spec.probe {
		t.Error("a declared component was not marked for probing")
	}
}

// TestDeclaredProbeUnknownName covers the lookup: the probe map is built from the node specs, so a
// name with no component behind it means the two have disagreed, and returning nil leaves the node
// reporting "not checked" rather than panicking on a nil URL.
func TestDeclaredProbeUnknownName(t *testing.T) {
	s := declaredServer(t, TopologyComponent{Name: "known", Kind: "bridge", HealthURL: "http://x:1"})

	if probe := s.declaredProbe("missing"); probe != nil {
		t.Error("a probe was returned for a component that is not declared")
	}

	if probe := s.declaredProbe("known"); probe == nil {
		t.Error("no probe was returned for a declared component")
	}
}

// TestTopologyFromViperSurvivesAMalformedComponentList covers the reload path. validateConfig
// rejects a malformed list at startup, so reaching here means the configuration changed under a
// running process - and losing the declared half of a diagnostic view is not a reason to refuse to
// build the server that serves the store.
func TestTopologyFromViperSurvivesAMalformedComponentList(t *testing.T) {
	t.Cleanup(func() { viper.Set("topology.components", nil) })

	viper.Set("topology.components", "not a list at all")

	topology := topologyFromViper("v1.2.3")

	if len(topology.components) != 0 {
		t.Errorf("a malformed list produced %d components", len(topology.components))
	}
}

// TestCollectorNodeReportsATracesOnlyDeployment covers the one combination the two tests above
// cannot: tracing on with metrics off. Both of them enable metrics, so the collector node appeared
// for that reason alone and the traces half of the condition was never read - which is how it went
// out asking viper for `observability.traces.enabled`, a key nothing in the service sets. The
// symptom was a node shown as DISABLED while spans were being exported to it, carrying an
// `enable_with` hint naming a key an operator could set to no effect.
func TestCollectorNodeReportsATracesOnlyDeployment(t *testing.T) {
	t.Cleanup(func() {
		viper.Set("observability.tracing.enabled", false)
		viper.Set("observability.otlp.endpoint", "")
	})

	viper.Set("observability.tracing.enabled", true)
	viper.Set("observability.metrics.enabled", false)
	viper.Set("observability.otlp.endpoint", "otel-lgtm:4317")

	spec := collectorNodeSpec()

	if spec.staticStatus == contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED {
		t.Error("the collector is DISABLED with tracing enabled")
	}

	if spec.detail != "otel-lgtm:4317" {
		t.Errorf("detail = %q, want the configured endpoint", spec.detail)
	}

	var traces string

	for _, attribute := range spec.attributes {
		if attribute.key == "traces" {
			traces = attribute.value
		}
	}

	if traces != "enabled" {
		t.Errorf("the traces attribute is %q, want \"enabled\"", traces)
	}
}
