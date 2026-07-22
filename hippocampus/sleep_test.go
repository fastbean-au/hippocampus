package hippocampus

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// failConsolidateStore wraps a real db.Store but forces the loose-memory consolidation scan to
// fail, so consolidate()'s error propagation can be exercised without a broken database.
type failConsolidateStore struct {
	db.Store
	err error
}

func (f failConsolidateStore) ConsolidateMemories(ctx context.Context, s db.Server) (int, error) {
	return 0, f.err
}

// TestConsolidate_PropagatesScanError is a regression test: the consolidation
// scans used to log their errors and return bare counts, so consolidate() always returned nil and a
// sleep cycle where a scan failed still recorded success=true (its error branch in sleep() was dead
// code). A failing scan must now surface through consolidate() while the other passes still run.
func TestConsolidate_PropagatesScanError(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	wantErr := errors.New("scan boom")

	s := &Server{
		db: failConsolidateStore{Store: database, err: wantErr},
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	err = s.consolidate(context.Background())
	if err == nil {
		t.Fatal("consolidate returned nil despite a failing scan; the error was swallowed")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("expected the failing scan's error to propagate, got %v", err)
	}
}

// countingUsedBytesStore wraps a real db.Store and counts UsedBytes calls, so a test can pin how
// many table scans a sleep cycle costs.
type countingUsedBytesStore struct {
	db.Store
	usedBytesCalls int
}

func (c *countingUsedBytesStore) UsedBytes(ctx context.Context) (int64, error) {
	c.usedBytesCalls++

	return c.Store.UsedBytes(ctx)
}

// TestSleep_UsedBytesScannedOncePerCycle is a regression test: with a byte capacity
// configured, a sleep cycle used to read UsedBytes twice (once for pressure in consolidate, once in
// evict) - two full table scans on the server drivers. Pressure now reuses the previous cycle's
// eviction reading, so a cycle scans UsedBytes exactly once.
func TestSleep_UsedBytesScannedOncePerCycle(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	c := &countingUsedBytesStore{Store: database}

	s := &Server{
		db: c,
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
			capacityBytes:     1 << 30, // large, so eviction reads UsedBytes but evicts nothing
		},
	}

	if err := s.sleep(); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	if c.usedBytesCalls != 1 {
		t.Errorf("expected UsedBytes scanned once per cycle, got %d", c.usedBytesCalls)
	}
}

// TestSleep_WrapsUnderlyingError is a regression test: sleep() used to replace a
// failing pass's real error with a static string, so the Sleep RPC caller and the span never saw
// the cause. It must now wrap it so errors.Is reaches the underlying error.
func TestSleep_WrapsUnderlyingError(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	wantErr := errors.New("scan boom")

	s := &Server{
		db: failConsolidateStore{Store: database, err: wantErr},
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	err = s.sleep()
	if err == nil {
		t.Fatal("sleep returned nil despite a failing consolidation pass")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("sleep did not wrap the underlying cause; errors.Is could not reach it: %v", err)
	}
}

// candidate builds a MemoryConsolidationCandidate for a never-recalled memory.
func candidate(eventSignificance int32, memorySignificance int32, relationshipSignificance int64, timestamp int64) db.MemoryConsolidationCandidate {
	return db.MemoryConsolidationCandidate{
		EventSignificance:        eventSignificance,
		MemorySignificance:       memorySignificance,
		RelationshipSignificance: relationshipSignificance,
		Timestamp:                timestamp,
	}
}

// TestCalculateValue_ReturnsPreciseFloat verifies that calculateValue preserves fractional
// precision. Before the fix the return type was int, so 3/2.0=1.5 was truncated to 1.
func TestCalculateValue_ReturnsPreciseFloat(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:           1,
			aggressiveness:   1.0,
			unitsOfAgeInDays: 1.0,
		},
	}

	// method 1: 3 / 2^1 = 1.5
	got := s.calculateValue(3, 2.0)
	if got != 1.5 {
		t.Errorf("expected 1.5, got %v — fractional value was likely truncated", got)
	}
}

// TestCalculateValue_Method4_ExponentialDecay verifies the half-life property of the exponential
// method: value halves every ln(2)/aggressiveness age units, independent of the starting age —
// the defining trait of exponential (constant relative rate) decay, distinct from the linear
// methods 2 and 3 and from the power law of method 1.
func TestCalculateValue_Method4_ExponentialDecay(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 4, aggressiveness: 1.0}}

	halfLife := math.Ln2 / s.consolidation.aggressiveness

	full := s.calculateValue(1000, 1.0)
	afterOneHalfLife := s.calculateValue(1000, 1.0+halfLife)
	afterTwoHalfLives := s.calculateValue(1000, 1.0+2*halfLife)

	if diff := math.Abs(afterOneHalfLife - full/2); diff > 1e-9 {
		t.Errorf("expected value to halve after one half-life: %v vs %v", afterOneHalfLife, full/2)
	}

	if diff := math.Abs(afterTwoHalfLives - full/4); diff > 1e-9 {
		t.Errorf("expected value to quarter after two half-lives: %v vs %v", afterTwoHalfLives, full/4)
	}
}

// TestCalculateValue_Method5_LogarithmicDecay verifies the long-tail shape of the logarithmic
// method: an old memory retains far more of its value than the same memory would under the other
// methods, and a non-positive aggressiveness degrades safely to "never consolidate" rather than a
// negative or NaN value, mirroring method 3's guard against the same class of misconfiguration.
func TestCalculateValue_Method5_LogarithmicDecay(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 5, aggressiveness: 1.0}}

	// significance / (1 * ln(97 + e)) = 1000 / ln(99.718) ~= 1000 / 4.602
	got := s.calculateValue(1000, 97)
	want := 1000 / math.Log(97+math.E)

	if diff := math.Abs(got - want); diff > 1e-9 {
		t.Errorf("expected %v, got %v", want, got)
	}

	// A thousand-day-old memory should still retain a large fraction of its starting value —
	// the point of a long-tail curve.
	if got < 100 {
		t.Errorf("expected a long-tail curve to retain substantial value at age 97, got %v", got)
	}

	s.consolidation.aggressiveness = 0
	if got := s.calculateValue(1000, 1); got != math.MaxFloat64 {
		t.Errorf("zero aggressiveness should degrade to never-consolidate, got %v", got)
	}

	s.consolidation.aggressiveness = -1.0
	if got := s.calculateValue(1000, 1); got != math.MaxFloat64 {
		t.Errorf("negative aggressiveness should degrade to never-consolidate, got %v", got)
	}
}

// TestCalculateValue_Method6_SigmoidDecay verifies the consolidation-window shape: value sits
// close to significance well before the aggressiveness midpoint, is exactly half of significance
// at the midpoint, and is close to zero well beyond it — a smooth S-curve rather than a hard
// cutoff, so eviction ranking among memories on the same side of the window stays meaningful.
func TestCalculateValue_Method6_SigmoidDecay(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 6, aggressiveness: 10.0}}

	atMidpoint := s.calculateValue(1000, 10.0)
	if diff := math.Abs(atMidpoint - 500); diff > 1e-6 {
		t.Errorf("expected exactly half of significance at the midpoint, got %v", atMidpoint)
	}

	wellBefore := s.calculateValue(1000, 0.001)
	if wellBefore < 990 {
		t.Errorf("expected value well before the midpoint to be close to significance, got %v", wellBefore)
	}

	wellAfter := s.calculateValue(1000, 20.0)
	if wellAfter > 10 {
		t.Errorf("expected value well past the midpoint to be close to zero, got %v", wellAfter)
	}

	// A zero aggressiveness collapses the midpoint to age 0, so every positive age is "past the
	// window" — the curve must degrade to immediate consolidation, not divide by zero into NaN.
	s.consolidation.aggressiveness = 0
	if got := s.calculateValue(1000, 1.0); got != 0 {
		t.Errorf("zero aggressiveness should degrade to immediate consolidation (0), got %v", got)
	}
}

// TestCalculateValue_Method2_LinearVariant verifies method 2's formula directly: despite its name
// suggesting an exponential curve, it is linear in age (a constant divisor scaled by e raised to
// aggressiveness), unlike method 4, the one genuinely exponential method.
func TestCalculateValue_Method2_LinearVariant(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 2, aggressiveness: 1.0}}

	got := s.calculateValue(1000, 2.0)
	want := 1000 / (2.0 * math.Pow(math.E, 1.0))

	if diff := math.Abs(got - want); diff > 1e-9 {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// TestCalculateValue_UnknownMethodNeverConsolidates verifies the default arm: a method number
// outside the six documented algorithms degrades safely to "never consolidate" (math.MaxFloat64)
// rather than a zero value that would delete everything.
func TestCalculateValue_UnknownMethodNeverConsolidates(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 99, aggressiveness: 1.0}}

	if got := s.calculateValue(1000, 5.0); got != math.MaxFloat64 {
		t.Errorf("expected an unknown method to degrade to never-consolidate, got %v", got)
	}
}

// TestShouldConsolidateMemory_Method3_NegativeLogFactor verifies that method 3 with an
// aggressiveness value that drives (1 + ln(aggressiveness)) negative does not incorrectly
// consolidate memories. Before the fix, calculateValue returned a negative number in this case,
// which is always < any positive threshold, so every memory would be deleted.
func TestShouldConsolidateMemory_Method3_NegativeLogFactor(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:            3,
			aggressiveness:    0.1, // 1/e ≈ 0.368, so 0.1 makes (1+ln(aggressiveness)) negative
			unitsOfAgeInDays:  1.0,
			minimumAgeInDays:  0,
			deletionThreshold: 1.0,
		},
	}

	// A very significant, one-day-old memory should not be consolidated.
	oneDayAgo := time.Now().UnixNano() - int64(DAY_IN_NANOSECONDS)
	if s.ShouldConsolidateMemory(candidate(100, 100, 0, oneDayAgo)) {
		t.Error("high-significance memory should not be consolidated when method 3 aggressiveness produces a negative log factor")
	}
}

// TestCalculateValue_Method3_NegativeAggressivenessNotNaN is a regression test:
// method 3's factor is 1 + ln(aggressiveness), and ln of a negative aggressiveness is NaN. NaN
// fails the factor <= 0 guard (every NaN comparison is false), so before the fix it propagated out
// of calculateValue; MemoryValue then fed NaN into eviction's sort.Slice, whose comparator is not a
// valid ordering over NaN, so eviction deleted essentially arbitrary memories. The value must
// degrade to MaxFloat64 (never-consolidate), not NaN.
func TestCalculateValue_Method3_NegativeAggressivenessNotNaN(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 3, aggressiveness: -1.0}}

	got := s.calculateValue(1000, 2.0)

	if math.IsNaN(got) {
		t.Fatal("method 3 with negative aggressiveness produced NaN; it must degrade to MaxFloat64")
	}

	if got != math.MaxFloat64 {
		t.Errorf("expected MaxFloat64 (never-consolidate), got %v", got)
	}
}

// TestCalculateValue_Method5_NaNFactorNotNaN exercises method 5's hardened guard directly: its
// factor is only NaN-free because the caller guarantees age > 0 (so ln(age+e) stays finite). A
// negative age reaching this method - which the IsNaN guard now tolerates - would make ln(age+e)
// NaN; the result must still degrade to MaxFloat64 rather than leak NaN into eviction's sort.
func TestCalculateValue_Method5_NaNFactorNotNaN(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 5, aggressiveness: 1.0}}

	// age + e < 0 makes ln NaN; the guard must catch it.
	got := s.calculateValue(1000, -10.0)

	if math.IsNaN(got) {
		t.Fatal("method 5 with a NaN factor produced NaN; it must degrade to MaxFloat64")
	}

	if got != math.MaxFloat64 {
		t.Errorf("expected MaxFloat64 (never-consolidate), got %v", got)
	}
}

// TestShouldConsolidateMemory_NonPositiveAge verifies that a memory whose computed age is ≤ 0
// (just created, or timestamped in the future) is never consolidated. A non-positive ageUnits
// causes division by zero or sign-flip in the formula, producing ±Inf or NaN.
func TestShouldConsolidateMemory_NonPositiveAge(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:            3,
			aggressiveness:    0.1, // negative factor + near-zero age → -Inf → incorrect consolidation
			unitsOfAgeInDays:  1.0,
			minimumAgeInDays:  0,
			deletionThreshold: 1.0,
		},
	}

	// Timestamp is effectively "now" — age rounds to zero.
	now := time.Now().UnixNano()
	if s.ShouldConsolidateMemory(candidate(100, 100, 0, now)) {
		t.Error("memory with near-zero age should not be consolidated")
	}

	// Timestamp in the future — age is negative.
	future := time.Now().UnixNano() + int64(10*DAY_IN_NANOSECONDS)
	if s.ShouldConsolidateMemory(candidate(100, 100, 0, future)) {
		t.Error("memory timestamped in the future should not be consolidated")
	}
}

// TestShouldConsolidateMemory_TinyFutureAgeUnitsNonPositive is a regression-style test for
// shouldConsolidate's own ageUnits <= 0 guard, distinct from the minimumAgeInDays guard that
// TestShouldConsolidateMemory_NonPositiveAge's future-timestamp case actually exercises: a
// timestamp only a few milliseconds in the future still truncates to "0 days old" for the
// minimumAgeInDays check (so that guard does not fire with minimumAgeInDays 0), but ageUnits itself
// - computed at finer precision - is still negative, and must be caught rather than flowing into
// calculateValue.
func TestShouldConsolidateMemory_TinyFutureAgeUnitsNonPositive(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			minimumAgeInDays:  0,
			deletionThreshold: 1.0,
		},
	}

	tinyFuture := time.Now().UnixNano() + int64(time.Millisecond)

	if s.ShouldConsolidateMemory(candidate(100, 100, 0, tinyFuture)) {
		t.Error("a timestamp a millisecond in the future must not be consolidated")
	}
}

// TestCalculateValue_Method3_PositiveFactor exercises method 3's ordinary (non-degenerate) path,
// where (1 + ln(aggressiveness)) is positive - the two existing method-3 tests only cover the
// negative/NaN guard, never this line.
func TestCalculateValue_Method3_PositiveFactor(t *testing.T) {
	s := &Server{consolidation: Consolidation{method: 3, aggressiveness: math.E * math.E}} // ln(e^2) = 2

	got := s.calculateValue(1000, 2.0)
	want := 1000 / (2.0 * 3.0) // factor = 1 + ln(e^2) = 1 + 2 = 3

	if diff := math.Abs(got - want); diff > 1e-6 {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// TestShouldConsolidateMemory_FractionalThreshold verifies that a fractional deletionThreshold
// is respected. Before the fix, deletionThreshold was int so values like 1.5 were impossible,
// and float computed values were truncated before comparison causing wrong decisions near the
// boundary.
func TestShouldConsolidateMemory_FractionalThreshold(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                        1,
			aggressiveness:                1.0,
			unitsOfAgeInDays:              1.0,
			minimumAgeInDays:              0,
			deletionThreshold:             1.5,
			defaultEventSignificanceValue: 1,
		},
	}

	// method 1: (es + ms) / age^1 = 3 / age
	//   age = 1.5 days → value = 2.0 > 1.5 → should NOT consolidate
	//   age = 3.0 days → value = 1.0 < 1.5 → SHOULD consolidate

	onePointFiveDaysAgo := time.Now().UnixNano() - int64(1.5*float64(DAY_IN_NANOSECONDS))
	threeDaysAgo := time.Now().UnixNano() - int64(3.0*float64(DAY_IN_NANOSECONDS))

	if s.ShouldConsolidateMemory(candidate(1, 2, 0, onePointFiveDaysAgo)) {
		t.Error("memory above threshold (value = 2.0) should not be consolidated")
	}

	if !s.ShouldConsolidateMemory(candidate(1, 2, 0, threeDaysAgo)) {
		t.Error("memory below threshold (value = 1.0) should be consolidated")
	}
}

// TestShouldConsolidateMemory_RelationshipSignificance verifies that an event's relationship
// significance, scaled by the configured weight, extends the survival of that event's memories.
// Two otherwise identical memories must diverge: the one whose event is well-connected survives.
func TestShouldConsolidateMemory_RelationshipSignificance(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                         1,
			aggressiveness:                 1.0,
			unitsOfAgeInDays:               1.0,
			minimumAgeInDays:               0,
			deletionThreshold:              1.0,
			relationshipSignificanceWeight: 1.0,
		},
	}

	// method 1: (es + ms + w*rs) / age^1
	//   age = 10 days, es = 2, ms = 3, rs = 0  → 5/10  = 0.5 < 1.0 → SHOULD consolidate
	//   age = 10 days, es = 2, ms = 3, rs = 10 → 15/10 = 1.5 > 1.0 → should NOT consolidate

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	if !s.ShouldConsolidateMemory(candidate(2, 3, 0, tenDaysAgo)) {
		t.Error("memory of an unconnected event should be consolidated")
	}

	if s.ShouldConsolidateMemory(candidate(2, 3, 10, tenDaysAgo)) {
		t.Error("memory of a well-connected event should not be consolidated")
	}
}

// TestShouldConsolidateMemory_RelationshipWeightZero verifies that a zero weight disables the
// relationship contribution entirely, preserving the previous behaviour.
func TestShouldConsolidateMemory_RelationshipWeightZero(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                         1,
			aggressiveness:                 1.0,
			unitsOfAgeInDays:               1.0,
			minimumAgeInDays:               0,
			deletionThreshold:              1.0,
			relationshipSignificanceWeight: 0.0,
		},
	}

	// value = (2 + 3 + 0*1000) / 10 = 0.5 < 1.0 → consolidated despite huge relationship
	// significance.
	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	if !s.ShouldConsolidateMemory(candidate(2, 3, 1000, tenDaysAgo)) {
		t.Error("relationship significance should have no effect when the weight is zero")
	}
}

// TestCalculateCapacityPressure verifies the deletion-threshold multiplier across the utilisation
// range: 1 when capacity is disabled or the store is empty, negligible when far from capacity,
// 2 at capacity, and growing beyond it when overfull.
func TestCalculateCapacityPressure(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			capacityMemories:         1000,
			capacityPressureExponent: 4.0,
		},
	}

	if got := s.calculateCapacityPressure(0, 0); got != 1.0 {
		t.Errorf("empty store: expected pressure 1.0, got %v", got)
	}

	if got := s.calculateCapacityPressure(500, 0); got != 1.0625 {
		t.Errorf("half full: expected pressure 1.0625, got %v", got)
	}

	if got := s.calculateCapacityPressure(1000, 0); got != 2.0 {
		t.Errorf("at capacity: expected pressure 2.0, got %v", got)
	}

	if got := s.calculateCapacityPressure(1500, 0); got <= 2.0 {
		t.Errorf("over capacity: expected pressure > 2.0, got %v", got)
	}

	s.consolidation.capacityMemories = 0
	if got := s.calculateCapacityPressure(1000000, 0); got != 1.0 {
		t.Errorf("capacity disabled: expected pressure 1.0, got %v", got)
	}
}

// TestCalculateCapacityPressure_ByteUtilisation verifies that byte utilisation against
// capacityBytes contributes to pressure, and that the greater of the two utilisations wins:
// a store with few rows but large bodies must feel byte pressure, and vice versa.
func TestCalculateCapacityPressure_ByteUtilisation(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			capacityMemories:         1000,
			capacityBytes:            1000000,
			capacityPressureExponent: 4.0,
		},
	}

	// Few rows, bytes at capacity: byte utilisation (1.0) beats count utilisation (0.01).
	if got := s.calculateCapacityPressure(10, 1000000); got != 2.0 {
		t.Errorf("bytes at capacity: expected pressure 2.0, got %v", got)
	}

	// Rows at capacity, few bytes: count utilisation (1.0) beats byte utilisation (0.001).
	if got := s.calculateCapacityPressure(1000, 1000); got != 2.0 {
		t.Errorf("rows at capacity: expected pressure 2.0, got %v", got)
	}

	// Bytes over capacity keep growing pressure past 2.
	if got := s.calculateCapacityPressure(10, 1500000); got <= 2.0 {
		t.Errorf("bytes over capacity: expected pressure > 2.0, got %v", got)
	}

	// With the byte capacity disabled, used bytes must not contribute.
	s.consolidation.capacityBytes = 0
	if got := s.calculateCapacityPressure(500, 1000000000); got != 1.0625 {
		t.Errorf("byte capacity disabled: expected count-only pressure 1.0625, got %v", got)
	}
}

// TestEvictionFloor verifies the hysteresis floor: a valid floor is used as the reclaim level,
// while an unset floor, or one above the capacity target, falls back to the target itself.
func TestEvictionFloor(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			capacityBytes: 1000,
		},
	}

	if got := s.evictionFloor(); got != 1000 {
		t.Errorf("unset floor: expected the capacity target 1000, got %d", got)
	}

	s.consolidation.capacityBytesFloor = 900
	if got := s.evictionFloor(); got != 900 {
		t.Errorf("valid floor: expected 900, got %d", got)
	}

	s.consolidation.capacityBytesFloor = 1500
	if got := s.evictionFloor(); got != 1000 {
		t.Errorf("floor above target: expected fallback to 1000, got %d", got)
	}
}

// failPreserveStore wraps a real db.Store but forces Preserve to fail, so preserve()'s error arm
// can be exercised deterministically without breaking a real database.
type failPreserveStore struct {
	db.Store
	err error
}

func (f failPreserveStore) Preserve(ctx context.Context) error {
	return f.err
}

// TestPreserve_PropagatesError verifies preserve() surfaces a Preserve failure (so a failed
// compaction is reflected in the sleep cycle's success metric) rather than swallowing it.
func TestPreserve_PropagatesError(t *testing.T) {
	s := &Server{db: failPreserveStore{err: errors.New("checkpoint failed")}}

	if err := s.preserve(context.Background()); err == nil {
		t.Fatal("expected preserve to surface the Preserve error")
	}
}

// TestEvict_DisabledAndUnderCapacityAreNoOps verifies evict() leaves the store untouched when byte
// eviction is disabled (capacityBytes <= 0) and when the store is comfortably under its target.
func TestEvict_DisabledAndUnderCapacityAreNoOps(t *testing.T) {
	s := newTestServer(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "body"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	// capacityBytes <= 0 disables byte eviction entirely.
	s.consolidation.capacityBytes = 0

	if err := s.evict(context.Background()); err != nil {
		t.Fatalf("evict (disabled): %s", err)
	}

	// A target far above the store's footprint evicts nothing.
	s.consolidation.capacityBytes = 1 << 30

	if err := s.evict(context.Background()); err != nil {
		t.Fatalf("evict (under capacity): %s", err)
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 3 {
		t.Fatalf("expected all 3 memories to survive, got %d", with+without)
	}
}

// TestEvict_ReclaimsWhenOverCapacity verifies the eviction path: with the store over its byte
// target, evict() deletes lowest-value memories down toward the floor and caches the fresh used
// reading for the next cycle's pressure calculation.
func TestEvict_ReclaimsWhenOverCapacity(t *testing.T) {
	s := newTestServer(t)
	s.consolidation = Consolidation{
		method:           1,
		aggressiveness:   1.0,
		unitsOfAgeInDays: 1.0,
	}

	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "a reasonably sized memory body"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	// A 1-byte target forces eviction down to the floor, reclaiming almost everything.
	s.consolidation.capacityBytes = 1

	if err := s.evict(context.Background()); err != nil {
		t.Fatalf("evict: %s", err)
	}

	with, without := s.db.CountMemories(context.Background())
	if with+without >= 5 {
		t.Fatalf("expected eviction to delete memories, still have %d", with+without)
	}

	if s.consolidation.lastUsedBytes <= 0 {
		t.Fatalf("expected evict to cache a positive used-bytes reading, got %d", s.consolidation.lastUsedBytes)
	}
}

// TestShouldConsolidateMemory_CapacityPressure verifies that capacity pressure raises the
// effective deletion threshold: a memory that survives in an unpressured store is consolidated
// when the store is under pressure.
func TestShouldConsolidateMemory_CapacityPressure(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			minimumAgeInDays:  0,
			deletionThreshold: 1.0,
			capacityPressure:  1.0,
		},
	}

	// method 1: (2 + 3) / 4 = 1.25; base threshold 1.0 → survives.
	fourDaysAgo := time.Now().UnixNano() - int64(4*DAY_IN_NANOSECONDS)

	if s.ShouldConsolidateMemory(candidate(2, 3, 0, fourDaysAgo)) {
		t.Error("memory should survive without capacity pressure")
	}

	// Under pressure the effective threshold is 1.0 × 1.5 = 1.5 > 1.25 → consolidated.
	s.consolidation.capacityPressure = 1.5

	if !s.ShouldConsolidateMemory(candidate(2, 3, 0, fourDaysAgo)) {
		t.Error("memory should be consolidated under capacity pressure")
	}
}

// TestShouldConsolidateMemory_ZeroPressureIsSafe verifies that an unset (zero) capacityPressure
// behaves as no pressure rather than zeroing the threshold and disabling all forgetting.
func TestShouldConsolidateMemory_ZeroPressureIsSafe(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			minimumAgeInDays:  0,
			deletionThreshold: 1.0,
		},
	}

	// (2 + 3) / 10 = 0.5 < 1.0 → must still be consolidated with capacityPressure unset.
	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	if !s.ShouldConsolidateMemory(candidate(2, 3, 0, tenDaysAgo)) {
		t.Error("zero capacityPressure should behave as pressure 1.0, not disable forgetting")
	}
}

// TestShouldConsolidateEvent verifies that events without memories age out under the same decay
// rules as memories: old low-significance events are consolidated, relationship significance
// protects an event, and age is measured from the most recent of the start and end times.
func TestShouldConsolidateEvent(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                         1,
			aggressiveness:                 1.0,
			unitsOfAgeInDays:               1.0,
			minimumAgeInDays:               0,
			deletionThreshold:              1.0,
			relationshipSignificanceWeight: 1.0,
		},
	}

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)
	twoDaysAgo := time.Now().UnixNano() - int64(2*DAY_IN_NANOSECONDS)

	// method 1: 5 / 10 = 0.5 < 1.0 → consolidated.
	old := db.EventConsolidationCandidate{Significance: 5, TimeStart: tenDaysAgo}
	if !s.ShouldConsolidateEvent(old) {
		t.Error("old low-significance event should be consolidated")
	}

	// Relationship significance protects: (5 + 10) / 10 = 1.5 > 1.0 → survives.
	connected := db.EventConsolidationCandidate{Significance: 5, RelationshipSignificance: 10, TimeStart: tenDaysAgo}
	if s.ShouldConsolidateEvent(connected) {
		t.Error("well-connected event should not be consolidated")
	}

	// A recent TimeEnd resets the age even when TimeStart is old: 5 / 2 = 2.5 > 1.0 → survives.
	recentEnd := db.EventConsolidationCandidate{Significance: 5, TimeStart: tenDaysAgo, TimeEnd: twoDaysAgo}
	if s.ShouldConsolidateEvent(recentEnd) {
		t.Error("event with a recent end time should not be consolidated")
	}
}

// TestShouldConsolidateMemory_RecallResetsDecayClock verifies that a recent recall resets the
// decay clock: age is measured from the last recall rather than creation, so an old but recently
// recalled memory survives where an identical unrecalled one is consolidated.
func TestShouldConsolidateMemory_RecallResetsDecayClock(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			minimumAgeInDays:  0,
			deletionThreshold: 1.0,
		},
	}

	// method 1: (2 + 3) / age
	//   age from creation (10 days)   → 0.5 < 1.0 → SHOULD consolidate
	//   age from last recall (2 days) → 2.5 > 1.0 → should NOT consolidate

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)
	twoDaysAgo := time.Now().UnixNano() - int64(2*DAY_IN_NANOSECONDS)

	unrecalled := candidate(2, 3, 0, tenDaysAgo)
	if !s.ShouldConsolidateMemory(unrecalled) {
		t.Error("old unrecalled memory should be consolidated")
	}

	recalled := candidate(2, 3, 0, tenDaysAgo)
	recalled.TimeRecalled = twoDaysAgo
	if s.ShouldConsolidateMemory(recalled) {
		t.Error("recently recalled memory should not be consolidated")
	}
}

// TestConsolidate_PercentileWithNoEvents exposes a bug where enabling
// defaultEventSignificancePercentile on a store with no events failed the whole sleep cycle:
// the percentile calculation errors on empty input and consolidate() returned that error, so
// nothing was consolidated, evicted, or compacted until the first event was stored. An empty
// store must fall back to the configured fixed value instead.
func TestConsolidate_PercentileWithNoEvents(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	defer func() { _ = database.Close() }()

	s := &Server{
		db: database,
		consolidation: Consolidation{
			method:                             1,
			aggressiveness:                     1.0,
			unitsOfAgeInDays:                   1.0,
			deletionThreshold:                  1.0,
			defaultEventSignificanceValue:      5,
			defaultEventSignificancePercentile: 25,
		},
	}

	if err := s.consolidate(context.Background()); err != nil {
		t.Errorf("consolidate() must not fail when the percentile has no events to work with: %s", err)
	}

	if s.consolidation.defaultEventSignificanceValue != 5 {
		t.Errorf(
			"the configured fixed value should be retained on fallback, got %d",
			s.consolidation.defaultEventSignificanceValue,
		)
	}
}

// TestScanSummarizationCandidates_PopulatesList verifies that the scan finds a quiet, populous
// event and stores it as a candidate, ready for GetSummarizationCandidates to serve.
func TestScanSummarizationCandidates_PopulatesList(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	defer func() { _ = database.Close() }()

	s := &Server{
		db: database,
		consolidation: Consolidation{
			summarizationMinMemories:   3,
			summarizationMinAgeInDays:  1,
			summarizationMaxCandidates: 10,
		},
	}

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	if _, err := database.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "quiet event", TimeStart: tenDaysAgo, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := database.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: tenDaysAgo, Significance: 1, EventId: "e1", Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	s.scanSummarizationCandidates(context.Background())

	if len(s.summarizationCandidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(s.summarizationCandidates), s.summarizationCandidates)
	}

	if s.summarizationCandidates[0].EventId != "e1" || s.summarizationCandidates[0].MemoryCount != 3 {
		t.Errorf("unexpected candidate: %+v", s.summarizationCandidates[0])
	}
}

// TestScanSummarizationCandidates_DisabledByDefault verifies that a non-positive
// summarizationMinMemories (the shipped default) disables the scan entirely, leaving the
// candidate list untouched even when qualifying events exist.
func TestScanSummarizationCandidates_DisabledByDefault(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	defer func() { _ = database.Close() }()

	s := &Server{
		db:            database,
		consolidation: Consolidation{summarizationMinMemories: 0},
	}

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	if _, err := database.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "quiet event", TimeStart: tenDaysAgo, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := database.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: tenDaysAgo, Significance: 1, EventId: "e1", Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	s.scanSummarizationCandidates(context.Background())

	if s.summarizationCandidates != nil {
		t.Errorf("expected the candidate list to remain untouched when disabled, got %+v", s.summarizationCandidates)
	}
}

// failFindCandidatesStore wraps a real db.Store but forces FindSummarizationCandidates to fail, so
// scanSummarizationCandidates' error-logging arm can be exercised without a broken database.
type failFindCandidatesStore struct {
	db.Store
	err error
}

func (f failFindCandidatesStore) FindSummarizationCandidates(ctx context.Context, minMemories int, maxTimestamp int64, limit int) ([]db.SummarizationCandidate, error) {
	return nil, f.err
}

// TestScanSummarizationCandidates_PropagatesScanError verifies a failing scan is logged and leaves
// the previously cached candidate list untouched, rather than panicking or clearing it.
func TestScanSummarizationCandidates_PropagatesScanError(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	s := &Server{
		db:                      failFindCandidatesStore{Store: database, err: errors.New("scan boom")},
		consolidation:           Consolidation{summarizationMinMemories: 1},
		summarizationCandidates: []db.SummarizationCandidate{{EventId: "stale"}},
	}

	s.scanSummarizationCandidates(context.Background())

	if len(s.summarizationCandidates) != 1 || s.summarizationCandidates[0].EventId != "stale" {
		t.Errorf("expected the stale candidate list left untouched on scan failure, got %+v", s.summarizationCandidates)
	}
}

// failCompactStore wraps a real db.Store but forces CompactSignificanceLevels to fail, so sleep()'s
// best-effort registry maintenance warning can be exercised without a broken database.
type failCompactStore struct {
	db.Store
	err error
}

func (f failCompactStore) CompactSignificanceLevels(ctx context.Context) error {
	return f.err
}

// TestSleep_CompactSignificanceLevelsFailureIsBestEffort verifies a failing registry compaction is
// logged and does not fail the sleep cycle - it sits outside the success flag.
func TestSleep_CompactSignificanceLevelsFailureIsBestEffort(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	s := &Server{
		db: failCompactStore{Store: database, err: errors.New("compaction boom")},
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	if err := s.sleep(); err != nil {
		t.Errorf("expected a failing CompactSignificanceLevels to be best-effort, got %s", err)
	}
}

// TestConsolidate_PercentileCalculatedFromEvents verifies the success arm of the default event
// significance percentile calculation: with events present, the computed percentile overwrites the
// configured fixed value (the complementary case to TestConsolidate_PercentileWithNoEvents, which
// only exercises the empty-store fallback).
func TestConsolidate_PercentileCalculatedFromEvents(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	for _, e := range []types.Event{
		{Id: "e1", Name: "one", TimeStart: 100, Significance: 2},
		{Id: "e2", Name: "two", TimeStart: 100, Significance: 8},
	} {
		if _, err := database.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	s := &Server{
		db: database,
		consolidation: Consolidation{
			method:                             1,
			aggressiveness:                     1.0,
			unitsOfAgeInDays:                   1.0,
			deletionThreshold:                  1.0,
			defaultEventSignificanceValue:      5,
			defaultEventSignificancePercentile: 50,
		},
	}

	if err := s.consolidate(context.Background()); err != nil {
		t.Fatalf("consolidate: %s", err)
	}

	if s.consolidation.defaultEventSignificanceValue == 5 {
		t.Error("expected the computed percentile to overwrite the configured fixed value")
	}
}

// failUsedBytesStore wraps a real db.Store but forces UsedBytes to fail, so evict()'s error arm can
// be exercised without a broken database.
type failUsedBytesStore struct {
	db.Store
	err error
}

func (f failUsedBytesStore) UsedBytes(ctx context.Context) (int64, error) {
	return 0, f.err
}

// TestEvict_UsedBytesErrorPropagates verifies a failing UsedBytes reading surfaces through evict()
// rather than being swallowed.
func TestEvict_UsedBytesErrorPropagates(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	wantErr := errors.New("used bytes boom")

	s := &Server{
		db:            failUsedBytesStore{Store: database, err: wantErr},
		consolidation: Consolidation{capacityBytes: 1000},
	}

	if err := s.evict(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("expected the UsedBytes failure to propagate, got %v", err)
	}
}

// failEvictMemoriesStore wraps a real db.Store, reports an inflated UsedBytes so eviction always
// judges the store over capacity, and forces EvictMemories itself to fail - so evict()'s second
// error arm (after a successful UsedBytes read) can be exercised.
type failEvictMemoriesStore struct {
	db.Store
	err error
}

func (f failEvictMemoriesStore) UsedBytes(ctx context.Context) (int64, error) {
	return math.MaxInt64 / 2, nil
}

func (f failEvictMemoriesStore) EvictMemories(ctx context.Context, s db.Server, freeBytes int64) (int, int, int64, error) {
	return 0, 0, 0, f.err
}

// TestEvict_EvictMemoriesErrorPropagates verifies a failing EvictMemories call surfaces through
// evict() (after used bytes are recorded and cached) rather than being swallowed.
func TestEvict_EvictMemoriesErrorPropagates(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	wantErr := errors.New("evict boom")

	s := &Server{
		db:            failEvictMemoriesStore{Store: database, err: wantErr},
		consolidation: Consolidation{capacityBytes: 1000},
	}

	if err := s.evict(context.Background()); !errors.Is(err, wantErr) {
		t.Errorf("expected the EvictMemories failure to propagate, got %v", err)
	}

	if s.consolidation.lastUsedBytes <= 0 {
		t.Error("expected the used-bytes reading to still be cached despite the later EvictMemories failure")
	}
}

// TestSleep_WrapsEvictAndPreserveErrors is a regression-style test for sleep()'s own switch: with
// consolidation succeeding, an evict() failure must surface as e2 and a preserve() failure as e3 -
// the two switch arms TestSleep_WrapsUnderlyingError (which drives the e1 arm) does not reach.
func TestSleep_WrapsEvictAndPreserveErrors(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	evictErr := errors.New("used bytes boom")

	s := &Server{
		db: failUsedBytesStore{Store: database, err: evictErr},
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
			capacityBytes:     1000,
		},
	}

	if err := s.sleep(); !errors.Is(err, evictErr) {
		t.Errorf("expected sleep to wrap the evict failure (e2), got %v", err)
	}

	preserveErr := errors.New("preserve boom")

	s2 := &Server{
		db: failPreserveStore{Store: database, err: preserveErr},
		consolidation: Consolidation{
			method:            1,
			aggressiveness:    1.0,
			unitsOfAgeInDays:  1.0,
			deletionThreshold: 1.0,
		},
	}

	// preserve() re-wraps its underlying cause into a generic message rather than carrying it via
	// %w (unlike consolidate/evict), so this checks the surfaced message rather than errors.Is.
	if err := s2.sleep(); err == nil || !strings.Contains(err.Error(), "failed to preserve") {
		t.Errorf("expected sleep to wrap the preserve failure (e3), got %v", err)
	}
}

// TestMemoryValue_RanksForEviction verifies the ordering contract MemoryValue provides to
// capacity eviction: lower significance ranks below higher at the same age, a fresh or
// future-dated memory ranks as maximally valuable, and a recent recall raises a memory's rank by
// resetting its decay clock.
func TestMemoryValue_RanksForEviction(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:           1,
			aggressiveness:   1.0,
			unitsOfAgeInDays: 1.0,
		},
	}

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)
	twoDaysAgo := time.Now().UnixNano() - int64(2*DAY_IN_NANOSECONDS)

	low := s.MemoryValue(candidate(2, 3, 0, tenDaysAgo))
	high := s.MemoryValue(candidate(20, 30, 0, tenDaysAgo))

	if low >= high {
		t.Errorf("lower significance should rank below higher at the same age: %v >= %v", low, high)
	}

	fresh := s.MemoryValue(candidate(1, 1, 0, time.Now().UnixNano()))
	if fresh <= high {
		t.Errorf("a fresh memory should rank above an aged one regardless of significance, got %v", fresh)
	}

	// A future-dated memory has non-positive age, which the formula cannot decay; it must pin to
	// the maximum rather than produce ±Inf or NaN.
	future := s.MemoryValue(candidate(1, 1, 0, time.Now().UnixNano()+int64(DAY_IN_NANOSECONDS)))
	if future != math.MaxFloat64 {
		t.Errorf("a future-dated memory should rank as maximally valuable, got %v", future)
	}

	recalled := candidate(2, 3, 0, tenDaysAgo)
	recalled.TimeRecalled = twoDaysAgo

	if s.MemoryValue(recalled) <= low {
		t.Error("a recent recall should raise a memory's value by resetting its decay clock")
	}
}

// TestShouldConsolidateMemory_RecallCountBoostsSignificance verifies that repeated recalls raise
// a memory's effective significance by recallSignificanceWeight per recall.
func TestShouldConsolidateMemory_RecallCountBoostsSignificance(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                   1,
			aggressiveness:           1.0,
			unitsOfAgeInDays:         1.0,
			minimumAgeInDays:         0,
			deletionThreshold:        1.0,
			recallSignificanceWeight: 1.0,
		},
	}

	// method 1: (2 + 3 + w*recalls) / age, with recalls long enough ago that the decay clock has
	// fully re-aged (recall 10 days ago, same as creation).
	//   recalls = 0  → 5/10  = 0.5 < 1.0 → SHOULD consolidate
	//   recalls = 10 → 15/10 = 1.5 > 1.0 → should NOT consolidate

	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	unrecalled := candidate(2, 3, 0, tenDaysAgo)
	if !s.ShouldConsolidateMemory(unrecalled) {
		t.Error("never-recalled memory should be consolidated")
	}

	recalled := candidate(2, 3, 0, tenDaysAgo)
	recalled.TimeRecalled = tenDaysAgo
	recalled.RecallCount = 10
	if s.ShouldConsolidateMemory(recalled) {
		t.Error("frequently recalled memory should not be consolidated")
	}
}
