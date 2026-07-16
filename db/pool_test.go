package db

import "testing"

// TestSetPoolLimits_AppliesMaxOpenConns verifies SetPoolLimits reaches the underlying pool: after
// setting a cap, sql.DB.Stats reports it. maxIdleConns has no Stats getter, so the idle default
// (=maxOpenConns when non-positive) is exercised only for not panicking.
func TestSetPoolLimits_AppliesMaxOpenConns(t *testing.T) {
	d := newTestDB(t)

	d.SetPoolLimits(25, 0)

	if got := d.sql.Stats().MaxOpenConnections; got != 25 {
		t.Errorf("expected MaxOpenConnections 25, got %d", got)
	}
}

// TestSetPoolLimits_NonPositiveIsNoOp verifies a non-positive maxOpenConns leaves the pool
// untouched, so a misconfiguration cannot accidentally uncap or zero the pool. newTestDB is SQLite,
// which caps itself at one connection in New.
func TestSetPoolLimits_NonPositiveIsNoOp(t *testing.T) {
	d := newTestDB(t)

	d.SetPoolLimits(0, 10)
	d.SetPoolLimits(-1, 10)

	if got := d.sql.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("expected the SQLite one-connection cap to be untouched, got MaxOpenConnections %d", got)
	}
}
