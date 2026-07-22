package main

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

func newTestGenerator(cfg Config, client contract.HippocampusClient) *Generator {
	if cfg.Seed == 0 {
		cfg.Seed = 1
	}

	return New(cfg, client, newLatencyTracker())
}

func TestNew(t *testing.T) {
	client := newFakeHippoClient()
	g := New(Config{Seed: 1, MaxBytes: 100}, client, newLatencyTracker())

	if g.reg == nil || g.budget == nil || g.limiter == nil || g.lat == nil {
		t.Fatal("New() left a required field nil")
	}
}

func TestStoreEventSuccessAndError(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	id, err := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	if err != nil || id == "" {
		t.Fatalf("storeEvent() = (%q, %v), want a non-empty id and no error", id, err)
	}

	if got := g.eventsStored.Load(); got != 1 {
		t.Errorf("eventsStored = %d, want 1", got)
	}

	client.errOn["StoreEvent"] = 1

	if _, err := g.storeEvent(context.Background(), &contract.Event{Name: "n"}); err == nil {
		t.Error("storeEvent() expected an error")
	}
}

func TestStoreMemoryVariants(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	rng := rand.New(rand.NewSource(1))

	// Run enough times to exercise both the text and (rarely) binary body branches.
	for i := 0; i < 50; i++ {
		g.storeMemory(context.Background(), rng, &contract.Memory{})
	}

	if g.memoriesStored.Load() == 0 {
		t.Error("expected at least one memory stored")
	}

	if g.bytesStored.Load() == 0 {
		t.Error("expected bytesStored to be non-zero")
	}
}

func TestStoreMemoryBodyBytesOverride(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{BodyBytes: 1000}, client)

	rng := rand.New(rand.NewSource(1))
	g.storeMemory(context.Background(), rng, &contract.Memory{})

	if g.memoriesStored.Load() != 1 {
		t.Errorf("memoriesStored = %d, want 1", g.memoriesStored.Load())
	}
}

func TestStoreMemoryError(t *testing.T) {
	client := newFakeHippoClient()
	client.errOn["StoreMemory"] = 1
	g := newTestGenerator(Config{}, client)

	rng := rand.New(rand.NewSource(1))
	g.storeMemory(context.Background(), rng, &contract.Memory{})

	if g.memoriesStored.Load() != 0 {
		t.Errorf("memoriesStored = %d, want 0 on error", g.memoriesStored.Load())
	}
}

func TestStoreMemoryLimiterBlocksOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{TargetBytesPerSec: 1}, client)
	// Drain the limiter so wait() has to actually block.
	g.limiter.tokens = 0

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	rng := rand.New(rand.NewSource(1))
	g.storeMemory(ctx, rng, &contract.Memory{})

	if g.memoriesStored.Load() != 0 {
		t.Errorf("memoriesStored = %d, want 0 when the limiter never admits the write", g.memoriesStored.Load())
	}
}

func TestEndEvent(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.endEvent(context.Background(), id, time.Now().UnixNano())

	client.errOn["EndEvent"] = 1
	g.endEvent(context.Background(), id, time.Now().UnixNano()) // just exercise the error log path
}

func TestQueryEvents(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	if _, err := g.storeEvent(context.Background(), &contract.Event{Name: "n"}); err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		g.queryEvents(context.Background(), rng)
	}

	if g.queriesRun.Load() == 0 {
		t.Error("expected at least one successful query")
	}

	client.errOn["GetEvents"] = 1
	g.queryEvents(context.Background(), rng)
}

func TestQueryMemories(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	rng := rand.New(rand.NewSource(1))
	g.storeMemory(context.Background(), rng, &contract.Memory{})

	for i := 0; i < 10; i++ {
		g.queryMemories(context.Background(), rng)
	}

	if g.queriesRun.Load() == 0 {
		t.Error("expected at least one successful query")
	}

	client.errOn["GetMemories"] = 1
	g.queryMemories(context.Background(), rng)
}

func TestQueryEventByID(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	// No known event ids: early return.
	g.queryEventByID(context.Background(), rng)

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id)
	client.memories["m1"] = &contract.Memory{Id: "m1", EventId: id}

	g.queryEventByID(context.Background(), rng)
	if g.queriesRun.Load() != 1 {
		t.Errorf("queriesRun = %d, want 1", g.queriesRun.Load())
	}

	// Remove the event from the fake store but keep it registered: lookup fails and it should be
	// pruned from the registry.
	delete(client.events, id)
	g.queryEventByID(context.Background(), rng)

	if _, ok := g.reg.randomEventID(rng); ok {
		t.Error("expected the stale event id to be pruned from the registry")
	}
}

func TestRecallMemories(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	// No known memory ids: early return, no RPC made.
	g.recallMemories(context.Background(), rng)
	if client.callCount("RecallMemories") != 0 {
		t.Error("expected no RecallMemories call with an empty registry")
	}

	for i := 0; i < 5; i++ {
		g.storeMemory(context.Background(), rng, &contract.Memory{})
	}

	g.recallMemories(context.Background(), rng)
	if g.recallsRun.Load() == 0 {
		t.Error("expected at least one recall")
	}

	// Register an id the fake store doesn't know about, forcing the "gone" pruning path.
	g.reg.addMemoryIDs([]string{"does-not-exist"})
	for i := 0; i < 20; i++ {
		g.recallMemories(context.Background(), rng)
	}

	client.errOn["RecallMemories"] = 1
	g.recallMemories(context.Background(), rng)
}

func TestUpdateEventSignificance(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	g.updateEventSignificance(context.Background(), rng) // no ids registered

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id)

	g.updateEventSignificance(context.Background(), rng)

	client.errOn["UpdateEventSignificance"] = 1
	g.updateEventSignificance(context.Background(), rng)
}

func TestEndRandomEvent(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	g.endRandomEvent(context.Background(), rng) // no ids registered

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id)
	g.endRandomEvent(context.Background(), rng)
}

func TestMergeEvents(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	g.mergeEvents(context.Background(), rng) // fewer than 2 ids registered

	a, _ := g.storeEvent(context.Background(), &contract.Event{Name: "a"})
	b, _ := g.storeEvent(context.Background(), &contract.Event{Name: "b"})
	g.reg.addEventID(a)
	g.reg.addEventID(b)

	for i := 0; i < 20; i++ {
		g.mergeEvents(context.Background(), rng)
	}
}

func TestMergeEventsError(t *testing.T) {
	client := newFakeHippoClient()
	client.errOn["MergeEvents"] = 1000 // a failed merge never removes an id, so this never runs dry
	g := newTestGenerator(Config{}, client)

	a, _ := g.storeEvent(context.Background(), &contract.Event{Name: "a"})
	b, _ := g.storeEvent(context.Background(), &contract.Event{Name: "b"})
	g.reg.addEventID(a)
	g.reg.addEventID(b)

	for seed := int64(0); seed < 30; seed++ {
		g.mergeEvents(context.Background(), rand.New(rand.NewSource(seed)))
	}

	if client.callCount("MergeEvents") == 0 {
		t.Fatal("expected at least one MergeEvents call across 30 seeds")
	}
}

func TestDeleteMemories(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	g.deleteMemories(context.Background(), rng) // no ids registered

	g.storeMemory(context.Background(), rng, &contract.Memory{})
	g.deleteMemories(context.Background(), rng)

	g.storeMemory(context.Background(), rng, &contract.Memory{})
	client.errOn["DeleteMemories"] = 1
	g.deleteMemories(context.Background(), rng)
}

func TestDeleteEvent(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	rng := rand.New(rand.NewSource(1))

	g.deleteEvent(context.Background(), rng) // no ids registered

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id)
	g.deleteEvent(context.Background(), rng)

	id2, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id2)
	client.errOn["DeleteEvent"] = 1
	g.deleteEvent(context.Background(), rng)
}

func TestRequestSleep(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	g.requestSleep(context.Background())

	client.errOn["Sleep"] = 1
	g.requestSleep(context.Background())
}

func TestPaceIdle(t *testing.T) {
	client := newFakeHippoClient()
	rng := rand.New(rand.NewSource(1))

	g := newTestGenerator(Config{}, client)
	if !g.paceIdle(context.Background(), rng, time.Millisecond, 2*time.Millisecond) {
		t.Error("paceIdle() should return true when the sleep completes and ctx is live")
	}

	limited := newTestGenerator(Config{TargetBytesPerSec: 1000}, client)
	if !limited.paceIdle(context.Background(), rng, time.Hour, time.Hour) {
		t.Error("paceIdle() with a limiter enabled should return immediately")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if limited.paceIdle(ctx, rng, time.Hour, time.Hour) {
		t.Error("paceIdle() with a limiter enabled but a dead context should return false")
	}
}

func TestBurst(t *testing.T) {
	client := newFakeHippoClient()
	// A high throughput target makes paceIdle a no-op, so burst() runs to completion instantly.
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)

	for seed := int64(0); seed < 10; seed++ {
		rng := rand.New(rand.NewSource(seed))
		g.burst(context.Background(), rng)
	}

	if g.eventsStored.Load() == 0 || g.memoriesStored.Load() == 0 {
		t.Error("expected burst() to store at least one event and memory")
	}
}

func TestBurstContextCancelledBreaksLoop(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rng := rand.New(rand.NewSource(1))
	g.burst(ctx, rng) // storeEvent succeeds even against a cancelled ctx (the fake ignores it);
	// the per-memory loop should break immediately on ctx.Err() != nil.

	if g.eventsStored.Load() != 1 {
		t.Errorf("eventsStored = %d, want 1", g.eventsStored.Load())
	}

	if g.memoriesStored.Load() != 0 {
		t.Errorf("memoriesStored = %d, want 0 when the loop breaks on a cancelled context", g.memoriesStored.Load())
	}
}

func TestBurstStoreEventFails(t *testing.T) {
	client := newFakeHippoClient()
	client.errOn["StoreEvent"] = 1
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)

	rng := rand.New(rand.NewSource(1))
	g.burst(context.Background(), rng) // should return immediately after the failed StoreEvent

	if g.memoriesStored.Load() != 0 {
		t.Error("expected no memories stored when StoreEvent fails")
	}
}

func TestBurstyWorkerStopsOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.burstyWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("burstyWorker() did not stop after context cancellation")

	}
}

func TestBurstyWorkerPausedContinue(t *testing.T) {
	client := newFakeHippoClient()
	// A high throughput target makes paceIdle a no-op regardless of pacing, so the loop spins on
	// the paused check without ever waiting on a real sleep.
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)
	g.budget.pausedFlag.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.burstyWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("burstyWorker() did not stop after context cancellation")

	}

	if g.eventsStored.Load() != 0 {
		t.Error("expected no bursts to run while the budget is paused")
	}
}

func TestBurstyWorkerPaceIdleReturnsFalse(t *testing.T) {
	client := newFakeHippoClient()
	// No throughput target: paceIdle falls through to sleepFor with a 5-45s minimum, which will
	// select on ctx.Done() well before the timer fires given the short-lived context below.
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.burstyWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("burstyWorker() did not stop after paceIdle observed context cancellation")

	}
}

func TestSlowEvent(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	rng := rand.New(rand.NewSource(1))
	g.slowEvent(ctx, rng)

	if g.eventsStored.Load() == 0 {
		t.Error("expected slowEvent() to store an event before the context expired")
	}
}

func TestSlowEventStoreEventFails(t *testing.T) {
	client := newFakeHippoClient()
	client.errOn["StoreEvent"] = 1
	g := newTestGenerator(Config{}, client)

	rng := rand.New(rand.NewSource(1))
	g.slowEvent(context.Background(), rng)
}

func TestSlowEventCtxCancelledBeforeLoop(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rng := rand.New(rand.NewSource(1))
	// storeEvent succeeds even against a cancelled ctx (the fake ignores it); the loop's own
	// ctx.Err() check should then return immediately, before ever calling storeMemory.
	g.slowEvent(ctx, rng)

	if g.eventsStored.Load() != 1 {
		t.Errorf("eventsStored = %d, want 1", g.eventsStored.Load())
	}

	if g.memoriesStored.Load() != 0 {
		t.Errorf("memoriesStored = %d, want 0 when the loop returns on a cancelled context", g.memoriesStored.Load())
	}
}

func TestSlowEventCompletesAndEndsEvent(t *testing.T) {
	origMin, origMax := slowEventMinDuration, slowEventMaxDuration
	origSleepMin, origSleepMax := slowEventSleepMin, slowEventSleepMax
	slowEventMinDuration, slowEventMaxDuration = time.Millisecond, 2*time.Millisecond
	slowEventSleepMin, slowEventSleepMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() {
		slowEventMinDuration, slowEventMaxDuration = origMin, origMax
		slowEventSleepMin, slowEventSleepMax = origSleepMin, origSleepMax
	})

	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	rng := rand.New(rand.NewSource(1))
	g.slowEvent(context.Background(), rng)

	if g.eventsStored.Load() != 1 {
		t.Errorf("eventsStored = %d, want 1", g.eventsStored.Load())
	}

	if client.callCount("EndEvent") != 1 {
		t.Errorf("EndEvent calls = %d, want 1 once slowEvent's deadline elapses", client.callCount("EndEvent"))
	}
}

func TestSlowWorkerStopsOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.slowWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("slowWorker() did not stop after context cancellation")

	}
}

func TestSlowWorkerRunsAnIteration(t *testing.T) {
	origIdleMin, origIdleMax := slowWorkerIdleMin, slowWorkerIdleMax
	origEventMin, origEventMax := slowEventMinDuration, slowEventMaxDuration
	slowWorkerIdleMin, slowWorkerIdleMax = time.Millisecond, 2*time.Millisecond
	slowEventMinDuration, slowEventMaxDuration = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() {
		slowWorkerIdleMin, slowWorkerIdleMax = origIdleMin, origIdleMax
		slowEventMinDuration, slowEventMaxDuration = origEventMin, origEventMax
	})

	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.slowWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("slowWorker() did not stop after context cancellation")

	}

	if g.eventsStored.Load() == 0 {
		t.Error("expected slowWorker() to have run at least one slowEvent() iteration")
	}
}

func TestSlowWorkerPausedContinue(t *testing.T) {
	origIdleMin, origIdleMax := slowWorkerIdleMin, slowWorkerIdleMax
	slowWorkerIdleMin, slowWorkerIdleMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { slowWorkerIdleMin, slowWorkerIdleMax = origIdleMin, origIdleMax })

	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)
	g.budget.pausedFlag.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.slowWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("slowWorker() did not stop after context cancellation")

	}

	if g.eventsStored.Load() != 0 {
		t.Error("expected no slowEvent() to run while the budget is paused")
	}
}

func TestLooseWorkerStopsOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.looseWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("looseWorker() did not stop after context cancellation")

	}

	if g.memoriesStored.Load() == 0 {
		t.Error("expected looseWorker() to store at least one memory with the limiter bypassing idle sleeps")
	}
}

func TestLooseWorkerPausedContinue(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{TargetBytesPerSec: 1 << 40}, client)
	g.budget.pausedFlag.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.looseWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("looseWorker() did not stop after context cancellation")

	}

	if g.memoriesStored.Load() != 0 {
		t.Error("expected no memories stored while the budget is paused")
	}
}

func TestQueryWorkerRunsAnIteration(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out queryWorker's real 2-6s pacing sleep")
	}

	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id)

	// queryWorker's pacing sleep has a real 2s floor; give it enough headroom to complete at least
	// one iteration before cancelling, so the queryIteration call site itself gets covered (not
	// just queryIteration in isolation).
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.queryWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(10 * time.Second):
		t.Fatal("queryWorker() did not stop after context cancellation")

	}
}

func TestQueryIterationAllBranches(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	id, _ := g.storeEvent(context.Background(), &contract.Event{Name: "n"})
	g.reg.addEventID(id)
	g.storeMemory(context.Background(), rand.New(rand.NewSource(1)), &contract.Memory{})

	// Sweep many seeds so every rng.Intn(5) branch in queryIteration executes at least once.
	for seed := int64(0); seed < 100; seed++ {
		g.queryIteration(context.Background(), rand.New(rand.NewSource(seed)))
	}
}

func TestQueryWorkerStopsOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		g.queryWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("queryWorker() did not stop with an already-cancelled context")

	}
}

func TestMutatorIterationAllBranches(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	a, _ := g.storeEvent(context.Background(), &contract.Event{Name: "a"})
	b, _ := g.storeEvent(context.Background(), &contract.Event{Name: "b"})
	g.reg.addEventID(a)
	g.reg.addEventID(b)
	g.storeMemory(context.Background(), rand.New(rand.NewSource(1)), &contract.Memory{})

	// Sweep many seeds so every rng.Intn(10) branch in mutatorIteration executes at least once.
	for seed := int64(0); seed < 200; seed++ {
		g.mutatorIteration(context.Background(), rand.New(rand.NewSource(seed)))
	}
}

func TestMutatorWorkerStopsOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		g.mutatorWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("mutatorWorker() did not stop with an already-cancelled context")

	}
}

func TestMutatorWorkerRunsAnIteration(t *testing.T) {
	origMin, origMax := mutatorWorkerIdleMin, mutatorWorkerIdleMax
	mutatorWorkerIdleMin, mutatorWorkerIdleMax = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { mutatorWorkerIdleMin, mutatorWorkerIdleMax = origMin, origMax })

	client := newFakeHippoClient()
	g := newTestGenerator(Config{}, client)

	a, _ := g.storeEvent(context.Background(), &contract.Event{Name: "a"})
	b, _ := g.storeEvent(context.Background(), &contract.Event{Name: "b"})
	g.reg.addEventID(a)
	g.reg.addEventID(b)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.mutatorWorker(ctx, 1)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("mutatorWorker() did not stop after context cancellation")

	}

	// Any of mutatorIteration's RPCs count as evidence the loop body ran at least once; over a
	// handful of iterations with two registered events, at least one lands on a real call.
	total := client.callCount("UpdateEventSignificance") + client.callCount("EndEvent") +
		client.callCount("MergeEvents") + client.callCount("DeleteMemories") +
		client.callCount("DeleteEvent") + client.callCount("RecallMemories") +
		client.callCount("Sleep")
	if total == 0 {
		t.Error("expected mutatorWorker() to have run at least one mutatorIteration()")
	}
}

func TestLogStats(t *testing.T) {
	client := newFakeHippoClient()
	g := newTestGenerator(Config{MaxBytes: 1000}, client)

	rng := rand.New(rand.NewSource(1))
	g.storeMemory(context.Background(), rng, &contract.Memory{})

	// logStats' ticker is a fixed 30s, too long to observe a tick in a unit test; just confirm it
	// starts and stops cleanly around context cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.logStats(ctx)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("logStats() did not stop after context cancellation")

	}
}

func TestLogStatsTicks(t *testing.T) {
	orig := logStatsInterval
	logStatsInterval = 5 * time.Millisecond
	t.Cleanup(func() { logStatsInterval = orig })

	client := newFakeHippoClient()
	g := newTestGenerator(Config{MaxBytes: 1000}, client)

	rng := rand.New(rand.NewSource(1))
	g.storeMemory(context.Background(), rng, &contract.Memory{})

	// Seed every latency class so drain()'s per-class loop finds a summary for each on the first
	// tick, covering both the "found" and (once drained) "not found on a later tick" branches.
	for _, class := range latencyClasses {
		g.lat.observe(class, time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.logStats(ctx)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("logStats() did not stop after context cancellation")

	}
}

func TestRunStopsOnCancel(t *testing.T) {
	client := newFakeHippoClient()
	g := New(Config{
		Seed:              1,
		TargetBytesPerSec: 1 << 40, // bypass idle pacing so bursty/loose workers do real work fast
		BurstyWorkers:     1,
		SlowWorkers:       0,
		LooseWorkers:      1,
		QueryWorkers:      1,
		MutatorWorkers:    1,
		SampleInterval:    5 * time.Millisecond,
	}, client, newLatencyTracker())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.Run(ctx)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after context cancellation and worker drain")

	}
}
