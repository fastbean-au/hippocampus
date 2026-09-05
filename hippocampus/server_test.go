package hippocampus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
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

// TestPurgeInProgress_ConcurrentAccess drives concurrent Purge calls and interceptor checks at
// the race detector. purgeInProgress is written by Purge and read by
// InterceptorBlockWhenPurgeInProgress from every RPC's own goroutine; before it became an
// atomic.Bool this was an unsynchronised read/write of a plain bool across goroutines.
func TestPurgeInProgress_ConcurrentAccess(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	defer func() { _ = database.Close() }()

	s := &Server{db: database}

	info := &grpc.UnaryServerInfo{FullMethod: "/hippocampus.v1.Hippocampus/GetEvents"}
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
	if _, err := database.CreateMemory(context.Background(), types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
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
	if _, err := database.CreateMemory(context.Background(), types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
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

func (r *recordingStore) CountMemories(ctx context.Context) (int, int) {
	r.mu.Lock()
	r.calls++
	r.lastCall = time.Now()
	r.mu.Unlock()

	return r.Store.CountMemories(ctx)
}

func (r *recordingStore) snapshot() (int, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls, r.lastCall
}

// WALBytes is overridden to a fixed small value so checkWALTrigger is deterministic in tests: with
// a high walTriggerBytes it never triggers a WAL-driven cycle, isolating the timed cycle.
func (r *recordingStore) WALBytes() (int64, error) {
	return 0, nil
}

// TestAutoSleep_TimedCycleFiresWithWALTriggerEnabled is a regression test: enabling
// consolidation.walTriggerBytes must not disable the timed sleep cycle. autoSleep used to recreate
// the period timer (time.After) on every loop iteration, so the walCheck ticker - firing every
// walCheckInterval, more often than the period - restarted the countdown before it could elapse and
// the timed cycle never fired. Here walTriggerBytes is so high the WAL never triggers a cycle, so
// the only way CountMemories runs is the timer firing.
func TestAutoSleep_TimedCycleFiresWithWALTriggerEnabled(t *testing.T) {
	// Poll the WAL far more often than the sleep period, the condition that exposed the bug.
	orig := walCheckInterval
	walCheckInterval = 5 * time.Millisecond
	t.Cleanup(func() { walCheckInterval = orig })

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
			walTriggerBytes:   math.MaxInt64,
		},
	}

	s.autoSleep(s.sleepReset, 40*time.Millisecond)
	t.Cleanup(s.Stop)

	// Several periods elapse; with the bug the 5 ms WAL poll keeps restarting the 40 ms timer.
	time.Sleep(300 * time.Millisecond)

	if calls, _ := rec.snapshot(); calls == 0 {
		t.Fatal("no timed sleep cycle ran with walTriggerBytes enabled: the WAL poll starved the period timer")
	}
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

	s := New(Dependencies{DB: rec})
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

// TestMapError_ContextErrors verifies mapError's context-cancellation branches directly: a
// wrapped context.Canceled/context.DeadlineExceeded must map to the matching gRPC code rather than
// falling through to the generic codes.Internal masking.
func TestMapError_ContextErrors(t *testing.T) {
	if got := status.Code(mapError(fmt.Errorf("query: %w", context.Canceled))); got != codes.Canceled {
		t.Errorf("expected codes.Canceled, got %s", got)
	}

	if got := status.Code(mapError(fmt.Errorf("query: %w", context.DeadlineExceeded))); got != codes.DeadlineExceeded {
		t.Errorf("expected codes.DeadlineExceeded, got %s", got)
	}
}

// TestMapWriteError_NilAndPassthrough verifies mapWriteError's own branches directly: a nil error
// stays nil, a write conflict maps to a retryable Aborted carrying the original message (unlike
// mapError, which masks it), and any other error is returned unchanged so an admin retrying a
// failed Clear sees the real cause.
func TestMapWriteError_NilAndPassthrough(t *testing.T) {
	if err := mapWriteError(nil); err != nil {
		t.Errorf("expected nil to stay nil, got %v", err)
	}

	wrapped := fmt.Errorf("clear: %w", db.ErrWriteConflict)
	if got := status.Code(mapWriteError(wrapped)); got != codes.Aborted {
		t.Errorf("expected codes.Aborted for a write conflict, got %s", got)
	}

	other := errors.New("manifest gone missing")
	if got := mapWriteError(other); got != other {
		t.Errorf("expected the original error unchanged, got %v", got)
	}
}

// failWALBytesStore wraps a real db.Store but forces WALBytes to fail, so checkWALTrigger's error
// arm can be exercised without a broken database.
type failWALBytesStore struct {
	db.Store
	err error
}

func (f failWALBytesStore) WALBytes() (int64, error) {
	return 0, f.err
}

// countingSleepStore wraps a real db.Store and counts CountMemories calls - a proxy for "a sleep
// cycle ran" - without shadowing WALBytes the way recordingStore deliberately does, so it composes
// with a fault store that overrides WALBytes underneath it.
type countingSleepStore struct {
	db.Store

	mu    sync.Mutex
	calls int
}

func (c *countingSleepStore) CountMemories(ctx context.Context) (int, int) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	return c.Store.CountMemories(ctx)
}

func (c *countingSleepStore) snapshot() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

// TestCheckWALTrigger_WALBytesErrorIsNoOp verifies a failure reading the WAL size is logged and
// otherwise ignored - it must not panic or run a spurious sleep cycle.
func TestCheckWALTrigger_WALBytesErrorIsNoOp(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	rec := &countingSleepStore{Store: failWALBytesStore{Store: database, err: errors.New("stat failed")}}

	s := &Server{
		db: rec,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
			walTriggerBytes:   1,
		},
	}

	s.checkWALTrigger()

	if calls := rec.snapshot(); calls != 0 {
		t.Errorf("expected no sleep cycle when the WAL size read fails, got %d", calls)
	}
}

// TestSleep_ResetSentWhenBufferHasRoom verifies the Sleep RPC's non-blocking nudge actually sends
// on the reset channel when there is room (the complementary case to
// TestSleep_NonBlockingResetWhenBufferFull, which only proves the full-buffer case doesn't block).
func TestSleep_ResetSentWhenBufferHasRoom(t *testing.T) {
	s, _ := walTestServer(t, 0)
	s.sleepReset = make(chan bool, 1)

	if _, err := s.Sleep(context.Background(), &contract.EmptyRequest{}); err != nil {
		t.Fatalf("Sleep: %s", err)
	}

	select {

	case v := <-s.sleepReset:
		if !v {
			t.Error("expected true sent on the reset channel")
		}

	default:
		t.Error("expected Sleep to send a reset signal onto a non-full buffer")
	}
}

// TestAutoSleep_ManualResetWithTimedSleepDisabled is a regression-style check for resetTimer's
// nil-timer guard: with the timed cycle disabled (period <= 0, e.g. a WAL-trigger-only or purely
// RPC-driven instance), a reset signal must still be handled - resetTimer must return immediately
// rather than dereferencing the nil timer. If it ever blocked or panicked, the reset channel would
// never drain and this test would time out.
func TestAutoSleep_ManualResetWithTimedSleepDisabled(t *testing.T) {
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

	s.autoSleep(s.sleepReset, 0)
	t.Cleanup(s.Stop)

	s.sleepReset <- true

	// If the reset case (and resetTimer's nil-timer branch inside it) processed cleanly, the
	// channel is drained and a second send succeeds well before the timeout.
	select {

	case s.sleepReset <- true:

	case <-time.After(2 * time.Second):
		t.Fatal("autoSleep did not drain the reset signal with the timed cycle disabled")
	}
}

// TestNew_StartsAndStopsReconcile verifies New's reconcile wiring end to end: with consolidation
// enabled, an enabled search index, and a positive reconcileIntervalSeconds, it must launch the
// sweep goroutine (stopReconcile/reconcileStopped non-nil), and Stop must drain it promptly rather
// than hanging.
func TestNew_StartsAndStopsReconcile(t *testing.T) {
	viper.Set("consolidation.enabled", true)
	viper.Set("opensearch.reconcileIntervalSeconds", 3600)
	viper.Set("sleep.periodSeconds", 0)
	viper.Set("consolidation.method", 1)
	viper.Set("consolidation.aggressiveness", 1.0)
	viper.Set("consolidation.unitsOfAgeInDays", 1.0)
	viper.Set("consolidation.deletionThreshold", 1.0)

	t.Cleanup(func() {
		viper.Set("opensearch.reconcileIntervalSeconds", 0)
		viper.Set("sleep.periodSeconds", 0)
	})

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	idx := &fakeIndex{enabled: true}

	s := New(Dependencies{DB: database, Search: idx})

	if s.stopReconcile == nil || s.reconcileStopped == nil {
		t.Fatal("expected startReconcile to launch the reconciliation sweep goroutine")
	}

	done := make(chan struct{})

	go func() {
		s.Stop()
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return promptly; the reconcile goroutine may not have drained")
	}
}

// TestNew_TransferTokenWithoutTLSWarns verifies New logs a warning when transfer.token is
// configured without transfer.tls, mirroring the server-side auth-without-TLS warning - a
// plaintext bearer token sent to the transfer target is a real exposure worth flagging.
func TestNew_TransferTokenWithoutTLSWarns(t *testing.T) {
	var buf bytes.Buffer

	restoreOutput := log.StandardLogger().Out
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(restoreOutput) })

	viper.Set("transfer.token", "secret-token")
	viper.Set("sleep.periodSeconds", 0)
	viper.Set("consolidation.method", 1)
	viper.Set("consolidation.aggressiveness", 1.0)
	viper.Set("consolidation.unitsOfAgeInDays", 1.0)
	viper.Set("consolidation.deletionThreshold", 1.0)

	t.Cleanup(func() {
		viper.Set("transfer.token", "")
		viper.Set("sleep.periodSeconds", 0)
	})

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	s := New(Dependencies{DB: database})
	t.Cleanup(s.Stop)

	if !strings.Contains(buf.String(), "transfer.token is configured without transfer.tls") {
		t.Errorf("expected a warning about the plaintext bearer token, got log output: %s", buf.String())
	}
}

// failPurgeStore wraps a real db.Store but forces Purge to fail, so the Purge RPC's error-mapping
// branch can be exercised without a broken database.
type failPurgeStore struct {
	db.Store
	err error
}

func (f failPurgeStore) Purge(ctx context.Context) error {
	return f.err
}

// TestPurge_ErrorMapped verifies a generic Purge failure is mapped via mapError rather than
// returned raw, and purgeInProgress is still cleared afterwards so subsequent RPCs are not blocked
// forever.
func TestPurge_ErrorMapped(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	wantErr := errors.New("purge boom")

	s := &Server{db: failPurgeStore{Store: database, err: wantErr}}

	if _, err := s.Purge(context.Background(), &contract.EmptyRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}

	if s.purgeInProgress.Load() {
		t.Error("expected purgeInProgress cleared after a failed Purge")
	}
}

// TestServicePrefixMatchesDescriptor holds hippocampusServicePrefix to the generated service
// descriptor, mirroring the same guard in auth and cmd/hippocampus. A stale copy here fails open:
// the purge gate would stop recognising Hippocampus RPCs and serve them from a store that is being
// emptied underneath them.
func TestServicePrefixMatchesDescriptor(t *testing.T) {
	if want := "/" + contract.Hippocampus_ServiceDesc.ServiceName + "/"; hippocampusServicePrefix != want {
		t.Fatalf("hippocampusServicePrefix = %q, want %q", hippocampusServicePrefix, want)
	}
}

// TestLogForgettingMode covers the startup line that turns a configuration ABSENCE into a
// declaration. The two modes in docs/consolidation.md are configured by leaving a capacity target
// at zero, which is what made them indistinguishable from each other and from a store misconfigured
// into forgetting nothing at all - all three presented as silence. Each branch must therefore name
// the mode it is in, and the row-capacity one must say plainly that nothing is evicted on the row
// count, since "capacityMemories: 100000" reads like a cap and is not one.
func TestLogForgettingMode(t *testing.T) {
	tests := []struct {
		name          string
		consolidation Consolidation
		want          []string
		notWant       []string
	}{
		{
			name: "neither axis configured is decay-only",
			consolidation: Consolidation{
				deletionThreshold: 0.5,
				method:            1,
				aggressiveness:    1.0,
			},
			want: []string{"forgetting mode: decay-only", "threshold 0.5", "method 1", "hippocampus.used_bytes"},
		},
		{
			name: "a row capacity alone scales pressure and evicts nothing",
			consolidation: Consolidation{
				deletionThreshold: 0.5,
				capacityMemories:  100000,
			},
			want: []string{
				"forgetting mode: decay with row-capacity pressure",
				"consolidation.capacityMemories (100000)",
				"nothing is evicted on the row count",
				"set consolidation.capacityBytes",
			},
		},
		{
			name: "a capacity target with no floor says so rather than printing the same number twice",
			consolidation: Consolidation{
				deletionThreshold: 0.5,
				capacityBytes:     1024,
			},
			want:    []string{"forgetting mode: decay with a capacity target", "at or below 1024 bytes", "no hysteresis floor set"},
			notWant: []string{"reclaiming down to a floor"},
		},
		{
			name: "a floor below the target is reported as hysteresis headroom",
			consolidation: Consolidation{
				deletionThreshold:  0.5,
				capacityBytes:      1024,
				capacityBytesFloor: 512,
			},
			want:    []string{"forgetting mode: decay with a capacity target", "at or below 1024 bytes", "reclaiming down to a floor of 512"},
			notWant: []string{"no hysteresis floor set"},
		},
		{
			// evictionFloor() rejects a floor above the target, so this must fall back to the
			// no-floor wording rather than printing a floor eviction would never reclaim to.
			name: "a floor above the target falls back to the no-floor wording",
			consolidation: Consolidation{
				deletionThreshold:  0.5,
				capacityBytes:      1024,
				capacityBytesFloor: 4096,
			},
			want:    []string{"no hysteresis floor set"},
			notWant: []string{"reclaiming down to a floor"},
		},
		{
			// Both axes set: the byte target wins the branch, since it is the one that actually
			// bounds the store.
			name: "both axes configured reports the capacity target",
			consolidation: Consolidation{
				deletionThreshold: 0.5,
				capacityMemories:  100000,
				capacityBytes:     1024,
			},
			want:    []string{"forgetting mode: decay with a capacity target"},
			notWant: []string{"row-capacity pressure", "decay-only"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			restoreOutput := log.StandardLogger().Out
			restoreLevel := log.GetLevel()

			log.SetOutput(&buf)
			log.SetLevel(log.InfoLevel)

			t.Cleanup(func() {
				log.SetOutput(restoreOutput)
				log.SetLevel(restoreLevel)
			})

			s := &Server{consolidation: tt.consolidation}
			s.logForgettingMode()

			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("expected the log line to contain %q, got: %s", want, buf.String())
				}
			}

			for _, notWant := range tt.notWant {
				if strings.Contains(buf.String(), notWant) {
					t.Errorf("expected the log line NOT to contain %q, got: %s", notWant, buf.String())
				}
			}
		})
	}
}

// TestStopDrainsEveryWorker verifies Stop closes each optional worker's stop channel and WAITS for
// it to confirm, not merely signals it. That wait is the contract: the caller closes the database
// next, so a worker still running when Stop returns is one issuing queries against a closed store.
//
// Each case makes exactly one worker slow to confirm and the rest immediate, so a dropped wait is
// caught for that worker specifically - with every worker slow, the others' waits would mask it.
//
// The channels are wired directly rather than through startCallbacks/startOutbox, which need a
// notifier and a delete-syncing search backend respectively; what is under test here is Stop's
// drain, not their construction.
func TestStopDrainsEveryWorker(t *testing.T) {
	// Long enough that a Stop skipping this worker's wait returns well inside it, short enough not
	// to slow the suite.
	const confirmDelay = 300 * time.Millisecond

	workers := []string{"callbacks", "outbox", "reconcile", "sleep"}

	for i, slow := range workers {
		t.Run(slow, func(t *testing.T) {
			s := &Server{}

			s.stopCallbacks = make(chan struct{})
			s.callbacksStopped = make(chan struct{})
			s.stopOutbox = make(chan struct{})
			s.outboxStopped = make(chan struct{})
			s.stopReconcile = make(chan struct{})
			s.reconcileStopped = make(chan struct{})
			s.stopSleep = make(chan struct{})
			s.sleepStopped = make(chan struct{})

			channels := []struct {
				stop    chan struct{}
				stopped chan struct{}
			}{
				{s.stopCallbacks, s.callbacksStopped},
				{s.stopOutbox, s.outboxStopped},
				{s.stopReconcile, s.reconcileStopped},
				{s.stopSleep, s.sleepStopped},
			}

			for j, channel := range channels {
				go func() {
					<-channel.stop

					if j == i {
						time.Sleep(confirmDelay)
					}

					close(channel.stopped)
				}()
			}

			started := time.Now()
			done := make(chan struct{})

			go func() {
				s.Stop()
				close(done)
			}()

			select {

			case <-done:

			case <-time.After(5 * time.Second):
				t.Fatal("Stop did not return; a worker's confirmation was never awaited or never arrived")
			}

			if elapsed := time.Since(started); elapsed < confirmDelay {
				t.Errorf("Stop returned after %s without waiting for the %s worker to confirm", elapsed, slow)
			}

			for j, channel := range channels {
				select {

				case <-channel.stopped:

				default:
					t.Errorf("the %s worker had not confirmed when Stop returned", workers[j])
				}
			}

			// Safe to call more than once: stopOnce means the second call must not re-close a channel
			// it already closed, which would panic.
			s.Stop()
		})
	}
}

// TestStopIsANoOpWithoutWorkers verifies Stop on a Server built directly in a test - one that never
// ran New, so none of the optional workers were started - returns rather than blocking on a
// confirmation nothing will send.
func TestStopIsANoOpWithoutWorkers(t *testing.T) {
	s := &Server{}

	done := make(chan struct{})

	go func() {
		s.Stop()
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked on a worker that was never started")
	}
}
