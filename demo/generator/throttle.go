package main

import (
	"context"
	"sync"
	"time"
)

// byteLimiter is a token bucket measured in bytes. Tokens refill at ratePerSec and callers block
// in wait() until enough are available, so it paces the aggregate write throughput of every writer
// goroutine to a target byte rate. A non-positive rate disables limiting - wait() returns
// immediately - which is the default demo behaviour. It is used by the throughput sweep to drive a
// chosen fraction of the storage cap per second.
type byteLimiter struct {
	mu         sync.Mutex
	ratePerSec float64
	burst      float64
	tokens     float64
	last       time.Time
}

// minByteLimiterBurst keeps the bucket at least one large body deep so a single big memory can
// never exceed the burst and deadlock, and so brief scheduling gaps do not starve throughput.
const minByteLimiterBurst = 4 * 1024 * 1024

func newByteLimiter(ratePerSec int64) *byteLimiter {
	if ratePerSec <= 0 {
		return &byteLimiter{}
	}

	burst := float64(ratePerSec)
	if burst < minByteLimiterBurst {
		burst = minByteLimiterBurst
	}

	return &byteLimiter{
		ratePerSec: float64(ratePerSec),
		burst:      burst,
		tokens:     burst,
		last:       time.Now(),
	}
}

func (l *byteLimiter) enabled() bool {
	return l.ratePerSec > 0
}

// wait blocks until n byte-tokens are available and consumes them, returning false only if ctx is
// cancelled first. A request larger than the burst is clamped to the burst so it can still proceed.
func (l *byteLimiter) wait(ctx context.Context, n int) bool {
	if !l.enabled() {

		return ctx.Err() == nil
	}

	need := float64(n)
	if need > l.burst {
		need = l.burst
	}

	for {
		l.mu.Lock()

		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.ratePerSec
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now

		if l.tokens >= need {
			l.tokens -= need
			l.mu.Unlock()

			return true
		}

		sleep := time.Duration((need - l.tokens) / l.ratePerSec * float64(time.Second))
		l.mu.Unlock()

		if sleep < time.Millisecond {
			sleep = time.Millisecond
		}

		select {

		case <-ctx.Done():
			return false

		case <-time.After(sleep):

		}
	}
}
