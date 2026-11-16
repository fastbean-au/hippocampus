package hippocampus

import (
	"context"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/db/dbtest"
	"github.com/fastbean-au/hippocampus/types"
)

// The Watch RPC over the change log (TODO-3 item 204): a stream resumes from a cursor, a watcher on
// any instance hears every instance's changes, a slow watcher is caught up rather than told it
// dropped events, and a cursor the caps have removed is refused.

// changeLogServer is a test server over a store that records a change log, read every few
// milliseconds.
func changeLogServer(t *testing.T, policy db.ChangeLogPolicy) (*Server, *db.DB) {
	t.Helper()

	store := dbtest.Open(t)

	policy.Enabled = true
	store.SetChangeLogPolicy(policy)

	return changeLogPeer(t, store), store
}

// changeLogPeer is a second instance over the same store, with its own hub and its own reader.
func changeLogPeer(t *testing.T, store db.Store) *Server {
	t.Helper()

	s := &Server{db: store, changeLogPoll: 5 * time.Millisecond}
	s.startWatch()

	t.Cleanup(s.stopChangeReader)

	return s
}

// createMemories stores memories m0..m(n-1) through the store directly, as a client's writes would.
func createMemories(t *testing.T, store db.Store, prefix string, n int) {
	t.Helper()

	for i := range n {
		id := prefix + strconv.Itoa(i)

		if _, err := store.CreateMemory(context.Background(), types.Memory{Id: id, Body: "x", TimeStamp: 100, Significance: 5, Group: "g"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}
}

// headerStream is a watchStream that keeps the response headers.
type headerStream struct {
	watchStream

	headers chan metadata.MD
}

func (h *headerStream) SendHeader(md metadata.MD) error {
	h.headers <- md

	return nil
}

// TestWatchResumesFromACursor: a stream that names the last seq it received is replayed what came
// after, then goes live, and every event carries its seq.
func TestWatchResumesFromACursor(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{})
	ctx := context.Background()

	createMemories(t, store, "m", 3)

	changes, err := store.ReadChanges(ctx, 0, 0, 10)
	if err != nil || len(changes) != 3 {
		t.Fatalf("ReadChanges = %d changes, %v; want 3", len(changes), err)
	}

	waitForSettled(t, s, changes[2].Seq)

	w := startWatching(t, s, ctx, &contract.WatchRequest{Resume: true, AfterSeq: changes[0].Seq})

	for i, want := range []string{"m1", "m2"} {
		event := w.next(t)

		if event.GetItems()[0].GetId() != want || event.GetSeq() != changes[i+1].Seq {
			t.Errorf("replayed event %d is %v, want %s at seq %d", i, event, want, changes[i+1].Seq)
		}
	}

	createMemories(t, store, "live", 1)

	live := w.next(t)

	if live.GetItems()[0].GetId() != "live0" || live.GetSeq() <= changes[2].Seq {
		t.Errorf("the live event after the replay is %v", live)
	}

	w.none(t)
}

// TestWatchResumeFromZeroReplaysTheWholeLog: after_seq 0 is everything the log still holds, which is
// what a client that has never received an event resumes from.
func TestWatchResumeFromZeroReplaysTheWholeLog(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{})

	createMemories(t, store, "m", 2)

	w := startWatching(t, s, context.Background(), &contract.WatchRequest{Resume: true})

	for _, want := range []string{"m0", "m1"} {
		if event := w.next(t); event.GetItems()[0].GetId() != want {
			t.Errorf("replayed %v, want %s", event, want)
		}
	}
}

// TestWatchRefusesACursorTheLogNoLongerHolds: the caps took the row, and an unknown number after
// it, so the client must re-read rather than be replayed a feed with a hole in it.
func TestWatchRefusesACursorTheLogNoLongerHolds(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{MaxRows: 1})
	ctx := context.Background()

	createMemories(t, store, "m", 3)

	changes, err := store.ReadChanges(ctx, 0, 0, 10)
	if err != nil || len(changes) != 3 {
		t.Fatalf("ReadChanges = %d changes, %v; want 3", len(changes), err)
	}

	if _, err := store.PruneChangeLog(ctx); err != nil {
		t.Fatalf("PruneChangeLog: %s", err)
	}

	err = s.Watch(
		&contract.WatchRequest{Resume: true, AfterSeq: changes[0].Seq},
		&watchStream{ctx: ctx, events: make(chan *contract.WatchEvent, 1)},
	)

	if status.Code(err) != codes.OutOfRange {
		t.Errorf("a resume from a pruned cursor returned %v, want OutOfRange", err)
	}
}

// TestWatchRefusesAResumeWithoutTheLog: with the log off there is no cursor, and a resume served live
// would silently miss whatever the client was resuming for.
func TestWatchRefusesAResumeWithoutTheLog(t *testing.T) {
	t.Parallel()

	s := watchServer(t)

	cases := []struct {
		in   *contract.WatchRequest
		want codes.Code
	}{
		{&contract.WatchRequest{Resume: true}, codes.FailedPrecondition},
		{&contract.WatchRequest{AfterSeq: 5}, codes.FailedPrecondition},
	}

	for _, c := range cases {
		err := s.Watch(c.in, &watchStream{ctx: context.Background(), events: make(chan *contract.WatchEvent, 1)})

		if status.Code(err) != c.want {
			t.Errorf("Watch(%v) returned %v, want %s", c.in, err, c.want)
		}
	}

	logged, _ := changeLogServer(t, db.ChangeLogPolicy{})

	err := logged.Watch(&contract.WatchRequest{AfterSeq: 5}, &watchStream{ctx: context.Background(), events: make(chan *contract.WatchEvent, 1)})

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("after_seq without resume returned %v, want InvalidArgument", err)
	}
}

// TestWatchHeadersCarryTheCursor: a client that has never received an event still learns where its
// stream started, and can resume from there.
func TestWatchHeadersCarryTheCursor(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{})

	createMemories(t, store, "m", 2)

	head, err := store.ChangeLogHead(context.Background())
	if err != nil {
		t.Fatalf("ChangeLogHead: %s", err)
	}

	waitForSettled(t, s, head)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &headerStream{
		watchStream: watchStream{ctx: ctx, events: make(chan *contract.WatchEvent, 8)},
		headers:     make(chan metadata.MD, 1),
	}

	go func() { _ = s.Watch(&contract.WatchRequest{}, stream) }()

	select {

	case md := <-stream.headers:
		if got := md.Get(cursorHeader); len(got) != 1 || got[0] != strconv.FormatInt(head, 10) {
			t.Errorf("the %s header is %v, want %d", cursorHeader, got, head)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("no headers arrived")

	}
}

// TestAnyInstanceServesTheWholeFeed is the item's cross-instance half: a write made through one
// instance and a cycle finished on it reach a watcher on another, through the log alone.
func TestAnyInstanceServesTheWholeFeed(t *testing.T) {
	t.Parallel()

	writer, store := changeLogServer(t, db.ChangeLogPolicy{})
	peer := changeLogPeer(t, store)

	w := startWatching(t, peer, context.Background(), &contract.WatchRequest{})

	createMemories(t, writer.db, "m", 1)

	writer.announceCycle(context.Background(), 42, &cycleReport{startedAt: time.Unix(0, 100), trigger: "timer", memoriesConsolidated: 3, success: true})

	if event := w.next(t); event.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED || event.GetItems()[0].GetId() != "m0" {
		t.Errorf("the peer's watcher heard %v, want m0 stored", event)
	}

	cycle := w.next(t)

	if cycle.GetKind() != contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED || cycle.GetCycleId() != 42 {
		t.Fatalf("the peer's watcher heard %v, want cycle 42's completion", cycle)
	}

	if cycle.GetCycle().GetMemoriesConsolidated() != 3 || cycle.GetCycle().GetTrigger() != "timer" {
		t.Errorf("the completion lost its report crossing the log: %v", cycle.GetCycle())
	}

	w.none(t)
}

// TestGroupScopeIsolation_WatchResume: a replay is filtered as a live stream is, so a scoped client
// resuming hears nothing of another group's changes.
func TestGroupScopeIsolation_WatchResume(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{})
	ctx := context.Background()

	for _, group := range []string{"b", "a"} {
		if _, err := store.CreateMemory(ctx, types.Memory{Id: "m-" + group, Body: "x", TimeStamp: 100, Significance: 5, Group: group}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", group, err)
		}
	}

	w := startWatching(t, s, scopedContext("a"), &contract.WatchRequest{Resume: true})

	event := w.next(t)

	if event.GetItems()[0].GetId() != "m-a" {
		t.Fatalf("the scoped replay sent %v, want m-a alone", event)
	}

	assertNoLeak(t, "Watch", event.String())

	w.none(t)
}

// gatedStream is a watchStream whose Send waits until the gate opens, so the server's buffer for it
// fills behind it.
type gatedStream struct {
	watchStream

	gate chan struct{}
}

func (g *gatedStream) Send(event *contract.WatchEvent) error {
	<-g.gate

	return g.watchStream.Send(event)
}

// TestASlowWatcherIsCaughtUpFromTheLog: with the log on, a watcher whose buffer overflowed is
// replayed what it missed, in order, once, and is never told it dropped anything.
func TestASlowWatcherIsCaughtUpFromTheLog(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	total := watchBuffer + 50

	stream := &gatedStream{
		watchStream: watchStream{ctx: ctx, events: make(chan *contract.WatchEvent, total+10)},
		gate:        make(chan struct{}),
	}

	before := s.watch.count.Load()

	go func() { _ = s.Watch(&contract.WatchRequest{}, stream) }()

	for s.watch.count.Load() == before {
		time.Sleep(time.Millisecond)
	}

	createMemories(t, store, "m", total)

	head, err := store.ChangeLogHead(context.Background())
	if err != nil {
		t.Fatalf("ChangeLogHead: %s", err)
	}

	waitForSettled(t, s, head)

	close(stream.gate)

	var last int64

	for i := range total {
		select {

		case event := <-stream.events:
			if event.GetDropped() != 0 {
				t.Fatalf("event %d reports %d dropped; with the log on a slow watcher is caught up", i, event.GetDropped())
			}

			if event.GetSeq() <= last {
				t.Fatalf("event %d has seq %d after %d: replayed out of order or twice", i, event.GetSeq(), last)
			}

			last = event.GetSeq()

		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d events arrived", i, total)

		}
	}

	select {

	case event := <-stream.events:
		t.Errorf("an event arrived past the %d written: %v", total, event)

	case <-time.After(50 * time.Millisecond):

	}
}

// gappyStore serves a fixed change log, with whatever gaps a test gives it.
type gappyStore struct {
	db.Store

	changes []db.Change
}

func (g *gappyStore) ReadChanges(_ context.Context, afterSeq int64, throughSeq int64, limit int) ([]db.Change, error) {
	var out []db.Change

	for _, change := range g.changes {
		if change.Seq <= afterSeq || throughSeq > 0 && change.Seq > throughSeq || len(out) == limit {
			continue
		}

		out = append(out, change)
	}

	return out, nil
}

// TestTheReaderHoldsAGapOpenThenPassesIt: a missing seq may be a transaction yet to commit, so the
// reader waits before passing it - and publishes nothing beyond it meanwhile, since a cursor past the
// gap would never see the change in it.
func TestTheReaderHoldsAGapOpenThenPassesIt(t *testing.T) {
	t.Parallel()

	store := &gappyStore{changes: []db.Change{
		{Seq: 1, Kind: db.CallbackKindMemoryStored, Items: []db.CallbackItem{{Id: "m1"}}},
		{Seq: 3, Kind: db.CallbackKindMemoryStored, Items: []db.CallbackItem{{Id: "m3"}}},
	}}

	reader := &changeReader{gapGrace: time.Hour}
	s := &Server{db: store, watch: newWatchHub(), changes: reader}

	w := &watcher{kinds: watchKinds, events: make(chan *contract.WatchEvent, 8)}
	s.watch.subscribe(w)

	s.readChangesOnce(reader)

	if reader.settled.Load() != 1 || len(w.events) != 1 {
		t.Fatalf("inside the grace the reader settled %d and published %d, want 1 and 1", reader.settled.Load(), len(w.events))
	}

	reader.gapGrace = 0

	s.readChangesOnce(reader)

	if reader.settled.Load() != 3 || len(w.events) != 2 {
		t.Errorf("past the grace the reader settled %d and published %d, want 3 and 2", reader.settled.Load(), len(w.events))
	}
}

// waitForSettled waits until the server's reader has reached seq.
func waitForSettled(t *testing.T, s *Server, seq int64) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for s.changes.settled.Load() < seq {
		if time.Now().After(deadline) {
			t.Fatalf("the reader never reached seq %d (at %d)", seq, s.changes.settled.Load())
		}

		time.Sleep(time.Millisecond)
	}
}

// TestTheCycleTrimsTheChangeLog: the consolidating instance applies the log's caps at the end of a
// cycle, and does nothing on a store that keeps no log.
func TestTheCycleTrimsTheChangeLog(t *testing.T) {
	t.Parallel()

	s, store := changeLogServer(t, db.ChangeLogPolicy{MaxRows: 1})
	ctx := context.Background()

	createMemories(t, store, "m", 3)

	s.pruneChangeLog(ctx)

	changes, err := store.ReadChanges(ctx, 0, 0, 10)
	if err != nil {
		t.Fatalf("ReadChanges: %s", err)
	}

	if len(changes) != 1 {
		t.Errorf("the log holds %d changes after the prune, want its cap of 1", len(changes))
	}

	plain := newTestServer(t)

	createMemories(t, plain.db, "m", 1)

	plain.pruneChangeLog(ctx)
}
