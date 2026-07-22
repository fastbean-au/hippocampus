package hippocampus

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/archive"
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

// importConflictStore wraps a real db.Store but forces ImportEvents to fail with a raw MySQL
// deadlock error - the unwrapped form a multi-statement transfer transaction surfaces (withWriteRetry
// only wraps single-statement execs in db.ErrWriteConflict). It stands in for a live MySQL deadlock so
// the transfer surface's error mapping can be exercised without a MySQL server.
type importConflictStore struct {
	db.Store
}

func (importConflictStore) ImportEvents(ctx context.Context, events []types.Event) (int, error) {
	return 0, &mysql.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"}
}

// TestImportBatch_WriteConflictMapsToAborted is a regression test: a transfer transaction that hit a
// MySQL deadlock surfaced its raw driver error as a gRPC Unknown, so a client could not tell the
// write was retryable. ImportBatch must now map it to Aborted.
func TestImportBatch_WriteConflictMapsToAborted(t *testing.T) {
	s := newTransferTestServer(t, nil)
	s.db = importConflictStore{Store: s.db}

	_, err := s.ImportBatch(context.Background(), &contract.ImportBatchRequest{
		Events: []*contract.Event{{Id: "e1", Name: "trip", Significance: 5}},
	})
	if err == nil {
		t.Fatal("expected ImportBatch to return the write conflict")
	}

	if got := status.Code(err); got != codes.Aborted {
		t.Errorf("expected codes.Aborted, got %s (%v)", got, err)
	}
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
		if _, err := s.db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 110, Significance: 5, EventId: "e1", Body: "first", Group: "billing"},
		{Id: "m2", TimeStamp: 120, Significance: 6, EventId: "e1", Body: "second", IsSummary: true},
		{Id: "m3", TimeStamp: 130, Significance: 7, Body: "eventless"},
	}

	for _, m := range memories {
		if _, err := s.db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// Give m1 recall history that must survive the round trip.
	if _, err := s.db.RecallMemories(context.Background(), []string{"m1"}); err != nil {
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
	if with, without := source.db.CountMemories(context.Background()); with+without != 3 {
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

	event, err := target.db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent(e1): %s", err)
	}

	if event.Group != "billing" || event.TimeEnd != 200 || event.RelationshipSignificance != 3 || len(event.Relationships) != 1 {
		t.Errorf("event state not preserved through the archive: %+v", event)
	}

	memories, err := target.db.GetMemoriesByIds(context.Background(), []string{"m1", "m2", "m3"})
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
	if _, err := s.db.RecallMemories(context.Background(), []string{"m2"}); err != nil {
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

	if _, err := s.db.GetEvent(context.Background(), "e1"); err != nil {
		t.Error("e1 still owns the recalled m2 and must survive the clear")
	}

	if _, err := s.db.GetEvent(context.Background(), "e2"); err == nil {
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

	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("expected an empty store after export with clear, got %d memories", with+without)
	}
}

// TestExportRefusesOverManifestCap verifies that when transfer.maxManifestRows is set below the
// store's size, Export refuses with FailedPrecondition before uploading anything, and that a cap of
// 0 (the default) leaves the export unbounded.
func TestExportRefusesOverManifestCap(t *testing.T) {
	objects := newFakeObjectStore()

	s := newTransferTestServer(t, objects)
	seedTransferFixture(t, s)

	// The fixture holds 2 events + 3 memories = 5 records; a cap of 3 must refuse.
	s.transfer.maxManifestRows = 3

	_, err := s.Export(context.Background(), &contract.ExportRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition over the manifest cap, got %v", err)
	}

	if len(objects.objects) != 0 {
		t.Errorf("expected no object uploaded when refused, got %d", len(objects.objects))
	}

	// A cap of 0 leaves the manifest unbounded, so the same store exports cleanly.
	s.transfer.maxManifestRows = 0

	if _, err := s.Export(context.Background(), &contract.ExportRequest{}); err != nil {
		t.Fatalf("expected an uncapped export to succeed, got %s", err)
	}
}

// failClearStore wraps a real db.Store but forces ClearMemories to fail, so the clear arm of
// Export/Transfer can be driven down its error path.
type failClearStore struct {
	db.Store
	err error
}

func (f failClearStore) ClearMemories(ctx context.Context, snapshots []db.MemoryRecallSnapshot) (int, error) {
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
	if with, without := s.db.CountMemories(context.Background()); with+without != 3 {
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
	if with, without := real.CountMemories(context.Background()); with+without != 3 {
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

	if with, without := real.CountMemories(context.Background()); with+without != 0 {
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

	if with, without := source.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("expected an empty source store, got %d memories", with+without)
	}

	memory := func(id string) *types.Memory {
		memories, err := target.db.GetMemoriesByIds(context.Background(), []string{id})
		if err != nil || len(*memories) != 1 {
			return nil
		}

		return &(*memories)[0]
	}

	if m := memory("m1"); m == nil || m.RecallCount != 1 || m.Group != "billing" {
		t.Errorf("m1 did not arrive in the target with its state, got %+v", m)
	}

	if event, err := target.db.GetEvent(context.Background(), "e1"); err != nil || event.Group != "billing" || event.RelationshipSignificance != 3 {
		t.Errorf("e1 did not arrive in the target with its state, got %+v (%v)", event, err)
	}
}

// TestTransfer_DialErrorMapped verifies a grpc.NewClient failure (a target address grpc-go itself
// rejects while parsing, before any network I/O) is mapped via mapError rather than returned raw.
func TestTransfer_DialErrorMapped(t *testing.T) {
	s := newTransferTestServer(t, nil)
	s.transfer.targetAddress = "\x00" // a control character fails target-string parsing outright

	if _, err := s.Transfer(context.Background(), &contract.TransferRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestTransferDirect_WithoutClearCachesManifest verifies Transfer with clear unset caches the
// manifest for a later Clear call, mirroring Export's own no-clear behaviour, rather than only ever
// being exercised with clear: true.
func TestTransferDirect_WithoutClearCachesManifest(t *testing.T) {
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
	source.transfer.token = "a-bearer-token" // also exercises the outgoing-metadata branch
	seedTransferFixture(t, source)

	transferred, err := source.Transfer(context.Background(), &contract.TransferRequest{})
	if err != nil {
		t.Fatalf("Transfer: %s", err)
	}

	if transferred.GetManifestId() == "" {
		t.Fatal("expected a manifest id when clear was not requested")
	}

	// The source is untouched (no clear), and the manifest is cached for a later Clear call.
	if with, without := source.db.CountMemories(context.Background()); with+without != 3 {
		t.Errorf("expected the source untouched, got %d memories", with+without)
	}

	if source.takeManifest(transferred.GetManifestId()) == nil {
		t.Error("expected the manifest cached for a later Clear call")
	}
}

// failImportMemoriesStore wraps a real db.Store but forces ImportMemories to fail, standing in for
// a target rejecting a memories batch mid-transfer.
type failImportMemoriesStore struct {
	db.Store
	err error
}

func (f failImportMemoriesStore) ImportMemories(ctx context.Context, memories []types.Memory) (int, error) {
	return 0, f.err
}

// TestTransferDirect_MemoriesImportFailurePropagates verifies a target rejecting the memories batch
// (after the events batch already succeeded) surfaces through Transfer, mapped rather than raw.
func TestTransferDirect_MemoriesImportFailurePropagates(t *testing.T) {
	target := newTransferTestServer(t, nil)
	target.db = failImportMemoriesStore{Store: target.db, err: errors.New("memories rejected")}

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

	if _, err := source.Transfer(context.Background(), &contract.TransferRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestTransferDirect_ClearFailureCachesManifestForRetry verifies Transfer's own inline clear
// failure (distinct from Export's and the standalone Clear RPC's, both already covered) re-caches
// the manifest so the caller can retry the delete with the returned manifest id.
func TestTransferDirect_ClearFailureCachesManifestForRetry(t *testing.T) {
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

	wantErr := errors.New("clear failed")
	source.db = failClearStore{Store: source.db, err: wantErr}

	res, err := source.Transfer(context.Background(), &contract.TransferRequest{Clear: true})
	if err == nil {
		t.Fatal("expected the clear failure to surface")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("expected the ClearMemories error to propagate, got %v", err)
	}

	if res.GetManifestId() == "" {
		t.Fatal("expected a manifest id after a failed clear")
	}

	if source.takeManifest(res.GetManifestId()) == nil {
		t.Error("expected the manifest re-cached after a failed clear so Clear can retry it")
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

// TestStoreManifest_EvictsOldestBeyondCap verifies the manifest cache is bounded: storing more than
// manifestCacheLimit manifests evicts the oldest (FIFO), so a long-running exporter cannot leak
// manifests unboundedly. The most recent manifestCacheLimit remain retrievable; the overflowed
// oldest are gone.
func TestStoreManifest_EvictsOldestBeyondCap(t *testing.T) {
	s := &Server{manifests: make(map[string]*transferManifest)}

	total := manifestCacheLimit + 3

	ids := make([]string, total)
	for i := range ids {
		ids[i] = fmt.Sprintf("manifest-%d", i)
		s.storeManifest(&transferManifest{id: ids[i]})
	}

	if len(s.manifestIds) != manifestCacheLimit {
		t.Fatalf("expected the cache bounded to %d, got %d", manifestCacheLimit, len(s.manifestIds))
	}

	// The first three (oldest) must have been evicted.
	for _, id := range ids[:3] {
		if s.takeManifest(id) != nil {
			t.Errorf("expected the overflowed manifest %q to be evicted", id)
		}
	}

	// The most recent manifestCacheLimit must still be present.
	for _, id := range ids[3:] {
		if s.takeManifest(id) == nil {
			t.Errorf("expected the recent manifest %q to be retained", id)
		}
	}
}

// TestTakeManifest_SkipsNonMatchingEntries verifies takeManifest's removal loop actually walks past
// non-matching ids (rather than only ever being exercised with the target already first), and
// leaves the remaining ids in their original order.
func TestTakeManifest_SkipsNonMatchingEntries(t *testing.T) {
	s := &Server{manifests: make(map[string]*transferManifest)}

	for _, id := range []string{"a", "b", "c"} {
		s.storeManifest(&transferManifest{id: id})
	}

	got := s.takeManifest("c")
	if got == nil || got.id != "c" {
		t.Fatalf("expected to retrieve manifest 'c', got %v", got)
	}

	if len(s.manifestIds) != 2 || s.manifestIds[0] != "a" || s.manifestIds[1] != "b" {
		t.Errorf("expected [a b] to remain in order, got %v", s.manifestIds)
	}
}

// failEventsPageStore wraps a real db.Store but forces GetEventsPage to fail, so walkStore's error
// arm reading the events pass can be exercised without a broken database.
type failEventsPageStore struct {
	db.Store
	err error
}

func (f failEventsPageStore) GetEventsPage(ctx context.Context, afterId string, limit int) ([]types.Event, error) {
	return nil, f.err
}

// failMemoriesPageStore wraps a real db.Store but forces GetMemoriesPage to fail, so walkStore's
// error arm reading the memories pass can be exercised without a broken database.
type failMemoriesPageStore struct {
	db.Store
	err error
}

func (f failMemoriesPageStore) GetMemoriesPage(ctx context.Context, afterId string, limit int) ([]types.Memory, error) {
	return nil, f.err
}

// TestWalkStore_PageReadErrorsPropagate verifies a failure paging either events or memories
// surfaces directly from walkStore rather than being swallowed.
func TestWalkStore_PageReadErrorsPropagate(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	noop := func(_ []types.Event) error { return nil }
	noopMem := func(_ []types.Memory) error { return nil }

	eventsErr := errors.New("events page boom")
	s := &Server{db: failEventsPageStore{Store: database, err: eventsErr}, transfer: Transfer{batchSize: 2}}

	if _, _, _, err := s.walkStore(context.Background(), noop, noopMem); !errors.Is(err, eventsErr) {
		t.Errorf("expected the GetEventsPage failure to propagate, got %v", err)
	}

	memoriesErr := errors.New("memories page boom")
	s2 := &Server{db: failMemoriesPageStore{Store: database, err: memoriesErr}, transfer: Transfer{batchSize: 2}}

	if _, _, _, err := s2.walkStore(context.Background(), noop, noopMem); !errors.Is(err, memoriesErr) {
		t.Errorf("expected the GetMemoriesPage failure to propagate, got %v", err)
	}
}

// TestWalkStore_CallbackErrorsPropagate verifies a failing onEvents/onMemories callback surfaces
// directly from walkStore, aborting the walk.
func TestWalkStore_CallbackErrorsPropagate(t *testing.T) {
	s := newTransferTestServer(t, nil)
	seedTransferFixture(t, s)

	eventsErr := errors.New("onEvents boom")

	if _, _, _, err := s.walkStore(context.Background(),
		func(_ []types.Event) error { return eventsErr },
		func(_ []types.Memory) error { return nil },
	); !errors.Is(err, eventsErr) {
		t.Errorf("expected the onEvents callback failure to propagate, got %v", err)
	}

	memoriesErr := errors.New("onMemories boom")

	if _, _, _, err := s.walkStore(context.Background(),
		func(_ []types.Event) error { return nil },
		func(_ []types.Memory) error { return memoriesErr },
	); !errors.Is(err, memoriesErr) {
		t.Errorf("expected the onMemories callback failure to propagate, got %v", err)
	}
}

// failCountEventsStore wraps a real db.Store but forces CountEvents to report -1 (as if it had
// errored), so walkStore's pre-flight manifest-cap check is skipped and only the in-walk check (re-
// verified as each page is accumulated) can trip - exercising that check on its own.
type failCountEventsStore struct {
	db.Store
}

func (f failCountEventsStore) CountEvents(ctx context.Context) int {
	return -1
}

// TestWalkStore_InWalkManifestCapTrips verifies the in-walk manifest-cap check (re-verified as each
// page is accumulated, independent of the pre-flight count) refuses an over-cap walk even when the
// pre-flight check itself was skipped (here, because CountEvents reported an error).
func TestWalkStore_InWalkManifestCapTrips(t *testing.T) {
	s := newTransferTestServer(t, nil)
	seedTransferFixture(t, s) // 2 events + 3 memories = 5 records

	s.db = failCountEventsStore{Store: s.db}
	s.transfer.maxManifestRows = 1

	noop := func(_ []types.Event) error { return nil }
	noopMem := func(_ []types.Memory) error { return nil }

	_, _, _, err := s.walkStore(context.Background(), noop, noopMem)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition from the in-walk cap check, got %v", err)
	}
}

// TestWalkStore_InWalkManifestCapTripsDuringMemoriesPass is the memories-pass counterpart to
// TestWalkStore_InWalkManifestCapTrips: the cap fits the events alone but is exceeded once the
// first page of memories is accumulated, tripping the second in-walk check.
func TestWalkStore_InWalkManifestCapTripsDuringMemoriesPass(t *testing.T) {
	s := newTransferTestServer(t, nil)
	seedTransferFixture(t, s) // 2 events + 3 memories = 5 records; batchSize 2

	s.db = failCountEventsStore{Store: s.db}
	s.transfer.maxManifestRows = 2 // fits the 2 events exactly, but not events + any memories

	noop := func(_ []types.Event) error { return nil }
	noopMem := func(_ []types.Memory) error { return nil }

	_, _, _, err := s.walkStore(context.Background(), noop, noopMem)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition from the in-walk cap check during the memories pass, got %v", err)
	}
}

// failPutObjectStore is an archive.ObjectStore whose Put always fails immediately without reading
// its body, so the archive writer's pipe is closed with an error before it can write anything -
// driving Export's WriteHeader failure arm without needing to corrupt a real archive stream.
type failPutObjectStore struct{}

func (failPutObjectStore) Put(ctx context.Context, key string, body io.Reader) error {
	return errors.New("put failed")
}

func (failPutObjectStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

// TestExport_WriteHeaderErrorSurfaces verifies a write-side failure while streaming the archive
// (here, the upload rejecting before reading anything, closing the pipe before the archive is
// flushed) is mapped and surfaced rather than hanging or panicking.
func TestExport_WriteHeaderErrorSurfaces(t *testing.T) {
	s := newTransferTestServer(t, nil)
	s.objects = failPutObjectStore{}
	seedTransferFixture(t, s)

	_, err := s.Export(context.Background(), &contract.ExportRequest{})
	if err == nil {
		t.Fatal("expected Export to surface the archive write failure")
	}
}

// TestExport_WriteEventErrorSurfaces is a variant of TestExport_WriteHeaderErrorSurfaces with many
// maximum-length events: enough that buffering them overflows the archive writer's internal buffer
// partway through the loop and forces an eager flush against the (already failing) upload pipe from
// within some WriteEvent call itself, rather than only at the final Close - the small fixture in
// the sibling test never triggers that eager flush, so this is the only path that reaches
// WriteEvent's own error return.
func TestExport_WriteEventErrorSurfaces(t *testing.T) {
	s := newTransferTestServer(t, nil)
	s.objects = failPutObjectStore{}

	// 256 is types.Event's own max name length; enough of these comfortably overflows bufio's
	// default 4096-byte buffer partway through the archive writer's event loop.
	for i := 0; i < 40; i++ {
		if _, err := s.db.CreateEvent(context.Background(), types.Event{
			Id:           fmt.Sprintf("e%d", i),
			Name:         strings.Repeat("x", 256),
			TimeStart:    100,
			Significance: 5,
		}); err != nil {
			t.Fatalf("CreateEvent(%d): %s", i, err)
		}
	}

	_, err := s.Export(context.Background(), &contract.ExportRequest{})
	if err == nil {
		t.Fatal("expected Export to surface the archive write failure")
	}
}

// TestExport_WriteMemoryErrorSurfaces is the memories-pass counterpart to
// TestExport_WriteEventErrorSurfaces: enough bulky memories that the archive writer's buffer
// overflows partway through the memories loop, forcing an eager flush against the (already
// failing) upload pipe from within some WriteMemory call itself.
func TestExport_WriteMemoryErrorSurfaces(t *testing.T) {
	s := newTransferTestServer(t, nil)
	s.objects = failPutObjectStore{}

	for i := 0; i < 40; i++ {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			TimeStamp:    100,
			Significance: 5,
			Body:         strings.Repeat("x", 256),
		}); err != nil {
			t.Fatalf("CreateMemory(%d): %s", i, err)
		}
	}

	_, err := s.Export(context.Background(), &contract.ExportRequest{})
	if err == nil {
		t.Fatal("expected Export to surface the archive write failure")
	}
}

// TestImport_EmptyObjectKeyRejected verifies a missing object_key is rejected before touching the
// object store.
func TestImport_EmptyObjectKeyRejected(t *testing.T) {
	s := newTransferTestServer(t, newFakeObjectStore())

	if _, err := s.Import(context.Background(), &contract.ImportRequest{}); err == nil {
		t.Error("expected an error for a missing object_key")
	}
}

// TestImport_ObjectGetErrorMapped verifies a failure fetching the archive object (here, simply an
// unknown key) is mapped via mapError rather than returned raw.
func TestImport_ObjectGetErrorMapped(t *testing.T) {
	s := newTransferTestServer(t, newFakeObjectStore())

	if _, err := s.Import(context.Background(), &contract.ImportRequest{ObjectKey: "does-not-exist"}); status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestImport_MalformedArchiveErrorMapped verifies a fetched object that is not a valid archive
// (garbage bytes) fails at Import via importArchive's archive.NewReader error, mapped rather than
// returned raw.
func TestImport_MalformedArchiveErrorMapped(t *testing.T) {
	objects := newFakeObjectStore()
	if err := objects.Put(context.Background(), "garbage", bytes.NewReader([]byte("not an archive"))); err != nil {
		t.Fatalf("Put: %s", err)
	}

	s := newTransferTestServer(t, objects)

	if _, err := s.Import(context.Background(), &contract.ImportRequest{ObjectKey: "garbage"}); status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// buildArchive writes a minimal, well-formed archive (valid header, given events and memories) to
// a buffer for import tests that need to drive ingestEvents/ingestMemories failures, which require
// getting past archive.NewReader's own header validation first.
func buildArchive(t *testing.T, events []*contract.Event, memories []*contract.Memory) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := archive.NewWriter(&buf)

	if err := w.WriteHeader(&contract.ArchiveHeader{Version: archive.Version}); err != nil {
		t.Fatalf("WriteHeader: %s", err)
	}

	for _, e := range events {
		if err := w.WriteEvent(e); err != nil {
			t.Fatalf("WriteEvent: %s", err)
		}
	}

	for _, m := range memories {
		if err := w.WriteMemory(m); err != nil {
			t.Fatalf("WriteMemory: %s", err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	return buf.Bytes()
}

// TestImportArchive_RejectsEventWithoutId_MidLoopFlush verifies an imported event without an id is
// rejected, with transfer.batchSize small enough that the bad record triggers a flush mid-loop
// (before EOF) rather than only at the final flush.
func TestImportArchive_RejectsEventWithoutId_MidLoopFlush(t *testing.T) {
	body := buildArchive(t, []*contract.Event{{Name: "no id", Significance: 5}}, nil)

	objects := newFakeObjectStore()
	if err := objects.Put(context.Background(), "bad", bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %s", err)
	}

	s := newTransferTestServer(t, objects)
	s.transfer.batchSize = 1

	if _, err := s.Import(context.Background(), &contract.ImportRequest{ObjectKey: "bad"}); err == nil {
		t.Error("expected an error for an imported event without an id")
	}
}

// TestImportArchive_RejectsMemoryWithoutId_FinalFlush verifies an imported memory without an id is
// rejected at the final flush (transfer.batchSize left large so the whole archive is buffered
// before the bad record is reached).
func TestImportArchive_RejectsMemoryWithoutId_FinalFlush(t *testing.T) {
	body := buildArchive(t,
		[]*contract.Event{{Id: "e1", Name: "fine", Significance: 5}},
		[]*contract.Memory{{Body: "no id", Significance: 5}},
	)

	objects := newFakeObjectStore()
	if err := objects.Put(context.Background(), "bad", bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %s", err)
	}

	s := newTransferTestServer(t, objects)

	// Large enough that both records are read before batchSize forces a mid-loop flush, so the
	// failure is caught by the final, post-EOF flush instead (the complementary case to the
	// mid-loop variant above).
	s.transfer.batchSize = 100

	if _, err := s.Import(context.Background(), &contract.ImportRequest{ObjectKey: "bad"}); err == nil {
		t.Error("expected an error for an imported memory without an id")
	}
}

// TestImportArchive_TruncatedStreamErrorMapped verifies a mid-stream (non-EOF) read failure - here,
// an otherwise well-formed archive truncated a few bytes short, so its header parses fine but a
// later record does not - surfaces through Import mapped rather than raw or silently accepted as a
// short read.
func TestImportArchive_TruncatedStreamErrorMapped(t *testing.T) {
	full := buildArchive(t,
		[]*contract.Event{{Id: "e1", Name: "one", Significance: 5}},
		[]*contract.Memory{{Id: "m1", Body: "a longer body so there is enough compressed data to truncate meaningfully", Significance: 5}},
	)

	truncated := full[:len(full)-4]

	objects := newFakeObjectStore()
	if err := objects.Put(context.Background(), "truncated", bytes.NewReader(truncated)); err != nil {
		t.Fatalf("Put: %s", err)
	}

	s := newTransferTestServer(t, objects)

	if _, err := s.Import(context.Background(), &contract.ImportRequest{ObjectKey: "truncated"}); status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestImportBatch_SkipsBinaryMemoriesInSearchIndex verifies ImportBatch's ingestMemories skips
// indexing binary memories, mirroring StoreMemory's own binary-skip contract.
func TestImportBatch_SkipsBinaryMemoriesInSearchIndex(t *testing.T) {
	idx := &fakeIndex{enabled: true}

	s := newTransferTestServer(t, nil)
	s.search = idx

	_, err := s.ImportBatch(context.Background(), &contract.ImportBatchRequest{
		Memories: []*contract.Memory{
			{Id: "text", Body: "hello", Significance: 5},
			{Id: "bin", Body: "AAEC", Significance: 5, IsBinary: contract.Bool_TRUE},
		},
	})
	if err != nil {
		t.Fatalf("ImportBatch: %s", err)
	}

	want := []string{"index:text"}

	if len(idx.calls) != len(want) || idx.calls[0] != want[0] {
		t.Errorf("expected only the non-binary memory indexed, got %v", idx.calls)
	}
}

// TestImportArchive_DefaultBatchSize verifies a non-positive transfer.batchSize falls back to the
// default rather than, say, never flushing - exercised via a real round trip with batchSize left at
// its zero value.
func TestImportArchive_DefaultBatchSize(t *testing.T) {
	objects := newFakeObjectStore()

	source := newTransferTestServer(t, objects)
	source.transfer.batchSize = 0
	seedTransferFixture(t, source)

	exported, err := source.Export(context.Background(), &contract.ExportRequest{})
	if err != nil {
		t.Fatalf("Export: %s", err)
	}

	target := newTransferTestServer(t, objects)
	target.transfer.batchSize = 0

	imported, err := target.Import(context.Background(), &contract.ImportRequest{ObjectKey: exported.GetObjectKey()})
	if err != nil {
		t.Fatalf("Import: %s", err)
	}

	if imported.GetEventsImported() != 2 || imported.GetMemoriesImported() != 3 {
		t.Errorf("expected 2 events and 3 memories imported under the default batch size, got %v", imported)
	}
}

// generateSelfSignedCertFiles writes a fresh self-signed certificate and private key to two PEM
// files under t.TempDir(), for exercising clientCredentials' certificate-loading branches without a
// live CA.
func generateSelfSignedCertFiles(t *testing.T) (certFile string, keyFile string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %s", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hippocampus-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %s", err)
	}

	dir := t.TempDir()

	certFile = dir + "/cert.pem"
	keyFile = dir + "/key.pem"

	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatalf("create cert file: %s", err)
	}

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %s", err)
	}

	if err := certOut.Close(); err != nil {
		t.Fatalf("close cert file: %s", err)
	}

	keyOut, err := os.Create(keyFile)
	if err != nil {
		t.Fatalf("create key file: %s", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		t.Fatalf("encode key: %s", err)
	}

	if err := keyOut.Close(); err != nil {
		t.Fatalf("close key file: %s", err)
	}

	return certFile, keyFile
}

// TestTransferClientCredentials_CACertAndClientCert covers the remaining clientCredentials
// branches: a valid CA bundle, an invalid (non-PEM) CA bundle, and a valid client certificate/key
// pair.
func TestTransferClientCredentials_CACertAndClientCert(t *testing.T) {
	certFile, keyFile := generateSelfSignedCertFiles(t)

	// Valid CA bundle: the self-signed cert doubles as its own CA.
	creds, err := (Transfer{tls: true, tlsCACertFile: certFile}).clientCredentials()
	if err != nil {
		t.Fatalf("valid CA cert: unexpected error: %s", err)
	}

	if proto := creds.Info().SecurityProtocol; proto != "tls" {
		t.Errorf("valid CA cert: expected tls credentials, got %q", proto)
	}

	// Invalid (non-PEM) CA bundle content.
	badCACert := t.TempDir() + "/bad-ca.pem"
	if err := os.WriteFile(badCACert, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write bad CA file: %s", err)
	}

	if _, err := (Transfer{tls: true, tlsCACertFile: badCACert}).clientCredentials(); err == nil {
		t.Error("invalid CA cert content: expected an error, got nil")
	}

	// Valid client certificate/key pair (mutual TLS).
	creds, err = (Transfer{tls: true, tlsCertFile: certFile, tlsKeyFile: keyFile}).clientCredentials()
	if err != nil {
		t.Fatalf("valid client cert: unexpected error: %s", err)
	}

	if proto := creds.Info().SecurityProtocol; proto != "tls" {
		t.Errorf("valid client cert: expected tls credentials, got %q", proto)
	}

	// Invalid client key file (unparseable content, valid cert).
	badKeyFile := t.TempDir() + "/bad-key.pem"
	if err := os.WriteFile(badKeyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write bad key file: %s", err)
	}

	if _, err := (Transfer{tls: true, tlsCertFile: certFile, tlsKeyFile: badKeyFile}).clientCredentials(); err == nil {
		t.Error("invalid client key: expected an error, got nil")
	}
}

// TestTransfer_ClientCredentialsErrorSurfaces verifies the Transfer RPC maps a clientCredentials
// failure (here, a half-configured client certificate pair) rather than returning it raw.
func TestTransfer_ClientCredentialsErrorSurfaces(t *testing.T) {
	s := newTransferTestServer(t, nil)
	s.transfer.targetAddress = "127.0.0.1:0"
	s.transfer.tls = true
	s.transfer.tlsCertFile = "cert-without-a-key.pem"

	if _, err := s.Transfer(context.Background(), &contract.TransferRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestClear_EmptyManifestIdRejected verifies a missing manifest_id is rejected before any lookup.
func TestClear_EmptyManifestIdRejected(t *testing.T) {
	s := newTransferTestServer(t, nil)

	if _, err := s.Clear(context.Background(), &contract.ClearRequest{}); err == nil {
		t.Error("expected an error for a missing manifest_id")
	}
}

// failDeleteEventIfEmptyStore wraps a real db.Store but forces DeleteEventIfEmpty to fail for one
// specific event id, so clearManifest's per-event error-logged-and-skipped loop can be exercised:
// the failing event is skipped (logged, not counted) while the rest of the manifest still clears.
type failDeleteEventIfEmptyStore struct {
	db.Store
	failId string
	err    error
}

func (f failDeleteEventIfEmptyStore) DeleteEventIfEmpty(ctx context.Context, id string) (bool, error) {
	if id == f.failId {
		return false, f.err
	}

	return f.Store.DeleteEventIfEmpty(ctx, id)
}

// TestClearManifest_EventDeleteErrorLoggedAndSkipped verifies a failing DeleteEventIfEmpty for one
// event in the manifest is logged and skipped rather than aborting the whole clear - the other
// captured event still clears, and memoriesCleared is unaffected.
func TestClearManifest_EventDeleteErrorLoggedAndSkipped(t *testing.T) {
	s := newTransferTestServer(t, newFakeObjectStore())
	seedTransferFixture(t, s) // events e1 (has memories) and e2 (referenced via relationship only)

	// e2 has no memories, so it is eligible for deletion in the manifest's event-cleanup pass.
	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m-e2", TimeStamp: 100, Significance: 5, EventId: "e2", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := s.db.DeleteMemories(context.Background(), []string{"m-e2"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	manifest := &transferManifest{id: "m1", eventIds: []string{"e1", "e2"}}

	wantErr := errors.New("delete event boom")
	s.db = failDeleteEventIfEmptyStore{Store: s.db, failId: "e2", err: wantErr}

	memoriesCleared, eventsCleared, err := s.clearManifest(context.Background(), manifest)
	if err != nil {
		t.Fatalf("clearManifest: %s", err)
	}

	if memoriesCleared != 0 {
		t.Errorf("expected no memories in this manifest, got %d", memoriesCleared)
	}

	// e1 still owns memories (not empty) so it is not deleted either; the point under test is that
	// e2's failure did not abort the loop or fail the call.
	if eventsCleared != 0 {
		t.Errorf("expected 0 events cleared (e1 not empty, e2 errored), got %d", eventsCleared)
	}
}
