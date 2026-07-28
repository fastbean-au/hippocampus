package hippocampus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/summarise"
	"github.com/fastbean-au/hippocampus/types"
)

// fakeSummariser is a test double for summarise.Summariser. It records the last request and
// returns a canned summary or a canned error. failNames lets a test fail only for specific events
// (matched on the request's EventName), so a mixed success/failure batch can be exercised.
type fakeSummariser struct {
	enabled   bool
	reply     string
	err       error
	failNames map[string]bool
	lastReq   summarise.Request
	calls     int
}

func (f *fakeSummariser) Summarise(ctx context.Context, req summarise.Request) (string, error) {
	f.calls++
	f.lastReq = req

	if f.failNames[req.EventName] {
		return "", errors.New("selective failure")
	}

	if f.err != nil {
		return "", f.err
	}

	return f.reply, nil
}

func (f *fakeSummariser) Enabled() bool {
	return f.enabled
}

// summariseFaultStore forces GetEvent or GetMemoriesByEventId to fail, to exercise summariseEvent's
// storage-error arms without a broken database.
type summariseFaultStore struct {
	db.Store

	getEventErr             error
	getMemoriesByEventIdErr error
}

func (f summariseFaultStore) GetEvent(ctx context.Context, id string) (*types.Event, error) {
	if f.getEventErr != nil {
		return nil, f.getEventErr
	}

	return f.Store.GetEvent(ctx, id)
}

func (f summariseFaultStore) GetMemoriesByEventId(ctx context.Context, eventId string) (*[]types.Memory, error) {
	if f.getMemoriesByEventIdErr != nil {
		return nil, f.getMemoriesByEventIdErr
	}

	return f.Store.GetMemoriesByEventId(ctx, eventId)
}

// newSummariseTestServer builds a Server over an in-memory database wired to the given summariser.
func newSummariseTestServer(t *testing.T, summariser summarise.Summariser) *Server {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return &Server{db: database, summarise: summariser}
}

// seedEvent creates an event with the given memories for the summarisation tests.
func seedEvent(t *testing.T, s *Server, eventId string, memories []types.Memory) {
	t.Helper()

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: eventId, Name: "trip", Group: "travel", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for i := range memories {
		if _, err := s.db.CreateMemory(context.Background(), memories[i]); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}
}

// TestSummariseMemories_HappyPath verifies the RPC reads the event's text memories, sends them to
// the summariser with event context, and replaces them with the generated summary.
func TestSummariseMemories_HappyPath(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "the gist of the trip"}
	s := newSummariseTestServer(t, f)

	seedEvent(t, s, "e1", []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 3, EventId: "e1", Body: "packed bags"},
		{Id: "m2", TimeStamp: 100, Significance: 7, EventId: "e1", Body: "boarded plane"},
	})

	res, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if err != nil {
		t.Fatalf("SummariseMemories: %s", err)
	}

	if res.GetSummary() != "the gist of the trip" {
		t.Errorf("unexpected summary: %q", res.GetSummary())
	}

	if res.GetMemoriesReplaced() != 2 {
		t.Errorf("expected 2 memories replaced, got %d", res.GetMemoriesReplaced())
	}

	if f.calls != 1 || len(f.lastReq.Bodies) != 2 {
		t.Errorf("unexpected summariser call: calls=%d bodies=%v", f.calls, f.lastReq.Bodies)
	}

	if f.lastReq.EventName != "trip" || f.lastReq.Group != "travel" {
		t.Errorf("event context not passed: name=%q group=%q", f.lastReq.EventName, f.lastReq.Group)
	}

	// The surviving memory is the summary; it should carry the highest replaced significance (7)
	// since the request specified none, and be flagged is_summary.
	memories, err := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*memories) != 1 {
		t.Fatalf("expected 1 memory after summarise, got %d", len(*memories))
	}

	m := (*memories)[0]

	if m.Id != res.GetId() || !m.IsSummary || m.Body != "the gist of the trip" {
		t.Errorf("unexpected surviving memory: %+v", m)
	}

	if m.Significance != 7 {
		t.Errorf("expected summary significance defaulted to max (7), got %d", m.Significance)
	}
}

// TestSummariseMemories_Disabled verifies the RPC fails with FAILED_PRECONDITION when no summariser
// is configured.
func TestSummariseMemories_Disabled(t *testing.T) {
	s := newSummariseTestServer(t, &fakeSummariser{enabled: false})

	seedEvent(t, s, "e1", []types.Memory{{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "x"}})

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

// TestSummariseMemories_NilSummariser verifies a Server built without a summariser (nil field, as
// in most tests) behaves as disabled rather than panicking.
func TestSummariseMemories_NilSummariser(t *testing.T) {
	s := newTestServer(t)

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

// TestSummariseMemories_EmptyEventId verifies an empty event_id is rejected with InvalidArgument.
func TestSummariseMemories_EmptyEventId(t *testing.T) {
	s := newSummariseTestServer(t, &fakeSummariser{enabled: true})

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

// TestSummariseMemories_UnknownEvent verifies an unknown event returns NotFound.
func TestSummariseMemories_UnknownEvent(t *testing.T) {
	s := newSummariseTestServer(t, &fakeSummariser{enabled: true})

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

// TestSummariseMemories_OnlyBinaryMemories verifies an event whose memories are all binary (opaque
// bodies) has nothing to summarise and fails with FAILED_PRECONDITION, leaving the memories intact.
func TestSummariseMemories_OnlyBinaryMemories(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "unused"}
	s := newSummariseTestServer(t, f)

	seedEvent(t, s, "e1", []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "AQID", IsBinary: true},
	})

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}

	if f.calls != 0 {
		t.Errorf("summariser should not be called with no text bodies, calls=%d", f.calls)
	}

	memories, _ := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if len(*memories) != 1 {
		t.Errorf("binary memory must survive, got %d memories", len(*memories))
	}
}

// TestSummariseMemories_SummariserError verifies a summariser failure surfaces as UNAVAILABLE and
// leaves the original memories untouched.
func TestSummariseMemories_SummariserError(t *testing.T) {
	f := &fakeSummariser{enabled: true, err: errors.New("model unreachable")}
	s := newSummariseTestServer(t, f)

	seedEvent(t, s, "e1", []types.Memory{{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "x"}})

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", err)
	}

	memories, _ := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if len(*memories) != 1 || (*memories)[0].Id != "m1" {
		t.Error("the original memory must survive a failed summarisation")
	}
}

// TestAutoSummariseCandidates_Summarises verifies the sleep-cycle auto path condenses the cached
// candidates and removes them from the candidate list.
func TestAutoSummariseCandidates_Summarises(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "auto summary"}
	s := newSummariseTestServer(t, f)
	s.consolidation.autoSummarise = true

	seedEvent(t, s, "e1", []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"},
		{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "b"},
	})

	s.summarisationCandidates = []db.SummarisationCandidate{{EventId: "e1", EventName: "trip", MemoryCount: 2}}

	s.autoSummariseCandidates(context.Background())

	if f.calls != 1 {
		t.Errorf("expected 1 summariser call, got %d", f.calls)
	}

	memories, _ := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if len(*memories) != 1 || (*memories)[0].Body != "auto summary" {
		t.Errorf("expected the event condensed to one summary, got %+v", memories)
	}

	if len(s.summarisationCandidates) != 0 {
		t.Errorf("summarised candidate should be dropped from the list, got %+v", s.summarisationCandidates)
	}
}

// TestAutoSummariseCandidates_DisabledByDefault verifies the auto path is a no-op when
// autoSummarise is off, even with a working summariser and candidates present.
func TestAutoSummariseCandidates_DisabledByDefault(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "auto summary"}
	s := newSummariseTestServer(t, f)
	// autoSummarise defaults to false.

	seedEvent(t, s, "e1", []types.Memory{{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"}})

	s.summarisationCandidates = []db.SummarisationCandidate{{EventId: "e1", EventName: "trip", MemoryCount: 1}}

	s.autoSummariseCandidates(context.Background())

	if f.calls != 0 {
		t.Errorf("summariser must not be called when autoSummarise is off, calls=%d", f.calls)
	}

	if len(s.summarisationCandidates) != 1 {
		t.Errorf("candidate list must be untouched, got %+v", s.summarisationCandidates)
	}
}

// TestSummariseMemories_GetEventError verifies a non-NotFound error from the store's GetEvent is
// mapped to an INTERNAL status rather than treated as a missing event.
func TestSummariseMemories_GetEventError(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	s := &Server{
		db:        summariseFaultStore{Store: database, getEventErr: errors.New("db down")},
		summarise: &fakeSummariser{enabled: true, reply: "x"},
	}

	_, err = s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

// TestSummariseMemories_GetMemoriesError verifies a failure reading the event's memories is mapped
// to an INTERNAL status.
func TestSummariseMemories_GetMemoriesError(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	s := &Server{
		db:        summariseFaultStore{Store: database, getMemoriesByEventIdErr: errors.New("read failed")},
		summarise: &fakeSummariser{enabled: true, reply: "x"},
	}

	_, err = s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
}

// TestSummariseMemories_InsertRejected verifies that when the generated summary fails insertion
// (here, its defaulted significance is below the configured minimum) the RPC returns the
// InvalidArgument insertSummary raises and the original memories survive.
func TestSummariseMemories_InsertRejected(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "the gist"}
	s := newSummariseTestServer(t, f)
	s.minimumMemorySignificance = 100

	seedEvent(t, s, "e1", []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 3, EventId: "e1", Body: "a"},
		{Id: "m2", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "b"},
	})

	_, err := s.SummariseMemories(context.Background(), &contract.SummariseMemoriesRequest{EventId: "e1"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}

	memories, _ := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if len(*memories) != 2 {
		t.Errorf("original memories must survive a rejected summary, got %d", len(*memories))
	}
}

// TestAutoSummariseCandidates_EmptyList verifies the auto path returns early (no summariser call)
// when there are no cached candidates, even with auto-summarisation enabled.
func TestAutoSummariseCandidates_EmptyList(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "x"}
	s := newSummariseTestServer(t, f)
	s.consolidation.autoSummarise = true

	// No candidates set.
	s.autoSummariseCandidates(context.Background())

	if f.calls != 0 {
		t.Errorf("summariser must not be called with an empty candidate list, calls=%d", f.calls)
	}
}

// TestAutoSummariseCandidates_MixedSuccessFailure verifies that when some candidates summarise and
// others fail, the successes are condensed and dropped from the list while the failures are kept.
func TestAutoSummariseCandidates_MixedSuccessFailure(t *testing.T) {
	f := &fakeSummariser{enabled: true, reply: "auto summary", failNames: map[string]bool{"bad": true}}
	s := newSummariseTestServer(t, f)
	s.consolidation.autoSummarise = true

	// e1 ("trip") summarises; e2 ("bad") fails.
	seedEvent(t, s, "e1", []types.Memory{{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"}})

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e2", Name: "bad", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, EventId: "e2", Body: "b"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	s.summarisationCandidates = []db.SummarisationCandidate{
		{EventId: "e1", EventName: "trip", MemoryCount: 1},
		{EventId: "e2", EventName: "bad", MemoryCount: 1},
	}

	s.autoSummariseCandidates(context.Background())

	// e1 condensed, e2 untouched.
	e1mems, _ := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if len(*e1mems) != 1 || (*e1mems)[0].Body != "auto summary" {
		t.Errorf("e1 should be condensed, got %+v", e1mems)
	}

	e2mems, _ := s.db.GetMemoriesByEventId(context.Background(), "e2")
	if len(*e2mems) != 1 || (*e2mems)[0].Id != "m2" {
		t.Errorf("e2 should be untouched, got %+v", e2mems)
	}

	// Only the failed candidate remains in the list.
	if len(s.summarisationCandidates) != 1 || s.summarisationCandidates[0].EventId != "e2" {
		t.Errorf("only the failed candidate should remain, got %+v", s.summarisationCandidates)
	}
}

// TestAutoSummariseCandidates_SkipsFailingEvent verifies a per-event failure is skipped (logged),
// leaves that event's memories intact, and keeps it in the candidate list, without failing.
func TestAutoSummariseCandidates_SkipsFailingEvent(t *testing.T) {
	f := &fakeSummariser{enabled: true, err: errors.New("boom")}
	s := newSummariseTestServer(t, f)
	s.consolidation.autoSummarise = true

	seedEvent(t, s, "e1", []types.Memory{{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "a"}})

	s.summarisationCandidates = []db.SummarisationCandidate{{EventId: "e1", EventName: "trip", MemoryCount: 1}}

	s.autoSummariseCandidates(context.Background())

	memories, _ := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if len(*memories) != 1 || (*memories)[0].Id != "m1" {
		t.Error("a failed auto-summarisation must leave the memories intact")
	}

	if len(s.summarisationCandidates) != 1 {
		t.Errorf("a failed candidate must remain in the list, got %+v", s.summarisationCandidates)
	}
}
