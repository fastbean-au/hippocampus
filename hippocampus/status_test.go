package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// statusServer builds a consolidating Server over an in-memory store, with the decay knobs a cycle
// needs to actually delete something.
func statusServer(t *testing.T) *Server {
	t.Helper()

	s := newTestServer(t)
	s.consolidationEnabled = true
	s.sleepPeriod = time.Minute
	s.consolidation.method = 1
	s.consolidation.aggressiveness = 1
	s.consolidation.unitsOfAgeInDays = 1
	s.consolidation.deletionThreshold = 5
	s.consolidation.capacityPressure = 1

	return s
}

// seedDoomed stores n memories old enough and insignificant enough that a cycle will consolidate
// them all.
func seedDoomed(t *testing.T, s *Server, n int) {
	t.Helper()

	old := time.Now().Add(-1000 * 24 * time.Hour).UnixNano()

	for i := range n {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{
			Id:           "doomed-" + string(rune('a'+i)),
			Body:         "forgettable",
			TimeStamp:    old,
			Significance: 1,
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}
}

// TestSleepRecordsACycleReport verifies a completed cycle publishes what it did, so
// GetConsolidationStatus has something to report.
func TestSleepRecordsACycleReport(t *testing.T) {
	s := statusServer(t)
	seedDoomed(t, s, 3)

	if s.lastCycle.Load() != nil {
		t.Fatal("a report exists before any cycle has run")
	}

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	report := s.lastCycle.Load()
	if report == nil {
		t.Fatal("no cycle report was published")
	}

	if !report.success {
		t.Errorf("report says the cycle failed: %s", report.failure)
	}

	if report.trigger != triggerManual {
		t.Errorf("trigger = %q, want %q", report.trigger, triggerManual)
	}

	if report.memoriesConsolidated != 3 {
		t.Errorf("memoriesConsolidated = %d, want 3", report.memoriesConsolidated)
	}

	if report.startedAt.IsZero() {
		t.Error("startedAt was never set")
	}
}

// TestCycleReportCountsMatchTheDeletions is the drift guard between the report and the passes it
// describes. The counts are copied out of the passes beside the telemetry they already feed, so
// nothing stops them being copied from the wrong variable, added twice, or quietly not updated when
// a fourth pass is added - except this: everything the cycle removed must be accounted for by
// exactly one of the two decay paths it reports.
func TestCycleReportCountsMatchTheDeletions(t *testing.T) {
	s := statusServer(t)
	seedDoomed(t, s, 5)

	// CountMemories reports memories with and without an event; the cycle can remove either, so the
	// guard has to account for both.
	beforeWith, beforeWithout := s.db.CountMemories(context.Background())

	if err := s.sleep(triggerTimer); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	afterWith, afterWithout := s.db.CountMemories(context.Background())
	before := beforeWith + beforeWithout
	after := afterWith + afterWithout

	report := s.lastCycle.Load()
	if report == nil {
		t.Fatal("no cycle report was published")
	}

	removed := before - after
	reported := report.memoriesConsolidated + report.memoriesEvicted

	if reported != removed {
		t.Errorf(
			"the report accounts for %d memories (%d consolidated + %d evicted) but the store lost %d",
			reported, report.memoriesConsolidated, report.memoriesEvicted, removed,
		)
	}
}

// A failed cycle must still publish. "The last cycle deleted 40 and then failed" is the reading an
// operator needs; leaving the previous success standing would hide it.
func TestCycleReportPublishedOnFailure(t *testing.T) {
	s := statusServer(t)
	wantErr := errors.New("preserve exploded")
	s.db = failPreserveStore{Store: s.db, err: wantErr}

	if err := s.sleep(triggerTimer); err == nil {
		t.Fatal("expected the cycle to fail")
	}

	report := s.lastCycle.Load()
	if report == nil {
		t.Fatal("a failed cycle published no report")
	}

	if report.success {
		t.Error("report claims success after a failing cycle")
	}

	if report.failure == "" {
		t.Error("report carries no failure reason")
	}
}

// TestNextSleepIsRecordedAndResets verifies the countdown the console shows advances with the
// timer, from the one chokepoint every reset goes through.
func TestNextSleepIsRecordedAndResets(t *testing.T) {
	s := statusServer(t)
	s.stopSleep = make(chan struct{})
	s.sleepStopped = make(chan struct{})

	reset := make(chan bool, 1)
	s.sleepReset = reset

	s.autoSleep(reset, 50*time.Millisecond)
	t.Cleanup(s.Stop)

	// Set as soon as the timer is created, before anything has fired.
	first := s.nextSleep.Load()

	if first == 0 {
		t.Fatal("nextSleep was not set when the timer was created")
	}

	// After a cycle fires, the countdown must have been restarted rather than left in the past.
	time.Sleep(200 * time.Millisecond)

	second := s.nextSleep.Load()

	if second <= first {
		t.Errorf("nextSleep did not advance after a cycle fired: %d then %d", first, second)
	}

	if second < time.Now().UnixNano() {
		t.Error("nextSleep is in the past, so the countdown would render as overdue forever")
	}
}

// A non-positive sleep.periodSeconds is a supported mode - an instance driven only by the Sleep RPC
// or the WAL trigger. It must report no schedule rather than a countdown to something that will
// never fire.
func TestNextSleepIsZeroWhenTimedSleepDisabled(t *testing.T) {
	s := statusServer(t)
	s.sleepPeriod = 0
	s.stopSleep = make(chan struct{})
	s.sleepStopped = make(chan struct{})

	reset := make(chan bool, 1)
	s.sleepReset = reset

	s.autoSleep(reset, 0)
	t.Cleanup(s.Stop)

	time.Sleep(50 * time.Millisecond)

	if got := s.nextSleep.Load(); got != 0 {
		t.Errorf("nextSleep = %d with timed sleep disabled, want 0", got)
	}

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if res.GetNextSleepAt() != 0 || res.GetPeriodSeconds() != 0 {
		t.Errorf("status reports a schedule (%d, %ds) with timed sleep disabled", res.GetNextSleepAt(), res.GetPeriodSeconds())
	}
}

// TestGetConsolidationStatusOnAReplica pins the one way this RPC differs from its neighbours: it
// answers rather than refusing. Reporting consolidation_enabled false IS the answer - refusing
// would leave a client unable to tell a replica from a consolidator whose cycle had stopped, which
// is the distinction the RPC exists to make.
func TestGetConsolidationStatusOnAReplica(t *testing.T) {
	s := statusServer(t)
	s.consolidationEnabled = false
	s.sleepPeriod = time.Minute
	s.nextSleep.Store(time.Now().UnixNano())

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("a replica must answer rather than refuse: %s", err)
	}

	if res.GetConsolidationEnabled() {
		t.Error("a replica reported consolidation_enabled true")
	}

	// Everything else stays zero: a replica's store is consolidated by another instance under THAT
	// instance's configuration, so this one's schedule would describe a cycle it never runs.
	if res.GetPeriodSeconds() != 0 || res.GetNextSleepAt() != 0 || res.GetLastCycle() != nil {
		t.Errorf("a replica reported a schedule it does not run: %s", res.String())
	}
}

// TestGetConsolidationStatusReportsTheCycle covers the projection onto the wire.
func TestGetConsolidationStatusReportsTheCycle(t *testing.T) {
	s := statusServer(t)
	s.consolidation.walTriggerBytes = 1 << 20
	seedDoomed(t, s, 2)

	if err := s.sleep(triggerWAL); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if !res.GetConsolidationEnabled() {
		t.Error("consolidation_enabled false on a consolidator")
	}

	if res.GetPeriodSeconds() != 60 {
		t.Errorf("period_seconds = %d, want 60", res.GetPeriodSeconds())
	}

	if !res.GetWalTriggerEnabled() {
		t.Error("wal_trigger_enabled false with walTriggerBytes set")
	}

	// The console paces its ExplainConsolidation polling from this rather than guessing, so it must
	// be the real TTL.
	if res.GetSnapshotTtlSeconds() != int64(explainStateTTL.Seconds()) {
		t.Errorf("snapshot_ttl_seconds = %d, want %d", res.GetSnapshotTtlSeconds(), int64(explainStateTTL.Seconds()))
	}

	last := res.GetLastCycle()

	if last == nil {
		t.Fatal("no last_cycle reported after a cycle ran")
	}

	if last.GetTrigger() != triggerWAL {
		t.Errorf("trigger = %q, want %q", last.GetTrigger(), triggerWAL)
	}

	if last.GetMemoriesConsolidated() != 2 {
		t.Errorf("memories_consolidated = %d, want 2", last.GetMemoriesConsolidated())
	}

	if !last.GetSuccess() || last.GetFailure() != "" {
		t.Errorf("last cycle reported unsuccessful: %s", last.GetFailure())
	}

	if last.GetStartedAt() == 0 {
		t.Error("started_at is zero")
	}
}

// A caller that joins an in-flight cycle must see sleep_in_progress, not only the caller that
// started it - the flag is set inside the singleflight closure for exactly that reason.
func TestSleepInProgressIsSetForTheCycle(t *testing.T) {
	s := statusServer(t)

	if s.sleepInProgress.Load() {
		t.Fatal("sleep_in_progress is set before any cycle")
	}

	if err := s.sleepOnce(triggerManual); err != nil {
		t.Fatalf("sleepOnce: %s", err)
	}

	if s.sleepInProgress.Load() {
		t.Error("sleep_in_progress stayed set after the cycle finished")
	}
}
