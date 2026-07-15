package hippocampus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// TestSleepOnce_ConcurrentCallersShareOneExecution reproduces the race between the autoSleep
// timer and a manual Sleep RPC: before sleepGroup, nothing stopped s.sleep() running concurrently
// with itself when both fired at once, letting two consolidation/eviction cycles interleave.
// sleepGroup.Do collapses concurrent callers into a single in-flight execution, so this drives
// many goroutines at s.sleepGroup with the same key sleepOnce uses and asserts at most one is
// ever inside the guarded section at a time, and that not every caller ran it themselves.
func TestSleepOnce_ConcurrentCallersShareOneExecution(t *testing.T) {
	s := &Server{}

	var (
		mu      sync.Mutex
		running int
		maxSeen int
		ran     int
	)

	const callers = 20

	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < callers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			_, _, _ = s.sleepGroup.Do(sleepSingleflightKey, func() (any, error) {
				mu.Lock()
				running++
				ran++
				if running > maxSeen {
					maxSeen = running
				}
				mu.Unlock()

				time.Sleep(20 * time.Millisecond)

				mu.Lock()
				running--
				mu.Unlock()

				return nil, nil
			})
		}()
	}

	close(start)
	wg.Wait()

	if maxSeen > 1 {
		t.Errorf("expected at most 1 concurrent sleep execution, saw %d", maxSeen)
	}

	if ran < 1 {
		t.Fatal("expected the guarded function to run at least once")
	}

	if ran == callers {
		t.Errorf("expected callers to share a single in-flight execution, but all %d ran it themselves", callers)
	}
}

// TestSleepTimer_NonPositivePeriodDisablesTimedSleep asserts the contract: a
// non-positive sleep.periodSeconds means "no automatic timed sleep". sleepTimer must return a nil
// channel for those periods (which blocks forever in autoSleep's select, dropping the timed case)
// and a live, firing timer for a positive period.
func TestSleepTimer_NonPositivePeriodDisablesTimedSleep(t *testing.T) {
	if ch := sleepTimer(0); ch != nil {
		t.Error("sleepTimer(0) returned a non-nil channel; a zero period must disable timed sleep")
	}

	if ch := sleepTimer(-5 * time.Second); ch != nil {
		t.Error("sleepTimer(-5s) returned a non-nil channel; a negative period must disable timed sleep")
	}

	ch := sleepTimer(time.Millisecond)
	if ch == nil {
		t.Fatal("sleepTimer(1ms) returned nil; a positive period must produce a firing timer")
	}

	select {
	case <-ch:
		// fired as expected

	case <-time.After(time.Second):
		t.Error("sleepTimer(1ms) did not fire within a second")
	}
}

// TestPurgeInProgress_ConcurrentAccess drives concurrent Purge calls and interceptor checks at
// the race detector. purgeInProgress is written by Purge and read by
// InterceptorBlockWhenPurgeInProgress from every RPC's own goroutine; before it became an
// atomic.Bool this was an unsynchronized read/write of a plain bool across goroutines.
func TestPurgeInProgress_ConcurrentAccess(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	defer func() { _ = database.Close() }()

	s := &Server{db: database}

	info := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/GetEvents"}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, _ = s.Purge(context.Background(), &contract.EmptyRequest{})
		}()
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, _ = s.InterceptorBlockWhenPurgeInProgress(context.Background(), nil, info, handler)
		}()
	}

	wg.Wait()
}

// walTestServer builds a Server over a real file-backed database with minimal-but-valid
// consolidation settings, so sleep() runs its full pipeline (consolidate/evict/preserve) without
// erroring, for exercising checkWALTrigger against a real WAL file.
func walTestServer(t *testing.T, walTriggerBytes int64) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New(t.TempDir())
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	s := &Server{
		db:                   database,
		consolidationEnabled: true,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
			walTriggerBytes:   walTriggerBytes,
		},
	}

	return s, database
}

// TestCheckWALTrigger_RunsSleepWhenOverThreshold verifies that checkWALTrigger runs a sleep cycle
// (and so checkpoints the WAL, per Preserve) once the on-disk WAL exceeds walTriggerBytes.
func TestCheckWALTrigger_RunsSleepWhenOverThreshold(t *testing.T) {
	s, database := walTestServer(t, 1)

	body := make([]byte, 256*1024)
	if _, err := database.CreateMemory(types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	before, err := database.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes: %s", err)
	}

	if before == 0 {
		t.Fatal("expected the write to grow the WAL before the trigger check")
	}

	s.checkWALTrigger()

	after, err := database.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes: %s", err)
	}

	if after >= before {
		t.Errorf("expected checkWALTrigger to checkpoint the WAL once over threshold, got %d (was %d)", after, before)
	}
}

// TestCheckWALTrigger_NoOpBelowThreshold verifies that checkWALTrigger leaves the WAL alone when
// it hasn't reached walTriggerBytes yet.
func TestCheckWALTrigger_NoOpBelowThreshold(t *testing.T) {
	s, database := walTestServer(t, 1<<30)

	body := make([]byte, 256*1024)
	if _, err := database.CreateMemory(types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	before, err := database.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes: %s", err)
	}

	s.checkWALTrigger()

	after, err := database.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes: %s", err)
	}

	if after < before {
		t.Errorf("expected checkWALTrigger to be a no-op below threshold, but the WAL shrank from %d to %d", before, after)
	}
}

// recordingStore wraps a real db.Store and timestamps the last call to CountMemories, which every
// consolidate() invokes once per cycle - a proxy for "a sleep cycle ran". Access is mutex-guarded
// because the autoSleep goroutine and the test read/write it concurrently.
type recordingStore struct {
	db.Store

	mu       sync.Mutex
	calls    int
	lastCall time.Time
}

func (r *recordingStore) CountMemories() (int, int) {
	r.mu.Lock()
	r.calls++
	r.lastCall = time.Now()
	r.mu.Unlock()

	return r.Store.CountMemories()
}

func (r *recordingStore) snapshot() (int, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls, r.lastCall
}

// TestStop_HaltsSleepBeforeClose is a regression test: Stop must halt the autoSleep
// loop and drain any in-flight cycle, so no store call lands after it returns and the database can
// be closed without a sleep cycle racing it. The server is built by hand (not New) against a tiny
// period, so the loop runs many cycles quickly.
func TestStop_HaltsSleepBeforeClose(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	rec := &recordingStore{Store: database}

	s := &Server{
		db:           rec,
		sleepReset:   make(chan bool, 1),
		stopSleep:    make(chan struct{}),
		sleepStopped: make(chan struct{}),
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	s.autoSleep(s.sleepReset, 5*time.Millisecond)

	// Let several cycles run so the loop is demonstrably active before we stop it.
	time.Sleep(60 * time.Millisecond)

	callsBefore, _ := rec.snapshot()
	if callsBefore == 0 {
		t.Fatal("autoSleep ran no cycles; the test cannot prove Stop halts it")
	}

	s.Stop()
	stopReturned := time.Now()

	// Give the loop several more tick intervals to (wrongly) fire again if Stop failed to halt it.
	time.Sleep(60 * time.Millisecond)

	callsAfter, lastCall := rec.snapshot()

	if lastCall.After(stopReturned) {
		t.Errorf("a store call landed %s after Stop returned; the sleep loop was not halted", lastCall.Sub(stopReturned))
	}

	if callsAfter != callsBefore {
		// Not necessarily fatal on its own (a cycle Stop waited for could bump the count before
		// Stop returned), but combined with the timestamp check above it pins the contract.
		if _, last := rec.snapshot(); last.After(stopReturned) {
			t.Errorf("store calls continued after Stop: %d before, %d after", callsBefore, callsAfter)
		}
	}
}

// TestStop_Idempotent verifies Stop can be called more than once (and on a server that never
// started autoSleep) without panicking on a double channel close.
func TestStop_Idempotent(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	s := &Server{
		db:           database,
		sleepReset:   make(chan bool, 1),
		stopSleep:    make(chan struct{}),
		sleepStopped: make(chan struct{}),
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	s.autoSleep(s.sleepReset, time.Hour)

	s.Stop()
	s.Stop() // second call must be a no-op, not a panic

	// A server built without New (stopSleep nil) must also tolerate Stop.
	(&Server{}).Stop()
}

// TestSleep_NonBlockingResetWhenBufferFull is a regression test: the Sleep RPC's
// nudge to the autoSleep timer must be a non-blocking send. With a blocking send, a full reset
// buffer (buffer size 1, e.g. autoSleep mid-cycle and not yet reading) would hang the RPC. Here
// nothing reads the channel and it is pre-filled, so a blocking send would deadlock.
func TestSleep_NonBlockingResetWhenBufferFull(t *testing.T) {
	s, _ := walTestServer(t, 0)

	s.sleepReset = make(chan bool, 1)
	s.sleepReset <- true // fill the buffer; no autoSleep goroutine is reading it

	done := make(chan struct{})

	go func() {
		_, _ = s.Sleep(context.Background(), &contract.EmptyRequest{})
		close(done)
	}()

	select {
	case <-done:
		// returned without blocking on the full reset channel

	case <-time.After(2 * time.Second):
		t.Fatal("Sleep blocked on a full sleepReset channel; the send must be non-blocking")
	}
}

// TestSleep_RejectedWhenConsolidationDisabled pins the replica contract: an instance with
// consolidation disabled must reject the manual Sleep RPC with FailedPrecondition and run no
// consolidation scan, since it does not hold the single-consolidator lock and would otherwise race
// the consolidating instance against shared data.
func TestSleep_RejectedWhenConsolidationDisabled(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	rec := &recordingStore{Store: database}

	s := &Server{
		db:                   rec,
		consolidationEnabled: false,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	res, err := s.Sleep(context.Background(), &contract.EmptyRequest{})

	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Sleep on a disabled instance: got %v, want FailedPrecondition", err)
	}

	if res.GetOk() {
		t.Error("Sleep reported Ok on a disabled instance")
	}

	if calls, _ := rec.snapshot(); calls != 0 {
		t.Errorf("Sleep ran %d consolidation scan(s) on a disabled instance; it must run none", calls)
	}
}

// TestNew_ConsolidationDisabledRunsNoTimedSleep verifies the New() wiring for the replica mode: with
// consolidation.enabled false, New must drop the timed sleep case even when sleep.periodSeconds is
// set, so no cycle ever fires. The period is a single second, so a consolidating instance would run
// a cycle well within the wait window; a replica must run none.
func TestNew_ConsolidationDisabledRunsNoTimedSleep(t *testing.T) {
	viper.Set("consolidation.enabled", false)
	viper.Set("sleep.periodSeconds", 1)
	viper.Set("consolidation.method", 1)
	viper.Set("consolidation.aggressiveness", 1.0)
	viper.Set("consolidation.unitsOfAgeInDays", 1.0)
	viper.Set("consolidation.deletionThreshold", 1.0)

	t.Cleanup(func() {
		viper.Set("consolidation.enabled", true)
		viper.Set("sleep.periodSeconds", 0)
	})

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	rec := &recordingStore{Store: database}

	s := New(rec, nil, nil)
	t.Cleanup(s.Stop)

	if s.consolidationEnabled {
		t.Fatal("consolidationEnabled should be false when consolidation.enabled is false")
	}

	time.Sleep(1300 * time.Millisecond)

	if calls, _ := rec.snapshot(); calls != 0 {
		t.Errorf("a disabled instance ran %d timed sleep cycle(s); it must run none", calls)
	}

	if _, err := s.Sleep(context.Background(), &contract.EmptyRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Sleep on a disabled instance: got %v, want FailedPrecondition", err)
	}
}
