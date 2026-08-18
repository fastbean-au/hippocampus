package hippocampus

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// Peer discovery: the half of the deployment view that nothing outside this instance had to be
// told about.
//
// Everything else in the topology is either this process or something it dials. Peers are neither -
// a replica sharing this store holds no address for this instance and is held by none - and yet the
// horizontally-scaled deployment (one consolidator plus N replicas over one database) is a headline
// feature that, until this file, no instance could describe. The single-consolidator lock proves
// that SOMEBODY holds it, not who; and it says nothing at all about the failure that matters, which
// is that NOBODY does.
//
// The mechanism is a registry table each instance writes its own row into (db/instances.go) and
// every instance reads. Four properties hold it together:
//
//  1. It is a view, never a control plane. An instance writes only its own row and reads the rest
//     to display them. Nothing acts on a peer, and there is no RPC that could.
//
//  2. It runs on the server drivers only, because SQLite is single-instance by construction. Where
//     there is no registry there is no goroutine, no table and no cost.
//
//  3. The reads and the write share one goroutine and one cadence. Reading peers is cheap, but
//     splitting the two would mean an instance could appear in others' views while showing none of
//     its own, or the reverse - an asymmetry with no explanation on either side of it.
//
//  4. Counting is done over FRESH rows only. A dead consolidator's row survives its process by
//     several intervals on purpose (so its disappearance is reported rather than silent), and
//     counting it would report two consolidators exactly when a replacement had correctly taken
//     over - turning the successful case into an alarm.

// instanceDeregisterTimeout bounds the row removal on a clean shutdown. Short: this runs on the
// shutdown path, and a store that cannot take a single-row delete promptly is not worth delaying an
// exit for - the row ages out on its own, which is what the staleness window is for.
const instanceDeregisterTimeout = 5 * time.Second

// peerSnapshot is one round of the registry, converted into the shape topologyResponse merges. It
// is built off the RPC path for the same reason probe results are: assembling a response must not
// query the store, or one console page becomes a query and several viewers become several.
type peerSnapshot struct {
	nodes []topologyNodeSpec
	edges []topologyEdgeSpec

	// storeAttributes are appended to the store node. The peers are all attached to the same store,
	// so how many instances share it - and which of them consolidates - is a property of that node
	// rather than of any one instance.
	storeAttributes []topologyAttribute

	warnings []string
}

// peerSnapshotOrEmpty returns the last round, or an empty snapshot before the first has completed
// (and permanently, where there is no registry). Never nil, so callers need no guard.
func (s *Server) peerSnapshotOrEmpty() peerSnapshot {
	if snapshot := s.peers.Load(); snapshot != nil {
		return *snapshot
	}

	return peerSnapshot{}
}

// startInstanceHeartbeat launches the registry goroutine, if this deployment has a registry to keep.
//
// Three gates, and each disables it completely rather than degrading it: the view is off, the
// cadence is zero (topology.heartbeatSeconds, an operator declining to write rows at all), or the
// store keeps no registry (SQLite). Started and stopped exactly as the prober and the reconcile
// sweep are, so shutdown needs no new pattern.
func (s *Server) startInstanceHeartbeat() {
	if !s.topology.enabled || s.topology.heartbeatInterval <= 0 {
		return
	}

	if !s.db.InstanceRegistryAvailable() {
		return
	}

	s.stopHeartbeat = make(chan struct{})
	s.heartbeatStopped = make(chan struct{})

	log.Infof("registering this instance in the shared store as '%s' every %s", s.instanceId(), s.topology.heartbeatInterval)

	go s.heartbeatLoop()
}

// heartbeatLoop writes and reads once immediately, then on the interval. The immediate round is what
// makes an instance visible to its peers - and its peers visible to it - from the moment it starts
// serving, rather than one interval later, which is long enough for somebody investigating a
// deployment that has just come up to conclude the feature does not work.
func (s *Server) heartbeatLoop() {
	defer close(s.heartbeatStopped)

	s.heartbeatOnce()

	ticker := time.NewTicker(s.topology.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {

		case <-s.stopHeartbeat:
			return

		case <-ticker.C:
			s.heartbeatOnce()

		}
	}
}

// heartbeatOnce writes this instance's row, prunes the rows of instances that have stopped writing
// theirs, and republishes the peer snapshot.
//
// Every failure here is best-effort and logged rather than propagated: nothing in the service
// depends on this table, and a store too unwell to take a liveness write is already being reported
// as such by its own node. The one thing that must not happen is a failed round replacing a good
// snapshot with an empty one, which would render as every peer having vanished - so a failed read
// leaves the previous round in place, and its checked_at then says how stale it is.
func (s *Server) heartbeatOnce() {
	log.Trace("func() hippocampus.heartbeatOnce")

	// A round that cannot finish within its own interval is abandoned rather than allowed to overlap
	// the next - the same bound, and the same reasoning, as the probe round's.
	ctx, cancel := context.WithTimeout(context.Background(), s.topology.heartbeatInterval)
	defer cancel()

	now := time.Now()

	if err := s.db.Heartbeat(ctx, s.instanceRecord(now)); err != nil {
		log.Warnf("failed to record this instance's heartbeat: %s", err.Error())
	}

	instances, err := s.db.ListInstances(ctx)
	if err != nil {
		log.Warnf("failed to read the instance registry: %s", err.Error())

		return
	}

	snapshot := s.buildPeerSnapshot(instances, now)

	s.logWarningChanges(snapshot.warnings)
	s.peers.Store(&snapshot)
}

// logWarningChanges writes each deployment warning to the log the first time it appears, and says so
// when one clears. Logging them every round would be a fault message repeating twice a minute for as
// long as the fault lasted, which trains a reader to filter it out; logging them never would leave
// the only report of "nothing is consolidating" inside a view somebody has to open.
//
// Only the heartbeat goroutine touches lastWarnings, so it needs no lock.
func (s *Server) logWarningChanges(warnings []string) {
	previous := make(map[string]bool, len(s.lastWarnings))

	for _, warning := range s.lastWarnings {
		previous[warning] = true
	}

	current := make(map[string]bool, len(warnings))

	for _, warning := range warnings {
		current[warning] = true

		if !previous[warning] {
			log.Warn(warning)
		}
	}

	for _, warning := range s.lastWarnings {
		if !current[warning] {
			log.Infof("resolved: %s", warning)
		}
	}

	s.lastWarnings = warnings
}

// stopInstanceHeartbeat shuts the goroutine down, waits for it, and removes this instance's row.
//
// The removal is what separates "stopped" from "stopped answering". An instance that exits cleanly
// should leave the view immediately; the staleness window exists for the instances that go without
// being able to say so, which is the case no one else can report on their behalf.
func (s *Server) stopInstanceHeartbeat() {
	if s.stopHeartbeat == nil {
		return
	}

	close(s.stopHeartbeat)
	<-s.heartbeatStopped

	ctx, cancel := context.WithTimeout(context.Background(), instanceDeregisterTimeout)
	defer cancel()

	if err := s.db.DeregisterInstance(ctx, s.instanceId()); err != nil {
		log.Warnf("failed to remove this instance from the shared store's registry: %s", err.Error())
	}
}

// instanceId is this instance's registry key: hostname:port, and deterministic on purpose.
//
// A random per-process id was the alternative and loses badly: every restart would leave a ghost row
// visible until it aged out, which in a rolling deployment is most of the rows on the board most of
// the time. Two processes cannot share a host and a port, so a deterministic id cannot collide -
// and a restarting instance upserts its own row rather than adding a second one.
func (s *Server) instanceId() string {
	return s.topology.instanceId
}

// instanceRecord describes this instance as its registry row.
//
// The capability flags are the point of the row beyond mere presence: two instances answering the
// same RPCs with different features enabled - one with the search index, one without - is a real
// misconfiguration that is otherwise entirely silent, since each instance is behaving exactly as its
// own configuration says it should.
func (s *Server) instanceRecord(now time.Time) db.Instance {
	role := db.InstanceRoleReplica
	if s.consolidationEnabled {
		role = db.InstanceRoleConsolidator
	}

	return db.Instance{
		Id:               s.instanceId(),
		Hostname:         s.topology.hostname,
		Version:          s.topology.version,
		Role:             role,
		StartedAt:        s.topology.startedAt.UnixNano(),
		LastSeen:         now.UnixNano(),
		HeartbeatSeconds: int(s.topology.heartbeatInterval / time.Second),
		Search:           s.searchIdx().Enabled(),
		Summariser:       s.summariser().Enabled(),
		Embedder:         s.embedder().Enabled(),
		Gateway:          viper.GetInt("gateway.port") > 0,
	}
}

// buildPeerSnapshot turns a round of registry rows into nodes, edges, store attributes and warnings.
//
// This instance's own row is counted but not drawn: it is already the self node, and a second box
// carrying the same hostname would read as a duplicate deployment rather than as the row proving
// this instance is registered.
func (s *Server) buildPeerSnapshot(instances []db.Instance, now time.Time) peerSnapshot {
	snapshot := peerSnapshot{}

	nanos := now.UnixNano()
	self := s.instanceId()

	// Counted over FRESH rows only. A consolidator that died leaves its row behind for several
	// intervals by design, and counting it would report two consolidators at exactly the moment a
	// replacement had correctly taken the lock - reporting the recovery as the fault.
	var (
		live          int
		consolidators []string
	)

	for _, instance := range instances {
		if instance.Stale(nanos) {
			continue
		}

		live++

		if instance.Role == db.InstanceRoleConsolidator {
			consolidators = append(consolidators, instance.Id)
		}
	}

	for _, instance := range instances {
		if instance.Id == self {
			continue
		}

		snapshot.nodes = append(snapshot.nodes, peerNodeSpec(instance, now, len(consolidators)))
		snapshot.edges = append(snapshot.edges, topologyEdgeSpec{
			from:  topologyPeerPrefix + instance.Id,
			to:    topologyNodeStore,
			label: "reads/writes",
		})
	}

	snapshot.storeAttributes = []topologyAttribute{
		{key: "instances", value: strconv.Itoa(live)},
		{key: "consolidators", value: consolidatorDescription(consolidators)},
	}

	snapshot.warnings = s.deploymentWarnings(live, consolidators, now)

	return snapshot
}

// consolidatorDescription names which instances are running sleep cycles, since that is the question
// a shared store is actually asked. "none" is spelled out rather than left as 0, because it is the
// one value here that is a fault.
func consolidatorDescription(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}

	return strconv.Itoa(len(ids)) + " (" + strings.Join(ids, ", ") + ")"
}

// deploymentWarnings reports the two conditions that no node can carry.
//
// The zero-consolidator case is the reason this whole file exists: every instance came up with
// consolidation.enabled false, so nothing is forgetting, nothing is evicting, and the store grows
// without bound while every individual instance reports itself perfectly healthy - because it is.
// There is no node to attach that to; the fault IS the absent node.
//
// It is suppressed for the first few intervals of this process's life. During a rolling deployment a
// replica routinely starts before the consolidator has registered, and a banner that always appears
// for a minute after every deploy is one nobody reads the second time.
func (s *Server) deploymentWarnings(live int, consolidators []string, now time.Time) []string {
	var warnings []string

	switch {

	case len(consolidators) == 0:
		if now.Sub(s.topology.startedAt) < 2*s.topology.heartbeatInterval {
			break
		}

		warnings = append(warnings, fmt.Sprintf(
			"no instance is running consolidation: %d instance(s) share this store and none holds the consolidator role, so nothing is forgetting, evicting or summarising and the store will grow without bound (set consolidation.enabled on exactly one of them)",
			live,
		))

	case len(consolidators) > 1:
		warnings = append(warnings, fmt.Sprintf(
			"%d instances report the consolidator role (%s): exactly one may run sleep cycles against a store, so check that they are not pointed at different databases by mistake",
			len(consolidators), strings.Join(consolidators, ", "),
		))

	}

	return warnings
}

// peerNodeSpec describes one peer instance.
//
// Its status is derived from the row's freshness rather than from a probe, and that is a deliberate
// difference from every other node here: this instance holds no address it could dial a peer on, and
// inventing one from the registry would be the first step towards the control plane this view is
// careful not to become. What the row proves is that the peer was writing to this store recently,
// which is the property that actually matters about a peer - one that has stopped writing has
// stopped serving from here whether or not its port still answers.
func peerNodeSpec(instance db.Instance, now time.Time, consolidators int) topologyNodeSpec {
	name := instance.Hostname
	if name == "" {
		name = instance.Id
	}

	spec := topologyNodeSpec{
		id:           topologyPeerPrefix + instance.Id,
		kind:         contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_INSTANCE,
		name:         name,
		detail:       redactEndpoint(instance.Id),
		source:       contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_DISCOVERED,
		version:      instance.Version,
		checkedAt:    now,
		staticStatus: contract.TopologyStatus_TOPOLOGY_STATUS_OK,
		attributes: []topologyAttribute{
			{key: "role", value: instance.Role},
			{key: "hostname", value: instance.Hostname},
			{key: "started", value: time.Unix(0, instance.StartedAt).UTC().Format(time.RFC3339)},
			{key: "last_heartbeat", value: heartbeatAgeDescription(now, instance)},
			{key: "search", value: enabledDescription(instance.Search)},
			{key: "summariser", value: enabledDescription(instance.Summariser)},
			{key: "embedder", value: enabledDescription(instance.Embedder)},
			{key: "gateway", value: enabledDescription(instance.Gateway)},
		},
	}

	if instance.Stale(now.UnixNano()) {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE
		spec.statusDetail = "has not written a heartbeat since " +
			heartbeatAgeDescription(now, instance) + "; its row will be removed if it does not return"

		return spec
	}

	// Marked on the peers rather than only stated in the warning, because the warning names ids and
	// the diagram is where somebody is looking when they ask which of these boxes is the duplicate.
	if consolidators > 1 && instance.Role == db.InstanceRoleConsolidator {
		spec.staticStatus = contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED
		spec.statusDetail = "another instance also reports the consolidator role"
	}

	return spec
}

// heartbeatAgeDescription renders how long ago a row was refreshed. Rounded to the second, since the
// cadence is measured in tens of them and a millisecond figure would only imply a precision the
// snapshot does not have.
func heartbeatAgeDescription(now time.Time, instance db.Instance) string {
	age := now.Sub(time.Unix(0, instance.LastSeen)).Round(time.Second)

	if age < 0 {
		age = 0
	}

	return age.String() + " ago"
}

// hostnameOrUnknown reads this host's name once at startup. It is the label every peer is drawn
// with and half of every instance id, so an error here is reported as a value rather than as a
// failure to start: an instance that cannot name itself is still an instance, and refusing to serve
// over a diagnostic view would be badly out of proportion.
func hostnameOrUnknown() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}

	return hostname
}
