package main

import (
	"context"
	"encoding/base64"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestRandomName(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	name := randomName(rng, "burst")
	if !strings.HasPrefix(name, "burst-") {
		t.Errorf("randomName() = %q, want prefix 'burst-'", name)
	}

	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		t.Errorf("randomName() = %q, want 3 dash-separated parts", name)
	}

	if len(parts[2]) != 8 {
		t.Errorf("uuid suffix = %q, want length 8", parts[2])
	}
}

func TestRandomText(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for _, size := range []int{0, 1, 50, 500} {
		text := randomText(rng, size)
		if len(text) < size {
			t.Errorf("randomText(%d) len = %d, want >= %d", size, len(text), size)
		}
	}
}

func TestRandomBinary(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	size := 32
	encoded := randomBinary(rng, size)

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("randomBinary produced invalid base64: %s", err)
	}

	if len(raw) != size {
		t.Errorf("decoded length = %d, want %d", len(raw), size)
	}
}

func TestBodySize(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	seenSmall, seenMedium, seenLarge, seenHuge := false, false, false, false

	for i := 0; i < 2000; i++ {
		size := bodySize(rng)

		switch {

		case size < 2048:
			seenSmall = true

		case size < 16384:
			seenMedium = true

		case size < 131072:
			seenLarge = true

		default:
			seenHuge = true

		}
	}

	if !seenSmall || !seenMedium || !seenLarge || !seenHuge {
		t.Errorf(
			"expected all size classes over many trials; small=%v medium=%v large=%v huge=%v",
			seenSmall, seenMedium, seenLarge, seenHuge,
		)
	}
}

func TestBackdatedTimestamp(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	now := time.Now().UnixNano()
	maxAge := 30 * time.Minute

	for i := 0; i < 100; i++ {
		ts := backdatedTimestamp(rng, maxAge)

		if ts > now {
			t.Errorf("backdatedTimestamp() = %d, want <= now (%d)", ts, now)
		}

		if ts < now-int64(maxAge) {
			t.Errorf("backdatedTimestamp() = %d, want >= now-maxAge", ts)
		}
	}
}

func TestRandomDuration(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	// max <= min returns min.
	if got := randomDuration(rng, 5*time.Second, 5*time.Second); got != 5*time.Second {
		t.Errorf("randomDuration(equal) = %s, want 5s", got)
	}

	if got := randomDuration(rng, 5*time.Second, 1*time.Second); got != 5*time.Second {
		t.Errorf("randomDuration(max<min) = %s, want 5s (min)", got)
	}

	for i := 0; i < 100; i++ {
		d := randomDuration(rng, 1*time.Second, 3*time.Second)
		if d < 1*time.Second || d >= 3*time.Second {
			t.Errorf("randomDuration() = %s, want within [1s,3s)", d)
		}
	}
}

func TestSleepForCompletes(t *testing.T) {
	if !sleepFor(context.Background(), 1*time.Millisecond) {
		t.Error("sleepFor() = false, want true when the timer fires first")
	}
}

func TestSleepForCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if sleepFor(ctx, time.Minute) {
		t.Error("sleepFor() = true, want false when context is already cancelled")
	}
}
