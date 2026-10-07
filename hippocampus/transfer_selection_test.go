package hippocampus

import (
	"context"
	"fmt"
	"net"
	"sort"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// Export and Transfer narrowed by a MemorySelection (TODO-3 item 159). The archive is what offboards
// a group or answers a subject-access request, and both used to need a specially scoped token to
// export anything less than the whole store - so "export, verify, delete" for one group could not
// be expressed with the predicate deletion that had just made the delete half possible.

// importedIds imports an archive into a fresh store and lists what arrived.
func importedIds(t *testing.T, objects *fakeObjectStore, key string) (events []string, memories []string) {
	t.Helper()

	fresh := newTransferTestServer(t, objects)

	if _, err := fresh.Import(context.Background(), &contract.ImportRequest{ObjectKey: key}); err != nil {
		t.Fatalf("Import: %s", err)
	}

	eventRows, err := fresh.db.GetEvents(context.Background(), db.EventFilter{Limit: 1000})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	for _, v := range *eventRows {
		events = append(events, v.Id)
	}

	memoryRows, err := fresh.GetMemories(context.Background(), &contract.GetMemoriesRequest{Limit: 200})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	for _, v := range memoryRows.GetMemories() {
		memories = append(memories, v.GetId())
	}

	sort.Strings(events)
	sort.Strings(memories)

	return events, memories
}

func TestExport_SelectsMemoriesAndTheirEvents(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		selection    *contract.MemorySelection
		wantEvents   string
		wantMemories string
	}{
		{"no selection is the whole store", nil, "[e1 e2]", "[m1 m2 m3]"},
		{"an empty selection is the whole store", &contract.MemorySelection{}, "[e1 e2]", "[m1 m2 m3]"},
		{"one group", &contract.MemorySelection{Group: "billing"}, "[e1]", "[m1]"},
		{
			"the most significant tier, whose memory has no event",
			&contract.MemorySelection{SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST},
			"[]",
			"[m3]",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objects := newFakeObjectStore()
			s := newTransferTestServer(t, objects)
			seedTransferFixture(t, s)

			res, err := s.Export(context.Background(), &contract.ExportRequest{Memories: c.selection})
			if err != nil {
				t.Fatalf("Export: %s", err)
			}

			events, memories := importedIds(t, objects, res.GetObjectKey())

			if fmt.Sprint(events) != c.wantEvents || fmt.Sprint(memories) != c.wantMemories {
				t.Errorf("archive holds events %v and memories %v, want %s and %s", events, memories, c.wantEvents, c.wantMemories)
			}
		})
	}
}

// TestExport_SelectionByGroupIncludesTheGroupsEmptyEvents: offboarding one group must take its
// events with it, including those holding no memory.
func TestExport_SelectionByGroupIncludesTheGroupsEmptyEvents(t *testing.T) {
	t.Parallel()

	objects := newFakeObjectStore()
	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e-empty", Name: "empty", TimeStart: 100, Significance: 1, Group: "billing"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.Export(context.Background(), &contract.ExportRequest{Memories: &contract.MemorySelection{Group: "billing"}})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	events, _ := importedIds(t, objects, res.GetObjectKey())

	if fmt.Sprint(events) != "[e-empty e1]" {
		t.Errorf("archive holds events %v, want [e-empty e1]", events)
	}
}

// TestExport_SelectionWithClearDeletesOnlyWhatWasSelected is "export, verify, delete" for one group:
// the memory goes, and its event stays because it still holds another group's memory.
func TestExport_SelectionWithClearDeletesOnlyWhatWasSelected(t *testing.T) {
	t.Parallel()

	objects := newFakeObjectStore()
	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	res, err := s.Export(context.Background(), &contract.ExportRequest{Clear: true, Memories: &contract.MemorySelection{Group: "billing"}})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	if res.GetMemoriesCleared() != 1 || res.GetEventsCleared() != 0 {
		t.Errorf("cleared %d memories and %d events, want 1 and 0", res.GetMemoriesCleared(), res.GetEventsCleared())
	}

	remaining, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Limit: 200})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := fmt.Sprint(len(remaining.GetMemories())); got != "2" {
		t.Errorf("%s memories remain, want 2", got)
	}
}

func TestExport_RefusesAnInvalidSelection(t *testing.T) {
	t.Parallel()

	s := newTransferTestServer(t, newFakeObjectStore())

	_, err := s.Export(context.Background(), &contract.ExportRequest{Memories: &contract.MemorySelection{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST,
		SignificanceMin:      3,
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("Export with a contradictory selection = %v, want InvalidArgument", err)
	}
}

func TestTransfer_SelectsMemoriesAndTheirEvents(t *testing.T) {
	t.Parallel()

	target := newTransferTestServer(t, nil)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %s", err)
	}

	grpcServer := grpc.NewServer()
	contract.RegisterHippocampusServer(grpcServer, target)

	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	source := newTransferTestServer(t, nil)
	source.transfer.targetAddress = listener.Addr().String()
	seedTransferFixture(t, source)

	res, err := source.Transfer(context.Background(), &contract.TransferRequest{Memories: &contract.MemorySelection{Group: "billing"}})
	if err != nil {
		t.Fatalf("Transfer: %s", err)
	}

	if res.GetEventsTransferred() != 1 || res.GetMemoriesTransferred() != 1 {
		t.Errorf("transferred %d events and %d memories, want 1 and 1", res.GetEventsTransferred(), res.GetMemoriesTransferred())
	}
}

// TestExport_SelectionPagesPastOneBatch: the selected walk resolves ids in transfer.batchSize pages
// on a keyset cursor, so a selection larger than one page must arrive whole, once each.
func TestExport_SelectionPagesPastOneBatch(t *testing.T) {
	t.Parallel()

	objects := newFakeObjectStore()
	s := newTransferTestServer(t, objects)
	s.transfer.batchSize = 2

	want := make([]string, 0, 5)

	for i := range 5 {
		id := fmt.Sprintf("p%d", i)
		want = append(want, id)

		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, Body: "b", TimeStamp: 1000, Significance: 5, Group: "paged"}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "other", Body: "b", TimeStamp: 1000, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.Export(context.Background(), &contract.ExportRequest{Memories: &contract.MemorySelection{Group: "paged"}})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	if res.GetMemoriesExported() != 5 {
		t.Errorf("memories_exported = %d, want 5", res.GetMemoriesExported())
	}

	_, memories := importedIds(t, objects, res.GetObjectKey())

	if fmt.Sprint(memories) != fmt.Sprint(want) {
		t.Errorf("archive holds %v, want %v", memories, want)
	}
}

// TestExport_SelectionHonoursGroupScope: a selection composes with the caller's scope rather than
// widening it - a caller bound to one group naming another selects nothing.
func TestExport_SelectionHonoursGroupScope(t *testing.T) {
	t.Parallel()

	objects := newFakeObjectStore()
	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	res, err := s.Export(scopedContext("other"), &contract.ExportRequest{Memories: &contract.MemorySelection{Group: "billing"}})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	if res.GetMemoriesExported() != 0 || res.GetEventsExported() != 0 {
		t.Errorf("a caller scoped to 'other' exported %d memories and %d events of 'billing'", res.GetMemoriesExported(), res.GetEventsExported())
	}
}

// TestExport_SelectionIsHeldToTheManifestCap: transfer.maxManifestRows bounds a selected run by
// what it selects, not by the size of the store.
func TestExport_SelectionIsHeldToTheManifestCap(t *testing.T) {
	t.Parallel()

	objects := newFakeObjectStore()
	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	s.transfer.maxManifestRows = 2

	if _, err := s.Export(context.Background(), &contract.ExportRequest{Memories: &contract.MemorySelection{Group: "billing"}}); err != nil {
		t.Errorf("a two-record selection under a cap of two was refused: %s", err)
	}

	_, err := s.Export(context.Background(), &contract.ExportRequest{Memories: &contract.MemorySelection{SignificanceMin: 1}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a five-record selection under a cap of two = %v, want FailedPrecondition", err)
	}
}
