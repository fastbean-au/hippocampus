package ratelimit

import (
	"math"
	"sync"
	"time"
)

// bucket is a token bucket: it holds at most burst tokens, refills continuously at rate tokens per
// second, and a request costs one. It is hand-rolled rather than taken from golang.org/x/time/rate
// because it is forty lines and that would be a new module dependency for the sake of them - and
// because the hierarchy needs refund (below), which x/time's Reserve/Cancel pair models less
// directly than adding a token back.
//
// Every method takes the current time rather than reading the clock itself, so a test can drive a
// bucket deterministically instead of sleeping through a refill.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// newBucket returns a bucket starting full, so a caller's first burst is served immediately rather
// than having to wait out a cold start.
func newBucket(rate float64, burst float64, now time.Time) *bucket {
	return &bucket{
		rate:   rate,
		burst:  burst,
		tokens: burst,
		last:   now,
	}
}

// take attempts to spend one token. When it cannot, the duration is how long until one is
// available, which is what the caller turns into a Retry-After.
func (b *bucket) take(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill(now)

	if b.tokens >= 1 {
		b.tokens--

		return true, 0
	}

	// The clock can go backwards (a step adjustment), which refill already guards by ignoring a
	// negative elapsed time; guard the division too so a zero rate cannot produce an infinite wait.
	if b.rate <= 0 {
		return false, 0
	}

	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// refund returns a token spent by take. It exists because the limits are a hierarchy evaluated in
// order: a request the per-client bucket admitted and the per-tier bucket then denied was never
// served, so spending the client's token would charge it for a request it did not get.
func (b *bucket) refund(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.refill(now)

	b.tokens = math.Min(b.burst, b.tokens+1)
}

// refill credits the tokens accrued since the last operation, capped at the burst. A non-positive
// elapsed time credits nothing, so a clock stepping backwards stalls the bucket rather than
// draining or filling it.
func (b *bucket) refill(now time.Time) {
	elapsed := now.Sub(b.last)

	if elapsed <= 0 {
		return
	}

	b.last = now
	b.tokens = math.Min(b.burst, b.tokens+elapsed.Seconds()*b.rate)
}
