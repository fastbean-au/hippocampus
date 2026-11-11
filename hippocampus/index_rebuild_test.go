package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
)

// rebuildStore wraps a real db.Store and scripts RebuildBloatedIndexes, recording the policy each
// pass asked with.
type rebuildStore struct {
	db.Store

	policies []db.IndexRebuildPolicy
	answer   []db.IndexRebuild
	err      error
}

func (r *rebuildStore) RebuildBloatedIndexes(ctx context.Context, policy db.IndexRebuildPolicy) ([]db.IndexRebuild, error) {
	r.policies = append(r.policies, policy)

	return r.answer, r.err
}

func newRebuildServer(store *rebuildStore, cfg IndexRebuildConfig) *Server {
	return &Server{
		db:                   store,
		indexRebuild:         cfg,
		indexRebuildFailures: make(map[string]time.Time),
	}
}

// TestRebuildBloatedIndexesIsOffUnlessEnabled holds the default: a deployment that has not set
// storage.indexRebuild.enabled never has an index rebuilt, which is the behaviour before the key.
func TestRebuildBloatedIndexesIsOffUnlessEnabled(t *testing.T) {
	t.Parallel()

	store := &rebuildStore{}
	s := newRebuildServer(store, IndexRebuildConfig{MinBytes: 16 << 20, MaxBytesPerEntry: 512})

	s.rebuildBloatedIndexes(context.Background())

	if len(store.policies) != 0 {
		t.Errorf("a disabled pass asked the store %d times", len(store.policies))
	}
}

// TestRebuildBloatedIndexesPassesTheConfiguredBounds holds the configuration to the policy the store
// is asked with, so the bounds an operator set are the ones applied.
func TestRebuildBloatedIndexesPassesTheConfiguredBounds(t *testing.T) {
	t.Parallel()

	store := &rebuildStore{}
	s := newRebuildServer(store, IndexRebuildConfig{Enabled: true, MinBytes: 16 << 20, MaxBytesPerEntry: 512})

	s.rebuildBloatedIndexes(context.Background())

	if len(store.policies) != 1 {
		t.Fatalf("an enabled pass asked the store %d times, want 1", len(store.policies))
	}

	policy := store.policies[0]

	if policy.MinBytes != 16<<20 || policy.MaxBytesPerEntry != 512 {
		t.Errorf("the store was asked with %d bytes and %d bytes an entry", policy.MinBytes, policy.MaxBytesPerEntry)
	}

	if len(policy.Skip) != 0 {
		t.Errorf("a first pass held back %v", policy.Skip)
	}
}

// TestRebuildBloatedIndexesHoldsBackAFailedIndex covers the retry wait. The likeliest failure is a
// role that does not own the table, which no number of retries fixes, so a failed index must be
// held back rather than warned about on every cycle - and tried again once the wait is over.
//
// Not parallel: it shortens indexRebuildRetry, a package timing variable.
func TestRebuildBloatedIndexesHoldsBackAFailedIndex(t *testing.T) {
	saved := indexRebuildRetry
	t.Cleanup(func() { indexRebuildRetry = saved })

	indexRebuildRetry = time.Hour

	store := &rebuildStore{
		answer: []db.IndexRebuild{
			{Table: "memories", Index: "idx_memories_listing_v1", BytesBefore: 1 << 30, Err: errors.New("must be owner of table memories")},
			{Table: "memories", Index: "memories_pkey", BytesBefore: 1 << 30, BytesAfter: 1 << 20},
		},
	}
	s := newRebuildServer(store, IndexRebuildConfig{Enabled: true, MinBytes: 16 << 20, MaxBytesPerEntry: 512})

	s.rebuildBloatedIndexes(context.Background())

	store.answer = nil

	s.rebuildBloatedIndexes(context.Background())

	held := store.policies[1].Skip

	if !held["memories.idx_memories_listing_v1"] {
		t.Errorf("the failed index was not held back on the next pass: %v", held)
	}

	if held["memories.memories_pkey"] {
		t.Error("an index that rebuilt cleanly was held back")
	}

	indexRebuildRetry = 0

	s.rebuildBloatedIndexes(context.Background())

	if len(store.policies[2].Skip) != 0 {
		t.Errorf("the failed index was still held back once its wait was over: %v", store.policies[2].Skip)
	}

	if len(s.indexRebuildFailures) != 0 {
		t.Errorf("the failure record was kept after its wait was over: %v", s.indexRebuildFailures)
	}
}

// TestRebuildBloatedIndexesSurvivesAMeasurementFailure holds the pass to best-effort: a store that
// cannot be measured is a cycle that carries on, not one that fails.
func TestRebuildBloatedIndexesSurvivesAMeasurementFailure(t *testing.T) {
	t.Parallel()

	store := &rebuildStore{err: errors.New("catalogue unavailable")}
	s := newRebuildServer(store, IndexRebuildConfig{Enabled: true, MinBytes: 16 << 20, MaxBytesPerEntry: 512})

	s.rebuildBloatedIndexes(context.Background())

	if len(s.indexRebuildFailures) != 0 {
		t.Errorf("a measurement failure was recorded against an index: %v", s.indexRebuildFailures)
	}
}
