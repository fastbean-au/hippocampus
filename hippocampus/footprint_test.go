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

// measuredFootprintStore answers with a fixed reading, leaving the rest of the store real. The
// embedded driver the tests run on reports nothing - correctly, page accounting already counting
// every index inside the capacity target - so the reporting half cannot be exercised against it.
type measuredFootprintStore struct {
	db.Store

	footprint db.StorageFootprint
}

func (s measuredFootprintStore) StorageFootprint(context.Context) (db.StorageFootprint, error) {
	return s.footprint, nil
}

// failingFootprintStore is the one behaviour a real database will not produce on demand.
type failingFootprintStore struct {
	db.Store
}

func (failingFootprintStore) StorageFootprint(context.Context) (db.StorageFootprint, error) {
	return db.StorageFootprint{}, errors.New("the catalogue could not be read")
}

// bloatedFootprint is the shape the measurement this feature came from had: a heap that is fine and
// one index carrying most of the store.
func bloatedFootprint() db.StorageFootprint {
	return db.StorageFootprint{
		Measured: true,
		Bytes:    892_000_000,
		Tables: []db.TableFootprint{
			{
				Table:      "memories",
				Bytes:      765_000_000,
				IndexBytes: 687_000_000,
				Indexes: []db.IndexFootprint{
					{Table: "memories", Index: "idx_memories_consolidation_v3", Bytes: 533_000_000, Entries: 145_524},
					{Table: "memories", Index: "idx_memories_listing_v1", Bytes: 60_000_000, Entries: 145_524},
					{Table: "memories", Index: "memories_pkey", Bytes: 94_000_000, Entries: 145_524},
				},
			},
		},
	}
}

// TestFootprintIsMeasuredAndServed is the whole path: a cycle takes the reading, caches it beside
// the estimate it is to be read against, and GetConsolidationStatus serves both.
func TestFootprintIsMeasuredAndServed(t *testing.T) {
	s := statusServer(t)
	s.db = measuredFootprintStore{Store: s.db, footprint: bloatedFootprint()}

	before := time.Now()

	s.recordStorageFootprint(context.Background(), 153_000_000)

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	footprint := res.GetFootprint()

	if footprint == nil {
		t.Fatal("no footprint reported after a measurement")
	}

	if !footprint.GetMeasured() {
		t.Error("a measurement that was taken reports measured false")
	}

	if footprint.GetMeasuredAt() < before.UnixNano() {
		t.Errorf("measured_at = %d, want a time at or after the measurement (%d)", footprint.GetMeasuredAt(), before.UnixNano())
	}

	if footprint.GetBytes() != 892_000_000 {
		t.Errorf("bytes = %d, want 892000000", footprint.GetBytes())
	}

	if footprint.GetIndexBytes() != 687_000_000 {
		t.Errorf("index_bytes = %d, want 687000000", footprint.GetIndexBytes())
	}

	// The estimate travels WITH the measurement rather than being left for a client to pair up from
	// a second gauge. The gap is the finding, and two readings minutes apart are not one reading.
	if footprint.GetEstimatedBytes() != 153_000_000 {
		t.Errorf("estimated_bytes = %d, want the cycle's 153000000", footprint.GetEstimatedBytes())
	}

	if len(footprint.GetTables()) != 1 || len(footprint.GetTables()[0].GetIndexes()) != 3 {
		t.Fatalf("the per-index breakdown did not survive the projection: %s", footprint.String())
	}
}

// TestFootprintIsAbsentWhereTheDriverCannotMeasure pins the choice not to publish a zeroed block.
// A footprint of 0 bytes on a console reads as a store occupying no disk, which is the one thing
// this reading exists to stop anybody believing - and it is the ordinary state on two of three
// drivers.
func TestFootprintIsAbsentWhereTheDriverCannotMeasure(t *testing.T) {
	s := statusServer(t)
	s.db = measuredFootprintStore{Store: s.db, footprint: db.StorageFootprint{}}

	s.recordStorageFootprint(context.Background(), 1024)

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if res.GetFootprint() != nil {
		t.Errorf("a driver that cannot measure published a footprint: %s", res.GetFootprint().String())
	}
}

// TestFootprintIsAbsentUntilACycleHasRun is the other absence, and the ordinary state of a freshly
// started instance - a console has to be able to tell it from a store occupying nothing.
func TestFootprintIsAbsentUntilACycleHasRun(t *testing.T) {
	s := statusServer(t)

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if res.GetFootprint() != nil {
		t.Errorf("an instance that has run no cycle reported a footprint: %s", res.GetFootprint().String())
	}
}

// TestFootprintMeasurementFailureKeepsTheLastReading is recordAncillaryStorage's rule applied here
// for the same reason: a stale figure carries its own measured_at and can be interpreted, while a
// fresh zero says the store occupies no disk.
func TestFootprintMeasurementFailureKeepsTheLastReading(t *testing.T) {
	s := statusServer(t)

	measured := time.Now().Add(-time.Hour)

	s.lastFootprint.Store(&footprintSnapshot{
		measuredAt: measured,
		estimated:  1000,
		footprint:  db.StorageFootprint{Measured: true, Bytes: 5000},
	})

	s.db = failingFootprintStore{Store: s.db}

	s.recordStorageFootprint(context.Background(), 2000)

	res, err := s.GetConsolidationStatus(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	footprint := res.GetFootprint()

	if footprint == nil {
		t.Fatal("a failed measurement discarded the previous one")
	}

	if footprint.GetMeasuredAt() != measured.UnixNano() {
		t.Errorf("measured_at moved to %d on a failed measurement, want the previous %d", footprint.GetMeasuredAt(), measured.UnixNano())
	}

	if footprint.GetBytes() != 5000 {
		t.Errorf("bytes = %d after a failed measurement, want the previous 5000", footprint.GetBytes())
	}
}

// TestBloatFactorNeedsBothHalves covers the ratio's one refusal. An estimate of zero is a cycle
// that could not take the reading, and dividing by it would publish an infinity; a reading nobody
// took is not a ratio of 1.
func TestBloatFactorNeedsBothHalves(t *testing.T) {
	cases := []struct {
		name string
		in   footprintSnapshot
		want float64
	}{
		{"bloated", footprintSnapshot{estimated: 100, footprint: db.StorageFootprint{Measured: true, Bytes: 580}}, 5.8},
		{"healthy", footprintSnapshot{estimated: 100, footprint: db.StorageFootprint{Measured: true, Bytes: 120}}, 1.2},
		{"no estimate", footprintSnapshot{estimated: 0, footprint: db.StorageFootprint{Measured: true, Bytes: 580}}, 0},
		{"not measured", footprintSnapshot{estimated: 100, footprint: db.StorageFootprint{Bytes: 580}}, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.bloatFactor(); got != c.want {
				t.Errorf("bloatFactor() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBloatIsReportedOnceUntilItChanges is why the log line is affordable: a cycle runs every
// sleep.periodSeconds, and a line per cycle is a line nobody reads. It pins the recovery line too -
// a reindex having worked is the other half of the same operator's question, and nothing else
// anywhere confirms it.
func TestBloatIsReportedOnceUntilItChanges(t *testing.T) {
	var buf bytes.Buffer

	restoreOutput := logrus.StandardLogger().Out
	restoreLevel := logrus.GetLevel()

	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.InfoLevel)

	t.Cleanup(func() {
		logrus.SetOutput(restoreOutput)
		logrus.SetLevel(restoreLevel)
	})

	bloated := &footprintSnapshot{estimated: 153_000_000, footprint: bloatedFootprint()}

	healthy := &footprintSnapshot{
		estimated: 153_000_000,
		footprint: db.StorageFootprint{Measured: true, Bytes: 170_000_000},
	}

	// A first measurement has nothing to have crossed, and after a restart the condition has
	// usually been building for days - so an instance starting into it says so.
	reportBloat(nil, bloated)

	if lines := strings.Count(buf.String(), "\n"); lines != 1 {
		t.Fatalf("the first measurement produced %d lines, want 1: %s", lines, buf.String())
	}

	if !strings.Contains(buf.String(), "level=warning") {
		t.Errorf("a store holding 5.8x its estimate was not reported at Warn: %s", buf.String())
	}

	// The index costing the most per entry is named, because an operator acts on a name and the
	// remedy is per index.
	if !strings.Contains(buf.String(), "idx_memories_consolidation_v3") {
		t.Errorf("the worst index was not named: %s", buf.String())
	}

	buf.Reset()

	reportBloat(bloated, bloated)

	if buf.Len() != 0 {
		t.Errorf("an unchanged reading was reported again: %s", buf.String())
	}

	buf.Reset()

	reportBloat(bloated, healthy)

	if !strings.Contains(buf.String(), "level=info") {
		t.Errorf("a store that came back under the threshold said nothing at Info: %s", buf.String())
	}

	buf.Reset()

	reportBloat(healthy, healthy)

	if buf.Len() != 0 {
		t.Errorf("a healthy store reported anything at all: %s", buf.String())
	}
}

// TestDescribeWorstIndexPicksByCostPerEntry pins which index the line names. It is deliberately not
// the largest: the largest index on a large store may be the honest cost of holding it, while an
// entry costing a kilobyte over a key of an id and a few numbers is air at any size.
func TestDescribeWorstIndexPicksByCostPerEntry(t *testing.T) {
	line := describeWorstIndex(db.StorageFootprint{
		Measured: true,
		Tables: []db.TableFootprint{{
			Table: "memories",
			Indexes: []db.IndexFootprint{
				{Table: "memories", Index: "big_but_honest", Bytes: 900_000_000, Entries: 20_000_000},
				{Table: "memories", Index: "small_and_mostly_air", Bytes: 3_500_000, Entries: 1_011},
			},
		}},
	})

	if !strings.Contains(line, "small_and_mostly_air") {
		t.Errorf("the line named the largest index rather than the costliest per entry: %q", line)
	}

	// Nothing to say must read as a complete sentence without it, rather than as a dangling clause.
	if got := describeWorstIndex(db.StorageFootprint{Measured: true}); got != "" {
		t.Errorf("a footprint with no indexes produced %q", got)
	}

	unanalysed := describeWorstIndex(db.StorageFootprint{
		Measured: true,
		Tables:   []db.TableFootprint{{Table: "memories", Indexes: []db.IndexFootprint{{Index: "never_analysed", Bytes: 8192}}}},
	})

	if unanalysed != "" {
		t.Errorf("an index the catalogue has not analysed was described anyway: %q", unanalysed)
	}
}

// TestFootprintToProtoCarriesEveryIndex is the drift guard on the projection: a field added to the
// storage struct and forgotten here is invisible on the wire, and a console renders whatever it is
// given without noticing that a figure never arrived.
func TestFootprintToProtoCarriesEveryIndex(t *testing.T) {
	out := footprintToProto(&footprintSnapshot{
		measuredAt: time.Unix(0, 12345),
		estimated:  100,
		footprint: db.StorageFootprint{
			Measured: true,
			Bytes:    900,
			Tables: []db.TableFootprint{
				{Table: "memories", Bytes: 700, IndexBytes: 400, Indexes: []db.IndexFootprint{
					{Table: "memories", Index: "memories_pkey", Bytes: 400, Entries: 10},
				}},
				{Table: "events", Bytes: 200, IndexBytes: 100, Indexes: []db.IndexFootprint{
					{Table: "events", Index: "events_pkey", Bytes: 100, Entries: 5},
				}},
			},
		},
	})

	for name, one := range map[string]struct{ got, want int64 }{
		"measured_at":     {out.GetMeasuredAt(), 12345},
		"bytes":           {out.GetBytes(), 900},
		"index_bytes":     {out.GetIndexBytes(), 500},
		"estimated_bytes": {out.GetEstimatedBytes(), 100},
	} {
		if one.got != one.want {
			t.Errorf("%s = %d, want %d", name, one.got, one.want)
		}
	}

	if len(out.GetTables()) != 2 {
		t.Fatalf("tables = %d, want 2", len(out.GetTables()))
	}

	for _, table := range out.GetTables() {
		if len(table.GetIndexes()) != 1 {
			t.Fatalf("%s: indexes = %d, want 1", table.GetTable(), len(table.GetIndexes()))
		}

		index := table.GetIndexes()[0]

		// The index's own table travels with it, so a flattened series is attributable without the
		// reader having to remember which table it was nested under.
		if index.GetTable() != table.GetTable() {
			t.Errorf("index %s reports table %q under %q", index.GetIndex(), index.GetTable(), table.GetTable())
		}

		if index.GetBytes() != table.GetIndexBytes() {
			t.Errorf("%s: the index bytes %d do not add up to the table's %d", table.GetTable(), index.GetBytes(), table.GetIndexBytes())
		}

		if index.GetEntries() == 0 {
			t.Errorf("%s: entries did not survive the projection", table.GetTable())
		}
	}
}

// TestSleepReportsTheFootprintAgainstTheCycleEstimate pins where the estimate comes from. Taking
// UsedBytes again for a figure nothing acts on would be a second full scan per cycle on the server
// drivers, which is the cost this whole reading is designed around.
func TestSleepReportsTheFootprintAgainstTheCycleEstimate(t *testing.T) {
	s := statusServer(t)
	s.db = measuredFootprintStore{Store: s.db, footprint: bloatedFootprint()}
	s.consolidation.capacityBytes = 160_000_000

	seedDoomed(t, s, 2)

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	snapshot := s.lastFootprint.Load()

	if snapshot == nil {
		t.Fatal("a cycle ran without taking a footprint")
	}

	if snapshot.estimated != s.consolidation.lastUsedBytes {
		t.Errorf("the footprint was compared against %d, want the cycle's own reading %d",
			snapshot.estimated, s.consolidation.lastUsedBytes)
	}

	if snapshot.estimated <= 0 {
		t.Error("the cycle reported no estimate to read the footprint against")
	}
}
