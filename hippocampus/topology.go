package hippocampus

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/search"
)

// The deployment topology view.
//
// What this file assembles is deliberately modest, and the modesty is the design. An instance is
// not a control plane: it knows itself and whatever it dials outward, and every other component of
// a real deployment - replicas sharing its database, the broker bridges, the ingestor, MCP servers,
// the CLI - dials IN. They hold an address for the service; the service holds nothing for them.
//
// So rather than present a picture that quietly implies otherwise, every node carries a source
// saying how this instance came to know about it, and the console renders that. A sparse diagram
// then reads as "nothing has been declared", which is true, instead of as "nothing is running",
// which would not be.
//
// Three properties hold this together and should survive any change here:
//
//  1. Nothing secret is built into a node. Endpoints are redacted at CONSTRUCTION (see New), so the
//     Server never even holds a DSN password for this purpose, and redactEndpoint is the only way an
//     address reaches a spec. The required tier is configurable and defaults to reader, which is
//     defensible only because of this.
//  2. The specs are plain structs, converted to proto per call. A proto message is not safe to
//     marshal concurrently - marshalling writes its internal size cache - so a shared, pre-built
//     response would be a race under two viewers, the same trap PreviewConsolidation documents.
//  3. Statuses come from a background prober (topology_probe.go), never from the RPC. Probing N
//     dependencies inside the handler would turn one console page into N outbound requests and one
//     hung dependency into a hung RPC.

// Topology is the deployment-view configuration, read from the topology.* block, plus the
// pre-redacted description of what this instance is attached to.
type Topology struct {
	enabled     bool
	minimumTier string

	// probeInterval is how often every probe runs, and probeTimeout bounds one of them. A round runs
	// its probes a few at a time (see probeTopologyOnce), so it takes roughly
	// len(probers)/topologyProbeConcurrency x probeTimeout; keeping the interval above that is what
	// stops a round still running when the next is due.
	probeInterval time.Duration
	probeTimeout  time.Duration

	// probeTransferTarget opts into probing the Transfer target, which is the one dependency whose
	// probe dials a remote Hippocampus instance on a timer. Off by default: a view is not a good
	// enough reason to hold a connection open to somebody else's service every interval.
	probeTransferTarget bool

	// components are the parts of the deployment an operator has declared, because this instance
	// cannot discover them: they dial IN. Empty by default, which is why a fresh deployment's view
	// shows only what it dials.
	components []TopologyComponent

	// heartbeatInterval paces the instance registry (peers.go): how often this instance writes its
	// own row and reads its peers'. Zero disables both halves - an instance then neither appears to
	// its peers nor sees them, which is the honest pairing, since a deployment where only some
	// instances register would produce a different diagram depending on which one you asked.
	heartbeatInterval time.Duration

	// instanceId is this instance's registry key, hostname:port. Resolved once at startup: it is
	// written on every heartbeat and must not change under a row that has already been written.
	instanceId string
	hostname   string

	// startedAt is when this process began serving, reported in its registry row and used to hold
	// the no-consolidator warning back over a rolling deployment's first moments.
	startedAt time.Time

	version string

	nodes []topologyNodeSpec
	edges []topologyEdgeSpec
}

// TopologyComponent is one declared component, as read from topology.components.
//
// The whole of it is three fields, and that is the point: this is a label and an address to probe,
// not a registration. The instance learns nothing from a component beyond what its health endpoint
// answers, cannot reach it for anything else, and forgets it entirely if the entry is removed.
type TopologyComponent struct {
	// Name labels the component in the view and forms its node id. It must be unique among the
	// declared components, since a client keeps per-node state keyed by that id.
	Name string `mapstructure:"name"`

	// Kind selects the shape drawn for it and nothing else - the service never treats a bridge
	// differently from an ingestor. See topologyComponentKinds for the accepted values.
	Kind string `mapstructure:"kind"`

	// HealthURL is where to probe it. A URL with no path gets "/readyz" appended, which is what the
	// bridges and the ingestor serve on their --health-port; a URL carrying a path is used verbatim,
	// for anything sitting behind a proxy or serving its health on a route of its own.
	HealthURL string `mapstructure:"healthUrl"`
}

// topologyComponentKinds maps the kind an operator writes onto the node kind drawn for it. It is
// the accepted set: main.go's validation rejects anything else at startup rather than quietly
// drawing an unlabelled box, since a misspelled kind is otherwise indistinguishable from a
// component that simply looks odd on the diagram.
var topologyComponentKinds = map[string]contract.TopologyNodeKind{
	"bridge":   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_BRIDGE,
	"ingestor": contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_INGESTOR,
	"mcp":      contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_MCP_BRIDGE,
	"client":   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_CLIENT,
}

// TopologyComponentKinds returns the accepted kind names, sorted, for validation messages and
// documentation. Derived from the table rather than restated, so the two cannot disagree.
func TopologyComponentKinds() []string {
	out := make([]string, 0, len(topologyComponentKinds))

	for kind := range topologyComponentKinds {
		out = append(out, kind)
	}

	sort.Strings(out)

	return out
}

// MaxTopologyComponents caps the declared list. It is a real bound rather than a formality: every
// entry is an outbound HTTP request on every probe round, and the component name is a metric
// attribute, so an unbounded list would be both unbounded work and unbounded cardinality.
const MaxTopologyComponents = 32

// topologyNodeSpec is a node's fixed half: everything derived from configuration, which cannot
// change while the process runs. The moving half - status, and when it was last checked - is merged
// in per call from the prober's snapshot.
type topologyNodeSpec struct {
	id     string
	kind   contract.TopologyNodeKind
	name   string
	detail string
	source contract.TopologyNodeSource

	// attributes is ordered, because it is rendered as a table and an operator reading two
	// instances side by side should not have to hunt for the same row in a different place.
	attributes []topologyAttribute

	// version is what the component reports of its own build. Only two kinds of node can carry one:
	// this instance, and a peer that wrote its own into the registry. Everything else would have to
	// return it from a probe, which none of them asks for.
	version string

	// probe says this node has an entry in the prober's map; staticStatus is what to report when it
	// does not. The two are exclusive: a node is either probed or it states a fixed status, and the
	// fixed ones are meaningful - DISABLED for a feature that is configured off, UNSPECIFIED for one
	// that is deliberately never probed.
	probe        bool
	staticStatus contract.TopologyStatus

	// statusDetail explains a staticStatus that is not OK, for the nodes whose status is derived
	// rather than probed - a peer that has stopped writing its heartbeat is the only one today.
	statusDetail string

	// checkedAt is when a derived status was last established, for the same nodes. Zero means the
	// status was asserted rather than observed, which is what an unprobed node reports.
	checkedAt time.Time
}

type topologyAttribute struct {
	key   string
	value string
}

type topologyEdgeSpec struct {
	from     string
	to       string
	label    string
	optional bool
}

// The node ids. They are stable strings rather than generated, because a client keeps per-node UI
// state (which row is expanded, which box is selected) across polls, and an id that moved would
// make that flicker.
const (
	topologyNodeSelf       = "self"
	topologyNodeStore      = "store"
	topologyNodeSearch     = "search"
	topologyNodeSummariser = "summariser"
	topologyNodeEmbedder   = "embedder"
	topologyNodeObjects    = "objects"
	topologyNodeTransfer   = "transfer"
	topologyNodeIdP        = "idp"
	topologyNodeCollector  = "collector"
	topologyNodeCallbacks  = "callbacks"

	// topologyDeclaredPrefix namespaces a declared component's id, so an operator naming a component
	// "store" cannot collide with the primary store's node and quietly replace it on the diagram.
	topologyDeclaredPrefix = "declared:"

	// topologyPeerPrefix namespaces a discovered peer's id for the same reason. A peer's id is
	// hostname:port, which nothing else here could produce, but the two namespaces make a node id
	// say where it came from without consulting its source.
	topologyPeerPrefix = "peer:"
)

// GetTopology reports the deployment as this instance understands it.
//
// The scope refusal comes first and is not negotiable: a group-scoped caller could only ever be
// shown infrastructure that is not theirs, since there is no per-group topology to answer with. The
// required TIER, by contrast, is the operator's to set (topology.minimumTier) - see the note beside
// the scopes entry in scope.go, which is where the two are told apart.
func (s *Server) GetTopology(ctx context.Context, _ *contract.EmptyRequest) (*contract.GetTopologyResponse, error) {
	log.Debug("GetTopology()")

	if err := s.requireUnbound(ctx, "GetTopology"); err != nil {
		return nil, err
	}

	if !s.topology.enabled {
		return nil, status.Error(codes.FailedPrecondition, "the deployment topology view is disabled on this instance (topology.enabled)")
	}

	return s.topologyResponse(), nil
}

// topologyResponse converts the specs to proto and merges in the prober's latest statuses. It
// builds a fresh message every call - see the concurrency note at the top of this file.
func (s *Server) topologyResponse() *contract.GetTopologyResponse {
	probes := s.topologyProbeResults()

	// The peers are the one part of the graph that is not fixed at startup: instances join and leave
	// a shared store while this one runs. They are merged here rather than rebuilt into the specs so
	// that the fixed half stays immutable and needs no lock - the snapshot is swapped whole by the
	// heartbeat goroutine, exactly as the probe results are by the prober.
	peers := s.peerSnapshotOrEmpty()

	// The observed callers are read here rather than published by a goroutine, because nothing about
	// them is periodic: an entry is written by whichever request created it, and assembling the view
	// is the only thing that ever reads the set.
	now := time.Now()
	observed := s.buildObservedSnapshot(now)

	out := &contract.GetTopologyResponse{
		Nodes:                make([]*contract.TopologyNode, 0, len(s.topology.nodes)+len(peers.nodes)+len(observed.nodes)),
		Edges:                make([]*contract.TopologyEdge, 0, len(s.topology.edges)+len(peers.edges)+len(observed.edges)),
		ProbeIntervalSeconds: int64(s.topology.probeInterval / time.Second),
		GeneratedAt:          now.UnixNano(),
		Warnings:             peers.warnings,
	}

	for _, spec := range concatSpecs(s.topology.nodes, peers.nodes, observed.nodes) {
		node := &contract.TopologyNode{
			Id:           spec.id,
			Kind:         spec.kind,
			Name:         spec.name,
			Detail:       spec.detail,
			Source:       spec.source,
			Status:       spec.staticStatus,
			StatusDetail: spec.statusDetail,
			Version:      spec.version,
			Attributes:   make([]*contract.TopologyAttribute, 0, len(spec.attributes)),
		}

		if !spec.checkedAt.IsZero() {
			node.CheckedAt = spec.checkedAt.UnixNano()
		}

		if spec.probe {
			if result, ok := probes[spec.id]; ok {
				node.Status = result.status
				node.StatusDetail = result.detail
				node.CheckedAt = result.checkedAt.UnixNano()
			}
		}

		attributes := spec.attributes

		// What a shared store is shared WITH belongs to the store rather than to any one instance,
		// and is only known once the registry has been read - so it is merged here for the same
		// reason the peer nodes are.
		if spec.id == topologyNodeStore {
			attributes = append(append([]topologyAttribute{}, attributes...), peers.storeAttributes...)
		}

		// Likewise for what the observed callers say about a node that was built elsewhere: how many
		// clients this instance has seen (on self), and whether a declared component has actually
		// called (on that component). Both are properties of a node that only the request path knows.
		if extra, ok := observed.attributes[spec.id]; ok {
			attributes = append(append([]topologyAttribute{}, attributes...), extra...)
		}

		for _, attribute := range attributes {
			node.Attributes = append(node.Attributes, &contract.TopologyAttribute{
				Key:   attribute.key,
				Value: attribute.value,
			})
		}

		out.Nodes = append(out.Nodes, node)
	}

	for _, spec := range concatEdges(s.topology.edges, peers.edges, observed.edges) {
		out.Edges = append(out.Edges, &contract.TopologyEdge{
			FromId:   spec.from,
			ToId:     spec.to,
			Label:    spec.label,
			Optional: spec.optional,
		})
	}

	return out
}

// topologyFromViper reads the topology.* block. The probe interval and timeout fall back to
// defaults rather than disabling themselves at zero, since a zero timeout would make every probe
// fail instantly and a zero interval would spin.
func topologyFromViper(version string) Topology {
	t := Topology{
		enabled:             viper.GetBool("topology.enabled"),
		minimumTier:         viper.GetString("topology.minimumTier"),
		probeInterval:       time.Duration(viper.GetInt("topology.probeIntervalSeconds")) * time.Second,
		probeTimeout:        time.Duration(viper.GetInt("topology.probeTimeoutSeconds")) * time.Second,
		probeTransferTarget: viper.GetBool("topology.probeTransferTarget"),
		version:             version,
		hostname:            hostnameOrUnknown(),
		startedAt:           time.Now(),
	}

	// Unlike the two probe settings above, a zero here does NOT fall back to a default: it turns the
	// instance registry off, which is a supported choice (a deployment that does not want its
	// instances writing to the store on a timer) and the only shape available on SQLite anyway.
	t.heartbeatInterval = time.Duration(viper.GetInt("topology.heartbeatSeconds")) * time.Second

	// hostname:port, and resolved once - see instanceId. The port is the gRPC listener's, since that
	// is the address a peer is reached on and the one that makes the pair unique on a host.
	t.instanceId = net.JoinHostPort(t.hostname, strconv.Itoa(viper.GetInt("port")))

	if t.probeInterval <= 0 {
		t.probeInterval = defaultTopologyProbeInterval
	}

	if t.probeTimeout <= 0 {
		t.probeTimeout = defaultTopologyProbeTimeout
	}

	if err := viper.UnmarshalKey("topology.components", &t.components); err != nil {
		// Not fatal, and deliberately so: a malformed list is already rejected at startup by
		// validateConfig, so reaching here means something changed under a running process. Losing
		// the declared half of a diagnostic view is not a reason to take the store down with it.
		log.Errorf("failed to read topology.components: %s", err.Error())

		t.components = nil
	}

	return t
}

// buildTopologySpecs assembles the fixed half of every node and edge.
//
// It reads the live dependencies rather than only the configuration wherever the two can disagree -
// which search backend is actually in use is the clearest case, since "opensearch.enabled: false"
// on SQLite yields a working store-backed index and on Postgres yields none at all, and no config
// key says which happened. What it takes from viper is the settings an operator would otherwise
// have to read the config file to see.
//
// Disabled optional components are included, with status DISABLED and an attribute naming the key
// that turns them on. That is deliberate: "why is semantic search returning nothing" is answered by
// a greyed Embedder node reading "disabled (ollama.embedding.enabled)", and by nothing else the
// service says. A client is expected to let them be hidden, not to be handed a picture with the
// answer left out.
func (s *Server) buildTopologySpecs() ([]topologyNodeSpec, []topologyEdgeSpec) {
	nodes := []topologyNodeSpec{
		s.selfNodeSpec(),
		s.storeNodeSpec(),
		s.searchNodeSpec(),
		s.summariserNodeSpec(),
		s.embedderNodeSpec(),
		s.objectStoreNodeSpec(),
		s.transferNodeSpec(),
		identityProviderNodeSpec(),
		collectorNodeSpec(),
		callbackNodeSpec(),
	}

	for _, component := range s.topology.components {
		nodes = append(nodes, declaredNodeSpec(component))
	}

	// Every edge below runs outward from this instance, because every one of those connections is
	// one it opens - including the identity provider, which it fetches keys FROM rather than being
	// called by. Direction here is "who dials", which is what a firewall rule and an outage both
	// follow, and it is also what makes the declared components point the OTHER way (see below):
	// they are the half of the deployment that dials in.
	edges := []topologyEdgeSpec{
		{from: topologyNodeSelf, to: topologyNodeStore, label: "reads/writes"},
		{from: topologyNodeSelf, to: topologyNodeSearch, label: "indexes", optional: true},
		{from: topologyNodeSelf, to: topologyNodeSummariser, label: "summarises with", optional: true},
		{from: topologyNodeSelf, to: topologyNodeEmbedder, label: "embeds with", optional: true},
		{from: topologyNodeSelf, to: topologyNodeObjects, label: "archives to", optional: true},
		{from: topologyNodeSelf, to: topologyNodeTransfer, label: "transfers to", optional: true},
		{from: topologyNodeSelf, to: topologyNodeIdP, label: "fetches keys from", optional: true},
		{from: topologyNodeSelf, to: topologyNodeCollector, label: "exports to", optional: true},
		{from: topologyNodeSelf, to: topologyNodeCallbacks, label: "notifies", optional: true},
	}

	// Inbound, and the only edges in the graph that are. A declared component holds an address for
	// this service; this service holds only a health endpoint for it, which is not the connection
	// being drawn. Getting the arrow the right way round is most of what the declared half adds -
	// it is what makes a diagram show a deployment rather than a dependency list.
	for _, component := range s.topology.components {
		edges = append(edges, topologyEdgeSpec{
			from:  topologyDeclaredPrefix + component.Name,
			to:    topologyNodeSelf,
			label: "writes to",
		})
	}

	return nodes, edges
}

// declaredNodeSpec describes a component an operator has told this instance about.
//
// It is always probed, because a declared component with no health endpoint has told the view
// nothing it did not already know: the name in the config file. That is also why HealthURL is
// required rather than optional - a component that cannot be checked is a comment, and a comment
// belongs in the config file rather than on a diagram where it looks like a live component.
func declaredNodeSpec(component TopologyComponent) topologyNodeSpec {
	kind, ok := topologyComponentKinds[strings.ToLower(strings.TrimSpace(component.Kind))]
	if !ok {
		// validateConfig refuses an unknown kind at startup, so this is unreachable in a running
		// service; drawing it as a generic client beats drawing nothing if it ever is reached.
		kind = contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_CLIENT
	}

	return topologyNodeSpec{
		id:     topologyDeclaredPrefix + component.Name,
		kind:   kind,
		name:   component.Name,
		detail: redactEndpoint(component.HealthURL),
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_DECLARED,
		probe:  true,
		attributes: []topologyAttribute{
			{key: "kind", value: strings.ToLower(strings.TrimSpace(component.Kind))},
			{key: "health_url", value: redactEndpoint(healthProbeURL(component.HealthURL))},
			{key: "declared_in", value: "topology.components"},
		},
	}
}

// selfNodeSpec describes this instance. Its attributes are the settings an operator asks about
// first - what it is, what it decides, and what it will refuse - not the whole configuration: this
// is a view, and viper.AllSettings() is a credential leak.
func (s *Server) selfNodeSpec() topologyNodeSpec {
	hostname := s.topology.hostname

	role := "consolidator"
	if !s.consolidationEnabled {
		role = "replica"
	}

	attributes := []topologyAttribute{
		{key: "role", value: role},
		{key: "hostname", value: hostname},
		{key: "grpc_address", value: net.JoinHostPort(bindAddressOrAll("bindAddress"), strconv.Itoa(viper.GetInt("port")))},
		{key: "gateway", value: gatewayDescription()},
		{key: "tls", value: enabledDescription(viper.GetBool("tls.enabled"))},
		{key: "auth_method", value: authMethodDescription()},
		{key: "rate_limiting", value: enabledDescription(viper.GetBool("rateLimit.enabled"))},
	}

	// The consolidation settings are the ones that decide what this store forgets, so they belong
	// on the instance that runs the cycle. A replica runs none, and reporting numbers it does not
	// act on would invite an operator to tune the wrong instance.
	if s.consolidationEnabled {
		attributes = append(attributes,
			topologyAttribute{key: "consolidation_method", value: strconv.Itoa(s.consolidation.method)},
			topologyAttribute{key: "aggressiveness", value: strconv.FormatFloat(s.consolidation.aggressiveness, 'g', -1, 64)},
			topologyAttribute{key: "units_of_age_in_days", value: strconv.FormatFloat(s.consolidation.unitsOfAgeInDays, 'g', -1, 64)},
			topologyAttribute{key: "sleep_period", value: periodDescription(s.sleepPeriod)},
			topologyAttribute{key: "capacity_memories", value: countDescription(int64(s.consolidation.capacityMemories))},
			topologyAttribute{key: "capacity_bytes", value: countDescription(s.consolidation.capacityBytes)},
			topologyAttribute{key: "minimum_retention_days", value: countDescription(int64(s.consolidation.minimumRetentionInDays))},
			topologyAttribute{key: "forgotten_log", value: enabledDescription(s.consolidation.tombstones)},
		)
	}

	return topologyNodeSpec{
		id:           topologyNodeSelf,
		kind:         contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_INSTANCE,
		name:         hostname,
		detail:       s.topology.instanceId,
		source:       contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_SELF,
		version:      s.topology.version,
		attributes:   attributes,
		staticStatus: contract.TopologyStatus_TOPOLOGY_STATUS_OK,
	}
}

// storeNodeSpec describes the primary store. It is the one node that is never optional and never
// disabled: without it there is no instance to ask.
func (s *Server) storeNodeSpec() topologyNodeSpec {
	driver := viper.GetString("storage.driver")

	spec := topologyNodeSpec{
		id:     topologyNodeStore,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_STORE,
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
		probe:  true,
	}

	switch driver {

	case "postgres":
		spec.name = "PostgreSQL"
		spec.detail = redactEndpoint(viper.GetString("storage.postgres.dsn"))

	case "mysql":
		spec.name = "MySQL"
		spec.detail = redactEndpoint(viper.GetString("storage.mysql.dsn"))

	default:
		spec.name = "SQLite"
		spec.detail = viper.GetString("storage.directory")

		if spec.detail == "" {
			spec.detail = "in-memory"
		}

	}

	spec.attributes = []topologyAttribute{
		{key: "driver", value: driver},
		{key: "compression", value: enabledDescription(viper.GetBool("storage.compression.enabled"))},
	}

	// The single-consolidator lock is what makes a shared store safe. This says whether THIS
	// instance holds it, which is all one process can know on its own; the registry then adds an
	// instance count and names the consolidators beside it (see peers.go), where it is running.
	if driver == "postgres" || driver == "mysql" {
		spec.attributes = append(spec.attributes, topologyAttribute{
			key:   "consolidator_lock",
			value: heldDescription(s.consolidationEnabled),
		})
	}

	return spec
}

// searchNodeSpec describes the content-search index. It reports the backend actually in use, which
// no configuration key states on its own: opensearch.enabled false leaves a working store-backed
// FTS5 index on SQLite and nothing at all on the server drivers.
func (s *Server) searchNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeSearch,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_SEARCH_INDEX,
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	index := s.searchIdx()

	switch backend := index.(type) {

	case *search.OpenSearch:
		spec.name = "OpenSearch"
		spec.detail = redactEndpoints(viper.GetStringSlice("opensearch.addresses"))
		spec.probe = true
		spec.attributes = []topologyAttribute{
			{key: "backend", value: "opensearch"},
			{key: "index", value: viper.GetString("opensearch.index")},
			{key: "semantic_search", value: enabledDescription(backend.SupportsVectors())},
			{key: "reconcile_interval", value: periodDescription(s.reconcileInterval)},
		}

	case *search.SQL:
		// Served from the primary store, so it has no endpoint of its own and nothing to probe: if
		// it were unreachable the store node would already be saying so, and a second ping would
		// only be a second way to say the same thing.
		spec.name = "SQLite FTS5"
		spec.detail = "served from the primary store"
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_OK
		spec.attributes = []topologyAttribute{
			{key: "backend", value: "sql"},
			{key: "semantic_search", value: "unavailable (opensearch only)"},
		}

	default:
		spec.name = "content search"
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{
			{key: "backend", value: "none"},
			{key: "enable_with", value: "opensearch.enabled"},
		}

	}

	return spec
}

func (s *Server) summariserNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeSummariser,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_SUMMARISER,
		name:   "Ollama (summarisation)",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	if !s.summariser().Enabled() {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{{key: "enable_with", value: "ollama.enabled"}}

		return spec
	}

	spec.probe = true
	spec.detail = redactEndpoint(viper.GetString("ollama.address"))
	spec.attributes = []topologyAttribute{
		{key: "model", value: viper.GetString("ollama.model")},
		{key: "auto_summarise", value: enabledDescription(s.consolidation.autoSummarise)},
	}

	return spec
}

func (s *Server) embedderNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeEmbedder,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_EMBEDDER,
		name:   "Ollama (embeddings)",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	embedder := s.embedder()

	if !embedder.Enabled() {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{{key: "enable_with", value: "ollama.embedding.enabled"}}

		return spec
	}

	spec.probe = true
	spec.detail = redactEndpoint(viper.GetString("ollama.embedding.address"))
	spec.attributes = []topologyAttribute{
		{key: "model", value: embedder.Model()},
		{key: "dimensions", value: countDescription(int64(viper.GetInt("ollama.embedding.dimensions")))},
	}

	return spec
}

func (s *Server) objectStoreNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeObjects,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_OBJECT_STORE,
		name:   "S3 archive",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	if s.objects == nil {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{{key: "enable_with", value: "s3.bucket"}}

		return spec
	}

	endpoint := redactEndpoint(viper.GetString("s3.endpoint"))
	if endpoint == "" {
		endpoint = "aws"
	}

	spec.probe = true
	spec.detail = endpoint + "/" + viper.GetString("s3.bucket")
	spec.attributes = []topologyAttribute{
		{key: "bucket", value: viper.GetString("s3.bucket")},
		{key: "region", value: viper.GetString("s3.region")},
		{key: "key_prefix", value: s.transfer.keyPrefix},
	}

	return spec
}

func (s *Server) transferNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeTransfer,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_TRANSFER_TARGET,
		name:   "transfer target",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	if s.transfer.targetAddress == "" {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{{key: "enable_with", value: "transfer.targetAddress"}}

		return spec
	}

	spec.detail = redactEndpoint(s.transfer.targetAddress)
	spec.probe = s.topology.probeTransferTarget
	spec.attributes = []topologyAttribute{
		{key: "tls", value: enabledDescription(s.transfer.tls)},
		{key: "authenticated", value: enabledDescription(s.transfer.token != "")},
	}

	// Saying that nothing is checking is worth a line: an operator looking at a node with no status
	// should be told it is policy rather than a probe that has not run yet.
	if !spec.probe {
		spec.attributes = append(spec.attributes, topologyAttribute{
			key:   "probing",
			value: "off (topology.probeTransferTarget)",
		})
	}

	return spec
}

// identityProviderNodeSpec describes the IdP whose JWKS verifies tokens. It is never probed: the
// verifier already re-fetches the key set on its own schedule, and a console poll must not turn
// into load on somebody else's identity provider - the one dependency here that is shared with
// every other system in the organisation.
func identityProviderNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeIdP,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_IDENTITY_PROVIDER,
		name:   "identity provider",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	if viper.GetString("auth.method") != "idp" {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{{key: "enable_with", value: `auth.method: "idp"`}}

		return spec
	}

	issuer := viper.GetString("auth.issuer")
	jwks := viper.GetString("auth.jwksUrl")

	spec.detail = redactEndpoint(issuer)
	if spec.detail == "" {
		spec.detail = redactEndpoint(jwks)
	}

	spec.attributes = []topologyAttribute{
		{key: "issuer", value: redactEndpoint(issuer)},
		{key: "jwks_url", value: redactEndpoint(jwks)},
		{key: "probing", value: "off (the verifier refreshes on its own schedule)"},
	}

	return spec
}

// collectorNodeSpec describes the OTLP endpoint. Also never probed, for a different reason: OTLP
// export is fire-and-forget over a connection this process does not otherwise inspect, so a probe
// would mean opening a second one to learn something no exporter acts on.
func collectorNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeCollector,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_COLLECTOR,
		name:   "OTLP collector",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	traces := viper.GetBool("observability.tracing.enabled")
	metrics := viper.GetBool("observability.metrics.enabled")

	if !traces && !metrics {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{
			{key: "enable_with", value: "observability.metrics.enabled / observability.tracing.enabled"},
		}

		return spec
	}

	spec.detail = redactEndpoint(viper.GetString("observability.otlp.endpoint"))
	spec.attributes = []topologyAttribute{
		{key: "traces", value: enabledDescription(traces)},
		{key: "metrics", value: enabledDescription(metrics)},
		{key: "probing", value: "off (export is fire-and-forget)"},
	}

	return spec
}

// callbackNodeSpec describes the outbound callback receiver.
//
// Never probed, and for the IdP's reason rather than the collector's: this endpoint belongs to
// somebody else. A reader-visible console page must not turn into a timer-driven request against a
// third party's service, and a probe would tell an operator nothing the delivery metrics do not -
// hippocampus.callbacks.queue_depth and the delivered counter already say whether the receiver is
// accepting real traffic, which is a better answer than whether it answers a synthetic one.
func callbackNodeSpec() topologyNodeSpec {
	spec := topologyNodeSpec{
		id:     topologyNodeCallbacks,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_CALLBACK_RECEIVER,
		name:   "Callback receiver",
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_CONFIGURED,
	}

	if !viper.GetBool("callbacks.enabled") {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DISABLED
		spec.attributes = []topologyAttribute{
			{key: "enable_with", value: "callbacks.enabled"},
		}

		return spec
	}

	// Redacted at construction, like every other endpoint here: a callback URL can carry credentials
	// in its userinfo, and the Server must never hold one for this purpose.
	spec.detail = redactEndpoint(viper.GetString("callbacks.url"))
	spec.attributes = []topologyAttribute{
		{key: "scope", value: callbackScopeDescription()},
		{key: "bodies", value: enabledDescription(viper.GetBool("callbacks.includeBodies"))},
		{key: "authentication", value: callbackAuthDescription()},
		{key: "probing", value: "off (a third party's endpoint, and the delivery metrics answer better)"},
	}

	return spec
}

// callbackScopeDescription renders which deletions the receiver is told about.
func callbackScopeDescription() string {
	if viper.GetBool("callbacks.allDeletions") {
		return "every deletion"
	}

	return "consolidation and eviction only"
}

// callbackAuthDescription names how deliveries authenticate themselves, without naming the secrets.
func callbackAuthDescription() string {
	token := viper.GetString("callbacks.token") != ""
	signed := viper.GetString("callbacks.signingSecret") != ""

	switch {

	case token && signed:
		return "bearer token and signature"

	case token:
		return "bearer token"

	case signed:
		return "signature"

	}

	return "none"
}

// The small renderers below exist so that a value an operator reads is never a bare zero. "0" in a
// capacity row is ambiguous between "no limit" and "no memories allowed", and the difference is the
// whole behaviour of the store.

func enabledDescription(enabled bool) string {
	if enabled {
		return "enabled"
	}

	return "disabled"
}

func heldDescription(held bool) string {
	if held {
		return "held by this instance"
	}

	return "held by another instance"
}

func countDescription(value int64) string {
	if value <= 0 {
		return "unset"
	}

	return strconv.FormatInt(value, 10)
}

func periodDescription(period time.Duration) string {
	if period <= 0 {
		return "disabled"
	}

	return period.String()
}

func gatewayDescription() string {
	port := viper.GetInt("gateway.port")
	if port <= 0 {
		return "disabled"
	}

	return net.JoinHostPort(bindAddressOrAll("gateway.bindAddress"), strconv.Itoa(port))
}

// authMethodDescription reports the scheme in force, resolving the deprecated boolean the same way
// main.go does so the view never disagrees with what is actually enforced.
func authMethodDescription() string {
	method := viper.GetString("auth.method")

	if method == "" {
		if viper.GetBool("auth.enabled") {
			return "hmac"
		}

		return "none"
	}

	return method
}

// bindAddressOrAll renders an unset bind address as what it actually means. An empty string in a
// config file reads as "not configured"; in a listener it means every interface, which is the more
// consequential of the two readings and the one worth showing.
func bindAddressOrAll(key string) string {
	address := viper.GetString(key)
	if address == "" {
		return "0.0.0.0"
	}

	return address
}

// concatSpecs and concatEdges join the fixed half of the graph to the halves discovered while
// running, without appending to either. The fixed slices are built once at startup and read by
// every caller concurrently, so appending to one - which reuses its backing array whenever it has
// spare capacity - would have two responses writing over each other's peers.
func concatSpecs(parts ...[]topologyNodeSpec) []topologyNodeSpec {
	total := 0

	for _, part := range parts {
		total += len(part)
	}

	out := make([]topologyNodeSpec, 0, total)

	for _, part := range parts {
		out = append(out, part...)
	}

	return out
}

func concatEdges(parts ...[]topologyEdgeSpec) []topologyEdgeSpec {
	total := 0

	for _, part := range parts {
		total += len(part)
	}

	out := make([]topologyEdgeSpec, 0, total)

	for _, part := range parts {
		out = append(out, part...)
	}

	return out
}
