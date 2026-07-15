package main

import (
	"context"
	"encoding/base64"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
)

var words = []string{
	"amber", "beacon", "cellar", "dawn", "ember", "fable", "garden", "harbour",
	"island", "jasper", "kestrel", "lantern", "meadow", "nectar", "orchard", "pebble",
	"quarry", "river", "saffron", "thicket", "umber", "valley", "willow", "yonder",
	"zephyr", "anchor", "bramble", "cinder", "drift", "estuary", "fern", "glade",
}

func randomName(rng *rand.Rand, kind string) string {
	return kind + "-" + words[rng.Intn(len(words))] + "-" + uuid.New().String()[:8]
}

// randomText builds a body of at least size bytes from the word list.
func randomText(rng *rand.Rand, size int) string {
	var b strings.Builder
	b.Grow(size + 16)

	for b.Len() < size {
		b.WriteString(words[rng.Intn(len(words))])
		b.WriteByte(' ')
	}

	return b.String()
}

// randomBinary returns size bytes of random data encoded as base64, since a proto string field
// must hold valid UTF-8.
func randomBinary(rng *rand.Rand, size int) string {
	raw := make([]byte, size)
	rng.Read(raw)

	return base64.StdEncoding.EncodeToString(raw)
}

// bodySize picks a size class: mostly small notes, some medium entries, and the occasional
// large blob so the store grows at a useful rate. The largest class stays well under the
// service's 1 MiB body limit even after base64 expansion.
func bodySize(rng *rand.Rand) int {
	switch p := rng.Intn(100); {

	case p < 60:
		return 200 + rng.Intn(1800)

	case p < 90:
		return 2048 + rng.Intn(14336)

	case p < 99:
		return 16384 + rng.Intn(114688)

	default:
		return 131072 + rng.Intn(393216)

	}
}

// backdatedTimestamp returns a time up to maxAge in the past, in unix nanoseconds. Backdating
// matters: consolidation measures age from the item's own timestamp, so freshly generated data
// must look old for the sleep cycle to have anything to forget.
func backdatedTimestamp(rng *rand.Rand, maxAge time.Duration) int64 {
	return time.Now().Add(-time.Duration(rng.Int63n(int64(maxAge)))).UnixNano()
}

func randomDuration(rng *rand.Rand, min time.Duration, max time.Duration) time.Duration {
	if max <= min {
		return min
	}

	return min + time.Duration(rng.Int63n(int64(max-min)))
}

// sleepFor waits for the given duration, returning false if the context is cancelled first.
func sleepFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {

	case <-ctx.Done():
		return false

	case <-timer.C:
		return true

	}
}
