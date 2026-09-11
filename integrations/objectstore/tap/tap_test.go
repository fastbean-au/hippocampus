package tap

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeRecaller records the batches it is asked for and answers with a configurable hit count.
type fakeRecaller struct {
	mu      sync.Mutex
	batches [][]string
	hits    func(ids []string) int
	err     error
}

func (f *fakeRecaller) Recall(ctx context.Context, ids []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.batches = append(f.batches, append([]string(nil), ids...))

	if f.err != nil {
		return 0, f.err
	}

	if f.hits == nil {
		return len(ids), nil
	}

	return f.hits(ids), nil
}

func (f *fakeRecaller) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([][]string(nil), f.batches...)
}

func TestTouchBatchesAtTheConfiguredSize(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller, BatchSize: 3, Window: time.Hour})

	for i := range 3 {
		if err := tap.Touch(context.Background(), fmt.Sprintf("b/%d", i)); err != nil {
			t.Fatalf("Touch failed: %s", err.Error())
		}
	}

	calls := recaller.calls()
	if len(calls) != 1 {
		t.Fatalf("expected one call once the batch filled, got %d", len(calls))
	}

	if len(calls[0]) != 3 {
		t.Errorf("expected three ids in the call, got %d", len(calls[0]))
	}
}

// Deduplication inside the window is what makes the hit rate a count of distinct memories rather
// than of requests, which is what somebody reading it wants when one object is being hammered.
func TestTouchDeduplicatesWithinAWindow(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller, BatchSize: 100, Window: time.Hour})

	for range 10 {
		if err := tap.Touch(context.Background(), "b/one"); err != nil {
			t.Fatalf("Touch failed: %s", err.Error())
		}
	}

	if err := tap.Flush(context.Background()); err != nil {
		t.Fatalf("Flush failed: %s", err.Error())
	}

	calls := recaller.calls()
	if len(calls) != 1 || len(calls[0]) != 1 {
		t.Fatalf("expected one call carrying one id, got %v", calls)
	}
}

func TestABatchSizeOfZeroRecallsImmediately(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller})

	for i := range 3 {
		if err := tap.Touch(context.Background(), fmt.Sprintf("b/%d", i)); err != nil {
			t.Fatalf("Touch failed: %s", err.Error())
		}
	}

	if got := len(recaller.calls()); got != 3 {
		t.Errorf("expected one call per read, got %d", got)
	}
}

func TestFlushOnAnEmptyBufferCallsNothing(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller, BatchSize: 10, Window: time.Hour})

	if err := tap.Flush(context.Background()); err != nil {
		t.Fatalf("Flush failed: %s", err.Error())
	}

	if got := len(recaller.calls()); got != 0 {
		t.Errorf("expected no call, got %d", got)
	}
}

func TestAnEmptyIdIsIgnored(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller})

	if err := tap.Touch(context.Background(), ""); err != nil {
		t.Fatalf("Touch failed: %s", err.Error())
	}

	if got := len(recaller.calls()); got != 0 {
		t.Errorf("expected no call for an empty id, got %d", got)
	}
}

func TestAFailedRecallIsReported(t *testing.T) {
	recaller := &fakeRecaller{err: fmt.Errorf("the service is down")}
	tap := New(Config{Recaller: recaller})

	if err := tap.Touch(context.Background(), "b/one"); err == nil {
		t.Error("expected the failure to surface")
	}
}

// Run is the ticker half: a partial batch must not sit in the buffer indefinitely, and a cancelled
// context must still flush what is there.
func TestRunFlushesOnTheWindow(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller, BatchSize: 100, Window: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go tap.Run(ctx)

	if err := tap.Touch(ctx, "b/one"); err != nil {
		t.Fatalf("Touch failed: %s", err.Error())
	}

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if len(recaller.calls()) > 0 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Error("expected the window to flush the partial batch")
}

func TestRunFlushesOnceMoreAtShutdown(t *testing.T) {
	recaller := &fakeRecaller{}
	tap := New(Config{Recaller: recaller, BatchSize: 100, Window: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		tap.Run(ctx)
		close(done)
	}()

	if err := tap.Touch(context.Background(), "b/one"); err != nil {
		t.Fatalf("Touch failed: %s", err.Error())
	}

	cancel()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")

	}

	if got := len(recaller.calls()); got != 1 {
		t.Errorf("expected the shutdown flush to send the partial batch, got %d calls", got)
	}
}

func TestRunReturnsImmediatelyWhenBatchingIsOff(t *testing.T) {
	tap := New(Config{Recaller: &fakeRecaller{}})

	done := make(chan struct{})

	go func() {
		tap.Run(context.Background())
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(time.Second):
		t.Error("expected Run to return at once with batching disabled")

	}
}

// The hit-rate report is the only detector for a derivation mismatch, so it has to fire on a run of
// pure misses and stay quiet on a working tap.
func TestTheZeroHitReportFiresOnlyOnAMeaningfulRunOfMisses(t *testing.T) {
	recaller := &fakeRecaller{hits: func(ids []string) int { return 0 }}
	tap := New(Config{Recaller: recaller})

	// Fewer ids than the minimum, however long ago the last report was: a quiet gateway that missed
	// twice is not a misconfiguration.
	tap.reportedAt = time.Now().Add(-2 * reportInterval)

	tap.report(reportMinimumIds-1, 0)

	if tap.everReported {
		t.Error("expected no report below the minimum number of ids")
	}

	tap.reportedAt = time.Now().Add(-2 * reportInterval)

	tap.report(reportMinimumIds, 0)

	if !tap.everReported {
		t.Error("expected a run of misses to be reported")
	}
}

func TestTheReportStaysQuietInsideItsInterval(t *testing.T) {
	tap := New(Config{Recaller: &fakeRecaller{}})

	tap.report(reportMinimumIds*10, 0)

	if tap.everReported {
		t.Error("expected nothing to be reported inside the interval")
	}

	if tap.sinceIds != reportMinimumIds*10 {
		t.Errorf("expected the ids to accumulate, got %d", tap.sinceIds)
	}
}
