package main

import (
	"context"
	"testing"
	"time"
)

func TestByteLimiterDisabled(t *testing.T) {
	l := newByteLimiter(0)

	if l.enabled() {
		t.Error("newByteLimiter(0) should be disabled")
	}

	if !l.wait(context.Background(), 1_000_000) {
		t.Error("wait() on a disabled limiter should always return true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if l.wait(ctx, 1) {
		t.Error("wait() on a disabled limiter should still honour an already-cancelled context")
	}
}

func TestByteLimiterEnabledBurstFloor(t *testing.T) {
	// A tiny rate still gets at least minByteLimiterBurst of burst capacity.
	l := newByteLimiter(10)

	if !l.enabled() {
		t.Error("newByteLimiter(10) should be enabled")
	}

	if l.burst != minByteLimiterBurst {
		t.Errorf("burst = %v, want floor %v", l.burst, minByteLimiterBurst)
	}
}

func TestByteLimiterWaitConsumesTokens(t *testing.T) {
	l := newByteLimiter(1_000_000)

	if !l.wait(context.Background(), 100) {
		t.Error("wait() for an available amount should succeed immediately")
	}
}

func TestByteLimiterWaitClampsToBurst(t *testing.T) {
	l := newByteLimiter(1000) // burst floored to minByteLimiterBurst

	// Request far more than the burst; should be clamped and still succeed since tokens start full.
	if !l.wait(context.Background(), minByteLimiterBurst*10) {
		t.Error("wait() for more than burst should still succeed (clamped)")
	}
}

func TestByteLimiterWaitBlocksThenSucceeds(t *testing.T) {
	// A small rate/burst forces wait() to actually sleep and refill before granting a second request.
	l := &byteLimiter{ratePerSec: 1000, burst: 100, tokens: 50, last: time.Now()}

	start := time.Now()
	if !l.wait(context.Background(), 100) {
		t.Error("wait() should eventually succeed once tokens refill")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("wait() took %s, expected well under 2s", elapsed)
	}
}

func TestByteLimiterWaitClampsMinimumSleep(t *testing.T) {
	// A high rate makes the computed refill wait sub-millisecond, exercising the floor that clamps
	// it up to time.Millisecond so the retry loop never busy-spins.
	l := &byteLimiter{ratePerSec: 100_000, burst: 10, tokens: 5, last: time.Now()}

	start := time.Now()
	if !l.wait(context.Background(), 6) {
		t.Error("wait() should succeed once the sub-millisecond refill completes")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait() took %s, expected well under 1s", elapsed)
	}
}

func TestByteLimiterWaitCancelled(t *testing.T) {
	l := &byteLimiter{ratePerSec: 1, burst: 10, tokens: 0, last: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if l.wait(ctx, 1000) {
		t.Error("wait() should return false once the context is cancelled while blocked")
	}
}
