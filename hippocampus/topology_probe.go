package hippocampus

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/observability"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/summarise"
)

const (
	// defaultTopologyProbeInterval paces the prober. Dependency health moves on the scale of a
	// process restart or a network partition, not of a page refresh, and every probe is an outbound
	// request that costs somebody else something.
	defaultTopologyProbeInterval = 30 * time.Second

	// defaultTopologyProbeTimeout bounds one probe. It is short on purpose: an unreachable
	// dependency should be REPORTED unreachable quickly, and a round runs its probes in sequence, so
	// a generous timeout on one delays the news about all the others.
	defaultTopologyProbeTimeout = 2 * time.Second
)

// topologyProbe is one dependency's health check. A nil error is healthy; an error wrapping one of
// the packages' ErrDegraded sentinels means "answered, but reporting a problem of its own", and any
// other error means unreachable.
type topologyProbe func(ctx context.Context) error

// topologyProbeResult is what the prober publishes for one node.
type topologyProbeResult struct {
	status    contract.TopologyStatus
	detail    string
	checkedAt time.Time
}

// startTopologyProber launches the background prober, if there is anything to probe.
//
// The prober exists so that GetTopology never performs I/O. Probing inside the handler would make
// one console page open N outbound connections, would let a single hung dependency hang the RPC,
// and would multiply both by the number of people looking - which is precisely the shape of an
// operational tool that makes an incident worse. Instead the handler reads a snapshot and reports
// how old it is.
//
// It runs on every instance, replica included: a replica's own view of its own dependencies is what
// an operator connected to that replica needs, and it is the only instance that can produce it.
func (s *Server) startTopologyProber() {
	if !s.topology.enabled {
		return
	}

	probers := s.topologyProbers()
	if len(probers) == 0 {
		return
	}

	s.stopTopology = make(chan struct{})
	s.topologyStopped = make(chan struct{})

	go s.topologyProberLoop(probers)
}

// topologyProberLoop probes once immediately and then on the interval. The immediate round matters:
// without it the first console load after a restart shows every dependency as never-checked, which
// is indistinguishable from a prober that failed to start.
func (s *Server) topologyProberLoop(probers map[string]topologyProbe) {
	defer close(s.topologyStopped)

	s.probeTopologyOnce(probers)

	ticker := time.NewTicker(s.topology.probeInterval)
	defer ticker.Stop()

	for {
		select {

		case <-s.stopTopology:
			return

		case <-ticker.C:
			s.probeTopologyOnce(probers)

		}
	}
}

// probeTopologyOnce runs every probe and publishes a fresh result map.
//
// Sequential rather than concurrent, and each probe bounded by its own timeout. A round therefore
// costs at most len(probers) x probeTimeout, which the default settings keep well inside one
// interval; running them together would save a few seconds at the price of opening every outbound
// connection this service has at the same instant, on a timer, forever.
//
// The map is replaced wholesale rather than updated in place, so a reader always sees one round's
// worth of results and never a half-written map.
func (s *Server) probeTopologyOnce(probers map[string]topologyProbe) {
	log.Trace("func() hippocampus.probeTopologyOnce")

	results := make(map[string]topologyProbeResult, len(probers))

	for id, probe := range probers {
		ctx, cancel := context.WithTimeout(context.Background(), s.topology.probeTimeout)
		err := probe(ctx)

		cancel()

		status, detail := topologyStatusFor(err)

		if err != nil {
			log.Debugf("topology probe %q: %s", id, err.Error())
		}

		results[id] = topologyProbeResult{status: status, detail: detail, checkedAt: time.Now()}
	}

	s.topologyProbes.Store(&results)
}

// topologyProbeResults returns the latest published round, or an empty map before the first has
// completed. Never nil, so callers need no guard.
func (s *Server) topologyProbeResults() map[string]topologyProbeResult {
	if results := s.topologyProbes.Load(); results != nil {
		return *results
	}

	return map[string]topologyProbeResult{}
}

// topologyStatusFor maps a probe's error onto a reported status.
//
// The three ErrDegraded sentinels are checked here rather than shared from one package because each
// optional integration is deliberately self-contained - search, summarise and embed import nothing
// of each other's - and a package existing solely to hold one sentinel would be a worse trade than
// three lines in the one place that has to distinguish them anyway.
func topologyStatusFor(err error) (contract.TopologyStatus, string) {
	if err == nil {
		return contract.TopologyStatus_TOPOLOGY_STATUS_OK, ""
	}

	if errors.Is(err, search.ErrDegraded) || errors.Is(err, summarise.ErrDegraded) || errors.Is(err, embed.ErrDegraded) {
		return contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED, err.Error()
	}

	return contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE, err.Error()
}

// topologyPinger is the optional interface a dependency implements to be probed. None of the
// dependency interfaces (search.Index, summarise.Summariser, embed.Embedder, archive.ObjectStore)
// declares it: only some implementations have anything to reach, and widening those interfaces
// would put a meaningless method on every no-op and every test fake. This mirrors how
// RecreateIndex and IndexMemorySync are reached - concrete methods, asserted for.
type topologyPinger interface {
	Ping(ctx context.Context) error
}

// topologyProbers builds the probe for each node that has one, keyed by node id. A node whose
// dependency is disabled, or whose implementation cannot be pinged, simply gets no entry - and its
// spec then reports its static status instead.
func (s *Server) topologyProbers() map[string]topologyProbe {
	probers := make(map[string]topologyProbe, len(s.topology.nodes))

	for _, spec := range s.topology.nodes {
		if !spec.probe {
			continue
		}

		if probe := s.probeFor(spec.id); probe != nil {
			probers[spec.id] = probe
		}
	}

	return probers
}

// probeFor returns the probe for one node id, or nil when there is none to run.
func (s *Server) probeFor(id string) topologyProbe {
	switch id {

	case topologyNodeStore:
		return s.db.Ping

	case topologyNodeSearch:
		return pingerProbe(s.searchIdx())

	case topologyNodeSummariser:
		return pingerProbe(s.summariser())

	case topologyNodeEmbedder:
		return pingerProbe(s.embedder())

	case topologyNodeObjects:
		return pingerProbe(s.objects)

	case topologyNodeTransfer:
		return s.probeTransferTarget

	default:
		return nil

	}
}

// pingerProbe adapts a dependency to a probe when it can be pinged, and returns nil when it cannot.
func pingerProbe(dependency any) topologyProbe {
	pinger, ok := dependency.(topologyPinger)
	if !ok {
		return nil
	}

	return pinger.Ping
}

// probeTransferTarget checks that the Transfer target is up and serving, using the same credentials
// and TLS trust options a real transfer would - so what this reports is whether a transfer could
// connect, not merely whether something is listening on the port.
//
// It dials per probe rather than holding a connection, which is why it is opt-in: a permanently
// open client connection to another organisation's instance is a real thing to maintain, where a
// short-lived one every interval is only a cost. It asks the gRPC health service, which is exempt
// from the target's auth interceptor and driven by the target's own database readiness - so "ready"
// there means it could serve an ImportBatch, not that a socket opened.
func (s *Server) probeTransferTarget(ctx context.Context) error {
	creds, err := s.transfer.clientCredentials()
	if err != nil {
		return err
	}

	conn, err := grpc.NewClient(s.transfer.targetAddress, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}

	defer func() { _ = conn.Close() }()

	return observability.GRPCHealthCheck(conn)(ctx)
}

// stopTopologyProber shuts the prober down and waits for it, like the sleep and reconcile loops.
func (s *Server) stopTopologyProber() {
	if s.stopTopology == nil {
		return
	}

	close(s.stopTopology)
	<-s.topologyStopped
}

// redactEndpoints redacts a list of addresses and joins them for display.
func redactEndpoints(raw []string) string {
	out := make([]string, 0, len(raw))

	for _, address := range raw {
		if redacted := redactEndpoint(address); redacted != "" {
			out = append(out, redacted)
		}
	}

	return strings.Join(out, ", ")
}

// redactEndpoint strips every credential from an address so it can be shown to a caller.
//
// This function is what makes the whole view safe to expose at reader tier, so it fails CLOSED: an
// address it cannot confidently parse loses everything up to the last "@" and its entire query
// string anyway, because the two places a secret hides in a connection string are the userinfo and
// the parameters (a Postgres keyword DSN carries "password=" as a parameter; a MySQL DSN can carry
// TLS material the same way).
//
// It handles the four shapes that actually reach it:
//
//   - a URL, "postgres://user:pass@host:5432/db?sslmode=require" -> "postgres://host:5432/db"
//   - a Postgres keyword/value DSN, "host=x port=5432 dbname=y password=z" -> "x:5432/y"
//   - a MySQL DSN, "user:pass@tcp(host:3306)/db?parseTime=true"  -> "tcp(host:3306)/db"
//   - a bare address or path, "localhost:11434" or "/var/lib/hippocampus" -> unchanged
//
// The URL branch rebuilds from the parsed parts rather than editing the string, so anything it did
// not explicitly decide to keep is gone by construction.
func redactEndpoint(raw string) string {
	address := strings.TrimSpace(raw)
	if address == "" {
		return ""
	}

	if strings.Contains(address, "://") {
		return redactURL(address)
	}

	if isKeywordDSN(address) {
		return redactKeywordDSN(address)
	}

	return redactBareAddress(address)
}

// redactURL keeps the scheme, host and path and discards everything else - userinfo, query and
// fragment - by building a new URL from only those three.
func redactURL(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		// Unparseable, so nothing below can be trusted to be in the position it looks like it is
		// in. Fall back to the blunt form rather than returning the original.
		return redactBareAddress(address)
	}

	clean := url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}

	return strings.TrimSuffix(clean.String(), "/")
}

// isKeywordDSN recognises the libpq keyword/value form, which is neither a URL nor a MySQL DSN and
// carries its password as a plain parameter.
func isKeywordDSN(address string) bool {
	return strings.Contains(address, "=") && !strings.Contains(address, "@")
}

// redactKeywordDSN rebuilds a libpq keyword/value DSN from only the three keys worth showing.
// Building UP from an allow-list rather than removing "password=" is deliberate: the list of keys
// that can carry a secret is not fixed (sslpassword, and whatever the next driver adds), and a
// removal list has to be right about all of them forever.
func redactKeywordDSN(address string) string {
	var host, port, database string

	for _, field := range strings.Fields(address) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}

		switch strings.ToLower(key) {

		case "host":
			host = value

		case "port":
			port = value

		case "dbname":
			database = value

		}
	}

	if host == "" {
		return "(host unset)"
	}

	out := host

	if port != "" {
		out += ":" + port
	}

	if database != "" {
		out += "/" + database
	}

	return out
}

// redactBareAddress drops any userinfo and any parameters from an address that is not a URL. This
// is the MySQL DSN's shape and also the safe fallback for anything unrecognised.
func redactBareAddress(address string) string {
	prefix, rest := "", address

	// A scheme separator has to come off first, or the "/" in "://" is mistaken below for the one
	// that starts the path - which leaves the userinfo on the wrong side of the split and the
	// credentials in the output. That is only reachable when url.Parse has already failed, so it is
	// exactly the case nobody would notice by inspection.
	if scheme := strings.Index(rest, "://"); scheme >= 0 {
		prefix, rest = rest[:scheme+3], rest[scheme+3:]
	}

	head, tail := rest, ""

	// The database name follows the first "/", and credentials can only precede it - so splitting
	// there keeps a "@" inside a path or a parameter from being mistaken for userinfo.
	if slash := strings.Index(rest, "/"); slash >= 0 {
		head, tail = rest[:slash], rest[slash:]
	}

	if at := strings.LastIndex(head, "@"); at >= 0 {
		head = head[at+1:]
	}

	if question := strings.Index(tail, "?"); question >= 0 {
		tail = tail[:question]
	}

	return prefix + head + tail
}
