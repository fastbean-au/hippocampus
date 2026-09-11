package reap

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

// fakeHeld answers which ids the store still holds.
type fakeHeld struct {
	holds map[string]bool
	err   error
	asked [][]string
}

func (f *fakeHeld) Held(ctx context.Context, ids []string) (map[string]bool, error) {
	f.asked = append(f.asked, append([]string(nil), ids...))

	if f.err != nil {
		return nil, f.err
	}

	held := map[string]bool{}

	for _, v := range ids {
		if f.holds[v] {
			held[v] = true
		}
	}

	return held, nil
}

func newSweep(t *testing.T, store *objects.Memory, held *fakeHeld, cfg SweepConfig) *Sweep {
	t.Helper()

	reaper, err := New(Config{Store: store, Delete: true})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	cfg.Store = store
	cfg.Memories = held
	cfg.Reaper = reaper

	sweep, err := NewSweep(cfg)
	if err != nil {
		t.Fatalf("NewSweep failed: %s", err.Error())
	}

	return sweep
}

func TestTheSweepDeletesWhatTheStoreNoLongerHolds(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)

	store := objects.NewMemory("payloads")
	store.Put("kept.json", []byte("kept"), old)
	store.Put("orphan.json", []byte("orphan"), old)

	held := &fakeHeld{holds: map[string]bool{"payloads/kept.json": true}}

	summary, err := newSweep(t, store, held, SweepConfig{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if summary.Examined != 2 {
		t.Errorf("expected two objects examined, got %d", summary.Examined)
	}

	if summary.Result.Deleted != 1 {
		t.Errorf("expected one deletion, got %+v", summary.Result)
	}

	if !store.Has("kept.json") {
		t.Error("expected the object whose memory is still held to survive")
	}

	if store.Has("orphan.json") {
		t.Error("expected the orphan to be deleted")
	}
}

// The grace period is what stops the sweep racing a producer that writes the object before it
// writes the memory. It cannot be switched off.
func TestAnObjectInsideTheGracePeriodIsNeverJudged(t *testing.T) {
	store := objects.NewMemory("payloads")
	store.Put("fresh.json", []byte("fresh"), time.Now())

	held := &fakeHeld{holds: map[string]bool{}}

	summary, err := newSweep(t, store, held, SweepConfig{MinAge: time.Hour}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if summary.Skipped != 1 || summary.Result.Deleted != 0 {
		t.Errorf("expected the fresh object to be skipped, got %+v", summary)
	}

	if len(held.asked) != 0 {
		t.Error("expected a fresh object not even to be asked about")
	}

	if !store.Has("fresh.json") {
		t.Error("expected the fresh object to survive")
	}
}

func TestAZeroMinAgeFallsBackToTheDefault(t *testing.T) {
	store := objects.NewMemory("payloads")

	sweep := newSweep(t, store, &fakeHeld{}, SweepConfig{MinAge: 0})

	if sweep.minAge != defaultSweepMinAge {
		t.Errorf("expected the grace period to be un-disableable, got %s", sweep.minAge)
	}
}

// An object this agent could never have minted a memory for says nothing by having none, so it is
// skipped rather than counted as an orphan.
func TestAnUnmappableObjectIsSkipped(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)

	store := objects.NewMemory("payloads")
	store.Put(longKey(), []byte("payload"), old)

	summary, err := newSweep(t, store, &fakeHeld{holds: map[string]bool{}}, SweepConfig{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if summary.Skipped != 1 || summary.Result.Deleted != 0 {
		t.Errorf("expected the unmappable object to be skipped, got %+v", summary)
	}

	if store.Len() != 1 {
		t.Error("expected the unmappable object to survive")
	}
}

// The one thing the sweep must never do: read "cannot ask" as "not held". That mistake empties the
// bucket the first time the service is unreachable.
func TestTheSweepStopsWhenTheStoreCannotBeAsked(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)

	store := objects.NewMemory("payloads")
	store.Put("one.json", []byte("one"), old)
	store.Put("two.json", []byte("two"), old)

	held := &fakeHeld{err: fmt.Errorf("the service is down")}

	if _, err := newSweep(t, store, held, SweepConfig{}).Run(context.Background()); err == nil {
		t.Fatal("expected the failure to stop the sweep")
	}

	if store.Len() != 2 {
		t.Error("expected nothing to be deleted when the store could not be asked")
	}
}

func TestAFailingListIsReported(t *testing.T) {
	store := objects.NewMemory("payloads")
	store.ListErr = fmt.Errorf("access denied")

	if _, err := newSweep(t, store, &fakeHeld{}, SweepConfig{}).Run(context.Background()); err == nil {
		t.Error("expected the listing failure to surface")
	}
}

// The tail of the walk must be judged too, or the last partial batch is never asked about.
func TestTheFinalPartialBatchIsJudged(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)

	store := objects.NewMemory("payloads")

	for i := range 5 {
		store.Put(fmt.Sprintf("object-%d.json", i), []byte("x"), old)
	}

	held := &fakeHeld{holds: map[string]bool{}}

	summary, err := newSweep(t, store, held, SweepConfig{BatchSize: 2}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if summary.Result.Deleted != 5 {
		t.Errorf("expected all five to be judged, got %+v", summary.Result)
	}

	if len(held.asked) != 3 {
		t.Errorf("expected three batches (2+2+1), got %d", len(held.asked))
	}
}

func TestTheSweepHonoursAPrefix(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)

	store := objects.NewMemory("payloads")
	store.Put("mine/one.json", []byte("one"), old)
	store.Put("theirs/two.json", []byte("two"), old)

	held := &fakeHeld{holds: map[string]bool{}}

	summary, err := newSweep(t, store, held, SweepConfig{Prefix: "mine/"}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %s", err.Error())
	}

	if summary.Examined != 1 || summary.Result.Deleted != 1 {
		t.Errorf("expected only the prefix to be swept, got %+v", summary)
	}

	if !store.Has("theirs/two.json") {
		t.Error("expected an object outside the prefix to be untouched")
	}
}

func TestABatchSizeOverTheServiceCapIsClamped(t *testing.T) {
	sweep := newSweep(t, objects.NewMemory("payloads"), &fakeHeld{}, SweepConfig{BatchSize: 10_000})

	if sweep.batchSize != defaultSweepBatch {
		t.Errorf("expected the batch to be clamped to the service's own cap, got %d", sweep.batchSize)
	}
}

func TestNewSweepValidatesItsConfiguration(t *testing.T) {
	store := objects.NewMemory("payloads")

	reaper, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("New failed: %s", err.Error())
	}

	cases := []struct {
		name string
		cfg  SweepConfig
	}{
		{name: "no store", cfg: SweepConfig{Memories: &fakeHeld{}, Reaper: reaper}},
		{name: "no store reader", cfg: SweepConfig{Store: store, Reaper: reaper}},
		{name: "no reaper", cfg: SweepConfig{Store: store, Memories: &fakeHeld{}}},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if _, err := NewSweep(v.cfg); err == nil {
				t.Error("expected the configuration to be refused")
			}
		})
	}
}

func longKey() string {
	key := make([]byte, 300)

	for i := range key {
		key[i] = 'k'
	}

	return string(key)
}
