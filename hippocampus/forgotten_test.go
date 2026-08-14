package hippocampus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// newForgottenTestServer returns a server whose store records what it forgets, plus a helper that
// forgets everything currently stored.
func newForgottenTestServer(t *testing.T) (*Server, func()) {
	t.Helper()

	s := newTestServer(t)
	s.consolidation.tombstones = true

	database, ok := s.db.(*db.DB)
	if !ok {
		t.Fatal("the forgotten log needs the concrete store to set its policy")
	}

	database.SetTombstonePolicy(db.TombstonePolicy{Enabled: true})

	return s, func() {
		t.Helper()

		if _, err := s.db.ConsolidateMemories(context.Background(), consolidateEverything{}); err != nil {
			t.Fatalf("ConsolidateMemories: %s", err)
		}
	}
}

func storeMemory(t *testing.T, s *Server, id string, group string) {
	t.Helper()

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{
		Id:           id,
		TimeStamp:    100,
		Significance: 5,
		Body:         "something worth losing",
		Group:        group,
	}); err != nil {
		t.Fatalf("CreateMemory(%s): %s", id, err)
	}
}

// TestGetForgottenMemories is the round trip: a memory forgotten by a cycle comes back as a record
// naming what it was and what decided it, and never as a body.
func TestGetForgottenMemories(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	storeMemory(t, s, "m1", "notes")
	forget()

	res, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(res.GetMemories()) != 1 {
		t.Fatalf("expected 1 record, got %d", len(res.GetMemories()))
	}

	record := res.GetMemories()[0]

	if record.GetId() != "m1" || record.GetGroup() != "notes" {
		t.Errorf("record = %+v, want m1 in group notes", record)
	}

	if record.GetRule() != contract.ForgetRule_FORGET_RULE_CONSOLIDATION {
		t.Errorf("record rule = %v, want CONSOLIDATION", record.GetRule())
	}

	if record.GetThreshold() != 1.0 {
		t.Errorf("record threshold = %v, want the threshold in force (1.0)", record.GetThreshold())
	}

	if res.GetTotal() != 1 {
		t.Errorf("total = %d, want 1", res.GetTotal())
	}

	if !res.GetEnabled() {
		t.Error("enabled = false on a recording store")
	}

	// A tombstone must not become a way to read a memory that is gone. There is no body field on
	// the message, so this is a check that none appears by any other route.
	if rendered := record.String(); strings.Contains(rendered, "something worth losing") {
		t.Errorf("the forgotten log returned the memory's body: %s", rendered)
	}
}

// TestGetForgottenMemoriesReportsWhetherItIsRecording pins the distinction an empty page cannot
// make on its own: nothing forgotten, or nothing written down.
func TestGetForgottenMemoriesReportsWhetherItIsRecording(t *testing.T) {
	s := newTestServer(t)

	res, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if res.GetEnabled() {
		t.Error("enabled = true on a store that is not recording")
	}

	if len(res.GetMemories()) != 0 {
		t.Errorf("expected an empty log, got %d records", len(res.GetMemories()))
	}
}

// TestGetForgottenMemoriesPaginates pins that next_seq is set only when there is another page: a
// cursor on a short page would send the client to fetch an empty one to find out.
func TestGetForgottenMemoriesPaginates(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		storeMemory(t, s, id, "")
	}

	forget()

	page, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{Limit: 2})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(page.GetMemories()) != 2 || page.GetNextSeq() == 0 {
		t.Fatalf("first page = %d records, next_seq %d; want 2 and a cursor", len(page.GetMemories()), page.GetNextSeq())
	}

	last, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{
		Limit:    2,
		AfterSeq: page.GetNextSeq(),
	})
	if err != nil {
		t.Fatalf("GetForgottenMemories (page 2): %s", err)
	}

	if len(last.GetMemories()) != 1 {
		t.Fatalf("second page = %d records, want 1", len(last.GetMemories()))
	}

	if last.GetNextSeq() != 0 {
		t.Errorf("next_seq = %d on the final page, want 0", last.GetNextSeq())
	}
}

// TestGetForgottenMemoriesFiltersByRule covers the wire enum's translation, including that
// UNSPECIFIED means "either" rather than "no rule".
func TestGetForgottenMemoriesFiltersByRule(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	storeMemory(t, s, "m1", "")
	forget()

	consolidated, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{
		Rule: contract.ForgetRule_FORGET_RULE_CONSOLIDATION,
	})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(consolidated.GetMemories()) != 1 {
		t.Errorf("filtering by CONSOLIDATION returned %d records, want 1", len(consolidated.GetMemories()))
	}

	evicted, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{
		Rule: contract.ForgetRule_FORGET_RULE_EVICTION,
	})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(evicted.GetMemories()) != 0 {
		t.Errorf("filtering by EVICTION returned %d records, want none", len(evicted.GetMemories()))
	}
}

// TestDeleteForgottenMemoriesRequiresAChoice is the guard on the one operation that destroys the
// record of what was destroyed: an empty request must never be read as "delete everything".
func TestDeleteForgottenMemoriesRequiresAChoice(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	storeMemory(t, s, "m1", "")
	forget()

	_, err := s.DeleteForgottenMemories(context.Background(), &contract.DeleteForgottenMemoriesRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an empty request = %v, want InvalidArgument", err)
	}

	res, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(res.GetMemories()) != 1 {
		t.Errorf("the refused request deleted %d record(s)", 1-len(res.GetMemories()))
	}
}

// TestDeleteForgottenMemories covers both accepted forms.
func TestDeleteForgottenMemories(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	storeMemory(t, s, "m1", "")
	forget()

	// A cutoff in the past matches nothing.
	before, err := s.DeleteForgottenMemories(context.Background(), &contract.DeleteForgottenMemoriesRequest{
		BeforeTime: 1,
	})
	if err != nil {
		t.Fatalf("DeleteForgottenMemories: %s", err)
	}

	if before.GetDeleted() != 0 {
		t.Errorf("a cutoff before every record deleted %d", before.GetDeleted())
	}

	all, err := s.DeleteForgottenMemories(context.Background(), &contract.DeleteForgottenMemoriesRequest{All: true})
	if err != nil {
		t.Fatalf("DeleteForgottenMemories(all): %s", err)
	}

	if all.GetDeleted() != 1 {
		t.Errorf("clearing the log deleted %d records, want 1", all.GetDeleted())
	}
}

// TestPruneTombstonesIsSkippedWhileDisabled pins the RPC layer's half of "disabling never deletes":
// the sleep cycle does not even ask.
func TestPruneTombstonesIsSkippedWhileDisabled(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	storeMemory(t, s, "m1", "")
	forget()

	database, ok := s.db.(*db.DB)
	if !ok {
		t.Fatal("expected the concrete store")
	}

	// A cap that would trim everything, and the feature switched off.
	database.SetTombstonePolicy(db.TombstonePolicy{Enabled: false, MaxRows: 1})
	s.consolidation.tombstones = false

	s.pruneTombstones(context.Background())

	count, err := s.db.CountForgottenMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("CountForgottenMemories: %s", err)
	}

	if count != 1 {
		t.Errorf("the disabled log holds %d records, want 1 still readable", count)
	}
}

// TestPruneTombstonesTrims is the other half: while enabled, the caps are applied by the cycle.
func TestPruneTombstonesTrims(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	for _, id := range []string{"m1", "m2", "m3"} {
		storeMemory(t, s, id, "")
	}

	forget()

	database, ok := s.db.(*db.DB)
	if !ok {
		t.Fatal("expected the concrete store")
	}

	database.SetTombstonePolicy(db.TombstonePolicy{Enabled: true, MaxRows: 1})

	s.pruneTombstones(context.Background())

	count, err := s.db.CountForgottenMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("CountForgottenMemories: %s", err)
	}

	if count != 1 {
		t.Errorf("the log holds %d records after pruning to a cap of 1", count)
	}
}

// failingForgottenStore fails one forgotten-log call at a time, so each error path is exercised
// without a broken database underneath it.
type failingForgottenStore struct {
	db.Store
	readErr   error
	countErr  error
	deleteErr error
	pruneErr  error
}

func (f *failingForgottenStore) GetForgottenMemories(ctx context.Context, filter db.ForgottenFilter) ([]db.ForgottenMemory, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}

	return f.Store.GetForgottenMemories(ctx, filter)
}

func (f *failingForgottenStore) CountForgottenMemories(ctx context.Context, groups []string) (int64, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}

	return f.Store.CountForgottenMemories(ctx, groups)
}

func (f *failingForgottenStore) DeleteForgottenMemories(ctx context.Context, before int64, groups []string) (int64, error) {
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}

	return f.Store.DeleteForgottenMemories(ctx, before, groups)
}

func (f *failingForgottenStore) PruneTombstones(ctx context.Context) (int64, error) {
	if f.pruneErr != nil {
		return 0, f.pruneErr
	}

	return f.Store.PruneTombstones(ctx)
}

// TestForgottenLogErrorsSurface: a store that cannot answer must say so rather than report an
// empty log, which would read as "nothing was forgotten".
func TestForgottenLogErrorsSurface(t *testing.T) {
	boom := errors.New("boom")

	t.Run("read", func(t *testing.T) {
		s, _ := newForgottenTestServer(t)
		s.db = &failingForgottenStore{Store: s.db, readErr: boom}

		if _, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("count", func(t *testing.T) {
		s, _ := newForgottenTestServer(t)
		s.db = &failingForgottenStore{Store: s.db, countErr: boom}

		if _, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("delete", func(t *testing.T) {
		s, _ := newForgottenTestServer(t)
		s.db = &failingForgottenStore{Store: s.db, deleteErr: boom}

		_, err := s.DeleteForgottenMemories(context.Background(), &contract.DeleteForgottenMemoriesRequest{All: true})
		if err == nil {
			t.Fatal("expected an error")
		}
	})
}

// TestPruneTombstonesNeverFailsTheCycle: trimming the log is tidiness, and a cycle that
// consolidated and evicted correctly has not failed because a record could not be pruned or
// counted.
func TestPruneTombstonesNeverFailsTheCycle(t *testing.T) {
	boom := errors.New("boom")

	for _, failure := range []*failingForgottenStore{{pruneErr: boom}, {countErr: boom}} {
		s, forget := newForgottenTestServer(t)

		storeMemory(t, s, "m1", "")
		forget()

		failure.Store = s.db
		s.db = failure

		// The call must return, and the cycle it sits in must still report success.
		s.pruneTombstones(context.Background())

		if err := s.sleep(); err != nil {
			t.Fatalf("a sleep cycle failed because the forgotten log could not be trimmed: %s", err)
		}
	}
}

// TestDeletionThresholdScalesWithPressure pins what the tombstone's threshold column records: the
// threshold in force, not the configured one.
func TestDeletionThresholdScalesWithPressure(t *testing.T) {
	s := newTestServer(t)
	s.consolidation.deletionThreshold = 10
	s.consolidation.capacityPressure = 1.5

	if got := s.DeletionThreshold(); got != 15 {
		t.Errorf("DeletionThreshold() = %v, want 15 (10 scaled by a pressure of 1.5)", got)
	}

	// A non-positive pressure means "no reading yet" and leaves the threshold unscaled, which is
	// what the first cycle and a store with no capacity target both see.
	s.consolidation.capacityPressure = 0

	if got := s.DeletionThreshold(); got != 10 {
		t.Errorf("DeletionThreshold() with no pressure reading = %v, want the unscaled 10", got)
	}
}

// TestForgottenLogSurvivesADisabledCycle is the end-to-end statement of the requirement: records
// written while the feature was on are still there after it is turned off, and go only when asked.
func TestForgottenLogSurvivesADisabledCycle(t *testing.T) {
	s, forget := newForgottenTestServer(t)

	storeMemory(t, s, "m1", "")
	forget()

	database, ok := s.db.(*db.DB)
	if !ok {
		t.Fatal("expected the concrete store")
	}

	database.SetTombstonePolicy(db.TombstonePolicy{Enabled: false, MaxRows: 1, MaxAgeInDays: 1})
	s.consolidation.tombstones = false

	// A full cycle, with the feature off.
	if err := s.sleep(); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	res, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(res.GetMemories()) != 1 {
		t.Fatalf("a cycle run with the log disabled removed the records it had already written")
	}

	if res.GetEnabled() {
		t.Error("enabled = true after the feature was turned off")
	}

	// And they go when, and only when, asked.
	if _, err := s.DeleteForgottenMemories(context.Background(), &contract.DeleteForgottenMemoriesRequest{
		BeforeTime: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("DeleteForgottenMemories: %s", err)
	}

	after, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(after.GetMemories()) != 0 {
		t.Errorf("the log still holds %d records after an explicit clear", len(after.GetMemories()))
	}
}
