package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// decisionServer implements the Server interface with per-candidate decision functions, so tests
// can consolidate selectively.
type decisionServer struct {
	memory func(MemoryConsolidationCandidate) bool
	event  func(EventConsolidationCandidate) bool
	value  func(MemoryConsolidationCandidate) float64
}

func (s *decisionServer) ShouldConsolidateMemory(candidate MemoryConsolidationCandidate) bool {
	if s.memory == nil {
		return false
	}

	return s.memory(candidate)
}

func (s *decisionServer) ShouldConsolidateEvent(candidate EventConsolidationCandidate) bool {
	if s.event == nil {
		return false
	}

	return s.event(candidate)
}

func (s *decisionServer) MemoryValue(candidate MemoryConsolidationCandidate) float64 {
	if s.value == nil {
		return 0
	}

	return s.value(candidate)
}

// getMemory returns the memory with the given id, or nil if it does not exist.
func getMemory(t *testing.T, db *DB, id string) *types.Memory {
	t.Helper()

	memories, err := db.GetMemories(context.Background(), MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	for _, memory := range *memories {
		if memory.Id != id {
			continue
		}

		return &memory
	}

	return nil
}

// TestRecallMemories verifies that recalling memories reinforces them: the recall time is set,
// the recall count is incremented on every recall, and the memories are returned. Memories not
// named in the request are untouched.
func TestRecallMemories(t *testing.T) {
	db := newTestDB(t)

	created := time.Now().UnixNano()

	memories := []types.Memory{
		{Id: "m1", TimeStamp: created, Significance: 1, Body: "recalled"},
		{Id: "m2", TimeStamp: created, Significance: 1, Body: "untouched"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// First recall.
	got, err := db.RecallMemories(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(*got) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(*got))
	}

	if (*got)[0].Id != "m1" {
		t.Errorf("expected memory m1, got %s", (*got)[0].Id)
	}

	if (*got)[0].RecallCount != 1 {
		t.Errorf("expected recall count 1, got %d", (*got)[0].RecallCount)
	}

	if (*got)[0].TimeRecalled < created {
		t.Errorf("expected recall time >= creation time, got %d", (*got)[0].TimeRecalled)
	}

	// Second recall increments again.
	got, err = db.RecallMemories(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if (*got)[0].RecallCount != 2 {
		t.Errorf("expected recall count 2, got %d", (*got)[0].RecallCount)
	}

	// The other memory is untouched.
	all, err := db.GetMemories(context.Background(), MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	for _, m := range *all {
		if m.Id != "m2" {
			continue
		}

		if m.RecallCount != 0 || m.TimeRecalled != 0 {
			t.Errorf("memory m2 should be untouched, got recall count %d, time recalled %d", m.RecallCount, m.TimeRecalled)
		}

		break
	}
}

// TestConsolidateEventMemories verifies the evented consolidation pass: an event whose memories
// are all consolidated is deleted with them, an event losing only some of its memories survives
// with MemoriesConsolidated set, and a memory whose event does not exist (a dangling reference) is
// now evaluated as if event-less and consolidated rather than left immortal — while
// its phantom event id stays out of the events-seen count.
func TestConsolidateEventMemories(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "gone", Name: "all memories consolidated", TimeStart: 100, Significance: 1},
		{Id: "partial", Name: "some memories consolidated", TimeStart: 100, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "gone", Body: "x"},
		{Id: "m2", TimeStamp: 100, Significance: 2, EventId: "gone", Body: "x"},
		{Id: "m3", TimeStamp: 100, Significance: 1, EventId: "partial", Body: "x"},
		{Id: "m4", TimeStamp: 100, Significance: 10, EventId: "partial", Body: "x"},
		{Id: "m5", TimeStamp: 100, Significance: 1, EventId: "ghost", Body: "x"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// Consolidate memories with significance below 5: m1, m2, m3, and now m5 — the dangling m5 is
	// evaluated as if event-less rather than skipped.
	server := &decisionServer{
		memory: func(candidate MemoryConsolidationCandidate) bool {
			return candidate.MemorySignificance < 5
		},
	}

	deletedMemories, eventsSeen, deletedEvents, err := db.ConsolidateEventMemories(context.Background(), server)
	if err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err)
	}

	if deletedMemories != 4 {
		t.Errorf("expected 4 memories deleted (m1, m2, m3, and the dangling m5), got %d", deletedMemories)
	}

	// Only the two real events are counted; m5's phantom event id does not inflate the tally.
	if eventsSeen != 2 {
		t.Errorf("expected 2 events seen, got %d", eventsSeen)
	}

	if deletedEvents != 1 {
		t.Errorf("expected 1 event deleted, got %d", deletedEvents)
	}

	if _, err := db.GetEvent(context.Background(), "gone"); err == nil {
		t.Error("event 'gone' should be deleted when its last memory is consolidated")
	}

	partial, err := db.GetEvent(context.Background(), "partial")
	if err != nil {
		t.Fatalf("GetEvent(partial): %s", err)
	}

	if !partial.MemoriesConsolidated {
		t.Error("event 'partial' should be flagged as consolidated after losing a memory")
	}

	if getMemory(t, db, "m4") == nil {
		t.Error("memory m4 should survive consolidation")
	}

	if getMemory(t, db, "m5") != nil {
		t.Error("memory m5 with a dangling event id should now be consolidated like an event-less memory")
	}
}

// TestConsolidateEventMemories_DanglingSurvivesWhenSignificant is the counterpart to the deletion
// case: a dangling memory the server chooses to keep must survive the evented pass and must not
// leave any phantom-event side effect behind (no event row is created or flagged for its absent
// event). It pins that healing a dangling reference is a pure memory-consolidation decision.
func TestConsolidateEventMemories_DanglingSurvivesWhenSignificant(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "d1", TimeStamp: 100, Significance: 50, EventId: "ghost", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Never consolidate: the dangling memory must be seen (evaluated) yet kept.
	server := &decisionServer{memory: func(MemoryConsolidationCandidate) bool { return false }}

	deletedMemories, eventsSeen, deletedEvents, err := db.ConsolidateEventMemories(context.Background(), server)
	if err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err)
	}

	if deletedMemories != 0 || eventsSeen != 0 || deletedEvents != 0 {
		t.Errorf("ConsolidateEventMemories = (%d, %d, %d), want (0, 0, 0) for a surviving dangling memory", deletedMemories, eventsSeen, deletedEvents)
	}

	if getMemory(t, db, "d1") == nil {
		t.Error("the dangling memory should survive when the server keeps it")
	}

	// No event row should have been conjured for the phantom event id.
	if db.CountEvents(context.Background()) != 0 {
		t.Error("consolidating a dangling memory must not create an event row")
	}
}

// TestDeleteMemoriesIfUnrecalled verifies the atomic check-and-delete
// deleteMemoriesIfUnrecalled relies on: a memory is only deleted if its recall state still
// matches the snapshot taken during the consolidation/eviction scan. This is what protects
// against the race where ConsolidateMemories, EvictMemories, or ConsolidateEventMemories decide
// to delete a memory from a stale scan, a concurrent RecallMemories call reinforces that same
// memory in the gap before the delete runs, and — without this re-check — the reinforcement would
// be discarded and the memory deleted anyway.
func TestDeleteMemoriesIfUnrecalled(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "stale", TimeStamp: 100, Significance: 1, Body: "x"},
		{Id: "recalled", TimeStamp: 100, Significance: 1, Body: "x"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// Snapshot both memories as they were created: unrecalled.
	snapshot := []memoryRecallSnapshot{
		{id: "stale", timeRecalled: 0, recallCount: 0},
		{id: "recalled", timeRecalled: 0, recallCount: 0},
	}

	// A recall lands on "recalled" after the snapshot was taken but before the delete runs.
	if _, err := db.RecallMemories(context.Background(), []string{"recalled"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	deleted, err := db.deleteMemoriesIfUnrecalled(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
	}

	if len(deleted) != 1 {
		t.Errorf("expected 1 deletion, got %d", len(deleted))
	}

	if getMemory(t, db, "stale") != nil {
		t.Error("expected the unrecalled memory to be deleted")
	}

	if getMemory(t, db, "recalled") == nil {
		t.Error("expected the concurrently recalled memory to survive")
	}
}

// TestRecallMemories_LargeBatchChunks is a regression test: a bulk recall whose id
// count far exceeds the dialect bound-parameter limit must succeed (chunked) rather than fail
// building one oversized IN (...). Only the ids that exist are reinforced and returned.
func TestRecallMemories_LargeBatchChunks(t *testing.T) {
	db := newTestDB(t)

	realIds := []string{"r0", "r1", "r2", "r3", "r4"}
	for _, id := range realIds {
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	// A batch far larger than any single-statement parameter limit (SQLite ~32k): the reals plus
	// 40k ids that do not exist.
	ids := append([]string{}, realIds...)
	for i := 0; i < 40000; i++ {
		ids = append(ids, fmt.Sprintf("missing-%05d", i))
	}

	got, err := db.RecallMemories(context.Background(), ids)
	if err != nil {
		t.Fatalf("RecallMemories on a large batch should chunk, not fail: %s", err)
	}

	if len(*got) != len(realIds) {
		t.Errorf("expected %d reinforced memories, got %d", len(realIds), len(*got))
	}

	for _, m := range *got {
		if m.RecallCount != 1 {
			t.Errorf("memory %s: expected recall count 1, got %d", m.Id, m.RecallCount)
		}
	}
}

// TestRecallMemories_DuplicateIdsReinforcedOnce verifies dedup across chunk boundaries: an id
// repeated (here padded past a chunk boundary) is still reinforced exactly once, matching the
// single-statement IN semantics.
func TestRecallMemories_DuplicateIdsReinforcedOnce(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "dup", TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// "dup" at the front and again past the first chunk boundary, so a naive chunker would reinforce
	// it twice.
	ids := []string{"dup"}
	for i := 0; i < deleteChunkSize; i++ {
		ids = append(ids, fmt.Sprintf("filler-%04d", i))
	}
	ids = append(ids, "dup")

	got, err := db.RecallMemories(context.Background(), ids)
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(*got) != 1 {
		t.Fatalf("expected the duplicate id reinforced once (1 memory), got %d", len(*got))
	}

	if (*got)[0].RecallCount != 1 {
		t.Errorf("expected recall count 1 for a de-duplicated id, got %d", (*got)[0].RecallCount)
	}
}

// TestDeleteMemoriesIfUnrecalled_ChunkBoundary is a regression test: the batched,
// chunked delete must stay correct across chunk boundaries - deleting exactly the still-matching
// snapshots and protecting any recalled since the scan, wherever they fall relative to the
// deleteChunkSize batches.
func TestDeleteMemoriesIfUnrecalled_ChunkBoundary(t *testing.T) {
	db := newTestDB(t)

	// More than two chunks so the batching logic is genuinely exercised.
	const total = deleteChunkSize*2 + 37

	var snapshot []memoryRecallSnapshot

	for i := 0; i < total; i++ {
		id := fmt.Sprintf("m%05d", i)

		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}

		snapshot = append(snapshot, memoryRecallSnapshot{id: id, timeRecalled: 0, recallCount: 0})
	}

	// Reinforce three memories placed in different chunks after the snapshot, so their recall state
	// no longer matches and the guard must protect them.
	protected := []string{"m00003", fmt.Sprintf("m%05d", deleteChunkSize+1), fmt.Sprintf("m%05d", deleteChunkSize*2+5)}
	if _, err := db.RecallMemories(context.Background(), protected); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	deleted, err := db.deleteMemoriesIfUnrecalled(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
	}

	if len(deleted) != total-len(protected) {
		t.Errorf("expected %d deletions, got %d", total-len(protected), len(deleted))
	}

	for _, id := range protected {
		if getMemory(t, db, id) == nil {
			t.Errorf("recalled memory %s should have survived the batched delete", id)
		}
	}

	with, without := db.CountMemories(context.Background())
	if with+without != len(protected) {
		t.Errorf("expected only the %d protected memories to remain, got %d", len(protected), with+without)
	}
}

// TestUpdateMemory_NoOpValueStillReportsExists guards the existence semantics after the single-
// statement rewrite: updating an existing memory to a value equal to its current one
// must still report the memory exists (true), not a spurious NotFound. On SQLite RowsAffected counts
// matched rows so this is direct; the MySQL changed-rows fallback is covered by the integration test.
func TestUpdateMemory_NoOpValueStillReportsExists(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Same significance it already has - a no-op value update.
	ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1", Significance: 5})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if !ok {
		t.Error("a no-op-value update of an existing memory must report it exists")
	}
}

// TestUpdateMemory_NoFieldsProbesExistence covers the len(sets)==0 arm: with no updatable fields the
// method probes for existence directly (there is no UPDATE to learn it from), returning true for a
// present memory and false for a missing one.
func TestUpdateMemory_NoFieldsProbesExistence(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1"}); err != nil || !ok {
		t.Errorf("no-field update of an existing memory = (%v, %v), want (true, nil)", ok, err)
	}

	if ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "ghost"}); err != nil || ok {
		t.Errorf("no-field update of a missing memory = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestUpdateMemory_PartialUpdate verifies the conditional-overwrite semantics of UpdateMemory:
// only fields carrying a non-zero value replace the stored ones.
func TestUpdateMemory_PartialUpdate(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "original"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Update only the body: every other field must be preserved.
	if ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1", Body: "changed"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	m := getMemory(t, db, "m1")
	if m == nil {
		t.Fatal("memory m1 not found after update")
	}

	if m.Body != "changed" {
		t.Errorf("expected body 'changed', got '%s'", m.Body)
	}

	if m.TimeStamp != 100 || m.Significance != 5 || m.EventId != "e1" {
		t.Errorf("zero-valued fields must not clobber stored values, got timestamp %d, significance %d, event id '%s'", m.TimeStamp, m.Significance, m.EventId)
	}

	// Update only the significance: the new body must be preserved.
	if ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1", Significance: 9}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	m = getMemory(t, db, "m1")
	if m == nil {
		t.Fatal("memory m1 not found after update")
	}

	if m.Significance != 9 || m.Body != "changed" {
		t.Errorf("expected significance 9 and body 'changed', got %d and '%s'", m.Significance, m.Body)
	}
}

// TestUpdateMemory_DoesNotInsertMissing verifies that updating a memory that does not exist neither
// creates a row nor errors: it reports (false, nil) so the RPC layer can surface NotFound rather
// than inserting a phantom memory (the same treatment UpdateEvent received).
func TestUpdateMemory_DoesNotInsertMissing(t *testing.T) {
	db := newTestDB(t)

	ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "new", TimeStamp: 50, Significance: 3, Body: "b"})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if ok {
		t.Fatal("UpdateMemory reported an unknown memory as existing")
	}

	if m := getMemory(t, db, "new"); m != nil {
		t.Fatalf("UpdateMemory created a phantom memory for an unknown id: %+v", m)
	}
}

// TestCreateMemory_BinaryBodyRoundTrip verifies that a body holding arbitrary (non-UTF-8) bytes
// survives storage and recall intact.
func TestCreateMemory_BinaryBodyRoundTrip(t *testing.T) {
	db := newTestDB(t)

	body := string([]byte{0x00, 0xff, 0xfe, 0x01, 0x80, 0x7f})

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "bin", TimeStamp: 100, Significance: 1, Body: body, IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	m := getMemory(t, db, "bin")
	if m == nil {
		t.Fatal("memory 'bin' not found")
	}

	if m.Body != body {
		t.Errorf("binary body corrupted: expected % x, got % x", body, m.Body)
	}

	if !m.IsBinary {
		t.Error("expected IsBinary to be true")
	}

	// The recall path returns the body through a different query; it must round-trip too.
	recalled, err := db.RecallMemories(context.Background(), []string{"bin"})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(*recalled) != 1 || (*recalled)[0].Body != body {
		t.Error("binary body corrupted through recall")
	}
}

// TestDeleteMemories_Chunked verifies deletion of more ids than fit in a single IN (...) chunk,
// including ids that do not exist.
func TestDeleteMemories_Chunked(t *testing.T) {
	db := newTestDB(t)

	total := 1200
	deletions := make([]string, 0, total)

	for i := 0; i < total; i++ {
		id := fmt.Sprintf("m%04d", i)

		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}

		if i < 1101 {
			deletions = append(deletions, id)
		}
	}

	deletions = append(deletions, "does-not-exist")

	cnt, err := db.DeleteMemories(context.Background(), deletions)
	if err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if cnt != 1101 {
		t.Errorf("expected 1101 memories deleted, got %d", cnt)
	}

	if _, without := db.CountMemories(context.Background()); without != total-1101 {
		t.Errorf("expected %d remaining memories, got %d", total-1101, without)
	}
}

// TestReplaceMemoriesWithSummary verifies that every memory associated with the event is deleted
// and replaced with the given summary memory, that the count of replaced memories is reported,
// and that a memory belonging to a different event is left untouched.
func TestReplaceMemoriesWithSummary(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "summarized", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent(e1): %s", err)
	}

	if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e2", Name: "untouched", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent(e2): %s", err)
	}

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "detail 1"},
		{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "detail 2"},
		{Id: "m3", TimeStamp: 100, Significance: 1, EventId: "e2", Body: "unrelated"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	summary := types.Memory{Id: "s1", TimeStamp: 200, Significance: 5, EventId: "e1", Body: "the gist", IsSummary: true}

	replaced, err := db.ReplaceMemoriesWithSummary(context.Background(), "e1", summary)
	if err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	if replaced != 2 {
		t.Errorf("expected 2 memories replaced, got %d", replaced)
	}

	if getMemory(t, db, "m1") != nil || getMemory(t, db, "m2") != nil {
		t.Error("original memories should have been deleted")
	}

	got := getMemory(t, db, "s1")
	if got == nil {
		t.Fatal("summary memory not found")
	}

	if got.Body != "the gist" || !got.IsSummary || got.EventId != "e1" {
		t.Errorf("unexpected summary memory: %+v", got)
	}

	if getMemory(t, db, "m3") == nil {
		t.Error("memory belonging to a different event must not be touched")
	}
}

// TestFindSummarizationCandidates verifies the candidate scan: an event only qualifies once it
// has at least minMemories memories that are all older than the age threshold, is_summary
// memories do not count towards the threshold, and results respect the row limit.
func TestFindSummarizationCandidates(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "quiet", Name: "quiet event", TimeStart: 100, Significance: 1},
		{Id: "active", Name: "active event", TimeStart: 100, Significance: 1},
		{Id: "small", Name: "too few memories", TimeStart: 100, Significance: 1},
		{Id: "already-summarized", Name: "already summarized", TimeStart: 100, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	old := int64(100)
	recent := int64(1_000_000)
	threshold := int64(500_000)

	memories := []types.Memory{
		// "quiet": 3 old memories — qualifies.
		{Id: "q1", TimeStamp: old, Significance: 1, EventId: "quiet", Body: "x"},
		{Id: "q2", TimeStamp: old, Significance: 1, EventId: "quiet", Body: "x"},
		{Id: "q3", TimeStamp: old, Significance: 1, EventId: "quiet", Body: "x"},

		// "active": 3 memories, but one was touched recently — the whole group is disqualified.
		{Id: "a1", TimeStamp: old, Significance: 1, EventId: "active", Body: "x"},
		{Id: "a2", TimeStamp: old, Significance: 1, EventId: "active", Body: "x"},
		{Id: "a3", TimeStamp: recent, Significance: 1, EventId: "active", Body: "x"},

		// "small": only 2 old memories — below the minimum of 3.
		{Id: "s1", TimeStamp: old, Significance: 1, EventId: "small", Body: "x"},
		{Id: "s2", TimeStamp: old, Significance: 1, EventId: "small", Body: "x"},

		// "already-summarized": 3 old memories, but they are already summaries and must not count.
		{Id: "as1", TimeStamp: old, Significance: 1, EventId: "already-summarized", Body: "x", IsSummary: true},
		{Id: "as2", TimeStamp: old, Significance: 1, EventId: "already-summarized", Body: "x", IsSummary: true},
		{Id: "as3", TimeStamp: old, Significance: 1, EventId: "already-summarized", Body: "x", IsSummary: true},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	candidates, err := db.FindSummarizationCandidates(context.Background(), 3, threshold, 0)
	if err != nil {
		t.Fatalf("FindSummarizationCandidates: %s", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("expected exactly 1 candidate, got %d: %+v", len(candidates), candidates)
	}

	if candidates[0].EventId != "quiet" || candidates[0].MemoryCount != 3 || candidates[0].EventName != "quiet event" {
		t.Errorf("unexpected candidate: %+v", candidates[0])
	}

	// A recall on one of "quiet"'s memories touches its decay timestamp and disqualifies the
	// event until it goes quiet again.
	if _, err := db.RecallMemories(context.Background(), []string{"q1"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	candidates, err = db.FindSummarizationCandidates(context.Background(), 3, threshold, 0)
	if err != nil {
		t.Fatalf("FindSummarizationCandidates after recall: %s", err)
	}

	if len(candidates) != 0 {
		t.Errorf("expected recall to disqualify the event, got %d candidates: %+v", len(candidates), candidates)
	}
}

// TestFindSummarizationCandidates_Limit verifies that a positive limit caps the number of rows
// returned, keeping the most populous events.
func TestFindSummarizationCandidates_Limit(t *testing.T) {
	db := newTestDB(t)

	for _, id := range []string{"e1", "e2"} {
		if _, err := db.CreateEvent(context.Background(), types.Event{Id: id, Name: id, TimeStart: 100, Significance: 1}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	// e1 has more memories than e2, so it must be the one kept under a limit of 1.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e1-m%d", i)
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, EventId: "e1", Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("e2-m%d", i)
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, EventId: "e2", Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	candidates, err := db.FindSummarizationCandidates(context.Background(), 3, 1_000_000, 1)
	if err != nil {
		t.Fatalf("FindSummarizationCandidates: %s", err)
	}

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate under the limit, got %d", len(candidates))
	}

	if candidates[0].EventId != "e1" {
		t.Errorf("expected the more populous event 'e1' to be kept, got '%s'", candidates[0].EventId)
	}
}

// TestAddColumnIfMissing verifies the schema-migration helper: adding a column that does not yet
// exist succeeds, and running it again against a column that already exists (the common case on
// every subsequent startup) is a safe no-op rather than an error.
func TestAddColumnIfMissing(t *testing.T) {
	database := newTestDB(t)

	if err := database.addColumnIfMissing("memories", "is_summary", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Fatalf("addColumnIfMissing on a pre-existing column must be a no-op, got: %s", err)
	}

	if err := database.addColumnIfMissing("memories", "migration_test_column", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Fatalf("addColumnIfMissing on a new column: %s", err)
	}

	rows, err := database.sql.Query(`SELECT migration_test_column FROM memories`)
	if err != nil {
		t.Fatalf("new column was not added: %s", err)
	}
	_ = rows.Close()

	// Adding it again must remain a no-op.
	if err := database.addColumnIfMissing("memories", "migration_test_column", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		t.Fatalf("addColumnIfMissing re-run on the now-existing column: %s", err)
	}
}

// TestGetMemories_Filters verifies the timestamp and significance range filters, alone and
// combined. Memory i has timestamp i*100 and significance i.
func TestGetMemories_Filters(t *testing.T) {
	db := newTestDB(t)

	for i := 1; i <= 5; i++ {
		memory := types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			TimeStamp:    int64(i) * 100,
			Significance: int32(i),
			Body:         "x",
		}

		if _, err := db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", memory.Id, err)
		}
	}

	cases := []struct {
		name     string
		filter   MemoryFilter
		expected int
	}{
		{name: "timestamp both bounds", filter: MemoryFilter{TimeStampMin: 200, TimeStampMax: 400}, expected: 3},
		{name: "timestamp min only", filter: MemoryFilter{TimeStampMin: 400}, expected: 2},
		{name: "timestamp max only", filter: MemoryFilter{TimeStampMax: 300}, expected: 3},
		{name: "significance min only", filter: MemoryFilter{SignificanceMin: 4}, expected: 2},
		{name: "significance max only", filter: MemoryFilter{SignificanceMax: 2}, expected: 2},
		{name: "timestamp and significance combined", filter: MemoryFilter{TimeStampMax: 300, SignificanceMin: 2}, expected: 2},
		{name: "no filters", expected: 5},
	}

	for _, c := range cases {
		got, err := db.GetMemories(context.Background(), c.filter)
		if err != nil {
			t.Fatalf("%s: GetMemories: %s", c.name, err)
		}

		if len(*got) != c.expected {
			t.Errorf("%s: expected %d memories, got %d", c.name, c.expected, len(*got))
		}
	}
}

// TestGetMemoriesByIds verifies the non-reinforcing fetch: requested rows come back untouched,
// ids with no matching row are simply absent, and recall state is never modified.
func TestGetMemoriesByIds(t *testing.T) {
	db := newTestDB(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	got, err := db.GetMemoriesByIds(context.Background(), []string{"m1", "missing", "m3"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*got) != 2 {
		t.Fatalf("expected 2 memories (missing id dropped), got %d", len(*got))
	}

	for _, m := range *got {
		if m.RecallCount != 0 || m.TimeRecalled != 0 {
			t.Errorf("fetch must not reinforce: memory %s has recall state %d/%d", m.Id, m.RecallCount, m.TimeRecalled)
		}
	}

	empty, err := db.GetMemoriesByIds(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetMemoriesByIds(nil): %s", err)
	}

	if len(*empty) != 0 {
		t.Errorf("expected no memories for an empty id list, got %d", len(*empty))
	}
}

// TestGetIndexableMemoriesPage verifies the backfill tool's keyset pagination: pages come back in
// ascending id order, resume correctly from the last id of the previous page, exclude binary
// memories, and terminate with an empty page.
func TestGetIndexableMemoriesPage(t *testing.T) {
	db := newTestDB(t)

	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "body of " + id}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "m2b", TimeStamp: 100, Significance: 1, Body: "\x00\x01", IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory(m2b): %s", err)
	}

	var ids []string
	afterId := ""

	for {
		page, err := db.GetIndexableMemoriesPage(context.Background(), afterId, 2)
		if err != nil {
			t.Fatalf("GetIndexableMemoriesPage(%q, 2): %s", afterId, err)
		}

		if len(page) == 0 {
			break
		}

		if len(page) > 2 {
			t.Fatalf("page exceeds its limit: %d memories", len(page))
		}

		for _, memory := range page {
			ids = append(ids, memory.Id)

			if memory.Body == "" {
				t.Errorf("memory %s came back without its body", memory.Id)
			}
		}

		afterId = page[len(page)-1].Id
	}

	want := []string{"m1", "m2", "m3", "m4", "m5"}

	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("expected pages to cover %v in order (binary m2b excluded), got %v", want, ids)
	}
}

// TestMemoryDeleteObserver verifies the observer receives exactly the ids the consolidation scan
// actually deleted - a memory recalled mid-scan survives the conditional delete and must not be
// reported as deleted.
func TestMemoryDeleteObserver(t *testing.T) {
	db := newTestDB(t)

	for _, id := range []string{"m1", "m2"} {
		if _, err := db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	var observed [][]string
	db.SetMemoryDeleteObserver(func(ids []string) {
		observed = append(observed, ids)
	})

	// Recall m2 after the scan snapshot would have been taken: simulate by deleting with stale
	// snapshots directly - m1's snapshot matches, m2's does not.
	if _, err := db.RecallMemories(context.Background(), []string{"m2"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	deleted, err := db.deleteMemoriesIfUnrecalled(context.Background(), []memoryRecallSnapshot{
		{id: "m1", timeRecalled: 0, recallCount: 0},
		{id: "m2", timeRecalled: 0, recallCount: 0}, // stale: m2 was recalled since
	})
	if err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
	}

	// The returned ids must be exactly the rows actually deleted (m2 was skipped) - eviction sums
	// freed bytes over this slice, so an over-inclusive return would overstate the bytes freed
	//.
	if len(deleted) != 1 || deleted[0] != "m1" {
		t.Fatalf("expected exactly [m1] deleted (m2 protected by its recall), got %v", deleted)
	}

	if len(observed) != 1 || len(observed[0]) != 1 || observed[0][0] != "m1" {
		t.Errorf("observer should receive exactly [m1], got %v", observed)
	}

	// An all-stale batch deletes nothing and must not invoke the observer at all.
	observed = nil

	if _, err := db.deleteMemoriesIfUnrecalled(context.Background(), []memoryRecallSnapshot{
		{id: "m2", timeRecalled: 0, recallCount: 0},
	}); err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
	}

	if len(observed) != 0 {
		t.Errorf("observer must not fire when nothing was deleted, got %v", observed)
	}
}

// TestMemoryGroup verifies the group label round-trips through create/read, filters GetMemories,
// and follows the upsert's only-non-zero-values-overwrite rule on update.
func TestMemoryGroup(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, Body: "x", Group: "billing"},
		{Id: "m2", TimeStamp: 100, Significance: 1, Body: "x", Group: "ingest"},
		{Id: "m3", TimeStamp: 100, Significance: 1, Body: "x"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	if m := getMemory(t, db, "m1"); m == nil || m.Group != "billing" {
		t.Errorf("expected m1 to carry group 'billing', got %+v", m)
	}

	if m := getMemory(t, db, "m3"); m == nil || m.Group != "" {
		t.Errorf("expected m3 to carry no group, got %+v", m)
	}

	got, err := db.GetMemories(context.Background(), MemoryFilter{Group: "billing"})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Id != "m1" {
		t.Errorf("expected only m1 in group 'billing', got %v", *got)
	}

	// An update without a group must leave the stored group untouched.
	if ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1", Significance: 2}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	if m := getMemory(t, db, "m1"); m == nil || m.Group != "billing" {
		t.Errorf("expected m1 to keep group 'billing' after an update without one, got %+v", m)
	}

	// An update carrying a group overwrites it.
	if ok, err := db.UpdateMemory(context.Background(), types.Memory{Id: "m1", Group: "ops"}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	} else if !ok {
		t.Fatal("UpdateMemory reported the existing memory as missing")
	}

	if m := getMemory(t, db, "m1"); m == nil || m.Group != "ops" {
		t.Errorf("expected m1 to carry group 'ops' after the update, got %+v", m)
	}
}

// memIds maps a slice of memories to their ids, in order, for concise ordering assertions.
func memIds(memories *[]types.Memory) []string {
	out := make([]string, len(*memories))

	for i, m := range *memories {
		out[i] = m.Id
	}

	return out
}

// TestGetMemoriesSortingAndPagination pins the two supported sort orders and the LIMIT/OFFSET
// paging against a fixed set of memories, plus CountMemoriesFiltered ignoring the page window.
func TestGetMemoriesSortingAndPagination(t *testing.T) {
	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "a", TimeStamp: 100, Significance: 30, Group: "g1", Body: "a"},
		{Id: "b", TimeStamp: 200, Significance: 50, Group: "g1", Body: "b"},
		{Id: "c", TimeStamp: 300, Significance: 50, Group: "g2", Body: "c"},
		{Id: "d", TimeStamp: 400, Significance: 10, Group: "g2", Body: "d"},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// significance: sig desc, then time desc, then id asc — c and b tie on 50 so newer (c) wins.
	bySig, err := db.GetMemories(context.Background(), MemoryFilter{OrderBy: "significance"})
	if err != nil {
		t.Fatalf("GetMemories(significance): %s", err)
	}

	if want := []string{"c", "b", "a", "d"}; !equalStrings(memIds(bySig), want) {
		t.Errorf("significance order = %v, want %v", memIds(bySig), want)
	}

	// timestamp: time desc only.
	byTime, err := db.GetMemories(context.Background(), MemoryFilter{OrderBy: "timestamp"})
	if err != nil {
		t.Fatalf("GetMemories(timestamp): %s", err)
	}

	if want := []string{"d", "c", "b", "a"}; !equalStrings(memIds(byTime), want) {
		t.Errorf("timestamp order = %v, want %v", memIds(byTime), want)
	}

	// an empty/unknown order_by falls back to significance.
	byDefault, err := db.GetMemories(context.Background(), MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories(default): %s", err)
	}

	if want := []string{"c", "b", "a", "d"}; !equalStrings(memIds(byDefault), want) {
		t.Errorf("default order = %v, want %v", memIds(byDefault), want)
	}

	// paging over the significance order: page 1 and page 2 partition the list with no overlap.
	page1, err := db.GetMemories(context.Background(), MemoryFilter{OrderBy: "significance", Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("GetMemories(page1): %s", err)
	}

	if want := []string{"c", "b"}; !equalStrings(memIds(page1), want) {
		t.Errorf("page1 = %v, want %v", memIds(page1), want)
	}

	page2, err := db.GetMemories(context.Background(), MemoryFilter{OrderBy: "significance", Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("GetMemories(page2): %s", err)
	}

	if want := []string{"a", "d"}; !equalStrings(memIds(page2), want) {
		t.Errorf("page2 = %v, want %v", memIds(page2), want)
	}

	// CountMemoriesFiltered ignores Limit/Offset and reflects the filter.
	total, err := db.CountMemoriesFiltered(context.Background(), MemoryFilter{Limit: 1, Offset: 3})
	if err != nil {
		t.Fatalf("CountMemoriesFiltered(all): %s", err)
	}

	if total != 4 {
		t.Errorf("total count = %d, want 4", total)
	}

	g2, err := db.CountMemoriesFiltered(context.Background(), MemoryFilter{Group: "g2"})
	if err != nil {
		t.Fatalf("CountMemoriesFiltered(g2): %s", err)
	}

	if g2 != 2 {
		t.Errorf("group g2 count = %d, want 2", g2)
	}

	sig, err := db.CountMemoriesFiltered(context.Background(), MemoryFilter{SignificanceMin: 50})
	if err != nil {
		t.Fatalf("CountMemoriesFiltered(sig>=50): %s", err)
	}

	if sig != 2 {
		t.Errorf("significance>=50 count = %d, want 2", sig)
	}
}
