package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// TestRunBackupCopiesTheStore drives the --backup mode against a SQLite store that is open for
// writing at the time, as a live instance's would be (TODO-3 item 158).
func TestRunBackupCopiesTheStore(t *testing.T) {
	source := t.TempDir()

	live, err := db.New(source)
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = live.Close() })

	if _, err := live.CreateMemory(context.Background(), types.Memory{Id: "m1", Body: "kept", TimeStamp: time.Now().UnixNano(), Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	restored := t.TempDir()

	if err := runBackup(backupConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: source,
		Destination:      filepath.Join(restored, "hippocampus.db"),
	}); err != nil {
		t.Fatalf("runBackup: %s", err)
	}

	copied, err := db.NewSQLiteReadOnly(restored)
	if err != nil {
		t.Fatalf("opening the backup: %s", err)
	}

	t.Cleanup(func() { _ = copied.Close() })

	if n := copied.CountEvents(context.Background()); n != 0 {
		t.Errorf("the backup holds %d events, want 0", n)
	}

	ids, err := copied.MemoryIdsMatching(context.Background(), db.MemoryFilter{})
	if err != nil || len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("the backup holds %v (%v), want [m1]", ids, err)
	}
}

// TestRunBackupRefusesWhatItCannotCopy: the server drivers are pointed at their own tools, and an
// in-memory store has no file to copy.
func TestRunBackupRefusesWhatItCannotCopy(t *testing.T) {
	cases := []struct {
		name string
		cfg  backupConfig
		want string
	}{
		{"postgres", backupConfig{StorageDriver: "postgres", Destination: "x"}, "pg_dump"},
		{"mysql", backupConfig{StorageDriver: "mysql", Destination: "x"}, "mysqldump"},
		{"in-memory", backupConfig{StorageDriver: "sqlite", Destination: "x"}, "in-memory"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := runBackup(c.cfg)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("runBackup = %v, want an error naming %q", err, c.want)
			}
		})
	}
}
