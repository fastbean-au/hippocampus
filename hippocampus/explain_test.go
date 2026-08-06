package hippocampus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// explainTestServer builds a Server over an in-memory store with the same decay configuration the
// preview tests use, so a memory's standing here can be checked against the cycle's own decision.
func explainTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()

	s, database := previewTestServer(t)

	return s, database
}

// storeAgedMemory stores one memory whose decay clock reads days old, and returns its id.
func storeAgedMemory(t *testing.T, database *db.DB, id string, days float64, significance int32) string {
	t.Helper()

	memory := types.Memory{
		Id:           id,
		TimeStamp:    time.Now().Add(-time.Duration(days * float64(24*time.Hour))).UnixNano(),
		Significance: significance,
		Body:         strings.Repeat("x", 64),
	}

	if _, err := database.CreateMemory(context.Background(), memory); err != nil {
		t.Fatalf("CreateMemory(%s): %s", id, err)
	}

	return memory.Id
}

// TestExplainConsolidationRejectedOnAReplica covers the replica guard: this instance's decay policy
// is not the one its store is consolidated under, so reporting it would describe a schedule nothing
// carries out.
func TestExplainConsolidationRejectedOnAReplica(t *testing.T) {
	s, _ := explainTestServer(t)
	s.consolidationEnabled = false

	_, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{})
	if err == nil {
		t.Fatal("expected an explanation on a replica to be rejected")
	}

	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", got)
	}
}

// TestExplainConsolidationAgreesWithTheCycle is the drift guard, and the reason the value is served
// rather than recomputed by each client: whatever the RPC says about a memory must be what the
// consolidation scan decides about the same memory.
func TestExplainConsolidationAgreesWithTheCycle(t *testing.T) {
	s, database := explainTestServer(t)

	ids := []string{
		storeAgedMemory(t, database, "ancient", 400, 1),
		storeAgedMemory(t, database, "middling", 5, 3),
		storeAgedMemory(t, database, "fresh", 0.5, 9),
	}

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{MemoryIds: ids})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if len(res.GetValuations()) != len(ids) {
		t.Fatalf("expected %d valuations, got %d", len(ids), len(res.GetValuations()))
	}

	// The same store, scanned by the real preview pass: every memory it would consolidate must be
	// exactly the set the explanation flagged.
	preview, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	forgetting := make(map[string]bool)
	for _, candidate := range preview.GetCandidates() {
		forgetting[candidate.GetId()] = true
	}

	for _, valuation := range res.GetValuations() {
		if valuation.GetWouldConsolidate() != forgetting[valuation.GetId()] {
			t.Errorf("%s: would_consolidate %t, but the cycle says %t",
				valuation.GetId(),
				valuation.GetWouldConsolidate(),
				forgetting[valuation.GetId()],
			)
		}

		// The flag must follow from the two numbers reported beside it, or the numbers explain
		// nothing.
		if valuation.GetWouldConsolidate() != (valuation.GetValue() < valuation.GetThreshold()) {
			t.Errorf("%s: value %g against threshold %g does not explain would_consolidate %t",
				valuation.GetId(),
				valuation.GetValue(),
				valuation.GetThreshold(),
				valuation.GetWouldConsolidate(),
			)
		}
	}
}

// TestExplainConsolidationOrdersAndDeduplicatesIds covers the two request-shaping rules: answers
// come back in the order they were asked for, and a repeated id costs neither a duplicate answer
// nor a duplicate lookup.
func TestExplainConsolidationOrdersAndDeduplicatesIds(t *testing.T) {
	s, database := explainTestServer(t)

	storeAgedMemory(t, database, "a", 10, 1)
	storeAgedMemory(t, database, "b", 10, 2)
	storeAgedMemory(t, database, "c", 10, 3)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"c", "a", "c", "missing", ""},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	var got []string
	for _, valuation := range res.GetValuations() {
		got = append(got, valuation.GetId())
	}

	if len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Errorf("expected [c a] in request order, got %v", got)
	}
}

// TestExplainConsolidationRejectsTooManyIds covers the request bound.
func TestExplainConsolidationRejectsTooManyIds(t *testing.T) {
	s, _ := explainTestServer(t)

	ids := make([]string, explainMaxMemoryIds+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%d", i)
	}

	_, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{MemoryIds: ids})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// TestExplainConsolidationNeverReturnsBodies pins the boundary the preview draws too: an
// explanation reports what the service thinks of a memory, and must not become a second way to read
// one.
func TestExplainConsolidationNeverReturnsBodies(t *testing.T) {
	s, database := explainTestServer(t)

	body := "the-secret-body-nobody-asked-for"

	if _, err := database.CreateMemory(context.Background(), types.Memory{
		Id:           "secret",
		TimeStamp:    time.Now().Add(-48 * time.Hour).UnixNano(),
		Significance: 1,
		Body:         body,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"secret"},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if strings.Contains(res.String(), body) {
		t.Error("an explanation returned the memory body")
	}
}

// TestExplainConsolidationReportsTheRetentionFloor covers the flag that overrides the value
// comparison in the memory's favour: a memory well past the threshold but inside the retention
// window is safe, and must be shown to be.
func TestExplainConsolidationReportsTheRetentionFloor(t *testing.T) {
	s, database := explainTestServer(t)
	s.consolidation.minimumRetentionInDays = 30

	storeAgedMemory(t, database, "held", 10, 1)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"held"},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	valuation := res.GetValuations()[0]

	if !valuation.GetRetained() {
		t.Error("expected the memory to be reported as retained")
	}

	if valuation.GetWouldConsolidate() {
		t.Error("a retained memory must never be reported as one a cycle would forget")
	}

	// Retention defers the projection: nothing can take this memory before the floor passes.
	if days := valuation.GetDaysUntilForgotten(); days < 19 || days > 21 {
		t.Errorf("expected ~20 days until forgotten (30 day floor, 10 days old), got %g", days)
	}
}

// TestExplainConsolidationProjectsTheDueDate covers the projection itself: a memory reported as due
// in n days must, when aged by n days, be one the cycle would take.
func TestExplainConsolidationProjectsTheDueDate(t *testing.T) {
	s, database := explainTestServer(t)

	storeAgedMemory(t, database, "counting-down", 1, 5)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"counting-down"},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	valuation := res.GetValuations()[0]

	days := valuation.GetDaysUntilForgotten()
	if days <= 0 {
		t.Fatalf("expected a future due date, got %g", days)
	}

	// Under method 1 with aggressiveness 1 the value is significance/age, so a significance-5
	// memory crosses a threshold of 1 at five days old - four days after the one it has lived.
	if math.Abs(days-4) > 0.1 {
		t.Errorf("expected ~4 days until forgotten, got %g", days)
	}

	candidate := db.MemoryConsolidationCandidate{
		MemorySignificance: 5,
		Timestamp:          time.Now().Add(-time.Duration((1 + days + 0.01) * float64(24*time.Hour))).UnixNano(),
	}

	if !s.ShouldConsolidateMemory(candidate) {
		t.Error("the cycle would not consolidate the memory at the age the projection promised")
	}
}

// TestExplainConsolidationCurve covers the served curve: it is sampled from the same maths, sized to
// show the crossing when the caller does not choose a span, and monotonically decreasing.
func TestExplainConsolidationCurve(t *testing.T) {
	s, _ := explainTestServer(t)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		Curve: &contract.DecayCurveRequest{Significance: 10},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	curve := res.GetCurve()
	if curve == nil {
		t.Fatal("expected a curve")
	}

	if len(curve.GetPoints()) != curveDefaultPoints {
		t.Errorf("expected %d points, got %d", curveDefaultPoints, len(curve.GetPoints()))
	}

	// Significance 10 against a threshold of 1 crosses at ten days under method 1, and the chosen
	// span must run past it or the plot hides the one thing worth seeing.
	if crossing := curve.GetCrossingAgeDays(); math.Abs(crossing-10) > 0.01 {
		t.Errorf("expected the crossing at ~10 days, got %g", crossing)
	}

	if curve.GetMaxAgeDays() <= curve.GetCrossingAgeDays() {
		t.Errorf("expected the span (%g) to run past the crossing (%g)", curve.GetMaxAgeDays(), curve.GetCrossingAgeDays())
	}

	previous := math.MaxFloat64

	for _, point := range curve.GetPoints() {
		if math.IsNaN(point.GetValue()) || math.IsInf(point.GetValue(), 0) {
			t.Fatalf("curve point at %g days is not a finite value", point.GetAgeDays())
		}

		if point.GetValue() > previous {
			t.Errorf("curve rose at %g days: %g after %g", point.GetAgeDays(), point.GetValue(), previous)
		}

		previous = point.GetValue()

		// Sampled from the same function the decisions use, not from a second implementation of it.
		want := s.calculateValue(10, point.GetAgeDays()/s.consolidation.unitsOfAgeInDays)
		if math.Abs(point.GetValue()-want) > 1e-9 {
			t.Errorf("curve point at %g days is %g, calculateValue says %g", point.GetAgeDays(), point.GetValue(), want)
		}
	}
}

// TestExplainConsolidationCurveWithoutACrossing covers the configuration that never forgets what it
// was asked about: the projection reports no crossing rather than a number that looks like one.
func TestExplainConsolidationCurveWithoutACrossing(t *testing.T) {
	s, _ := explainTestServer(t)

	// Method 5's logarithmic long tail against a high significance: the value falls so slowly that
	// no age worth reporting takes it under the threshold.
	s.consolidation.method = 5

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		Curve: &contract.DecayCurveRequest{Significance: 1e6},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if got := res.GetCurve().GetCrossingAgeDays(); got != -1 {
		t.Errorf("expected no crossing (-1), got %g", got)
	}

	if len(res.GetCurve().GetPoints()) == 0 {
		t.Error("expected a curve to be drawn even where it never crosses")
	}
}

// TestExplainConsolidationCurveValidation covers the one curve input with no sensible default.
func TestExplainConsolidationCurveValidation(t *testing.T) {
	s, _ := explainTestServer(t)

	for _, significance := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		_, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
			Curve: &contract.DecayCurveRequest{Significance: significance},
		})

		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("significance %g: expected InvalidArgument, got %v", significance, err)
		}
	}

	_, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		Curve: &contract.DecayCurveRequest{Significance: 1, MaxAgeDays: -5},
	})

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for a negative span, got %v", err)
	}
}

// TestExplainConsolidationCurvePointsAreBounded covers the sample-count normalisation.
func TestExplainConsolidationCurvePointsAreBounded(t *testing.T) {
	s, _ := explainTestServer(t)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		Curve: &contract.DecayCurveRequest{Significance: 10, Points: curveMaxPoints * 10, MaxAgeDays: 20},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if got := len(res.GetCurve().GetPoints()); got != curveMaxPoints {
		t.Errorf("expected the point count clamped to %d, got %d", curveMaxPoints, got)
	}

	if got := res.GetCurve().GetMaxAgeDays(); got != 20 {
		t.Errorf("expected the requested span of 20 days, got %g", got)
	}
}

// TestExplainConsolidationReportsTheDecisionInputs covers the half of the response that explains the
// threshold rather than a memory: without these the value has nothing to be read against.
func TestExplainConsolidationReportsTheDecisionInputs(t *testing.T) {
	s, database := explainTestServer(t)
	s.consolidation.capacityMemories = 10
	s.consolidation.minimumAgeInDays = 2
	s.consolidation.minimumRetentionInDays = 1

	storeAgedMemory(t, database, "one", 3, 1)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"one"},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if res.GetMemoryCount() != 1 {
		t.Errorf("expected a memory count of 1, got %d", res.GetMemoryCount())
	}

	if res.GetCapacityMemories() != 10 {
		t.Errorf("expected the row capacity reported, got %d", res.GetCapacityMemories())
	}

	if res.GetMethod() != int32(s.consolidation.method) || res.GetUnitsOfAgeInDays() != s.consolidation.unitsOfAgeInDays {
		t.Error("expected the decay policy echoed back")
	}

	if res.GetMinimumAgeInDays() != 2 || res.GetMinimumRetentionInDays() != 1 {
		t.Error("expected both age floors reported")
	}

	// One memory against a capacity of ten: pressure is above 1, and the threshold must be the
	// configured one scaled by it rather than the bare configured value.
	if res.GetCapacityPressure() <= 1 {
		t.Errorf("expected capacity pressure above 1, got %g", res.GetCapacityPressure())
	}

	want := s.consolidation.deletionThreshold * res.GetCapacityPressure()
	if math.Abs(res.GetDeletionThreshold()-want) > 1e-12 {
		t.Errorf("expected a threshold of %g, got %g", want, res.GetDeletionThreshold())
	}
}

// TestExplainConsolidationValuesAgainstASnapshot covers the field the RPC must never read live: the
// sleep goroutine rewrites Consolidation.capacityPressure, so an explanation evaluating against it
// would be racing that write - and would be reporting a pressure it did not compute.
func TestExplainConsolidationValuesAgainstASnapshot(t *testing.T) {
	s, database := explainTestServer(t)
	s.consolidation.capacityPressure = 99

	storeAgedMemory(t, database, "one", 3, 1)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"one"},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if res.GetCapacityPressure() == 99 {
		t.Error("the explanation read the live capacity pressure instead of its own snapshot")
	}
}

// TestExplainConsolidationCachesTheSnapshot covers the cost control: the two store readings behind
// the snapshot are full scans, so repeated calls inside the TTL must reuse one reading, and a call
// after it must take a fresh one.
func TestExplainConsolidationCachesTheSnapshot(t *testing.T) {
	counting := &countingUsedBytesStore{Store: mustExplainStore(t)}

	s := &Server{
		db:                   counting,
		consolidationEnabled: true,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	for range 5 {
		if _, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{}); err != nil {
			t.Fatalf("ExplainConsolidation: %s", err)
		}
	}

	if counting.usedBytesCalls != 1 {
		t.Errorf("expected one snapshot across five calls, got %d", counting.usedBytesCalls)
	}

	// Age the cached snapshot past its TTL: the next call must take a fresh reading.
	s.explainStateMu.Lock()
	s.explainState.at = time.Now().Add(-2 * explainStateTTL)
	s.explainStateMu.Unlock()

	if _, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{}); err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if counting.usedBytesCalls != 2 {
		t.Errorf("expected a second snapshot once the cached one expired, got %d", counting.usedBytesCalls)
	}
}

// TestExplainConsolidationFailsWhenItsInputsAreUnavailable covers the two reads it cannot proceed
// without. Like the preview, it must refuse rather than report a standing derived from a pressure
// it could not compute, or from memories it could not read.
func TestExplainConsolidationFailsWhenItsInputsAreUnavailable(t *testing.T) {
	tests := []struct {
		name  string
		store func(db.Store) db.Store
	}{
		{
			name:  "the snapshot's inputs are unavailable",
			store: func(s db.Store) db.Store { return &failingPreviewStore{Store: s, usedBytesErr: errors.New("boom")} },
		},
		{
			name:  "the candidate lookup fails",
			store: func(s db.Store) db.Store { return &failingExplainStore{Store: s, err: errors.New("boom")} },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, database := explainTestServer(t)

			storeAgedMemory(t, database, "one", 3, 1)

			s.db = test.store(database)

			_, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
				MemoryIds: []string{"one"},
			})

			if err == nil {
				t.Fatal("expected the explanation to fail when its inputs are unavailable")
			}
		})
	}
}

// failingExplainStore fails the candidate lookup while leaving every other read working, so the
// second error path is exercised without a broken database.
type failingExplainStore struct {
	db.Store
	err error
}

func (f *failingExplainStore) GetMemoryConsolidationCandidates(ctx context.Context, ids []string) ([]db.IdentifiedMemoryCandidate, error) {
	if f.err != nil {
		return nil, f.err
	}

	return f.Store.GetMemoryConsolidationCandidates(ctx, ids)
}

// TestExplainConsolidationWithoutAThreshold covers the configuration where nothing is ever
// forgotten by value: with no threshold to fall below there is no crossing to project or plot.
func TestExplainConsolidationWithoutAThreshold(t *testing.T) {
	s, database := explainTestServer(t)
	s.consolidation.deletionThreshold = 0

	storeAgedMemory(t, database, "immortal", 400, 1)

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		MemoryIds: []string{"immortal"},
		Curve:     &contract.DecayCurveRequest{Significance: 5},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	if got := res.GetValuations()[0].GetDaysUntilForgotten(); got != -1 {
		t.Errorf("expected no projected date without a threshold, got %g", got)
	}

	if got := res.GetCurve().GetCrossingAgeDays(); got != -1 {
		t.Errorf("expected no crossing without a threshold, got %g", got)
	}
}

// TestExplainConsolidationCurveDropsNonFinitePoints covers the sampling guard. An age unit so large
// that a whole day rounds to no age at all values as an infinity under method 1 - which neither
// JSON nor a plot has anywhere to put, so the point is dropped rather than returned.
func TestExplainConsolidationCurveDropsNonFinitePoints(t *testing.T) {
	s, _ := explainTestServer(t)
	s.consolidation.unitsOfAgeInDays = math.MaxFloat64

	res, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{
		Curve: &contract.DecayCurveRequest{Significance: 5, MaxAgeDays: 1},
	})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	for _, point := range res.GetCurve().GetPoints() {
		if math.IsInf(point.GetValue(), 0) || math.IsNaN(point.GetValue()) {
			t.Fatalf("a non-finite point at %g days reached the response", point.GetAgeDays())
		}
	}
}

// mustExplainStore opens an in-memory store for the caching test, which needs a Store it can wrap
// rather than the concrete database the other tests seed.
func mustExplainStore(t *testing.T) db.Store {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return database
}
