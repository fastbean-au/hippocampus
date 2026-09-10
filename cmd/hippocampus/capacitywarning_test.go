package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/fastbean-au/hippocampus/db"
)

// usedBytesStore is a db.Store stub reporting a fixed used-bytes figure, so the check below needs no
// database. Embedding db.Store means only UsedBytes has to be implemented.
type usedBytesStore struct {
	db.Store
	used  int64
	err   error
	calls int
}

func (u *usedBytesStore) UsedBytes(ctx context.Context) (int64, error) {
	u.calls++

	return u.used, u.err
}

// TestWarnIfOverCapacity covers the startup check that tells an operator their store is already past
// its byte target - which is what upgrading to 0.45.0 can do to a deployment whose capacity target
// was derived from the figure the old per-row allowance reported.
func TestWarnIfOverCapacity(t *testing.T) {
	tests := []struct {
		name     string
		capacity int64
		store    *usedBytesStore
		wantRead bool
		wantLog  string
	}{
		{
			name:     "no byte target reads nothing",
			capacity: 0,
			store:    &usedBytesStore{used: 9000000},
			wantRead: false,
		},
		{
			name:     "under the target says nothing",
			capacity: 1000000,
			store:    &usedBytesStore{used: 900000},
			wantRead: true,
		},
		{
			name:     "over the target names both figures",
			capacity: 1000000,
			store:    &usedBytesStore{used: 4200000},
			wantRead: true,
			wantLog:  "used_bytes 4200000 against consolidation.capacityBytes 1000000",
		},
		{
			name:     "an unreadable figure costs the advice, not the startup",
			capacity: 1000000,
			store:    &usedBytesStore{err: errors.New("boom")},
			wantRead: true,
			wantLog:  "could not read the store's used bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			log.SetLevel(log.WarnLevel)

			warnIfOverCapacity(test.store, test.capacity)

			if read := test.store.calls > 0; read != test.wantRead {
				t.Errorf("UsedBytes read = %t, want %t", read, test.wantRead)
			}

			entries := hook.AllEntries()

			if test.wantLog == "" {
				if len(entries) != 0 {
					t.Fatalf("expected no log line, got %q", entries[0].Message)
				}

				return
			}

			if len(entries) != 1 {
				t.Fatalf("expected one log line, got %d", len(entries))
			}

			if !strings.Contains(entries[0].Message, test.wantLog) {
				t.Errorf("log line %q does not mention %q", entries[0].Message, test.wantLog)
			}
		})
	}
}
