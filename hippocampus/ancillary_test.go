package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// failingAncillaryStore answers every AncillaryStorage call with an error, leaving the rest of the
// store real. It exists for the one behaviour a real database will not produce on demand: a
// measurement that cannot be taken.
type failingAncillaryStore struct {
	db.Store
}

func (failingAncillaryStore) AncillaryStorage(context.Context) (db.AncillaryStorage, error) {
	return db.AncillaryStorage{}, errors.New("the queue could not be counted")
}

// TestSleepMeasuresTheStorageOutsideTheTarget covers the whole path: the cycle takes the reading,
// caches it, and GetConsolidationStatus serves it.
//
// The forgotten log is the table used because a cycle fills it as a side effect of doing its job,
// which is the state this report exists to describe - the store spending bytes on the record of
// what it just deleted, in a place the capacity target will never look.
func TestSleepMeasuresTheStorageOutsideTheTarget(t *testing.T) {
	s := statusServer(t)
	s.consolidation.tombstones = true

	if store, ok := s.db.(*db.DB); ok {
		store.SetTombstonePolicy(db.TombstonePolicy{Enabled: true})
	}

	seedDoomed(t, s, 3)

	before := time.Now()

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	ancillary := res.GetAncillary()

	if ancillary == nil {
		t.Fatal("no ancillary measurement reported after a cycle ran")
	}

	// The reading is only interpretable with its age, since it is taken per cycle rather than per
	// request - a client shown a bare figure would take it for a live one.
	if ancillary.GetMeasuredAt() < before.UnixNano() {
		t.Errorf("measured_at = %d, want a time at or after the cycle started (%d)", ancillary.GetMeasuredAt(), before.UnixNano())
	}

	log := ancillary.GetForgottenLog()

	if !log.GetEnabled() {
		t.Error("the forgotten log reports disabled on a store recording into it")
	}

	if log.GetRows() != 3 {
		t.Errorf("forgotten log rows = %d, want the 3 memories the cycle forgot", log.GetRows())
	}

	if log.GetBytes() <= 0 {
		t.Error("a log holding rows reported no bytes")
	}

	if ancillary.GetTotalBytes() != log.GetBytes() {
		t.Errorf("total_bytes = %d with only the log holding anything, want %d", ancillary.GetTotalBytes(), log.GetBytes())
	}

	// The other two are reported whatever their state, because an omitted table reads as a table
	// with nothing in it - which is the opposite conclusion from a table nobody enabled.
	if ancillary.GetSearchOutbox() == nil || ancillary.GetCallbackQueue() == nil {
		t.Error("a table was omitted rather than reported as disabled")
	}
}

// TestAncillaryMeasurementFailureKeepsTheLastReading pins the one thing this must not do on a
// failure: replace a real figure with a zero. A stale reading carries its own measured_at and can be
// interpreted; a fresh zero says the queues are empty when nobody knows whether they are.
func TestAncillaryMeasurementFailureKeepsTheLastReading(t *testing.T) {
	s := statusServer(t)

	measured := time.Now().Add(-time.Hour)

	s.lastAncillary.Store(&ancillarySnapshot{
		measuredAt: measured,
		storage: db.AncillaryStorage{
			CallbackQueue: db.AncillaryTable{Enabled: true, Rows: 12, Bytes: 6144},
		},
	})

	s.db = failingAncillaryStore{Store: s.db}

	s.recordAncillaryStorage(context.Background())

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	ancillary := res.GetAncillary()

	if ancillary == nil {
		t.Fatal("a failed measurement discarded the previous one")
	}

	if ancillary.GetMeasuredAt() != measured.UnixNano() {
		t.Errorf("measured_at moved to %d on a failed measurement, want the previous %d",
			ancillary.GetMeasuredAt(),
			measured.UnixNano(),
		)
	}

	if ancillary.GetCallbackQueue().GetRows() != 12 {
		t.Errorf("callback queue rows = %d after a failed measurement, want the previous 12", ancillary.GetCallbackQueue().GetRows())
	}
}

// TestAncillaryIsAbsentUntilACycleHasRun is the state a console has to be able to tell from "the
// queues are empty", and it is the ordinary state of a freshly started instance.
func TestAncillaryIsAbsentUntilACycleHasRun(t *testing.T) {
	s := statusServer(t)

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if res.GetAncillary() != nil {
		t.Errorf("an instance that has run no cycle reported a measurement: %s", res.GetAncillary().String())
	}
}

// TestAncillaryToProtoCarriesEveryTable covers the projection on its own, so a table added to the
// storage struct and forgotten on the wire fails here rather than by being silently absent from a
// console that renders whatever it is given.
func TestAncillaryToProtoCarriesEveryTable(t *testing.T) {
	measured := time.Now()

	out := ancillaryToProto(&ancillarySnapshot{
		measuredAt: measured,
		storage: db.AncillaryStorage{
			ForgottenLog:  db.AncillaryTable{Enabled: true, Rows: 1, Bytes: 10},
			SearchOutbox:  db.AncillaryTable{Enabled: true, Rows: 2, Bytes: 20},
			CallbackQueue: db.AncillaryTable{Rows: 3, Bytes: 30},
		},
	})

	if out.GetMeasuredAt() != measured.UnixNano() {
		t.Errorf("measured_at = %d, want %d", out.GetMeasuredAt(), measured.UnixNano())
	}

	if out.GetTotalBytes() != 60 {
		t.Errorf("total_bytes = %d, want the three tables' 60", out.GetTotalBytes())
	}

	for name, got := range map[string]struct {
		rows    int64
		bytes   int64
		enabled bool
	}{
		"forgotten log":  {out.GetForgottenLog().GetRows(), out.GetForgottenLog().GetBytes(), out.GetForgottenLog().GetEnabled()},
		"search outbox":  {out.GetSearchOutbox().GetRows(), out.GetSearchOutbox().GetBytes(), out.GetSearchOutbox().GetEnabled()},
		"callback queue": {out.GetCallbackQueue().GetRows(), out.GetCallbackQueue().GetBytes(), out.GetCallbackQueue().GetEnabled()},
	} {
		if got.bytes != got.rows*10 {
			t.Errorf("%s: bytes %d do not belong to rows %d - the tables are crossed", name, got.bytes, got.rows)
		}
	}

	// A table holding rows while nothing records into it is a real state, and enabled must carry it
	// rather than being derived from the row count.
	if out.GetCallbackQueue().GetEnabled() {
		t.Error("the callback queue reported enabled from a measurement that said otherwise")
	}
}
