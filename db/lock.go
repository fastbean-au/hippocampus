package db

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// LockFileName is the inter-process instance lock file, created beside the database file in
// storage.directory.
const LockFileName = "hippocampus.lock"

// instanceLockFile holds the operating-system file lock that keeps a second process off a SQLite
// store. It is the sqlite driver's counterpart to the pinned lock connection the server drivers use
// (a Postgres advisory lock, a MySQL GET_LOCK): SQLite's WAL mode deliberately permits several
// processes to write one database file, so nothing in the storage engine itself stops a second
// service instance from running its own consolidation and eviction against the same store - two
// schedulers deleting from one set of memories, silently. The SetMaxOpenConns(1) cap in New is a
// per-process pool cap and excludes nothing outside the process.
//
// An advisory lock on a separate file is used rather than one on the database file itself because
// the database must stay openable by the read-only paths that are explicitly allowed to run beside
// a live service (NewSQLiteReadOnly for --backfill-search, or an operator's sqlite3 shell); those
// take no lock and are unaffected. The lock is held by the kernel for as long as the file handle is
// open, so it is released the instant the process exits however it exits - there is no stale-lock
// problem to reason about, and the file's contents are diagnostics only, never the lock itself.
type instanceLockFile struct {
	file *os.File
	path string
}

// acquireStorageLock takes the exclusive inter-process lock on the given storage directory, failing
// immediately (never waiting) when another process already holds it. On success the returned handle
// must be released by Close; on failure the loser's error names the holder from the file's
// contents, which is the whole point of the file having contents at all.
func acquireStorageLock(directory string) (*instanceLockFile, error) {
	log.Trace("func() db.acquireStorageLock")

	lockPath := path.Join(directory, LockFileName)

	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0640)
	if err != nil {
		log.Errorf("failed to open the storage lock file '%s': %s", lockPath, err.Error())

		return nil, err
	}

	if err := lockFileExclusive(file); err != nil {
		// Read the holder's details before dropping the handle: the lock excludes locking, not
		// reading, so whatever the holder wrote is still legible to the loser.
		holder := readLockHolder(file)

		_ = file.Close()

		return nil, fmt.Errorf(
			"another hippocampus instance already holds the storage lock on '%s'%s - the sqlite driver is single-instance, so give this instance its own storage.directory or move the store to the postgres/mysql driver, which supports one consolidating instance plus replicas",
			directory,
			holder,
		)
	}

	writeLockHolder(file)

	return &instanceLockFile{file: file, path: lockPath}, nil
}

// writeLockHolder records who holds the lock, for the error message the next process to try prints.
// Best-effort throughout: a lock file that could not be written still excludes correctly, which is
// the property that matters, so a failure here is logged and never fails the open.
func writeLockHolder(file *os.File) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	contents := fmt.Sprintf(
		"pid %d\nhost %s\nsince %s\n",
		os.Getpid(),
		hostname,
		time.Now().UTC().Format(time.RFC3339),
	)

	if err := file.Truncate(0); err != nil {
		log.Warnf("failed to truncate the storage lock file: %s", err.Error())

		return
	}

	if _, err := file.WriteAt([]byte(contents), 0); err != nil {
		log.Warnf("failed to record the lock holder in the storage lock file: %s", err.Error())
	}
}

// readLockHolder renders the holder details recorded by writeLockHolder as a parenthesised clause to
// append to the refusal message, or an empty string when the file says nothing useful (a lock taken
// by a version that did not write one, or a write that failed). It never fails: the refusal stands
// on the lock, not on what the file says about it.
func readLockHolder(file *os.File) string {
	contents := make([]byte, 256)

	read, err := file.ReadAt(contents, 0)
	if read == 0 {
		if err != nil {
			log.Debugf("could not read the storage lock file's holder details: %s", err.Error())
		}

		return ""
	}

	fields := strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(contents[:read])), "\n", " "))
	if len(fields) == 0 {
		return ""
	}

	return " (held by " + strings.Join(fields, " ") + ")"
}

// release drops the lock by closing the handle holding it. The file is left in place - deleting it
// would race a process that has already opened it and is about to lock it - but is truncated first
// so a leftover file never names a process that has since exited.
func (l *instanceLockFile) release() {
	log.Trace("func() db.instanceLockFile.release")

	if err := l.file.Truncate(0); err != nil {
		log.Warnf("failed to clear the storage lock file '%s': %s", l.path, err.Error())
	}

	if err := l.file.Close(); err != nil {
		log.Errorf("failed to release the storage lock '%s': %s", l.path, err.Error())
	}
}
