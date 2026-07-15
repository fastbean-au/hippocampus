package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/db"
)

// budget watches the size of the database on disk and pauses generation when it reaches the
// configured limit. Generation resumes once the sleep cycle has consolidated the store back
// below 90% of the limit, so the generator oscillates around the cap instead of flapping.
type budget struct {
	maxBytes   int64
	dbBytes    atomic.Int64
	pausedFlag atomic.Bool
}

func newBudget(maxBytes int64) *budget {
	return &budget{maxBytes: maxBytes}
}

func (b *budget) paused() bool {
	return b.pausedFlag.Load()
}

func (b *budget) databaseBytes() int64 {
	return b.dbBytes.Load()
}

func (b *budget) watch(ctx context.Context, directory string) {
	log.Trace("watch()")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			return

		case <-ticker.C:
			b.check(directory)

		}
	}
}

func (b *budget) check(directory string) {
	log.Trace("check()")

	size := databaseSize(directory)
	b.dbBytes.Store(size)

	switch {

	case size >= b.maxBytes && !b.pausedFlag.Load():
		b.pausedFlag.Store(true)
		log.Warnf(
			"database is %s (limit %s) - pausing generation",
			humanize.Bytes(uint64(size)),
			humanize.Bytes(uint64(b.maxBytes)),
		)

	case size < b.maxBytes/10*9 && b.pausedFlag.Load():
		b.pausedFlag.Store(false)
		log.Infof("database is %s - resuming generation", humanize.Bytes(uint64(size)))

	}
}

// databaseSize sums the main database file plus SQLite's WAL and shared-memory sidecars, since
// under load the WAL can hold a significant share of the data between checkpoints.
func databaseSize(directory string) int64 {
	var total int64

	for _, name := range []string{db.DataFile, db.DataFile + "-wal", db.DataFile + "-shm"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil {
			continue
		}

		total += info.Size()
	}

	return total
}
