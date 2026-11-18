package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// This file targets watchlog.go's remaining coverage gaps: the reader and the cursor checks over a
// store whose change log calls fail on demand, and the reader's read-again-at-once on a full batch.

var errChangeLogStore = errors.New("change log unavailable")

// failingChangeStore records a change log whose every call fails, except ReadChanges, which serves
// changes as gappyStore does until readErr is set.
type failingChangeStore struct {
	gappyStore

	readErr error
}

func (f *failingChangeStore) ChangeLogging() bool {
	return true
}

func (f *failingChangeStore) ChangeLogHead(_ context.Context) (int64, error) {
	return 0, errChangeLogStore
}

func (f *failingChangeStore) ReadChanges(ctx context.Context, afterSeq int64, throughSeq int64, limit int) ([]db.Change, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}

	return f.gappyStore.ReadChanges(ctx, afterSeq, throughSeq, limit)
}

func (f *failingChangeStore) HasChange(_ context.Context, _ int64) (bool, error) {
	return false, errChangeLogStore
}

func (f *failingChangeStore) AppendChange(_ context.Context, _ db.Change) error {
	return errChangeLogStore
}

func (f *failingChangeStore) PruneChangeLog(_ context.Context) (int64, error) {
	return 0, errChangeLogStore
}

// TestTheReaderStartsDespiteAnUnreadableHead: a head that cannot be read starts the reader from the
// beginning rather than leaving the service without a feed, and an unset poll interval defaults to
// a second.
func TestTheReaderStartsDespiteAnUnreadableHead(t *testing.T) {
	t.Parallel()

	s := &Server{db: &failingChangeStore{}}
	s.startWatch()

	t.Cleanup(s.stopChangeReader)

	if s.changes == nil {
		t.Fatal("expected the reader to start when the head cannot be read")
	}

	if s.changes.interval != time.Second || s.changes.settled.Load() != 0 {
		t.Errorf("the reader started every %s from seq %d, want every 1s from seq 0", s.changes.interval, s.changes.settled.Load())
	}
}

// TestTheReaderReadsAFullBatchAgainAtOnce: a poll that fills its batch reads again without waiting
// out the interval, so a backlog is drained at read speed rather than one batch per poll.
func TestTheReaderReadsAFullBatchAgainAtOnce(t *testing.T) {
	t.Parallel()

	total := changeReadBatch + 1
	store := &failingChangeStore{}

	for i := range total {
		store.changes = append(store.changes, db.Change{Seq: int64(i + 1), Kind: db.CallbackKindMemoryStored})
	}

	s := &Server{db: store, changeLogPoll: time.Hour}
	s.startWatch()

	t.Cleanup(s.stopChangeReader)

	waitForSettled(t, s, int64(total))
}

// TestAFailedReadPublishesNothing: a poll whose read fails leaves the cursor where it was and does not
// report a full batch, so the loop waits out its interval before trying again.
func TestAFailedReadPublishesNothing(t *testing.T) {
	t.Parallel()

	store := &failingChangeStore{readErr: errChangeLogStore}

	reader := &changeReader{gapGrace: time.Hour}
	s := &Server{db: store, watch: newWatchHub(), changes: reader}

	if full := s.readChangesOnce(reader); full || reader.settled.Load() != 0 {
		t.Errorf("a failed read reported full=%v and settled %d, want false and 0", full, reader.settled.Load())
	}
}

// TestAnUndecodableCycleReportStillAnnouncesTheCycle: a completion whose report cannot be decoded is
// still published, with no counts.
func TestAnUndecodableCycleReportStillAnnouncesTheCycle(t *testing.T) {
	t.Parallel()

	event := changeToEvent(db.Change{Seq: 7, Kind: db.CallbackKindSleepCompleted, CycleId: 3, Report: []byte{0xff}})

	if event.GetSeq() != 7 || event.GetCycleId() != 3 || event.GetCycle() != nil {
		t.Errorf("an undecodable report projected to %v, want seq 7, cycle 3 and no counts", event)
	}
}

// TestTheChangeLogsBestEffortCallsSurviveAFailingStore: recording a cycle and pruning the log are
// best-effort, so a store that fails both leaves the cycle to finish.
func TestTheChangeLogsBestEffortCallsSurviveAFailingStore(t *testing.T) {
	t.Parallel()

	s := &Server{db: &failingChangeStore{}}
	ctx := context.Background()

	s.logCycle(ctx, 1, &cycleReport{startedAt: time.Now()})
	s.pruneChangeLog(ctx)
}

// TestTheCursorChecksRefuseWhatTheyCannotServe: a negative cursor is an invalid argument, a cursor
// the store cannot look up is an internal failure rather than a silent live stream, and a replay
// whose read fails surfaces that failure.
func TestTheCursorChecksRefuseWhatTheyCannotServe(t *testing.T) {
	t.Parallel()

	store := &failingChangeStore{readErr: errChangeLogStore}

	reader := &changeReader{}
	reader.settled.Store(5)

	s := &Server{db: store, watch: newWatchHub(), changes: reader}
	ctx := context.Background()

	if _, _, err := s.openCursor(ctx, &contract.WatchRequest{Resume: true, AfterSeq: -1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a negative cursor = %v, want InvalidArgument", err)
	}

	if _, _, err := s.openCursor(ctx, &contract.WatchRequest{Resume: true, AfterSeq: 2}); status.Code(err) != codes.Internal {
		t.Errorf("a cursor the store cannot look up = %v, want Internal", err)
	}

	w := &watcher{kinds: watchKinds, events: make(chan *contract.WatchEvent, 1)}

	if sent, err := s.replay(ctx, w, &watchCursor{seq: 1}, nil); sent != 0 || status.Code(err) != codes.Internal {
		t.Errorf("a replay whose read fails = (%d, %v), want (0, Internal)", sent, err)
	}

	store.readErr = nil

	if sent, err := s.replay(ctx, w, &watchCursor{seq: 1}, nil); sent != 0 || err != nil {
		t.Errorf("a replay with nothing to read below the settled seq = (%d, %v), want (0, nil)", sent, err)
	}
}
