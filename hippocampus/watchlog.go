package hippocampus

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// The Watch RPC over the change log (TODO-3 item 204): resumable, and the whole store's feed on
// every instance. See db/changelog.go for why the log exists and how it is written; this is how it
// is read.
//
// Each instance runs one READER, which polls the log and publishes what it finds to this process's
// hub, exactly as the commit path publishes when the log is off. So a replica's watchers hear the
// consolidating instance's forgetting, and every instance's watchers hear every write. Each event
// carries its seq, and a watcher remembers the highest seq it has passed, which is what makes two
// things possible that the live feed cannot do:
//
//   - RESUME. A watcher that names a cursor is replayed the log after it before it hears anything
//     live, and the live events that arrived meanwhile are skipped up to the last seq replayed.
//   - CATCH UP. A watcher too slow for its buffer is no longer told it dropped events; it re-reads
//     what it missed from the log, the same way it would resume.
//
// The reader advances its cursor only over a contiguous run of seqs, because a seq becomes visible
// at COMMIT rather than at insert (db/changelog.go). A missing number is held open for
// changeGapGrace, which must outlast any transaction that writes a change, and then taken to have
// rolled back. The cost is latency: while a gap is held, the changes after it wait. On SQLite there
// is one writer and no gaps.
//
// The reader's cursor is also the bound every replay reads to. A replay that read past it could send
// a change the reader has not settled, and then skip the one in the gap when it did settle.

// changeReadBatch is how many changes one poll or one replay page reads.
var changeReadBatch = 500

// changeGapGrace is how long the reader waits for a missing seq to commit before passing it. It has
// to outlast the longest transaction that writes a change, which storage.queryTimeout bounds; ten
// seconds is far past any the service runs that records one.
const changeGapGrace = 10 * time.Second

// changeReader is the instance's reader of the change log.
type changeReader struct {
	// settled is the highest seq published, every seq at or below it having been published or given
	// up on. It is stored BEFORE the change is published, so a watcher that sees an event can always
	// replay up to it.
	settled atomic.Int64

	interval time.Duration

	// gapGrace is changeGapGrace, held per reader so a test can shorten it.
	gapGrace time.Duration

	// gapSeq and gapSince are the gap being held open, if any. Touched only by the reader goroutine.
	gapSeq   int64
	gapSince time.Time

	stop    chan struct{}
	stopped chan struct{}
}

// startChangeReader starts the reader when the store records a change log. A no-op otherwise, and
// the hub is then fed in process, as item 193 built it.
func (s *Server) startChangeReader() {
	log.Trace("func() startChangeReader")

	if !s.db.ChangeLogging() {

		return
	}

	interval := s.changeLogPoll
	if interval <= 0 {
		interval = time.Second
	}

	reader := &changeReader{
		interval: interval,
		gapGrace: changeGapGrace,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}

	// The reader starts at the head: what is already in the log is history, served to a watcher that
	// asks for it by resuming, not news.
	head, err := s.db.ChangeLogHead(context.Background())
	if err != nil {
		log.Warnf("watch: failed to read the change log's head, starting from the beginning: %s", err.Error())
	}

	reader.settled.Store(head)

	s.changes = reader

	log.Infof("watch: reading the change log every %s, from seq %d", interval, head)

	go s.changeReadLoop(reader)
}

// stopChangeReader stops the reader and waits for it.
func (s *Server) stopChangeReader() {
	if s.changes == nil {

		return
	}

	close(s.changes.stop)
	<-s.changes.stopped
}

// changeReadLoop polls until stopped. A poll that filled its batch goes again at once.
func (s *Server) changeReadLoop(reader *changeReader) {
	defer close(reader.stopped)

	for {
		full := s.readChangesOnce(reader)

		if full {
			select {

			case <-reader.stop:
				return

			default:

			}

			continue
		}

		select {

		case <-reader.stop:
			return

		case <-time.After(reader.interval):

		}
	}
}

// readChangesOnce reads one batch past the cursor and publishes the contiguous run at its front. It
// reports whether the batch was full, so the loop knows there is more to read now.
func (s *Server) readChangesOnce(reader *changeReader) bool {
	ctx := context.Background()

	changes, err := s.db.ReadChanges(ctx, reader.settled.Load(), 0, changeReadBatch)
	if err != nil {
		log.Warnf("watch: failed to read the change log: %s", err.Error())

		return false
	}

	now := time.Now()

READ:
	for _, change := range changes {
		next := reader.settled.Load() + 1

		if change.Seq != next {
			if reader.gapSeq != next {
				reader.gapSeq = next
				reader.gapSince = now
			}

			if now.Sub(reader.gapSince) < reader.gapGrace {
				break READ
			}

			log.Debugf("watch: seqs %d to %d never committed, passing them", next, change.Seq-1)

			tel.watchGapsPassed.Add(ctx, 1)
		}

		reader.gapSeq = 0

		reader.settled.Store(change.Seq)

		s.watch.publishEvent(ctx, changeToEvent(change))
	}

	return len(changes) == changeReadBatch && reader.gapSeq == 0
}

// changeToEvent projects one change log row onto the wire.
func changeToEvent(change db.Change) *contract.WatchEvent {
	event := &contract.WatchEvent{
		Kind:    callbackKinds[change.Kind],
		Cause:   deleteCauses[change.Cause],
		CycleId: change.CycleId,
		Time:    change.RecordedAt,
		Seq:     change.Seq,
		Items:   watchItems(change.Items),
	}

	if len(change.Report) > 0 {
		report := &contract.CycleReport{}

		// A report that cannot be decoded still announces the completion, with no counts.
		if err := proto.Unmarshal(change.Report, report); err != nil {
			log.Warnf("watch: change %d carries a cycle report that could not be decoded: %s", change.Seq, err.Error())
		} else {
			event.Cycle = report
		}
	}

	return event
}

// watchItems projects the store's items onto the wire.
func watchItems(in []db.CallbackItem) []*contract.WatchItem {
	out := make([]*contract.WatchItem, 0, len(in))

	for _, item := range in {
		out = append(out, &contract.WatchItem{
			Id:           item.Id,
			EventId:      item.EventId,
			Group:        item.Group,
			Significance: item.Significance,
			Bytes:        item.Bytes,
		})
	}

	return out
}

// logCycle records a finished cycle in the change log, where every instance's reader picks it up.
// Best-effort, as the completion callback is: a cycle never fails for its announcement.
func (s *Server) logCycle(ctx context.Context, cycleId int64, report *cycleReport) {
	encoded, err := proto.Marshal(cycleReportToProto(report))
	if err != nil {
		log.Warnf("watch: failed to encode the cycle report for the change log: %s", err.Error())

		return
	}

	if err := s.db.AppendChange(ctx, db.Change{Kind: db.CallbackKindSleepCompleted, CycleId: cycleId, Report: encoded}); err != nil {
		log.Warnf("watch: failed to record the cycle's completion in the change log: %s", err.Error())
	}
}

// pruneChangeLog applies the change log's caps, at the end of every sleep cycle on the consolidating
// instance. Best-effort, like pruneTombstones.
func (s *Server) pruneChangeLog(ctx context.Context) {
	log.Debug("pruneChangeLog()")

	if !s.db.ChangeLogging() {

		return
	}

	ctx, span := tel.tracer.Start(ctx, "prune_change_log")
	defer span.End()

	pruned, err := s.db.PruneChangeLog(ctx)
	if err != nil {
		log.Warnf("failed to prune the change log: %s", err.Error())
		span.RecordError(err)

		return
	}

	if pruned > 0 {
		log.Debugf("pruned %d changes from the change log", pruned)

		span.AddEvent("change_log_pruned", trace.WithAttributes(attribute.Int64("pruned", pruned)))
	}
}

// watchCursor is where one Watch stream stands in the change log: the highest seq it has passed,
// whether it sent that change or the change was not for it. Live events at or below it are ones a
// replay has already dealt with.
type watchCursor struct {
	seq int64
}

// openCursor validates a request's resume and returns where the stream starts. Without the log, a
// cursor means nothing and a resume is refused rather than quietly served live.
func (s *Server) openCursor(ctx context.Context, in *contract.WatchRequest) (*watchCursor, bool, error) {
	if s.changes == nil {
		if in.GetResume() || in.GetAfterSeq() != 0 {

			return nil, false, status.Error(codes.FailedPrecondition, "Watch cannot resume: watch.changeLog.enabled is off on this service")
		}

		return nil, false, nil
	}

	if !in.GetResume() {
		if in.GetAfterSeq() != 0 {

			return nil, false, status.Error(codes.InvalidArgument, "after_seq is only meaningful with resume")
		}

		return &watchCursor{seq: s.changes.settled.Load()}, false, nil
	}

	if in.GetAfterSeq() < 0 {

		return nil, false, status.Error(codes.InvalidArgument, "after_seq must not be negative")
	}

	if err := s.checkCursor(ctx, in.GetAfterSeq()); err != nil {

		return nil, false, err
	}

	return &watchCursor{seq: in.GetAfterSeq()}, true, nil
}

// checkCursor refuses a cursor the log no longer holds. A cursor is a seq the client was handed, so
// the row existed; if it is gone the caps took it, and an unknown number of changes after it.
func (s *Server) checkCursor(ctx context.Context, seq int64) error {
	if seq == 0 {

		return nil
	}

	present, err := s.db.HasChange(ctx, seq)
	if err != nil {

		return mapError(err)
	}

	if !present {

		return status.Errorf(codes.OutOfRange, "the change log no longer holds seq %d: re-read the store and watch again from a new cursor", seq)
	}

	return nil
}

// replay sends a watcher the changes after its cursor, up to what the reader has settled, filtered
// as live events are. It is the resume, the start of a live stream, and the catch-up after a full
// buffer; the caller has checked the cursor. It reports how many events it sent.
func (s *Server) replay(
	ctx context.Context,
	w *watcher,
	cursor *watchCursor,
	stream grpc.ServerStreamingServer[contract.WatchEvent],
) (int64, error) {
	var sent int64

	for {
		through := s.changes.settled.Load()

		if cursor.seq >= through {

			return sent, nil
		}

		changes, err := s.db.ReadChanges(ctx, cursor.seq, through, changeReadBatch)
		if err != nil {

			return sent, mapError(err)
		}

		if len(changes) == 0 {

			return sent, nil
		}

		for _, change := range changes {
			cursor.seq = change.Seq

			event, ok := w.filter(changeToEvent(change))
			if !ok {
				continue
			}

			if err := stream.Send(event); err != nil {

				return sent, err
			}

			sent++
		}

		tel.watchReplayed.Add(ctx, int64(len(changes)))
	}
}

// cursorHeader is the response header naming where a stream starts in the change log.
const cursorHeader = "hippocampus-watch-seq"

// cursorHeaderValue renders the header's value.
func cursorHeaderValue(cursor *watchCursor) string {
	return strconv.FormatInt(cursor.seq, 10)
}
