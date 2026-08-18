package hippocampus

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// registryStore is a store whose instance registry is entirely under the test's control. It embeds
// db.Store so the four registry methods can be driven without a real database, and records what was
// written so the heartbeat's own row can be inspected.
type registryStore struct {
	db.Store

	mu           sync.Mutex
	available    bool
	instances    []db.Instance
	written      []db.Instance
	deregistered []string
	listErr      error
}

func (r *registryStore) InstanceRegistryAvailable() bool {
	return r.available
}

func (r *registryStore) Heartbeat(_ context.Context, instance db.Instance) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.written = append(r.written, instance)

	return nil
}

func (r *registryStore) ListInstances(_ context.Context) ([]db.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.listErr != nil {
		return nil, r.listErr
	}

	return append([]db.Instance{}, r.instances...), nil
}

func (r *registryStore) DeregisterInstance(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.deregistered = append(r.deregistered, id)

	return nil
}

func (r *registryStore) snapshot() ([]db.Instance, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]db.Instance{}, r.written...), append([]string{}, r.deregistered...)
}

// newPeerServer builds a topology server whose store keeps a registry, with the registry's rows set
// to the given list and this process presented as having started long enough ago that the
// no-consolidator warning is no longer being held back.
func newPeerServer(t *testing.T, instances []db.Instance) (*Server, *registryStore) {
	t.Helper()

	s := newTopologyServer(t)

	store := &registryStore{available: true, instances: instances}

	s.db = store
	s.topology.heartbeatInterval = 30 * time.Second
	s.topology.instanceId = "hippo-1:50051"
	s.topology.hostname = "hippo-1"
	s.topology.startedAt = time.Now().Add(-time.Hour)

	return s, store
}

// liveInstance is a registry row written just now, so it reads as fresh whatever the clock does
// between building it and asserting on it.
func liveInstance(id string, hostname string, role string) db.Instance {
	return db.Instance{
		Id:               id,
		Hostname:         hostname,
		Role:             role,
		Version:          "v1.2.3",
		StartedAt:        time.Now().Add(-time.Hour).UnixNano(),
		LastSeen:         time.Now().UnixNano(),
		HeartbeatSeconds: 30,
	}
}

// TestPeersAreDiscoveredNotDeclared covers the case the whole phase exists for: a replica sharing
// this store becomes a node without anybody configuring it anywhere, sourced DISCOVERED so a reader
// can tell it apart from the components an operator had to type in.
func TestPeersAreDiscoveredNotDeclared(t *testing.T) {
	s, _ := newPeerServer(t, []db.Instance{
		liveInstance("hippo-1:50051", "hippo-1", db.InstanceRoleConsolidator),
		liveInstance("hippo-2:50051", "hippo-2", db.InstanceRoleReplica),
	})

	s.heartbeatOnce()

	res := s.topologyResponse()
	nodes := nodesById(res)

	peer, ok := nodes["peer:hippo-2:50051"]
	if !ok {
		t.Fatal("the peer sharing this store was not discovered")
	}

	if peer.GetSource() != contract.TopologyNodeSource_TOPOLOGY_NODE_SOURCE_DISCOVERED {
		t.Errorf("a peer is sourced %s, want DISCOVERED", peer.GetSource())
	}

	if peer.GetKind() != contract.TopologyNodeKind_TOPOLOGY_NODE_KIND_INSTANCE {
		t.Errorf("a peer is kind %s, want INSTANCE", peer.GetKind())
	}

	if peer.GetVersion() != "v1.2.3" {
		t.Errorf("a peer reports version %q; a mid-rollout deployment is exactly where this matters", peer.GetVersion())
	}

	if peer.GetCheckedAt() == 0 {
		t.Error("a peer must say when its row was last read: every status here is a snapshot")
	}

	// This instance's own row is counted but never drawn: it is already the self node, and a second
	// box carrying the same hostname reads as a duplicate deployment.
	if _, drawn := nodes["peer:hippo-1:50051"]; drawn {
		t.Error("this instance was drawn as its own peer")
	}

	// The edge runs to the store, not to this instance. Peers are siblings; nothing dials anything.
	var found bool

	for _, edge := range res.GetEdges() {
		if edge.GetFromId() == "peer:hippo-2:50051" {
			found = true

			if edge.GetToId() != topologyNodeStore {
				t.Errorf("a peer's edge points at %q, want the shared store", edge.GetToId())
			}
		}
	}

	if !found {
		t.Error("the peer has no edge; it is drawn attached to nothing")
	}

	// The store gains what only the registry can say: how many instances share it, and which of them
	// consolidates - the question a horizontally-scaled deployment actually asks.
	store := nodes[topologyNodeStore]

	if got := attributeValue(store, "instances"); got != "2" {
		t.Errorf("store instances = %q, want 2", got)
	}

	if got := attributeValue(store, "consolidators"); !strings.Contains(got, "hippo-1:50051") {
		t.Errorf("store consolidators = %q, want the consolidator named", got)
	}

	if len(res.GetWarnings()) != 0 {
		t.Errorf("a healthy deployment warned: %v", res.GetWarnings())
	}
}

// TestNoConsolidatorIsWarned covers the silent failure this phase was built to expose: every
// instance came up as a replica, so nothing is forgetting or evicting and the store simply grows -
// while each instance individually reports itself perfectly healthy, because it is.
func TestNoConsolidatorIsWarned(t *testing.T) {
	s, _ := newPeerServer(t, []db.Instance{
		liveInstance("hippo-1:50051", "hippo-1", db.InstanceRoleReplica),
		liveInstance("hippo-2:50051", "hippo-2", db.InstanceRoleReplica),
	})

	s.heartbeatOnce()

	res := s.topologyResponse()

	if len(res.GetWarnings()) != 1 {
		t.Fatalf("expected exactly one warning, got %v", res.GetWarnings())
	}

	if !strings.Contains(res.GetWarnings()[0], "no instance is running consolidation") {
		t.Errorf("the warning does not say what is wrong: %q", res.GetWarnings()[0])
	}

	if got := attributeValue(nodesById(res)[topologyNodeStore], "consolidators"); got != "none" {
		t.Errorf("consolidators = %q, want it spelled out as none", got)
	}
}

// TestNoConsolidatorWarningIsHeldBackAtStartup pins the rolling-deployment case. A replica routinely
// starts before the consolidator has registered, and a banner that appears for a minute after every
// deploy is one nobody reads the second time.
func TestNoConsolidatorWarningIsHeldBackAtStartup(t *testing.T) {
	s, _ := newPeerServer(t, []db.Instance{
		liveInstance("hippo-1:50051", "hippo-1", db.InstanceRoleReplica),
	})

	s.topology.startedAt = time.Now()

	s.heartbeatOnce()

	if warnings := s.topologyResponse().GetWarnings(); len(warnings) != 0 {
		t.Errorf("warned during the startup grace: %v", warnings)
	}
}

// TestTwoConsolidatorsAreWarned covers the other side: the single-consolidator lock has been
// circumvented, or two tiers are pointed at different databases and each believes it is alone.
func TestTwoConsolidatorsAreWarned(t *testing.T) {
	s, _ := newPeerServer(t, []db.Instance{
		liveInstance("hippo-1:50051", "hippo-1", db.InstanceRoleConsolidator),
		liveInstance("hippo-2:50051", "hippo-2", db.InstanceRoleConsolidator),
	})

	s.heartbeatOnce()

	res := s.topologyResponse()

	if len(res.GetWarnings()) != 1 || !strings.Contains(res.GetWarnings()[0], "consolidator role") {
		t.Fatalf("expected a duplicate-consolidator warning, got %v", res.GetWarnings())
	}

	// Marked on the node as well as stated in the warning: the warning names ids, and the diagram is
	// where somebody is looking when they ask which of these boxes is the duplicate.
	peer := nodesById(res)["peer:hippo-2:50051"]

	if peer.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_DEGRADED {
		t.Errorf("the duplicate consolidator is %s, want DEGRADED", peer.GetStatus())
	}
}

// TestStaleRowsAreReportedButNotCounted is the trap worth a test of its own. A dead consolidator's
// row outlives its process by design, so that its disappearance is reported rather than silent -
// and counting it would report two consolidators at exactly the moment a replacement had correctly
// taken over, turning the recovery into an alarm.
func TestStaleRowsAreReportedButNotCounted(t *testing.T) {
	dead := liveInstance("hippo-2:50051", "hippo-2", db.InstanceRoleConsolidator)
	dead.LastSeen = time.Now().Add(-5 * time.Minute).UnixNano()

	s, _ := newPeerServer(t, []db.Instance{
		liveInstance("hippo-1:50051", "hippo-1", db.InstanceRoleConsolidator),
		dead,
	})

	s.heartbeatOnce()

	res := s.topologyResponse()

	if warnings := res.GetWarnings(); len(warnings) != 0 {
		t.Errorf("a stale row was counted as a live consolidator: %v", warnings)
	}

	nodes := nodesById(res)

	if got := attributeValue(nodes[topologyNodeStore], "instances"); got != "1" {
		t.Errorf("store instances = %q, want only the live one counted", got)
	}

	peer := nodes["peer:hippo-2:50051"]

	if peer.GetStatus() != contract.TopologyStatus_TOPOLOGY_STATUS_UNREACHABLE {
		t.Errorf("a peer that stopped heartbeating is %s, want UNREACHABLE", peer.GetStatus())
	}

	if peer.GetStatusDetail() == "" {
		t.Error("an unreachable peer reported no reason; a reader is left with a colour")
	}
}

// TestHeartbeatFailureKeepsTheLastSnapshot covers the failure mode that would be worse than the
// failure: a round that cannot read the registry must not replace a good snapshot with an empty
// one, which renders as every peer having vanished.
func TestHeartbeatFailureKeepsTheLastSnapshot(t *testing.T) {
	s, store := newPeerServer(t, []db.Instance{
		liveInstance("hippo-1:50051", "hippo-1", db.InstanceRoleConsolidator),
		liveInstance("hippo-2:50051", "hippo-2", db.InstanceRoleReplica),
	})

	s.heartbeatOnce()

	store.mu.Lock()
	store.listErr = context.DeadlineExceeded
	store.mu.Unlock()

	s.heartbeatOnce()

	if _, ok := nodesById(s.topologyResponse())["peer:hippo-2:50051"]; !ok {
		t.Error("a failed round dropped the peers it could not re-read")
	}
}

// TestHeartbeatWritesThisInstancesOwnRow checks what the row actually says. The capability flags are
// the point of it beyond mere presence: two instances answering the same RPCs with different
// features enabled is a real misconfiguration that is otherwise entirely silent.
func TestHeartbeatWritesThisInstancesOwnRow(t *testing.T) {
	s, store := newPeerServer(t, nil)

	s.heartbeatOnce()

	written, _ := store.snapshot()

	if len(written) != 1 {
		t.Fatalf("expected one row written, got %d", len(written))
	}

	row := written[0]

	if row.Id != "hippo-1:50051" {
		t.Errorf("id = %q, want the deterministic hostname:port", row.Id)
	}

	if row.Role != db.InstanceRoleConsolidator {
		t.Errorf("role = %q, want consolidator (this server consolidates)", row.Role)
	}

	if row.HeartbeatSeconds != 30 {
		t.Errorf("heartbeat_seconds = %d, want the interval this instance writes on", row.HeartbeatSeconds)
	}

	if !row.Gateway {
		t.Error("the gateway flag is unset although this instance serves one")
	}

	if row.Summariser || row.Embedder {
		t.Error("a capability this instance does not have was reported")
	}
}

// TestHeartbeatIsNotStartedWithoutARegistry covers the three gates, and in particular the SQLite one:
// there is no table there, so there must be no goroutine, no write, and nothing to stop.
func TestHeartbeatIsNotStartedWithoutARegistry(t *testing.T) {
	for name, prepare := range map[string]func(s *Server, store *registryStore){
		"no registry in the store": func(_ *Server, store *registryStore) { store.available = false },
		"heartbeating disabled":    func(s *Server, _ *registryStore) { s.topology.heartbeatInterval = 0 },
		"the view is disabled":     func(s *Server, _ *registryStore) { s.topology.enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			s, store := newPeerServer(t, nil)

			prepare(s, store)
			s.startInstanceHeartbeat()

			if s.stopHeartbeat != nil {
				t.Error("a heartbeat goroutine was started where there is nothing to register with")
			}

			// Safe to call regardless, so Stop needs no gate of its own.
			s.stopInstanceHeartbeat()

			if written, _ := store.snapshot(); len(written) != 0 {
				t.Errorf("%d rows were written anyway", len(written))
			}
		})
	}
}

// TestStoppingDeregistersThisInstance pins the difference between "stopped" and "stopped answering".
// An instance that exits cleanly leaves the view at once; the staleness window is for the ones that
// went without being able to say so.
func TestStoppingDeregistersThisInstance(t *testing.T) {
	s, store := newPeerServer(t, nil)

	s.startInstanceHeartbeat()

	if s.stopHeartbeat == nil {
		t.Fatal("the heartbeat goroutine was not started")
	}

	s.stopInstanceHeartbeat()

	written, deregistered := store.snapshot()

	if len(written) == 0 {
		t.Error("the immediate first round did not run: an instance would be invisible to its peers for a whole interval")
	}

	if len(deregistered) != 1 || deregistered[0] != "hippo-1:50051" {
		t.Errorf("deregistered = %v, want this instance's own id once", deregistered)
	}
}

// TestWarningsAreLoggedOnChange checks the bookkeeping behind logging a fault once rather than twice
// a minute for as long as it lasts - and saying so when it clears, which is the half that is easy to
// leave out and the half a reader needs to stop worrying.
func TestWarningsAreLoggedOnChange(t *testing.T) {
	s, _ := newPeerServer(t, nil)

	s.logWarningChanges([]string{"a", "b"})

	if len(s.lastWarnings) != 2 {
		t.Fatalf("lastWarnings = %v, want both recorded", s.lastWarnings)
	}

	s.logWarningChanges(nil)

	if len(s.lastWarnings) != 0 {
		t.Errorf("lastWarnings = %v, want cleared once the conditions resolved", s.lastWarnings)
	}
}
