package db

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestSQLiteKeepsNoInstanceRegistry pins the whole of Decision 6's SQLite half in one place: the
// table is never created, the three methods are no-ops rather than errors, and the store says so.
//
// The table's absence is the assertion that matters. SQLite's UsedBytes is page-based, so a registry
// inside that file would be counted as live data - and the record of the deployment would then raise
// capacity pressure and evict real memories to make room for itself, which is exactly the trap the
// forgotten log had to be excluded from.
func TestSQLiteKeepsNoInstanceRegistry(t *testing.T) {
	database, err := New("")
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	if database.InstanceRegistryAvailable() {
		t.Error("SQLite must report no instance registry: it is single-instance by construction")
	}

	var name string

	err = database.sql.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, instancesTable).Scan(&name)
	if err == nil {
		t.Fatalf("the %s table must never be created on SQLite (found %q)", instancesTable, name)
	}

	// The no-ops are what let the server call these unconditionally, which is what keeps the driver
	// check out of every call site.
	if err := database.Heartbeat(context.Background(), Instance{Id: "host:50051"}); err != nil {
		t.Errorf("Heartbeat on SQLite should be a no-op, got %s", err)
	}

	instances, err := database.ListInstances(context.Background())
	if err != nil {
		t.Errorf("ListInstances on SQLite should be a no-op, got %s", err)
	}

	if len(instances) != 0 {
		t.Errorf("ListInstances on SQLite should return nothing, got %d", len(instances))
	}

	if err := database.DeregisterInstance(context.Background(), "host:50051"); err != nil {
		t.Errorf("DeregisterInstance on SQLite should be a no-op, got %s", err)
	}
}

// TestInstanceStale checks the freshness rule each row is judged by. It is judged against the row's
// OWN interval, which is the point: an instance heartbeating every five minutes beside peers on
// thirty seconds is not stale, and reading it against the reader's interval would mark it so on
// every round.
func TestInstanceStale(t *testing.T) {
	now := time.Now().UnixNano()

	tests := []struct {
		name     string
		instance Instance
		want     bool
	}{
		{
			name:     "just written",
			instance: Instance{LastSeen: now, HeartbeatSeconds: 30},
		},
		{
			name:     "one missed beat is tolerated",
			instance: Instance{LastSeen: now - int64(45*time.Second), HeartbeatSeconds: 30},
		},
		{
			name:     "beyond twice its interval",
			instance: Instance{LastSeen: now - int64(90*time.Second), HeartbeatSeconds: 30},
			want:     true,
		},
		{
			name:     "a slow instance is judged by its own interval",
			instance: Instance{LastSeen: now - int64(90*time.Second), HeartbeatSeconds: 300},
		},
		{
			name:     "no interval recorded is never stale",
			instance: Instance{LastSeen: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.instance.Stale(now); got != tt.want {
				t.Errorf("Stale() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInstanceUpsertDialects pins the dialect split. MySQL has no ON CONFLICT, and its
// ON DUPLICATE KEY UPDATE needs the 8.0.20+ row alias - the same trap linkUpsert documents.
func TestInstanceUpsertDialects(t *testing.T) {
	mysql := (&DB{driver: driverMySQL}).instanceUpsert()

	if !strings.Contains(mysql, "ON DUPLICATE KEY UPDATE") || !strings.Contains(mysql, "AS new") {
		t.Errorf("the MySQL upsert needs ON DUPLICATE KEY UPDATE with the row alias, got %q", mysql)
	}

	postgres := (&DB{driver: driverPostgres}).instanceUpsert()

	if !strings.Contains(postgres, "ON CONFLICT (id) DO UPDATE") {
		t.Errorf("the Postgres upsert needs ON CONFLICT, got %q", postgres)
	}

	// Every column is refreshed, including started_at: an instance that restarts with a different
	// configuration must not be described by the row its previous incarnation left behind.
	for _, column := range []string{"role", "started_at", "has_search", "has_gateway"} {
		if !strings.Contains(mysql, column+" = new."+column) {
			t.Errorf("the MySQL upsert does not refresh %s", column)
		}

		if !strings.Contains(postgres, column+" = excluded."+column) {
			t.Errorf("the Postgres upsert does not refresh %s", column)
		}
	}
}

// TestInstancesDDLHasNoSQLiteBranch guards the one thing that would quietly undo Decision 6: the
// table's DDL must never be reachable on SQLite. The default branch is Postgres', and a SQLite DB
// asking for it at all would mean initInstances had been wired into the wrong initialiser.
func TestInstancesDDLHasNoSQLiteBranch(t *testing.T) {
	if got := (&DB{driver: driverMySQL}).instancesDDL(); !strings.Contains(got, mysqlBinaryCollation) {
		t.Errorf("the MySQL registry id must collate byte-for-byte like every other id, got %q", got)
	}

	lite := &DB{driver: driverSQLite}

	if lite.InstanceRegistryAvailable() {
		t.Error("a SQLite DB must never report the registry as available")
	}
}

// --- the failure paths, driven against a mocked handle so they need no server ---

// newRegistryMockDB is a mocked server-driver handle with the registry marked present, standing in
// for one whose initInstances has run.
func newRegistryMockDB(t *testing.T) (*DB, sqlmock.Sqlmock) {
	t.Helper()

	d, mock := newMockDB(t, driverPostgres)
	d.instanceTable = true

	return d, mock
}

// TestInitInstancesFailures covers the two statements the initialiser issues. A registry that cannot
// be created must fail startup rather than leave instanceTable set, which would send every later
// query at a table that is not there.
func TestInitInstancesFailures(t *testing.T) {
	t.Run("the table", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS instances`).WillReturnError(errors.New("boom"))

		if err := d.initInstances(); err == nil {
			t.Fatal("expected an error")
		}

		if d.instanceTable {
			t.Error("the registry was marked available although its table was not created")
		}

		expectationsMet(t, mock)
	})

	t.Run("the index", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS instances`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`CREATE INDEX IF NOT EXISTS idx_instances`).WillReturnError(errors.New("boom"))

		if err := d.initInstances(); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("the mysql index probe", func(t *testing.T) {
		d, mock := newMockDB(t, driverMySQL)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS instances`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`information_schema.statistics`).WillReturnError(errors.New("boom"))

		if err := d.initInstances(); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

// TestHeartbeatFailures covers both statements. The prune is part of the same call because there is
// no other occasion for it - a registry pruned only by processes that have exited is not pruned at
// all - so a failure there has to be reported rather than swallowed.
func TestHeartbeatFailures(t *testing.T) {
	t.Run("the upsert", func(t *testing.T) {
		d, mock := newRegistryMockDB(t)

		mock.ExpectExec(`INSERT INTO instances`).WillReturnError(errors.New("boom"))

		if err := d.Heartbeat(context.Background(), Instance{Id: "a:1"}); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("the prune", func(t *testing.T) {
		d, mock := newRegistryMockDB(t)

		mock.ExpectExec(`INSERT INTO instances`).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`DELETE FROM instances`).WillReturnError(errors.New("boom"))

		if err := d.Heartbeat(context.Background(), Instance{Id: "a:1"}); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

// TestListInstancesFailures covers the three ways a read can fail. The scan case is the one worth
// having: a column added to the table without being added to the scan list fails exactly here, and
// silently returning a short list would look like a deployment that had shrunk.
func TestListInstancesFailures(t *testing.T) {
	columns := []string{
		"id", "hostname", "version", "role", "started_at", "last_seen", "heartbeat_seconds",
		"has_search", "has_summariser", "has_embedder", "has_gateway",
	}

	t.Run("the query", func(t *testing.T) {
		d, mock := newRegistryMockDB(t)

		mock.ExpectQuery(`FROM instances`).WillReturnError(errors.New("boom"))

		if _, err := d.ListInstances(context.Background()); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("a scan", func(t *testing.T) {
		d, mock := newRegistryMockDB(t)

		mock.ExpectQuery(`FROM instances`).WillReturnRows(
			sqlmock.NewRows(columns).AddRow("a:1", "a", "v1", "replica", "not a number", 0, 30, false, false, false, false),
		)

		if _, err := d.ListInstances(context.Background()); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("the row iteration", func(t *testing.T) {
		d, mock := newRegistryMockDB(t)

		mock.ExpectQuery(`FROM instances`).WillReturnRows(
			sqlmock.NewRows(columns).
				AddRow("a:1", "a", "v1", "replica", 0, 0, 30, false, false, false, false).
				RowError(0, errors.New("boom")),
		)

		if _, err := d.ListInstances(context.Background()); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

func TestDeregisterInstanceFailure(t *testing.T) {
	d, mock := newRegistryMockDB(t)

	mock.ExpectExec(`DELETE FROM instances WHERE id`).WillReturnError(errors.New("boom"))

	if err := d.DeregisterInstance(context.Background(), "a:1"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- the server-driver integration tests (skipped without a disposable database) ---

// TestPostgresInstanceRegistry exercises the registry end to end against a real Postgres: an upsert
// is idempotent under a deterministic id, a peer is visible to a reader, a stale row is pruned, and
// deregistration is immediate.
func TestPostgresInstanceRegistry(t *testing.T) {
	if os.Getenv(postgresTestDSNEnv) == "" {
		t.Skipf("set %s to run postgres integration tests", postgresTestDSNEnv)
	}

	runInstanceRegistryTest(t, newPostgresTestDB(t))
}

// TestMySQLInstanceRegistry is the same against MySQL, which shares none of the upsert syntax.
func TestMySQLInstanceRegistry(t *testing.T) {
	if os.Getenv(mysqlTestDSNEnv) == "" {
		t.Skipf("set %s to run mysql integration tests", mysqlTestDSNEnv)
	}

	runInstanceRegistryTest(t, newMySQLTestDB(t))
}

// runInstanceRegistryTest is the shared body of the two integration tests above. It is one function
// rather than two because the registry's logic is entirely shared - only the upsert's syntax
// differs - and the value of running it twice is proving exactly that.
func runInstanceRegistryTest(t *testing.T, database *DB) {
	t.Helper()

	ctx := context.Background()

	if !database.InstanceRegistryAvailable() {
		t.Fatal("a server driver must keep the instance registry")
	}

	// Purge deliberately leaves the registry alone (it is infrastructure, not data, and purging it
	// would delete live peers' rows), so the table is cleared here instead.
	if _, err := database.sql.Exec(`DELETE FROM ` + instancesTable); err != nil {
		t.Fatalf("clearing the registry: %s", err)
	}

	now := time.Now()

	self := Instance{
		Id:               "hippo-1:50051",
		Hostname:         "hippo-1",
		Version:          "v1.2.3",
		Role:             InstanceRoleConsolidator,
		StartedAt:        now.Add(-time.Hour).UnixNano(),
		LastSeen:         now.UnixNano(),
		HeartbeatSeconds: 30,
		Search:           true,
		Gateway:          true,
	}

	if err := database.Heartbeat(ctx, self); err != nil {
		t.Fatalf("Heartbeat: %s", err)
	}

	// The id is deterministic, so a restart replaces its own row rather than adding a second. This
	// is what a rolling deployment depends on: a random id would leave a ghost per restart.
	self.Version = "v1.2.4"
	self.LastSeen = now.Add(time.Second).UnixNano()

	if err := database.Heartbeat(ctx, self); err != nil {
		t.Fatalf("Heartbeat (restart): %s", err)
	}

	peer := Instance{
		Id:               "hippo-2:50051",
		Hostname:         "hippo-2",
		Version:          "v1.2.3",
		Role:             InstanceRoleReplica,
		StartedAt:        now.Add(-time.Hour).UnixNano(),
		LastSeen:         now.UnixNano(),
		HeartbeatSeconds: 30,
	}

	if err := database.Heartbeat(ctx, peer); err != nil {
		t.Fatalf("Heartbeat (peer): %s", err)
	}

	instances, err := database.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances: %s", err)
	}

	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d: %+v", len(instances), instances)
	}

	// Ordered by id, so a view built from this does not reshuffle between polls.
	if instances[0].Id != "hippo-1:50051" || instances[1].Id != "hippo-2:50051" {
		t.Errorf("expected the rows ordered by id, got %q then %q", instances[0].Id, instances[1].Id)
	}

	if instances[0].Version != "v1.2.4" {
		t.Errorf("the upsert must refresh every column, got version %q", instances[0].Version)
	}

	if instances[0].Role != InstanceRoleConsolidator || !instances[0].Search || !instances[0].Gateway || instances[0].Summariser {
		t.Errorf("the capability flags did not round-trip: %+v", instances[0])
	}

	if instances[1].Stale(now.UnixNano()) {
		t.Error("a peer that has just written must not read as stale")
	}

	// A row well past four of its own intervals is pruned by the next heartbeat. Written directly
	// rather than by waiting, since the alternative is a two-minute test.
	dead := peer
	dead.Id = "hippo-3:50051"
	dead.Hostname = "hippo-3"
	dead.LastSeen = now.Add(-10 * time.Minute).UnixNano()

	if err := database.Heartbeat(ctx, dead); err != nil {
		t.Fatalf("Heartbeat (dead): %s", err)
	}

	self.LastSeen = time.Now().UnixNano()

	if err := database.Heartbeat(ctx, self); err != nil {
		t.Fatalf("Heartbeat (pruning round): %s", err)
	}

	instances, err = database.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances after pruning: %s", err)
	}

	for _, instance := range instances {
		if instance.Id == "hippo-3:50051" {
			t.Error("a row past four of its own intervals should have been pruned")
		}
	}

	if len(instances) != 2 {
		t.Errorf("pruning removed a live row: %d remain, %+v", len(instances), instances)
	}

	// A clean shutdown leaves immediately rather than lingering as unreachable for four intervals -
	// the staleness window is for the instances that could not say they were going.
	if err := database.DeregisterInstance(ctx, peer.Id); err != nil {
		t.Fatalf("DeregisterInstance: %s", err)
	}

	instances, err = database.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances after deregistering: %s", err)
	}

	if len(instances) != 1 || instances[0].Id != self.Id {
		t.Errorf("expected only this instance to remain, got %+v", instances)
	}
}
