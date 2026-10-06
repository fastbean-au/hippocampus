package db

import "testing"

// TestStandbyIsRefusedOnSQLite: SQLite is single-instance by construction, so there is no lock for a
// standby to wait on, and claiming one would report a consolidator that is not there.
func TestStandbyIsRefusedOnSQLite(t *testing.T) {
	requireSQLite(t)

	d := newTestDB(t)

	if won, err := d.TryAcquireInstanceLock(); err == nil || won {
		t.Errorf("TryAcquireInstanceLock on SQLite = %v, %v; want a refusal", won, err)
	}
}
