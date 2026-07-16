package hippocampus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// fakeObjectStore is an in-memory archive.ObjectStore.
type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: make(map[string][]byte)}
}

func (f *fakeObjectStore) Put(ctx context.Context, key string, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.objects[key] = b

	return nil
}

func (f *fakeObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	b, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("object '%s' not found", key)
	}

	return io.NopCloser(bytes.NewReader(b)), nil
}

// newTransferTestServer builds a Server over an in-memory database, without autoSleep, with a
// small batch size so the pagination paths are exercised by small fixtures.
func newTransferTestServer(t *testing.T, objects *fakeObjectStore) *Server {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}

	s := &Server{
		db:        database,
		manifests: make(map[string]*transferManifest),
		transfer:  Transfer{batchSize: 2},
	}

	if objects != nil {
		s.objects = objects
	}

	return s
}

// seedTransferFixture stores two linked events and three memories carrying every piece of state
// an archive must preserve (recall history, group, summary flag, event links, relationships).
func seedTransferFixture(t *testing.T, s *Server) {
	t.Helper()

	events := []types.Event{
		{Id: "e1", Name: "one", TimeStart: 100, TimeEnd: 200, Significance: 5, Group: "billing",
			Relationships: []types.Relationship{{EventId: "e2", Significance: 3}}},
		{Id: "e2", Name: "two", TimeStart: 300, Significance: 4},
	}

	for _, e := range events {
		if _, err := s.db.CreateEvent(e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 110, Significance: 5, EventId: "e1", Body: "first", Group: "billing"},
		{Id: "m2", TimeStamp: 120, Significance: 6, EventId: "e1", Body: "second", IsSummary: true},
		{Id: "m3", TimeStamp: 130, Significance: 7, Body: "eventless"},
	}

	for _, m := range memories {
		if _, err := s.db.CreateMemory(m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// Give m1 recall history that must survive the round trip.
	if _, err := s.db.RecallMemories([]string{"m1"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}
}

// TestExportImportRoundTrip verifies the full S3 path: export from one instance, import into a
// fresh one, with every piece of state preserved and a re-import staying idempotent.
func TestExportImportRoundTrip(t *testing.T) {
	objects := newFakeObjectStore()

	source := newTransferTestServer(t, objects)
	seedTransferFixture(t, source)

	exported, err := source.Export(context.Background(), &contract.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	if exported.GetEventsExported() != 2 || exported.GetMemoriesExported() != 3 {
		t.Errorf("expected 2 events and 3 memories exported, got %v", exported)
	}

	if exported.GetManifestId() == "" || exported.GetObjectKey() == "" {
		t.Errorf("expected a manifest id and object key, got %v", exported)
	}

	// The export must not have deleted anything (clear was not requested).
	if with, without := source.db.CountMemories(); with+without != 3 {
		t.Errorf("export without clear must leave the store untouched, got %d memories", with+without)
	}

	target := newTransferTestServer(t, objects)

	for range 2 { // twice: re-importing the same archive must be idempotent
		imported, err := target.Import(context.Background(), &contract.ImportRequest{ObjectKey: exported.GetObjectKey()})
		if err != nil {
			t.Fatalf("Import: %s", err)
		}

		if imported.GetEventsImported() != 2 || imported.GetMemoriesImported() != 3 {
			t.Errorf("expected 2 events and 3 memories imported, got %v", imported)
		}
	}

	event, err := target.db.GetEvent("e1")
	if err != nil {
		t.Fatalf("GetEvent(e1): %s", err)
	}

	if event.Group != "billing" || event.TimeEnd != 200 || event.RelationshipSignificance != 3 || len(event.Relationships) != 1 {
		t.Errorf("event state not preserved through the archive: %+v", event)
	}

	memories, err := target.db.GetMemoriesByIds([]string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 3 {
		t.Fatalf("expected 3 memories in the target, got %d", len(*memories))
	}

	for _, m := range *memories {
		switch m.Id {

		case "m1":
			if m.RecallCount != 1 || m.TimeRecalled == 0 || m.Group != "billing" || m.EventId != "e1" {
				t.Errorf("m1 state not preserved: %+v", m)
			}

		case "m2":
			if !m.IsSummary || m.Body != "second" {
				t.Errorf("m2 state not preserved: %+v", m)
			}

		case "m3":
			if m.EventId != "" || m.TimeStamp != 130 {
				t.Errorf("m3 state not preserved: %+v", m)
			}
		}
	}
}

// TestClearRespectsActivitySinceCapture verifies the move semantics: records captured by an
// export are deleted by Clear, but a memory recalled after the capture — and the event that
// still owns it — survive to the next run.
func TestClearRespectsActivitySinceCapture(t *testing.T) {
	objects := newFakeObjectStore()

	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	exported, err := s.Export(context.Background(), &contract.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	// m2 is recalled between the capture and the clear; it and its event must survive.
	if _, err := s.db.RecallMemories([]string{"m2"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	cleared, err := s.Clear(context.Background(), &contract.ClearRequest{ManifestId: exported.GetManifestId()})
	if err != nil {
		t.Fatalf("Clear: %s", err)
	}

	if cleared.GetMemoriesCleared() != 2 {
		t.Errorf("expected 2 memories cleared (m2 protected), got %v", cleared)
	}

	if cleared.GetEventsCleared() != 1 {
		t.Errorf("expected only e2 cleared (e1 still owns m2), got %v", cleared)
	}

	if _, err := s.db.GetEvent("e1"); err != nil {
		t.Error("e1 still owns the recalled m2 and must survive the clear")
	}

	if _, err := s.db.GetEvent("e2"); err == nil {
		t.Error("e2 was captured and empty, it should have been cleared")
	}

	// The manifest is consumed: a second clear reports it unknown.
	if _, err := s.Clear(context.Background(), &contract.ClearRequest{ManifestId: exported.GetManifestId()}); status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for a consumed manifest, got %v", err)
	}
}

// TestExportWithClearFlag verifies the one-shot move: a successful export with clear set deletes
// everything it captured in the same call.
func TestExportWithClearFlag(t *testing.T) {
	objects := newFakeObjectStore()

	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	exported, err := s.Export(context.Background(), &contract.ExportRequest{Clear: true})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	if exported.GetMemoriesCleared() != 3 || exported.GetEventsCleared() != 2 {
		t.Errorf("expected the whole fixture cleared, got %v", exported)
	}

	if with, without := s.db.CountMemories(); with+without != 0 {
		t.Errorf("expected an empty store after export with clear, got %d memories", with+without)
	}
}

// failClearStore wraps a real db.Store but forces ClearMemories to fail, so the clear arm of
// Export/Transfer can be driven down its error path.
type failClearStore struct {
	db.Store
	err error
}

func (f failClearStore) ClearMemories(snapshots []db.MemoryRecallSnapshot) (int, error) {
	return 0, f.err
}

// TestExportWithClearFailure_CachesManifestForRetry is a regression test: when the
// clear step of an Export fails, the manifest must be cached (not consumed) so the caller can retry
// the delete with the returned manifest id. The previous code took (removed) the manifest before
// clearing and never put it back on failure, so the id in the response was already useless.
func TestExportWithClearFailure_CachesManifestForRetry(t *testing.T) {
	objects := newFakeObjectStore()

	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	wantErr := errors.New("clear failed")
	s.db = failClearStore{Store: s.db, err: wantErr}

	res, err := s.Export(context.Background(), &contract.ExportRequest{Clear: true})
	if err == nil {
		t.Fatal("Export swallowed the ClearMemories failure; expected an error")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("expected the ClearMemories error to propagate, got %v", err)
	}

	if res.GetManifestId() == "" {
		t.Fatal("Export returned no manifest id after a failed clear")
	}

	// The export itself succeeded, so the response still reports what was exported.
	if res.GetMemoriesExported() != 3 || res.GetEventsExported() != 2 {
		t.Errorf("expected the export counts populated, got %v", res)
	}

	// The manifest must still be cached so Clear can retry it.
	if s.takeManifest(res.GetManifestId()) == nil {
		t.Error("Export did not cache the manifest after a failed clear; the returned id is unusable for retry")
	}

	// Nothing should have been cleared.
	if with, without := s.db.CountMemories(); with+without != 3 {
		t.Errorf("expected the store untouched after a failed clear, got %d memories", with+without)
	}
}

// TestExportWithClearConcurrent_NoPanic drives many concurrent Export-with-clear runs. The previous
// store-then-take round trip could evict a just-stored manifest before taking it (beyond
// manifestCacheLimit concurrent runs), passing nil into clearManifest and panicking; clearing the
// local manifest directly removes that window. Run under -race, this must complete without panicking.
func TestExportWithClearConcurrent_NoPanic(t *testing.T) {
	objects := newFakeObjectStore()

	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	const runs = manifestCacheLimit * 3

	var wg sync.WaitGroup

	for i := 0; i < runs; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// The first run clears the fixture; the rest capture empty/partial manifests. Either
			// way none may panic, which is the property under test.
			_, _ = s.Export(context.Background(), &contract.ExportRequest{Clear: true})
		}()
	}

	wg.Wait()
}

// TestClearFailureCachesManifestForRetry is a regression test: when the standalone
// Clear RPC's delete step fails, the manifest must be re-cached (takeManifest already removed it) so
// the caller can retry with the same id instead of getting NotFound. The retry, against a working
// store, must then find the manifest and clear the captured records.
func TestClearFailureCachesManifestForRetry(t *testing.T) {
	objects := newFakeObjectStore()

	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	// Export without clear caches a manifest that Clear can then act on.
	exp, err := s.Export(context.Background(), &contract.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	manifestId := exp.GetManifestId()
	if manifestId == "" {
		t.Fatal("Export returned no manifest id")
	}

	real := s.db
	wantErr := errors.New("clear failed")
	s.db = failClearStore{Store: real, err: wantErr}

	res, err := s.Clear(context.Background(), &contract.ClearRequest{ManifestId: manifestId})
	if err == nil {
		t.Fatal("Clear swallowed the ClearMemories failure; expected an error")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("expected the ClearMemories error to propagate, got %v", err)
	}

	if res.GetMemoriesCleared() != 0 || res.GetEventsCleared() != 0 {
		t.Errorf("expected nothing reported cleared on failure, got %v", res)
	}

	// Nothing should have been cleared, and the store must be untouched.
	if with, without := real.CountMemories(); with+without != 3 {
		t.Errorf("expected the store untouched after a failed clear, got %d memories", with+without)
	}

	// The manifest must still be usable: a retry with a working store finds it (not NotFound) and
	// clears the captured records.
	s.db = real

	retry, err := s.Clear(context.Background(), &contract.ClearRequest{ManifestId: manifestId})
	if err != nil {
		t.Fatalf("retry Clear after a failed clear should find the re-cached manifest, got: %s", err)
	}

	if retry.GetMemoriesCleared() != 3 || retry.GetEventsCleared() != 2 {
		t.Errorf("expected the retry to clear the captured records (3 memories, 2 events), got %v", retry)
	}

	if with, without := real.CountMemories(); with+without != 0 {
		t.Errorf("expected the store cleared after the retry, got %d memories", with+without)
	}
}

// TestTransferDirect verifies the direct gRPC path end to end against a real in-process target
// instance: full state lands in the target and the clear flag empties the source.
func TestTransferDirect(t *testing.T) {
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

	transferred, err := source.Transfer(context.Background(), &contract.TransferRequest{Clear: true})
	if err != nil {
		t.Fatalf("Transfer: %s", err)
	}

	if transferred.GetEventsTransferred() != 2 || transferred.GetMemoriesTransferred() != 3 {
		t.Errorf("expected 2 events and 3 memories transferred, got %v", transferred)
	}

	if transferred.GetMemoriesCleared() != 3 || transferred.GetEventsCleared() != 2 {
		t.Errorf("expected the source cleared, got %v", transferred)
	}

	if with, without := source.db.CountMemories(); with+without != 0 {
		t.Errorf("expected an empty source store, got %d memories", with+without)
	}

	memory := func(id string) *types.Memory {
		memories, err := target.db.GetMemoriesByIds([]string{id})
		if err != nil || len(*memories) != 1 {
			return nil
		}

		return &(*memories)[0]
	}

	if m := memory("m1"); m == nil || m.RecallCount != 1 || m.Group != "billing" {
		t.Errorf("m1 did not arrive in the target with its state, got %+v", m)
	}

	if event, err := target.db.GetEvent("e1"); err != nil || event.Group != "billing" || event.RelationshipSignificance != 3 {
		t.Errorf("e1 did not arrive in the target with its state, got %+v (%v)", event, err)
	}
}

// TestTransferSurfacePreconditions verifies the unconfigured-feature and bad-input failure modes.
func TestTransferSurfacePreconditions(t *testing.T) {
	s := newTransferTestServer(t, nil)

	if _, err := s.Export(context.Background(), &contract.ExportRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Export without an object store should be FailedPrecondition, got %v", err)
	}

	if _, err := s.Import(context.Background(), &contract.ImportRequest{ObjectKey: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Import without an object store should be FailedPrecondition, got %v", err)
	}

	if _, err := s.Transfer(context.Background(), &contract.TransferRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Transfer without a target should be FailedPrecondition, got %v", err)
	}

	if _, err := s.Clear(context.Background(), &contract.ClearRequest{ManifestId: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("Clear with an unknown manifest should be NotFound, got %v", err)
	}

	if _, err := s.ImportBatch(context.Background(), &contract.ImportBatchRequest{
		Memories: []*contract.Memory{{Body: "no id"}},
	}); err == nil {
		t.Error("ImportBatch must reject a memory without an id")
	}
}

// batchTotalSize sums memoryTransferSize over a batch, mirroring the budget accounting.
func batchTotalSize(batch []types.Memory) int {
	total := 0
	for _, memory := range batch {
		total += memoryTransferSize(memory)
	}

	return total
}

// TestBatchMemoriesByBytes_RespectsBudget verifies a page of memories is split so every batch
// (except one holding a single oversized memory) stays within the byte budget, and that no memory
// is dropped or reordered - the guard against Transfer→ImportBatch overflowing the receiver's
// max-receive-message size and failing every retry against the same deterministic page.
func TestBatchMemoriesByBytes_RespectsBudget(t *testing.T) {
	// Ten ~1 KiB memories; a 3 KiB budget forces several batches.
	memories := make([]types.Memory, 10)
	for i := range memories {
		memories[i] = types.Memory{Id: fmt.Sprintf("m%d", i), Body: string(bytes.Repeat([]byte("x"), 1024))}
	}

	budget := 3 * 1024
	batches := batchMemoriesByBytes(memories, budget)

	if len(batches) < 2 {
		t.Fatalf("expected the page to split into multiple batches, got %d", len(batches))
	}

	seen := 0

	for i, batch := range batches {
		if len(batch) == 0 {
			t.Fatalf("batch %d is empty", i)
		}

		if len(batch) > 1 && batchTotalSize(batch) > budget {
			t.Errorf("batch %d exceeds the byte budget: %d > %d", i, batchTotalSize(batch), budget)
		}

		seen += len(batch)
	}

	if seen != len(memories) {
		t.Errorf("expected all %d memories batched, got %d", len(memories), seen)
	}
}

// TestBatchMemoriesByBytes_OversizedMemoryGoesAlone verifies a single memory larger than the whole
// budget is not dropped: it is emitted alone in its own batch (the receiver must accept it).
func TestBatchMemoriesByBytes_OversizedMemoryGoesAlone(t *testing.T) {
	memories := []types.Memory{
		{Id: "small-a", Body: "a"},
		{Id: "huge", Body: string(bytes.Repeat([]byte("x"), 8*1024))},
		{Id: "small-b", Body: "b"},
	}

	batches := batchMemoriesByBytes(memories, 1024)

	// small-a, then huge alone, then small-b: three batches.
	if len(batches) != 3 {
		t.Fatalf("expected 3 batches (small, oversized-alone, small), got %d", len(batches))
	}

	if len(batches[1]) != 1 || batches[1][0].Id != "huge" {
		t.Errorf("expected the oversized memory alone in the second batch, got %+v", batches[1])
	}
}

// TestBatchMemoriesByBytes_DefaultBudget verifies a non-positive budget falls back to the default
// rather than putting every memory in its own batch.
func TestBatchMemoriesByBytes_DefaultBudget(t *testing.T) {
	memories := []types.Memory{{Id: "a", Body: "a"}, {Id: "b", Body: "b"}}

	batches := batchMemoriesByBytes(memories, 0)

	if len(batches) != 1 {
		t.Errorf("expected two tiny memories to share one batch under the default budget, got %d batches", len(batches))
	}
}
