package hippocampus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// previewTestServer builds a Server over an in-memory store with a decay configuration under which
// everything seeded by seedPreviewMemories has decayed well past the threshold, so a preview has
// something to report.
func previewTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New("")
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
			capacityPressure:  1.0,
		},
	}

	return s, database
}

// seedPreviewMemories stores count memories aged days old, so their decayed value is a function of
// a real elapsed age rather than of a fabricated timestamp.
func seedPreviewMemories(t *testing.T, database *db.DB, prefix string, days int, significance int32, count int) {
	t.Helper()

	timestamp := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixNano()

	for i := range count {
		memory := types.Memory{
			Id:           fmt.Sprintf("%s%d", prefix, i),
			TimeStamp:    timestamp,
			Significance: significance,
			Body:         strings.Repeat("x", 128),
		}

		if _, err := database.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", memory.Id, err)
		}
	}
}

// TestPreviewConsolidationRejectedOnAReplica covers the replica guard: an instance that never runs
// a cycle has no forgetting of its own to describe.
func TestPreviewConsolidationRejectedOnAReplica(t *testing.T) {
	s, _ := previewTestServer(t)
	s.consolidationEnabled = false

	_, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err == nil {
		t.Fatal("expected a preview on a replica to be rejected")
	}

	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", got)
	}
}

// TestPreviewConsolidationReportsWhatWouldGo covers the happy path end to end: old, low
// significance memories are reported as consolidation candidates, and the store is untouched.
func TestPreviewConsolidationReportsWhatWouldGo(t *testing.T) {
	s, database := previewTestServer(t)

	seedPreviewMemories(t, database, "old", 100, 1, 5)

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if res.GetMemoriesConsolidated() != 5 {
		t.Errorf("expected 5 memories consolidated, got %d", res.GetMemoriesConsolidated())
	}

	if len(res.GetCandidates()) != 5 {
		t.Fatalf("expected 5 candidates, got %d", len(res.GetCandidates()))
	}

	for _, candidate := range res.GetCandidates() {
		if candidate.GetRule() != contract.ForgetRule_FORGET_RULE_CONSOLIDATION {
			t.Errorf("expected the consolidation rule, got %s", candidate.GetRule())
		}

		// The two sides of the comparison that decided it must both be reported, and must agree
		// with the verdict: a consolidation candidate is one whose value fell below the threshold.
		if candidate.GetValue() >= candidate.GetThreshold() {
			t.Errorf("candidate %s reported value %f which is not below its threshold %f",
				candidate.GetId(), candidate.GetValue(), candidate.GetThreshold())
		}
	}

	// Nothing was deleted.
	memories, err := database.GetMemories(context.Background(), db.MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*memories) != 5 {
		t.Errorf("the preview deleted memories: %d of 5 remain", len(*memories))
	}
}

// TestPreviewConsolidationPredictsTheCycle is the RPC-level counterpart of the db package's
// drift test: whatever the preview says, running the real cycle must then do.
func TestPreviewConsolidationPredictsTheCycle(t *testing.T) {
	s, database := previewTestServer(t)

	// A mix: old and doomed, plus recent and safe.
	seedPreviewMemories(t, database, "old", 100, 1, 6)
	seedPreviewMemories(t, database, "new", 0, 100, 4)

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	predicted := res.GetMemoriesConsolidated()

	if predicted == 0 {
		t.Fatal("expected the preview to predict some consolidation")
	}

	before, err := database.GetMemories(context.Background(), db.MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	after, err := database.GetMemories(context.Background(), db.MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := int32(len(*before) - len(*after)); got != predicted {
		t.Errorf("the preview predicted %d memories would go, the cycle deleted %d", predicted, got)
	}
}

// TestPreviewConsolidationReportsItsInputs covers the decision inputs the response carries so the
// numbers can be read against the configuration that produced them.
func TestPreviewConsolidationReportsItsInputs(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.capacityBytes = 1 << 20
	s.consolidation.capacityMemories = 10

	seedPreviewMemories(t, database, "m", 100, 1, 5)

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	// Half the row capacity is used, so pressure is above 1 but not yet at its ceiling.
	if res.GetCapacityPressure() <= 1.0 {
		t.Errorf("expected capacity pressure above 1.0, got %f", res.GetCapacityPressure())
	}

	// The reported threshold is the configured one scaled by that pressure - the value the
	// decisions were actually made against.
	want := s.consolidation.deletionThreshold * res.GetCapacityPressure()
	if res.GetDeletionThreshold() != want {
		t.Errorf("deletion threshold: got %f, want %f", res.GetDeletionThreshold(), want)
	}

	if res.GetCapacityBytes() != s.consolidation.capacityBytes {
		t.Errorf("capacity bytes: got %d, want %d", res.GetCapacityBytes(), s.consolidation.capacityBytes)
	}

	if res.GetUsedBytes() <= 0 {
		t.Errorf("expected used bytes to be reported, got %d", res.GetUsedBytes())
	}
}

// TestPreviewConsolidationUsesASnapshotNotTheLiveFields pins the reason previewDecider exists: the
// preview must decide against the pressure it computed for itself, not whatever the sleep
// goroutine last left in the server's field.
func TestPreviewConsolidationUsesASnapshotNotTheLiveFields(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.capacityMemories = 10

	seedPreviewMemories(t, database, "m", 100, 1, 5)

	// A stale live value that no longer reflects the store. If the preview read it instead of
	// computing its own, the reported pressure would come back as this.
	s.consolidation.capacityPressure = 99.0

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if res.GetCapacityPressure() == 99.0 {
		t.Error("the preview read the server's live capacity pressure instead of computing its own")
	}

	// And it must not have written its snapshot back over the live field either.
	if s.consolidation.capacityPressure != 99.0 {
		t.Errorf("the preview mutated the server's capacity pressure: %f", s.consolidation.capacityPressure)
	}
}

// TestPreviewConsolidationLimitBoundsTheSample covers the bounded-sample/complete-counts split at
// the RPC layer.
func TestPreviewConsolidationLimitBoundsTheSample(t *testing.T) {
	s, database := previewTestServer(t)

	seedPreviewMemories(t, database, "m", 100, 1, 12)

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{Limit: 5})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if res.GetMemoriesConsolidated() != 12 {
		t.Errorf("expected the count to stay complete at 12, got %d", res.GetMemoriesConsolidated())
	}

	if len(res.GetCandidates()) != 5 {
		t.Errorf("expected 5 sampled candidates, got %d", len(res.GetCandidates()))
	}

	if !res.GetTruncated() {
		t.Error("expected truncated to be set")
	}
}

// TestPreviewConsolidationCountsRetained covers the retention reporting, including the byte figure
// that makes it actionable: retention overrides the capacity target, so retained bytes approaching
// the capacity is what tells an operator the target has become unreachable.
func TestPreviewConsolidationCountsRetained(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.minimumRetentionInDays = 365

	seedPreviewMemories(t, database, "m", 100, 1, 5)

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if res.GetMemoriesConsolidated() != 0 {
		t.Errorf("retention should have spared everything, got %d consolidated", res.GetMemoriesConsolidated())
	}

	if res.GetMemoriesRetained() != 5 {
		t.Errorf("expected 5 retained, got %d", res.GetMemoriesRetained())
	}

	if res.GetRetainedBytes() <= 0 {
		t.Errorf("expected retained bytes to be reported, got %d", res.GetRetainedBytes())
	}

	if len(res.GetCandidates()) != 0 {
		t.Errorf("retained memories were listed: %d candidates", len(res.GetCandidates()))
	}
}

// failingPreviewStore wraps a real store and fails one chosen read, so the preview's error paths
// are exercised without a broken database.
type failingPreviewStore struct {
	db.Store
	usedBytesErr  error
	countMemories bool
	percentileErr error
	previewErr    error
}

func (f *failingPreviewStore) PreviewConsolidation(ctx context.Context, s db.Server, opts db.PreviewOptions) (db.ConsolidationPreview, error) {
	if f.previewErr != nil {
		return db.ConsolidationPreview{}, f.previewErr
	}

	return f.Store.PreviewConsolidation(ctx, s, opts)
}

func (f *failingPreviewStore) UsedBytes(ctx context.Context) (int64, error) {
	if f.usedBytesErr != nil {
		return 0, f.usedBytesErr
	}

	return f.Store.UsedBytes(ctx)
}

func (f *failingPreviewStore) CountMemories(ctx context.Context) (int, int) {
	if f.countMemories {
		return -1, -1
	}

	return f.Store.CountMemories(ctx)
}

func (f *failingPreviewStore) CalculateSignificancePercentile(ctx context.Context, percent float64) (float64, error) {
	if f.percentileErr != nil {
		return 0, f.percentileErr
	}

	return f.Store.CalculateSignificancePercentile(ctx, percent)
}

// TestPreviewConsolidationFailsWhenItsInputsAreUnavailable covers the two reads the preview cannot
// proceed without: it must refuse rather than report a forgetting schedule derived from a pressure
// it could not compute.
func TestPreviewConsolidationFailsWhenItsInputsAreUnavailable(t *testing.T) {
	tests := []struct {
		name  string
		store func(db.Store) db.Store
	}{
		{
			name:  "used bytes unavailable",
			store: func(s db.Store) db.Store { return &failingPreviewStore{Store: s, usedBytesErr: errors.New("boom")} },
		},
		{
			name:  "memory count unavailable",
			store: func(s db.Store) db.Store { return &failingPreviewStore{Store: s, countMemories: true} },
		},
		{
			name:  "the scan itself fails",
			store: func(s db.Store) db.Store { return &failingPreviewStore{Store: s, previewErr: errors.New("boom")} },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, database := previewTestServer(t)
			seedPreviewMemories(t, database, "m", 100, 1, 2)

			s.db = test.store(database)

			if _, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{}); err == nil {
				t.Fatal("expected the preview to fail when its inputs are unavailable")
			}
		})
	}
}

// TestPreviewConsolidationRecomputesThePercentile covers the derived default event significance:
// like a real cycle, the preview recomputes it rather than reading what the last cycle left behind.
func TestPreviewConsolidationRecomputesThePercentile(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.defaultEventSignificancePercentile = 50

	if _, err := database.CreateEvent(context.Background(), types.Event{
		Id: "e1", Name: "e1", TimeStart: time.Now().UnixNano(), Significance: 500,
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	seedPreviewMemories(t, database, "m", 100, 1, 3)

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	// The percentile lifts the memories' effective significance well above their own, so they are
	// worth far more than the bare significance of 1 would make them.
	if res.GetMemoriesConsolidated() != 0 {
		t.Errorf("expected the percentile to protect the memories, got %d consolidated", res.GetMemoriesConsolidated())
	}

	// And an unavailable percentile must fall back rather than fail, as the cycle does.
	s.db = &failingPreviewStore{Store: database, percentileErr: errors.New("no events")}

	if _, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{}); err != nil {
		t.Errorf("an unavailable percentile should not fail the preview: %s", err)
	}
}

// TestPreviewConsolidationCountsEmptyEvents covers the third consolidation pass through the RPC:
// an event holding no memories, decayed past the threshold.
func TestPreviewConsolidationCountsEmptyEvents(t *testing.T) {
	s, database := previewTestServer(t)

	old := time.Now().Add(-500 * 24 * time.Hour).UnixNano()

	if _, err := database.CreateEvent(context.Background(), types.Event{
		Id: "stale", Name: "stale", TimeStart: old, TimeEnd: old, Significance: 1,
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if res.GetEventsDeleted() != 1 {
		t.Errorf("expected the empty decayed event to be counted, got %d", res.GetEventsDeleted())
	}

	// And the cycle then does exactly that.
	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	events, err := database.GetEvents(context.Background(), db.EventFilter{})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*events) != 0 {
		t.Errorf("the cycle kept an event the preview said would go: %d remain", len(*events))
	}
}

// TestPreviewConsolidationNeverReturnsBodies pins the boundary that keeps a dry run from doubling
// as a way to read the store.
func TestPreviewConsolidationNeverReturnsBodies(t *testing.T) {
	s, database := previewTestServer(t)

	secret := "the body that must not be returned"

	memory := types.Memory{
		Id:           "m1",
		TimeStamp:    time.Now().Add(-100 * 24 * time.Hour).UnixNano(),
		Significance: 1,
		Body:         secret,
	}

	if _, err := database.CreateMemory(context.Background(), memory); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if got := res.String(); strings.Contains(got, secret) {
		t.Errorf("the preview returned a memory body: %s", got)
	}

	if len(res.GetCandidates()) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(res.GetCandidates()))
	}

	// The size is reported even though the content is not - that is the point of the field.
	if res.GetCandidates()[0].GetBodyBytes() <= 0 {
		t.Errorf("expected a body size, got %d", res.GetCandidates()[0].GetBodyBytes())
	}
}

// countingPreviewStore counts how many scans reach the storage layer, so a test can pin that
// concurrent previews collapse onto one.
type countingPreviewStore struct {
	db.Store
	mu      sync.Mutex
	scans   int
	release chan struct{}
}

func (c *countingPreviewStore) PreviewConsolidation(ctx context.Context, s db.Server, opts db.PreviewOptions) (db.ConsolidationPreview, error) {
	c.mu.Lock()
	c.scans++
	c.mu.Unlock()

	// Hold the scan open until the test has all its callers waiting, so they genuinely overlap
	// rather than arriving one after another.
	if c.release != nil {
		<-c.release
	}

	return c.Store.PreviewConsolidation(ctx, s, opts)
}

func (c *countingPreviewStore) scanCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.scans
}

// TestPreviewConcurrentCallsShareOneScan is why previewGroup exists: on SQLite the connection pool
// is one connection by design, so a stream of previews each running its own full scan would crowd
// out the sleep cycle's own queries.
func TestPreviewConcurrentCallsShareOneScan(t *testing.T) {
	s, database := previewTestServer(t)
	seedPreviewMemories(t, database, "m", 100, 1, 5)

	counting := &countingPreviewStore{Store: database, release: make(chan struct{})}
	s.db = counting

	const callers = 8

	var wg sync.WaitGroup
	results := make([]*contract.PreviewConsolidationResponse, callers)
	errs := make([]error, callers)

	for i := range callers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			results[i], errs[i] = s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{})
		}()
	}

	// Let the callers pile up behind the in-flight scan, then release it.
	time.Sleep(50 * time.Millisecond)
	close(counting.release)

	wg.Wait()

	if got := counting.scanCount(); got != 1 {
		t.Errorf("expected the concurrent previews to share one scan, got %d", got)
	}

	// Every caller must still get a complete, correct answer of its own.
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %s", i, errs[i])
		}

		if results[i].GetMemoriesConsolidated() != 5 {
			t.Errorf("caller %d got %d consolidated, want 5", i, results[i].GetMemoriesConsolidated())
		}
	}

	// Each caller must own its response, not share one message: a proto message is not safe to
	// marshal concurrently, which is why the shared value is a plain struct.
	for i := 1; i < callers; i++ {
		if results[i] == results[0] {
			t.Fatalf("callers %d and 0 were handed the same response message", i)
		}
	}
}

// TestPreviewDifferentLimitsDoNotShare is the other half of the keying: a caller asking for more
// rows must not be handed a shorter list because someone else asked first.
func TestPreviewDifferentLimitsDoNotShare(t *testing.T) {
	s, database := previewTestServer(t)
	seedPreviewMemories(t, database, "m", 100, 1, 20)

	small, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{Limit: 2})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	large, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{Limit: 15})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if len(small.GetCandidates()) != 2 {
		t.Errorf("small: got %d candidates, want 2", len(small.GetCandidates()))
	}

	if len(large.GetCandidates()) != 15 {
		t.Errorf("large: got %d candidates, want 15", len(large.GetCandidates()))
	}
}

// TestPreviewNeverJoinsASleepCycle pins the property the separate group exists to preserve: the
// preview must not be collapsed into the sleep singleflight, where it would describe a run that is
// at that moment deleting.
func TestPreviewNeverJoinsASleepCycle(t *testing.T) {
	s, database := previewTestServer(t)
	seedPreviewMemories(t, database, "m", 100, 1, 4)

	// A preview in flight must leave the sleep group free, and vice versa - if the two shared a
	// group, one of these would return the other's result.
	if _, err := s.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{}); err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if err := s.sleepOnce(triggerManual); err != nil {
		t.Fatalf("sleepOnce: %s", err)
	}

	// The cycle really ran: the memories the preview only described are now gone.
	memories, err := database.GetMemories(context.Background(), db.MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*memories) != 0 {
		t.Errorf("the sleep cycle did not run: %d memories remain", len(*memories))
	}
}

// retentionCountingStore records whether the retained-stats scan was issued, so a test can pin the
// gate that keeps it off stores with no retention floor.
type retentionCountingStore struct {
	db.Store
	calls  int
	cutoff int64
}

func (r *retentionCountingStore) RetainedStats(ctx context.Context, cutoff int64) (int, int64, error) {
	r.calls++
	r.cutoff = cutoff

	return r.Store.RetainedStats(ctx, cutoff)
}

// TestRecordRetentionOnlyScansWhenThereIsAFloor pins the gate: the retained gauges cost an extra
// aggregate scan per cycle, so a deployment with no retention floor - where the answer is always
// zero - must not pay for it.
func TestRecordRetentionOnlyScansWhenThereIsAFloor(t *testing.T) {
	tests := []struct {
		name      string
		retention int
		wantScan  bool
	}{
		{name: "no floor configured", retention: 0, wantScan: false},
		{name: "negative disables it", retention: -1, wantScan: false},
		{name: "a floor is measured", retention: 7, wantScan: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, database := previewTestServer(t)
			s.consolidation.minimumRetentionInDays = test.retention

			counting := &retentionCountingStore{Store: database}
			s.db = counting

			seedPreviewMemories(t, database, "m", 1, 1, 3)

			s.recordRetention(context.Background())

			if got := counting.calls > 0; got != test.wantScan {
				t.Errorf("scanned = %t, want %t", got, test.wantScan)
			}

			if !test.wantScan {
				return
			}

			// The cutoff must be the retention window back from now, so the same clock
			// consolidation measures age from decides what counts as retained.
			want := time.Now().Add(-time.Duration(test.retention) * 24 * time.Hour).UnixNano()

			if diff := counting.cutoff - want; diff > int64(time.Minute) || diff < -int64(time.Minute) {
				t.Errorf("cutoff is %d, want approximately %d", counting.cutoff, want)
			}
		})
	}
}

// TestRecordRetentionSurvivesAFailedScan covers the best-effort contract: the gauges are a
// measurement, and failing to take one must not fail the sleep cycle.
func TestRecordRetentionSurvivesAFailedScan(t *testing.T) {
	s, database := previewTestServer(t)
	s.consolidation.minimumRetentionInDays = 7
	s.consolidation.capacityBytes = 1 << 20

	seedPreviewMemories(t, database, "m", 1, 1, 3)

	s.db = &failingRetentionStore{Store: database}

	// The call itself must not panic or propagate, and a full cycle over the same store must still
	// succeed.
	s.recordRetention(context.Background())

	if err := s.sleep(triggerManual); err != nil {
		t.Errorf("a failed retention measurement failed the sleep cycle: %s", err)
	}
}

type failingRetentionStore struct {
	db.Store
}

func (f *failingRetentionStore) RetainedStats(context.Context, int64) (int, int64, error) {
	return 0, 0, errors.New("boom")
}
