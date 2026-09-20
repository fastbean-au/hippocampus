package hippocampus

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// failingAncillaryStore answers every AncillaryStorage call with an error, leaving the rest of the
// store real. It exists for the one behaviour a real database will not produce on demand: a
// measurement that cannot be taken.
type failingAncillaryStore struct {
	db.Store
}

func (failingAncillaryStore) AncillaryStorage(context.Context, db.AncillaryBounds) (db.AncillaryStorage, error) {
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
			ForgottenLog:  db.AncillaryTable{Enabled: true, Rows: 1, Bytes: 10, LimitBytes: 100},
			SearchOutbox:  db.AncillaryTable{Enabled: true, Rows: 2, Bytes: 20, LimitBytes: 200},
			CallbackQueue: db.AncillaryTable{Rows: 3, Bytes: 30, LimitBytes: 300},
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

	// Each table's cap travels with its figure, and belongs to that table rather than to whichever
	// one happened to be projected first - a byte count with the wrong bound beside it is worse
	// than one with none, since it reads as headroom that is not there.
	for name, one := range map[string]struct{ got, want int64 }{
		"forgotten log":  {out.GetForgottenLog().GetLimitBytes(), 100},
		"search outbox":  {out.GetSearchOutbox().GetLimitBytes(), 200},
		"callback queue": {out.GetCallbackQueue().GetLimitBytes(), 300},
	} {
		if one.got != one.want {
			t.Errorf("%s: limit_bytes = %d, want %d", name, one.got, one.want)
		}
	}
}

// TestAncillaryBoundsComeFromTheEnforcedFields is what stops the console reassuring about a bound
// that is not the one being applied: the bounds handed to the measurement are the same fields the
// prune paths are given, not a second reading of configuration.
//
// The forgotten log is deliberately not among them - its policy lives on the store, and the
// measurement reads it there - which is why there was a tombstoneMaxBytes mirror on the server to
// remove when this landed.
func TestAncillaryBoundsComeFromTheEnforcedFields(t *testing.T) {
	s := &Server{
		outboxBounds:   db.QueueBounds{MaxRows: 10, MaxBytes: 4096, MaxAge: time.Hour},
		callbackBounds: db.QueueBounds{MaxRows: 20, MaxBytes: 8192, MaxAge: 2 * time.Hour},
	}

	bounds := s.ancillaryBounds()

	if bounds.SearchOutbox != s.outboxBounds {
		t.Errorf("the outbox is measured against %+v but %+v is enforced", bounds.SearchOutbox, s.outboxBounds)
	}

	if bounds.CallbackQueue != s.callbackBounds {
		t.Errorf("the callback queue is measured against %+v but %+v is enforced", bounds.CallbackQueue, s.callbackBounds)
	}

	// Unset is unbounded, and must be reported as 0 rather than as some sentinel a client would
	// render as a cap.
	if (&Server{}).ancillaryBounds() != (db.AncillaryBounds{}) {
		t.Error("an unconfigured instance reported a cap")
	}
}

// TestAncillaryTableProjectionCarriesEveryBound is the drift guard on the other half of the
// projection: a field added to the storage struct and forgotten here is invisible on the wire, and a
// console renders whatever it is given without noticing that a bound never arrived.
func TestAncillaryTableProjectionCarriesEveryBound(t *testing.T) {
	out := ancillaryTableToProto(db.AncillaryTable{
		Enabled:    true,
		Rows:       10,
		Bytes:      1920,
		DiskBytes:  4096,
		Oldest:     12345,
		LimitRows:  50,
		LimitBytes: 9600,
		LimitAge:   48 * time.Hour,
		Binding:    db.BindingRows,
	})

	for name, one := range map[string]struct{ got, want int64 }{
		"rows":              {out.GetRows(), 10},
		"bytes":             {out.GetBytes(), 1920},
		"disk_bytes":        {out.GetDiskBytes(), 4096},
		"oldest_at":         {out.GetOldestAt(), 12345},
		"limit_rows":        {out.GetLimitRows(), 50},
		"limit_bytes":       {out.GetLimitBytes(), 9600},
		"limit_age_seconds": {out.GetLimitAgeSeconds(), int64((48 * time.Hour).Seconds())},
	} {
		if one.got != one.want {
			t.Errorf("%s = %d, want %d", name, one.got, one.want)
		}
	}

	if out.GetBindingLimit() != string(db.BindingRows) {
		t.Errorf("binding_limit = %q, want %q", out.GetBindingLimit(), db.BindingRows)
	}
}

// TestTotalBytesIsTheFootprint pins which of the two byte figures the wire's total is summed from.
// The question total_bytes answers is what disk this deployment needs beyond its capacity target, so
// it is the engine's reading where there is one - and is therefore deliberately NOT the sum of the
// three `bytes` fields beside it.
func TestTotalBytesIsTheFootprint(t *testing.T) {
	out := ancillaryToProto(&ancillarySnapshot{
		measuredAt: time.Now(),
		storage: db.AncillaryStorage{
			ForgottenLog:  db.AncillaryTable{Enabled: true, Rows: 1, Bytes: 100, DiskBytes: 400},
			SearchOutbox:  db.AncillaryTable{Enabled: true, Rows: 1, Bytes: 10},
			CallbackQueue: db.AncillaryTable{Enabled: true, Rows: 1, Bytes: 5, DiskBytes: 40},
		},
	})

	if out.GetTotalBytes() != 450 {
		t.Errorf("total_bytes = %d, want 450 - the two measurements plus the one estimate", out.GetTotalBytes())
	}
}

// TestUnreachableWindow is the state TODO-2 item 126 is about, stated as a predicate: a table held at
// its row cap while an age cap is also configured is holding less history than its operator asked
// for, and nothing about it is an error - both caps are enforced and neither is violated.
func TestUnreachableWindow(t *testing.T) {
	for _, one := range []struct {
		name  string
		table db.AncillaryTable
		want  bool
	}{
		{"row cap with an age cap beside it", db.AncillaryTable{Binding: db.BindingRows, LimitAge: time.Hour}, true},
		{"byte cap with an age cap beside it", db.AncillaryTable{Binding: db.BindingBytes, LimitAge: time.Hour}, true},
		{"the age cap is the one acting", db.AncillaryTable{Binding: db.BindingAge, LimitAge: time.Hour}, false},
		{"a row cap and no age cap asked for", db.AncillaryTable{Binding: db.BindingRows}, false},
		{"nothing bounds it at all", db.AncillaryTable{Binding: db.BindingNone}, false},
	} {
		t.Run(one.name, func(t *testing.T) {
			if got := unreachableWindow(one.table); got != one.want {
				t.Errorf("unreachableWindow(%+v) = %t, want %t", one.table, got, one.want)
			}
		})
	}
}

// TestDescribeBindingStatesTheSpan is what an operator reads in a log with no metrics stack behind
// it. The span is the number that makes the mismatch visible: a row count is the same whether the
// log holds five days or thirty.
func TestDescribeBindingStatesTheSpan(t *testing.T) {
	measured := time.Now()

	line := describeBinding("forgotten_log", db.AncillaryTable{
		Rows:     100000,
		Oldest:   measured.Add(-125 * time.Hour).UnixNano(),
		LimitAge: 30 * 24 * time.Hour,
		Binding:  db.BindingRows,
	}, measured)

	for _, want := range []string{"forgotten_log", "row cap", "100000 rows", "5.2 days of history", "30 days"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line %q does not state %q", line, want)
		}
	}

	// An unbounded table has no span worth stating and one thing worth saying instead.
	unbounded := describeBinding("search_outbox", db.AncillaryTable{Rows: 7, Binding: db.BindingNone}, measured)

	if !strings.Contains(unbounded, "nothing will remove them") {
		t.Errorf("an unbounded table's line is %q", unbounded)
	}

	// And a table bound by exactly the cap that was asked for says what it holds and stops there.
	aged := describeBinding("forgotten_log", db.AncillaryTable{
		Rows:     5,
		Oldest:   measured.Add(-24 * time.Hour).UnixNano(),
		LimitAge: 30 * 24 * time.Hour,
		Binding:  db.BindingAge,
	}, measured)

	if strings.Contains(aged, "short of") {
		t.Errorf("an age-bound table was reported as falling short of its own cap: %q", aged)
	}
}

// TestBindingIsReportedOnceUntilItChanges is why this line is affordable at all: a cycle runs every
// sleep.periodSeconds, and a line per table per cycle is a line nobody reads.
//
// It pins the severity split too, which is most of what the line is for. A table held by exactly the
// cap its operator expressed is the arrangement working and is Info; a window the store forgets too
// fast to reach, and a table nothing will ever trim, are the two an operator has to act on.
func TestBindingIsReportedOnceUntilItChanges(t *testing.T) {
	var buf bytes.Buffer

	restoreOutput := logrus.StandardLogger().Out
	restoreLevel := logrus.GetLevel()

	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.InfoLevel)

	t.Cleanup(func() {
		logrus.SetOutput(restoreOutput)
		logrus.SetLevel(restoreLevel)
	})

	at := time.Now()

	// The row cap holds a log that also asks for thirty days: TODO-2 item 126's state, and the one
	// worth waking somebody for.
	rowBound := db.AncillaryStorage{
		ForgottenLog: db.AncillaryTable{
			Enabled:   true,
			Rows:      10,
			LimitRows: 10,
			LimitAge:  30 * 24 * time.Hour,
			Oldest:    at.Add(-125 * time.Hour).UnixNano(),
			Binding:   db.BindingRows,
		},
	}

	reportBindingChanges(nil, rowBound, at)

	if lines := strings.Count(buf.String(), "\n"); lines != 1 {
		t.Fatalf("the first measurement produced %d lines, want 1: %s", lines, buf.String())
	}

	if !strings.Contains(buf.String(), "level=warning") {
		t.Errorf("an unreachable age cap was not reported at Warn: %s", buf.String())
	}

	buf.Reset()

	reportBindingChanges(&ancillarySnapshot{measuredAt: at, storage: rowBound}, rowBound, at)

	if buf.Len() != 0 {
		t.Errorf("an unchanged binding was reported again: %s", buf.String())
	}

	ageBound := db.AncillaryStorage{
		ForgottenLog: db.AncillaryTable{
			Enabled:   true,
			Rows:      4,
			LimitRows: 10,
			LimitAge:  30 * 24 * time.Hour,
			Oldest:    at.Add(-24 * time.Hour).UnixNano(),
			Binding:   db.BindingAge,
		},
	}

	reportBindingChanges(&ancillarySnapshot{measuredAt: at, storage: rowBound}, ageBound, at)

	if !strings.Contains(buf.String(), "level=info") {
		t.Errorf("the cap the operator asked for was not reported at Info: %s", buf.String())
	}

	buf.Reset()

	// A table nothing records into is not reported at all: it has no binding worth stating, since
	// disabling the feature stops the trimming as well as the writing.
	reportBindingChanges(nil, db.AncillaryStorage{
		ForgottenLog: db.AncillaryTable{Rows: 10, Binding: db.BindingNone},
	}, at)

	if buf.Len() != 0 {
		t.Errorf("a table nothing records into was reported: %s", buf.String())
	}

	// One nothing bounds, on the other hand, is exactly what wants saying.
	reportBindingChanges(nil, db.AncillaryStorage{
		ForgottenLog: db.AncillaryTable{Enabled: true, Rows: 10, Binding: db.BindingNone},
	}, at)

	if !strings.Contains(buf.String(), "level=warning") {
		t.Errorf("an unbounded table was not reported at Warn: %s", buf.String())
	}
}
