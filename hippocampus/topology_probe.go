package hippocampus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
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
	// dependency should be REPORTED unreachable quickly, and a slow probe holds one of the few slots
	// a round has, delaying the news about the others.
	defaultTopologyProbeTimeout = 2 * time.Second

	// topologyProbeConcurrency is how many probes run at once.
	//
	// A round was sequential while the only things probed were the handful this instance dials.
	// Declared components (topology.components) made the count operator-controlled - up to
	// MaxTopologyComponents plus the built-ins - and at that size a sequential round no longer fits
	// inside its own interval, which would leave the prober permanently behind and the interval it
	// reports a number nothing observes.
	//
	// Small rather than unbounded: the point of not firing every probe at once is that this process
	// opens a connection to every dependency it has, on a timer, forever. Four keeps the worst-case
	// round (40 probes at the 2s default) around 20 seconds, inside the 30s default interval, while
	// keeping the burst to something a network notices as nothing.
	topologyProbeConcurrency = 4

	// topologyVersionRefresh is how long a dependency's reported version is reused before the
	// prober asks for it again.
	//
	// A version is asked for separately because, for most dependencies, nothing a health check
	// returns carries it - OpenSearch's cluster health, Ollama's model list and a database ping all
	// say nothing about the build - so reading it every round would double the requests this
	// process makes to everything it depends on, to learn something that changes on an upgrade.
	// Long, then, but not forever: a rolling upgrade behind a load balancer never fails a probe, so
	// the "ask again after a failure" rule alone would leave the old version on the diagram for the
	// life of the process.
	topologyVersionRefresh = 10 * time.Minute
)

// topologyProbe is one dependency's health check. A nil error is healthy; an error wrapping one of
// the packages' ErrDegraded sentinels means "answered, but reporting a problem of its own", and any
// other error means unreachable.
//
// It also returns the dependency's version where it learned one. wantVersion says the prober would
// like it asked for (see wantTopologyVersion): a probe whose dependency needs a request of its own
// to answer that makes it only when asked, while one that learns the version from the health check
// itself - a declared component's /readyz body - returns it every round regardless. Failing to read
// a version is never a probe failure: it returns "" and a nil error, because a dependency serving
// correctly and declining to say which build it is is healthy.
type topologyProbe func(ctx context.Context, wantVersion bool) (string, error)

// topologyProbeResult is what the prober publishes for one node.
type topologyProbeResult struct {
	status    contract.TopologyStatus
	detail    string
	checkedAt time.Time

	// version is the most recent version the dependency reported, carried from round to round -
	// including across a failed probe, since "what build was the thing that just went unreachable" is
	// a question an incident asks, and the status beside it already says the reading is not live.
	version string

	// versionAskedAt is when the prober last asked for the version, whatever came back. Recorded
	// separately from version so that a dependency with nothing to report (a proxy answering 404
	// for the version endpoint) is asked once per refresh rather than once per round.
	versionAskedAt time.Time
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
// A few at a time (topologyProbeConcurrency), each bounded by its own timeout, so a round stays
// inside its interval however many components have been declared without this process opening every
// outbound connection it has at the same instant.
//
// The map is replaced wholesale rather than updated in place, so a reader always sees one round's
// worth of results and never a half-written map.
func (s *Server) probeTopologyOnce(probers map[string]topologyProbe) {
	log.Trace("func() hippocampus.probeTopologyOnce")

	type outcome struct {
		id     string
		result topologyProbeResult
	}

	outcomes := make(chan outcome, len(probers))
	slots := make(chan struct{}, topologyProbeConcurrency)

	// The previous round is what the version cache lives in. Only this goroutine ever publishes a
	// round, so reading the last one and building the next from it needs no lock.
	previous := s.topologyProbeResults()

	var wg sync.WaitGroup

	for id, probe := range probers {
		wg.Add(1)

		go func(id string, probe topologyProbe) {
			defer wg.Done()

			slots <- struct{}{}
			defer func() { <-slots }()

			last, seen := previous[id]
			wantVersion := wantTopologyVersion(last, seen, time.Now())

			ctx, cancel := context.WithTimeout(context.Background(), s.topology.probeTimeout)
			version, err := probe(ctx, wantVersion)

			cancel()

			status, detail := topologyStatusFor(err)

			if err != nil {
				log.Debugf("topology probe %q: %s", id, err.Error())
			}

			result := topologyProbeResult{
				status:         status,
				detail:         detail,
				checkedAt:      time.Now(),
				version:        last.version,
				versionAskedAt: last.versionAskedAt,
			}

			if wantVersion {
				result.versionAskedAt = result.checkedAt
			}

			if version != "" {
				result.version = version
			}

			outcomes <- outcome{id: id, result: result}
		}(id, probe)
	}

	wg.Wait()
	close(outcomes)

	results := make(map[string]topologyProbeResult, len(probers))

	for out := range outcomes {
		results[out.id] = out.result
	}

	s.topologyProbes.Store(&results)
}

// wantTopologyVersion decides whether this round should ask a dependency for its version: when it
// has never been asked, when the last round did not find it healthy (a restart is usually an
// upgrade, and passes through unreachable on the way), and otherwise once per
// topologyVersionRefresh.
func wantTopologyVersion(last topologyProbeResult, seen bool, now time.Time) bool {
	switch {

	case !seen:
		return true

	case last.status != contract.TopologyStatus_TOPOLOGY_STATUS_OK:
		return true

	default:
		return now.Sub(last.versionAskedAt) >= topologyVersionRefresh

	}
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

	if errors.Is(err, errDeclaredDegraded) ||
		errors.Is(err, search.ErrDegraded) || errors.Is(err, summarise.ErrDegraded) || errors.Is(err, embed.ErrDegraded) {
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

// topologyVersioner is the optional interface a dependency implements to report its version, for
// the same reasons topologyPinger is optional: OpenSearch and Ollama have a version endpoint to
// ask, while an OpenAI-compatible provider, an S3 bucket and a directory have nothing to say.
type topologyVersioner interface {
	Version(ctx context.Context) (string, error)
}

// askTopologyVersion reads a dependency's version, swallowing a failure: see topologyProbe for why a
// version that cannot be read does not make the dependency unhealthy.
func askTopologyVersion(ctx context.Context, id string, ask func(context.Context) (string, error)) string {
	version, err := ask(ctx)
	if err != nil {
		log.Debugf("topology probe %q: version not read: %s", id, err.Error())

		return ""
	}

	return version
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
		return s.probeStore

	case topologyNodeSearch:
		return pingerProbe(id, s.searchIdx())

	case topologyNodeSummariser:
		return pingerProbe(id, s.summariser())

	case topologyNodeEmbedder:
		return pingerProbe(id, s.embedder())

	case topologyNodeObjects:
		return pingerProbe(id, s.objects)

	case topologyNodeTransfer:
		return s.probeTransferTarget

	default:
		if name, ok := strings.CutPrefix(id, topologyDeclaredPrefix); ok {
			return s.declaredProbe(name)
		}

		return nil

	}
}

// declaredProbe returns the probe for a declared component, matched by name. It looks the component
// up rather than closing over the loop variable in topologyProbers, so the probe map and the node
// specs cannot disagree about which URL belongs to which name.
func (s *Server) declaredProbe(name string) topologyProbe {
	for _, component := range s.topology.components {
		if component.Name != name {
			continue
		}

		url := healthProbeURL(component.HealthURL)

		// The version comes back in the /readyz body every round, so wantVersion is ignored: asking
		// for it costs nothing here, and a component that reports one is always current.
		return func(ctx context.Context, _ bool) (string, error) {
			return probeHealthEndpoint(ctx, url)
		}
	}

	return nil
}

// pingerProbe adapts a dependency to a probe when it can be pinged, and returns nil when it cannot.
// Its version is read only after a successful ping, and only where the dependency can report one.
func pingerProbe(id string, dependency any) topologyProbe {
	pinger, ok := dependency.(topologyPinger)
	if !ok {
		return nil
	}

	versioner, _ := dependency.(topologyVersioner)

	return func(ctx context.Context, wantVersion bool) (string, error) {
		if err := pinger.Ping(ctx); err != nil {
			return "", err
		}

		if !wantVersion || versioner == nil {
			return "", nil
		}

		return askTopologyVersion(ctx, id, versioner.Version), nil
	}
}

// storeVersioner is what the concrete store implements to report its engine's version. Asserted for
// rather than declared on db.Store, so the interface every test fake satisfies stays as it is.
type storeVersioner interface {
	ServerVersion(ctx context.Context) (string, error)
}

// probeStore pings the primary store and, when asked, reads the engine's version. It is not a
// pingerProbe because the store's version method is named for what it reports - ServerVersion - so
// that it cannot be mistaken for the store's SCHEMA version, which is a different number entirely.
func (s *Server) probeStore(ctx context.Context, wantVersion bool) (string, error) {
	if err := s.db.Ping(ctx); err != nil {
		return "", err
	}

	versioner, ok := s.db.(storeVersioner)
	if !wantVersion || !ok {
		return "", nil
	}

	return askTopologyVersion(ctx, topologyNodeStore, versioner.ServerVersion), nil
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
//
// The version comes from the target's WhoAmI, which is the one gRPC-reachable place an instance
// reports its build. Unlike the health check it is authenticated, so it carries the same token a
// transfer would - and a token the target refuses is a version not read rather than a target
// reported unhealthy, since a transfer target answering its health check is ready whether or not
// this instance's token may ask it anything else.
func (s *Server) probeTransferTarget(ctx context.Context, wantVersion bool) (string, error) {
	creds, err := s.transfer.clientCredentials()
	if err != nil {
		return "", err
	}

	conn, err := grpc.NewClient(s.transfer.targetAddress, grpc.WithTransportCredentials(creds))
	if err != nil {
		return "", err
	}

	defer func() { _ = conn.Close() }()

	if err := observability.GRPCHealthCheck(conn)(ctx); err != nil {
		return "", err
	}

	if !wantVersion {
		return "", nil
	}

	ask := func(ctx context.Context) (string, error) {
		res, err := contract.NewHippocampusClient(conn).WhoAmI(s.transferOutgoingContext(ctx), &contract.EmptyRequest{})
		if err != nil {
			return "", err
		}

		return res.GetVersion(), nil
	}

	return askTopologyVersion(ctx, topologyNodeTransfer, ask), nil
}

// stopTopologyProber shuts the prober down and waits for it, like the sleep and reconcile loops.
func (s *Server) stopTopologyProber() {
	if s.stopTopology == nil {
		return
	}

	close(s.stopTopology)
	<-s.topologyStopped
}

// declaredProbeClient is the HTTP client every declared component is probed with. One shared client
// so connections are pooled across rounds; no redirect following, because a health endpoint that
// redirects is not the health endpoint that was declared, and quietly following it somewhere else
// would report the wrong thing being healthy.
var declaredProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// healthProbeURL resolves what to actually request for a declared component.
//
// A URL with no path gets "/readyz" appended - which is what the bridges and the ingestor serve on
// their --health-port, so the common case is that an operator writes the address and nothing else. A
// URL carrying a path is used verbatim, for a component behind a proxy or serving its health
// somewhere of its own choosing.
func healthProbeURL(raw string) string {
	address := strings.TrimSpace(raw)
	if address == "" {
		return ""
	}

	parsed, err := url.Parse(address)
	if err != nil {
		return address
	}

	if strings.Trim(parsed.Path, "/") == "" {
		parsed.Path = "/readyz"
	}

	return parsed.String()
}

// healthResponse is the body the shared health server serves at /readyz
// (observability/health.go): a state, the component's own name for itself and its version, and a
// per-dependency breakdown naming WHICH end is unreachable. Parsing it is what makes a declared bridge report
// "cannot reach the broker" rather than an opaque red box - and it costs nothing on either side,
// since every bridge and the ingestor already serve exactly this.
//
// Every field is optional here. A component that is not one of ours, or that sits behind a proxy
// answering with something else entirely, still gets a status from the HTTP code alone.
type healthResponse struct {
	Status       string            `json:"status"`
	Component    string            `json:"component"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
}

// failing returns the names of the dependencies this component says it cannot reach, sorted so the
// message does not reorder itself between rounds.
func (h healthResponse) failing() []string {
	out := make([]string, 0, len(h.Dependencies))

	for name, state := range h.Dependencies {
		if state != "ok" {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// probeHealthEndpoint checks one declared component.
//
// Three outcomes, and the middle one is why this parses a body at all:
//
//   - 2xx: healthy.
//   - 503: DEGRADED, not unreachable. The component answered - it is running and it is telling us
//     it cannot serve - and the body names which of ITS dependencies is the reason, which is the
//     single most useful sentence this whole view can produce. A bridge that cannot reach its broker
//     and a bridge that cannot reach us look identical from here otherwise.
//   - anything else: unreachable, carrying the status. A 404 in particular is a declared URL that is
//     wrong rather than a component that is down, and reporting the code is what makes that
//     distinguishable.
//
// The body is read with a bound: a health endpoint returns a few hundred bytes, and a probe must not
// be a way for a misconfigured URL pointing at something large to consume this process's memory on a
// timer.
//
// The version the body carries is returned on a 2xx and a 503 alike - a component reporting itself
// not ready has still said which build it is - and passed through the same sanitising a
// client-reported version gets, since it is equally the far end's own claim.
func probeHealthEndpoint(ctx context.Context, address string) (string, error) {
	if address == "" {
		return "", fmt.Errorf("no health URL is configured for this component")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build the health request: %w", err)
	}

	res, err := declaredProbeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("unreachable: %w", err)
	}

	defer func() { _ = res.Body.Close() }()

	var body healthResponse

	_ = json.NewDecoder(io.LimitReader(res.Body, maxHealthBodyBytes)).Decode(&body)

	version := sanitiseReportedVersion(body.Version)

	switch {

	case res.StatusCode >= 200 && res.StatusCode <= 299:
		return version, nil

	case res.StatusCode == http.StatusServiceUnavailable:
		if failing := body.failing(); len(failing) > 0 {
			return version, fmt.Errorf("%w: cannot reach %s", errDeclaredDegraded, strings.Join(failing, ", "))
		}

		return version, fmt.Errorf("%w: reports itself not ready", errDeclaredDegraded)

	default:
		return "", fmt.Errorf("health endpoint returned %s", res.Status)

	}
}

// errDeclaredDegraded is the declared components' counterpart to the search/summarise/embed
// ErrDegraded sentinels: the component answered and is reporting a problem of its own. It lives here
// rather than in one of those packages because nothing outside this file produces it.
var errDeclaredDegraded = errors.New("degraded")

// maxHealthBodyBytes bounds what a probe will read from a health endpoint. A real one answers in a
// few hundred bytes; this is generous enough for a component with many dependencies and small enough
// that a URL pointing at the wrong thing cannot turn a probe into a download.
const maxHealthBodyBytes = 64 << 10

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
