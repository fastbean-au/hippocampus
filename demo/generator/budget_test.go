package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
)

func TestNewBudgetInitialState(t *testing.T) {
	b := newBudget(1000)

	if b.paused() {
		t.Error("newBudget() should start unpaused")
	}

	if b.databaseBytes() != 0 {
		t.Errorf("databaseBytes() = %d, want 0", b.databaseBytes())
	}
}

func TestBudgetCheckPausesAndResumes(t *testing.T) {
	dir := t.TempDir()

	b := newBudget(1000)

	// No files on disk yet: size 0, well under the limit.
	b.check(dir)
	if b.paused() {
		t.Error("expected unpaused with no data file present")
	}

	// Write a data file at/above the limit; check() should pause.
	if err := os.WriteFile(filepath.Join(dir, db.DataFile), make([]byte, 1200), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	b.check(dir)
	if !b.paused() {
		t.Error("expected paused once the database file reaches the limit")
	}

	if b.databaseBytes() != 1200 {
		t.Errorf("databaseBytes() = %d, want 1200", b.databaseBytes())
	}

	// Shrink well below 90% of the limit; check() should resume.
	if err := os.WriteFile(filepath.Join(dir, db.DataFile), make([]byte, 100), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	b.check(dir)
	if b.paused() {
		t.Error("expected resumed once the database shrinks below 90% of the limit")
	}
}

func TestBudgetCheckStaysPausedInHysteresisBand(t *testing.T) {
	dir := t.TempDir()

	b := newBudget(1000)

	if err := os.WriteFile(filepath.Join(dir, db.DataFile), make([]byte, 1000), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	b.check(dir)
	if !b.paused() {
		t.Fatal("expected paused at the limit")
	}

	// Shrink to 950 bytes (within the 900-1000 hysteresis band): still paused.
	if err := os.WriteFile(filepath.Join(dir, db.DataFile), make([]byte, 950), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	b.check(dir)
	if !b.paused() {
		t.Error("expected still paused within the hysteresis band")
	}
}

func TestDatabaseSizeSumsSidecars(t *testing.T) {
	dir := t.TempDir()

	files := map[string]int{
		db.DataFile:           100,
		db.DataFile + "-wal":  50,
		db.DataFile + "-shm":  25,
		"unrelated-file.json": 1000,
	}

	for name, size := range files {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %s", name, err)
		}
	}

	if got, want := databaseSize(dir), int64(175); got != want {
		t.Errorf("databaseSize() = %d, want %d", got, want)
	}
}

func TestDatabaseSizeMissingDirectory(t *testing.T) {
	if got := databaseSize(filepath.Join(t.TempDir(), "does-not-exist")); got != 0 {
		t.Errorf("databaseSize() on missing directory = %d, want 0", got)
	}
}

func TestBudgetWatch(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, db.DataFile), make([]byte, 100), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	b := newBudget(1000)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		b.watch(ctx, dir)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("watch() did not return after context cancellation")

	}
}

func TestBudgetWatchTicks(t *testing.T) {
	orig := budgetWatchInterval
	budgetWatchInterval = 5 * time.Millisecond
	t.Cleanup(func() { budgetWatchInterval = orig })

	dir := t.TempDir()

	// Above the limit from the very first tick, so a tick firing is directly observable as paused().
	if err := os.WriteFile(filepath.Join(dir, db.DataFile), make([]byte, 1200), 0o600); err != nil {
		t.Fatalf("WriteFile: %s", err)
	}

	b := newBudget(1000)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		b.watch(ctx, dir)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("watch() did not return after context cancellation")

	}

	if !b.paused() {
		t.Error("expected watch() to have ticked at least once and paused generation")
	}
}
