package db

import (
	"testing"
	"time"
)

// TestOpContext_DeadlineOnlyWhenConfigured verifies the wiring of SetQueryTimeout: with no timeout
// the operation context carries no deadline (unbounded, the previous behaviour), and with one set
// it does.
func TestOpContext_DeadlineOnlyWhenConfigured(t *testing.T) {
	d := newTestDB(t)

	ctx, cancel := d.opContext()
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no deadline when the query timeout is disabled")
	}

	d.SetQueryTimeout(time.Second)

	ctx, cancel = d.opContext()
	defer cancel()

	if _, ok := ctx.Deadline(); !ok {
		t.Error("expected a deadline once the query timeout is configured")
	}
}

// TestSetQueryTimeout_NonPositiveDisables verifies that a zero or negative timeout leaves the
// bound off, so a misconfiguration cannot accidentally make every query deadline-zero (and so fail
// immediately).
func TestSetQueryTimeout_NonPositiveDisables(t *testing.T) {
	d := newTestDB(t)
	d.SetQueryTimeout(0)
	d.SetQueryTimeout(-time.Second)

	ctx, cancel := d.opContext()
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("expected a non-positive query timeout to leave the bound disabled")
	}
}

// TestQueryTimeout_ExpiredBoundFailsRead verifies the timeout actually reaches the driver: a
// one-nanosecond bound is already expired by the time the statement runs, so a read fails rather
// than returning results. A read against an unbounded DB (the default) still succeeds.
func TestQueryTimeout_ExpiredBoundFailsRead(t *testing.T) {
	d := newTestDB(t)

	if _, err := d.GetMemories(MemoryFilter{}); err != nil {
		t.Fatalf("read against an unbounded DB should succeed, got: %s", err)
	}

	d.SetQueryTimeout(time.Nanosecond)

	if _, err := d.GetMemories(MemoryFilter{}); err == nil {
		t.Error("expected a read to fail under an already-expired query timeout")
	}

	// The transaction path is bounded too: beginTx opens with the same expired context, so a
	// delete cannot begin its transaction.
	if _, err := d.DeleteMemories([]string{"some-id"}); err == nil {
		t.Error("expected a transactional write to fail under an already-expired query timeout")
	}
}
