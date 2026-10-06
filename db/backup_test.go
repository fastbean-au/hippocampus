package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestBackupToCopiesALiveStore takes a backup through a read-only connection while the writer that
// owns the store is still open with writes it has not checkpointed - the case a plain file copy gets
// wrong - and then opens the copy as a store of its own (TODO-3 item 158).
func TestBackupToCopiesALiveStore(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()

	writer, err := New(source)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = writer.Close() })

	for _, id := range []string{"a", "b", "c"} {
		if _, err := writer.CreateMemory(ctx, types.Memory{Id: id, Body: "body " + id, TimeStamp: time.Now().UnixNano(), Significance: 5}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	reader, err := NewSQLiteReadOnly(source)
	if err != nil {
		t.Fatalf("NewSQLiteReadOnly: %s", err)
	}

	t.Cleanup(func() { _ = reader.Close() })

	restored := t.TempDir()
	destination := filepath.Join(restored, "hippocampus.db")

	if err := reader.BackupTo(ctx, destination); err != nil {
		t.Fatalf("BackupTo: %s", err)
	}

	// No sidecar: the copy is one self-contained file.
	if _, err := os.Stat(destination + "-wal"); err == nil {
		t.Error("the backup left a -wal file beside it")
	}

	copied, err := NewSQLiteReadOnly(restored)
	if err != nil {
		t.Fatalf("opening the backup: %s", err)
	}

	t.Cleanup(func() { _ = copied.Close() })

	ids, err := copied.MemoryIdsMatching(ctx, MemoryFilter{})
	if err != nil {
		t.Fatalf("MemoryIdsMatching on the backup: %s", err)
	}

	if len(ids) != 3 {
		t.Errorf("the backup holds %d memories, want 3", len(ids))
	}
}

// TestBackupToRefusesToOverwrite: an existing file is never clobbered.
func TestBackupToRefusesToOverwrite(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = store.Close() })

	existing := filepath.Join(t.TempDir(), "previous.db")

	if err := os.WriteFile(existing, []byte("the last backup"), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	if err := store.BackupTo(context.Background(), existing); err == nil {
		t.Fatal("BackupTo overwrote an existing file")
	}

	if content, _ := os.ReadFile(existing); string(content) != "the last backup" {
		t.Error("the existing file was changed")
	}
}

// TestBackupToIsRefusedOnAServerDialect: the server dialects back up with their own tools.
func TestBackupToIsRefusedOnAServerDialect(t *testing.T) {
	for _, drv := range []driver{driverPostgres, driverMySQL} {
		d := &DB{driver: drv}

		if err := d.BackupTo(context.Background(), filepath.Join(t.TempDir(), "x.db")); !errors.Is(err, ErrBackupUnsupported) {
			t.Errorf("BackupTo on driver %d = %v, want ErrBackupUnsupported", drv, err)
		}
	}
}
