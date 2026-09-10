package hippocampus

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// stallServer builds a consolidating server whose decay policy takes everything, so a cycle that is
// not stalled is unmistakable: it empties the store.
func stallServer(t *testing.T, policy BacklogPolicy, bounds db.QueueBounds) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	database.SetCallbackPolicy(db.CallbackPolicy{
		Enabled:         true,
		RetainDeletions: policy.retains(),
		MemoryEvents:    true,
		EventEvents:     true,
	})

	return &Server{
		db:                   database,
		consolidationEnabled: true,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1e9,
			capacityPressure:  1.0,
		},
		callbacksEnabled:      true,
		callbackBounds:        bounds,
		callbackBatchSize:     10,
		callbackBaseBack:      time.Second,
		callbackMaxBack:       time.Minute,
		callbackChunkIds:      3,
		callbackBacklogPolicy: policy,
		stopCallbacks:         make(chan struct{}),
	}, database
}

// seedForgettable stores memories old enough and slight enough that the threshold above takes all
// of them.
func seedForgettable(t *testing.T, database *db.DB, ids ...string) {
	t.Helper()

	old := time.Now().Add(-365 * 24 * time.Hour).UnixNano()

	for _, id := range ids {
		if _, err := database.CreateMemory(context.Background(), types.Memory{
			Id: id, Body: "body of " + id, Significance: 1, Group: "svc-a", TimeStamp: old,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}
}

// backlogUp queues n undelivered memory-forgotten deliveries, as a receiver outage would leave.
func backlogUp(t *testing.T, database *db.DB, n int, queuedAt int64) {
	t.Helper()

	for i := range n {
		if err := database.QueueCallbacks(context.Background(), []db.CallbackDelivery{{
			Kind:      db.CallbackKindMemoryForgotten,
			Cause:     db.CauseConsolidation,
			QueuedAt:  queuedAt,
			ItemCount: 1,
			Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "gone", Significance: int32(i)}}},
		}}); err != nil {
			t.Fatalf("QueueCallbacks: %s", err)
		}
	}
}

func countMemories(t *testing.T, database *db.DB) int {
	t.Helper()

	with, without := database.CountMemories(context.Background())

	if with < 0 || without < 0 {
		t.Fatalf("CountMemories reported %d/%d", with, without)
	}

	return with + without
}

// TestAStalledCycleForgetsNothing is the whole point of the stall: with the backlog past the caps,
// a real cycle runs and deletes nothing, and says so.
func TestAStalledCycleForgetsNothing(t *testing.T) {
	s, database := stallServer(t, BacklogStall, db.QueueBounds{MaxRows: 3})

	seedForgettable(t, database, "m1", "m2", "m3")
	backlogUp(t, database, 5, time.Now().UnixNano())

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	if held := countMemories(t, database); held != 3 {
		t.Errorf("a stalled cycle left %d memories, want all 3 - it forgot something", held)
	}

	report := s.lastCycle.Load()
	if report == nil {
		t.Fatal("a stalled cycle published no report")
	}

	if !report.stalled {
		t.Error("the report does not say the cycle stalled, which is the only thing separating it from a quiet one")
	}

	if report.stalledReason == "" {
		t.Error("the report gives no reason, so an operator is told what happened and not why")
	}

	// The cycle itself is not a failure: nothing went wrong, the store declined to forget. Reporting
	// it as a failure would fire the sleep-cycle alert instead of the one that names the cause.
	if !report.success {
		t.Errorf("a stalled cycle reported failure %q, want success with stalled set", report.failure)
	}
}

// TestTheStallClearsWhenTheBacklogDrains pins that the valve is a valve. A stall that outlived the
// outage would be a store that never forgets again after one bad afternoon.
func TestTheStallClearsWhenTheBacklogDrains(t *testing.T) {
	s, database := stallServer(t, BacklogStall, db.QueueBounds{MaxRows: 3})
	ctx := context.Background()

	seedForgettable(t, database, "m1", "m2", "m3")
	backlogUp(t, database, 5, time.Now().UnixNano())

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("stalled sleep: %s", err)
	}

	if held := countMemories(t, database); held != 3 {
		t.Fatalf("the first cycle forgot %d memories, want a stall", 3-held)
	}

	// The receiver comes back: everything queued is accepted.
	claimed, err := database.ClaimCallbacks(ctx, 100, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	seqs := make([]int64, 0, len(claimed))

	for _, entry := range claimed {
		seqs = append(seqs, entry.Seq)
	}

	if err := database.ConfirmCallbacks(ctx, seqs); err != nil {
		t.Fatalf("ConfirmCallbacks: %s", err)
	}

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("second sleep: %s", err)
	}

	if held := countMemories(t, database); held != 0 {
		t.Errorf("%d memories survived the cycle after the backlog drained, want the stall cleared", held)
	}

	if report := s.lastCycle.Load(); report == nil || report.stalled {
		t.Error("the cycle after the drain still reports stalled")
	}
}

// TestOnlyTheStallPolicyStopsForgetting is the other two values, and the one that protects every
// existing deployment: the same backlog under abandon or retain must not hold the cycle.
func TestOnlyTheStallPolicyStopsForgetting(t *testing.T) {
	for _, policy := range []BacklogPolicy{BacklogAbandon, BacklogRetain} {
		t.Run(policy.String(), func(t *testing.T) {
			s, database := stallServer(t, policy, db.QueueBounds{MaxRows: 3})

			seedForgettable(t, database, "m1", "m2", "m3")
			backlogUp(t, database, 5, time.Now().UnixNano())

			if err := s.sleep(triggerManual); err != nil {
				t.Fatalf("sleep: %s", err)
			}

			if held := countMemories(t, database); held != 0 {
				t.Errorf("%d memories survived under %q, which stops forgetting only under stall", held, policy)
			}
		})
	}
}

// TestTheStallIsJudgedOnEveryCap covers the three axes separately, because they are three different
// comparisons and an operator who set only one of them gets only that one.
func TestTheStallIsJudgedOnEveryCap(t *testing.T) {
	stale := time.Now().Add(-48 * time.Hour).UnixNano()

	cases := []struct {
		name     string
		bounds   db.QueueBounds
		rows     int
		queuedAt int64
		want     bool
	}{
		{"rows over", db.QueueBounds{MaxRows: 2}, 3, time.Now().UnixNano(), true},
		{"rows under", db.QueueBounds{MaxRows: 20}, 3, time.Now().UnixNano(), false},
		{"age over", db.QueueBounds{MaxAge: time.Hour}, 1, stale, true},
		{"age under", db.QueueBounds{MaxAge: 96 * time.Hour}, 1, stale, false},
		{"bytes over", db.QueueBounds{MaxBytes: 1}, 1, time.Now().UnixNano(), true},
		{"bytes under", db.QueueBounds{MaxBytes: 1 << 30}, 1, time.Now().UnixNano(), false},
		// No cap set is not a stall: startup refuses that arrangement, and a check that stalled on it
		// anyway would make a refused configuration behave as the most drastic one there is.
		{"no cap", db.QueueBounds{}, 50, stale, false},
		// And an empty queue never stalls, whatever the caps say.
		{"nothing queued", db.QueueBounds{MaxRows: 1}, 0, stale, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, database := stallServer(t, BacklogStall, tc.bounds)

			backlogUp(t, database, tc.rows, tc.queuedAt)

			stalled, reason := s.forgettingStalled(context.Background())

			if stalled != tc.want {
				t.Fatalf("forgettingStalled = %t (%q), want %t", stalled, reason, tc.want)
			}

			if stalled && reason == "" {
				t.Error("a stall with no reason tells an operator nothing they can act on")
			}
		})
	}
}

// TestASleepCompletedDeliveryCarriesTheStall pins the one thing that reaches the party who can fix
// it. The receiver is by definition down while this is happening, so it learns of the stall when it
// comes back or not at all.
func TestASleepCompletedDeliveryCarriesTheStall(t *testing.T) {
	s, database := stallServer(t, BacklogStall, db.QueueBounds{MaxRows: 3})
	s.callbackSleepEvents = true

	backlogUp(t, database, 5, time.Now().UnixNano())

	s.queueCycleCallback(context.Background(), 7, &cycleReport{
		trigger:       triggerTimer,
		startedAt:     time.Now(),
		stalled:       true,
		stalledReason: "the receiver is refusing deliveries",
		success:       true,
	})

	claimed, err := database.ClaimCallbacks(context.Background(), 100, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	for _, delivery := range claimed {
		if delivery.Kind != db.CallbackKindSleepCompleted {
			continue
		}

		if delivery.Payload.Cycle == nil || !delivery.Payload.Cycle.Stalled {
			t.Fatalf("the completion delivery does not carry the stall: %+v", delivery.Payload.Cycle)
		}

		if delivery.Payload.Cycle.StalledReason == "" {
			t.Error("the completion delivery carries the flag without the reason")
		}

		return
	}

	t.Fatal("the cycle queued no completion delivery")
}

// TestParseBacklogPolicy pins the spellings, since a value the validator accepts and the server
// silently defaults would be the worst of both.
func TestParseBacklogPolicy(t *testing.T) {
	for spelling, want := range map[string]BacklogPolicy{
		"":         BacklogAbandon,
		"abandon":  BacklogAbandon,
		"retain":   BacklogRetain,
		"stall":    BacklogStall,
		"  STALL ": BacklogStall,
	} {
		policy, ok := ParseBacklogPolicy(spelling)

		if !ok || policy != want {
			t.Errorf("ParseBacklogPolicy(%q) = %v, %t; want %v, true", spelling, policy, ok, want)
		}
	}

	if _, ok := ParseBacklogPolicy("keep"); ok {
		t.Error("ParseBacklogPolicy accepted a spelling this build does not implement")
	}

	// The names the error message lists must be the ones the parser takes, or the message sends an
	// operator round the restart loop with a value that is refused again.
	for _, name := range BacklogPolicyNames() {
		if _, ok := ParseBacklogPolicy(name); !ok {
			t.Errorf("BacklogPolicyNames lists %q, which ParseBacklogPolicy refuses", name)
		}
	}
}
