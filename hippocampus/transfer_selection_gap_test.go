package hippocampus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/db/dbtest"
	"github.com/fastbean-au/hippocampus/types"
)

// This file targets transfer_selection.go's remaining coverage gaps: a selected walk over a store
// that fails, or loses records, at one named step, and the manifest cap applied to the events a
// selection brings with it.

var errSelectionStore = errors.New("selection store unavailable")

// selectionStore fails the method named by fail, or, for the two "vanished" steps, returns an empty
// page as a store does for records deleted since their ids were read.
type selectionStore struct {
	db.Store

	fail string
}

func (f *selectionStore) MemoryIdsMatching(ctx context.Context, filter db.MemoryFilter) ([]string, error) {
	if f.fail == "MemoryIdsMatching" {
		return nil, errSelectionStore
	}

	return f.Store.MemoryIdsMatching(ctx, filter)
}

func (f *selectionStore) EventIdsForMemories(ctx context.Context, ids []string) ([]string, error) {
	if f.fail == "EventIdsForMemories" {
		return nil, errSelectionStore
	}

	return f.Store.EventIdsForMemories(ctx, ids)
}

func (f *selectionStore) EventIdsMatching(ctx context.Context, filter db.EventFilter) ([]string, error) {
	if f.fail == "EventIdsMatching" {
		return nil, errSelectionStore
	}

	return f.Store.EventIdsMatching(ctx, filter)
}

func (f *selectionStore) GetEvents(ctx context.Context, filter db.EventFilter) (*[]types.Event, error) {
	switch f.fail {

	case "GetEvents":
		return nil, errSelectionStore

	case "events vanished":
		return &[]types.Event{}, nil

	}

	return f.Store.GetEvents(ctx, filter)
}

func (f *selectionStore) GetMemoriesByIds(ctx context.Context, ids []string) (*[]types.Memory, error) {
	switch f.fail {

	case "GetMemoriesByIds":
		return nil, errSelectionStore

	case "memories vanished":
		return &[]types.Memory{}, nil

	}

	return f.Store.GetMemoriesByIds(ctx, ids)
}

// selectionServer is a server over a store holding group g: memory m1 in event e1, a loose memory
// m2, and an empty event e2, with the named step failing.
func selectionServer(t *testing.T, fail string) *Server {
	t.Helper()

	store := dbtest.Open(t)
	ctx := context.Background()

	for _, id := range []string{"e1", "e2"} {
		if _, err := store.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 5, Group: "g"}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	memories := []types.Memory{
		{Id: "m1", EventId: "e1", Body: "x", TimeStamp: 100, Significance: 5, Group: "g"},
		{Id: "m2", Body: "x", TimeStamp: 100, Significance: 5, Group: "g"},
	}

	for _, m := range memories {
		if _, err := store.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	return &Server{db: &selectionStore{Store: store, fail: fail}}
}

// noRows accepts a page and does nothing with it.
func noRows[T any](_ []T) error {
	return nil
}

// TestWalkSelection_StoreFailuresStopTheWalk: a failure at any step of a selected walk ends it with
// that failure rather than writing a partial archive.
func TestWalkSelection_StoreFailuresStopTheWalk(t *testing.T) {
	t.Parallel()

	steps := []string{"MemoryIdsMatching", "EventIdsForMemories", "EventIdsMatching", "GetEvents", "GetMemoriesByIds"}

	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			t.Parallel()

			s := selectionServer(t, step)

			_, _, _, err := s.walkSelection(context.Background(), db.MemoryFilter{Group: "g"}, noRows[types.Event], noRows[types.Memory])
			if !errors.Is(err, errSelectionStore) {
				t.Errorf("a failing %s = %v, want its failure", step, err)
			}
		})
	}
}

// TestWalkSelection_WriterFailuresStopTheWalk: a page the archive writer refuses ends the walk with
// the writer's failure.
func TestWalkSelection_WriterFailuresStopTheWalk(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	selection := db.MemoryFilter{Group: "g"}

	failEvents := func(_ []types.Event) error { return errSelectionStore }
	failMemories := func(_ []types.Memory) error { return errSelectionStore }

	if _, _, _, err := selectionServer(t, "").walkSelection(ctx, selection, failEvents, noRows[types.Memory]); !errors.Is(err, errSelectionStore) {
		t.Errorf("a refused events page = %v, want the writer's failure", err)
	}

	if _, _, _, err := selectionServer(t, "").walkSelection(ctx, selection, noRows[types.Event], failMemories); !errors.Is(err, errSelectionStore) {
		t.Errorf("a refused memories page = %v, want the writer's failure", err)
	}
}

// TestWalkSelection_RecordsDeletedMidWalkAreAbsent: a record deleted between its id being read and its
// page being fetched is simply left out of the archive, as the whole-store walk leaves it.
func TestWalkSelection_RecordsDeletedMidWalkAreAbsent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	selection := db.MemoryFilter{Group: "g"}

	manifest, events, memories, err := selectionServer(t, "events vanished").walkSelection(ctx, selection, noRows[types.Event], noRows[types.Memory])
	if err != nil || events != 0 || memories != 2 || len(manifest.eventIds) != 0 {
		t.Errorf("with the events gone the walk = (%d events, %d memories, %v), want (0, 2, nil)", events, memories, err)
	}

	manifest, events, memories, err = selectionServer(t, "memories vanished").walkSelection(ctx, selection, noRows[types.Event], noRows[types.Memory])
	if err != nil || events != 2 || memories != 0 || len(manifest.memories) != 0 {
		t.Errorf("with the memories gone the walk = (%d events, %d memories, %v), want (2, 0, nil)", events, memories, err)
	}
}

// TestWalkSelection_TheCapCountsTheEventsASelectionBrings: the manifest cap bounds the selected
// memories and the events they carry together, so a selection within the cap on memories alone is
// still refused when its events take it over.
func TestWalkSelection_TheCapCountsTheEventsASelectionBrings(t *testing.T) {
	t.Parallel()

	s := selectionServer(t, "")
	s.transfer.maxManifestRows = 3

	_, _, _, err := s.walkSelection(context.Background(), db.MemoryFilter{Group: "g"}, noRows[types.Event], noRows[types.Memory])
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "4 records") {
		t.Errorf("two memories and two events under a cap of three = %v, want FailedPrecondition for 4 records", err)
	}
}

// TestRedactEndpointStripsCredentialsForMain: the exported form main logs through strips what the
// topology view strips.
func TestRedactEndpointStripsCredentialsForMain(t *testing.T) {
	t.Parallel()

	if got := RedactEndpoint("postgres://user:secret@db:5432/hippocampus?sslmode=disable"); strings.Contains(got, "secret") {
		t.Errorf("RedactEndpoint kept the password: %q", got)
	}
}
