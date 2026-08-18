package hippocampus

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// Observed callers: the last part of the deployment view, and the only one that names a component
// nobody configured, declared, or shared a store with.
//
// Everything else in the topology is reached by this instance (CONFIGURED), is this instance
// (SELF), writes to the same registry (DISCOVERED), or was listed by an operator (DECLARED). The
// components that dial IN are invisible to all four unless somebody remembers to declare them - and
// the one thing the service unavoidably learns about such a component is that it called, and who it
// said it was. That is what this file records.
//
// Five properties hold it together, and each is a deliberate limit rather than an omission:
//
//  1. It carries NO health. A caller node's status is UNSPECIFIED, permanently. A call proves the
//     client was alive at that moment and nothing more: an idle client is not unhealthy, and a
//     client that polls while its real work is broken is not healthy. When the answer is not known,
//     saying so beats a green tick - the same reasoning that leaves the collector and the IdP
//     unprobed.
//
//  2. It reports nothing when authentication is off. A caller is identified by the verified
//     client_id claim and by nothing else - never by a source address, which names a proxy or a pod
//     that has since been replaced, and never by a user agent, which is whatever the caller typed.
//     An unauthenticated instance therefore shows no callers, and the self node says so rather than
//     leaving an empty column to be read as "nothing is calling".
//
//  3. It is BOUNDED, and the bound is a security property rather than tidiness: the map is keyed on
//     a value that arrives in a token, so an unbounded one would be memory a caller controls. At
//     the cap the least recently seen entry is evicted, and the self node says the view is capped.
//
//  4. Entries are never expired on a timer, only evicted under pressure. A bridge that stopped
//     calling six hours ago is worth MORE on the diagram than an absence is, because "last call: 6h
//     ago" is the report of the fault; a node that quietly vanished would be indistinguishable from
//     one that had never been configured. The registry is in memory only, so a restart is what
//     clears it, which is also the only event that makes stale entries meaningless.
//
//  5. A client_id is never a metric attribute. The declared components are capped and named by an
//     operator, so they may be; this set is capped but named by whoever holds a token, which is
//     exactly the shape the low-cardinality rule refuses.

// maxObservedCallers bounds the registry. It matches MaxTopologyComponents deliberately: both are
// the left-hand column of the same diagram, and a picture whose inbound half runs to hundreds of
// boxes has stopped being a picture. It is also the memory bound of item 3 above.
const maxObservedCallers = 32

// observedTransport is a bitmask rather than a scalar because one client routinely uses both - the
// CLI over gRPC and a browser over the gateway can share a token, and reporting only the most
// recent would make the row flap between two values that are both true.
type observedTransport uint32

const (
	observedTransportGRPC observedTransport = 1 << iota
	observedTransportHTTP
)

// topologyObservedPrefix namespaces an observed caller's node id, for the same reason the declared
// and peer prefixes exist: a client_id is chosen by whoever mints the token, and without a
// namespace one calling itself "store" would replace the primary store's node on the diagram.
const topologyObservedPrefix = "observed:"

// observedCaller is one client's record. Every field that changes is an atomic, because this is
// written from every authenticated request's own goroutine and the alternative - taking the
// registry's write lock per request - would put a single mutex on the hot path of the whole service
// to maintain a diagnostic view.
type observedCaller struct {
	// id and firstSeen are fixed at insertion, so they need no synchronisation of their own: they
	// are published by the write lock that inserted the entry and never written again.
	id        string
	firstSeen time.Time

	lastSeen   atomic.Int64
	calls      atomic.Int64
	transports atomic.Uint32

	// identity is the roles and scope the caller's token carried, replaced only when they actually
	// change. A token is re-minted with different roles often enough that pinning the first one seen
	// would be wrong, and comparing before storing keeps the steady state allocation-free.
	identity atomic.Pointer[observedIdentity]
}

// observedIdentity is immutable once published, which is what lets it be swapped under an atomic
// pointer rather than guarded.
type observedIdentity struct {
	roles []string

	// scoped says the caller's token is bound to a group scope, WITHOUT naming the groups. The
	// distinction matters: the topology is visible at reader tier by default, and a group name is
	// frequently a customer's name, so listing another caller's partitions here would leak exactly
	// the thing group scoping exists to separate. Whether a client is bound at all is the part an
	// operator is actually diagnosing.
	scoped bool
}

// observedCallers is the registry itself.
//
// The lock is an RWMutex used asymmetrically on purpose: the steady state is a read lock plus two
// atomic increments on an existing entry, and the write lock is taken only when a client_id is seen
// for the first time - which, in a deployment with a fixed set of components, is a handful of times
// per process.
type observedCallers struct {
	mu      sync.RWMutex
	callers map[string]*observedCaller

	// evicted records that the cap has been reached and something was dropped, so the view can say
	// it is showing a subset rather than presenting a truncated list as complete.
	evicted bool
}

// record notes one call from a verified client.
func (o *observedCallers) record(id string, roles []string, scoped bool, transport observedTransport, now time.Time) {
	o.mu.RLock()
	caller, ok := o.callers[id]
	o.mu.RUnlock()

	if !ok {
		caller = o.insert(id, now)
	}

	caller.lastSeen.Store(now.UnixNano())
	caller.calls.Add(1)
	caller.transports.Or(uint32(transport))

	// Compared before storing so the common case - a client whose token says the same thing it said
	// last call - allocates nothing. The roles slice belongs to the request's claims, so the copy
	// kept here must be its own.
	if current := caller.identity.Load(); current == nil || current.scoped != scoped || !slices.Equal(current.roles, roles) {
		caller.identity.Store(&observedIdentity{roles: slices.Clone(roles), scoped: scoped})
	}
}

// insert adds an entry, evicting the least recently seen when the registry is full.
//
// It re-checks under the write lock because two requests from the same new client can both find it
// missing under the read lock; returning the entry the winner inserted, rather than a second one,
// is what keeps the counts of one client from splitting across two boxes on the diagram.
func (o *observedCallers) insert(id string, now time.Time) *observedCaller {
	o.mu.Lock()
	defer o.mu.Unlock()

	if caller, ok := o.callers[id]; ok {
		return caller
	}

	if o.callers == nil {
		o.callers = make(map[string]*observedCaller, maxObservedCallers)
	}

	if len(o.callers) >= maxObservedCallers {
		o.evictOldest()
	}

	caller := &observedCaller{
		id:        id,
		firstSeen: now,
	}

	o.callers[id] = caller

	return caller
}

// evictOldest drops the least recently seen entry. A linear scan over at most maxObservedCallers
// entries, run only when a new client_id arrives at a full registry - a heap would be a data
// structure to maintain on every request in order to speed up something that happens rarely.
//
// Called with the write lock held.
func (o *observedCallers) evictOldest() {
	var (
		oldestId string
		oldest   int64
	)

	for id, caller := range o.callers {
		seen := caller.lastSeen.Load()

		if oldestId == "" || seen < oldest {
			oldestId, oldest = id, seen
		}
	}

	if oldestId == "" {
		return
	}

	delete(o.callers, oldestId)

	o.evicted = true
}

// observedRecord is one entry, read out of the atomics into plain values so the caller assembling a
// response is not reading fields that move while it works.
type observedRecord struct {
	id         string
	firstSeen  time.Time
	lastSeen   time.Time
	calls      int64
	transports observedTransport
	roles      []string
	scoped     bool
}

// snapshot copies the registry out, sorted by client id.
//
// Sorted rather than most-recent-first, because this list is drawn as boxes in a column and an
// order that changes with traffic would reshuffle the diagram between polls - the same reason
// topologyLayout fixes its column order rather than laying the graph out afresh each time.
func (o *observedCallers) snapshot() ([]observedRecord, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	out := make([]observedRecord, 0, len(o.callers))

	for _, caller := range o.callers {
		record := observedRecord{
			id:         caller.id,
			firstSeen:  caller.firstSeen,
			lastSeen:   time.Unix(0, caller.lastSeen.Load()),
			calls:      caller.calls.Load(),
			transports: observedTransport(caller.transports.Load()),
		}

		if identity := caller.identity.Load(); identity != nil {
			record.roles = identity.roles
			record.scoped = identity.scoped
		}

		out = append(out, record)
	}

	slices.SortFunc(out, func(a observedRecord, b observedRecord) int {
		return strings.Compare(a.id, b.id)
	})

	return out, o.evicted
}

// observeCaller records the caller behind ctx, if there is one to record.
//
// Everything this feature does not do is in the guards: no topology means no view to feed, and no
// client_id means either that authentication is off or that the token identifies its bearer by
// something this service does not treat as a client - in both cases there is nothing honest to draw.
func (s *Server) observeCaller(ctx context.Context, transport observedTransport) {
	if !s.topology.enabled {
		return
	}

	claims := auth.ClaimsFromContext(ctx)
	if claims == nil || claims.ClientID == "" {
		return
	}

	s.observed.record(claims.ClientID, claims.Roles, len(claims.Groups) > 0, transport, time.Now())
}

// InterceptorObserveCaller records the verified caller of each RPC.
//
// It belongs AFTER authentication and BEFORE authorisation in the chain, and the order is the
// decision: a client whose token is valid but whose role is refused every call is precisely the one
// an operator is looking for on this diagram, and behind the authoriser it would never appear.
// Roles are recorded rather than the resolved tier for the same reason - the tier is stashed by the
// interceptor that has not run yet.
//
// Scoped to the Hippocampus RPC surface like the purge gate, so health checking cannot register a
// caller.
func (s *Server) InterceptorObserveCaller(ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	if strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
		s.observeCaller(ctx, observedTransportGRPC)
	}

	return handler(ctx, req)
}

// HTTPMiddlewareObserveCaller is the gateway counterpart, needed for the usual reason: the gateway
// calls this server's methods directly and never runs the gRPC interceptor chain, so without it
// every /v1 client would be invisible while its gRPC equivalent was drawn.
//
// It needs no path allow-list, because it records only what carries verified claims and the open
// paths - the probes, the console's assets, the login endpoints - never run the verifier. Nor does
// it exclude the console: a browser signed in against this service IS a client of it, and drawing
// it under the OAuth client id it authenticated with is the honest rendering. That the console's
// own topology poll therefore appears in the topology is a curiosity rather than a problem; hiding
// it would make a client that only reads this view invisible in it.
func (s *Server) HTTPMiddlewareObserveCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.observeCaller(r.Context(), observedTransportHTTP)

		next.ServeHTTP(w, r)
	})
}

// observedSnapshot is the registry rendered into the pieces topologyResponse merges: the caller
// nodes, their inbound edges, and the attributes that belong on nodes built elsewhere.
type observedSnapshot struct {
	nodes []topologyNodeSpec
	edges []topologyEdgeSpec

	// attributes are merged onto nodes by id. Two things use it: the self node, which reports how
	// many callers have been seen (and why none have, where authentication is off), and a declared
	// component whose name matches an observed client_id - see buildObservedSnapshot.
	attributes map[string][]topologyAttribute
}

// buildObservedSnapshot turns the registry into nodes, edges and attributes.
//
// The one piece of real logic here is the correlation with the declared components. An operator who
// declares "nats-bridge" and mints it a token with client_id "nats-bridge" should see ONE box, not
// two describing the same process from different directions - and the merged box is worth more than
// either half, because "declared, health OK, last called 3s ago" and "declared, health OK, has
// never called" are the two cases a bridge that is running but not writing sits between, and
// nothing else in the view tells them apart.
func (s *Server) buildObservedSnapshot(now time.Time) observedSnapshot {
	records, evicted := s.observed.snapshot()

	snapshot := observedSnapshot{attributes: map[string][]topologyAttribute{}}

	declared := make(map[string]bool, len(s.topology.components))
	for _, component := range s.topology.components {
		declared[component.Name] = true
	}

	snapshot.attributes[topologyNodeSelf] = []topologyAttribute{
		{key: "observed_callers", value: observedCallersDescription(len(records), evicted)},
	}

	for _, record := range records {
		if declared[record.id] {
			snapshot.attributes[topologyDeclaredPrefix+record.id] = []topologyAttribute{
				{key: "last_call", value: agoDescription(now, record.lastSeen)},
				{key: "calls", value: strconv.FormatInt(record.calls, 10)},
				{key: "transport", value: transportDescription(record.transports)},
			}

			continue
		}

		snapshot.nodes = append(snapshot.nodes, observedNodeSpec(record, now))
		snapshot.edges = append(snapshot.edges, topologyEdgeSpec{
			from:  topologyObservedPrefix + record.id,
			to:    topologyNodeSelf,
			label: "calls",
		})
	}

	return snapshot
}

// observedNodeSpec describes one caller.
//
// Its kind is always CLIENT, and never inferred from the client_id. Guessing that "nats-bridge" is
// a bridge would be a fiction dressed as discovery, and it would be wrong exactly when it mattered
// - the declared list is where an operator says what something is, and correlating with it is what
// buildObservedSnapshot does instead.
//
// detail is empty because there is nothing honest to put in it: this instance holds no address for
// a caller, which is the whole reason inbound components have to be declared.
func observedNodeSpec(record observedRecord, now time.Time) topologyNodeSpec {
	return topologyNodeSpec{
		id:     topologyObservedPrefix + record.id,
		kind:   contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_CLIENT,
		name:   record.id,
		source: contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_OBSERVED,

		// Never probed, and so no status and no checked_at - see property 1 at the top of this file.
		staticStatus: contract.TopologyStatus_TOPOLOGY_STATUS_UNSPECIFIED,

		attributes: []topologyAttribute{
			{key: "client_id", value: record.id},
			{key: "first_call", value: agoDescription(now, record.firstSeen)},
			{key: "last_call", value: agoDescription(now, record.lastSeen)},
			{key: "calls", value: strconv.FormatInt(record.calls, 10)},
			{key: "transport", value: transportDescription(record.transports)},
			{key: "roles", value: rolesDescription(record.roles)},
			{key: "group_scoped", value: scopedDescription(record.scoped)},
			{key: "health", value: "not known (this instance holds no address for a caller)"},
		},
	}
}

// observedCallersDescription is the self node's account of this whole feature, including the case
// where it can report nothing.
//
// Saying why the inbound column is empty matters more here than anywhere else in the view: an
// operator looking at a deployment with six bridges writing to it, and no boxes for any of them,
// should be told that callers are not identified rather than left to conclude the diagram is broken.
func observedCallersDescription(count int, evicted bool) string {
	if authMethodDescription() == "none" {
		return "not identified (authentication is disabled: auth.method)"
	}

	if count == 0 {
		return "0 (none has called since this instance started)"
	}

	if evicted {
		return strconv.Itoa(count) + " (capped; showing the most recently seen)"
	}

	return strconv.Itoa(count)
}

// transportDescription names which of the two surfaces a caller has used. Both is a normal answer,
// not a fault: one client using the CLI over gRPC and the console over the gateway is one client.
func transportDescription(transports observedTransport) string {
	var used []string

	if transports&observedTransportGRPC != 0 {
		used = append(used, "grpc")
	}

	if transports&observedTransportHTTP != 0 {
		used = append(used, "http")
	}

	if len(used) == 0 {
		return "unknown"
	}

	return strings.Join(used, ", ")
}

// rolesDescription lists the roles the caller's last token carried. "none" is spelled out because it
// is the interesting value: a token resolving to no known tier is refused every RPC, and a client
// that is calling constantly and being refused constantly looks identical to a healthy one here
// unless this row says so.
func rolesDescription(roles []string) string {
	if len(roles) == 0 {
		return "none"
	}

	return strings.Join(roles, ", ")
}

func scopedDescription(scoped bool) string {
	if scoped {
		return "yes"
	}

	return "no"
}

// agoDescription renders a moment as an age, rounded to the second for the same reason the
// heartbeat's is: this is a snapshot taken on an interval, and a millisecond figure would imply a
// precision it does not have. A zero time means it never happened.
func agoDescription(now time.Time, when time.Time) string {
	if when.IsZero() || when.UnixNano() <= 0 {
		return "never"
	}

	age := now.Sub(when).Round(time.Second)

	if age < 0 {
		age = 0
	}

	return age.String() + " ago"
}
