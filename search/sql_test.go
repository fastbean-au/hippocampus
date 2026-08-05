package search

import (
	"context"
	"errors"
	"testing"

	"github.com/fastbean-au/hippocampus/db"
)

// fakeContentStore stands in for *db.DB so the adapter is testable without a database, and records
// what it was asked for so the mapping from Query to db.ContentQuery can be asserted.
type fakeContentStore struct {
	available bool
	ids       []string
	err       error

	gotQuery   db.ContentQuery
	rebuilds   int
	rebuildErr error
}

func (f *fakeContentStore) SearchMemoryIds(ctx context.Context, query db.ContentQuery) ([]string, error) {
	f.gotQuery = query

	return f.ids, f.err
}

func (f *fakeContentStore) ContentSearchAvailable() bool {
	return f.available
}

func (f *fakeContentStore) RebuildContentSearch(ctx context.Context) error {
	f.rebuilds++

	return f.rebuildErr
}

func TestNewSQLRequiresAnAvailableStore(t *testing.T) {
	if _, err := NewSQL(&fakeContentStore{available: false}); !errors.Is(err, db.ErrContentSearchUnavailable) {
		t.Errorf("NewSQL on an unsupported store: got %v, want ErrContentSearchUnavailable", err)
	}

	idx, err := NewSQL(&fakeContentStore{available: true})
	if err != nil {
		t.Fatalf("NewSQL on a supported store: %s", err)
	}

	if !idx.Enabled() {
		t.Error("a constructed SQL index reports itself disabled")
	}
}

func TestSQLSearchPassesTheQueryThrough(t *testing.T) {
	store := &fakeContentStore{available: true, ids: []string{"m1", "m2"}}

	idx, err := NewSQL(store)
	if err != nil {
		t.Fatalf("NewSQL: %s", err)
	}

	query := Query{Text: "deployment", EventId: "e1", Group: "ops", Limit: 7}

	ids, err := idx.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search: %s", err)
	}

	if len(ids) != 2 || ids[0] != "m1" {
		t.Errorf("Search returned %v, want [m1 m2]", ids)
	}

	want := db.ContentQuery{Text: "deployment", EventId: "e1", Group: "ops", Limit: 7}
	if store.gotQuery != want {
		t.Errorf("store received %+v, want %+v", store.gotQuery, want)
	}
}

func TestSQLSearchPropagatesTheStoreError(t *testing.T) {
	failure := errors.New("store is unwell")
	store := &fakeContentStore{available: true, err: failure}

	idx, err := NewSQL(store)
	if err != nil {
		t.Fatalf("NewSQL: %s", err)
	}

	if _, err := idx.Search(context.Background(), Query{Text: "anything"}); !errors.Is(err, failure) {
		t.Errorf("Search: got %v, want the store's error", err)
	}
}

func TestSQLRebuildDelegates(t *testing.T) {
	store := &fakeContentStore{available: true}

	idx, err := NewSQL(store)
	if err != nil {
		t.Fatalf("NewSQL: %s", err)
	}

	if err := idx.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %s", err)
	}

	if store.rebuilds != 1 {
		t.Errorf("store saw %d rebuilds, want 1", store.rebuilds)
	}

	failure := errors.New("rebuild failed")
	store.rebuildErr = failure

	if err := idx.Rebuild(context.Background()); !errors.Is(err, failure) {
		t.Errorf("Rebuild: got %v, want the store's error", err)
	}
}

// The mutators are no-ops by design - the index is maintained inside the primary write - so this
// pins that they neither panic nor reach the store, which would double-index.
func TestSQLMutatorsAreInert(t *testing.T) {
	store := &fakeContentStore{available: true}

	idx, err := NewSQL(store)
	if err != nil {
		t.Fatalf("NewSQL: %s", err)
	}

	idx.IndexMemory(Doc{Id: "m1", Body: "something"})
	idx.DeleteMemories([]string{"m1"})
	idx.DeleteByEventId("e1")
	idx.SetEventId("e1", "e2")
	idx.Purge()

	if store.gotQuery != (db.ContentQuery{}) {
		t.Errorf("a mutator reached the store: %+v", store.gotQuery)
	}

	if store.rebuilds != 0 {
		t.Errorf("a mutator triggered %d rebuilds, want 0", store.rebuilds)
	}

	if err := idx.Close(); err != nil {
		t.Errorf("Close: %s", err)
	}
}
