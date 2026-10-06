package hippocampus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
)

// Three concurrency loose ends (TODO-3 item 164): a data race between the preview snapshot and the
// sleep goroutine, a singleflight leader's cancellation failing every caller that joined it, and
// the database closing beneath a cycle still running when the server stopped.

// TestPreviewSnapshotDoesNotRaceTheCycle is only a finding under -race, which the CI race job runs:
// decisionSnapshot read the default event significance the sleep goroutine writes, with nothing
// ordering the two.
//
// Both sides run on a cancelled context and the percentile is answered without the store. Reaching
// the store would serialise them on SQLite's one pooled connection, and the race detector reads that
// as ordering, so the race would go unreported.
func TestPreviewSnapshotDoesNotRaceTheCycle(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	s := &Server{
		db: fixedPercentile{Store: database, value: 7},
		consolidation: Consolidation{
			method:                             1,
			aggressiveness:                     1.0,
			unitsOfAgeInDays:                   1.0,
			deletionThreshold:                  1.0,
			defaultEventSignificanceValue:      5,
			defaultEventSignificancePercentile: 50,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for range 50 {
			_ = s.consolidate(ctx, &cycleReport{})
		}
	}()

	for range 50 {
		_, _ = s.decisionSnapshot(ctx)
	}

	wg.Wait()

	if got := s.defaultEventSignificance(); got != 7 {
		t.Errorf("default event significance = %d, want the computed percentile (7) in place of the configured 5", got)
	}
}

// fixedPercentile answers the significance percentile without the store.
type fixedPercentile struct {
	db.Store

	value float64
}

func (f fixedPercentile) CalculateSignificancePercentile(context.Context, float64) (float64, error) {
	return f.value, nil
}

// gatedUsedBytes holds UsedBytes - the first store read a decision snapshot makes - until released,
// answering a cancelled context the way a driver does.
type gatedUsedBytes struct {
	db.Store

	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (g gatedUsedBytes) UsedBytes(ctx context.Context) (int64, error) {
	g.once.Do(func() { close(g.entered) })

	select {

	case <-ctx.Done():
		return 0, ctx.Err()

	case <-g.release:
		return g.Store.UsedBytes(ctx)

	}
}

func gatedServer(t *testing.T) (*Server, gatedUsedBytes) {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	gate := gatedUsedBytes{Store: database, entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}

	return &Server{
		db:                   gate,
		consolidationEnabled: true,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}, gate
}

// TestALeaderGivingUpDoesNotFailItsFollowers drives both shared snapshots: one console tab closing
// must not fail the request of every other viewer that joined its scan.
func TestALeaderGivingUpDoesNotFailItsFollowers(t *testing.T) {
	calls := map[string]func(s *Server, ctx context.Context) error{
		"preview": func(s *Server, ctx context.Context) error {
			_, err := s.previewOnce(ctx, db.PreviewLimit(0))

			return err
		},
		"explain": func(s *Server, ctx context.Context) error {
			_, err := s.cachedDecisionSnapshot(ctx)

			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			s, gate := gatedServer(t)

			leaderCtx, cancelLeader := context.WithCancel(context.Background())
			leader := make(chan error, 1)

			go func() { leader <- call(s, leaderCtx) }()

			<-gate.entered

			follower := make(chan error, 1)

			go func() { follower <- call(s, context.Background()) }()

			// Long enough for the follower to join the in-flight call; nothing observable marks it.
			time.Sleep(50 * time.Millisecond)

			cancelLeader()

			select {

			case err := <-leader:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("the leader that gave up returned %v, want its own cancellation", err)
				}

			case <-time.After(5 * time.Second):
				t.Fatal("the leader did not give up when its context was cancelled")

			}

			close(gate.release)

			select {

			case err := <-follower:
				if err != nil {
					t.Errorf("a follower failed with %v when only its leader gave up", err)
				}

			case <-time.After(5 * time.Second):
				t.Fatal("the follower never received the shared result")

			}
		})
	}
}

// gatedCycle holds a sleep cycle at its first store call until released.
type gatedCycle struct {
	db.Store

	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (g gatedCycle) BeginCallbackCycle(cycleId int64) {
	g.once.Do(func() { close(g.entered) })

	<-g.release

	g.Store.BeginCallbackCycle(cycleId)
}

// TestStopWaitsForACycleInFlight: a cycle an RPC started runs on its own context, so a forced
// shutdown that stopped the gRPC server could close the database beneath it. Stop now waits, and no
// cycle starts once it has begun.
func TestStopWaitsForACycleInFlight(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	gate := gatedCycle{Store: database, entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}

	s := &Server{
		db:                   gate,
		consolidationEnabled: true,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	cycle := make(chan error, 1)

	go func() { cycle <- s.sleepOnce(triggerManual) }()

	<-gate.entered

	stopped := make(chan struct{})

	go func() {
		s.Stop()
		close(stopped)
	}()

	select {

	case <-stopped:
		t.Fatal("Stop returned while a cycle was still running, leaving the database to close beneath it")

	case <-time.After(100 * time.Millisecond):

	}

	close(gate.release)

	select {

	case <-stopped:

	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return once the cycle finished")

	}

	if err := <-cycle; err != nil {
		t.Errorf("the cycle in flight failed: %s", err)
	}

	if err := s.sleepOnce(triggerManual); err == nil {
		t.Error("a cycle started after Stop, against a store that is about to close")
	}
}
