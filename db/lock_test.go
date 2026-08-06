package db

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// lockHelperDirectoryEnvVar names the storage directory the re-executed helper process should try
// to open. Its presence is what tells the helper test it is the helper rather than an ordinary run
// of the package's tests.
const lockHelperDirectoryEnvVar = "HIPPOCAMPUS_TEST_LOCK_HELPER_DIRECTORY"

// The markers the helper prints, matched by the parent. They are deliberately unlike anything the
// testing framework emits around them.
const (
	lockHelperAcquired = "HELPER_ACQUIRED_THE_LOCK"
	lockHelperRefused  = "HELPER_REFUSED: "
)

// TestStorageLockRefusesASecondProcess is the test the inter-process lock exists for, and the only
// one that can prove it: a lock that merely excluded a second open within one process would pass
// every other test here while leaving two service instances free to consolidate one store. It
// re-executes this test binary as a genuinely separate process (the helper below) against a
// directory this process already holds.
func TestStorageLockRefusesASecondProcess(t *testing.T) {
	directory := t.TempDir()

	held, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	defer func() { _ = held.Close() }()

	output := runLockHelper(t, directory)

	if strings.Contains(output, lockHelperAcquired) {
		t.Fatal("a second process opened a storage directory this process holds the lock on - two instances would consolidate one store")
	}

	if !strings.Contains(output, lockHelperRefused) {
		t.Fatalf("the helper process neither acquired nor reported a refusal; output:\n%s", output)
	}

	// The refusal has to say enough for an operator to act on it: which directory, and who has it.
	if !strings.Contains(output, directory) {
		t.Errorf("the refusal does not name the storage directory; output:\n%s", output)
	}

	if !strings.Contains(output, "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("the refusal does not name the holding process; output:\n%s", output)
	}
}

// TestStorageLockIsReleasedForASecondProcessOnClose covers the other half: a restart, a failover, or
// an operator's `--backfill-search` rebuild must be able to take the store the moment the previous
// process lets it go. Same helper, run after Close rather than before.
func TestStorageLockIsReleasedForASecondProcessOnClose(t *testing.T) {
	directory := t.TempDir()

	held, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if err := held.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	output := runLockHelper(t, directory)

	if !strings.Contains(output, lockHelperAcquired) {
		t.Fatalf("a second process could not open a storage directory nothing holds any more; output:\n%s", output)
	}
}

// runLockHelper re-executes this test binary running only TestStorageLockHelperProcess, pointed at
// the given directory, and returns everything it printed. A non-zero exit is not itself a failure -
// the helper reports its outcome on stdout, and the caller decides what the outcome should be.
func runLockHelper(t *testing.T, directory string) string {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageLockHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), lockHelperDirectoryEnvVar+"="+directory)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("helper process exited with %s", err)
	}

	return string(output)
}

// TestStorageLockHelperProcess is not a test of anything: it is the body of the child process the
// two tests above spawn, skipped in every ordinary run of the package. It opens the directory named
// in the environment and prints whether it got in.
func TestStorageLockHelperProcess(t *testing.T) {
	directory := os.Getenv(lockHelperDirectoryEnvVar)
	if directory == "" {
		t.Skip("not the helper process")
	}

	database, err := New(directory)
	if err != nil {
		fmt.Println(lockHelperRefused + err.Error())

		return
	}

	_ = database.Close()

	fmt.Println(lockHelperAcquired)
}

// TestStorageLockRefusesASecondOpenInOneProcess pins the in-process case too. It is the weaker
// property, but it is the one a mistake in the service's own wiring would trip over, and on both
// supported platforms the lock is per open handle rather than per process, so it should hold.
func TestStorageLockRefusesASecondOpenInOneProcess(t *testing.T) {
	directory := t.TempDir()

	first, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	defer func() { _ = first.Close() }()

	second, err := New(directory)
	if err == nil {
		_ = second.Close()

		t.Fatal("a second open of a locked storage directory succeeded")
	}

	if !strings.Contains(err.Error(), "already holds the storage lock") {
		t.Errorf("expected a storage lock refusal, got: %s", err)
	}
}

// TestStorageLockIsReleasedOnClose verifies the ordinary restart path in-process: reopening after a
// Close must work, since every reopen test in this package - and every service restart - depends on
// it.
func TestStorageLockIsReleasedOnClose(t *testing.T) {
	directory := t.TempDir()

	first, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := first.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "remember me"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	second, err := New(directory)
	if err != nil {
		t.Fatalf("reopen after Close: %s", err)
	}

	defer func() { _ = second.Close() }()

	memories, err := second.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 1 {
		t.Error("the reopened database does not hold the row written before the lock was released")
	}
}

// TestStorageLockFileRecordsTheHolder covers the diagnostics the refusal message is built from, and
// that a clean release leaves nothing behind naming a process that has exited.
func TestStorageLockFileRecordsTheHolder(t *testing.T) {
	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	lockPath := path.Join(directory, LockFileName)

	contents, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading the lock file: %s", err)
	}

	if !strings.Contains(string(contents), "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("the lock file does not record the holding pid, got: %q", string(contents))
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	contents, err = os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading the lock file after Close: %s", err)
	}

	if len(contents) != 0 {
		t.Errorf("a released lock file still names a holder, got: %q", string(contents))
	}
}

// TestInMemoryDatabaseTakesNoStorageLock pins that the empty-directory (in-memory) open is
// unaffected: there is no file to guard, and the package's own tests open many of them at once.
func TestInMemoryDatabaseTakesNoStorageLock(t *testing.T) {
	first, err := New("")
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	defer func() { _ = first.Close() }()

	second, err := New("")
	if err != nil {
		t.Fatalf("a second in-memory open was refused: %s", err)
	}

	defer func() { _ = second.Close() }()

	if first.lockFile != nil || second.lockFile != nil {
		t.Error("an in-memory database took a storage lock")
	}
}

// TestStorageLockFailsOnAnUnwritableDirectory covers the open failure: a directory the process
// cannot create the lock file in fails the open rather than starting unguarded.
func TestStorageLockFailsOnAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which bypasses directory permissions")
	}

	directory := t.TempDir()

	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatalf("chmod: %s", err)
	}

	// Restored so t.TempDir's cleanup can remove it.
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })

	database, err := New(directory)
	if err == nil {
		_ = database.Close()

		t.Fatal("New succeeded on a directory it cannot take the storage lock in")
	}
}

// TestLockHolderDetailsAreBestEffort covers the diagnostic helpers' failure paths: recording and
// reading the holder must never be able to fail an open or a release, because the exclusion is the
// lock's, not the file contents'.
func TestLockHolderDetailsAreBestEffort(t *testing.T) {
	directory := t.TempDir()
	lockPath := path.Join(directory, LockFileName)

	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0640)
	if err != nil {
		t.Fatalf("OpenFile: %s", err)
	}

	// An empty lock file - what a holder that failed to write one leaves - must produce no holder
	// clause rather than a half-formed one.
	if holder := readLockHolder(file); holder != "" {
		t.Errorf("expected no holder clause from an empty lock file, got %q", holder)
	}

	writeLockHolder(file)

	if holder := readLockHolder(file); !strings.Contains(holder, "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("expected the holder clause to name this process, got %q", holder)
	}

	// Every helper is driven against a closed handle, so each of its failure branches runs. None
	// may panic, and readLockHolder must still degrade to no clause at all.
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	writeLockHolder(file)

	if holder := readLockHolder(file); holder != "" {
		t.Errorf("expected no holder clause from an unreadable lock file, got %q", holder)
	}

	(&instanceLockFile{file: file, path: lockPath}).release()
}

// TestReadOnlyOpenIgnoresTheStorageLock pins the exemption the lock is deliberately scoped around:
// --backfill-search's read-only open is documented as safe to run beside a live service, so it must
// keep working while that service holds the lock.
func TestReadOnlyOpenIgnoresTheStorageLock(t *testing.T) {
	directory := t.TempDir()

	live, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	defer func() { _ = live.Close() }()

	if _, err := live.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "hello"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	readOnly, err := NewSQLiteReadOnly(directory)
	if err != nil {
		t.Fatalf("NewSQLiteReadOnly beside a locked store: %s", err)
	}

	defer func() { _ = readOnly.Close() }()

	if readOnly.lockFile != nil {
		t.Error("a read-only open took the storage lock")
	}

	memories, err := readOnly.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 1 {
		t.Error("the read-only open could not read the live store")
	}
}
