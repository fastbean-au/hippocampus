package db

import (
	"context"
	"errors"
	"testing"
)

// TestContextCancellation_AbortsRead verifies the caller's own context - not just the server-side
// queryTimeout - reaches the driver: a context already cancelled by the caller makes a read fail
// with a cancellation error rather than running to completion. This is the propagation the Store
// interface's ctx parameter exists for; the queryTimeout tests cover only the server-owned bound.
func TestContextCancellation_AbortsRead(t *testing.T) {
	d := newTestDB(t)

	// No queryTimeout is configured, so any failure here is the caller's cancellation reaching the
	// driver and nothing else.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.GetMemories(ctx, MemoryFilter{})
	if err == nil {
		t.Fatal("expected a read under an already-cancelled context to fail")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %s", err)
	}
}

// TestContextCancellation_AbortsWrite verifies the same propagation on the transactional path:
// beginTx opens the transaction with the caller's context, so a cancelled caller cannot even begin
// the write.
func TestContextCancellation_AbortsWrite(t *testing.T) {
	d := newTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.DeleteMemories(ctx, []string{"some-id"})
	if err == nil {
		t.Fatal("expected a transactional write under an already-cancelled context to fail")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %s", err)
	}
}

// TestContextCancellation_LiveContextSucceeds guards against the cancellation check being
// over-eager: a normal (uncancelled) context must let operations run as before, so the propagation
// added no behaviour change on the happy path.
func TestContextCancellation_LiveContextSucceeds(t *testing.T) {
	d := newTestDB(t)

	if _, err := d.GetMemories(context.Background(), MemoryFilter{}); err != nil {
		t.Errorf("expected a read under a live context to succeed, got: %s", err)
	}
}
