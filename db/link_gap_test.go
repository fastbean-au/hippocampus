package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// --- the event-graph wrappers, against a real store. They are one-liners over the shared
// implementation, but they are also the only thing pinning each to the right graph: a wrapper
// pointed at memoryGraph would still compile and still pass every memory test. ---

// seedLinkedEvents creates n events, ready to be linked.
func seedLinkedEvents(t *testing.T, d *DB, n int) {
	t.Helper()

	for i := range n {
		event := types.Event{Id: eventId(i), Name: eventId(i), TimeStart: 100, Significance: 5}

		if _, err := d.CreateEvent(context.Background(), event); err != nil {
			t.Fatalf("CreateEvent(%s): %v", eventId(i), err)
		}
	}
}

func eventId(i int) string {
	return "e" + string(rune('0'+i))
}

// TestUnlinkEventsIsSymmetric verifies the event graph unlinks in either direction, as the memory
// graph does: value is symmetric, so a caller asking to unlink A from B does not mean "only if I
// was the one who declared it".
func TestUnlinkEventsIsSymmetric(t *testing.T) {
	d := newTestDB(t)
	seedLinkedEvents(t, d, 3)

	links := []types.Link{
		{Id: eventId(1), Significance: 10},
		{Id: eventId(2), Significance: 20},
	}

	if err := d.LinkEvents(context.Background(), eventId(0), links); err != nil {
		t.Fatalf("LinkEvents: %v", err)
	}

	// Unlink naming the far end first, which is the direction the edge was NOT declared in.
	if err := d.UnlinkEvents(context.Background(), eventId(1), []string{eventId(0)}); err != nil {
		t.Fatalf("UnlinkEvents: %v", err)
	}

	edges, total, err := d.GetEventLinks(context.Background(), eventId(0), types.LinkDirectionBoth)
	if err != nil {
		t.Fatalf("GetEventLinks: %v", err)
	}

	if len(edges) != 1 || edges[0].Id != eventId(2) {
		t.Fatalf("expected only the link to %s to survive, got %+v", eventId(2), edges)
	}

	if total != 20 {
		t.Errorf("expected the aggregate to drop to 20, got %d", total)
	}

	// The far end's own aggregate must have been recalculated too.
	_, farTotal, err := d.GetEventLinks(context.Background(), eventId(1), types.LinkDirectionBoth)
	if err != nil {
		t.Fatalf("GetEventLinks: %v", err)
	}

	if farTotal != 0 {
		t.Errorf("expected the unlinked event's aggregate to fall to 0, got %d", farTotal)
	}
}

// TestMissingEventIds verifies the event half of the existence check the link RPCs run.
func TestMissingEventIds(t *testing.T) {
	d := newTestDB(t)
	seedLinkedEvents(t, d, 2)

	missing, err := d.MissingEventIds(context.Background(), []string{eventId(0), "ghost", eventId(1), "phantom"})
	if err != nil {
		t.Fatalf("MissingEventIds: %v", err)
	}

	if len(missing) != 2 || missing[0] != "ghost" || missing[1] != "phantom" {
		t.Errorf("expected exactly the unknown ids, in request order, got %+v", missing)
	}
}

// TestLinksForEventsIsOutboundOnly pins the property that makes an archive round trip faithful:
// returning both directions would put every edge in the archive twice, and an import would then
// write two directed rows where one existed and double the aggregate.
func TestLinksForEventsIsOutboundOnly(t *testing.T) {
	d := newTestDB(t)
	seedLinkedEvents(t, d, 2)

	if err := d.LinkEvents(context.Background(), eventId(0), []types.Link{{Id: eventId(1), Significance: 7}}); err != nil {
		t.Fatalf("LinkEvents: %v", err)
	}

	links, err := d.LinksForEvents(context.Background(), []string{eventId(0), eventId(1)})
	if err != nil {
		t.Fatalf("LinksForEvents: %v", err)
	}

	if len(links[eventId(0)]) != 1 || links[eventId(0)][0].Id != eventId(1) {
		t.Errorf("expected the declaring event to carry the edge, got %+v", links[eventId(0)])
	}

	if len(links[eventId(1)]) != 0 {
		t.Errorf("expected the far end to carry nothing outbound, got %+v", links[eventId(1)])
	}
}

// TestLinkHelpers_EmptyInputsAreNoOps covers the early returns every batched link helper opens
// with, which must not issue a statement at all - a mocked handle with no expectations set fails
// loudly if one does.
func TestLinkHelpers_EmptyInputsAreNoOps(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	missing, err := d.MissingIds(context.Background(), memoryGraph, nil)
	if err != nil || missing != nil {
		t.Errorf("MissingIds(nil) = %v, %v; want nil, nil", missing, err)
	}

	linked, err := d.LinkedMemoryIds(context.Background(), nil)
	if err != nil || linked != nil {
		t.Errorf("LinkedMemoryIds(nil) = %v, %v; want nil, nil", linked, err)
	}

	links, err := d.LinksForMemories(context.Background(), nil)
	if err != nil || links != nil {
		t.Errorf("LinksForMemories(nil) = %v, %v; want nil, nil", links, err)
	}

	written, dropped, err := d.ImportMemoryLinks(context.Background(), nil)
	if err != nil || written != 0 || dropped != 0 {
		t.Errorf("ImportMemoryLinks(nil) = %d, %d, %v; want 0, 0, nil", written, dropped, err)
	}

	if err := d.ReinforceLinkedMemories(context.Background(), nil, 0.5); err != nil {
		t.Errorf("ReinforceLinkedMemories(nil) = %v; want nil", err)
	}

	// A non-positive fraction is the disabled setting, and must be just as inert as an empty set.
	if err := d.ReinforceLinkedMemories(context.Background(), []string{"m1"}, 0); err != nil {
		t.Errorf("ReinforceLinkedMemories(fraction 0) = %v; want nil", err)
	}

	expectationsMet(t, mock)
}

// TestRecalculateLinkSignificance_EmptyIsANoOp covers the early return on the one function every
// link mutation funnels through.
func TestRecalculateLinkSignificance_EmptyIsANoOp(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.recalculateLinkSignificance(tx, memoryGraph, nil); err != nil {
		t.Errorf("expected an empty id set to be a no-op, got %v", err)
	}

	_ = tx.Rollback()
}

// --- initLinkTables and the legacy-column drop, both of which run at startup and whose failures a
// real handle cannot be made to produce on demand. ---

// TestInitLinkTables_IndexErrorPropagates covers the reverse-index creation failure. The index is
// not optional: without it the inbound half of every symmetric aggregate becomes a table scan.
func TestInitLinkTables_IndexErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memory_links`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE INDEX IF NOT EXISTS idx_memory_links_to`).WillReturnError(errors.New("boom"))

	if err := d.initLinkTables(); err == nil {
		t.Fatal("expected the index failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestInitLinkTables_MySQLIndexErrorPropagates covers the MySQL branch, which has to probe
// information_schema because it has no CREATE INDEX IF NOT EXISTS.
func TestInitLinkTables_MySQLIndexErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memory_links`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`information_schema.statistics`).WillReturnError(errors.New("boom"))

	if err := d.initLinkTables(); err == nil {
		t.Fatal("expected the MySQL index probe failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestInitLinkTables_MySQLCreatesTheMissingIndex covers the MySQL branch's success path, including
// the `continue` that skips the portable CREATE INDEX IF NOT EXISTS.
func TestInitLinkTables_MySQLCreatesTheMissingIndex(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	for _, table := range []string{"memory_links", "event_links"} {
		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS ` + table).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`information_schema.statistics`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectExec(`CREATE INDEX idx_` + table + `_to`).WillReturnResult(sqlmock.NewResult(0, 0))
	}

	if err := d.initLinkTables(); err != nil {
		t.Fatalf("initLinkTables: %v", err)
	}

	expectationsMet(t, mock)
}

// TestDropLegacyRelationshipColumns_ProbeErrorPropagates covers the column-existence probe failing.
func TestDropLegacyRelationshipColumns_ProbeErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).WillReturnError(errors.New("boom"))

	if err := d.dropLegacyRelationshipColumns(); err == nil {
		t.Fatal("expected the probe failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestDropLegacyRelationshipColumns_DropErrorPropagates covers the ALTER itself failing, on a
// database old enough to still carry the column.
func TestDropLegacyRelationshipColumns_DropErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("relationships"))
	mock.ExpectExec(`ALTER TABLE events DROP COLUMN relationships`).WillReturnError(errors.New("boom"))

	if err := d.dropLegacyRelationshipColumns(); err == nil {
		t.Fatal("expected the drop failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestDropLegacyRelationshipColumns_DropsBothWhenPresent covers the success path on an old
// database, which must drop both columns and migrate nothing out of them.
func TestDropLegacyRelationshipColumns_DropsBothWhenPresent(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	for _, column := range []string{"relationships", "relationship_significance"} {
		mock.ExpectQuery(`pragma_table_info`).
			WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow(column))
		mock.ExpectExec(`ALTER TABLE events DROP COLUMN ` + column).WillReturnResult(sqlmock.NewResult(0, 0))
	}

	if err := d.dropLegacyRelationshipColumns(); err != nil {
		t.Fatalf("dropLegacyRelationshipColumns: %v", err)
	}

	expectationsMet(t, mock)
}

// --- pruneLinks' remaining branches. ---

// TestPruneLinks_ScanFailurePropagates drives the row scan itself failing, which a NULL far end
// produces: a link row with a NULL end should never exist, and if one does the prune must not
// quietly carry on and leave the aggregate counting it.
func TestPruneLinks_ScanFailurePropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id"}).AddRow(nil, "m2"))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err == nil {
		t.Fatal("expected the scan failure to propagate")
	}

	_ = tx.Rollback()
}

// TestPruneLinks_NothingMatchedSkipsTheDelete pins the read-then-delete shortcut: the read has
// already answered the question the delete would ask, so a chunk that matched nothing must not
// issue one. The mock fails the test if a DELETE arrives.
func TestPruneLinks_NothingMatchedSkipsTheDelete(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id"}))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if err := d.pruneMemoryLinks(tx, []string{"m1"}); err != nil {
		t.Fatalf("pruneMemoryLinks: %v", err)
	}

	_ = tx.Rollback()

	expectationsMet(t, mock)
}

// TestPruneLinks_NoSurvivorsSkipsTheRecalculation covers the case where every far end is itself in
// the deletion set: their rows are about to disappear, so there is no aggregate left to recompute.
func TestPruneLinks_NoSurvivorsSkipsTheRecalculation(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	mock.ExpectQuery(`SELECT from_id, to_id FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id"}).AddRow("m1", "m2"))
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	// Both ends are being deleted, so no UPDATE ... link_significance may follow.
	if err := d.pruneMemoryLinks(tx, []string{"m1", "m2"}); err != nil {
		t.Fatalf("pruneMemoryLinks: %v", err)
	}

	_ = tx.Rollback()

	expectationsMet(t, mock)
}

// TestLinkTableEmpty_RowErrorPropagates covers the probe's row-error branch.
func TestLinkTableEmpty_RowErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM memory_links`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1).RowError(0, errors.New("boom")))

	tx, _, err := d.beginTx(context.Background())
	if err != nil {
		t.Fatalf("beginTx: %v", err)
	}

	if _, err := d.linkTableEmpty(tx, memoryGraph); err == nil {
		t.Fatal("expected the probe's row error to propagate")
	}

	_ = tx.Rollback()
}

// --- createLinks / deleteLinks transaction boundaries. ---

// TestCreateLinks_BeginErrorPropagates covers the transaction failing to open.
func TestCreateLinks_BeginErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin().WillReturnError(errors.New("boom"))

	if err := d.LinkMemories(context.Background(), "m1", []types.Link{{Id: "m2", Significance: 1}}); err == nil {
		t.Fatal("expected the begin failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestCreateLinks_CommitErrorPropagates covers the commit failing after every edge was written:
// the aggregate and the edges must stand or fall together, so a failed commit is a failed write.
func TestCreateLinks_CommitErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit().WillReturnError(errors.New("boom"))

	if err := d.LinkMemories(context.Background(), "m1", []types.Link{{Id: "m2", Significance: 1}}); err == nil {
		t.Fatal("expected the commit failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestDeleteLinks_BeginErrorPropagates covers the unlink transaction failing to open.
func TestDeleteLinks_BeginErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin().WillReturnError(errors.New("boom"))

	if err := d.UnlinkMemories(context.Background(), "m1", []string{"m2"}); err == nil {
		t.Fatal("expected the begin failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestDeleteLinks_RecalculateErrorRollsBack covers the aggregate recomputation failing after the
// edges were deleted, which must roll the deletion back rather than leave a stale aggregate.
func TestDeleteLinks_RecalculateErrorRollsBack(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.UnlinkMemories(context.Background(), "m1", []string{"m2"}); err == nil {
		t.Fatal("expected the recalculation failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestDeleteLinks_CommitErrorPropagates covers the unlink's commit failing.
func TestDeleteLinks_CommitErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit().WillReturnError(errors.New("boom"))

	if err := d.UnlinkMemories(context.Background(), "m1", []string{"m2"}); err == nil {
		t.Fatal("expected the commit failure to propagate")
	}

	expectationsMet(t, mock)
}

// --- getLinks and its aggregate lookup. ---

// TestGetLinks_QueryErrorPropagates covers the read failing outright.
func TestGetLinks_QueryErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT to_id, significance, created`).WillReturnError(errors.New("boom"))

	if _, _, err := d.GetMemoryLinks(context.Background(), "m1", types.LinkDirectionBoth); err == nil {
		t.Fatal("expected the query failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestGetLinks_ScanErrorPropagates covers a row that cannot be scanned.
func TestGetLinks_ScanErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT to_id, significance, created`).
		WillReturnRows(sqlmock.NewRows([]string{"to_id", "significance", "created", "outbound"}).
			AddRow(nil, 1, 100, true))

	if _, _, err := d.GetMemoryLinks(context.Background(), "m1", types.LinkDirectionBoth); err == nil {
		t.Fatal("expected the scan failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestGetLinks_RowErrorPropagates covers the result set failing partway through.
func TestGetLinks_RowErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT to_id, significance, created`).
		WillReturnRows(sqlmock.NewRows([]string{"to_id", "significance", "created", "outbound"}).
			AddRow("m2", 1, 100, true).RowError(1, errors.New("boom")).
			AddRow("m3", 1, 100, true))

	if _, _, err := d.GetMemoryLinks(context.Background(), "m1", types.LinkDirectionBoth); err == nil {
		t.Fatal("expected the row error to propagate")
	}

	expectationsMet(t, mock)
}

// TestGetLinks_NarrowedDirectionReadsTheStoredAggregate pins the reason a narrowed read makes a
// second query: it saw only part of the graph, so summing the rows it read would under-report the
// figure the decay maths damps.
func TestGetLinks_NarrowedDirectionReadsTheStoredAggregate(t *testing.T) {
	d := newTestDB(t)
	seedLinkedMemories(t, d, 3)

	links := []types.Link{
		{Id: memoryId(2), Significance: 10},
		{Id: memoryId(3), Significance: 20},
	}

	if err := d.LinkMemories(context.Background(), memoryId(1), links); err != nil {
		t.Fatalf("LinkMemories: %v", err)
	}

	// memoryId(2) declared nothing, so its outbound read sees no rows at all - but it still carries
	// the significance of the edge pointing at it.
	edges, total, err := d.GetMemoryLinks(context.Background(), memoryId(2), types.LinkDirectionOutbound)
	if err != nil {
		t.Fatalf("GetMemoryLinks: %v", err)
	}

	if len(edges) != 0 {
		t.Errorf("expected no outbound edges, got %+v", edges)
	}

	if total != 10 {
		t.Errorf("expected the stored aggregate of 10, got %d", total)
	}
}

// TestGetLinks_AggregateErrorPropagates covers the narrowed read's second query failing.
func TestGetLinks_AggregateErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT to_id, significance, created`).
		WillReturnRows(sqlmock.NewRows([]string{"to_id", "significance", "created", "outbound"}).
			AddRow("m2", 1, 100, true))
	mock.ExpectQuery(`SELECT link_significance FROM memories`).WillReturnError(errors.New("boom"))

	if _, _, err := d.GetMemoryLinks(context.Background(), "m1", types.LinkDirectionOutbound); err == nil {
		t.Fatal("expected the aggregate read's failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestLinkSignificanceOf_MissingRowReportsZero pins the deliberate choice not to error: callers
// have already established the item exists, so a missing row is a zero rather than a failure.
func TestLinkSignificanceOf_MissingRowReportsZero(t *testing.T) {
	d := newTestDB(t)

	total, err := d.linkSignificanceOf(context.Background(), memoryGraph, "never-existed")
	if err != nil {
		t.Fatalf("expected a missing row to report zero, got %v", err)
	}

	if total != 0 {
		t.Errorf("expected 0, got %d", total)
	}
}

// --- MissingIds, linkedIds and linksFor: the three batched readers, each with the same three
// failure points. ---

// TestMissingIds_Failures walks the query, scan and row-error branches.
func TestMissingIds_Failures(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).WillReturnError(errors.New("boom"))

		if _, err := d.MissingMemoryIds(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the query failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nil))

		if _, err := d.MissingMemoryIds(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).
				AddRow("m1").RowError(1, errors.New("boom")).AddRow("m2"))

		if _, err := d.MissingMemoryIds(context.Background(), []string{"m1", "m2"}); err == nil {
			t.Fatal("expected the row error to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestLinkedIds_Failures walks the same three branches on the one-hop neighbour lookup.
func TestLinkedIds_Failures(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT to_id FROM memory_links`).WillReturnError(errors.New("boom"))

		if _, err := d.LinkedMemoryIds(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the query failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT to_id FROM memory_links`).
			WillReturnRows(sqlmock.NewRows([]string{"to_id"}).AddRow(nil))

		if _, err := d.LinkedMemoryIds(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT to_id FROM memory_links`).
			WillReturnRows(sqlmock.NewRows([]string{"to_id"}).
				AddRow("m2").RowError(1, errors.New("boom")).AddRow("m3"))

		if _, err := d.LinkedMemoryIds(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the row error to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestLinksFor_Failures walks the same three branches on the outbound-link reader.
func TestLinksFor_Failures(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT from_id, to_id, significance FROM memory_links`).
			WillReturnError(errors.New("boom"))

		if _, err := d.LinksForMemories(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the query failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT from_id, to_id, significance FROM memory_links`).
			WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id", "significance"}).
				AddRow(nil, "m2", 1))

		if _, err := d.LinksForMemories(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the scan failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("row error", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT from_id, to_id, significance FROM memory_links`).
			WillReturnRows(sqlmock.NewRows([]string{"from_id", "to_id", "significance"}).
				AddRow("m1", "m2", 1).RowError(1, errors.New("boom")).AddRow("m1", "m3", 1))

		if _, err := d.LinksForMemories(context.Background(), []string{"m1"}); err == nil {
			t.Fatal("expected the row error to propagate")
		}

		expectationsMet(t, mock)
	})
}

// TestReinforceLinkedMemories_ExecErrorPropagates covers the update failing.
func TestReinforceLinkedMemories_ExecErrorPropagates(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnError(errors.New("boom"))

	if err := d.ReinforceLinkedMemories(context.Background(), []string{"m1"}, 0.5); err == nil {
		t.Fatal("expected the update failure to propagate")
	}

	expectationsMet(t, mock)
}

// TestReinforceLinkedMemories_FractionIsClamped pins the upper bound: a fraction above 1 would
// advance a clock past now, which is not a reinforcement but a lie about when the memory was last
// touched.
func TestReinforceLinkedMemories_FractionIsClamped(t *testing.T) {
	d := newTestDB(t)
	seedLinkedMemories(t, d, 1)

	before := time.Now().UnixNano()

	if err := d.ReinforceLinkedMemories(context.Background(), []string{memoryId(1)}, 5); err != nil {
		t.Fatalf("ReinforceLinkedMemories: %v", err)
	}

	memories, err := d.GetMemoriesByIds(context.Background(), []string{memoryId(1)})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %v", err)
	}

	recalled := (*memories)[0].TimeRecalled

	// A fraction of 1 lands on "now". The comparison carries a tolerance because the SQL side
	// computes the advance in floating point, and a nanosecond epoch needs more mantissa than a
	// float64 has - so the landing point is accurate to a few hundred nanoseconds, not exact.
	const tolerance = int64(time.Millisecond)

	if recalled < before-tolerance {
		t.Errorf("expected a fraction above 1 to be clamped to now, got %d (now was %d)", recalled, before)
	}

	if recalled > time.Now().UnixNano()+tolerance {
		t.Errorf("expected the clamp to stop the clock at now, got %d", recalled)
	}
}

// --- importLinks. ---

// TestImportLinks_DropsSelfLinks pins the deliberate parallel with a missing far end: an import
// replays what an archive holds, and refusing the whole batch over one edge that validation would
// never have accepted is the wrong trade.
func TestImportLinks_DropsSelfLinks(t *testing.T) {
	d := newTestDB(t)
	seedLinkedMemories(t, d, 2)

	links := map[string][]types.Link{
		memoryId(1): {
			{Id: memoryId(1), Significance: 5},
			{Id: memoryId(2), Significance: 7},
		},
	}

	written, dropped, err := d.ImportMemoryLinks(context.Background(), links)
	if err != nil {
		t.Fatalf("ImportMemoryLinks: %v", err)
	}

	if written != 1 || dropped != 1 {
		t.Errorf("expected 1 written and the self-link dropped, got %d written, %d dropped", written, dropped)
	}
}

// TestImportLinks_DropsAnEntireSetWhoseOwnerIsAbsent covers the owner-missing branch, which drops
// the whole set in one go rather than per edge.
func TestImportLinks_DropsAnEntireSetWhoseOwnerIsAbsent(t *testing.T) {
	d := newTestDB(t)
	seedLinkedMemories(t, d, 2)

	links := map[string][]types.Link{
		"absent": {
			{Id: memoryId(1), Significance: 5},
			{Id: memoryId(2), Significance: 7},
		},
	}

	written, dropped, err := d.ImportMemoryLinks(context.Background(), links)
	if err != nil {
		t.Fatalf("ImportMemoryLinks: %v", err)
	}

	if written != 0 || dropped != 2 {
		t.Errorf("expected the whole set dropped, got %d written, %d dropped", written, dropped)
	}
}

// TestImportLinks_Failures walks the transaction boundaries an import crosses.
func TestImportLinks_Failures(t *testing.T) {
	links := map[string][]types.Link{"m1": {{Id: "m2", Significance: 1}}}

	t.Run("existence check", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).WillReturnError(errors.New("boom"))

		if _, _, err := d.ImportMemoryLinks(context.Background(), links); err == nil {
			t.Fatal("expected the existence check's failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("begin", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1").AddRow("m2"))
		mock.ExpectBegin().WillReturnError(errors.New("boom"))

		if _, _, err := d.ImportMemoryLinks(context.Background(), links); err == nil {
			t.Fatal("expected the begin failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("upsert", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1").AddRow("m2"))
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO memory_links`).WillReturnError(errors.New("boom"))
		mock.ExpectRollback()

		if _, _, err := d.ImportMemoryLinks(context.Background(), links); err == nil {
			t.Fatal("expected the upsert failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("recalculate", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1").AddRow("m2"))
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnError(errors.New("boom"))
		mock.ExpectRollback()

		if _, _, err := d.ImportMemoryLinks(context.Background(), links); err == nil {
			t.Fatal("expected the recalculation failure to propagate")
		}

		expectationsMet(t, mock)
	})

	t.Run("commit", func(t *testing.T) {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectQuery(`SELECT id FROM memories`).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1").AddRow("m2"))
		mock.ExpectBegin()
		mock.ExpectExec(`INSERT INTO memory_links`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE memories SET link_significance`).WillReturnResult(sqlmock.NewResult(0, 2))
		mock.ExpectCommit().WillReturnError(errors.New("boom"))

		if _, _, err := d.ImportMemoryLinks(context.Background(), links); err == nil {
			t.Fatal("expected the commit failure to propagate")
		}

		expectationsMet(t, mock)
	})
}
