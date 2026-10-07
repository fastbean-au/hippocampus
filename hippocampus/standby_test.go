package hippocampus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// Consolidator failover (TODO-3 item 169): a standby serves as a replica until it wins the
// single-consolidator lock, and then runs the sleep cycle.

// lockableStore stands in for a shared store whose lock another instance holds until released.
type lockableStore struct {
	db.Store

	free  *atomic.Bool
	asked *atomic.Int32
}

func (l lockableStore) TryAcquireInstanceLock() (bool, error) {
	l.asked.Add(1)

	return l.free.Load(), nil
}

func standbyServer(t *testing.T) (*Server, *atomic.Bool) {
	t.Helper()

	cfg := testConfig()
	cfg.Consolidation.Enabled = false
	cfg.Consolidation.Standby = true
	cfg.Consolidation.StandbyPoll = time.Second
	cfg.SleepPeriod = time.Hour
	cfg.Topology.Enabled = true

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	free := &atomic.Bool{}

	s := New(Dependencies{DB: lockableStore{Store: database, free: free, asked: &atomic.Int32{}}}, cfg)
	t.Cleanup(s.Stop)

	return s, free
}

func TestAStandbyServesAsAReplicaUntilItWinsTheLock(t *testing.T) {
	t.Parallel()

	s, free := standbyServer(t)

	if s.consolidating() {
		t.Fatal("a standby consolidates while another instance holds the lock")
	}

	if _, err := s.Sleep(context.Background(), &contract.EmptyRequest{}); err == nil {
		t.Error("a standby that has not won the lock ran a sleep cycle")
	}

	free.Store(true)

	deadline := time.Now().Add(5 * time.Second)

	for !s.consolidating() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if !s.consolidating() {
		t.Fatal("the standby did not take over once the lock was free")
	}

	if s.nextSleep.Load() == 0 {
		t.Error("the promoted standby's sleep loop has no timed cycle")
	}

	if _, err := s.Sleep(context.Background(), &contract.EmptyRequest{}); err != nil {
		t.Errorf("the promoted standby refused a sleep cycle: %s", err)
	}

	status, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if !status.GetConsolidationEnabled() {
		t.Error("GetConsolidationStatus still reports a replica after the takeover")
	}

	topology, err := s.GetTopology(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetTopology: %s", err)
	}

	for _, node := range topology.GetNodes() {
		if node.GetId() != topologyNodeSelf {
			continue
		}

		for _, a := range node.GetAttributes() {
			if a.GetKey() == "role" && a.GetValue() != "consolidator" {
				t.Errorf("the topology view still names the promoted standby a %q", a.GetValue())
			}
		}
	}
}

// TestStopEndsAStandbyThatNeverWon: shutdown must not wait on a lock that is never released.
func TestStopEndsAStandbyThatNeverWon(t *testing.T) {
	t.Parallel()

	s, _ := standbyServer(t)

	stopped := make(chan struct{})

	go func() {
		s.Stop()
		close(stopped)
	}()

	select {

	case <-stopped:

	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while the standby was still waiting")

	}

	if s.consolidating() {
		t.Error("a stopped standby reports consolidating")
	}
}
