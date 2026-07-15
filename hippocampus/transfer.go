package hippocampus

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/archive"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// defaultTransferBatchSize is the page size used when transfer.batchSize is not configured.
const defaultTransferBatchSize = 500

// manifestCacheLimit caps how many manifests are kept for Clear; beyond it the oldest is
// discarded. Manifests are in-memory only — a restart discards them all — so a Clear separated
// from its Export/Transfer by too many runs (or a restart) simply reports an unknown manifest,
// and the records are recaptured by the next run.
const manifestCacheLimit = 8

// transferManifest records exactly what one Export/Transfer run captured: the memory recall
// snapshots and event ids Clear needs to delete precisely those records and nothing newer.
type transferManifest struct {
	id       string
	memories []db.MemoryRecallSnapshot
	eventIds []string
}

// walkStore pages the entire store — events first, then memories — through the callbacks in
// transfer.batchSize pages, building the manifest of what was seen. Writes landing behind the
// pagination cursor are simply not captured; they belong to the next run.
func (s *Server) walkStore(
	onEvents func([]types.Event) error,
	onMemories func([]types.Memory) error,
) (*transferManifest, int, int, error) {
	log.Trace("func() walkStore")

	batchSize := s.transfer.batchSize
	if batchSize <= 0 {
		batchSize = defaultTransferBatchSize
	}

	manifest := &transferManifest{id: uuid.New().String()}
	events := 0
	memories := 0

	afterId := ""

	for {
		page, err := s.db.GetEventsPage(afterId, batchSize)
		if err != nil {
			return nil, 0, 0, err
		}

		if len(page) == 0 {
			break
		}

		for _, event := range page {
			manifest.eventIds = append(manifest.eventIds, event.Id)
		}

		if err := onEvents(page); err != nil {
			return nil, 0, 0, err
		}

		events += len(page)
		afterId = page[len(page)-1].Id
	}

	afterId = ""

	for {
		page, err := s.db.GetMemoriesPage(afterId, batchSize)
		if err != nil {
			return nil, 0, 0, err
		}

		if len(page) == 0 {
			break
		}

		for _, memory := range page {
			manifest.memories = append(manifest.memories, db.MemoryRecallSnapshot{
				Id:           memory.Id,
				TimeRecalled: memory.TimeRecalled,
				RecallCount:  memory.RecallCount,
			})
		}

		if err := onMemories(page); err != nil {
			return nil, 0, 0, err
		}

		memories += len(page)
		afterId = page[len(page)-1].Id
	}

	return manifest, events, memories, nil
}

// storeManifest caches the manifest for a later Clear, evicting the oldest beyond the cap.
func (s *Server) storeManifest(manifest *transferManifest) {
	s.manifestsMu.Lock()
	defer s.manifestsMu.Unlock()

	s.manifests[manifest.id] = manifest
	s.manifestIds = append(s.manifestIds, manifest.id)

	if len(s.manifestIds) > manifestCacheLimit {
		delete(s.manifests, s.manifestIds[0])
		s.manifestIds = s.manifestIds[1:]
	}
}

// takeManifest removes and returns the manifest with the given id, or nil.
func (s *Server) takeManifest(id string) *transferManifest {
	s.manifestsMu.Lock()
	defer s.manifestsMu.Unlock()

	manifest, ok := s.manifests[id]
	if !ok {
		return nil
	}

	delete(s.manifests, id)

	for i, v := range s.manifestIds {
		if v != id {
			continue
		}

		s.manifestIds = append(s.manifestIds[:i], s.manifestIds[i+1:]...)

		break
	}

	return manifest
}

// clearManifest deletes what the manifest captured: memories only while their recall state still
// matches the captured snapshot, then events only once they have no memories left — both
// re-verified live, so records touched since the capture survive to the next run. Event deletion
// errors are logged and skipped rather than failing the clear, matching the consolidation scans.
func (s *Server) clearManifest(ctx context.Context, manifest *transferManifest) (int, int, error) {
	log.Trace("func() clearManifest")

	memoriesCleared, err := s.db.ClearMemories(manifest.memories)
	if err != nil {
		return 0, 0, err
	}

	eventsCleared := 0

	for _, id := range manifest.eventIds {
		deleted, err := s.db.DeleteEventIfEmpty(id)
		if err != nil {
			log.Errorf("failed to delete event '%s' during clear: %s", id, err.Error())

			continue
		}

		if deleted {
			eventsCleared++
		}
	}

	tel.recordsCleared.Add(ctx, int64(memoriesCleared), metric.WithAttributes(attribute.String("kind", "memory")))
	tel.recordsCleared.Add(ctx, int64(eventsCleared), metric.WithAttributes(attribute.String("kind", "event")))

	return memoriesCleared, eventsCleared, nil
}

// Export snapshots the whole store into an archive object in S3 and caches a manifest of exactly
// what it captured; with clear set the captured records are deleted once the upload has
// succeeded. The archive streams through an io.Pipe, so it is never buffered whole in memory.
func (s *Server) Export(ctx context.Context, in *contract.ExportRequest) (*contract.ExportResponse, error) {
	log.Debug("Export()")

	var res contract.ExportResponse

	if s.objects == nil {
		return &res, status.Error(codes.FailedPrecondition, "no object store is configured (s3.bucket)")
	}

	manifestShort := uuid.New().String()[:8]
	key := fmt.Sprintf("%s%s-%s.archive.gz", s.transfer.keyPrefix, time.Now().UTC().Format("20060102T150405Z"), manifestShort)

	// The header's counts are informational — they are read before the walk and the store can
	// move underneath; the response carries the exact figures.
	memoriesWith, memoriesWithout := s.db.CountMemories()

	header := &contract.ArchiveHeader{
		Version:     archive.Version,
		ExportedAt:  time.Now().UnixNano(),
		EventCount:  int32(max(s.db.CountEvents(), 0)),
		MemoryCount: int32(max(memoriesWith+memoriesWithout, 0)),
	}

	type exportResult struct {
		manifest *transferManifest
		events   int
		memories int
		err      error
	}

	pr, pw := io.Pipe()
	results := make(chan exportResult, 1)

	go func() {
		w := archive.NewWriter(pw)

		writeArchive := func() (*transferManifest, int, int, error) {
			if err := w.WriteHeader(header); err != nil {
				return nil, 0, 0, err
			}

			return s.walkStore(
				func(events []types.Event) error {
					for _, event := range events {
						if err := w.WriteEvent(event.ToProto()); err != nil {
							return err
						}
					}

					return nil
				},
				func(memories []types.Memory) error {
					for _, memory := range memories {
						if err := w.WriteMemory(memory.ToProto()); err != nil {
							return err
						}
					}

					return nil
				},
			)
		}

		manifest, events, memories, err := writeArchive()
		if err == nil {
			err = w.Close()
		}

		// CloseWithError propagates a writer-side failure to the uploader's read; a nil error
		// closes the pipe cleanly.
		_ = pw.CloseWithError(err)

		results <- exportResult{manifest: manifest, events: events, memories: memories, err: err}
	}()

	putErr := s.objects.Put(ctx, key, pr)

	// Unblock the writer goroutine when the upload failed before consuming the whole archive.
	_ = pr.CloseWithError(putErr)

	result := <-results

	err := result.err
	if err == nil {
		err = putErr
	}

	tel.exports.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

	if err != nil {
		return &res, err
	}

	tel.recordsExported.Add(ctx, int64(result.events), metric.WithAttributes(attribute.String("kind", "event")))
	tel.recordsExported.Add(ctx, int64(result.memories), metric.WithAttributes(attribute.String("kind", "memory")))

	res.ManifestId = result.manifest.id
	res.ObjectKey = key
	res.EventsExported = int32(result.events)
	res.MemoriesExported = int32(result.memories)

	if in.GetClear() {

		// Clear against the local manifest directly rather than store-then-take: the round trip
		// could return nil (the manifest evicted by concurrent runs between the two calls, panicking
		// clearManifest). On success the manifest is consumed and never cached; on failure it is
		// cached so the caller can retry the delete with the returned manifest id.
		memoriesCleared, eventsCleared, err := s.clearManifest(ctx, result.manifest)
		if err != nil {
			s.storeManifest(result.manifest)

			return &res, fmt.Errorf("export succeeded (object '%s') but the clear failed; retry Clear with manifest '%s': %w", key, result.manifest.id, err)
		}

		res.MemoriesCleared = int32(memoriesCleared)
		res.EventsCleared = int32(eventsCleared)

		return &res, nil
	}

	// Clear was not requested, so cache the manifest for a later Clear call.
	s.storeManifest(result.manifest)

	return &res, nil
}

// Import streams an archive object from S3 into the store, upserting every record by id with its
// full state preserved — a data migration, not fresh writes. Re-importing the same archive is
// idempotent. Aborts on the first error; a failed run is simply rerun.
func (s *Server) Import(ctx context.Context, in *contract.ImportRequest) (*contract.ImportResponse, error) {
	log.Debug("Import()")

	var res contract.ImportResponse

	if s.objects == nil {
		return &res, status.Error(codes.FailedPrecondition, "no object store is configured (s3.bucket)")
	}

	if in.GetObjectKey() == "" {
		return &res, fmt.Errorf("object_key must be provided")
	}

	body, err := s.objects.Get(ctx, in.GetObjectKey())
	if err != nil {
		tel.imports.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", false)))

		return &res, err
	}
	defer func() { _ = body.Close() }()

	events, memories, err := s.importArchive(ctx, body)

	tel.imports.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

	if err != nil {
		return &res, err
	}

	res.EventsImported = int32(events)
	res.MemoriesImported = int32(memories)

	return &res, nil
}

// importArchive reads an archive stream and ingests its records in transfer.batchSize batches.
func (s *Server) importArchive(ctx context.Context, body io.Reader) (int, int, error) {
	log.Trace("func() importArchive")

	r, err := archive.NewReader(body)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = r.Close() }()

	batchSize := s.transfer.batchSize
	if batchSize <= 0 {
		batchSize = defaultTransferBatchSize
	}

	events := 0
	memories := 0

	var eventBatch []*contract.Event
	var memoryBatch []*contract.Memory

	flush := func() error {
		n, err := s.ingestEvents(ctx, eventBatch)
		events += n

		if err != nil {
			return err
		}

		n, err = s.ingestMemories(ctx, memoryBatch)
		memories += n

		if err != nil {
			return err
		}

		eventBatch = eventBatch[:0]
		memoryBatch = memoryBatch[:0]

		return nil
	}

READ_LOOP:
	for {
		record, err := r.Read()

		switch {

		case err == io.EOF:
			break READ_LOOP

		case err != nil:
			return events, memories, err
		}

		if event := record.GetEvent(); event != nil {
			eventBatch = append(eventBatch, event)
		}

		if memory := record.GetMemory(); memory != nil {
			memoryBatch = append(memoryBatch, memory)
		}

		if len(eventBatch)+len(memoryBatch) >= batchSize {
			if err := flush(); err != nil {
				return events, memories, err
			}
		}
	}

	if err := flush(); err != nil {
		return events, memories, err
	}

	return events, memories, nil
}

// ImportBatch upserts full-state rows by id — the ingest half of Transfer, and usable directly
// for any migration tooling. Unlike StoreEvent/StoreMemory nothing is defaulted or gated on
// minimum significance: the rows carry their history.
func (s *Server) ImportBatch(ctx context.Context, in *contract.ImportBatchRequest) (*contract.ImportBatchResponse, error) {
	log.Debug("ImportBatch()")

	var res contract.ImportBatchResponse

	events, err := s.ingestEvents(ctx, in.GetEvents())
	res.EventsImported = int32(events)

	if err != nil {
		return &res, err
	}

	memories, err := s.ingestMemories(ctx, in.GetMemories())
	res.MemoriesImported = int32(memories)

	if err != nil {
		return &res, err
	}

	return &res, nil
}

// ingestEvents converts and upserts a batch of full-state events.
func (s *Server) ingestEvents(ctx context.Context, protos []*contract.Event) (int, error) {
	if len(protos) == 0 {
		return 0, nil
	}

	events := make([]types.Event, len(protos))

	for i, in := range protos {
		if in.GetId() == "" {
			return 0, fmt.Errorf("imported event without an id")
		}

		event := types.EventFromProto(in)

		// EventFromProto deliberately drops the output-only fields; a full-state import carries
		// them through.
		event.MemoriesConsolidated = in.GetMemoriesConsolidated()

		events[i] = event
	}

	count, err := s.db.ImportEvents(events)
	if err != nil {
		return 0, err
	}

	tel.recordsImported.Add(ctx, int64(count), metric.WithAttributes(attribute.String("kind", "event")))

	return count, nil
}

// ingestMemories converts and upserts a batch of full-state memories, indexing the non-binary
// ones into the optional content-search index.
func (s *Server) ingestMemories(ctx context.Context, protos []*contract.Memory) (int, error) {
	if len(protos) == 0 {
		return 0, nil
	}

	memories := make([]types.Memory, len(protos))

	for i, in := range protos {
		if in.GetId() == "" {
			return 0, fmt.Errorf("imported memory without an id")
		}

		memories[i] = types.MemoryFromProto(in)
	}

	count, err := s.db.ImportMemories(memories)
	if err != nil {
		return 0, err
	}

	for _, memory := range memories {
		if memory.IsBinary {
			continue
		}

		s.searchIdx().IndexMemory(search.DocFromMemory(memory))
	}

	tel.recordsImported.Add(ctx, int64(count), metric.WithAttributes(attribute.String("kind", "memory")))

	return count, nil
}

// Transfer streams the whole store directly into a centralised instance's ImportBatch RPC
// (transfer.targetAddress), caching a manifest like Export; with clear set the captured records
// are deleted once the target has accepted every batch.
func (s *Server) Transfer(ctx context.Context, in *contract.TransferRequest) (*contract.TransferResponse, error) {
	log.Debug("Transfer()")

	var res contract.TransferResponse

	if s.transfer.targetAddress == "" {
		return &res, status.Error(codes.FailedPrecondition, "no transfer target is configured (transfer.targetAddress)")
	}

	creds := insecure.NewCredentials()
	if s.transfer.tls {
		creds = credentials.NewClientTLSFromCert(nil, "")
	}

	conn, err := grpc.NewClient(s.transfer.targetAddress, grpc.WithTransportCredentials(creds))
	if err != nil {
		tel.transfers.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", false)))

		return &res, err
	}
	defer func() { _ = conn.Close() }()

	client := contract.NewHippocampusClient(conn)

	if s.transfer.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+s.transfer.token)
	}

	manifest, events, memories, err := s.walkStore(
		func(events []types.Event) error {
			batch := make([]*contract.Event, len(events))
			for i, event := range events {
				batch[i] = event.ToProto()
			}

			_, err := client.ImportBatch(ctx, &contract.ImportBatchRequest{Events: batch})

			return err
		},
		func(memories []types.Memory) error {
			batch := make([]*contract.Memory, len(memories))
			for i, memory := range memories {
				batch[i] = memory.ToProto()
			}

			_, err := client.ImportBatch(ctx, &contract.ImportBatchRequest{Memories: batch})

			return err
		},
	)

	tel.transfers.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))

	if err != nil {
		return &res, err
	}

	tel.recordsExported.Add(ctx, int64(events), metric.WithAttributes(attribute.String("kind", "event")))
	tel.recordsExported.Add(ctx, int64(memories), metric.WithAttributes(attribute.String("kind", "memory")))

	res.ManifestId = manifest.id
	res.EventsTransferred = int32(events)
	res.MemoriesTransferred = int32(memories)

	if in.GetClear() {

		// Clear against the local manifest directly rather than store-then-take (see Export): the
		// round trip could return nil under concurrent runs and panic clearManifest. On success the
		// manifest is consumed; on failure it is cached so the caller can retry with its id.
		memoriesCleared, eventsCleared, err := s.clearManifest(ctx, manifest)
		if err != nil {
			s.storeManifest(manifest)

			return &res, fmt.Errorf("transfer succeeded but the clear failed; retry Clear with manifest '%s': %w", manifest.id, err)
		}

		res.MemoriesCleared = int32(memoriesCleared)
		res.EventsCleared = int32(eventsCleared)

		return &res, nil
	}

	// Clear was not requested, so cache the manifest for a later Clear call.
	s.storeManifest(manifest)

	return &res, nil
}

// Clear deletes exactly the records captured by the Export/Transfer run that produced the
// manifest. Memories recalled (or re-created) since the capture survive, as do events that still
// have memories. The manifest is consumed on success (whether or not anything was actually
// deleted), but on a failed clear it is re-cached so the caller can retry with the same id rather
// than being left with an unusable NotFound.
func (s *Server) Clear(ctx context.Context, in *contract.ClearRequest) (*contract.ClearResponse, error) {
	log.Debug("Clear()")

	var res contract.ClearResponse

	if in.GetManifestId() == "" {
		return &res, fmt.Errorf("manifest_id must be provided")
	}

	manifest := s.takeManifest(in.GetManifestId())
	if manifest == nil {
		return &res, status.Errorf(codes.NotFound, "unknown manifest '%s' (manifests do not survive a restart)", in.GetManifestId())
	}

	memoriesCleared, eventsCleared, err := s.clearManifest(ctx, manifest)
	if err != nil {

		// takeManifest already removed it; re-cache it so the caller can retry the delete with the
		// same manifest id rather than being left with an unusable NotFound (mirrors the failed-clear
		// handling Export/Transfer got for their one-shot clear flag).
		s.storeManifest(manifest)

		return &res, err
	}

	res.MemoriesCleared = int32(memoriesCleared)
	res.EventsCleared = int32(eventsCleared)

	return &res, nil
}
