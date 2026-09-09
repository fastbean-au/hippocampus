package hippocampus

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// The RPC layer's half of the external capacity axis: the third pressure term, the second eviction
// target, and the gate that keeps a deployment which has never heard of the axis from paying for it.

// TestCalculateCapacityPressure_ExternalUtilisation pins the third axis into the max(): a store
// small on both of its own axes but over the external one must still feel pressure, which is the
// whole point - a pointer-memory is two hundred bytes and the payload behind it is forty kilobytes.
func TestCalculateCapacityPressure_ExternalUtilisation(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			capacityMemories:         1000,
			capacityBytes:            1000000,
			capacityExternalBytes:    1000000000,
			capacityPressureExponent: 4.0,
		},
	}

	// Ten rows and a thousand bytes here; the payload elsewhere is at its capacity. External
	// utilisation (1.0) beats both of the others.
	if got := s.calculateCapacityPressure(10, 1000, 1000000000); got != 2.0 {
		t.Errorf("external at capacity: expected pressure 2.0, got %v", got)
	}

	// Over the external capacity, pressure keeps climbing past 2 exactly as the other axes do.
	if got := s.calculateCapacityPressure(10, 1000, 1500000000); got <= 2.0 {
		t.Errorf("external over capacity: expected pressure > 2.0, got %v", got)
	}

	// A fuller axis still wins, so adding the third term cannot lower a reading.
	if got := s.calculateCapacityPressure(1000, 1000, 1000); got != 2.0 {
		t.Errorf("rows at capacity: expected pressure 2.0, got %v", got)
	}

	// With the external capacity disabled, external bytes must not contribute at all - so an
	// absurd reading and no reading at all produce the same pressure, whatever the other two axes
	// are contributing.
	s.consolidation.capacityExternalBytes = 0

	if got, want := s.calculateCapacityPressure(10, 1000, 1<<62), s.calculateCapacityPressure(10, 1000, 0); got != want {
		t.Errorf("external capacity disabled: pressure moved from %v to %v on external bytes alone", want, got)
	}
}

// TestExternalEvictionFloor covers the second hysteresis floor, which reads exactly as the first
// does: honoured only when positive and actually below the target it provides headroom under.
func TestExternalEvictionFloor(t *testing.T) {
	s := &Server{consolidation: Consolidation{capacityExternalBytes: 1000}}

	tests := []struct {
		name  string
		floor int64
		want  int64
	}{
		{name: "a valid floor is the reclaim level", floor: 800, want: 800},
		{name: "unset falls back to the target", floor: 0, want: 1000},
		{name: "negative falls back to the target", floor: -1, want: 1000},
		{name: "above the target falls back to it", floor: 2000, want: 1000},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s.consolidation.capacityExternalBytesFloor = test.floor

			if got := s.externalEvictionFloor(); got != test.want {
				t.Errorf("externalEvictionFloor() = %d, want %d", got, test.want)
			}
		})
	}
}

// externalCountingStore records whether the external sum was taken, so the gate that keeps the extra
// aggregate scan off a deployment with no external capacity can be pinned.
type externalCountingStore struct {
	db.Store
	calls int
}

func (e *externalCountingStore) ExternalBytes(ctx context.Context) (int64, error) {
	e.calls++

	return e.Store.ExternalBytes(ctx)
}

// TestEvictOnlyMeasuresTheExternalAxisWhenConfigured is the cost gate. Unlike used bytes - which is
// measured in both forgetting modes because a decay-only store's size is the one figure nothing else
// reports - an unconfigured external axis has nothing to say: every memory's external size is 0 and
// no decision reads the sum, so the scan would buy a gauge nobody asked for.
func TestEvictOnlyMeasuresTheExternalAxisWhenConfigured(t *testing.T) {
	tests := []struct {
		name     string
		capacity int64
		want     bool
	}{
		{name: "unconfigured", capacity: 0, want: false},
		{name: "configured", capacity: 1 << 30, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, database := previewTestServer(t)
			s.consolidation.capacityExternalBytes = test.capacity

			counting := &externalCountingStore{Store: database}
			s.db = counting

			seedPreviewMemories(t, database, "m", 1, 1, 3)

			if err := s.evict(context.Background(), &cycleReport{}); err != nil {
				t.Fatalf("evict: %s", err)
			}

			if got := counting.calls > 0; got != test.want {
				t.Errorf("measured the external axis = %t, want %t", got, test.want)
			}
		})
	}
}

// TestEvictRunsOnTheExternalAxisAlone is the behaviour the axis exists to add: a store well under
// its own byte target, over the target for the payload it points at, evicting anyway.
func TestEvictRunsOnTheExternalAxisAlone(t *testing.T) {
	s, database := previewTestServer(t)

	// No byte capacity at all, so nothing about this store's own size can trigger the pass.
	s.consolidation.capacityBytes = 0
	s.consolidation.capacityExternalBytes = 5000

	for i, external := range []int64{4000, 4000, 4000} {
		if _, err := database.CreateMemory(context.Background(), types.Memory{
			Id:            string(rune('a' + i)),
			TimeStamp:     100,
			Significance:  int32(i + 1),
			Body:          "a pointer",
			ExternalBytes: external,
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	report := &cycleReport{}

	if err := s.evict(context.Background(), report); err != nil {
		t.Fatalf("evict: %s", err)
	}

	if report.memoriesEvicted == 0 {
		t.Fatal("expected eviction to run on the external axis alone")
	}

	if report.externalBytesFreed == 0 {
		t.Error("expected the cycle report to carry what was released on the external axis")
	}

	// 12,000 external bytes against a 5,000 target, reclaiming to the target itself: two memories
	// (8,000) is the first selection in ascending value order that gets there.
	if report.memoriesEvicted != 2 {
		t.Errorf("evicted %d memories, want 2", report.memoriesEvicted)
	}

	external, err := database.ExternalBytes(context.Background())
	if err != nil {
		t.Fatalf("ExternalBytes: %s", err)
	}

	if external > s.consolidation.capacityExternalBytes {
		t.Errorf("the store still points at %d external bytes, above its %d target",
			external, s.consolidation.capacityExternalBytes)
	}
}

// TestEvictDoesNotRunWhenNeitherAxisIsOver guards the other direction: a store inside both targets
// must delete nothing, so a configured external axis cannot make eviction unconditional.
func TestEvictDoesNotRunWhenNeitherAxisIsOver(t *testing.T) {
	s, database := previewTestServer(t)

	s.consolidation.capacityBytes = 1 << 30
	s.consolidation.capacityExternalBytes = 1 << 30

	seedPreviewMemories(t, database, "m", 1, 1, 3)

	report := &cycleReport{}

	if err := s.evict(context.Background(), report); err != nil {
		t.Fatalf("evict: %s", err)
	}

	if report.memoriesEvicted != 0 || report.externalBytesFreed != 0 {
		t.Errorf("a store inside both targets evicted %d memories and %d external bytes",
			report.memoriesEvicted, report.externalBytesFreed)
	}
}

// TestPreviewReportsTheExternalAxis covers the dry run's half: the figures a client reads to
// understand a pressure reading its own size does not account for.
func TestPreviewReportsTheExternalAxis(t *testing.T) {
	s, database := previewTestServer(t)

	s.consolidation.capacityExternalBytes = 5000

	for i, external := range []int64{4000, 4000, 4000} {
		if _, err := database.CreateMemory(context.Background(), types.Memory{
			Id:            string(rune('a' + i)),
			TimeStamp:     100,
			Significance:  int32(i + 1),
			Body:          "a pointer",
			ExternalBytes: external,
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	result, err := s.previewOnce(context.Background(), db.PreviewLimit(0))
	if err != nil {
		t.Fatalf("previewOnce: %s", err)
	}

	response := s.previewResponse(result)

	if response.GetExternalBytes() != 12000 {
		t.Errorf("preview reported %d external bytes, want 12000", response.GetExternalBytes())
	}

	if response.GetCapacityExternalBytes() != 5000 {
		t.Errorf("preview reported a %d external capacity, want 5000", response.GetCapacityExternalBytes())
	}

	if response.GetExternalBytesFreed() == 0 {
		t.Error("preview reported no external bytes freed though the axis is over its target")
	}

	// Every candidate carries the payload its deletion would release, which is the figure a client
	// needs to decide whether the far system is worth telling.
	for _, candidate := range response.GetCandidates() {
		if candidate.GetExternalBytes() != 4000 {
			t.Errorf("candidate %s reported %d external bytes, want 4000",
				candidate.GetId(), candidate.GetExternalBytes())
		}
	}
}

// failingExternalStore refuses to sum the external axis, so the two callers that read it under a
// configured target can be driven through their failure arms. Under a target the reading IS the
// decision - eviction cannot judge the axis without it - which is why both fail rather than carrying
// on with a zero, and why this needs its own fake rather than an unconfigured axis.
type failingExternalStore struct {
	db.Store
}

func (*failingExternalStore) ExternalBytes(context.Context) (int64, error) {
	return 0, errors.New("boom")
}

// TestSleepFailsWhenTheExternalAxisCannotBeMeasured pins that the cycle stops rather than evicting
// on an unread axis. A zero here would read as an empty external store and quietly switch the
// target off for that cycle, which is the worst of the available answers: eviction would stop
// reclaiming exactly when it was most needed and nothing would say so.
func TestSleepFailsWhenTheExternalAxisCannotBeMeasured(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.capacityExternalBytes = 1 << 20

	seedPreviewMemories(t, database, "m", 1, 1, 3)

	s.db = &failingExternalStore{Store: database}

	if err := s.sleep(triggerManual); err == nil {
		t.Error("expected an unmeasurable external axis to fail the cycle")
	}
}

// TestPreviewFailsWhenTheExternalAxisCannotBeMeasured is the same rule on the read-only side. The
// preview reports the axis's utilisation, so a zero would describe a store with a target it is
// nowhere near - a dry run that disagrees with the cycle it is describing.
func TestPreviewFailsWhenTheExternalAxisCannotBeMeasured(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.capacityExternalBytes = 1 << 20

	seedPreviewMemories(t, database, "m", 1, 1, 3)

	s.db = &failingExternalStore{Store: database}

	if _, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{}); err == nil {
		t.Error("expected an unmeasurable external axis to fail the preview")
	}
}

// TestPressureReusesTheExternalReading covers the second cycle's arm: the figure eviction took at
// the end of the previous cycle is reused rather than scanned for again. Pressure is a smoothing
// factor, so a one-cycle-old reading is fine, and the hard caps are still enforced against fresh
// readings in evict().
func TestPressureReusesTheExternalReading(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.capacityExternalBytes = 10_000
	s.consolidation.deletionThreshold = -1

	ctx := context.Background()

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id:            "pointer",
		Body:          "x",
		TimeStamp:     time.Now().UnixNano(),
		Significance:  1000,
		ExternalBytes: 5_000,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// The first cycle measures and records the reading; the second is the one that reuses it.
	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	if s.consolidation.lastExternalBytes != 5_000 {
		t.Fatalf("lastExternalBytes = %d, want the measured 5000", s.consolidation.lastExternalBytes)
	}

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep (second cycle): %s", err)
	}

	// Half the target on the external axis, and nothing on the store's own, so the pressure the
	// second cycle computed can only have come from the reading carried across.
	if s.consolidation.capacityPressure <= 0 {
		t.Errorf("capacity pressure = %g; the external reading did not reach it", s.consolidation.capacityPressure)
	}
}

// TestRetentionWarnsWhenItHoldsTheExternalAxisOverItsTarget covers the external half of the
// retention gauges. It is the one condition under which the external capacity target silently stops
// being achievable - retention overrides the target, so eviction can never bring the axis back under
// it - and it is logged as well as counted for the deployments with no metrics stack.
func TestRetentionWarnsWhenItHoldsTheExternalAxisOverItsTarget(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.minimumRetentionInDays = 7
	s.consolidation.capacityBytes = 1 << 20
	s.consolidation.capacityExternalBytes = 1_000

	ctx := context.Background()

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id:            "retained",
		Body:          "x",
		TimeStamp:     time.Now().UnixNano(),
		Significance:  1000,
		ExternalBytes: 9_000,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	var buf bytes.Buffer

	restoreOutput := log.StandardLogger().Out
	restoreLevel := log.GetLevel()

	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)

	t.Cleanup(func() {
		log.SetOutput(restoreOutput)
		log.SetLevel(restoreLevel)
	})

	s.recordRetention(ctx)

	if !strings.Contains(buf.String(), "external capacity target") {
		t.Errorf("expected a warning about the external axis, got: %s", buf.String())
	}
}

// TestRetentionSaysNothingAboutAnUnconfiguredExternalAxis is the gate's other side: without a target
// there is nothing for the figure to be unreachable against, so neither the gauge nor the warning is
// published.
func TestRetentionSaysNothingAboutAnUnconfiguredExternalAxis(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.minimumRetentionInDays = 7
	s.consolidation.capacityBytes = 1 << 20

	seedPreviewMemories(t, database, "m", 0, 1000, 1)

	var buf bytes.Buffer

	restoreOutput := log.StandardLogger().Out
	restoreLevel := log.GetLevel()

	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)

	t.Cleanup(func() {
		log.SetOutput(restoreOutput)
		log.SetLevel(restoreLevel)
	})

	s.recordRetention(context.Background())

	if strings.Contains(buf.String(), "external capacity target") {
		t.Errorf("an unconfigured axis must not be warned about, got: %s", buf.String())
	}
}

// TestPreviewDeciderReportsTheSnapshotThreshold covers the decider's threshold accessor, which
// exists so the store's scans read the preview's own snapshot pressure rather than the field the
// sleep goroutine is mutating. The preview writes no tombstones - it deletes nothing - so this is
// reached only through db.Server, and only a direct call exercises it.
func TestPreviewDeciderReportsTheSnapshotThreshold(t *testing.T) {
	s, _ := previewTestServer(t)
	s.consolidation.deletionThreshold = 2

	decider := previewDecider{server: s, capacityPressure: 3}

	want := s.deletionThresholdUnder(3)

	if got := decider.DeletionThreshold(); got != want {
		t.Errorf("DeletionThreshold() = %g, want the snapshot's %g", got, want)
	}

	if got := decider.DeletionThreshold(); got == s.deletionThresholdUnder(s.consolidation.capacityPressure) && want != s.deletionThresholdUnder(s.consolidation.capacityPressure) {
		t.Error("the decider read the server's live pressure rather than its own snapshot")
	}
}
